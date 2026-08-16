// Package broker is the credential broker: the kernel holds the secrets, and
// releases one only against proof that the effect using it passed the Effect
// Checkpoint.
//
// # The hole this closes
//
// `aura guard` and the node policy share a limitation the documentation has
// always stated plainly: they hold exactly as far as an operator's control over
// the agent's configuration does. Nothing stops a process from skipping the
// gate — it just calls the ERP directly. The gate is a chokepoint only for
// traffic that chooses to pass through it, which makes it a very good safety
// rail and a very weak security boundary, and no amount of policy fixes that,
// because policy is enforced at the place the caller decided to visit.
//
// The usual answer is enforcement at the network: a proxy, eBPF, a sidecar.
// All of them work and all of them need infrastructure this runtime promises
// you will not need — and none of them are portable to a Raspberry Pi.
//
// The other answer is to make the bypass useless instead of impossible. An
// agent that skips the gate reaches the ERP and has no credential for it,
// because the credential was never in its environment, its config, or its
// memory — it is here, and the only way to obtain it is to present a receipt
// this node sealed for this capability moments ago. Going around the checkpoint
// stops being a way to avoid scrutiny and becomes a way to get a 401.
//
// # What a receipt proves
//
// A receipt is a sealed entry's hash (C4). Presenting one demonstrates, without
// the broker having to trust the presenter at all:
//
//   - the effect existed and was authorized — the entry is in the chain, and
//     an entry only gets there by passing the checkpoint;
//   - the policy allowed it, or a human approved it — the entry records which,
//     and Resolve refuses a denied one;
//   - it is *this* effect — the entry names the capability, so one skill cannot
//     spend another skill's approval;
//   - it is recent — an old receipt is not a standing licence.
//
// A forged receipt is not in the ledger. A stolen one is bound to someone
// else's capability. A replayed one is expired. None of this requires the
// broker to authenticate the caller beyond the node token it already holds,
// which matters: the strength comes from the ledger, not from a second
// permission system nobody would keep in sync with the first.
//
// # What it does not do
//
// It does not stop a skill that legitimately received a secret from keeping it.
// Once a credential is handed to a process that process has it, and no design
// short of never revealing it — a signing oracle, an egress proxy — changes
// that. What this removes is the standing, ambient credential: the token in the
// environment of a long-running agent, available for every call it ever makes,
// gated or not. The window becomes one authorized effect wide.
package broker

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"

	"aura/kernel/internal/ledger"
	"aura/kernel/internal/signing"
	"aura/kernel/internal/spec"
	"aura/kernel/internal/store"
)

// ReceiptTTL bounds how long after sealing a receipt can still buy a secret.
//
// The effect and the call it makes are the same act separated by a network
// round trip, so the honest window is seconds. It is not shorter because a
// gated effect may be delivered to a skill that is briefly busy, and it is not
// longer because the whole point is that a receipt is not a bearer token with a
// useful shelf life.
const ReceiptTTL = 90 * time.Second

// maxUses bounds how many times one receipt may be redeemed. One effect is one
// call, so this is 1 in the ordinary case; the allowance exists for a retry
// after a connection error, not for fan-out.
const maxUses = 3

// SecretRef matches a ${secret:name} reference in a projection header or
// connector spec. Deliberately its own syntax rather than reusing ${VAR}: the
// two resolve in different places at different times against different trust
// assumptions, and a reader has to be able to tell at a glance which one they
// are looking at.
var SecretRef = regexp.MustCompile(`\$\{secret:([A-Za-z0-9_.-]+)\}`)

// Broker holds this node's secrets and rations them against receipts.
type Broker struct {
	st   *store.Store
	aead cipher.AEAD

	mu   sync.Mutex
	used map[string]int // receipt -> redemptions so far
}

// Open prepares the broker, deriving its encryption key from the node's own
// Ed25519 private key.
//
// Deriving rather than generating a second key is deliberate. A separate key
// would be a second thing to back up and a second thing to lose, and losing it
// would strand every secret with no way back. Binding to the node identity
// means the secrets travel with the data directory exactly as the ledger does,
// and a data directory copied without its identity/ subdirectory yields
// ciphertext nobody can open — which is the correct outcome for a stolen
// database file.
//
// The domain separation in the digest keeps this key distinct from the signing
// key it comes from: the same bytes must never be usable both to sign a
// checkpoint and to decrypt a credential.
func Open(st *store.Store, keys *signing.Keypair) (*Broker, error) {
	if keys == nil {
		return nil, errors.New("the credential broker needs the node identity keypair")
	}
	sum := sha256.Sum256(append([]byte("aura-secret-box-v1:"), keys.Private...))
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		return nil, fmt.Errorf("derive secret box key: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("derive secret box: %w", err)
	}
	return &Broker{st: st, aead: aead, used: map[string]int{}}, nil
}

// Set stores a secret, replacing any previous value.
func (b *Broker) Set(name, value string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("a secret needs a name")
	}
	if !SecretRef.MatchString("${secret:" + name + "}") {
		return fmt.Errorf("secret name %q must be letters, digits, dot, dash or underscore — "+
			"it has to be referenceable as ${secret:%s}", name, name)
	}
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return err
	}
	// The name is authenticated as additional data, so a ciphertext cannot be
	// moved from one secret's row to another's — swapping the read-only token
	// into the row the write path reads would otherwise be an undetectable
	// privilege change made entirely with a text editor.
	sealed := b.aead.Seal(nonce, nonce, []byte(value), []byte(name))
	return b.st.SaveSecret(name, base64.StdEncoding.EncodeToString(sealed), time.Now().UnixMilli())
}

