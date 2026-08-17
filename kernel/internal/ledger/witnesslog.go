package ledger

// C4 v1.4 — the witness's own log, and why a witness needs one.
//
// witness.go closes the gap a self-signed ledger cannot: a node holding its own
// key can rewrite history and re-sign it, and only an outside party who
// remembers what it already saw can contradict that. So far, so Certificate
// Transparency.
//
// But it leaves the obvious question unanswered, and it is the question anyone
// evaluating this asks second: **who watches the witness?** Until this file the
// honest answer was nobody. A witness kept one row per node
// (`witnessed_heads`), replaced as that node advanced. A replaced row records
// nothing about what it used to say, so a witness could quietly revise what it
// had vouched for and no one could demonstrate it. That makes a witness a party
// you have to *trust* — which is the exact thing the ledger was built to stop
// needing.
//
// CT's answer is that the logs are themselves Merkle logs, published, followed
// by monitors. This is that, applied to the witness:
//
//   - **Every countersignature it issues is appended to its own log**, never
//     updated, never deleted, with an RFC 6962 tree over it.
//   - **The log head is signed and served**, so anyone can take a snapshot.
//   - **Consistency proofs** are served over it, so a follower can check the
//     history it saw last week is still a prefix of this week's.
//   - **`last-seen` is signed**, which is the load-bearing one — see below.
//
// # Why a signed last-seen is the load-bearing part
//
// A node asks a witness "how far have you vouched for me?" before building its
// consistency proof. When that answer was a bare integer, a witness could tell
// the node one thing and an auditor another, and neither could prove it: two
// unsigned numbers are two rumours.
//
// Signed, the same two answers are two signed statements that cannot both be
// true. **Split-view stops being undetectable and becomes self-incriminating**
// — the witness's own key is on both. That is the property that lets several
// mutually-suspicious parties rely on one witness without any of them having to
// believe it, which is the only arrangement in which a shared witness is worth
// having at all.

import (
	"encoding/json"
	"fmt"
	"time"

	"aura/kernel/internal/signing"
	"aura/kernel/internal/store"
)

// Domain-separated payloads. Three roles, three prefixes, one key — the same
// discipline the checkpoint, package and countersignature payloads already
// follow, so no signature is usable outside the role it was made for.
const (
	witnessLogDomain      = "aura-witness-log-v1"
	witnessLastSeenDomain = "aura-witness-lastseen-v1"
)

// WitnessRecord is one entry in a witness's own log: the countersignature it
// issued, as a self-contained statement.
//
// The leaf hashed into the tree is this record's canonical JSON, so a monitor
// that fetches the log rebuilds exactly the tree the witness signed. Field
// order is fixed by the struct, and there is no map-typed field, so encoding is
// deterministic without further canonicalisation — the same argument Entry.Hash
// rests on.
type WitnessRecord struct {
	Seq        uint64 `json:"seq"`
	Node       string `json:"node"`
	NodeSeq    uint64 `json:"node_seq"`
	MerkleRoot string `json:"merkle_root"`
	NodePubkey string `json:"node_pubkey"`
	Signature  string `json:"signature"`
	TS         int64  `json:"ts"`
}

func (r WitnessRecord) marshal() []byte {
	raw, _ := json.Marshal(r) // a struct of strings and integers never fails
	return raw
}

// LogHead is a witness's signed statement about its own history.
type LogHead struct {
	WitnessKey string `json:"witness_key"`
	Size       uint64 `json:"size"`
	Root       string `json:"root"`
	TS         int64  `json:"ts"`
	Signature  string `json:"signature"`
}

// logHeadPayload is what a witness signs over its own head.
func logHeadPayload(witnessKey string, size uint64, root string) []byte {
	return []byte(fmt.Sprintf("%s:%s:%d:%s", witnessLogDomain, witnessKey, size, root))
}

// VerifyLogHead checks a witness's signature over its own head. Exported
// because the interesting caller is a monitor holding nothing but this struct.
func VerifyLogHead(h LogHead) error {
	if h.WitnessKey == "" || h.Root == "" {
		return fmt.Errorf("witness head is missing its key or root")
	}
	return signing.Verify(h.WitnessKey, h.Signature,
		logHeadPayload(h.WitnessKey, h.Size, h.Root))
}

