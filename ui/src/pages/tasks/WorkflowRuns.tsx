import type { ApiTarget } from "@/lib/api"
import { getTaskScriptRunLog, type ScriptRun } from "@/lib/tasks"
import { cn } from "@/lib/utils"
import { COUNT, EMPTY, LABEL, MONO } from "./panelStyles"
import RunRow from "./RunRow"

/**
 * The task's script runs, newest first. The view carries only the latest 20.
 * A log is read only when asked for, at the daemon's default size.
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
        <span className="text-[11.5px] text-muted-foreground">latest 20</span>
      </div>
      {sorted.length === 0 && <span className={EMPTY}>No script has run yet.</span>}
      <ul className="flex min-w-0 flex-col gap-1">
        {sorted.map((run) => (
          <RunRow key={run.id}
            head={<>
              <span className={cn(MONO, "font-medium")}>{run.kind}</span>
              <span className={cn(MONO, "min-w-0 truncate")} title={run.script}>{run.script}</span>
            </>}
            state={run.state} verdict={run.verdict} exitCode={run.exit_code}
            startedAt={run.started_at ?? run.created_at} finishedAt={run.finished_at} message={run.message}
            going={run.state === "pending" || run.state === "running"}
            readLog={() => getTaskScriptRunLog(taskKey, run.id, undefined, target)} />
        ))}
      </ul>
    </section>
  )
}
