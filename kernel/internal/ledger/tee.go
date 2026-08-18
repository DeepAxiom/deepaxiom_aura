package ledger

// C5 v1.1 — hardware evidence for an inference attestation.
//
// # The gap this closes, and the part of it that stays open
//
// C5 has always said the uncomfortable thing out loud: an attestation is an
// *assertion by a skill*, and a skill that lies about which model it ran
// produces a sealed, chained, signed record of a lie. Everything the kernel
// guarantees is about the claim's integrity after the fact — it cannot be
// altered, detached from its effects, or backdated — and nothing about whether
// it was true when made.
//
// A Trusted Execution Environment can narrow that, because a quote is signed by
// hardware the skill does not control. But a quote on its own proves almost
// nothing useful here. "This process runs in an enclave" is a claim about a
// process, and the question is about a *record*: did the enclave produce
// **this** declaration, naming **this** model, with **these** sampling
// parameters?
//
// # The binding rule
//
// Every TEE quote format carries a caller-supplied nonce, present so that a
// verifier can tell a fresh quote from a replayed one. That field is what makes
// this work:
//
//	nonce == sha256(the attestation record with `tee` removed)
//
// The skill computes its declaration, hashes it, asks the hardware to quote
// that hash, and attaches the result. A verifier recomputes the hash and
// compares. A quote taken from another machine, another moment, or another
// model's run has a different nonce and fails — so the evidence is *about* the
// declaration rather than merely accompanying it.
//
// This is the whole contribution of this file. It needs no vendor cooperation,
// works with every format that has a nonce, and is checkable offline by anyone.
//
// # Three levels, and why they are not one
//
// What a verifier can conclude depends on what it has, and collapsing that into
// a boolean would be the same error C5's package comment spends a page warning
// about:
//
//	EvidenceNone      no `tee` block. The claim is the skill's word. Today's state.
//	EvidenceBound     a quote is present and its nonce binds this exact record.
//	                  Checkable by anyone, offline, with no vendor roots. Proves
//	                  the evidence was produced for this declaration — not that
//	                  the hardware is genuine.
//	EvidenceVerified  the quote's signature chains to a vendor root the operator
//	                  supplied. Proves genuine hardware attested this record.
//
// `Bound` is deliberately not called "attested". It is a real improvement over
// nothing and it is not the guarantee an unqualified word would imply, and the
// gap between them is exactly where a reader would otherwise be misled.
//
// # What is implemented here, and what is refused
//
// Binding, structure and freshness are implemented for every declared format,
// because they need nothing but the document. Vendor chain verification needs
// roots, and this package will not embed roots it cannot itself validate or
// rotate — a stale root compiled into a binary is worse than no root, because
// it fails closed at the worst moment or open at the wrong one. An operator
// supplies them, and a format asked to reach `Verified` without them is
// **refused with an explanation rather than silently downgraded**. That is the
// same discipline internal/sandbox applies to its `microvm` backend, for the
// same reason: a backend that quietly degrades leaves an operator believing
// they have a guarantee they do not.

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// EvidenceLevel is what a verifier could actually conclude.
type EvidenceLevel string

const (
	// EvidenceNone is a self-declared attestation: no hardware evidence.
	EvidenceNone EvidenceLevel = "none"
	// EvidenceBound means a quote is present and cryptographically bound to
	// this record, with the hardware's own signature unchecked.
	EvidenceBound EvidenceLevel = "bound"
	// EvidenceVerified means the quote also chains to a trusted vendor root.
	EvidenceVerified EvidenceLevel = "verified"
)

// TEE evidence formats this contract knows. A format is a name plus the
// location of its nonce; everything else about it is opaque here.
const (
	// FormatNvidiaEAT is an NVIDIA GPU attestation result: an Entity
	// Attestation Token (a JWT) issued by a remote attestation service that
	// already checked the GPU's own evidence.
	FormatNvidiaEAT = "nvidia-eat/1"
	// FormatSEVSNP is a raw AMD SEV-SNP attestation report, whose REPORT_DATA
	// field carries the nonce.
	FormatSEVSNP = "amd-sev-snp/1"
	// FormatTDX is a raw Intel TDX quote, whose REPORTDATA carries the nonce.
	FormatTDX = "intel-tdx/1"
)

// EvidenceFormats is every format this binary can bind and structurally check.
// Ordered, and exported, so `aura verify` and the guide print the same list the
// code enforces rather than a second one that drifts.
var EvidenceFormats = []string{FormatNvidiaEAT, FormatSEVSNP, FormatTDX}

// MaxEvidenceBytes bounds a quote. Real quotes are a few kilobytes; an EAT with
// a certificate chain is larger. The cap exists because this rides inside an
// attestation that is itself capped, and an unbounded field inside a bounded
// record is a way to make the record unparseable later.
const MaxEvidenceBytes = 64 << 10

