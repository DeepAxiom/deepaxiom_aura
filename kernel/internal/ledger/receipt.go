package ledger

// The portable receipt — evidence about one effect, detached from the ledger
// that produced it.
//
// Until now, proving anything about a sealed effect meant handing over the
// whole SQLite file. That is bad on three counts: every unrelated effect
// travels with it, the file may be large or simply gone, and the verifier
// still needs a tool that speaks the node's storage layout. In practice it
// meant the evidence was never actually shared, which makes "verifiable"
// theoretical.
//
// A receipt is a single self-contained JSON document about one effect:
//
//	the sealed entry              what happened, under what authority
//	an RFC 6962 inclusion proof   that this entry really is at this position
//	                              in a tree of this size
//	the signed checkpoint         the node's own commitment to that tree head
//	any witness countersignatures third parties that saw the same head
//	the cited C5 attestations     what the models upstream claimed (see attest.go)
//
// VerifyReceipt checks all of it with no database, no network and no running
// node — it is a pure function of the document. That is the difference
// between evidence you can email and evidence you have to grant someone
// server access to inspect.
//
// The privacy property falls out of the design rather than being bolted on:
// a receipt reveals the effect it is about and nothing whatsoever about the
// other entries in the ledger, because an inclusion proof is log₂(n) sibling
// hashes and a hash discloses nothing about its preimage.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"

	"aura/kernel/internal/signing"
	"aura/kernel/internal/store"
)

// ReceiptVersion identifies the document shape. Bumped only on a breaking
// change; new optional fields do not bump it.
const ReceiptVersion = "aura-receipt/1"

// Receipt is the self-contained evidence document.
//
// ## Why the hash-critical bytes are base64, not JSON
//
// The entry and the attestations are carried base64-encoded rather than as
// embedded JSON. That looks like a downgrade in readability and is in fact
// the only thing that makes the document work.
//
// The leaf hash is `SHA256(0x00 || entry_json)` over the *exact bytes* that
// were sealed. Embedding those bytes as JSON inside a JSON document does not
// preserve them: `json.MarshalIndent` re-indents nested values, every
// pretty-printer reorders or respaces, and any of those is a different byte
// string and therefore a different hash. The first CLI build of this shipped
// with the entry as embedded JSON, wrote receipts with `MarshalIndent`, and
// produced documents that could never verify — the Go tests missed it because
// they round-tripped through compact `json.Marshal`, which happens to
// preserve the bytes.
//
// Base64 makes the payload opaque to every JSON layer it passes through,
// which is the same reason JWS and COSE do it. A receipt can now be
// pretty-printed, reformatted, re-serialised by a proxy or pasted through a
// tool that sorts keys, and it still verifies.
type Receipt struct {
	Version string `json:"version"`

	// EntryB64 is the sealed C4 entry: base64 of exactly the bytes that were
	// hashed into the tree. Decode it to read the entry — `aura receipt
	// --verify` does, and prints it.
	EntryB64  string `json:"entry_b64"`
	EntryHash string `json:"entry_hash"`

	// Inclusion proves Entry sits at LeafIndex in a tree of TreeSize leaves
	// whose head is MerkleRoot. LeafIndex is seq-1: the ledger counts entries
	// from 1, the tree indexes leaves from 0.
	LeafIndex  int      `json:"leaf_index"`
	TreeSize   int      `json:"tree_size"`
	MerkleRoot string   `json:"merkle_root"`
	Inclusion  []string `json:"inclusion"`

	// Checkpoint is the node's signature over that tree head. Without it the
	// inclusion proof shows only "this entry is in *a* tree" — the signature
	// is what ties the tree to a node that committed to it.
	Checkpoint ReceiptCheckpoint `json:"checkpoint"`

	// Witnesses are third-party countersignatures over the same head. Empty
	// is normal and is not a failure: it means nobody outside this node has
	// vouched, which VerifyReceipt reports rather than punishes.
	Witnesses []ReceiptWitness `json:"witnesses,omitempty"`

	// Attestations are the C5 records the entry's `inference` field cites,
	// keyed by content address, base64 for the same reason EntryB64 is.
	// Included so a receipt answers "on what basis" without a second lookup —
	// and so the content addresses can be re-checked here, which is what
	// keeps the citation meaningful.
	Attestations map[string]string `json:"attestations,omitempty"`
}

