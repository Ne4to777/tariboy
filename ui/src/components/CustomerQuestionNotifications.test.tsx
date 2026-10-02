import { act, render, screen, waitFor } from "@testing-library/react"
import { useLayoutEffect, useRef } from "react"
import { beforeEach, describe, expect, it, vi } from "vitest"
import { MemoryRouter, Route, Routes, useLocation, useParams, useSearchParams } from "react-router-dom"
import type { Task, TaskDetail, TaskNotification } from "@/lib/tasks"

const model = vi.hoisted(() => ({
  daemons: [] as Array<{ id: string; label: string }>,
  snapshots: new Map<string, TaskNotification[]>(),
  sequences: new Map<string, number>(),
  failures: new Set<string>(),
  shown: [] as Array<{
    host_id: string
    notification_id: string
    task_key: string
    server_label: string
    agent_name: string
  }>,
  sockets: new Map<string, {
    after?: number
    onHint: (hint: { sequence: number; kind: string }) => void
    setStatus: (status: "connecting" | "open" | "closed") => void
  }>(),
  deferredInbox: null as Promise<{ notifications: TaskNotification[]; count: number }> | null,
  activation: undefined as undefined | ((payload: { host_id: string; notification_id: string; task_key: string }) => void),
  readAttempts: [] as Array<{ notificationId: string; hostId: string | undefined }>,
  rejectRead: false,
  markReadPromise: null as Promise<void> | null,
  taskDetails: new Map<string, TaskDetail>(),
}))

vi.mock("@/components/DaemonProvider", () => ({
  useDaemons: () => ({ daemons: model.daemons }),
  useOptionalDaemons: () => ({ activeId: "" }),
}))

vi.mock("@/lib/daemons", () => ({
  resolveDaemon: async (id: string) => id ? {
    id,
    label: model.daemons.find((host) => host.id === id)?.label ?? id,
    baseURL: `http://${id}.test`,
    token: "test-token",
  } : null,
}))

vi.mock("@/lib/tasks", async (importOriginal) => ({
  ...await importOriginal<typeof import("@/lib/tasks")>(),
  listTaskNotifications: async (_includeDismissed: boolean, target?: { id?: string }) => {
    const hostId = target?.id ?? ""
    if (model.failures.has(hostId)) throw new Error(`inbox failed for ${hostId || "local"}`)
    if (model.deferredInbox) return model.deferredInbox
    const notifications = model.snapshots.get(hostId) ?? []
    return { notifications, count: notifications.length }
  },
  listTasks: async (_filters: unknown, target?: { id?: string }) => {
    const hostId = target?.id ?? ""
    if (model.failures.has(hostId)) throw new Error(`tasks failed for ${hostId || "local"}`)
    return { tasks: [], sequence: model.sequences.get(hostId) ?? 1 }
  },
  listTaskQueues: async () => ({ queues: [], count: 0 }),
  listTaskPrincipals: async () => ({ customer: "user:customer", agents: ["alice"], groups: [] }),
  listTaskEvents: async () => ({ events: [], count: 0 }),
  getTask: async (key: string) => {
    const detail = model.taskDetails.get(key)
    if (!detail) throw new Error(`missing task ${key}`)
    return detail
  },
  markTaskNotificationRead: async (notificationId: string, target?: { id?: string } | null) => {
    const hostId = target?.id
    model.readAttempts.push({ notificationId, hostId })
    if (model.rejectRead) throw new Error("read failed")
    if (model.markReadPromise) await model.markReadPromise
    const id = hostId ?? ""
    model.snapshots.set(id, (model.snapshots.get(id) ?? []).map((item) =>
      item.id === notificationId ? { ...item, read_at: "2026-08-18T12:05:00Z" } : item,
    ))
  },
}))

vi.mock("@/hooks/useTasksSocket", async () => {
  const React = await import("react")
  return {
    useTasksSocket: (options: {
    target?: { id?: string }
    enabled?: boolean
    after?: number
    onHint: (hint: { sequence: number; kind: string }) => void
    }) => {
    const hostId = options.target?.id ?? ""
    const [status, setStatus] = React.useState<"connecting" | "open" | "closed">("open")
    React.useEffect(() => {
      if (options.enabled) model.sockets.set(hostId, { ...options, setStatus })
      else model.sockets.delete(hostId)
      return () => { model.sockets.delete(hostId) }
    }, [hostId, options.enabled])
    return options.enabled ? status : "closed"
  },
  }
})

