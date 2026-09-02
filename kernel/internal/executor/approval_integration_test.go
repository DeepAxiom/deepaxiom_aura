package executor

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aura/kernel/internal/approver"
	"aura/kernel/internal/channel"
	"aura/kernel/internal/ledger"
	"aura/kernel/internal/registry"
	"aura/kernel/internal/store"
)

// C4 v1.3 end to end, through the real gate.
//
// The unit tests in internal/ledger prove a signature binds what it claims to,
// and internal/approver proves the roster refuses a stranger. What is left, and
// what these cover, is the part that can only be wrong in the wiring: that the
// executor puts the verified approval into the entry it seals, that it refuses
// rather than downgrades when the policy demands one, and that a denial is
// recorded with the name of whoever refused it.

type testOperator struct {
	id   string
	priv ed25519.PrivateKey
	pub  string
}

func enrolledOperator(t *testing.T, st *store.Store, id string) (*approver.Registry, testOperator) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	pubB64 := base64.StdEncoding.EncodeToString(pub)
	r, err := approver.Load(st)
	if err != nil {
		t.Fatalf("load roster: %v", err)
	}
	if err := r.Enroll(id, pubB64, ""); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	return r, testOperator{id: id, priv: priv, pub: pubB64}
}

// answerGate sends a confirm_response, optionally carrying a signed approval
// over the delivery the gate is actually holding.
func answerGate(t *testing.T, sess *Session, req channel.Envelope, sessionID string,
	approve bool, op *testOperator, decisionOverride string, shown ...ledger.ContextEntry) {
	t.Helper()

	body := map[string]any{"approve": approve}
	if op != nil {
		var held struct {
			Held string `json:"held"`
		}
		if err := json.Unmarshal(req.Payload, &held); err != nil || held.Held == "" {
			t.Fatalf("the confirm_request does not name the held delivery: %s", req.Payload)
		}
		decision := ledger.ApprovalApprove
		if !approve {
			decision = ledger.ApprovalDeny
		}
		if decisionOverride != "" {
			decision = decisionOverride
		}
		a, err := ledger.SignApproval(op.id, op.priv, "node-test", sessionID,
			held.Held, decision, time.Now().UnixMilli(), shown)
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		body["approval"] = a
	}
	payload, _ := json.Marshal(body)
	sess.Route(channel.Envelope{
		V: channel.ProtocolMajor, ID: channel.NewID(), CauseID: req.ID,
		Session: sessionID, Kind: channel.KindConfirmResponse, Payload: payload,
	})
}

// gatedSession wires a session whose single motor edge is gated, and returns
// what the skill received plus what the client was sent.
func gatedSession(t *testing.T, id string, pol *Policy, roster *approver.Registry) (
	*Session, *store.Store, *[]channel.Envelope, *[]channel.Envelope) {
	t.Helper()
	reg := registry.New()
	skillID, m := motorManifestWithCompensation()
	delivered := liveSkillWithManifest(t, reg, skillID, m)
	ldg, st := testLedgerForSession(t)

	toClient := &[]channel.Envelope{}
	sess, err := NewSession(id, graphInto("w", "motor.api.writer", GateHumanApproval),
		reg, st, "local", pol, ldg, "",
		func(raw []byte, _ string) error {
			var env channel.Envelope
			_ = json.Unmarshal(raw, &env)
			*toClient = append(*toClient, env)
			return nil
		}, testLogger())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	sess.approvers = roster
	return sess, st, delivered, toClient
}

func lastConfirmRequest(t *testing.T, toClient []channel.Envelope) channel.Envelope {
	t.Helper()
	for i := len(toClient) - 1; i >= 0; i-- {
		if toClient[i].Kind == channel.KindConfirmRequest {
			return toClient[i]
		}
	}
	t.Fatal("no confirm_request was sent to the client")
	return channel.Envelope{}
}

