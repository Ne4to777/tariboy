import { MoreHorizontal } from "lucide-react"
import { useCallback, useEffect, useRef, useState } from "react"
import { Button } from "@/components/ui/button"
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from "@/components/ui/dropdown-menu"
import { Input } from "@/components/ui/input"
import type { ApiTarget } from "@/lib/api"
import { cancelWorkflowTask, getTaskWorkflow, moveTaskWorkflow, type Task, type WorkflowView } from "@/lib/tasks"
import { cn } from "@/lib/utils"
import { EMPTY, FIELD, LABEL, MONO, QUIET_ACTION, ROW, SelectShell } from "./panelStyles"
import { formatTaskTime } from "./taskTime"
import WorkflowArtifacts from "./WorkflowArtifacts"
import { errorText, useWorkflowConfirm } from "./WorkflowConfirm"
import WorkflowOutcomes from "./WorkflowOutcomes"
import WorkflowPauseBanner from "./WorkflowPauseBanner"
import WorkflowRuns from "./WorkflowRuns"

/**
 * The workflow of a task, in the task detail: where it is, what it may do
 * next, what it produced, and what ran. It replaces the status control of a
 * flexible task; the status changes only through an outcome, a move, a cancel,
 * or a pause decision. The Desktop acts as the customer (operator routes).
 */
export function WorkflowPanel({ task, target, onTaskChanged }: {
  task: Task
  target?: ApiTarget
  onTaskChanged: () => void
}) {
  const [view, setView] = useState<WorkflowView | null>(null)
  const [error, setError] = useState("")
  const [moving, setMoving] = useState(false)
  const [cancelError, setCancelError] = useState("")
  const { confirm, dialog } = useWorkflowConfirm()
  const mounted = useRef(true)
  useEffect(() => {
    mounted.current = true
    return () => { mounted.current = false }
  }, [])

  const load = useCallback(async () => {
    try {
      const next = await getTaskWorkflow(task.key, target)
      if (!mounted.current) return
      setView(next)
      setError("")
    } catch (failure) {
      if (mounted.current) setError(errorText(failure))
    }
  }, [task.key, target])
  // The realtime path refetches the task; a new revision refetches the view.
  useEffect(() => { void Promise.resolve().then(load) }, [load, task.revision])
  const changed = () => { void load(); onTaskChanged() }
  const refresh = () => { void load() }

  const failure = error && <div role="alert" className="flex items-center gap-2 text-[12px] text-status-failed">
    <span className="min-w-0 flex-1">{error}</span>
    <Button type="button" variant="ghost" className={QUIET_ACTION} onClick={refresh}>Retry</Button>
  </div>
  if (!view) {
    return <section className="flex min-w-0 flex-col gap-1.5">
      <span className={LABEL}>Workflow</span>
      {failure || <span className={EMPTY} role="status">Loading workflow…</span>}
    </section>
  }

  const closed = view.category === "done" || view.category === "cancelled"
  const editable = task.access !== "context" && task.access !== "respond"
  const missing = view.outcomes.flatMap((outcome) => outcome.missing ?? [])
  const cancel = () => confirm({
    title: "Cancel this task?",
    description: "The task closes as cancelled and its scripts stop. The workflow status stays where it stopped.",
    action: "Cancel task",
    run: () => {
      setCancelError("")
      cancelWorkflowTask(task.key, target).then(
        () => { if (mounted.current) changed() },
        (failed) => { if (mounted.current) setCancelError(errorText(failed)) })
    },
  })
  // Resolves true once moved, false when the confirmation is declined.
  const move = (to: string, reason: string) => new Promise<boolean>((resolve, reject) => confirm({
    title: `Move this task to ${to}?`,
    description: "The move skips outcomes, required artifacts, and checks, stops the current scripts, and cancels a pending request.",
    action: "Move task",
    onCancel: () => resolve(false),
    run: () => { moveTaskWorkflow(task.key, to, reason, target).then(() => { resolve(true); changed() }, reject) },
  }))

  return (
    <div className="flex min-w-0 flex-col gap-[18px]">
      {view.waiting_on === "pause" && <WorkflowPauseBanner taskKey={task.key}
        reason={view.paused_reason ?? task.workflow_paused_reason ?? ""} pool={view.owner.startsWith("pool:")}
        target={target} onChanged={changed} />}
      <div className="flex min-w-0 items-center gap-2">
        <div className="flex min-w-0 flex-1 flex-wrap items-center gap-x-2.5 gap-y-0.5 text-[12px] text-muted-foreground">
          <span className={cn(MONO, "font-medium text-foreground")} title={view.digest}>{view.name}@{view.version}</span>
          <span>status <span className={cn(MONO, "text-foreground")}>{view.status}</span></span>
          {view.owner && <span>owner <span className={cn(MONO, "text-foreground")}>{view.owner}</span></span>}
          {view.holder && <span>holder <span className={cn(MONO, "text-foreground")}>{view.holder}</span></span>}
        </div>
        {editable && <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <Button variant="ghost" size="icon" aria-label="Workflow actions"
              className="size-7 rounded-[8px] text-muted-foreground hover:bg-accent hover:text-foreground"><MoreHorizontal className="size-3.5" /></Button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end">
            <DropdownMenuItem onSelect={() => setMoving(true)}>Move to status…</DropdownMenuItem>
            {!closed && <DropdownMenuItem className="text-destructive focus:text-destructive" onSelect={cancel}>Cancel task</DropdownMenuItem>}
          </DropdownMenuContent>
        </DropdownMenu>}
      </div>
      {failure}
      {cancelError && <p role="alert" className="text-[12px] text-status-failed">{cancelError}</p>}
      {moving && <MoveForm view={view} onDone={() => setMoving(false)} onMove={move} />}
      <WorkflowOutcomes taskKey={task.key} view={view} target={target} onChanged={changed} onRefresh={refresh} />
      <WorkflowArtifacts taskKey={task.key} artifacts={view.artifacts} missing={missing} editable={editable && !closed}
        target={target} onChanged={changed} />
      <WorkflowRuns taskKey={task.key} runs={view.runs ?? []} target={target} />
      <section className="flex min-w-0 flex-col gap-1">
        <span className={LABEL}>Visits</span>
        <ol className="flex min-w-0 flex-col">
          {[...view.visits].sort((a, b) => a.sequence - b.sequence).map((visit) => (
            <li key={visit.id} className={cn(ROW, "flex-wrap py-1")}>
              <span className={cn(MONO, "font-medium")}>{visit.status}</span>
              <span className={cn(MONO, "text-muted-foreground")}>{formatTaskTime(visit.entered_at)} · {visit.entered_by}</span>
              {visit.left_at
                ? <span className="text-[12px] text-muted-foreground">left by <span className={MONO}>{visit.outcome || "move"}</span></span>
                : <span className="text-[12px] text-muted-foreground">current</span>}
              {visit.message && <span className="w-full min-w-0 text-[12px] break-words whitespace-pre-wrap">{visit.message}</span>}
            </li>
          ))}
        </ol>
      </section>
      {dialog}
    </div>
  )
}

