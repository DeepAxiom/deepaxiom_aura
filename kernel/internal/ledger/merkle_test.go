package ledger

import (
	"crypto/sha256"
	"fmt"
	"testing"
)

// leavesN builds n distinct, deterministic leaves.
func leavesN(n int) []Hash {
	out := make([]Hash, n)
	for i := range out {
		out[i] = LeafHash([]byte(fmt.Sprintf("entry-%d", i)))
	}
	return out
}

// TestMerkleRootHandComputed pins the tree shape against values computed by
// hand from RFC 6962's definition, so a refactor that changes the shape (and
// would therefore invalidate every proof ever issued) fails loudly rather
// than silently agreeing with itself.
func TestMerkleRootHandComputed(t *testing.T) {
	empty := sha256.Sum256(nil)
	if got := EmptyRoot(); got != Hash(empty) {
		t.Fatalf("MTH({}) = %s, want SHA256() = %x", got, empty)
	}

	l := leavesN(3)

	// MTH({d0}) == leaf hash itself, no interior node.
	if got := MerkleRoot(l[:1]); got != l[0] {
		t.Fatalf("MTH({d0}) = %s, want the bare leaf %s", got, l[0])
	}

	// MTH({d0,d1}) = H(0x01 || d0 || d1)
	want2 := interiorHash(l[0], l[1])
	if got := MerkleRoot(l[:2]); got != want2 {
		t.Fatalf("MTH(2) = %s, want %s", got, want2)
	}

	// MTH({d0,d1,d2}): k = 2, so left = MTH({d0,d1}), right = MTH({d2}) = d2.
	want3 := interiorHash(want2, l[2])
	if got := MerkleRoot(l[:3]); got != want3 {
		t.Fatalf("MTH(3) = %s, want %s", got, want3)
	}
}

// TestLeafInteriorDomainSeparation is the second-preimage property: an
// interior node's hash must never be producible as a leaf hash, or a prover
// could pass a subtree off as a single entry.
func TestLeafInteriorDomainSeparation(t *testing.T) {
	l := leavesN(2)
	interior := interiorHash(l[0], l[1])
	// Feed the exact preimage an interior node hashes, but through LeafHash.
	asLeaf := LeafHash(append(l[0][:], l[1][:]...))
	if interior == asLeaf {
		t.Fatal("interior hash collides with a leaf hash over the same bytes — " +
			"domain separation prefixes are not being applied")
	}
}

// TestCompactTreeMatchesReference is the load-bearing agreement: the O(log n)
// incremental tree the ledger uses while sealing must produce exactly the
// root the reference recomputation produces from storage during verification.
// If these two ever disagree, every signed head is a signature over a number
// no verifier will reproduce.
func TestCompactTreeMatchesReference(t *testing.T) {
	var ct CompactTree
	all := leavesN(300)
	for n := 0; n <= 300; n++ {
		if got, want := ct.Root(), MerkleRoot(all[:n]); got != want {
			t.Fatalf("size %d: CompactTree.Root() = %s, MerkleRoot() = %s", n, got, want)
		}
		if got := ct.Size(); got != uint64(n) {
			t.Fatalf("size %d: CompactTree.Size() = %d", n, got)
		}
		if n < len(all) {
			ct.Append(all[n])
		}
	}
}

// TestInclusionProofRoundTrip verifies every leaf of every tree size in the
// range — the proof for leaf i of a tree of n must recompute the root, for
// all i < n.
func TestInclusionProofRoundTrip(t *testing.T) {
	for n := 1; n <= 64; n++ {
		l := leavesN(n)
		root := MerkleRoot(l)
		for i := 0; i < n; i++ {
			proof, err := InclusionProof(l, i)
			if err != nil {
				t.Fatalf("n=%d i=%d: %v", n, i, err)
			}
			if !VerifyInclusion(l[i], i, n, proof, root) {
				t.Fatalf("n=%d i=%d: valid proof rejected", n, i)
			}
		}
	}
}

