package executor

import (
	"encoding/json"
	"fmt"
	"testing"

	"aura/kernel/internal/channel"
	"aura/kernel/internal/registry"
)

// Phase 2, session resume: a Session's ephemeral bookkeeping (dedup, causal/
// in-flight, pending gates, per-hop Seq) is never durable — only the event
// log is. Every test here proves the SAME property: discard the live Session
// object entirely and rebuild one with NewSession against the SAME store —
// which is exactly what happens whether the client reconnected or the kernel
// process itself restarted, since neither is distinguished anywhere in this
// code path — and check the rebuilt session behaves as if it had never gone
// away.

// --- dedup ---------------------------------------------------------------

func TestResumeReconstructsDedup(t *testing.T) {
	reg := registry.New()
	delivered := liveSkill(t, reg, "acme/logical/echo", "logical", "logical.echo")
	st := testStore(t)

	newSess := func() *Session {
		sess, err := NewSession("s-resume-dedup", graphInto("e", "logical.echo", ""),
			reg, st, "local", DefaultPolicy(), nil, "", func([]byte, string) error { return nil }, testLogger())
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		return sess
	}

	sess1 := newSess()
	msg := clientData("s-resume-dedup")
	sess1.Route(msg)
	if len(*delivered) != 1 {
		t.Fatalf("got %d deliveries before resume, want 1", len(*delivered))
	}

	sess2 := newSess() // simulates a reconnect or a kernel restart
	sess2.Route(msg)   // the exact same envelope — same Idem — retried
	if len(*delivered) != 1 {
		t.Fatalf("got %d deliveries after resume, want 1 — the retry should have been deduped", len(*delivered))
	}
}

// --- pending human-approval gates ------------------------------------------

func TestResumeReconstructsAPendingGate(t *testing.T) {
	reg := registry.New()
	delivered := liveSkill(t, reg, "acme/motor/writer", "motor", "motor.api.writer")
	st := testStore(t)

	var toClient []channel.Envelope
	captureToClient := func(raw []byte, _ string) error {
		var env channel.Envelope
		_ = json.Unmarshal(raw, &env)
		toClient = append(toClient, env)
		return nil
	}
	newSess := func() *Session {
		sess, err := NewSession("s-resume-gate", graphInto("w", "motor.api.writer", ""),
			reg, st, "local", DefaultPolicy(), nil, "", captureToClient, testLogger())
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		return sess
	}

	sess1 := newSess()
	sess1.Route(clientData("s-resume-gate"))
	if len(*delivered) != 0 {
		t.Fatal("the effect was delivered before approval")
	}
	if len(toClient) != 1 || toClient[0].Kind != channel.KindConfirmRequest {
		t.Fatalf("want one confirm_request, got %+v", toClient)
	}
	reqID := toClient[0].ID

	sess2 := newSess() // the process that was holding the gate is gone

	payload, _ := json.Marshal(map[string]bool{"approve": true})
	sess2.Route(channel.Envelope{
		V: channel.ProtocolMajor, ID: channel.NewID(), CauseID: reqID,
		Session: "s-resume-gate", Kind: channel.KindConfirmResponse, Payload: payload,
	})

	if len(*delivered) != 1 {
		t.Fatalf("approving the original gate after resume produced %d deliveries, want 1", len(*delivered))
	}
}

// A denial after resume must be sealed exactly like a same-process denial —
// re-checks that the reconstructed pendingGate carries the right `dest`
// (including its gate), not just that *a* delivery happens.
func TestResumeReconstructedGateDenialSealsCorrectly(t *testing.T) {
	reg := registry.New()
	id, m := motorManifestWithCompensation()
	delivered := liveSkillWithManifest(t, reg, id, m)
	ldg, st := testLedgerForSession(t)

	var toClient []channel.Envelope
	captureToClient := func(raw []byte, _ string) error {
		var env channel.Envelope
		_ = json.Unmarshal(raw, &env)
		toClient = append(toClient, env)
		return nil
	}
	newSess := func() *Session {
		sess, err := NewSession("s-resume-deny", graphInto("w", "motor.api.writer", ""),
			reg, st, "local", DefaultPolicy(), ldg, "", captureToClient, testLogger())
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		return sess
	}

	sess1 := newSess()
	sess1.Route(clientData("s-resume-deny"))
	reqID := toClient[len(toClient)-1].ID

	sess2 := newSess()
	payload, _ := json.Marshal(map[string]bool{"approve": false})
	sess2.Route(channel.Envelope{
		V: channel.ProtocolMajor, ID: channel.NewID(), CauseID: reqID,
		Session: "s-resume-deny", Kind: channel.KindConfirmResponse, Payload: payload,
	})

	if len(*delivered) != 0 {
		t.Fatal("a denied gate was delivered anyway")
	}
	entries := allEntries(t, st)
	if len(entries) != 1 || entries[0].Decision != "gate" || entries[0].Outcome != "denied" {
		t.Fatalf("entries = %+v; want exactly one gate/denied entry", entries)
	}
}

// --- cancel: causal + in-flight reconstruction -----------------------------

