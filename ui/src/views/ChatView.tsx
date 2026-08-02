import { useCallback, useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { api } from "../api/client";
import type { Envelope } from "../api/types";
import { useSession } from "../api/useSession";
import { GateCard } from "../components/GateCard";

type Item =
  | { kind: "user" | "status" | "error"; text: string; key: string }
  | { kind: "aura"; text: string; key: string; open: boolean }
  | { kind: "result"; json: string; key: string }
  | { kind: "gate"; question: string; requestId: string; key: string };

export function ChatView() {
  const { t } = useTranslation();
  const [graphs, setGraphs] = useState<string[]>([]);
  const [graph, setGraph] = useState<string | null>(null);
  const [items, setItems] = useState<Item[]>([]);
  const [input, setInput] = useState("");
  const logRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    api.graphs().then((list) => {
      setGraphs(list);
      setGraph(list.includes("chat") ? "chat" : (list[0] ?? null));
    }).catch(() => {});
  }, []);

  useEffect(() => {
    logRef.current?.scrollTo({ top: logRef.current.scrollHeight });
  }, [items]);

  const push = useCallback((item: Item) => setItems((prev) => [...prev, item]), []);

  const onEnvelope = useCallback(
    (env: Envelope) => {
      const payload = (env.payload ?? {}) as Record<string, unknown>;
      switch (env.kind) {
        case "status": {
          if (payload.state === "ready") {
            push({ kind: "status", key: env.id, text: t("chat.sessionReady", { session: payload.session, graph }) });
          } else if (payload.detail) {
            push({ kind: "status", key: env.id, text: String(payload.detail) });
          }
          break;
        }
        case "data": {
          if (typeof payload.text === "string") {
            setItems((prev) => {
              const last = prev[prev.length - 1];
              if (last?.kind === "aura" && last.open) {
                const closed = payload.final === true;
                return [
                  ...prev.slice(0, -1),
                  { ...last, text: last.text + payload.text, open: !closed },
                ];
              }
              return [
                ...prev,
                { kind: "aura", key: env.id, text: String(payload.text), open: payload.final !== true },
              ];
            });
          } else {
            push({ kind: "result", key: env.id, json: JSON.stringify(env.payload, null, 2) });
          }
          break;
        }
        case "done": {
          setItems((prev) => {
            const last = prev[prev.length - 1];
            return last?.kind === "aura" ? [...prev.slice(0, -1), { ...last, open: false }] : prev;
          });
          break;
        }
        case "error":
          push({ kind: "error", key: env.id, text: String(payload.detail ?? "error") });
          break;
        case "confirm_request":
          push({ kind: "gate", key: env.id, requestId: env.id, question: String(payload.question ?? "Approve?") });
          break;
      }
    },
    [push, t, graph],
  );

  const { sendText, respondGate } = useSession(graph, onEnvelope);

  const submit = () => {
    const text = input.trim();
    if (!text) return;
    push({ kind: "user", key: `u-${Date.now()}`, text });
    sendText(text);
    setInput("");
  };

  return (
    <section className="view view--flush">
      <div className="chat">
        <div className="chat__log" ref={logRef}>
          {items.length === 0 && <div className="msg msg--status">{t("chat.empty")}</div>}
          {items.map((item) => {
            switch (item.kind) {
              case "gate":
                return (
                  <GateCard
                    key={item.key}
                    question={item.question}
                    onRespond={(ok) => respondGate(item.requestId, ok)}
                  />
                );
              case "result":
                return (
                  <div key={item.key} className="msg msg--result">
                    <pre>{item.json}</pre>
                  </div>
                );
              default:
                return (
                  <div key={item.key} className={`msg msg--${item.kind}`}>
                    {item.text}
                  </div>
                );
            }
          })}
        </div>
        <div className="chat__bar">
          <select
            className="select"
            aria-label={t("chat.graph")}
            value={graph ?? ""}
            onChange={(e) => {
              setGraph(e.target.value);
              setItems([]);
            }}
          >
            {graphs.map((g) => (
              <option key={g} value={g}>{g}</option>
            ))}
          </select>
          <input
            className="input"
            placeholder={t("chat.placeholder")}
            value={input}
            onChange={(e) => setInput(e.target.value)}
            onKeyDown={(e) => e.key === "Enter" && submit()}
          />
          <button className="btn" onClick={submit}>{t("chat.send")}</button>
        </div>
      </div>
    </section>
  );
}
