import { useEffect, useMemo, useState } from "react"
import { toast } from "sonner"
import { Input } from "@/components/ui/input"
import { useTasksSocket } from "@/hooks/useTasksSocket"
import type { HostAgents } from "@/lib/aggregate"
import { buildTaskForest, flattenVisible, type VisibleTaskRow } from "@/lib/taskTree"
import { createTask, listTaskQueues, type TaskQueue, type TaskStatusView } from "@/lib/tasks"
import { hostToParam, targetFor } from "@/lib/terminalsHost"
import { TaskFilterBar } from "./TaskFilterBar"
import TaskDrawer, { type AssigneeChoice } from "./TaskDrawer"
import TaskRow, { TaskTableHeader } from "./TaskRow"
import { readTaskQueueFilter, persistTaskQueueFilter } from "./taskFilterStorage"
import { useAllServersTasks, type ServerTask } from "./useAllServersTasks"
import "./tasks.css"

// Search, status and server survive the Agents <-> All tasks switch the same
// way the queue filter does: per session, beside it.
const VIEW_KEY = "tasks:all-view:v1"
type ViewFilters = { query: string; statusView: TaskStatusView; server: string }
const DEFAULT_VIEW: ViewFilters = { query: "", statusView: "active", server: "" }

function readView(): ViewFilters {
  try {
    return { ...DEFAULT_VIEW, ...JSON.parse(globalThis.sessionStorage?.getItem(VIEW_KEY) ?? "{}") }
  } catch {
    return DEFAULT_VIEW
  }
}

function persistView(view: ViewFilters): void {
  try {
    globalThis.sessionStorage?.setItem(VIEW_KEY, JSON.stringify(view))
  } catch {
    // Web Storage is a best-effort Desktop convenience.
  }
}

const rowId = (task: ServerTask) => `${task.serverId}\u0000${task.key}`

/** One server's live stream. A hint only says "something changed", so the
 *  answer is to read that server again. */
function ServerTasksLive({ serverId, after, onChange }: { serverId: string; after: number; onChange: () => void }) {
  useTasksSocket({ target: targetFor(serverId), after, onHint: onChange, onReset: onChange })
  return null
}

/**
 * Every task on every server, in one table. The rows come from each server
 * separately, so trees are built per server and only their roots are merged,
 * newest first. A task opens in place, on its own server, and its assignee can
 * be any agent anywhere — one on another server moves the task there.
 */