// EvidenceMaxAge bounds how stale a quote may be relative to the attestation it
// accompanies. A quote is taken *for* one inference, so the two are seconds
// apart in any honest implementation; a wide window would let one quote be
// reused across a day's worth of declarations that each hash differently — but
// see the binding rule, which already makes that impossible. This is a second
// line, not the first.
const EvidenceMaxAge = 10 * time.Minute

// Evidence is the `tee` block of a C5 attestation.
type Evidence struct {
	// Format names the quote's encoding. Required.
	Format string `json:"format"`
	// Nonce is the hex-encoded value the hardware was asked to quote. It MUST
	// equal BindingHash of the enclosing record with `tee` removed.
	Nonce string `json:"nonce"`
	// Quote is the raw evidence, base64. Opaque to this package except for the
	// structural checks each format defines.
	Quote string `json:"quote"`
	// TS is when the quote was taken, unix millis. Required, and checked
	// against the enclosing attestation's own timestamp.
	TS int64 `json:"ts"`
	// Verifier optionally names the remote attestation service that already
	// checked the raw hardware evidence — NVIDIA NRAS, Intel Trust Authority.
	// Informational: this package does not treat its presence as trust.
	Verifier string `json:"verifier,omitempty"`
}

// BindingHash is the value a quote's nonce must carry: sha256 over the
// attestation record with its `tee` member removed.
//
// Removing rather than zeroing, and re-marshalling through a sorted map rather
// than through the Attestation struct, because a skill and a verifier must
// agree on these bytes without agreeing on a Go type. A skill written in Python
// computes this from its own dict; the rule has to be expressible as "the
// object you were going to send, minus one key, with keys sorted".
//
// This is the one place in the codebase that canonicalises JSON, and it is
// unavoidable here: the hash has to be computable by a party that never saw the
// raw bytes, which is exactly the situation AttestationHash avoids by hashing
// what was sent. The two hashes answer different questions and both exist.
func BindingHash(raw []byte) (string, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return "", fmt.Errorf("attestation is not a JSON object: %w", err)
	}
	delete(obj, "tee")

	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var buf strings.Builder
	buf.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		buf.Write(kb)
		buf.WriteByte(':')
		buf.Write(obj[k])
	}
	buf.WriteByte('}')

	sum := sha256.Sum256([]byte(buf.String()))
	return hex.EncodeToString(sum[:]), nil
}

// TrustAnchors is what an operator supplies to reach EvidenceVerified: vendor
// roots, per format.
//
// Empty is the default and is not a degraded mode — it is the honest one. A
// node with no anchors reports `bound` for evidence it can bind and says so;
// it never reports `verified` on the strength of a root nobody chose.
type TrustAnchors struct {
	// Roots maps a format to PEM-encoded certificates the operator trusts for
	// it. A format absent here cannot reach EvidenceVerified on this node.
	Roots map[string][]byte
}

// EvidenceReport is what checking one `tee` block concluded.
type EvidenceReport struct {
	Level  EvidenceLevel `json:"level"`
	Format string        `json:"format,omitempty"`
	// Bound reports whether the nonce matched this record. False with a
	// non-empty Detail is a failure; false with no evidence at all is simply
	// EvidenceNone.
	Bound bool `json:"bound"`
	// Detail explains a level lower than the evidence appeared to claim.
	Detail string `json:"detail,omitempty"`
}

