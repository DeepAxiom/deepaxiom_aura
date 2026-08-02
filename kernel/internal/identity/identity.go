// Package identity implements kernel primitive P1.
//
// Security is progressive: in "local" mode identities are
// implicit UUIDs with full trust; "site" and "published" modes add keys
// and signature checks without changing this interface.
package identity

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"aura/kernel/internal/signing"
)

// Mode is the progressive security mode of this node.
type Mode string

const (
	ModeLocal     Mode = "local"
	ModeSite      Mode = "site"
	ModePublished Mode = "published"
)

// keyDir is where a node's own Ed25519 identity lives, as a subdirectory of
// its data dir rather than the data dir itself.
//
// `aura publish` keeps a *publisher's* key at <data>/keys — a different
// identity for a different purpose (signing packages a person authors) than
// this one (a running node signing its own effect ledger). Two Ed25519
// keypairs in the same directory would be an easy mix-up with a bad outcome:
// a node that accidentally signed checkpoints with the key that also signs
// published packages. Separate subdirectories make that impossible by
// construction rather than by convention.
const keyDir = "identity"

// Node is this kernel's own identity.
type Node struct {
	ID   string
	Mode Mode
	// Keys signs this node's effect ledger checkpoints (C4). Reusing
	// signing.LoadOrCreate is deliberate: it is the same mechanism `aura
	// publish` already uses, just pointed at a different directory — one
	// keypair type, two purposes, and the second (mutual authentication for
	// `site` mode) needs no new code when it lands.
	Keys *signing.Keypair
}

// Load reads or creates the node identity under dataDir.
func Load(dataDir string, mode Mode) (*Node, error) {
	if mode == "" {
		mode = ModeLocal
	}
	id, err := loadOrCreateID(dataDir)
	if err != nil {
		return nil, err
	}
	keys, err := signing.LoadOrCreate(filepath.Join(dataDir, keyDir))
	if err != nil {
		return nil, fmt.Errorf("load node identity keypair: %w", err)
	}
	return &Node{ID: id, Mode: mode, Keys: keys}, nil
}

func loadOrCreateID(dataDir string) (string, error) {
	idPath := filepath.Join(dataDir, "node-id")
	if b, err := os.ReadFile(idPath); err == nil {
		return strings.TrimSpace(string(b)), nil
	}
	id := "node-" + randomHex(8)
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return "", fmt.Errorf("create data dir: %w", err)
	}
	if err := os.WriteFile(idPath, []byte(id+"\n"), 0o644); err != nil {
		return "", fmt.Errorf("persist node id: %w", err)
	}
	return id, nil
}

// NewSessionID mints a session identity.
func NewSessionID() string { return "sess-" + randomHex(6) }

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return fmt.Sprintf("%x", b)
}
