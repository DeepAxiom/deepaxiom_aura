package executor

import (
	"encoding/json"
	"testing"

	"aura/kernel/internal/channel"
	"aura/kernel/internal/registry"
)

// chain builds client -> a -> b -> client, the shape a voice graph has
// (ears -> brain -> mouth) and the shape single-hop cancel could not reach.
func chain() *Graph {
	g := &Graph{IR: IRMajor, GraphID: "chain"}
	g.Origin.Kind = "declared"
	g.Nodes = []Node{{Ref: "a", Resolve: "logical.a"}, {Ref: "b", Resolve: "logical.b"}}
	g.Edges = []Edge{
		{From: "client.text_out", To: "a.text_in"},
		{From: "a.text_out", To: "b.text_in"},
		{From: "b.text_out", To: "client.text_in"},
	}
	return g
}

// emission is what a skill sends back after being handed `in`.
func emission(in channel.Envelope, node, port, text string) channel.Envelope {
	payload, _ := json.Marshal(map[string]any{"text": text, "final": false})
	return channel.Envelope{
		V: channel.ProtocolMajor, ID: channel.NewID(), CauseID: in.ID,
		Session: in.Session, Node: node, Port: port, Seq: 1,
		Idem: channel.NewID(), Schema: "std/text@1",
		Kind: channel.KindData, Payload: payload,
	}
}

func cancelFor(session, causeID string) channel.Envelope {
	return channel.Envelope{
		V: channel.ProtocolMajor, ID: channel.NewID(), CauseID: causeID,
		Session: session, Kind: channel.KindCancel,
	}
}

func onlyCancels(got []channel.Envelope) []channel.Envelope {
	var out []channel.Envelope
	for _, e := range got {
		if e.Kind == channel.KindCancel {
			out = append(out, e)
		}
	}
	return out
}

type chainFixture struct {
	sess     *Session
	toA      *[]channel.Envelope
	toB      *[]channel.Envelope
	toClient *[]channel.Envelope
}

func newChain(t *testing.T, sessionID string) *chainFixture {
	t.Helper()
	reg := registry.New()
	toA := liveSkill(t, reg, "acme/logical/a", "logical", "logical.a")
	toB := liveSkill(t, reg, "acme/logical/b", "logical", "logical.b")

	toClient := &[]channel.Envelope{}
	sess, err := NewSession(sessionID, chain(), reg, testStore(t), "local", DefaultPolicy(), nil,
		func(raw []byte, _ string) error {
			var env channel.Envelope
			_ = json.Unmarshal(raw, &env)
			*toClient = append(*toClient, env)
			return nil
		}, testLogger())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	return &chainFixture{sess: sess, toA: toA, toB: toB, toClient: toClient}
}

// The identity problem, which is the whole trick: a cancel must reach every
// skill in the chain, and each must be addressed with the cause_id *it* saw.
// A cancel carrying the client's own id means nothing to a skill two hops
// down — it never saw that id — so a naive broadcast silently does nothing.
func TestCancelReachesEverySkillAddressedInItsOwnCausalTerms(t *testing.T) {
	f := newChain(t, "s1")

	client := clientData("s1")
	f.sess.Route(client)

	if len(*f.toA) != 1 {
		t.Fatalf("skill a should have received the client message, got %d", len(*f.toA))
	}
	deliveredToA := (*f.toA)[0]
	if deliveredToA.CauseID != client.ID {
		t.Fatalf("a's incoming cause_id = %q, want the client message id", deliveredToA.CauseID)
	}

	// a reacts; the kernel forwards its emission to b.
	emitA := emission(deliveredToA, "a", "text_out", "partial")
	f.sess.Route(emitA)

	if len(*f.toB) != 1 {
		t.Fatalf("skill b should have received a's emission, got %d", len(*f.toB))
	}
	deliveredToB := (*f.toB)[0]
	if deliveredToB.CauseID != emitA.ID {
		t.Fatalf("b's incoming cause_id = %q, want a's emission id", deliveredToB.CauseID)
	}

	f.sess.Route(cancelFor("s1", client.ID))

	cancelsToA := onlyCancels(*f.toA)
	if len(cancelsToA) != 1 {
		t.Fatalf("skill a got %d cancel(s), want 1", len(cancelsToA))
	}
	if cancelsToA[0].CauseID != client.ID {
		t.Fatalf("a's cancel carries cause_id %q, want %q — a would ignore it",
			cancelsToA[0].CauseID, client.ID)
	}

	cancelsToB := onlyCancels(*f.toB)
	if len(cancelsToB) != 1 {
		t.Fatalf("skill b got %d cancel(s), want 1 — single-hop cancel never reached it",
			len(cancelsToB))
	}
	if cancelsToB[0].CauseID != emitA.ID {
		t.Fatalf("b's cancel carries cause_id %q, want %q (what b actually saw) — "+
			"b would ignore it", cancelsToB[0].CauseID, emitA.ID)
	}
}