func TestSignedApprovalIsSealedIntoTheEntry(t *testing.T) {
	const sid = "sess-appr-1"
	st0 := testStore(t)
	roster, op := enrolledOperator(t, st0, "grace")
	sess, st, delivered, toClient := gatedSession(t, sid, DefaultPolicy(), roster)

	sess.Route(clientData(sid))
	answerGate(t, sess, lastConfirmRequest(t, *toClient), sid, true, &op, "")

	if len(*delivered) != 1 {
		t.Fatalf("got %d deliveries after a signed approval, want 1", len(*delivered))
	}
	entries := allEntries(t, st)
	if len(entries) != 1 {
		t.Fatalf("ledger has %d entries, want 1", len(entries))
	}
	e := entries[0]
	if e.Approver == nil {
		t.Fatal("the sealed entry does not record who approved it")
	}
	if e.Approver.Operator != "grace" {
		t.Errorf("approver = %q, want grace", e.Approver.Operator)
	}
	if e.Approver.Decision != ledger.ApprovalApprove {
		t.Errorf("sealed decision = %q", e.Approver.Decision)
	}
	// The whole point: it still verifies from the entry alone, with no roster
	// and no kernel — which is what an auditor will actually have.
	if err := e.Approver.Verify(e.Node, e.Session); err != nil {
		t.Errorf("the sealed approval does not verify offline: %v", err)
	}
}

func TestSignedDenialRecordsWhoRefused(t *testing.T) {
	const sid = "sess-appr-2"
	st0 := testStore(t)
	roster, op := enrolledOperator(t, st0, "grace")
	sess, st, delivered, toClient := gatedSession(t, sid, DefaultPolicy(), roster)

	sess.Route(clientData(sid))
	answerGate(t, sess, lastConfirmRequest(t, *toClient), sid, false, &op, "")

	if len(*delivered) != 0 {
		t.Fatal("a refused effect was delivered anyway")
	}
	entries := allEntries(t, st)
	if len(entries) != 1 {
		t.Fatalf("ledger has %d entries, want 1 (the refusal)", len(entries))
	}
	e := entries[0]
	if e.Outcome != "denied" {
		t.Errorf("outcome = %q, want denied", e.Outcome)
	}
	if e.Approver == nil || e.Approver.Operator != "grace" {
		t.Fatalf("the refusal does not record who gave it: %+v", e.Approver)
	}
	if e.Approver.Decision != ledger.ApprovalDeny {
		t.Errorf("sealed decision = %q, want deny", e.Approver.Decision)
	}
}

// require_signed_approval means what it says: an unsigned answer is a refusal,
// not a fallback. An enforcement that can be skipped by omitting a field
// enforces nothing.
func TestRequiredSignatureRefusesAnUnsignedAnswer(t *testing.T) {
	const sid = "sess-appr-3"
	st0 := testStore(t)
	roster, _ := enrolledOperator(t, st0, "grace")
	pol := mustLoadPolicy(t, `
policy: 1
default_effect: gate
require_signed_approval: true
`)
	sess, st, delivered, toClient := gatedSession(t, sid, pol, roster)

	sess.Route(clientData(sid))
	answerGate(t, sess, lastConfirmRequest(t, *toClient), sid, true, nil, "")

	if len(*delivered) != 0 {
		t.Fatal("an unsigned 'approve' released the effect on a node requiring signatures")
	}
	entries := allEntries(t, st)
	if len(entries) != 1 || entries[0].Outcome != "denied" {
		t.Fatalf("the refusal was not sealed as a denial: %+v", entries)
	}
	if entries[0].Approver != nil {
		t.Error("an entry with no valid approval must not carry an approver")
	}
}

func TestRequiredSignatureAcceptsASignedAnswer(t *testing.T) {
	const sid = "sess-appr-4"
	st0 := testStore(t)
	roster, op := enrolledOperator(t, st0, "grace")
	pol := mustLoadPolicy(t, `
policy: 1
default_effect: gate
require_signed_approval: true
`)
	sess, _, delivered, toClient := gatedSession(t, sid, pol, roster)

	sess.Route(clientData(sid))
	answerGate(t, sess, lastConfirmRequest(t, *toClient), sid, true, &op, "")

	if len(*delivered) != 1 {
		t.Fatalf("a properly signed approval was refused (%d deliveries)", len(*delivered))
	}
}