// Names lists the configured secrets. Names only, always: an endpoint that can
// enumerate values is one compromise away from being the whole problem.
func (b *Broker) Names() ([]string, error) { return b.st.SecretNames() }

// Delete removes a secret.
func (b *Broker) Delete(name string) (bool, error) { return b.st.DeleteSecret(name) }

// Has reports whether a secret is configured, without decrypting it.
func (b *Broker) Has(name string) bool {
	_, ok, err := b.st.Secret(name)
	return err == nil && ok
}

func (b *Broker) decrypt(name string) (string, error) {
	ct, ok, err := b.st.Secret(name)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("no secret named %q is configured on this node "+
			"(`aura secret set %s`)", name, name)
	}
	raw, err := base64.StdEncoding.DecodeString(ct)
	if err != nil || len(raw) < b.aead.NonceSize() {
		return "", fmt.Errorf("secret %q is corrupt", name)
	}
	nonce, body := raw[:b.aead.NonceSize()], raw[b.aead.NonceSize():]
	plain, err := b.aead.Open(nil, nonce, body, []byte(name))
	if err != nil {
		return "", fmt.Errorf("secret %q cannot be decrypted with this node's identity key — "+
			"the data directory and the identity/ subdirectory do not belong together", name)
	}
	return string(plain), nil
}

// Grant is what a caller gets back: the secrets it asked for, and the entry
// that bought them, so the caller can log what it spent.
type Grant struct {
	Values     map[string]string `json:"values"`
	Capability string            `json:"capability"`
	Seq        uint64            `json:"seq"`
}

// Resolve exchanges a receipt for secrets.
//
// Every check below is a real attack being refused, in the order that reveals
// the least to a caller who should not have got this far.
func (b *Broker) Resolve(receipt, capability string, names []string) (*Grant, error) {
	if receipt == "" {
		return nil, errors.New("a secret is only released against the receipt of a sealed effect; " +
			"this request carried none")
	}
	if len(names) == 0 {
		return nil, errors.New("no secrets requested")
	}

	// Not in the chain means invented, or lifted from another node's ledger.
	// Either way there is nothing here to authorize a credential, and the
	// reply says only that — not which of the two it was.
	raw, err := b.st.LedgerEntryByHash(receipt)
	if err != nil || len(raw) == 0 {
		return nil, errors.New("that receipt names no effect this node sealed")
	}
	var e ledger.Entry
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, fmt.Errorf("receipt names an unreadable entry: %w", err)
	}

	if e.Outcome != spec.OutcomeDelivered {
		return nil, fmt.Errorf("effect %d was %s, not delivered — a refused effect buys nothing",
			e.Seq, e.Outcome)
	}
	// Bind the credential to the capability the ledger says acted. Without
	// this, any skill that ever produced one sealed effect could redeem it for
	// the payment credential.
	if capability != "" && e.Capability != capability {
		return nil, fmt.Errorf("receipt was sealed for %q but %q is asking — "+
			"a receipt authorizes its own effect and no other", e.Capability, capability)
	}
	if age := time.Since(time.UnixMilli(e.TS)); age > ReceiptTTL {
		return nil, fmt.Errorf("receipt is %s old (limit %s) — a receipt authorizes the call that "+
			"follows its effect, not later ones", age.Truncate(time.Second), ReceiptTTL)
	}

	b.mu.Lock()
	n := b.used[receipt]
	if n >= maxUses {
		b.mu.Unlock()
		return nil, fmt.Errorf("receipt has already been redeemed %d times — one effect, one call", n)
	}
	b.used[receipt] = n + 1
	if len(b.used) > 4096 {
		b.sweepLocked()
	}
	b.mu.Unlock()

	values := make(map[string]string, len(names))
	for _, name := range names {
		v, err := b.decrypt(name)
		if err != nil {
			return nil, err
		}
		values[name] = v
	}
	return &Grant{Values: values, Capability: e.Capability, Seq: e.Seq}, nil
}

// sweepLocked drops redemption counters that can no longer matter, because the
// receipts they track have outlived the TTL anyway. Caller holds mu.
//
// Counting is bounded rather than persisted deliberately: the counter only has
// to outlive the TTL, and a node that restarts has also dropped every in-flight
// call the counters were protecting.
func (b *Broker) sweepLocked() {
	b.used = make(map[string]int, 64)
}

// ResolveIn substitutes every ${secret:name} in a set of strings, spending one
// receipt for the whole set. Returns the strings with values in place.
//
// This is the shape callers actually need — a header map, a query string — and
// putting it here rather than at each call site means the substitution and the
// receipt check can never drift apart into "resolved the secret, forgot to
// check the receipt".
func (b *Broker) ResolveIn(receipt, capability string, in map[string]string) (map[string]string, error) {
	var names []string
	seen := map[string]bool{}
	for _, v := range in {
		for _, m := range SecretRef.FindAllStringSubmatch(v, -1) {
			if !seen[m[1]] {
				seen[m[1]] = true
				names = append(names, m[1])
			}
		}
	}
	if len(names) == 0 {
		return in, nil
	}
	grant, err := b.Resolve(receipt, capability, names)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = SecretRef.ReplaceAllStringFunc(v, func(ref string) string {
			m := SecretRef.FindStringSubmatch(ref)
			return grant.Values[m[1]]
		})
	}
	return out, nil
}

// NeedsSecret reports whether any of these strings reference one, so a caller
// can tell "this operation needs a receipt" from "this one does not" without
// attempting a resolution it does not need.
func NeedsSecret(in map[string]string) bool {
	for _, v := range in {
		if SecretRef.MatchString(v) {
			return true
		}
	}
	return false
}
