package ledger

// External anchoring: the part of C4 that self-signing cannot do.
//
// A node signs its own ledger head with its own key. That catches an attacker
// who edits the database without holding the key. It does not catch the node's
// own operator, who holds the key and can therefore rewrite history and
// re-sign the result — the chain recomputes, every signature verifies, and
// nothing in the file says the past was different an hour ago. The README used
// to describe the ledger as "evidence an auditor can check without trusting
// the process that produced it", which overstated it: the auditor still had to
// trust whoever held the key.
//
// A witness closes that gap without any new trust assumption about the node.
// The idea is Certificate Transparency's, applied unchanged:
//
//  1. A node shows a witness its signed head — size n, Merkle root R_n — plus
//     an RFC 6962 **consistency proof** from the last size m that witness saw.
//  2. The witness verifies the node's signature, then verifies the proof:
//     "the m entries I already vouched for are still, unchanged and in the
//     same order, a prefix of these n." A node that rewrote entry 3 cannot
//     produce this proof. There is no proof to forge — it either exists,
//     because history really is an extension, or it does not.
//  3. Only then does the witness counter-sign, and it keeps its own record of
//     what it signed.
//
// The property that results: a node can still lie, but it cannot lie
// *consistently to two parties over time*. To pass off a rewritten history it
// would need every witness to forget, simultaneously, what it had already
// signed. That is a far stronger guarantee than a lone key, and it costs one
// HTTP round trip.
//
// What this deliberately does NOT claim: witness signatures stored here are
// stored by the node, so a node can drop the ones it dislikes. `aura verify`
// reporting "0 witnesses" is therefore not proof that none were ever issued —
// the authoritative copy is the witness's own record, and detecting a fork
// means asking the witness, not the node. Verify says so rather than implying
// its own count is complete.

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"aura/kernel/internal/signing"
	"aura/kernel/internal/store"
)

// witnessPayload is the domain-separated byte string a witness signs. It
// names the node being witnessed, so a signature obtained for node A can
// never be replayed as though it vouched for node B — the attack a payload of
// just seq+root would allow between two nodes that happen to share a size.
//
// The domain prefix differs from checkpointPayload's for the same reason that
// one differs from the package-signing prefix: one key, several roles, and no
// signature usable outside the role it was made for.
func witnessPayload(nodeID string, seq uint64, merkleRoot string) []byte {
	return []byte(fmt.Sprintf("aura-ledger-witness-v1:%s:%d:%s", nodeID, seq, merkleRoot))
}

// Statement is what a node presents to a witness: its current head, its own
// signature over that head, and a consistency proof back to whatever size
// this witness last vouched for.
type Statement struct {
	Node       string `json:"node"`
	Seq        uint64 `json:"seq"`
	HeadHash   string `json:"head_hash"`
	MerkleRoot string `json:"merkle_root"`
	Pubkey     string `json:"pubkey"`    // the node's Ed25519 public key, base64
	Signature  string `json:"signature"` // the node's own checkpoint signature over this head
	// FromSeq / Consistency prove this head extends what the witness already
	// signed. Empty when the witness has never seen this node: there is
	// nothing to be consistent with, and the witness records the head as its
	// baseline instead of verifying it.
	FromSeq     uint64   `json:"from_seq,omitempty"`
	Consistency []string `json:"consistency,omitempty"`
}

// Countersignature is what a witness returns.
type Countersignature struct {
	WitnessKey string `json:"witness_key"` // the witness's Ed25519 public key, base64
	Signature  string `json:"signature"`
	TS         int64  `json:"ts"`
}

// Statement builds what this node should present to a witness that last saw
// it at size fromSeq (0 for a witness that has never seen it).
//
// The consistency proof is computed from stored entries rather than from the
// in-memory tree, deliberately: a proof generated from what the process
// believes would prove the process's belief, and what a witness is being
// asked to vouch for is what is on disk.
func (l *Ledger) Statement(fromSeq uint64) (Statement, error) {
	head := l.Head()
	if head.Seq == 0 {
		return Statement{}, fmt.Errorf("nothing to witness: this ledger has sealed no effects yet")
	}
	if fromSeq > head.Seq {
		return Statement{}, fmt.Errorf(
			"witness claims to have seen %d entries but this ledger has %d — "+
				"either it is confusing this node with another, or entries were lost here",
			fromSeq, head.Seq)
	}

	st := Statement{
		Node: head.Node, Seq: head.Seq, HeadHash: head.HeadHash,
		MerkleRoot: head.MerkleRoot, Pubkey: head.Pubkey,
	}

	// Reuse the checkpoint signature over this exact head when one exists, so
	// the witness is verifying the same artifact `aura verify` checks rather
	// than a second, differently-scoped signature.
	cp, ok, err := l.st.LastCheckpoint()
	if err != nil {
		return Statement{}, fmt.Errorf("read last checkpoint: %w", err)
	}
	if !ok || cp.Seq != head.Seq {
		// No checkpoint at the current head — force one so the witness always
		// receives a head this node has committed to under its own key.
		if err := l.Checkpoint(); err != nil {
			return Statement{}, fmt.Errorf("checkpoint before witnessing: %w", err)
		}
		if cp, ok, err = l.st.LastCheckpoint(); err != nil || !ok {
			return Statement{}, fmt.Errorf("read checkpoint just written: %w", err)
		}
	}
	st.Signature = cp.Signature

	if fromSeq > 0 && fromSeq < head.Seq {
		proof, err := l.consistencyProof(fromSeq, head.Seq)
		if err != nil {
			return Statement{}, err
		}
		st.FromSeq = fromSeq
		st.Consistency = proof
	}
	return st, nil
}

