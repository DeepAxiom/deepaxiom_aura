// Package lease is the single-writer guarantee for a data directory.
//
// # The hole this closes
//
// Nothing stopped two kernels from opening the same data directory. Both would
// append to the same effect ledger and the same causal event log, interleaving
// sequences and hash-chain links that each believed it owned. The result is not
// a crash — it is a ledger that fails to verify, discovered later, with no way
// to tell which process wrote which entry. For a component whose entire value
// is being evidence, that is the worst available failure.
//
// It has stayed harmless so far only because the documented deployment is one
// process. It stops being harmless the moment anyone runs a second one: a
// standby, a stray `aura up` in another terminal, a container that did not
// fully stop before its replacement started.
//
// # Why a lease rather than a lock file
//
// A POSIX flock or a Windows LockFileEx would need build-tagged files per
// platform, would not survive a shared volume, and — the decisive part —
// answers only "is someone here right now". A lease answers "who is the
// writer, since when, and are they still alive", which is the question a
// standby has to ask before taking over.
//
// So the same mechanism that closes today's hole is the one a warm standby
// needs later, and there is no second thing to build.
//
// # Fencing: losing the lease is fatal, by design
//
// A holder renews on a timer. If a renewal finds that the lease now belongs to
// somebody else — because this process was stopped, paused, or partitioned long
// enough for its heartbeat to go stale, and another took over — then this
// process is no longer the writer, and it must stop writing *immediately*.
// Continuing is precisely the split-brain the lease exists to prevent.
//
// That is why Lost() is a channel rather than a flag to poll politely: the
// caller is expected to treat it as a shutdown signal, not as advice. A lease
// that can be lost without consequence is decoration.
package lease

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// DefaultTTL is how long a lease stays valid without a renewal.
//
// It bounds two things that pull in opposite directions: how long a standby
// waits before taking over from a dead leader, and how long a *live* leader
// can be stalled — a long GC pause, a suspended laptop, a throttled container
// — before it is presumed dead while still running. Ten seconds is long enough
// that ordinary scheduling hiccups do not trigger a false takeover, and short
// enough that recovery is measured in seconds.
const DefaultTTL = 10 * time.Second

// renewDivisor sets the renewal interval to TTL/3, so a holder gets two
// consecutive failed renewals before its lease can expire. One dropped
// renewal must never be enough to lose it.
const renewDivisor = 3

// ErrHeld is returned when another live process holds the lease.
var ErrHeld = errors.New("another process holds this data directory")

// Holder is the acquired lease. It renews itself until Release is called or
// the lease is lost.
type Holder struct {
	db  *sql.DB
	id  string
	ttl time.Duration

	// Since is when this holder took the lease, for reporting.
	Since time.Time

	stop     chan struct{}
	lost     chan struct{}
	lostOnce sync.Once
	wg       sync.WaitGroup
}

// Info describes whoever currently holds the lease.
type Info struct {
	Holder    string
	Since     time.Time
	Heartbeat time.Time
	// TTL is the grace period the holder declared. A challenger honours this
	// rather than its own, so a node cannot be displaced by someone else's
	// impatience.
	TTL time.Duration
}

// Age is how long since the holder last proved it was alive.
func (i Info) Age() time.Duration { return time.Since(i.Heartbeat) }

// Migrate creates the lease table. Callers that already own a *sql.DB for the
// data directory pass it here; the package deliberately does not open its own
// connection, so the lease lives in the same file whose writes it guards.
func Migrate(db *sql.DB) error {
	_, err := db.Exec(`
CREATE TABLE IF NOT EXISTS node_lease (
  id        INTEGER PRIMARY KEY CHECK (id = 1),
  holder    TEXT NOT NULL,
  since     INTEGER NOT NULL,
  heartbeat INTEGER NOT NULL,
  ttl_ms    INTEGER NOT NULL DEFAULT 10000
);`)
	if err != nil {
		return err
	}
	// CREATE TABLE IF NOT EXISTS is a no-op against a directory written by an
	// earlier build, so the column has to be added separately or a node that
	// upgrades in place reports "no such column" and refuses to start. The
	// default matches DefaultTTL, which is what those rows were written under.
	_, err = db.Exec(`ALTER TABLE node_lease ADD COLUMN ttl_ms INTEGER NOT NULL DEFAULT 10000`)
	if err != nil && !isDuplicateColumn(err) {
		return fmt.Errorf("migrate node_lease.ttl_ms: %w", err)
	}
	return nil
}

// isDuplicateColumn recognises the "already migrated" case. Matched on the
// message because modernc's driver does not expose a typed code for it, and
// the alternative — reading the schema first — is a second round trip to learn
// something the ALTER already tells us.
func isDuplicateColumn(err error) bool {
	return err != nil && strings.Contains(err.Error(), "duplicate column name")
}

