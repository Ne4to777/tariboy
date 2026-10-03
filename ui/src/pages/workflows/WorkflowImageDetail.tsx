import { useEffect, useState } from "react"
import { Link, useNavigate } from "react-router-dom"
import { ApiError } from "@/lib/api"
import type { Daemon } from "@/lib/daemons"
import { getWorkflowImage, removeWorkflowImage, type WorkflowManifest } from "@/lib/tasks"
import { Button } from "@/components/ui/button"
import { useWorkflowConfirm } from "@/pages/tasks/WorkflowConfirm"
import { errorText, useMounted } from "@/pages/tasks/workflowShared"
import { WorkflowTransferDialog } from "./WorkflowTransferDialog"
import { boundTo, effectiveLimits, ownerLabel, useQueueBindings, useTransferDaemons } from "./workflowImages"

const CELL = "px-3 py-2 align-top"
const HEAD = "bg-muted/50 text-left text-xs text-muted-foreground"
const DEFAULT_TIMEOUT = "60s"

function Section({ title, children }: { title: string; children: React.ReactNode }) {
  return <section className="space-y-2">
    <h2 className="font-medium">{title}</h2>
    {children}
  </section>
}

/** One workflow image, read from its manifest. Every manifest value renders as plain text. */
export default function WorkflowImageDetail({ target, name, tag, basePath }: {
  target: Daemon | null
  name: string
  tag: string
  basePath: string
}) {
  const [manifest, setManifest] = useState<WorkflowManifest | null>(null)
  const [error, setError] = useState("")
  const [removeError, setRemoveError] = useState("")
  const [busy, setBusy] = useState(false)
  const [transfer, setTransfer] = useState(false)
  const bindings = useQueueBindings(target)
  const transferDaemons = useTransferDaemons()
  const { confirm, dialog } = useWorkflowConfirm()
  const navigate = useNavigate()
  const mounted = useMounted()

  useEffect(() => {
    let alive = true
    getWorkflowImage(name, tag, target).then(
      (result) => { if (alive) setManifest(result) },
      (err) => { if (alive) setError(errorText(err)) },
    )
    return () => { alive = false }
  }, [name, tag, target])

  if (error) return <p role="alert" className="p-3 text-sm text-destructive">{error}</p>
  if (!manifest) return <p className="p-3 text-sm text-muted-foreground">Loading workflow image…</p>

  const ref = `${name}:${tag}`
  const definition = manifest.definition
  const bound = bindings.loaded ? boundTo(bindings, manifest.digest) : { text: "…" }
  // A digest names content, not a tag: there is no tag to remove.
  const byDigest = tag === manifest.digest

  const remove = () => confirm({
    title: `Remove tag ${ref}?`,
    description: "When no other tag names this content, the content is deleted too.",
    action: `Remove ${ref}`,
    run: () => {
      setRemoveError("")
      setBusy(true)
      removeWorkflowImage(name, tag, target).then(
        () => navigate(basePath),
        (err) => {
          if (!mounted.current) return
          setBusy(false)
          setRemoveError(err instanceof ApiError && err.code === "workflow_in_use"
            ? `${err.message}. Bound to: ${bound.text}`
            : errorText(err))
        },
      )
    },
  })

  return <div className="h-full space-y-5 overflow-auto p-3 text-sm">
    <nav className="text-xs text-muted-foreground">
      <Link className="text-primary hover:underline" to={basePath}>Workflow images</Link> / <span className="font-mono">{ref}</span>
    </nav>
    <header className="space-y-2">
      <h1 className="text-lg font-medium">{manifest.name} {manifest.version}</h1>
      <dl className="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1 text-xs">
        <dt className="text-muted-foreground">Digest</dt><dd className="break-all font-mono">{manifest.digest}</dd>
        <dt className="text-muted-foreground">Built at</dt><dd>{manifest.built_at}</dd>
        <dt className="text-muted-foreground">Initial status</dt><dd className="font-mono">{definition.initial_status}</dd>
        <dt className="text-muted-foreground">Bound to</dt><dd data-testid="bound-to" title={bound.title}>{bound.text}</dd>
      </dl>
      <div className="flex gap-2">
        <Button size="sm" variant="outline" disabled={!transferDaemons.ready} onClick={() => setTransfer(true)}>Upload to servers</Button>
        {!byDigest && <Button size="sm" variant="destructive" disabled={busy} onClick={remove}>Remove tag</Button>}
      </div>
      {removeError && <p role="alert" className="text-destructive">{removeError}</p>}
    </header>

    <Section title="Statuses">
      <div className="overflow-x-auto rounded border">
        <table aria-label="Statuses" className="w-full">
          <thead className={HEAD}><tr>{["Status", "Owner", "Instructions", "Outcomes", "Watch"].map((label) => <th key={label} className={CELL}>{label}</th>)}</tr></thead>
          <tbody>
            {definition.statuses.map((status) => <tr key={status.id} className="border-t">
              <td className={CELL}>
                <span className="font-mono">{status.id}</span>
                {[status.id === definition.initial_status && "initial", status.terminal && "terminal", status.cancelled && "cancelled"]
                  .filter(Boolean).map((label) => <span key={String(label)} className="ml-1 rounded bg-muted px-1 text-xs">{label}</span>)}
              </td>
              <td className={`${CELL} font-mono`}>{ownerLabel(status)}</td>
              <td className={`${CELL} font-mono text-xs`}>{status.instructions ?? "—"}</td>
              <td className={CELL}>
                <ul className="space-y-1">
                  {(status.transitions ?? []).map((transition) => <li key={transition.on}>
                    <span className="font-mono">{transition.on} → {transition.to}</span>
                    {transition.requires?.length ? <div className="text-xs text-muted-foreground">requires {transition.requires.join(", ")}</div> : null}
                    {(transition.checks ?? []).map((check, index) => <div key={index} className="font-mono text-xs text-muted-foreground">
                      check {check.script} as {check.run_as || "queue"}, {check.timeout || DEFAULT_TIMEOUT}
                    </div>)}
                  </li>)}
                </ul>
              </td>
              <td className={`${CELL} font-mono text-xs`}>
                {status.watch ? `${status.watch.script} every ${status.watch.every}, timeout ${status.watch.timeout || DEFAULT_TIMEOUT}` : "—"}
              </td>
            </tr>)}
          </tbody>
        </table>
      </div>
    </Section>

    <Section title="Limits">
      <div className="overflow-x-auto rounded border">
        <table aria-label="Limits" className="w-full">
          <thead className={HEAD}><tr>
            {["Scope", "idle_iterations", "rejected_requests", "script_failures", "unavailable_grace"].map((label) => <th key={label} className={CELL}>{label}</th>)}
          </tr></thead>
          <tbody>
            {effectiveLimits(definition).map(({ scope, values }) => <tr key={scope} className="border-t">
              <td className={`${CELL} font-mono`}>{scope}</td>
              {values.map((limit) => <td key={limit.key} className={CELL}>
                {limit.value} <span className="text-xs text-muted-foreground">({limit.source})</span>
              </td>)}
            </tr>)}
          </tbody>
        </table>
      </div>
    </Section>

    <Section title="Artifacts">
      {definition.artifacts?.length ? <ul className="space-y-1">
        {definition.artifacts.map((artifact) => <li key={artifact.name}>
          <span className="font-mono">{artifact.name}</span>{artifact.description && <span className="text-muted-foreground"> — {artifact.description}</span>}
        </li>)}
      </ul> : <p className="text-muted-foreground">None</p>}
    </Section>

    <Section title="Requires secrets">
      {definition.requires_secrets?.length
        ? <ul className="font-mono">{definition.requires_secrets.map((secret) => <li key={secret}>{secret}</li>)}</ul>
        : <p className="text-muted-foreground">None</p>}
    </Section>

    <Section title="Env">
      {Object.keys(definition.env ?? {}).length
        ? <ul className="font-mono">{Object.entries(definition.env!).map(([key, value]) => <li key={key}>{key}={value}</li>)}</ul>
        : <p className="text-muted-foreground">None</p>}
    </Section>

    <Section title="Files">
      <div className="overflow-x-auto rounded border">
        <table aria-label="Files" className="w-full">
          <thead className={HEAD}><tr>{["Path", "Size", "Executable"].map((label) => <th key={label} className={CELL}>{label}</th>)}</tr></thead>
          <tbody>
            {manifest.files.map((file) => <tr key={file.path} className="border-t">
              <td className={`${CELL} font-mono`}><span title={file.sha256}>{file.path}</span></td>
              <td className={CELL}>{file.size} B</td>
              <td className={CELL}>{file.executable ? "yes" : "no"}</td>
            </tr>)}
          </tbody>
        </table>
      </div>
    </Section>

    {dialog}
    {transfer && <WorkflowTransferDialog open onOpenChange={setTransfer} source={target} name={name} tag={tag}
      daemons={transferDaemons.daemons} onComplete={() => undefined} />}
  </div>
}
