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
}
