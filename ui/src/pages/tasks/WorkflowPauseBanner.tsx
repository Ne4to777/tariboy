import { PauseCircle } from "lucide-react"
import { useState } from "react"
import { Button } from "@/components/ui/button"
import type { ApiTarget } from "@/lib/api"
import { cancelWorkflowTask, resumeTaskWorkflow } from "@/lib/tasks"
import { cn } from "@/lib/utils"
import { DANGER_FILL } from "./panelStyles"
import { useWorkflowConfirm } from "./WorkflowConfirm"
import { errorText } from "./workflowShared"

/** The four reasons the daemon pauses a task, as a person reads them. */
const REASONS: Record<string, string> = {
  idle_iterations: "The holder worked several iterations in a row without requesting a transition.",
  rejected_requests: "Checks rejected too many transition requests in this status.",
  script_failures: "Scripts failed too many times in a row in this status.",
  holder_unavailable: "The holder of this status cannot work and has been unavailable for too long.",
}

const BUTTON = "h-6 shrink-0 rounded-[7px] px-2.5 text-[12px] text-status-failed hover:bg-[color-mix(in_oklab,var(--status-failed)_10%,transparent)] hover:text-status-failed"

/**
 * The paused task asks the customer to decide: continue with the same holder,
 * release the holder (pool statuses only), or cancel. A plain comment does not
 * resume a task, so the decisions live here and nowhere else.
 */
export default function WorkflowPauseBanner({ taskKey, reason, pool, target, onChanged }: {
  taskKey: string
  reason: string
  pool: boolean
  target?: ApiTarget
  onChanged: () => void
}) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState("")
  const { confirm, dialog } = useWorkflowConfirm()
  const decide = (run: () => Promise<unknown>) => {
    setBusy(true)
    setError("")
    run()
      .then(() => onChanged())
      .catch((failure) => setError(errorText(failure)))
      .finally(() => setBusy(false))
  }
  return (
    <div className={cn("flex flex-col gap-2 rounded-[8px] px-2.5 py-2", DANGER_FILL)}>
      <div role="alert" className="flex items-start gap-[9px]">
        <PauseCircle className="mt-0.5 size-3.5 shrink-0 [stroke-width:1.4]" />
        <p className="min-w-0 flex-1 text-[12.5px] leading-[1.45] font-medium text-pretty">
          Paused: {REASONS[reason] ?? reason} The workflow waits for your decision.
        </p>
      </div>
      <div className="flex flex-wrap gap-1.5">
        <Button type="button" variant="ghost" className={BUTTON} disabled={busy} onClick={() => confirm({
          title: "Continue this task?",
          // Only a pool status has a holder to keep.
          description: pool
            ? "The same holder continues and the counters are reset."
            : "Resume the task in its current status. The counters are reset.",
          action: "Continue",
          run: () => decide(() => resumeTaskWorkflow(taskKey, "continue", target)),
        })}>Continue</Button>
        {pool && <Button type="button" variant="ghost" className={BUTTON} disabled={busy} onClick={() => confirm({
          title: "Release the holder?",
          description: "The holder loses this task, and the task is dispatched to another member of the pool.",
          action: "Release holder",
          run: () => decide(() => resumeTaskWorkflow(taskKey, "release", target)),
        })}>Release holder</Button>}
        <Button type="button" variant="ghost" className={BUTTON} disabled={busy} onClick={() => confirm({
          title: "Cancel this task?",
          description: "The task closes as cancelled and its scripts stop.",
          action: "Cancel task",
          run: () => decide(() => cancelWorkflowTask(taskKey, target)),
        })}>Cancel task</Button>
      </div>
      {error && <p role="alert" className="text-[12px]">{error}</p>}
      {dialog}
    </div>
  )
}
