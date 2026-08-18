package ledger

import (
	"strings"
	"testing"
	"time"

	"aura/kernel/internal/spec"
)

// An audit report is the document a node hands to someone who does not trust
// it, so the tests that matter are the ones about it being wrong: a period that
// includes what it should not, counts that disagree with the evidence attached,
// receipts borrowed from another node's ledger.

// auditFixture seals a spread of effects and returns the ledger plus the node
// key everything verifies against.
func auditFixture(t *testing.T) (*Ledger, string) {
	t.Helper()
	keys := testKeys(t)
	ldg, err := Open(mustStore(t), "node-test", keys)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return ldg, keys.PublicB64()
}

// checkpoint commits the current head, which is what gives every sealed effect
// a signed tree for its inclusion proof to point at. `aura audit` does this
// before building for the same reason: checkpoints are periodic, so a report
// generated right after a burst of effects would otherwise summarise entries it
// could attach no evidence for.
func checkpoint(t *testing.T, ldg *Ledger) {
	t.Helper()
	if err := ldg.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
}

func sealAt(t *testing.T, ldg *Ledger, capability, decision, outcome string, waived bool) string {
	t.Helper()
	receipt, err := ldg.Seal(SealRequest{
		Session: "sess-audit", Envelope: "env-" + capability, Cause: "cause-1",
		Actor: "acme/motor/writer@1.0.0", Capability: capability,
		Decision: decision, Outcome: outcome, Policy: "sha256:policy-a",
		Payload: []byte(`{"n":1}`), Waived: waived,
	})
	if err != nil {
		t.Fatalf("seal %s: %v", capability, err)
	}
	return receipt
}

func TestAuditCountsWhatActed(t *testing.T) {
	ldg, pub := auditFixture(t)
	sealAt(t, ldg, "motor.erp.write", spec.DecisionAllow, spec.OutcomeDelivered, false)
	sealAt(t, ldg, "motor.erp.write", spec.DecisionAllow, spec.OutcomeDelivered, false)
	sealAt(t, ldg, "motor.pay.send", spec.DecisionGate, spec.OutcomeDelivered, false)
	sealAt(t, ldg, "motor.pay.send", spec.DecisionGate, spec.OutcomeDenied, false)
	sealAt(t, ldg, "motor.tts.speak", spec.DecisionAllow, spec.OutcomeDelivered, true)
	checkpoint(t, ldg)

	rep, err := BuildAudit(ldg.st, "node-test", pub,
		time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("BuildAudit: %v", err)
	}

	s := rep.Summary
	if s.Effects != 5 {
		t.Fatalf("counted %d effects, want 5", s.Effects)
	}
	if s.Delivered != 4 || s.Denied != 1 {
		t.Errorf("delivered/denied = %d/%d, want 4/1", s.Delivered, s.Denied)
	}
	if s.Gated != 2 || s.Allowed != 3 {
		t.Errorf("gated/allowed = %d/%d, want 2/3", s.Gated, s.Allowed)
	}
	// The distinction the whole `waived` field exists for has to survive into
	// the document an auditor reads, or it was recorded for nobody.
	if s.Waived != 1 {
		t.Errorf("waived = %d, want 1", s.Waived)
	}
	// Nobody signed anything here, and a report that quietly implied otherwise
	// would be the most damaging kind of wrong.
	if s.Signed != 0 || s.UnsignedGated != 2 {
		t.Errorf("signed/unsigned gated = %d/%d, want 0/2", s.Signed, s.UnsignedGated)
	}
	if len(rep.Capabilities) != 3 {
		t.Errorf("got %d capabilities, want 3", len(rep.Capabilities))
	}
	if len(rep.Gated) != 2 {
		t.Errorf("attached %d receipts for 2 gated effects", len(rep.Gated))
	}
}

