package executor

import (
	"strings"
	"testing"

	"aura/kernel/internal/registry"
)

// Sealing writes to disk, and disks fill up. What happens then used to be
// decided here — the error was logged and the effect went out anyway — which
// made "every effect is attested" true only while the disk held. That is a
// reasonable default and a bad guarantee, so it is now the operator's call.
//
// These tests drive the real failure: the store is closed underneath a live
// session, so Seal fails the way it would fail for real, and the two stances
// are checked against what actually reaches the skill.

// breakLedger closes the store a session is sealing into, so the next Seal
// fails. Closing is what a full disk or a broken file would look like from the
// ledger's side, and it is a genuine failure rather than an injected one.
func breakLedger(t *testing.T, sess *Session) {
	t.Helper()
	if err := sess.st.Close(); err != nil {
		t.Fatalf("closing the store under the session: %v", err)
	}
}

// The default: the node stays up, the effect goes through, and the gap is the
// operator's to notice. This is behaviour worth pinning down precisely because
// it is the weaker of the two — a silent change to it would turn a documented
// hole into an undocumented one.
func TestSealFailureDeliversByDefault(t *testing.T) {
	reg := registry.New()
	id, m := motorManifestWithCompensation()
	delivered := liveSkillWithManifest(t, reg, id, m)
	ldg, st := testLedgerForSession(t)

	pol := mustLoadPolicy(t, `
policy: 1
default_effect: gate
rules:
  - match: "motor.api.writer"
    decision: allow
`)
	if pol.SealFailureRefuses() {
		t.Fatal("a policy that says nothing about on_seal_failure must default to deliver")
	}
	sess, err := NewSession("sess-seal-fail-1", graphInto("w", "motor.api.writer", ""),
		reg, st, "local", pol, ldg, "", func([]byte, string) error { return nil }, testLogger())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	breakLedger(t, sess)
	sess.Route(clientData("sess-seal-fail-1"))

	if len(*delivered) != 1 {
		t.Fatalf("got %d deliveries, want 1 — the default must not stop the effect", len(*delivered))
	}
	// And it must be honest about what it could not do: an unsealed effect
	// carries no receipt, so nothing downstream can mistake it for attested.
	if r := (*delivered)[0].Receipt; r != "" {
		t.Errorf("an unsealed effect carried receipt %q; that receipt attests nothing", r)
	}
}

// `refuse` is the stance a node sealing payments wants: nothing acts on the
// world that this node cannot afterwards prove it authorized.
func TestSealFailureRefusesWhenPolicySaysSo(t *testing.T) {
	reg := registry.New()
	id, m := motorManifestWithCompensation()
	delivered := liveSkillWithManifest(t, reg, id, m)
	ldg, st := testLedgerForSession(t)

	var toClient []string
	pol := mustLoadPolicy(t, `
policy: 1
default_effect: gate
on_seal_failure: refuse
rules:
  - match: "motor.api.writer"
    decision: allow
`)
	if !pol.SealFailureRefuses() {
		t.Fatal("on_seal_failure: refuse did not take effect")
	}
	sess, err := NewSession("sess-seal-fail-2", graphInto("w", "motor.api.writer", ""),
		reg, st, "local", pol, ldg, "",
		func(raw []byte, _ string) error { toClient = append(toClient, string(raw)); return nil },
		testLogger())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	breakLedger(t, sess)
	sess.Route(clientData("sess-seal-fail-2"))

	if len(*delivered) != 0 {
		t.Fatalf("%d effect(s) reached the skill unattested under on_seal_failure: refuse", len(*delivered))
	}
	// A refusal the caller never hears about is a hang, not a refusal.
	joined := strings.Join(toClient, "\n")
	if !strings.Contains(joined, "could not seal") {
		t.Errorf("the client was not told why the effect was refused; got: %s", joined)
	}
}

// A policy file that misspells the stance must be refused at load, not silently
// treated as the permissive one. An operator who wrote `on_seal_failure: reject`
// meant `refuse`, and quietly running as `deliver` is the failure mode this
// whole option exists to remove.
func TestUnknownSealFailureStanceIsRefusedAtLoad(t *testing.T) {
	if _, err := LoadPolicy(writePolicy(t, "policy: 1\non_seal_failure: reject\n")); err == nil {
		t.Fatal("a misspelt on_seal_failure loaded as if it were valid")
	} else if !strings.Contains(err.Error(), "deliver|refuse") {
		t.Errorf("the error should name the valid stances; got: %v", err)
	}
}
