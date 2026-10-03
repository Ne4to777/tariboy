import { useState } from "react"
import { toast } from "sonner"
import { ApiError, type ApiTarget } from "@/lib/api"
import {
  buildWorkflowDirectory, validateWorkflowDirectory,
  type WorkflowValidation, type WorkflowValidationError,
} from "@/lib/tasks"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { errorText } from "@/pages/tasks/workflowShared"

/** Validate and build a workflow source directory on the host of `target`. */
export default function WorkflowBuildFromDirectory({ target, onBuilt }: { target: ApiTarget; onBuilt: () => void }) {
  const [path, setPath] = useState("")
  const [busy, setBusy] = useState(false)
  const [validation, setValidation] = useState<WorkflowValidation | null>(null)
  const [errors, setErrors] = useState<WorkflowValidationError[]>([])
  const [failure, setFailure] = useState<{ message: string; hint?: string } | null>(null)

  const run = async (action: () => Promise<void>) => {
    setBusy(true)
    setValidation(null)
    setErrors([])
    setFailure(null)
    try { await action() } finally { setBusy(false) }
  }

  const validate = () => run(async () => {
    try {
      const result = await validateWorkflowDirectory(path.trim(), target)
      if (result.valid) setValidation(result)
      else setErrors(result.errors)
    } catch (err) { setFailure({ message: errorText(err) }) }
  })

  const build = () => run(async () => {
    try {
      const result = await buildWorkflowDirectory(path.trim(), target)
      const ref = `${result.name}:${result.version}`
      toast.success(result.created ? `built ${ref}` : `${ref} already built`)
      onBuilt()
    } catch (err) {
      const list = err instanceof ApiError && Array.isArray(err.details?.errors) ? err.details.errors as WorkflowValidationError[] : []
      if (list.length) setErrors(list)
      else if (err instanceof ApiError && err.code === "workflow_version_published") {
        setFailure({ message: err.message, hint: "Bump the version with tariboy workflow version update, then build again." })
      } else setFailure({ message: errorText(err) })
    }
  })

  return <section className="space-y-3 rounded border p-4">
    <div>
      <h2 className="font-medium">Build from directory</h2>
      <p className="text-sm text-muted-foreground">A Workflowfile.yaml, or the directory holding it, on this server. The name and version come from the file.</p>
    </div>
    <div className="grid gap-3 md:grid-cols-[1fr_auto_auto]">
      <Input aria-label="Workflow source directory" placeholder="/absolute/path/to/workflow" value={path} onChange={(event) => setPath(event.target.value)} />
      <Button variant="outline" disabled={busy || !path.trim()} onClick={() => void validate()}>Validate</Button>
      <Button disabled={busy || !path.trim()} onClick={() => void build()}>Build</Button>
    </div>
    {errors.length > 0 && <ul role="alert" className="space-y-1 text-sm text-destructive">
      {errors.map((item, index) => <li key={`${item.path}-${index}`}><span className="font-mono">{item.code} · {item.path}</span>: {item.message}</li>)}
    </ul>}
    {failure && <div role="alert" className="text-sm text-destructive">
      <p>{failure.message}</p>
      {failure.hint && <p className="text-muted-foreground">{failure.hint}</p>}
    </div>}
    {validation && <div className="space-y-1 rounded border bg-muted/20 p-3 text-sm" aria-label="Validated workflow">
      <p><strong>{validation.name} {validation.version}</strong> is valid.</p>
      <p>Pools: <span className="font-mono">{validation.pools.join(", ") || "none"}</span></p>
      <p>Files: <span className="font-mono">{validation.files.join(", ") || "none"}</span></p>
    </div>}
  </section>
}
