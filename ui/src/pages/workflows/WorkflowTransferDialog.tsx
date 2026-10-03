import { useEffect, useRef, useState } from "react"
import type { Daemon } from "@/lib/daemons"
import { downloadWorkflowArchiveOn, importWorkflowArchiveOn } from "@/lib/teamApi"
import { Button } from "@/components/ui/button"
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog"
import { eligibleImageTransferTargets } from "@/pages/images/imageTransferTargets"
import { errorText } from "@/pages/tasks/workflowShared"

type Status = "queued" | "exporting" | "importing" | "completed" | "already-present" | "failed" | "cancelled"
interface Row { status: Status; error?: string }

const statusLabel = (status: Status) =>
  status === "already-present" ? "Already present" : status[0].toUpperCase() + status.slice(1)

interface Props {
  open: boolean
  onOpenChange: (open: boolean) => void
  source: Daemon | null
  name: string
  tag: string
  daemons: Daemon[]
  onComplete: () => void
}

/**
 * Copies one workflow image to other servers: the archive is exported from the
 * source once, then imported on each selected server in turn. A server that
 * refuses it does not stop the others.
 */
export function WorkflowTransferDialog({ open, ...props }: Props) {
  if (!open) return null
  return <TransferSession {...props} />
}

function TransferSession({ onOpenChange, source, name, tag, daemons, onComplete }: Omit<Props, "open">) {
  const [targets] = useState(() => eligibleImageTransferTargets(source, daemons))
  const [selected, setSelected] = useState<Set<string>>(() => new Set())
  const [rows, setRows] = useState<Record<string, Row>>({})
  const [phase, setPhase] = useState<"idle" | "exporting" | "transferring">("idle")
  const cancelled = useRef(false)
  const mounted = useRef(true)
  useEffect(() => {
    mounted.current = true
    return () => { mounted.current = false; cancelled.current = true }
  }, [])
  const setRow = (id: string, row: Row) => { if (mounted.current) setRows((current) => ({ ...current, [id]: row })) }
  const allSelected = targets.length > 0 && selected.size === targets.length

  const toggle = (id: string) => setSelected((current) => {
    const next = new Set(current)
    if (!next.delete(id)) next.add(id)
    return next
  })

  const start = async () => {
    const chosen = targets.filter((target) => selected.has(target.id))
    cancelled.current = false
    setPhase("exporting")
    setRows(Object.fromEntries(chosen.map((target) => [target.id, { status: "exporting" }])))
    let archive: Blob
    try {
      archive = await downloadWorkflowArchiveOn(source, name, tag)
    } catch (err) {
      if (!mounted.current) return
      setRows(Object.fromEntries(chosen.map((target) => [target.id, { status: "failed", error: errorText(err) }])))
      setPhase("idle")
      return
    }
    if (!mounted.current) return
    setRows(Object.fromEntries(chosen.map((target) => [target.id, { status: "queued" }])))
    setPhase("transferring")
    for (const target of chosen) {
      if (cancelled.current) { setRow(target.id, { status: "cancelled" }); continue }
      setRow(target.id, { status: "importing" })
      try {
        const result = await importWorkflowArchiveOn(target.target, archive)
        setRow(target.id, { status: result.created ? "completed" : "already-present" })
      } catch (err) {
        setRow(target.id, { status: "failed", error: errorText(err) })
      }
    }
    if (!mounted.current) return
    setPhase("idle")
    onComplete()
  }

  const exporting = phase === "exporting"
  return (
    <Dialog open onOpenChange={(next) => { if (!next && !exporting) onOpenChange(false) }}>
      <DialogContent showCloseButton={!exporting}>
        <DialogHeader>
          <DialogTitle>Upload workflow {name}:{tag}</DialogTitle>
          <DialogDescription>
            Each selected server gets this content tagged with its version and latest, so latest there moves to it.
          </DialogDescription>
        </DialogHeader>
        {targets.length === 0 ? <p className="text-sm text-muted-foreground">No ready servers are available for transfer.</p> : <div className="space-y-3">
          <Button variant="outline" size="sm" disabled={phase !== "idle"}
            onClick={() => setSelected(allSelected ? new Set() : new Set(targets.map((target) => target.id)))}>
            {allSelected ? "Clear all servers" : "All servers"}
          </Button>
          <ul className="space-y-2" aria-label="Eligible transfer targets">
            {targets.map((target) => <li key={target.id}>
              <label className="flex items-center gap-2 text-sm">
                <input type="checkbox" aria-label={`Transfer to ${target.label}`} checked={selected.has(target.id)}
                  disabled={phase !== "idle"} onChange={() => toggle(target.id)} />
                {target.label}
              </label>
            </li>)}
          </ul>
          <ul className="space-y-1" aria-label="Transfer progress">
            {targets.filter((target) => rows[target.id]).map((target) => <li key={target.id} className="text-sm">
              {target.label}: {statusLabel(rows[target.id].status)}{rows[target.id].error ? ` — ${rows[target.id].error}` : ""}
            </li>)}
          </ul>
        </div>}
        <DialogFooter>
          {phase === "transferring"
            ? <Button variant="outline" onClick={() => { cancelled.current = true }}>Cancel transfer</Button>
            : <Button variant="outline" disabled={exporting} onClick={() => onOpenChange(false)}>Close</Button>}
          <Button disabled={selected.size === 0 || phase !== "idle"} onClick={() => void start()}>Start transfer</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
