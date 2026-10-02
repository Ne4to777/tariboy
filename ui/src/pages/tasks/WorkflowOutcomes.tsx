import { ArrowRight } from "lucide-react"
import { useState } from "react"
import { Button } from "@/components/ui/button"
import { Textarea } from "@/components/ui/textarea"
import { ApiError, type ApiTarget } from "@/lib/api"
import { advanceTask, getTransitionRequest, type TransitionRequest, type WorkflowView } from "@/lib/tasks"
import { cn } from "@/lib/utils"
import { DANGER_FILL, EMPTY, LABEL, MONO } from "./panelStyles"
import { errorText, useMounted } from "./workflowShared"

/** How long to wait for a pending request when the daemon does not say. */
const DEFAULT_WAIT_SECONDS = 90
const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms))
const RESULT_LABEL: Record<string, string> = { rejected: "Rejected", failed: "Failed", cancelled: "Cancelled" }

type Notice = { kind: "result"; request: TransitionRequest } | { kind: "error"; label: string; message: string } | { kind: "timeout" }
/** A refusal that means the view is stale, so the panel refetches it. */
const STALE_CODES = new Set(["status_changed", "transition_pending", "workflow_paused", "artifact_missing"])

/**
 * The outcomes of the current status. The Desktop acts as the customer, so a
 * customer status gets one button per outcome; a pool or script status is
 * listed read-only, since its holder or its watch script decides.
 */
export default function WorkflowOutcomes({ taskKey, view, target, onChanged, onRefresh }: {
  taskKey: string
  view: WorkflowView
  target?: ApiTarget
  /** The transition applied: refetch the view and the task. */
  onChanged: () => void
  /** Refetch the view only. */
  onRefresh: () => void
}) {
  const customer = view.owner === "customer"
  const paused = view.waiting_on === "pause"
  const [message, setMessage] = useState("")
  const [checking, setChecking] = useState(false)
  // A notice belongs to the status it was given in; once the view shows
  // another status it no longer applies and is not shown.
  const [held, setHeld] = useState<{ status: string; notice: Notice } | null>(null)
  const notice = held?.status === view.status ? held.notice : null
  const setNotice = (next: Notice | null, status = view.status) => setHeld(next && { status, notice: next })
  const mountedRef = useMounted()

  const advance = async (outcome: string) => {
    setChecking(true)
    setNotice(null)
    try {
      let request = await advanceTask(taskKey, outcome, message, view.status, target)
      // Poll once a second for as long as the checks may take.
      let polls = Math.ceil(request.wait_seconds ?? DEFAULT_WAIT_SECONDS)
      while (request.state === "pending") {
        if (polls-- <= 0) {
          if (mountedRef.current) setNotice({ kind: "timeout" })
          return
        }
        await sleep(1000)
        if (!mountedRef.current) return
        try {
          request = await getTransitionRequest(taskKey, request.id, target)
        } catch (error) {
          // The request may still be applying; only reading it failed.
          if (mountedRef.current) setNotice({ kind: "error", label: "Could not read the request", message: errorText(error) })
          return
        }
      }
      if (!mountedRef.current) return
      if (request.state === "applied") {
        setMessage("")
        onChanged()
      } else {
        setNotice({ kind: "result", request })
        onRefresh()
      }
    } catch (error) {
      if (!mountedRef.current) return
      const stale = error instanceof ApiError && STALE_CODES.has(error.code)
      // status_changed names the status the task is in now, where the
      // refetched view lands, so its message survives that refetch.
      const now = stale && typeof error.details?.status === "string" ? error.details.status : view.status
      setNotice({ kind: "error", label: "Refused", message: errorText(error) }, now)
      if (stale) onRefresh()
    } finally {
      if (mountedRef.current) setChecking(false)
    }
  }

  const last = view.last_request
  // A timeout says the result is still to come; once the view has the
  // request settled, it no longer applies.
  const current = notice?.kind === "timeout" && last && last.state !== "pending" ? null : notice
  const shown: Notice | null = current
    ?? (last && (last.state === "rejected" || last.state === "failed") && inCurrentVisit(view, last)
      ? { kind: "result", request: last } : null)
  const pendingElsewhere = !checking && last?.state === "pending"
  const disabled = checking || paused || pendingElsewhere

  return (
    <section className="flex min-w-0 flex-col gap-1.5">
      <span className={LABEL}>Outcomes</span>
      {view.outcomes.length === 0 && <span className={EMPTY}>This status has no outcomes.</span>}
      {!customer && view.outcomes.length > 0 && <span className={EMPTY}>
        {view.owner === "script" ? "The status's watch script decides." : "The agent decides; the outcomes are listed for reference."}
      </span>}
      <ul className="flex min-w-0 flex-col gap-1.5">
        {view.outcomes.map((outcome) => {
          const missing = new Set(outcome.missing ?? [])
          return (
            <li key={outcome.on} className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1">
              {customer
                ? <Button type="button" size="sm" className="h-7" disabled={disabled || missing.size > 0}
                  onClick={() => void advance(outcome.on)}>{outcome.on}</Button>
                : <span className={cn(MONO, "font-medium")}>{outcome.on}</span>}
              {customer && missing.size > 0 && <span className="text-[11.5px] text-status-failed">needs: {[...missing].join(", ")}</span>}
              <ArrowRight aria-hidden="true" className="size-3 text-muted-foreground" />
              <span className={cn(MONO, "min-w-0 break-all")}>{outcome.to}</span>
              {(outcome.requires ?? []).length > 0 && <span className="text-[11.5px] text-muted-foreground">requires</span>}
              {(outcome.requires ?? []).map((name) => missing.has(name)
                ? <span key={name} data-missing="true" title="No value yet"
                  className={cn(MONO, "rounded-[5px] px-1.5", DANGER_FILL)}>{name}</span>
                : <span key={name} className={MONO}>{name}</span>)}
              {(outcome.checks ?? []).length > 0 && <span className="text-[11.5px] text-muted-foreground">checks</span>}
              {(outcome.checks ?? []).map((script) => <span key={script} className={cn(MONO, "text-muted-foreground")}>{script}</span>)}
            </li>
          )
        })}
      </ul>
      {customer && view.outcomes.length > 0 && <Textarea aria-label="Outcome message" placeholder="Message (optional)"
        value={message} disabled={checking} maxLength={4096} onChange={(event) => setMessage(event.target.value)}
        className="min-h-12 rounded-[8px] border-0 bg-muted text-[12.5px] md:text-[12.5px]" />}
      {(checking || pendingElsewhere) && <div className="flex items-center gap-2">
        <span role="status" className={EMPTY}>checking…</span>
        {pendingElsewhere && <Button type="button" variant="ghost" className="h-6 px-2 text-[12px]" onClick={onRefresh}>Refresh</Button>}
      </div>}
      {shown && <OutcomeNotice notice={shown} onRefresh={onRefresh} />}
    </section>
  )
}

