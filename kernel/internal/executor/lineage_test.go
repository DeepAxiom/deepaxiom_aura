package executor

import (
	"fmt"
	"testing"
	"time"

	"aura/kernel/internal/channel"
)

func TestLineageEvictsTheLeastRecentlyReferenced(t *testing.T) {
	stats := &lineageCounters{}
	idx := newLineageIndex(3, stats)
	for _, id := range []string{"d1", "d2", "d3"} {
		idx.record(delivery{id: id})
	}
	// d1 is the oldest, and it is the one still being answered.
	if _, ok := idx.touch("d1"); !ok {
		t.Fatal("d1 missing before any eviction")
	}
	idx.record(delivery{id: "d4"})

	if _, ok := idx.peek("d1"); !ok {
		t.Fatal("a delivery that was just referenced was evicted; age is the wrong rule")
	}
	if _, ok := idx.peek("d2"); ok {
		t.Fatal("d2 was the least recently referenced and should be the one gone")
	}
	if got := stats.evicted.Load(); got != 1 {
		t.Fatalf("evicted counter is %d, want 1", got)
	}
	if idx.len() != 3 {
		t.Fatalf("index holds %d entries, bound is 3", idx.len())
	}
}

func TestLineagePeekDoesNotCountAsAReference(t *testing.T) {
	idx := newLineageIndex(2, nil)
	idx.record(delivery{id: "d1"})
	idx.record(delivery{id: "d2"})
	idx.peek("d1") // looking is not using
	idx.record(delivery{id: "d3"})
	if _, ok := idx.peek("d1"); ok {
		t.Fatal("peek kept an entry alive")
	}
}

func TestLineageFrontIsTakenExactlyOnce(t *testing.T) {
	idx := newLineageIndex(4, nil)
	idx.record(delivery{id: "d1", respondBy: 42})
	if !idx.takeFront("d1") {
		t.Fatal("the first output was not recognised as the front")
	}
	if idx.takeFront("d1") {
		t.Fatal("a second output was also treated as the front")
	}
	if idx.takeFront("never-delivered") {
		t.Fatal("an unknown delivery has no front to take")
	}
}

func TestLineageReRecordingReplaces(t *testing.T) {
	stats := &lineageCounters{}
	idx := newLineageIndex(4, stats)
	idx.record(delivery{id: "d1", validUntil: 1})
	idx.record(delivery{id: "d1", validUntil: 2})
	got, _ := idx.peek("d1")
	if got.validUntil != 2 || idx.len() != 1 {
		t.Fatalf("re-recording did not replace: %+v, %d entries", got, idx.len())
	}
	if n := stats.recorded.Load(); n != 1 {
		t.Fatalf("recorded counter is %d, want 1", n)
	}
}

// The case oldest-first eviction gets wrong, at the level it matters. One
// delivery into a; a streams, and every token fans out as a delivery into b.
// The delivery a is answering is the oldest entry in the index and the only
// one that must not be forgotten — forget it and the rest of a's reply
// inherits no deadline and can never be verified again.
func TestALongReplyKeepsItsOwnDeliveryAlive(t *testing.T) {
	r := newRig(t, nil, testStore(t), "s",
		Edge{From: "client.text_out", To: "a.text_in", DeadlineMS: 600_000},
		Edge{From: "a.text_out", To: "b.text_in"},
	)
	r.sess.lineage.max = 4 // far smaller than the reply is long

	r.sess.Route(clientData("s"))
	toA := last(t, r.a)
	for i := 0; i < 200; i++ {
		r.advance(10 * time.Millisecond)
		r.sess.RouteFrom(emission(toA, "a", "text_out", fmt.Sprintf("t%d", i)), emitter(idA))
	}

	if len(*r.b) != 200 {
		t.Fatalf("b received %d of 200 tokens", len(*r.b))
	}
	if got := last(t, r.b).Deadline; got != toA.Deadline {
		t.Fatalf("the 200th token lost the chain's deadline (%d): the delivery being "+
			"answered was evicted by its own output", got)
	}
	if got := r.sess.lineage.stats.unverified.Load(); got != 0 {
		t.Fatalf("%d emission(s) could not be verified against a delivery still in use", got)
	}
}

// --- resume ----------------------------------------------------------------

// A restart must not be where a chain loses its budget. Everything the
// lineage needs is on the envelopes the log already holds.
func TestResumeRebuildsLineageAndBounds(t *testing.T) {
	st := testStore(t)
	edges := []Edge{
		{From: "client.text_out", To: "a.text_in", DeadlineMS: 60_000},
		{From: "a.text_out", To: "b.text_in"},
	}
	r1 := newRig(t, nil, st, "s-resume-lineage", edges...)
	r1.sess.Route(clientData("s-resume-lineage"))
	toA := last(t, r1.a)

	// The process is gone. A new one comes up on the same data directory, and
	// the Expertise — which never noticed — finishes its answer.
	r2 := newRig(t, nil, st, "s-resume-lineage", edges...)
	r2.sess.RouteFrom(emission(toA, "a", "text_out", "x"), emitter(idA))

	if got := last(t, r2.b).Deadline; got != toA.Deadline {
		t.Fatalf("after a restart the chain's deadline is %d, want %d", got, toA.Deadline)
	}
	if got := r2.sess.lineage.stats.unverified.Load(); got != 0 {
		t.Fatalf("a delivery made before the restart was not recognised after it")
	}
}

