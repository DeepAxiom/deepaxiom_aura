package ledger

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
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
//
// C4 v1.7 adds the third property, and it is the one the first two leave open.
// A signature over an envelope id proves that a named person answered *this*
// delivery; it says nothing about what they were looking at when they did, so
// a surface that rendered a reassuring summary over an effect that did
// something else produced an approval indistinguishable from an honest one.
// Approval.Context closes it: the approver signs digests of the artifacts they
// were shown, so the record commits to the *material* as well as to the act.
//
//   - **Consent, not a click.** What the approver saw is inside their own
//     signature, so an auditor holding the artifact can prove it is the one
//     that was approved — and, just as important, prove when it is not.
//
// The node cannot supply that context, and the design depends on it not being
// able to: a digest the node chose would be a digest of whatever the node
// wished it had shown. It comes from the side that did the rendering, which is
// the operator's side of the trust boundary — see the `--shown` flags on
// `aura approve`.

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
	// Context is what the approver was shown, as labelled digests (C4 v1.7,
	// additive). Empty on an approval that binds only the delivery, which is
	// every approval made before this field existed.
	//
	// Everything else in this struct answers *which effect* was approved. This
	// answers *what the person was looking at when they approved it*, and the
	// two are not the same question: a gate resolved by id proves a click on an
	// envelope, and an operator who clicked it having been shown a summary that
	// did not match the effect has still, on the evidence, approved the effect.
	//
	// Digests and never values — see ContextEntry for why that is a rule and
	// not a size optimisation.
	Context []ContextEntry `json:"context,omitempty"`
	// Sig is base64 Ed25519 over ApprovalPayload.
	Sig string `json:"sig"`
}

