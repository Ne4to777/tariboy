import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { beforeEach, expect, it, vi } from "vitest"
import type { Task, TaskDetail as Detail } from "@/lib/tasks"
import TaskDrawer from "./TaskDrawer"
import { customerView, workflowTask } from "./workflowFixtures"

const api = vi.hoisted(() => ({
  addTaskComment: vi.fn(),
  getTask: vi.fn(),
  getTaskWorkflow: vi.fn(),
  listTaskEvents: vi.fn(),
  listTaskPrincipals: vi.fn(),
  listTasks: vi.fn(),
  updateTask: vi.fn(),
}))
const toast = vi.hoisted(() => ({ error: vi.fn(), success: vi.fn() }))
const taskSocket = vi.hoisted(() => ({
  options: undefined as {
    after?: number
    enabled?: boolean
    onHint?: (hint: { sequence: number; task_key?: string }) => void
    onReset?: (sequence: number) => void
  } | undefined,
}))

vi.mock("@/lib/tasks", async (importOriginal) => ({
  ...await importOriginal<typeof import("@/lib/tasks")>(),
  ...api,
}))
vi.mock("@/hooks/useTasksSocket", () => ({
  useTasksSocket: vi.fn((options) => {
    taskSocket.options = options
    return "open"
  }),
}))
vi.mock("sonner", () => ({ toast }))

const task: Task = {
  key: "TEST-1",
  queue: "TEST",
  parent_key: "",
  position: 0,
  priority: "P1",
  title: "Ship the drawer",
  description: "Open a task from chat",
  status: "open",
  category: "open",
  pull_request: "",
  author: "user:owner",
  customer: "user:owner",
  group: "",
  assignee: "agent:worker",
  manual_block_reason: "",
  blocked: false,
  revision: 2,
  created_at: "2026-07-31T10:00:00Z",
  updated_at: "2026-07-31T10:00:00Z",
  completed_at: "",
}
const detail: Detail = { task, comments: [], waiting_for: [], relations: [] }

beforeEach(() => {
  vi.clearAllMocks()
  localStorage.clear()
  taskSocket.options = undefined
  api.getTask.mockResolvedValue(detail)
  api.listTaskEvents.mockResolvedValue({ events: [], count: 0 })
  api.listTasks.mockResolvedValue({ tasks: [], sequence: 412 })
  api.listTaskPrincipals.mockResolvedValue({ customer: "user:owner", agents: ["worker"], groups: [] })
  api.updateTask.mockResolvedValue({ ...task, status: "in_progress", category: "in_progress", revision: 3 })
  api.addTaskComment.mockResolvedValue({ comment: null, created_waits: [], resolved_waits: [] })
})

it("loads the task it was given and closes without touching the tasks list", async () => {
  const onClose = vi.fn()
  render(<TaskDrawer taskKey="TEST-1" onClose={onClose} />)
  expect(await screen.findByText("Ship the drawer")).toBeInTheDocument()
  expect(api.getTask).toHaveBeenCalledWith("TEST-1", undefined)
  await userEvent.click(screen.getByRole("button", { name: "Close task detail" }))
  expect(onClose).toHaveBeenCalled()
})

it("saves task edits from the drawer itself", async () => {
  render(<TaskDrawer taskKey="TEST-1" onClose={vi.fn()} />)
  await screen.findByText("Ship the drawer")
  await userEvent.selectOptions(screen.getByLabelText("Status"), "in_progress")
  await userEvent.click(screen.getByRole("button", { name: "Save task" }))
  await waitFor(() => expect(api.updateTask).toHaveBeenCalledWith(
    "TEST-1",
    expect.objectContaining({ revision: 2, status: "in_progress" }),
    undefined,
  ))
})

it("posts a comment and reloads the task", async () => {
  render(<TaskDrawer taskKey="TEST-1" onClose={vi.fn()} />)
  await screen.findByText("Ship the drawer")
  const comments = screen.getByText("Comments").closest("section")!
  await userEvent.click(within(comments).getByRole("button", { name: "Markdown" }))
  await userEvent.type(screen.getByLabelText("Comment"), "on it")
  await userEvent.click(screen.getByRole("button", { name: "Send comment" }))
  await waitFor(() => expect(api.addTaskComment).toHaveBeenCalledWith(
    "TEST-1", expect.stringContaining("on it"), undefined, expect.any(String),
  ))
  await waitFor(() => expect(api.getTask).toHaveBeenCalledTimes(2))
})

it("reloads the task when the tasks socket hints a change", async () => {
  render(<TaskDrawer taskKey="TEST-1" onClose={vi.fn()} />)
  await screen.findByText("Ship the drawer")
  expect(api.getTask).toHaveBeenCalledTimes(1)
  taskSocket.options?.onHint?.({ sequence: 7 })
  await waitFor(() => expect(api.getTask).toHaveBeenCalledTimes(2))
})

it("refetches a workflow task's panel on a hint for this task, and not for another", async () => {
  // The drawer's history is the oldest page and the revision stays the same:
  // only the hint can tell the panel something happened.
  api.getTask.mockResolvedValue({ ...detail, task: workflowTask })
  api.getTaskWorkflow.mockResolvedValue(customerView)
  render(<TaskDrawer taskKey="REL-1" onClose={vi.fn()} />)
  await screen.findByText("release@1.2.0")
  await waitFor(() => expect(taskSocket.options?.enabled).toBe(true))
  expect(api.getTaskWorkflow).toHaveBeenCalledTimes(1)
  await act(async () => { taskSocket.options?.onHint?.({ sequence: 500, task_key: "OTHER-2" }) })
  await waitFor(() => expect(taskSocket.options?.after).toBe(500))
  expect(api.getTaskWorkflow).toHaveBeenCalledTimes(1)
  await act(async () => { taskSocket.options?.onHint?.({ sequence: 501, task_key: "REL-1" }) })
  await waitFor(() => expect(api.getTaskWorkflow).toHaveBeenCalledTimes(2))
})

