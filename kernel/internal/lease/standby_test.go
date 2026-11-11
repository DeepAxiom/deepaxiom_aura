package lease

import (
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"
)

// A long TTL throughout: these tests are about a standby that does NOT have
// to wait for a heartbeat to age, so the heartbeat must never be the reason
// one of them passes.
const longTTL = time.Hour

// deadPID returns the id of a process that has exited.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start a throwaway process: %v", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Wait()
	// The OS may take a moment to stop reporting it.
	deadline := time.Now().Add(5 * time.Second)
	for processExists(pid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if processExists(pid) {
		t.Skip("the OS still reports the exited process; cannot construct a dead PID here")
	}
	return pid
}

// plant writes a lease row as some other process would have, with a fresh
// heartbeat and a TTL nowhere near expiring.
func plant(t *testing.T, db *sql.DB, holder string, pid int, host, ns string) {
	t.Helper()
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	if _, err := db.Exec(`INSERT OR REPLACE INTO node_lease
		(id, holder, since, heartbeat, ttl_ms, pid, host, ns) VALUES (1, ?, ?, ?, ?, ?, ?, ?)`,
		holder, now, now, longTTL.Milliseconds(), pid, host, ns); err != nil {
		t.Fatal(err)
	}
}

func standbyWithin(t *testing.T, db *sql.DB, limit time.Duration) (*Holder, StandbyResult, error) {
	t.Helper()
	stop := make(chan struct{})
	timer := time.AfterFunc(limit, func() { close(stop) })
	defer timer.Stop()
	return AcquireStandby(db, longTTL, stop, nil)
}

func TestProcessExists(t *testing.T) {
	if !processExists(os.Getpid()) {
		t.Fatal("this process is reported as not existing")
	}
	if processExists(deadPID(t)) {
		t.Fatal("an exited process is reported as existing")
	}
}

// The point of the whole file: a leader that was killed is replaced at once,
// not after its TTL.
func TestAStandbyTakesOverFromADeadProcessWithoutWaitingForTheTTL(t *testing.T) {
	db := testDB(t)
	me := whoAmI()
	plant(t, db, "w-dead", deadPID(t), me.host, me.ns)

	started := time.Now()
	h, result, err := standbyWithin(t, db, 10*time.Second)
	if err != nil {
		t.Fatalf("standby did not take over from a dead process: %v", err)
	}
	defer h.Release()
	took := time.Since(started)
	t.Logf("takeover from a dead process: %v (TTL was %v)", took.Round(time.Millisecond), longTTL)
	if took > 3*time.Second {
		t.Fatalf("takeover took %v; it should not depend on the TTL at all", took)
	}
	if !result.ProcessGone || result.From != "w-dead" {
		t.Fatalf("result %+v; want a takeover from w-dead because its process is gone", result)
	}
	info, _ := Current(db)
	if info.Holder != h.ID() || info.PID != os.Getpid() {
		t.Fatalf("the row names %s pid %d after the takeover", info.Holder, info.PID)
	}
}

// The safety half. Each of these is a holder that may well be alive, and a
// standby that took its lease would be a second writer.
func TestAStandbyNeverDisplacesAHolderItCannotProveIsGone(t *testing.T) {
	me := whoAmI()
	dead := deadPID(t)
	cases := []struct {
		name string
		pid  int
		host string
		ns   string
	}{
		{"a live process on this host", os.Getpid() + 0, me.host, me.ns},
		{"a dead PID, but on another host", dead, me.host + "-elsewhere", me.ns},
		{"a dead PID, but in another PID namespace", dead, me.host, me.ns + "-other"},
		{"a row from a build that recorded no process", 0, "", ""},
		{"a host with no PID recorded", 0, me.host, me.ns},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := testDB(t)
			pid := tc.pid
			if tc.name == "a live process on this host" {
				// Another live process, not this one: the parent.
				pid = os.Getppid()
				if pid <= 0 || !processExists(pid) {
					t.Skip("no live parent process to stand in for a leader")
				}
			}
			plant(t, db, "w-alive", pid, tc.host, tc.ns)

			_, _, err := standbyWithin(t, db, 400*time.Millisecond)
			if !errors.Is(err, ErrStandbyStopped) {
				t.Fatalf("the standby took a lease it had no proof was abandoned (err=%v)", err)
			}
			if info, _ := Current(db); info.Holder != "w-alive" {
				t.Fatalf("the holder was replaced by %s", info.Holder)
			}
		})
	}
}

