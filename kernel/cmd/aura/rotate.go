package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"aura/kernel/internal/broker"
	"aura/kernel/internal/ledger"
	"aura/kernel/internal/signing"
	"aura/kernel/internal/store"
)

// `aura rotate` — replace this node's signing key without orphaning what the
// old one signed.
//
// # Why this could not be done before
//
// The signing key was forever. Generating a new one made every checkpoint the
// node had ever written fail to verify, so an operator who suspected
// compromise faced a choice between keeping a key they no longer trusted and
// throwing away their evidence. Faced with that, everyone keeps the key.
//
// C4 v1.6 adds the succession record — see internal/ledger/succession.go for
// the record and how a verifier holding only the *current* public key
// reconstructs the whole key history from it.
//
// # The two halves, and why the second one is the dangerous one
//
// Rotation is a signing change and a *decryption* change at once. The
// credential broker derives its secret-box key from the node's private key, so
// a rotation that only swapped the signing key would leave every stored
// credential unopenable by anyone, forever — silently at rotation time, and
// totally at the next effect that needs one. So the secrets are re-encrypted in
// the same transaction that records the succession. See broker.Rekey.
//
// # Surviving a crash mid-rotation
//
// The order below is chosen so that no interruption can lose a key:
//
//  1. The incoming keypair is written to identity/incoming/ **before**
//     anything commits. A crash here leaves a node that never rotated, plus a
//     staged directory the next run recognises and discards.
//  2. The succession and the re-encrypted secrets commit in one transaction.
//     A crash here leaves either a node that never rotated or one that fully
//     did — the database has no in-between.
//  3. The active keypair moves to identity/retired/<fingerprint>/ and the
//     staged one takes its place. A crash here leaves the database rotated and
//     the files not yet moved; both keys are on disk, and the next run detects
//     the committed succession and finishes the move rather than starting over.
//
// The retired key is kept rather than deleted. It is no longer used to sign
// anything, and it is what lets someone re-derive an old broker box if a
// backup taken before the rotation ever has to be opened.
//
// # What rotation does not do
//
// It bounds future damage; it does not undo past damage. Everything the old
// key signed still verifies, because it really did sign it. If the key leaked
// at time T, every checkpoint after T is suspect and no rotation changes that.
// Only a witness countersignature obtained before T narrows the window. The
// banner says this out loud rather than letting a green report be over-read.

const (
	incomingDir = "incoming"
	retiredDir  = "retired"
)

func cmdRotate(args []string) {
	fs := flag.NewFlagSet("rotate", flag.ExitOnError)
	data := fs.String("data", defaultDataDir(), "data directory whose node key to rotate")
	reason := fs.String("reason", "routine",
		"why this rotation is happening; recorded in the succession (e.g. \"suspected compromise\")")
	history := fs.Bool("history", false, "print this node's key history and exit, changing nothing")
	yes := fs.Bool("yes", false, "proceed without the confirmation prompt")
	_ = fs.Parse(args)

	if *history {
		if err := runKeyHistory(os.Stdout, *data); err != nil {
			fatal(err)
		}
		return
	}
	if err := runRotate(os.Stdout, os.Stderr, *data, *reason, *yes); err != nil {
		fatal(err)
	}
}

