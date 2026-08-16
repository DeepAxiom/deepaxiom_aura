package ledger

// Merkle tree head over the effect ledger — RFC 6962 (Certificate
// Transparency), not a shape invented here.
//
// Why a tree at all, when C4 already chains every entry to the one before it?
// Because the chain answers "has this ledger been altered" only for someone
// holding the whole ledger. An auditor handed one receipt and asked "was this
// effect really sealed by that node, at that position, in that history?" had
// to be handed the entire SQLite file to find out — which is both a privacy
// problem (every other effect comes along for the ride) and an availability
// problem (the file may be gigabytes, or gone).
//
// A Merkle tree turns that into O(log n) evidence:
//
//   - **inclusion proof** — "entry 42 is in the tree whose head you already
//     have a signature over", proven with ~log2(n) hashes and nothing else.
//     That is what makes a receipt portable (see Receipt in receipt.go).
//   - **consistency proof** — "the tree of size m you saw last week is a
//     strict prefix of the tree of size n I am showing you now; nothing was
//     retroactively inserted, reordered or dropped between them." This is
//     what a witness needs (see witness.go): a witness that only ever saw
//     signed heads can catch a node that forks its own history, which no
//     amount of self-signing can.
//
// RFC 6962 specifically, rather than a hand-rolled tree, for three reasons:
// it is the most analysed append-only log construction there is; it defines
// both proof types against one tree shape; and its leaf/interior domain
// separation (0x00 / 0x01 prefixes) closes the second-preimage attack a naive
// tree has, where an interior node's hash can be passed off as a leaf.
//
// The chain stays exactly as C4 v1 specified it. This is additive: `prev` is
// untouched, old entries verify unchanged, and a checkpoint that predates
// this file has no `merkle_root` and is still checked the way it always was.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// Domain separation prefixes (RFC 6962 §2.1). Without these, SHA256(interior)
// and SHA256(leaf) live in the same space and a prover can present an interior
// node as though it were a leaf.
const (
	leafPrefix     byte = 0x00
	interiorPrefix byte = 0x01
)

// Hash is one node of the tree. A fixed-size array rather than a slice so it
// cannot be nil, cannot be resliced, and compares with ==.
type Hash [sha256.Size]byte

// String renders a hash the way every other content hash in this codebase is
// rendered, so a Merkle root and an entry hash are visibly the same kind of
// thing in logs and JSON.
func (h Hash) String() string { return "sha256:" + hex.EncodeToString(h[:]) }

// ParseHash reads back what String wrote.
func ParseHash(s string) (Hash, error) {
	var h Hash
	const prefix = "sha256:"
	if len(s) < len(prefix) || s[:len(prefix)] != prefix {
		return h, fmt.Errorf("hash %q: want a %q prefix", s, prefix)
	}
	raw, err := hex.DecodeString(s[len(prefix):])
	if err != nil {
		return h, fmt.Errorf("hash %q: %w", s, err)
	}
	if len(raw) != sha256.Size {
		return h, fmt.Errorf("hash %q: got %d bytes, want %d", s, len(raw), sha256.Size)
	}
	copy(h[:], raw)
	return h, nil
}

// LeafHash is MTH({d}) for a single entry: SHA256(0x00 || entry_json).
//
// The input is the entry's canonical JSON — byte for byte what
// store.AppendLedgerEntry persisted — so a verifier recomputes the leaf from
// storage without needing to re-marshal, and therefore without any risk that
// its JSON encoder differs from the sealer's.
func LeafHash(entryJSON []byte) Hash {
	h := sha256.New()
	h.Write([]byte{leafPrefix})
	h.Write(entryJSON)
	var out Hash
	copy(out[:], h.Sum(nil))
	return out
}

// interiorHash is MTH(left || right): SHA256(0x01 || left || right).
func interiorHash(left, right Hash) Hash {
	h := sha256.New()
	h.Write([]byte{interiorPrefix})
	h.Write(left[:])
	h.Write(right[:])
	var out Hash
	copy(out[:], h.Sum(nil))
	return out
}

// EmptyRoot is MTH({}) = SHA256() — the head of a ledger that has sealed
// nothing. Defined so "no effects yet" has a real, signable head rather than
// a special case every caller has to remember.
func EmptyRoot() Hash {
	var out Hash
	copy(out[:], sha256.New().Sum(nil))
	return out
}