// Acquire takes the lease for this process, or reports who holds it.
//
// The take is a compare-and-swap in SQL: the UPDATE only applies when the row
// is still the one this caller read, or when the current holder's heartbeat
// has aged past the TTL. Two processes racing here cannot both succeed,
// because SQLite serialises the writes and the loser's WHERE clause no longer
// matches.
func Acquire(db *sql.DB, ttl time.Duration) (*Holder, error) {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	if err := Migrate(db); err != nil {
		return nil, fmt.Errorf("prepare the lease table: %w", err)
	}

	id, err := newHolderID()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	nowMS := now.UnixMilli()

	// Insert if the row has never existed; on conflict, take it only from a
	// holder whose heartbeat has gone stale. One statement, so there is no
	// window between the check and the take.
	//
	// Staleness is judged against `node_lease.ttl_ms` — the TTL the *holder*
	// declared — not against the challenger's. Using the challenger's would
	// let a node started with a short TTL displace a perfectly healthy one
	// that had chosen a longer grace period, which is a live node being killed
	// by a flag on a different command line. Found by running it.
	res, err := db.Exec(`
INSERT INTO node_lease (id, holder, since, heartbeat, ttl_ms) VALUES (1, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET holder = excluded.holder, since = excluded.since,
                              heartbeat = excluded.heartbeat, ttl_ms = excluded.ttl_ms
WHERE ? - node_lease.heartbeat >= node_lease.ttl_ms`,
		id, nowMS, nowMS, ttl.Milliseconds(), nowMS)
	if err != nil {
		return nil, fmt.Errorf("acquire the lease: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		current, readErr := Current(db)
		if readErr != nil {
			return nil, fmt.Errorf("%w (and its details could not be read: %v)", ErrHeld, readErr)
		}
		return nil, fmt.Errorf("%w — holder %s, alive %s ago (its lease grants %s), since %s",
			ErrHeld, current.Holder, current.Age().Round(time.Millisecond),
			current.TTL, current.Since.Format(time.RFC3339))
	}

	h := &Holder{
		db: db, id: id, ttl: ttl, Since: now,
		stop: make(chan struct{}), lost: make(chan struct{}),
	}
	h.wg.Add(1)
	go h.renew()
	return h, nil
}

// AcquireWaiting is Acquire for a process that expects to be a *replacement*:
// it waits for a stale claim to expire instead of failing on it.
//
// This is what a supervised restart needs, and leaving it out would have made
// the restart path quietly useless. A node that is SIGKILLed does not release
// its lease, so for up to a full TTL afterwards the row still names a process
// that no longer exists. A replacement started by systemd or a container
// runtime a second later would refuse, exit, and be restarted again — a crash
// loop that ends only when the supervisor's backoff happens to exceed the TTL,
// which is a recovery time nobody chose.
//
// Waiting is bounded, and the bound is not a formality: if the lease never
// frees, the holder is alive and this process must not start. Waiting forever
// would turn "another node is already running" from an error into a hang.
//
// It returns the time spent waiting, because that number is most of a crash
// recovery and an operator tuning it needs to see it rather than infer it.
func AcquireWaiting(db *sql.DB, ttl, maxWait time.Duration) (*Holder, time.Duration, error) {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	started := time.Now()

	// Poll rather than sleep the whole TTL: the common case is a clean
	// shutdown that released the lease, where the first attempt succeeds and
	// nothing waits at all.
	const poll = 250 * time.Millisecond
	for {
		h, err := Acquire(db, ttl)
		if err == nil {
			return h, time.Since(started), nil
		}
		if !errors.Is(err, ErrHeld) {
			return nil, time.Since(started), err
		}
		if time.Since(started) >= maxWait {
			return nil, time.Since(started), fmt.Errorf(
				"%w — still held after waiting %s", err, maxWait)
		}
		time.Sleep(poll)
	}
}

// Current reads whoever holds the lease, without taking it.
func Current(db *sql.DB) (Info, error) {
	var holder string
	var since, heartbeat, ttlMS int64
	err := db.QueryRow(`SELECT holder, since, heartbeat, ttl_ms FROM node_lease WHERE id = 1`).
		Scan(&holder, &since, &heartbeat, &ttlMS)
	if err == sql.ErrNoRows {
		return Info{}, nil
	}
	if err != nil {
		return Info{}, err
	}
	return Info{
		Holder:    holder,
		Since:     time.UnixMilli(since),
		Heartbeat: time.UnixMilli(heartbeat),
		TTL:       time.Duration(ttlMS) * time.Millisecond,
	}, nil
}

// ID is this holder's identity in the lease row — printed on startup so an
// operator reading two nodes' logs can tell which one owns the directory.
func (h *Holder) ID() string { return h.id }

// Lost is closed when this process is no longer the writer. A caller MUST
// treat it as a shutdown signal: continuing to write after another process
// took over is the corruption this package exists to prevent.
func (h *Holder) Lost() <-chan struct{} { return h.lost }

// Release gives the lease up so a replacement can start immediately rather
// than waiting out the TTL. Safe to call more than once.
func (h *Holder) Release() error {
	select {
	case <-h.stop:
		return nil // already released
	default:
		close(h.stop)
	}
	h.wg.Wait()

	// Only if we still hold it. A process that lost the lease and is shutting
	// down must not delete the row its successor now owns.
	_, err := h.db.Exec(`DELETE FROM node_lease WHERE id = 1 AND holder = ?`, h.id)
	return err
}

func (h *Holder) renew() {
	defer h.wg.Done()
	tick := time.NewTicker(h.ttl / renewDivisor)
	defer tick.Stop()

	for {
		select {
		case <-h.stop:
			return
		case <-tick.C:
			held, err := h.beat()
			if err != nil {
				// A transient database error is not proof that the lease was
				// taken. Keep trying: two more failures inside the TTL will
				// let it expire and a standby will take over honestly, which
				// is the correct outcome for a node whose storage is failing.
				continue
			}
			if !held {
				h.lostOnce.Do(func() { close(h.lost) })
				return
			}
		}
	}
}

// beat renews the lease and reports whether this process still holds it. The
// WHERE clause is the fence: an UPDATE that matches no row means the holder
// column no longer names us.
func (h *Holder) beat() (bool, error) {
	res, err := h.db.Exec(
		`UPDATE node_lease SET heartbeat = ? WHERE id = 1 AND holder = ?`,
		time.Now().UnixMilli(), h.id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func newHolderID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("mint a lease holder id: %w", err)
	}
	return "w-" + hex.EncodeToString(b), nil
}