it("opens at the width the Tasks tab saved and resizes it with the same handle", async () => {
  localStorage.setItem("tasks:workspace:v1", JSON.stringify({ schemaVersion: 1, detailWidth: 520 }))
  render(<TaskDrawer taskKey="TEST-1" onClose={vi.fn()} />)
  await screen.findByText("Ship the drawer")
  expect(screen.getByRole("dialog").style.getPropertyValue("--tasks-detail-width")).toBe("520px")

  const handle = screen.getByRole("separator", { name: "Resize task details" })
  fireEvent.keyDown(handle, { key: "ArrowLeft" })
  expect(screen.getByRole("dialog").style.getPropertyValue("--tasks-detail-width")).toBe("528px")
  expect(JSON.parse(localStorage.getItem("tasks:workspace:v1") ?? "{}"))
    .toEqual({ schemaVersion: 1, detailWidth: 528 })
})

it("drags the drawer from the right edge of the window", async () => {
  vi.stubGlobal("innerWidth", 1200)
  render(<TaskDrawer taskKey="TEST-1" onClose={vi.fn()} />)
  await screen.findByText("Ship the drawer")
  const handle = screen.getByRole("separator", { name: "Resize task details" })
  expect(handle).toHaveAttribute("aria-valuemax", "1120")

  fireEvent.pointerDown(handle, { pointerId: 1, button: 0, isPrimary: true })
  fireEvent.pointerMove(window, { pointerId: 1, clientX: 500 })
  fireEvent.pointerUp(window, { pointerId: 1 })
  expect(JSON.parse(localStorage.getItem("tasks:workspace:v1") ?? "{}"))
    .toMatchObject({ detailWidth: 700 })
  vi.unstubAllGlobals()
})

it("starts the tasks socket at the current sequence instead of replaying the whole log", async () => {
  render(<TaskDrawer taskKey="TEST-1" onClose={vi.fn()} />)
  await screen.findByText("Ship the drawer")
  await waitFor(() => expect(taskSocket.options?.enabled).toBe(true))
  expect(taskSocket.options?.after).toBe(412)
})

it("keeps the socket closed until the current sequence is known", async () => {
  let release: (page: { tasks: never[]; sequence: number }) => void = () => {}
  api.listTasks.mockReturnValue(new Promise((resolve) => { release = resolve }))
  render(<TaskDrawer taskKey="TEST-1" onClose={vi.fn()} />)
  await screen.findByText("Ship the drawer")
  expect(taskSocket.options?.enabled).toBe(false)
  release({ tasks: [], sequence: 9 })
  await waitFor(() => expect(taskSocket.options?.enabled).toBe(true))
})

it("ignores a hint about another task", async () => {
  render(<TaskDrawer taskKey="TEST-1" onClose={vi.fn()} />)
  await screen.findByText("Ship the drawer")
  expect(api.getTask).toHaveBeenCalledTimes(1)
  taskSocket.options?.onHint?.({ sequence: 413, task_key: "OTHER-2" })
  await waitFor(() => expect(taskSocket.options?.after).toBe(413))
  expect(api.getTask).toHaveBeenCalledTimes(1)
})

it("reloads on a socket reset without rewinding the stream to the beginning", async () => {
  render(<TaskDrawer taskKey="TEST-1" onClose={vi.fn()} />)
  await screen.findByText("Ship the drawer")
  await waitFor(() => expect(taskSocket.options?.after).toBe(412))
  taskSocket.options?.onReset?.(412)
  await waitFor(() => expect(api.getTask).toHaveBeenCalledTimes(2))
  expect(taskSocket.options?.after).toBe(412)
})

it("opens at once in a loading state and fills in when the task arrives", async () => {
  let resolve!: (value: Detail) => void
  api.getTask.mockReturnValue(new Promise<Detail>((done) => { resolve = done }))
  const onClose = vi.fn()
  render(<TaskDrawer taskKey="TEST-1" onClose={onClose} />)
  const dialog = await screen.findByRole("dialog", { name: "TEST-1" })
  expect(within(dialog).getByText("Loading task…")).toBeInTheDocument()
  expect(screen.queryByText("Ship the drawer")).not.toBeInTheDocument()
  resolve(detail)
  expect(await screen.findByText("Ship the drawer")).toBeInTheDocument()
  expect(screen.queryByText("Loading task…")).not.toBeInTheDocument()
})

it("shows a workflow task by its status label and category tone, and closes it without a draft prompt", async () => {
  api.getTask.mockResolvedValue({
    ...detail,
    task: { ...task, status: "implement", category: "in_progress", workflow_name: "delivery" },
  })
  const onClose = vi.fn()
  render(<TaskDrawer taskKey="TEST-1" onClose={onClose} />)
  const pill = (await screen.findAllByText("Implement"))[0].closest("[data-slot='status-pill']")!
  expect(pill).toHaveAttribute("data-tone", "live")
  await userEvent.click(screen.getByRole("button", { name: "Close task detail" }))
  expect(onClose).toHaveBeenCalled()
})

it("closes from the loading state", async () => {
  api.getTask.mockReturnValue(new Promise<Detail>(() => {}))
  const onClose = vi.fn()
  render(<TaskDrawer taskKey="TEST-1" onClose={onClose} />)
  await screen.findByText("Loading task…")
  await userEvent.click(screen.getByRole("button", { name: "Close task detail" }))
  expect(onClose).toHaveBeenCalled()
})
