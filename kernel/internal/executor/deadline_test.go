package executor

import (
	"encoding/json"
	"testing"
	"time"

	"aura/kernel/internal/channel"
	"aura/kernel/internal/ledger"
	"aura/kernel/internal/registry"
	"aura/kernel/internal/spec"
	"aura/kernel/internal/store"
)

// These tests are the contract for C2 rules 7 and 9 as the kernel now
// implements them. Before delivery lineage existed, no executor test mentioned
// a deadline at all — which is how "deadlines are absolute and inherited"
// could be written in the spec and the README while a deadline died at the
// first Expertise it reached.

const (
	idA = "acme/logical/a"
	idB = "acme/logical/b"
	idW = "acme/motor/writer"
)

// rig is a session wired over three Expertise — a and b (logical), w (motor)
// — with a clock the test moves by hand.
type rig struct {
	sess     *Session
	st       *store.Store
	a, b, w  *[]channel.Envelope
	toClient *[]channel.Envelope
	clock    *time.Time
}

func (r *rig) advance(d time.Duration) { *r.clock = r.clock.Add(d) }
func (r *rig) ms() int64               { return r.clock.UnixMilli() }

// emitter is who a registered test Expertise is to the executor: the identity
// the gateway derives from a connection's registration.
func emitter(id string) Emitter { return Emitter{Conn: "conn-" + id, Expertise: id} }

func newRig(t *testing.T, ldg *ledger.Ledger, st *store.Store, sessionID string, edges ...Edge) *rig {
	t.Helper()
	reg := registry.New()
	r := &rig{st: st}
	r.a = liveSkill(t, reg, idA, "logical", "logical.a")
	r.b = liveSkill(t, reg, idB, "logical", "logical.b")
	r.w = liveSkill(t, reg, idW, "motor", "motor.api.writer")

	g := &Graph{IR: IRMajor, GraphID: "deadlines"}
	g.Origin.Kind = "declared"
	g.Nodes = []Node{
		{Ref: "a", Resolve: "logical.a"},
		{Ref: "b", Resolve: "logical.b"},
		{Ref: "w", Resolve: "motor.api.writer"},
	}
	g.Edges = edges

	toClient := []channel.Envelope{}
	r.toClient = &toClient
	sess, err := NewSession(sessionID, g, reg, st, "local", DefaultPolicy(), ldg, "",
		func(raw []byte, _ string) error {
			var env channel.Envelope
			_ = json.Unmarshal(raw, &env)
			*r.toClient = append(*r.toClient, env)
			return nil
		}, testLogger())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	clock := time.UnixMilli(1_800_000_000_000)
	r.clock = &clock
	sess.now = func() time.Time { return *r.clock }
	r.sess = sess
	return r
}

func last(t *testing.T, got *[]channel.Envelope) channel.Envelope {
	t.Helper()
	if len(*got) == 0 {
		t.Fatal("nothing was delivered")
	}
	return (*got)[len(*got)-1]
}

func statusEvents(got []channel.Envelope, event string) int {
	n := 0
	for _, e := range got {
		if e.Kind != channel.KindStatus {
			continue
		}
		var body struct {
			Event string `json:"event"`
		}
		_ = json.Unmarshal(e.Payload, &body)
		if body.Event == event {
			n++
		}
	}
	return n
}

// The bug this whole change exists for. The deadline is declared on the first
// edge; the Expertise in the middle emits an envelope with no deadline on it,
// as every SDK does; the delivery after it must still be under that deadline.
func TestADeadlineIsInheritedAcrossAnExpertise(t *testing.T) {
	r := newRig(t, nil, testStore(t), "s",
		Edge{From: "client.text_out", To: "a.text_in", DeadlineMS: 1000},
		Edge{From: "a.text_out", To: "b.text_in"},
	)
	r.sess.Route(clientData("s"))
	toA := last(t, r.a)
	if want := r.ms() + 1000; toA.Deadline != want {
		t.Fatalf("delivery to a carries deadline %d, want %d", toA.Deadline, want)
	}

	r.advance(300 * time.Millisecond)
	r.sess.RouteFrom(emission(toA, "a", "text_out", "x"), emitter(idA))

	toB := last(t, r.b)
	if toB.Deadline != toA.Deadline {
		t.Fatalf("delivery to b carries deadline %d; the chain was given %d and an Expertise "+
			"in the middle must not be where a budget is lost", toB.Deadline, toA.Deadline)
	}
}

