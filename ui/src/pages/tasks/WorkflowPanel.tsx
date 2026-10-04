import { useCallback, useEffect, useRef, useState } from "react"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import type { ApiTarget } from "@/lib/api"
import { cancelWorkflowTask, getTaskWorkflow, moveTaskWorkflow, type Task, type WorkflowView } from "@/lib/tasks"
import { cn } from "@/lib/utils"
import { EMPTY, FIELD, LABEL, MONO, QUIET_ACTION, ROW, SelectShell } from "./panelStyles"
import { formatTaskTime } from "./taskTime"
import WorkflowArtifacts from "./WorkflowArtifacts"
import { useWorkflowConfirm } from "./WorkflowConfirm"
import WorkflowOutcomes from "./WorkflowOutcomes"
import WorkflowPauseBanner from "./WorkflowPauseBanner"
import WorkflowRuns from "./WorkflowRuns"
import { errorText, useMounted } from "./workflowShared"

/** A mono value that may be a 64-character ID: it wraps rather than overflows. */
const MONO_ID = cn(MONO, "min-w-0 break-all")

/**
 * The workflow of a task, in the task detail: where it is, what it may do
 * next, what it produced, and what ran. It replaces the status control of a
 * flexible task; the status changes only through an outcome, a move, a cancel,
 * or a pause decision. The Desktop acts as the customer (operator routes).
 */
export function WorkflowPanel({ task, eventSequence = 0, target, onTaskChanged }: {
  task: Task
  /**
   * The latest task event the detail holds. A workflow event (a rejected
   * request, a script run) need not change the task's revision, so a new
   * sequence refetches the view too.
   */
  eventSequence?: number
  target?: ApiTarget
  onTaskChanged: () => void
}) {
  const [view, setView] = useState<WorkflowView | null>(null)
  const [error, setError] = useState("")
  const [moving, setMoving] = useState(false)
  const [canceling, setCanceling] = useState(false)
  const [cancelError, setCancelError] = useState("")
  const { confirm, dialog } = useWorkflowConfirm()
  const mountedRef = useMounted()
  // Only the newest request may set the view: an older response that
  // arrives late is dropped.
  const latest = useRef(0)

  const load = useCallback(async () => {
    const request = ++latest.current
    try {
      const next = await getTaskWorkflow(task.key, target)
      if (!mountedRef.current || request !== latest.current) return
      setView(next)
      setError("")
    } catch (failure) {
      if (mountedRef.current && request === latest.current) setError(errorText(failure))
    }
  }, [mountedRef, task.key, target])
  // The realtime path refetches the task and its events; a new revision or a
  // new event refetches the view.
  useEffect(() => { void Promise.resolve().then(load) }, [load, task.revision, eventSequence])
  // Another action starting makes an earlier cancel failure stale.
  const changed = () => { setCancelError(""); void load(); onTaskChanged() }
  const refresh = () => { setCancelError(""); void load() }

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
  const cancel = () => confirm({
    title: "Cancel this task?",
    description: "The task closes as cancelled and its scripts stop. The workflow status stays where it stopped.",
    action: "Cancel task",
    run: () => {
      setCancelError("")
      setCanceling(true)
      cancelWorkflowTask(task.key, target).then(
        () => { if (mountedRef.current) changed() },
        (failed) => { if (mountedRef.current) setCancelError(errorText(failed)) })
        .finally(() => { if (mountedRef.current) setCanceling(false) })
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
          <span className="min-w-0">status <span className={cn(MONO_ID, "text-foreground")}>{view.status}</span></span>
          {view.owner && <span className="min-w-0">owner <span className={cn(MONO_ID, "text-foreground")}>{view.owner}</span></span>}
          {view.holder && <span className="min-w-0">holder <span className={cn(MONO_ID, "text-foreground")}>{view.holder}</span></span>}
        </div>
        {/* Labeled, not behind a menu: the customer's manual way out of the
            workflow has to be found without hunting for it. */}
        {editable && <div className="flex shrink-0 items-center gap-1">
          <Button type="button" variant="ghost" size="sm" className="h-7"
            onClick={() => { setCancelError(""); setMoving(true) }}>Move to status…</Button>
          {!closed && <Button type="button" variant="ghost" size="sm" className="h-7 text-destructive hover:text-destructive"
            disabled={canceling} onClick={cancel}>Cancel task</Button>}
        </div>}
      </div>
      {failure}
      {cancelError && <p role="alert" className="text-[12px] text-status-failed">{cancelError}</p>}
      {moving && <MoveForm view={view} onDone={() => setMoving(false)} onMove={move} />}
      <WorkflowOutcomes taskKey={task.key} view={view} target={target} onChanged={changed} onRefresh={refresh} />
      <WorkflowArtifacts taskKey={task.key} artifacts={view.artifacts} declared={view.declared_artifacts ?? []}
        editable={editable && !closed} target={target} onChanged={changed} />
      <WorkflowRuns taskKey={task.key} runs={view.runs ?? []} target={target} />
      <section className="flex min-w-0 flex-col gap-1">
        <span className={LABEL}>Visits</span>
        <ol className="flex min-w-0 flex-col">
          {[...view.visits].sort((a, b) => a.sequence - b.sequence).map((visit) => (
            <li key={visit.id} className={cn(ROW, "flex-wrap py-1")}>
              <span className={cn(MONO_ID, "font-medium")}>{visit.status}</span>
              <span className={cn(MONO, "text-muted-foreground")}>{formatTaskTime(visit.entered_at)} · {visit.entered_by}</span>
              {visit.left_at
                ? <span className="text-[12px] text-muted-foreground">
                  {visit.outcome ? <>left by <span className={MONO}>{visit.outcome}</span></> : "left"}</span>
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
 * The statuses a move may target: every declared status but the current one,
 * the terminal ones last. A move to a terminal status is the operator's
 * decision, so they stay on offer.
 */
function moveTargets(view: WorkflowView) {
  const others = (view.statuses ?? []).filter((status) => status.id !== view.status)
  return { open: others.filter((status) => !status.terminal), terminal: others.filter((status) => status.terminal) }
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
  const mountedRef = useMounted()
  const { open, terminal } = moveTargets(view)
  const submit = () => {
    setError("")
    setBusy(true)
    onMove(to, reason.trim())
      .then((moved) => { if (moved && mountedRef.current) onDone() }, (failure) => { if (mountedRef.current) setError(errorText(failure)) })
      .finally(() => { if (mountedRef.current) setBusy(false) })
  }
  return (
    <form className="flex min-w-0 flex-col gap-1.5 rounded-[8px] bg-muted/50 p-2.5"
      onSubmit={(event) => { event.preventDefault(); submit() }}>
      <span className={LABEL}>Move to status</span>
      <div className="flex min-w-0 flex-wrap items-center gap-1.5">
        <SelectShell autoFocus aria-label="Target status" value={to} className="h-[26px] w-auto max-w-full text-[12px] md:text-[12px]"
          onChange={(event) => setTo(event.target.value)}>
          <option value="">Choose a status</option>
          {open.map((status) => <option key={status.id} value={status.id}>{status.id}</option>)}
          {terminal.length > 0 && <optgroup label="Terminal">
            {terminal.map((status) => <option key={status.id} value={status.id}>{status.id}</option>)}
          </optgroup>}
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
