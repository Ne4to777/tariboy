import { useState, type ReactNode } from "react"
import { Button } from "@/components/ui/button"
import { cn } from "@/lib/utils"
import { EMPTY, MONO, QUIET_ACTION } from "./panelStyles"
import { formatTaskTime } from "./taskTime"
import { errorText } from "./workflowShared"

/**
 * One script run: a task's check or watch, or a queue's source. A log is read
 * only when asked for, through readLog, and shown as text: it is the script's
 * own output. A run still going keeps writing its log, so its open log has
 * Refresh.
 */
export default function RunRow({ head, state, verdict, exitCode, startedAt, finishedAt, message, going, readLog }: {
  /** The leading cells: what ran. */
  head: ReactNode
  state: string
  verdict?: string
  exitCode?: number | null
  startedAt: string
  finishedAt?: string
  message?: string
  going: boolean
  readLog: () => Promise<{ text: string; truncated: boolean }>
}) {
  const [log, setLog] = useState<{ text: string; truncated: boolean } | null>(null)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState("")
  const read = () => {
    setLoading(true)
    setError("")
    readLog()
      .then(setLog)
      .catch((failure) => setError(errorText(failure)))
      .finally(() => setLoading(false))
  }
  const toggleLog = () => {
    if (log) { setLog(null); return }
    read()
  }
  return (
    <li className="flex min-w-0 flex-col gap-1">
      <div className="flex min-w-0 flex-wrap items-center gap-x-2.5 gap-y-0.5">
        {head}
        <span className={MONO}>{state}</span>
        {verdict && <span className={MONO}>{verdict}</span>}
        {exitCode !== undefined && exitCode !== null && <span className={MONO}>exit {exitCode}</span>}
        <time dateTime={startedAt} className={cn(MONO, "text-muted-foreground")}>{formatTaskTime(startedAt)}</time>
        {finishedAt && <time dateTime={finishedAt} className={cn(MONO, "text-muted-foreground")}>
          → {formatTaskTime(finishedAt)}</time>}
        <span className="ml-auto flex gap-1">
          {log && going && <Button type="button" variant="ghost" className={QUIET_ACTION} disabled={loading}
            aria-label="Refresh log" onClick={read}>Refresh</Button>}
          <Button type="button" variant="ghost" className={QUIET_ACTION} disabled={loading}
            aria-expanded={log !== null} onClick={toggleLog}>{log ? "Hide log" : "Log"}</Button>
        </span>
      </div>
      {message && <span className="min-w-0 text-[12px] break-words whitespace-pre-wrap text-muted-foreground">{message}</span>}
      {error && <p role="alert" className="text-[12px] text-status-failed">{error}</p>}
      {log && <div className="flex min-w-0 flex-col gap-0.5">
        {log.truncated && <span className={EMPTY}>The log is truncated; only its end is shown.</span>}
        <pre tabIndex={0} className="max-h-80 min-w-0 overflow-auto rounded-[8px] bg-muted px-2.5 py-2 font-mono text-[11px] whitespace-pre-wrap break-words">{log.text}</pre>
      </div>}
    </li>
  )
}