/**
 * The statuses a move may target. The view does not list the image's
 * statuses, so the choice is every status the task visited, every outcome
 * target, and the current status; a move error lists the rest.
 */
function knownStatuses(view: WorkflowView): string[] {
  const all = [...view.visits].sort((a, b) => a.sequence - b.sequence).map((visit) => visit.status)
    .concat(view.status, view.outcomes.map((outcome) => outcome.to))
  return all.filter((status, index) => status && all.indexOf(status) === index)
}

function MoveForm({ view, onMove, onDone }: {
  view: WorkflowView
  onMove: (to: string, reason: string) => Promise<boolean>
  onDone: () => void
}) {
  const [to, setTo] = useState("")
  const [reason, setReason] = useState("")
  const [error, setError] = useState("")
  const [busy, setBusy] = useState(false)
  const mounted = useRef(true)
  useEffect(() => {
    mounted.current = true
    return () => { mounted.current = false }
  }, [])
  const submit = () => {
    setError("")
    setBusy(true)
    onMove(to, reason.trim())
      .then((moved) => { if (moved && mounted.current) onDone() }, (failure) => { if (mounted.current) setError(errorText(failure)) })
      .finally(() => { if (mounted.current) setBusy(false) })
  }
  return (
    <form className="flex min-w-0 flex-col gap-1.5 rounded-[8px] bg-muted/50 p-2.5"
      onSubmit={(event) => { event.preventDefault(); submit() }}>
      <span className={LABEL}>Move to status</span>
      <div className="flex min-w-0 flex-wrap items-center gap-1.5">
        <SelectShell aria-label="Target status" value={to} className="h-[26px] w-auto text-[12px] md:text-[12px]"
          onChange={(event) => setTo(event.target.value)}>
          <option value="">Choose a status</option>
          {knownStatuses(view).map((status) => <option key={status} value={status}>
            {status === view.status ? `${status} (current)` : status}</option>)}
        </SelectShell>
        <Input aria-label="Reason" placeholder="Reason (required)" value={reason}
          className={cn(FIELD, "h-[26px] min-w-[160px] flex-1")} onChange={(event) => setReason(event.target.value)} />
        <Button type="submit" size="sm" className="h-[26px]" disabled={busy || !to || !reason.trim()}>Move</Button>
        <Button type="button" variant="ghost" size="sm" className="h-[26px]" onClick={onDone}>Close</Button>
      </div>
      {error && <p role="alert" className="text-[12px] text-status-failed">{error}</p>}
    </form>
  )
}
