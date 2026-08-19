/**
 * Talking to a graph, by typing or out loud, beside the graph you are talking to.
 *
 * This replaces the separate Chat and Voice screens. They were never two
 * features — both open a session, push input in through a client port and
 * render what comes back — so they were two copies of one thing that had
 * already started to drift: only one of them rendered approval gates, and only
 * one knew what to do with a payload that was not text.
 *
 * What genuinely differs is the **transport**, and that is what the toggle
 * selects:
 *
 *   - *text* runs whichever registered graph you pick, usually `chat`: one
 *     `cognitive.llm.chat` node, text in, text out.
 *   - *voice* runs the `voice` graph, which is four skills
 *     (`ASR -> LLM -> chunker -> TTS`) across six client ports at once, with
 *     barge-in — speaking over the assistant cancels what it was saying.
 *
 * Those are different graphs, not different settings, so the toggle swaps the
 * session rather than pretending one graph can do both. The transcript model
 * they share lives in `converse/log.ts`.
 */

import { useCallback, useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { api } from "../../api/client";
import type { Envelope } from "../../api/types";
import { useSession } from "../../api/useSession";
import { Capture } from "../../audio/capture";
import { Player } from "../../audio/player";
import { fold, spoken, type Line } from "../../converse/log";
import { GateCard } from "../GateCard";
import { IconChat, IconMic } from "../Icons";

/** The graph the voice transport speaks to. Declared in the node's examples. */
const VOICE_GRAPH = "voice";

type Transport = "text" | "voice";

/**
 * The graph a typed message can actually reach.
 *
 * `sendText` puts the message on `client.text_out`, so the default has to be a
 * graph with an edge leaving that port. `chat` is exactly that.
 *
 * Deliberately **not** the graph open on the canvas, which is the tempting
 * choice and the wrong one: a planner graph consumes `client.step<N>_out` and
 * would swallow every typed message in silence — a text box that looks like it
 * works and does nothing is worse than one pointed somewhere obvious. Any graph
 * is still one click away in the picker; only the default is opinionated.
 */
function pickDefault(list: string[]): string | null {
  if (list.includes("chat")) return "chat";
  return list[0] ?? null;
}

export function ConversePanel() {
  const { t } = useTranslation();
  const [transport, setTransport] = useState<Transport>("text");
  const [graphs, setGraphs] = useState<string[]>([]);
  const [textGraph, setTextGraph] = useState<string | null>(null);
  const [lines, setLines] = useState<Line[]>([]);
  const [input, setInput] = useState("");

  // Voice-only state.
  const [listening, setListening] = useState(false);
  const [speaking, setSpeaking] = useState(false);
  const [playing, setPlaying] = useState(false);
  const [halfDuplex, setHalfDuplex] = useState(false);
  const [level, setLevel] = useState(0);
  const [error, setError] = useState<string | null>(null);

  const capture = useRef<Capture | null>(null);
  const player = useRef<Player | null>(null);
  /** The envelope that began the exchange being answered — what a cancel names. */
  const answering = useRef<string | null>(null);
  const logRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    api.graphs().then((list) => {
      setGraphs(list);
      setTextGraph((cur) => cur ?? pickDefault(list));
    }).catch(() => {});
  }, []);

  useEffect(() => {
    logRef.current?.scrollTo({ top: logRef.current.scrollHeight });
  }, [lines]);

  const onEnvelope = useCallback((env: Envelope) => {
    // Audio is a side effect, not a line, so it is handled before the fold —
    // which returns the transcript untouched for these and skips the render.
    const payload = (env.payload ?? {}) as Record<string, unknown>;
    if (env.port === "audio_in" && typeof payload.pcm_b64 === "string") {
      player.current?.play(payload.pcm_b64, Number(payload.sample_rate) || 22050);
      // Give the microphone a moment to stop hearing the speakers.
      capture.current?.guardFor(300);
    }
    setLines((prev) => fold(prev, env, {
      sessionReady: (session: string, graph: string) => t("converse.sessionReady", { session, graph }),
      approve: t("converse.approve"),
      unknownError: t("converse.unknownError"),
    }));
  }, [t]);

  const activeGraph = transport === "voice" ? (listening ? VOICE_GRAPH : null) : textGraph;
  const { open, sendText, sendData, cancel, respondGate } = useSession(activeGraph, onEnvelope);

  const submit = () => {
    const text = input.trim();
    if (!text) return;
    setLines((prev) => spoken(prev, text));
    sendText(text);
    setInput("");
  };

  const startVoice = useCallback(async () => {
    setError(null);
    if (!window.isSecureContext) {
      // localhost counts as secure; a LAN address over plain HTTP does not, and
      // getUserMedia fails there with a confusing permissions error instead.
      setError(t("converse.insecureContext"));
      return;
    }
    player.current = new Player(setPlaying);
    capture.current = new Capture({
      onLevel: setLevel,
      onSpeechStart: () => {
        setSpeaking(true);
        // Order matters. Silence the speaker locally first — instant, no round
        // trip — and only then tell the kernel to abandon the chain. Waiting on
        // the server would leave the assistant talking over the person for as
        // long as the network takes.
        player.current?.flush();
        if (answering.current) {
          cancel(answering.current);
          answering.current = null;
        }
      },
      onSpeechEnd: () => setSpeaking(false),
      onChunk: (pcm, final) => {
        const id = sendData("audio_out", "std/audio-chunk@1", {
          pcm_b64: pcm, sample_rate: 16000, final,
        });
        // The last chunk of an utterance roots everything said back, so it is
        // what a later barge-in cancels.
        if (final) answering.current = id;
      },
    });
    try {
      await capture.current.start();
      setListening(true);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
      await capture.current.stop();
      capture.current = null;
    }
  }, [cancel, sendData, t]);

  const stopVoice = useCallback(async () => {
    await capture.current?.stop();
    await player.current?.close();
    capture.current = null;
    player.current = null;
    answering.current = null;
    setListening(false);
    setSpeaking(false);
  }, []);

  // Switching back to text, or unmounting, must release the microphone. A tab
  // that quietly keeps recording is the one bug in here nobody would forgive,
  // and the browser's recording dot would be the only clue it happened.
  useEffect(() => {
    if (transport !== "voice" && listening) void stopVoice();
  }, [transport, listening, stopVoice]);
  useEffect(() => () => { void stopVoice(); }, [stopVoice]);

  useEffect(() => {
    capture.current?.setMuted(halfDuplex && playing);
  }, [halfDuplex, playing]);

  return (
    <div className="converse">
      <header className="converse__head">
        <div className="seg" role="tablist" aria-label={t("converse.transport")}>
          {([["text", IconChat], ["voice", IconMic]] as [Transport, typeof IconChat][]).map(
            ([mode, Icon]) => (
              <button
                key={mode}
                role="tab"
                aria-selected={transport === mode}
                className={`seg__btn ${transport === mode ? "seg__btn--on" : ""}`}
                onClick={() => {
                  if (mode === transport) return;
                  // A transport is a different graph, so it is a different
                  // conversation. Leaving the old transcript under the new
                  // header would attribute one graph's replies to another.
                  setTransport(mode);
                  setLines([]);
                }}
              >
                <Icon size={13} />
                {t("converse." + mode)}
              </button>
            ),
          )}
        </div>

        {transport === "text" ? (
          <select
            className="select select--sm"
            aria-label={t("converse.graph")}
            value={textGraph ?? ""}
            onChange={(e) => { setTextGraph(e.target.value); setLines([]); }}
          >
            {graphs.map((g) => <option key={g} value={g}>{g}</option>)}
          </select>
        ) : (
          <span className="converse__graph" title={t("converse.voiceGraphHelp")}>{VOICE_GRAPH}</span>
        )}

        <button
          className="btn btn--ghost btn--sm converse__clear"
          onClick={() => setLines([])}
          disabled={lines.length === 0}
        >
          {t("converse.clear")}
        </button>
      </header>

      {transport === "voice" && (
        <div className="converse__voicebar">
          {listening ? (
            <button className="btn btn--danger btn--sm" onClick={() => void stopVoice()}>
              {t("converse.stop")}
            </button>
          ) : (
            <button className="btn btn--primary btn--sm" onClick={() => void startVoice()}>
              {t("converse.start")}
            </button>
          )}
          <label className="check" title={t("converse.halfDuplexHelp")}>
            <input type="checkbox" checked={halfDuplex} onChange={(e) => setHalfDuplex(e.target.checked)} />
            {t("converse.halfDuplex")}
          </label>
          {listening && (
            <span className={`pill ${speaking ? "pill--live" : ""}`}>
              {speaking ? t("converse.hearingYou") : playing ? t("converse.speaking") : t("converse.waiting")}
            </span>
          )}
          {listening && !open && <span className="pill">{t("converse.connecting")}</span>}
        </div>
      )}

      {transport === "voice" && listening && (
        <div className="meter" aria-hidden>
          <div className="meter__bar" style={{ width: `${Math.min(level * 400, 100)}%` }} />
        </div>
      )}

      {error && <div className="alert">{error}</div>}

      <div className="converse__log" ref={logRef}>
        {lines.length === 0 && (
          <div className="converse__empty">
            {t(transport === "voice" ? "converse.emptyVoice" : "converse.emptyText")}
          </div>
        )}
        {lines.map((line) => {
          switch (line.kind) {
            case "gate":
              return (
                <GateCard
                  key={line.key}
                  question={line.question}
                  onRespond={(ok) => respondGate(line.requestId, ok)}
                />
              );
            case "result":
              return <pre key={line.key} className="msg msg--result">{line.json}</pre>;
            case "aura":
              return (
                <div key={line.key} className="msg msg--aura">
                  {line.text}
                  {line.open && <span className="cursor" />}
                </div>
              );
            case "user":
              return (
                <div key={line.key} className={`msg msg--user ${line.partial ? "msg--partial" : ""}`}>
                  {line.text}
                </div>
              );
            default:
              return <div key={line.key} className={`msg msg--${line.kind}`}>{line.text}</div>;
          }
        })}
      </div>

      {transport === "text" && (
        <div className="converse__bar">
          <input
            className="input"
            placeholder={t("converse.placeholder")}
            value={input}
            onChange={(e) => setInput(e.target.value)}
            onKeyDown={(e) => e.key === "Enter" && submit()}
          />
          <button className="btn btn--sm" onClick={submit} disabled={!input.trim()}>
            {t("converse.send")}
          </button>
        </div>
      )}

      {transport === "voice" && <p className="converse__note">{t("converse.requires")}</p>}
    </div>
  );
}
