package executor

import (
	"encoding/json"
	"testing"

	"aura/kernel/internal/channel"
	"aura/kernel/internal/ledger"
	"aura/kernel/internal/registry"
	"aura/kernel/internal/signing"
	"aura/kernel/internal/store"
)

// The rest of this package proves the policy and gate machinery in isolation
// with a nil ledger — deliberately, so those tests are not obscured by
// attestation plumbing they are not about. These tests are the other half:
// proof that a *real* ledger, wired the way main.go wires one, actually gets
// written to by the Effect Checkpoint in Session.forward and Session.resolveGate.

// testLedgerForSession builds a real ledger over a fresh store and keypair,
// and hands back the store too — session tests read entries straight out of
// it rather than through a second Ledger-level accessor, since inspecting
// exactly what got persisted is the point.
func testLedgerForSession(t *testing.T) (*ledger.Ledger, *store.Store) {
	t.Helper()
	st := testStore(t)
	keys, err := signing.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("signing.LoadOrCreate: %v", err)
	}
	ldg, err := ledger.Open(st, "node-test", keys)
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	return ldg, st
}

// allEntries reads every sealed entry back out, in seq order.
func allEntries(t *testing.T, st *store.Store) []ledger.Entry {
	t.Helper()
	raw, err := st.LedgerEntries(1, 0)
	if err != nil {
		t.Fatalf("LedgerEntries: %v", err)
	}
	out := make([]ledger.Entry, len(raw))
	for i, r := range raw {
		if err := json.Unmarshal(r, &out[i]); err != nil {
			t.Fatalf("entry %d is not valid: %v", i, err)
		}
	}
	return out
}

func motorManifestWithCompensation() (id string, m registry.Manifest) {
	id = "acme/motor/writer"
	man := registry.Manifest{
		ID: id, Version: "1.2.0", Protocol: "1",
		Name: "Writer", Description: "writes",
		Capability: "motor.api.writer", Type: "motor", Format: "source",
	}
	man.Ports.Ingress = []registry.Port{
		{Name: "text_in", Schema: "std/text@1"},
		{Name: "undo_in", Schema: "std/text@1"},
	}
	man.Ports.Egress = []registry.Port{{Name: "text_out", Schema: "std/text@1"}}
	man.Compensates = &registry.Compensation{Port: "undo_in", Schema: "std/text@1"}
	return id, man
}

// liveSkillWithManifest is liveSkill but for a caller-built manifest, needed
// here because these tests want a motor skill that also declares compensates.
func liveSkillWithManifest(t *testing.T, reg *registry.Registry, id string, m registry.Manifest) *[]channel.Envelope {
	t.Helper()
	got := &[]channel.Envelope{}
	reg.Register(id, &registry.Live{
		Manifest: m,
		Send: func(raw []byte, _ string) error {
			var env channel.Envelope
			if err := json.Unmarshal(raw, &env); err != nil {
				return err
			}
			*got = append(*got, env)
			return nil
		},
	})
	return got
}

// --- an ungated effect is sealed as allow/delivered --------------------------

func TestForwardSealsAnUngatedEffectAsAllowDelivered(t *testing.T) {
	reg := registry.New()
	id, m := motorManifestWithCompensation()
	delivered := liveSkillWithManifest(t, reg, id, m)
	ldg, st := testLedgerForSession(t)

	// Policy allows this capability outright, so no gate is asked for.
	pol := mustLoadPolicy(t, `
policy: 1
default_effect: gate
rules:
  - match: "motor.api.writer"
    decision: allow
`)
	sess, err := NewSession("sess-seal-1", graphInto("w", "motor.api.writer", ""),
		reg, st, "local", pol, ldg, func([]byte, string) error { return nil }, testLogger())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	sess.Route(clientData("sess-seal-1"))

	if len(*delivered) != 1 {
		t.Fatalf("got %d deliveries, want 1", len(*delivered))
	}

	entries := allEntries(t, st)
	if len(entries) != 1 {
		t.Fatalf("ledger has %d entries, want exactly 1", len(entries))
	}
	if entries[0].Decision != "allow" || entries[0].Outcome != "delivered" {
		t.Fatalf("entry = %+v; want decision=allow outcome=delivered", entries[0])
	}

	// The delivered envelope must carry the receipt the ledger produced.
	got := (*delivered)[0]
	if got.Receipt == "" {
		t.Fatal("the delivered envelope carries no receipt")
	}
	if got.Receipt != entries[0].Hash() {
		t.Fatalf("receipt on the envelope (%s) does not match the sealed entry's own hash (%s)",
			got.Receipt, entries[0].Hash())
	}
}

// --- an approved gate is sealed as gate/delivered -----------------------------

func TestApprovedGateIsSealedAsGateDelivered(t *testing.T) {
	reg := registry.New()
	id, m := motorManifestWithCompensation()
	delivered := liveSkillWithManifest(t, reg, id, m)
	ldg, st := testLedgerForSession(t)

	var toClient []channel.Envelope
	sess, err := NewSession("sess-seal-2", graphInto("w", "motor.api.writer", ""),
		reg, st, "local", DefaultPolicy(), ldg,
		func(raw []byte, _ string) error {
			var env channel.Envelope
			_ = json.Unmarshal(raw, &env)
			toClient = append(toClient, env)
			return nil
		}, testLogger())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	sess.Route(clientData("sess-seal-2"))

	if len(*delivered) != 0 {
		t.Fatal("the effect was delivered before approval")
	}
	if len(allEntries(t, st)) != 0 {
		t.Fatal("a held gate was sealed before it was resolved")
	}

	// Approve.
	req := toClient[len(toClient)-1]
	payload, _ := json.Marshal(map[string]bool{"approve": true})
	sess.Route(channel.Envelope{
		V: channel.ProtocolMajor, ID: channel.NewID(), CauseID: req.ID,
		Session: "sess-seal-2", Kind: channel.KindConfirmResponse, Payload: payload,
	})

	if len(*delivered) != 1 {
		t.Fatalf("got %d deliveries after approval, want 1", len(*delivered))
	}
	entries := allEntries(t, st)
	if len(entries) != 1 {
		t.Fatalf("ledger has %d entries after approval, want 1", len(entries))
	}
	if entries[0].Decision != "gate" || entries[0].Outcome != "delivered" {
		t.Fatalf("entry = %+v; want decision=gate outcome=delivered", entries[0])
	}
	if (*delivered)[0].Receipt == "" {
		t.Fatal("the approved-and-delivered envelope carries no receipt")
	}
}

