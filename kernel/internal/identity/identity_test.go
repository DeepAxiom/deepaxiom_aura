package identity

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A node's id appears in every event log and every federated proxy manifest,
// so "it survives a restart" is load-bearing for anything that reads history
// back.

func TestNodeIdentityIsCreatedOnceAndReloaded(t *testing.T) {
	dir := t.TempDir()

	first, err := Load(dir, ModeLocal)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if first.ID == "" {
		t.Fatal("a node was created with no id")
	}

	second, err := Load(dir, ModeLocal)
	if err != nil {
		t.Fatalf("Load (second): %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("node id changed across restarts: %q then %q", first.ID, second.ID)
	}
}

func TestDifferentDataDirsAreDifferentNodes(t *testing.T) {
	a, err := Load(t.TempDir(), ModeLocal)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Load(t.TempDir(), ModeLocal)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == b.ID {
		t.Fatal("two independent data dirs produced the same node id")
	}
}

func TestModeIsCarriedAndDefaultsToLocal(t *testing.T) {
	dir := t.TempDir()

	for _, mode := range []Mode{ModeLocal, ModeSite, ModePublished} {
		node, err := Load(dir, mode)
		if err != nil {
			t.Fatalf("Load(%q): %v", mode, err)
		}
		if node.Mode != mode {
			t.Errorf("mode = %q, want %q", node.Mode, mode)
		}
	}

	// An empty mode is the safe default rather than an error: `aura up` with no
	// --mode has to work.
	node, err := Load(dir, "")
	if err != nil {
		t.Fatalf("Load(\"\"): %v", err)
	}
	if node.Mode != ModeLocal {
		t.Errorf("empty mode became %q; want local", node.Mode)
	}
}

// The mode is a runtime choice, not a property of the data dir: the same node
// can be started local today and published tomorrow without becoming a
// different node.
func TestModeDoesNotChangeTheNodeIdentity(t *testing.T) {
	dir := t.TempDir()

	local, err := Load(dir, ModeLocal)
	if err != nil {
		t.Fatal(err)
	}
	published, err := Load(dir, ModePublished)
	if err != nil {
		t.Fatal(err)
	}
	if local.ID != published.ID {
		t.Fatalf("changing mode changed the node id: %q vs %q", local.ID, published.ID)
	}
}

func TestIdentityIsPersistedWhereItCanBeFound(t *testing.T) {
	dir := t.TempDir()
	node, err := Load(dir, ModeLocal)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "node-id"))
	if err != nil {
		t.Fatalf("node id was not persisted: %v", err)
	}
	if strings.TrimSpace(string(raw)) != node.ID {
		t.Errorf("persisted %q but reported %q", strings.TrimSpace(string(raw)), node.ID)
	}
}

func TestLoadCreatesTheDataDir(t *testing.T) {
	// First run on a fresh machine: the directory does not exist yet.
	dir := filepath.Join(t.TempDir(), "nested", "aura")
	if _, err := Load(dir, ModeLocal); err != nil {
		t.Fatalf("Load could not create its data dir: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("data dir was not created: %v", err)
	}
}

func TestSessionIdsAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := NewSessionID()
		if seen[id] {
			t.Fatalf("duplicate session id after %d draws: %q", i, id)
		}
		seen[id] = true
		if !strings.HasPrefix(id, "sess-") {
			t.Fatalf("session id %q is not recognisable as one", id)
		}
	}
}