// largestPowerOfTwoBelow returns the largest k = 2^i with k < n (n > 1).
// RFC 6962 splits every subtree at this point, which is what makes the tree
// shape a function of size alone — and therefore what makes two verifiers
// agree without exchanging anything but hashes.
func largestPowerOfTwoBelow(n int) int {
	k := 1
	for k<<1 < n {
		k <<= 1
	}
	return k
}

// MerkleRoot computes MTH(leaves) — the tree head over these leaves in order.
func MerkleRoot(leaves []Hash) Hash {
	switch len(leaves) {
	case 0:
		return EmptyRoot()
	case 1:
		return leaves[0]
	}
	k := largestPowerOfTwoBelow(len(leaves))
	return interiorHash(MerkleRoot(leaves[:k]), MerkleRoot(leaves[k:]))
}

// InclusionProof returns the audit path for leaves[index] in a tree of
// len(leaves) leaves: the sibling hashes, bottom-up, needed to recompute the
// root. RFC 6962 PATH(m, D[n]).
func InclusionProof(leaves []Hash, index int) ([]Hash, error) {
	n := len(leaves)
	if index < 0 || index >= n {
		return nil, fmt.Errorf("inclusion proof: index %d out of range for a tree of %d leaves", index, n)
	}
	return inclusionPath(leaves, index), nil
}

func inclusionPath(leaves []Hash, index int) []Hash {
	n := len(leaves)
	if n == 1 {
		return nil
	}
	k := largestPowerOfTwoBelow(n)
	if index < k {
		return append(inclusionPath(leaves[:k], index), MerkleRoot(leaves[k:]))
	}
	return append(inclusionPath(leaves[k:], index-k), MerkleRoot(leaves[:k]))
}

// VerifyInclusion recomputes a root from a leaf and its audit path and reports
// whether it matches the root the caller already trusts (because it was
// signed, or counter-signed by a witness).
//
// This is the whole point of the tree: it needs the leaf, the path, and the
// tree size — never the other entries, and never the node that produced them.
//
// The walk is bottom-up, driven by the bits of the leaf index, because that
// is the order InclusionProof emits siblings in (deepest first). `sn` tracks
// the last index of the current level, which is what distinguishes a genuine
// right sibling from the ragged edge of a tree whose size is not a power of
// two — RFC 6962 §2.1.1.
func VerifyInclusion(leaf Hash, index, treeSize int, proof []Hash, root Hash) bool {
	if index < 0 || treeSize <= 0 || index >= treeSize {
		return false
	}
	computed := leaf
	fn, sn := index, treeSize-1
	for _, sibling := range proof {
		if sn == 0 {
			// Already at the root with proof left over: this is not the path
			// for this leaf. Rejecting keeps verification a bijection rather
			// than a prefix match, so a padded proof cannot be smuggled past.
			return false
		}
		if fn&1 == 1 || fn == sn {
			computed = interiorHash(sibling, computed)
			for fn != 0 && fn&1 == 0 {
				fn >>= 1
				sn >>= 1
			}
		} else {
			computed = interiorHash(computed, sibling)
		}
		fn >>= 1
		sn >>= 1
	}
	// sn == 0 means the walk actually reached the root rather than stopping
	// short on a truncated path.
	return sn == 0 && computed == root
}

// ConsistencyProof proves the tree of size m is a prefix of the tree of size
// n — RFC 6962 PROOF(m, D[n]). m == 0 needs no proof (every tree extends the
// empty one); m == n needs none either.
func ConsistencyProof(leaves []Hash, m, n int) ([]Hash, error) {
	switch {
	case m < 0 || n < 0:
		return nil, fmt.Errorf("consistency proof: sizes must not be negative (m=%d n=%d)", m, n)
	case m > n:
		return nil, fmt.Errorf("consistency proof: old size %d exceeds new size %d", m, n)
	case n > len(leaves):
		return nil, fmt.Errorf("consistency proof: asked about %d leaves, only %d available", n, len(leaves))
	case m == 0 || m == n:
		return nil, nil
	}
	return subProof(leaves[:n], m, true), nil
}

