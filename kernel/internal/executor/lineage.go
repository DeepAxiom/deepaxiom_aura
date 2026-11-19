package executor

import (
	"container/list"
	"sync"
	"sync/atomic"
)

// Delivery lineage: which delivery caused which.
//
// The session has always known, for any envelope, the *root* of its chain —
// the client message it started from (causal.go). It has never known the step
// in between: that this delivery into the speech Expertise exists because of
// that delivery into the language model. Four things need exactly that and
// were each failing without it:
//
//   - **Deadlines.** C2 rule 7 says a deadline is inherited down a chain. The
//     kernel inherited it from the envelope an Expertise *emitted*, and no SDK
//     ever put it there, so a deadline died at the first Expertise it reached.
//     Keying it by root instead would be worse than the bug: every token of a
//     reply shares one root, so the first instant recorded would condemn the
//     rest of the stream. It has to be per delivery.
//   - **Whose work is this.** An inference request, a cancel, a piece of
//     evidence: each names the delivery it belongs to, and the kernel has to be
//     able to say what that delivery was and who it was handed to.
//   - **Cause checking.** An Expertise states the cause of what it emits. The
//     kernel used that statement to find the chain without ever checking it
//     named something that Expertise had actually been given.
//   - **Evidence by path.** What argued for an effect is whatever happened on
//     the way to it, which is a path through deliveries, not a root.
//
// So this is one entry per delivery made to an Expertise. Deliveries to the
// client are not recorded: nothing is ever caused by one.
//
// # Eviction
//
// Bounded, and evicted by last reference rather than by age: an entry is
// touched whenever an emission names it as its cause. A long generation keeps
// its own delivery alive for as long as it keeps producing, however many
// thousand deliveries it fans out downstream in the meantime — which is the
// case the root index's oldest-first rule gets wrong (it counts every
// forward, so a busy stream evicts the delivery that is feeding it).
//
// Losing an entry is not silent and not fatal. The emission that names a
// forgotten delivery still routes; it simply inherits no deadline and its
// link is counted as unverified. See Session.inherit.

// maxLineage bounds one session's index. About 200 bytes an entry, so under a
// megabyte for a session that fills it.
const maxLineage = 4096

// delivery is what the kernel remembers about one thing it handed an Expertise.
type delivery struct {
	id string
	// prev is the delivery that caused the emission this one forwarded. Empty
	// at the head of a chain (the source was the client, or a source Expertise
	// emitting on its own), and empty when the emission's stated cause could
	// not be verified.
	prev string
	root string
	// cause is the emission this delivery forwarded: the id the receiving
	// Expertise sees as `cause_id`, and the one a cancel aimed at this single
	// delivery names (a speculation that missed abandons by cause, not by
	// root).
	cause string
	// priority is the edge's. Kept here so that something acting on this
	// delivery's behalf can be told how urgent it is without re-deriving which
	// edge it travelled.
	priority int

	// Where it went. expertise and token identify *who* in a way that survives
	// a reconnect: a connection id changes when a process comes back, the
	// manifest id and the credential do not.
	ref, port string
	expertise string
	token     string

	// validUntil is the C3 `deadline`: the instant after which nothing this
	// delivery causes is worth delivering. Zero when there is none.
	validUntil int64
	// respondBy is the C3 `respond_by`: the instant the first output caused by
	// this delivery is due. Zero when there is none.
	respondBy int64
	// frontSent records that the first `data` output caused by this delivery
	// has been routed. respondBy is a bound on the *front* of a stream, so it
	// is handed on exactly once.
	frontSent bool
}

// lineageIndex is a session's bounded map of deliveries, least recently
// referenced first out.
type lineageIndex struct {
	mu    sync.Mutex
	byID  map[string]*list.Element // value: *delivery
	order *list.List               // front = most recently referenced
	max   int
	stats *lineageCounters
}

// lineageCounters are node-wide, shared by every session, so the question
// "is the bound too small on this node" has one number to look at.
type lineageCounters struct {
	recorded   atomic.Uint64
	evicted    atomic.Uint64
	unverified atomic.Uint64
	foreign    atomic.Uint64
}

// LineageStats is what a node reports about delivery lineage.
type LineageStats struct {
	// Recorded counts deliveries entered into the index.
	Recorded uint64 `json:"recorded"`
	// Evicted counts entries dropped to stay within the bound.
	Evicted uint64 `json:"evicted"`
	// Unverified counts emissions whose stated cause the kernel no longer
	// held — evicted, or never a delivery at all. Each inherited no deadline.
	// Rising alongside Evicted means the bound is too small for the workload;
	// rising on its own means something is emitting with causes it was not
	// given.
	Unverified uint64 `json:"unverified"`
	// Foreign counts emissions whose stated cause was a delivery made to a
	// *different* Expertise. Never legitimate.
	Foreign uint64 `json:"foreign"`
}

