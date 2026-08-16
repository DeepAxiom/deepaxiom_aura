package executor

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
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
	approve bool, op *testOperator, decisionOverride string) {
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
			held.Held, decision, time.Now().UnixMilli())
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
		"env-something-else", ledger.ApprovalApprove, time.Now().UnixMilli())
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