// CheckEvidence evaluates the hardware evidence on one attestation.
//
// `raw` is the attestation exactly as the skill sent it — the same bytes
// AttestationHash covers — because the binding is computed from them.
func CheckEvidence(raw []byte, anchors TrustAnchors) EvidenceReport {
	var probe struct {
		TEE json.RawMessage `json:"tee,omitempty"`
		TS  int64           `json:"ts,omitempty"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return EvidenceReport{Level: EvidenceNone, Detail: "attestation is not valid JSON"}
	}
	if len(probe.TEE) == 0 {
		// The overwhelmingly common case, and not a failure: the skill made a
		// claim and did not offer hardware evidence for it.
		return EvidenceReport{Level: EvidenceNone}
	}
	if len(probe.TEE) > MaxEvidenceBytes {
		return EvidenceReport{Level: EvidenceNone,
			Detail: fmt.Sprintf("evidence is %d bytes, over the %d-byte cap",
				len(probe.TEE), MaxEvidenceBytes)}
	}

	var ev Evidence
	if err := json.Unmarshal(probe.TEE, &ev); err != nil {
		return EvidenceReport{Level: EvidenceNone,
			Detail: "the tee block is not a recognised evidence object: " + err.Error()}
	}
	rep := EvidenceReport{Level: EvidenceNone, Format: ev.Format}

	if !oneOf(ev.Format, EvidenceFormats) {
		rep.Detail = fmt.Sprintf("evidence format %q is not one of %v — this binary cannot "+
			"check it, and reports what it verified rather than what it was handed",
			ev.Format, EvidenceFormats)
		return rep
	}
	if err := checkQuoteShape(ev); err != nil {
		rep.Detail = err.Error()
		return rep
	}

	// The binding. Everything above is hygiene; this is the claim.
	want, err := BindingHash(raw)
	if err != nil {
		rep.Detail = err.Error()
		return rep
	}
	if !strings.EqualFold(strings.TrimSpace(ev.Nonce), want) {
		rep.Detail = fmt.Sprintf("the quote's nonce does not bind this record "+
			"(quote says %s, this record hashes to %s) — the evidence is for some other "+
			"declaration, or the declaration was edited after the quote was taken",
			shortNonce(ev.Nonce), shortNonce(want))
		return rep
	}
	if probe.TS != 0 && ev.TS != 0 {
		if age := time.Duration(abs64(probe.TS-ev.TS)) * time.Millisecond; age > EvidenceMaxAge {
			rep.Detail = fmt.Sprintf("the quote was taken %s from the inference it attests "+
				"(limit %s)", age.Truncate(time.Second), EvidenceMaxAge)
			return rep
		}
	}

	rep.Bound = true
	rep.Level = EvidenceBound

	// Vendor chain verification, only where the operator supplied a root.
	root, ok := anchors.Roots[ev.Format]
	if !ok || len(root) == 0 {
		rep.Detail = fmt.Sprintf("bound to this record, but no trust anchor is configured for "+
			"%s — the hardware's own signature was not checked. Supply one with "+
			"`--tee-root %s=<pem>` to reach `verified`.", ev.Format, ev.Format)
		return rep
	}
	if err := verifyAgainstAnchor(ev, root); err != nil {
		rep.Detail = "bound to this record, but the vendor chain did not verify: " + err.Error()
		return rep
	}
	rep.Level = EvidenceVerified
	rep.Detail = ""
	return rep
}

// checkQuoteShape applies the per-format structural checks that need no roots.
//
// Shallow on purpose. Parsing a vendor's binary report layout in order to
// reject a malformed one — while still not verifying its signature — would be a
// large amount of code buying a small amount of confidence, and would have to
// track every vendor's format revisions to keep buying it. What is checked here
// is what a verifier can be sure of: the evidence decodes, is not empty, and
// has the coarse shape its format requires.
func checkQuoteShape(ev Evidence) error {
	if strings.TrimSpace(ev.Nonce) == "" {
		return fmt.Errorf("evidence carries no nonce, so it cannot be bound to anything")
	}
	if _, err := hex.DecodeString(ev.Nonce); err != nil {
		return fmt.Errorf("evidence nonce is not hex: %w", err)
	}
	quote, err := base64.StdEncoding.DecodeString(strings.TrimSpace(ev.Quote))
	if err != nil {
		return fmt.Errorf("evidence quote is not base64: %w", err)
	}
	if len(quote) == 0 {
		return fmt.Errorf("evidence carries no quote")
	}
	if ev.TS == 0 {
		return fmt.Errorf("evidence carries no timestamp")
	}

	switch ev.Format {
	case FormatNvidiaEAT:
		// An EAT is a JWT: three base64url segments separated by dots.
		if n := strings.Count(string(quote), "."); n != 2 {
			return fmt.Errorf("%s evidence is not a JWT (%d dot-separated segments, want 3)",
				ev.Format, n+1)
		}
	case FormatSEVSNP:
		// An SEV-SNP attestation report is a fixed 1184-byte structure.
		if len(quote) < 1184 {
			return fmt.Errorf("%s evidence is %d bytes; an SEV-SNP report is at least 1184",
				ev.Format, len(quote))
		}
	case FormatTDX:
		// A TDX quote's header plus report body is 632 bytes before the
		// signature data that follows it.
		if len(quote) < 632 {
			return fmt.Errorf("%s evidence is %d bytes; a TDX quote is at least 632",
				ev.Format, len(quote))
		}
	}
	return nil
}

// verifyAgainstAnchor is where vendor chain verification will live.
//
// Declared and refused, not stubbed. Returning "verified" here without doing
// the work would be the single most damaging line in this repository: every
// layer above reports what this function concludes, and an operator who
// supplied a root did so precisely because they wanted the check performed.
//
// Each format needs genuinely different machinery — an EAT is a JWT whose
// issuer key is fetched and pinned, an SEV-SNP report is verified against an
// AMD VCEK chain, a TDX quote against a PCK chain and a QE identity — and none
// of them is honestly implementable without hardware to test against. So this
// says so, names what is missing, and refuses.
func verifyAgainstAnchor(ev Evidence, root []byte) error {
	return fmt.Errorf(
		"chain verification for %s is not implemented in this build — the binding above "+
			"is checked and reported, but this node will not claim `verified` on the "+
			"strength of code that has never run against real hardware", ev.Format)
}

func shortNonce(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 12 {
		return s[:12] + "…"
	}
	if s == "" {
		return "(none)"
	}
	return s
}

func abs64(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}
