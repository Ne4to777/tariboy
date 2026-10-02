import { useCallback, useEffect, useMemo, useState, type ReactNode } from "react"
import { Button } from "@/components/ui/button"
import { ApiError, type ApiTarget } from "@/lib/api"
import {
  clearQueueWorkflow, getQueueWorkflow, listWorkflowImages, setQueueWorkflow,
  type QueueWorkflow, type WorkflowImage,
} from "@/lib/tasks"
import { EMPTY, LABEL, MONO, SelectShell } from "./panelStyles"
import QueueSecrets from "./QueueSecrets"
import { errorText, useWorkflowConfirm } from "./WorkflowConfirm"

function detailList(error: ApiError, name: string): string[] {
  const list = error.details?.[name]
  return Array.isArray(list) ? list.map(String) : []
}

/** Images by name, then tag with `latest` first. */
function sortImages(images: WorkflowImage[]): WorkflowImage[] {
  const rank = (image: WorkflowImage) => (image.tag === "latest" ? 0 : 1)
  return [...images].sort((a, b) => a.name.localeCompare(b.name) || rank(a) - rank(b) || a.tag.localeCompare(b.tag))
}

/**
 * The Workflow section of a queue: the bound image, a select to bind another,
 * the pools control (the existing editor, passed as `pools`), and secrets. A
 * bind refused for empty pools or missing secrets lists them beside the
 * control that fixes them.
 */
export default function QueueWorkflowSettings({ queue, target, pools }: {
  queue: string
  target?: ApiTarget
  pools: ReactNode
}) {
  const [binding, setBinding] = useState<QueueWorkflow | null>(null)
  // True only after a load succeeded: until then the binding is unknown.
  const [loaded, setLoaded] = useState(false)
  const [loadError, setLoadError] = useState("")
  const [images, setImages] = useState<WorkflowImage[]>([])
  const [ref, setRef] = useState("")
  const [error, setError] = useState("")
  const [notice, setNotice] = useState("")
  const [emptyPools, setEmptyPools] = useState<{ names: string[]; message: string } | null>(null)
  const [missingSecrets, setMissingSecrets] = useState<{ names: string[]; message: string } | null>(null)
  const [busy, setBusy] = useState(false)
  const { confirm, dialog } = useWorkflowConfirm()

  const loadBinding = useCallback(async () => {
    try {
      setBinding(await getQueueWorkflow(queue, target))
      setLoaded(true)
      setLoadError("")
    } catch (err) {
      setLoaded(false)
      setLoadError(errorText(err))
    }
  }, [queue, target])
  useEffect(() => { void Promise.resolve().then(loadBinding) }, [loadBinding])
  useEffect(() => {
    void listWorkflowImages(target).then(setImages, (err) => setError(errorText(err)))
  }, [target])
  const sorted = useMemo(() => sortImages(images), [images])

  const refused = async (err: unknown) => {
    if (err instanceof ApiError && err.code === "revision_conflict") {
      await loadBinding()
      setNotice("The binding changed elsewhere. It was reloaded; review and retry.")
    } else if (err instanceof ApiError && err.code === "workflow_pool_empty") {
      setEmptyPools({ names: detailList(err, "pools"), message: err.message })
    } else if (err instanceof ApiError && err.code === "workflow_secret_missing") {
      setMissingSecrets({ names: detailList(err, "secrets"), message: err.message })
    } else setError(errorText(err))
  }

  const reset = () => { setError(""); setNotice(""); setEmptyPools(null); setMissingSecrets(null) }

  const bind = async () => {
    reset()
    setBusy(true)
    try {
      setBinding(await setQueueWorkflow(queue, ref, binding?.revision ?? 0, target))
      setRef("")
    } catch (err) { await refused(err) } finally { setBusy(false) }
  }

  const clear = () => {
    if (!binding) return
    confirm({
      title: `Clear the workflow of ${queue}?`,
      description: "New tasks in this queue become flexible tasks. Tasks already running the workflow keep it.",
      action: "Clear binding",
      run: () => {
        reset()
        setBusy(true)
        clearQueueWorkflow(queue, binding.revision, target).then(() => setBinding(null), refused).finally(() => setBusy(false))
      },
    })
  }

  return (
    <section className="flex min-w-0 flex-col gap-2.5" aria-label={`Workflow ${queue}`}>
      <span className={LABEL}>Workflow</span>
      {loaded && (binding ? (
        <p data-testid="binding" className="text-[12.5px]">
          <strong>{binding.name}</strong> {binding.version}{" "}
          <span className={MONO} title={binding.digest}>{binding.digest.slice(0, 12)}</span>
        </p>
      ) : <span className={EMPTY}>No workflow</span>)}
      {loadError && (
        <div role="alert" className="flex items-center gap-2 text-[12px] text-destructive">
          <span>{loadError}</span>
          <Button type="button" size="sm" variant="secondary" className="h-[26px]" onClick={() => void loadBinding()}>Retry</Button>
        </div>
      )}
      <div className="flex min-w-0 flex-wrap items-center gap-1.5">
        <SelectShell aria-label="Workflow image" value={ref} className="h-[26px] w-auto text-[12px] md:text-[12px]"
          onChange={(event) => setRef(event.target.value)}>
          <option value="">Choose an image</option>
          {sorted.map((image) => <option key={`${image.name}:${image.tag}`} value={`${image.name}:${image.tag}`}>{image.name}:{image.tag}</option>)}
        </SelectShell>
        <Button type="button" size="sm" className="h-[26px]" disabled={busy || !ref || !loaded} onClick={() => void bind()}>Bind</Button>
        <Button type="button" size="sm" variant="secondary" className="h-[26px]" disabled={busy || !loaded || !binding} onClick={clear}>Clear</Button>
      </div>
      {notice && <p role="status" className="text-[12px] text-muted-foreground">{notice}</p>}
      {error && <p role="alert" className="text-[12px] text-destructive">{error}</p>}
      {emptyPools && (
        <div role="alert" aria-label="Pools needed" className="text-[12px] text-destructive">
          <p>{emptyPools.message}</p>
          <p className="font-mono">{emptyPools.names.join(", ")}</p>
        </div>
      )}
      {pools}
      <QueueSecrets queue={queue} target={target} missing={missingSecrets?.names} missingMessage={missingSecrets?.message} />
      {dialog}
    </section>
  )
}
