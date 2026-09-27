import { useCallback, useEffect, useState } from "react";
import { Link, NavLink, Navigate, useNavigate, useParams, useSearchParams } from "react-router-dom";
import { Container } from "lucide-react";
import { toast } from "sonner";
import { useDaemons } from "@/components/DaemonProvider";
import { Button } from "@/components/ui/button";
import { StatusDot, StatusPill } from "@/components/ui/status";
import { AgentControls } from "@/components/AgentControls";
import { agentTone } from "@/lib/statusTone";
import { AgentNameContext, AgentStatusContext } from "@/lib/agent";
import { agentGetOn } from "@/lib/api";
import { openHostPathInVSCode } from "@/lib/desktop";
import { cn } from "@/lib/utils";
import { hostToParam, targetFor } from "@/lib/terminalsHost";
import type { AgentStatus, AgentSummary, AgentView } from "@/lib/types";
import AgentConsoleTab from "./AgentConsoleTab";
import AgentAutopilotTab from "./AgentAutopilotTab";
import AgentActivityTab from "./AgentActivityTab";
import AgentConfigurationTab from "./AgentConfigurationTab";
import AgentAdvancedTab from "./AgentAdvancedTab";
import AgentChatTab from "./AgentChatTab";
import TasksWorkspace from "@/pages/tasks/TasksWorkspace";
import { useCustomerQuestionNotifications } from "@/components/customerQuestionNotificationsContext";
import { customerQuestionAttentionKey } from "@/components/customerQuestionNotificationModel";
import { canOpenAgentCwdInVSCode } from "./agentCwdVSCode";
import { GoalHelp } from "@/components/GoalHelp";
import { useChatUnread } from "./chat/useChatUnread";
import { useUiMode } from "@/lib/uiMode";

const TABS = [
  ["chat", "Chat"],
  ["tasks", "Tasks"],
  ["console", "Console"],
  ["autopilot", "Autopilot"],
  ["activity", "Activity"],
  ["configuration", "Configuration"],
  ["advanced", "Advanced"],
] as const;
// The tabs Simple mode keeps; the header, Start/Stop included, is the same in both.
const SIMPLE_TABS: ReadonlySet<string> = new Set(["chat", "tasks", "console", "configuration"]);

