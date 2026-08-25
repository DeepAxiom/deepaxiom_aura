package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aura/kernel/internal/ledger"
	"aura/kernel/internal/signing"
	"aura/kernel/internal/store"
)

// The property rotation exists for: after replacing the signing key, a
// verifier handed *only the new public key* still validates every checkpoint
// the retired key ever wrote. Before C4 v1.6 that was impossible, which is why
// nobody rotated.

// sealMore adds effects and a checkpoint under whatever key currently sits in
// identity/ — the way the node itself would after a restart.
func sealMore(t *testing.T, dataDir string, n int, tag string) {
	t.Helper()
	st, err := store.Open(dataDir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	keys, err := signing.LoadOrCreate(filepath.Join(dataDir, "identity"))
	if err != nil {
		t.Fatalf("keys: %v", err)
	}
	ldg, err := ledger.Open(st, readNodeID(dataDir), keys)
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	for i := 0; i < n; i++ {
		if _, err := ldg.Seal(ledger.SealRequest{
			Session: "sess-" + tag, Envelope: tag + string(rune('a'+i)), Cause: "ENV-0",
			Actor: "acme/motor/writer@1.0.0", Capability: "motor.erp.write",
			Decision: "allow", Outcome: "delivered",
			Policy: "sha256:policyhash", Payload: json.RawMessage(`{}`),
		}); err != nil {
			t.Fatalf("seal: %v", err)
		}
	}
	if err := ldg.Checkpoint(); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
}

func rotate(t *testing.T, dataDir, reason string) string {
	t.Helper()
	var out, errOut bytes.Buffer
	if err := runRotate(&out, &errOut, dataDir, reason, true); err != nil {
		t.Fatalf("runRotate: %v\nstdout: %s", err, out.String())
	}
	return out.String()
}

func TestAfterRotationTheWholeHistoryStillVerifiesUnderTheNewKeyAlone(t *testing.T) {
	dir := newNodeDataDir(t, 3) // three effects and a checkpoint under key A
	keyA, err := signing.LoadPublicKey(filepath.Join(dir, "identity"))
	if err != nil {
		t.Fatalf("read key A: %v", err)
	}
	before, err := verifyRestored(dir)
	if err != nil || before.Checkpoints == 0 {
		t.Fatalf("fixture is not checkpointed: %+v %v", before, err)
	}

	report := rotate(t, dir, "suspected compromise")

	keyB, err := signing.LoadPublicKey(filepath.Join(dir, "identity"))
	if err != nil {
		t.Fatalf("read key B: %v", err)
	}
	if keyB == keyA {
		t.Fatal("the key on disk did not change")
	}
	sealMore(t, dir, 2, "post") // more effects, checkpointed under key B

	// The verification an auditor performs: this data directory, this public
	// key, nothing else.
	after, err := verifyRestored(dir)
	if err != nil {
		t.Fatalf("verify after rotation: %v", err)
	}
	if !after.Sound() {
		t.Fatalf("ledger is not sound after rotation: %+v", after)
	}
	if after.KeyRotations != 1 {
		t.Errorf("report says %d rotations, expected 1", after.KeyRotations)
	}
	// This is the assertion that would have failed before this feature: the
	// checkpoints written by key A are still counted valid.
	if after.CheckpointsValid != after.Checkpoints {
		t.Errorf("%d of %d checkpoints verify — the retired key's were orphaned",
			after.CheckpointsValid, after.Checkpoints)
	}
	if after.Checkpoints <= before.Checkpoints {
		t.Errorf("expected checkpoints on both sides of the boundary, got %d", after.Checkpoints)
	}
	if !strings.Contains(report, "suspected compromise") {
		t.Errorf("the reason was not recorded in the report: %s", report)
	}

	// The retired private key is kept, because it is what opens a backup taken
	// before the rotation.
	retired := filepath.Join(dir, "identity", retiredDir, ledger.Fingerprint(keyA), "ed25519.key")
	if _, err := os.Stat(retired); err != nil {
		t.Errorf("the retired private key was not kept at %s: %v", retired, err)
	}
}

func TestRotatingTwiceKeepsBothRetiredKeysVerifying(t *testing.T) {
	dir := newNodeDataDir(t, 2)
	rotate(t, dir, "first")
	sealMore(t, dir, 2, "mid")
	rotate(t, dir, "second")
	sealMore(t, dir, 2, "last")

	report, err := verifyRestored(dir)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !report.Sound() {
		t.Fatalf("not sound after two rotations: %+v", report)
	}
	if report.KeyRotations != 2 {
		t.Errorf("report says %d rotations, expected 2", report.KeyRotations)
	}
	if len(report.KeyTimeline) != 3 {
		t.Errorf("timeline has %d segments, expected 3", len(report.KeyTimeline))
	}
	if report.CheckpointsValid != report.Checkpoints {
		t.Errorf("%d of %d checkpoints verify across two rotations",
			report.CheckpointsValid, report.Checkpoints)
	}
}

func TestATamperedSuccessionMakesTheLedgerUnsound(t *testing.T) {
	dir := newNodeDataDir(t, 2)
	rotate(t, dir, "routine")
	sealMore(t, dir, 1, "post")

	// Someone with write access to the file edits the handover to point at a
	// key they hold. The signatures no longer cover what the row says.
	tamperKernelDB(t, dir, `UPDATE ledger_successions SET seq = seq + 5`)

	report, err := verifyRestored(dir)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if report.Sound() {
		t.Fatal("a tampered succession record verified as sound")
	}
	if report.SuccessionFailure == "" {
		t.Error("the report does not say the succession chain is the problem")
	}
}

func TestRotationIsRefusedOnALedgerThatDoesNotVerify(t *testing.T) {
	dir := newNodeDataDir(t, 3)
	tamperKernelDB(t, dir, `UPDATE ledger_entries SET entry = replace(entry, 'delivered', 'refused')`)

	var out, errOut bytes.Buffer
	err := runRotate(&out, &errOut, dir, "routine", true)
	if err == nil {
		t.Fatal("rotated a ledger that does not verify")
	}
	if !strings.Contains(err.Error(), "does not currently verify") {
		t.Errorf("error does not explain the refusal: %v", err)
	}
	// And nothing was changed on the way to refusing.
	if got, _ := os.ReadDir(filepath.Join(dir, "identity")); len(got) > 2 {
		t.Errorf("the refused rotation left files behind: %d entries in identity/", len(got))
	}
}

func TestRotationWithoutYesExplainsItselfAndChangesNothing(t *testing.T) {
	dir := newNodeDataDir(t, 2)
	before, _ := signing.LoadPublicKey(filepath.Join(dir, "identity"))

	var out, errOut bytes.Buffer
	err := runRotate(&out, &errOut, dir, "routine", false)
	if err == nil {
		t.Fatal("rotated without confirmation")
	}
	for _, want := range []string{"--yes", "node must be stopped", "current key"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
	if after, _ := signing.LoadPublicKey(filepath.Join(dir, "identity")); after != before {
		t.Error("the key changed on a run that was supposed to change nothing")
	}
}

func TestAnInterruptedRotationIsFinishedRatherThanRestarted(t *testing.T) {
	dir := newNodeDataDir(t, 2)
	keyDir := filepath.Join(dir, "identity")
	staged := filepath.Join(keyDir, incomingDir)

	// Reproduce a crash between the commit and the file move: the succession
	// is in the database, the staged key is on disk, identity/ still holds the
	// outgoing key.
	outgoing, err := signing.LoadOrCreate(keyDir)
	if err != nil {
		t.Fatalf("keys: %v", err)
	}
	incoming, err := stageIncomingKey(staged)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	rec, err := signSuccession(2, outgoing, incoming, "interrupted")
	if err != nil {
		t.Fatalf("sign succession: %v", err)
	}
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if _, err := st.CommitSuccession(rec, func(_, ct string) (string, error) { return ct, nil }); err != nil {
		t.Fatalf("commit: %v", err)
	}
	st.Close()

	// The next run finishes the move instead of starting a second rotation.
	var out, errOut bytes.Buffer
	if err := runRotate(&out, &errOut, dir, "routine", true); err != nil {
		t.Fatalf("resume: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "finished an interrupted rotation") {
		t.Errorf("the run did not report a resume: %s", out.String())
	}
	if active, _ := signing.LoadPublicKey(keyDir); active != incoming.PublicB64() {
		t.Error("identity/ does not hold the key the committed succession names")
	}
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Error("the staging directory outlived the rotation")
	}
	if got, _ := verifyRestored(dir); !got.Sound() || got.KeyRotations != 1 {
		t.Errorf("resumed node does not verify cleanly: sound=%v rotations=%d",
			got.Sound(), got.KeyRotations)
	}
}

func TestAStagedKeyFromARunThatNeverCommittedIsDiscarded(t *testing.T) {
	dir := newNodeDataDir(t, 2)
	staged := filepath.Join(dir, "identity", incomingDir)
	abandoned, err := stageIncomingKey(staged)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}

	var out, errOut bytes.Buffer
	if err := runRotate(&out, &errOut, dir, "routine", true); err != nil {
		t.Fatalf("runRotate: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "discarded a staged key") {
		t.Errorf("the abandoned key was not reported: %s", out.String())
	}
	// It rotated to a *fresh* key, not to the abandoned one.
	if active, _ := signing.LoadPublicKey(filepath.Join(dir, "identity")); active == abandoned.PublicB64() {
		t.Error("rotation adopted a key from a run that never committed")
	}
	if got, _ := verifyRestored(dir); !got.Sound() {
		t.Error("the node does not verify after discarding a staged key")
	}
}

func TestKeyHistoryNamesEveryKeyAndWhyItWasRetired(t *testing.T) {
	dir := newNodeDataDir(t, 2)
	original, _ := signing.LoadPublicKey(filepath.Join(dir, "identity"))
	rotate(t, dir, "suspected compromise")

	var out bytes.Buffer
	if err := runKeyHistory(&out, dir); err != nil {
		t.Fatalf("runKeyHistory: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		ledger.Fingerprint(original), "suspected compromise", "1 rotation", "current",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("history does not mention %q:\n%s", want, got)
		}
	}
}

func TestBackupAfterRotationRestoresANodeThatStillVerifies(t *testing.T) {
	// Rotation and backup both touch identity/; a node that rotated must still
	// round-trip, and the restored copy must carry the succession with it.
	dir := newNodeDataDir(t, 2)
	rotate(t, dir, "routine")
	sealMore(t, dir, 2, "post")

	archive := backupTo(t, dir)
	restored := filepath.Join(t.TempDir(), "restored")
	var out, errOut bytes.Buffer
	if err := runRestore(&out, &errOut, archive, restored, false); err != nil {
		t.Fatalf("runRestore: %v", err)
	}
	report, err := verifyRestored(restored)
	if err != nil {
		t.Fatalf("verify restored: %v", err)
	}
	if !report.Sound() || report.KeyRotations != 1 {
		t.Errorf("restored rotated node: sound=%v rotations=%d", report.Sound(), report.KeyRotations)
	}
}
