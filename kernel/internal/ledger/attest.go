package ledger

// C5 — the inference attestation.
//
// C4 answers "what acted on the world, and who authorized it". It does not
// answer the question that comes immediately after in any real incident
// review: **what produced the judgement that led to the act?** A sealed entry
// saying `motor.erp.invoice.create` was delivered under policy 7c1e… is
// evidence about the effect and says nothing about the model whose output
// argued for it — which model, which weights, which quantization, which
// sampling parameters, which seed. Those are exactly the variables that decide
// whether a bad outcome was a policy failure, a prompt failure, or a silently
// swapped model.
//
// An attestation is one skill's assertion about how it produced an output. The
// kernel content-addresses it, stores it once, and binds its hash into every
// effect that the attested output causally led to. What that buys:
//
//   - "This invoice was created on the strength of a reply from
//     Qwen2.5-1.5B-Instruct at Q4_K_M, revision f1d2…, temperature 0.7,
//     seed 42" becomes a checkable claim rather than a story.
//   - Swapping a model under a running graph changes the attestation hash,
//     which changes every subsequent ledger entry, which breaks the chain
//     unless it is sealed honestly. A silent model swap stops being silent.
//   - Replay (`aura replay`) can compare not just outputs but the inference
//     conditions that produced them, which is what makes a diff meaningful
//     when sampling is stochastic.
//
// ## What this proves, and what it does not
//
// Stated plainly, because the difference decides whether a reader is misled:
// **an attestation is an assertion by the skill, cryptographically bound to
// the effects it led to and to the moment it was made — it is not proof that
// the skill told the truth.** A skill that lies about which model it ran
// produces a sealed, chained, signed record of a lie. What the kernel
// guarantees is narrower and still useful: the assertion cannot be altered
// afterwards, cannot be detached from the effects it caused, and cannot be
// backdated. Making the assertion itself trustworthy needs hardware
// attestation (a TEE quote binding the measurement of the process that ran
// the model), which C5 leaves room for in `tee` and the roadmap tracks
// separately.
//
// The honest one-line summary, which the README and `aura receipt` both use:
// *who claimed what, when, and what it caused — verifiable; whether the claim
// was true — only as far as you trust the skill that made it.*

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"aura/kernel/internal/spec"
	"aura/kernel/internal/store"
)

// MaxAttestationBytes bounds one attestation record. Attestations are
// metadata about an inference, never its content: a few hundred bytes of
// model identity and sampling parameters. The cap exists so a skill cannot
// use the field as an unbounded, permanently-retained side channel into the
// node's storage — the same reasoning that keeps payloads out of the ledger.
const MaxAttestationBytes = 8 << 10

// Attestation is the parsed view of what a skill asserted. The raw bytes are
// what gets hashed and stored; this struct exists so the kernel can validate
// the shape, and so `aura bom` and `aura receipt` can read it without
// re-deriving the schema.
type Attestation struct {
	// Engine identifies the runtime that executed the inference —
	// "llama.cpp", "faster-whisper", "piper", "openai". Mandatory: an
	// attestation that does not say what ran it is not evidence of anything.
	Engine        string `json:"engine"`
	EngineVersion string `json:"engine_version,omitempty"`

	// Model is the model's identity in whatever namespace the engine uses.
	// For anything from the Hugging Face Hub this is the repo id
	// ("Qwen/Qwen2.5-1.5B-Instruct-GGUF"), which is what makes ModelRevision
	// meaningful and what lets `aura bom` emit a resolvable component.
	Model string `json:"model"`
	// ModelRevision is the immutable revision the weights came from — an HF
	// commit sha. A repo id alone names a moving target; the pair names one
	// specific set of bytes, which is the only version of the claim worth
	// sealing.
	ModelRevision string `json:"model_revision,omitempty"`
	// ModelFile and ModelSHA256 pin the artifact itself, for the local case
	// where the weights are a file on disk rather than a Hub reference.
	ModelFile   string `json:"model_file,omitempty"`
	ModelSHA256 string `json:"model_sha256,omitempty"`
	// Quantization ("Q4_K_M", "fp16", "int8") is separated from the file name
	// because it changes the output distribution and is the single most
	// common undeclared difference between "the same model" in two places.
	Quantization string `json:"quantization,omitempty"`

	// Params carries the sampling configuration — temperature, top_p, top_k,
	// max_tokens, and crucially `seed`. Left as raw JSON so the kernel does
	// not have to model every engine's knobs, and so the hash covers exactly
	// what the skill sent.
	Params json.RawMessage `json:"params,omitempty"`

	// PromptSHA256 binds the attestation to the input it was made about,
	// without storing the prompt: the same reason the ledger stores
	// payload_sha256 and never the payload. A prompt is user data and must
	// not become permanently undeletable.
	PromptSHA256 string `json:"prompt_sha256,omitempty"`
	// OutputSHA256 does the same for what came back.
	OutputSHA256 string `json:"output_sha256,omitempty"`

	// TEE is reserved for a hardware attestation quote (Intel TDX, AMD
	// SEV-SNP, NVIDIA CC) binding this record to a measured enclave. Absent
	// today on every node; declared now so its later arrival is additive
	// rather than a new major. Its absence is what makes everything above an
	// assertion rather than a proof — see the package comment.
	TEE json.RawMessage `json:"tee,omitempty"`

	// Energy is what the skill measured or estimated for this inference.
	// Optional, and honest about which of the two it is — see EnergyReport.
	Energy *EnergyReport `json:"energy,omitempty"`
}

