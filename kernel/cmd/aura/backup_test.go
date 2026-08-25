package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"aura/kernel/internal/identity"
	"aura/kernel/internal/ledger"
	"aura/kernel/internal/store"
)

// The property this file exists to hold: a data directory that went through
// `aura backup` and came back through `aura restore` is the *same node* — not
// a directory with similar files in it. "Same node" is checkable rather than
// felt: identical entry count, identical recomputed Merkle root, checkpoints
// that still verify under the restored key.
//
// Everything else here is a refusal. A backup tool's failure mode is not
// crashing, it is succeeding quietly on something that will not restore, and
// each test below is one way that happens.

// newNodeDataDir builds a data directory the way a real node would: an
// identity (which mints node-id and the keypair) and a ledger with sealed
// effects and a signed checkpoint over them.
func newNodeDataDir(t *testing.T, effects int) string {
	t.Helper()
	dir := t.TempDir()

	node, err := identity.Load(dir, identity.ModeLocal)
	if err != nil {
		t.Fatalf("identity.Load: %v", err)
	}

	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	ldg, err := ledger.Open(st, node.ID, node.Keys)
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	for i := 0; i < effects; i++ {
		if _, err := ldg.Seal(ledger.SealRequest{
			Session: "sess-backup", Envelope: "ENV-" + string(rune('A'+i)), Cause: "ENV-0",
			Actor: "acme/motor/writer@1.0.0", Capability: "motor.erp.write",
			Decision: "allow", Outcome: "delivered",
			Policy: "sha256:policyhash", Payload: json.RawMessage(`{}`),
		}); err != nil {
			t.Fatalf("Seal %d: %v", i, err)
		}
	}
	if err := ldg.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	return dir
}

func backupTo(t *testing.T, dataDir string) string {
	t.Helper()
	archive := filepath.Join(t.TempDir(), "backup.tar.gz")
	var out, errOut bytes.Buffer
	if err := runBackup(&out, &errOut, dataDir, archive, false); err != nil {
		t.Fatalf("runBackup: %v\nstderr: %s", err, errOut.String())
	}
	return archive
}

func TestBackupRestoreRoundTripReproducesTheLedgerExactly(t *testing.T) {
	source := newNodeDataDir(t, 3)

	// What the source node's ledger actually says, before anything is copied.
	want, err := verifyRestored(source)
	if err != nil {
		t.Fatalf("verify source: %v", err)
	}
	if want.TotalEntries != 3 {
		t.Fatalf("fixture sealed %d entries, expected 3", want.TotalEntries)
	}

	archive := backupTo(t, source)
	restored := filepath.Join(t.TempDir(), "restored")

	var out, errOut bytes.Buffer
	if err := runRestore(&out, &errOut, archive, restored, false); err != nil {
		t.Fatalf("runRestore: %v\nstdout: %s", err, out.String())
	}

	got, err := verifyRestored(restored)
	if err != nil {
		t.Fatalf("verify restored: %v", err)
	}
	if !got.ChainIntact {
		t.Error("restored ledger's hash chain is broken")
	}
	if got.TotalEntries != want.TotalEntries {
		t.Errorf("restored %d entries, source had %d", got.TotalEntries, want.TotalEntries)
	}
	if got.MerkleRoot != want.MerkleRoot {
		t.Errorf("restored Merkle root %s, source %s", got.MerkleRoot, want.MerkleRoot)
	}
	if got.CheckpointsValid != want.CheckpointsValid || got.CheckpointsValid == 0 {
		t.Errorf("restored %d/%d valid checkpoints, source %d/%d",
			got.CheckpointsValid, got.Checkpoints, want.CheckpointsValid, want.Checkpoints)
	}

	// The node id has to come back too, or the restored node seals future
	// entries under a name its own history does not use.
	if readNodeID(restored) != readNodeID(source) {
		t.Errorf("restored node-id %q, source %q", readNodeID(restored), readNodeID(source))
	}

	// And the private key, byte for byte — this is what the credential
	// broker derives its secret-box key from.
	srcKey, _ := os.ReadFile(filepath.Join(source, "identity", "ed25519.key"))
	dstKey, _ := os.ReadFile(filepath.Join(restored, "identity", "ed25519.key"))
	if len(srcKey) == 0 || !bytes.Equal(srcKey, dstKey) {
		t.Error("restored identity key does not match the source key")
	}
}

