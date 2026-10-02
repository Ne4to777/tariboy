import { useState } from "react"
import { Button } from "@/components/ui/button"
import { Textarea } from "@/components/ui/textarea"
import type { ApiTarget } from "@/lib/api"
import { getTaskArtifactHistory, setTaskArtifact, type WorkflowArtifact } from "@/lib/tasks"
import { cn } from "@/lib/utils"
import { COUNT, EMPTY, LABEL, MONO, QUIET_ACTION } from "./panelStyles"
import { formatTaskTime } from "./taskTime"
import { errorText } from "./WorkflowConfirm"

/** A value longer than this many lines starts collapsed. */
const COLLAPSED_LINES = 6

/**
 * The task's artifacts: the current value of each, its author and time, the
 * history on demand, and for the customer an edit action. Values are text the
 * daemon never parses, so they render as preformatted text, never as Markdown.
 */
export default function WorkflowArtifacts({ taskKey, artifacts, missing, editable, target, onChanged }: {
  taskKey: string
  artifacts: WorkflowArtifact[]
  /** Required names with no value yet, so the customer can set them. */
  missing: string[]
  editable: boolean
  target?: ApiTarget
  onChanged: () => void
}) {
  const unset = missing.filter((name, index) => missing.indexOf(name) === index && !artifacts.some((item) => item.name === name))
  return (
    <section className="flex min-w-0 flex-col gap-1.5">
      <div className="flex items-center gap-1.5">
        <span className={LABEL}>Artifacts</span>
        <span className={COUNT}>{artifacts.length}</span>
      </div>
      {artifacts.length === 0 && unset.length === 0 && <span className={EMPTY}>No artifact has a value yet.</span>}
      <ul className="flex min-w-0 flex-col gap-2.5">
        {artifacts.map((artifact) => <ArtifactItem key={artifact.name} taskKey={taskKey} name={artifact.name}
          artifact={artifact} editable={editable} target={target} onChanged={onChanged} />)}
        {unset.map((name) => <ArtifactItem key={name} taskKey={taskKey} name={name}
          editable={editable} target={target} onChanged={onChanged} />)}
      </ul>
    </section>
  )
}

function ArtifactItem({ taskKey, name, artifact, editable, target, onChanged }: {
  taskKey: string
  name: string
  artifact?: WorkflowArtifact
  editable: boolean
  target?: ApiTarget
  onChanged: () => void
}) {
  const [draft, setDraft] = useState<string | null>(null)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState("")
  const [history, setHistory] = useState<WorkflowArtifact[] | null>(null)
  const [historyError, setHistoryError] = useState("")
  const save = () => {
    if (draft === null) return
    setSaving(true)
    setError("")
    setTaskArtifact(taskKey, name, draft, target)
      .then(() => { setDraft(null); setHistory(null); onChanged() })
      .catch((failure) => setError(errorText(failure)))
      .finally(() => setSaving(false))
  }
  const toggleHistory = () => {
    if (history) { setHistory(null); return }
    setHistoryError("")
    getTaskArtifactHistory(taskKey, name, target)
      .then(setHistory)
      .catch((failure) => setHistoryError(errorText(failure)))
  }
  return (
    <li className="flex min-w-0 flex-col gap-1">
      <div className="flex min-w-0 items-center gap-2">
        <span className={cn(MONO, "font-medium")}>{name}</span>
        {artifact ? <>
          <span className={cn(MONO, "text-muted-foreground")}>{artifact.author}</span>
          <time dateTime={artifact.created_at} className={cn(MONO, "text-muted-foreground")}>{formatTaskTime(artifact.created_at)}</time>
        </> : <span className={EMPTY}>no value</span>}
        <span className="ml-auto flex gap-1">
          {artifact && <Button type="button" variant="ghost" className={QUIET_ACTION} aria-expanded={history !== null}
            onClick={toggleHistory}>History</Button>}
          {editable && draft === null && <Button type="button" variant="ghost" className={QUIET_ACTION}
            onClick={() => setDraft(artifact?.value ?? "")}>{artifact ? "Edit" : "Set"}</Button>}
        </span>
      </div>
      {draft !== null ? <div className="flex min-w-0 flex-col gap-1.5">
        <Textarea aria-label={`Value of ${name}`} value={draft} disabled={saving} onChange={(event) => setDraft(event.target.value)}
          className="min-h-20 rounded-[8px] border-0 bg-muted font-mono text-[11.5px] md:text-[11.5px]" />
        <div className="flex gap-1.5">
          <Button type="button" size="sm" className="h-7" disabled={saving || !draft} onClick={save}>Save</Button>
          <Button type="button" variant="ghost" size="sm" className="h-7" disabled={saving}
            onClick={() => { setDraft(null); setError("") }}>Discard</Button>
        </div>
      </div> : artifact && <ArtifactValue value={artifact.value} />}
      {error && <p role="alert" className="text-[12px] text-status-failed">{error}</p>}
      {historyError && <p role="alert" className="text-[12px] text-status-failed">{historyError}</p>}
      {history && <ol className="flex min-w-0 flex-col gap-1.5 border-l border-border pl-2.5">
        {history.map((entry) => <li key={entry.id} className="flex min-w-0 flex-col gap-0.5">
          <span className={cn(MONO, "text-muted-foreground")}>{entry.author} · {formatTaskTime(entry.created_at)}</span>
          <ArtifactValue value={entry.value} />
        </li>)}
      </ol>}
    </li>
  )
}

function ArtifactValue({ value }: { value: string }) {
  const [expanded, setExpanded] = useState(false)
  const lines = value.split("\n")
  const long = lines.length > COLLAPSED_LINES
  return <div className="flex min-w-0 flex-col items-start gap-0.5">
    <pre className="w-full min-w-0 overflow-x-auto rounded-[8px] bg-muted px-2.5 py-2 font-mono text-[11.5px] whitespace-pre-wrap break-words">
      {long && !expanded ? lines.slice(0, COLLAPSED_LINES).join("\n") : value}
    </pre>
    {long && <Button type="button" variant="ghost" className={QUIET_ACTION} onClick={() => setExpanded(!expanded)}>
      {expanded ? "Show less" : `Show all ${lines.length} lines`}
    </Button>}
  </div>
}
