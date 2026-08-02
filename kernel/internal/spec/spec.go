// Code generated from spec/enums.yaml and spec/VERSION. DO NOT EDIT.
// Regenerate with: python scripts/gen_ssot.py

// Package spec holds the contract constants shared by every kernel
// package. It imports nothing, so any package may depend on it.
package spec

// Version is the product version (spec/VERSION).
const Version = "0.3.0"

// EffectType is the skill type whose edges act on the world, and so the
// one the authorization policy treats specially.
const EffectType = "motor"

// Skill types (C1).
const (
	TypeSensorial = "sensorial" // Perceives — turns the world into data.
	TypeCognitive = "cognitive" // Reasons — decides, plans, generates.
	TypeMotor     = "motor"     // Acts — produces effects in the world.
	TypeMemory    = "memory"    // Remembers — persists and retrieves context.
	TypeLogical   = "logical"   // Transforms/validates — deterministic data to data.
)

// SkillTypes is every value of C1 `type`, in declaration order.
var SkillTypes = []string{TypeSensorial, TypeCognitive, TypeMotor, TypeMemory, TypeLogical}

// Skill formats (C1).
const (
	FormatSource     = "source"     // Runs from source in a language runtime.
	FormatWasm       = "wasm"       // A sandboxed Wasm module (wazero, WASI command model; one ingress/egress port; filesystem and egress_http both enforced).
	FormatModel      = "model"      // A model artifact behind a driver.
	FormatProjection = "projection" // An existing system exposed as a skill.
)

// SkillFormats is every value of C1 `format`.
var SkillFormats = []string{FormatSource, FormatWasm, FormatModel, FormatProjection}

// Envelope kinds (C3).
const (
	KindData            = "data"             // Payload on a typed channel.
	KindDone            = "done"             // Logical stream close.
	KindError           = "error"            // Terminal failure of a chain.
	KindStatus          = "status"           // Explanatory, non-terminal.
	KindRegister        = "register"         // A skill's first frame, carrying its C1 manifest.
	KindConfirmRequest  = "confirm_request"  // The kernel asking a human to approve an effect.
	KindConfirmResponse = "confirm_response" // The human's answer.
	KindCancel          = "cancel"           // Abandon a causal chain.
	KindConfigUpdate    = "config_update"    // Kernel to skill — runtime config changed live.
)

// EnvelopeKinds is every value of C3 `kind`.
var EnvelopeKinds = []string{KindData, KindDone, KindError, KindStatus, KindRegister, KindConfirmRequest, KindConfirmResponse, KindCancel, KindConfigUpdate}

// QoS classes (C3 rule 4).
const (
	QoSReliable = "reliable" // Blocks the producer; nothing is lost.
	QoSRealtime = "realtime" // Never blocks; drops the oldest frame under pressure.
	QoSBulk     = "bulk"     // Named but unspecified; treated as reliable.
)

// QoSClasses is every value of a C2 edge's `qos`.
var QoSClasses = []string{QoSReliable, QoSRealtime, QoSBulk}

// Policy decisions (C4).
const (
	DecisionAllow = "allow" // The effect proceeds unsupervised.
	DecisionGate  = "gate"  // The effect waits for human approval.
	DecisionDeny  = "deny"  // The effect is refused and the session is not built.
)

// PolicyDecisions is every decision a node policy may reach.
var PolicyDecisions = []string{DecisionAllow, DecisionGate, DecisionDeny}

// Effect outcomes (C4): what became of an effect the ledger sealed.
const (
	OutcomeDelivered = "delivered" // The effect was authorized (directly, or after approval) and reached the skill.
	OutcomeDenied    = "denied"    // A human held the gate and refused the effect.
)

// EffectOutcomes is every outcome a sealed ledger entry may record.
var EffectOutcomes = []string{OutcomeDelivered, OutcomeDenied}

// Energy sources (C5): where an attestation's energy figure came from,
// ordered most to least trustworthy.
const (
	EnergyNvml         = "nvml"         // NVIDIA hardware counters, read from the GPU.
	EnergyRapl         = "rapl"         // Intel/AMD running-average power limit counters.
	EnergyPowermetrics = "powermetrics" // Apple Silicon power counters.
	EnergyEstimated    = "estimated"    // An analytical model; the attestation MUST state its basis.
)

// EnergySources is every source an attestation may cite for an energy
// figure. The source is mandatory whenever energy is reported: a measured
// joule and a modelled one differ by an order of magnitude in
// trustworthiness, and a bare number hides which it is.
var EnergySources = []string{EnergyNvml, EnergyRapl, EnergyPowermetrics, EnergyEstimated}

// CapabilityPattern matches a C1 `capability`: <type>.<function>[.<sub>].
const CapabilityPattern = `^(sensorial|cognitive|motor|memory|logical)(\.[a-z0-9_-]+)+$`
