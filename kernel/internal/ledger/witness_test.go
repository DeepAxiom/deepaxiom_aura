package ledger

import (
	"encoding/json"
	"strings"
	"testing"

	"aura/kernel/internal/signing"
	"aura/kernel/internal/store"
)

// witnessedNode is a node under observation: its ledger, the store behind it,
// the data dir (for direct tampering) and the keypair it signs with — which
// tests need to hold onto so a re-opened ledger keeps the same identity.
type witnessedNode struct {
	st   *store.Store
	dir  string
	keys *signing.Keypair
	ldg  *Ledger
	id   string
}

// witnessPair builds a node with a ledger and a separate node acting as its
// witness — separate keypairs, because a witness signing with the same key as
// the node it witnesses proves nothing at all.
func witnessPair(t *testing.T) (witnessedNode, *Witness) {
	t.Helper()
	st, dir := testStore(t)
	keys := testKeys(t)
	const id = "node-under-witness"
	ldg, err := Open(st, id, keys)
	if err != nil {
		t.Fatalf("Open node ledger: %v", err)
	}

	witnessStore, _ := testStore(t)
	return witnessedNode{st: st, dir: dir, keys: keys, ldg: ldg, id: id},
		NewWitness(witnessStore, testKeys(t))
}

func sealN(t *testing.T, l *Ledger, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := l.Seal(req("motor.erp.op")); err != nil {
			t.Fatalf("seal %d: %v", i, err)
		}
	}
}

// vouch runs the full present-and-countersign round trip.
func vouch(t *testing.T, n witnessedNode, w *Witness) Countersignature {
	t.Helper()
	seen, err := w.LastSeen(n.id)
	if err != nil {
		t.Fatalf("LastSeen: %v", err)
	}
	stmt, err := n.ldg.Statement(seen)
	if err != nil {
		t.Fatalf("Statement: %v", err)
	}
	cs, err := w.Countersign(stmt)
	if err != nil {
		t.Fatalf("Countersign: %v", err)
	}
	return cs
}

func TestWitnessCountersignsAndVerifies(t *testing.T) {
	n, w := witnessPair(t)
	sealN(t, n.ldg, 5)

	seen, err := w.LastSeen(n.id)
	if err != nil {
		t.Fatalf("LastSeen: %v", err)
	}
	if seen != 0 {
		t.Fatalf("a witness that has never seen this node reported %d", seen)
	}

	stmt, err := n.ldg.Statement(seen)
	if err != nil {
		t.Fatalf("Statement: %v", err)
	}
	cs, err := w.Countersign(stmt)
	if err != nil {
		t.Fatalf("Countersign: %v", err)
	}
	if err := n.ldg.RecordCountersignature(stmt.Seq, stmt.MerkleRoot, "http://witness.test", cs); err != nil {
		t.Fatalf("RecordCountersignature: %v", err)
	}

	if seen, _ = w.LastSeen(n.id); seen != 5 {
		t.Fatalf("witness remembers %d entries, want 5", seen)
	}
}

// The property witnessing exists for: an honest extension is provable, and
// the witness accepts it.
func TestWitnessAcceptsAnHonestExtension(t *testing.T) {
	n, w := witnessPair(t)
	sealN(t, n.ldg, 4)
	vouch(t, n, w)

	sealN(t, n.ldg, 7) // grow honestly

	seen, _ := w.LastSeen(n.id)
	second, err := n.ldg.Statement(seen)
	if err != nil {
		t.Fatalf("Statement after growth: %v", err)
	}
	if second.FromSeq != 4 {
		t.Fatalf("statement anchored at %d, want 4", second.FromSeq)
	}
	if len(second.Consistency) == 0 {
		t.Fatal("an extension from 4 to 11 needs a consistency proof")
	}
	if _, err := w.Countersign(second); err != nil {
		t.Fatalf("witness rejected an honest extension: %v", err)
	}
}

// The property that makes a witness worth more than a second copy of the
// node's own signature: a rewritten history cannot be proven consistent with
// what was already vouched for, even though the node re-signs the result
// perfectly well with its own key and `aura verify` on a repaired chain would
// have nothing to complain about.
func TestWitnessRefusesARewrittenHistory(t *testing.T) {
	n, w := witnessPair(t)
	sealN(t, n.ldg, 6)
	vouch(t, n, w)

	// Rewrite entry 3 in place, the way an operator holding the node's key
	// would, then reopen with the SAME identity so every later signature is
	// genuinely this node's.
	tamperEntry(t, n.dir, 3, func(e *Entry) { e.Capability = "motor.payments.transfer" })

	forked, err := Open(n.st, n.id, n.keys)
	if err != nil {
		t.Fatalf("reopen after rewrite: %v", err)
	}
	sealN(t, forked, 3)

	seen, _ := w.LastSeen(n.id)
	bad, err := forked.Statement(seen)
	if err != nil {
		t.Fatalf("Statement on the forked ledger: %v", err)
	}
	if _, err := w.Countersign(bad); err == nil {
		t.Fatal("the witness vouched for a history that no longer contains what it " +
			"had already signed")
	} else if !strings.Contains(err.Error(), "REFUSING TO VOUCH") {
		t.Fatalf("expected an explicit refusal, got: %v", err)
	}
}