// An unenrolled key that signs as "grace" produces a cryptographically valid
// statement. The gate must still refuse it — and must refuse rather than seal
// it unverified, since an entry is permanent.
func TestGateRefusesAnUnenrolledSigner(t *testing.T) {
	const sid = "sess-appr-5"
	st0 := testStore(t)
	roster, _ := enrolledOperator(t, st0, "grace")

	_, attackerPriv, _ := ed25519.GenerateKey(rand.Reader)
	attacker := testOperator{id: "grace", priv: attackerPriv}

	sess, st, delivered, toClient := gatedSession(t, sid, DefaultPolicy(), roster)
	sess.Route(clientData(sid))
	answerGate(t, sess, lastConfirmRequest(t, *toClient), sid, true, &attacker, "")

	if len(*delivered) != 0 {
		t.Fatal("an approval signed by a key this node never enrolled released the effect")
	}
	entries := allEntries(t, st)
	if len(entries) != 1 || entries[0].Outcome != "denied" {
		t.Fatalf("the forgery was not sealed as a denial: %+v", entries)
	}
	if entries[0].Approver != nil {
		t.Error("a rejected signature was sealed into the entry anyway")
	}
}

// The transport boolean and the signed decision must agree. Otherwise
// intercepting a refusal and flipping one JSON field turns it into an approval,
// with the signature still verifying because it covers "deny".
func TestGateRefusesWhenTheSignedDecisionDisagreesWithTheAnswer(t *testing.T) {
	const sid = "sess-appr-6"
	st0 := testStore(t)
	roster, op := enrolledOperator(t, st0, "grace")
	sess, st, delivered, toClient := gatedSession(t, sid, DefaultPolicy(), roster)

	sess.Route(clientData(sid))
	// approve:true on the wire, but what grace signed was a denial.
	answerGate(t, sess, lastConfirmRequest(t, *toClient), sid, true, &op, ledger.ApprovalDeny)

	if len(*delivered) != 0 {
		t.Fatal("a signed denial was forwarded as an approval by flipping the boolean")
	}
	if entries := allEntries(t, st); len(entries) != 1 || entries[0].Outcome != "denied" {
		t.Fatalf("entries = %+v", entries)
	}
}

// An approval for one delivery must not release another. Within a single
// session this is the difference between approving one payment and approving
// the next one.
func TestGateRefusesAnApprovalForAnotherDelivery(t *testing.T) {
	const sid = "sess-appr-7"
	st0 := testStore(t)
	roster, op := enrolledOperator(t, st0, "grace")
	sess, st, delivered, toClient := gatedSession(t, sid, DefaultPolicy(), roster)

	sess.Route(clientData(sid))
	req := lastConfirmRequest(t, *toClient)

	wrong, err := ledger.SignApproval(op.id, op.priv, "node-test", sid,
		"env-something-else", ledger.ApprovalApprove, time.Now().UnixMilli(), nil)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	payload, _ := json.Marshal(map[string]any{"approve": true, "approval": wrong})
	sess.Route(channel.Envelope{
		V: channel.ProtocolMajor, ID: channel.NewID(), CauseID: req.ID,
		Session: sid, Kind: channel.KindConfirmResponse, Payload: payload,
	})

	if len(*delivered) != 0 {
		t.Fatal("an approval naming a different delivery released this one")
	}
	if entries := allEntries(t, st); len(entries) != 1 || entries[0].Outcome != "denied" {
		t.Fatalf("entries = %+v", entries)
	}
}

// A node with no roster must refuse a signature rather than seal one nobody
// checked: an unverified approval in the ledger reads as proof forever.
func TestSignatureWithoutARosterIsRefused(t *testing.T) {
	const sid = "sess-appr-8"
	st0 := testStore(t)
	_, op := enrolledOperator(t, st0, "grace")

	sess, st, delivered, toClient := gatedSession(t, sid, DefaultPolicy(), nil)
	sess.Route(clientData(sid))
	answerGate(t, sess, lastConfirmRequest(t, *toClient), sid, true, &op, "")

	if len(*delivered) != 0 {
		t.Fatal("a node with no roster accepted a signature it could not check")
	}
	if entries := allEntries(t, st); len(entries) != 1 || entries[0].Approver != nil {
		t.Fatalf("an unverifiable approval was sealed: %+v", entries)
	}
}