// The other half of "inherited, never extended". The Expertise used to carry
// its own budget on what it emitted, so it could have written a later one.
func TestAnExpertiseCannotExtendItsDeadline(t *testing.T) {
	r := newRig(t, nil, testStore(t), "s",
		Edge{From: "client.text_out", To: "a.text_in", DeadlineMS: 1000},
		Edge{From: "a.text_out", To: "b.text_in"},
	)
	r.sess.Route(clientData("s"))
	toA := last(t, r.a)

	out := emission(toA, "a", "text_out", "x")
	out.Deadline = toA.Deadline + 3_600_000 // "I would like another hour"
	r.sess.RouteFrom(out, emitter(idA))

	if got := last(t, r.b).Deadline; got != toA.Deadline {
		t.Fatalf("an Expertise moved its own deadline from %d to %d", toA.Deadline, got)
	}
}

// A tighter budget downstream is honoured; a looser one is not.
func TestADownstreamEdgeMayTightenAndNeverLoosen(t *testing.T) {
	for _, tc := range []struct {
		name      string
		secondMS  int
		wantAfter int64 // relative to the first delivery
	}{
		{"tighter wins", 200, 200},
		{"looser is ignored", 5000, 1000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, nil, testStore(t), "s",
				Edge{From: "client.text_out", To: "a.text_in", DeadlineMS: 1000},
				Edge{From: "a.text_out", To: "b.text_in", DeadlineMS: tc.secondMS},
			)
			start := r.ms()
			r.sess.Route(clientData("s"))
			r.sess.RouteFrom(emission(last(t, r.a), "a", "text_out", "x"), emitter(idA))
			if got := last(t, r.b).Deadline; got != start+tc.wantAfter {
				t.Fatalf("deadline is start+%d, want start+%d", got-start, tc.wantAfter)
			}
		})
	}
}

// Late data is dropped; what closes a stream is not. A consumer waiting
// forever is worse than a late answer (C2 rule 7).
func TestLateDataIsDroppedAndTerminalsStillTravel(t *testing.T) {
	r := newRig(t, nil, testStore(t), "s",
		Edge{From: "client.text_out", To: "a.text_in", DeadlineMS: 1000},
		Edge{From: "a.text_out", To: "b.text_in"},
	)
	r.sess.Route(clientData("s"))
	toA := last(t, r.a)

	r.advance(1500 * time.Millisecond)
	r.sess.RouteFrom(emission(toA, "a", "text_out", "too late"), emitter(idA))
	r.sess.RouteFrom(emission(toA, "a", "text_out", "also late"), emitter(idA))
	if len(*r.b) != 0 {
		t.Fatalf("b was handed %d delivery(ies) after the chain's deadline", len(*r.b))
	}
	if got := r.sess.deadlines.dropped.Load(); got != 2 {
		t.Fatalf("dropped counter is %d, want 2", got)
	}
	// Explained once, not once per dropped token.
	if n := statusEvents(*r.toClient, "deadline_exceeded"); n != 1 {
		t.Fatalf("the client was told %d time(s) that late output is being dropped, want 1", n)
	}

	done := emission(toA, "a", "text_out", "")
	done.Kind = channel.KindDone
	r.sess.RouteFrom(done, emitter(idA))
	if len(*r.b) != 1 || (*r.b)[0].Kind != channel.KindDone {
		t.Fatalf("the stream's `done` did not reach b; a consumer would wait forever")
	}
}

