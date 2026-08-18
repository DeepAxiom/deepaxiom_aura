package store

import (
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// Group commit: the one change that lets concurrency help instead of hurt.
//
// # The problem it solves
//
// SQLite has a single writer. Every envelope is one INSERT in its own implicit
// transaction, taken on the delivery path, so N concurrent sessions form a
// queue at one write lock and each pays the full cost of a transaction alone.
// Measured on one machine before this existed: 1 session did 1,556 round-trips
// a second, 50 sessions did 365, and 200 did 344 with a p50 of 489 ms. Adding
// load made the node slower in absolute terms — not slower per session, slower
// in total. That is the signature of serialization, and no amount of tuning the
// pragma fixes it, because the cost being paid is per *transaction* and there
// was one transaction per event.
//
// # Why this does not weaken anything
//
// It would be easy, and wrong, to buy throughput by acknowledging an effect
// before its record is durable. That is the one trade this runtime cannot make:
// the whole argument is that a sealed entry is evidence, and evidence written
// after the fact is not evidence.
//
// So the deal here is narrower, and is the same one PostgreSQL, MySQL and
// RocksDB have made for decades. A caller still waits for its own write to be
// durably committed before it proceeds. What changes is only that *while it
// waits*, everyone else waiting joins the same transaction. One commit covers
// the batch. Nothing is acknowledged early, nothing is reordered, nothing is
// dropped:
//
//   - Order is preserved — a single writer goroutine drains the queue in
//     submission order and writes in that order, so the events table and the
//     ledger's sequence numbers land exactly as they were produced.
//   - Durability is preserved — Submit does not return until the transaction
//     containing that row has committed.
//   - The hash chain is untouched — chaining happens before submission, under
//     the ledger's own lock, so batching changes when bytes reach the disk and
//     never what they say.
//
// The pattern is Trillian's sequencer applied locally: queue the entries,
// sequence and commit them in batches, keep every verifiability property. That
// is how Certificate Transparency runs at internet scale, and there is no
// reason a node needs a weaker guarantee to get a stronger number.
//
// # The shape of the win
//
// Under one caller a batch is one row and the behaviour is exactly what it was
// before — no added latency, no timer to wait out. Under load the batch fills
// itself with whatever is already queued, so the busier the node is, the more
// each commit amortizes. Concurrency stops being the thing that slows the node
// down and becomes the thing that makes each commit worth more.

// # Shutdown
//
// A write must either land or come back with an error — never neither. That is
// not a nicety on this path: `Submit` is called synchronously from a session
// goroutine, so a request that is accepted and then abandoned hangs that
// goroutine for the life of the process, and the two things flowing through
// here are the causal event log and the *effect ledger*. Losing the ack on the
// evidence path is the worst version of this bug there is.
//
// Getting that right is not a `select` on a `stop` channel, which is the
// obvious and wrong answer: `reqs` is buffered, so after Close both cases of
// `select { case w.reqs <- req: case <-w.stop: }` are ready and Go picks between
// them at random. Roughly half of a racing burst is enqueued into a channel
// whose reader has already returned. Measured on this exact code before the fix:
// 56 of 100 submits after Close neither landed nor returned.
//
// So admission is a lock, and the order is what makes it work:
//
//  1. Close takes `admit` for write, sets `closing`, releases. From here every
//     new Submit is refused, so what is queued is a finite set.
//  2. Close signals the writer, which drains that finite set and commits it —
//     rather than walking away from callers already blocked on `done`.
//  3. Close waits on the WaitGroup before returning, so `Store.Close` can close
//     the database knowing no transaction is still in flight against it.
//
// Taking `admit` for read cannot deadlock against Close even when the queue is
// full, because Close cannot have signalled the writer yet — it sets `closing`
// under the write lock first, so the writer is still draining.
//
// This is the same shape internal/seglog uses, for the same reason. Two
// append-only writers with the same shutdown contract should not have two
// different answers to it.

// maxBatch caps one transaction. Large enough that a busy node amortizes hard,
// small enough that a single commit stays quick and a failure re-runs a bounded
// amount of work.
const maxBatch = 512

// ErrClosed is returned by Submit once the writer has been closed. Callers
// treat it as a shutdown signal, not as data loss: nothing that returned nil is
// affected by it.
var ErrClosed = errors.New("store: the write path is closed")

type writeReq struct {
	query string
	args  []any
	done  chan error
}

// writer serializes every hot-path write through one goroutine, which is what
// makes batching possible and ordering free.
type writer struct {
	reqs chan *writeReq
	stop chan struct{}

	closed sync.Once
	wg     sync.WaitGroup

	// admit guards closing. See the shutdown note above for why this is a lock
	// and not a select. Held for read on every Submit — uncontended except
	// against Close, which is the one writer.
	admit   sync.RWMutex
	closing bool

	// Instrumentation. Batch size is the number that decides whether this
	// design has more to give: if batches are filling, the commit itself is the
	// limit and no amount of extra writers on the same file helps — they would
	// contend on one write lock and make batches smaller. If batches are small
	// under load, the limit is elsewhere.
	batches   atomic.Uint64
	rows      atomic.Uint64
	maxSeen   atomic.Uint64
	commitNs  atomic.Uint64
	fallbacks atomic.Uint64
}

// WriteStats reports what the batcher actually did, for diagnosing where the
// write path stands rather than guessing.
type WriteStats struct {
	Batches       uint64
	Rows          uint64
	MeanBatch     float64
	LargestBatch  uint64
	MeanCommitMs  float64
	FallbackCount uint64
}

func newWriter(db *sql.DB) *writer {
	w := &writer{
		// Buffered so a burst of producers does not block on handoff before the
		// batcher has even looked; the batcher drains it in one pass.
		reqs: make(chan *writeReq, maxBatch*2),
		stop: make(chan struct{}),
	}
	w.wg.Add(1)
	go w.run(db)
	return w
}

// Submit queues a write and blocks until it is durably committed.
func (w *writer) Submit(query string, args ...any) error {
	req := &writeReq{query: query, args: args, done: make(chan error, 1)}

	w.admit.RLock()
	if w.closing {
		w.admit.RUnlock()
		return ErrClosed
	}
	w.reqs <- req
	w.admit.RUnlock()

	return <-req.done
}

// Close stops admission, commits what was already accepted, and waits for the
// writer goroutine to finish — so a caller may close the database as soon as
// this returns.
func (w *writer) Close() {
	w.closed.Do(func() {
		// Order matters. Refusing new writes *before* signalling the writer is
		// what bounds the queue: once this returns, every Submit either
		// completed its enqueue or was refused, so the drain below sees a finite
		// set and terminates.
		w.admit.Lock()
		w.closing = true
		w.admit.Unlock()
		close(w.stop)
	})
	w.wg.Wait()
}

func (w *writer) run(db *sql.DB) {
	defer w.wg.Done()
	batch := make([]*writeReq, 0, maxBatch)
	for {
		select {
		case <-w.stop:
			// Admission is already closed, so what is still queued is a finite
			// set of writes accepted while the store was open. Commit them
			// rather than walk away: their callers are blocked on `done`.
			w.drain(db, batch)
			return
		case first := <-w.reqs:
			batch = append(batch[:0], first)
			// Take whatever else is already waiting. No timer: waiting on the
			// chance that more work arrives would add latency to an idle node
			// to speed up a busy one, and an idle node is the common case.
			// Whatever is queued *right now* is free to batch.
		drain:
			for len(batch) < maxBatch {
				select {
				case r := <-w.reqs:
					batch = append(batch, r)
				default:
					break drain
				}
			}
			w.commit(db, batch)
		}
	}
}

// drain commits everything left in the queue at shutdown, reusing the caller's
// batch buffer. It terminates because Close closes admission before signalling
// the writer, so no new request can arrive while this runs.
func (w *writer) drain(db *sql.DB, batch []*writeReq) {
	for {
		batch = batch[:0]
	fill:
		for len(batch) < maxBatch {
			select {
			case r := <-w.reqs:
				batch = append(batch, r)
			default:
				break fill
			}
		}
		if len(batch) == 0 {
			return
		}
		w.commit(db, batch)
	}
}

// commit writes the batch in one transaction, falling back to one-at-a-time if
// the batch fails.
//
// The fallback matters for honesty rather than speed: if one row in a batch is
// bad — a duplicate ledger sequence, a constraint nobody expected — a single
// transaction fails all of them, and reporting failure to callers whose writes
// were fine would be a lie that loses their data. Re-running individually costs
// one slow batch and tells each caller the truth about its own write.
func (w *writer) commit(db *sql.DB, batch []*writeReq) {
	start := time.Now()
	w.batches.Add(1)
	w.rows.Add(uint64(len(batch)))
	for {
		cur := w.maxSeen.Load()
		if uint64(len(batch)) <= cur || w.maxSeen.CompareAndSwap(cur, uint64(len(batch))) {
			break
		}
	}
	defer func() { w.commitNs.Add(uint64(time.Since(start).Nanoseconds())) }()

	if err := w.tryBatch(db, batch); err == nil {
		for _, r := range batch {
			r.done <- nil
		}
		return
	}
	w.fallbacks.Add(1)
	for _, r := range batch {
		_, err := db.Exec(r.query, r.args...)
		r.done <- err
	}
}

func (w *writer) tryBatch(db *sql.DB, batch []*writeReq) error {
	if len(batch) == 1 {
		_, err := db.Exec(batch[0].query, batch[0].args...)
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	for _, r := range batch {
		if _, err := tx.Exec(r.query, r.args...); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// Stats snapshots what the batcher has done since the process started.
//
// Exposed because these are the numbers that answer "is the write path the
// limit?" and they were previously computed and then only printed once, at
// startup, when they were all zero. Mean batch size is the load-bearing one: if
// batches are filling, the commit itself is the ceiling and adding writers to
// one SQLite file would make it worse rather than better.
func (w *writer) Stats() WriteStats {
	batches := w.batches.Load()
	s := WriteStats{
		Batches:       batches,
		Rows:          w.rows.Load(),
		LargestBatch:  w.maxSeen.Load(),
		FallbackCount: w.fallbacks.Load(),
	}
	if batches > 0 {
		s.MeanBatch = float64(s.Rows) / float64(batches)
		s.MeanCommitMs = float64(w.commitNs.Load()) / float64(batches) / 1e6
	}
	return s
}
