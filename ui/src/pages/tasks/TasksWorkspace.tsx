import { useCallback, useEffect, useMemo, useRef, useState } from "react"
import { toast } from "sonner"
import { useOptionalDaemons } from "@/components/DaemonProvider"
import { useTasksSocket } from "@/hooks/useTasksSocket"
import { ApiError, type ApiTarget } from "@/lib/api"
import { buildTaskForest, canDropTaskInside } from "@/lib/taskTree"
import { resolveDaemon } from "@/lib/daemons"
import {
  addTaskComment,
  addTaskRelation,
  createTask,
  createTaskQueue,
  deleteTaskRelation,
  getTask,
  getTaskWorkflow,
  listTaskNotifications,
  listTaskEvents,
  listTaskPrincipals,
  listTaskQueues,
  type listTasks,
  markTaskNotificationRead,
  moveTask,
  transferTask,
  updateTaskQueue,
  updateTask,
  type CreateQueueInput,
  type Task,
  type TaskDetail as Detail,
  type TaskPriority,
  type TaskEvent,
  type TaskNotification,
  type TaskPrincipals,
  type TaskQueue,
  type TaskRelationType,
  type TaskStatus,
  type TaskStatusView,
  type WorkflowExecutionView,
} from "@/lib/tasks"
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet"
import QueueSettings from "./QueueSettings"
import { TaskFilterBar, type TaskPrincipalFilter } from "./TaskFilterBar"
import TaskDetail, { TaskDetailLoading } from "./TaskDetail"
import TaskForm from "./TaskForm"
import TaskTree from "./TaskTree"
import TaskPanelResizeHandle from "./TaskPanelResizeHandle"
import { listAllTasks } from "./useAllServersTasks"
import { persistTaskQueueFilter, readTaskQueueFilter } from "./taskFilterStorage"
import { MIN_TASK_DETAIL_WIDTH, useTaskPanelWidths } from "./useTaskPanelWidths"
import "./tasks.css"

function actorAgent(name: string): string {
  return name.startsWith("agent:") ? name : `agent:${name}`
}

function idempotencyKey(): string {
  return globalThis.crypto?.randomUUID?.() ?? `task-action-${Date.now()}-${Math.random()}`
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error)
}

type TasksWorkspaceProps = {
  scopeAgent?: string
  target?: ApiTarget
  initialTaskKey?: string
  onNotificationsChanged?: () => void
}