// Entry decodes the sealed entry's raw bytes. Returns nil if the field is not
// valid base64, which VerifyReceipt reports as a problem rather than panicking
// on.
func (r Receipt) Entry() []byte {
	raw, err := base64.StdEncoding.DecodeString(r.EntryB64)
	if err != nil {
		return nil
	}
	return raw
}

// Attestation decodes one cited C5 record.
func (r Receipt) Attestation(hash string) ([]byte, bool) {
	enc, ok := r.Attestations[hash]
	if !ok {
		return nil, false
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return nil, false
	}
	return raw, true
}

// ReceiptCheckpoint is the signed head a receipt is anchored to.
type ReceiptCheckpoint struct {
	Seq        uint64 `json:"seq"`
	HeadHash   string `json:"head_hash"`
	MerkleRoot string `json:"merkle_root"`
	Node       string `json:"node"`
	Pubkey     string `json:"pubkey"`
	Signature  string `json:"signature"`
	TS         int64  `json:"ts"`
}

// ReceiptWitness is one third party's countersignature over that head.
type ReceiptWitness struct {
	WitnessKey string `json:"witness_key"`
	Signature  string `json:"signature"`
	URL        string `json:"url,omitempty"`
	TS         int64  `json:"ts"`
}

// BuildReceipt assembles the evidence document for one sealed effect,
// identified by its receipt hash (the value that rode back on the delivered
// envelope, and that `aura undo` also accepts).
//
// It anchors to the earliest checkpoint that covers the entry. Earliest
// rather than latest, deliberately: that checkpoint is the oldest signed
// commitment naming this entry, so it is the strongest statement available
// about *when* the node committed — a later checkpoint would prove the same
// membership while implying the commitment is more recent than it is.
func BuildReceipt(st *store.Store, entryHash string) (Receipt, error) {
	raw, err := st.LedgerEntryByHash(entryHash)
	if err != nil {
		return Receipt{}, err
	}
	var entry Entry
	if err := json.Unmarshal(raw, &entry); err != nil {
		return Receipt{}, fmt.Errorf("stored entry %s is not valid JSON: %w", entryHash, err)
	}

	checkpoints, err := st.LedgerCheckpoints()
	if err != nil {
		return Receipt{}, fmt.Errorf("read checkpoints: %w", err)
	}
	var anchor *store.CheckpointRow
	for i := range checkpoints {
		if checkpoints[i].Seq >= entry.Seq && checkpoints[i].MerkleRoot != "" {
			anchor = &checkpoints[i]
			break
		}
	}
	if anchor == nil {
		return Receipt{}, fmt.Errorf(
			"effect %s (seq %d) is sealed but not yet covered by a signed checkpoint carrying "+
				"a merkle root — checkpoints are periodic, so either wait for the next one or "+
				"force one; a receipt with no signed head would prove only that this node's "+
				"own database currently says so", shortHash(entryHash), entry.Seq)
	}

	entries, err := st.LedgerEntries(1, 0)
	if err != nil {
		return Receipt{}, fmt.Errorf("read entries: %w", err)
	}
	if uint64(len(entries)) < anchor.Seq {
		return Receipt{}, fmt.Errorf(
			"checkpoint covers %d entries but only %d are stored", anchor.Seq, len(entries))
	}
	leaves := make([]Hash, anchor.Seq)
	for i := range leaves {
		leaves[i] = LeafHash(entries[i])
	}
	index := int(entry.Seq - 1)
	proof, err := InclusionProof(leaves, index)
	if err != nil {
		return Receipt{}, err
	}
	path := make([]string, len(proof))
	for i, h := range proof {
		path[i] = h.String()
	}

	r := Receipt{
		Version:  ReceiptVersion,
		EntryB64: base64.StdEncoding.EncodeToString(raw), EntryHash: entryHash,
		LeafIndex: index, TreeSize: int(anchor.Seq), MerkleRoot: anchor.MerkleRoot,
		Inclusion: path,
		Checkpoint: ReceiptCheckpoint{
			Seq: anchor.Seq, HeadHash: anchor.HeadHash, MerkleRoot: anchor.MerkleRoot,
			Node: entry.Node, Pubkey: anchor.Pubkey, Signature: anchor.Signature, TS: anchor.TS,
		},
	}

	witnesses, err := st.LedgerWitnesses()
	if err != nil {
		return Receipt{}, fmt.Errorf("read witnesses: %w", err)
	}
	for _, w := range witnesses {
		if w.Seq == anchor.Seq && w.MerkleRoot == anchor.MerkleRoot {
			r.Witnesses = append(r.Witnesses, ReceiptWitness{
				WitnessKey: w.WitnessKey, Signature: w.Signature, URL: w.WitnessURL, TS: w.TS,
			})
		}
	}

	for _, hash := range entry.Inference {
		record, err := st.Attestation(hash)
		if err != nil {
			// A cited attestation that cannot be found is a real problem, but
			// not a reason to withhold the rest of the evidence: the receipt
			// is still valid about the effect, and VerifyReceipt will report
			// the citation as unresolved rather than silently complete.
			continue
		}
		if r.Attestations == nil {
			r.Attestations = map[string]string{}
		}
		r.Attestations[hash] = base64.StdEncoding.EncodeToString(record)
	}
	return r, nil
}

