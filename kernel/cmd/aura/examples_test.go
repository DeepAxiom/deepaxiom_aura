package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"aura/kernel/internal/registry"
)

// fakeCatalog is a live catalogue with no node behind it.
type fakeCatalog []string

func (f fakeCatalog) Catalog() []registry.Manifest {
	out := make([]registry.Manifest, 0, len(f))
	for _, id := range f {
		out = append(out, registry.Manifest{ID: id})
	}
	return out
}

func writeSkill(t *testing.T, dir, id string) string {
	t.Helper()
	path := filepath.Join(dir, "skill.yaml")
	body := "id: \"" + id + "\"\nname: \"x\"\ncapability: \"logical.echo\"\ntype: logical\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSkillIDIsReadNotDerived(t *testing.T) {
	// skills/model-fit is example/sensorial/model-fit. Any rule that maps one
	// to the other breaks the first time somebody renames a folder, so the id
	// has to come out of the manifest.
	dir := t.TempDir()
	writeSkill(t, dir, "example/sensorial/model-fit")
	if got := skillID(dir); got != "example/sensorial/model-fit" {
		t.Fatalf("skillID = %q, want the declared id", got)
	}
}

func TestSkillIDIsEmptyWhenThereIsNoManifest(t *testing.T) {
	// An unreadable manifest must not be reported as "already connected" —
	// that would silently skip a skill the launcher was asked to start.
	if got := skillID(t.TempDir()); got != "" {
		t.Fatalf("skillID = %q, want empty for a directory with no skill.yaml", got)
	}
}

func TestAttachedIDsCountsConnectionsNotSkills(t *testing.T) {
	// Catalog() yields one manifest per connection, which is exactly what
	// makes a second copy of a skill visible.
	got := attachedIDs(fakeCatalog{"a/b/one", "a/b/one", "a/b/two"})
	want := map[string]int{"a/b/one": 2, "a/b/two": 1}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("attachedIDs = %v, want %v", got, want)
	}
}

func TestAttachedIDsToleratesNoRegistry(t *testing.T) {
	// startExamples is also reachable from paths that have no node yet.
	if got := attachedIDs(nil); len(got) != 0 {
		t.Fatalf("attachedIDs(nil) = %v, want empty", got)
	}
}

func TestReplicasNamesOnlyWhatIsDoubled(t *testing.T) {
	got := replicas(fakeCatalog{"a/b/one", "a/b/one", "a/b/two", "a/b/three", "a/b/three", "a/b/three"})
	want := []string{"a/b/one (2)", "a/b/three (3)"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("replicas = %v, want %v", got, want)
	}
}

func TestReplicasIsSilentOnAHealthyNode(t *testing.T) {
	// One connection per skill is the normal case and must produce no banner:
	// a warning that fires every time is a warning nobody reads.
	if got := replicas(fakeCatalog{"a/b/one", "a/b/two"}); len(got) != 0 {
		t.Fatalf("replicas = %v, want none", got)
	}
}

func TestStartExamplesLeavesAttachedSkillsAlone(t *testing.T) {
	// The whole point: a skill that outlived an earlier node and reconnected
	// here must not be started a second time.
	root := t.TempDir()
	one := filepath.Join(root, "already-up")
	two := filepath.Join(root, "not-up")
	for _, d := range []string{one, two} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		// A runnable skill needs a main.py for the launcher to consider it.
		if err := os.WriteFile(filepath.Join(d, "main.py"), []byte("import time; time.sleep(60)\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeSkill(t, one, "example/logical/already-up")
	writeSkill(t, two, "example/logical/not-up")

	stop, results, _ := startExamples(root, "ws://127.0.0.1:1/ws/skill", "",
		fakeCatalog{"example/logical/already-up"})
	defer stop()

	byName := map[string]exampleResult{}
	for _, r := range results {
		byName[r.name] = r
	}

	got, ok := byName["already-up"]
	if !ok {
		t.Fatal("the attached skill is missing from the report entirely")
	}
	if !got.attached || got.started {
		t.Fatalf("already-up = %+v, want attached and not started", got)
	}
	if other := byName["not-up"]; other.attached {
		t.Fatalf("not-up = %+v, want the launcher to actually start it", other)
	}
}