// The regression the first attempt at this would have introduced. A producer
// streams for seconds; the edge after it gives each item 200ms. Every item is
// under its own budget, counted from when *it* was produced. Keyed by the
// chain's root instead, the first item's instant would have condemned every
// one after it.
func TestAStreamUnderAPerItemBudgetIsNotTruncated(t *testing.T) {
	r := newRig(t, nil, testStore(t), "s",
		Edge{From: "client.text_out", To: "a.text_in"},
		Edge{From: "a.text_out", To: "b.text_in", DeadlineMS: 200},
	)
	r.sess.Route(clientData("s"))
	toA := last(t, r.a)

	for i := 0; i < 5; i++ {
		r.advance(time.Second) // far longer than any one item's budget
		r.sess.RouteFrom(emission(toA, "a", "text_out", "token"), emitter(idA))
		if len(*r.b) != i+1 {
			t.Fatalf("item %d of a stream was dropped: each is under its own 200ms", i+1)
		}
		if want := r.ms() + 200; last(t, r.b).Deadline != want {
			t.Fatalf("item %d carries deadline %d, want its own %d", i+1, last(t, r.b).Deadline, want)
		}
	}
}

// A client that starts a chain may put a bound on it, and it is kept.
func TestAClientSuppliedDeadlineIsKept(t *testing.T) {
	r := newRig(t, nil, testStore(t), "s",
		Edge{From: "client.text_out", To: "a.text_in"},
		Edge{From: "a.text_out", To: "b.text_in"},
	)
	msg := clientData("s")
	msg.Deadline = r.ms() + 750
	r.sess.Route(msg)
	if got := last(t, r.a).Deadline; got != msg.Deadline {
		t.Fatalf("delivery carries %d, want the client's own %d", got, msg.Deadline)
	}
	r.sess.RouteFrom(emission(last(t, r.a), "a", "text_out", "x"), emitter(idA))
	if got := last(t, r.b).Deadline; got != msg.Deadline {
		t.Fatalf("the client's deadline was lost one hop in: %d", got)
	}
}

// The order bug. forward used to seal the effect and *then* notice it was
// late, which would have left a ledger entry saying `delivered`, with a
// receipt, for an act that never happened.
func TestAnExpiredEffectIsSealedAsExpiredAndNeverAsDelivered(t *testing.T) {
	ldg, st := testLedgerForSession(t)
	r := newRig(t, ldg, st, "s",
		Edge{From: "client.text_out", To: "a.text_in", DeadlineMS: 100},
		Edge{From: "a.text_out", To: "w.text_in", Gate: GateNone},
	)
	r.sess.Route(clientData("s"))
	toA := last(t, r.a)

	r.advance(500 * time.Millisecond)
	r.sess.RouteFrom(emission(toA, "a", "text_out", "move the arm"), emitter(idA))

	if len(*r.w) != 0 {
		t.Fatal("a motor Expertise was handed an effect after its deadline")
	}
	entries := allEntries(t, st)
	if len(entries) != 1 {
		t.Fatalf("ledger holds %d entries, want exactly 1", len(entries))
	}
	if entries[0].Outcome != spec.OutcomeExpired {
		t.Fatalf("an effect that was not delivered is sealed as %q", entries[0].Outcome)
	}
	if got := r.sess.deadlines.effectsExpired.Load(); got != 1 {
		t.Fatalf("effects_expired counter is %d, want 1", got)
	}
}

// In time, the same effect is delivered and sealed as it always was.
func TestAnEffectInsideItsDeadlineIsDeliveredAndSealed(t *testing.T) {
	ldg, st := testLedgerForSession(t)
	r := newRig(t, ldg, st, "s",
		Edge{From: "client.text_out", To: "a.text_in", DeadlineMS: 1000},
		Edge{From: "a.text_out", To: "w.text_in", Gate: GateNone},
	)
	r.sess.Route(clientData("s"))
	r.advance(50 * time.Millisecond)
	r.sess.RouteFrom(emission(last(t, r.a), "a", "text_out", "move the arm"), emitter(idA))

	got := last(t, r.w)
	if got.Receipt == "" {
		t.Fatal("a delivered effect carries no receipt")
	}
	entries := allEntries(t, st)
	if len(entries) != 1 || entries[0].Outcome != spec.OutcomeDelivered {
		t.Fatalf("ledger: %+v", entries)
	}
}

func approve(sess *Session, req channel.Envelope, yes bool) {
	payload, _ := json.Marshal(map[string]bool{"approve": yes})
	sess.Route(channel.Envelope{
		V: channel.ProtocolMajor, ID: channel.NewID(), CauseID: req.ID,
		Session: sess.ID, Kind: channel.KindConfirmResponse, Payload: payload,
	})
}

