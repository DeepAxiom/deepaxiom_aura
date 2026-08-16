package ledger

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"aura/kernel/internal/signing"
	"aura/kernel/internal/store"
)

// Report is the result of walking a ledger's chain and checking every
// checkpoint signature. It is the whole payload of `aura verify` and of
// `GET /v1/ledger/verify` — the same function backs both, so a verification
// done with the node running and one done against a cold data directory can
// never quietly disagree.
type Report struct {
	TotalEntries int64  `json:"total_entries"`
	ChainIntact  bool   `json:"chain_intact"`
	BrokenAtSeq  uint64 `json:"broken_at_seq,omitempty"` // 0 when ChainIntact
	BrokenReason string `json:"broken_reason,omitempty"`

	Checkpoints      int  `json:"checkpoints"`
	CheckpointsValid int  `json:"checkpoints_valid"`
	KeyAvailable     bool `json:"key_available"` // false: chain was checked, signatures were not

	PubkeyFingerprint string `json:"pubkey_fingerprint,omitempty"`

	// MerkleRoot is the recomputed RFC 6962 tree head over every entry (C4
	// v1.2) — the value a receipt's inclusion proof is checked against, and
	// the value a witness counter-signs.
	MerkleRoot string `json:"merkle_root,omitempty"`
	// MerkleCheckpoints counts checkpoints that carry a root at all; older
	// v1 checkpoints do not, and are still valid.
	MerkleCheckpoints int `json:"merkle_checkpoints"`
	// MerkleMismatches counts checkpoints whose signature verifies but whose
	// committed root disagrees with the tree recomputed from storage. This
	// should be impossible without a bug: a valid signature over a wrong root
	// means the node signed a head it could not have computed from its own
	// entries.
	MerkleMismatches int `json:"merkle_mismatches"`

	// Witnesses counts third-party counter-signatures found, and
	// WitnessesValid how many verify. A ledger with no witnesses is not
	// unsound — it is unwitnessed, which is a weaker claim, and the CLI says
	// so rather than passing silently. See witness.go.
	Witnesses      int `json:"witnesses"`
	WitnessesValid int `json:"witnesses_valid"`

	// Approvals counts entries carrying an operator signature (C4 v1.3), and
	// ApprovalsInvalid how many of those fail to verify.
	//
	// Asymmetric in the verdict, like witnesses and for the same reason: a
	// ledger with no approvals at all is not unsound — nothing on this node was
	// gated, or it was answered by clients that do not sign, both ordinary
	// states. But an approval that no longer verifies is direct evidence that
	// an entry was edited after sealing, or that one was written claiming a
	// human said yes when no human did. That is the accusation this field
	// exists to be able to make.
	Approvals        int    `json:"approvals"`
	ApprovalsInvalid int    `json:"approvals_invalid"`
	ApprovalFailure  string `json:"approval_failure,omitempty"`
}

// Sound reports whether the ledger passed every check this report ran.
// Deliberately conjunctive: a chain that recomputes cleanly but whose
// checkpoints do not all verify is not sound, because an attacker capable of
// rewriting a whole suffix to stay self-consistent is exactly the threat
// checkpoints exist to catch — see the walkthrough in ledger_test.go.
//
// Witnesses enter the verdict, but asymmetrically, because the two states
// they can be in mean completely different things:
//
//   - **No witnesses at all** is not a failure. It means no third party has
//     attested — a weaker claim, not a corrupt ledger. Treating it as failure
//     would make every fresh node report itself unsound.
//   - **A witness row that no longer verifies IS a failure.** Somebody
//     recorded a third party's signature over this history, and the history
//     no longer produces the head that was signed. That is direct evidence of
//     a rewrite, and it is the *only* signal available in the node's own data
//     directory when the rewriter held the node's key: the chain recomputes,
//     the checkpoint verifies, and this is what disagrees.
//
// The second case is why this is not merely informational. An end-to-end run
// of the rewrite-repair-and-re-sign attack reported SOUND with `0/1 witness
// countersignature(s) verify` printed just above it — technically consistent
// with how this was first written, and exactly the wrong headline.
func (r Report) Sound() bool {
	return r.ChainIntact &&
		r.MerkleMismatches == 0 &&
		(!r.KeyAvailable || r.CheckpointsValid == r.Checkpoints) &&
		r.WitnessesValid == r.Witnesses &&
		r.ApprovalsInvalid == 0
}