func (c *lineageCounters) snapshot() LineageStats {
	return LineageStats{
		Recorded:   c.recorded.Load(),
		Evicted:    c.evicted.Load(),
		Unverified: c.unverified.Load(),
		Foreign:    c.foreign.Load(),
	}
}

func newLineageIndex(max int, stats *lineageCounters) *lineageIndex {
	if stats == nil {
		stats = &lineageCounters{}
	}
	return &lineageIndex{
		byID: make(map[string]*list.Element), order: list.New(), max: max, stats: stats,
	}
}

// record enters a delivery. Re-recording an id replaces it, which is what a
// resume replaying the log wants.
func (l *lineageIndex) record(d delivery) {
	if d.id == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if el, ok := l.byID[d.id]; ok {
		*(el.Value.(*delivery)) = d
		l.order.MoveToFront(el)
		return
	}
	l.byID[d.id] = l.order.PushFront(&d)
	l.stats.recorded.Add(1)
	for l.order.Len() > l.max {
		oldest := l.order.Back()
		l.order.Remove(oldest)
		delete(l.byID, oldest.Value.(*delivery).id)
		l.stats.evicted.Add(1)
	}
}

// touch returns a copy of the entry for id and marks it referenced. The copy
// is deliberate: callers read it outside the lock, and an entry is mutated
// only through the index.
func (l *lineageIndex) touch(id string) (delivery, bool) {
	if id == "" {
		return delivery{}, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	el, ok := l.byID[id]
	if !ok {
		return delivery{}, false
	}
	l.order.MoveToFront(el)
	return *(el.Value.(*delivery)), true
}

// peek is touch without the reference: for reads that are about the entry
// rather than on behalf of work continuing under it.
func (l *lineageIndex) peek(id string) (delivery, bool) {
	if id == "" {
		return delivery{}, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	el, ok := l.byID[id]
	if !ok {
		return delivery{}, false
	}
	return *(el.Value.(*delivery)), true
}

// takeFront reports whether this is the first `data` output caused by id and,
// if so, records that it has now been sent. Atomic, because two emissions
// under one delivery may be routed from different connections at once and
// exactly one of them is the front.
func (l *lineageIndex) takeFront(id string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	el, ok := l.byID[id]
	if !ok {
		return false
	}
	d := el.Value.(*delivery)
	if d.frontSent {
		return false
	}
	d.frontSent = true
	return true
}

// markFront is takeFront for a resume replaying history: the front went out
// before this process existed.
func (l *lineageIndex) markFront(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if el, ok := l.byID[id]; ok {
		el.Value.(*delivery).frontSent = true
	}
}

func (l *lineageIndex) len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.order.Len()
}

// Emitter identifies who put an envelope on a connection.
//
// An Expertise states its own `node`, `port` and `cause_id` on everything it
// emits, and until this existed the kernel had nothing to check those
// statements against: Dispatch received an envelope and no notion of which
// connection it had arrived on.
//
// The zero Emitter means the kernel's own authority — the client socket, an
// ingress route, a pinned output — where there is no Expertise to check.
type Emitter struct {
	// Conn is the connection id, for logs. It changes on reconnect, so it is
	// never what identity is decided on.
	Conn string
	// Expertise is the manifest id the connection registered.
	Expertise string
	// TokenID is the scoped credential it registered with. Empty for the
	// operator token, which is a file rather than a row.
	TokenID string
}

// trusted reports whether this is the kernel's own authority rather than an
// Expertise's.
func (e Emitter) trusted() bool { return e.Expertise == "" }

// linkState is what the kernel could establish about an emission's stated
// cause.
type linkState int

const (
	// linkRoot: no cause to check — the envelope starts a chain.
	linkRoot linkState = iota
	// linkVerified: the cause is a delivery that was made to this emitter.
	linkVerified
	// linkUnverified: the kernel no longer holds (or never held) the delivery
	// the emission names.
	linkUnverified
	// linkForeign: the cause is a delivery that was made to someone else.
	linkForeign
)

// madeTo reports whether d was handed to this emitter.
//
// Decided on the manifest id, and on the credential when both sides have one.
// Not on the connection: an Expertise that drops and reconnects mid-reply is
// still the Expertise that was given the work, and refusing the rest of its
// reply would turn a network blip into a lost answer.
func (d delivery) madeTo(e Emitter) bool {
	if d.expertise != e.Expertise {
		return false
	}
	return d.token == "" || e.TokenID == "" || d.token == e.TokenID
}