func confirmRequest(t *testing.T, got []channel.Envelope) channel.Envelope {
	t.Helper()
	for _, e := range got {
		if e.Kind == channel.KindConfirmRequest {
			return e
		}
	}
	t.Fatal("no confirm_request reached the client")
	return channel.Envelope{}
}

// A human's yes does not bring a stale act back. The deadline says when the
// act stops being valid; an approval that arrives after it approves something
// that should no longer happen. It is recorded — as expired, with the gate —
// and the approver is told.
func TestAnApprovalAfterTheDeadlineDoesNotDeliver(t *testing.T) {
	ldg, st := testLedgerForSession(t)
	r := newRig(t, ldg, st, "s",
		Edge{From: "client.text_out", To: "a.text_in", DeadlineMS: 5000},
		Edge{From: "a.text_out", To: "w.text_in"}, // motor, so gated by the node
	)
	r.sess.Route(clientData("s"))
	r.advance(100 * time.Millisecond)
	r.sess.RouteFrom(emission(last(t, r.a), "a", "text_out", "unlock the door"), emitter(idA))

	req := confirmRequest(t, *r.toClient)
	var q struct {
		ExpiresAt int64 `json:"expires_at"`
	}
	_ = json.Unmarshal(req.Payload, &q)
	if q.ExpiresAt == 0 {
		t.Fatal("the approval request does not say when the answer stops mattering")
	}

	r.advance(10 * time.Minute)
	approve(r.sess, req, true)

	if len(*r.w) != 0 {
		t.Fatal("an effect was carried out on an approval that arrived after its deadline")
	}
	entries := allEntries(t, st)
	if len(entries) != 1 || entries[0].Outcome != spec.OutcomeExpired ||
		entries[0].Decision != spec.DecisionGate {
		t.Fatalf("want one gate/expired entry, got %+v", entries)
	}
	var told bool
	for _, e := range *r.toClient {
		if e.Kind == channel.KindError {
			told = true
		}
	}
	if !told {
		t.Fatal("the approver was never told their approval did not result in the act")
	}
}

// And the budget of a gated edge is spent while it is held: it does not start
// again when the human answers.
func TestAHeldDeliveryKeepsTheBudgetItWasHeldUnder(t *testing.T) {
	r := newRig(t, nil, testStore(t), "s",
		Edge{From: "client.text_out", To: "a.text_in"},
		Edge{From: "a.text_out", To: "w.text_in", DeadlineMS: 30_000},
	)
	r.sess.Route(clientData("s"))
	heldAt := r.ms()
	r.sess.RouteFrom(emission(last(t, r.a), "a", "text_out", "x"), emitter(idA))
	req := confirmRequest(t, *r.toClient)

	r.advance(10 * time.Second)
	approve(r.sess, req, true)
	if want := heldAt + 30_000; last(t, r.w).Deadline != want {
		t.Fatalf("a delivery released after 10s carries deadline held+%dms, want held+30000ms",
			last(t, r.w).Deadline-heldAt)
	}
}

// respond_by is a bound on the front of a reply. The first output carries it
// on; everything behind the front is under no responsiveness target.
func TestRespondByTravelsWithTheFrontOnly(t *testing.T) {
	r := newRig(t, nil, testStore(t), "s",
		Edge{From: "client.text_out", To: "a.text_in", RespondMS: 300},
		Edge{From: "a.text_out", To: "b.text_in"},
	)
	r.sess.Route(clientData("s"))
	toA := last(t, r.a)
	if want := r.ms() + 300; toA.RespondBy != want {
		t.Fatalf("delivery to a carries respond_by %d, want %d", toA.RespondBy, want)
	}

	r.advance(100 * time.Millisecond)
	r.sess.RouteFrom(emission(toA, "a", "text_out", "first"), emitter(idA))
	if got := last(t, r.b).RespondBy; got != toA.RespondBy {
		t.Fatalf("the front of the reply carries respond_by %d, want %d", got, toA.RespondBy)
	}
	r.sess.RouteFrom(emission(toA, "a", "text_out", "second"), emitter(idA))
	if got := last(t, r.b).RespondBy; got != 0 {
		t.Fatalf("an output behind the front still carries respond_by %d", got)
	}
}