func TestResumeRemembersThatTheFrontAlreadyWentOut(t *testing.T) {
	st := testStore(t)
	edges := []Edge{
		{From: "client.text_out", To: "a.text_in", RespondMS: 300},
		{From: "a.text_out", To: "b.text_in"},
	}
	r1 := newRig(t, nil, st, "s-resume-front", edges...)
	r1.sess.Route(clientData("s-resume-front"))
	toA := last(t, r1.a)
	r1.sess.RouteFrom(emission(toA, "a", "text_out", "first"), emitter(idA))
	if last(t, r1.b).RespondBy == 0 {
		t.Fatal("precondition: the front should have carried respond_by")
	}

	r2 := newRig(t, nil, st, "s-resume-front", edges...)
	r2.sess.RouteFrom(emission(toA, "a", "text_out", "second"), emitter(idA))
	if got := last(t, r2.b).RespondBy; got != 0 {
		t.Fatalf("after a restart a second output was treated as the front again (%d)", got)
	}
}

// A delivery held at a gate before a restart is still under the budget it was
// held under. Recomputing it from the edge would hand it a fresh one for
// having survived.
func TestAResumedGateKeepsItsBudget(t *testing.T) {
	edges := []Edge{
		{From: "client.text_out", To: "a.text_in", DeadlineMS: 5000},
		{From: "a.text_out", To: "w.text_in"},
	}
	for _, tc := range []struct {
		name    string
		wait    time.Duration
		deliver bool
	}{
		{"answered in time", time.Second, true},
		{"answered too late", time.Minute, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := testStore(t)
			session := "s-resume-budget"
			r1 := newRig(t, nil, st, session, edges...)
			start := r1.ms()
			r1.sess.Route(clientData(session))
			r1.sess.RouteFrom(emission(last(t, r1.a), "a", "text_out", "x"), emitter(idA))
			req := confirmRequest(t, *r1.toClient)

			r2 := newRig(t, nil, st, session, edges...) // same clock origin as r1
			r2.advance(tc.wait)
			approve(r2.sess, req, true)

			if delivered := len(*r2.w) == 1; delivered != tc.deliver {
				t.Fatalf("delivered=%v, want %v", delivered, tc.deliver)
			}
			if tc.deliver {
				if got := last(t, r2.w).Deadline; got != start+5000 {
					t.Fatalf("released under deadline start+%d, want start+5000", got-start)
				}
			}
		})
	}
}

// Resume used to read the first thousand events and stop. A gate opened late
// in a long session was simply gone after a restart: the human's approval
// arrived for a request the new process had never heard of.
func TestResumeReadsPastTheFirstThousandEvents(t *testing.T) {
	st := testStore(t)
	session := "s-resume-long"
	edges := []Edge{
		{From: "client.text_out", To: "a.text_in"},
		{From: "a.text_out", To: "b.text_in"},
		{From: "b.text_out", To: "w.text_in"},
	}
	r1 := newRig(t, nil, st, session, edges...)
	r1.sess.Route(clientData(session))
	toA := last(t, r1.a)
	for i := 0; i < 700; i++ { // an emission and a delivery each: 1400 events
		r1.sess.RouteFrom(emission(toA, "a", "text_out", "t"), emitter(idA))
	}
	lastToB := last(t, r1.b)
	final := emission(lastToB, "b", "text_out", "act")
	r1.sess.RouteFrom(final, emitter(idB))
	req := confirmRequest(t, *r1.toClient)

	events, _, err := st.SessionEvents(session, 5000)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) <= 1000 {
		t.Fatalf("precondition: want more than 1000 events, have %d", len(events))
	}

	r2 := newRig(t, nil, st, session, edges...)
	approve(r2.sess, req, true)
	if len(*r2.w) != 1 {
		t.Fatal("a gate opened after the thousandth event did not survive a restart")
	}

	// And the dedup window is the session's recent past, not its first minute.
	r2.sess.RouteFrom(final, emitter(idB))
	if n := len(*r2.toClient); n != 0 {
		for _, e := range *r2.toClient {
			if e.Kind == channel.KindConfirmRequest {
				t.Fatal("an envelope already processed before the restart was processed again")
			}
		}
	}

	// Per-hop sequence continues from where it actually was.
	r2.sess.RouteFrom(emission(toA, "a", "text_out", "after"), emitter(idA))
	if got, want := last(t, r2.b).Seq, lastToB.Seq+1; got != want {
		t.Fatalf("sequence into b resumed at %d, want %d", got, want)
	}
}
