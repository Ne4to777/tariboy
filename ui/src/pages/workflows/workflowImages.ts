import { useCallback, useEffect, useMemo, useState } from "react"
import { useOptionalDaemons } from "@/components/DaemonProvider"
import type { ApiTarget } from "@/lib/api"
import { resolveDaemon, type Daemon } from "@/lib/daemons"
import {
  getQueueWorkflow, listTaskQueues,
  type WorkflowDefinition, type WorkflowDefinitionStatus, type WorkflowImage, type WorkflowLimits,
} from "@/lib/tasks"
import { errorText } from "@/pages/tasks/workflowShared"

/** `latest` first, then the other tags newest version first. */
function compareTags(a: WorkflowImage, b: WorkflowImage): number {
  if (a.tag === "latest" || b.tag === "latest") return a.tag === "latest" ? -1 : 1
  return b.tag.localeCompare(a.tag, undefined, { numeric: true })
}

export function groupWorkflowImages(images: WorkflowImage[]) {
  const byName = new Map<string, WorkflowImage[]>()
  for (const image of images) byName.set(image.name, [...byName.get(image.name) ?? [], image])
  return [...byName]
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([name, tags]) => ({ name, tags: tags.sort(compareTags) }))
}

export interface QueueBindings {
  loaded: boolean
  byDigest: Map<string, string[]>
  /** Queues whose binding could not be read. */
  unknown: string[]
  /** Why the queue list itself could not be read; "" when it could. */
  failed: string
  reload: () => void
}

/**
 * Which queues bind which digest, read from the queue list and the binding of
 * each queue. A queue whose binding fails is reported in `unknown`; the rest
 * still show.
 */
export function useQueueBindings(target: ApiTarget): QueueBindings {
  const [state, setState] = useState<Omit<QueueBindings, "reload">>(
    { loaded: false, byDigest: new Map(), unknown: [], failed: "" })
  const [revision, setRevision] = useState(0)
  useEffect(() => {
    let alive = true
    void (async () => {
      let queues: string[]
      try {
        queues = (await listTaskQueues(target)).queues.map((queue) => queue.prefix)
      } catch (err) {
        if (alive) setState({ loaded: true, byDigest: new Map(), unknown: [], failed: errorText(err) })
        return
      }
      const results = await Promise.allSettled(queues.map((queue) => getQueueWorkflow(queue, target)))
      const byDigest = new Map<string, string[]>()
      const unknown: string[] = []
      results.forEach((result, index) => {
        if (result.status === "rejected") unknown.push(queues[index])
        else if (result.value) byDigest.set(result.value.digest, [...byDigest.get(result.value.digest) ?? [], queues[index]])
      })
      if (alive) setState({ loaded: true, byDigest, unknown, failed: "" })
    })()
    return () => { alive = false }
  }, [target, revision])
  const reload = useCallback(() => setRevision((value) => value + 1), [])
  return { ...state, reload }
}

/** The "Bound to" cell of a digest. */
export function boundTo(bindings: Omit<QueueBindings, "reload">, digest: string): { text: string; title?: string } {
  if (bindings.failed) return { text: "unknown", title: `Could not list queues: ${bindings.failed}` }
  const queues = [...bindings.byDigest.get(digest) ?? []]
  if (bindings.unknown.length) queues.push("unknown")
  return {
    text: queues.length ? queues.join(", ") : "—",
    title: bindings.unknown.length ? `Could not read the binding of ${bindings.unknown.join(", ")}` : undefined,
  }
}

export function ownerLabel(status: Pick<WorkflowDefinitionStatus, "owner" | "terminal">): string {
  if (status.terminal) return "terminal"
  return status.owner.kind === "pool" ? `pool:${status.owner.pool}` : status.owner.kind
}

const LIMIT_DEFAULTS: Required<WorkflowLimits> = {
  idle_iterations: 3, rejected_requests: 5, script_failures: 3, unavailable_grace: "5m",
}
const LIMIT_KEYS = Object.keys(LIMIT_DEFAULTS) as (keyof WorkflowLimits)[]

/** Each limit of the workflow and of every non-terminal status, with where its value comes from. */
export function effectiveLimits(definition: WorkflowDefinition) {
  const resolve = (status?: WorkflowLimits) => LIMIT_KEYS.map((key) => {
    const [value, source] = status?.[key] !== undefined ? [status[key], "status"]
      : definition.limits?.[key] !== undefined ? [definition.limits[key], "workflow"]
        : [LIMIT_DEFAULTS[key], "default"]
    return { key, value: String(value), source }
  })
  return [
    { scope: "workflow", values: resolve() },
    ...definition.statuses.filter((status) => !status.terminal)
      .map((status) => ({ scope: status.id, values: resolve(status.limits) })),
  ]
}

/**
 * The registered servers resolved to live endpoints for a transfer, as
 * `BuiltImages` resolves them; `ready` is false until the current registry is
 * resolved.
 */
export function useTransferDaemons(): { daemons: Daemon[]; ready: boolean } {
  const context = useOptionalDaemons()
  const registry = useMemo(() => context?.daemons ?? [], [context?.daemons])
  const generation = useMemo(() => JSON.stringify(registry.map(({ id, label, baseURL, state }) => ({ id, label, baseURL, state }))), [registry])
  const [resolved, setResolved] = useState<{ generation: string; daemons: Daemon[] }>({ generation: "", daemons: [] })
  useEffect(() => {
    let alive = true
    void Promise.allSettled(registry.map((daemon) => resolveDaemon(daemon.id))).then((results) => {
      if (alive) setResolved({
        generation,
        daemons: results.flatMap((result, index) => result.status === "fulfilled" && result.value
          ? [{ ...registry[index], ...result.value }] : []),
      })
    })
    return () => { alive = false }
  }, [registry, generation])
  return { daemons: resolved.daemons, ready: resolved.generation === generation }
}