// LastSeenStatement is a witness's signed answer to "how far have you vouched
// for this node?"
//
// It carries the witness's own log position alongside the answer, and that
// pairing is what makes it evidence rather than a claim: a witness cannot
// truthfully say "I vouched for node N at 100" while its own published log,
// committed to in the same signature, contains no such record. The two halves
// can be checked against each other by anyone.
type LastSeenStatement struct {
	WitnessKey string `json:"witness_key"`
	Node       string `json:"node"`
	// Seq and MerkleRoot are what this witness last vouched for about Node.
	// Zero and empty mean "never seen", which is a real, signable answer and
	// not an error — a witness should be able to be held to a denial too.
	Seq        uint64 `json:"seq"`
	MerkleRoot string `json:"merkle_root,omitempty"`
	// LogSize and LogRoot pin the witness's own history at the moment it
	// answered.
	LogSize uint64 `json:"log_size"`
	LogRoot string `json:"log_root"`
	TS      int64  `json:"ts"`
	Sig     string `json:"sig"`
}

func lastSeenPayload(s LastSeenStatement) []byte {
	return []byte(fmt.Sprintf("%s:%s:%s:%d:%s:%d:%s:%d",
		witnessLastSeenDomain, s.WitnessKey, s.Node, s.Seq, s.MerkleRoot,
		s.LogSize, s.LogRoot, s.TS))
}

// VerifyLastSeen checks a witness's signature over its last-seen answer.
func VerifyLastSeen(s LastSeenStatement) error {
	if s.WitnessKey == "" {
		return fmt.Errorf("last-seen statement names no witness")
	}
	if s.Node == "" {
		return fmt.Errorf("last-seen statement names no node")
	}
	return signing.Verify(s.WitnessKey, s.Sig, lastSeenPayload(s))
}

// Contradicts reports whether two signed last-seen statements from the same
// witness about the same node cannot both be true.
//
// This is the detector the whole file exists for. Two statements conflict when
// the witness claims different things about the same node at the same log size
// — it had one history when it answered, so it had one answer — or when it
// reports a *smaller* position for a node than it did earlier, since what a
// witness has vouched for only ever grows.
//
// Both statements have to verify first; a caller that skips that is comparing
// forgeries. Verification is left to the caller rather than done here so the
// failure ("this did not verify") stays distinguishable from the finding
// ("these two verified statements are irreconcilable"), which is a much more
// serious thing to report.
func Contradicts(a, b LastSeenStatement) (bool, string) {
	if a.WitnessKey != b.WitnessKey || a.Node != b.Node {
		return false, ""
	}
	if a.LogSize == b.LogSize && (a.Seq != b.Seq || a.MerkleRoot != b.MerkleRoot) {
		return true, fmt.Sprintf(
			"at log size %d this witness signed two different answers about %s: "+
				"%d/%s and %d/%s", a.LogSize, a.Node,
			a.Seq, shortHash(a.MerkleRoot), b.Seq, shortHash(b.MerkleRoot))
	}
	earlier, later := a, b
	if b.TS < a.TS {
		earlier, later = b, a
	}
	if later.Seq < earlier.Seq {
		return true, fmt.Sprintf(
			"this witness vouched for %s at %d entries and later reported only %d — "+
				"what a witness has vouched for does not shrink",
			a.Node, earlier.Seq, later.Seq)
	}
	return false, ""
}

// ── the witness side ────────────────────────────────────────────────

// initLog rebuilds the witness's own tree from storage. Called once at
// construction, for the same reason Ledger.Open rebuilds its tree: the head a
// witness signs must be a function of what is durably on disk, not of what this
// process happens to remember.
func (w *Witness) initLog() error {
	rows, err := w.st.WitnessLogEntries(1, 0)
	if err != nil {
		return fmt.Errorf("rebuild witness log tree: %w", err)
	}
	for i, r := range rows {
		if uint64(i+1) != r.Seq {
			return fmt.Errorf(
				"witness log is inconsistent: expected seq %d at position %d, found %d — "+
					"an entry was deleted or reordered", i+1, i+1, r.Seq)
		}
		w.logTree.Append(LeafHash(r.Entry))
	}
	w.logSize = uint64(len(rows))
	return nil
}