func runRotate(out, errOut io.Writer, dataDir, reason string, yes bool) error {
	keyDir := filepath.Join(dataDir, "identity")
	staged := filepath.Join(keyDir, incomingDir)

	st, err := store.Open(dataDir)
	if err != nil {
		return fmt.Errorf("open %s: %w", dataDir, err)
	}
	defer st.Close()

	outgoing, err := signing.LoadOrCreate(keyDir)
	if err != nil {
		return fmt.Errorf("load the node identity: %w", err)
	}

	// A rotation interrupted between steps 2 and 3 shows up here. Finishing it
	// is correct and starting a second one is not: the database already names
	// the staged key as the successor.
	if resumed, err := resumeInterrupted(out, st, keyDir, staged); err != nil {
		return err
	} else if resumed {
		return nil
	}

	// Never rotate a ledger that does not currently verify. Doing so would
	// carry the break across the succession boundary and make it look like it
	// happened under the new key.
	before, err := verifyRestored(dataDir)
	if err != nil {
		return fmt.Errorf("check the ledger before rotating: %w", err)
	}
	if !before.Sound() {
		return fmt.Errorf(
			"this node's ledger does not currently verify, so rotating would carry the problem "+
				"across the key boundary. Run `aura verify --data %s` and resolve it first", dataDir)
	}

	if !yes {
		return fmt.Errorf(
			"rotation replaces the key that signs this node's evidence and re-encrypts every "+
				"stored secret.\n\n  the node must be stopped — a running one keeps signing with the "+
				"old key in memory\n  current key   %s\n  ledger        %d entries, %d checkpoints, all valid\n\n"+
				"  re-run with --yes to proceed",
			ledger.Fingerprint(outgoing.PublicB64()), before.TotalEntries, before.Checkpoints)
	}

	// The outgoing key's last act is to sign its own final head, so the
	// boundary between the two keys is anchored to a signature rather than to
	// wherever the sequence happened to be.
	ldg, err := ledger.Open(st, readNodeID(dataDir), outgoing)
	if err != nil {
		return fmt.Errorf("open the ledger: %w", err)
	}
	if before.TotalEntries > 0 {
		if err := ldg.Checkpoint(); err != nil {
			return fmt.Errorf("seal a final checkpoint under the outgoing key: %w", err)
		}
	}
	boundary := ldg.Head().Seq

	incoming, err := stageIncomingKey(staged)
	if err != nil {
		return err
	}

	rec, err := signSuccession(boundary, outgoing, incoming, reason)
	if err != nil {
		return err
	}

	brk, err := broker.Open(st, outgoing)
	if err != nil {
		return fmt.Errorf("open the credential broker: %w", err)
	}
	secrets, err := brk.Rekey(rec, incoming)
	if err != nil {
		// Nothing committed: the staged key is now noise, and leaving it would
		// make the next run think a rotation was interrupted.
		_ = os.RemoveAll(staged)
		return fmt.Errorf("rotate: %w", err)
	}

	if err := promoteStagedKey(keyDir, staged, outgoing); err != nil {
		return fmt.Errorf(
			"the succession committed but the key files could not be swapped (%w).\n"+
				"Both keys are on disk; re-run `aura rotate` and it will finish the move", err)
	}

	after, err := verifyRestored(dataDir)
	if err != nil {
		return fmt.Errorf("rotation applied, but re-verification failed: %w", err)
	}
	printRotateReport(out, rec, secrets, after)
	if !after.Sound() {
		return fmt.Errorf("rotation applied, but the ledger no longer verifies — see the report above")
	}
	return nil
}

