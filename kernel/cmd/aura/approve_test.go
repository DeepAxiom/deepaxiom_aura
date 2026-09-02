package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aura/kernel/internal/ledger"
)

// `aura approve --shown` is where the C4 v1.7 context is actually produced, and
// the one property worth a test here is that it is produced *from the file the
// operator names*. The kernel can verify a digest binds a signature; nothing
// downstream can tell whether the digest describes what the person read. That
// is settled here or nowhere.

func TestShownHashesTheFileTheOperatorNamed(t *testing.T) {
	dir := t.TempDir()
	note := filepath.Join(dir, "note.html")
	if err := os.WriteFile(note, []byte("<p>Metformina 850 mg</p>"), 0o600); err != nil {
		t.Fatal(err)
	}

	shown, err := gatherShown([]string{"screen=" + note}, nil)
	if err != nil {
		t.Fatalf("gatherShown: %v", err)
	}
	if len(shown) != 1 || shown[0].Label != "screen" {
		t.Fatalf("gatherShown = %+v, want one screen entry", shown)
	}
	// sha256 of that exact string, computed independently of the code under
	// test. A digest the implementation agrees with itself about would pass
	// even if it hashed the path, or nothing.
	const want = "sha256:0a4d0d016543258e6b7c51de664c6b1a42ca17b3f10eab5f51664d68fab4445f"
	if shown[0].Digest != want {
		t.Errorf("digest = %s,\n     want %s", shown[0].Digest, want)
	}

	// Editing the file has to change it, or it is not binding the content.
	if err := os.WriteFile(note, []byte("<p>Metformina 1 g</p>"), 0o600); err != nil {
		t.Fatal(err)
	}
	after, err := gatherShown([]string{"screen=" + note}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if after[0].Digest == shown[0].Digest {
		t.Error("the digest survived an edit to the file, so it does not bind the content")
	}
}

func TestShownRefusesWhatCouldNotBeSigned(t *testing.T) {
	dir := t.TempDir()
	ok := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(ok, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		files   []string
		digests []string
	}{
		{"no equals sign", []string{"screen"}, nil},
		{"no path", []string{"screen="}, nil},
		{"missing file", []string{"screen=" + filepath.Join(dir, "nope.txt")}, nil},
		{"label the contract cannot carry", []string{"Screen=" + ok}, nil},
		{"digest that is not one", nil, []string{"screen=the note I read"}},
		{"digest with no algorithm", nil, []string{"screen=" + strings.Repeat("a", 64)}},
		{"same label twice", []string{"screen=" + ok}, []string{"screen=sha256:" + strings.Repeat("a", 64)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := gatherShown(c.files, c.digests); err == nil {
				t.Error("accepted, and would be refused by the node after a human had already answered")
			}
		})
	}
}

func TestAPrecomputedDigestIsTakenAsGiven(t *testing.T) {
	// Not every operator client renders to a file. One that already hashed what
	// it displayed can pass the digest, and the node cannot tell the difference
	// — which is correct: both come from the approver's side.
	digest := "sha256:" + strings.Repeat("4", 64)
	shown, err := gatherShown(nil, []string{"invoice=" + digest})
	if err != nil {
		t.Fatalf("gatherShown: %v", err)
	}
	if len(shown) != 1 || shown[0].Digest != digest {
		t.Fatalf("gatherShown = %+v, want the digest verbatim", shown)
	}
	if _, err := ledger.CanonicalContext(shown); err != nil {
		t.Errorf("what the CLI produced is not a context the ledger accepts: %v", err)
	}
}

func TestMissingLabelsNamesWhatTheNodeAskedFor(t *testing.T) {
	shown := []ledger.ContextEntry{{Label: "screen", Digest: "sha256:" + strings.Repeat("a", 64)}}
	if got := missingLabels([]string{"screen"}, shown); len(got) != 0 {
		t.Errorf("missingLabels = %v for a label that is bound", got)
	}
	got := missingLabels([]string{"screen", "certificate"}, shown)
	if len(got) != 1 || got[0] != "certificate" {
		t.Errorf("missingLabels = %v, want [certificate]", got)
	}
	if got := missingLabels(nil, nil); len(got) != 0 {
		t.Errorf("a node that requires nothing reported %v missing", got)
	}
}