// Asking a skill to stop is best-effort; suppression is not. Whatever a skill
// emits after the cancel must not travel any further, even if that skill
// never looked at the cancel at all.
func TestPostCancelEmissionsAreSuppressed(t *testing.T) {
	f := newChain(t, "s2")

	client := clientData("s2")
	f.sess.Route(client)
	deliveredToA := (*f.toA)[0]

	f.sess.Route(cancelFor("s2", client.ID))

	// a ignores the cancel entirely and keeps emitting.
	before := len(*f.toB)
	for i := 0; i < 3; i++ {
		f.sess.Route(emission(deliveredToA, "a", "text_out", "still talking"))
	}
	if len(*f.toB) != before {
		t.Fatalf("b received %d envelope(s) after the cancel; want none to get through",
			len(*f.toB)-before)
	}
	if n := len(*f.toClient); n != 0 {
		t.Fatalf("client received %d envelope(s) from a cancelled chain, want 0", n)
	}
}

// Suppression must be scoped to the cancelled chain. Cancelling one utterance
// while a second is in flight — or while a concurrent stream shares the
// session — must not silence the other. This is the property that makes
// per-root cancel worth the bookkeeping instead of a session-wide flag.
func TestCancelDoesNotAffectAConcurrentChain(t *testing.T) {
	f := newChain(t, "s3")

	first := clientData("s3")
	f.sess.Route(first)

	second := clientData("s3")
	second.Idem = "s3:second" // distinct idem, or dedup would drop it
	f.sess.Route(second)

	if len(*f.toA) != 2 {
		t.Fatalf("a should have received both client messages, got %d", len(*f.toA))
	}
	deliveredSecond := (*f.toA)[1]

	f.sess.Route(cancelFor("s3", first.ID))

	// Work on the second chain must still flow through to b.
	before := len(*f.toB)
	f.sess.Route(emission(deliveredSecond, "a", "text_out", "second chain"))
	if len(*f.toB) != before+1 {
		t.Fatal("cancelling one chain suppressed an unrelated one")
	}
}

// A client that names a mid-chain envelope — the last thing it saw, say —
// should still abandon the whole chain, not a suffix of it.
func TestCancelNamingAMidChainEnvelopeCancelsTheWholeChain(t *testing.T) {
	f := newChain(t, "s4")

	client := clientData("s4")
	f.sess.Route(client)
	deliveredToA := (*f.toA)[0]

	// Cancel naming what the kernel delivered to a, not the client's own id.
	f.sess.Route(cancelFor("s4", deliveredToA.ID))

	before := len(*f.toB)
	f.sess.Route(emission(deliveredToA, "a", "text_out", "after cancel"))
	if len(*f.toB) != before {
		t.Fatal("naming a mid-chain envelope did not cancel the chain it belongs to")
	}
}

// The causal log is the record of what happened, not of what was delivered.
// `aura why` has to be able to show that a skill kept working after a cancel.
func TestSuppressedEnvelopesAreStillRecordedInTheEventLog(t *testing.T) {
	st := testStore(t)
	reg := registry.New()
	liveSkill(t, reg, "acme/logical/a", "logical", "logical.a")
	liveSkill(t, reg, "acme/logical/b", "logical", "logical.b")

	sess, err := NewSession("s5", chain(), reg, st, "local", DefaultPolicy(), nil,
		func([]byte, string) error { return nil }, testLogger())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	client := clientData("s5")
	sess.Route(client)
	sess.Route(cancelFor("s5", client.ID))

	suppressed := channel.Envelope{
		V: channel.ProtocolMajor, ID: channel.NewID(), CauseID: client.ID,
		Session: "s5", Node: "a", Port: "text_out", Seq: 9,
		Idem: channel.NewID(), Schema: "std/text@1", Kind: channel.KindData,
		Payload: json.RawMessage(`{"text":"ignored the cancel"}`),
	}
	sess.Route(suppressed)

	events, _, err := st.SessionEvents("s5", 0)
	if err != nil {
		t.Fatalf("SessionEvents: %v", err)
	}
	found := false
	for _, raw := range events {
		var e channel.Envelope
		_ = json.Unmarshal(raw, &e)
		if e.ID == suppressed.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("a suppressed envelope is missing from the event log; " +
			"the log must record what the skill actually did")
	}
}

// A cancel for something the kernel has never seen must be a no-op, not a
// panic or a session-wide mute.
func TestCancelForAnUnknownChainIsHarmless(t *testing.T) {
	f := newChain(t, "s6")

	f.sess.Route(cancelFor("s6", channel.NewID()))

	client := clientData("s6")
	f.sess.Route(client)
	if len(*f.toA) != 1 {
		t.Fatal("an unrelated cancel suppressed a fresh chain")
	}
}