export default function AllTasksWorkspace({ hosts }: { hosts: readonly HostAgents[] }) {
  const [view, setViewState] = useState(readView)
  const setView = (patch: Partial<ViewFilters>) => setViewState((current) => {
    const next = { ...current, ...patch }
    persistView(next)
    return next
  })
  const [queue, setQueueState] = useState(readTaskQueueFilter)
  const setQueue = (prefix: string) => {
    setQueueState(prefix)
    persistTaskQueueFilter(prefix)
  }
  const [expanded, setExpanded] = useState<Set<string>>(new Set())
  const [open, setOpen] = useState<{ serverId: string; key: string } | null>(null)
  const [creating, setCreating] = useState(false)

  const servers = useMemo(
    () => hosts.map((entry) => ({ id: entry.host.id, label: entry.host.label, error: entry.error })),
    [hosts],
  )
  const labelOf = (id: string) => servers.find((server) => server.id === id)?.label ?? (id || "local")
  const { tasks, errors, sequences, loading, reload, refresh } = useAllServersTasks(servers, {
    text: view.query,
    statusView: view.statusView,
  })

  const visible = useMemo(() => tasks.filter((task) =>
    (!view.server || hostToParam(task.serverId) === view.server) && (!queue || task.queue === queue)), [tasks, queue, view.server])

  const queueCounts = useMemo(() => {
    const counts = new Map<string, number>()
    for (const task of tasks) counts.set(task.queue, (counts.get(task.queue) ?? 0) + 1)
    return counts
  }, [tasks])
  const queues = useMemo(
    () => [...new Set([...queueCounts.keys(), ...(queue ? [queue] : [])])].sort().map((prefix) => ({ prefix })),
    [queue, queueCounts],
  )
  const serverItems = servers.map((server) => ({
    id: hostToParam(server.id),
    label: server.label,
    count: tasks.filter((task) => task.serverId === server.id).length,
    unavailable: Boolean(errors[server.id]) && !tasks.some((task) => task.serverId === server.id),
  }))

  const rows = useMemo(() => {
    const byServer = new Map<string, ServerTask[]>()
    for (const task of visible) byServer.set(task.serverId, [...(byServer.get(task.serverId) ?? []), task])
    const roots: Array<{ serverId: string; node: ReturnType<typeof buildTaskForest>[number] }> = []
    for (const [serverId, list] of byServer) {
      for (const node of buildTaskForest(list)) roots.push({ serverId, node })
    }
    roots.sort((left, right) => right.node.task.updated_at.localeCompare(left.node.task.updated_at))
    return roots.flatMap(({ serverId, node }) => {
      const open = new Set([...expanded].filter((id) => id.startsWith(`${serverId}\u0000`)).map((id) => id.slice(serverId.length + 1)))
      return flattenVisible([node], open).map((row) => ({ ...row, task: row.task as ServerTask }))
    })
  }, [expanded, visible])

  const assignees = useMemo<AssigneeChoice[]>(() => {
    if (!open) return []
    return hosts.filter((entry) => !entry.error).flatMap((entry) => entry.agents.map((item) => {
      const assignee = `agent:${item.name}`
      const local = entry.host.id === open.serverId
      return {
        // Same-server values are plain assignees, so an untouched field reads
        // as unchanged; another server's carry its id to stay distinct.
        value: local ? assignee : `${assignee}@${entry.host.id || "local"}`,
        label: `${item.name} · ${entry.host.label}`,
        assignee,
        ...(local ? {} : { hostId: entry.host.id, hostLabel: entry.host.label }),
      }
    }))
  }, [hosts, open])

  const filtersActive = Boolean(view.query || view.statusView !== "active" || view.server || queue)
  const clearFilters = () => {
    setView(DEFAULT_VIEW)
    setQueue("")
  }

  return (
    <div className="tasks-workspace" data-testid="all-tasks-workspace">
      {Object.entries(sequences).map(([serverId, after]) => (
        <ServerTasksLive key={serverId} serverId={serverId} after={after} onChange={() => refresh(serverId)} />
      ))}
      <main className="tasks-center">
        <TaskFilterBar
          mode="servers"
          query={view.query}
          onQuery={(query) => setView({ query })}
          statusView={view.statusView}
          onStatusView={(statusView) => setView({ statusView })}
          queues={queues}
          queue={queue}
          onQueue={setQueue}
          queueCounts={queueCounts}
          servers={serverItems}
          server={view.server}
          onServer={(server) => setView({ server })}
          refreshing={false}
          onRefresh={() => servers.forEach((server) => reload(server.id))}
          onCreate={() => setCreating(true)}
        />
        {creating && (
          <NewTaskForm
            hosts={hosts}
            onCancel={() => setCreating(false)}
            onCreated={(serverId, key) => {
              setCreating(false)
              reload(serverId)
              setOpen({ serverId, key })
            }}
          />
        )}
        {Object.entries(errors).map(([serverId, message]) => (
          <div key={serverId} data-testid="server-warning" title={message}
            className="px-4 pb-1.5 text-[12px] text-muted-foreground">
            {labelOf(serverId)} unavailable ·{" "}
            <button type="button" className="underline-offset-2 hover:underline" onClick={() => reload(serverId)}>Retry</button>
          </div>
        ))}
        {loading ? (
          <div aria-label="Loading tasks" role="status">
            {Array.from({ length: 6 }, (_, index) => (
              <div key={index} className="mx-4 my-[5px] h-5 animate-pulse rounded-[6px] bg-muted" />
            ))}
          </div>
        ) : (
          <div className="task-tree-list">
            <TaskTableHeader mode="servers" sticky />
            {rows.map((row: VisibleTaskRow & { task: ServerTask }) => {
              const id = rowId(row.task)
              return (
                <TaskRow
                  key={id}
                  row={row}
                  mode="servers"
                  server={row.task.serverName}
                  hasActiveQuestion={false}
                  expanded={expanded.has(id)}
                  selected={open !== null && `${open.serverId}\u0000${open.key}` === id}
                  onToggle={() => setExpanded((current) => {
                    const next = new Set(current)
                    if (!next.delete(id)) next.add(id)
                    return next
                  })}
                  onSelect={() => setOpen({ serverId: row.task.serverId, key: row.task.key })}
                />
              )
            })}
            {rows.length === 0 && (
              <div className="flex flex-col items-center gap-1 py-12">
                <span className="text-[13px] font-medium">No tasks</span>
                <span className="text-[12px] text-muted-foreground">
                  {filtersActive ? "Nothing matches the current filters." : "No server has tasks yet."}
                </span>
                {filtersActive && (
                  <button type="button" className="text-[12px] text-primary hover:underline" onClick={clearFilters}>
                    Clear filters
                  </button>
                )}
              </div>
            )}
          </div>
        )}
      </main>
      {open && (
        <TaskDrawer
          key={`${open.serverId}\u0000${open.key}`}
          taskKey={open.key}
          target={targetFor(open.serverId)}
          assignees={assignees}
          onClose={() => setOpen(null)}
          onMoved={(serverId) => {
            reload(open.serverId)
            reload(serverId)
            setOpen({ serverId, key: open.key })
          }}
        />
      )}
    </div>
  )
}