// The difference between the two bounds, in one test: a reply that starts
// late is recorded as late and delivered in full.
func TestALateResponseIsRecordedAndNothingIsDropped(t *testing.T) {
	st := testStore(t)
	r := newRig(t, nil, st, "s",
		Edge{From: "client.text_out", To: "a.text_in", RespondMS: 300},
		Edge{From: "a.text_out", To: "client.text_in"},
	)
	r.sess.Route(clientData("s"))
	toA := last(t, r.a)

	r.advance(2 * time.Second)
	for _, text := range []string{"sorry", "for", "the", "wait"} {
		r.sess.RouteFrom(emission(toA, "a", "text_out", text), emitter(idA))
	}

	var data int
	for _, e := range *r.toClient {
		if e.Kind == channel.KindData {
			data++
		}
	}
	if data != 4 {
		t.Fatalf("the client received %d of 4 outputs; a late response must not be cut", data)
	}
	if got := r.sess.deadlines.lateResponses.Load(); got != 1 {
		t.Fatalf("late_responses counter is %d, want 1 (one reply, late once)", got)
	}
	if got := r.sess.deadlines.dropped.Load(); got != 0 {
		t.Fatalf("%d delivery(ies) dropped for missing a response target", got)
	}
	// And it is in the causal log, beside what was late.
	events, _, err := st.SessionEvents("s", 0)
	if err != nil {
		t.Fatal(err)
	}
	var logged []channel.Envelope
	for _, raw := range events {
		var e channel.Envelope
		_ = json.Unmarshal(raw, &e)
		logged = append(logged, e)
	}
	if n := statusEvents(logged, "response_late"); n != 1 {
		t.Fatalf("the causal log records %d late response(s), want 1", n)
	}
}

// Both bounds on one delivery, independently.
func TestADeliveryMayBeUnderBothBounds(t *testing.T) {
	r := newRig(t, nil, testStore(t), "s",
		Edge{From: "client.text_out", To: "a.text_in", DeadlineMS: 10_000, RespondMS: 300},
		Edge{From: "a.text_out", To: "b.text_in"},
	)
	start := r.ms()
	r.sess.Route(clientData("s"))
	toA := last(t, r.a)
	if toA.Deadline != start+10_000 || toA.RespondBy != start+300 {
		t.Fatalf("delivery carries deadline +%d and respond_by +%d, want +10000 and +300",
			toA.Deadline-start, toA.RespondBy-start)
	}
	// Late to respond, well inside validity: delivered, and still bound.
	r.advance(time.Second)
	r.sess.RouteFrom(emission(toA, "a", "text_out", "x"), emitter(idA))
	if got := last(t, r.b); got.Deadline != start+10_000 {
		t.Fatalf("validity bound lost: %d", got.Deadline)
	}
	// Past validity: dropped, whatever the response target said.
	r.advance(20 * time.Second)
	before := len(*r.b)
	r.sess.RouteFrom(emission(toA, "a", "text_out", "y"), emitter(idA))
	if len(*r.b) != before {
		t.Fatal("a delivery past its deadline was made")
	}
}

// An Expertise names the cause of what it emits. The kernel now checks that
// the cause is something that Expertise was actually given.
func TestAnEmissionUnderSomeoneElsesDeliveryIsNotBelieved(t *testing.T) {
	r := newRig(t, nil, testStore(t), "s",
		Edge{From: "client.text_out", To: "a.text_in", DeadlineMS: 60_000},
		Edge{From: "client.text_out", To: "b.text_in"},
		Edge{From: "b.text_out", To: "client.text_in"},
	)
	r.sess.Route(clientData("s"))
	toA := last(t, r.a)

	// b answers, claiming the delivery that was made to a.
	r.sess.RouteFrom(emission(toA, "b", "text_out", "x"), emitter(idB))

	if got := r.sess.lineage.stats.foreign.Load(); got != 1 {
		t.Fatalf("foreign-link counter is %d, want 1", got)
	}
	var out channel.Envelope
	for _, e := range *r.toClient {
		if e.Kind == channel.KindData {
			out = e
		}
	}
	if out.Deadline != 0 {
		t.Fatalf("an emission inherited a deadline (%d) from a delivery it was never given", out.Deadline)
	}
}