// A period is a filter, and a filter that leaks is a report about the wrong
// thing. Until is exclusive so consecutive reports tile without double-counting.
func TestAuditPeriodExcludesWhatIsOutsideIt(t *testing.T) {
	ldg, pub := auditFixture(t)
	sealAt(t, ldg, "motor.erp.write", spec.DecisionAllow, spec.OutcomeDelivered, false)

	past, err := BuildAudit(ldg.st, "node-test", pub,
		time.Now().AddDate(0, 0, -30), time.Now().AddDate(0, 0, -29))
	if err != nil {
		t.Fatalf("BuildAudit: %v", err)
	}
	if past.Summary.Effects != 0 {
		t.Errorf("a period before anything happened counted %d effects", past.Summary.Effects)
	}
	// ...and the integrity section still covers the whole chain, because an
	// edit anywhere breaks every hash after it.
	if past.Integrity.Entries == 0 {
		t.Error("an empty period reported an empty ledger; integrity is chain-wide, not period-wide")
	}
	if !past.Integrity.Sound {
		t.Error("an intact ledger was reported unsound")
	}
}

func TestAuditRejectsAnInvertedPeriod(t *testing.T) {
	ldg, pub := auditFixture(t)
	_, err := BuildAudit(ldg.st, "node-test", pub, time.Now(), time.Now().Add(-time.Hour))
	if err == nil {
		t.Fatal("a period ending before it starts was accepted")
	}
}

// The document has to verify from itself, with no database and no node.
func TestAuditVerifiesStandalone(t *testing.T) {
	ldg, pub := auditFixture(t)
	sealAt(t, ldg, "motor.pay.send", spec.DecisionGate, spec.OutcomeDelivered, false)
	sealAt(t, ldg, "motor.erp.write", spec.DecisionAllow, spec.OutcomeDelivered, false)
	checkpoint(t, ldg)

	rep, err := BuildAudit(ldg.st, "node-test", pub,
		time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("BuildAudit: %v", err)
	}
	v := VerifyAudit(rep)
	if !v.Sound {
		t.Fatalf("a freshly built report did not verify: %s %+v", v.Failure, v.Findings)
	}
	if v.ReceiptsChecked != 1 || v.ReceiptsValid != 1 {
		t.Errorf("checked/valid receipts = %d/%d, want 1/1", v.ReceiptsChecked, v.ReceiptsValid)
	}
}

// Editing the summary must not be a way to make a report say something the
// evidence does not. This is the attack a document format has to survive:
// everything attached is genuine, and the numbers on top are a lie.
func TestAuditRefusesASummaryTheEvidenceContradicts(t *testing.T) {
	ldg, pub := auditFixture(t)
	sealAt(t, ldg, "motor.pay.send", spec.DecisionGate, spec.OutcomeDelivered, false)
	checkpoint(t, ldg)

	rep, err := BuildAudit(ldg.st, "node-test", pub,
		time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("BuildAudit: %v", err)
	}
	// Claim more gated effects than there are receipts for, without truncation.
	rep.Summary.Gated = 9
	rep.Summary.Effects = 9 + rep.Summary.Allowed

	v := VerifyAudit(rep)
	if v.Sound {
		t.Fatal("a report claiming gated effects it did not attach verified as sound")
	}
	if !hasFinding(v, "high", "receipts are attached") {
		t.Errorf("the mismatch was not reported; findings: %+v", v.Findings)
	}
}

// A receipt from another node's ledger verifies perfectly on its own. Binding
// each one to the key the report is about is what stops a report being padded
// with genuine evidence about somebody else.
func TestAuditRefusesAReceiptFromAnotherNode(t *testing.T) {
	ldg, pub := auditFixture(t)
	sealAt(t, ldg, "motor.pay.send", spec.DecisionGate, spec.OutcomeDelivered, false)
	checkpoint(t, ldg)
	rep, err := BuildAudit(ldg.st, "node-test", pub,
		time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("BuildAudit: %v", err)
	}

	// Same document, claiming to be about a node whose key signed none of it.
	other, otherPub := auditFixture(t)
	_ = other
	rep.NodePubkey = otherPub

	v := VerifyAudit(rep)
	if v.Sound {
		t.Fatal("a report verified against a key that signed none of its receipts")
	}
	if !hasFinding(v, "high", "different node") {
		t.Errorf("the wrong-node receipt was not reported; findings: %+v", v.Findings)
	}
}