// The pre-v1.3 path is untouched: no signature, no requirement, unchanged
// behaviour. This is what makes the whole feature additive rather than a
// breaking change to every client that already answers gates.
func TestUnsignedApprovalStillWorksWhenNotRequired(t *testing.T) {
	const sid = "sess-appr-9"
	sess, st, delivered, toClient := gatedSession(t, sid, DefaultPolicy(), nil)

	sess.Route(clientData(sid))
	answerGate(t, sess, lastConfirmRequest(t, *toClient), sid, true, nil, "")

	if len(*delivered) != 1 {
		t.Fatalf("an unsigned approval stopped working (%d deliveries)", len(*delivered))
	}
	entries := allEntries(t, st)
	if len(entries) != 1 || entries[0].Outcome != "delivered" {
		t.Fatalf("entries = %+v", entries)
	}
	if entries[0].Approver != nil {
		t.Error("an unsigned answer produced an approver field")
	}
}

// A verified approval belongs to exactly one delivery and is spent by it. A
// second gate in the same session has to be answered on its own.
func TestApprovalIsConsumedBySealing(t *testing.T) {
	const sid = "sess-appr-10"
	st0 := testStore(t)
	roster, op := enrolledOperator(t, st0, "grace")
	sess, _, _, toClient := gatedSession(t, sid, DefaultPolicy(), roster)

	sess.Route(clientData(sid))
	answerGate(t, sess, lastConfirmRequest(t, *toClient), sid, true, &op, "")

	sess.apprMu.Lock()
	left := len(sess.approved)
	sess.apprMu.Unlock()
	if left != 0 {
		t.Errorf("%d approval(s) left parked after sealing — a long session would accumulate them", left)
	}
}

func TestPolicyParsesRequireSignedApproval(t *testing.T) {
	pol := mustLoadPolicy(t, `
policy: 1
default_effect: gate
require_signed_approval: true
`)
	if !pol.SignedApprovalRequired() {
		t.Error("require_signed_approval: true did not take effect")
	}
	if DefaultPolicy().SignedApprovalRequired() {
		t.Error("the built-in default must not require signatures — a fresh node has no operators")
	}
	// The flag has to reach the document hash, or two policies that differ only
	// in whether approvals must be signed would cite the same hash in the
	// ledger and be indistinguishable to an auditor.
	if pol.Hash() == DefaultPolicy().Hash() {
		t.Error("require_signed_approval does not change the policy hash")
	}
}

func TestErrorMessageNamesTheMissingSignature(t *testing.T) {
	const sid = "sess-appr-11"
	st0 := testStore(t)
	roster, _ := enrolledOperator(t, st0, "grace")
	pol := mustLoadPolicy(t, `
policy: 1
default_effect: gate
require_signed_approval: true
`)
	sess, _, _, toClient := gatedSession(t, sid, pol, roster)
	sess.Route(clientData(sid))
	answerGate(t, sess, lastConfirmRequest(t, *toClient), sid, true, nil, "")

	var errText string
	for _, e := range *toClient {
		if e.Kind == channel.KindError {
			errText = string(e.Payload)
		}
	}
	if !strings.Contains(errText, "signed approval") {
		t.Errorf("the client was not told why it was refused: %s", errText)
	}
}

// C4 v1.7 — an approval that binds what the operator was shown.
//
// The gate is where the requirement has to bite. Everything the ledger package
// proves about the signature is true of an approval sitting in a variable; what
// these ask is whether an effect can still reach a skill when the person who
// released it never committed to a document.

func shownNote(hexDigit string) ledger.ContextEntry {
	return ledger.ContextEntry{Label: "screen", Digest: "sha256:" + strings.Repeat(hexDigit, 64)}
}

func requiringScreen(t *testing.T) *Policy {
	t.Helper()
	return mustLoadPolicy(t, `
policy: 1
default_effect: gate
require_approval_context: [screen]
`)
}

