package signing

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// signing is the basis of the marketplace trust model: `aura publish` signs and
// `aura add` verifies before installing. It had no tests at all, which meant
// the one property that matters — that a tampered package fails to verify —
// rested on nobody having broken it yet.

func mustKeypair(t *testing.T) (*Keypair, string) {
	t.Helper()
	dir := t.TempDir()
	kp, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	return kp, dir
}

func manifestOf(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("bad test manifest: %v", err)
	}
	return m
}

// --- LoadPublicKey ---------------------------------------------------------

// LoadPublicKey must never create anything. A verification tool that quietly
// minted an identity on a directory that never had one would be lying about
// what it verified.
func TestLoadPublicKeyDoesNotCreateAKeypair(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadPublicKey(dir); err == nil {
		t.Fatal("LoadPublicKey succeeded on a directory with no keypair")
	}
	if _, err := os.Stat(filepath.Join(dir, "ed25519.key")); err == nil {
		t.Fatal("LoadPublicKey created a private key as a side effect")
	}
}

func TestLoadPublicKeyMatchesTheKeypair(t *testing.T) {
	kp, dir := mustKeypair(t)
	pub, err := LoadPublicKey(dir)
	if err != nil {
		t.Fatalf("LoadPublicKey: %v", err)
	}
	if pub != kp.PublicB64() {
		t.Fatalf("LoadPublicKey = %q, want %q", pub, kp.PublicB64())
	}
}

func TestLoadPublicKeyRejectsCorruptFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ed25519.pub"), []byte("not base64!!"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPublicKey(dir); err == nil {
		t.Fatal("a corrupt public key file was accepted")
	}
}

// --- keys ---------------------------------------------------------------------

func TestKeypairIsCreatedOnceAndReloaded(t *testing.T) {
	kp, dir := mustKeypair(t)

	again, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("LoadOrCreate (second): %v", err)
	}
	if kp.PublicB64() != again.PublicB64() {
		t.Fatal("a publisher's identity changed on reload; every earlier package would stop verifying")
	}
	if !kp.Private.Equal(again.Private) {
		t.Fatal("the private key changed on reload")
	}
}

