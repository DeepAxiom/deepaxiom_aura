/**
 * The studio — state a goal, watch the graph that answers it run.
 *
 * The home screen, and the two halves it joins were separate before: Operate
 * showed a plan as a list of steps and a stream of results, and Canvas showed
 * a graph you could draw. Neither showed the thing that is actually happening —
 * which node has the data, which edge is holding for you, where it stopped.
 *
 * **What is live and what is not**, stated because the difference matters:
 *
 *   - Planning is not. The planner emits progress lines and then one complete
 *     `std/plan@1`. The graph arrives whole, so the canvas appears whole. It
 *     is not drawn edge by edge, and animating it that way would be theatre
 *     about a decision already made.
 *   - Execution is. Every envelope carries the node it came from, so nodes
 *     light as data reaches them, a gate pulses while it waits for you, and a
 *     failure stays red where it happened.
 *
 * **It costs the node nothing.** These envelopes already arrive on the
 * session's own socket; drawing them is the browser's work. The one real cost
 * is client-side, and it is why `useRunActivity` exists: a streaming skill
 * emits an envelope per token, and rendering per envelope would drop frames.
 * Activity is batched to one update per animation frame, and every state it
 * paints is a class on an element already in the DOM — no node moves while a
 * session runs, so nothing re-layouts.
 */

