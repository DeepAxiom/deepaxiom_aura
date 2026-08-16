// Package approver is the node's roster of humans who may answer a gate.
//
// It exists because C4 v1.3 splits one question into two, and the split is the
// whole design:
//
//   - **Did this operator really sign this resolution?** Pure cryptography over
//     the sealed entry, answerable forever from the database file alone with no
//     roster and no kernel. That is ledger.Approval.Verify.
//   - **Is that operator allowed to approve on this node right now?** Mutable
//     state, answerable only against the live roster, and meaningful only at
//     the moment the gate is answered. That is this package.
//
// Collapsing them would make history depend on the present: removing an
// employee would retroactively invalidate every effect they legitimately
// approved, and an audit trail that changes when the org chart changes is not
// one. So enrollment is checked when the answer arrives and never again, and
// the entry records the key rather than the roster row.
//
// The node stores public keys only. It never has an operator's private key,
// which is what makes an approval something the node cannot manufacture about
// itself — the one claim a node's own signature can never establish, because
// the node is the party under audit.
package approver

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"strings"
	"sync"
	"time"

	"aura/kernel/internal/ledger"
	"aura/kernel/internal/store"
)

// Registry is the enrolled roster, cached in memory over the store.
//
// The cache is not an optimisation for throughput — gates are rare by
// construction — but for availability: resolving a gate must not fail because
// a disk read failed at that moment, having already made a human wait.
type Registry struct {
	st *store.Store

	mu  sync.RWMutex
	ops map[string]store.OperatorRow
}

// Load reads the roster.
func Load(st *store.Store) (*Registry, error) {
	r := &Registry{st: st, ops: map[string]store.OperatorRow{}}
	if err := r.Reload(); err != nil {
		return nil, err
	}
	return r, nil
}

// Reload re-reads the roster from storage.
func (r *Registry) Reload() error {
	rows, err := r.st.Operators()
	if err != nil {
		return fmt.Errorf("read operator roster: %w", err)
	}
	next := make(map[string]store.OperatorRow, len(rows))
	for _, o := range rows {
		next[o.ID] = o
	}
	r.mu.Lock()
	r.ops = next
	r.mu.Unlock()
	return nil
}

// Enroll adds or re-adds an operator by public key.
func (r *Registry) Enroll(id, pubkeyB64, name string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("an operator needs an id")
	}
	pubkeyB64 = strings.TrimSpace(pubkeyB64)
	raw, err := base64.StdEncoding.DecodeString(pubkeyB64)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return fmt.Errorf("operator %q: %q is not a base64 ed25519 public key", id, pubkeyB64)
	}
	// A key enrolled under two ids would make an approval ambiguous about who
	// gave it, which is the one thing this whole mechanism exists to pin down.
	r.mu.RLock()
	for _, o := range r.ops {
		if o.Pubkey == pubkeyB64 && o.ID != id && o.Revoked == 0 {
			r.mu.RUnlock()
			return fmt.Errorf("that public key is already enrolled as %q — "+
				"one key, one operator, or an approval cannot say who gave it", o.ID)
		}
	}
	r.mu.RUnlock()

	if err := r.st.SaveOperator(store.OperatorRow{
		ID: id, Pubkey: pubkeyB64, Name: name, Added: time.Now().UnixMilli(),
	}); err != nil {
		return err
	}
	return r.Reload()
}

// Revoke stops an operator approving anything further. Their past approvals
// remain valid and remain verifiable — see the package comment.
func (r *Registry) Revoke(id string) (bool, error) {
	ok, err := r.st.RevokeOperator(id, time.Now().UnixMilli())
	if err != nil || !ok {
		return ok, err
	}
	return true, r.Reload()
}

// List returns the roster, by id.
func (r *Registry) List() []store.OperatorRow {
	rows, err := r.st.Operators()
	if err != nil {
		return nil
	}
	return rows
}

// Empty reports whether nobody is enrolled. A node with an empty roster cannot
// require signed approval — every gate would be unanswerable — so callers
// enforcing that policy check this first and say so plainly at startup rather
// than deadlocking the first gated effect.
func (r *Registry) Empty() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, o := range r.ops {
		if o.Revoked == 0 {
			return false
		}
	}
	return true
}

// Check validates one approval end to end: the signature binds this node,
// session and delivery, and the key that made it belongs to an operator
// enrolled and not revoked right now.
//
// Order matters. Cryptography first, roster second, so an unenrolled key never
// gets a different error message depending on whether its signature was also
// bad — the failure a caller sees should not be an oracle for which half was
// wrong.
func (r *Registry) Check(a *ledger.Approval, node, session, envelope string) error {
	if a == nil {
		return ledger.ErrNoApproval
	}
	if !a.Binds(envelope) {
		return fmt.Errorf("approval by %q answers delivery %s, not %s — "+
			"an approval is an answer to one specific effect",
			a.Operator, a.Envelope, envelope)
	}
	if err := a.Verify(node, session); err != nil {
		return err
	}

	r.mu.RLock()
	o, known := r.ops[a.Operator]
	r.mu.RUnlock()
	if !known {
		return fmt.Errorf("%q is not an enrolled operator on this node", a.Operator)
	}
	if o.Revoked != 0 {
		return fmt.Errorf("operator %q was revoked at %s and may no longer approve",
			a.Operator, time.UnixMilli(o.Revoked).Format(time.RFC3339))
	}
	// The signature proved someone holding *a* key signed it; this proves it
	// was the key this node enrolled under that name. Without this check an
	// attacker approves as "alice" using their own keypair, and the entry reads
	// as alice's approval forever.
	if o.Pubkey != strings.TrimSpace(a.Pubkey) {
		return fmt.Errorf("approval claims to be %q but is signed by a key that node has not enrolled for them",
			a.Operator)
	}
	return nil
}
