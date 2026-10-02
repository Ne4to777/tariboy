import { ArrowRight } from "lucide-react"
import { useEffect, useRef, useState } from "react"
import { Button } from "@/components/ui/button"
import { Textarea } from "@/components/ui/textarea"
import { ApiError, type ApiTarget } from "@/lib/api"
import { advanceTask, getTransitionRequest, type TransitionRequest, type WorkflowView } from "@/lib/tasks"
import { cn } from "@/lib/utils"
import { DANGER_FILL, EMPTY, LABEL, MONO } from "./panelStyles"
import { errorText } from "./WorkflowConfirm"

/** How long to wait for a pending request when the daemon does not say. */
const DEFAULT_WAIT_SECONDS = 90
const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms))
const RESULT_LABEL: Record<string, string> = { rejected: "Rejected", failed: "Failed", cancelled: "Cancelled" }

type Notice = { kind: "result"; request: TransitionRequest } | { kind: "error"; message: string } | { kind: "timeout" }

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
  const [notice, setNotice] = useState<Notice | null>(null)
  const mounted = useRef(true)
  useEffect(() => {
    mounted.current = true
    return () => { mounted.current = false }
  }, [])

  const advance = async (outcome: string) => {
    setChecking(true)
    setNotice(null)
    try {
      let request = await advanceTask(taskKey, outcome, message, view.status, target)
      // Poll once a second for as long as the checks may take.
      let polls = Math.ceil(request.wait_seconds ?? DEFAULT_WAIT_SECONDS)
      while (request.state === "pending") {
        if (polls-- <= 0) {
          if (mounted.current) setNotice({ kind: "timeout" })
          return
        }
        await sleep(1000)
        if (!mounted.current) return
        request = await getTransitionRequest(taskKey, request.id, target)
      }
      if (!mounted.current) return
      if (request.state === "applied") {
        setMessage("")
        onChanged()
      } else {
        setNotice({ kind: "result", request })
        onRefresh()
      }
    } catch (error) {
      if (!mounted.current) return
      setNotice({ kind: "error", message: errorText(error) })
      if (error instanceof ApiError && (error.code === "status_changed" || error.code === "transition_pending")) onRefresh()
    } finally {
      if (mounted.current) setChecking(false)
    }
  }

  const last = view.last_request
  const shown: Notice | null = notice
    ?? (last && (last.state === "rejected" || last.state === "failed") ? { kind: "result", request: last } : null)
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
              <ArrowRight aria-hidden="true" className="size-3 text-muted-foreground" />
              <span className={MONO}>{outcome.to}</span>
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
        value={message} disabled={checking} onChange={(event) => setMessage(event.target.value)}
        className="min-h-12 rounded-[8px] border-0 bg-muted text-[12.5px] md:text-[12.5px]" />}
      {(checking || pendingElsewhere) && <span role="status" className={EMPTY}>checking…</span>}
      {shown && <OutcomeNotice notice={shown} onRefresh={onRefresh} />}
    </section>
  )
}

function OutcomeNotice({ notice, onRefresh }: { notice: Notice; onRefresh: () => void }) {
  if (notice.kind === "timeout") {
    return <div role="status" className="flex items-center gap-2 text-[12px] text-muted-foreground">
      Still checking — the result appears here when the checks finish.
      <Button type="button" variant="ghost" className="h-6 px-2 text-[12px]" onClick={onRefresh}>Refresh</Button>
    </div>
  }
  const text = notice.kind === "error" ? notice.message : notice.request.result_message
  const label = notice.kind === "error" ? "Refused" : RESULT_LABEL[notice.request.state] ?? notice.request.state
  return <div role="alert" className={cn("flex min-w-0 flex-col gap-1 rounded-[8px] px-2.5 py-2 text-[12px]", DANGER_FILL)}>
    <span className="font-medium">{label}{notice.kind === "result" ? ` · ${notice.request.outcome}` : ""}</span>
    {text && <pre className="min-w-0 font-mono text-[11.5px] whitespace-pre-wrap break-words">{text}</pre>}
  </div>
}