// stageIncomingKey generates the successor and persists it before anything
// commits, so an interruption can never leave a committed succession pointing
// at a private key that exists only in a dead process's memory.
func stageIncomingKey(staged string) (*signing.Keypair, error) {
	if err := os.MkdirAll(staged, 0o700); err != nil {
		return nil, fmt.Errorf("stage the incoming key: %w", err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate the incoming key: %w", err)
	}
	if err := os.WriteFile(filepath.Join(staged, "ed25519.key"),
		[]byte(base64.StdEncoding.EncodeToString(priv)), 0o600); err != nil {
		return nil, fmt.Errorf("write the incoming private key: %w", err)
	}
	if err := os.WriteFile(filepath.Join(staged, "ed25519.pub"),
		[]byte(base64.StdEncoding.EncodeToString(pub)), 0o644); err != nil {
		return nil, fmt.Errorf("write the incoming public key: %w", err)
	}
	return &signing.Keypair{Public: pub, Private: priv}, nil
}

// signSuccession builds the record and has both keys sign the same payload.
func signSuccession(boundary uint64, outgoing, incoming *signing.Keypair, reason string) (store.SuccessionRow, error) {
	if reason == "" {
		reason = "routine"
	}
	ts := time.Now().UnixMilli()
	from, to := outgoing.PublicB64(), incoming.PublicB64()
	payload := ledger.SuccessionPayload(boundary, from, to, ts)

	rec := store.SuccessionRow{
		Seq: boundary, FromPubkey: from, ToPubkey: to,
		FromSig: outgoing.Sign(payload), ToSig: incoming.Sign(payload),
		Reason: reason, TS: ts,
	}
	// Check our own work before it becomes the thing every future verification
	// depends on. A record that does not verify here would verify nowhere.
	if err := ledger.VerifySuccession(rec); err != nil {
		return store.SuccessionRow{}, fmt.Errorf("built a succession that does not verify: %w", err)
	}
	return rec, nil
}

// promoteStagedKey retires the outgoing keypair and moves the staged one into
// place. Retiring first: at no point is there no key on disk.
func promoteStagedKey(keyDir, staged string, outgoing *signing.Keypair) error {
	retired := filepath.Join(keyDir, retiredDir, ledger.Fingerprint(outgoing.PublicB64()))
	if err := os.MkdirAll(retired, 0o700); err != nil {
		return fmt.Errorf("create the retired key directory: %w", err)
	}
	for _, name := range []string{"ed25519.key", "ed25519.pub"} {
		from := filepath.Join(keyDir, name)
		if _, err := os.Stat(from); err != nil {
			continue
		}
		if err := os.Rename(from, filepath.Join(retired, name)); err != nil {
			return fmt.Errorf("retire %s: %w", name, err)
		}
	}
	for _, name := range []string{"ed25519.key", "ed25519.pub"} {
		if err := os.Rename(filepath.Join(staged, name), filepath.Join(keyDir, name)); err != nil {
			return fmt.Errorf("promote %s: %w", name, err)
		}
	}
	return os.Remove(staged)
}

// resumeInterrupted finishes or discards a staged rotation.
//
// The database is the authority on which happened. If it already carries a
// succession to the staged key, step 2 committed and only the file move is
// outstanding — finish it. If it does not, nothing committed and the staged
// key is noise from a run that died early — discard it and let the caller
// start a fresh rotation.
func resumeInterrupted(out io.Writer, st *store.Store, keyDir, staged string) (bool, error) {
	stagedPub, err := signing.LoadPublicKey(staged)
	if err != nil {
		return false, nil // nothing staged: the ordinary path
	}

	successions, err := st.LedgerSuccessions()
	if err != nil {
		return false, fmt.Errorf("read key successions: %w", err)
	}
	for _, rec := range successions {
		if rec.ToPubkey != stagedPub {
			continue
		}
		outgoing, err := signing.LoadOrCreate(keyDir)
		if err != nil {
			return false, fmt.Errorf("load the outgoing key to retire it: %w", err)
		}
		if err := promoteStagedKey(keyDir, staged, outgoing); err != nil {
			return false, err
		}
		fmt.Fprintf(out, "\n  finished an interrupted rotation: %s → %s\n  The succession had already "+
			"committed; only the key files were outstanding.\n\n",
			ledger.Fingerprint(rec.FromPubkey), ledger.Fingerprint(rec.ToPubkey))
		return true, nil
	}

	if err := os.RemoveAll(staged); err != nil {
		return false, fmt.Errorf("discard the staged key from an interrupted rotation: %w", err)
	}
	fmt.Fprintln(out, "  discarded a staged key from a rotation that never committed")
	return false, nil
}

func printRotateReport(out io.Writer, rec store.SuccessionRow, secrets int, after ledger.Report) {
	fmt.Fprintf(out, "\n  rotated   %s → %s\n",
		ledger.Fingerprint(rec.FromPubkey), ledger.Fingerprint(rec.ToPubkey))
	fmt.Fprintf(out, "  boundary  seq %d — everything up to here stays signed by the retired key\n", rec.Seq)
	fmt.Fprintf(out, "  reason    %s\n", rec.Reason)
	fmt.Fprintf(out, "  secrets   %d re-encrypted under the new key\n", secrets)
	fmt.Fprintf(out, "  ledger    %d entries · %d/%d checkpoints valid · %d rotation(s) in history\n",
		after.TotalEntries, after.CheckpointsValid, after.Checkpoints, after.KeyRotations)
	fmt.Fprintln(out, "\n  The retired private key is kept under identity/retired/. It signs nothing")
	fmt.Fprintln(out, "  now; it is what opens a backup taken before this rotation.")
	fmt.Fprintln(out, "\n  Rotation bounds future damage and does not undo past damage: everything the")
	fmt.Fprintln(out, "  retired key signed still verifies, because it really did sign it. If that key")
	fmt.Fprintln(out, "  leaked, only a witness countersignature from before the leak narrows the window.")
	fmt.Fprintln(out)
}

func runKeyHistory(out io.Writer, dataDir string) error {
	st, err := store.Open(dataDir)
	if err != nil {
		return fmt.Errorf("open %s: %w", dataDir, err)
	}
	defer st.Close()

	pubkey, err := signing.LoadPublicKey(filepath.Join(dataDir, "identity"))
	if err != nil {
		return fmt.Errorf("this data directory has no node identity: %w", err)
	}
	successions, err := st.LedgerSuccessions()
	if err != nil {
		return fmt.Errorf("read key successions: %w", err)
	}
	timeline, err := ledger.BuildKeyTimeline(successions, pubkey)
	if err != nil {
		return fmt.Errorf("the key history does not reconstruct: %w", err)
	}

	fmt.Fprintf(out, "\n  node key history — %d rotation(s)\n\n", timeline.Rotations)
	byFrom := map[string]store.SuccessionRow{}
	for _, rec := range successions {
		byFrom[rec.FromPubkey] = rec
	}
	for i, seg := range timeline.Segments {
		scope := fmt.Sprintf("seq 1–%d", seg.UpTo)
		if i > 0 {
			scope = fmt.Sprintf("seq %d–%d", timeline.Segments[i-1].UpTo+1, seg.UpTo)
		}
		if i == len(timeline.Segments)-1 {
			scope = "current"
			if i > 0 {
				scope = fmt.Sprintf("seq %d– (current)", timeline.Segments[i-1].UpTo+1)
			}
		}
		fmt.Fprintf(out, "  %-14s %-16s", ledger.Fingerprint(seg.Pubkey), scope)
		if rec, ok := byFrom[seg.Pubkey]; ok {
			fmt.Fprintf(out, "  retired %s · %s",
				time.UnixMilli(rec.TS).Format("2006-01-02"), rec.Reason)
		}
		fmt.Fprintln(out)
	}
	fmt.Fprintln(out)
	return nil
}
