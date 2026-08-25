package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"aura/kernel/internal/lease"
	"aura/kernel/internal/ledger"
	"aura/kernel/internal/signing"
	"aura/kernel/internal/store"
)

// How long a node takes to come back.
//
// This exists because the failover question was being argued without the
// number. "A node is one process and nothing restarts it" is true and says
// nothing about whether that matters: if a supervised restart is back in two
// seconds, the honest answer for most deployments is a restart policy and a
// written degradation contract, and a warm standby buys very little. If it is
// two minutes, the answer is different.
//
// So these are measurements first and assertions second. The bounds are loose
// on purpose — they exist to catch a regression that changes the *shape* of
// recovery (a scan that became linear in the ledger, a lease that stopped
// expiring), not to pin a number that varies with the machine. The numbers
// themselves go to `go test -v`, and the ones in the docs came from here.

// seedLedger fills a data directory with sealed effects, so the phases that
// grow with history are measured against something rather than against an
// empty file.
func seedLedger(t *testing.T, dir string, entries int) {
	t.Helper()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	keys, err := signing.LoadOrCreate(filepath.Join(dir, "identity"))
	if err != nil {
		t.Fatalf("keys: %v", err)
	}
	ldg, err := ledger.Open(st, "node-recovery", keys)
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	for i := 0; i < entries; i++ {
		if _, err := ldg.Seal(ledger.SealRequest{
			Session: "sess-load", Envelope: fmt.Sprintf("env-%d", i), Cause: "cause",
			Actor: "acme/motor/erp@1.0.0", Capability: "motor.erp.write",
			Decision: "allow", Outcome: "delivered",
			Policy: "sha256:policy", Payload: json.RawMessage(`{"amount":1}`),
		}); err != nil {
			t.Fatalf("seal %d: %v", i, err)
		}
	}
	if err := ldg.Checkpoint(); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
}

// buildOnce constructs a node against dir and returns what it cost.
func buildOnce(t *testing.T, dir string, port int) Startup {
	t.Helper()
	n, err := buildNode(nodeOptions{DataDir: dir, Port: port, NoAuth: true})
	if err != nil {
		t.Fatalf("buildNode: %v", err)
	}
	defer n.Close()
	return n.Startup
}

func TestRecoveryCostOnAColdDirectory(t *testing.T) {
	got := buildOnce(t, t.TempDir(), 9301)
	t.Logf("cold start: total %v (store %v, ledger %v, lease wait %v)",
		got.Total.Round(time.Millisecond), got.Store.Round(time.Millisecond),
		got.Ledger.Round(time.Millisecond), got.LeaseWait.Round(time.Millisecond))

	if got.LeaseWait > time.Second {
		t.Errorf("an unclaimed directory made the node wait %v for its lease", got.LeaseWait)
	}
	if got.Total > 15*time.Second {
		t.Errorf("cold start took %v — something is scanning that should not be", got.Total)
	}
}

func TestRecoveryCostGrowsGentlyWithLedgerSize(t *testing.T) {
	if testing.Short() {
		t.Skip("seeds thousands of ledger entries")
	}
	// The phase that could plausibly become the problem is rebuilding the RFC
	// 6962 tree over every sealed entry at ledger.Open. If that is linear with
	// a small constant, recovery stays flat for years; if it is not, this is
	// where it shows up first.
	small, large := t.TempDir(), t.TempDir()
	seedLedger(t, small, 200)
	seedLedger(t, large, 4000)

	gotSmall := buildOnce(t, small, 9302)
	gotLarge := buildOnce(t, large, 9303)

	t.Logf("   200 entries: total %v (ledger %v)",
		gotSmall.Total.Round(time.Millisecond), gotSmall.Ledger.Round(time.Millisecond))
	t.Logf(" 4,000 entries: total %v (ledger %v)",
		gotLarge.Total.Round(time.Millisecond), gotLarge.Ledger.Round(time.Millisecond))

	// Twenty times the entries must not cost anything like twenty times the
	// wall clock in *total* — the fixed costs dominate at this size, and a
	// regression that made the whole build scale with history would break this
	// long before it broke a production node.
	if gotLarge.Total > 20*time.Second {
		t.Errorf("a 4,000-entry ledger took %v to open", gotLarge.Total)
	}
}

