import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { IterationTimeoutControl } from "@/components/IterationTimeoutControl";
import { LoopToggle } from "@/components/LoopControls";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { useAgentName, useAgentStatus } from "@/lib/agent";
import { apiGet } from "@/lib/api";

interface BudgetStatus {
  scope: string;
  limit_usd: number;
  spent_usd: number;
  mode: string;
  over: boolean;
}

export default function AgentAutopilotTab() {
  const name = useAgentName();
  const { status, refresh } = useAgentStatus();
  const [budgets, setBudgets] = useState<BudgetStatus[]>([]);
  // Event triggers are the agent's subscriptions, edited only in Chat →
  // Channels; Autopilot shows their count. null leaves the count out.
  const [triggers, setTriggers] = useState<number | null>(null);
  useEffect(() => {
    void apiGet<{ channels: unknown[] }>(`/api/agents/${encodeURIComponent(name)}/subscriptions`)
      .then((result) => setTriggers(result.channels.length))
      .catch(() => setTriggers(null));
  }, [name]);
  useEffect(() => {
    void apiGet<{ budgets: BudgetStatus[] }>("/api/budgets/status")
      .then((result) => setBudgets(result.budgets.filter(
        (row) => row.scope === "global" || row.scope === `agent:${name}`,
      )))
      .catch(() => setBudgets([]));
  }, [name]);
  return (
    <div className="grid max-w-3xl gap-4">
      <Card>
        <CardHeader>
          <CardTitle>Autopilot</CardTitle>
          <CardDescription>Scheduled iterations are independent from the interactive Console session.</CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          <div className="flex items-center justify-between">
            <div>
              <div className="font-medium">{status?.loop_enabled ? "Running" : "Stopped"}</div>
              <div className="text-sm text-muted-foreground">
                {status?.iterations ?? 0} iterations · {status?.state ?? "loading"}
              </div>
              {status?.halt_reason && (
                <div
                  data-testid="halt-reason"
                  className={`text-sm ${status.halt_kind === "error" ? "text-destructive" : "text-muted-foreground"}`}
                >
                  {status.halt_reason}
                </div>
              )}
              {status?.error_reason && !status.halt_reason && (
                <div className="text-sm text-destructive">{status.error_reason}</div>
              )}
            </div>
            <LoopToggle
              name={name}
              enabled={status?.loop_enabled ?? false}
              onChanged={() => void refresh()}
            />
          </div>
          <IterationTimeoutControl name={name} status={status} refresh={refresh} />
          <div className="flex items-center gap-2 rounded-md border p-3 text-sm">
            <span className="font-medium">
              {triggers === null ? "Event triggers" : `Event triggers: ${triggers} channel${triggers === 1 ? "" : "s"}`}
            </span>
            <Link
              to={{ pathname: "../chat", search: "?view=channels" }}
              relative="path"
              className="ml-auto text-muted-foreground underline-offset-2 hover:text-foreground hover:underline"
            >
              Manage in Chat → Channels
            </Link>
          </div>
          <div className="space-y-2 rounded-md border p-3 text-sm">
            <div className="font-medium">Budget status</div>
            {budgets.map((budget) => (
              <div key={budget.scope} className="flex items-center gap-2">
                <span className="font-mono">{budget.scope}</span>
                <span className="ml-auto">${budget.spent_usd.toFixed(4)} / ${budget.limit_usd.toFixed(2)}</span>
                <Badge variant={budget.over ? "destructive" : "secondary"}>
                  {budget.over ? "over" : budget.mode}
                </Badge>
              </div>
            ))}
            {budgets.length === 0 && (
              <p className="text-muted-foreground">No global or agent-specific budget is configured.</p>
            )}
          </div>
        </CardContent>
      </Card>
    </div>
  );
}
