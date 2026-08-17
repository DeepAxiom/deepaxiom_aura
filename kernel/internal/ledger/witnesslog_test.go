package ledger

import (
	"encoding/json"
	"testing"
)

// What these defend: a witness is the party everyone else is asked to rely on,
// so the interesting failures are the ones where the *witness* is dishonest.
// Every test below is an attempt by a witness to get away with something.

func TestCountersigningAppendsToTheWitnessOwnLog(t *testing.T) {
	node, wit := witnessPair(t)
	sealN(t, node.ldg, 3)

	stmt, err := node.ldg.Statement(0)
	if err != nil {
		t.Fatalf("statement: %v", err)
	}
	cs, err := wit.Countersign(stmt)
	if err != nil {
		t.Fatalf("countersign: %v", err)
	}
	if cs.LogSeq != 1 {
		t.Errorf("countersignature reports log position %d, want 1", cs.LogSeq)
	}

	head := wit.LogHead()
	if head.Size != 1 {
		t.Fatalf("witness log size = %d, want 1", head.Size)
	}
	if err := VerifyLogHead(head); err != nil {
		t.Fatalf("the witness's own head does not verify: %v", err)
	}

	entries, err := wit.LogEntries(1, 0)
	if err != nil || len(entries) != 1 {
		t.Fatalf("log entries = %v, err %v", entries, err)
	}
	if entries[0].Node != node.id || entries[0].NodeSeq != stmt.Seq {
		t.Errorf("logged entry does not describe what was signed: %+v", entries[0])
	}
	if entries[0].Signature != cs.Signature {
		t.Error("the log records a different signature than the one handed out")
	}
}

// The published root has to be the root the published entries produce. A
// witness that signs one thing and serves another is exactly what a monitor
// exists to catch, so the tree must be rebuildable from the log alone.
func TestPublishedRootIsRebuildableFromThePublishedEntries(t *testing.T) {
	node, wit := witnessPair(t)
	for i := 0; i < 4; i++ {
		sealN(t, node.ldg, 2)
		stmt, err := node.ldg.Statement(lastSeenOf(t, wit, node.id))
		if err != nil {
			t.Fatalf("statement %d: %v", i, err)
		}
		if _, err := wit.Countersign(stmt); err != nil {
			t.Fatalf("countersign %d: %v", i, err)
		}
	}

	head := wit.LogHead()
	entries, err := wit.LogEntries(1, 0)
	if err != nil {
		t.Fatalf("entries: %v", err)
	}
	leaves := make([]Hash, len(entries))
	for i, e := range entries {
		raw, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("marshal entry %d: %v", i, err)
		}
		leaves[i] = LeafHash(raw)
	}
	if got := MerkleRoot(leaves).String(); got != head.Root {
		t.Fatalf("the root the witness signed (%s) is not the root its entries produce (%s)",
			head.Root, got)
	}
}

// A monitor's core check: the witness must be able to prove today's log still
// contains yesterday's, unchanged.
func TestWitnessLogProvesItsOwnConsistency(t *testing.T) {
	node, wit := witnessPair(t)
	sealN(t, node.ldg, 2)
	stmt, _ := node.ldg.Statement(0)
	if _, err := wit.Countersign(stmt); err != nil {
		t.Fatalf("countersign: %v", err)
	}
	early := wit.LogHead()

	for i := 0; i < 3; i++ {
		sealN(t, node.ldg, 2)
		s, err := node.ldg.Statement(lastSeenOf(t, wit, node.id))
		if err != nil {
			t.Fatalf("statement: %v", err)
		}
		if _, err := wit.Countersign(s); err != nil {
			t.Fatalf("countersign: %v", err)
		}
	}
	later := wit.LogHead()
	if later.Size <= early.Size {
		t.Fatalf("log did not grow: %d → %d", early.Size, later.Size)
	}

	proof, err := wit.LogConsistency(early.Size, later.Size)
	if err != nil {
		t.Fatalf("consistency proof: %v", err)
	}
	oldRoot, _ := ParseHash(early.Root)
	newRoot, _ := ParseHash(later.Root)
	hashes := make([]Hash, len(proof))
	for i, s := range proof {
		hashes[i], _ = ParseHash(s)
	}
	if !VerifyConsistency(int(early.Size), int(later.Size), oldRoot, newRoot, hashes) {
		t.Fatal("a witness that only appended could not prove it")
	}

	// And the negative: a root that is not what the witness published must not
	// verify against the same proof, or the check proves nothing.
	bogus := EmptyRoot()
	if VerifyConsistency(int(early.Size), int(later.Size), bogus, newRoot, hashes) {
		t.Fatal("consistency verified against a root the witness never published")
	}
}

// An inclusion proof is what turns "a witness signed this for me" into
// something a third party can check: the anchor was published, not handed over
// in private.
func TestInclusionProvesACountersignatureWasPublished(t *testing.T) {
	node, wit := witnessPair(t)
	sealN(t, node.ldg, 2)
	stmt, _ := node.ldg.Statement(0)
	cs, err := wit.Countersign(stmt)
	if err != nil {
		t.Fatalf("countersign: %v", err)
	}

	rec, proof, size, err := wit.LogInclusion(cs.LogSeq)
	if err != nil {
		t.Fatalf("inclusion: %v", err)
	}
	raw, _ := json.Marshal(rec)
	root, _ := ParseHash(wit.LogHead().Root)
	hashes := make([]Hash, len(proof))
	for i, s := range proof {
		hashes[i], _ = ParseHash(s)
	}
	if !VerifyInclusion(LeafHash(raw), int(cs.LogSeq-1), int(size), hashes, root) {
		t.Fatal("a countersignature the witness issued does not prove to be in its own log")
	}
}