/**
 * Whether a request was made in the open visit of an open task. A rejection
 * from an earlier visit, or on a closed task, says nothing about what to do now.
 */
function inCurrentVisit(view: WorkflowView, request: TransitionRequest): boolean {
  if (view.category === "done" || view.category === "cancelled") return false
  const open = view.visits.find((visit) => !visit.left_at)
  if (!open) return false
  return Date.parse(request.created_at) >= Date.parse(open.entered_at)
}

function OutcomeNotice({ notice, onRefresh }: { notice: Notice; onRefresh: () => void }) {
  if (notice.kind === "timeout") {
    return <div role="status" className="flex items-center gap-2 text-[12px] text-muted-foreground">
      Still checking — the result appears here when the checks finish.
      <Button type="button" variant="ghost" className="h-6 px-2 text-[12px]" onClick={onRefresh}>Refresh</Button>
    </div>
  }
  const text = notice.kind === "error" ? notice.message : notice.request.result_message
  const label = notice.kind === "error" ? notice.label : RESULT_LABEL[notice.request.state] ?? notice.request.state
  return <div role="alert" className={cn("flex min-w-0 flex-col gap-1 rounded-[8px] px-2.5 py-2 text-[12px]", DANGER_FILL)}>
    <span className="font-medium">{label}{notice.kind === "result" ? ` · ${notice.request.outcome}` : ""}</span>
    {text && <pre className="min-w-0 font-mono text-[11.5px] whitespace-pre-wrap break-words">{text}</pre>}
  </div>
}