func TestRecoveryFromACrashWaitsOutTheDeadHoldersLease(t *testing.T) {
	dir := t.TempDir()
	seedLedger(t, dir, 50)

	// A SIGKILLed node: the lease row is fresh and its holder is gone. Built
	// by taking the lease and abandoning it without releasing, which is
	// exactly what the dead process left behind.
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	dead, err := st.AcquireWriteLease(lease.DefaultTTL)
	if err != nil {
		t.Fatalf("stage the dead holder: %v", err)
	}
	_ = dead // deliberately never released
	st.Close()

	started := time.Now()
	n, err := buildNode(nodeOptions{DataDir: dir, Port: 9304, NoAuth: true})
	if err != nil {
		t.Fatalf("the replacement never started: %v", err)
	}
	defer n.Close()
	elapsed := time.Since(started)

	t.Logf("crash recovery: total %v (lease wait %v, store %v, ledger %v)",
		elapsed.Round(time.Millisecond), n.Startup.LeaseWait.Round(time.Millisecond),
		n.Startup.Store.Round(time.Millisecond), n.Startup.Ledger.Round(time.Millisecond))

	// This is the headline: after a crash, recovery is dominated by the lease
	// TTL, not by anything the node does. That makes the TTL the tuning knob
	// for recovery time, which is worth knowing before anyone builds a standby
	// to shave milliseconds off the parts that were never the cost.
	if n.Startup.LeaseWait < lease.DefaultTTL/2 {
		t.Errorf("the replacement started after only %v — it did not wait out the dead claim, "+
			"which means two writers overlapped", n.Startup.LeaseWait)
	}
	if elapsed > lease.DefaultTTL+10*time.Second {
		t.Errorf("crash recovery took %v, far past the lease TTL of %v",
			elapsed, lease.DefaultTTL)
	}
}

func TestACleanShutdownCostsNoLeaseWaitAtAll(t *testing.T) {
	dir := t.TempDir()
	seedLedger(t, dir, 50)

	// The rolling-deploy path: the outgoing node released its claim, so the
	// replacement must not pay the TTL. If this regresses, every deploy grows
	// a ten-second hole nobody asked for.
	first, err := buildNode(nodeOptions{DataDir: dir, Port: 9305, NoAuth: true})
	if err != nil {
		t.Fatalf("first node: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("clean shutdown: %v", err)
	}

	second := buildOnce(t, dir, 9306)
	t.Logf("rolling restart: total %v (lease wait %v)",
		second.Total.Round(time.Millisecond), second.LeaseWait.Round(time.Millisecond))

	if second.LeaseWait > time.Second {
		t.Errorf("a clean shutdown still cost the replacement %v of lease wait", second.LeaseWait)
	}
}

func TestAReplacementGivesUpRatherThanHangingOnALiveNode(t *testing.T) {
	dir := t.TempDir()

	live, err := buildNode(nodeOptions{DataDir: dir, Port: 9307, NoAuth: true})
	if err != nil {
		t.Fatalf("live node: %v", err)
	}
	defer live.Close()

	// A genuinely running node keeps renewing, so the claim never goes stale.
	// Waiting forever would turn "another node is already running" from an
	// error into a hang, so the wait is bounded and this must return.
	started := time.Now()
	_, err = buildNode(nodeOptions{
		DataDir: dir, Port: 9308, NoAuth: true, LeaseWait: 2 * time.Second,
	})
	if err == nil {
		t.Fatal("a second node started beside a live one")
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Errorf("the refusal took %v — the bound is not holding", elapsed)
	}
	t.Logf("refused a live directory after %v: %v", time.Since(started).Round(time.Millisecond), err)
}
