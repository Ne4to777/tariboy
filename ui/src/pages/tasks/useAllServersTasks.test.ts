import { act, renderHook, waitFor } from "@testing-library/react"
import { beforeEach, expect, it, vi } from "vitest"
import type { Task } from "@/lib/tasks"
import { useAllServersTasks } from "./useAllServersTasks"

const api = vi.hoisted(() => ({ listTasks: vi.fn() }))
vi.mock("@/lib/tasks", async (importOriginal) => ({
  ...await importOriginal<typeof import("@/lib/tasks")>(),
  ...api,
}))
vi.mock("@/lib/terminalsHost", () => ({
  targetFor: (id: string) => (id === "" ? null : { id, label: id, baseURL: `https://${id}`, token: "t" }),
}))

function task(key: string, extra: Partial<Task> = {}): Task {
  return {
    key, queue: key.split("-")[0], parent_key: "", position: 0, priority: "P2", title: key,
    description: "", status: "open", category: "open", author: "", customer: "", group: "", assignee: "",
    manual_block_reason: "", blocked: false, revision: 1, created_at: "", updated_at: "",
    completed_at: "", ...extra,
  }
}

const servers = [
  { id: "", label: "local" },
  { id: "h2", label: "hetzner-02" },
  { id: "down", label: "gone", error: "unreachable" },
]
const filters = { text: "fix", statusView: "active" as const }

beforeEach(() => {
  api.listTasks.mockReset()
})

it("merges every reachable server's pages and tags each task with its server", async () => {
  api.listTasks.mockImplementation(async (query: { after?: string }, target: { id: string } | null) => {
    if (target === null) {
      return query.after
        ? { tasks: [task("A-2")], sequence: 7 }
        : { tasks: [task("A-1")], next_cursor: "c1", sequence: 5 }
    }
    return { tasks: [task("A-1", { title: "same key elsewhere" })], sequence: 3 }
  })

  const { result } = renderHook(() => useAllServersTasks(servers, filters))
  await waitFor(() => expect(result.current.loading).toBe(false))
  await waitFor(() => expect(result.current.tasks).toHaveLength(3))

  expect(result.current.tasks.map((item) => `${item.serverId}:${item.key}`).sort())
    .toEqual([":A-1", ":A-2", "h2:A-1"])
  expect(result.current.tasks.find((item) => item.serverId === "h2")?.serverName).toBe("hetzner-02")
  expect(api.listTasks).toHaveBeenCalledWith(
    expect.objectContaining({ text: "fix", status_view: "active", limit: 500 }), null,
  )
  // An unreachable server is not asked; it is reported.
  expect(api.listTasks).not.toHaveBeenCalledWith(expect.anything(), expect.objectContaining({ id: "down" }))
  expect(result.current.errors).toEqual({ down: "unreachable" })
  expect(result.current.sequences).toEqual({ "": 7, h2: 3 })
})

it("keeps the other servers when one fails and retries only that one", async () => {
  let fail = true
  api.listTasks.mockImplementation(async (_query: unknown, target: { id: string } | null) => {
    if (target?.id === "h2" && fail) throw new Error("connection refused")
    return { tasks: [task(target ? "B-1" : "A-1")], sequence: 1 }
  })

  const { result } = renderHook(() => useAllServersTasks(servers.slice(0, 2), filters))
  await waitFor(() => expect(result.current.errors).toEqual({ h2: "connection refused" }))
  expect(result.current.tasks.map((item) => item.key)).toEqual(["A-1"])

  fail = false
  api.listTasks.mockClear()
  act(() => result.current.reload("h2"))
  await waitFor(() => expect(result.current.errors).toEqual({}))
  expect(result.current.tasks.map((item) => item.key).sort()).toEqual(["A-1", "B-1"])
  expect(api.listTasks).toHaveBeenCalledTimes(1)
})

it("stays loading until the first server answers", async () => {
  let release!: (value: unknown) => void
  api.listTasks.mockReturnValue(new Promise((resolve) => { release = resolve }))
  const { result } = renderHook(() => useAllServersTasks(servers.slice(0, 1), filters))
  expect(result.current.loading).toBe(true)
  await act(async () => release({ tasks: [], sequence: 0 }))
  expect(result.current.loading).toBe(false)
})

it("coalesces live hints into one follow-up read per server", async () => {
  const pending: Array<(value: unknown) => void> = []
  api.listTasks.mockImplementation(() => new Promise((resolve) => pending.push(resolve)))
  const { result } = renderHook(() => useAllServersTasks(servers.slice(0, 1), filters))
  await waitFor(() => expect(pending).toHaveLength(1))

  act(() => {
    result.current.refresh("")
    result.current.refresh("")
    result.current.refresh("")
  })
  expect(api.listTasks).toHaveBeenCalledTimes(1)
  await act(async () => pending[0]({ tasks: [task("A-1")], sequence: 1 }))
  await waitFor(() => expect(api.listTasks).toHaveBeenCalledTimes(2))
  await act(async () => pending[1]({ tasks: [task("A-1"), task("A-2")], sequence: 2 }))
  expect(result.current.tasks).toHaveLength(2)
  expect(api.listTasks).toHaveBeenCalledTimes(2)
})

it("drops a server's rows when its read fails, so an old filter's rows never linger", async () => {
  let fail = false
  api.listTasks.mockImplementation(async () => {
    if (fail) throw new Error("gone")
    return { tasks: [task("A-1")], sequence: 1 }
  })
  const { result } = renderHook(() => useAllServersTasks(servers.slice(0, 1), filters))
  await waitFor(() => expect(result.current.tasks).toHaveLength(1))
  fail = true
  act(() => result.current.reload(""))
  await waitFor(() => expect(result.current.errors).toEqual({ "": "gone" }))
  expect(result.current.tasks).toEqual([])
})

it("is loading while no server is known yet", () => {
  const { result } = renderHook(() => useAllServersTasks([], filters))
  expect(result.current.loading).toBe(true)
})