func TestResumeReconstructsInFlightForCancel(t *testing.T) {
	reg := registry.New()
	toA := liveSkill(t, reg, "acme/logical/a", "logical", "logical.a")
	toB := liveSkill(t, reg, "acme/logical/b", "logical", "logical.b")
	st := testStore(t)

	newSess := func() *Session {
		sess, err := NewSession("s-resume-cancel", chain(), reg, st, "local", DefaultPolicy(), nil, "",
			func([]byte, string) error { return nil }, testLogger())
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		return sess
	}

	sess1 := newSess()
	client := clientData("s-resume-cancel")
	sess1.Route(client)
	if len(*toA) != 1 {
		t.Fatalf("a should have received the client message, got %d", len(*toA))
	}
	deliveredToA := (*toA)[0]

	emitA := emission(deliveredToA, "a", "text_out", "partial")
	sess1.Route(emitA)
	if len(*toB) != 1 {
		t.Fatalf("b should have received a's emission, got %d", len(*toB))
	}

	sess2 := newSess() // discard sess1 — the in-memory bookkeeping is gone
	sess2.Route(cancelFor("s-resume-cancel", client.ID))

	cancelsToA := onlyCancels(*toA)
	if len(cancelsToA) != 1 {
		t.Fatalf("skill a got %d cancel(s) after resume, want 1", len(cancelsToA))
	}
	if cancelsToA[0].CauseID != client.ID {
		t.Fatalf("a's cancel carries cause_id %q, want %q", cancelsToA[0].CauseID, client.ID)
	}

	cancelsToB := onlyCancels(*toB)
	if len(cancelsToB) != 1 {
		t.Fatalf("skill b got %d cancel(s) after resume, want 1 — "+
			"in-flight tracking was not reconstructed", len(cancelsToB))
	}
	if cancelsToB[0].CauseID != emitA.ID {
		t.Fatalf("b's cancel carries cause_id %q, want %q (what b actually saw)",
			cancelsToB[0].CauseID, emitA.ID)
	}
}

// A root cancelled just before the session disappeared must stay suppressed
// after resume — not just be cancellable again.
func TestResumeKeepsAPreviouslyCancelledRootSuppressed(t *testing.T) {
	reg := registry.New()
	toA := liveSkill(t, reg, "acme/logical/a", "logical", "logical.a")
	toB := liveSkill(t, reg, "acme/logical/b", "logical", "logical.b")
	st := testStore(t)

	newSess := func() *Session {
		sess, err := NewSession("s-resume-suppress", chain(), reg, st, "local", DefaultPolicy(), nil, "",
			func([]byte, string) error { return nil }, testLogger())
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		return sess
	}

	sess1 := newSess()
	client := clientData("s-resume-suppress")
	sess1.Route(client)
	deliveredToA := (*toA)[0]
	sess1.Route(cancelFor("s-resume-suppress", client.ID))

	sess2 := newSess()
	before := len(*toB)
	sess2.Route(emission(deliveredToA, "a", "text_out", "late, after resume"))
	if len(*toB) != before {
		t.Fatal("a root cancelled before resume was not suppressed after it")
	}
}

// --- per-hop Seq continuity --------------------------------------------------

// The adversarial case a naive reset-to-zero reconstruction would pass every
// other test above while still silently breaking: C3 requires a hop's Seq to
// be monotonic, and a resumed hop restarting at 1 violates that for any
// consumer still watching the old numbering.
func TestResumePerHopSeqContinuesRatherThanRestarting(t *testing.T) {
	reg := registry.New()
	liveSkill(t, reg, "acme/logical/echo", "logical", "logical.echo")
	st := testStore(t)

	newSess := func() *Session {
		sess, err := NewSession("s-resume-seq", graphInto("e", "logical.echo", ""),
			reg, st, "local", DefaultPolicy(), nil, "", func([]byte, string) error { return nil }, testLogger())
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		return sess
	}

	sess1 := newSess()
	for i := 0; i < 3; i++ {
		m := clientData("s-resume-seq")
		m.Idem = fmt.Sprintf("s-resume-seq:%d", i)
		sess1.Route(m)
	}

	sess2 := newSess()
	m := clientData("s-resume-seq")
	m.Idem = "s-resume-seq:final"
	sess2.Route(m)

	events, _, err := st.SessionEvents("s-resume-seq", 0)
	if err != nil {
		t.Fatalf("SessionEvents: %v", err)
	}
	var maxSeq uint64
	for _, raw := range events {
		var e channel.Envelope
		_ = json.Unmarshal(raw, &e)
		if e.Node == "e" && e.Port == "text_in" && e.Seq > maxSeq {
			maxSeq = e.Seq
		}
	}
	if maxSeq != 4 {
		t.Fatalf("hop e.text_in reached Seq %d after resume; want 4 "+
			"(3 before + 1 after — a reset would show 1)", maxSeq)
	}
}

// --- a brand-new session is unaffected ---------------------------------------

// This is what makes it safe to call resumeFromLog unconditionally on every
// session, not just reconnects: with no prior log, it is a no-op.
func TestResumeIsANoOpForABrandNewSession(t *testing.T) {
	reg := registry.New()
	delivered := liveSkill(t, reg, "acme/logical/echo", "logical", "logical.echo")
	st := testStore(t)

	sess, err := NewSession("s-resume-fresh", graphInto("e", "logical.echo", ""),
		reg, st, "local", DefaultPolicy(), nil, "", func([]byte, string) error { return nil }, testLogger())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	sess.Route(clientData("s-resume-fresh"))
	if len(*delivered) != 1 {
		t.Fatalf("got %d deliveries for a brand-new session, want 1", len(*delivered))
	}
}
