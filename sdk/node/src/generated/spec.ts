// Code generated from spec/enums.yaml and spec/VERSION. DO NOT EDIT.
// Regenerate with: python scripts/gen_ssot.py

export const VERSION = "0.3.0";

/** The skill type whose edges act on the world. */
export const EFFECT_TYPE = "motor";

/** C1 `type` — the five kinds of skill. */
export type SkillType = "sensorial" | "cognitive" | "motor" | "memory" | "logical";
export const SKILL_TYPES: readonly SkillType[] = ["sensorial", "cognitive", "motor", "memory", "logical"];

/** C1 `format` — how a skill is packaged. */
export type SkillFormat = "source" | "wasm" | "model" | "projection";
export const SKILL_FORMATS: readonly SkillFormat[] = ["source", "wasm", "model", "projection"];

/** C3 `kind` — every envelope kind. */
export type EnvelopeKind = "data" | "done" | "error" | "status" | "register" | "confirm_request" | "confirm_response" | "cancel" | "config_update";
export const ENVELOPE_KINDS: readonly EnvelopeKind[] = ["data", "done", "error", "status", "register", "confirm_request", "confirm_response", "cancel", "config_update"];

/** C3 rule 4 — per-edge delivery classes. */
export type QoSClass = "reliable" | "realtime" | "bulk";
export const QOS_CLASSES: readonly QoSClass[] = ["reliable", "realtime", "bulk"];

/** C2 — what an edge may declare about approval. */
export type Gate = "human-approval" | "none";
export const GATES: readonly Gate[] = ["human-approval", "none"];

/** C4 — what a node policy may decide about an effect. */
export type PolicyDecision = "allow" | "gate" | "deny";
export const POLICY_DECISIONS: readonly PolicyDecision[] = ["allow", "gate", "deny"];

/** C4 — what became of an effect the ledger sealed. */
export type EffectOutcome = "delivered" | "denied";
export const EFFECT_OUTCOMES: readonly EffectOutcome[] = ["delivered", "denied"];

/** C5 — where an attestation's energy figure came from, most to least trustworthy. */
export type EnergySource = "nvml" | "rapl" | "powermetrics" | "estimated";
export const ENERGY_SOURCES: readonly EnergySource[] = ["nvml", "rapl", "powermetrics", "estimated"];
