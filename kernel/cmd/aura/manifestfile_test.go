package main

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

const testManifest = "id: \"acme/logical/echo\"\nversion: \"1.0.0\"\n"

func putFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func zipNames(t *testing.T, artifact []byte) map[string]string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(artifact), int64(len(artifact)))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(rc)
		rc.Close()
		out[f.Name] = buf.String()
	}
	return out
}

func TestManifestIsReadUnderEitherName(t *testing.T) {
	current := t.TempDir()
	putFile(t, filepath.Join(current, "expertise.yaml"), testManifest)
	if _, name, err := readManifestFile(current); err != nil || name != "expertise.yaml" {
		t.Fatalf("expertise.yaml: name=%q err=%v", name, err)
	}

	// A package written before the rename is untouched by it.
	legacy := t.TempDir()
	putFile(t, filepath.Join(legacy, "skill.yaml"), testManifest)
	raw, name, err := readManifestFile(legacy)
	if err != nil || name != "skill.yaml" || string(raw) != testManifest {
		t.Fatalf("skill.yaml: name=%q err=%v", name, err)
	}
	if got := skillID(legacy); got != "acme/logical/echo" {
		t.Fatalf("id read from a legacy manifest = %q", got)
	}

	// Both present: the current name wins, so a package mid-migration has
	// one answer.
	both := t.TempDir()
	putFile(t, filepath.Join(both, "expertise.yaml"), "id: \"acme/logical/new\"\n")
	putFile(t, filepath.Join(both, "skill.yaml"), "id: \"acme/logical/old\"\n")
	if got := skillID(both); got != "acme/logical/new" {
		t.Fatalf("with both files the id is %q", got)
	}

	if _, _, err := readManifestFile(t.TempDir()); err == nil {
		t.Fatal("a directory with no manifest was accepted")
	}
}

// What a current node publishes must still install on a node, and run on an
// SDK, that only knows the old file name.
func TestPublishedArtifactCarriesTheLegacyManifestName(t *testing.T) {
	dir := t.TempDir()
	putFile(t, filepath.Join(dir, "expertise.yaml"), testManifest)
	putFile(t, filepath.Join(dir, "main.py"), "print('hi')\n")

	artifact, files, err := zipSkillDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := zipNames(t, artifact)
	if names["skill.yaml"] != testManifest || names["expertise.yaml"] != testManifest {
		t.Fatalf("artifact holds %v; it needs the manifest under both names", keys(names))
	}
	if files != 3 {
		t.Fatalf("file count %d, want 3", files)
	}

	// A legacy package is zipped exactly as it always was: nothing added.
	old := t.TempDir()
	putFile(t, filepath.Join(old, "skill.yaml"), testManifest)
	artifact, files, err = zipSkillDir(old)
	if err != nil {
		t.Fatal(err)
	}
	if names := zipNames(t, artifact); len(names) != 1 || files != 1 {
		t.Fatalf("a legacy package's artifact changed: %v", keys(names))
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestExamplesDirFollowsAnOlderCheckout(t *testing.T) {
	root := t.TempDir()
	prev, _ := os.Getwd()
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(prev)

	if got := examplesDirOrLegacy("expertise"); got != "expertise" {
		t.Fatalf("with neither directory: %q", got)
	}
	_ = os.Mkdir("skills", 0o755)
	if got := examplesDirOrLegacy("expertise"); got != "skills" {
		t.Fatalf("an older checkout has skills/ and the default resolved to %q", got)
	}
	_ = os.Mkdir("expertise", 0o755)
	if got := examplesDirOrLegacy("expertise"); got != "expertise" {
		t.Fatalf("with both: %q", got)
	}
	if got := examplesDirOrLegacy("custom"); got != "custom" {
		t.Fatalf("an explicit directory was second-guessed: %q", got)
	}
}