vi.mock("@/lib/desktop", async (importOriginal) => ({
  ...await importOriginal<typeof import("@/lib/desktop")>(),
  showTaskNotification: async (input: (typeof model.shown)[number]) => {
    model.shown.push(input)
    return { outcome: "shown" as const }
  },
  onTaskNotificationActivated: (cb: (payload: { host_id: string; notification_id: string; task_key: string }) => void) => {
    model.activation = cb
    return () => {
      if (model.activation === cb) model.activation = undefined
    }
  },
}))

import { CustomerQuestionNotifications } from "./CustomerQuestionNotifications"
import {
  useCustomerQuestionNotifications,
} from "./customerQuestionNotificationsContext"
import TasksWorkspace from "@/pages/tasks/TasksWorkspace"

function notification(overrides: Partial<TaskNotification> = {}): TaskNotification {
  return {
    id: "notification-1",
    channel: "user:customer",
    type: "task.question",
    text: "alice needs your answer",
    requesting_principal: "agent:alice",
    task_key: "ASK-1",
    event_sequence: 1,
    created_at: "2026-08-18T12:00:00Z",
    published_at: "2026-08-18T12:00:00Z",
    read_at: "",
    dismissed_at: "",
    ...overrides,
  }
}

function AttentionProbe() {
  const value = useCustomerQuestionNotifications()
  return (
    <>
      <output data-testid="attention">{JSON.stringify([...value.attention.keys()].sort())}</output>
      <output data-testid="attention-counts">{JSON.stringify([...value.attention].sort())}</output>
    </>
  )
}

function renderCoordinator() {
  return render(
    <MemoryRouter>
      <CustomerQuestionNotifications>
        <AttentionProbe />
      </CustomerQuestionNotifications>
    </MemoryRouter>,
  )
}

function RoutedLocation() {
  const location = useLocation()
  return <output data-testid="location">{location.pathname + location.search}</output>
}

function ActivateOnAttention({
  payload,
}: {
  payload: { host_id: string; notification_id: string; task_key: string }
}) {
  const { attention } = useCustomerQuestionNotifications()
  const activated = useRef(false)
  useLayoutEffect(() => {
    if (activated.current || attention.size === 0) return
    activated.current = true
    model.activation?.(payload)
  }, [attention, payload])
  return null
}

function ActivatedTaskWorkspace() {
  const { hostId = "" } = useParams()
  const [searchParams] = useSearchParams()
  return <TasksWorkspace
    initialTaskKey={searchParams.get("task") ?? undefined}
    target={hostId === "local" ? null : { id: hostId, label: hostId, baseURL: `http://${hostId}.test`, token: "test-token" }}
  />
}

function taskDetail(key: string, description: string): TaskDetail {
  const task: Task = {
    key,
    category: "open",
    queue: "ASK",
    parent_key: "",
    position: 0,
    priority: "P1",
    title: "Answer the customer question",
    description,
    status: "open",
    author: "user:customer",
    customer: "user:customer",
    group: "",
    assignee: "agent:alice",
    manual_block_reason: "",
    blocked: false,
    revision: 1,
    created_at: "2026-08-18T12:00:00Z",
    updated_at: "2026-08-18T12:00:00Z",
    completed_at: "",
  }
  return { task, comments: [], waiting_for: [], relations: [] }
}

function renderActivationFlow() {
  return render(
    <MemoryRouter initialEntries={["/"]}>
      <CustomerQuestionNotifications>
        <AttentionProbe />
        <RoutedLocation />
        <Routes>
          <Route path="/" element={<p>Inbox</p>} />
          <Route path="/servers/:hostId/tasks" element={<ActivatedTaskWorkspace />} />
        </Routes>
      </CustomerQuestionNotifications>
    </MemoryRouter>,
  )
}

function renderImmediateActivationFlow(payload: {
  host_id: string
  notification_id: string
  task_key: string
}) {
  return render(
    <MemoryRouter initialEntries={["/"]}>
      <CustomerQuestionNotifications>
        <ActivateOnAttention payload={payload} />
        <AttentionProbe />
        <RoutedLocation />
        <Routes>
          <Route path="/" element={<p>Inbox</p>} />
          <Route path="/servers/:hostId/tasks" element={<ActivatedTaskWorkspace />} />
        </Routes>
      </CustomerQuestionNotifications>
    </MemoryRouter>,
  )
}

async function expectAttention(...keys: string[]) {
  await waitFor(() => {
    expect(screen.getByTestId("attention")).toHaveTextContent(JSON.stringify(keys.sort()))
  })
}

async function hint(hostId: string, sequence = 2) {
  await act(async () => {
    model.sockets.get(hostId)?.onHint({ sequence, kind: "task.question" })
  })
}