// ContextEntry is one artifact the approver was shown, named and hashed.
//
// The label says what the artifact *was* — "screen", "invoice", "diff",
// "certificate" — and the digest pins which one. A verifier five years later
// reads `screen=sha256:4b8c…` and knows both what to go and find and how to
// tell whether they found it; a bare unlabelled hash would tell them only that
// the approver committed to something.
//
// The label vocabulary is deliberately open. What a person has to be shown
// before they may authorise an act is a question each deployment answers for
// itself — a clinical note, a wire transfer's beneficiary, the diff about to
// be merged — and a fixed enumeration here would be this contract guessing at
// domains it does not know. What the contract does fix is the shape, so that
// two deployments' entries can be read by one tool.
//
// **The value is always a digest, never the artifact.** The ledger already
// refuses to store payloads (Entry.PayloadSHA256: "a payload carrying personal
// data must not become permanently undeletable"), and a context field that
// accepted free text would be a way around that rule — the very artifacts most
// worth binding are the ones most likely to carry personal data. A digest binds
// them without republishing them.
type ContextEntry struct {
	// Label names the kind of artifact. Lowercase, no separators — see
	// CanonicalContext for why the charset is load-bearing.
	Label string `json:"label"`
	// Digest is "<algorithm>:<lowercase hex>", e.g. "sha256:4b8c…".
	Digest string `json:"digest"`
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

// MaxContextEntries bounds how much an approval may commit to.
//
// Not a storage limit — eight digests are a few hundred bytes. It is a limit on how much
// a person can be said to have examined in one decision: a client that binds
// forty artifacts to one click is describing a review that did not happen, and
// the field would be documenting diligence rather than evidencing it.
const MaxContextEntries = 8

// The charsets are the contract, not a validation nicety. Neither a label nor a
// digest can contain "=" or a newline, which is precisely what makes the
// canonical form below parse back to exactly one list of pairs — see
// CanonicalContext.
//
// The 64-character floor on the hex half is the other load-bearing constraint,
// and it is about collisions rather than tidiness. This field's whole claim is
// that the artifact the signer held is the artifact an auditor now holds, so an
// attacker who can produce two documents with one digest can show a person the
// harmless one and file the other. 64 hex characters is 256 bits, which every
// current algorithm meets and which the broken ones do not.
var (
	contextLabelRe  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	contextDigestRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,15}:[0-9a-f]{64,128}$`)
)

// CanonicalContext renders a context as the one string its digest is taken over.
//
//	label "=" digest, sorted by label, joined by "\n"
//
// Sorted, so the array order a client happened to send is not part of what was
// signed. Injective, so two different contexts cannot produce one string: a
// label cannot contain "=" and neither field can contain a newline, so the
// split back into pairs is unambiguous. That property is the whole reason for
// the charset restrictions, and it is what lets a *structure* be bound by a
// payload that is deliberately a flat string (see ApprovalPayload).
//
// Duplicate labels are refused rather than resolved. "The screen was this, and
// also that" is not a statement with a meaning, and picking either one would be
// this function deciding what a person approved.
func CanonicalContext(entries []ContextEntry) (string, error) {
	if len(entries) == 0 {
		return "", nil
	}
	if len(entries) > MaxContextEntries {
		return "", fmt.Errorf("an approval may bind at most %d artifacts, got %d",
			MaxContextEntries, len(entries))
	}
	sorted := make([]ContextEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Label < sorted[j].Label })

	lines := make([]string, 0, len(sorted))
	for i, e := range sorted {
		if !contextLabelRe.MatchString(e.Label) {
			return "", fmt.Errorf("%q is not a usable context label — lowercase letters, "+
				"digits and underscore, starting with a letter", e.Label)
		}
		if !contextDigestRe.MatchString(e.Digest) {
			return "", fmt.Errorf("context %q carries %q, which is not an <algorithm>:<lowercase hex> "+
				"digest of at least 256 bits — an approval binds the hash of what was shown, "+
				"never the thing itself", e.Label, e.Digest)
		}
		if i > 0 && sorted[i-1].Label == e.Label {
			return "", fmt.Errorf("context label %q appears twice — an approval names each "+
				"artifact once, so that it cannot say the same thing was two things", e.Label)
		}
		lines = append(lines, e.Label+"="+e.Digest)
	}
	return strings.Join(lines, "\n"), nil
}

// ContextDigest reduces a context to the single value the signed payload carries.
//
// Empty string for an empty context, which is how the payload stays byte for
// byte what it was before this field existed — every approval signed by an
// older client still verifies, and every approval an older verifier can read
// still means what it meant.
func ContextDigest(entries []ContextEntry) (string, error) {
	canonical, err := CanonicalContext(entries)
	if err != nil || canonical == "" {
		return "", err
	}
	sum := sha256.Sum256([]byte(canonical))
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

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
//   - context: what the approver was shown, when they bound anything (C4 v1.7).
//
// The context arrives as one digest rather than as its entries, because this
// payload is a flat string on purpose: agreeing on the canonical encoding of a
// structure is a well-known source of both interoperability failure and
// signature bypass. Reducing the structure to a digest under the rules of
// CanonicalContext keeps the string flat and keeps the reduction injective.
//
// A context-free approval produces exactly the pre-v1.7 string, so this change
// is additive in the only sense that matters: no signature made before it stops
// verifying. It is also not strippable. An attacker who removes the context
// field leaves a signature made over the longer string, which then fails; one
// who adds a context to an approval that had none does the same in reverse.
// Both directions fail closed, which is the direction a gate has to fail.
func ApprovalPayload(node, session, envelope, decision string, ts int64, shown []ContextEntry) ([]byte, error) {
	base := fmt.Sprintf("%s:%s:%s:%s:%s:%d",
		ApprovalDomain, node, session, envelope, decision, ts)
	digest, err := ContextDigest(shown)
	if err != nil {
		return nil, err
	}
	if digest == "" {
		return []byte(base), nil
	}
	return []byte(base + ":" + digest), nil
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
	// Canonical order in place, so what gets sealed hashes over the *set* of
	// artifacts and not over the order a client happened to send them in —
	// the same reason Entry.Inference is sorted before it is sealed.
	sort.Slice(a.Context, func(i, j int) bool { return a.Context[i].Label < a.Context[j].Label })

	payload, err := ApprovalPayload(node, session, a.Envelope, a.Decision, a.TS, a.Context)
	if err != nil {
		return fmt.Errorf("approval by %q carries an unusable context: %w", a.Operator, err)
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), payload, sig) {
		shown := "the signature was made over a different node, session, delivery, decision or time"
		if len(a.Context) > 0 {
			shown += ", or over different material than the context now attached to it"
		} else {
			// The likeliest cause once v1.7 clients exist: a context was signed
			// and then dropped in transit or by a relay that did not know the
			// field. Naming it saves an hour of looking at the wrong things.
			shown += ", or was made over a context this copy of the approval no longer carries"
		}
		return fmt.Errorf("approval by %q does not verify against %s.%s — %s",
			a.Operator, session, a.Envelope, shown)
	}
	return nil
}

// Shows reports whether this approval binds a digest under label.
func (a *Approval) Shows(label string) bool {
	if a == nil {
		return false
	}
	for _, e := range a.Context {
		if e.Label == label {
			return true
		}
	}
	return false
}

// Unbound returns the required labels this approval does not bind, in the order
// they were required, so a refusal can name what is missing rather than saying
// only that something is.
func (a *Approval) Unbound(required []string) []string {
	var missing []string
	for _, label := range required {
		if !a.Shows(label) {
			missing = append(missing, label)
		}
	}
	return missing
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
func SignApproval(operator string, priv ed25519.PrivateKey, node, session, envelope, decision string, ts int64, shown []ContextEntry) (*Approval, error) {
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
	shown = append([]ContextEntry(nil), shown...)
	sort.Slice(shown, func(i, j int) bool { return shown[i].Label < shown[j].Label })
	payload, err := ApprovalPayload(node, session, envelope, decision, ts, shown)
	if err != nil {
		return nil, err
	}
	sig := ed25519.Sign(priv, payload)
	return &Approval{
		Operator: operator,
		Pubkey:   base64.StdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey)),
		Envelope: envelope,
		Decision: decision,
		TS:       ts,
		Context:  shown,
		Sig:      base64.StdEncoding.EncodeToString(sig),
	}, nil
}
