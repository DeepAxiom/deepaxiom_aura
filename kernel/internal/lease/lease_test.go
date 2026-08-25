package lease

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// What a lease has to be true about: exactly one writer at a time, a dead
// writer's claim expires rather than wedging the directory forever, and a
// writer that lost the claim finds out — because a lease that can be lost
// silently is worse than none, having replaced a visible conflict with an
// invisible one.

const testTTL = 150 * time.Millisecond

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "kernel.db") +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestASecondProcessIsRefusedWhileTheFirstIsAlive(t *testing.T) {
	db := testDB(t)

	first, err := Acquire(db, testTTL)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	defer first.Release()

	_, err = Acquire(db, testTTL)
	if err == nil {
		t.Fatal("two processes hold the same data directory")
	}
	if !errors.Is(err, ErrHeld) {
		t.Errorf("wrong error kind: %v", err)
	}
	// The refusal has to name the holder, or an operator staring at two
	// containers has no way to tell which one to stop.
	if !strings.Contains(err.Error(), first.ID()) {
		t.Errorf("the refusal does not name the holder %s: %v", first.ID(), err)
	}
}

func TestReleasingLetsAReplacementStartImmediately(t *testing.T) {
	db := testDB(t)

	first, err := Acquire(db, testTTL)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	if err := first.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}

	// Immediately, not after the TTL: a clean shutdown should not cost a
	// rolling deploy ten seconds of downtime.
	second, err := Acquire(db, testTTL)
	if err != nil {
		t.Fatalf("Acquire after a clean release: %v", err)
	}
	defer second.Release()
}

func TestADeadHoldersClaimExpires(t *testing.T) {
	db := testDB(t)

	dead, err := Acquire(db, testTTL)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	// A process that died without releasing: its renewal loop is gone, so the
	// heartbeat stops. Simulated by stopping the loop and ageing the row.
	stopRenewals(t, dead)
	ageHeartbeat(t, db, 2*testTTL)

	taken, err := Acquire(db, testTTL)
	if err != nil {
		t.Fatalf("a stale lease did not expire: %v", err)
	}
	defer taken.Release()
	if taken.ID() == dead.ID() {
		t.Error("the replacement reused the dead holder's identity")
	}
}

func TestAHolderThatLosesTheLeaseIsToldSo(t *testing.T) {
	db := testDB(t)

	original, err := Acquire(db, testTTL)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	// The shape of a stalled process: still running, heartbeat gone stale,
	// somebody else took over. This is exactly the split brain the fence
	// exists to stop, so the original must find out on its next renewal.
	ageHeartbeat(t, db, 2*testTTL)
	successor, err := Acquire(db, testTTL)
	if err != nil {
		t.Fatalf("takeover: %v", err)
	}
	defer successor.Release()

	select {
	case <-original.Lost():
	case <-time.After(2 * time.Second):
		t.Fatal("the displaced holder was never told it had lost the lease")
	}
}

func TestADisplacedHolderDoesNotDeleteItsSuccessorsClaim(t *testing.T) {
	db := testDB(t)

	original, err := Acquire(db, testTTL)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	ageHeartbeat(t, db, 2*testTTL)
	successor, err := Acquire(db, testTTL)
	if err != nil {
		t.Fatalf("takeover: %v", err)
	}
	<-original.Lost()

	// The displaced process shuts down and cleans up after itself. It must not
	// take the successor's claim with it, or a third process could start
	// beside the one that is legitimately running.
	if err := original.Release(); err != nil {
		t.Fatalf("Release after losing: %v", err)
	}
	held, err := Current(db)
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if held.Holder != successor.ID() {
		t.Errorf("the successor's claim is gone: row holds %q, expected %q",
			held.Holder, successor.ID())
	}
	if _, err := Acquire(db, testTTL); !errors.Is(err, ErrHeld) {
		t.Error("a third process could start beside the live successor")
	}
}

func TestALiveHolderKeepsItsClaimAcrossSeveralTTLs(t *testing.T) {
	db := testDB(t)

	held, err := Acquire(db, testTTL)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer held.Release()

	// Renewals must actually run: a lease that expires under a healthy node
	// would hand the directory to a standby for no reason.
	time.Sleep(3 * testTTL)
	select {
	case <-held.Lost():
		t.Fatal("a live holder lost its lease to its own renewal loop")
	default:
	}
	if _, err := Acquire(db, testTTL); !errors.Is(err, ErrHeld) {
		t.Error("a live holder's lease was takeable")
	}
}

func TestCurrentOnAnUnclaimedDirectoryIsEmptyNotAnError(t *testing.T) {
	db := testDB(t)
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	info, err := Current(db)
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if info.Holder != "" {
		t.Errorf("an unclaimed directory reported holder %q", info.Holder)
	}
}

func TestReleaseIsSafeToCallTwice(t *testing.T) {
	db := testDB(t)
	held, err := Acquire(db, testTTL)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := held.Release(); err != nil {
		t.Fatalf("first Release: %v", err)
	}
	if err := held.Release(); err != nil {
		t.Fatalf("second Release: %v", err)
	}
}

// ─────────────────────────── helpers ───────────────────────────

// stopRenewals halts a holder's heartbeat without releasing its row — what a
// process that was SIGKILLed leaves behind.
func stopRenewals(t *testing.T, h *Holder) {
	t.Helper()
	select {
	case <-h.stop:
	default:
		close(h.stop)
	}
	h.wg.Wait()
}

// ageHeartbeat backdates the current claim so it reads as stale, standing in
// for wall-clock time the test should not have to spend.
func ageHeartbeat(t *testing.T, db *sql.DB, by time.Duration) {
	t.Helper()
	if _, err := db.Exec(`UPDATE node_lease SET heartbeat = ? WHERE id = 1`,
		time.Now().Add(-by).UnixMilli()); err != nil {
		t.Fatalf("age the heartbeat: %v", err)
	}
}