func TestInclusionProofRejectsTampering(t *testing.T) {
	const n = 17
	l := leavesN(n)
	root := MerkleRoot(l)
	proof, err := InclusionProof(l, 5)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("wrong leaf", func(t *testing.T) {
		if VerifyInclusion(LeafHash([]byte("forged")), 5, n, proof, root) {
			t.Fatal("a proof for entry 5 verified against a different leaf")
		}
	})
	t.Run("wrong index", func(t *testing.T) {
		if VerifyInclusion(l[5], 6, n, proof, root) {
			t.Fatal("a proof for entry 5 verified at position 6")
		}
	})
	t.Run("wrong root", func(t *testing.T) {
		if VerifyInclusion(l[5], 5, n, proof, LeafHash([]byte("other head"))) {
			t.Fatal("proof verified against an unrelated root")
		}
	})
	t.Run("mutated path", func(t *testing.T) {
		bad := append([]Hash(nil), proof...)
		bad[0] = LeafHash([]byte("swapped sibling"))
		if VerifyInclusion(l[5], 5, n, bad, root) {
			t.Fatal("proof with a substituted sibling verified")
		}
	})
	t.Run("extra path element", func(t *testing.T) {
		bad := append(append([]Hash(nil), proof...), LeafHash([]byte("padding")))
		if VerifyInclusion(l[5], 5, n, bad, root) {
			t.Fatal("over-long proof verified — verification is a prefix match, not a bijection")
		}
	})
	t.Run("truncated path", func(t *testing.T) {
		if VerifyInclusion(l[5], 5, n, proof[:len(proof)-1], root) {
			t.Fatal("truncated proof verified")
		}
	})
	t.Run("out of range", func(t *testing.T) {
		if _, err := InclusionProof(l, n); err == nil {
			t.Fatal("expected an error for an index past the last leaf")
		}
	})
}

// TestConsistencyProofRoundTrip checks every (m, n) pair in the range: the
// tree of size m must be provably a prefix of the tree of size n.
func TestConsistencyProofRoundTrip(t *testing.T) {
	for n := 1; n <= 48; n++ {
		l := leavesN(n)
		newRoot := MerkleRoot(l)
		for m := 0; m <= n; m++ {
			proof, err := ConsistencyProof(l, m, n)
			if err != nil {
				t.Fatalf("m=%d n=%d: %v", m, n, err)
			}
			oldRoot := MerkleRoot(l[:m])
			if !VerifyConsistency(m, n, oldRoot, newRoot, proof) {
				t.Fatalf("m=%d n=%d: valid consistency proof rejected", m, n)
			}
		}
	}
}

// TestConsistencyProofCatchesAForkedHistory is the property a witness relies
// on: if a node rewrites an already-published entry and keeps growing, no
// consistency proof can connect the head the witness saw to the new head.
func TestConsistencyProofCatchesAForkedHistory(t *testing.T) {
	const m, n = 5, 12
	honest := leavesN(n)
	witnessed := MerkleRoot(honest[:m]) // what a witness counter-signed earlier

	forked := append([]Hash(nil), honest...)
	forked[2] = LeafHash([]byte("entry-2 rewritten after the fact"))
	forkedHead := MerkleRoot(forked)

	// The node offers the best proof it can build from its rewritten history.
	proof, err := ConsistencyProof(forked, m, n)
	if err != nil {
		t.Fatal(err)
	}
	if VerifyConsistency(m, n, witnessed, forkedHead, proof) {
		t.Fatal("a rewritten history proved consistent with the head a witness had already signed")
	}
}

func TestConsistencyProofRejectsBadInput(t *testing.T) {
	l := leavesN(10)
	if _, err := ConsistencyProof(l, 12, 10); err == nil {
		t.Fatal("expected an error when the old size exceeds the new size")
	}
	if _, err := ConsistencyProof(l, 2, 99); err == nil {
		t.Fatal("expected an error when n exceeds the leaves available")
	}
	if VerifyConsistency(3, 2, MerkleRoot(l[:3]), MerkleRoot(l[:2]), nil) {
		t.Fatal("a shrinking tree verified as consistent")
	}
	if VerifyConsistency(2, 8, MerkleRoot(l[:2]), MerkleRoot(l[:8]), nil) {
		t.Fatal("an empty proof verified for a real extension")
	}
}

func TestParseHashRoundTrip(t *testing.T) {
	h := LeafHash([]byte("round trip"))
	back, err := ParseHash(h.String())
	if err != nil {
		t.Fatal(err)
	}
	if back != h {
		t.Fatalf("ParseHash(%q) = %s, want %s", h.String(), back, h)
	}
	for _, bad := range []string{"", "deadbeef", "sha256:zz", "sha256:00"} {
		if _, err := ParseHash(bad); err == nil {
			t.Fatalf("ParseHash(%q) should have failed", bad)
		}
	}
}
