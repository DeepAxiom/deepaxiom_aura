import { useState } from "react";
import { Sidebar, TopBar, type ViewId } from "./components/Layout";
import { ChatView } from "./views/ChatView";
import { GraphsView } from "./views/GraphsView";
import { OperateView } from "./views/OperateView";
import { ProjectionsView } from "./views/ProjectionsView";
import { SessionsView } from "./views/SessionsView";
import { SkillsView } from "./views/SkillsView";
import { VoiceView } from "./views/VoiceView";

const VIEWS: Record<ViewId, () => React.ReactNode> = {
  chat: ChatView,
  voice: VoiceView,
  operate: OperateView,
  skills: SkillsView,
  projections: ProjectionsView,
  graphs: GraphsView,
  sessions: SessionsView,
};

export default function App() {
  const [view, setView] = useState<ViewId>("chat");
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