// ReceiptReport is what verification concluded. Every field is a separate
// claim so a caller can decide which ones it cares about, rather than being
// handed a single boolean that hides which check carried the verdict.
type ReceiptReport struct {
	// EntryHashMatches: the entry's bytes really do hash to EntryHash.
	EntryHashMatches bool `json:"entry_hash_matches"`
	// InclusionValid: the audit path recomputes MerkleRoot from this entry.
	InclusionValid bool `json:"inclusion_valid"`
	// CheckpointValid: the node's signature over that head verifies.
	CheckpointValid bool `json:"checkpoint_valid"`
	// AttestationsResolved / AttestationsCited: how many of the C5 records
	// the entry cites were included AND re-hash to their content address.
	AttestationsCited    int `json:"attestations_cited"`
	AttestationsResolved int `json:"attestations_resolved"`
	// Witnesses / WitnessesValid: third-party countersignatures over the
	// same head, and how many verify.
	Witnesses      int `json:"witnesses"`
	WitnessesValid int `json:"witnesses_valid"`

	Problems []string `json:"problems,omitempty"`
}

// Sound is the conjunction of the checks that must hold for the receipt to be
// evidence at all.
//
// Witness count is excluded for the same reason it is excluded from
// Report.Sound: zero witnesses is a weaker claim, not a broken one. An
// unresolved attestation citation is likewise not fatal — the effect is still
// proven; what is missing is the record of what argued for it, which the
// report states explicitly instead of folding into a verdict.
func (r ReceiptReport) Sound() bool {
	return r.EntryHashMatches && r.InclusionValid && r.CheckpointValid
}

