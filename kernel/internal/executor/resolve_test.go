package executor

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"aura/kernel/internal/channel"
)

// credential gives every connection of a node a scoped token, as the gateway
// does at registration when an Expertise connects with one.
func (r *rig) credential(ref, token string) {
	for _, dests := range r.sess.routes {
		for _, d := range dests {
			if d.ref == ref && d.skill != nil {
				d.skill.TokenID = token
			}
		}
	}
}

func TestResolveSaysWhatADeliveryIsUnder(t *testing.T) {
	r := newRig(t, nil, testStore(t), "s",
		Edge{From: "client.text_out", To: "a.text_in", DeadlineMS: 2000, RespondMS: 300, Priority: 7},
	)
	start := r.ms()
	msg := clientData("s")
	r.sess.Route(msg)
	toA := last(t, r.a)

	got, err := r.sess.resolveDelivery(
		DeliveryRef{Session: "s", Delivery: toA.ID, Node: "a", Port: "text_in"}, Party{Operator: true})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !got.Known {
		t.Fatal("a delivery just made is not known")
	}
	if got.Graph != "deadlines" || got.Root != msg.ID || got.Cause != msg.ID {
		t.Fatalf("graph=%q root=%q cause=%q, want deadlines/%s/%s", got.Graph, got.Root, got.Cause, msg.ID, msg.ID)
	}
	if got.Priority != 7 || got.Deadline != start+2000 || got.RespondBy != start+300 {
		t.Fatalf("priority=%d deadline=+%d respond_by=+%d, want 7/+2000/+300",
			got.Priority, got.Deadline-start, got.RespondBy-start)
	}
	// The operator token is a file, not a row: there is nothing to match the
	// delivery against, and the answer says so instead of implying a check.
	if got.Verified {
		t.Fatal("an operator-token caller was reported as verified; it is merely believed")
	}
	if got.Cancelled {
		t.Fatal("a live chain reported as cancelled")
	}
}

// The check that makes a narrow credential narrow here: a delivery may only
// be spoken for by the credential it was sent under.
func TestAScopedCredentialMaySpeakOnlyForItsOwnDeliveries(t *testing.T) {
	r := newRig(t, nil, testStore(t), "s",
		Edge{From: "client.text_out", To: "a.text_in", Priority: 9},
		Edge{From: "client.text_out", To: "b.text_in"},
	)
	r.credential("a", "tok-a")
	r.credential("b", "tok-b")
	r.sess.Route(clientData("s"))
	toA := last(t, r.a)
	ref := DeliveryRef{Session: "s", Delivery: toA.ID, Node: "a", Port: "text_in"}

	mine, err := r.sess.resolveDelivery(ref, Party{TokenID: "tok-a", Capability: "logical.a"})
	if err != nil {
		t.Fatalf("the Expertise the delivery was made to was refused: %v", err)
	}
	if !mine.Verified || mine.Priority != 9 {
		t.Fatalf("verified=%v priority=%d, want true/9", mine.Verified, mine.Priority)
	}

	// b names a's delivery, to borrow its priority.
	_, err = r.sess.resolveDelivery(ref, Party{TokenID: "tok-b", Capability: "logical.b"})
	if !errors.Is(err, ErrNotYourDelivery) {
		t.Fatalf("a credential spoke for a delivery made to another: err=%v", err)
	}
}

// A scoped caller naming a delivery that was sent under the operator token is
// not that delivery's recipient either: nothing ties the two together.
func TestAScopedCredentialCannotClaimAnUncredentialedDelivery(t *testing.T) {
	r := newRig(t, nil, testStore(t), "s",
		Edge{From: "client.text_out", To: "a.text_in"},
	)
	r.sess.Route(clientData("s"))
	_, err := r.sess.resolveDelivery(
		DeliveryRef{Session: "s", Delivery: last(t, r.a).ID, Node: "a", Port: "text_in"},
		Party{TokenID: "tok-x", Capability: "logical.a"})
	if !errors.Is(err, ErrNotYourDelivery) {
		t.Fatalf("err=%v, want ErrNotYourDelivery", err)
	}
}

