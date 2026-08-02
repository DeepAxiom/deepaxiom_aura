// Package ledger implements kernel primitive P5 — the effect ledger (C4).
//
// Todos los demás primitives contestan "qué pasó". Este contesta algo más
// puntual: qué actuó sobre el mundo, quién lo permitió, y si eso se puede
// demostrar después del hecho sin tener que confiar en nadie. Un skill que
// solo lee jamás aparece acá — el ledger es evidencia de *efectos*, punto,
// no una copia barata del causal log.
//
// La idea en una frase: cada efecto lo autoriza una policy de nodo (P4, ver
// package executor), se sella acá en un registro hash-chained, y cada N
// entries o T segundos (lo que llegue primero) el head actual se firma con
// la Ed25519 key del propio nodo (identity.Node.Keys). Tocar una entry vieja
// rompe el hash de todo lo que viene después; tocar una entry ya
// checkpointeada además rompe una firma que nadie, salvo quien tenga la
// private key del nodo, pudo haber producido. Verify (verify.go) chequea
// las dos cosas, y lo hace leyendo solo el archivo SQLite — sin kernel
// corriendo — que es justo lo que convierte esto en evidencia de verdad y
// no en un log más.
//
// El kernel se mantiene puro acá: este package importa store y signing y
// nada que sepa de LLMs, modelos o marketplace. Si algún día una feature
// necesita "inteligencia" para decidir qué entra al ledger, esa decisión se
// toma en executor (que ya la calcula, vía el policy engine) y se pasa como
// SealRequest — este package nunca pregunta "qué debería decidirse aquí",
// solo hace una cosa: "queda registrado que se decidió, y ese registro no
// se mueve más".
package ledger

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"aura/kernel/internal/signing"
	"aura/kernel/internal/spec"
	"aura/kernel/internal/store"
)

// Checkpoint cadence: whichever comes first. Signing costs the same for one
// entry as for a thousand — Ed25519 signs a fixed-size digest, not the
// entries themselves — so amortising over up to 100 entries or 60 seconds
// costs nothing extra on the busiest node and never leaves more than a
// minute of unattested history on the quietest one.
const (
	checkpointEveryN = 100
	checkpointEveryT = 60 * time.Second
)

// Compensation is carried into an entry when the skill that produced the
// effect declared C1 `compensates`. Nil means the effect was recorded as
// irreversible — itself useful information, not a gap.
type Compensation struct {
	Capability string `json:"capability"`
	Port       string `json:"port"`
	Schema     string `json:"schema,omitempty"`
}

// Entry is one sealed effect (C4). Deliberately small: ~300-400 bytes, so a
// million effects is a few hundred MB, not the multi-gigabyte data lake a
// full-payload log would become.
type Entry struct {
	Seq     uint64 `json:"seq"`
	Prev    string `json:"prev"` // previous entry's Hash(), "" for the genesis entry (seq 1)
	TS      int64  `json:"ts"`   // unix millis
	Node    string `json:"node"` // this node's identity.Node.ID
	Session string `json:"session"`
	// Envelope is the id of the envelope that carried (or would have
	// carried) the effect — the join key back into the causal event log,
	// so `aura why` and this ledger describe the same history from two
	// angles.
	Envelope string `json:"envelope"`
	Cause    string `json:"cause"`
	// Actor is the package that produced the effect: "org/cat/name@version".
	// The version matters — "what version did this" is the first question
	// any incident review asks.
	Actor      string `json:"actor"`
	Capability string `json:"capability"`
	// Decision is the policy_decisions value that authorized this
	// (allow|gate); Outcome is the effect_outcomes value recording what
	// happened next (delivered|denied). A "gate" decision that is later
	// denied is still one sealed entry, not two — the entry records the
	// proposal and its resolution together.
	Decision string `json:"decision"`
	Outcome  string `json:"outcome"`
	// Policy is the hash of the policy document in force (Policy.Hash()) —
	// an auditor can go find the exact document that authorized this.
	Policy string `json:"policy"`
	// PayloadSHA256, never the payload. The ledger is evidence, not a data
	// lake: a payload carrying personal data must not become permanently
	// undeletable, and this is what keeps entries small regardless of what
	// the effect actually carried.
	PayloadSHA256 string        `json:"payload_sha256"`
	Compensation  *Compensation `json:"compensation,omitempty"`
	// Compensates is set only on an entry that IS an undo: the receipt (Hash())
	// of the entry it reverses. Additive C4 field (Phase 2). Its presence is
	// also what makes an undo idempotent — see ledger.Entry.Compensates usage
	// in executor.NewSession, which refuses a second undo of the same receipt
	// by checking whether any entry already carries it here.
	Compensates string `json:"compensates,omitempty"`
	// Inference lists the content addresses of every C5 attestation upstream
	// in this effect's causal chain — what the models that argued for this
	// act claimed about themselves (see attest.go). Sorted, so the entry hash
	// is a function of the set and not of goroutine timing.
	//
	// This is the field that makes the ledger answer "on what basis" rather
	// than only "by whose authority". It is a []string of hashes rather than
	// inline records for three reasons: entries stay small and uniform, the
	// same configuration across a thousand effects costs one stored record,
	// and the binding stays cryptographic — editing an attestation breaks its
	// content address, and editing the list breaks the entry hash and every
	// hash after it.
	//
	// Empty on an effect no attested inference contributed to: a webhook that
	// fires a write directly, a hand-driven CLI call. Absence is meaningful
	// and is not the same as "unknown".
	Inference []string `json:"inference,omitempty"`
}

