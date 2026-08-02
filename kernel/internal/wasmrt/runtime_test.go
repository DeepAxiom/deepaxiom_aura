package wasmrt

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The guest fixture (testdata/guest) is compiled once, for real, with
// GOOS=wasip1 GOARCH=wasm — not mocked. This package's whole reason to
// exist is "does the sandbox actually hold", so its tests run a real wasm
// binary through it, the same way ledger's tests seal real entries and
// executor's undo tests build real sessions rather than asserting against
// a stub.
var (
	guestOnce  sync.Once
	guestBytes []byte
	guestErr   error
)

func buildGuest(t *testing.T) []byte {
	t.Helper()
	guestOnce.Do(func() {
		dir := filepath.Join(os.TempDir(), "aura-wasmrt-test-guest")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			guestErr = err
			return
		}
		out := filepath.Join(dir, "guest.wasm")
		cmd := exec.Command("go", "build", "-o", out, "./testdata/guest")
		cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
		if raw, err := cmd.CombinedOutput(); err != nil {
			guestErr = err
			t.Logf("go build (GOOS=wasip1 GOARCH=wasm) failed:\n%s", raw)
			return
		}
		guestBytes, guestErr = os.ReadFile(out)
	})
	if guestErr != nil {
		t.Skipf("cannot build the wasip1 test guest in this environment: %v", guestErr)
	}
	return guestBytes
}

func testModule(t *testing.T) *Module {
	t.Helper()
	ctx := context.Background()
	rt, err := New(ctx)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	mod, err := rt.Compile(ctx, buildGuest(t))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return mod
}

func req(t *testing.T, v map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	return raw
}

// --- happy path --------------------------------------------------------------

func TestInvokeHappyPath(t *testing.T) {
	mod := testModule(t)
	stdout, stderr, exitCode, err := mod.Invoke(context.Background(),
		req(t, map[string]any{"mode": "echo", "text": "hello"}), Permission{})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, want 0; stderr=%s", exitCode, stderr)
	}
	var resp struct {
		OK   bool   `json:"ok"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(stdout, &resp); err != nil {
		t.Fatalf("unmarshal stdout %q: %v", stdout, err)
	}
	if !resp.OK || resp.Text != "HELLO" {
		t.Fatalf("response = %+v, want ok=true text=HELLO", resp)
	}
}

// --- a guest that fails on purpose --------------------------------------------

func TestInvokeSurfacesANonZeroExitAndStderr(t *testing.T) {
	mod := testModule(t)
	_, stderr, exitCode, err := mod.Invoke(context.Background(),
		req(t, map[string]any{"mode": "fail"}), Permission{})
	if err != nil {
		t.Fatalf("Invoke: %v (a guest's own non-zero exit must not surface as err)", err)
	}
	if exitCode != 3 {
		t.Fatalf("exitCode = %d, want 3", exitCode)
	}
	if !strings.Contains(string(stderr), "guest failed on purpose") {
		t.Fatalf("stderr = %q, want it to contain the guest's own message", stderr)
	}
}

// --- adversarial: filesystem denied by default --------------------------------

func TestInvokeFilesystemDeniedByDefault(t *testing.T) {
	mod := testModule(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "should-not-be-writable.txt")

	_, stderr, exitCode, err := mod.Invoke(context.Background(),
		req(t, map[string]any{"mode": "fswrite", "path": "/should-not-be-writable.txt", "text": "x"}),
		Permission{}) // no grant
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if exitCode == 0 {
		t.Fatalf("a guest with no filesystem grant was allowed to write a file; stderr=%s", stderr)
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Fatal("the file was created on the host despite no grant — the kernel, not just the guest, must never touch it")
	}
}

// --- adversarial: filesystem granted, scoped exactly to the preopen ----------

func TestInvokeFilesystemGrantedIsScopedToThePreopen(t *testing.T) {
	mod := testModule(t)
	dir := t.TempDir()

	// Write inside the granted directory: must succeed.
	_, stderr, exitCode, err := mod.Invoke(context.Background(),
		req(t, map[string]any{"mode": "fswrite", "path": "/inside.txt", "text": "granted"}),
		Permission{FSPath: dir, FSWritable: true})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("write inside the granted directory failed: exit=%d stderr=%s", exitCode, stderr)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "inside.txt")); statErr != nil {
		t.Fatalf("the file the guest wrote is not where the grant pointed: %v", statErr)
	}

	// Read it back through the same grant: must succeed and match.
	stdout, stderr, exitCode, err := mod.Invoke(context.Background(),
		req(t, map[string]any{"mode": "fsread", "path": "/inside.txt"}),
		Permission{FSPath: dir})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("read inside the granted directory failed: exit=%d stderr=%s", exitCode, stderr)
	}
	var resp struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(stdout, &resp)
	if resp.Text != "granted" {
		t.Fatalf("read back %q, want %q", resp.Text, "granted")
	}

	// A path outside the preopen — escaping via ".." — must still fail: the
	// grant is one directory, not the whole host filesystem reachable from it.
	_, _, escapeExit, err := mod.Invoke(context.Background(),
		req(t, map[string]any{"mode": "fsread", "path": "/../etc/hosts"}),
		Permission{FSPath: dir})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if escapeExit == 0 {
		t.Fatal("a path escaping the granted directory via .. was readable")
	}
}

// A grant given as read-only must not allow a write, even to a path inside it.
func TestInvokeReadOnlyGrantRefusesWrites(t *testing.T) {
	mod := testModule(t)
	dir := t.TempDir()

	_, stderr, exitCode, err := mod.Invoke(context.Background(),
		req(t, map[string]any{"mode": "fswrite", "path": "/blocked.txt", "text": "x"}),
		Permission{FSPath: dir, FSWritable: false})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if exitCode == 0 {
		t.Fatal("a write succeeded through a read-only grant")
	}
	_ = stderr
	if _, statErr := os.Stat(filepath.Join(dir, "blocked.txt")); statErr == nil {
		t.Fatal("a file was created on the host through a read-only grant")
	}
}

// --- adversarial: a runaway guest is killed, not left to hang the kernel -----

func TestInvokeKillsARunawayGuest(t *testing.T) {
	mod := testModule(t)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, _, exitCode, err := mod.Invoke(ctx, req(t, map[string]any{"mode": "loop"}), Permission{})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("an infinite loop returned with no error — it should have been interrupted")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded — a caller must be able to tell "+
			"'the sandbox killed this' apart from any exit code the guest could have chosen itself", err)
	}
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, want 0 — wazero's internal sentinel exit code must not leak "+
			"out and be mistaken for the guest's own choice", exitCode)
	}
	// Generous upper bound: this proves the guest was actually killed near
	// the timeout, not that the test merely didn't hang forever.
	if elapsed > 5*time.Second {
		t.Fatalf("Invoke took %s to return after a 500ms timeout — the guest was not really interrupted", elapsed)
	}
}
