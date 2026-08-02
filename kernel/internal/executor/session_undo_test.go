package executor

import (
	"encoding/json"
	"testing"

	"aura/kernel/internal/channel"
	"aura/kernel/internal/registry"
)

// Phase 2, `aura undo`: an undo is an ordinary graph edge — client.undo_out
// -> <skill>.<compensates.port> — validated against the ledger before the
// session that carries it is ever built (validateUndo). These tests reuse
// the helpers ledger_integration_test.go already built for the same motor
// skill fixture, since undo is the second half of exactly what those tests
// seal the first half of.

// graphUndo builds the one-edge ephemeral graph `aura undo` sends: the
// client delivering the original effect's payload to the declared
// compensation port of the exact skill package (use, not capability — an
// undo must reach the same actor that produced the effect, not merely
// something offering the same capability).
func graphUndo(use, port string) *Graph {
	g := &Graph{IR: IRMajor, GraphID: "undo-t"}
	g.Origin.Kind = "declared"
	g.Nodes = []Node{{Ref: "target", Use: use}}
	g.Edges = []Edge{{From: "client.undo_out", To: "target." + port}}
	return g
}

func undoData(session string) channel.Envelope {
	payload, _ := json.Marshal(map[string]any{"text": "undo the thing", "final": true})
	return channel.Envelope{
		V: channel.ProtocolMajor, ID: channel.NewID(), Session: session,
		Node: ClientRef, Port: "undo_out", Seq: 1, Idem: session + ":undo:1",
		Schema: "std/text@1", Kind: channel.KindData, Payload: payload,
	}
}

// allowPolicy is a policy that allows one capability outright, so a test can
// exercise the undo delivery itself without an unrelated gate in the way.
func allowPolicy(t *testing.T, capability string) *Policy {
	t.Helper()
	return mustLoadPolicy(t, `
policy: 1
default_effect: gate
rules:
  - match: "`+capability+`"
    decision: allow
`)
}

// --- the happy path: undo delivers and seals compensates -----------------------

func TestValidUndoDeliversAndSealsCompensates(t *testing.T) {
	reg := registry.New()
	id, m := motorManifestWithCompensation()
	delivered := liveSkillWithManifest(t, reg, id, m)
	ldg, st := testLedgerForSession(t)
	pol := allowPolicy(t, "motor.api.writer")

	orig, err := NewSession("sess-orig", graphInto("w", "motor.api.writer", ""),
		reg, st, "local", pol, ldg, "", func([]byte, string) error { return nil }, testLogger())
	if err != nil {
		t.Fatalf("NewSession (original): %v", err)
	}
	orig.Route(clientData("sess-orig"))
	entries := allEntries(t, st)
	if len(entries) != 1 {
		t.Fatalf("got %d entries after the original effect, want 1", len(entries))
	}
	receipt := entries[0].Hash()

	undo, err := NewSession("sess-undo", graphUndo(id, "undo_in"),
		reg, st, "local", pol, ldg, receipt, func([]byte, string) error { return nil }, testLogger())
	if err != nil {
		t.Fatalf("NewSession (undo): %v", err)
	}
	undo.Route(undoData("sess-undo"))

	if len(*delivered) != 2 {
		t.Fatalf("got %d deliveries (original + undo), want 2", len(*delivered))
	}
	if (*delivered)[1].Port != "undo_in" {
		t.Fatalf("the undo delivery landed on port %q, want undo_in", (*delivered)[1].Port)
	}

	entries = allEntries(t, st)
	if len(entries) != 2 {
		t.Fatalf("got %d ledger entries after the undo, want 2", len(entries))
	}
	if entries[1].Compensates != receipt {
		t.Fatalf("undo entry Compensates = %q, want the original receipt %q", entries[1].Compensates, receipt)
	}
}

// --- adversarial: a receipt cannot be undone twice ------------------------------