// EnergyReport carries the cost of an inference in millijoules, together with
// how the number was arrived at.
//
// `Source` is mandatory and not decorative. A measured joule and a modelled
// joule differ by an order of magnitude in trustworthiness, and a field that
// reported only the number would quietly launder an estimate into a
// measurement the first time anyone put it in a sustainability report. A
// consumer that only accepts hardware counters can filter on this; one that
// accepts estimates knows what it is accepting.
type EnergyReport struct {
	Millijoules float64 `json:"millijoules"`
	// Source is one of the EnergySources values: what produced this number.
	Source string `json:"source"`
	// Basis explains an estimate's inputs in one line ("14.2s wall x 45W
	// declared TDP"), so a reader can judge it instead of trusting it. Empty
	// for a hardware measurement, where the counter is the basis.
	Basis string `json:"basis,omitempty"`
}

// EnergySources are the ways an energy figure can come to exist, ordered from
// most to least trustworthy. Generated from spec/enums.yaml, like every other
// contract enumeration — a list duplicated here would be a list that drifts
// from the Python SDK's copy without anything failing.
var EnergySources = spec.EnergySources

// Hash content-addresses an attestation: sha256 over exactly the bytes the
// skill sent.
//
// Over the raw bytes, not over a re-marshalling of the parsed struct, for the
// same reason LeafHash hashes stored bytes: any canonicalization step is a
// place where the sealer's encoder and a verifier's encoder can differ, and
// Params/TEE are free-form JSON where that risk is real rather than
// theoretical.
func AttestationHash(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ParseAttestation validates a skill's assertion and returns both the parsed
// view and the canonical hash of the raw bytes.
//
// Validation is deliberately thin — engine and model must be present and the
// record must fit — because the kernel is not in a position to judge whether
// a claim is *true*, only whether it is well-formed enough to be evidence.
// Rejecting a malformed one matters anyway: an attestation that cannot be
// parsed later is a hash bound into the chain pointing at nothing readable.
func ParseAttestation(raw []byte) (Attestation, string, error) {
	var a Attestation
	if len(raw) == 0 {
		return a, "", fmt.Errorf("attestation is empty")
	}
	if len(raw) > MaxAttestationBytes {
		return a, "", fmt.Errorf(
			"attestation is %d bytes, over the %d-byte cap — attestations carry model "+
				"identity and sampling parameters, never prompts or outputs",
			len(raw), MaxAttestationBytes)
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, "", fmt.Errorf("attestation is not valid JSON: %w", err)
	}
	if strings.TrimSpace(a.Engine) == "" {
		return a, "", fmt.Errorf("attestation declares no engine")
	}
	if strings.TrimSpace(a.Model) == "" {
		return a, "", fmt.Errorf("attestation declares no model")
	}
	if a.Energy != nil {
		if a.Energy.Source == "" {
			return a, "", fmt.Errorf(
				"attestation reports energy with no source — a number whose provenance is " +
					"unstated cannot be told apart from a guess")
		}
		if !oneOf(a.Energy.Source, EnergySources) {
			return a, "", fmt.Errorf("attestation energy source %q is not one of %v",
				a.Energy.Source, EnergySources)
		}
	}
	return a, AttestationHash(raw), nil
}

// RecordAttestation stores one attestation, content-addressed. Storing the
// same record twice is a no-op: identical inference conditions across a
// thousand deliveries are one row, which is what keeps a busy node's storage
// proportional to its *distinct* configurations rather than to its traffic.
func RecordAttestation(st *store.Store, hash string, raw []byte) error {
	return st.SaveAttestation(hash, raw, time.Now().UnixMilli())
}

// LoadAttestation reads one back by hash and re-verifies the content address
// before returning it.
//
// Re-hashing on read is not paranoia: the attestation table is outside the
// Merkle tree (it holds records, not ledger entries), so its integrity rests
// entirely on the hashes the sealed entries cite. Checking here is what makes
// that binding load-bearing rather than decorative — an edited attestation
// record fails to load rather than quietly returning different claims than
// the ones the chain committed to.
func LoadAttestation(st *store.Store, hash string) (Attestation, error) {
	raw, err := st.Attestation(hash)
	if err != nil {
		return Attestation{}, err
	}
	if got := AttestationHash(raw); got != hash {
		return Attestation{}, fmt.Errorf(
			"attestation %s does not match its content address (recomputes to %s) — "+
				"the stored record was altered after the ledger cited it", hash, got)
	}
	a, _, err := ParseAttestation(raw)
	return a, err
}
