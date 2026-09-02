// GENERATED FILE — do not edit.
//
// Produced from spec/schemas/*.json by scripts/generate-types.mjs.
// The frozen contracts are the source of truth; run `npm run generate`
// after changing a schema. CI checks this file is current.

/** C3 Channel Envelope — aura:spec:envelope@1 */
export interface Envelope {
  v: "1";
  id: string;
  cause_id?: string;
  session?: string;
  node?: string;
  port?: string;
  seq?: number;
  idem?: string;
  schema?: string;
  kind: "data" | "done" | "error" | "status" | "register" | "confirm_request" | "confirm_response" | "cancel" | "config_update";
  payload?: unknown;
  receipt?: string;
  attest?: {
    engine: string;
    engine_version?: string;
    model: string;
    model_revision?: string;
    model_file?: string;
    model_sha256?: string;
    quantization?: string;
    params?: Record<string, unknown>;
    prompt_sha256?: string;
    output_sha256?: string;
    energy?: {
      millijoules: number;
      source: "nvml" | "rapl" | "powermetrics" | "estimated";
      basis?: string;
    };
    tee?: unknown;
  };
}

/** C1 Skill Manifest — aura:spec:manifest@1 */
export interface Manifest {
  id: string;
  version: string;
  protocol: string;
  name: string;
  description: string;
  capability: string;
  type: "sensorial" | "cognitive" | "motor" | "memory" | "logical";
  format: "source" | "wasm" | "model" | "projection";
  runtime?: {
    language?: string;
    version?: string;
  };
  ports: {
    ingress: PortList;
    egress: PortList;
  };
  requirements?: {
    memory?: string;
    accelerator?: "none" | "gpu" | "npu" | "any";
  };
  permissions?: {
    egress_http?: string[];
    filesystem?: string;
    channels?: string;
  };
  config?: ConfigParam[];
  compensates?: {
    port: string;
    schema?: string;
  };
  signature?: unknown;
}

export type PortList = {
  name: string;
  schema: string;
}[];

export interface ConfigParam {
  key: string;
  type: "string" | "int" | "float" | "bool" | "enum";
  default: unknown;
  min?: number;
  max?: number;
  options?: string[];
  description?: string;
  restart_required?: boolean;
}

/** C2 Graph IR — aura:spec:graph-ir@1 */
export interface GraphIR {
  ir: "1";
  graph_id: string;
  origin: {
    kind: "declared" | "planner";
    skill?: string;
    cause?: string;
  };
  nodes: {
    ref: string;
    resolve?: string;
    use?: string;
    constraints?: Record<string, unknown>;
  }[];
  edges: {
    from: string;
    to: string;
    gate?: "human-approval" | "none";
    qos?: "reliable" | "realtime" | "bulk";
  }[];
  waves?: string[][];
  context_budget?: number;
}

export interface Edge {
  speculative?: boolean;
  deadline_ms?: number;
  priority?: number;
}

export type EnvelopeKind = Envelope["kind"];