// consistencyProof builds an RFC 6962 consistency proof from storage.
func (l *Ledger) consistencyProof(m, n uint64) ([]string, error) {
	entries, err := l.st.LedgerEntries(1, 0)
	if err != nil {
		return nil, fmt.Errorf("read entries for consistency proof: %w", err)
	}
	if uint64(len(entries)) < n {
		return nil, fmt.Errorf("consistency proof: need %d entries, storage has %d", n, len(entries))
	}
	leaves := make([]Hash, len(entries))
	for i, raw := range entries {
		leaves[i] = LeafHash(raw)
	}
	proof, err := ConsistencyProof(leaves, int(m), int(n))
	if err != nil {
		return nil, err
	}
	out := make([]string, len(proof))
	for i, h := range proof {
		out[i] = h.String()
	}
	return out, nil
}

// WitnessLimits bound what an *open* witness will accept.
//
// Witnessing behind a bearer token needs no limits: the caller is already
// someone you gave a credential to. An open witness is a different animal —
// anyone can mint an Ed25519 keypair, invent a node id, and present a
// perfectly valid one-entry ledger. Without bounds, `witnessed_heads` becomes
// a free database that grows one row per keypair an attacker feels like
// generating.
//
// These are what makes an open witness offerable at all, which is why the
// feature waited for them rather than shipping as a one-line route change.
type WitnessLimits struct {
	// MaxNodes caps how many distinct nodes this witness will remember.
	// Beyond it, a node it has never seen is refused while nodes it already
	// vouched for keep working — the useful failure mode, since the value of
	// witnessing is in the continuity of what you already promised.
	MaxNodes int
	// PerNodePerHour caps how often one node may present a head. A node with
	// anything to prove presents on the order of once per checkpoint.
	PerNodePerHour int
	// MinEntriesGrowth is how many new entries a returning node must have
	// sealed before it is worth re-signing the same history. Zero allows
	// re-presenting an unchanged head, which is legitimate after a restart.
	MinEntriesGrowth uint64
	// RetentionDays drops nodes not seen in this long. A witness that
	// remembers forever is a witness whose storage is an attack surface;
	// forgetting is also honest, since a countersignature this witness can no
	// longer contradict is one it should stop implying it can.
	RetentionDays int
}

// DefaultWitnessLimits are deliberately generous for legitimate use and
// hostile to bulk registration: a real node checkpoints every ~100 effects or
// 60 seconds and would present far below 12/hour.
func DefaultWitnessLimits() WitnessLimits {
	return WitnessLimits{
		MaxNodes:         10_000,
		PerNodePerHour:   12,
		MinEntriesGrowth: 0,
		RetentionDays:    90,
	}
}

// Witness is the receiving half: this node acting as a witness for others.
// It holds the key it counter-signs with and the store where it remembers
// what it has already vouched for.
type Witness struct {
	st   *store.Store
	keys *signing.Keypair

	// limits apply only when Open is true. A token-guarded witness trusts its
	// callers by construction.
	limits WitnessLimits
	open   bool

	mu    sync.Mutex
	rate  map[string][]time.Time
	nodes int64
}

// NewWitness builds the witnessing service. The same node identity keypair
// signs both this node's own checkpoints and its countersignatures for
// others; the domain-separated payloads keep the two roles from overlapping.
func NewWitness(st *store.Store, keys *signing.Keypair) *Witness {
	return &Witness{st: st, keys: keys, limits: DefaultWitnessLimits(), rate: map[string][]time.Time{}}
}

// Open turns this into a witness anyone may present to, with the given limits.
//
// This is what makes the Certificate Transparency analogy actually hold. CT's
// strength comes from witnesses being *independent* of the log they watch, and
// a witness that only serves parties who already exchanged a bearer token is a
// witness inside the same trust domain as the thing it is vouching for.
func (w *Witness) Open(limits WitnessLimits) *Witness {
	w.open = true
	w.limits = limits
	return w
}