func TestAuditRefusesAnUnknownVersion(t *testing.T) {
	v := VerifyAudit(AuditReport{Version: "aura-audit-report/99", NodePubkey: "k", From: 1, Until: 2})
	if v.Sound || !strings.Contains(v.Failure, "unknown document version") {
		t.Fatalf("an unreadable document version was not refused: %+v", v)
	}
}

// Notes are not failures. A sound report should still tell a reviewer what is
// worth looking at — an unwitnessed history, an unsigned gate, a waiver.
func TestAuditRaisesNotesWithoutFailing(t *testing.T) {
	ldg, pub := auditFixture(t)
	sealAt(t, ldg, "motor.pay.send", spec.DecisionGate, spec.OutcomeDelivered, false)
	sealAt(t, ldg, "motor.tts.speak", spec.DecisionAllow, spec.OutcomeDelivered, true)
	checkpoint(t, ldg)

	rep, err := BuildAudit(ldg.st, "node-test", pub,
		time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("BuildAudit: %v", err)
	}
	v := VerifyAudit(rep)
	if !v.Sound {
		t.Fatalf("notes were treated as failures: %+v", v.Findings)
	}
	for _, want := range []string{"no operator signature", "excused from their gate", "no third party"} {
		if !hasFinding(v, "note", want) {
			t.Errorf("expected a note mentioning %q; findings: %+v", want, v.Findings)
		}
	}
}

// Two reports over the same period must be byte-identical, or consecutive
// periods cannot be diffed — which is most of what a reviewer does.
func TestAuditIsDeterministic(t *testing.T) {
	ldg, pub := auditFixture(t)
	for _, c := range []string{"motor.a.x", "motor.b.y", "motor.c.z", "motor.a.x"} {
		sealAt(t, ldg, c, spec.DecisionAllow, spec.OutcomeDelivered, false)
	}
	from, to := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)

	first, err := BuildAudit(ldg.st, "node-test", pub, from, to)
	if err != nil {
		t.Fatalf("BuildAudit: %v", err)
	}
	second, err := BuildAudit(ldg.st, "node-test", pub, from, to)
	if err != nil {
		t.Fatalf("BuildAudit: %v", err)
	}
	if len(first.Capabilities) != len(second.Capabilities) {
		t.Fatal("two reports over the same period disagree on how many capabilities acted")
	}
	for i := range first.Capabilities {
		if first.Capabilities[i].Capability != second.Capabilities[i].Capability {
			t.Fatalf("capability ordering is not stable at %d: %q vs %q",
				i, first.Capabilities[i].Capability, second.Capabilities[i].Capability)
		}
	}
}

func hasFinding(v AuditVerdict, severity, substr string) bool {
	for _, f := range v.Findings {
		if f.Severity == severity && strings.Contains(f.Detail, substr) {
			return true
		}
	}
	return false
}

// Committing the current head is what `aura audit` does before every run, so
// doing it twice must not be an error. It used to fail on a UNIQUE constraint,
// which made "checkpoint, then read" an operation performable once per head.
func TestCheckpointIsIdempotentAtAnUnchangedHead(t *testing.T) {
	ldg, _ := auditFixture(t)
	sealAt(t, ldg, "motor.erp.write", spec.DecisionAllow, spec.OutcomeDelivered, false)

	checkpoint(t, ldg)
	if err := ldg.Checkpoint(); err != nil {
		t.Fatalf("checkpointing an unchanged head: %v", err)
	}

	before, err := ldg.st.LedgerCheckpoints()
	if err != nil {
		t.Fatalf("LedgerCheckpoints: %v", err)
	}
	// A new effect moves the head, and that one does get signed.
	sealAt(t, ldg, "motor.erp.write", spec.DecisionAllow, spec.OutcomeDelivered, false)
	checkpoint(t, ldg)
	after, err := ldg.st.LedgerCheckpoints()
	if err != nil {
		t.Fatalf("LedgerCheckpoints: %v", err)
	}
	if len(after) != len(before)+1 {
		t.Fatalf("a moved head produced %d checkpoints, want %d", len(after), len(before)+1)
	}
}