function TasksWorkspaceContent({
  scopeAgent,
  target,
  initialTaskKey,
  onNotificationsChanged,
}: TasksWorkspaceProps) {
  const workspaceRef = useRef<HTMLDivElement | null>(null)
  const { detailWidth, setDetailWidth } = useTaskPanelWidths()
  const [workspaceWidth, setWorkspaceWidth] = useState(0)
  useEffect(() => {
    const workspace = workspaceRef.current
    if (!workspace) return
    const measure = () => setWorkspaceWidth(workspace.getBoundingClientRect().width)
    measure()
    const observer = new ResizeObserver(measure)
    observer.observe(workspace)
    window.addEventListener("resize", measure)
    return () => {
      observer.disconnect()
      window.removeEventListener("resize", measure)
    }
  }, [])
  const detailMaximum = workspaceWidth > 0
    ? Math.max(MIN_TASK_DETAIL_WIDTH, workspaceWidth - 80)
    : Math.max(MIN_TASK_DETAIL_WIDTH, (globalThis.innerWidth || 820) - 80)
  const effectiveDetailWidth = Math.min(detailWidth, detailMaximum)
  const [queues, setQueues] = useState<TaskQueue[]>([])
  const [principals, setPrincipals] = useState<TaskPrincipals | null>(null)
  const [metadataError, setMetadataError] = useState("")
  const [tasks, setTasks] = useState<Task[]>([])
  const [notifications, setNotifications] = useState<TaskNotification[]>([])
  const [view, setView] = useState<TaskPrincipalFilter>("")
  const [queuesOpen, setQueuesOpen] = useState(false)
  const [statusView, setStatusView] = useState<TaskStatusView>("active")
  const [queue, setQueue] = useState(readTaskQueueFilter)
  const [query, setQuery] = useState("")
  const selectQueue = useCallback((prefix: string) => {
    setQueue(prefix)
    persistTaskQueueFilter(prefix)
  }, [])
  const [expanded, setExpanded] = useState<Set<string>>(new Set())
  const [selectedKey, setSelectedKey] = useState("")
  const selectedKeyRef = useRef("")
  const [detail, setDetail] = useState<Detail | null>(null)
  // The task a click asked for whose detail has not arrived yet; the sheet
  // opens on it at once instead of waiting for the read.
  const [loadingKey, setLoadingKey] = useState("")
  const [events, setEvents] = useState<TaskEvent[]>([])
  const [workflow, setWorkflow] = useState<WorkflowExecutionView | null>(null)
  const [creatingParent, setCreatingParent] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)
  const [refreshing, setRefreshing] = useState(false)
  const [sequence, setSequence] = useState(0)
  const [socketEnabled, setSocketEnabled] = useState(false)
  const hasLoadedTreeRef = useRef(false)
  const treeRequestRef = useRef(0)
  const metadataRequestRef = useRef(0)
  const detailRequestRef = useRef(0)
  const mountedRef = useRef(true)
  const realtimeRefreshRef = useRef({ running: false, pending: false, scheduled: false })
  const principalFilter = !scopeAgent && view !== "" ? principals?.customer : undefined
  const principalMetadataError = !scopeAgent && view !== "" ? metadataError : ""

  const loadMetadata = useCallback(async () => {
    const request = ++metadataRequestRef.current
    try {
      const [queuePage, people, inbox] = await Promise.all([
        listTaskQueues(target),
        listTaskPrincipals(target),
        listTaskNotifications(false, target),
      ])
      if (!mountedRef.current || request !== metadataRequestRef.current) return
      const nextQueues = queuePage.queues ?? []
      setQueues(nextQueues)
      setPrincipals(people)
      setNotifications(inbox.notifications ?? [])
      setMetadataError("")
    } catch (error) {
      if (!mountedRef.current || request !== metadataRequestRef.current) return
      const message = error instanceof Error ? error.message : String(error)
      setMetadataError(message)
      toast.error(message)
    }
  }, [target])

  const loadTree = useCallback(async () => {
    const request = ++treeRequestRef.current
    if (!scopeAgent && view !== "" && !principalFilter) {
      setLoading(!principalMetadataError)
      setRefreshing(false)
      return
    }
    const initial = !hasLoadedTreeRef.current
    if (initial) setLoading(true)
    else setRefreshing(true)
    try {
      const filters: Parameters<typeof listTasks>[0] = {
        text: query,
        scope_agent: scopeAgent,
        status_view: statusView,
      }
      if (view === "mine") filters.assignee = scopeAgent ? actorAgent(scopeAgent) : principalFilter
      if (view === "waiting") filters.waiting_for = scopeAgent ? actorAgent(scopeAgent) : principalFilter
      const { tasks: loaded, sequence: latestSequence } = await listAllTasks(filters, target)
      if (request !== treeRequestRef.current) return
      setTasks(loaded)
      setSequence((current) => Math.max(current, latestSequence))
      hasLoadedTreeRef.current = true
      setSocketEnabled(true)
    } catch (error) {
      if (mountedRef.current && request === treeRequestRef.current) {
        toast.error(error instanceof Error ? error.message : String(error))
      }
    } finally {
      if (request === treeRequestRef.current) {
        setLoading(false)
        if (!initial) setRefreshing(false)
      }
    }
  }, [principalFilter, principalMetadataError, query, scopeAgent, statusView, target, view])

  const loadDetail = useCallback(async (key: string) => {
    const refreshingSelection = selectedKeyRef.current === key
    selectedKeyRef.current = key
    const request = ++detailRequestRef.current
    if (!refreshingSelection) setLoadingKey(key)
    try {
      const loadHistory = async (): Promise<TaskEvent[]> => {
        const history: TaskEvent[] = []
        let after = 0
        for (;;) {
          const page = await listTaskEvents(key, after, 200, target)
          const batch = page.events ?? []
          history.push(...batch)
          if (batch.length < 200) return history.slice(-200)
          const next = batch[batch.length - 1]?.sequence ?? after
          if (next <= after) throw new Error("task history cursor did not advance")
          after = next
        }
      }
      const [next, history] = await Promise.all([
        getTask(key, target),
        loadHistory(),
      ])
      if (!mountedRef.current || request !== detailRequestRef.current || selectedKeyRef.current !== key) return
      setLoadingKey("")
      setDetail(next)
      setEvents(history)
      setWorkflow(null)
      setSelectedKey(key)
      // The execution projection is read for one thing the panel still shows:
      // whether the workflow is frozen. A failure leaves that banner off
      // rather than speaking for itself.
      if (next.task.workflow_version_id) {
        const current = () => mountedRef.current && request === detailRequestRef.current
        await getTaskWorkflow(key, target)
          .then((value) => { if (current()) setWorkflow(value) })
          .catch(() => {})
        if (!current()) return
      }
    } catch (error) {
      if (!mountedRef.current || request !== detailRequestRef.current) return
      setLoadingKey("")
      if (refreshingSelection) {
        toast.error(errorMessage(error))
        return
      }
      selectedKeyRef.current = ""
      setSelectedKey("")
      setDetail(null)
      setEvents([])
      setWorkflow(null)
      toast.error(error instanceof Error ? error.message : String(error))
    }
  }, [target])

  // Opening a task is the customer seeing its question. Marking it read here is
  // what clears the row's indicator, and the only place that does: the workspace
  // has no notification inbox of its own.
  useEffect(() => {
    if (!selectedKey) return
    const unread = notifications.filter((notification) =>
      notification.task_key === selectedKey
      && notification.type === "task.question"
      && !notification.read_at
      && !notification.dismissed_at)
    if (unread.length === 0) return
    void (async () => {
      const read = new Set<string>()
      for (const notification of unread) {
        try {
          await markTaskNotificationRead(notification.id, target)
          read.add(notification.id)
        } catch {
          // The indicator stays; the next open of the task tries again.
        }
      }
      if (!mountedRef.current || read.size === 0) return
      const readAt = new Date().toISOString()
      setNotifications((current) => current.map((notification) =>
        read.has(notification.id) ? { ...notification, read_at: readAt } : notification))
      onNotificationsChanged?.()
    })()
  }, [notifications, onNotificationsChanged, selectedKey, target])

  const refreshInbox = useCallback(async () => {
    try {
      const page = await listTaskNotifications(false, target)
      setNotifications(page.notifications)
    } catch {
      // HTTP remains primary; an inbox refresh failure must not blank the tree.
    }
  }, [target])

  const refreshFromRealtimeHint = useCallback(() => {
    const refresh = realtimeRefreshRef.current
    if (refresh.running) {
      refresh.pending = true
      return
    }
    if (refresh.scheduled) return
    refresh.scheduled = true
    void Promise.resolve().then(async () => {
      refresh.scheduled = false
      if (!mountedRef.current) return
      refresh.running = true
      do {
        refresh.pending = false
        await Promise.all([
          loadTree(),
          loadMetadata(),
          refreshInbox(),
          selectedKeyRef.current ? loadDetail(selectedKeyRef.current) : Promise.resolve(),
        ])
      } while (refresh.pending && mountedRef.current)
      refresh.running = false
    })
  }, [loadDetail, loadMetadata, loadTree, refreshInbox])

  useEffect(() => {
    mountedRef.current = true
    return () => {
      mountedRef.current = false
    }
  }, [])
  useEffect(() => {
    void Promise.resolve().then(loadMetadata)
  }, [loadMetadata])
  useEffect(() => {
    void Promise.resolve().then(loadTree)
  }, [loadTree])
  // Open each deep-link key once: loadDetail changes identity whenever the
  // host Daemon object is re-created, and the key stays in the URL after close.
  const openedInitialKeyRef = useRef("")
  useEffect(() => {
    if (!initialTaskKey || openedInitialKeyRef.current === initialTaskKey) return
    openedInitialKeyRef.current = initialTaskKey
    void Promise.resolve().then(() => loadDetail(initialTaskKey))
  }, [initialTaskKey, loadDetail])

  useTasksSocket({
    target,
    after: sequence,
    enabled: socketEnabled,
    onHint: (hint) => {
      setSequence(hint.sequence)
      refreshFromRealtimeHint()
    },
    onReset: () => {
      setSequence(0)
      refreshFromRealtimeHint()
    },
  })

  const create = async (input: { queue: string; parent_key: string; title: string }) => {
    try {
      const created = await createTask(input, target)
      setCreatingParent(null)
      if (input.parent_key) {
        setExpanded((current) => new Set(current).add(input.parent_key))
      }
      await loadTree()
      await loadDetail(created.key)
    } catch (error) {
      toast.error(error instanceof Error ? error.message : String(error))
    }
  }

  const saveDetail = async (input: {
    revision: number
    title: string
    description: string
    pull_request: string
    status?: TaskStatus
    assignee?: string
    manual_block_reason?: string
    priority: TaskPriority
  }) => {
    if (!detail) throw new Error("Task is no longer selected")
    try {
      const updated = await updateTask(detail.task.key, {
        ...input,
      }, target)
      setDetail((current) => current ? { ...current, task: updated } : current)
      setTasks((current) => current.map((task) => task.key === updated.key ? updated : task))
      toast.success("Task updated")
      return updated
    } catch (error) {
      if (error instanceof ApiError && error.status === 409) {
        await loadTree()
        await loadDetail(detail.task.key)
      }
      toast.error(error instanceof Error ? error.message : String(error))
      throw error
    }
  }

  const comment = async (body: string, idempotencyKey: string) => {
    if (!detail) return
    try {
      await addTaskComment(detail.task.key, body, target, idempotencyKey)
      await loadDetail(detail.task.key)
    } catch (error) {
      toast.error(error instanceof Error ? error.message : String(error))
      throw error
    }
  }

  const performMove = async (key: string, parentKey: string, beforeKey: string) => {
    const moving = tasks.find((task) => task.key === key)
    const parent = tasks.find((task) => task.key === parentKey)
    if (!moving) return
    const forest = buildTaskForest(tasks)
    if ((parent && parent.queue !== moving.queue) || (parentKey && !canDropTaskInside(forest, key, parentKey))) {
      toast.error("Tasks can only move inside their queue and outside their own subtree")
      return
    }
    const previous = tasks
    setTasks((current) => current.map((task) => task.key === key ? { ...task, parent_key: parentKey } : task))
    try {
      const updated = await moveTask(key, {
        parent_key: parentKey,
        before_key: beforeKey,
        revision: moving.revision,
      }, target)
      setTasks((current) => current.map((task) => task.key === key ? updated : task))
      if (parentKey) setExpanded((current) => new Set(current).add(parentKey))
      await loadTree()
    } catch (error) {
      setTasks(previous)
      if (error instanceof ApiError && error.status === 409) await loadTree()
      toast.error(error instanceof Error ? error.message : String(error))
    }
  }

  const transferDetail = async (hostID: string) => {
    if (!detail) throw new Error("Task is no longer selected")
    const key = detail.task.key
    const host = await resolveDaemon(hostID)
    if (!host) throw new Error("That server is no longer registered")
    const label = host.label || host.id
    await transferTask(key, target, host, label, idempotencyKey())
    toast.success(`${key} moved to ${label}`)
    await loadTree()
    await loadDetail(key)
  }

  const createQueue = async (input: CreateQueueInput) => {
    try {
      const created = await createTaskQueue(input, target)
      setQueues((current) => [...current, created])
      selectQueue(created.prefix)
    } catch (error) {
      toast.error(error instanceof Error ? error.message : String(error))
    }
  }

  const updateQueue = async (prefix: string, input: Parameters<typeof updateTaskQueue>[1]) => {
    try {
      const updated = await updateTaskQueue(prefix, input, target)
      setQueues((current) => current.map((item) => item.prefix === prefix ? updated : item))
      toast.success("Queue updated")
    } catch (error) {
      if (error instanceof ApiError && error.status === 409) await loadMetadata()
      toast.error(error instanceof Error ? error.message : String(error))
    }
  }

  // Which shape the table takes: one agent's tasks, or a whole server's.
  const mode = scopeAgent ? "agent" : "all"

  // "+ New task" from the handoff: the new row must be visible the moment it
  // lands, so the search that would hide it is cleared and a Closed-only view
  // switches back to Active. The expansion state is deliberately untouched.
  const startCreate = () => {
    setQuery("")
    setStatusView((current) => (current === "closed" ? "active" : current))
    setCreatingParent("")
  }

  const activeQuestionTaskKeys = useMemo(
    () => new Set(notifications
      .filter((notification) => notification.type === "task.question" && !notification.read_at && !notification.dismissed_at)
      .map((notification) => notification.task_key)),
    [notifications],
  )

  // An unread question stays until its task is opened, but the Active list
  // leaves out closed tasks, so their questions would count in the badge with
  // no row to open. Such tasks are read one by one and kept in the Active list
  // until their question is read.
  const [questionTasks, setQuestionTasks] = useState<Task[]>([])
  const missingQuestionKeys = useMemo(() => {
    if (statusView !== "active" || query || view !== "") return []
    const listed = new Set(tasks.map((task) => task.key))
    return [...new Set(notifications
      .filter((notification) => notification.type === "task.question" && !notification.read_at && !notification.dismissed_at
        && (!scopeAgent || notification.requesting_principal === actorAgent(scopeAgent)))
      .map((notification) => notification.task_key))]
      .filter((key) => !listed.has(key))
      .sort()
  }, [notifications, query, scopeAgent, statusView, tasks, view])
  const missingQuestionKey = missingQuestionKeys.join(" ")
  useEffect(() => {
    if (!missingQuestionKey) return
    let current = true
    void Promise.all(missingQuestionKey.split(" ").map((key) =>
      getTask(key, target).then((loaded) => loaded.task, () => null)))
      .then((loaded) => {
        if (current && mountedRef.current) setQuestionTasks(loaded.filter((task): task is Task => task !== null))
      })
    return () => { current = false }
  }, [missingQuestionKey, target])
  const listedTasks = useMemo(() => {
    const wanted = new Set(missingQuestionKeys)
    const extra = questionTasks.filter((task) => wanted.has(task.key) || task.key === selectedKey)
    const listed = new Set(tasks.map((task) => task.key))
    return [...tasks, ...extra.filter((task) => !listed.has(task.key))]
  }, [missingQuestionKeys, questionTasks, selectedKey, tasks])

  // The queue filter is applied here rather than in `listTasks`, so the queue
  // menu can count the queues from the same load instead of asking again.
  const visibleTasks = useMemo(
    () => queue ? listedTasks.filter((task) => task.queue === queue) : listedTasks,
    [listedTasks, queue],
  )
  const queueCounts = useMemo(() => {
    const counts = new Map<string, number>()
    for (const task of tasks) counts.set(task.queue, (counts.get(task.queue) ?? 0) + 1)
    return counts
  }, [tasks])

  const closeDetail = () => {
    detailRequestRef.current += 1
    selectedKeyRef.current = ""
    setLoadingKey("")
    setSelectedKey("")
    setDetail(null)
    setEvents([])
    setWorkflow(null)
  }
  const detailResizeHandle = <TaskPanelResizeHandle
    width={effectiveDetailWidth}
    maximum={detailMaximum}
    workspaceRef={workspaceRef}
    onResize={setDetailWidth}
  />

  return (
    <div
      ref={workspaceRef}
      className="tasks-workspace"
      data-testid="tasks-workspace"
      data-scope-agent={scopeAgent ?? ""}
    >
      <main className="tasks-center">
        <TaskFilterBar
          mode={mode}
          query={query}
          onQuery={setQuery}
          statusView={statusView}
          onStatusView={setStatusView}
          principalFilter={view}
          onPrincipalFilter={setView}
          queues={queues}
          queue={queue}
          onQueue={selectQueue}
          queueCounts={queueCounts}
          onManageQueues={() => setQueuesOpen(true)}
          scopeAgent={scopeAgent}
          refreshing={refreshing}
          onRefresh={() => {
            if (!scopeAgent && view !== "" && !principalFilter) void loadMetadata()
            else void loadTree()
          }}
          onCreate={startCreate}
        />
        {creatingParent === "" && (
          <TaskForm queues={queues} initialQueue={queue} onCreate={create} onCancel={() => setCreatingParent(null)} />
        )}
        {principalMetadataError && !principalFilter
          ? <div className="tasks-empty" role="alert">{principalMetadataError}</div>
          : loading ? <div className="tasks-empty">Loading tasks…</div> : (
          <TaskTree
            tasks={visibleTasks}
            mode={mode}
            activeQuestionTaskKeys={activeQuestionTaskKeys}
            expanded={expanded}
            selectedKey={selectedKey}
            onToggle={(key) => setExpanded((current) => {
              const next = new Set(current)
              if (next.has(key)) next.delete(key)
              else next.add(key)
              return next
            })}
            onSelect={(key) => void loadDetail(key)}
            onAddChild={setCreatingParent}
            onMove={(key, parentKey, beforeKey) => void performMove(key, parentKey, beforeKey)}
          />
        )}
        {creatingParent && (
          <TaskForm
            queues={queues}
            initialQueue={tasks.find((task) => task.key === creatingParent)?.queue || queue}
            parentKey={creatingParent}
            onCreate={create}
            onCancel={() => setCreatingParent(null)}
          />
        )}
      </main>
      <Sheet open={queuesOpen} onOpenChange={setQueuesOpen}>
        <SheetContent style={{ width: 340, maxWidth: 340 }} className="gap-0 p-0">
          <SheetHeader className="px-4 pt-4 pb-2">
            <SheetTitle className="text-[14px] font-semibold">Queues</SheetTitle>
            <SheetDescription className="sr-only">
              Create a queue, rename one, or bind its workflow.
            </SheetDescription>
          </SheetHeader>
          <QueueSettings queues={queues} onCreate={createQueue} onUpdate={updateQueue} target={target} />
        </SheetContent>
      </Sheet>
      {loadingKey && detail?.task.key !== loadingKey ? (
        <TaskDetailLoading
          taskKey={loadingKey}
          width={effectiveDetailWidth}
          resizeHandle={detailResizeHandle}
          onClose={closeDetail}
        />
      ) : detail && (
        <TaskDetail
          key={detail.task.key}
          target={target}
          detail={detail}
          principals={principals}
          width={effectiveDetailWidth}
          resizeHandle={detailResizeHandle}
          onClose={closeDetail}
          events={events}
          workflow={workflow}
          onSave={saveDetail}
          onComment={comment}
          onAddRelation={async (targetKey: string, type: TaskRelationType) => {
            await addTaskRelation(
              detail.task.key, targetKey, type, detail.task.revision, target, idempotencyKey(),
            )
            await loadDetail(detail.task.key)
          }}
          onTransfer={transferDetail}
          onDeleteRelation={async (relationID: number) => {
            await deleteTaskRelation(
              detail.task.key, relationID, detail.task.revision, target, idempotencyKey(),
            )
            await loadDetail(detail.task.key)
          }}
        />
      )}
    </div>
  )
}

export default function TasksWorkspace(props: TasksWorkspaceProps) {
  const activeId = useOptionalDaemons()?.activeId ?? ""
  const targetKey = props.target === undefined
    ? `active:${activeId || "local"}`
    : props.target === null
      ? "explicit:local"
      : `explicit:${props.target.id}:${props.target.baseURL}:${props.target.token}`
  return <TasksWorkspaceContent key={`${targetKey}:${props.scopeAgent ?? ""}`} {...props} />
}