func TestRestoredPrivateKeyIsNotWorldReadable(t *testing.T) {
	// Windows has no POSIX permission bits: os.Chmod there only toggles the
	// read-only attribute, so every file reads back as 0666 and the assertion
	// below would be checking nothing. The property still holds where it can
	// be enforced, and CI runs Linux.
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes are not enforced on Windows")
	}
	archive := backupTo(t, newNodeDataDir(t, 1))
	restored := filepath.Join(t.TempDir(), "restored")
	var out, errOut bytes.Buffer
	if err := runRestore(&out, &errOut, archive, restored, false); err != nil {
		t.Fatalf("runRestore: %v", err)
	}
	info, err := os.Stat(filepath.Join(restored, "identity", "ed25519.key"))
	if err != nil {
		t.Fatalf("stat restored key: %v", err)
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		t.Errorf("restored private key is mode %04o — readable beyond its owner", mode)
	}
}

func TestBackupRefusesADataDirectoryWithNoIdentity(t *testing.T) {
	// A directory with a database and no keypair: exactly what you get by
	// pointing the tool at the wrong path, and exactly the archive that looks
	// fine until the day it is needed.
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	st.Close()

	var out, errOut bytes.Buffer
	err = runBackup(&out, &errOut, dir, filepath.Join(t.TempDir(), "b.tar.gz"), false)
	if err == nil {
		t.Fatal("wrote a backup of a data directory with no identity")
	}
	for _, want := range []string{"identity/ed25519.key", "node-id"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name the missing %s: %v", want, err)
		}
	}
}

func TestRestoreRefusesAnArchiveStrippedOfItsIdentityKey(t *testing.T) {
	archive := backupTo(t, newNodeDataDir(t, 2))

	// Someone rebuilt the archive without the private key — a "sanitised"
	// backup, or a partial copy. It still holds the whole ledger, so it looks
	// complete, and restoring it produces a node whose secrets are gone.
	doctored := rewriteArchive(t, archive, func(name string, body []byte) ([]byte, bool) {
		if name == dataPrefix+"identity/ed25519.key" {
			return nil, false // drop it
		}
		if name == manifestName {
			return dropManifestEntry(t, body, "identity/ed25519.key"), true
		}
		return body, true
	})

	var out, errOut bytes.Buffer
	err := runRestore(&out, &errOut, doctored, filepath.Join(t.TempDir(), "restored"), false)
	if err == nil {
		t.Fatal("restored an archive with no identity key")
	}
	if !strings.Contains(err.Error(), "identity/ed25519.key") {
		t.Errorf("error does not name the missing key: %v", err)
	}
}

func TestRestoreDetectsAnArchiveWhoseContentNoLongerMatchesItsManifest(t *testing.T) {
	archive := backupTo(t, newNodeDataDir(t, 2))

	// The ledger's bytes are changed while the manifest still claims the old
	// digest — bit rot on the backup medium, or an edit.
	doctored := rewriteArchive(t, archive, func(name string, body []byte) ([]byte, bool) {
		if name == dataPrefix+"kernel.db" && len(body) > 0 {
			out := append([]byte(nil), body...)
			out[len(out)/2] ^= 0xFF
			return out, true
		}
		return body, true
	})

	var out, errOut bytes.Buffer
	err := runRestore(&out, &errOut, doctored, filepath.Join(t.TempDir(), "restored"), false)
	if err == nil {
		t.Fatal("restored a corrupted archive without complaint")
	}
	if !strings.Contains(err.Error(), "corrupt") {
		t.Errorf("error does not report corruption: %v", err)
	}

	// The same damage has to be visible without restoring, since that is the
	// check an operator runs on a schedule.
	var vout bytes.Buffer
	if err := runBackupVerify(&vout, doctored); err == nil {
		t.Error("--verify passed a corrupted archive")
	}
}