// --- a denied gate is sealed as gate/denied, with no delivery -----------------

func TestDeniedGateIsSealedAsGateDenied(t *testing.T) {
	reg := registry.New()
	id, m := motorManifestWithCompensation()
	delivered := liveSkillWithManifest(t, reg, id, m)
	ldg, st := testLedgerForSession(t)

	var toClient []channel.Envelope
	sess, err := NewSession("sess-seal-3", graphInto("w", "motor.api.writer", ""),
		reg, st, "local", DefaultPolicy(), ldg,
		func(raw []byte, _ string) error {
			var env channel.Envelope
			_ = json.Unmarshal(raw, &env)
			toClient = append(toClient, env)
			return nil
		}, testLogger())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	sess.Route(clientData("sess-seal-3"))

	req := toClient[len(toClient)-1]
	payload, _ := json.Marshal(map[string]bool{"approve": false})
	sess.Route(channel.Envelope{
		V: channel.ProtocolMajor, ID: channel.NewID(), CauseID: req.ID,
		Session: "sess-seal-3", Kind: channel.KindConfirmResponse, Payload: payload,
	})

	if len(*delivered) != 0 {
		t.Fatal("a denied effect was delivered to the skill")
	}
	entries := allEntries(t, st)
	if len(entries) != 1 {
		t.Fatalf("ledger has %d entries after a denial, want 1 (the refusal itself is a record)", len(entries))
	}
	if entries[0].Decision != "gate" || entries[0].Outcome != "denied" {
		t.Fatalf("entry = %+v; want decision=gate outcome=denied", entries[0])
	}
}

// --- non-effects are never sealed ---------------------------------------------

func TestNonMotorDeliveryIsNeverSealed(t *testing.T) {
	reg := registry.New()
	liveSkill(t, reg, "acme/logical/echo", "logical", "logical.echo")
	ldg, st := testLedgerForSession(t)

	sess, err := NewSession("sess-seal-4", graphInto("e", "logical.echo", ""),
		reg, st, "local", DefaultPolicy(), ldg,
		func([]byte, string) error { return nil }, testLogger())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	sess.Route(clientData("sess-seal-4"))

	if entries := allEntries(t, st); len(entries) != 0 {
		t.Fatalf("a non-effect delivery was sealed (%d entries)", len(entries))
	}
}

// --- the sealed entry cites the policy that authorized it, and its actor ------

func TestSealedEntryCitesThePolicyActorAndCompensation(t *testing.T) {
	reg := registry.New()
	id, m := motorManifestWithCompensation()
	liveSkillWithManifest(t, reg, id, m)
	ldg, st := testLedgerForSession(t)

	pol := mustLoadPolicy(t, `
policy: 1
default_effect: gate
rules:
  - match: "motor.api.writer"
    decision: allow
`)
	sess, err := NewSession("sess-seal-5", graphInto("w", "motor.api.writer", ""),
		reg, st, "local", pol, ldg, func([]byte, string) error { return nil }, testLogger())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	sess.Route(clientData("sess-seal-5"))

	entries := allEntries(t, st)
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].Policy != pol.Hash() {
		t.Fatalf("sealed entry cites policy %q, want the session's policy hash %q",
			entries[0].Policy, pol.Hash())
	}
	if entries[0].Actor != id+"@1.2.0" {
		t.Fatalf("actor = %q, want %q", entries[0].Actor, id+"@1.2.0")
	}
	if entries[0].Compensation == nil || entries[0].Compensation.Port != "undo_in" {
		t.Fatalf("compensation metadata missing or wrong: %+v", entries[0].Compensation)
	}
}

// A skill with no declared compensates is recorded as irreversible — the
// field is absent, not zero-valued.
func TestNoCompensatesLeavesTheEntryIrreversible(t *testing.T) {
	reg := registry.New()
	m := registry.Manifest{
		ID: "acme/motor/tts", Version: "1.0.0", Protocol: "1",
		Name: "TTS", Description: "speaks",
		Capability: "motor.tts.speak", Type: "motor", Format: "source",
	}
	m.Ports.Ingress = []registry.Port{{Name: "text_in", Schema: "std/text@1"}}
	m.Ports.Egress = []registry.Port{{Name: "text_out", Schema: "std/text@1"}}
	liveSkillWithManifest(t, reg, "acme/motor/tts", m)
	ldg, st := testLedgerForSession(t)

	pol := mustLoadPolicy(t, `
policy: 1
default_effect: gate
rules:
  - match: "motor.tts.speak"
    decision: allow
`)
	sess, err := NewSession("sess-seal-6", graphInto("w", "motor.tts.speak", ""),
		reg, st, "local", pol, ldg, func([]byte, string) error { return nil }, testLogger())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	sess.Route(clientData("sess-seal-6"))

	entries := allEntries(t, st)
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].Compensation != nil {
		t.Fatalf("an undeclared compensation was recorded anyway: %+v", entries[0].Compensation)
	}
}
