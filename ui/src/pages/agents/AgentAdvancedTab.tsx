import { useEffect, useState } from "react";
import { Navigate, useSearchParams } from "react-router-dom";
import AgentPrompt from "@/pages/AgentPrompt";
import AgentContext from "@/pages/AgentContext";
import AgentFiles from "@/pages/AgentFiles";
import AgentScripts from "@/pages/AgentScripts";
import AuditLogPage from "@/pages/AuditLogPage";
import AgentUsagePage from "@/pages/AgentUsagePage";
import { Button } from "@/components/ui/button";
import { getAgent, getAgentStatus } from "@/lib/api";
import { useAgentName } from "@/lib/agent";

const VIEWS = [
  ["prompt", "Prompt"],
  ["context", "Context"],
  ["files", "Files"],
  ["scripts", "Scripts"],
  ["audit", "Full audit"],
  ["usage", "Raw usage"],
  ["raw", "Raw settings"],
] as const;
type View = typeof VIEWS[number][0];

// Channels and Messages moved to the Chat tab (Channels and Queue); links saved
// before the move land there instead of on an unrelated default view.
const MOVED: Record<string, string> = { channels: "channels", messages: "queue" };

function isView(value: string | null): value is View {
  return VIEWS.some(([key]) => key === value);
}

function RawSnapshot() {
  const name = useAgentName();
  const [snapshot, setSnapshot] = useState<unknown>(null);
  useEffect(() => {
    void Promise.all([getAgent(name), getAgentStatus(name)])
      .then(([configuration, status]) => setSnapshot({ configuration, status }))
      .catch((error) => setSnapshot({ error: String(error) }));
  }, [name]);
  return <pre className="overflow-auto rounded-md border bg-muted/30 p-4 text-xs">{JSON.stringify(snapshot, null, 2)}</pre>;
}

export default function AgentAdvancedTab() {
  const [params, setParams] = useSearchParams();
  const requested = params.get("view");
  const moved = requested ? MOVED[requested] : undefined;
  if (moved) return <Navigate to={{ pathname: "../chat", search: `?view=${moved}` }} relative="path" replace />;
  const view: View = isView(requested) ? requested : "prompt";
  const content =
    view === "prompt" ? <AgentPrompt />
    : view === "context" ? <AgentContext />
    : view === "files" ? <AgentFiles />
    : view === "scripts" ? <AgentScripts />
    : view === "audit" ? <AuditLogPage />
    : view === "usage" ? <AgentUsagePage />
    : <RawSnapshot />;
  return (
    <div className="flex h-full min-h-0 flex-col gap-3">
      <div className="flex flex-wrap gap-2">
        {VIEWS.map(([key, label]) => (
          <Button
            key={key}
            size="sm"
            variant={view === key ? "default" : "outline"}
            onClick={() => setParams({ view: key })}
          >
            {label}
          </Button>
        ))}
      </div>
      <div className="min-h-0 flex-1 overflow-auto">{content}</div>
    </div>
  );
}
