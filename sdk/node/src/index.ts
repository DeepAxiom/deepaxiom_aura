/**
 * @deepaxiom/aura — the TypeScript client for the AURA runtime.
 *
 * Two things, both talking the same frozen protocol:
 *
 *   createNode()   expose functions your app already has as skills
 *   openSession()  drive a graph from a frontend or a script
 *
 * Types come from spec/schemas — the same JSON Schemas the kernel and the
 * conformance suite use — so they cannot drift from the contracts.
 */
export { AuraNode, createNode } from "./node.js";
export type { ExposeOptions, Handler, HandlerContext, NodeOptions } from "./node.js";

export { Session, openSession } from "./session.js";
export type { Approval, ApprovalContextEntry, SessionOptions } from "./session.js";

export { envelope, newId, newSessionId, PROTOCOL_MAJOR } from "./envelope.js";

export type {
  ConfigParam,
  Envelope,
  EnvelopeKind,
  GraphIR,
  Manifest,
  PortList,
} from "./generated/types.js";