/** "+ New task" across servers: the agent decides the server, the server
 *  decides which queues exist. */
function NewTaskForm({
  hosts,
  onCancel,
  onCreated,
}: {
  hosts: readonly HostAgents[]
  onCancel: () => void
  onCreated: (serverId: string, key: string) => void
}) {
  const choices = hosts.filter((entry) => !entry.error).flatMap((entry) =>
    entry.agents.map((item) => ({ id: `${entry.host.id}\u0000${item.name}`, hostId: entry.host.id, name: item.name, label: `${item.name} · ${entry.host.label}` })))
  const [choice, setChoice] = useState("")
  const picked = choices.find((item) => item.id === choice)
  const [queues, setQueues] = useState<TaskQueue[]>([])
  const [queue, setQueue] = useState("")
  const [title, setTitle] = useState("")
  const [busy, setBusy] = useState(false)

  const hostId = picked?.hostId
  useEffect(() => {
    if (hostId === undefined) return
    let alive = true
    listTaskQueues(targetFor(hostId))
      .then((page) => {
        if (!alive) return
        setQueues(page.queues ?? [])
        setQueue((current) => page.queues?.some((item) => item.prefix === current) ? current : page.queues?.[0]?.prefix ?? "")
      })
      .catch((error: unknown) => {
        // The previous agent's queues do not exist on this server.
        if (alive) {
          setQueues([])
          setQueue("")
        }
        toast.error(error instanceof Error ? error.message : String(error))
      })
    return () => { alive = false }
  }, [hostId])

  const submit = async (event: React.FormEvent) => {
    event.preventDefault()
    if (!picked || !queue || !title.trim()) return
    setBusy(true)
    try {
      const created = await createTask(
        { queue, title: title.trim(), assignee: `agent:${picked.name}` },
        targetFor(picked.hostId),
      )
      onCreated(picked.hostId, created.key)
    } catch (error) {
      toast.error(error instanceof Error ? error.message : String(error))
    } finally {
      setBusy(false)
    }
  }

  return (
    <form className="task-inline-form" onSubmit={(event) => void submit(event)}>
      <select aria-label="Task agent" value={choice} onChange={(event) => setChoice(event.target.value)}>
        <option value="">Choose an agent</option>
        {choices.map((item) => <option key={item.id} value={item.id}>{item.label}</option>)}
      </select>
      <select aria-label="Task queue" value={queue} disabled={!picked} onChange={(event) => setQueue(event.target.value)}>
        {queues.map((item) => <option key={item.prefix} value={item.prefix}>{item.prefix}</option>)}
      </select>
      <Input
        autoFocus
        aria-label="Task title"
        placeholder="What needs to be done?"
        value={title}
        onChange={(event) => setTitle(event.target.value)}
      />
      <button type="submit" disabled={busy || !picked || !queue || !title.trim()}>Create task</button>
      <button type="button" onClick={onCancel}>Cancel</button>
    </form>
  )
}
