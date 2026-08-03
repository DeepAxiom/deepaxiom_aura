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

// checkpointPayload is the domain-separated byte string a checkpoint's
// signature covers — same pattern as signing.Payload for package artifacts,
// so a checkpoint signature can never be replayed as a package signature or
// vice versa.
func checkpointPayload(seq uint64, headHash string) []byte {
	return []byte(fmt.Sprintf("aura-ledger-checkpoint-v1:%d:%s", seq, headHash))
}

// sealCheckpointLocked signs the current head. Caller must hold mu.
func (l *Ledger) sealCheckpointLocked() error {
	sig := l.keys.Sign(checkpointPayload(l.lastSeq, l.lastHash))
	cp := store.CheckpointRow{
		Seq: l.lastSeq, HeadHash: l.lastHash, Pubkey: l.pubkeyB6,
		Signature: sig, TS: time.Now().UnixMilli(),
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