export default function AgentWorkspace({ hostId, hostLabel, agent, refresh, unavailable = false }: {
  hostId: string;
  hostLabel?: string;
  agent: AgentSummary;
  refresh: () => void;
  unavailable?: boolean;
}) {
  const simple = useUiMode() === "simple";
  const tabs = simple ? TABS.filter(([key]) => SIMPLE_TABS.has(key)) : TABS;
  const { tab = "console" } = useParams();
  const navigate = useNavigate();
  const [searchParams] = useSearchParams();
  const { activeId, daemons, select } = useDaemons();
  const [connection, setConnection] = useState<"selecting" | "ready" | "unavailable">("selecting");
  const [status, setStatus] = useState<AgentStatus | null>(null);
  const [view, setView] = useState<{ key: string; view: AgentView | null }>({ key: "", view: null });
  const agentKey = `${hostId}\0${agent.name}`;
  const currentView = view.key === agentKey ? view.view : null;
  const effectiveCwd = agent.cwd || currentView?.cwd || "";
  // Configured runtime, compactly: whatever the harness actually reports.
  // Codex has no model until one is chosen; only Codex reports an effort.
  const runtime = [currentView?.harness || agent.harness, currentView?.model, currentView?.effort]
    .filter(Boolean).join(" · ");
  const target = targetFor(hostId);
  const [execCount, setExecCount] = useState(0);
  const { attention, refreshHost } = useCustomerQuestionNotifications();
  // The Chat tab's dot. It is the daemon's own unread count rather than tab
  // state, so it is right while another tab is open and survives a remount.
  const unreadChat = useChatUnread(target, agent.name, connection === "ready" && !unavailable);

  useEffect(() => {
    let cancelled = false;
    void select(hostId).then((ok) => {
      if (!cancelled) setConnection(ok ? "ready" : "unavailable");
    });
    return () => { cancelled = true; };
  }, [hostId, select]);

  // The URL is authoritative inside an agent workspace. A global host change
  // must never retarget legacy tab components to a same-named agent elsewhere.
  // Hide the tabs immediately and restore the host named by the route.
  useEffect(() => {
    if (connection !== "ready" || activeId === hostId) return;
    let cancelled = false;
    void select(hostId).then((ok) => {
      if (!cancelled && !ok) setConnection("unavailable");
    });
    return () => { cancelled = true; };
  }, [activeId, connection, hostId, select]);

  const refreshStatus = useCallback(async () => {
    if (connection !== "ready" || unavailable) return;
    const requestTarget = targetFor(hostId);
    try {
      setStatus(await agentGetOn<AgentStatus>(requestTarget, agent.name, "status"));
    } catch {
      setStatus(null);
    }
    try {
      setView({ key: agentKey, view: await agentGetOn<AgentView>(requestTarget, agent.name, "") });
    } catch {
      // Preserve the last successful value and retry with the next status poll.
    }
  }, [agent.name, agentKey, connection, hostId, unavailable]);

  useEffect(() => {
    if (connection !== "ready" || unavailable) return;
    void Promise.resolve().then(refreshStatus);
    const timer = window.setInterval(() => void refreshStatus(), 3000);
    return () => window.clearInterval(timer);
  }, [connection, refreshStatus, unavailable]);

  // An unknown or hidden tab opens the default one: Console, or Chat in Simple.
  if (!tabs.some(([key]) => key === tab)) {
    return <Navigate to={`/agents/${hostToParam(hostId)}/${encodeURIComponent(agent.name)}/${simple ? "chat" : "console"}`} replace />;
  }
  if (connection === "unavailable") {
    return (
      <div className="flex h-full flex-col items-center justify-center gap-2 text-sm text-muted-foreground">
        <p>Host unavailable.</p>
        <p>The route was not reassigned to the local daemon.</p>
      </div>
    );
  }
  if (connection === "selecting") {
    return <div className="flex h-full items-center justify-center text-sm text-muted-foreground">Connecting to host…</div>;
  }
  if (activeId !== hostId) {
    return <div className="flex h-full items-center justify-center text-sm text-muted-foreground">Restoring route host…</div>;
  }

  const base = `/agents/${hostToParam(hostId)}/${encodeURIComponent(agent.name)}`;
  const exhaustedPeriods = status?.budget?.exhausted ?? agent.budget?.exhausted ?? [];
  const outOfBudget = exhaustedPeriods.length > 0;
  const tone = agentTone(agent.state, outOfBudget);
  const alive = agent.enabled ?? agent.state !== "stopped";
  const questionTasks = attention.get(customerQuestionAttentionKey(hostId, agent.name)) ?? 0;
  const content =
    tab === "console" ? <AgentConsoleTab hostId={hostId} agent={agent} refresh={refresh} execCount={execCount} />
    : tab === "autopilot" ? <AgentAutopilotTab />
    : tab === "activity" ? <AgentActivityTab />
    : tab === "tasks" ? <TasksWorkspace
      scopeAgent={agent.name}
      target={target}
      initialTaskKey={searchParams.get("task") ?? undefined}
      onNotificationsChanged={() => void refreshHost(hostId)}
    />
    : tab === "chat" ? <AgentChatTab hostId={hostId} />
    : tab === "configuration" ? <AgentConfigurationTab target={target} refresh={refresh} />
    : <AgentAdvancedTab />;

  return (
    <AgentNameContext.Provider value={agent.name}>
      <AgentStatusContext.Provider value={{ status, refresh: refreshStatus }}>
        <div className="flex h-full min-h-0 flex-col">
          {/* Agent header: one line of identity (who, how it is doing, where it
              runs, what it is working toward), one line of location, and the
              lifecycle cluster — visible on every tab. */}
          <header className="shrink-0">
            <div className="flex items-start gap-2.5 px-4 pt-3.5 pb-[11px]">
              <div className="flex min-w-0 flex-1 flex-col gap-[5px]">
                <div className="flex min-w-0 items-center gap-[9px]">
                  <StatusDot tone={tone} size={8} />
                  <h1 className="shrink-0 text-[15px] font-semibold tracking-[-.01em] whitespace-nowrap">
                    {agent.name}
                  </h1>
                  <StatusPill tone={tone}>{outOfBudget ? "no budget" : agent.state}</StatusPill>
                  <span className="shrink-0 text-[12.5px] whitespace-nowrap text-muted-foreground">
                    {hostLabel || (hostId ? hostId : "Local")}
                  </span>
                  <span
                    title={agent.image}
                    className="inline-flex h-5 shrink-0 items-center gap-[5px] rounded-[6px] bg-muted px-[7px] font-mono text-[11.5px] whitespace-nowrap text-muted-foreground"
                  >
                    <Container className="size-2.5" aria-hidden="true" />
                    {agent.image}
                  </span>
                  {runtime && (
                    <span
                      data-testid="agent-runtime"
                      title="Harness · model · effort"
                      className="inline-flex h-5 shrink-0 items-center rounded-[6px] bg-muted px-[7px] font-mono text-[11.5px] whitespace-nowrap text-muted-foreground"
                    >
                      {runtime}
                    </span>
                  )}
                  <span className="flex min-w-0 flex-1 items-center gap-[5px] overflow-hidden text-[12.5px] whitespace-nowrap text-muted-foreground">
                    <span className="shrink-0">Goal:</span>
                    {agent.current_goal_task_key ? (
                      <Link
                        className="shrink-0 border-b border-dotted border-border font-mono text-[11.5px] font-medium text-foreground tabular-nums hover:border-foreground"
                        to={`${base}/tasks?task=${encodeURIComponent(agent.current_goal_task_key)}`}
                      >
                        {agent.current_goal_task_key}
                      </Link>
                    ) : <span className="shrink-0">No current goal</span>}
                    <GoalHelp />
                  </span>
                </div>
                {/* The reference ellipsises the path to hold the header to one
                    line; this app shows it whole on purpose (a wrapped cwd is
                    better than a path the operator cannot read), so it wraps. */}
                <div className="flex min-w-0 flex-wrap items-baseline gap-x-2.5 gap-y-1 text-[12px]">
                  <span className="flex min-w-0 items-baseline gap-1.5">
                    <span className="shrink-0 text-muted-foreground">cwd:</span>
                    <span data-testid="agent-cwd" className="min-w-0 break-all font-mono text-[11.5px]">
                      {effectiveCwd || "…"}
                    </span>
                  </span>
                  {effectiveCwd && canOpenAgentCwdInVSCode(hostId, daemons) && (
                    <Button
                      type="button"
                      data-testid="open-agent-cwd-vscode"
                      variant="link"
                      size="xs"
                      className="h-auto shrink-0 px-0 py-0 text-[12px] font-medium text-foreground"
                      disabled={unavailable}
                      onClick={() => void openHostPathInVSCode(hostId, effectiveCwd)
                        .catch((error) => toast.error(String(error)))}
                    >
                      Open in VS Code
                    </Button>
                  )}
                </div>
              </div>
              <AgentControls
                target={target}
                name={agent.name}
                alive={alive}
                image={agent.image}
                disabled={unavailable}
                configurationPath={`${base}/configuration`}
                refresh={refresh}
                onExec={() => setExecCount((n) => n + 1)}
                onDeleted={() => navigate("/")}
              />
            </div>
            {/* Why the loop is off: a halted loop (including a failed iteration
                launch, which never creates an iteration row) is otherwise only
                visible on the Autopilot tab. */}
            {(status?.halt_reason || status?.error_reason) && (
              <p
                data-testid="agent-halt-reason"
                className={cn("px-4 pb-2 text-[11px] font-medium",
                  status.halt_kind === "idle_limit" ? "text-muted-foreground" : "text-destructive")}
              >
                {status.halt_reason || status.error_reason}
              </p>
            )}
            {status?.messages_queue_full && (
              <p className="px-4 pb-2 text-[11px] font-medium text-destructive">
                Message queue full: {status.messages_pending} / {status.messages_max_queue}
              </p>
            )}
            {status?.budget && (status.budget.hour_usd > 0 || status.budget.day_usd > 0 || status.budget.week_usd > 0 || status.budget.month_usd > 0) && <div className="px-4 pb-2 text-[11px]" data-testid="agent-budget-header">
              {exhaustedPeriods.length > 0 && <p className="font-medium text-destructive">Out of budget: {exhaustedPeriods.join(", ")}</p>}
              <p className="text-muted-foreground tabular-nums">Hour {status.budget.hour_spent_usd.toFixed(2)} / {status.budget.hour_usd || "Unlimited"} · Day {status.budget.day_spent_usd.toFixed(2)} / {status.budget.day_usd || "Unlimited"} · Week {status.budget.week_spent_usd.toFixed(2)} / {status.budget.week_usd || "Unlimited"} · Month {status.budget.month_spent_usd.toFixed(2)} / {status.budget.month_usd || "Unlimited"}</p>
            </div>}
            <nav aria-label="Agent workspace" className="flex items-center gap-0.5 border-b px-3">
              {tabs.map(([key, label]) => (
                <NavLink
                  key={key}
                  to={`${base}/${key}`}
                  className={({ isActive }) =>
                    cn("flex h-[34px] items-center border-b-2 border-transparent px-2.5 text-[13px] text-muted-foreground hover:text-foreground",
                      isActive && "border-primary font-medium text-foreground")
                  }
                >
                  {label}
                  {key === "chat" && unreadChat > 0 && (
                    <span
                      role="img"
                      aria-label={`${unreadChat} unread message${unreadChat === 1 ? "" : "s"} from ${agent.name}`}
                      title={`${unreadChat} unread message${unreadChat === 1 ? "" : "s"}`}
                      className="ml-1.5 size-[5px] rounded-full bg-primary"
                    />
                  )}
                  {key === "tasks" && questionTasks > 0 && (
                    <span
                      role="img"
                      aria-label={`${questionTasks} task${questionTasks === 1 ? "" : "s"} with unread agent questions`}
                      title={`${questionTasks} task${questionTasks === 1 ? "" : "s"} with unread agent questions`}
                      className="ml-1.5 rounded-full bg-primary px-1.5 text-[11px] font-medium leading-4 text-primary-foreground"
                    >
                      {questionTasks}
                    </span>
                  )}
                </NavLink>
              ))}
            </nav>
          </header>
          {unavailable && (
            <p role="status" className="mb-2 text-sm text-muted-foreground">
              This host is temporarily unavailable; actions are disabled until it reconnects.
            </p>
          )}
          <section
            data-testid="agent-workspace-content"
            inert={unavailable}
            aria-disabled={unavailable || undefined}
            className="min-h-0 flex-1 overflow-auto"
          >
            {content}
          </section>
        </div>
      </AgentStatusContext.Provider>
    </AgentNameContext.Provider>
  );
}
