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
}

// Sound reports whether the ledger passed every check this report ran.
// Deliberately conjunctive: a chain that recomputes cleanly but whose
// checkpoints do not all verify is not sound, because an attacker capable of
// rewriting a whole suffix to stay self-consistent is exactly the threat
// checkpoints exist to catch — see the walkthrough in ledger_test.go.
func (r Report) Sound() bool {
	return r.ChainIntact && (!r.KeyAvailable || r.CheckpointsValid == r.Checkpoints)
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

	byHash := make(map[string]Entry, len(entries))
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
	}

	if !report.KeyAvailable {
		checkpoints, err := st.LedgerCheckpoints()
		if err != nil {
			return report, fmt.Errorf("read checkpoints: %w", err)
		}
		report.Checkpoints = len(checkpoints)
		return report, nil
	}

	checkpoints, err := st.LedgerCheckpoints()
	if err != nil {
		return report, fmt.Errorf("read checkpoints: %w", err)
	}
	report.Checkpoints = len(checkpoints)
	for _, cp := range checkpoints {
		entry, ok := byHash[cp.HeadHash]
		if !ok || entry.Seq != cp.Seq {
			// The checkpoint points at a hash the chain no longer produces at
			// that position — either the entry was altered after signing, or
			// the checkpoint itself was forged. Either way it does not verify.
			continue
		}
		if err := signing.Verify(pubkeyB64, cp.Signature, checkpointPayload(cp.Seq, cp.HeadHash)); err != nil {
			continue
		}
		report.CheckpointsValid++
	}
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