func TestWitnessRefusesAShrinkingLedger(t *testing.T) {
	n, w := witnessPair(t)
	sealN(t, n.ldg, 8)
	stmt, err := n.ldg.Statement(0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Countersign(stmt); err != nil {
		t.Fatal(err)
	}

	shrunk := stmt
	shrunk.Seq = 3
	if _, err := w.Countersign(shrunk); err == nil {
		t.Fatal("the witness vouched for a ledger that had shrunk")
	}
}

func TestWitnessRefusesAnUnsignedStatement(t *testing.T) {
	n, w := witnessPair(t)
	sealN(t, n.ldg, 3)
	stmt, err := n.ldg.Statement(0)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("forged node signature", func(t *testing.T) {
		bad := stmt
		bad.Signature = "Zm9yZ2Vk"
		if _, err := w.Countersign(bad); err == nil {
			t.Fatal("countersigned a head the node did not sign")
		}
	})

	t.Run("head swapped under a valid signature", func(t *testing.T) {
		bad := stmt
		bad.MerkleRoot = LeafHash([]byte("someone else's tree")).String()
		if _, err := w.Countersign(bad); err == nil {
			t.Fatal("countersigned a root the signature does not cover")
		}
	})

	t.Run("empty ledger", func(t *testing.T) {
		bad := stmt
		bad.Seq = 0
		if _, err := w.Countersign(bad); err == nil {
			t.Fatal("countersigned an empty ledger")
		}
	})
}

// A countersignature is bound to the node it was issued for, so it cannot be
// lifted from one node's ledger and presented as vouching for another's.
func TestCountersignatureIsBoundToItsNode(t *testing.T) {
	a, w := witnessPair(t)
	sealN(t, a.ldg, 4)
	stmt, err := a.ldg.Statement(0)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := w.Countersign(stmt)
	if err != nil {
		t.Fatal(err)
	}

	storeB, _ := testStore(t)
	nodeB, err := Open(storeB, "node-someone-else", testKeys(t))
	if err != nil {
		t.Fatal(err)
	}
	sealN(t, nodeB, 4)
	if err := nodeB.RecordCountersignature(stmt.Seq, stmt.MerkleRoot, "http://witness.test", cs); err == nil {
		t.Fatal("a countersignature issued for one node was accepted by another")
	}
}

func TestRecordCountersignatureRejectsAForgedOne(t *testing.T) {
	n, _ := witnessPair(t)
	sealN(t, n.ldg, 2)
	head := n.ldg.Head()
	forged := Countersignature{WitnessKey: head.Pubkey, Signature: "Zm9yZ2Vk"}
	if err := n.ldg.RecordCountersignature(head.Seq, head.MerkleRoot, "x", forged); err == nil {
		t.Fatal("stored a countersignature that does not verify")
	}
}

func TestVerifyReportsWitnesses(t *testing.T) {
	n, w := witnessPair(t)
	sealN(t, n.ldg, 5)
	stmt, err := n.ldg.Statement(0)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := w.Countersign(stmt)
	if err != nil {
		t.Fatal(err)
	}
	if err := n.ldg.RecordCountersignature(stmt.Seq, stmt.MerkleRoot, "http://w", cs); err != nil {
		t.Fatal(err)
	}

	rep, err := Verify(n.st, n.ldg.NodePublicKey())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !rep.Sound() {
		t.Fatalf("a witnessed ledger did not verify: %+v", rep)
	}
	if rep.Witnesses != 1 || rep.WitnessesValid != 1 {
		t.Fatalf("expected 1/1 witnesses, got %d/%d", rep.WitnessesValid, rep.Witnesses)
	}
	if rep.MerkleCheckpoints == 0 {
		t.Fatal("expected the checkpoint to carry a merkle root")
	}
	if rep.MerkleMismatches != 0 {
		t.Fatalf("unexpected merkle mismatches: %d", rep.MerkleMismatches)
	}
}

// The attack self-signing cannot survive: the key's holder rewrites an entry,
// repairs every `prev` pointer after it, discards the old checkpoints and
// re-signs the repaired history with the node's own key.
//
// Everything self-referential then passes — the chain recomputes, the new
// checkpoint verifies, the Merkle root matches the entries. The *only* thing
// in the data directory that disagrees is the witness row signed over the
// head this history used to have.
//
// This is the test the end-to-end run demanded: before it, Verify reported
// SOUND on a ledger in exactly this state, with "0/1 witness
// countersignature(s) verify" printed immediately above the verdict.
func TestVerifyIsNotSoundWhenAWitnessRowNoLongerMatches(t *testing.T) {
	n, w := witnessPair(t)
	sealN(t, n.ldg, 6)

	stmt, err := n.ldg.Statement(0)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := w.Countersign(stmt)
	if err != nil {
		t.Fatal(err)
	}
	if err := n.ldg.RecordCountersignature(stmt.Seq, stmt.MerkleRoot, "http://w", cs); err != nil {
		t.Fatal(err)
	}

	// Rewrite entry 3 and repair the chain behind it, then re-sign.
	rewriteAndRepair(t, n.dir, 3, "motor.payments.transfer")
	repaired, err := Open(n.st, n.id, n.keys)
	if err != nil {
		t.Fatalf("reopen after repair: %v", err)
	}
	if err := repaired.Checkpoint(); err != nil {
		t.Fatalf("re-sign the repaired history: %v", err)
	}

	rep, err := Verify(n.st, repaired.NodePublicKey())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	// Precondition: the attack really did make everything self-referential
	// pass. Without this the test could pass for the wrong reason.
	if !rep.ChainIntact {
		t.Fatalf("precondition failed: the chain should recompute cleanly after repair")
	}
	if rep.CheckpointsValid != rep.Checkpoints || rep.Checkpoints == 0 {
		t.Fatalf("precondition failed: checkpoints should verify (%d/%d)",
			rep.CheckpointsValid, rep.Checkpoints)
	}

	// The verdict.
	if rep.WitnessesValid != 0 || rep.Witnesses != 1 {
		t.Fatalf("expected 0/1 witnesses to verify, got %d/%d",
			rep.WitnessesValid, rep.Witnesses)
	}
	if rep.Sound() {
		t.Fatal("a rewritten-and-re-signed ledger reported SOUND even though a witness " +
			"countersignature over its former history no longer matches")
	}
}

// An unwitnessed ledger is not unsound — the asymmetry Sound() depends on.
func TestVerifyIsSoundWithNoWitnessesAtAll(t *testing.T) {
	n, _ := witnessPair(t)
	sealN(t, n.ldg, 4)
	if err := n.ldg.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	rep, err := Verify(n.st, n.ldg.NodePublicKey())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Witnesses != 0 {
		t.Fatalf("expected no witnesses, got %d", rep.Witnesses)
	}
	if !rep.Sound() {
		t.Fatalf("an unwitnessed but otherwise intact ledger reported unsound: %+v", rep)
	}
}

// rewriteAndRepair performs the full attack against the raw store: change one
// entry, then recompute every `prev` and hash after it so the chain is
// internally consistent again, and drop the checkpoints that no longer fit.
func rewriteAndRepair(t *testing.T, dir string, seq uint64, capability string) {
	t.Helper()
	db := rawDB(t, dir)
	defer db.Close()

	rows, err := db.Query(`SELECT seq, entry FROM ledger_entries ORDER BY seq`)
	if err != nil {
		t.Fatalf("read entries: %v", err)
	}
	var seqs []uint64
	var entries []Entry
	for rows.Next() {
		var s uint64
		var raw string
		if err := rows.Scan(&s, &raw); err != nil {
			t.Fatalf("scan: %v", err)
		}
		var e Entry
		if err := json.Unmarshal([]byte(raw), &e); err != nil {
			t.Fatalf("unmarshal entry %d: %v", s, err)
		}
		seqs = append(seqs, s)
		entries = append(entries, e)
	}
	rows.Close()

	entries[seq-1].Capability = capability
	prev := ""
	for i := range entries {
		entries[i].Prev = prev
		raw, err := json.Marshal(entries[i])
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		h := entries[i].Hash()
		if _, err := db.Exec(`UPDATE ledger_entries SET entry=?, hash=? WHERE seq=?`,
			string(raw), h, seqs[i]); err != nil {
			t.Fatalf("repair entry %d: %v", seqs[i], err)
		}
		prev = h
	}
	if _, err := db.Exec(`DELETE FROM ledger_checkpoints`); err != nil {
		t.Fatalf("drop checkpoints: %v", err)
	}
}

// A ledger whose entries were rewritten keeps its genuinely-signed witness
// rows, and they stop matching the tree those entries now produce. That
// mismatch is the signal `aura verify` surfaces to someone who has only the
// node's own data directory.
func TestVerifyCatchesWitnessRowsThatNoLongerMatch(t *testing.T) {
	n, w := witnessPair(t)
	sealN(t, n.ldg, 6)
	stmt, err := n.ldg.Statement(0)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := w.Countersign(stmt)
	if err != nil {
		t.Fatal(err)
	}
	if err := n.ldg.RecordCountersignature(stmt.Seq, stmt.MerkleRoot, "http://w", cs); err != nil {
		t.Fatal(err)
	}

	tamperEntry(t, n.dir, 2, func(e *Entry) { e.Capability = "motor.payments.transfer" })

	rep, err := Verify(n.st, n.ldg.NodePublicKey())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if rep.Sound() {
		t.Fatal("a rewritten ledger reported itself sound")
	}
	if rep.WitnessesValid != 0 {
		t.Fatalf("a witness row vouching for a head these entries no longer produce "+
			"was counted valid (%d/%d)", rep.WitnessesValid, rep.Witnesses)
	}
}
