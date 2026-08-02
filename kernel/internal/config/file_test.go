package config

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "aura.config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

// An empty path means "no file", not "a missing file" — `aura up` without
// --config has to work, and callers pass "" for exactly that.
func TestNoPathYieldsAnEmptyLayer(t *testing.T) {
	f, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\"): %v", err)
	}
	if f.Skills == nil {
		t.Fatal("Skills is nil; every caller indexes it without checking")
	}
	if len(f.Skills) != 0 {
		t.Errorf("Skills = %v; want empty", f.Skills)
	}
}

// A path that was given and cannot be read is an error, because the operator
// asked for that file and running without it would silently change behaviour.
func TestMissingFileIsAnError(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("a --config path that does not exist was ignored rather than reported")
	}
}

func TestMalformedYAMLIsAnError(t *testing.T) {
	if _, err := Load(write(t, "skills: [unclosed\n")); err == nil {
		t.Fatal("malformed YAML was accepted")
	}
}

func TestValuesAreReadPerSkill(t *testing.T) {
	f, err := Load(write(t, `
skills:
  "acme/cognitive/chat":
    temperature: 0.2
    max_tokens: 512
  "acme/motor/tts":
    voice: "en-GB"
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	chat := f.Skills["acme/cognitive/chat"]
	if chat["temperature"] != 0.2 {
		t.Errorf("temperature = %v (%T); want 0.2", chat["temperature"], chat["temperature"])
	}
	if chat["max_tokens"] != 512 {
		t.Errorf("max_tokens = %v (%T); want 512", chat["max_tokens"], chat["max_tokens"])
	}
	if f.Skills["acme/motor/tts"]["voice"] != "en-GB" {
		t.Errorf("voice = %v", f.Skills["acme/motor/tts"]["voice"])
	}
}

// A file with no `skills:` key is valid — it is just an empty layer — and must
// still hand back a usable map.
func TestFileWithoutSkillsKeyIsUsable(t *testing.T) {
	f, err := Load(write(t, "# nothing here yet\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if f.Skills == nil {
		t.Fatal("Skills is nil for a file with no skills key")
	}
	if len(f.Skills["anything"]) != 0 {
		t.Error("an absent skill returned values")
	}
}