// admit applies the open-witness bounds. Returns nil when the statement may
// proceed to verification.
func (w *Witness) admit(s Statement, previouslySeen bool, prevSeq uint64) error {
	if !w.open {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	now := time.Now()
	window := now.Add(-time.Hour)
	kept := w.rate[s.Node][:0]
	for _, t := range w.rate[s.Node] {
		if t.After(window) {
			kept = append(kept, t)
		}
	}
	w.rate[s.Node] = kept

	if w.limits.PerNodePerHour > 0 && len(kept) >= w.limits.PerNodePerHour {
		return fmt.Errorf(
			"witness: rate limit — %s has presented %d heads in the last hour and this "+
				"witness accepts %d. A node checkpointing normally presents far fewer",
			s.Node, len(kept), w.limits.PerNodePerHour)
	}

	if !previouslySeen {
		if w.limits.MaxNodes > 0 {
			count, err := w.st.WitnessedHeadCount()
			if err == nil && count >= int64(w.limits.MaxNodes) {
				return fmt.Errorf(
					"witness: at capacity (%d nodes). Nodes this witness already vouched "+
						"for continue to be served; new ones are refused", w.limits.MaxNodes)
			}
		}
	} else if w.limits.MinEntriesGrowth > 0 && s.Seq > prevSeq &&
		s.Seq-prevSeq < w.limits.MinEntriesGrowth {
		return fmt.Errorf(
			"witness: %s grew by %d entries since the last countersignature and this "+
				"witness re-signs after %d", s.Node, s.Seq-prevSeq, w.limits.MinEntriesGrowth)
	}

	w.rate[s.Node] = append(kept, now)
	return nil
}

// Prune drops nodes this witness has not heard from within its retention
// window. Called periodically by the node that hosts an open witness.
//
// Forgetting is not only hygiene. A countersignature whose baseline this
// witness has discarded is one it can no longer contradict, and continuing to
// imply otherwise would overstate what the witness can still detect.
func (w *Witness) Prune() (dropped int64, err error) {
	if w.limits.RetentionDays <= 0 {
		return 0, nil
	}
	cutoff := time.Now().AddDate(0, 0, -w.limits.RetentionDays).UnixMilli()
	return w.st.PruneWitnessedHeads(cutoff)
}

// LastSeen reports the size this witness last vouched for, for the given
// node. A node asks this first so it knows which consistency proof to build.
func (w *Witness) LastSeen(nodeID string) (uint64, error) {
	head, ok, err := w.st.LastWitnessedHead(nodeID)
	if err != nil || !ok {
		return 0, err
	}
	return head.Seq, nil
}

// Countersign verifies a statement and, if it holds up, records it and signs
// it. Every rejection path here is a refusal to vouch, never a silent pass:
// a witness that counter-signs something it did not check is worse than no
// witness, because it manufactures confidence that was never earned.
func (w *Witness) Countersign(s Statement) (Countersignature, error) {
	switch {
	case s.Node == "":
		return Countersignature{}, fmt.Errorf("witness: statement names no node")
	case s.Pubkey == "":
		return Countersignature{}, fmt.Errorf("witness: statement carries no public key")
	case s.MerkleRoot == "":
		return Countersignature{}, fmt.Errorf(
			"witness: statement carries no merkle root — this node speaks an older " +
				"checkpoint format and cannot be witnessed")
	case s.Seq == 0:
		return Countersignature{}, fmt.Errorf("witness: refusing to vouch for an empty ledger")
	}

	// 1. The node really did sign this head. Without this a third party could
	//    present someone else's head and collect a countersignature for it.
	if err := signing.Verify(s.Pubkey, s.Signature,
		checkpointPayload(s.Seq, s.HeadHash, s.MerkleRoot)); err != nil {
		return Countersignature{}, fmt.Errorf(
			"witness: the node's own signature over this head does not verify: %w", err)
	}

	newRoot, err := ParseHash(s.MerkleRoot)
	if err != nil {
		return Countersignature{}, fmt.Errorf("witness: %w", err)
	}

	// 2. This head extends what we already vouched for. This is the check
	//    that makes witnessing worth anything at all.
	prev, seen, err := w.st.LastWitnessedHead(s.Node)
	if err != nil {
		return Countersignature{}, fmt.Errorf("witness: read what we last saw: %w", err)
	}

	// Bounds for an open witness, applied after the cheap shape checks and
	// before the expensive proof verification — a refused caller should not be
	// able to make this node do cryptography.
	if err := w.admit(s, seen, prev.Seq); err != nil {
		return Countersignature{}, err
	}

	if seen {
		if s.Seq < prev.Seq {
			return Countersignature{}, fmt.Errorf(
				"witness: refusing to vouch — we already signed %s at %d entries and it now "+
					"presents only %d; an append-only log does not shrink",
				s.Node, prev.Seq, s.Seq)
		}
		prevRoot, err := ParseHash(prev.MerkleRoot)
		if err != nil {
			return Countersignature{}, fmt.Errorf("witness: stored root for %s: %w", s.Node, err)
		}
		proof := make([]Hash, len(s.Consistency))
		for i, raw := range s.Consistency {
			if proof[i], err = ParseHash(raw); err != nil {
				return Countersignature{}, fmt.Errorf("witness: consistency proof element %d: %w", i, err)
			}
		}
		if s.Seq != prev.Seq && s.FromSeq != prev.Seq {
			return Countersignature{}, fmt.Errorf(
				"witness: refusing to vouch — proof is anchored at %d entries but we last "+
					"signed this node at %d", s.FromSeq, prev.Seq)
		}
		if !VerifyConsistency(int(prev.Seq), int(s.Seq), prevRoot, newRoot, proof) {
			return Countersignature{}, fmt.Errorf(
				"witness: REFUSING TO VOUCH — %s cannot prove its history at %d entries "+
					"still contains, unchanged, the %d entries we already signed. This is what "+
					"a rewritten ledger looks like from the outside",
				s.Node, s.Seq, prev.Seq)
		}
	}

	// 3. Remember before signing: a countersignature we hand out but do not
	//    record is one we cannot hold the node to next time.
	if err := w.st.SaveWitnessedHead(store.WitnessedHead{
		Node: s.Node, Seq: s.Seq, MerkleRoot: s.MerkleRoot,
		Pubkey: s.Pubkey, TS: time.Now().UnixMilli(),
	}); err != nil {
		return Countersignature{}, fmt.Errorf("witness: record what we are about to sign: %w", err)
	}

	return Countersignature{
		WitnessKey: w.keys.PublicB64(),
		Signature:  w.keys.Sign(witnessPayload(s.Node, s.Seq, s.MerkleRoot)),
		TS:         time.Now().UnixMilli(),
	}, nil
}

// RecordCountersignature stores a countersignature this node collected about
// its own ledger, after checking it actually verifies — storing an unverified
// one would let a broken or hostile witness plant a signature that makes
// `aura verify` report a witness the ledger does not really have.
func (l *Ledger) RecordCountersignature(seq uint64, merkleRoot, witnessURL string, cs Countersignature) error {
	if err := signing.Verify(cs.WitnessKey, cs.Signature,
		witnessPayload(l.nodeID, seq, merkleRoot)); err != nil {
		return fmt.Errorf("countersignature from %s does not verify: %w", witnessURL, err)
	}
	return l.st.SaveWitness(store.WitnessRow{
		Seq: seq, WitnessKey: cs.WitnessKey, MerkleRoot: merkleRoot,
		Signature: cs.Signature, WitnessURL: witnessURL, TS: cs.TS,
	})
}

// verifyWitnesses checks every stored countersignature against the tree this
// verification recomputed from storage. Called by Verify.
//
// A witness row is valid when its signature verifies AND the root it vouches
// for is the root the entries actually produce at that seq. The second half
// matters: a node that rewrote history keeps its old, genuinely-signed witness
// rows, and they stop matching — which is exactly the signal.
func verifyWitnesses(st *store.Store, checkpoints []store.CheckpointRow, rootAt map[uint64]Hash) (total, valid int) {
	rows, err := st.LedgerWitnesses()
	if err != nil {
		return 0, 0
	}
	// The node id a witness signed over is not stored per row (it is always
	// this node), so recover it from any checkpoint-bearing entry set. The
	// caller has already established these belong to this ledger.
	var nodeID string
	if len(checkpoints) > 0 {
		nodeID = witnessedNodeID(st)
	}
	for _, row := range rows {
		total++
		want, ok := rootAt[row.Seq]
		if !ok || want.String() != row.MerkleRoot {
			continue // vouches for a head this history no longer produces
		}
		if nodeID == "" {
			continue
		}
		if err := signing.Verify(row.WitnessKey, row.Signature,
			witnessPayload(nodeID, row.Seq, row.MerkleRoot)); err != nil {
			continue
		}
		valid++
	}
	return total, valid
}

// witnessedNodeID recovers this ledger's node id from its first entry. Every
// entry carries it, and verification has already confirmed the chain, so the
// first one is as authoritative as any.
func witnessedNodeID(st *store.Store) string {
	entries, err := st.LedgerEntries(1, 1)
	if err != nil || len(entries) == 0 {
		return ""
	}
	var e Entry
	if json.Unmarshal(entries[0], &e) != nil {
		return ""
	}
	return e.Node
}