// Verify recomputes the hash chain from scratch and checks every checkpoint
// signature against pubkeyB64 (pass "" to check only the chain — see
// KeyAvailable in the result). It reads directly from st and touches no
// in-memory Ledger state, so it works identically whether or not the node
// that wrote this ledger is currently running. That is the property that
// turns the log into evidence: nobody has to trust the process, only the math.
func Verify(st *store.Store, pubkeyB64 string) (Report, error) {
	entries, err := st.LedgerEntries(1, 0)
	if err != nil {
		return Report{}, fmt.Errorf("read ledger entries: %w", err)
	}
	report := Report{TotalEntries: int64(len(entries)), ChainIntact: true}
	if pubkeyB64 != "" {
		report.KeyAvailable = true
		report.PubkeyFingerprint = Fingerprint(pubkeyB64)
	}

	checkpoints, err := st.LedgerCheckpoints()
	if err != nil {
		return report, fmt.Errorf("read checkpoints: %w", err)
	}
	report.Checkpoints = len(checkpoints)

	// Which seqs a checkpoint committed to, so the tree head at exactly those
	// points can be captured during the single walk below rather than by
	// recomputing the tree once per checkpoint.
	wantRootAt := make(map[uint64]bool, len(checkpoints))
	for _, cp := range checkpoints {
		wantRootAt[cp.Seq] = true
	}

	byHash := make(map[string]Entry, len(entries))
	rootAt := make(map[uint64]Hash, len(checkpoints))
	var tree CompactTree
	prevHash := ""
	for i, raw := range entries {
		var e Entry
		if err := json.Unmarshal(raw, &e); err != nil {
			return brokenReport(report, uint64(i+1),
				fmt.Sprintf("entry at position %d is not valid JSON: %v", i+1, err)), nil
		}
		wantSeq := uint64(i + 1)
		if e.Seq != wantSeq {
			return brokenReport(report, wantSeq,
				fmt.Sprintf("expected seq %d at this position, found %d — an entry was deleted, "+
					"reordered, or duplicated", wantSeq, e.Seq)), nil
		}
		if e.Prev != prevHash {
			return brokenReport(report, e.Seq,
				fmt.Sprintf("entry %d's prev (%s) does not match the recomputed hash of entry %d (%s) — "+
					"its content was altered after it was sealed",
					e.Seq, shortHash(e.Prev), e.Seq-1, shortHash(prevHash))), nil
		}
		hash := e.Hash()
		byHash[hash] = e
		prevHash = hash

		// C4 v1.3: an approval signature is checked here, in the same offline
		// walk as the chain, because it is evidence of the same kind — a claim
		// about the past that has to survive without the node, without the
		// operator roster as it stands today, and without anyone's word for it.
		if e.Approver != nil {
			report.Approvals++
			if err := e.Approver.Verify(e.Node, e.Session); err != nil {
				report.ApprovalsInvalid++
				if report.ApprovalFailure == "" {
					report.ApprovalFailure = fmt.Sprintf("entry %d: %v", e.Seq, err)
				}
			}
		}

		// The leaf is hashed over the stored bytes, matching how Seal built
		// it — see LeafHash.
		tree.Append(LeafHash(raw))
		if wantRootAt[e.Seq] {
			rootAt[e.Seq] = tree.Root()
		}
	}
	report.MerkleRoot = tree.Root().String()

	if !report.KeyAvailable {
		return report, nil
	}

	for _, cp := range checkpoints {
		entry, ok := byHash[cp.HeadHash]
		if !ok || entry.Seq != cp.Seq {
			// The checkpoint points at a hash the chain no longer produces at
			// that position — either the entry was altered after signing, or
			// the checkpoint itself was forged. Either way it does not verify.
			continue
		}
		if err := signing.Verify(pubkeyB64, cp.Signature,
			checkpointPayload(cp.Seq, cp.HeadHash, cp.MerkleRoot)); err != nil {
			continue
		}
		report.CheckpointsValid++

		if cp.MerkleRoot == "" {
			continue // a v1 checkpoint, sealed before the tree existed
		}
		report.MerkleCheckpoints++
		if got, ok := rootAt[cp.Seq]; !ok || got.String() != cp.MerkleRoot {
			// A signature that verifies over a root the entries do not
			// produce means the signed head and the stored history disagree.
			// The chain check above can miss this: it only relates each entry
			// to its neighbour, while the tree commits to all of them at once.
			report.MerkleMismatches++
		}
	}

	report.Witnesses, report.WitnessesValid = verifyWitnesses(st, checkpoints, rootAt)
	return report, nil
}

func brokenReport(r Report, atSeq uint64, reason string) Report {
	r.ChainIntact = false
	r.BrokenAtSeq = atSeq
	r.BrokenReason = reason
	return r
}

func shortHash(h string) string {
	if h == "" {
		return "genesis"
	}
	if len(h) > 15 {
		return h[:15] + "…"
	}
	return h
}

// Fingerprint reduces a base64 Ed25519 public key to a short, displayable
// identity — the SSH-key-fingerprint idea, so a banner or `aura verify` does
// not have to print 44 characters of base64 every time it names a node.
func Fingerprint(pubkeyB64 string) string {
	raw, err := base64.StdEncoding.DecodeString(pubkeyB64)
	if err != nil {
		return "invalid"
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:6])
}
