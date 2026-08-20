import { useCallback, useEffect, useRef, useState } from "react";
import { newId, streamURL } from "./client";
import type { Envelope, Pin } from "./types";

/**
 * One live stream session against a graph (C3 over WebSocket).
 * Opens a new socket whenever `graph` changes; envelopes are surfaced raw to
 * the caller. No automatic reconnect on drop yet — see the README's Beta
 * gaps (no session resume) — the caller must remount to retry.
 */
export function useSession(graph: string | null, onEnvelope: (env: Envelope) => void) {
  const ws = useRef<WebSocket | null>(null);
  const seq = useRef(0);
  const [session, setSession] = useState<string>("");
  const [open, setOpen] = useState(false);
  const handler = useRef(onEnvelope);
  handler.current = onEnvelope;

  useEffect(() => {
    if (!graph) return;
    const socket = new WebSocket(streamURL(graph));
    ws.current = socket;
    seq.current = 0;
    setSession("");

    socket.onopen = () => setOpen(true);
    socket.onclose = () => setOpen(false);
    socket.onmessage = (ev) => {
      const env = JSON.parse(ev.data) as Envelope;
      const payload = env.payload as { state?: string; session?: string } | undefined;
      if (env.kind === "status" && payload?.state === "ready" && payload.session) {
        setSession(payload.session);
      }
      handler.current(env);
    };
    return () => {
      socket.onclose = null;
      socket.close();
    };
  }, [graph]);

  /** Shorthand text frame (std/text@1). */
  const sendText = useCallback((text: string) => {
    ws.current?.send(JSON.stringify({ text }));
  }, []);

  /**
   * Full C3 envelope on an arbitrary client port.
   *
   * Returns the envelope id, which is what `cancel` names — without it a
   * caller cannot abandon what it just asked for, which is the whole of
   * barge-in.
   */
  const sendData = useCallback(
    (port: string, schema: string, payload: unknown): string => {
      seq.current += 1;
      const env: Envelope = {
        v: "1",
        id: newId(),
        node: "client",
        port,
        seq: seq.current,
        idem: `ui:${port}:${seq.current}:${newId()}`,
        schema,
        kind: "data",
        payload,
      };
      ws.current?.send(JSON.stringify(env));
      return env.id;
    },
    [],
  );

  /**
   * Abandon a chain. The kernel stops routing anything belonging to it, so
   * a reply already being generated never arrives (C3 §Cancel).
   */
  const cancel = useCallback((envelopeId: string) => {
    const env: Envelope = {
      v: "1",
      id: newId(),
      cause_id: envelopeId,
      kind: "cancel",
    };
    ws.current?.send(JSON.stringify(env));
  }, []);

  /** Answer a human-approval gate. */
  const respondGate = useCallback((requestId: string, approve: boolean) => {
    const env: Envelope = {
      v: "1",
      id: newId(),
      cause_id: requestId,
      kind: "confirm_response",
      payload: { approve },
    };
    ws.current?.send(JSON.stringify(env));
  }, []);

  /**
   * Fix node outputs for this session, before anything is sent.
   *
   * Rides on `config_update`, which C3 already has — pinning is session
   * configuration and the contract is frozen. The kernel refuses a pin frame
   * that arrives after the first data message, so this has to run before the
   * first send, which is why it is a separate call rather than folded into one.
   */
  const setPins = useCallback((pins: Record<string, Pin>) => {
    if (!Object.keys(pins).length) return;
    ws.current?.send(JSON.stringify({
      v: "1",
      id: newId(),
      node: "client",
      kind: "config_update",
      schema: "aura/pins@1",
      payload: { pins },
    }));
  }, []);

  return { session, open, sendText, sendData, cancel, respondGate, setPins };
}
