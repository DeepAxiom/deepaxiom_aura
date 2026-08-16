package store

import (
	"database/sql"
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

// maxBatch caps one transaction. Large enough that a busy node amortizes hard,
// small enough that a single commit stays quick and a failure re-runs a bounded
// amount of work.
const maxBatch = 512

type writeReq struct {
	query string
	args  []any
	done  chan error
}

// writer serializes every hot-path write through one goroutine, which is what
// makes batching possible and ordering free.
type writer struct {
	reqs   chan *writeReq
	stop   chan struct{}
	closed sync.Once

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
	go w.run(db)
	return w
}

// Submit queues a write and blocks until it is durably committed.
func (w *writer) Submit(query string, args ...any) error {
	req := &writeReq{query: query, args: args, done: make(chan error, 1)}
	select {
	case w.reqs <- req:
	case <-w.stop:
		return sql.ErrConnDone
	}
	return <-req.done
}

func (w *writer) Close() {
	w.closed.Do(func() { close(w.stop) })
}

func (w *writer) run(db *sql.DB) {
	batch := make([]*writeReq, 0, maxBatch)
	for {
		select {
		case <-w.stop:
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
