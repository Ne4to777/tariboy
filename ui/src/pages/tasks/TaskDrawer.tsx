import { useCallback, useEffect, useRef, useState } from "react"
import { toast } from "sonner"
import { useTasksSocket } from "@/hooks/useTasksSocket"
import type { ApiTarget } from "@/lib/api"
import { resolveDaemon } from "@/lib/daemons"
import { targetFor } from "@/lib/terminalsHost"
import {
  addTaskComment,
  addTaskRelation,
  deleteTaskRelation,
  exportTask,
  getTask,
  importTask,
  listTaskEvents,
  listTaskPrincipals,
  listTasks,
  transferTask,
  updateTask,
  type Task,
  type TaskDetail as Detail,
  type TaskEvent,
  type TaskPrincipals,
  type TaskPriority,
  type TaskRelationType,
  type TaskStatus,
} from "@/lib/tasks"
import TaskDetail, { TaskDetailLoading } from "./TaskDetail"
import TaskPanelResizeHandle from "./TaskPanelResizeHandle"
import { clampTaskDetailWidth, useTaskPanelWidths } from "./useTaskPanelWidths"
import "./tasks.css"

function idempotencyKey(): string {
  return globalThis.crypto?.randomUUID?.() ?? `task-action-${Date.now()}-${Math.random()}`
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error)
}

type TaskEdit = {
  revision: number
  title: string
  description: string
  pull_request: string
  status?: TaskStatus
  assignee?: string
  manual_block_reason?: string
  priority: TaskPriority
}

/** An assignee the drawer may offer. `hostId` is set when the agent lives on
 *  another server: picking it moves the task there first. */
export interface AssigneeChoice {
  value: string
  label: string
  assignee: string
  hostId?: string
  hostLabel?: string
}

/**
 * One task, opened where it was mentioned. It is the same panel the Tasks
 * workspace shows, but it owns its own load and its own writes, so a chat — or
 * anything else that names a task key — can open it without navigating to
 * Tasks and without borrowing that workspace's tree state.
 */
