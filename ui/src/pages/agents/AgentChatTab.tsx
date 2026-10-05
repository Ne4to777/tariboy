import { useSearchParams } from "react-router-dom";
import ChannelsPage from "@/pages/ChannelsPage";
import AgentMessages from "@/pages/AgentMessages";
import AgentChat from "./chat/AgentChat";
import { useUiMode } from "@/lib/uiMode";

// The one section for everything an agent's messaging holds: the conversation
// the customer has with it, the channels it is subscribed to (its event
// triggers), and its delivery queue. They share a section because they are the
// same bus read three ways; Autopilot and Advanced link here instead.
const VIEWS = [
  ["chat", "Chat"],
  ["channels", "Channels"],
  ["queue", "Queue"],
] as const;
type View = typeof VIEWS[number][0];

function isView(value: string | null): value is View {
  return VIEWS.some(([key]) => key === value);
}

export default function AgentChatTab({ hostId = "" }: { hostId?: string }) {
  const [params, setParams] = useSearchParams();
  const simple = useUiMode() === "simple";
  const requested = params.get("view");
  const view: View = isView(requested) ? requested : "chat";
  const pick = (key: View) => {
    const next = new URLSearchParams(params);
    next.set("view", key);
    setParams(next, { replace: true });
  };
  /* The switch is rendered into each view's own toolbar rather than above it,
     so the section keeps one 48px control row instead of stacking two. */
  const segment = (
    <div role="group" aria-label="Messaging view" className="flex shrink-0 gap-0.5 rounded-[9px] bg-muted p-0.5">
      {VIEWS.map(([key, label]) => (
        <button
          key={key}
          type="button"
          aria-pressed={view === key}
          onClick={() => pick(key)}
          className={`h-[22px] rounded-[7px] px-[9px] text-[11.5px] ${view === key
            ? "bg-card font-medium text-foreground shadow-[var(--raise)]"
            : "text-muted-foreground hover:text-foreground"}`}
        >
          {label}
        </button>
      ))}
    </div>
  );
  // Simple is the personal chat alone: no Channels, no other chats.
  if (simple) return <AgentChat hostId={hostId} personalOnly />;
  if (view === "chat") return <AgentChat hostId={hostId} leading={segment} />;
  if (view === "queue") return <AgentMessages leading={segment} />;
  return <ChannelsPage leading={segment} />;
}
