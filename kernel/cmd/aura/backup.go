package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite" // same pure-Go driver the store uses; no CGO

	"aura/kernel/internal/ledger"
	"aura/kernel/internal/signing"
	"aura/kernel/internal/spec"
	"aura/kernel/internal/store"
)

// `aura backup` and `aura restore` — the pair that makes a data directory
// survivable.
//
// # Why this is a command and not a documented `tar czf`
//
// Three properties a shell script cannot have:
//
//  1. **The database is snapshotted, not copied.** kernel.db runs in WAL mode,
//     so `cp` on a live node yields a torn file: the main database without the
//     tail of its write-ahead log, or a -wal that does not match its -shm.
//     `VACUUM INTO` asks SQLite for a consistent snapshot of the committed
//     state, which is defined behaviour on a database being written to.
//
//  2. **The identity travels with the ledger, or the archive is refused.**
//     This is the one that actually loses data. The credential broker derives
//     its secret-box key from the node's Ed25519 private key
//     (internal/broker: sha256("aura-secret-box-v1:" + priv)), so a data
//     directory restored without identity/ed25519.key is a database full of
//     ciphertext nobody — including its owner — can ever open again. Every
//     backup procedure written by hand gets this wrong once, and finds out
//     during the incident it was written for. Here identity/ is not a file you
//     might remember to include: `backup` refuses to produce an archive
//     without it, and `restore` refuses to consume one.
//
//  3. **The restore is checkable, not hopeful.** The manifest records the
//     ledger's entry count and its recomputed RFC 6962 Merkle root at the
//     moment of the backup. After extracting, `restore` recomputes both from
//     the restored files and compares. "The files copied without error" and
//     "the evidence chain survived intact" are different claims, and only the
//     second one is worth anything to an auditor.
//
// # Taking a backup of a running node
//
// Supported, with one honest caveat. The SQLite snapshot is consistent as of
// the moment it is taken. The causal event log is a set of append-only segment
// files with a CRC per record, so copying one mid-write can capture a torn
// final record — which the reader detects and stops at, rather than
// misparsing. The effect is that a backup taken from a live node may lag the
// node by the last few events. It is never internally inconsistent, and the
// ledger — the part that is evidence — is exact, because it comes from the
// snapshot.
//
// An operator who wants the event log exact too stops the node first. The
// banner says which kind of backup was taken.

const backupFormat = "aura-backup/1"

// manifestName sits at the archive root rather than under the data-directory
// prefix, so that reading it never requires trusting a path from inside the
// archive.
const manifestName = "MANIFEST.json"

// dataPrefix namespaces the data directory's own files inside the archive.
const dataPrefix = "data/"

// backupManifest is the archive's self-description: what node it came from,
// what the ledger looked like when it left, and a digest per file.
type backupManifest struct {
	Format      string    `json:"format"`
	CreatedAt   time.Time `json:"created_at"`
	AuraVersion string    `json:"aura_version"`
	NodeID      string    `json:"node_id"`
	// Live records whether the node was serving while this was taken, which
	// is what decides whether the event log may lag. See the package comment.
	Live bool `json:"live"`

	// The ledger's state at snapshot time. RestoredRoot must equal MerkleRoot
	// after a restore, or the restore did not reproduce this node.
	LedgerEntries     int64  `json:"ledger_entries"`
	MerkleRoot        string `json:"merkle_root,omitempty"`
	PubkeyFingerprint string `json:"pubkey_fingerprint,omitempty"`

	Files []backupFile `json:"files"`
}

// backupFile is one archived file. Role is not decoration: restore uses it to
// decide what is mandatory (identity, ledger) and what is merely nice to have
// (registry, regenerable certificates).
type backupFile struct {
	Path   string `json:"path"` // relative to the data directory
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	Role   string `json:"role"`
}

const (
	roleIdentity  = "identity"  // node keypair + node-id — without these, nothing decrypts
	roleLedger    = "ledger"    // kernel.db snapshot — skills, graphs, sessions, effects
	roleEventLog  = "eventlog"  // causal event log segments
	rolePublisher = "publisher" // `aura publish` keypair — a different identity, still yours
	roleConfig    = "config"    // operator token, WebTransport certificate
	roleRegistry  = "registry"  // hosted registry index and blobs
)