func TestPrivateKeyIsNotWorldReadable(t *testing.T) {
	if filepath.Separator == '\\' {
		t.Skip("POSIX mode bits do not apply on this platform")
	}
	_, dir := mustKeypair(t)
	info, err := os.Stat(filepath.Join(dir, "ed25519.key"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("private key is %v; want owner-only", perm)
	}
}

func TestCorruptPrivateKeyIsReportedNotRegenerated(t *testing.T) {
	_, dir := mustKeypair(t)
	if err := os.WriteFile(filepath.Join(dir, "ed25519.key"), []byte("not-a-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreate(dir); err == nil {
		t.Fatal("a corrupt key was silently replaced; the publisher's identity would change without warning")
	}
}

// --- the round trip -----------------------------------------------------------

func TestSignedPackageVerifies(t *testing.T) {
	kp, _ := mustKeypair(t)
	manifest := manifestOf(t, `{"id":"acme/logical/echo","version":"1.0.0"}`)
	artifact := []byte("the package bytes")

	mh, err := CanonicalManifestHash(manifest)
	if err != nil {
		t.Fatalf("CanonicalManifestHash: %v", err)
	}
	payload := Payload(mh, ArtifactHash(artifact))
	sig := kp.Sign(payload)

	if err := Verify(kp.PublicB64(), sig, payload); err != nil {
		t.Fatalf("a freshly signed package did not verify: %v", err)
	}
}

// The signed payload covers the manifest AND the artifact, so neither can be
// swapped for the other's signature. Both directions are checked because a
// scheme that binds only one of them is the classic packaging vulnerability.
func TestTamperingWithEitherHalfBreaksVerification(t *testing.T) {
	kp, _ := mustKeypair(t)
	manifest := manifestOf(t, `{"id":"acme/logical/echo","version":"1.0.0"}`)
	artifact := []byte("the package bytes")

	mh, _ := CanonicalManifestHash(manifest)
	original := Payload(mh, ArtifactHash(artifact))
	sig := kp.Sign(original)

	t.Run("swapped artifact", func(t *testing.T) {
		evil := Payload(mh, ArtifactHash([]byte("malware")))
		if err := Verify(kp.PublicB64(), sig, evil); err == nil {
			t.Fatal("a package with a replaced artifact verified against the original signature")
		}
	})

	t.Run("altered manifest", func(t *testing.T) {
		altered := manifestOf(t, `{"id":"acme/logical/echo","version":"9.9.9"}`)
		ah, _ := CanonicalManifestHash(altered)
		evil := Payload(ah, ArtifactHash(artifact))
		if err := Verify(kp.PublicB64(), sig, evil); err == nil {
			t.Fatal("a package with an altered manifest verified against the original signature")
		}
	})
}

// Trust-on-first-use means later versions must be signed by the key bound at
// first publish. A different key must not pass.
func TestAnotherPublishersKeyDoesNotVerify(t *testing.T) {
	first, _ := mustKeypair(t)
	second, _ := mustKeypair(t)

	payload := Payload(ArtifactHash([]byte("m")), ArtifactHash([]byte("a")))
	sig := second.Sign(payload)

	if err := Verify(first.PublicB64(), sig, payload); err == nil {
		t.Fatal("a package signed by a different publisher verified against the bound key")
	}
}

func TestVerifyRejectsMalformedInputs(t *testing.T) {
	kp, _ := mustKeypair(t)
	payload := Payload(ArtifactHash([]byte("m")), ArtifactHash([]byte("a")))
	good := kp.Sign(payload)

	for name, tc := range map[string]struct{ pub, sig string }{
		"garbage public key":     {"not-base64!!", good},
		"short public key":       {base64.StdEncoding.EncodeToString([]byte("too short")), good},
		"garbage signature":      {kp.PublicB64(), "not-base64!!"},
		"empty signature":        {kp.PublicB64(), ""},
		"wrong-length signature": {kp.PublicB64(), base64.StdEncoding.EncodeToString([]byte("short"))},
	} {
		t.Run(name, func(t *testing.T) {
			if err := Verify(tc.pub, tc.sig, payload); err == nil {
				t.Fatalf("%s was accepted", name)
			}
		})
	}
}

// --- canonicalisation ---------------------------------------------------------

// The hash has to be stable across serialisations, or a manifest that survived
// a JSON round trip would stop verifying for no reason a user could see.
func TestManifestHashIgnoresKeyOrder(t *testing.T) {
	a := manifestOf(t, `{"id":"x","version":"1.0.0","type":"logical"}`)
	b := manifestOf(t, `{"type":"logical","id":"x","version":"1.0.0"}`)

	ha, err := CanonicalManifestHash(a)
	if err != nil {
		t.Fatal(err)
	}
	hb, err := CanonicalManifestHash(b)
	if err != nil {
		t.Fatal(err)
	}
	if string(ha) != string(hb) {
		t.Fatal("key order changed the manifest hash; a JSON round trip would break the signature")
	}
}

func TestManifestHashIgnoresNestedKeyOrder(t *testing.T) {
	a := manifestOf(t, `{"ports":{"ingress":[{"name":"a","schema":"std/text@1"}]},"id":"x"}`)
	b := manifestOf(t, `{"id":"x","ports":{"ingress":[{"schema":"std/text@1","name":"a"}]}}`)

	ha, _ := CanonicalManifestHash(a)
	hb, _ := CanonicalManifestHash(b)
	if string(ha) != string(hb) {
		t.Fatal("nested key order changed the hash; canonicalisation must recurse")
	}
}

// Array order is data, not formatting. Reordering ports changes the manifest,
// so it must change the hash.
func TestManifestHashRespectsArrayOrder(t *testing.T) {
	a := manifestOf(t, `{"tags":["a","b"]}`)
	b := manifestOf(t, `{"tags":["b","a"]}`)

	ha, _ := CanonicalManifestHash(a)
	hb, _ := CanonicalManifestHash(b)
	if string(ha) == string(hb) {
		t.Fatal("reordering an array left the hash unchanged; that is real content")
	}
}

// The signature field is excluded, or a manifest could never carry the
// signature over itself.
func TestManifestHashExcludesTheSignatureField(t *testing.T) {
	unsigned := manifestOf(t, `{"id":"x","version":"1.0.0"}`)
	signed := manifestOf(t, `{"id":"x","version":"1.0.0","signature":"whatever"}`)

	hu, _ := CanonicalManifestHash(unsigned)
	hs, _ := CanonicalManifestHash(signed)
	if string(hu) != string(hs) {
		t.Fatal("the signature field is part of the hash; a manifest could never carry its own signature")
	}
}

func TestManifestHashChangesWithContent(t *testing.T) {
	a := manifestOf(t, `{"id":"x","version":"1.0.0"}`)
	b := manifestOf(t, `{"id":"x","version":"1.0.1"}`)

	ha, _ := CanonicalManifestHash(a)
	hb, _ := CanonicalManifestHash(b)
	if string(ha) == string(hb) {
		t.Fatal("two different manifests share a hash")
	}
}

// --- payload framing ----------------------------------------------------------

// The domain prefix stops a signature made for something else being replayed
// as a package signature.
func TestPayloadIsDomainSeparated(t *testing.T) {
	payload := Payload([]byte("mmmm"), []byte("aaaa"))
	if !strings.HasPrefix(string(payload), "aura-pkg-v1:") {
		t.Fatalf("payload = %q; it must be domain-separated", payload)
	}
}

func TestArtifactHashIsStableAndDistinguishing(t *testing.T) {
	if string(ArtifactHash([]byte("x"))) != string(ArtifactHash([]byte("x"))) {
		t.Fatal("hashing the same bytes twice gave different answers")
	}
	if string(ArtifactHash([]byte("x"))) == string(ArtifactHash([]byte("y"))) {
		t.Fatal("different artifacts share a hash")
	}
	if n := len(ArtifactHash([]byte("x"))); n != 32 {
		t.Errorf("artifact hash is %d bytes; want a full sha256", n)
	}
}