// VerifyReceipt checks a receipt using nothing but the receipt.
//
// No store, no network, no node, no clock. Everything it needs is in the
// document, which is the property that makes a receipt worth having: the
// party checking it does not have to be, or trust, the party that issued it.
func VerifyReceipt(r Receipt) ReceiptReport {
	var rep ReceiptReport

	if r.Version != ReceiptVersion {
		rep.Problems = append(rep.Problems, fmt.Sprintf(
			"receipt version %q, this verifier speaks %q", r.Version, ReceiptVersion))
	}

	// 1. The entry is what it claims to be. Hash over the carried bytes.
	entryBytes := r.Entry()
	if entryBytes == nil {
		rep.Problems = append(rep.Problems, "entry_b64 is not valid base64")
		return rep
	}
	var entry Entry
	if err := json.Unmarshal(entryBytes, &entry); err != nil {
		rep.Problems = append(rep.Problems, "entry is not valid JSON: "+err.Error())
		return rep
	}
	if got := entry.Hash(); got == r.EntryHash {
		rep.EntryHashMatches = true
	} else {
		rep.Problems = append(rep.Problems, fmt.Sprintf(
			"entry hashes to %s but the receipt claims %s — the entry was altered",
			shortHash(got), shortHash(r.EntryHash)))
	}

	// 2. The entry is in the tree, at the position claimed.
	root, err := ParseHash(r.MerkleRoot)
	if err != nil {
		rep.Problems = append(rep.Problems, "merkle root: "+err.Error())
		return rep
	}
	proof := make([]Hash, len(r.Inclusion))
	for i, raw := range r.Inclusion {
		if proof[i], err = ParseHash(raw); err != nil {
			rep.Problems = append(rep.Problems, fmt.Sprintf("inclusion path element %d: %v", i, err))
			return rep
		}
	}
	if VerifyInclusion(LeafHash(entryBytes), r.LeafIndex, r.TreeSize, proof, root) {
		rep.InclusionValid = true
	} else {
		rep.Problems = append(rep.Problems,
			"inclusion proof does not place this entry at the position it claims in the "+
				"tree the checkpoint signed")
	}
	if int(entry.Seq) != r.LeafIndex+1 {
		rep.Problems = append(rep.Problems, fmt.Sprintf(
			"entry says seq %d but the receipt places it at leaf index %d",
			entry.Seq, r.LeafIndex))
	}

	// 3. A node committed to that tree head under its own key.
	if r.Checkpoint.MerkleRoot != r.MerkleRoot {
		rep.Problems = append(rep.Problems,
			"the checkpoint signs a different tree head than the inclusion proof was built against")
	} else if int(r.Checkpoint.Seq) != r.TreeSize {
		rep.Problems = append(rep.Problems, fmt.Sprintf(
			"checkpoint covers %d entries but the proof assumes a tree of %d",
			r.Checkpoint.Seq, r.TreeSize))
	} else if err := signing.Verify(r.Checkpoint.Pubkey, r.Checkpoint.Signature,
		checkpointPayload(r.Checkpoint.Seq, r.Checkpoint.HeadHash, r.Checkpoint.MerkleRoot)); err != nil {
		rep.Problems = append(rep.Problems, "checkpoint signature does not verify: "+err.Error())
	} else {
		rep.CheckpointValid = true
	}

	// 4. The cited C5 attestations are present and are what was cited.
	rep.AttestationsCited = len(entry.Inference)
	for _, hash := range entry.Inference {
		record, ok := r.Attestation(hash)
		if !ok {
			rep.Problems = append(rep.Problems, fmt.Sprintf(
				"entry cites inference attestation %s, which the receipt does not carry "+
					"(or carries un-decodable)", shortHash(hash)))
			continue
		}
		if AttestationHash(record) != hash {
			rep.Problems = append(rep.Problems, fmt.Sprintf(
				"attestation %s does not match its content address — the record was altered",
				shortHash(hash)))
			continue
		}
		rep.AttestationsResolved++
	}

	// 5. Third parties, if any, that saw the same head.
	rep.Witnesses = len(r.Witnesses)
	for _, w := range r.Witnesses {
		if err := signing.Verify(w.WitnessKey, w.Signature,
			witnessPayload(r.Checkpoint.Node, r.Checkpoint.Seq, r.Checkpoint.MerkleRoot)); err != nil {
			rep.Problems = append(rep.Problems, fmt.Sprintf(
				"witness %s: signature does not verify", Fingerprint(w.WitnessKey)))
			continue
		}
		rep.WitnessesValid++
	}
	return rep
}