export default function TaskDrawer({
  taskKey,
  target,
  onClose,
  assignees,
  onMoved,
}: {
  taskKey: string
  target?: ApiTarget
  onClose: () => void
  assignees?: readonly AssigneeChoice[]
  /** The task now lives on `hostId`; the owner reopens it there. */
  onMoved?: (hostId: string) => void
}) {
  // The same stored width and grip as the Tasks tab, so the panel is one size
  // wherever it opens; the drawer spans the window rather than a workspace.
  const { detailWidth, setDetailWidth } = useTaskPanelWidths()
  const resizeHandle = <TaskPanelResizeHandle
    width={detailWidth}
    maximum={clampTaskDetailWidth(Infinity)}
    onResize={setDetailWidth}
  />
  const [detail, setDetail] = useState<Detail | null>(null)
  const [events, setEvents] = useState<TaskEvent[]>([])
  const [principals, setPrincipals] = useState<TaskPrincipals | null>(null)
  const mountedRef = useRef(true)
  useEffect(() => {
    mountedRef.current = true
    return () => { mountedRef.current = false }
  }, [])

  const load = useCallback(async () => {
    try {
      const [next, history] = await Promise.all([
        getTask(taskKey, target),
        listTaskEvents(taskKey, 0, 200, target),
      ])
      if (!mountedRef.current) return
      setDetail(next)
      setEvents(history.events ?? [])
    } catch (error) {
      if (!mountedRef.current) return
      toast.error(errorMessage(error))
      onClose()
    }
  }, [onClose, target, taskKey])

  useEffect(() => { void Promise.resolve().then(load) }, [load])
  useEffect(() => {
    // The assignee picker needs the host's principals; without them the panel
    // still edits everything else.
    listTaskPrincipals(target)
      .then((people) => { if (mountedRef.current) setPrincipals(people) })
      .catch(() => {})
  }, [target])

  // The socket replays everything after the sequence it is given, so it opens
  // only once the current one is known: from zero it would replay the host's
  // whole task log and reload this panel once per event.
  const [sequence, setSequence] = useState(0)
  const [live, setLive] = useState(false)
  useEffect(() => {
    listTasks({ limit: 1 }, target)
      .then((page) => {
        if (!mountedRef.current) return
        setSequence(page.sequence ?? 0)
        setLive(true)
      })
      .catch(() => {})
  }, [target])
  useTasksSocket({
    target,
    after: sequence,
    enabled: live,
    onHint: (hint) => {
      setSequence(hint.sequence)
      // Every task on the host shares this stream; only this one's events
      // change what the panel shows.
      if (!hint.task_key || hint.task_key === taskKey) void load()
    },
    // A reset says the host pruned events this panel may have missed; the
    // reload covers that, and the stream stays where it is.
    onReset: () => { void load() },
  })

  // An agent on another server can only work a task that lives there. Other
  // edits are saved here first; then the tree is exported and imported with
  // its root unassigned, so no same-named agent over there takes it before
  // the real assignment, which is an ordinary update and notifies the agent.
  // Once the import succeeds the task lives there: a later failure is
  // reported, and the sheet follows the task all the same.
  const moveToAgent = async (task: Task, input: TaskEdit, remote: AssigneeChoice): Promise<Task> => {
    const key = task.key
    const edited = (["title", "description", "pull_request", "status", "priority", "manual_block_reason"] as const)
      .some((field) => input[field] !== undefined && input[field] !== (task[field] ?? ""))
    if (edited) await updateTask(key, { ...input, assignee: task.assignee }, target)
    const hostId = remote.hostId!
    const destination = targetFor(hostId)
    const label = remote.hostLabel || hostId || "local"
    const bundle = await exportTask(key, target)
    const tasks = (bundle.tasks as Array<{ key: string; assignee: string }>)
      .map((item) => item.key === bundle.root_key ? { ...item, assignee: "" } : item)
    const imported = await importTask({ ...bundle, tasks }, destination)
    let result = imported
    try {
      await addTaskComment(key, `Moved to ${label} as ${key}.`, target, idempotencyKey())
      const left = await getTask(key, target)
      await updateTask(key, { status: "cancelled", revision: left.task.revision }, target)
      const arrived = await getTask(key, destination)
      result = await updateTask(key, { assignee: remote.assignee, revision: arrived.task.revision }, destination)
      toast.success(`${key} moved to ${label}`)
    } catch (error) {
      toast.error(`${key} moved to ${label}, but the move did not finish: ${errorMessage(error)}`)
    }
    onMoved?.(hostId)
    return result
  }

  if (detail?.task.key !== taskKey) {
    return <TaskDetailLoading taskKey={taskKey} width={detailWidth} resizeHandle={resizeHandle} onClose={onClose} />
  }

  return (
    <TaskDetail
      key={detail.task.key}
      target={target}
      detail={detail}
      principals={principals}
      events={events}
      width={detailWidth}
      resizeHandle={resizeHandle}
      onClose={onClose}
      assigneeOptions={assignees}
      onTransfer={async (hostID: string) => {
        const host = await resolveDaemon(hostID)
        if (!host) throw new Error("That server is no longer registered")
        const label = host.label || host.id
        await transferTask(detail.task.key, target, host, label, idempotencyKey())
        toast.success(`${detail.task.key} moved to ${label}`)
        await load()
      }}
      onSave={async (input: TaskEdit) => {
        const task = detail.task
        const remote = assignees?.find((choice) => choice.hostId !== undefined && choice.value === input.assignee)
        // A closed list can lose an entry between the pick and Save (its server
        // stopped answering). Its value is not a real assignee, so never write it.
        if (assignees && !remote && input.assignee && input.assignee !== task.assignee
          && input.assignee !== principals?.customer && !assignees.some((choice) => choice.value === input.assignee)) {
          const message = "That agent's server is unavailable; pick the assignee again"
          toast.error(message)
          throw new Error(message)
        }
        try {
          if (remote?.hostId !== undefined) {
            return await moveToAgent(task, input, remote)
          }
          const updated = await updateTask(detail.task.key, input, target)
          setDetail((current) => current ? { ...current, task: updated } : current)
          toast.success("Task updated")
          return updated
        } catch (error) {
          toast.error(errorMessage(error))
          await load()
          throw error
        }
      }}
      onComment={async (body: string, key: string) => {
        try {
          await addTaskComment(detail.task.key, body, target, key)
          await load()
        } catch (error) {
          toast.error(errorMessage(error))
          throw error
        }
      }}
      onAddRelation={async (targetKey: string, type: TaskRelationType) => {
        await addTaskRelation(
          detail.task.key, targetKey, type, detail.task.revision, target, idempotencyKey(),
        )
        await load()
      }}
      onDeleteRelation={async (relationID: number) => {
        await deleteTaskRelation(
          detail.task.key, relationID, detail.task.revision, target, idempotencyKey(),
        )
        await load()
      }}
    />
  )
}
