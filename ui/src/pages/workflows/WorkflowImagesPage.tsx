import { useCallback, useEffect, useMemo, useState } from "react"
import { Link } from "react-router-dom"
import type { Daemon } from "@/lib/daemons"
import { listWorkflowImages, type WorkflowImage } from "@/lib/tasks"
import { Button } from "@/components/ui/button"
import { errorText } from "@/pages/tasks/workflowShared"
import WorkflowBuildFromDirectory from "./WorkflowBuildFromDirectory"
import { WorkflowTransferDialog } from "./WorkflowTransferDialog"
import { boundTo, groupWorkflowImages, useQueueBindings, useTransferDaemons } from "./workflowImages"

const CELL = "px-3 py-2"

/** The workflow image registry of one server: build, list, and copy to other servers. */
export default function WorkflowImagesPage({ target, basePath }: { target: Daemon | null; basePath: string }) {
  const [images, setImages] = useState<WorkflowImage[] | null>(null)
  const [error, setError] = useState("")
  const [revision, setRevision] = useState(0)
  const [transfer, setTransfer] = useState<WorkflowImage | null>(null)
  const bindings = useQueueBindings(target)
  const transferDaemons = useTransferDaemons()
  const reloadBindings = bindings.reload

  useEffect(() => {
    let alive = true
    listWorkflowImages(target).then(
      (result) => { if (alive) { setImages(result); setError("") } },
      (err) => { if (alive) setError(errorText(err)) },
    )
    return () => { alive = false }
  }, [target, revision])
  const reload = useCallback(() => { setRevision((value) => value + 1); reloadBindings() }, [reloadBindings])
  const groups = useMemo(() => groupWorkflowImages(images ?? []), [images])

  return <div className="h-full space-y-3 overflow-auto p-3">
    <WorkflowBuildFromDirectory target={target} onBuilt={reload} />
    {error ? <div role="alert" className="flex items-center gap-2 text-sm text-destructive">
      <span>{error}</span>
      <Button size="sm" variant="secondary" onClick={() => { setError(""); reload() }}>Retry</Button>
    </div> : images === null ? <p className="text-sm text-muted-foreground">Loading workflow images…</p>
      : images.length === 0 ? <p className="text-sm text-muted-foreground">No workflow images</p>
        : <div className="overflow-x-auto rounded border">
          <table className="w-full text-sm">
            <thead className="bg-muted/50 text-left text-xs text-muted-foreground"><tr>
              {["Name", "Tag", "Version", "Digest", "Built at", "Bound to", ""].map((label) => <th key={label} className={CELL}>{label}</th>)}
            </tr></thead>
            <tbody>
              {groups.flatMap(({ name, tags }) => tags.map((image, index) => {
                const ref = `${image.name}:${image.tag}`
                const bound = bindings.loaded ? boundTo(bindings, image.digest) : { text: "…" }
                return <tr key={ref} className={index === 0 ? "border-t" : ""}>
                  <td className={`${CELL} font-medium`}>{index === 0 ? name : ""}</td>
                  <td className={CELL}>
                    <Link className="font-mono text-primary hover:underline"
                      to={`${basePath}/${encodeURIComponent(image.name)}/${encodeURIComponent(image.tag)}`}>{image.tag}</Link>
                  </td>
                  <td className={CELL}>{image.version}</td>
                  <td className={`${CELL} font-mono text-xs`}><span title={image.digest}>{image.digest.slice(0, 12)}</span></td>
                  <td className={`${CELL} text-xs text-muted-foreground`}>{image.built_at}</td>
                  <td className={`${CELL} text-xs`}><span title={bound.title}>{bound.text}</span></td>
                  <td className={CELL}>
                    <Button size="sm" variant="outline" aria-label={`Upload to servers ${ref}`} disabled={!transferDaemons.ready}
                      onClick={() => setTransfer(image)}>Upload to servers</Button>
                  </td>
                </tr>
              }))}
            </tbody>
          </table>
        </div>}
    {transfer && <WorkflowTransferDialog open onOpenChange={(open) => { if (!open) setTransfer(null) }}
      source={target} name={transfer.name} tag={transfer.tag} daemons={transferDaemons.daemons} onComplete={() => undefined} />}
  </div>
}
