package lease

// Standby — taking over from a leader that died.
//
// # What was missing
//
// The lease already made one thing true: two processes cannot both be the
// writer. It also already made a *restart* work: a replacement waits out the
// dead holder's claim and starts. What nothing did was stand ready. A node
// was one process, and when it died the recovery time was "however long until
// something starts another one, plus the TTL".
//
// A standby is a second process pointed at the same data directory that holds
// no claim, opens nothing it could damage, and takes the lease the moment it
// is free.
//
// # Two ways to learn the leader is gone
//
// **The heartbeat went stale.** This is the only evidence available about a
// leader on another machine, or one that is alive but stalled. It costs a full
// TTL, by construction: the TTL *is* the promise not to displace a holder that
// merely paused.
//
// **The process no longer exists.** When the standby runs on the same host as
// the leader, it does not have to wait for a heartbeat to age: it can ask the
// operating system whether that process is there. A leader that was SIGKILLed,
// OOM-killed or crashed is gone in a way that is checkable in microseconds,
// and waiting ten seconds to conclude what is already certain is ten seconds
// of outage for nothing. Measured on this code, that is the difference between
// a takeover in about a tenth of a second and one in ten.
//
// # Why the second check cannot cause a split brain
//
// Taking a lease from a holder that is in fact alive is the one thing this
// package exists to prevent, so the process check is only trusted when it
// cannot be wrong in that direction:
//
//   - The holder recorded the host it ran on, and it is this host.
//   - The holder recorded its PID namespace, and it is this one. Two
//     containers sharing a volume can share a hostname and still have
//     unrelated PID tables; "PID 1 does not exist here" would say nothing
//     about a leader in the other. Where the namespace cannot be read on
//     either side, the check is not used.
//   - The operating system says no such process exists.
//
// The errors that remain all fall the safe way. A PID reused by an unrelated
// process reads as "alive" and the standby waits out the TTL, as it would
// have anyway. A row written by a build that predates these columns has no
// PID and is never judged dead by this route.
//
// And the takeover itself is still a compare-and-swap on the holder id, so two
// standbys reaching the same conclusion cannot both win.

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"
)

// ErrStandbyStopped is returned when a standby is told to stop waiting.
var ErrStandbyStopped = errors.New("stopped while standing by")

// standbyPoll is how often a standby looks at the lease. Short, because on
// the same host this *is* the failover time, and cheap, because it is one
// indexed read of a one-row table.
const standbyPoll = 100 * time.Millisecond

// self describes this process the way a holder records it.
type self struct {
	pid  int
	host string
	ns   string
}

func whoAmI() self {
	host, _ := os.Hostname()
	return self{pid: os.Getpid(), host: host, ns: pidNamespace()}
}

// HolderGone reports whether the holder's process is known not to exist.
//
// False means "cannot tell" at least as often as it means "alive": a holder
// on another host, in another PID namespace, or written by an older build all
// answer false, and the caller falls back to the heartbeat.
func (i Info) HolderGone() bool {
	me := whoAmI()
	if i.PID <= 0 || i.Host == "" || i.Host != me.host {
		return false
	}
	if i.NS != me.ns {
		return false
	}
	if i.PID == me.pid {
		return false
	}
	return !processExists(i.PID)
}

// takeFromGone replaces a holder whose process no longer exists. The WHERE
// clause names that holder, so it only applies if the row is still theirs.
func takeFromGone(db *sql.DB, gone string, ttl time.Duration) (*Holder, error) {
	id, err := newHolderID()
	if err != nil {
		return nil, err
	}
	me := whoAmI()
	now := time.Now()
	res, err := db.Exec(`
UPDATE node_lease SET holder = ?, since = ?, heartbeat = ?, ttl_ms = ?, pid = ?, host = ?, ns = ?
WHERE id = 1 AND holder = ?`,
		id, now.UnixMilli(), now.UnixMilli(), ttl.Milliseconds(), me.pid, me.host, me.ns, gone)
	if err != nil {
		return nil, fmt.Errorf("take the lease from a dead holder: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrHeld // someone else got there first
	}
	return startHolder(db, id, ttl, now), nil
}

// StandbyResult says how a standby came to hold the lease.
type StandbyResult struct {
	// Waited is how long it stood by.
	Waited time.Duration
	// From is the holder it replaced, or "" if the lease was simply free.
	From string
	// ProcessGone is true when the takeover happened because the previous
	// holder's process no longer existed, rather than because its heartbeat
	// expired.
	ProcessGone bool
}

// AcquireStandby waits, without bound, until this process can become the
// writer: the lease is free, the holder's heartbeat has expired, or the
// holder's process is known to be gone.
//
// `stop` ends the wait with ErrStandbyStopped. `watching`, if non-nil, is told
// once who is being stood by for, the first time the lease is found held.
func AcquireStandby(db *sql.DB, ttl time.Duration, stop <-chan struct{},
	watching func(Info)) (*Holder, StandbyResult, error) {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	started := time.Now()
	result := StandbyResult{}
	told := false
	for {
		h, err := Acquire(db, ttl)
		if err == nil {
			result.Waited = time.Since(started)
			return h, result, nil
		}
		if !errors.Is(err, ErrHeld) {
			return nil, result, err
		}

		info, readErr := Current(db)
		if readErr == nil && info.Holder != "" {
			result.From = info.Holder
			if !told && watching != nil {
				watching(info)
				told = true
			}
			if info.HolderGone() {
				h, err := takeFromGone(db, info.Holder, ttl)
				if err == nil {
					result.Waited = time.Since(started)
					result.ProcessGone = true
					return h, result, nil
				}
				if !errors.Is(err, ErrHeld) {
					return nil, result, err
				}
			}
		}

		select {
		case <-stop:
			return nil, result, ErrStandbyStopped
		case <-time.After(standbyPoll):
		}
	}
}
