import { useCallback, useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { CommandPalette } from "./components/CommandPalette";
import { LinkBanner, Sidebar, Toasts, TokenDialog, TopBar } from "./components/Layout";
import { useHealth, useRoute, useStats } from "./app/store";
import { EvidenceView } from "./views/EvidenceView";
import { ExpertiseView } from "./views/ExpertiseView";
import { GraphsView } from "./views/GraphsView";
import { ProjectionsView } from "./views/ProjectionsView";
import { PulseView } from "./views/PulseView";
import { ReferenceView } from "./views/ReferenceView";
import { SessionsView } from "./views/SessionsView";
import { StudioView } from "./views/StudioView";

export default function App() {
  const { t } = useTranslation();
  const route = useRoute();
  const health = useHealth();
  // One poll for the whole shell: Pulse draws it, the sidebar counts from it,
  // the catalog reads replica load from it.
  const feed = useStats();
  const [palette, setPalette] = useState(false);
  const [token, setToken] = useState(false);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        setPalette((p) => !p);
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  const openToken = useCallback(() => setToken(true), []);
  const { view, args } = route;

  return (
    <div className={`shell ${view === "studio" ? "shell--studio" : ""}`}>
      <a className="skip" href="#main">{t("common.skip")}</a>
      <Sidebar view={view} stats={feed.latest} />
      <div className="main">
        <TopBar view={view} health={health} stats={feed.latest} onPalette={() => setPalette(true)} onToken={openToken} />
        <LinkBanner health={health} onToken={openToken} />
        <main id="main" className="main__body" tabIndex={-1}>
          {view === "pulse" && <PulseView feed={feed} health={health} />}
          {/* The studio stays mounted: a graph being edited, or a session being
              watched, must survive a look at another screen. */}
          <div className="main__keep" hidden={view !== "studio"}>
            <StudioView active={view === "studio"} graphArg={view === "studio" ? args[0] : undefined} />
          </div>
          {view === "sessions" && <SessionsView sessionId={args[0]} eventId={args[1]} />}
          {view === "evidence" && <EvidenceView args={args} />}
          {view === "expertise" && <ExpertiseView selected={args[0]} stats={feed.latest} />}
          {view === "graphs" && <GraphsView />}
          {view === "projections" && <ProjectionsView />}
          {view === "reference" && <ReferenceView tab={args[0]} />}
        </main>
      </div>
      {palette && <CommandPalette onClose={() => setPalette(false)} onToken={openToken} />}
      {token && <TokenDialog onClose={() => setToken(false)} />}
      <Toasts />
    </div>
  );
}
