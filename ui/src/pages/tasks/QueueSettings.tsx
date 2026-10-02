import { useCallback, useEffect, useState } from "react"
import { toast } from "sonner"
import type { ApiTarget } from "@/lib/api"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Textarea } from "@/components/ui/textarea"
import {
  listAgentPools,
  rebindAgentPool,
  type AgentPool,
  type CreateQueueInput,
  type TaskQueue,
  type UpdateQueueInput,
} from "@/lib/tasks"
import QueueWorkflowSettings from "./QueueWorkflowSettings"

export default function QueueSettings({
  queues,
  onCreate,
  onUpdate,
  target,
}: {
  queues: TaskQueue[]
  onCreate: (input: CreateQueueInput) => Promise<void>
  onUpdate: (prefix: string, input: UpdateQueueInput) => Promise<void>
  target?: ApiTarget
}) {
  const [creating, setCreating] = useState(false)
  // One card section at a time: the sheet is 340px wide.
  const [open, setOpen] = useState("")
  const [prefix, setPrefix] = useState("")
  const [name, setName] = useState("")
  const [owners, setOwners] = useState("")
  const [responsible, setResponsible] = useState("")

  const submit = async (event: React.FormEvent) => {
    event.preventDefault()
    await onCreate({
      prefix: prefix.trim().toUpperCase(),
      name: name.trim(),
      owners: owners.split(",").map((owner) => owner.trim()).filter(Boolean),
      responsible_agent: responsible.trim(),
    })
    setCreating(false)
    setPrefix("")
    setName("")
    setOwners("")
    setResponsible("")
  }

  return (
    <section className="task-queues" aria-label="Queue settings">
      <div className="task-queue-list">
        {queues.map((queue) => (
          <article key={`${queue.prefix}:${queue.revision}`}>
            <div className="task-queue-card-head">
              <strong>{queue.prefix}</strong>
              <h3>{queue.name}</h3>
            </div>
            <p className="task-queue-card-agents">
              Agents: <span>{queue.owners.length > 0 ? queue.owners.join(", ") : "—"}</span>
            </p>
            <div className="task-queue-card-actions">
              <Button
                type="button"
                size="xs"
                variant="secondary"
                aria-label={`Rename ${queue.prefix}`}
                aria-expanded={open === `name:${queue.prefix}`}
                onClick={() => setOpen((current) => current === `name:${queue.prefix}` ? "" : `name:${queue.prefix}`)}
              >
                Rename
              </Button>
              <Button
                type="button"
                size="xs"
                variant="secondary"
                aria-label={`Agent pools ${queue.prefix}`}
                aria-expanded={open === `pools:${queue.prefix}`}
                onClick={() => setOpen((current) => current === `pools:${queue.prefix}` ? "" : `pools:${queue.prefix}`)}
              >
                Pools
              </Button>
              <Button
                type="button"
                size="xs"
                variant="secondary"
                aria-label={`Workflow ${queue.prefix}`}
                aria-expanded={open === `workflow:${queue.prefix}`}
                onClick={() => setOpen((current) => current === `workflow:${queue.prefix}` ? "" : `workflow:${queue.prefix}`)}
              >
                Workflow
              </Button>
            </div>
            {open === `workflow:${queue.prefix}` && (
              <QueueWorkflowSettings queue={queue.prefix} target={target} pools={<QueuePoolEditor queue={queue} target={target} />} />
            )}
            {open === `name:${queue.prefix}` && (
              <form onSubmit={(event) => {
                event.preventDefault()
                const values = new FormData(event.currentTarget)
                void onUpdate(queue.prefix, {
                  name: String(values.get("name") ?? "").trim(),
                  description: String(values.get("description") ?? "").trim(),
                  owners: String(values.get("owners") ?? "").split(",").map((value) => value.trim()).filter(Boolean),
                  responsible_agent: String(values.get("responsible") ?? "").trim(),
                  revision: queue.revision,
                })
              }}>
                <label>Queue name<Input name="name" aria-label={`Queue name ${queue.prefix}`} defaultValue={queue.name} /></label>
                <label>Description<Textarea name="description" aria-label={`Queue description ${queue.prefix}`} defaultValue={queue.description} /></label>
                <label>Owner agents<Input name="owners" aria-label={`Queue owners ${queue.prefix}`} defaultValue={queue.owners.join(", ")} /></label>
                <label>Responsible agent<Input name="responsible" aria-label={`Queue triager ${queue.prefix}`} defaultValue={queue.responsible_agent} /></label>
                <button type="submit">Save {queue.prefix}</button>
              </form>
            )}
            {open === `pools:${queue.prefix}` && <QueuePoolEditor queue={queue} target={target} />}
          </article>
        ))}
      </div>
      <footer>
        <button type="button" onClick={() => setCreating((current) => !current)}>New queue</button>
        {creating && (
          <form onSubmit={(event) => void submit(event)}>
            <label>Queue prefix<Input aria-label="Queue prefix" value={prefix} placeholder="NEW-QUEUE" onChange={(event) => setPrefix(event.target.value.toUpperCase())} /></label>
            <label>Queue name<Input aria-label="New queue name" value={name} onChange={(event) => setName(event.target.value)} /></label>
            <label>Owner agents<Input value={owners} placeholder="alice, agent:bob" onChange={(event) => setOwners(event.target.value)} /></label>
            <label>Responsible agent<Input value={responsible} onChange={(event) => setResponsible(event.target.value)} /></label>
            <div><button type="submit" disabled={!prefix.trim() || !name.trim()}>Add queue</button><button type="button" onClick={() => setCreating(false)}>Cancel</button></div>
          </form>
        )}
      </footer>
    </section>
  )
}