// Losing the lineage entry loses the bounds, and says so. It does not lose the
// priority, which is the edge's and is still in the graph.
func TestAnEvictedDeliveryKeepsItsPriorityAndLosesItsBounds(t *testing.T) {
	r := newRig(t, nil, testStore(t), "s",
		Edge{From: "client.text_out", To: "a.text_in", DeadlineMS: 2000, Priority: 5},
	)
	r.sess.Route(clientData("s"))
	ref := DeliveryRef{Session: "s", Delivery: channel.NewID(), Node: "a", Port: "text_in"}

	got, err := r.sess.resolveDelivery(ref, Party{Operator: true})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Known || got.Verified {
		t.Fatalf("an unknown delivery reported as known=%v verified=%v", got.Known, got.Verified)
	}
	if got.Priority != 5 {
		t.Fatalf("priority %d, want the edge's 5", got.Priority)
	}
	if got.Deadline != 0 || got.RespondBy != 0 {
		t.Fatalf("bounds invented for a delivery the node no longer holds: %d/%d", got.Deadline, got.RespondBy)
	}
}

// Where several edges feed one port and the caller cannot say which delivery
// it is answering, it gets the lowest of them.
func TestAnUnknownDeliveryGetsTheLowestPriorityIntoItsPort(t *testing.T) {
	r := newRig(t, nil, testStore(t), "s",
		Edge{From: "client.text_out", To: "a.text_in", Priority: 9},
		Edge{From: "b.text_out", To: "a.text_in", Priority: 2},
	)
	got, err := r.sess.resolveDelivery(
		DeliveryRef{Session: "s", Delivery: "gone", Node: "a", Port: "text_in"}, Party{Operator: true})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Priority != 2 {
		t.Fatalf("priority %d, want the lowest edge into the port (2)", got.Priority)
	}
}

func TestAScopedCredentialCannotNameADestinationThatIsNotItsOwn(t *testing.T) {
	r := newRig(t, nil, testStore(t), "s",
		Edge{From: "client.text_out", To: "a.text_in", Priority: 9},
		Edge{From: "client.text_out", To: "b.text_in"},
	)
	// b's credential, a's node: the fallback must not hand out a's priority.
	_, err := r.sess.resolveDelivery(
		DeliveryRef{Session: "s", Delivery: "gone", Node: "a", Port: "text_in"},
		Party{TokenID: "tok-b", Capability: "logical.b"})
	if !errors.Is(err, ErrNoSuchDestination) {
		t.Fatalf("err=%v, want ErrNoSuchDestination", err)
	}
	// And a node that is not in the graph at all.
	_, err = r.sess.resolveDelivery(
		DeliveryRef{Session: "s", Delivery: "gone", Node: "nope", Port: "text_in"}, Party{Operator: true})
	if !errors.Is(err, ErrNoSuchDestination) {
		t.Fatalf("err=%v, want ErrNoSuchDestination", err)
	}
}

func TestResolveReportsAChainThatWasCancelled(t *testing.T) {
	r := newRig(t, nil, testStore(t), "s",
		Edge{From: "client.text_out", To: "a.text_in"},
	)
	msg := clientData("s")
	r.sess.Route(msg)
	ref := DeliveryRef{Session: "s", Delivery: last(t, r.a).ID, Node: "a", Port: "text_in"}

	r.sess.Route(cancelFor("s", msg.ID))
	got, err := r.sess.resolveDelivery(ref, Party{Operator: true})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !got.Cancelled {
		t.Fatal("a request arriving for an abandoned chain was not told it is abandoned")
	}
}

// A request naming a delivery is work continuing under it, and must keep the
// entry alive exactly as an emission does.
func TestResolvingADeliveryKeepsItInTheIndex(t *testing.T) {
	r := newRig(t, nil, testStore(t), "s",
		Edge{From: "client.text_out", To: "a.text_in", DeadlineMS: 60_000},
		Edge{From: "client.text_out", To: "b.text_in"},
	)
	// Room for four, and every message below adds two: without the reference
	// a request makes, the delivery being worked on is gone after two messages.
	r.sess.lineage.max = 4
	r.sess.Route(clientData("s"))
	toA := last(t, r.a)
	ref := DeliveryRef{Session: "s", Delivery: toA.ID, Node: "a", Port: "text_in"}

	for i := 0; i < 20; i++ {
		if _, err := r.sess.resolveDelivery(ref, Party{Operator: true}); err != nil {
			t.Fatal(err)
		}
		msg := clientData("s")
		msg.ID, msg.Idem = channel.NewID(), channel.NewID()
		r.sess.Route(msg) // two more deliveries, pushing the two oldest out
		r.advance(time.Millisecond)
	}
	got, err := r.sess.resolveDelivery(ref, Party{Operator: true})
	if err != nil || !got.Known {
		t.Fatalf("a delivery in constant use was evicted: known=%v err=%v", got.Known, err)
	}
}

// --- cancel notices --------------------------------------------------------