import { useCallback, useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { api, newId, streamURL } from "../api/client";
import type { Envelope, GraphIR, Plan, SkillManifest } from "../api/types";
import { GateCard } from "../components/GateCard";
import { LiveGraph } from "../components/canvas/LiveGraph";
import { useRunActivity } from "../hooks/useRunActivity";

type Item =
  | { kind: "status"; text: string; key: string }
  | { kind: "result"; json: string; key: string }
  | { kind: "gate"; question: string; requestId: string; key: string }
  | { kind: "error"; text: string; key: string };

type Phase = "idle" | "planning" | "executing" | "done" | "failed";

export function StudioView() {
  const { t } = useTranslation();
  const [goal, setGoal] = useState("");
  const [phase, setPhase] = useState<Phase>("idle");
  const [ir, setIr] = useState<GraphIR | null>(null);
  const [items, setItems] = useState<Item[]>([]);
  const [error, setError] = useState("");
  const [skills, setSkills] = useState<SkillManifest[]>([]);
  const execWS = useRef<WebSocket | null>(null);

  const running = phase === "planning" || phase === "executing";
  const { activity, observe, reset } = useRunActivity(running);

  const push = useCallback((item: Item) => setItems((prev) => [...prev, item]), []);

  useEffect(() => {
    api.skills().then(setSkills).catch(() => setSkills([]));
  }, []);

  // Close the session's socket if the view goes away mid-run, or the node
  // keeps a session open for a page nobody is looking at.
  useEffect(() => () => execWS.current?.close(), []);

  /** Ask the planner. One shot: progress lines, then the whole plan. */
  const requestPlan = (text: string): Promise<Plan> =>
    new Promise((resolve, reject) => {
      const ws = new WebSocket(streamURL("plan"));
      const timeout = setTimeout(() => {
        ws.close();
        reject(new Error(t("studio.planTimeout")));
      }, 120_000);
      ws.onerror = () => reject(new Error(t("studio.unreachable")));
      ws.onmessage = (ev) => {
        const env = JSON.parse(ev.data) as Envelope;
        const payload = (env.payload ?? {}) as Record<string, unknown>;
        if (env.kind === "status" && payload.state === "ready") {
          ws.send(JSON.stringify({ text }));
        } else if (env.kind === "status" && payload.detail) {
          push({ kind: "status", key: env.id, text: String(payload.detail) });
        } else if (env.kind === "data" && env.schema === "std/plan@1") {
          clearTimeout(timeout);
          ws.close();
          resolve(env.payload as Plan);
        } else if (env.kind === "error") {
          clearTimeout(timeout);
          ws.close();
          reject(new Error(String(payload.detail ?? t("studio.needPlanner"))));
        }
      };
    });

  const execute = (p: Plan) => {
    setPhase("executing");
    let pending = p.inputs.length;
    const ws = new WebSocket(streamURL(p.graph.graph_id));
    execWS.current = ws;
    let seq = 0;

    ws.onmessage = (ev) => {
      const env = JSON.parse(ev.data) as Envelope;
      const payload = (env.payload ?? {}) as Record<string, unknown>;

      // Every envelope feeds the canvas. This is the cheap call — a few map
      // writes, no render; the frame loop decides when React hears about it.
      observe(env);

      switch (env.kind) {
        case "status":
          if (payload.state === "ready") {
            for (const input of p.inputs) {
              seq += 1;
              ws.send(JSON.stringify({
                v: "1", id: newId(), node: "client", port: input.port, seq,
                idem: `ui:studio:${input.port}:${seq}:${newId()}`,
                schema: input.schema, kind: "data", payload: input.payload,
              }));
            }
          } else if (payload.detail) {
            push({ kind: "status", key: env.id, text: String(payload.detail) });
          }
          break;
        case "confirm_request":
          push({ kind: "gate", key: env.id, requestId: env.id, question: String(payload.question ?? "Approve?") });
          break;
        case "data":
          push({ kind: "result", key: env.id, json: JSON.stringify(env.payload, null, 2) });
          pending -= 1;
          if (pending <= 0) { setPhase("done"); ws.close(); }
          break;
        case "done":
          pending -= 1;
          if (pending <= 0) { setPhase("done"); ws.close(); }
          break;
        case "error":
          push({ kind: "error", key: env.id, text: String(payload.detail ?? "error") });
          setPhase("failed");
          setError(String(payload.detail ?? "error"));
          ws.close();
          break;
      }
    };
  };

  const respondGate = (requestId: string, approve: boolean) => {
    execWS.current?.send(JSON.stringify({
      v: "1", id: newId(), cause_id: requestId,
      kind: "confirm_response", payload: { approve },
    }));
  };

  const run = async () => {
    const text = goal.trim();
    if (!text || running) return;
    setItems([]);
    setError("");
    setIr(null);
    reset();
    setPhase("planning");
    try {
      const p = await requestPlan(text);
      // The graph appears here, complete, because this is when it exists.
      setIr(p.graph);
      await api.registerGraph(p.graph);
      execute(p);
    } catch (e) {
      setPhase("failed");
      setError((e as Error).message);
    }
  };

  return (
    <section className="view view--flush studio">
      <div className="studio__bar">
        <input
          className="input studio__goal"
          placeholder={t("studio.placeholder")}
          value={goal}
          disabled={running}
          onChange={(e) => setGoal(e.target.value)}
          onKeyDown={(e) => e.key === "Enter" && run()}
        />
        <button className="btn" onClick={run} disabled={!goal.trim() || running}>
          {running ? t("studio.running") : t("studio.run")}
        </button>
        <span className={`studio__phase studio__phase--${phase}`}>{t(`studio.phase.${phase}`)}</span>
      </div>

      {error && <div className="error-banner">{error}</div>}

      <div className="studio__body">
        <LiveGraph
          ir={ir}
          skills={skills}
          activity={activity}
          placeholder={t("studio.empty")}
        />

        <aside className="studio__side">
          {items.length === 0 && <div className="studio__hint">{t("studio.hint")}</div>}
          {items.map((item) => {
            switch (item.kind) {
              case "status":
                return <div key={item.key} className="status-line">{item.text}</div>;
              case "gate":
                return (
                  <GateCard
                    key={item.key}
                    question={item.question}
                    onRespond={(approve) => respondGate(item.requestId, approve)}
                  />
                );
              case "result":
                return (
                  <pre key={item.key} className="studio__result">{item.json}</pre>
                );
              case "error":
                return <div key={item.key} className="finding finding--error">{item.text}</div>;
            }
          })}
        </aside>
      </div>
    </section>
  );
}