func TestDoubleUndoOfTheSameReceiptIsRefused(t *testing.T) {
	reg := registry.New()
	id, m := motorManifestWithCompensation()
	liveSkillWithManifest(t, reg, id, m)
	ldg, st := testLedgerForSession(t)
	pol := allowPolicy(t, "motor.api.writer")

	orig, err := NewSession("sess-orig", graphInto("w", "motor.api.writer", ""),
		reg, st, "local", pol, ldg, "", func([]byte, string) error { return nil }, testLogger())
	if err != nil {
		t.Fatalf("NewSession (original): %v", err)
	}
	orig.Route(clientData("sess-orig"))
	receipt := allEntries(t, st)[0].Hash()

	undo1, err := NewSession("sess-undo-1", graphUndo(id, "undo_in"),
		reg, st, "local", pol, ldg, receipt, func([]byte, string) error { return nil }, testLogger())
	if err != nil {
		t.Fatalf("first undo should be accepted: %v", err)
	}
	undo1.Route(undoData("sess-undo-1"))
	if len(allEntries(t, st)) != 2 {
		t.Fatalf("the first undo did not seal — nothing to refuse a second one against")
	}

	_, err = NewSession("sess-undo-2", graphUndo(id, "undo_in"),
		reg, st, "local", pol, ldg, receipt, func([]byte, string) error { return nil }, testLogger())
	if err == nil {
		t.Fatal("a second undo of the same receipt was accepted")
	}
	if !contains(err.Error(), "already undone") {
		t.Fatalf("refusal reads %q; it should say the receipt was already undone", err.Error())
	}
	if len(allEntries(t, st)) != 2 {
		t.Fatal("a refused second undo still sealed an entry")
	}
}

// --- adversarial: nothing to undo without a declared compensation --------------

func TestUndoOfAnEffectWithNoCompensationIsRefused(t *testing.T) {
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
	pol := allowPolicy(t, "motor.tts.speak")

	orig, err := NewSession("sess-orig", graphInto("w", "motor.tts.speak", ""),
		reg, st, "local", pol, ldg, "", func([]byte, string) error { return nil }, testLogger())
	if err != nil {
		t.Fatalf("NewSession (original): %v", err)
	}
	orig.Route(clientData("sess-orig"))
	receipt := allEntries(t, st)[0].Hash()

	_, err = NewSession("sess-undo", graphUndo("acme/motor/tts", "text_in"),
		reg, st, "local", pol, ldg, receipt, func([]byte, string) error { return nil }, testLogger())
	if err == nil {
		t.Fatal("undo of an effect with no declared compensation was accepted")
	}
	if !contains(err.Error(), "no compensation") {
		t.Fatalf("refusal reads %q; it should say no compensation was declared", err.Error())
	}
}

// --- adversarial: a denied gate was never delivered, so there is nothing to
// --- reverse — refused before the effect's compensation is even inspected -----

func TestUndoOfADeniedGateIsRefused(t *testing.T) {
	reg := registry.New()
	id, m := motorManifestWithCompensation()
	delivered := liveSkillWithManifest(t, reg, id, m)
	ldg, st := testLedgerForSession(t)

	var toClient []channel.Envelope
	orig, err := NewSession("sess-orig", graphInto("w", "motor.api.writer", ""),
		reg, st, "local", DefaultPolicy(), ldg, "",
		func(raw []byte, _ string) error {
			var env channel.Envelope
			_ = json.Unmarshal(raw, &env)
			toClient = append(toClient, env)
			return nil
		}, testLogger())
	if err != nil {
		t.Fatalf("NewSession (original): %v", err)
	}
	orig.Route(clientData("sess-orig"))
	if len(*delivered) != 0 {
		t.Fatal("the effect was delivered before the gate was resolved")
	}

	// Deny it.
	req := toClient[len(toClient)-1]
	payload, _ := json.Marshal(map[string]bool{"approve": false})
	orig.Route(channel.Envelope{
		V: channel.ProtocolMajor, ID: channel.NewID(), CauseID: req.ID,
		Session: "sess-orig", Kind: channel.KindConfirmResponse, Payload: payload,
	})

	entries := allEntries(t, st)
	if len(entries) != 1 || entries[0].Outcome != "denied" {
		t.Fatalf("want exactly 1 entry with outcome=denied, got %+v", entries)
	}
	receipt := entries[0].Hash()

	_, err = NewSession("sess-undo", graphUndo(id, "undo_in"),
		reg, st, "local", DefaultPolicy(), ldg, receipt, func([]byte, string) error { return nil }, testLogger())
	if err == nil {
		t.Fatal("undo of a denied (never-delivered) effect was accepted")
	}
	if !contains(err.Error(), "never delivered") {
		t.Fatalf("refusal reads %q; it should say the effect was never delivered", err.Error())
	}
}

