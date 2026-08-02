// Package signing implements package signing for published mode.
//
// Model: Ed25519 with trust-on-first-use per package id — the registry binds
// the publisher key on first publish; later versions must be signed by the
// same key. The signed payload covers the canonical manifest AND the artifact,
// so neither can be swapped independently.
package signing

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Keypair is a publisher identity.
type Keypair struct {
	Public  ed25519.PublicKey
	Private ed25519.PrivateKey
}

// LoadOrCreate returns the keypair under dir, generating it on first use.
func LoadOrCreate(dir string) (*Keypair, error) {
	privPath := filepath.Join(dir, "ed25519.key")
	pubPath := filepath.Join(dir, "ed25519.pub")

	if raw, err := os.ReadFile(privPath); err == nil {
		priv, err := base64.StdEncoding.DecodeString(string(raw))
		if err != nil || len(priv) != ed25519.PrivateKeySize {
			return nil, fmt.Errorf("corrupt private key at %s", privPath)
		}
		key := ed25519.PrivateKey(priv)
		return &Keypair{Public: key.Public().(ed25519.PublicKey), Private: key}, nil
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(privPath, []byte(base64.StdEncoding.EncodeToString(priv)), 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(pubPath, []byte(base64.StdEncoding.EncodeToString(pub)), 0o644); err != nil {
		return nil, err
	}
	return &Keypair{Public: pub, Private: priv}, nil
}

// LoadPublicKey reads only the public half of a keypair, never touching or
// creating the private key.
//
// This exists for verification that must not have side effects. `aura
// verify` reads a node's ledger-signing public key to check checkpoint
// signatures; if it fell back to LoadOrCreate on a directory that never had
// a keypair, a read-only command would silently mint a new node identity —
// exactly the kind of surprise a *verification* tool must never produce.
func LoadPublicKey(dir string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "ed25519.pub"))
	if err != nil {
		return "", fmt.Errorf("no public key at %s: %w", dir, err)
	}
	pub := strings.TrimSpace(string(raw))
	if _, err := base64.StdEncoding.DecodeString(pub); err != nil {
		return "", fmt.Errorf("corrupt public key at %s: %w", dir, err)
	}
	return pub, nil
}

// CanonicalManifestHash hashes the manifest with sorted keys and no
// signature field, so the hash is stable across serializations.
func CanonicalManifestHash(manifest map[string]any) ([]byte, error) {
	clean := make(map[string]any, len(manifest))
	for k, v := range manifest {
		if k != "signature" {
			clean[k] = v
		}
	}
	canonical, err := marshalCanonical(clean)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(canonical)
	return sum[:], nil
}

// Payload builds the signed byte string: manifest hash || artifact hash.
func Payload(manifestHash, artifactHash []byte) []byte {
	return append(append([]byte("aura-pkg-v1:"), manifestHash...), artifactHash...)
}

// Sign returns the base64 signature over the payload.
func (k *Keypair) Sign(payload []byte) string {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(k.Private, payload))
}

// PublicB64 returns the base64 public key.
func (k *Keypair) PublicB64() string {
	return base64.StdEncoding.EncodeToString(k.Public)
}

// Verify checks a base64 signature against a base64 public key.
func Verify(pubB64, sigB64 string, payload []byte) error {
	pub, err := base64.StdEncoding.DecodeString(pubB64)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("invalid public key")
	}
	sig, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		return fmt.Errorf("invalid signature encoding")
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), payload, sig) {
		return fmt.Errorf("signature verification FAILED")
	}
	return nil
}

// ArtifactHash hashes an artifact blob.
func ArtifactHash(artifact []byte) []byte {
	sum := sha256.Sum256(artifact)
	return sum[:]
}

// marshalCanonical emits JSON with lexicographically sorted keys at every level.
func marshalCanonical(v any) ([]byte, error) {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := []byte{'{'}
		for i, k := range keys {
			if i > 0 {
				out = append(out, ',')
			}
			kb, _ := json.Marshal(k)
			vb, err := marshalCanonical(t[k])
			if err != nil {
				return nil, err
			}
			out = append(append(append(out, kb...), ':'), vb...)
		}
		return append(out, '}'), nil
	case []any:
		out := []byte{'['}
		for i, item := range t {
			if i > 0 {
				out = append(out, ',')
			}
			ib, err := marshalCanonical(item)
			if err != nil {
				return nil, err
			}
			out = append(out, ib...)
		}
		return append(out, ']'), nil
	default:
		return json.Marshal(v)
	}
}