func TestAnEmissionWithAnUnknownCauseInheritsNothing(t *testing.T) {
	r := newRig(t, nil, testStore(t), "s",
		Edge{From: "client.text_out", To: "a.text_in", DeadlineMS: 60_000},
		Edge{From: "a.text_out", To: "b.text_in"},
	)
	r.sess.Route(clientData("s"))
	toA := last(t, r.a)

	bogus := emission(toA, "a", "text_out", "x")
	bogus.CauseID = channel.NewID()
	r.sess.RouteFrom(bogus, emitter(idA))

	if got := r.sess.lineage.stats.unverified.Load(); got != 1 {
		t.Fatalf("unverified-link counter is %d, want 1", got)
	}
}

// A process that drops and reconnects mid-reply is still the Expertise that
// was given the work. Identity is the manifest and the credential, never the
// connection.
func TestAReconnectedExpertiseKeepsItsWork(t *testing.T) {
	r := newRig(t, nil, testStore(t), "s",
		Edge{From: "client.text_out", To: "a.text_in", DeadlineMS: 1000},
		Edge{From: "a.text_out", To: "b.text_in"},
	)
	r.sess.Route(clientData("s"))
	toA := last(t, r.a)

	reconnected := Emitter{Conn: "a-brand-new-socket", Expertise: idA}
	r.sess.RouteFrom(emission(toA, "a", "text_out", "x"), reconnected)

	if got := last(t, r.b).Deadline; got != toA.Deadline {
		t.Fatalf("a reconnect cost the chain its deadline: %d", got)
	}
	if got := r.sess.lineage.stats.unverified.Load() + r.sess.lineage.stats.foreign.Load(); got != 0 {
		t.Fatalf("a reconnected Expertise was treated as a stranger (%d bad link(s))", got)
	}
}

// With scoped credentials, a second provider of the same Expertise is a
// different party.
func TestADifferentCredentialIsADifferentParty(t *testing.T) {
	d := delivery{expertise: idA, token: "tok-1"}
	if !d.madeTo(Emitter{Expertise: idA, TokenID: "tok-1"}) {
		t.Fatal("the party a delivery was made to is not recognised")
	}
	if d.madeTo(Emitter{Expertise: idA, TokenID: "tok-2"}) {
		t.Fatal("a different credential was accepted as the same party")
	}
	if d.madeTo(Emitter{Expertise: idB, TokenID: "tok-1"}) {
		t.Fatal("a different Expertise was accepted as the same party")
	}
	// The operator token is a file, not a row: there is nothing to compare,
	// and the manifest is all that can be checked.
	if !d.madeTo(Emitter{Expertise: idA}) {
		t.Fatal("an operator-token connection of the same Expertise was refused")
	}
}

// A pinned node stands in for a real one, inside the chain's budget.
func TestAPinnedNodeSitsInsideTheChainsBudget(t *testing.T) {
	r := newRig(t, nil, testStore(t), "s",
		Edge{From: "client.text_out", To: "a.text_in", DeadlineMS: 1000},
		Edge{From: "a.text_out", To: "b.text_in"},
	)
	if err := r.sess.SetPins(map[string]Pin{"a": textPin("pinned")}, "local"); err != nil {
		t.Fatalf("SetPins: %v", err)
	}
	start := r.ms()
	r.sess.Route(clientData("s"))
	if len(*r.a) != 0 {
		t.Fatal("a pinned node was dispatched to")
	}
	if got := last(t, r.b).Deadline; got != start+1000 {
		t.Fatalf("the delivery after a pinned node carries deadline start+%d, want start+1000", got-start)
	}
}

func TestNegativeRespondMSIsRefused(t *testing.T) {
	raw := []byte(`{"ir":"1","graph_id":"g","origin":{"kind":"declared"},
		"nodes":[{"ref":"a","resolve":"logical.a"}],
		"edges":[{"from":"client.text_out","to":"a.text_in","respond_ms":-1}]}`)
	if _, err := ParseGraph(raw); err == nil {
		t.Fatal("a negative respond_ms was accepted")
	}
}