// requiredFiles are the exact paths an archive must carry to be restorable.
//
// Checked by path rather than by role deliberately. A role-level check would
// pass an archive holding identity/ed25519.pub and nothing else — which
// carries the *role* "identity" and none of its value, since the public half
// verifies signatures and derives nothing. Each of these four is load-bearing
// on its own:
//
//   - identity/ed25519.key   derives the broker's secret-box key and signs
//     checkpoints. Without it the secrets are gone.
//   - identity/ed25519.pub   lets a second reader check the chain offline,
//     which is the whole point of the ledger.
//   - node-id                names this node in every entry it sealed. A
//     missing one is not an error at startup: the
//     kernel mints a fresh id, and the restored node
//     then disagrees with its own history.
//   - kernel.db              the ledger itself.
var requiredFiles = []string{
	"node-id",
	"identity/ed25519.key",
	"identity/ed25519.pub",
	"kernel.db",
}

func cmdBackup(args []string) {
	fs := flag.NewFlagSet("backup", flag.ExitOnError)
	data := fs.String("data", defaultDataDir(), "data directory to back up")
	out := fs.String("out", "", "archive to write (default aura-backup-<node>-<timestamp>.tar.gz)")
	verify := fs.String("verify", "", "verify an existing archive instead of writing one")
	withRegistry := fs.Bool("include-registry", false, "also archive the hosted registry index and blobs")
	_ = fs.Parse(args)

	if *verify != "" {
		if err := runBackupVerify(os.Stdout, *verify); err != nil {
			fatal(err)
		}
		return
	}
	if err := runBackup(os.Stdout, os.Stderr, *data, *out, *withRegistry); err != nil {
		fatal(err)
	}
}

func runBackup(out, errOut io.Writer, dataDir, archivePath string, withRegistry bool) error {
	if _, err := os.Stat(dataDir); err != nil {
		return fmt.Errorf("data directory %s: %w", dataDir, err)
	}

	nodeID := readNodeID(dataDir)
	if archivePath == "" {
		stamp := time.Now().UTC().Format("20060102-150405")
		name := nodeID
		if name == "" {
			name = "node"
		}
		archivePath = fmt.Sprintf("aura-backup-%s-%s.tar.gz", name, stamp)
	}

	// The snapshot goes to a temp file beside the archive rather than inside
	// the data directory: VACUUM INTO refuses an existing target, and writing
	// scratch state into the directory being backed up is how a backup tool
	// ends up in its own archive.
	snapshot, err := os.CreateTemp(filepath.Dir(archivePath), ".aura-kernel-snapshot-*.db")
	if err != nil {
		return fmt.Errorf("create snapshot temp file: %w", err)
	}
	snapshotPath := snapshot.Name()
	_ = snapshot.Close()
	// VACUUM INTO wants to create the file itself.
	_ = os.Remove(snapshotPath)
	defer os.Remove(snapshotPath)

	live, err := snapshotKernelDB(dataDir, snapshotPath)
	if err != nil {
		return err
	}

	man := backupManifest{
		Format:      backupFormat,
		CreatedAt:   time.Now().UTC(),
		AuraVersion: spec.Version,
		NodeID:      nodeID,
		Live:        live,
	}

	// Read the ledger's state from the *snapshot*, not from the live
	// directory. The number recorded here is the number a restore has to
	// reproduce, so it must describe the bytes actually being archived.
	if err := describeLedger(&man, dataDir, snapshotPath); err != nil {
		fmt.Fprintf(errOut, "note: could not summarise the ledger (%v) — "+
			"the archive is still written, but `aura restore` cannot prove it came back whole\n", err)
	}

	plan, err := planBackup(dataDir, snapshotPath, withRegistry)
	if err != nil {
		return err
	}
	if err := requireComplete(plan); err != nil {
		return err
	}

	// 0600 from the moment it exists: this archive contains the node's
	// private key and the operator token. A backup that lands world-readable
	// in a shared directory has handed over the node.
	f, err := os.OpenFile(archivePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", archivePath, err)
	}
	defer f.Close()

	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)

	for i := range plan {
		digest, size, err := writeArchiveFile(tw, dataPrefix+plan[i].archivePath, plan[i].source)
		if err != nil {
			return err
		}
		man.Files = append(man.Files, backupFile{
			Path: plan[i].archivePath, Size: size, SHA256: digest, Role: plan[i].role,
		})
	}

	// The manifest is written last, because it describes what came before it.
	raw, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	if err := writeArchiveBytes(tw, manifestName, raw); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("close tar: %w", err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("close gzip: %w", err)
	}

	printBackupReport(out, archivePath, man, live)
	return nil
}

