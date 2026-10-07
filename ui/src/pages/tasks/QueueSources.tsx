import { useCallback, useEffect, useState } from "react"
import { Button } from "@/components/ui/button"
import type { ApiTarget } from "@/lib/api"
import { getQueueSourceRunLog, listQueueSources, type QueueSource } from "@/lib/tasks"
import { cn } from "@/lib/utils"
import { COUNT, EMPTY, LABEL, MONO, QUIET_ACTION } from "./panelStyles"
import RunRow from "./RunRow"
import { formatTaskTime } from "./taskTime"
import { errorText } from "./workflowShared"

/**
 * The sources of a queue's bound workflow: each with its schedule and its
 * newest runs (at most 20, newest first), whose logs are read on demand. The
 * list is read once and on Reload; it does not poll.
 */
export default function QueueSources({ queue, target }: { queue: string; target?: ApiTarget }) {
  const [sources, setSources] = useState<QueueSource[]>([])
  // Until a load succeeds the list is unknown, so "No sources" would be a guess.
  const [loaded, setLoaded] = useState(false)
  const [error, setError] = useState("")
  const [busy, setBusy] = useState(false)

  const load = useCallback(async () => {
    setBusy(true)
    try {
      setSources(await listQueueSources(queue, target))
      setLoaded(true)
      setError("")
    } catch (err) { setError(errorText(err)) } finally { setBusy(false) }
  }, [queue, target])
  useEffect(() => { void Promise.resolve().then(load) }, [load])

  return (
    <section className="flex min-w-0 flex-col gap-1.5" aria-label={`Sources ${queue}`}>
      <div className="flex items-center gap-1.5">
        <span className={LABEL}>Sources</span>
        <span className={COUNT}>{sources.length}</span>
        <Button type="button" variant="ghost" className={cn(QUIET_ACTION, "ml-auto")} disabled={busy}
          aria-label="Reload sources" onClick={() => void load()}>Reload</Button>
      </div>
      {error && (
        <div role="alert" className="flex items-center gap-2 text-[12px] text-destructive">
          <span>{error}</span>
          <Button type="button" size="sm" variant="secondary" className="h-[26px]" disabled={busy} onClick={() => void load()}>Retry</Button>
        </div>
      )}
      {loaded && sources.length === 0 && <span className={EMPTY}>No sources</span>}
      {sources.map((source) => (
        <div key={source.name} role="group" aria-label={`Source ${source.name}`} className="flex min-w-0 flex-col gap-1">
          <div className="flex min-w-0 flex-wrap items-center gap-x-2.5 gap-y-0.5">
            <span className={cn(MONO, "font-medium")}>{source.name}</span>
            <span className={cn(MONO, "min-w-0 truncate")} title={source.script}>{source.script}</span>
            <span className={MONO}>every {source.every}</span>
            {source.next_run_at && <span className={cn(MONO, "text-muted-foreground")}>
              next <time dateTime={source.next_run_at}>{formatTaskTime(source.next_run_at)}</time></span>}
            {source.failures > 0 && <span className={cn(MONO, "text-status-failed")}>{source.failures} failed in a row</span>}
          </div>
          {(source.runs ?? []).length === 0 && <span className={EMPTY}>No run yet.</span>}
          <ul className="flex min-w-0 flex-col gap-1">
            {(source.runs ?? []).map((run) => (
              <RunRow key={run.id}
                head={<span className={cn(MONO, "text-muted-foreground")}>#{run.id}</span>}
                state={run.state} verdict={run.verdict} exitCode={run.exit_code}
                startedAt={run.started_at} finishedAt={run.finished_at}
                message={[run.tasks_created > 0 ? `${run.tasks_created} tasks created` : "", run.message ?? ""].filter(Boolean).join("\n")}
                going={run.state === "running"}
                readLog={() => getQueueSourceRunLog(queue, run.id, undefined, target)} />
            ))}
          </ul>
        </div>
      ))}
    </section>
  )
}
