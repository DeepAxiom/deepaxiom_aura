/** C3 envelope — the unit of everything that flows through the system. */
export interface Envelope {
  v: string;
  id: string;
  cause_id?: string;
  session?: string;
  node?: string;
  port?: string;
  seq?: number;
  idem?: string;
  schema?: string;
  kind:
    | "data"
    | "done"
    | "error"
    | "status"
    | "register"
    | "confirm_request"
    | "confirm_response"
    | "cancel"
    | "config_update";
  payload?: unknown;
  /** C4: hash of the ledger entry that sealed this delivery into a motor Expertise. */
  receipt?: string;
  /** C5: what the emitting Expertise asserts about the inference, incl. `usage`. */
  attest?: { engine?: string; model?: string; usage?: Record<string, number>; [k: string]: unknown };
  /** C3 v1.7: W3C traceparent carried along the causal chain. */
  trace?: string;
  deadline?: number;
  priority?: number;
  speculative?: boolean;
}

export type SkillType = "sensorial" | "cognitive" | "motor" | "memory" | "logical";

export interface Port {
  name: string;
  schema: string;
}

export type ConfigParamType = "string" | "int" | "float" | "bool" | "enum";

/** C1 config param declaration — what CAN be tuned, not its current value. */
export interface ConfigParam {
  key: string;
  type: ConfigParamType;
  default: unknown;
  min?: number;
  max?: number;
  options?: string[];
  description?: string;
  restart_required?: boolean;
}

/** C1 manifest as returned by GET /v1/skills. */
export interface SkillManifest {
  id: string;
  version: string;
  protocol: string;
  name: string;
  description: string;
  capability: string;
  type: SkillType;
  format: string;
  ports: { ingress: Port[]; egress: Port[] };
  requirements?: Record<string, unknown>;
  permissions?: Record<string, unknown>;
  config?: ConfigParam[];
}

/** GET/PUT /v1/skills/config?id=<id> response. */
export interface SkillConfig {
  id: string;
  schema: ConfigParam[];
  values: Record<string, unknown>;
  pushed_live?: boolean;
}

/** C2 graph IR. */
export interface GraphIR {
  ir: string;
  graph_id: string;
  origin: { kind: "declared" | "planner"; skill?: string; cause?: string };
  nodes: { ref: string; resolve?: string; use?: string; constraints?: Record<string, unknown> }[];
  edges: { from: string; to: string; gate?: string; qos?: string }[];
  waves?: string[][];
}

/** std/plan@1 — what the planner emits. */
export interface Plan {
  reasoning: string;
  graph: GraphIR;
  inputs: { port: string; schema: string; payload: unknown }[];
}

export type OpMode = "live" | "dry-run" | "disabled";

export interface ProjectionOp {
  op_id: string;
  method: string;
  path: string;
  summary: string;
  write: boolean;
  mode: OpMode;
}

export interface Projection {
  name: string;
  base_url: string;
  headers?: Record<string, string>;
  ops: ProjectionOp[];
}

/**
 * GET /v1/sessions — one row per session, newest first.
 *
 * `ended` is 0 (omitted) while the session is still running, which is what
 * makes this list the answer to "what is live right now" as well as "what has
 * run": a graph with an unended session is a graph currently connected.
 */
export interface SessionMeta {
  session_id: string;
  graph_id: string;
  started: number;
  ended?: number;
  events: number;
  errors: number;
}

export interface Health {
  ok: boolean;
  node: string;
  mode: string;
  protocol: string;
  ir: string;
  ledger?: { entries: number; checkpoints: number; node_pubkey: string };
}

/**
 * One registered version of a graph.
 *
 * `digest` is the SHA-256 of the IR, which is what makes two revisions
 * comparable without downloading either: same digest, same graph.
 */
export interface GraphRevision {
  n: number;
  digest: string;
  created: number;
  bytes: number;
}

/** A node output supplied by the caller instead of produced by the skill. */
export interface Pin {
  port: string;
  schema?: string;
  payload: unknown;
}

/** One sealed effect (C4), as GET /v1/ledger returns it. */
export interface LedgerEntry {
  seq: number;
  prev: string;
  ts: number;
  node: string;
  session: string;
  envelope: string;
  cause: string;
  actor: string;
  capability: string;
  decision: string;
  outcome: string;
  policy: string;
  payload_sha256: string;
  waived?: boolean;
  /** The signed identity of the human who answered the gate (C4 v1.3); absent when unsigned. */
  approver?: { operator?: string; [k: string]: unknown };
  inference?: string[];
  compensates?: string;
  [k: string]: unknown;
}

/** GET /v1/ledger/verify. 200 when sound, 409 when not; the body is the same. */
export interface LedgerVerify {
  sound: boolean;
  report: {
    total_entries: number;
    chain_intact: boolean;
    checkpoints: number;
    checkpoints_valid: number;
    key_available: boolean;
    pubkey_fingerprint: string;
    merkle_root: string;
    merkle_mismatches: number;
    witnesses: number;
    witnesses_valid: number;
    approvals: number;
    approvals_invalid: number;
    key_rotations: number;
    [k: string]: unknown;
  };
}

export interface LedgerHead {
  seq: number;
  head_hash: string;
  merkle_root: string;
  node: string;
  pubkey: string;
}

/** A human-approval gate raised for a client that is not a person (GET /v1/approvals). */
export interface Approval {
  id: string;
  question: string;
  origin?: string;
  tool?: string;
  capability?: string;
  session?: string;
  arguments?: unknown;
  requested: string;
  expires: string;
  node?: string;
  envelope?: string;
  context_required?: string[];
}