// appendLog records an issued countersignature in the witness's own log.
// Caller holds mu.
//
// Appended *before* the countersignature is returned, deliberately: a witness
// that hands out a signature it has not committed to is one that can later deny
// having issued it, which is the failure this log exists to make impossible.
func (w *Witness) appendLogLocked(s Statement, sig string, ts int64) error {
	rec := WitnessRecord{
		Seq: w.logSize + 1, Node: s.Node, NodeSeq: s.Seq,
		MerkleRoot: s.MerkleRoot, NodePubkey: s.Pubkey, Signature: sig, TS: ts,
	}
	raw := rec.marshal()
	if err := w.st.AppendWitnessLog(store.WitnessLogRow{
		Seq: rec.Seq, Node: rec.Node, NodeSeq: rec.NodeSeq,
		MerkleRoot: rec.MerkleRoot, NodePubkey: rec.NodePubkey,
		Signature: rec.Signature, TS: rec.TS, Entry: raw,
	}); err != nil {
		return err
	}
	w.logTree.Append(LeafHash(raw))
	w.logSize = rec.Seq
	return nil
}

// LogHead is this witness's current signed head.
func (w *Witness) LogHead() LogHead {
	w.mu.Lock()
	size, root := w.logSize, w.logTree.Root().String()
	w.mu.Unlock()

	key := w.keys.PublicB64()
	return LogHead{
		WitnessKey: key, Size: size, Root: root, TS: time.Now().UnixMilli(),
		Signature: w.keys.Sign(logHeadPayload(key, size, root)),
	}
}

// LastSeenSigned answers "how far have you vouched for this node?" as a signed
// statement that commits to the witness's own log position at the same time.
func (w *Witness) LastSeenSigned(nodeID string) (LastSeenStatement, error) {
	head, seen, err := w.st.LastWitnessedHead(nodeID)
	if err != nil {
		return LastSeenStatement{}, err
	}
	w.mu.Lock()
	size, root := w.logSize, w.logTree.Root().String()
	w.mu.Unlock()

	s := LastSeenStatement{
		WitnessKey: w.keys.PublicB64(), Node: nodeID,
		LogSize: size, LogRoot: root, TS: time.Now().UnixMilli(),
	}
	if seen {
		s.Seq, s.MerkleRoot = head.Seq, head.MerkleRoot
	}
	s.Sig = w.keys.Sign(lastSeenPayload(s))
	return s, nil
}

// LogEntries serves a slice of the witness's own log, for a monitor rebuilding
// the tree.
func (w *Witness) LogEntries(fromSeq uint64, limit int) ([]WitnessRecord, error) {
	if fromSeq == 0 {
		fromSeq = 1
	}
	rows, err := w.st.WitnessLogEntries(fromSeq, limit)
	if err != nil {
		return nil, err
	}
	out := make([]WitnessRecord, 0, len(rows))
	for _, r := range rows {
		var rec WitnessRecord
		if err := json.Unmarshal(r.Entry, &rec); err != nil {
			return nil, fmt.Errorf("witness log entry %d is unreadable: %w", r.Seq, err)
		}
		out = append(out, rec)
	}
	return out, nil
}

// LogConsistency proves the witness's log at size m is a prefix of its log at
// size n — what a monitor checks on every visit.
func (w *Witness) LogConsistency(m, n uint64) ([]string, error) {
	rows, err := w.st.WitnessLogEntries(1, 0)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		n = uint64(len(rows))
	}
	if n > uint64(len(rows)) {
		return nil, fmt.Errorf("witness log has %d entries; asked for a proof to %d", len(rows), n)
	}
	leaves := make([]Hash, len(rows))
	for i, r := range rows {
		leaves[i] = LeafHash(r.Entry)
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

// LogInclusion proves one countersignature really is in the published log at
// the position the witness claims.
//
// This is what a node presents when it wants to show a third party that its
// anchor was actually recorded, not merely handed to it in private.
func (w *Witness) LogInclusion(seq uint64) (rec WitnessRecord, proof []string, size uint64, err error) {
	rows, err := w.st.WitnessLogEntries(1, 0)
	if err != nil {
		return WitnessRecord{}, nil, 0, err
	}
	if seq == 0 || seq > uint64(len(rows)) {
		return WitnessRecord{}, nil, 0, fmt.Errorf(
			"witness log has %d entries; there is no entry %d", len(rows), seq)
	}
	leaves := make([]Hash, len(rows))
	for i, r := range rows {
		leaves[i] = LeafHash(r.Entry)
	}
	raw, err := InclusionProof(leaves, int(seq-1))
	if err != nil {
		return WitnessRecord{}, nil, 0, err
	}
	if err := json.Unmarshal(rows[seq-1].Entry, &rec); err != nil {
		return WitnessRecord{}, nil, 0, err
	}
	proof = make([]string, len(raw))
	for i, h := range raw {
		proof[i] = h.String()
	}
	return rec, proof, uint64(len(rows)), nil
}
