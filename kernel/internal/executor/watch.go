package executor

// Watchers — many clients on one live session.
//
// A session has exactly one client: the socket that opened it, which is the
// only party that can send into the graph or answer a gate. That fits a
// conversation and nothing with an audience. A live product has one person
// driving and many people watching — the chat beside a stream, a support
// conversation a supervisor is following, a dashboard on a wall — and until
// now each of those would have needed its own session, running its own copy
// of the graph, which is neither the same conversation nor affordable.
//
// A watcher is a read-only observer of a session's output. It receives every
// envelope the session delivers to its client, in the same order, and can do
// nothing else.
//
// # The rule that makes it safe: a watcher can never slow the session
//
// The session's client sits on a `reliable` lane, so a slow client applies
// backpressure — that is the contract, and it is right for the party the
// conversation is *for*. A watcher gets the opposite. Fan-out is one
// non-blocking offer per watcher on the routing path, and a watcher that
// cannot take it is **disconnected**, not waited for and not skipped.
//
// Disconnected rather than skipped, deliberately. `std/text@1` partials are
// deltas a consumer concatenates; silently dropping one frame for a viewer who
// fell behind would leave them reading a sentence with a hole in it and no way
// to know. Cutting them off is honest: a watcher either has the whole stream
// or knows it does not, and can reconnect.
//
// # What a watcher does not get
//
//   - **Gates.** A `confirm_request` goes to the session's client and nobody
//     else. Who is asked to approve an effect is the heart of the evidence
//     this runtime produces, and an audience is not that person.
//   - **A voice.** Nothing a watcher sends is routed. It cannot inject input,
//     cancel a chain or answer anything.
//   - **History.** Watching starts at the moment of attaching. What came
//     before is in the causal log, behind the API that already serves it.
//
// Watchers are not recorded in the causal log: they cause nothing.

import (
	"fmt"
	"sync"
	"sync/atomic"
)

// MaxWatchersPerSession bounds the audience of one session. Each watcher is a
// socket and a goroutine on this node; past this, the answer is a relay in
// front of the node, not more sockets on it.
const MaxWatchersPerSession = 10000

type watcher struct {
	// offer hands a frame to the watcher without blocking, and reports
	// whether it was accepted.
	offer func(raw []byte) bool
	// closed is called once, when the watcher is removed for any reason other
	// than its own cancel: it fell behind, or the session ended.
	closed func(reason string)
}

type watchSet struct {
	mu     sync.Mutex
	next   uint64
	byID   map[uint64]*watcher
	ended  bool
	count  atomic.Int32
	cutOff atomic.Uint64 // watchers disconnected for falling behind
}

// Watch attaches a read-only observer to this session.
//
// `offer` must not block: it is called on the session's routing path. Return
// false to say the frame could not be taken, and the watcher is removed.
// `closed` is told why when the kernel removes the watcher. The returned
// function detaches it and is safe to call more than once.
func (s *Session) Watch(offer func(raw []byte) bool, closed func(reason string)) (func(), error) {
	w := &s.watch
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ended {
		return nil, fmt.Errorf("session %s has ended", s.ID)
	}
	if len(w.byID) >= MaxWatchersPerSession {
		return nil, fmt.Errorf("session %s already has %d watchers; put a relay in front of the node "+
			"rather than more sockets on it", s.ID, MaxWatchersPerSession)
	}
	if w.byID == nil {
		w.byID = map[uint64]*watcher{}
	}
	w.next++
	id := w.next
	w.byID[id] = &watcher{offer: offer, closed: closed}
	w.count.Store(int32(len(w.byID)))
	return func() {
		w.mu.Lock()
		delete(w.byID, id)
		w.count.Store(int32(len(w.byID)))
		w.mu.Unlock()
	}, nil
}

// Watchers is how many observers this session has right now.
func (s *Session) Watchers() int { return int(s.watch.count.Load()) }

// fanout offers a client-bound frame to every watcher.
func (s *Session) fanout(raw []byte) {
	w := &s.watch
	// The common case is no audience at all, and it must cost nothing: one
	// atomic load on the routing path, no lock.
	if w.count.Load() == 0 {
		return
	}
	var behind []*watcher
	w.mu.Lock()
	for id, each := range w.byID {
		if !each.offer(raw) {
			behind = append(behind, each)
			delete(w.byID, id)
		}
	}
	w.count.Store(int32(len(w.byID)))
	w.mu.Unlock()
	// Outside the lock: a close callback tears down a socket, and that must
	// not hold up the frames still to be offered to everyone else.
	for _, each := range behind {
		w.cutOff.Add(1)
		if each.closed != nil {
			each.closed("fell behind the session; reconnect to resume watching")
		}
	}
}

// closeWatchers detaches every watcher when the session ends.
func (s *Session) closeWatchers() {
	w := &s.watch
	w.mu.Lock()
	w.ended = true
	all := make([]*watcher, 0, len(w.byID))
	for id, each := range w.byID {
		all = append(all, each)
		delete(w.byID, id)
	}
	w.count.Store(0)
	w.mu.Unlock()
	for _, each := range all {
		if each.closed != nil {
			each.closed("the session ended")
		}
	}
}

// toClient delivers a frame to the session's client and offers it to the
// audience. Everything the client is sent goes through here except a gate's
// confirm_request, which is for the client alone.
func (s *Session) toClient(raw []byte, qos string) error {
	err := s.sendClient(raw, qos)
	s.fanout(raw)
	return err
}

// ClientPorts is the set of ports this session delivers to its client on —
// the `client.<port>` destinations of the graph's edges. It is what tells an
// envelope the kernel sent the client apart from one the client sent, since
// both are logged under the `client` node.
func (s *Session) ClientPorts() map[string]bool {
	out := map[string]bool{}
	for _, dests := range s.routes {
		for _, d := range dests {
			if d.ref == ClientRef {
				out[d.port] = true
			}
		}
	}
	return out
}

// Watch attaches an observer to a live session by id.
func (m *Manager) Watch(sessionID string, offer func(raw []byte) bool,
	closed func(reason string)) (func(), error) {
	sess, ok := m.Get(sessionID)
	if !ok {
		return nil, fmt.Errorf("no live session %q", sessionID)
	}
	return sess.Watch(offer, closed)
}

// WatchStats is what a node reports about its audiences.
type WatchStats struct {
	// Watchers is how many observers are attached across all live sessions.
	Watchers int `json:"watchers"`
	// CutOff counts watchers disconnected for falling behind, over the life
	// of the sessions still live.
	CutOff uint64 `json:"cut_off"`
}

// WatchStats sums the audiences of every live session.
func (m *Manager) WatchStats() WatchStats {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out WatchStats
	for _, sess := range m.sessions {
		out.Watchers += sess.Watchers()
		out.CutOff += sess.watch.cutOff.Load()
	}
	return out
}