// --- an undo delivery is gated like any other effect, and a denial of it -------
// --- still seals an entry carrying compensates -----------------------------

func TestUndoDeliveryIsGatedAndADenialStillSealsCompensates(t *testing.T) {
	reg := registry.New()
	id, m := motorManifestWithCompensation()
	delivered := liveSkillWithManifest(t, reg, id, m)
	ldg, st := testLedgerForSession(t)
	pol := allowPolicy(t, "motor.api.writer") // the ORIGINAL effect is ungated

	orig, err := NewSession("sess-orig", graphInto("w", "motor.api.writer", ""),
		reg, st, "local", pol, ldg, "", func([]byte, string) error { return nil }, testLogger())
	if err != nil {
		t.Fatalf("NewSession (original): %v", err)
	}
	orig.Route(clientData("sess-orig"))
	receipt := allEntries(t, st)[0].Hash()

	// DefaultPolicy gates motor.* by default — the undo itself must still
	// pass through the same checkpoint as anything else.
	var toClient []channel.Envelope
	undo, err := NewSession("sess-undo", graphUndo(id, "undo_in"),
		reg, st, "local", DefaultPolicy(), ldg, receipt,
		func(raw []byte, _ string) error {
			var env channel.Envelope
			_ = json.Unmarshal(raw, &env)
			toClient = append(toClient, env)
			return nil
		}, testLogger())
	if err != nil {
		t.Fatalf("NewSession (undo): %v", err)
	}
	undo.Route(undoData("sess-undo"))

	if len(*delivered) != 1 {
		t.Fatalf("the undo was delivered without approval; deliveries = %d, want 1 (the original only)", len(*delivered))
	}
	if len(toClient) != 1 || toClient[0].Kind != channel.KindConfirmRequest {
		t.Fatalf("want one confirm_request for the undo, got %+v", toClient)
	}

	// Deny the undo.
	req := toClient[len(toClient)-1]
	payload, _ := json.Marshal(map[string]bool{"approve": false})
	undo.Route(channel.Envelope{
		V: channel.ProtocolMajor, ID: channel.NewID(), CauseID: req.ID,
		Session: "sess-undo", Kind: channel.KindConfirmResponse, Payload: payload,
	})

	entries := allEntries(t, st)
	if len(entries) != 2 {
		t.Fatalf("got %d entries after denying the undo, want 2", len(entries))
	}
	undoEntry := entries[1]
	if undoEntry.Outcome != "denied" || undoEntry.Decision != "gate" {
		t.Fatalf("undo entry = %+v; want decision=gate outcome=denied", undoEntry)
	}
	if undoEntry.Compensates != receipt {
		t.Fatalf("a denied undo still must cite what it tried to reverse: Compensates = %q, want %q",
			undoEntry.Compensates, receipt)
	}

	// And since the undo was never delivered, the original is still undoable —
	// a refused undo attempt must not itself count as "already undone".
	if _, found, err := st.LedgerFindByCompensates(receipt); err != nil {
		t.Fatalf("LedgerFindByCompensates: %v", err)
	} else if found {
		t.Fatal("a denied undo (never delivered) was recorded as if it succeeded")
	}
}
