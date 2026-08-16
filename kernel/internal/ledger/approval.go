package ledger

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// C4 v1.3 — who approved, provably.
//
// The contract opens by asking "who authorized this, and can it be proven
// afterward?" Until this file, the ledger answered the first half and only the
// first half: the entry cited the *policy* that authorized the class of effect
// and recorded that a gate was resolved, but the identity of the human who
// resolved it existed nowhere. An auditor reading a sealed entry could see
// that a person said yes and could not see which person, which is the half a
// compliance review is actually asking about — "a human approved" without
// "which human" is a log, not an audit trail.
//
// Worse, it was not merely absent but unprovable in principle. The node itself
// wrote the entry, so a node that wanted to claim an approval had happened
// could simply write one. Every guarantee in C4 is built on a signature the
// node cannot forge on someone else's behalf; approval had none.
//
// An Approval closes that. The operator signs a domain-separated statement
// binding *their* key to *this* envelope's resolution, the executor verifies it
// against the enrolled key before it acts on it, and the signature is sealed
// into the entry — where the entry hash, the chain, the Merkle tree and the
// checkpoint signature all commit to it exactly as they commit to every other
// field. Two properties follow, and neither existed before:
//
//   - **Non-repudiation.** The approver cannot later deny approving: only their
//     private key produces that signature.
//   - **The node cannot fabricate approvals.** It holds no operator private
//     key, so it cannot manufacture a yes that nobody gave — the one attack the
//     node's own signature can never defend against, because the node is the
//     thing being audited.

// ApprovalDomain is the prefix that separates these signatures from every
// other Ed25519 signature this codebase produces (checkpoints, packages,
// witness statements). Without it a checkpoint signature and an approval
// signature are both "64 bytes over some string", and a value that verifies in
// one context could be presented in another.
const ApprovalDomain = "aura-approval-v1"

// Approval is one operator's signed answer to one gate, as sealed into an entry.
//
// It carries the public key inline rather than only the operator id. The entry
// has to be verifiable from the database file alone with no kernel running —
// that is the whole premise of `aura verify` — and an id that has to be looked
// up in a mutable operators table would make verification depend on the
// current state of that table. An operator removed after the fact would
// retroactively invalidate history they legitimately approved. The key that
// signed it is a fact about the past; who is enrolled today is not.
type Approval struct {
	// Operator is the enrolled identity that answered, for humans reading the
	// entry. It is *not* what verification trusts — Pubkey is.
	Operator string `json:"operator"`
	// Pubkey is the base64 Ed25519 public key whose private half produced Sig.
	Pubkey string `json:"pubkey"`
	// Envelope is the id of the delivery the operator was actually shown — the
	// envelope the executor held at the gate.
	//
	// It is recorded rather than inferred because the entry's own Envelope
	// field is not the same id on both paths: a delivered effect seals the
	// outbound envelope the gate released (whose Cause is the held one), while
	// a denied effect seals the held envelope itself, there being no outbound
	// one. Verification must not have to know which path produced the entry, so
	// the approval names its own subject and is checkable in isolation.
	Envelope string `json:"envelope"`
	// Decision is what this operator actually said: "approve" or "deny". It is
	// signed, so a yes cannot be replayed as a no or the reverse.
	Decision string `json:"decision"`
	// TS is unix millis at signing, bound into the signature so an approval
	// carries its own timestamp rather than inheriting the node's.
	TS int64 `json:"ts"`
	// Sig is base64 Ed25519 over ApprovalPayload.
	Sig string `json:"sig"`
}

// Approval decisions. Deliberately not reusing spec.PolicyDecisions: that
// vocabulary is what a *policy* decided about a class of effect (allow, gate,
// deny), while this is what a *person* answered about one delivery. Collapsing
// them would let "the policy said gate" and "the human said deny" share a
// value and stop being distinguishable in the entry.
const (
	ApprovalApprove = "approve"
	ApprovalDeny    = "deny"
)

