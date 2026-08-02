import { useCallback, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { api, newId, streamURL } from "../api/client";
import type { Envelope, Plan } from "../api/types";
import { GateCard } from "../components/GateCard";

/**
 * The `aura do` flow as a view: goal → plan (via the planner skill) →
 * register the generated graph → execute it with human-approval gates.
 */

type ExecItem =
  | { kind: "status"; text: string; key: string }
  | { kind: "result"; json: string; key: string }
  | { kind: "gate"; question: string; requestId: string; key: string }
  | { kind: "error"; text: string; key: string };

type Phase = "idle" | "planning" | "executing" | "done" | "failed";

export function OperateView() {
  const { t } = useTranslation();
  const [goal, setGoal] = useState("");
  const [phase, setPhase] = useState<Phase>("idle");
  const [plan, setPlan] = useState<Plan | null>(null);
  const [session, setSession] = useState("");
  const [items, setItems] = useState<ExecItem[]>([]);
  const [error, setError] = useState("");
  const execWS = useRef<WebSocket | null>(null);

  const push = useCallback((item: ExecItem) => setItems((prev) => [...prev, item]), []);

  /** Phase 1 — ask the planner for a plan over a one-shot WS session. */
  const requestPlan = (text: string): Promise<Plan> =>
    new Promise((resolve, reject) => {
      const ws = new WebSocket(streamURL("plan"));
      const timeout = setTimeout(() => {
        ws.close();
        reject(new Error("timeout waiting for the plan"));
      }, 120_000);
      ws.onerror = () => reject(new Error("kernel unreachable"));
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
          reject(new Error(String(payload.detail ?? t("operate.needPlanner"))));
        }
      };
    });

  /** Phase 3 — execute the registered graph, answering gates inline. */
  const execute = (p: Plan) => {
    setPhase("executing");
    let pending = p.inputs.length;
    const ws = new WebSocket(streamURL(p.graph.graph_id));
    execWS.current = ws;
    let seq = 0;

    ws.onmessage = (ev) => {
      const env = JSON.parse(ev.data) as Envelope;
      const payload = (env.payload ?? {}) as Record<string, unknown>;
      switch (env.kind) {
        case "status":
          if (payload.state === "ready") {
            setSession(String(payload.session ?? ""));
            for (const input of p.inputs) {
              seq += 1;
              ws.send(JSON.stringify({
                v: "1", id: newId(), node: "client", port: input.port, seq,
                idem: `ui:do:${input.port}:${seq}:${newId()}`,
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
    if (!text || phase === "planning" || phase === "executing") return;
    setPlan(null);
    setItems([]);
    setError("");
    setSession("");
    setPhase("planning");
    try {
      const p = await requestPlan(text);
      setPlan(p);
      await api.registerGraph(p.graph);
      execute(p);
    } catch (e) {
      setPhase("failed");
      setError((e as Error).message);
    }
  };

  return (
    <section className="view">
      <div className="operate">
        <div className="page-head">
          <h1>{t("operate.title")}</h1>
          <p>{t("operate.subtitle")}</p>
        </div>

        <div className="operate__bar">
          <input
            className="input"
            placeholder={t("operate.placeholder")}
            value={goal}
            onChange={(e) => setGoal(e.target.value)}
            onKeyDown={(e) => e.key === "Enter" && run()}
          />
          <button className="btn" onClick={run} disabled={phase === "planning" || phase === "executing"}>
            {phase === "planning" ? t("operate.planning") : t("operate.run")}
          </button>
        </div>

        {error && <div className="error-banner">{t("operate.failed", { error })}</div>}

        {plan && (
          <div className="card">
            {plan.reasoning && (
              <>
                <div className="section-label">{t("operate.reasoning")}</div>
                <p style={{ color: "var(--text-dim)", fontSize: 13 }}>{plan.reasoning}</p>
              </>
            )}
            <div className="section-label">{t("operate.steps")}</div>
            {plan.graph.nodes.map((n) => (
              <div className="plan-step" key={n.ref}>
                <span className="plan-step__ref">{n.ref}</span>
                <span className="plan-step__arrow">→</span>
                <span className="plan-step__cap">{n.resolve ?? n.use}</span>
              </div>
            ))}
          </div>
        )}

        {session && (
          <div className="section-label">{t("operate.executing", { session })}</div>
        )}
        {items.map((item) => {
          switch (item.kind) {
            case "gate":
              return (
                <GateCard key={item.key} question={item.question}
                  onRespond={(ok) => respondGate(item.requestId, ok)} />
              );
            case "result":
              return (
                <div key={item.key} className="card result-block" style={{ marginTop: 10 }}>
                  <div className="section-label" style={{ margin: "0 0 8px" }}>{t("operate.result")}</div>
                  <pre>{item.json}</pre>
                </div>
              );
            case "error":
              return <div key={item.key} className="error-banner" style={{ marginTop: 10 }}>{item.text}</div>;
            default:
              return <div key={item.key} className="status-line">· {item.text}</div>;
          }
        })}

        {phase === "done" && <div className="done-banner">{t("operate.completed")}</div>}
      </div>
    </section>
  );
}