func TestRequiringContextImpliesRequiringASignature(t *testing.T) {
	pol := requiringScreen(t)
	if !pol.SignedApprovalRequired() {
		t.Error("a policy that requires a bound approval still accepts unsigned answers, " +
			"which is the cheaper route around it")
	}
	if got := pol.ApprovalContextRequired(); len(got) != 1 || got[0] != "screen" {
		t.Errorf("ApprovalContextRequired = %v, want [screen]", got)
	}
	if len(DefaultPolicy().ApprovalContextRequired()) != 0 {
		t.Error("the built-in default requires context — a fresh node would deny its first write")
	}
	if pol.Hash() == DefaultPolicy().Hash() {
		t.Error("require_approval_context does not change the policy hash, so two policies " +
			"that differ on it cite the same document in the ledger")
	}
}

func TestPolicyRefusesARequirementNothingCouldSatisfy(t *testing.T) {
	// Each of these denies every gated effect on the node while looking, in the
	// logs, like a client that will not send what it is asked for. They are
	// refused at load, where the file that caused it can be named.
	cases := map[string]string{
		"a label no approval could carry":       `["Screen Hash"]`,
		"the same label twice":                  `[screen, screen]`,
		"more labels than an approval may bind": `[a, b, c, d, e, f, g, h, i]`,
	}
	for name, list := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "policy.yaml")
			doc := "policy: 1\ndefault_effect: gate\nrequire_approval_context: " + list + "\n"
			if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadPolicy(path); err == nil {
				t.Fatal("loaded without complaint")
			}
		})
	}

	// And the ordinary case still loads.
	path := filepath.Join(t.TempDir(), "policy.yaml")
	if err := os.WriteFile(path, []byte("policy: 1\ndefault_effect: gate\nrequire_approval_context: [screen]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pol, err := LoadPolicy(path)
	if err != nil {
		t.Fatalf("a policy naming one ordinary label was refused: %v", err)
	}
	if got := pol.ApprovalContextRequired(); len(got) != 1 || got[0] != "screen" {
		t.Errorf("ApprovalContextRequired = %v, want [screen]", got)
	}
}

func TestTheQuestionSaysWhatAnsweringItWillTake(t *testing.T) {
	const sid = "sess-ctx-0"
	st0 := testStore(t)
	roster, _ := enrolledOperator(t, st0, "grace")
	sess, _, _, toClient := gatedSession(t, sid, requiringScreen(t), roster)

	sess.Route(clientData(sid))
	req := lastConfirmRequest(t, *toClient)

	var body struct {
		ContextRequired []string `json:"context_required"`
	}
	if err := json.Unmarshal(req.Payload, &body); err != nil {
		t.Fatalf("confirm_request payload: %v", err)
	}
	// Without this a client learns the requirement by being refused, which is
	// after a human has already read the question and answered it.
	if len(body.ContextRequired) != 1 || body.ContextRequired[0] != "screen" {
		t.Fatalf("context_required = %v, want [screen]", body.ContextRequired)
	}
	// And it must be labels, never digests: a digest this node supplied would
	// be a digest of whatever it wished it had displayed.
	if strings.Contains(string(req.Payload), "sha256:") {
		t.Errorf("the question carries a digest, which the node has no business computing: %s", req.Payload)
	}
}

func TestAGateThatRequiresNothingSaysNothing(t *testing.T) {
	const sid = "sess-ctx-0b"
	st0 := testStore(t)
	roster, _ := enrolledOperator(t, st0, "grace")
	sess, _, _, toClient := gatedSession(t, sid, DefaultPolicy(), roster)

	sess.Route(clientData(sid))
	req := lastConfirmRequest(t, *toClient)
	if strings.Contains(string(req.Payload), "context_required") {
		t.Errorf("a node requiring nothing still advertises a requirement: %s", req.Payload)
	}
}

func TestAnApprovalThatBindsNothingDoesNotReleaseTheEffect(t *testing.T) {
	const sid = "sess-ctx-1"
	st0 := testStore(t)
	roster, op := enrolledOperator(t, st0, "grace")
	sess, st, delivered, toClient := gatedSession(t, sid, requiringScreen(t), roster)

	sess.Route(clientData(sid))
	answerGate(t, sess, lastConfirmRequest(t, *toClient), sid, true, &op, "")

	if len(*delivered) != 0 {
		t.Fatal("a signed approval that binds no document released the effect anyway")
	}
	entries := allEntries(t, st)
	if len(entries) != 1 || entries[0].Outcome != "denied" {
		t.Fatalf("want one sealed denial, got %d entries", len(entries))
	}
	// The refusal has to name the label, or the operator has no way to comply.
	if !mentionsScreen(*toClient) {
		t.Error("the refusal does not say which artifact the answer had to bind")
	}
}

func TestAnApprovalThatBindsTheScreenReleasesItAndIsSealed(t *testing.T) {
	const sid = "sess-ctx-2"
	st0 := testStore(t)
	roster, op := enrolledOperator(t, st0, "grace")
	sess, st, delivered, toClient := gatedSession(t, sid, requiringScreen(t), roster)

	sess.Route(clientData(sid))
	answerGate(t, sess, lastConfirmRequest(t, *toClient), sid, true, &op, "", shownNote("a"))

	if len(*delivered) != 1 {
		t.Fatalf("got %d deliveries, want 1", len(*delivered))
	}
	entries := allEntries(t, st)
	if len(entries) != 1 {
		t.Fatalf("ledger has %d entries, want 1", len(entries))
	}
	e := entries[0]
	if e.Approver == nil || len(e.Approver.Context) != 1 {
		t.Fatalf("the sealed entry does not record what the approver was shown: %+v", e.Approver)
	}
	if e.Approver.Context[0].Label != "screen" {
		t.Errorf("sealed context = %+v, want the screen", e.Approver.Context)
	}
	// Still checkable from the entry alone, which is what an auditor will have.
	if err := e.Approver.Verify(e.Node, e.Session); err != nil {
		t.Errorf("the sealed context-bound approval does not verify offline: %v", err)
	}
	// And what was sealed is what was signed: editing the digest in the stored
	// entry has to break it, or the binding is a note rather than evidence.
	e.Approver.Context[0].Digest = "sha256:" + strings.Repeat("b", 64)
	if err := e.Approver.Verify(e.Node, e.Session); err == nil {
		t.Error("the sealed document can be swapped and the approval still verifies")
	}
}

func TestBindingTheWrongArtifactIsNotBindingTheRightOne(t *testing.T) {
	const sid = "sess-ctx-3"
	st0 := testStore(t)
	roster, op := enrolledOperator(t, st0, "grace")
	sess, _, delivered, toClient := gatedSession(t, sid, requiringScreen(t), roster)

	sess.Route(clientData(sid))
	answerGate(t, sess, lastConfirmRequest(t, *toClient), sid, true, &op, "",
		ledger.ContextEntry{Label: "invoice", Digest: "sha256:" + strings.Repeat("c", 64)})

	if len(*delivered) != 0 {
		t.Fatal("binding some other artifact satisfied a requirement for the screen")
	}
}

func TestAnUnsignedAnswerUnderContextPolicySaysWhatIsMissing(t *testing.T) {
	const sid = "sess-ctx-4"
	st0 := testStore(t)
	roster, _ := enrolledOperator(t, st0, "grace")
	sess, _, delivered, toClient := gatedSession(t, sid, requiringScreen(t), roster)

	sess.Route(clientData(sid))
	answerGate(t, sess, lastConfirmRequest(t, *toClient), sid, true, nil, "")

	if len(*delivered) != 0 {
		t.Fatal("an unsigned answer released an effect on a node that requires a bound approval")
	}
	if !mentionsScreen(*toClient) {
		t.Error("the refusal does not tell an unsigned client what this node wants")
	}
}

// mentionsScreen reports whether any error sent back to the client names the
// label the node required. The message is the only thing the operator has.
func mentionsScreen(toClient []channel.Envelope) bool {
	for _, env := range toClient {
		if env.Kind == channel.KindError && strings.Contains(string(env.Payload), "screen") {
			return true
		}
	}
	return false
}