// Hash is this entry's identity in the chain: sha256 of its canonical JSON,
// prefixed like every other content hash in this codebase (see
// executor.Policy.Hash). Go's encoding/json marshals struct fields in their
// declared order — fixed at compile time — so this is already deterministic
// without the map-key sorting signing.CanonicalManifestHash needs for
// arbitrary user-supplied YAML; Entry has no such field.
func (e Entry) Hash() string {
	raw, _ := json.Marshal(e) // a struct of strings/uint64/*struct never fails to marshal
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// SealRequest is what a caller (executor.Session) asks to have sealed. It
// mirrors Entry minus the fields the ledger itself owns: Seq, Prev, TS, Node,
// Hash.
type SealRequest struct {
	Session      string
	Envelope     string
	Cause        string
	Actor        string
	Capability   string
	Decision     string
	Outcome      string
	Policy       string
	Payload      json.RawMessage
	Compensation *Compensation
	// Compensates, when set, marks this entry as the undo of the entry whose
	// Hash() equals this value. Empty for every ordinary effect.
	Compensates string
	// Inference is the set of C5 attestation hashes upstream of this effect.
	// Seal sorts and de-duplicates it, so callers may pass it in whatever
	// order they collected it.
	Inference []string
}

// Ledger is one node's effect ledger: a single writer serialised by mu, so
// the chain — which is a property of *sequence*, not of any one entry — can
// never be built from two entries computed concurrently against a stale head.
type Ledger struct {
	st       *store.Store
	nodeID   string
	keys     *signing.Keypair
	pubkeyB6 string

	mu               sync.Mutex
	lastSeq          uint64
	lastHash         string
	sinceCheckpoint  int
	lastCheckpointAt time.Time
	// tree is the RFC 6962 head over every entry sealed so far (C4 v1.2). It
	// is maintained incrementally — O(log n) hashes per effect — because the
	// alternative, rehashing the whole history at each checkpoint, would make
	// sealing cost grow with the ledger's age. See merkle.go for why the tree
	// exists at all: the linear chain proves the whole ledger to someone
	// holding the whole ledger; the tree proves one entry to someone holding
	// one receipt.
	tree CompactTree
}

// Open resumes (or starts) a node's ledger. It reads the current head and the
// most recent checkpoint from the store so cadence and chaining are correct
// across a restart — a freshly opened ledger does not forget it had already
// checkpointed five seconds before the process was killed.
func Open(st *store.Store, nodeID string, keys *signing.Keypair) (*Ledger, error) {
	if keys == nil {
		return nil, fmt.Errorf("ledger requires a node identity keypair to sign checkpoints")
	}
	seq, hash, err := st.LedgerHead()
	if err != nil {
		return nil, fmt.Errorf("read ledger head: %w", err)
	}
	l := &Ledger{
		st: st, nodeID: nodeID, keys: keys, pubkeyB6: keys.PublicB64(),
		lastSeq: seq, lastHash: hash, lastCheckpointAt: time.Now(),
	}
	// Rebuild the Merkle tree from storage. This is the one O(n) step at
	// startup, and it is deliberate: the tree has to be a function of what is
	// durably on disk, not of what this process remembers, or a node that
	// restarted mid-write would sign a head no verifier could reproduce.
	entries, err := st.LedgerEntries(1, 0)
	if err != nil {
		return nil, fmt.Errorf("rebuild merkle tree: %w", err)
	}
	for _, raw := range entries {
		l.tree.Append(LeafHash(raw))
	}
	if uint64(len(entries)) != seq {
		return nil, fmt.Errorf(
			"ledger is inconsistent: head is at seq %d but %d entries are stored — "+
				"run `aura verify` against this data directory", seq, len(entries))
	}

	if last, ok, err := st.LastCheckpoint(); err != nil {
		return nil, fmt.Errorf("read last checkpoint: %w", err)
	} else if ok {
		l.sinceCheckpoint = int(seq - last.Seq)
		l.lastCheckpointAt = time.UnixMilli(last.TS)
	} else if seq > 0 {
		// Entries exist from before checkpointing was ever run against this
		// data dir (or all prior checkpoints were somehow lost) — checkpoint
		// promptly rather than waiting up to a full cadence window with no
		// signed head at all.
		l.sinceCheckpoint = checkpointEveryN
	}
	return l, nil
}

// Head is the node's current position: how many effects it has sealed, the
// linear chain head, and the Merkle tree head over all of them.
//
// This is what a node hands a witness (see witness.go) and what a witness
// counter-signs. It is deliberately cheap and lock-scoped: asking for the
// head must never contend with sealing an effect for longer than reading
// three fields takes.
type Head struct {
	Seq        uint64 `json:"seq"`
	HeadHash   string `json:"head_hash"`
	MerkleRoot string `json:"merkle_root"`
	Node       string `json:"node"`
	Pubkey     string `json:"pubkey"`
}

// Head returns the current head. A ledger that has sealed nothing reports
// seq 0 with the empty tree's root, which is a real signable value rather
// than a special case callers have to branch on.
func (l *Ledger) Head() Head {
	l.mu.Lock()
	defer l.mu.Unlock()
	return Head{
		Seq: l.lastSeq, HeadHash: l.lastHash, MerkleRoot: l.tree.Root().String(),
		Node: l.nodeID, Pubkey: l.pubkeyB6,
	}
}

// NodePublicKey is the base64 Ed25519 public key this ledger signs
// checkpoints with — what `aura verify` needs to check them.
func (l *Ledger) NodePublicKey() string { return l.pubkeyB6 }

// Seal authorizes-and-attests one effect: it assigns the next seq, chains it
// to the current head, persists it, and returns a receipt (the entry's own
// hash) the caller can attach to the envelope it delivers. It also seals a
// checkpoint if cadence says one is due.
//
// Callers do not choose Decision/Outcome freely — those are the policy
// engine's own vocabulary (spec.PolicyDecisions, spec.EffectOutcomes) — but
// this package does not import executor to check them, to avoid a cycle back
// toward the thing that calls it. It validates against the shared spec
// constants instead, which is exactly what both sides already agree on.
func (l *Ledger) Seal(req SealRequest) (receipt string, err error) {
	if err := validateVocabulary(req); err != nil {
		return "", err
	}

	sum := sha256.Sum256(req.Payload)
	payloadHash := hex.EncodeToString(sum[:])

	l.mu.Lock()
	defer l.mu.Unlock()

	seq := l.lastSeq + 1
	entry := Entry{
		Seq: seq, Prev: l.lastHash, TS: time.Now().UnixMilli(), Node: l.nodeID,
		Session: req.Session, Envelope: req.Envelope, Cause: req.Cause,
		Actor: req.Actor, Capability: req.Capability,
		Decision: req.Decision, Outcome: req.Outcome, Policy: req.Policy,
		PayloadSHA256: payloadHash, Compensation: req.Compensation,
		Compensates: req.Compensates,
		Inference:   normalizeInference(req.Inference),
	}
	hash := entry.Hash()

	raw, err := json.Marshal(entry)
	if err != nil {
		return "", fmt.Errorf("marshal ledger entry: %w", err)
	}
	if err := l.st.AppendLedgerEntry(seq, hash, req.Session, raw); err != nil {
		return "", fmt.Errorf("seal effect: %w", err)
	}

	l.lastSeq, l.lastHash = seq, hash
	// The leaf is hashed over exactly the bytes that were persisted, not over
	// a re-marshalling of the struct, so the tree this node signs and the tree
	// a verifier rebuilds from the database cannot diverge on an encoding
	// detail.
	l.tree.Append(LeafHash(raw))
	l.sinceCheckpoint++

	if l.sinceCheckpoint >= checkpointEveryN || time.Since(l.lastCheckpointAt) >= checkpointEveryT {
		if err := l.sealCheckpointLocked(); err != nil {
			// The entry is already durable; a failed checkpoint is not a
			// failed effect. It will be retried at the next Seal, since
			// sinceCheckpoint was not reset.
			return hash, fmt.Errorf("effect sealed (receipt %s) but checkpoint failed: %w", hash, err)
		}
	}
	return hash, nil
}

// validateVocabulary checks Decision/Outcome/Actor/Capability are non-empty
// and drawn from the shared enum, catching a caller bug (a typo'd decision
// string) before it becomes a permanent, unfixable entry in the chain — the
// one kind of mistake this package cannot let through, because nothing can
// edit an entry afterward.
func validateVocabulary(req SealRequest) error {
	if req.Capability == "" {
		return fmt.Errorf("seal: capability is required")
	}
	if req.Actor == "" {
		return fmt.Errorf("seal: actor is required")
	}
	if req.Policy == "" {
		return fmt.Errorf("seal: policy hash is required — an entry must cite what authorized it")
	}
	if !oneOf(req.Decision, spec.PolicyDecisions) {
		return fmt.Errorf("seal: decision %q is not one of %v", req.Decision, spec.PolicyDecisions)
	}
	if !oneOf(req.Outcome, spec.EffectOutcomes) {
		return fmt.Errorf("seal: outcome %q is not one of %v", req.Outcome, spec.EffectOutcomes)
	}
	return nil
}

func oneOf(v string, set []string) bool {
	for _, s := range set {
		if v == s {
			return true
		}
	}
	return false
}

// normalizeInference sorts and de-duplicates the attestation hashes bound
// into an entry.
//
// Determinism is the point. The executor collects these by walking a causal
// chain whose deliveries are concurrent, so the same effect could otherwise
// produce two different orderings — and the entry hash is over the marshalled
// struct, so a different ordering is a different hash, which is a different
// chain, which no verifier could reproduce. Sorting makes the field a
// function of the *set* of inferences involved, which is what it means
// semantically anyway.
//
// Returning nil rather than an empty slice for the empty case keeps
// `omitempty` working, so an effect with no upstream inference serializes
// exactly as it did before C5 existed — old entries rehash unchanged.
func normalizeInference(hashes []string) []string {
	if len(hashes) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(hashes))
	out := make([]string, 0, len(hashes))
	for _, h := range hashes {
		if h == "" {
			continue
		}
		if _, dup := seen[h]; dup {
			continue
		}
		seen[h] = struct{}{}
		out = append(out, h)
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}

// checkpointPayload is the domain-separated byte string a checkpoint's
// signature covers — same pattern as signing.Payload for package artifacts,
// so a checkpoint signature can never be replayed as a package signature or
// vice versa.
//
// Two versions exist, and which one applies is decided by the data rather
// than by a flag: a checkpoint carrying a Merkle root is a v2 checkpoint and
// its signature covers that root; one without is a v1 checkpoint written
// before the tree existed, and it verifies exactly as it always did. That is
// what lets a node upgraded in place keep every signature it ever made
// instead of orphaning its own history.
//
// The version lives inside the signed bytes, so a v2 checkpoint's signature
// can never be re-presented as a v1 signature over the same seq and head —
// stripping the Merkle root from a row does not yield a valid v1 checkpoint,
// it yields one that fails to verify.
func checkpointPayload(seq uint64, headHash, merkleRoot string) []byte {
	if merkleRoot == "" {
		return []byte(fmt.Sprintf("aura-ledger-checkpoint-v1:%d:%s", seq, headHash))
	}
	return []byte(fmt.Sprintf("aura-ledger-checkpoint-v2:%d:%s:%s", seq, headHash, merkleRoot))
}

// sealCheckpointLocked signs the current head. Caller must hold mu.
func (l *Ledger) sealCheckpointLocked() error {
	root := l.tree.Root().String()
	sig := l.keys.Sign(checkpointPayload(l.lastSeq, l.lastHash, root))
	cp := store.CheckpointRow{
		Seq: l.lastSeq, HeadHash: l.lastHash, Pubkey: l.pubkeyB6,
		Signature: sig, TS: time.Now().UnixMilli(), MerkleRoot: root,
	}
	if err := l.st.SaveCheckpoint(cp); err != nil {
		return err
	}
	l.sinceCheckpoint = 0
	l.lastCheckpointAt = time.Now()
	return nil
}

// Checkpoint forces a checkpoint now, regardless of cadence. `aura verify`
// and tests use this to get a deterministic, inspectable signature without
// waiting on the clock or manufacturing 100 entries.
func (l *Ledger) Checkpoint() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lastSeq == 0 {
		return fmt.Errorf("nothing to checkpoint: the ledger has sealed no effects yet")
	}
	return l.sealCheckpointLocked()
}

// Summary is the /healthz-sized view of ledger state.
type Summary struct {
	Entries     int64  `json:"entries"`
	LastHash    string `json:"last_hash,omitempty"`
	Checkpoints int    `json:"checkpoints"`
	NodePubkey  string `json:"node_pubkey"`
}

// Summarize reports the ledger's current size without walking the whole
// chain — cheap enough to compute on every health check.
func (l *Ledger) Summarize() (Summary, error) {
	n, err := l.st.LedgerEntryCount()
	if err != nil {
		return Summary{}, err
	}
	checkpoints, err := l.st.LedgerCheckpoints()
	if err != nil {
		return Summary{}, err
	}
	l.mu.Lock()
	lastHash := l.lastHash
	l.mu.Unlock()
	return Summary{
		Entries: n, LastHash: lastHash, Checkpoints: len(checkpoints),
		NodePubkey: l.pubkeyB6,
	}, nil
}