function deferred<T>() {
  let resolve!: (value: T) => void
  const promise = new Promise<T>((next) => { resolve = next })
  return { promise, resolve }
}

beforeEach(() => {
  model.daemons = []
  model.snapshots = new Map()
  model.sequences = new Map([["", 1]])
  model.failures = new Set()
  model.shown = []
  model.sockets = new Map()
  model.deferredInbox = null
  model.activation = undefined
  model.readAttempts = []
  model.rejectRead = false
  model.markReadPromise = null
  model.taskDetails = new Map()
})

describe("CustomerQuestionNotifications", () => {
  it("baselines unread questions into attention without native notifications", async () => {
    model.daemons = [{ id: "remote-1", label: "prod" }]
    model.snapshots.set("remote-1", [notification()])
    model.sequences.set("remote-1", 4)

    renderCoordinator()

    await expectAttention('["remote-1","alice"]')
    expect(model.shown).toEqual([])
  })

  it("counts distinct tasks with unread questions per agent", async () => {
    model.daemons = [{ id: "remote-1", label: "prod" }]
    model.snapshots.set("remote-1", [
      notification(),
      notification({ id: "notification-2" }),
      notification({ id: "notification-3", task_key: "ASK-2" }),
      notification({ id: "notification-4", task_key: "ASK-3", read_at: "2026-08-18T12:01:00Z" }),
    ])

    renderCoordinator()

    await waitFor(() => {
      expect(screen.getByTestId("attention-counts"))
        .toHaveTextContent(JSON.stringify([[JSON.stringify(["remote-1", "alice"]), 2]]))
    })
  })

  it("adds a new authoritative question once and sends its host-scoped native notification", async () => {
    model.daemons = [{ id: "remote-1", label: "prod" }]
    model.snapshots.set("remote-1", [notification()])
    renderCoordinator()
    await expectAttention('["remote-1","alice"]')

    model.snapshots.set("remote-1", [
      notification(),
      notification({ id: "notification-2", requesting_principal: "agent:bob", task_key: "ASK-2" }),
    ])
    await hint("remote-1")

    await expectAttention('["remote-1","alice"]', '["remote-1","bob"]')
    expect(model.shown).toEqual([{
      host_id: "remote-1",
      notification_id: "notification-2",
      task_key: "ASK-2",
      server_label: "prod",
      agent_name: "bob",
    }])
  })

  it("does not duplicate a notification across repeated hints or snapshots", async () => {
    model.snapshots.set("", [notification({ id: "baseline" })])
    renderCoordinator()
    await expectAttention('["","alice"]')
    model.snapshots.set("", [notification({ id: "baseline" }), notification({ id: "new" })])

    await hint("")
    await hint("")

    await waitFor(() => expect(model.shown).toHaveLength(1))
    expect(model.shown[0].notification_id).toBe("new")
  })

  it("does not replay an in-flight host refresh when only its label changes", async () => {
    model.daemons = [{ id: "remote-1", label: "prod" }]
    model.snapshots.set("remote-1", [notification()])
    const view = renderCoordinator()
    await expectAttention('["remote-1","alice"]')
    await waitFor(() => expect(model.sockets.has("")).toBe(true))

    const pending = deferred<{ notifications: TaskNotification[]; count: number }>()
    model.deferredInbox = pending.promise
    await hint("remote-1")
    model.daemons = [{ id: "remote-1", label: "Production" }]
    view.rerender(
      <MemoryRouter>
        <CustomerQuestionNotifications>
          <AttentionProbe />
        </CustomerQuestionNotifications>
      </MemoryRouter>,
    )

    await act(async () => {
      pending.resolve({ notifications: [notification()], count: 1 })
      await pending.promise
    })

    await expectAttention('["remote-1","alice"]')
    expect(model.shown).toEqual([])
  })

  it("ignores read, dismissed, non-question, and non-agent notifications", async () => {
    model.snapshots.set("", [
      notification({ id: "read", read_at: "2026-08-18T12:05:00Z" }),
      notification({ id: "dismissed", dismissed_at: "2026-08-18T12:05:00Z" }),
      notification({ id: "assigned", type: "task.assigned" }),
      notification({ id: "customer", requesting_principal: "user:customer" }),
    ])

    renderCoordinator()

    await expectAttention()
    expect(model.shown).toEqual([])
  })

  it("keeps a healthy host's attention when another host fetch fails", async () => {
    model.daemons = [{ id: "healthy", label: "prod" }, { id: "broken", label: "staging" }]
    model.snapshots.set("healthy", [notification({ requesting_principal: "agent:alice" })])
    model.failures.add("broken")

    renderCoordinator()

    await expectAttention('["healthy","alice"]')
  })

  it("removes attention and its socket child when a host disappears", async () => {
    model.daemons = [{ id: "remote-1", label: "prod" }]
    model.snapshots.set("remote-1", [notification()])
    const view = renderCoordinator()
    await expectAttention('["remote-1","alice"]')
    await waitFor(() => expect(model.sockets.has("remote-1")).toBe(true))

    model.daemons = []
    view.rerender(
      <MemoryRouter>
        <CustomerQuestionNotifications>
          <AttentionProbe />
        </CustomerQuestionNotifications>
      </MemoryRouter>,
    )

    await expectAttention()
    expect(model.sockets.has("remote-1")).toBe(false)
  })

  it("treats recovery after an initial failure as a new silent baseline", async () => {
    vi.useFakeTimers()
    model.daemons = [{ id: "remote-1", label: "prod" }]
    model.failures.add("remote-1")
    renderCoordinator()

    await act(async () => {
      await vi.advanceTimersByTimeAsync(0)
      model.failures.delete("remote-1")
      model.snapshots.set("remote-1", [notification()])
      await vi.advanceTimersByTimeAsync(3000)
    })

    expect(screen.getByTestId("attention")).toHaveTextContent('["[\\"remote-1\\",\\"alice\\"]"]')
    expect(model.shown).toEqual([])
    vi.useRealTimers()
  })

  it("rebaselines an established watcher after an outage without replaying native notifications", async () => {
    model.daemons = [{ id: "remote-1", label: "prod" }]
    model.snapshots.set("remote-1", [notification()])
    renderCoordinator()
    await expectAttention('["remote-1","alice"]')
    await waitFor(() => expect(model.sockets.has("remote-1")).toBe(true))

    vi.useFakeTimers()
    try {
      model.failures.add("remote-1")
      await hint("remote-1")
      await vi.waitFor(() => expect(model.sockets.has("remote-1")).toBe(false))
      model.snapshots.set("remote-1", [
        notification(),
        notification({ id: "notification-2", requesting_principal: "agent:bob", task_key: "ASK-2" }),
      ])
      model.failures.delete("remote-1")
      await act(async () => { await vi.advanceTimersByTimeAsync(3000) })

      expect(screen.getByTestId("attention")).toHaveTextContent('["[\\"remote-1\\",\\"alice\\"]","[\\"remote-1\\",\\"bob\\"]"]')
      expect(model.shown).toEqual([])
    } finally {
      vi.useRealTimers()
    }
  })

  it("rebaselines silently when a socket close discovers an HTTP outage", async () => {
    model.daemons = [{ id: "remote-1", label: "prod" }]
    model.snapshots.set("remote-1", [notification()])
    renderCoordinator()
    await expectAttention('["remote-1","alice"]')
    await waitFor(() => expect(model.sockets.has("remote-1")).toBe(true))

    vi.useFakeTimers()
    try {
      model.failures.add("remote-1")
      await act(async () => {
        model.sockets.get("remote-1")?.setStatus("closed")
        await Promise.resolve()
      })
      expect(model.sockets.has("remote-1")).toBe(false)

      model.snapshots.set("remote-1", [
        notification(),
        notification({ id: "notification-2", requesting_principal: "agent:bob", task_key: "ASK-2" }),
      ])
      model.failures.delete("remote-1")
      await act(async () => { await vi.advanceTimersByTimeAsync(3000) })

      expect(screen.getByTestId("attention")).toHaveTextContent('["[\\"remote-1\\",\\"alice\\"]","[\\"remote-1\\",\\"bob\\"]"]')
      expect(model.shown).toEqual([])
    } finally {
      vi.useRealTimers()
    }
  })

  it("resets a recovered host cursor before receiving later silent hints", async () => {
    model.daemons = [{ id: "remote-1", label: "prod" }]
    model.sequences.set("remote-1", 100)
    model.snapshots.set("remote-1", [notification()])
    renderCoordinator()
    await expectAttention('["remote-1","alice"]')
    await waitFor(() => expect(model.sockets.get("remote-1")?.after).toBe(100))

    vi.useFakeTimers()
    try {
      model.failures.add("remote-1")
      await hint("remote-1", 101)
      model.failures.delete("remote-1")
      model.sequences.set("remote-1", 2)
      await act(async () => { await vi.advanceTimersByTimeAsync(3000) })

      expect(model.sockets.get("remote-1")?.after).toBe(2)
      await hint("remote-1", 3)
      expect(screen.getByTestId("attention")).toHaveTextContent('["[\\"remote-1\\",\\"alice\\"]"]')
      expect(model.shown).toEqual([])
    } finally {
      vi.useRealTimers()
    }
  })

  it("reads an activated remote question, clears attention, and opens its encoded task route", async () => {
    const taskKey = "ASK-8/needs-answer"
    model.daemons = [{ id: "remote-1", label: "prod" }]
    model.snapshots.set("remote-1", [notification({ task_key: taskKey })])
    model.taskDetails.set(taskKey, taskDetail(taskKey, "The customer needs a decision"))

    renderActivationFlow()
    await expectAttention('["remote-1","alice"]')

    await act(async () => {
      model.activation?.({ host_id: "remote-1", notification_id: "notification-1", task_key: taskKey })
    })

    await waitFor(() => expect(screen.getByTestId("location")).toHaveTextContent("/servers/remote-1/tasks?task=ASK-8%2Fneeds-answer"))
    expect(await screen.findByRole("heading", { name: taskKey })).toBeInTheDocument()
    expect(screen.getByRole("textbox", { name: "Description" })).toHaveTextContent("The customer needs a decision")
    await expectAttention()
    expect(model.readAttempts).toEqual([{ notificationId: "notification-1", hostId: "remote-1" }])
  })

  it("clears attention when activation arrives as soon as baseline attention renders", async () => {
    const taskKey = "ASK-8"
    model.daemons = [{ id: "remote-1", label: "prod" }]
    model.snapshots.set("remote-1", [notification({ task_key: taskKey })])
    model.taskDetails.set(taskKey, taskDetail(taskKey, "The customer needs an immediate decision"))

    renderImmediateActivationFlow({
      host_id: "remote-1",
      notification_id: "notification-1",
      task_key: taskKey,
    })

    await waitFor(() => expect(screen.getByTestId("location")).toHaveTextContent("/servers/remote-1/tasks?task=ASK-8"))
    expect(await screen.findByRole("heading", { name: taskKey })).toBeInTheDocument()
    await expectAttention()
    expect(model.readAttempts).toEqual([{ notificationId: "notification-1", hostId: "remote-1" }])
  })

  it("opens the task route when marking the activation read fails", async () => {
    const taskKey = "ASK-9"
    model.daemons = [{ id: "remote-1", label: "prod" }]
    model.snapshots.set("remote-1", [notification({ task_key: taskKey })])
    model.taskDetails.set(taskKey, taskDetail(taskKey, "The customer still needs an answer"))
    model.rejectRead = true

    renderActivationFlow()
    await expectAttention('["remote-1","alice"]')

    await act(async () => {
      model.activation?.({ host_id: "remote-1", notification_id: "notification-1", task_key: taskKey })
    })

    await waitFor(() => expect(screen.getByTestId("location")).toHaveTextContent("/servers/remote-1/tasks?task=ASK-9"))
    expect(await screen.findByRole("heading", { name: taskKey })).toBeInTheDocument()
  })

  it("opens the task route when marking the activated notification never settles", async () => {
    const taskKey = "ASK-10"
    model.daemons = [{ id: "remote-1", label: "prod" }]
    model.snapshots.set("remote-1", [notification({ task_key: taskKey })])
    model.taskDetails.set(taskKey, taskDetail(taskKey, "The customer needs a bounded response"))
    model.markReadPromise = new Promise(() => {})

    renderActivationFlow()
    await expectAttention('["remote-1","alice"]')
    vi.useFakeTimers()
    try {
      await act(async () => {
        model.activation?.({ host_id: "remote-1", notification_id: "notification-1", task_key: taskKey })
        await vi.advanceTimersByTimeAsync(1000)
      })

      expect(screen.getByTestId("location")).toHaveTextContent("/servers/remote-1/tasks?task=ASK-10")
    } finally {
      vi.useRealTimers()
    }
  })

  it("opens the task route without waiting for a never-settling notification refresh", async () => {
    const taskKey = "ASK-11"
    model.daemons = [{ id: "remote-1", label: "prod" }]
    model.snapshots.set("remote-1", [notification({ task_key: taskKey })])
    model.taskDetails.set(taskKey, taskDetail(taskKey, "The customer needs navigation now"))

    renderActivationFlow()
    await expectAttention('["remote-1","alice"]')
    model.deferredInbox = new Promise(() => {})
    await act(async () => {
      model.activation?.({ host_id: "remote-1", notification_id: "notification-1", task_key: taskKey })
    })

    await waitFor(() => expect(screen.getByTestId("location")).toHaveTextContent("/servers/remote-1/tasks?task=ASK-11"))
  })
})