// "On what basis" is the third question an auditor asks, after "what acted" and
// "who authorized it". The report has to answer it without laundering a claim
// into evidence: a model configuration a skill merely declared and one a TEE
// quote binds to that exact declaration are different things.
func TestAuditSeparatesDeclaredFromHardwareBoundInference(t *testing.T) {
	ldg, pub := auditFixture(t)

	// One self-declared attestation, one carrying evidence bound to itself.
	declared := []byte(`{"engine":"llama.cpp","model":"acme/declared-model","ts":1}`)
	declaredHash := AttestationHash(declared)
	if err := RecordAttestation(ldg.st, declaredHash, declared); err != nil {
		t.Fatalf("RecordAttestation: %v", err)
	}
	bound := attestWithEvidence(t, nil)
	boundHash := AttestationHash(bound)
	if err := RecordAttestation(ldg.st, boundHash, bound); err != nil {
		t.Fatalf("RecordAttestation: %v", err)
	}

	if _, err := ldg.Seal(SealRequest{
		Session: "sess-basis", Envelope: "env-1", Cause: "c-1",
		Actor: "acme/motor/writer@1.0.0", Capability: "motor.erp.write",
		Decision: spec.DecisionAllow, Outcome: spec.OutcomeDelivered,
		Policy: "sha256:p", Payload: []byte(`{}`),
		Inference: []string{declaredHash, boundHash},
	}); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	checkpoint(t, ldg)

	rep, err := BuildAudit(ldg.st, "node-test", pub,
		time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("BuildAudit: %v", err)
	}

	in := rep.Inference
	if in.Cited != 2 {
		t.Fatalf("cited %d attestations, want 2", in.Cited)
	}
	if in.SelfDeclared != 1 {
		t.Errorf("self-declared = %d, want 1", in.SelfDeclared)
	}
	if in.Bound != 1 {
		t.Errorf("hardware-bound = %d, want 1", in.Bound)
	}
	// No anchors were supplied, so nothing may claim `verified`.
	if in.Verified != 0 {
		t.Errorf("verified = %d with no trust anchor configured; that claim was not earned",
			in.Verified)
	}
	if len(in.Models) != 2 {
		t.Errorf("got %d distinct models, want 2: %v", len(in.Models), in.Models)
	}

	// A mixed period is still sound, and the reviewer is told which half is
	// which rather than left to assume the stronger one.
	v := VerifyAudit(rep)
	if !v.Sound {
		t.Fatalf("a report with mixed evidence was reported unsound: %+v", v.Findings)
	}
	if !hasFinding(v, "note", "no vendor trust anchor") {
		t.Errorf("the unchecked vendor chain was not surfaced; findings: %+v", v.Findings)
	}
}

// An entry citing evidence that is gone or altered is a broken promise, not a
// note: the ledger points at something that no longer says what it said.
func TestAuditFlagsInferenceThatNoLongerMatchesItsAddress(t *testing.T) {
	ldg, pub := auditFixture(t)

	record := []byte(`{"engine":"llama.cpp","model":"acme/m","ts":1}`)
	hash := AttestationHash(record)
	if err := RecordAttestation(ldg.st, hash, record); err != nil {
		t.Fatalf("RecordAttestation: %v", err)
	}
	if _, err := ldg.Seal(SealRequest{
		Session: "sess-broken", Envelope: "env-1", Cause: "c-1",
		Actor: "acme/motor/writer@1.0.0", Capability: "motor.erp.write",
		Decision: spec.DecisionAllow, Outcome: spec.OutcomeDelivered,
		Policy: "sha256:p", Payload: []byte(`{}`),
		// Cite an attestation that was never stored.
		Inference: []string{"sha256:0000000000000000000000000000000000000000000000000000000000000000"},
	}); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	checkpoint(t, ldg)

	rep, err := BuildAudit(ldg.st, "node-test", pub,
		time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("BuildAudit: %v", err)
	}
	if rep.Inference.Unreadable != 1 {
		t.Fatalf("unreadable = %d, want 1", rep.Inference.Unreadable)
	}
	v := VerifyAudit(rep)
	if v.Sound {
		t.Fatal("a report citing evidence that is gone verified as sound")
	}
	if !hasFinding(v, "high", "no longer") {
		t.Errorf("the missing evidence was not reported as a failure; findings: %+v", v.Findings)
	}
}