// snapshotKernelDB asks SQLite for a consistent copy of kernel.db. It reports
// whether the database was being written to — which is the honest signal for
// whether the event log alongside it may lag.
func snapshotKernelDB(dataDir, dest string) (live bool, err error) {
	src := filepath.Join(dataDir, "kernel.db")
	if _, statErr := os.Stat(src); statErr != nil {
		return false, fmt.Errorf("kernel.db in %s: %w", dataDir, statErr)
	}

	// busy_timeout matters here: VACUUM INTO takes a read lock, and a node
	// mid-group-commit holds the write lock briefly. Waiting is correct;
	// failing because a healthy node was busy for 3ms is not.
	dsn := src + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return false, fmt.Errorf("open kernel.db: %w", err)
	}
	defer db.Close()

	// A non-empty -wal is the tell that a node has the database open, or had
	// it open and did not check-point on the way out. Either way the event
	// log's tail is not guaranteed to be settled.
	if info, statErr := os.Stat(src + "-wal"); statErr == nil && info.Size() > 0 {
		live = true
	}

	if _, err := db.Exec("VACUUM INTO ?", dest); err != nil {
		return live, fmt.Errorf("snapshot kernel.db: %w", err)
	}
	return live, nil
}

// describeLedger records what the ledger looked like at snapshot time, by
// verifying the snapshot itself in a scratch directory. The public key comes
// from the real data directory: it is the same key, and copying it into the
// scratch directory only to read it back would be theatre.
func describeLedger(man *backupManifest, dataDir, snapshotPath string) error {
	scratch, err := os.MkdirTemp("", "aura-backup-verify-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)

	if err := copyFile(snapshotPath, filepath.Join(scratch, "kernel.db"), 0o600); err != nil {
		return err
	}
	st, err := store.Open(scratch)
	if err != nil {
		return err
	}
	defer st.Close()

	pubkey, keyErr := signing.LoadPublicKey(filepath.Join(dataDir, "identity"))
	if keyErr != nil {
		// Not fatal: the chain is still recomputable without the key, and the
		// Merkle root is what the restore check actually compares.
		pubkey = ""
	}
	report, err := ledger.Verify(st, pubkey)
	if err != nil {
		return err
	}
	man.LedgerEntries = report.TotalEntries
	man.MerkleRoot = report.MerkleRoot
	man.PubkeyFingerprint = report.PubkeyFingerprint
	return nil
}

// plannedFile pairs a file on disk with the name it takes inside the archive.
// They differ in exactly one case — the kernel.db snapshot, which lives in a
// temp file and is archived under its real name.
type plannedFile struct {
	source      string
	archivePath string
	role        string
}

func planBackup(dataDir, snapshotPath string, withRegistry bool) ([]plannedFile, error) {
	var plan []plannedFile
	add := func(rel, role string) {
		full := filepath.Join(dataDir, rel)
		if info, err := os.Stat(full); err == nil && !info.IsDir() {
			plan = append(plan, plannedFile{source: full, archivePath: rel, role: role})
		}
	}

	add("node-id", roleIdentity)
	add(filepath.Join("identity", "ed25519.key"), roleIdentity)
	add(filepath.Join("identity", "ed25519.pub"), roleIdentity)

	plan = append(plan, plannedFile{source: snapshotPath, archivePath: "kernel.db", role: roleLedger})

	// The causal event log: every segment, plus the pre-segmentation file if
	// this directory predates the split and was never migrated.
	segments, err := filepath.Glob(filepath.Join(dataDir, "events-*.log"))
	if err != nil {
		return nil, fmt.Errorf("list event log segments: %w", err)
	}
	sort.Strings(segments)
	for _, seg := range segments {
		plan = append(plan, plannedFile{
			source: seg, archivePath: filepath.Base(seg), role: roleEventLog,
		})
	}
	add("events.log", roleEventLog)

	add(filepath.Join("keys", "ed25519.key"), rolePublisher)
	add(filepath.Join("keys", "ed25519.pub"), rolePublisher)

	add("node.token", roleConfig)
	add("webtransport-cert.pem", roleConfig)
	add("webtransport-key.pem", roleConfig)

	if withRegistry {
		add("registry.db", roleRegistry)
		blobs := filepath.Join(dataDir, "blobs")
		err := filepath.Walk(blobs, func(p string, info os.FileInfo, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			if info.IsDir() {
				return nil
			}
			rel, relErr := filepath.Rel(dataDir, p)
			if relErr != nil {
				return relErr
			}
			plan = append(plan, plannedFile{
				source: p, archivePath: filepath.ToSlash(rel), role: roleRegistry,
			})
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("walk blobs: %w", err)
		}
	}

	// Archive paths are slash-separated regardless of host, so an archive
	// taken on Windows restores on Linux.
	for i := range plan {
		plan[i].archivePath = filepath.ToSlash(plan[i].archivePath)
	}
	return plan, nil
}

// requireComplete is the refusal that gives this command its point: an archive
// missing the node identity is not a partial backup, it is a decoy.
func requireComplete(plan []plannedFile) error {
	have := map[string]bool{}
	for _, p := range plan {
		have[p.archivePath] = true
	}
	return missingFiles(have)
}

func missingFiles(have map[string]bool) error {
	var missing []string
	for _, want := range requiredFiles {
		if !have[want] {
			missing = append(missing, want)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf(
		"incomplete backup — missing %s. A data directory restored without these cannot "+
			"decrypt the credential broker's secrets and cannot prove its own ledger, so the "+
			"archive would look like a backup and not be one. Refusing rather than producing it",
		strings.Join(missing, ", "))
}

func writeArchiveFile(tw *tar.Writer, name, source string) (digest string, size int64, err error) {
	info, err := os.Stat(source)
	if err != nil {
		return "", 0, fmt.Errorf("stat %s: %w", source, err)
	}
	f, err := os.Open(source)
	if err != nil {
		return "", 0, fmt.Errorf("open %s: %w", source, err)
	}
	defer f.Close()

	hdr := &tar.Header{
		Name:    name,
		Mode:    0o600,
		Size:    info.Size(),
		ModTime: info.ModTime(),
		Format:  tar.FormatPAX,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return "", 0, fmt.Errorf("write header for %s: %w", name, err)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tw, h), f)
	if err != nil {
		return "", 0, fmt.Errorf("archive %s: %w", name, err)
	}
	if n != info.Size() {
		return "", 0, fmt.Errorf("archive %s: file changed size mid-copy (%d → %d)", name, info.Size(), n)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func writeArchiveBytes(tw *tar.Writer, name string, body []byte) error {
	hdr := &tar.Header{
		Name: name, Mode: 0o600, Size: int64(len(body)),
		ModTime: time.Now().UTC(), Format: tar.FormatPAX,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("write header for %s: %w", name, err)
	}
	if _, err := tw.Write(body); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	return nil
}

func printBackupReport(out io.Writer, archivePath string, man backupManifest, live bool) {
	info, _ := os.Stat(archivePath)
	var size int64
	if info != nil {
		size = info.Size()
	}
	byRole := map[string]int{}
	for _, f := range man.Files {
		byRole[f.Role]++
	}

	fmt.Fprintf(out, "\n  wrote     %s (%s)\n", archivePath, humanBytes(int(size)))
	fmt.Fprintf(out, "  node      %s\n", man.NodeID)
	fmt.Fprintf(out, "  ledger    %d entries", man.LedgerEntries)
	if man.MerkleRoot != "" {
		fmt.Fprintf(out, " · root %s", shortHash(man.MerkleRoot))
	}
	fmt.Fprintln(out)

	roles := make([]string, 0, len(byRole))
	for r := range byRole {
		roles = append(roles, r)
	}
	sort.Strings(roles)
	parts := make([]string, 0, len(roles))
	for _, r := range roles {
		parts = append(parts, fmt.Sprintf("%s %d", r, byRole[r]))
	}
	fmt.Fprintf(out, "  files     %s\n", strings.Join(parts, " · "))

	if live {
		fmt.Fprintln(out, "\n  taken from a live node: the ledger snapshot is exact, the event log")
		fmt.Fprintln(out, "  may lag by the last few events. Stop the node first if you need both exact.")
	}
	fmt.Fprintln(out, "\n  This archive contains the node's private key and operator token.")
	fmt.Fprintln(out, "  It is written 0600. Store it where a database backup would go, not where logs go.")
	fmt.Fprintf(out, "\n  aura backup --verify %s     check it without restoring\n", archivePath)
	fmt.Fprintf(out, "  aura restore --in %s --data ./restored\n\n", archivePath)
}

// ─────────────────────────────── restore ───────────────────────────────

func cmdRestore(args []string) {
	fs := flag.NewFlagSet("restore", flag.ExitOnError)
	in := fs.String("in", "", "archive to restore (required)")
	data := fs.String("data", "", "data directory to restore into (required; must be empty unless --force)")
	force := fs.Bool("force", false, "restore into a non-empty directory, overwriting files the archive carries")
	_ = fs.Parse(args)

	if *in == "" || *data == "" {
		fatal(fmt.Errorf("usage: aura restore --in <archive.tar.gz> --data <dir> [--force]"))
	}
	if err := runRestore(os.Stdout, os.Stderr, *in, *data, *force); err != nil {
		fatal(err)
	}
}

func runRestore(out, errOut io.Writer, archivePath, dataDir string, force bool) error {
	man, err := readManifest(archivePath)
	if err != nil {
		return err
	}
	if man.Format != backupFormat {
		return fmt.Errorf("archive format %q, this binary understands %q", man.Format, backupFormat)
	}
	if err := manifestIsComplete(man); err != nil {
		return err
	}

	// A non-empty target is refused rather than merged. Restoring a node's
	// identity on top of a *different* node's ledger produces a directory
	// whose checkpoints do not verify, and the operator finds out when an
	// auditor does.
	if entries, err := os.ReadDir(dataDir); err == nil && len(entries) > 0 && !force {
		return fmt.Errorf(
			"%s is not empty — restoring over an existing data directory can pair one node's "+
				"identity with another's ledger. Restore into a fresh directory, or pass --force "+
				"if you mean to overwrite", dataDir)
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dataDir, err)
	}

	want := map[string]backupFile{}
	for _, f := range man.Files {
		want[f.Path] = f
	}

	extracted, err := extractArchive(archivePath, dataDir, want)
	if err != nil {
		return err
	}
	for p := range want {
		if !extracted[p] {
			return fmt.Errorf("archive manifest lists %s but the archive does not contain it", p)
		}
	}

	// The check that makes this a restore rather than an extraction.
	report, verifyErr := verifyRestored(dataDir)
	printRestoreReport(out, dataDir, man, report, verifyErr)
	if verifyErr != nil {
		return fmt.Errorf("restored files are in place but the ledger did not verify: %w", verifyErr)
	}
	if man.MerkleRoot != "" && report.MerkleRoot != man.MerkleRoot {
		return fmt.Errorf(
			"restored ledger does not match the archive: manifest root %s, recomputed %s — "+
				"the archive is damaged or was tampered with",
			shortHash(man.MerkleRoot), shortHash(report.MerkleRoot))
	}
	if report.TotalEntries != man.LedgerEntries {
		return fmt.Errorf("restored ledger has %d entries, the archive recorded %d",
			report.TotalEntries, man.LedgerEntries)
	}
	return nil
}

func manifestIsComplete(man *backupManifest) error {
	have := map[string]bool{}
	for _, f := range man.Files {
		have[f.Path] = true
	}
	return missingFiles(have)
}

// extractArchive writes the archive's data files into dataDir, verifying each
// digest as it goes and refusing any path that tries to escape.
func extractArchive(archivePath, dataDir string, want map[string]backupFile) (map[string]bool, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", archivePath, err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", archivePath, err)
	}
	defer gz.Close()

	done := map[string]bool{}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read archive: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg || !strings.HasPrefix(hdr.Name, dataPrefix) {
			continue
		}
		rel := strings.TrimPrefix(hdr.Name, dataPrefix)

		// Refuse anything that is not a plain relative path inside the data
		// directory. An archive is untrusted input even when you wrote it.
		if rel == "" || path.IsAbs(rel) || strings.Contains(rel, "..") || filepath.IsAbs(rel) {
			return nil, fmt.Errorf("archive entry %q is not a safe relative path", hdr.Name)
		}
		meta, listed := want[rel]
		if !listed {
			return nil, fmt.Errorf("archive contains %s, which its own manifest does not list", rel)
		}

		dest := filepath.Join(dataDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return nil, fmt.Errorf("create %s: %w", filepath.Dir(dest), err)
		}

		// Private keys and the operator token come back 0600. Everything else
		// is 0644 — the ledger is evidence, and evidence a second reader
		// cannot open is not evidence.
		mode := os.FileMode(0o644)
		if meta.Role == roleIdentity || meta.Role == rolePublisher || rel == "node.token" ||
			strings.HasSuffix(rel, "-key.pem") {
			mode = 0o600
		}
		digest, n, err := writeVerified(dest, tr, mode)
		if err != nil {
			return nil, err
		}
		if digest != meta.SHA256 {
			return nil, fmt.Errorf("%s is corrupt: manifest says %s, archive contains %s",
				rel, shortHash(meta.SHA256), shortHash(digest))
		}
		if n != meta.Size {
			return nil, fmt.Errorf("%s is %d bytes, manifest says %d", rel, n, meta.Size)
		}
		done[rel] = true
	}
	return done, nil
}

func writeVerified(dest string, src io.Reader, mode os.FileMode) (digest string, n int64, err error) {
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return "", 0, fmt.Errorf("create %s: %w", dest, err)
	}
	defer out.Close()
	h := sha256.New()
	n, err = io.Copy(io.MultiWriter(out, h), src)
	if err != nil {
		return "", 0, fmt.Errorf("write %s: %w", dest, err)
	}
	// Restoring onto a filesystem that ignored the create mode (or a
	// pre-existing file) still ends up with the mode we asked for.
	if err := os.Chmod(dest, mode); err != nil {
		return "", 0, fmt.Errorf("chmod %s: %w", dest, err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func verifyRestored(dataDir string) (ledger.Report, error) {
	st, err := store.Open(dataDir)
	if err != nil {
		return ledger.Report{}, fmt.Errorf("open restored store: %w", err)
	}
	defer st.Close()
	pubkey, keyErr := signing.LoadPublicKey(filepath.Join(dataDir, "identity"))
	if keyErr != nil {
		return ledger.Report{}, fmt.Errorf("restored identity: %w", keyErr)
	}
	return ledger.Verify(st, pubkey)
}

func printRestoreReport(out io.Writer, dataDir string, man *backupManifest, report ledger.Report, verifyErr error) {
	fmt.Fprintf(out, "\n  restored  %s\n", dataDir)
	fmt.Fprintf(out, "  node      %s (archived %s)\n", man.NodeID, man.CreatedAt.Format(time.RFC3339))
	fmt.Fprintf(out, "  files     %d\n", len(man.Files))
	if verifyErr != nil {
		fmt.Fprintf(out, "  ledger    DID NOT VERIFY — %v\n\n", verifyErr)
		return
	}
	fmt.Fprintf(out, "  ledger    %d entries · chain %s · %d/%d checkpoints valid\n",
		report.TotalEntries, intactWord(report.ChainIntact), report.CheckpointsValid, report.Checkpoints)
	if man.MerkleRoot != "" {
		match := "matches the archive"
		if report.MerkleRoot != man.MerkleRoot {
			match = "DOES NOT MATCH the archive"
		}
		fmt.Fprintf(out, "  root      %s — %s\n", shortHash(report.MerkleRoot), match)
	}
	fmt.Fprintf(out, "\n  aura up --data %s\n\n", dataDir)
}

// ─────────────────────────────── verify ───────────────────────────────

// runBackupVerify checks an archive without restoring it: every digest, and
// the presence of the roles a restore will require. It is the command an
// operator runs on a schedule, because a backup nobody has ever read is a
// hypothesis.
func runBackupVerify(out io.Writer, archivePath string) error {
	man, err := readManifest(archivePath)
	if err != nil {
		return err
	}
	if err := manifestIsComplete(man); err != nil {
		return err
	}

	f, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("open %s: %w", archivePath, err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("read %s: %w", archivePath, err)
	}
	defer gz.Close()

	want := map[string]backupFile{}
	for _, file := range man.Files {
		want[file.Path] = file
	}
	seen := 0
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read archive: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg || !strings.HasPrefix(hdr.Name, dataPrefix) {
			continue
		}
		rel := strings.TrimPrefix(hdr.Name, dataPrefix)
		meta, listed := want[rel]
		if !listed {
			return fmt.Errorf("archive contains %s, which its own manifest does not list", rel)
		}
		h := sha256.New()
		n, err := io.Copy(h, tr)
		if err != nil {
			return fmt.Errorf("read %s: %w", rel, err)
		}
		if got := hex.EncodeToString(h.Sum(nil)); got != meta.SHA256 {
			return fmt.Errorf("%s is corrupt: manifest says %s, archive contains %s",
				rel, shortHash(meta.SHA256), shortHash(got))
		}
		if n != meta.Size {
			return fmt.Errorf("%s is %d bytes, manifest says %d", rel, n, meta.Size)
		}
		seen++
	}
	if seen != len(man.Files) {
		return fmt.Errorf("manifest lists %d files, archive contains %d", len(man.Files), seen)
	}

	fmt.Fprintf(out, "\n  archive   %s\n", archivePath)
	fmt.Fprintf(out, "  node      %s (archived %s)\n", man.NodeID, man.CreatedAt.Format(time.RFC3339))
	fmt.Fprintf(out, "  files     %d, every digest matches\n", seen)
	fmt.Fprintf(out, "  ledger    %d entries", man.LedgerEntries)
	if man.MerkleRoot != "" {
		fmt.Fprintf(out, " · root %s", shortHash(man.MerkleRoot))
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "\n  Digests check out. That the ledger itself verifies is proven by")
	fmt.Fprintln(out, "  restoring it — `aura restore` recomputes the root and compares.")
	fmt.Fprintln(out)
	return nil
}

func readManifest(archivePath string) (*backupManifest, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", archivePath, err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", archivePath, err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read archive: %w", err)
		}
		if hdr.Name != manifestName {
			continue
		}
		raw, err := io.ReadAll(io.LimitReader(tr, 8<<20))
		if err != nil {
			return nil, fmt.Errorf("read manifest: %w", err)
		}
		var man backupManifest
		if err := json.Unmarshal(raw, &man); err != nil {
			return nil, fmt.Errorf("parse manifest: %w", err)
		}
		return &man, nil
	}
	return nil, fmt.Errorf("%s carries no %s — it was not written by `aura backup`", archivePath, manifestName)
}

// ─────────────────────────────── helpers ───────────────────────────────

func readNodeID(dataDir string) string {
	b, err := os.ReadFile(filepath.Join(dataDir, "node-id"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func intactWord(ok bool) string {
	if ok {
		return "intact"
	}
	return "BROKEN"
}