func TestRestoreRefusesANonEmptyDataDirectoryUnlessForced(t *testing.T) {
	archive := backupTo(t, newNodeDataDir(t, 1))

	occupied := filepath.Join(t.TempDir(), "occupied")
	if err := os.MkdirAll(occupied, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(occupied, "node-id"), []byte("node-someone-else\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	var out, errOut bytes.Buffer
	err := runRestore(&out, &errOut, archive, occupied, false)
	if err == nil {
		t.Fatal("restored over a non-empty data directory without --force")
	}
	if !strings.Contains(err.Error(), "not empty") {
		t.Errorf("error does not explain the refusal: %v", err)
	}

	// --force is the operator saying they meant it.
	out.Reset()
	errOut.Reset()
	if err := runRestore(&out, &errOut, archive, occupied, true); err != nil {
		t.Fatalf("--force did not restore: %v", err)
	}
}

func TestBackupVerifyPassesOnAnUntouchedArchive(t *testing.T) {
	archive := backupTo(t, newNodeDataDir(t, 2))
	var out bytes.Buffer
	if err := runBackupVerify(&out, archive); err != nil {
		t.Fatalf("runBackupVerify: %v", err)
	}
	if !strings.Contains(out.String(), "every digest matches") {
		t.Errorf("report does not confirm the digests: %s", out.String())
	}
	if !strings.Contains(out.String(), "2 entries") {
		t.Errorf("report does not carry the ledger summary: %s", out.String())
	}
}

func TestReadManifestRejectsSomethingThatIsNotAnAuraBackup(t *testing.T) {
	notABackup := filepath.Join(t.TempDir(), "random.tar.gz")
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := writeArchiveBytes(tw, "hello.txt", []byte("hi")); err != nil {
		t.Fatalf("write: %v", err)
	}
	tw.Close()
	gz.Close()
	if err := os.WriteFile(notABackup, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write archive: %v", err)
	}

	if _, err := readManifest(notABackup); err == nil {
		t.Fatal("accepted an archive with no manifest")
	} else if !strings.Contains(err.Error(), manifestName) {
		t.Errorf("error does not say what is missing: %v", err)
	}
}

// ─────────────────────────── test helpers ───────────────────────────

// rewriteArchive reads a backup and writes a new one, passing every entry
// through mutate. Returning false drops the entry. It exists so the tests
// above can build the archives an attacker or a failing disk would produce,
// rather than asserting against functions in isolation.
func rewriteArchive(t *testing.T, src string, mutate func(name string, body []byte) ([]byte, bool)) string {
	t.Helper()

	in, err := os.Open(src)
	if err != nil {
		t.Fatalf("open %s: %v", src, err)
	}
	defer in.Close()
	gzr, err := gzip.NewReader(in)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	defer gzr.Close()

	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)

	tr := tar.NewReader(gzr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read archive: %v", err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read %s: %v", hdr.Name, err)
		}
		next, keep := mutate(hdr.Name, body)
		if !keep {
			continue
		}
		if err := writeArchiveBytes(tw, hdr.Name, next); err != nil {
			t.Fatalf("write %s: %v", hdr.Name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gzw.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}

	dst := filepath.Join(t.TempDir(), "doctored.tar.gz")
	if err := os.WriteFile(dst, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write %s: %v", dst, err)
	}
	return dst
}

// dropManifestEntry removes one file from a manifest, so that a doctored
// archive stays internally consistent and has to be caught by the
// completeness rule rather than by a digest mismatch.
func dropManifestEntry(t *testing.T, raw []byte, path string) []byte {
	t.Helper()
	var man backupManifest
	if err := json.Unmarshal(raw, &man); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	kept := man.Files[:0]
	for _, f := range man.Files {
		if f.Path != path {
			kept = append(kept, f)
		}
	}
	man.Files = kept
	out, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		t.Fatalf("encode manifest: %v", err)
	}
	return out
}