// The load-bearing one. An unsigned last-seen let a witness tell a node one
// thing and an auditor another with neither able to prove it. Signed, the two
// answers are two statements over the same key that cannot both be true.
func TestLastSeenIsSignedAndCommitsToTheWitnessOwnLog(t *testing.T) {
	node, wit := witnessPair(t)
	sealN(t, node.ldg, 5)
	stmt, _ := node.ldg.Statement(0)
	if _, err := wit.Countersign(stmt); err != nil {
		t.Fatalf("countersign: %v", err)
	}

	s, err := wit.LastSeenSigned(node.id)
	if err != nil {
		t.Fatalf("last seen: %v", err)
	}
	if err := VerifyLastSeen(s); err != nil {
		t.Fatalf("a witness's own last-seen answer does not verify: %v", err)
	}
	if s.Seq != stmt.Seq {
		t.Errorf("last-seen reports %d, the witness vouched for %d", s.Seq, stmt.Seq)
	}
	if s.LogSize != wit.LogHead().Size || s.LogRoot == "" {
		t.Error("last-seen does not commit to the witness's own log position")
	}

	// Every field is inside the signature, so none of them can be edited after
	// the fact and still verify.
	for name, mangle := range map[string]func(*LastSeenStatement){
		"seq":      func(x *LastSeenStatement) { x.Seq += 1 },
		"node":     func(x *LastSeenStatement) { x.Node = "someone-else" },
		"root":     func(x *LastSeenStatement) { x.MerkleRoot = EmptyRoot().String() },
		"log size": func(x *LastSeenStatement) { x.LogSize += 1 },
		"log root": func(x *LastSeenStatement) { x.LogRoot = EmptyRoot().String() },
		"ts":       func(x *LastSeenStatement) { x.TS -= 100000 },
	} {
		t.Run("tampered "+name, func(t *testing.T) {
			bad := s
			mangle(&bad)
			if err := VerifyLastSeen(bad); err == nil {
				t.Errorf("editing %s left the statement verifying", name)
			}
		})
	}
}

// A witness answering "never seen this node" is also making a claim, and it
// must be one it can be held to — otherwise denial is the one free lie.
func TestNeverSeenIsAlsoASignedAnswer(t *testing.T) {
	_, wit := witnessPair(t)
	s, err := wit.LastSeenSigned("node-nobody-ever-heard-of")
	if err != nil {
		t.Fatalf("last seen: %v", err)
	}
	if s.Seq != 0 || s.MerkleRoot != "" {
		t.Errorf("expected a zero answer, got %+v", s)
	}
	if err := VerifyLastSeen(s); err != nil {
		t.Fatalf("a signed denial does not verify: %v", err)
	}
}

// The detector. Two statements the witness cannot reconcile are the evidence
// that it served a split view.
func TestContradictionDetectsASplitView(t *testing.T) {
	base := LastSeenStatement{
		WitnessKey: "k", Node: "node-a", Seq: 100, MerkleRoot: "sha256:aa",
		LogSize: 50, LogRoot: "sha256:rr", TS: 1000,
	}

	t.Run("two answers at one log size", func(t *testing.T) {
		other := base
		other.Seq, other.MerkleRoot = 90, "sha256:bb"
		bad, why := Contradicts(base, other)
		if !bad {
			t.Fatal("two different answers at the same log size were not flagged")
		}
		if why == "" {
			t.Error("a contradiction was reported with no explanation")
		}
	})

	t.Run("went backwards", func(t *testing.T) {
		later := base
		later.LogSize, later.TS, later.Seq = 60, 2000, 40
		if bad, _ := Contradicts(base, later); !bad {
			t.Fatal("a witness reporting a smaller position later was not flagged")
		}
	})

	t.Run("honest growth is not a contradiction", func(t *testing.T) {
		later := base
		later.LogSize, later.TS, later.Seq, later.MerkleRoot = 60, 2000, 140, "sha256:cc"
		if bad, why := Contradicts(base, later); bad {
			t.Fatalf("ordinary growth was flagged as a contradiction: %s", why)
		}
	})

	t.Run("different witnesses or nodes never conflict", func(t *testing.T) {
		other := base
		other.WitnessKey, other.Seq = "k2", 1
		if bad, _ := Contradicts(base, other); bad {
			t.Error("statements from two different witnesses were compared")
		}
		other = base
		other.Node, other.Seq = "node-b", 1
		if bad, _ := Contradicts(base, other); bad {
			t.Error("statements about two different nodes were compared")
		}
	})
}

// The log survives a restart, and rebuilds to the same root — otherwise a
// witness would publish a different history every time it was restarted, and
// every monitor following it would report a fork.
func TestWitnessLogSurvivesRestart(t *testing.T) {
	node, wit := witnessPair(t)
	sealN(t, node.ldg, 3)
	stmt, _ := node.ldg.Statement(0)
	if _, err := wit.Countersign(stmt); err != nil {
		t.Fatalf("countersign: %v", err)
	}
	before := wit.LogHead()

	reopened, err := NewWitness(wit.st, wit.keys)
	if err != nil {
		t.Fatalf("reopen witness: %v", err)
	}
	after := reopened.LogHead()
	if after.Size != before.Size || after.Root != before.Root {
		t.Fatalf("the witness published a different history after restart: %d/%s → %d/%s",
			before.Size, before.Root, after.Size, after.Root)
	}
}

func lastSeenOf(t *testing.T, w *Witness, node string) uint64 {
	t.Helper()
	seq, err := w.LastSeen(node)
	if err != nil {
		t.Fatalf("last seen: %v", err)
	}
	return seq
}