// subProof is RFC 6962 SUBPROOF(m, D[n], b). `complete` (the RFC's b) records
// whether the old tree of size m is exactly the subtree being described — when
// it is not, its root has to be named explicitly, because the verifier cannot
// derive it.
func subProof(leaves []Hash, m int, complete bool) []Hash {
	n := len(leaves)
	if m == n {
		if complete {
			return nil
		}
		return []Hash{MerkleRoot(leaves)}
	}
	k := largestPowerOfTwoBelow(n)
	if m <= k {
		return append(subProof(leaves[:k], m, complete), MerkleRoot(leaves[k:]))
	}
	return append(subProof(leaves[k:], m-k, false), MerkleRoot(leaves[:k]))
}

// VerifyConsistency checks that oldRoot (a tree of m leaves) really is a
// prefix of newRoot (a tree of n leaves), using only the proof.
//
// A node that quietly rewrote history would have to produce two signed heads
// with no consistency proof between them. That is the fork a witness detects
// and a lone self-signing node cannot — see witness.go.
func VerifyConsistency(m, n int, oldRoot, newRoot Hash, proof []Hash) bool {
	switch {
	case m < 0 || n < 0 || m > n:
		return false
	case m == 0:
		return len(proof) == 0 // everything extends the empty tree
	case m == n:
		return len(proof) == 0 && oldRoot == newRoot
	case len(proof) == 0:
		return false
	}

	// RFC 6962 §2.1.2. Two roots are recomputed in lockstep from the same
	// proof: `oldHash` may only absorb siblings that both trees share, while
	// `newHash` also absorbs the nodes that exist solely because the tree
	// grew. A proof that satisfies both at once is exactly the statement
	// "the first m leaves are unchanged and still in these positions".
	fn, sn := m-1, n-1
	for fn&1 == 1 {
		fn >>= 1
		sn >>= 1
	}

	// fn == 0 here iff m is an exact power of two — in that case the old root
	// is itself a perfect subtree the verifier already holds, so the prover
	// omits it and we seed with oldRoot. Otherwise the proof's first element
	// names the subtree root the verifier cannot derive on its own.
	oldHash, newHash, rest := oldRoot, oldRoot, proof
	if fn != 0 {
		oldHash, newHash, rest = proof[0], proof[0], proof[1:]
	}

	for _, sibling := range rest {
		if sn == 0 {
			return false // proof longer than the tree is deep: malformed
		}
		if fn&1 == 1 || fn == sn {
			// A node both trees share: it constrains the old root too.
			oldHash = interiorHash(sibling, oldHash)
			newHash = interiorHash(sibling, newHash)
			for fn != 0 && fn&1 == 0 {
				fn >>= 1
				sn >>= 1
			}
		} else {
			// Growth the old tree never saw: only the new root absorbs it.
			newHash = interiorHash(newHash, sibling)
		}
		fn >>= 1
		sn >>= 1
	}
	return sn == 0 && oldHash == oldRoot && newHash == newRoot
}

// CompactTree is an append-only Merkle tree kept in O(log n) memory: one
// perfect-subtree root per set bit of the size, exactly the way binary
// addition carries.
//
// The ledger uses this on the sealing path so producing a head costs
// O(log n) hashes per effect instead of rehashing the whole history. Proofs
// still read the full leaf list from storage — they are asked for rarely and
// off the hot path, and reading is what keeps them honest anyway, since a
// proof computed from live memory would be proving the node's belief rather
// than what is on disk.
type CompactTree struct {
	size  uint64
	nodes []Hash // nodes[i] is a perfect subtree of 2^i leaves, live iff bit i of size is set
}

// Size is how many leaves have been appended.
func (t *CompactTree) Size() uint64 { return t.size }

// Append adds one leaf, carrying completed subtrees upward.
func (t *CompactTree) Append(leaf Hash) {
	carry, level := leaf, 0
	for (t.size>>level)&1 == 1 {
		carry = interiorHash(t.nodes[level], carry)
		level++
	}
	for len(t.nodes) <= level {
		t.nodes = append(t.nodes, Hash{})
	}
	t.nodes[level] = carry
	t.size++
}

// Root is the current tree head.
//
// The live subtrees are folded lowest level first: a higher level covers
// earlier leaves, so it is always the left child of whatever has been
// accumulated from the levels below it.
func (t *CompactTree) Root() Hash {
	if t.size == 0 {
		return EmptyRoot()
	}
	var root Hash
	have := false
	for level := 0; level < len(t.nodes); level++ {
		if (t.size>>level)&1 == 0 {
			continue
		}
		if !have {
			root, have = t.nodes[level], true
			continue
		}
		root = interiorHash(t.nodes[level], root)
	}
	return root
}
