import { useCallback, useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { useSession } from "../api/useSession";
import type { Envelope } from "../api/types";
import { Capture } from "../audio/capture";
import { Player } from "../audio/player";

/**
 * Talking to a graph.
 *
 * Six client ports on one socket at the same time — audio up, audio down,
 * partial transcripts, the reply text, status — which the kernel has always
 * supported and nothing until now used.
 */
const GRAPH = "voice";

type Line = { who: "you" | "aura"; text: string; partial?: boolean };

export function VoiceView() {
  const { t } = useTranslation();
  const [listening, setListening] = useState(false);
  const [speaking, setSpeaking] = useState(false);
  const [playing, setPlaying] = useState(false);
  const [halfDuplex, setHalfDuplex] = useState(false);
  const [level, setLevel] = useState(0);
  const [error, setError] = useState<string | null>(null);
  const [lines, setLines] = useState<Line[]>([]);

  const capture = useRef<Capture | null>(null);
  const player = useRef<Player | null>(null);
  /** The envelope that started the exchange currently being answered. This is
   *  what a cancel has to name; without it barge-in cannot say what to stop. */
  const answering = useRef<string | null>(null);
  const bottom = useRef<HTMLDivElement>(null);

  const onEnvelope = useCallback((env: Envelope) => {
    const payload = (env.payload ?? {}) as Record<string, unknown>;

    if (env.port === "audio_in" && typeof payload.pcm_b64 === "string") {
      player.current?.play(payload.pcm_b64, Number(payload.sample_rate) || 22050);
      // Give the microphone a moment to stop hearing the speakers.
      capture.current?.guardFor(300);
      return;
    }

    if (env.port === "transcript_in") {
      // A transcript REPLACES the previous hypothesis rather than extending
      // it — that is why std/transcript@1 exists apart from std/text@1.
      const text = String(payload.text ?? "");
      const settled = payload.final === true;
      setLines((prev) => {
        const next = [...prev];
        const last = next[next.length - 1];
        if (last?.who === "you" && last.partial) next[next.length - 1] = { who: "you", text, partial: !settled };
        else next.push({ who: "you", text, partial: !settled });
        return next;
      });
      return;
    }

    if (env.port === "text_in" && env.kind === "data") {
      // Here `text` IS a delta: the model streams token by token.
      const token = String(payload.text ?? "");
      const done = payload.final === true;
      if (!token && !done) return;
      setLines((prev) => {
        const next = [...prev];
        const last = next[next.length - 1];
        if (last?.who === "aura" && last.partial) {
          next[next.length - 1] = { who: "aura", text: last.text + token, partial: !done };
        } else if (token) {
          next.push({ who: "aura", text: token, partial: !done });
        }
        return next;
      });
      return;
    }

    if (env.kind === "error") {
      setError(String(payload.detail ?? t("voice.unknownError")));
    }
  }, [t]);

  const { open, sendData, cancel } = useSession(listening ? GRAPH : null, onEnvelope);

  useEffect(() => {
    bottom.current?.scrollIntoView({ behavior: "smooth" });
  }, [lines]);

  const start = useCallback(async () => {
    setError(null);
    if (!window.isSecureContext) {
      // localhost counts as secure; a LAN address over plain HTTP does not,
      // and getUserMedia fails there with a confusing permissions error.
      setError(t("voice.insecureContext"));
      return;
    }
    player.current = new Player(setPlaying);
    capture.current = new Capture({
      onLevel: setLevel,
      onSpeechStart: () => {
        setSpeaking(true);
        // Order matters. Stop the sound locally first — instant, no round
        // trip — and only then tell the kernel to abandon the chain. Waiting
        // for the server would leave the assistant talking over the user for
        // as long as the network takes.
        player.current?.flush();
        if (answering.current) {
          cancel(answering.current);
          answering.current = null;
        }
      },
      onSpeechEnd: () => setSpeaking(false),
      onChunk: (pcm, final) => {
        const id = sendData("audio_out", "std/audio-chunk@1", {
          pcm_b64: pcm,
          sample_rate: 16000,
          final,
        });
        // The last chunk of an utterance is the root of everything the
        // assistant will say back, so it is what a later cancel names.
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

  const stop = useCallback(async () => {
    await capture.current?.stop();
    await player.current?.close();
    capture.current = null;
    player.current = null;
    answering.current = null;
    setListening(false);
    setSpeaking(false);
  }, []);

  useEffect(() => () => { void stop(); }, [stop]);

  useEffect(() => {
    capture.current?.setMuted(halfDuplex && playing);
  }, [halfDuplex, playing]);

  return (
    <section className="view">
      <header className="view__head">
        <h1>{t("voice.title")}</h1>
        <p className="muted">{t("voice.subtitle")}</p>
      </header>

      <div className="row">
        {listening ? (
          <button className="btn btn--danger" onClick={() => void stop()}>
            {t("voice.stop")}
          </button>
        ) : (
          <button className="btn btn--primary" onClick={() => void start()}>
            {t("voice.start")}
          </button>
        )}

        <label className="check" title={t("voice.halfDuplexHelp")}>
          <input
            type="checkbox"
            checked={halfDuplex}
            onChange={(e) => setHalfDuplex(e.target.checked)}
          />
          {t("voice.halfDuplex")}
        </label>

        {listening && (
          <span className={`pill ${speaking ? "pill--live" : ""}`}>
            {speaking ? t("voice.hearingYou") : playing ? t("voice.speaking") : t("voice.waiting")}
          </span>
        )}
        {listening && !open && <span className="pill">{t("voice.connecting")}</span>}
      </div>

      {listening && (
        <div className="meter" aria-hidden>
          <div className="meter__bar" style={{ width: `${Math.min(level * 400, 100)}%` }} />
        </div>
      )}

      {error && <div className="alert">{error}</div>}

      <div className="chat">
        {lines.length === 0 && <p className="muted">{t("voice.empty")}</p>}
        {lines.map((line, i) => (
          <div key={i} className={`bubble bubble--${line.who === "you" ? "user" : "aura"}`}>
            {line.text}
            {line.partial && <span className="cursor" />}
          </div>
        ))}
        <div ref={bottom} />
      </div>

      <p className="muted small">{t("voice.requires")}</p>
    </section>
  );
}
