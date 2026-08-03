package ledger

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	_ "modernc.org/sqlite" // same pure-Go driver store.Store uses

	"aura/kernel/internal/signing"
	"aura/kernel/internal/spec"
	"aura/kernel/internal/store"
)

// testStore opens a store the normal way and also hands back its data dir,
// which the tamper helpers below need to open a second, raw connection to the
// same kernel.db — simulating someone opening the file directly rather than
// going through the Store API, which is the actual threat model Verify exists
// to catch and which by construction cannot be exercised through Store's own
// honest write path.
func testStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, dir
}

// mustStore is testStore for the majority of tests that never need the raw
// data dir.
func mustStore(t *testing.T) *store.Store {
	t.Helper()
	st, _ := testStore(t)
	return st
}

func testKeys(t *testing.T) *signing.Keypair {
	t.Helper()
	kp, err := signing.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("signing.LoadOrCreate: %v", err)
	}
	return kp
}

func testLedger(t *testing.T, st *store.Store) *Ledger {
	t.Helper()
	l, err := Open(st, "node-test", testKeys(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return l
}

func req(capability string) SealRequest {
	return SealRequest{
		Session: "sess-1", Envelope: "ENV-1", Cause: "ENV-0",
		Actor: "acme/motor/writer@1.0.0", Capability: capability,
		Decision: spec.DecisionAllow, Outcome: spec.OutcomeDelivered,
		Policy: "sha256:policyhash", Payload: json.RawMessage(`{"amount":100}`),
	}
}

// --- sealing and chaining -----------------------------------------------------

func TestSealReturnsAReceiptAndPersists(t *testing.T) {
	l := testLedger(t, mustStore(t))
	receipt, err := l.Seal(req("motor.erp.write"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if receipt == "" {
		t.Fatal("Seal returned an empty receipt")
	}
	sum, err := l.Summarize()
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if sum.Entries != 1 {
		t.Fatalf("Entries = %d, want 1", sum.Entries)
	}
	if sum.LastHash != receipt {
		t.Fatalf("LastHash = %q, want the receipt %q", sum.LastHash, receipt)
	}
}

// The whole point of the chain: entry N+1 cites entry N's hash, so altering
// N's content is detectable from N+1 without needing a separate "hash" column
// anywhere — the linkage itself is the check.
func TestConsecutiveEntriesChain(t *testing.T) {
	st, _ := testStore(t)
	l := testLedger(t, st)

	r1, err := l.Seal(req("motor.erp.write"))
	if err != nil {
		t.Fatalf("Seal 1: %v", err)
	}
	r2, err := l.Seal(req("motor.tts.speak"))
	if err != nil {
		t.Fatalf("Seal 2: %v", err)
	}
	if r1 == r2 {
		t.Fatal("two different effects produced the same receipt")
	}

	entries, err := st.LedgerEntries(1, 0)
	if err != nil {
		t.Fatalf("LedgerEntries: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	var e1, e2 Entry
	_ = json.Unmarshal(entries[0], &e1)
	_ = json.Unmarshal(entries[1], &e2)
	if e1.Prev != "" {
		t.Errorf("genesis entry has prev = %q, want empty", e1.Prev)
	}
	if e2.Prev != r1 {
		t.Errorf("second entry's prev = %q, want the first receipt %q", e2.Prev, r1)
	}
	if e1.Seq != 1 || e2.Seq != 2 {
		t.Errorf("seq = %d, %d; want 1, 2", e1.Seq, e2.Seq)
	}
}

// The payload itself never lands in the ledger — only its hash. This is what
// keeps entries small and keeps a payload containing personal data from
// becoming permanently undeletable.
func TestPayloadIsNeverStoredOnlyItsHash(t *testing.T) {
	st, _ := testStore(t)
	l := testLedger(t, st)

	secret := `{"ssn":"123-45-6789","amount":9999}`
	r := req("motor.erp.write")
	r.Payload = json.RawMessage(secret)
	if _, err := l.Seal(r); err != nil {
		t.Fatalf("Seal: %v", err)
	}

	entries, _ := st.LedgerEntries(1, 0)
	if len(entries) != 1 {
		t.Fatalf("got %d entries", len(entries))
	}
	if bytesContain(entries[0], "123-45-6789") || bytesContain(entries[0], "9999") {
		t.Fatalf("the raw payload leaked into the sealed entry: %s", entries[0])
	}
	var e Entry
	_ = json.Unmarshal(entries[0], &e)
	if e.PayloadSHA256 == "" {
		t.Fatal("payload_sha256 is empty")
	}
}

func bytesContain(b []byte, s string) bool {
	return len(b) > 0 && (string(b) == s || indexOf(string(b), s) >= 0)
}
func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}

// Compensation metadata rides along when the caller supplies it, and is
// simply absent — not zero-valued, absent — when it does not, so a reader can
// tell "no compensation was declared" from "compensation with empty fields".
func TestCompensationMetadataIsCarriedWhenPresent(t *testing.T) {
	st, _ := testStore(t)
	l := testLedger(t, st)

	r := req("motor.erp.write")
	r.Compensation = &Compensation{Capability: "motor.erp.write", Port: "undo_in", Schema: "acme/undo@1"}
	if _, err := l.Seal(r); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	entries, _ := st.LedgerEntries(1, 0)
	var e Entry
	_ = json.Unmarshal(entries[0], &e)
	if e.Compensation == nil || e.Compensation.Port != "undo_in" {
		t.Fatalf("compensation not carried: %+v", e.Compensation)
	}

	if !bytesContain(entries[0], "compensation") {
		t.Fatal("compensation key missing from the JSON entirely")
	}
}

func TestNoCompensationLeavesTheFieldAbsent(t *testing.T) {
	st, _ := testStore(t)
	l := testLedger(t, st)
	if _, err := l.Seal(req("motor.tts.speak")); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	entries, _ := st.LedgerEntries(1, 0)
	if bytesContain(entries[0], "compensation") {
		t.Fatalf("an irreversible effect still mentions compensation: %s", entries[0])
	}
}

// --- undo marking (Phase 2, `aura undo`) ---------------------------------------

// A second entry that undoes the first must carry `compensates` pointing at
// the first entry's own receipt, and the chain must still verify — undo
// marking rides the same hash-chained entry as everything else, not a
// side channel.
func TestCompensatesIsCarriedAndTheChainStillVerifies(t *testing.T) {
	st, _ := testStore(t)
	l := testLedger(t, st)

	original := req("motor.erp.write")
	original.Compensation = &Compensation{Capability: "motor.erp.write", Port: "undo_in", Schema: "acme/undo@1"}
	receipt, err := l.Seal(original)
	if err != nil {
		t.Fatalf("Seal (original): %v", err)
	}

	undo := req("motor.erp.write")
	undo.Compensates = receipt
	undoReceipt, err := l.Seal(undo)
	if err != nil {
		t.Fatalf("Seal (undo): %v", err)
	}

	entries, _ := st.LedgerEntries(1, 0)
	var second Entry
	if err := json.Unmarshal(entries[1], &second); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if second.Compensates != receipt {
		t.Fatalf("Compensates = %q, want the original receipt %q", second.Compensates, receipt)
	}
	if second.Hash() != undoReceipt {
		t.Fatalf("recomputed hash %q does not match the receipt Seal returned %q", second.Hash(), undoReceipt)
	}

	report, err := Verify(st, l.NodePublicKey())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !report.ChainIntact {
		t.Fatalf("chain broken at seq %d: %s", report.BrokenAtSeq, report.BrokenReason)
	}
}

// An ordinary effect — the overwhelming majority — must not mention
// `compensates` at all, the same way TestNoCompensationLeavesTheFieldAbsent
// already holds for `compensation`.
func TestOrdinaryEffectLeavesCompensatesAbsent(t *testing.T) {
	st, _ := testStore(t)
	l := testLedger(t, st)
	if _, err := l.Seal(req("motor.tts.speak")); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	entries, _ := st.LedgerEntries(1, 0)
	if bytesContain(entries[0], "compensates") {
		t.Fatalf("an ordinary effect mentions compensates: %s", entries[0])
	}
}

// --- validation ----------------------------------------------------------------

// A bad caller (a typo'd decision string, say) must be refused before it
// becomes a permanent entry — nothing can edit the chain afterward.
func TestSealRejectsMalformedRequests(t *testing.T) {
	l := testLedger(t, mustStore(t))
	base := req("motor.erp.write")

	cases := []struct {
		name   string
		mutate func(*SealRequest)
	}{
		{"empty capability", func(r *SealRequest) { r.Capability = "" }},
		{"empty actor", func(r *SealRequest) { r.Actor = "" }},
		{"empty policy hash", func(r *SealRequest) { r.Policy = "" }},
		{"unknown decision", func(r *SealRequest) { r.Decision = "maybe" }},
		{"unknown outcome", func(r *SealRequest) { r.Outcome = "sort-of" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := base
			tc.mutate(&r)
			if _, err := l.Seal(r); err == nil {
				t.Fatal("a malformed seal request was accepted")
			}
		})
	}

	sum, err := l.Summarize()
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if sum.Entries != 0 {
		t.Fatalf("a rejected request still produced %d entries", sum.Entries)
	}
}

func TestOpenRequiresAKeypair(t *testing.T) {
	if _, err := Open(mustStore(t), "node-test", nil); err == nil {
		t.Fatal("Open accepted a nil keypair; it would have nothing to sign checkpoints with")
	}
}

// --- resuming across a restart --------------------------------------------------

func TestLedgerResumesTheChainAcrossARestart(t *testing.T) {
	st, _ := testStore(t)
	keys := testKeys(t)

	l1, err := Open(st, "node-test", keys)
	if err != nil {
		t.Fatalf("Open (first): %v", err)
	}
	r1, err := l1.Seal(req("motor.erp.write"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	// Simulate a restart: a fresh Ledger over the same store and keys.
	l2, err := Open(st, "node-test", keys)
	if err != nil {
		t.Fatalf("Open (second): %v", err)
	}
	r2, err := l2.Seal(req("motor.tts.speak"))
	if err != nil {
		t.Fatalf("Seal after reopen: %v", err)
	}

	entries, _ := st.LedgerEntries(1, 0)
	if len(entries) != 2 {
		t.Fatalf("got %d entries across the restart, want 2", len(entries))
	}
	var e2 Entry
	_ = json.Unmarshal(entries[1], &e2)
	if e2.Seq != 2 {
		t.Fatalf("seq after reopen = %d, want 2 (the chain must not reset)", e2.Seq)
	}
	if e2.Prev != r1 {
		t.Fatalf("prev after reopen = %q, want %q (the chain must not fork)", e2.Prev, r1)
	}
	_ = r2
}

// --- checkpoints -----------------------------------------------------------------

func TestCheckpointSignsTheCurrentHead(t *testing.T) {
	st, _ := testStore(t)
	l := testLedger(t, st)
	receipt, err := l.Seal(req("motor.erp.write"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if err := l.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}

	cps, err := st.LedgerCheckpoints()
	if err != nil {
		t.Fatalf("LedgerCheckpoints: %v", err)
	}
	if len(cps) != 1 {
		t.Fatalf("got %d checkpoints, want 1", len(cps))
	}
	if cps[0].HeadHash != receipt {
		t.Fatalf("checkpoint head = %q, want the last receipt %q", cps[0].HeadHash, receipt)
	}
	if err := signing.Verify(cps[0].Pubkey, cps[0].Signature,
		checkpointPayload(cps[0].Seq, cps[0].HeadHash)); err != nil {
		t.Fatalf("checkpoint signature does not verify: %v", err)
	}
}

func TestCheckpointOnAnEmptyLedgerIsAnError(t *testing.T) {
	l := testLedger(t, mustStore(t))
	if err := l.Checkpoint(); err == nil {
		t.Fatal("checkpointing an empty ledger succeeded")
	}
}

// Automatic cadence: every checkpointEveryN entries, one gets sealed without
// anyone calling Checkpoint() explicitly.
func TestAutomaticCheckpointCadence(t *testing.T) {
	st, _ := testStore(t)
	l := testLedger(t, st)

	for i := 0; i < checkpointEveryN; i++ {
		if _, err := l.Seal(req(fmt.Sprintf("motor.erp.op%d", i))); err != nil {
			t.Fatalf("seal %d: %v", i, err)
		}
	}
	cps, err := st.LedgerCheckpoints()
	if err != nil {
		t.Fatalf("LedgerCheckpoints: %v", err)
	}
	if len(cps) != 1 {
		t.Fatalf("got %d checkpoints after %d entries, want exactly 1", len(cps), checkpointEveryN)
	}
	if cps[0].Seq != checkpointEveryN {
		t.Fatalf("checkpoint covers seq %d, want %d", cps[0].Seq, checkpointEveryN)
	}
}

func TestNoCheckpointBeforeCadenceIsDue(t *testing.T) {
	st, _ := testStore(t)
	l := testLedger(t, st)
	for i := 0; i < checkpointEveryN-1; i++ {
		if _, err := l.Seal(req(fmt.Sprintf("motor.erp.op%d", i))); err != nil {
			t.Fatalf("seal %d: %v", i, err)
		}
	}
	cps, _ := st.LedgerCheckpoints()
	if len(cps) != 0 {
		t.Fatalf("got a checkpoint after only %d entries; cadence is %d", checkpointEveryN-1, checkpointEveryN)
	}
}

// A restart must not lose track of "how long since the last checkpoint" — or
// every restart would immediately reseal one, and a node restarted often would
// checkpoint far more often than the cadence promises.
func TestCheckpointCadenceSurvivesARestart(t *testing.T) {
	st, _ := testStore(t)
	keys := testKeys(t)

	l1, _ := Open(st, "node-test", keys)
	for i := 0; i < 40; i++ {
		if _, err := l1.Seal(req(fmt.Sprintf("motor.erp.op%d", i))); err != nil {
			t.Fatalf("seal %d: %v", i, err)
		}
	}
	if err := l1.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}

	l2, _ := Open(st, "node-test", keys)
	for i := 40; i < 40+checkpointEveryN-1; i++ {
		if _, err := l2.Seal(req(fmt.Sprintf("motor.erp.op%d", i))); err != nil {
			t.Fatalf("seal %d: %v", i, err)
		}
	}
	cps, _ := st.LedgerCheckpoints()
	if len(cps) != 1 {
		t.Fatalf("got %d checkpoints; the resumed count should not have triggered a second one yet "+
			"(sealed %d since the first checkpoint, cadence is %d)",
			len(cps), checkpointEveryN-1, checkpointEveryN)
	}
}

// --- Verify: the honest case -----------------------------------------------------

func TestVerifyOnAnEmptyLedgerIsIntact(t *testing.T) {
	st, _ := testStore(t)
	report, err := Verify(st, "")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !report.ChainIntact || report.TotalEntries != 0 {
		t.Fatalf("empty ledger report = %+v; want intact with 0 entries", report)
	}
	if !report.Sound() {
		t.Fatal("an empty, untampered ledger should be Sound")
	}
}

func TestVerifyOnAnUntamperedLedgerIsSound(t *testing.T) {
	st, _ := testStore(t)
	l := testLedger(t, st)
	for i := 0; i < 10; i++ {
		if _, err := l.Seal(req(fmt.Sprintf("motor.erp.op%d", i))); err != nil {
			t.Fatalf("seal %d: %v", i, err)
		}
	}
	if err := l.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}

	report, err := Verify(st, l.NodePublicKey())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !report.Sound() {
		t.Fatalf("an untampered, checkpointed ledger reported unsound: %+v", report)
	}
	if report.TotalEntries != 10 {
		t.Errorf("TotalEntries = %d, want 10", report.TotalEntries)
	}
	if report.Checkpoints != 1 || report.CheckpointsValid != 1 {
		t.Errorf("checkpoints = %d/%d valid, want 1/1", report.CheckpointsValid, report.Checkpoints)
	}
}

// Verifying with no key at all still catches chain tampering — the chain and
// the signature are two independent guarantees, and losing the key must not
// mean losing the ability to detect an altered entry.
func TestVerifyWithNoKeyStillChecksTheChain(t *testing.T) {
	st, _ := testStore(t)
	l := testLedger(t, st)
	if _, err := l.Seal(req("motor.erp.write")); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	report, err := Verify(st, "")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if report.KeyAvailable {
		t.Fatal("KeyAvailable is true despite no key being given")
	}
	if !report.ChainIntact {
		t.Fatal("the chain check should still run with no key")
	}
	if !report.Sound() {
		t.Fatal("an intact chain with no key to check signatures should still be Sound")
	}
}

// --- Verify: tamper detection — the property the whole design rests on --------

// THE regression the roadmap promised closing: altering a row must make
// verification fail. This is the simple case — an interior entry, so the
// forward hash chain alone catches it without needing a checkpoint at all.
func TestVerifyDetectsATamperedInteriorEntry(t *testing.T) {
	st, dir := testStore(t)
	l := testLedger(t, st)
	for i := 0; i < 5; i++ {
		if _, err := l.Seal(req(fmt.Sprintf("motor.erp.op%d", i))); err != nil {
			t.Fatalf("seal %d: %v", i, err)
		}
	}

	tamperEntry(t, dir, 3, func(e *Entry) { e.Capability = "motor.payments.send" })

	report, err := Verify(st, l.NodePublicKey())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if report.ChainIntact {
		t.Fatal("tampering with entry 3's content was not detected")
	}
	if report.BrokenAtSeq != 4 {
		// Entry 3 was altered, so its recomputed hash no longer matches what
		// entry 4 recorded as `prev` — the break surfaces one entry later,
		// which is the correct diagnosis: "4 disagrees with 3", not "3 is bad"
		// (verify cannot know which of the two is the forgery from hashes
		// alone; it can only say where the chain stopped agreeing with itself).
		t.Errorf("BrokenAtSeq = %d, want 4 (where the disagreement becomes visible)", report.BrokenAtSeq)
	}
	if report.Sound() {
		t.Fatal("a tampered ledger reported itself Sound")
	}
}

// The harder case, and the one that actually justifies checkpoints existing
// at all: an attacker with direct database access does not merely edit one
// row, they edit a row *and* repair every `prev` pointer after it so the
// chain stays internally self-consistent. Plain hash-chaining cannot catch
// this — by construction, the doctored chain recomputes cleanly. A signed
// checkpoint can, because the attacker cannot forge a signature over the new
// (wrong) head without the node's private key.
func TestVerifyDetectsATamperedEntryEvenWhenTheChainWasRepaired(t *testing.T) {
	st, dir := testStore(t)
	l := testLedger(t, st)
	for i := 0; i < 5; i++ {
		if _, err := l.Seal(req(fmt.Sprintf("motor.erp.op%d", i))); err != nil {
			t.Fatalf("seal %d: %v", i, err)
		}
	}
	if err := l.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}

	// Forge: alter entry 3, then rewrite entries 4 and 5 so their `prev`
	// chain and the JSON bytes are internally consistent again. No access to
	// the node's private key is assumed or used.
	forgeChainFrom(t, dir, 3, func(e *Entry) { e.Capability = "motor.payments.send" })

	report, err := Verify(st, l.NodePublicKey())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !report.ChainIntact {
		t.Fatal("the forged chain should recompute as internally consistent — that is the point being tested")
	}
	if report.CheckpointsValid != 0 {
		t.Fatalf("the checkpoint signed the ORIGINAL entry 5; it must not validate against the forged one "+
			"(CheckpointsValid = %d)", report.CheckpointsValid)
	}
	if report.Sound() {
		t.Fatal("a forged-but-self-consistent chain reported itself Sound; " +
			"the checkpoint signature is supposed to be exactly what stops this")
	}
}

func TestVerifyDetectsADeletedEntry(t *testing.T) {
	st, dir := testStore(t)
	l := testLedger(t, st)
	for i := 0; i < 5; i++ {
		if _, err := l.Seal(req(fmt.Sprintf("motor.erp.op%d", i))); err != nil {
			t.Fatalf("seal %d: %v", i, err)
		}
	}
	deleteEntry(t, dir, 3)

	report, err := Verify(st, l.NodePublicKey())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if report.ChainIntact {
		t.Fatal("deleting entry 3 was not detected")
	}
	if report.BrokenAtSeq != 3 {
		t.Errorf("BrokenAtSeq = %d, want 3 (the position where the gap appears)", report.BrokenAtSeq)
	}
}

// A checkpoint whose signature does not verify — a forged signature, or one
// made with the wrong key — must not be silently accepted as valid.
func TestVerifyRejectsAForgedCheckpointSignature(t *testing.T) {
	st, _ := testStore(t)
	l := testLedger(t, st)
	if _, err := l.Seal(req("motor.erp.write")); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if err := l.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}

	other := testKeys(t) // a different node's key
	report, err := Verify(st, other.PublicB64())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if report.CheckpointsValid != 0 {
		t.Fatalf("a checkpoint verified against the wrong public key (%d valid)", report.CheckpointsValid)
	}
}

// --- concurrency -----------------------------------------------------------------

// A busy node seals effects from more than one session at once. The chain
// must stay strictly ordered and gap-free regardless.
func TestConcurrentSealsProduceAValidChain(t *testing.T) {
	st, _ := testStore(t)
	l := testLedger(t, st)

	const n = 50
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := l.Seal(req(fmt.Sprintf("motor.erp.op%d", i))); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent seal failed: %v", err)
	}

	report, err := Verify(st, l.NodePublicKey())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if report.TotalEntries != n {
		t.Fatalf("TotalEntries = %d, want %d", report.TotalEntries, n)
	}
	if !report.ChainIntact {
		t.Fatalf("concurrent sealing produced a broken chain: %+v", report)
	}
}

// --- Fingerprint -----------------------------------------------------------------

func TestFingerprintIsStableAndDistinguishing(t *testing.T) {
	a := testKeys(t)
	b := testKeys(t)
	if Fingerprint(a.PublicB64()) != Fingerprint(a.PublicB64()) {
		t.Error("the same key fingerprinted differently across calls")
	}
	if Fingerprint(a.PublicB64()) == Fingerprint(b.PublicB64()) {
		t.Error("two different keys share a fingerprint")
	}
	if Fingerprint("not valid base64!!") == "" {
		t.Error("an invalid key should still return something rather than panic")
	}
}

// --- test helpers that mutate the store directly, simulating DB-level tampering ---

func tamperEntry(t *testing.T, dir string, seq uint64, mutate func(*Entry)) {
	t.Helper()
	db := rawDB(t, dir)
	var raw string
	if err := db.QueryRow(`SELECT entry FROM ledger_entries WHERE seq=?`, seq).Scan(&raw); err != nil {
		t.Fatalf("could not read entry %d to tamper with: %v", seq, err)
	}
	var e Entry
	if err := json.Unmarshal([]byte(raw), &e); err != nil {
		t.Fatal(err)
	}
	mutate(&e)
	tampered, _ := json.Marshal(e)
	if _, err := db.Exec(`UPDATE ledger_entries SET entry=? WHERE seq=?`, string(tampered), seq); err != nil {
		t.Fatalf("tamper UPDATE: %v", err)
	}
}

func deleteEntry(t *testing.T, dir string, seq uint64) {
	t.Helper()
	db := rawDB(t, dir)
	if _, err := db.Exec(`DELETE FROM ledger_entries WHERE seq=?`, seq); err != nil {
		t.Fatalf("tamper DELETE: %v", err)
	}
}

// forgeChainFrom simulates a privileged attacker who edits entry `seq` and
// then repairs every subsequent entry's `prev`/content so the chain recomputes
// as internally consistent — the scenario only a signed checkpoint catches.
func forgeChainFrom(t *testing.T, dir string, seq uint64, mutate func(*Entry)) {
	t.Helper()
	db := rawDB(t, dir)

	rows, err := db.Query(`SELECT entry FROM ledger_entries ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	var parsed []Entry
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var e Entry
		if err := json.Unmarshal([]byte(raw), &e); err != nil {
			t.Fatal(err)
		}
		parsed = append(parsed, e)
	}
	rows.Close()

	idx := int(seq) - 1
	mutate(&parsed[idx])
	prevHash := ""
	if idx > 0 {
		prevHash = parsed[idx-1].Hash()
	}
	for i := idx; i < len(parsed); i++ {
		parsed[i].Prev = prevHash
		prevHash = parsed[i].Hash()
		raw, _ := json.Marshal(parsed[i])
		if _, err := db.Exec(`UPDATE ledger_entries SET entry=? WHERE seq=?`,
			string(raw), parsed[i].Seq); err != nil {
			t.Fatalf("forge UPDATE seq %d: %v", parsed[i].Seq, err)
		}
	}
}

// rawDB opens a second, independent connection to the same kernel.db a
// *store.Store already has open — standing in for "someone opened the file
// directly" rather than going through the Store API, which is the actual
// threat model Verify exists to catch and which cannot, by definition, be
// exercised through Store's own honest write path. WAL mode is what makes
// this safe to do concurrently with the Store's own connection.
func rawDB(t *testing.T, dir string) *sql.DB {
	t.Helper()
	dsn := filepath.Join(dir, "kernel.db") + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