// ApprovalPayload is the exact byte string an operator signs.
//
// Everything that makes this approval *this* approval is inside it. Dropping
// any field would make a signature transplantable:
//
//   - node and session: an approval captured on one node cannot be replayed on
//     another, nor moved between sessions on the same node.
//   - envelope: the id of the delivery being held. Unique and unpredictable,
//     so it doubles as the nonce — there is nothing to replay a signature onto.
//   - decision: a signed "deny" cannot be presented as an "approve".
//   - ts: pins when, so an approval cannot be backdated after the fact without
//     invalidating itself.
func ApprovalPayload(node, session, envelope, decision string, ts int64) []byte {
	return []byte(fmt.Sprintf("%s:%s:%s:%s:%s:%d",
		ApprovalDomain, node, session, envelope, decision, ts))
}

// ErrNoApproval reports an absent approval, distinct from an invalid one. A
// caller enforcing policy has to tell "nobody signed" from "somebody signed
// wrongly": the first is an un-upgraded client, the second is an attack.
var ErrNoApproval = errors.New("no approval signature present")

// Verify checks this approval really binds the named delivery.
//
// It answers exactly one question — did the holder of Pubkey sign *this*
// resolution — and deliberately not "is that key allowed to approve here",
// which is enrollment's job (see internal/approver) and which depends on
// mutable state this function must not read. Splitting them is what lets
// `aura verify` re-check a five-year-old entry's cryptography without needing
// the operator roster as it stood five years ago.
func (a *Approval) Verify(node, session string) error {
	if a == nil {
		return ErrNoApproval
	}
	if a.Operator == "" {
		return errors.New("approval names no operator")
	}
	if a.Envelope == "" {
		return errors.New("approval names no delivery")
	}
	if a.Decision != ApprovalApprove && a.Decision != ApprovalDeny {
		return fmt.Errorf("approval decision %q must be %q or %q",
			a.Decision, ApprovalApprove, ApprovalDeny)
	}
	pub, err := base64.StdEncoding.DecodeString(strings.TrimSpace(a.Pubkey))
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("approval by %q carries an unusable public key", a.Operator)
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(a.Sig))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("approval by %q carries an unusable signature", a.Operator)
	}
	payload := ApprovalPayload(node, session, a.Envelope, a.Decision, a.TS)
	if !ed25519.Verify(ed25519.PublicKey(pub), payload, sig) {
		return fmt.Errorf("approval by %q does not verify against %s.%s — "+
			"the signature was made over a different node, session, delivery, "+
			"decision or time", a.Operator, session, a.Envelope)
	}
	return nil
}

// Binds reports whether this approval is the answer to the given delivery. The
// executor checks it against the envelope it actually held at the gate, so an
// approval collected for one delivery cannot be attached to another within the
// same session — the signature alone proves authorship, not relevance.
func (a *Approval) Binds(envelope string) bool {
	return a != nil && a.Envelope != "" && a.Envelope == envelope
}

// SignApproval produces an Approval for a delivery. Used by `aura approve` and
// by the control-plane UI's signing path; the kernel never calls it, because
// the kernel must never be able to produce one.
func SignApproval(operator string, priv ed25519.PrivateKey, node, session, envelope, decision string, ts int64) (*Approval, error) {
	if operator == "" {
		return nil, errors.New("an approval must name its operator")
	}
	if decision != ApprovalApprove && decision != ApprovalDeny {
		return nil, fmt.Errorf("approval decision %q must be %q or %q",
			decision, ApprovalApprove, ApprovalDeny)
	}
	if len(priv) != ed25519.PrivateKeySize {
		return nil, errors.New("approval signing needs an ed25519 private key")
	}
	if envelope == "" {
		return nil, errors.New("an approval must name the delivery it answers")
	}
	sig := ed25519.Sign(priv, ApprovalPayload(node, session, envelope, decision, ts))
	return &Approval{
		Operator: operator,
		Pubkey:   base64.StdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey)),
		Envelope: envelope,
		Decision: decision,
		TS:       ts,
		Sig:      base64.StdEncoding.EncodeToString(sig),
	}, nil
}
