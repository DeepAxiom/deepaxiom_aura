package executor

// Per-session mailboxes — so one slow session cannot stall the others that
// share a skill's connection.
//
// # The problem
//
// A skill holds one connection to the kernel and every session it serves is
// multiplexed onto it. The gateway reads that connection in one loop and, for
// each envelope, calls Dispatch — which until now ran the session's routing
// inline: log the event, forward along each edge, write to the destination.
// On a `reliable` edge that last step blocks while the destination's buffer is
// full, because that is what reliable means.
//
// So when one session's consumer fell behind — a browser on a bad link, a TTS
// skill synthesising slower than the model writes — the read loop blocked
// inside that session's delivery, and the next envelope on the same socket,
// belonging to a *different* session, waited behind it. One slow listener
// stalled every conversation the same llm-chat replica was serving. That is
// head-of-line blocking, and it grows with exactly the thing a node is meant
// to be good at: many live sessions at once.
//
// # The fix
//
// Each session gets a bounded queue and one goroutine that drains it. The
// read loop's job shrinks to "put the envelope in its session's queue", which
// does not block unless that one session is already `depth` envelopes behind.
// A slow session now only slows itself.
//
// # What does not change
//
//   - **Order within a session.** Envelopes from one connection are enqueued
//     in the order they were read and drained by a single goroutine, so the
//     order a session sees is the order the skill sent. That is the same
//     guarantee as before; C3 never promised order *across* channels.
//   - **Durability and the causal log.** Route still appends every envelope
//     before acting on it. Nothing is acknowledged early; the queue is in
//     front of the log, not instead of it. An envelope still in a mailbox when
//     the process dies was never logged and never acted on — exactly the state
//     of an envelope still in a socket buffer, which is where it would
//     otherwise have been.
//   - **Backpressure.** The queue is bounded. A session that stays behind
//     fills it, and then the producer's read loop blocks as it did before —
//     the pressure still reaches the skill, it just has to be sustained
//     rather than momentary to do so.
//   - **Cancel is not queued.** A client's envelopes, cancel included, do not
//     pass through here: the client's own socket loop calls Route directly.
//     A cancel therefore overtakes every token still waiting in the mailbox,
//     and suppression drops them as they are drained. Queuing it behind the
//     output it is trying to stop would be the one way to make this slower
//     than what it replaces.
//   - **Blast radius.** The drain goes through Manager.route, so a panic in
//     one session's routing is recovered there and the goroutine carries on
//     with the next envelope.
//
// # Why it is optional
//
// Inline dispatch is synchronous, and a great many tests — and any embedder
// that calls Dispatch and then immediately inspects the result — rely on
// that. So the Manager defaults to inline and a node turns mailboxes on
// (`aura up --session-mailbox`, default 1024, 0 for the old behaviour).

import (
	"sync"
	"sync/atomic"
	"time"

	"aura/kernel/internal/channel"
)

// DefaultMailboxDepth is how many envelopes a session may be behind before
// its producers start to feel it. A few seconds of a fast model's output per
// session, and at ~1 KiB an envelope about a megabyte at worst — per session
// that is actually behind, not per session.
const DefaultMailboxDepth = 1024

// mailbox is one session's inbound queue.
type mailbox struct {
	ch   chan channel.Envelope
	done chan struct{}
	once sync.Once
	wg   sync.WaitGroup
}

// MailboxStats is what a node reports about its dispatch queues.
type MailboxStats struct {
	// Enabled is false on a node dispatching inline.
	Enabled bool `json:"enabled"`
	// Depth is the configured bound per session.
	Depth int `json:"depth"`
	// Queued is how many envelopes are waiting across all sessions right now.
	Queued int64 `json:"queued"`
	// Waits counts enqueues that found a session's mailbox full and had to
	// block the producer. Rising steadily means some session is persistently
	// slower than what feeds it — the number to look at before raising Depth.
	Waits uint64 `json:"waits"`
	// Dropped counts envelopes that arrived for a session as it ended.
	Dropped uint64 `json:"dropped"`
}

type mailboxCounters struct {
	queued  atomic.Int64
	waits   atomic.Uint64
	dropped atomic.Uint64
}