func TestHolderGoneIsFalseForThisVeryProcess(t *testing.T) {
	me := whoAmI()
	if (Info{PID: me.pid, Host: me.host, NS: me.ns}).HolderGone() {
		t.Fatal("a holder with this process's own PID was judged gone")
	}
}

// A leader on another machine: only the heartbeat can say it is gone, and the
// standby still takes over once it has.
func TestAStandbyStillTakesOverWhenTheHeartbeatExpires(t *testing.T) {
	db := testDB(t)
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-time.Second).UnixMilli()
	if _, err := db.Exec(`INSERT INTO node_lease (id, holder, since, heartbeat, ttl_ms, pid, host, ns)
		VALUES (1, 'w-remote', ?, ?, 200, 4242, 'another-machine', '')`, stale, stale); err != nil {
		t.Fatal(err)
	}
	h, result, err := standbyWithin(t, db, 5*time.Second)
	if err != nil {
		t.Fatalf("standby: %v", err)
	}
	defer h.Release()
	if result.ProcessGone {
		t.Fatal("a takeover from another machine was attributed to a process check")
	}
}

func TestAStandbyOnAFreeDirectoryStartsAtOnce(t *testing.T) {
	db := testDB(t)
	h, result, err := standbyWithin(t, db, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Release()
	if result.From != "" || result.ProcessGone {
		t.Fatalf("result %+v for a directory nobody held", result)
	}
}

// Two standbys reach the same conclusion at the same moment. One wins.
func TestTwoStandbysCannotBothTakeOver(t *testing.T) {
	db := testDB(t)
	me := whoAmI()
	plant(t, db, "w-dead", deadPID(t), me.host, me.ns)

	type outcome struct {
		h   *Holder
		err error
	}
	results := make(chan outcome, 2)
	for i := 0; i < 2; i++ {
		go func() {
			h, _, err := standbyWithin(t, db, 1500*time.Millisecond)
			results <- outcome{h, err}
		}()
	}
	won := 0
	for i := 0; i < 2; i++ {
		r := <-results
		if r.err == nil {
			won++
			defer r.h.Release()
		} else if !errors.Is(r.err, ErrStandbyStopped) {
			t.Fatalf("unexpected error: %v", r.err)
		}
	}
	if won != 1 {
		t.Fatalf("%d standbys hold the lease; exactly one may", won)
	}
}

// A directory written before the location columns existed must still open,
// and its holder must still be honoured.
func TestALeaseTableFromAnOlderBuildIsMigratedInPlace(t *testing.T) {
	db := testDB(t)
	if _, err := db.Exec(`CREATE TABLE node_lease (
		id INTEGER PRIMARY KEY CHECK (id = 1), holder TEXT NOT NULL,
		since INTEGER NOT NULL, heartbeat INTEGER NOT NULL,
		ttl_ms INTEGER NOT NULL DEFAULT 10000)`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	if _, err := db.Exec(`INSERT INTO node_lease (id, holder, since, heartbeat, ttl_ms)
		VALUES (1, 'w-old', ?, ?, 3600000)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(db, testTTL); !errors.Is(err, ErrHeld) {
		t.Fatalf("an older build's live holder was not honoured: %v", err)
	}
	info, err := Current(db)
	if err != nil || info.Holder != "w-old" || info.PID != 0 || info.Host != "" {
		t.Fatalf("migrated row reads %+v (err %v)", info, err)
	}
	if info.HolderGone() {
		t.Fatal("a holder that recorded no process was judged gone")
	}
}
