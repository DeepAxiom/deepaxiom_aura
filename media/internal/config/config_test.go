package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func base(dir string) Config {
	return Config{DSN: "postgres://x/y", Bind: "127.0.0.1:9090", DataDir: dir}
}

func TestValidateRefusesAnOpenNodeOnTheNetwork(t *testing.T) {
	dir := t.TempDir()
	ok := []string{"127.0.0.1:9090", "localhost:9090", "[::1]:9090"}
	for _, bind := range ok {
		c := base(dir)
		c.Bind, c.NoAuth = bind, true
		if err := c.Validate(); err != nil {
			t.Errorf("--no-auth on %s was refused: %v", bind, err)
		}
	}
	// Reachable from the network with no credential: anyone who can route to
	// this port could queue work onto a machine that encodes video.
	reachable := []string{"0.0.0.0:9090", ":9090", "192.168.1.10:9090"}
	for _, bind := range reachable {
		c := base(dir)
		c.Bind, c.NoAuth = bind, true
		if err := c.Validate(); err == nil {
			t.Errorf("--no-auth on %s was accepted", bind)
		}
	}
	// The same binds are fine once there is a token.
	for _, bind := range reachable {
		c := base(dir)
		c.Bind = bind
		if err := c.Validate(); err != nil {
			t.Errorf("%s with auth was refused: %v", bind, err)
		}
	}
}

func TestValidateNamesWhatIsMissing(t *testing.T) {
	dir := t.TempDir()
	c := base(dir)
	c.DSN = ""
	err := c.Validate()
	if err == nil {
		t.Fatal("a node with no database was accepted")
	}
	if !strings.Contains(err.Error(), "--dsn") {
		t.Errorf("the refusal does not say what to pass: %q", err)
	}

	c = base(dir)
	c.Bind = "9090"
	if err := c.Validate(); err == nil {
		t.Fatal("a bind with no host was accepted")
	}
}

func TestResolveTokenMintsOnceAndKeepsIt(t *testing.T) {
	t.Setenv("AURA_MEDIA_TOKEN", "")
	dir := t.TempDir()
	c := base(dir)

	first, err := ResolveToken(c)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if len(first) < 32 {
		t.Errorf("token is %d characters; that is guessable", len(first))
	}
	second, err := ResolveToken(c)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	// A token that changed on restart would log every skill and every operator
	// out on every deploy.
	if first != second {
		t.Errorf("the token changed on the second start: %q then %q", first, second)
	}
	onDisk, err := os.ReadFile(filepath.Join(dir, "media.token"))
	if err != nil {
		t.Fatalf("token file: %v", err)
	}
	if strings.TrimSpace(string(onDisk)) != first {
		t.Error("the token on disk is not the one in use")
	}
}

func TestResolveTokenPrefersTheEnvironment(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AURA_MEDIA_TOKEN", "  from-the-environment  ")
	got, err := ResolveToken(base(dir))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "from-the-environment" {
		t.Errorf("token is %q, want the trimmed environment value", got)
	}
	// Nothing was minted: a container that is handed its credential should not
	// leave a second one on a volume.
	if _, err := os.Stat(filepath.Join(dir, "media.token")); err == nil {
		t.Error("a token file was written even though the environment supplied one")
	}
}

func TestResolveTokenIsEmptyOnlyWhenAuthIsOff(t *testing.T) {
	t.Setenv("AURA_MEDIA_TOKEN", "")
	c := base(t.TempDir())
	c.NoAuth = true
	got, err := ResolveToken(c)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "" {
		t.Errorf("--no-auth still produced a token: %q", got)
	}
}