// SetMailboxDepth turns on per-session mailboxes for sessions started after
// the call. Zero or negative restores inline dispatch. Call before the node
// serves; it is not meant to be flipped under load.
func (m *Manager) SetMailboxDepth(depth int) {
	if depth < 0 {
		depth = 0
	}
	m.mailboxDepth = depth
}

// MailboxStats snapshots the dispatch queues.
func (m *Manager) MailboxStats() MailboxStats {
	return MailboxStats{
		Enabled: m.mailboxDepth > 0,
		Depth:   m.mailboxDepth,
		Queued:  m.mbox.queued.Load(),
		Waits:   m.mbox.waits.Load(),
		Dropped: m.mbox.dropped.Load(),
	}
}

// SpeculationStats sums the guessing record of every live session. Live
// sessions only: a session takes its counters with it when it ends, so this
// is a picture of now rather than a lifetime total.
func (m *Manager) SpeculationStats() SpeculationStats {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var total SpeculationStats
	for _, sess := range m.sessions {
		s := sess.SpeculationStats()
		total.Attempts += s.Attempts
		total.Hits += s.Hits
		total.Misses += s.Misses
		total.Refused += s.Refused
	}
	return total
}

// startMailbox gives a session its queue and the goroutine that drains it.
func (m *Manager) startMailbox(sess *Session) {
	if m.mailboxDepth <= 0 {
		return
	}
	mb := &mailbox{
		ch:   make(chan channel.Envelope, m.mailboxDepth),
		done: make(chan struct{}),
	}
	sess.mailbox = mb
	mb.wg.Add(1)
	go func() {
		defer mb.wg.Done()
		// The session is over. Whatever is still queued belongs to a
		// conversation nobody is in any more; before mailboxes those envelopes
		// would have been refused as "unknown session". Counted, so it is
		// visible rather than silent.
		abandon := func() {
			if n := len(mb.ch); n > 0 {
				m.mbox.queued.Add(-int64(n))
				m.mbox.dropped.Add(uint64(n))
			}
		}
		for {
			// Checked on its own first. A `select` with both cases ready picks
			// at random, so without this an ended session would keep draining
			// roughly half its backlog — each envelope a delivery to a client
			// that has gone, waiting out a backpressure timeout.
			select {
			case <-mb.done:
				abandon()
				return
			default:
			}
			select {
			case <-mb.done:
				abandon()
				return
			case env := <-mb.ch:
				m.mbox.queued.Add(-1)
				// route recovers a panic and returns it as an error. Inline, the
				// caller logged that; here there is no caller left to, and route
				// has already logged it with the session and the stack.
				_ = m.route(sess, env)
			}
		}
	}()
}

// mailboxStopWait bounds how long End waits for a delivery already in flight.
var mailboxStopWait = 2 * time.Second

// stopMailbox ends a session's drain goroutine and waits briefly for it, so
// in the ordinary case End returns only once nothing is routing into a
// session that no longer exists.
//
// Briefly, not indefinitely. The one delivery in flight may be blocked on a
// consumer that has stopped reading — which is often *why* the session is
// ending — and that write gives up on its own timeout. Holding End for that
// long would hold the socket handler that called it, for nothing: inline
// dispatch never gave this guarantee either, since a skill's read loop could
// equally be mid-delivery when a client hung up.
func (m *Manager) stopMailbox(sess *Session) {
	mb := sess.mailbox
	if mb == nil {
		return
	}
	mb.once.Do(func() { close(mb.done) })
	finished := make(chan struct{})
	go func() { mb.wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(mailboxStopWait):
	}
}

// enqueue hands an envelope to a session's mailbox. It blocks only when that
// session is `depth` envelopes behind, and never on a session that has ended.
func (m *Manager) enqueue(mb *mailbox, env channel.Envelope) {
	select {
	case <-mb.done:
		m.mbox.dropped.Add(1)
		return
	default:
	}
	// Counted before the send, so the drain's decrement can never be observed
	// first and take the gauge below zero.
	m.mbox.queued.Add(1)
	select {
	case mb.ch <- env:
		return
	default:
	}
	// Full: this session is behind. Now, and only now, the producer waits.
	m.mbox.waits.Add(1)
	select {
	case mb.ch <- env:
	case <-mb.done:
		m.mbox.queued.Add(-1)
		m.mbox.dropped.Add(1)
	}
}
