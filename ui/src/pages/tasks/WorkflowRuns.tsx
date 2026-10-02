import { useState } from "react"
import { Button } from "@/components/ui/button"
import type { ApiTarget } from "@/lib/api"
import { getTaskScriptRunLog, type ScriptRun } from "@/lib/tasks"
import { cn } from "@/lib/utils"
import { COUNT, EMPTY, LABEL, MONO, QUIET_ACTION } from "./panelStyles"
import { formatTaskTime } from "./taskTime"
import { errorText } from "./WorkflowConfirm"

/**
 * The task's script runs, newest first. A log is read only when asked for, at
 * the daemon's default size, and shown as text: it is the script's own output.
 */
export default function WorkflowRuns({ taskKey, runs, target }: {
  taskKey: string
  runs: ScriptRun[]
  target?: ApiTarget
}) {
  const sorted = [...runs].sort((a, b) => b.id - a.id)
  return (
    <section className="flex min-w-0 flex-col gap-1.5">
      <div className="flex items-center gap-1.5">
        <span className={LABEL}>Script runs</span>
        <span className={COUNT}>{runs.length}</span>
      </div>
      {sorted.length === 0 && <span className={EMPTY}>No script has run yet.</span>}
      <ul className="flex min-w-0 flex-col gap-1">
        {sorted.map((run) => <RunItem key={run.id} taskKey={taskKey} run={run} target={target} />)}
      </ul>
    </section>
  )
}

function RunItem({ taskKey, run, target }: { taskKey: string; run: ScriptRun; target?: ApiTarget }) {
  const [log, setLog] = useState<{ text: string; truncated: boolean } | null>(null)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState("")
  const toggleLog = () => {
    if (log) { setLog(null); return }
    setLoading(true)
    setError("")
    getTaskScriptRunLog(taskKey, run.id, undefined, target)
      .then(setLog)
      .catch((failure) => setError(errorText(failure)))
      .finally(() => setLoading(false))
  }
  const started = run.started_at ?? run.created_at
  return (
    <li className="flex min-w-0 flex-col gap-1">
      <div className="flex min-w-0 flex-wrap items-center gap-x-2.5 gap-y-0.5">
        <span className={cn(MONO, "font-medium")}>{run.kind}</span>
        <span className={cn(MONO, "min-w-0 truncate")} title={run.script}>{run.script}</span>
        <span className={MONO}>{run.state}</span>
        {run.verdict && <span className={MONO}>{run.verdict}</span>}
        {run.exit_code !== undefined && run.exit_code !== null && <span className={MONO}>exit {run.exit_code}</span>}
        <time dateTime={started} className={cn(MONO, "text-muted-foreground")}>{formatTaskTime(started)}</time>
        {run.finished_at && <time dateTime={run.finished_at} className={cn(MONO, "text-muted-foreground")}>
          → {formatTaskTime(run.finished_at)}</time>}
        <Button type="button" variant="ghost" className={cn(QUIET_ACTION, "ml-auto")} disabled={loading}
          aria-expanded={log !== null} onClick={toggleLog}>{log ? "Hide log" : "Log"}</Button>
      </div>
      {run.message && <span className="min-w-0 text-[12px] break-words whitespace-pre-wrap text-muted-foreground">{run.message}</span>}
      {error && <p role="alert" className="text-[12px] text-status-failed">{error}</p>}
      {log && <div className="flex min-w-0 flex-col gap-0.5">
        {log.truncated && <span className={EMPTY}>The log is truncated; only its end is shown.</span>}
        <pre className="max-h-80 min-w-0 overflow-auto rounded-[8px] bg-muted px-2.5 py-2 font-mono text-[11px] whitespace-pre-wrap break-words">{log.text}</pre>
      </div>}
    </li>
  )
}