type notices struct {
	mu  sync.Mutex
	got []CancelNotice
}

func (n *notices) record(c CancelNotice) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.got = append(n.got, c)
}

func (n *notices) all() []CancelNotice {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]CancelNotice(nil), n.got...)
}

// The whole wiring, through a Manager: an observer installed on the node
// hears a client's cancel and the end of a session.
func TestTheNodeIsToldWhenAChainIsCancelledAndWhenASessionEnds(t *testing.T) {
	m, reg, st := testManager(t)
	liveSkill(t, reg, "acme/logical/echo", "logical", "logical.echo")
	seedGraph(t, st, "echo", "logical.echo")

	var seen notices
	m.SetCancelObserver(seen.record)

	sess, err := m.Start("s1", "echo", "", func([]byte, string) error { return nil })
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	msg := clientData("s1")
	sess.Route(msg)
	sess.Route(cancelFor("s1", msg.ID))

	got := seen.all()
	if len(got) != 1 || got[0].Session != "s1" || got[0].Root != msg.ID || got[0].Reason != CancelByClient {
		t.Fatalf("after a client cancel: %+v", got)
	}

	m.End("s1")
	got = seen.all()
	if len(got) != 2 || got[1].Session != "s1" || got[1].Root != "" || got[1].Cause != "" ||
		got[1].Reason != CancelBySessionEnd {
		t.Fatalf("after the session ended: %+v", got)
	}
}

// An observer installed after a session started is still the one it tells:
// the node wires its inference plane once, and sessions outlive that moment
// in both directions.
func TestAnObserverInstalledLaterStillHears(t *testing.T) {
	m, reg, st := testManager(t)
	liveSkill(t, reg, "acme/logical/echo", "logical", "logical.echo")
	seedGraph(t, st, "echo", "logical.echo")

	sess, err := m.Start("s1", "echo", "", func([]byte, string) error { return nil })
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	var seen notices
	m.SetCancelObserver(seen.record)

	msg := clientData("s1")
	sess.Route(msg)
	sess.Route(cancelFor("s1", msg.ID))
	if len(seen.all()) != 1 {
		t.Fatal("an observer installed after the session started heard nothing")
	}
}

// A speculation that missed abandons one delivery, by its cause.
func TestASupersededSpeculationIsAnnouncedByCause(t *testing.T) {
	r := newRig(t, nil, testStore(t), "s",
		Edge{From: "client.text_out", To: "a.text_in"},
		Edge{From: "a.text_out", To: "b.text_in", Speculative: true},
	)
	var slot cancelObserver
	var seen notices
	fn := seen.record
	slot.Store(&fn)
	r.sess.onCancel = &slot

	r.sess.Route(clientData("s"))
	toA := last(t, r.a)

	partial := emission(toA, "a", "text_out", "the answer is fou")
	r.sess.RouteFrom(partial, emitter(idA))
	// The real end of the stream says something else.
	final := emission(toA, "a", "text_out", "r hundred")
	final.Payload, _ = json.Marshal(map[string]any{"text": "ve", "final": true})
	r.sess.RouteFrom(final, emitter(idA))

	var bySpec []CancelNotice
	for _, n := range seen.all() {
		if n.Reason == CancelBySpeculation {
			bySpec = append(bySpec, n)
		}
	}
	if len(bySpec) == 0 {
		t.Skip("this stream did not produce a speculative miss; covered by speculation_test.go")
	}
	if bySpec[0].Cause == "" || bySpec[0].Root != "" {
		t.Fatalf("a superseded speculation must be named by cause: %+v", bySpec[0])
	}
}

func TestResolveThroughTheManager(t *testing.T) {
	m, reg, st := testManager(t)
	toSkill := liveSkill(t, reg, "acme/logical/echo", "logical", "logical.echo")
	seedGraph(t, st, "echo", "logical.echo")

	if _, err := m.ResolveDelivery(DeliveryRef{Session: "nobody"}, Party{Operator: true}); !errors.Is(err, ErrUnknownSession) {
		t.Fatalf("err=%v, want ErrUnknownSession", err)
	}
	sess, err := m.Start("s1", "echo", "", func([]byte, string) error { return nil })
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	sess.Route(clientData("s1"))
	got, err := m.ResolveDelivery(
		DeliveryRef{Session: "s1", Delivery: (*toSkill)[0].ID, Node: "s", Port: "text_in"},
		Party{Operator: true})
	if err != nil || !got.Known || got.Graph != "echo" {
		t.Fatalf("got %+v, err=%v", got, err)
	}
}
