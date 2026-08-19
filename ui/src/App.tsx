import { useState } from "react";
import { Sidebar, TopBar, type ViewId } from "./components/Layout";
import { GraphsView } from "./views/GraphsView";
import { ProjectionsView } from "./views/ProjectionsView";
import { ReferenceView } from "./views/ReferenceView";
import { SessionsView } from "./views/SessionsView";
import { SkillsView } from "./views/SkillsView";
import { StudioView } from "./views/StudioView";

const VIEWS: Record<ViewId, () => React.ReactNode> = {
  studio: StudioView,
  skills: SkillsView,
  projections: ProjectionsView,
  graphs: GraphsView,
  sessions: SessionsView,
  reference: ReferenceView,
};

export default function App() {
  const [view, setView] = useState<ViewId>("studio");
  const View = VIEWS[view];
  return (
    <div className="shell">
      <Sidebar view={view} onNavigate={setView} />
      <div className="main">
        <TopBar />
        <View />
      </div>
    </div>
  );
}
