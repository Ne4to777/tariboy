import { useState } from "react"
import { Button } from "@/components/ui/button"
import { Textarea } from "@/components/ui/textarea"
import type { ApiTarget } from "@/lib/api"
import { getTaskArtifactHistory, setTaskArtifact, type WorkflowArtifact, type WorkflowDeclaredArtifact } from "@/lib/tasks"
import { cn } from "@/lib/utils"
import { COUNT, EMPTY, LABEL, MONO, QUIET_ACTION } from "./panelStyles"
import { MarkdownContent, MarkdownModeSegment, type MarkdownMode } from "./TaskMarkdown"
import { formatTaskTime } from "./taskTime"
import { errorText } from "./workflowShared"

/** A value longer than this many lines, or characters, starts collapsed. */
const COLLAPSED_LINES = 6
const COLLAPSED_CHARS = 600

/**
 * The task's artifacts: the current value of each, its author and time, the
 * history on demand, and for the customer an edit action. The daemon never
 * parses a value: it renders as Markdown, or as plain text when switched.
 */
export default function WorkflowArtifacts({ taskKey, artifacts, declared, editable, target, onChanged }: {
  taskKey: string
  artifacts: WorkflowArtifact[]
  /** Every artifact the manifest declares, so one with no value can be set. */
  declared: WorkflowDeclaredArtifact[]
  editable: boolean
  target?: ApiTarget
  onChanged: () => void
}) {
  const unset = declared.filter((item) => !artifacts.some((artifact) => artifact.name === item.name))
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
        {unset.map((item) => <ArtifactItem key={item.name} taskKey={taskKey} name={item.name}
          description={item.description} editable={editable} target={target} onChanged={onChanged} />)}
      </ul>
    </section>
  )
}

function ArtifactItem({ taskKey, name, artifact, description, editable, target, onChanged }: {
  taskKey: string
  name: string
  artifact?: WorkflowArtifact
  /** The manifest's description, shown while the artifact has no value. */
  description?: string
  editable: boolean
  target?: ApiTarget
  onChanged: () => void
}) {
  const [draft, setDraft] = useState<string | null>(null)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState("")
  const [history, setHistory] = useState<WorkflowArtifact[] | null>(null)
  const [historyError, setHistoryError] = useState("")
  const [mode, setMode] = useState<MarkdownMode>("rich")
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
          <span className={cn(MONO, "min-w-0 break-all text-muted-foreground")}>{artifact.author}</span>
          <time dateTime={artifact.created_at} className={cn(MONO, "text-muted-foreground")}>{formatTaskTime(artifact.created_at)}</time>
        </> : <span className={EMPTY}>no value</span>}
        <span className="ml-auto flex items-center gap-1">
          {artifact && draft === null && <MarkdownModeSegment label={`Show ${name} as`} names={["Markdown", "Text"]}
            mode={mode} onModeChange={setMode} />}
          {artifact && <Button type="button" variant="ghost" className={QUIET_ACTION} aria-expanded={history !== null}
            onClick={toggleHistory}>History</Button>}
          {editable && draft === null && <Button type="button" variant="ghost" className={QUIET_ACTION}
            onClick={() => setDraft(artifact?.value ?? "")}>{artifact ? "Edit" : "Set"}</Button>}
        </span>
      </div>
      {!artifact && description && <span className="min-w-0 text-[12px] break-words text-muted-foreground">{description}</span>}
      {draft !== null ? <div className="flex min-w-0 flex-col gap-1.5">
        <Textarea aria-label={`Value of ${name}`} value={draft} disabled={saving} onChange={(event) => setDraft(event.target.value)}
          className="min-h-20 rounded-[8px] border-0 bg-muted font-mono text-[11.5px] md:text-[11.5px]" />
        <div className="flex gap-1.5">
          <Button type="button" size="sm" className="h-7" disabled={saving || !draft} onClick={save}>Save</Button>
          <Button type="button" variant="ghost" size="sm" className="h-7" disabled={saving}
            onClick={() => { setDraft(null); setError("") }}>Discard</Button>
        </div>
      </div> : artifact && <ArtifactValue value={artifact.value} markdown={mode === "rich"} />}
      {error && <p role="alert" className="text-[12px] text-status-failed">{error}</p>}
      {historyError && <p role="alert" className="text-[12px] text-status-failed">{historyError}</p>}
      {history && <ol className="flex min-w-0 flex-col gap-1.5 border-l border-border pl-2.5">
        {history.map((entry) => <li key={entry.id} className="flex min-w-0 flex-col gap-0.5">
          <span className={cn(MONO, "text-muted-foreground")}>{entry.author} · {formatTaskTime(entry.created_at)}</span>
          <ArtifactValue value={entry.value} markdown={mode === "rich"} />
        </li>)}
      </ol>}
    </li>
  )
}

function ArtifactValue({ value, markdown }: { value: string, markdown: boolean }) {
  const [expanded, setExpanded] = useState(false)
  const lines = value.split("\n")
  const manyLines = lines.length > COLLAPSED_LINES
  const short = lines.slice(0, COLLAPSED_LINES).join("\n").slice(0, COLLAPSED_CHARS)
  const long = short.length < value.length
  const shown = long && !expanded ? short : value
  return <div className="flex min-w-0 flex-col items-start gap-0.5">
    {markdown
      ? <div className="w-full min-w-0 rounded-[8px] bg-muted px-2.5 py-2"><MarkdownContent>{shown}</MarkdownContent></div>
      : <pre className="w-full min-w-0 rounded-[8px] bg-muted px-2.5 py-2 font-mono text-[11.5px] whitespace-pre-wrap break-words">{shown}</pre>}
    {long && <Button type="button" variant="ghost" className={QUIET_ACTION} aria-expanded={expanded}
      onClick={() => setExpanded(!expanded)}>
      {expanded ? "Show less" : manyLines ? `Show all ${lines.length} lines` : `Show all ${value.length} characters`}
    </Button>}
  </div>
}