function actionKey(prefix: string): string {
  return globalThis.crypto?.randomUUID?.() ?? `${prefix}-${Date.now()}-${Math.random()}`
}

function QueuePoolEditor({ queue, target }: { queue: TaskQueue; target?: ApiTarget }) {
  const [pools, setPools] = useState<AgentPool[]>([])
  const [poolName, setPoolName] = useState("")
  const [poolAgents, setPoolAgents] = useState("")
  const [busy, setBusy] = useState(false)
  const [stateLoading, setStateLoading] = useState(true)
  const [stateError, setStateError] = useState("")
  const [staleMessage, setStaleMessage] = useState("")

  const loadState = useCallback(async () => {
    setStateLoading(true)
    setStateError("")
    try {
      const page = await listAgentPools(queue.prefix, target)
      setPools(page.items ?? [])
    } catch (error) {
      setStateError(error instanceof Error ? error.message : String(error))
    }
    setStateLoading(false)
  }, [queue.prefix, target])

  useEffect(() => { void Promise.resolve().then(loadState) }, [loadState])

  const protect = async (operation: () => Promise<void>) => {
    setBusy(true)
    setStaleMessage("")
    try { await operation() } catch (error) {
      const message = error instanceof Error ? error.message : String(error)
      if (typeof error === "object" && error && "status" in error && error.status === 409) {
        await loadState()
        setStaleMessage("Configuration changed elsewhere. Current revisions were reloaded; review and retry.")
      }
      toast.error(message)
    } finally { setBusy(false) }
  }

  return <section className="task-queue-pools" aria-label={`Agent pools ${queue.prefix}`}>
    <h4>Agent pools</h4>
    {stateLoading ? <p>Loading agent pools…</p> : stateError ? <div role="alert" aria-label={`Agent pools ${queue.prefix} error`}><p>{stateError}</p><button type="button" aria-label={`Retry agent pools ${queue.prefix}`} onClick={() => void loadState()}>Retry</button></div> : null}
    {staleMessage && <p role="alert">{staleMessage}</p>}
    <fieldset disabled={stateLoading || Boolean(stateError)}>
    <div className="task-pool-list"><h3>Explicit pools <span>{pools.length}</span></h3><ul>{pools.map((pool) => <li key={pool.name}><strong>{pool.name}</strong><span>{pool.agents.join(", ")} · rev {pool.revision}</span></li>)}</ul></div>
    <form onSubmit={(event) => { event.preventDefault(); void protect(async () => {
      const previous = pools.find((pool) => pool.name === poolName.trim())
      const updated = await rebindAgentPool(queue.prefix, poolName.trim(), poolAgents.split(",").map((agent) => agent.trim()).filter(Boolean), previous?.revision ?? 0, actionKey("pool"), target)
      setPools((current) => [...current.filter((pool) => pool.name !== updated.name), updated])
      toast.success("Agent pool updated")
    }) }}>
      <label>Pool name<Input aria-label={`Pool name ${queue.prefix}`} value={poolName} onChange={(event) => setPoolName(event.target.value)} /></label>
      <label>Explicit agents<Input aria-label={`Pool agents ${queue.prefix}`} value={poolAgents} onChange={(event) => setPoolAgents(event.target.value)} placeholder="dev-a, dev-b" /></label>
      <button type="submit" disabled={busy || !poolName.trim() || !poolAgents.trim()}>Save pool</button>
    </form>
    </fieldset>
  </section>
}
