import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { beforeEach, describe, expect, it, vi } from "vitest"
import { ApiError } from "@/lib/api"
import type { Task, TaskDetail, TaskNotification, TaskQueue } from "@/lib/tasks"
import TasksWorkspace from "./TasksWorkspace"

const api = vi.hoisted(() => ({
  addTaskComment: vi.fn(),
  addTaskRelation: vi.fn(),
  createTask: vi.fn(),
  createTaskQueue: vi.fn(),
  deleteTaskRelation: vi.fn(),
  getTask: vi.fn(),
  getTaskWorkflow: vi.fn(),
  getQueueWorkflow: vi.fn(),
  listAgentPools: vi.fn(),
  listWorkflowArtifacts: vi.fn(),
  listWorkflowQuestions: vi.fn(),
  listWorkflowVersions: vi.fn(),
  activateQueueWorkflow: vi.fn(),
  rebindAgentPool: vi.fn(),
  listTaskNotifications: vi.fn(),
  listTaskEvents: vi.fn(),
  listTaskPrincipals: vi.fn(),
  listTaskQueues: vi.fn(),
  listTasks: vi.fn(),
  markTaskNotificationRead: vi.fn(),
  moveTask: vi.fn(),
  updateTask: vi.fn(),
  updateTaskQueue: vi.fn(),
}))
const toast = vi.hoisted(() => ({ error: vi.fn(), success: vi.fn() }))
const daemonContext = vi.hoisted(() => ({ activeId: "" }))
const taskSocket = vi.hoisted(() => ({ options: undefined as { onHint?: (event: { sequence: number }) => void } | undefined }))

function deferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (reason?: unknown) => void
  const promise = new Promise<T>((resolvePromise, rejectPromise) => {
    resolve = resolvePromise
    reject = rejectPromise
  })
  return { promise, resolve, reject }
}

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
vi.mock("@/components/DaemonProvider", async (importOriginal) => ({
  ...await importOriginal<typeof import("@/components/DaemonProvider")>(),
  useOptionalDaemons: () => ({ activeId: daemonContext.activeId }),
}))

const queue: TaskQueue = {
  prefix: "TEST",
  name: "Test work",
  description: "",
  owners: ["user:owner"],
  responsible_agent: "agent:triager",
  revision: 1,
  created_at: "2026-07-31T10:00:00Z",
  updated_at: "2026-07-31T10:00:00Z",
}
const root: Task = {
  key: "TEST-1",
  queue: "TEST",
  parent_key: "",
  position: 0,
  priority: "P0",
  title: "Ship native tasks",
  description: "Central work system",
  status: "in_progress",
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
const child: Task = {
  ...root,
  key: "TEST-2",
  parent_key: "TEST-1",
  position: 0,
  title: "Desktop tree",
  status: "open",
  assignee: "",
  revision: 1,
}
const detail: TaskDetail = {
  task: root,
  comments: [{
    id: 1,
    task_key: root.key,
    author: "agent:worker",
    body: "Starting now",
    revision: 1,
    created_at: root.created_at,
    updated_at: root.updated_at,
  }],
  waiting_for: [],
  relations: [{
    id: 9,
    source_key: "TEST-1",
    source_title: root.title,
    source_status: "open",
    target_key: "TEST-2",
    target_title: "Child task",
    target_status: "in_progress",
    type: "blocks",
    created_by: "user:owner",
    created_at: root.created_at,
  }],
}
const notification: TaskNotification = {
  id: "notification-1",
  channel: "user:owner",
  type: "task.question",
  text: "Need a product decision",
  requesting_principal: "agent:worker",
  task_key: root.key,
  event_sequence: 4,
  created_at: root.created_at,
  published_at: root.created_at,
  read_at: "",
  dismissed_at: "",
}

beforeEach(() => {
  vi.clearAllMocks()
  localStorage.clear()
  sessionStorage.clear()
  daemonContext.activeId = ""
  taskSocket.options = undefined
  api.listTaskQueues.mockResolvedValue({ queues: [queue], count: 1 })
  api.listTaskPrincipals.mockResolvedValue({
    customer: "user:owner",
    agents: ["worker", "triager"],
    groups: ["platform"],
  })
  api.listTasks.mockResolvedValue({ tasks: [root, child], sequence: 10 })
  api.getTask.mockResolvedValue(detail)
  api.getTaskWorkflow.mockRejectedValue(new ApiError(404, "workflow_not_found", "workflow not found"))
  api.getQueueWorkflow.mockRejectedValue(new ApiError(404, "workflow_not_found", "workflow not found"))
  api.listAgentPools.mockResolvedValue({ items: [], count: 0 })
  api.listWorkflowArtifacts.mockResolvedValue({ items: [], count: 0 })
  api.listWorkflowQuestions.mockResolvedValue({ items: [], count: 0 })
  api.listWorkflowVersions.mockResolvedValue({ items: [], count: 0 })
  api.listTaskNotifications.mockResolvedValue({ notifications: [notification], count: 1 })
  api.listTaskEvents.mockResolvedValue({
    events: [{
      sequence: 3,
      event_id: "event-3",
      task_key: root.key,
      queue: "TEST",
      kind: "task.updated",
      actor: "user:owner",
      task_revision: 2,
      payload: { status: "in_progress" },
      created_at: root.updated_at,
    }],
    count: 1,
  })
  api.createTask.mockResolvedValue(child)
  api.updateTask.mockResolvedValue({ ...root, assignee: "freelance-reviewer", revision: 3 })
  api.updateTaskQueue.mockResolvedValue({ ...queue, name: "Tests updated", revision: 2 })
  api.addTaskComment.mockResolvedValue({ comment: detail.comments[0], created_waits: [], resolved_waits: [] })
  api.addTaskRelation.mockResolvedValue(detail.relations[0])
  api.deleteTaskRelation.mockResolvedValue({ deleted: true, relation_id: 9 })
  api.moveTask.mockResolvedValue({ ...child, parent_key: "", revision: 2 })
  api.markTaskNotificationRead.mockResolvedValue({ ...notification, read_at: root.updated_at })
  api.createTaskQueue.mockResolvedValue({ ...queue, prefix: "OPS", name: "Operations" })
})

describe("TasksWorkspace", () => {
  it("uploads files without an agent to the task host and appends only to the selected draft", async () => {
    const target = { id: "remote", label: "Remote", baseURL: "https://remote.example", token: "test" }
    api.getTask.mockResolvedValue({ ...detail, task: { ...root, assignee: "" } })
    const upload = deferred<Response>()
    const fetchMock = vi.spyOn(globalThis, "fetch").mockReturnValue(upload.promise)
    render(<TasksWorkspace target={target} />)
    await userEvent.click(await screen.findByRole("button", { name: /Ship native tasks/ }))
    const description = screen.getByLabelText("Description").closest(".task-description")!
    const comments = screen.getByText("Comments").closest("section")!
    expect(within(description as HTMLElement).getByRole("button", { name: "Send files" })).toBeEnabled()
    expect(within(comments).getByRole("button", { name: "Send files" })).toBeEnabled()
    await userEvent.click(within(description as HTMLElement).getByRole("button", { name: "Markdown" }))
    await userEvent.click(within(comments).getByRole("button", { name: "Markdown" }))
    fireEvent.change(screen.getByLabelText("Comment"), { target: { value: "Comment draft" } })
    const file = new File(["hello"], "notes.txt")
    await userEvent.upload(description.querySelector('input[type="file"]') as HTMLInputElement, file)
    fireEvent.change(screen.getByLabelText("Description"), { target: { value: "Still typing" } })
    expect(screen.getByRole("button", { name: "Save task" })).toBeDisabled()
    const envelope = { ok: true, result: { path: "files/one/notes.txt", abs: "/server/files/one/notes.txt", bytes: 5 } }
    await act(async () => upload.resolve({ ok: true, status: 200, text: async () => JSON.stringify(envelope), json: async () => envelope } as Response))
    await waitFor(() => expect(screen.getByLabelText("Description")).toHaveValue("Still typing\n/server/files/one/notes.txt"))
    expect(screen.getByLabelText("Comment")).toHaveValue("Comment draft")
    expect(fetchMock).toHaveBeenCalledWith("https://remote.example/api/files/raw?name=notes.txt", expect.objectContaining({ method: "PUT", body: file }))
    await userEvent.upload(comments.querySelector('input[type="file"]') as HTMLInputElement, new File(["hello"], "notes.txt"))
    await waitFor(() => expect(screen.getByLabelText("Comment")).toHaveValue("Comment draft\n/server/files/one/notes.txt"))
    expect(screen.getByLabelText("Description")).toHaveValue("Still typing\n/server/files/one/notes.txt")
    expect(api.updateTask).not.toHaveBeenCalled()
    expect(api.addTaskComment).not.toHaveBeenCalled()
    fetchMock.mockRestore()
  })

  it("opens details only on selection and protects an unsaved title on outside close", async () => {
    render(<TasksWorkspace />)
    const row = await screen.findByRole("button", { name: /Ship native tasks/ })
    expect(screen.queryByText("Select a task")).not.toBeInTheDocument()
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument()
    await userEvent.click(row)
    expect(await screen.findByRole("dialog")).toBeInTheDocument()
    expect(screen.getByRole("button", { name: "Close task detail" })).toHaveFocus()
    fireEvent.change(screen.getByLabelText("Title"), { target: { value: "Unsaved title" } })
    await userEvent.click(document.querySelector('[data-slot="dialog-overlay"]') as HTMLElement)
    expect(screen.getByDisplayValue("Unsaved title")).toBeInTheDocument()
    expect(screen.getByText("Are you sure you want to close this task? Unsaved changes will be discarded.")).toBeInTheDocument()
    await userEvent.click(screen.getByRole("button", { name: "Keep editing" }))
    expect(screen.getByDisplayValue("Unsaved title")).toBeInTheDocument()
    await userEvent.click(screen.getByRole("button", { name: "Close task detail" }))
    await userEvent.click(screen.getByRole("button", { name: "Discard changes" }))
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument()
    expect(row).toHaveFocus()
  })

  it("opens the drawer at once in a loading state and fills it when the task arrives", async () => {
    const pending = deferred<TaskDetail>()
    api.getTask.mockReturnValue(pending.promise)
    render(<TasksWorkspace />)
    const row = await screen.findByRole("button", { name: /Ship native tasks/ })
    await userEvent.click(row)
    const dialog = await screen.findByRole("dialog", { name: "TEST-1" })
    expect(within(dialog).getByText("Loading task…")).toBeInTheDocument()
    pending.resolve(detail)
    expect(await screen.findByLabelText("Title")).toHaveValue("Ship native tasks")
    expect(screen.queryByText("Loading task…")).not.toBeInTheDocument()
    expect(screen.getByRole("button", { name: "Close task detail" })).toHaveFocus()
    await userEvent.click(screen.getByRole("button", { name: "Close task detail" }))
    expect(row).toHaveFocus()
  })

  it("closes a loading drawer and keeps it closed when the task arrives", async () => {
    const pending = deferred<TaskDetail>()
    api.getTask.mockReturnValue(pending.promise)
    render(<TasksWorkspace />)
    await userEvent.click(await screen.findByRole("button", { name: /Ship native tasks/ }))
    await screen.findByText("Loading task…")
    await userEvent.click(screen.getByRole("button", { name: "Close task detail" }))
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument()
    await act(async () => pending.resolve(detail))
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument()
  })

  it("shows the next task loading at once and refreshes the open one without a skeleton", async () => {
    render(<TasksWorkspace />)
    await userEvent.click(await screen.findByRole("button", { name: /Ship native tasks/ }))
    await screen.findByRole("heading", { name: "TEST-1" })
    const refresh = deferred<TaskDetail>()
    api.getTask.mockReturnValue(refresh.promise)
    await act(async () => taskSocket.options?.onHint?.({ sequence: 11 }))
    expect(screen.queryByText("Loading task…")).not.toBeInTheDocument()
    expect(screen.getByLabelText("Title")).toBeInTheDocument()
    await act(async () => refresh.resolve(detail))
    const next = deferred<TaskDetail>()
    api.getTask.mockReturnValue(next.promise)
    await userEvent.click(screen.getByRole("button", { name: "Close task detail" }))
    await userEvent.click(screen.getByRole("button", { name: "Expand TEST-1" }))
    await userEvent.click(screen.getByRole("button", { name: /Desktop tree/ }))
    expect(await screen.findByRole("dialog", { name: "TEST-2" })).toHaveTextContent("Loading task…")
    next.resolve({ ...detail, task: child })
    expect(await screen.findByLabelText("Title")).toHaveValue(child.title)
  })

  it("closes a clean task when the backdrop is clicked", async () => {
    render(<TasksWorkspace />)
    const row = await screen.findByRole("button", { name: /Ship native tasks/ })
    await userEvent.click(row)

    await userEvent.click(document.querySelector('[data-slot="dialog-overlay"]') as HTMLElement)

    expect(screen.queryByRole("dialog")).not.toBeInTheDocument()
    expect(row).toHaveFocus()
  })

  it("keeps unsaved fields through a remote revision and failed save", async () => {
    render(<TasksWorkspace />)
    await userEvent.click(await screen.findByRole("button", { name: /Ship native tasks/ }))
    fireEvent.change(screen.getByLabelText("Title"), { target: { value: "My draft" } })
    api.getTask.mockResolvedValue({ ...detail, task: { ...root, revision: 3, title: "Remote title" } })
    await act(async () => taskSocket.options?.onHint?.({ sequence: 11 }))
    await waitFor(() => expect(api.getTask).toHaveBeenCalledTimes(2))
    expect(screen.getByDisplayValue("My draft")).toBeInTheDocument()
    api.updateTask.mockRejectedValue(new Error("Save failed"))
    await userEvent.click(screen.getByRole("button", { name: "Save task" }))
    await waitFor(() => expect(toast.error).toHaveBeenCalledWith("Save failed"))
    expect(screen.getByDisplayValue("My draft")).toBeInTheDocument()
  })

  it("retains a draft when background detail refresh fails", async () => {
    render(<TasksWorkspace />)
    await userEvent.click(await screen.findByRole("button", { name: /Ship native tasks/ }))
    fireEvent.change(screen.getByLabelText("Title"), { target: { value: "Keep this draft" } })
    api.getTask.mockRejectedValueOnce(new Error("Refresh failed"))
    await act(async () => taskSocket.options?.onHint?.({ sequence: 12 }))
    await waitFor(() => expect(toast.error).toHaveBeenCalledWith("Refresh failed"))
    expect(screen.getByDisplayValue("Keep this draft")).toBeInTheDocument()
  })

  it("recovers a conflict explicitly while preserving draft and unrelated remote fields", async () => {
    render(<TasksWorkspace />)
    await userEvent.click(await screen.findByRole("button", { name: /Ship native tasks/ }))
    fireEvent.change(screen.getByLabelText("Title"), { target: { value: "My title" } })
    api.getTask.mockResolvedValue({ ...detail, task: { ...root, title: "Remote title", priority: "P1", revision: 3 } })
    api.updateTask.mockRejectedValueOnce(new ApiError(409, "conflict", "revision conflict"))
    await userEvent.click(screen.getByRole("button", { name: "Save task" }))
    await userEvent.click(await screen.findByRole("button", { name: "Keep my edits" }))
    await userEvent.click(screen.getByRole("button", { name: "Save task" }))
    expect(api.updateTask).toHaveBeenLastCalledWith(root.key, expect.objectContaining({
      title: "My title", priority: "P1", revision: 3,
    }), undefined)
  })

  it("opens the detail sheet at half width and restores its persisted resize", async () => {
    localStorage.setItem("tasks:workspace:v1", JSON.stringify({
      schemaVersion: 1,
      navigationWidth: 280,
      detailWidth: 520,
    }))

    render(<TasksWorkspace />)

    const workspace = await screen.findByTestId("tasks-workspace")
    expect(workspace.style.getPropertyValue("--tasks-detail-width")).toBe("")
    expect(screen.queryByRole("separator", { name: "Resize task details" })).not.toBeInTheDocument()

    await userEvent.click(screen.getByRole("button", { name: /Ship native tasks/ }))

    expect(screen.getByRole("separator", { name: "Resize task details" })).toHaveAttribute("aria-valuenow", "520")
    expect(screen.getByRole("dialog").style.getPropertyValue("--tasks-detail-width")).toBe("520px")
  })

  it("uses half the viewport for the first detail sheet", async () => {
    vi.stubGlobal("innerWidth", 1200)
    render(<TasksWorkspace />)
    await userEvent.click(await screen.findByRole("button", { name: /Ship native tasks/ }))

    expect(screen.getByRole("separator", { name: "Resize task details" })).toHaveAttribute("aria-valuenow", "600")
    expect(screen.getByRole("dialog").style.getPropertyValue("--tasks-detail-width")).toBe("600px")
    vi.unstubAllGlobals()
  })

  it("resizes the detail sheet with the keyboard and keeps the legacy storage record readable", async () => {
    localStorage.setItem("tasks:workspace:v1", JSON.stringify({
      schemaVersion: 1,
      navigationWidth: 280,
      detailWidth: 512,
    }))
    render(<TasksWorkspace />)
    await userEvent.click(await screen.findByRole("button", { name: /Ship native tasks/ }))

    fireEvent.keyDown(screen.getByRole("separator", { name: "Resize task details" }), { key: "ArrowRight" })
    expect(JSON.parse(localStorage.getItem("tasks:workspace:v1") ?? "{}"))
      .toEqual({ schemaVersion: 1, detailWidth: 504 })

    fireEvent.keyDown(screen.getByRole("separator", { name: "Resize task details" }), { key: "Home" })
    expect(JSON.parse(localStorage.getItem("tasks:workspace:v1") ?? "{}"))
      .toEqual({ schemaVersion: 1, detailWidth: 512 })
  })

  it("drags the detail handle, stops at pointer-up, and resets on double-click", async () => {
    render(<TasksWorkspace />)
    const workspace = await screen.findByTestId("tasks-workspace")
    vi.spyOn(workspace, "getBoundingClientRect").mockReturnValue({
      x: 100,
      y: 0,
      left: 100,
      right: 1200,
      top: 0,
      bottom: 800,
      width: 1100,
      height: 800,
      toJSON: () => ({}),
    })
    fireEvent(window, new Event("resize"))
    await userEvent.click(screen.getByRole("button", { name: /Ship native tasks/ }))
    const handle = screen.getByRole("separator", { name: "Resize task details" })

    fireEvent.pointerDown(handle, { pointerId: 1, button: 0, isPrimary: true })
    fireEvent.pointerMove(window, { pointerId: 1, clientX: 420 })
    fireEvent.pointerUp(window, { pointerId: 1 })
    fireEvent.pointerMove(window, { pointerId: 1, clientX: 300 })
    expect(JSON.parse(localStorage.getItem("tasks:workspace:v1") ?? "{}"))
      .toMatchObject({ detailWidth: 780 })

    fireEvent.doubleClick(handle)
    expect(JSON.parse(localStorage.getItem("tasks:workspace:v1") ?? "{}"))
      .toEqual({ schemaVersion: 1, detailWidth: 512 })
  })

  it("cleans up an active resize on pointer cancellation and unmount", async () => {
    const view = render(<TasksWorkspace />)
    const workspace = await screen.findByTestId("tasks-workspace")
    vi.spyOn(workspace, "getBoundingClientRect").mockReturnValue({
      x: 0, y: 0, left: 0, right: 1400, top: 0, bottom: 800, width: 1400, height: 800,
      toJSON: () => ({}),
    })
    fireEvent(window, new Event("resize"))
    await userEvent.click(screen.getByRole("button", { name: /Ship native tasks/ }))
    const handle = screen.getByRole("separator", { name: "Resize task details" })

    fireEvent.pointerDown(handle, { pointerId: 7, button: 0, isPrimary: true })
    fireEvent.pointerMove(window, { pointerId: 7, clientX: 1000 })
    fireEvent.pointerCancel(window, { pointerId: 7 })
    fireEvent.pointerMove(window, { pointerId: 7, clientX: 900 })
    expect(JSON.parse(localStorage.getItem("tasks:workspace:v1") ?? "{}"))
      .toMatchObject({ detailWidth: 400 })
    expect(document.body.style.cursor).toBe("")
    expect(document.body.style.userSelect).toBe("")

    fireEvent.pointerDown(handle, { pointerId: 8, button: 0, isPrimary: true })
    view.unmount()
    fireEvent.pointerMove(window, { pointerId: 8, clientX: 800 })
    expect(JSON.parse(localStorage.getItem("tasks:workspace:v1") ?? "{}"))
      .toMatchObject({ detailWidth: 400 })
    expect(document.body.style.cursor).toBe("")
    expect(document.body.style.userSelect).toBe("")
  })

  it("clamps the detail resize to the workspace width", async () => {
    render(<TasksWorkspace />)
    const workspace = await screen.findByTestId("tasks-workspace")
    vi.spyOn(workspace, "getBoundingClientRect").mockReturnValue({
      x: 0, y: 0, left: 0, right: 1100, top: 0, bottom: 800, width: 1100, height: 800,
      toJSON: () => ({}),
    })
    fireEvent(window, new Event("resize"))
    await userEvent.click(screen.getByRole("button", { name: /Ship native tasks/ }))
    const handle = screen.getByRole("separator", { name: "Resize task details" })
    expect(handle).toHaveAttribute("aria-valuemax", "1020")

    fireEvent.pointerDown(handle, { pointerId: 9, button: 0, isPrimary: true })
    fireEvent.pointerMove(window, { pointerId: 9, clientX: -5000 })
    fireEvent.pointerUp(window, { pointerId: 9 })

    // The handle stops at the workspace, the stored width at the viewport.
    expect(JSON.parse(localStorage.getItem("tasks:workspace:v1") ?? "{}"))
      .toMatchObject({ detailWidth: 944 })
  })

  it("opens the task named by an initial deep-link key after loading the workspace", async () => {
    const asked = { ...root, key: "ASK-7", title: "Choose the release date", description: "Customer needs a date" }
    const askedDetail = { ...detail, task: asked }
    api.listTasks.mockResolvedValue({ tasks: [asked], sequence: 10 })
    api.getTask.mockResolvedValue(askedDetail)

    render(<TasksWorkspace initialTaskKey="ASK-7" />)

    expect(await screen.findByRole("heading", { name: "ASK-7" })).toBeInTheDocument()
    expect(screen.getByRole("textbox", { name: "Description" })).toHaveTextContent("Customer needs a date")
  })

  it("keeps a closed deep-linked task closed when the host target is re-created", async () => {
    const asked = { ...root, key: "ASK-7", title: "Choose the release date" }
    api.listTasks.mockResolvedValue({ tasks: [asked], sequence: 10 })
    api.getTask.mockResolvedValue({ ...detail, task: asked })
    const target = () => ({ id: "remote", label: "Remote", baseURL: "https://remote.example", token: "test" })

    const view = render(<TasksWorkspace target={target()} initialTaskKey="ASK-7" />)
    expect(await screen.findByRole("heading", { name: "ASK-7" })).toBeInTheDocument()
    await userEvent.click(screen.getByRole("button", { name: "Close task detail" }))
    const loads = api.listTaskEvents.mock.calls.length

    view.rerender(<TasksWorkspace target={target()} initialTaskKey="ASK-7" />)
    await act(async () => {})

    expect(api.listTaskEvents).toHaveBeenCalledTimes(loads)
    expect(screen.queryByRole("heading", { name: "ASK-7" })).not.toBeInTheDocument()
  })

  it("updates the selected task when the deep-link key changes", async () => {
    const first = { ...root, key: "ASK-7", title: "First question", description: "First answer needed" }
    const second = { ...root, key: "ASK-8", title: "Second question", description: "Second answer needed" }
    api.listTasks.mockResolvedValue({ tasks: [first, second], sequence: 10 })
    api.getTask.mockImplementation(async (key: string) => ({
      ...detail,
      task: key === "ASK-8" ? second : first,
    }))

    const view = render(<TasksWorkspace initialTaskKey="ASK-7" />)
    expect(await screen.findByRole("heading", { name: "ASK-7" })).toBeInTheDocument()

    view.rerender(<TasksWorkspace initialTaskKey="ASK-8" />)

    expect(await screen.findByRole("heading", { name: "ASK-8" })).toBeInTheDocument()
    expect(screen.getByRole("textbox", { name: "Description" })).toHaveTextContent("Second answer needed")
  })

  it("shows newest comments first and can switch to oldest first", async () => {
    const oldComment = { ...detail.comments[0], id: 1, body: "Oldest" }
    const newComment = { ...detail.comments[0], id: 2, body: "Newest", created_at: "2026-08-01T10:00:00Z" }
    api.getTask.mockResolvedValue({ ...detail, comments: [oldComment, newComment] })

    render(<TasksWorkspace />)
    await userEvent.click(await screen.findByRole("button", { name: /Ship native tasks/ }))

    const comments = screen.getByText("Comments").closest("section")!
    const commentList = comments.querySelector(".task-comment-list")!
    const commentForm = comments.querySelector("form")!
    expect(within(comments).getAllByRole("article")[0]).toHaveTextContent("Newest")
    expect(commentForm.compareDocumentPosition(commentList)).toBe(Node.DOCUMENT_POSITION_FOLLOWING)
    await userEvent.selectOptions(screen.getByLabelText("Comment order"), "oldest")
    expect(within(comments).getAllByRole("article")[0]).toHaveTextContent("Oldest")
    expect(commentList.compareDocumentPosition(commentForm)).toBe(Node.DOCUMENT_POSITION_FOLLOWING)
  })

  it("copies the original comment Markdown", async () => {
    const markdown = "# Status\n\n- **ready**\n- `tested`"
    const writeText = vi.fn().mockResolvedValue(undefined)
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText } })
    api.getTask.mockResolvedValue({
      ...detail,
      comments: [{ ...detail.comments[0], body: markdown }],
    })

    render(<TasksWorkspace />)
    await userEvent.click(await screen.findByRole("button", { name: /Ship native tasks/ }))
    await userEvent.click(screen.getByRole("button", { name: "Copy comment Markdown" }))

    expect(writeText).toHaveBeenCalledWith(markdown)
    expect(toast.success).toHaveBeenCalledWith("Comment Markdown copied")
  })

  it("keeps the latest task selected during a real-time refresh", async () => {
    const first = deferred<TaskDetail>()
    const second = deferred<TaskDetail>()
    const secondDetail = { ...detail, task: child }
    api.getTask.mockImplementation((key: string) => key === "TEST-1" ? first.promise : Promise.resolve(secondDetail))

    render(<TasksWorkspace />)
    await userEvent.click(await screen.findByRole("button", { name: /Ship native tasks/ }))
    first.resolve(detail)
    await screen.findByRole("heading", { name: "TEST-1" })
    api.getTask.mockImplementation((key: string) => key === "TEST-2" ? second.promise : Promise.resolve(detail))
    await userEvent.click(screen.getByRole("button", { name: "Close task detail" }))
    await userEvent.click(screen.getByRole("button", { name: "Expand TEST-1" }))
    await userEvent.click(screen.getByRole("button", { name: /Desktop tree/ }))
    await act(async () => taskSocket.options?.onHint?.({ sequence: 11 }))
    second.resolve(secondDetail)

    expect(await screen.findByRole("heading", { name: "TEST-2" })).toBeInTheDocument()
  })

  it("keeps the frozen banner and hides lifecycle controls on a managed task", async () => {
    const managed = {
      ...root,
      workflow_version_id: 7,
      workflow_version: "development@2",
      workflow_status: "review",
      workflow_revision: 5,
    }
    api.listTasks.mockResolvedValue({ tasks: [managed], sequence: 10 })
    api.getTask.mockResolvedValue({ ...detail, task: managed })
    api.getTaskWorkflow.mockResolvedValue({
      task: managed,
      workflow: { id: 7, name: "development", version: 2, state: "published", definition: { name: "development", version: 2, initial_status: "implement", statuses: [] }, created_at: root.created_at, updated_at: root.updated_at, published_at: root.updated_at },
      status_executions: [{ id: 1, task_key: root.key, workflow_version_id: 7, status: "review", sequence: 2, state: "frozen", task_revision: 2, created_at: root.created_at }],
      requirement_executions: [],
      assignments: [
        { id: 11, requirement_execution_id: 1, agent: "review-a", attempt: 1, state: "leased", lease_owner: "agent:review-a", revision: 1, created_at: root.created_at, updated_at: root.updated_at },
        { id: 12, requirement_execution_id: 2, agent: "qa-a", attempt: 1, state: "claimable", revision: 1, created_at: root.created_at, updated_at: root.updated_at },
      ],
      holds: [{ id: 2, task_key: root.key, assignment_id: 11, scope: "assignment", reason: "Need decision", created_at: root.created_at }],
      observations: [{ id: 3, task_key: root.key, assignment_id: 11, kind: "logs", payload: { service: "api" }, observed_at: root.created_at }],
    })
    api.listTaskEvents.mockResolvedValue({ events: [{ sequence: 9, event_id: "workflow-error", task_key: root.key, queue: "TEST", kind: "workflow.escalated", actor: "system", task_revision: 2, payload: { error_code: "no_matching_transition", message: "No transition matched" }, created_at: root.updated_at }], count: 1 })

    render(<TasksWorkspace />)
    await userEvent.click(await screen.findByRole("button", { name: /Ship native tasks/ }))

    expect(await screen.findByText("No transition matched")).toBeInTheDocument()
    // The execution lists left with the workflow section; the freeze it
    // reports still reaches the panel through the banner.
    expect(screen.queryByText("review-a")).not.toBeInTheDocument()
    expect(screen.queryByText("Need decision")).not.toBeInTheDocument()
    expect(screen.getAllByText("no_matching_transition").length).toBeGreaterThan(0)
    expect(screen.getByText(/error_code no_matching_transition/)).toBeInTheDocument()
    expect(screen.queryByLabelText("Status")).not.toBeInTheDocument()
    expect(screen.queryByLabelText("Assignee")).not.toBeInTheDocument()
    expect(screen.queryByLabelText("Manual block reason")).not.toBeInTheDocument()
    fireEvent.change(screen.getByLabelText("Title"), { target: { value: "Managed title" } })
    await userEvent.click(screen.getByRole("button", { name: "Save task" }))
    expect(api.updateTask).toHaveBeenCalledWith("TEST-1", {
      title: "Managed title", description: managed.description, pull_request: "", priority: managed.priority, revision: managed.revision,
    }, undefined)
  })

  it("activates published workflow revisions and edits explicit pools", async () => {
    api.getQueueWorkflow.mockResolvedValue({ queue: "TEST", workflow_version_id: 7, workflow_name: "development", workflow_version: 1, revision: 3, bound_by: "user:owner", bound_at: root.created_at })
    api.listWorkflowVersions.mockResolvedValue({ items: [{ id: 8, name: "development", version: 2, state: "published", definition: { name: "development", version: 2, initial_status: "implement", statuses: [] }, created_at: root.created_at, updated_at: root.updated_at, published_at: root.updated_at }], count: 1 })
    api.listAgentPools.mockResolvedValue({ items: [{ id: 4, queue: "TEST", name: "developers", agents: ["dev-a"], revision: 2, created_at: root.created_at, updated_at: root.updated_at }], count: 1 })
    api.activateQueueWorkflow.mockResolvedValue({ queue: "TEST", workflow_version_id: 8, workflow_name: "development", workflow_version: 2, revision: 4, bound_by: "user:owner", bound_at: root.created_at })
    api.rebindAgentPool.mockResolvedValue({ id: 4, queue: "TEST", name: "developers", agents: ["dev-a", "dev-b"], revision: 3, created_at: root.created_at, updated_at: root.updated_at })

    render(<TasksWorkspace />)
    await userEvent.click(screen.getByRole("button", { name: "Queue: all" }))
    await userEvent.click(await screen.findByRole("menuitem", { name: "Manage queues…" }))
    await userEvent.click(await screen.findByRole("button", { name: "Workflow settings TEST" }))
    await screen.findByText(/Active: development@1/)
    await userEvent.click(screen.getByRole("button", { name: "Load versions" }))
    await userEvent.selectOptions(await screen.findByLabelText("Published workflow version TEST"), "8")
    await userEvent.click(screen.getByRole("button", { name: "Activate workflow" }))
    expect(api.activateQueueWorkflow).toHaveBeenCalledWith("TEST", 8, 3, expect.any(String), undefined)

    fireEvent.change(screen.getByLabelText("Pool name TEST"), { target: { value: "developers" } })
    fireEvent.change(screen.getByLabelText("Pool agents TEST"), { target: { value: "dev-a, dev-b" } })
    await userEvent.click(screen.getByRole("button", { name: "Save pool" }))
    expect(api.rebindAgentPool).toHaveBeenCalledWith("TEST", "developers", ["dev-a", "dev-b"], 2, expect.any(String), undefined)
  })

  it("fails queue workflow state closed and refreshes revisions after a conflict", async () => {
    api.getQueueWorkflow
      .mockRejectedValueOnce(Object.assign(new Error("permission denied"), { status: 403 }))
      .mockResolvedValueOnce({ queue: "TEST", workflow_version_id: 7, workflow_name: "development", workflow_version: 1, revision: 5, bound_by: "user:owner", bound_at: root.created_at })
    api.listAgentPools
      .mockResolvedValueOnce({ items: [{ id: 4, queue: "TEST", name: "developers", agents: ["dev-a"], revision: 2, created_at: root.created_at, updated_at: root.updated_at }], count: 1 })
      .mockResolvedValueOnce({ items: [{ id: 4, queue: "TEST", name: "developers", agents: ["dev-a"], revision: 6, created_at: root.created_at, updated_at: root.updated_at }], count: 1 })
      .mockResolvedValueOnce({ items: [{ id: 4, queue: "TEST", name: "developers", agents: ["dev-a"], revision: 6, created_at: root.created_at, updated_at: root.updated_at }], count: 1 })

    render(<TasksWorkspace />)
    await userEvent.click(screen.getByRole("button", { name: "Queue: all" }))
    await userEvent.click(await screen.findByRole("menuitem", { name: "Manage queues…" }))
    await userEvent.click(await screen.findByRole("button", { name: "Workflow settings TEST" }))
    expect(await screen.findByRole("alert", { name: "Workflow TEST error" })).toHaveTextContent("permission denied")
    expect(screen.queryByText("Legacy queue (no workflow)")).not.toBeInTheDocument()
    await userEvent.click(screen.getByRole("button", { name: "Retry workflow state TEST" }))
    expect(await screen.findByText(/Active: development@1 · rev 5/)).toBeInTheDocument()

    api.rebindAgentPool
      .mockRejectedValueOnce(Object.assign(new Error("revision conflict"), { status: 409 }))
      .mockResolvedValueOnce({ id: 4, queue: "TEST", name: "developers", agents: ["dev-a", "dev-b"], revision: 7, created_at: root.created_at, updated_at: root.updated_at })
    fireEvent.change(screen.getByLabelText("Pool name TEST"), { target: { value: "developers" } })
    fireEvent.change(screen.getByLabelText("Pool agents TEST"), { target: { value: "dev-a, dev-b" } })
    await userEvent.click(screen.getByRole("button", { name: "Save pool" }))
    expect(await screen.findByText(/Configuration changed elsewhere/)).toBeInTheDocument()
    expect(api.listAgentPools).toHaveBeenCalledTimes(3)
    await userEvent.click(screen.getByRole("button", { name: "Save pool" }))
    expect(api.rebindAgentPool).toHaveBeenLastCalledWith("TEST", "developers", ["dev-a", "dev-b"], 6, expect.any(String), undefined)
  })

  it("shows a managed task without the workflow section and without its auxiliary projections", async () => {
    const managed = { ...root, workflow_version_id: 7, workflow_version: "development@2", workflow_status: "review", workflow_revision: 5 }
    api.listTasks.mockResolvedValue({ tasks: [managed], sequence: 10 })
    api.getTask.mockResolvedValue({ ...detail, task: managed })
    api.getTaskWorkflow.mockResolvedValue({ task: managed, workflow: { id: 7, name: "development", version: 2, state: "published", definition: { name: "development", version: 2, initial_status: "implement", statuses: [] }, created_at: root.created_at, updated_at: root.updated_at }, status_executions: [], requirement_executions: [], assignments: [], holds: [], observations: [] })

    render(<TasksWorkspace />)
    await userEvent.click(await screen.findByRole("button", { name: /Ship native tasks/ }))
    expect(await screen.findByRole("heading", { name: "TEST-1" })).toBeInTheDocument()
    expect(screen.getByText("Starting now")).toBeInTheDocument()
    expect(screen.queryByText("Managed workflow")).not.toBeInTheDocument()
    expect(screen.queryByText("Assignments")).not.toBeInTheDocument()
    expect(screen.queryByText("development@2")).not.toBeInTheDocument()
    expect(screen.queryByText(/unmanaged/)).not.toBeInTheDocument()
    expect(api.listWorkflowArtifacts).not.toHaveBeenCalled()
    expect(api.listWorkflowQuestions).not.toHaveBeenCalled()
  })

  it("names a dependency by its title and status, and the parent only once", async () => {
    render(<TasksWorkspace />)
    await userEvent.click(await screen.findByRole("button", { name: /Ship native tasks/ }))
    const row = (await screen.findByText("Child task")).closest("div")!
    expect(within(row).getByText("TEST-2")).toBeInTheDocument()
    expect(within(row).getByText("In progress")).toBeInTheDocument()
  })

  it("shows one accessible question indicator only for task rows with active question notifications", async () => {
    api.listTaskNotifications.mockResolvedValue({
      notifications: [
        notification,
        { ...notification, id: "unrelated", task_key: child.key, type: "task.assigned" },
        { ...notification, id: "read", task_key: child.key, read_at: root.updated_at },
        { ...notification, id: "dismissed", task_key: child.key, dismissed_at: root.updated_at },
      ],
      count: 4,
    })

    render(<TasksWorkspace />)

    const rootRow = await screen.findByTestId("task-row-TEST-1")
    expect(await within(rootRow).findByRole("img", { name: "Unread question notification for TEST-1" })).toBeVisible()
    await userEvent.click(within(rootRow).getByRole("button", { name: "Expand TEST-1" }))
    const childRow = await screen.findByTestId("task-row-TEST-2")
    expect(within(childRow).queryByRole("img", { name: "Unread question notification for TEST-2" })).toBeNull()
  })

  it("marks a task's question notification read when the task is opened, clearing its indicator", async () => {
    render(<TasksWorkspace />)

    const rootRow = await screen.findByTestId("task-row-TEST-1")
    expect(await within(rootRow).findByRole("img", { name: "Unread question notification for TEST-1" })).toBeVisible()
    await userEvent.click(within(rootRow).getByRole("button", { name: /Ship native tasks/ }))

    await waitFor(() => expect(api.markTaskNotificationRead).toHaveBeenCalledWith("notification-1", undefined))
    expect(within(await screen.findByTestId("task-row-TEST-1")).queryByRole("img", { name: "Unread question notification for TEST-1" })).toBeNull()
  })

  it("keeps a closed task with an unread question in Active until it is opened", async () => {
    const closed: Task = { ...root, key: "TEST-9", title: "Closed with a question", status: "done" }
    api.getTask.mockImplementation(async (key: string) => key === closed.key ? { ...detail, task: closed } : detail)
    api.listTaskNotifications.mockResolvedValue({
      notifications: [notification, { ...notification, id: "closed-question", task_key: closed.key }],
      count: 2,
    })

    render(<TasksWorkspace />)

    const closedRow = await screen.findByTestId("task-row-TEST-9")
    expect(await within(closedRow).findByRole("img", { name: "Unread question notification for TEST-9" })).toBeVisible()
    expect(api.getTask).toHaveBeenCalledWith("TEST-9", undefined)
    expect(api.getTask).not.toHaveBeenCalledWith("TEST-1", undefined)
    await userEvent.click(within(closedRow).getByRole("button", { name: /Closed with a question/ }))
    await waitFor(() => expect(api.markTaskNotificationRead).toHaveBeenCalledWith("closed-question", undefined))
  })

  it("pulls in only the scoped agent's closed questions", async () => {
    const closed: Task = { ...root, key: "TEST-9", title: "Closed with a question", status: "done" }
    api.getTask.mockImplementation(async (key: string) => key === closed.key ? { ...detail, task: closed } : detail)
    api.listTaskNotifications.mockResolvedValue({
      notifications: [{ ...notification, id: "other-agent", task_key: closed.key, requesting_principal: "agent:triager" }],
      count: 1,
    })

    render(<TasksWorkspace scopeAgent="worker" />)

    await screen.findByTestId("task-row-TEST-1")
    await act(async () => {})
    expect(screen.queryByTestId("task-row-TEST-9")).toBeNull()
    expect(api.getTask).not.toHaveBeenCalled()
  })

  it("has no notification inbox of its own: the task rows carry the questions", async () => {
    render(<TasksWorkspace />)

    await screen.findByTestId("task-row-TEST-1")
    expect(screen.queryByRole("button", { name: /Notifications/ })).toBeNull()
    expect(screen.queryByText("Need a product decision")).toBeNull()
  })

  it("updates task row question indicators when realtime refreshes the notification inbox", async () => {
    render(<TasksWorkspace />)

    const rootRow = await screen.findByTestId("task-row-TEST-1")
    expect(await within(rootRow).findByRole("img", { name: "Unread question notification for TEST-1" })).toBeVisible()
    api.listTaskNotifications.mockResolvedValue({ notifications: [], count: 0 })
    const onHint = taskSocket.options?.onHint as (hint: { sequence: number; kind: string; task_key: string }) => void
    act(() => onHint({ sequence: 11, kind: "task.question", task_key: root.key }))

    await waitFor(() => expect(within(screen.getByTestId("task-row-TEST-1")).queryByRole("img", { name: "Unread question notification for TEST-1" })).toBeNull())
  })

  it("labels nested in-progress tasks visibly and accessibly without changing open rows", async () => {
    api.listTasks.mockResolvedValue({
      tasks: [
        { ...root, status: "open" },
        { ...child, status: "in_progress" },
      ],
      sequence: 10,
    })

    render(<TasksWorkspace />)

    const parentRow = await screen.findByTestId("task-row-TEST-1")
    await userEvent.click(within(parentRow).getByRole("button", { name: "Expand TEST-1" }))
    const childRow = await screen.findByTestId("task-row-TEST-2")
    // The status is its own column now, not a badge inside the row's button:
    // visible text, and the tone that carries the meaning.
    const badge = within(childRow).getByText("In progress")
    expect(badge).toBeVisible()
    expect(badge).toHaveAttribute("data-tone", "live")
    expect(within(parentRow).queryByText("In progress")).toBeNull()
  })

  it("defaults to Active and accepts only the latest status-view tree", async () => {
    const closedPage = deferred<{ tasks: Task[]; sequence: number }>()
    const allPage = deferred<{ tasks: Task[]; sequence: number }>()
    api.listTasks
      .mockResolvedValueOnce({ tasks: [root, child], sequence: 10 })
      .mockReturnValueOnce(closedPage.promise)
      .mockReturnValueOnce(allPage.promise)

    render(<TasksWorkspace />)

    await screen.findByText("Ship native tasks")
    expect(api.listTasks).toHaveBeenLastCalledWith(
      expect.objectContaining({ status_view: "active" }),
      undefined,
    )

    await userEvent.click(within(screen.getByRole("radiogroup", { name: "Task status" })).getByRole("radio", { name: "Closed" }))
    await waitFor(() => expect(api.listTasks).toHaveBeenLastCalledWith(
      expect.objectContaining({ status_view: "closed" }),
      undefined,
    ))
    await userEvent.click(within(screen.getByRole("radiogroup", { name: "Task status" })).getByRole("radio", { name: "All" }))
    await waitFor(() => expect(api.listTasks).toHaveBeenLastCalledWith(
      expect.objectContaining({ status_view: "all" }),
      undefined,
    ))

    await act(async () => closedPage.resolve({ tasks: [], sequence: 11 }))
    expect(screen.getByText("Ship native tasks")).toBeInTheDocument()
    await act(async () => allPage.resolve({ tasks: [{ ...root, title: "Completed task", status: "done" }], sequence: 12 }))
    expect(await screen.findByText("Completed task")).toBeInTheDocument()
    expect(screen.queryByText("Ship native tasks")).toBeNull()

    api.listTasks.mockResolvedValueOnce({ tasks: [{ ...root, title: "Completed task", status: "done" }], sequence: 13 })
    const onHint = taskSocket.options?.onHint as (hint: { sequence: number; kind: string; task_key: string }) => void
    act(() => onHint({ sequence: 13, kind: "task.updated", task_key: root.key }))
    await waitFor(() => expect(api.listTasks).toHaveBeenLastCalledWith(
      expect.objectContaining({ status_view: "all" }),
      undefined,
    ))
  })

  it("waits for the accepted initial sequence and coalesces a replay burst into one refresh", async () => {
    const firstPage = deferred<{ tasks: Task[]; sequence: number }>()
    const refreshPage = deferred<{ tasks: Task[]; sequence: number }>()
    api.listTasks.mockReturnValueOnce(firstPage.promise)

    render(<TasksWorkspace />)

    expect(taskSocket.options).toEqual(expect.objectContaining({ after: 0, enabled: false }))
    await act(async () => firstPage.resolve({ tasks: [root, child], sequence: 10 }))
    await screen.findByText("Ship native tasks")
    await waitFor(() => expect(taskSocket.options).toEqual(expect.objectContaining({ after: 10, enabled: true })))
    await act(async () => {
      await Promise.resolve()
      await Promise.resolve()
    })
    const metadataCalls = api.listTaskQueues.mock.calls.length
    const principalCalls = api.listTaskPrincipals.mock.calls.length
    const notificationCalls = api.listTaskNotifications.mock.calls.length
    api.listTasks.mockReturnValueOnce(refreshPage.promise)

    const onHint = taskSocket.options?.onHint as (hint: { sequence: number; kind: string; task_key: string }) => void
    act(() => {
      for (let sequence = 11; sequence <= 110; sequence += 1) {
        onHint({ sequence, kind: "task.updated", task_key: root.key })
      }
    })

    await waitFor(() => expect(api.listTasks).toHaveBeenCalledTimes(2))
    await act(async () => refreshPage.resolve({ tasks: [root, child], sequence: 110 }))
    await new Promise((resolve) => setTimeout(resolve, 0))

    expect(api.listTasks).toHaveBeenCalledTimes(2)
    expect(api.listTaskQueues).toHaveBeenCalledTimes(metadataCalls + 1)
    expect(api.listTaskPrincipals).toHaveBeenCalledTimes(principalCalls + 1)
    // Metadata refreshes notifications once and the realtime cycle refreshes the inbox once.
    expect(api.listTaskNotifications).toHaveBeenCalledTimes(notificationCalls + 2)
  })

  it("runs one trailing refresh when a hint arrives during an active refresh", async () => {
    const refresh = deferred<{ tasks: Task[]; sequence: number }>()
    render(<TasksWorkspace />)
    await screen.findByText("Ship native tasks")
    const onHint = taskSocket.options?.onHint as (hint: { sequence: number; kind: string; task_key: string }) => void
    api.listTasks.mockReturnValueOnce(refresh.promise)

    act(() => onHint({ sequence: 11, kind: "task.updated", task_key: root.key }))
    await waitFor(() => expect(api.listTasks).toHaveBeenCalledTimes(2))
    act(() => onHint({ sequence: 12, kind: "task.updated", task_key: root.key }))
    await act(async () => refresh.resolve({ tasks: [{ ...root, title: "First refresh" }], sequence: 11 }))

    await waitFor(() => expect(api.listTasks).toHaveBeenCalledTimes(3))
    expect(await screen.findByText("Ship native tasks")).toBeInTheDocument()
  })

  it("renders priority accessibly and saves edits through the task update", async () => {
    render(<TasksWorkspace />)

    expect(await screen.findByLabelText("P0 Critical")).toHaveTextContent("P0")
    await userEvent.click(screen.getByRole("button", { name: /Ship native tasks/ }))
    const priority = await screen.findByLabelText("Priority")
    expect(priority).toHaveValue("P0")

    await userEvent.selectOptions(priority, "P1")
    await userEvent.click(screen.getByRole("button", { name: "Save task" }))

    expect(api.updateTask).toHaveBeenCalledWith(
      "TEST-1",
      expect.objectContaining({ priority: "P1", revision: 2 }),
      undefined,
    )
  })

  it("saves wait-customer and set/clear pull request edits with loaded revisions on the explicit host", async () => {
    const target = {
      id: "remote", label: "Remote", baseURL: "https://remote.test", token: "secret",
    }
    api.updateTask.mockImplementation((_key: string, input: Partial<Task>) => Promise.resolve({
      ...root,
      ...input,
      revision: Number(input.revision) + 1,
    }))
    render(<TasksWorkspace target={target} />)

    await userEvent.click(await screen.findByRole("button", { name: /Ship native tasks/ }))
    await userEvent.selectOptions(await screen.findByLabelText("Status"), "wait_customer")
    await userEvent.type(screen.getByLabelText("Pull request"), "https://example.test/pull/7")
    await userEvent.click(screen.getByRole("button", { name: "Save task" }))

    expect(api.updateTask).toHaveBeenNthCalledWith(1, "TEST-1", expect.objectContaining({
      status: "wait_customer",
      pull_request: "https://example.test/pull/7",
      revision: 2,
    }), target)
    const pullRequest = await screen.findByLabelText("Pull request")
    expect(pullRequest).toHaveValue("https://example.test/pull/7")
    await userEvent.clear(pullRequest)
    await userEvent.click(screen.getByRole("button", { name: "Save task" }))

    expect(api.updateTask).toHaveBeenNthCalledWith(2, "TEST-1", expect.objectContaining({
      status: "wait_customer",
      pull_request: "",
      revision: 3,
    }), target)
  })

  it("shows the loading placeholder only until the initial task tree arrives", async () => {
    const firstPage = deferred<{ tasks: Task[]; sequence: number }>()
    api.listTasks.mockReturnValueOnce(firstPage.promise)

    render(<TasksWorkspace />)

    expect(await screen.findByText("Loading tasks…")).toBeInTheDocument()
    expect(screen.queryByText("Ship native tasks")).toBeNull()

    await act(async () => firstPage.resolve({ tasks: [root, child], sequence: 10 }))
    expect(await screen.findByText("Ship native tasks")).toBeInTheDocument()
    expect(screen.queryByText("Loading tasks…")).toBeNull()
  })

  it("keeps the current tree visible and marks a background refresh busy", async () => {
    const refreshPage = deferred<{ tasks: Task[]; sequence: number }>()
    render(<TasksWorkspace />)
    expect(await screen.findByText("Ship native tasks")).toBeInTheDocument()
    api.listTasks.mockReturnValueOnce(refreshPage.promise)

    await userEvent.click(screen.getByRole("button", { name: "Refresh tasks" }))

    expect(screen.getByText("Ship native tasks")).toBeInTheDocument()
    expect(screen.queryByText("Loading tasks…")).toBeNull()
    expect(screen.getByRole("button", { name: "Refresh tasks" })).toHaveAttribute("aria-busy", "true")

    await act(async () => refreshPage.resolve({ tasks: [root], sequence: 11 }))
    await waitFor(() => expect(screen.getByRole("button", { name: "Refresh tasks" })).toHaveAttribute("aria-busy", "false"))
  })

  it("retains the current tree when a background refresh fails", async () => {
    const refreshPage = deferred<{ tasks: Task[]; sequence: number }>()
    render(<TasksWorkspace />)
    expect(await screen.findByText("Ship native tasks")).toBeInTheDocument()
    api.listTasks.mockReturnValueOnce(refreshPage.promise)

    await userEvent.click(screen.getByRole("button", { name: "Refresh tasks" }))
    await act(async () => refreshPage.reject(new Error("refresh failed")))

    await waitFor(() => expect(screen.getByRole("button", { name: "Refresh tasks" })).toHaveAttribute("aria-busy", "false"))
    expect(screen.getByText("Ship native tasks")).toBeInTheDocument()
    expect(screen.queryByText("Loading tasks…")).toBeNull()
    expect(toast.error).toHaveBeenCalledWith("refresh failed")
  })

  it("does not reload the initial tree when principal metadata arrives", async () => {
    const people = deferred<{ customer: string; agents: string[]; groups: string[] }>()
    api.listTaskPrincipals.mockReturnValueOnce(people.promise)

    render(<TasksWorkspace />)
    expect(await screen.findByText("Ship native tasks")).toBeInTheDocument()
    expect(api.listTasks).toHaveBeenCalledTimes(1)

    await act(async () => people.resolve({
      customer: "user:owner",
      agents: ["worker", "triager"],
      groups: ["platform"],
    }))
    await waitFor(() => expect(api.listTaskPrincipals).toHaveBeenCalledTimes(1))
    expect(api.listTasks).toHaveBeenCalledTimes(1)
  })

  it("reloads a principal-filtered view when delayed customer metadata arrives", async () => {
    const people = deferred<{ customer: string; agents: string[]; groups: string[] }>()
    api.listTaskPrincipals.mockReturnValueOnce(people.promise)

    render(<TasksWorkspace />)
    expect(await screen.findByText("Ship native tasks")).toBeInTheDocument()
    const callsBeforeFilter = api.listTasks.mock.calls.length

    await userEvent.click(screen.getByRole("button", { name: "My tasks" }))
    expect(await screen.findByText("Loading tasks…")).toBeInTheDocument()
    expect(api.listTasks).toHaveBeenCalledTimes(callsBeforeFilter)

    await act(async () => people.resolve({
      customer: "user:owner",
      agents: ["worker", "triager"],
      groups: ["platform"],
    }))

    await waitFor(() => expect(api.listTasks).toHaveBeenLastCalledWith(
      expect.objectContaining({ assignee: "user:owner" }),
      undefined,
    ))
  })

  it("does not accept an in-flight unfiltered result while waiting for principals", async () => {
    const allTasks = deferred<{ tasks: Task[]; sequence: number }>()
    const people = deferred<{ customer: string; agents: string[]; groups: string[] }>()
    api.listTasks.mockReturnValueOnce(allTasks.promise)
    api.listTaskPrincipals.mockReturnValueOnce(people.promise)

    render(<TasksWorkspace />)
    expect(await screen.findByText("Loading tasks…")).toBeInTheDocument()
    await userEvent.click(screen.getByRole("button", { name: "My tasks" }))
    await act(async () => allTasks.resolve({ tasks: [root, child], sequence: 10 }))

    expect(screen.getByText("Loading tasks…")).toBeInTheDocument()
    expect(screen.queryByText("Ship native tasks")).toBeNull()
    await act(async () => people.resolve({ customer: "user:owner", agents: [], groups: [] }))
    await waitFor(() => expect(api.listTasks).toHaveBeenLastCalledWith(
      expect.objectContaining({ assignee: "user:owner" }),
      undefined,
    ))
    expect(await screen.findByText("Ship native tasks")).toBeInTheDocument()
  })

  it("shows principal metadata errors and retries them from Refresh", async () => {
    const people = deferred<{ customer: string; agents: string[]; groups: string[] }>()
    api.listTaskPrincipals.mockReturnValueOnce(people.promise)

    render(<TasksWorkspace />)
    expect(await screen.findByText("Ship native tasks")).toBeInTheDocument()
    await act(async () => people.reject(new Error("principals failed")))
    await userEvent.click(screen.getByRole("button", { name: "My tasks" }))
    expect(await screen.findByText("principals failed")).toBeInTheDocument()

    api.listTaskPrincipals.mockResolvedValueOnce({ customer: "user:owner", agents: [], groups: [] })
    await userEvent.click(screen.getByRole("button", { name: "Refresh tasks" }))

    await waitFor(() => expect(api.listTasks).toHaveBeenLastCalledWith(
      expect.objectContaining({ assignee: "user:owner" }),
      undefined,
    ))
    expect(await screen.findByText("Ship native tasks")).toBeInTheDocument()
  })

  it("ignores an older metadata failure after a newer metadata load succeeds", async () => {
    const olderPeople = deferred<{ customer: string; agents: string[]; groups: string[] }>()
    const newerPeople = deferred<{ customer: string; agents: string[]; groups: string[] }>()
    api.listTaskPrincipals
      .mockReturnValueOnce(olderPeople.promise)
      .mockReturnValueOnce(newerPeople.promise)

    render(<TasksWorkspace />)
    expect(await screen.findByText("Ship native tasks")).toBeInTheDocument()
    await userEvent.click(screen.getByRole("button", { name: "My tasks" }))
    await userEvent.click(screen.getByRole("button", { name: "Refresh tasks" }))
    await act(async () => newerPeople.resolve({ customer: "user:new", agents: [], groups: [] }))
    await waitFor(() => expect(api.listTasks).toHaveBeenLastCalledWith(
      expect.objectContaining({ assignee: "user:new" }),
      undefined,
    ))

    await act(async () => olderPeople.reject(new Error("obsolete metadata failure")))

    expect(toast.error).not.toHaveBeenCalledWith("obsolete metadata failure")
    expect(screen.queryByText("obsolete metadata failure")).toBeNull()
  })

  it("ignores older metadata that succeeds after the latest customer", async () => {
    const olderPeople = deferred<{ customer: string; agents: string[]; groups: string[] }>()
    const newerPeople = deferred<{ customer: string; agents: string[]; groups: string[] }>()
    api.listTaskPrincipals
      .mockReturnValueOnce(olderPeople.promise)
      .mockReturnValueOnce(newerPeople.promise)

    render(<TasksWorkspace />)
    expect(await screen.findByText("Ship native tasks")).toBeInTheDocument()
    await userEvent.click(screen.getByRole("button", { name: "Waiting for me" }))
    await userEvent.click(screen.getByRole("button", { name: "Refresh tasks" }))
    await act(async () => newerPeople.resolve({ customer: "user:new", agents: [], groups: [] }))
    await waitFor(() => expect(api.listTasks).toHaveBeenLastCalledWith(
      expect.objectContaining({ waiting_for: "user:new" }),
      undefined,
    ))
    const callsAfterLatest = api.listTasks.mock.calls.length

    await act(async () => olderPeople.resolve({ customer: "user:old", agents: [], groups: [] }))

    expect(api.listTasks).toHaveBeenCalledTimes(callsAfterLatest)
  })

  it("does not reload an unfiltered view when principal metadata fails", async () => {
    const people = deferred<{ customer: string; agents: string[]; groups: string[] }>()
    api.listTaskPrincipals.mockReturnValueOnce(people.promise)
    render(<TasksWorkspace />)
    expect(await screen.findByText("Ship native tasks")).toBeInTheDocument()
    const callsBeforeFailure = api.listTasks.mock.calls.length

    await act(async () => people.reject(new Error("principals failed")))

    expect(api.listTasks).toHaveBeenCalledTimes(callsBeforeFailure)
    expect(screen.getByText("Ship native tasks")).toBeInTheDocument()
  })

  it("resets the global workspace when the active daemon changes", async () => {
    const nextTree = deferred<{ tasks: Task[]; sequence: number }>()
    const { rerender } = render(<TasksWorkspace />)
    expect(await screen.findByText("Ship native tasks")).toBeInTheDocument()
    api.listTasks.mockReturnValueOnce(nextTree.promise)

    daemonContext.activeId = "second-host"
    rerender(<TasksWorkspace />)

    expect(await screen.findByText("Loading tasks…")).toBeInTheDocument()
    expect(screen.queryByText("Ship native tasks")).toBeNull()
    await act(async () => nextTree.resolve({ tasks: [{ ...root, title: "Second host tree" }], sequence: 1 }))
    expect(await screen.findByText("Second host tree")).toBeInTheDocument()
  })

  it("reloads a principal-filtered view with the new target customer", async () => {
    const firstTarget = { id: "first", label: "First", baseURL: "http://first.test", token: "first" }
    const secondTarget = { id: "second", label: "Second", baseURL: "http://second.test", token: "second" }
    const secondPeople = deferred<{ customer: string; agents: string[]; groups: string[] }>()
    api.listTaskPrincipals
      .mockResolvedValueOnce({ customer: "user:first", agents: [], groups: [] })
      .mockReturnValueOnce(secondPeople.promise)

    const { rerender } = render(<TasksWorkspace target={firstTarget} />)
    expect(await screen.findByText("Ship native tasks")).toBeInTheDocument()
    await userEvent.click(screen.getByRole("button", { name: "Waiting for me" }))
    await waitFor(() => expect(api.listTasks).toHaveBeenLastCalledWith(
      expect.objectContaining({ waiting_for: "user:first" }),
      firstTarget,
    ))

    rerender(<TasksWorkspace target={secondTarget} />)
    expect(await screen.findByText("Ship native tasks")).toBeInTheDocument()
    await userEvent.click(screen.getByRole("button", { name: "Waiting for me" }))
    const secondTargetCalls = () => api.listTasks.mock.calls.filter(([, callTarget]) => callTarget === secondTarget)
    expect(secondTargetCalls()).toHaveLength(1)
    await act(async () => secondPeople.resolve({ customer: "user:second", agents: [], groups: [] }))

    await waitFor(() => expect(api.listTasks).toHaveBeenLastCalledWith(
      expect.objectContaining({ waiting_for: "user:second" }),
      secondTarget,
    ))
  })

  it("ignores principal metadata that resolves after switching targets", async () => {
    const firstTarget = { id: "first", label: "First", baseURL: "http://first.test", token: "first" }
    const secondTarget = { id: "second", label: "Second", baseURL: "http://second.test", token: "second" }
    const firstPeople = deferred<{ customer: string; agents: string[]; groups: string[] }>()
    api.listTaskPrincipals
      .mockReturnValueOnce(firstPeople.promise)
      .mockResolvedValueOnce({ customer: "user:second", agents: [], groups: [] })

    const { rerender } = render(<TasksWorkspace target={firstTarget} />)
    expect(await screen.findByText("Ship native tasks")).toBeInTheDocument()
    rerender(<TasksWorkspace target={secondTarget} />)
    expect(await screen.findByText("Ship native tasks")).toBeInTheDocument()
    await userEvent.click(screen.getByRole("button", { name: "My tasks" }))
    await waitFor(() => expect(api.listTasks).toHaveBeenLastCalledWith(
      expect.objectContaining({ assignee: "user:second" }),
      secondTarget,
    ))
    const callsAfterSecondCustomer = api.listTasks.mock.calls.length

    await act(async () => firstPeople.resolve({ customer: "user:first", agents: [], groups: [] }))

    expect(api.listTasks).toHaveBeenCalledTimes(callsAfterSecondCustomer)
  })

  it("does not toast an obsolete refresh failure after a newer refresh succeeds", async () => {
    const olderRefresh = deferred<{ tasks: Task[]; sequence: number }>()
    const newerTask = { ...root, title: "Newest tree" }
    render(<TasksWorkspace />)
    expect(await screen.findByText("Ship native tasks")).toBeInTheDocument()
    api.listTasks
      .mockReturnValueOnce(olderRefresh.promise)
      .mockResolvedValueOnce({ tasks: [newerTask], sequence: 12 })

    await userEvent.click(screen.getByRole("button", { name: "Refresh tasks" }))
    await userEvent.click(screen.getByRole("button", { name: "Refresh tasks" }))
    expect(await screen.findByText("Newest tree")).toBeInTheDocument()
    await act(async () => olderRefresh.reject(new Error("obsolete failure")))

    expect(screen.getByText("Newest tree")).toBeInTheDocument()
    expect(toast.error).not.toHaveBeenCalledWith("obsolete failure")
  })

  it("renders the shared tree and opens task detail", async () => {
    render(<TasksWorkspace scopeAgent="worker" />)

    expect(await screen.findByText("Ship native tasks")).toBeInTheDocument()
    expect(screen.getByRole("button", { name: "Queue: all" })).toBeInTheDocument()
    expect(screen.queryByText("Desktop tree")).toBeNull()
    await userEvent.click(screen.getByRole("button", { name: "Expand TEST-1" }))
    expect(screen.getByText("Desktop tree")).toBeInTheDocument()

    await userEvent.click(screen.getByRole("button", { name: /Ship native tasks/ }))
    expect(await screen.findByRole("heading", { name: "TEST-1" })).toBeInTheDocument()
    expect(screen.getByRole("textbox", { name: "Description" })).toHaveTextContent("Central work system")
    expect(screen.getByText("Starting now")).toBeInTheDocument()
  })

  it("creates tasks and children inline in the selected queue", async () => {
    render(<TasksWorkspace />)
    await screen.findByText("Ship native tasks")

    await userEvent.click(screen.getByRole("button", { name: "New task" }))
    fireEvent.change(screen.getByLabelText("Task title"), { target: { value: "New root" } })
    await userEvent.click(screen.getByRole("button", { name: "Create task" }))
    expect(api.createTask).toHaveBeenCalledWith(
      expect.objectContaining({ queue: "TEST", title: "New root", parent_key: "" }),
      undefined,
    )

    await userEvent.click(await screen.findByRole("button", { name: "Close task detail" }))
    await userEvent.click(screen.getByRole("button", { name: "Add child to TEST-1" }))
    fireEvent.change(screen.getByLabelText("Task title"), { target: { value: "Child work" } })
    await userEvent.click(screen.getByRole("button", { name: "Create task" }))
    expect(api.createTask).toHaveBeenLastCalledWith(
      expect.objectContaining({ parent_key: "TEST-1", title: "Child work" }),
      undefined,
    )
  })

  it("accepts a suggested or arbitrary assignee and posts questions as mentions", async () => {
    render(<TasksWorkspace />)
    await userEvent.click(await screen.findByRole("button", { name: /Ship native tasks/ }))
    await screen.findByRole("heading", { name: "TEST-1" })

    const assignee = screen.getByLabelText("Assignee")
    fireEvent.change(assignee, { target: { value: "freelance-reviewer" } })
    await userEvent.click(screen.getByRole("button", { name: "Save task" }))
    expect(api.updateTask).toHaveBeenCalledWith(
      "TEST-1",
      expect.objectContaining({ assignee: "freelance-reviewer", revision: 2 }),
      undefined,
    )

    await userEvent.selectOptions(screen.getByLabelText("Ask"), "user:owner")
    await userEvent.click(within(screen.getByText("Comments").closest("section")!).getByRole("button", { name: "Markdown" }))
    fireEvent.change(screen.getByLabelText("Comment"), { target: { value: "Which release?" } })
    await userEvent.click(screen.getByRole("button", { name: "Send comment" }))
    expect(api.addTaskComment).toHaveBeenCalledWith(
      "TEST-1",
      "@user:owner\n\nWhich release?",
      undefined,
      expect.any(String),
    )
  })

  it("defaults a comment question to the task assignee", async () => {
    render(<TasksWorkspace />)
    await userEvent.click(await screen.findByRole("button", { name: /Ship native tasks/ }))
    await screen.findByRole("heading", { name: "TEST-1" })

    await userEvent.click(within(screen.getByText("Comments").closest("section")!).getByRole("button", { name: "Markdown" }))
    fireEvent.change(screen.getByLabelText("Comment"), { target: { value: "Can you verify?" } })
    await userEvent.click(screen.getByRole("button", { name: "Send comment" }))

    expect(api.addTaskComment).toHaveBeenCalledWith(
      "TEST-1",
      "@agent:worker\n\nCan you verify?",
      undefined,
      expect.any(String),
    )
  })

  it("sends Ok with the selected question principal", async () => {
    render(<TasksWorkspace />)
    await userEvent.click(await screen.findByRole("button", { name: /Ship native tasks/ }))
    await screen.findByRole("heading", { name: "TEST-1" })

    const comments = screen.getByText("Comments").closest("section")!
    await userEvent.selectOptions(screen.getByLabelText("Ask"), "user:owner")

    const sendOk = within(comments).getByRole("button", { name: "Send Ok" })
    const sendComment = within(comments).getByRole("button", { name: "Send comment" })
    expect(sendOk).toHaveAttribute("data-variant", "secondary")
    expect(sendOk.compareDocumentPosition(sendComment) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy()
    await userEvent.click(sendOk)

    await waitFor(() => expect(api.addTaskComment).toHaveBeenCalledWith(
      "TEST-1",
      "@user:owner\n\nOk",
      undefined,
      expect.any(String),
    ))
    expect(screen.getByLabelText("Ask")).toHaveValue("user:owner")
  })

  it("hides Send Ok when the comment contains text", async () => {
    render(<TasksWorkspace />)
    await userEvent.click(await screen.findByRole("button", { name: /Ship native tasks/ }))
    await screen.findByRole("heading", { name: "TEST-1" })

    const comments = screen.getByText("Comments").closest("section")!
    await userEvent.click(within(comments).getByRole("button", { name: "Markdown" }))
    fireEvent.change(screen.getByLabelText("Comment"), { target: { value: "Keep this draft" } })

    expect(within(comments).queryByRole("button", { name: "Send Ok" })).not.toBeInTheDocument()
  })

  it("updates and restores the default question when the assignee changes", async () => {
    render(<TasksWorkspace />)
    await userEvent.click(await screen.findByRole("button", { name: /Ship native tasks/ }))
    await screen.findByRole("heading", { name: "TEST-1" })

    const ask = screen.getByLabelText("Ask")
    expect(ask).toHaveValue("worker")
    await userEvent.selectOptions(ask, "")
    await userEvent.click(within(screen.getByText("Comments").closest("section")!).getByRole("button", { name: "Markdown" }))
    fireEvent.change(screen.getByLabelText("Comment"), { target: { value: "Can you verify?" } })
    expect(ask).toHaveValue("")

    const reassigned = { ...root, assignee: "freelance-reviewer", revision: 3 }
    api.getTask.mockResolvedValue({ ...detail, task: reassigned })
    fireEvent.change(screen.getByLabelText("Assignee"), { target: { value: reassigned.assignee } })
    await userEvent.click(screen.getByRole("button", { name: "Save task" }))
    expect(ask).toHaveValue("freelance-reviewer")
    expect(within(ask).getByRole("option", { name: "freelance-reviewer" })).toBeInTheDocument()

    await userEvent.selectOptions(ask, "user:owner")
    await userEvent.click(screen.getByRole("button", { name: "Send comment" }))
    await waitFor(() => expect(api.addTaskComment).toHaveBeenCalledWith(
      "TEST-1",
      "@user:owner\n\nCan you verify?",
      undefined,
      expect.any(String),
    ))
    expect(ask).toHaveValue("freelance-reviewer")
  })

  it("shows task metadata, dependencies, and history and edits relations", async () => {
    render(<TasksWorkspace />)
    await userEvent.click(await screen.findByRole("button", { name: /Ship native tasks/ }))
    await screen.findByRole("heading", { name: "TEST-1" })

    expect(screen.getAllByText("user:owner", { selector: "dd" })).toHaveLength(2)
    expect(screen.getByText("TEST", { selector: "dd" })).toBeInTheDocument()
    const dependencies = screen.getByText("Dependencies").closest("section")!
    expect(within(dependencies).getByText("blocks")).toBeInTheDocument()
    expect(within(dependencies).getByText("TEST-2")).toBeInTheDocument()
    expect(screen.getByText("task.updated")).toBeInTheDocument()

    await userEvent.selectOptions(screen.getByLabelText("Relation type"), "related")
    fireEvent.change(screen.getByLabelText("Related task key"), { target: { value: "TEST-2" } })
    expect(screen.getByRole("button", { name: "Add relation" })).toBeEnabled()
    await userEvent.click(screen.getByRole("button", { name: "Add relation" }))
    await waitFor(() => expect(api.addTaskRelation).toHaveBeenCalledWith(
      "TEST-1", "TEST-2", "related", 2, undefined, expect.any(String),
    ))

    await userEvent.click(screen.getByRole("button", { name: "Remove relation to TEST-2" }))
    expect(api.deleteTaskRelation).toHaveBeenCalledWith(
      "TEST-1", 9, 2, undefined, expect.any(String),
    )
  })

  it("filters waiting work by the customer principal", async () => {
    render(<TasksWorkspace />)
    await screen.findByText("Ship native tasks")

    await userEvent.click(screen.getByRole("button", { name: "Waiting for me" }))
    await waitFor(() => expect(api.listTasks).toHaveBeenLastCalledWith(
      expect.objectContaining({ waiting_for: "user:owner" }),
      undefined,
    ))
  })

  it("creates and updates queues", async () => {
    render(<TasksWorkspace />)
    await screen.findByText("Ship native tasks")

    await userEvent.click(screen.getByRole("button", { name: "Queue: all" }))
    await userEvent.click(await screen.findByRole("menuitem", { name: "Manage queues…" }))
    await userEvent.click(await screen.findByRole("button", { name: "New queue" }))
    fireEvent.change(screen.getByLabelText("Queue prefix"), { target: { value: "OPS" } })
    fireEvent.change(screen.getByLabelText("New queue name"), { target: { value: "Operations" } })
    await userEvent.click(screen.getByRole("button", { name: "Add queue" }))
    expect(api.createTaskQueue).toHaveBeenCalledWith(
      expect.objectContaining({ prefix: "OPS", name: "Operations" }),
      undefined,
    )

    await userEvent.click(screen.getByRole("button", { name: "Rename TEST" }))
    fireEvent.change(screen.getByLabelText("Queue name TEST"), { target: { value: "Tests updated" } })
    await userEvent.click(screen.getByRole("button", { name: "Save TEST" }))
    expect(api.updateTaskQueue).toHaveBeenCalledWith(
      "TEST",
      expect.objectContaining({ name: "Tests updated", revision: 1 }),
      undefined,
    )

  })
  it("replaces the navigation rail with toolbar filters", async () => {
    render(<TasksWorkspace />)
    await screen.findByText("Ship native tasks")

    expect(document.querySelector(".tasks-navigation")).toBeNull()
    expect(screen.queryByRole("separator", { name: "Resize task navigation" })).toBeNull()

    expect(screen.getByRole("button", { name: "My tasks" })).toHaveAttribute("aria-pressed", "false")
    await userEvent.click(screen.getByRole("button", { name: "My tasks" }))
    await waitFor(() => expect(api.listTasks).toHaveBeenLastCalledWith(
      expect.objectContaining({ assignee: "user:owner" }),
      undefined,
    ))
    expect(screen.getByRole("button", { name: "My tasks" })).toHaveAttribute("aria-pressed", "true")

    await userEvent.click(screen.getByRole("button", { name: "Waiting for me" }))
    await waitFor(() => expect(api.listTasks).toHaveBeenLastCalledWith(
      expect.objectContaining({ waiting_for: "user:owner" }),
      undefined,
    ))
    expect(api.listTasks.mock.calls.at(-1)?.[0].assignee).toBeUndefined()

    await userEvent.click(screen.getByRole("button", { name: "Waiting for me" }))
    await waitFor(() => expect(api.listTasks.mock.calls.at(-1)?.[0].waiting_for).toBeUndefined())
  })

  it("filters the tree by queue from the toolbar without narrowing the daemon request", async () => {
    const ops: Task = { ...root, key: "OPS-1", queue: "OPS", title: "Rotate credentials" }
    api.listTaskQueues.mockResolvedValue({
      queues: [queue, { ...queue, prefix: "OPS", name: "Operations" }],
      count: 2,
    })
    api.listTasks.mockResolvedValue({ tasks: [root, child, ops], sequence: 10 })
    render(<TasksWorkspace />)
    await screen.findByText("Ship native tasks")
    expect(screen.getByText("Rotate credentials")).toBeInTheDocument()

    await userEvent.click(screen.getByRole("button", { name: "Queue: all" }))
    await userEvent.click(await screen.findByRole("menuitem", { name: /^TEST/ }))

    expect(await screen.findByRole("button", { name: "Queue: TEST" })).toBeInTheDocument()
    expect(screen.queryByText("Rotate credentials")).toBeNull()
    expect(screen.getByText("Ship native tasks")).toBeInTheDocument()
    for (const call of api.listTasks.mock.calls) expect(call[0].queue).toBeUndefined()
  })
  it("keeps the queue filter across the Agents and All tasks switch", async () => {
    const ops: Task = { ...root, key: "OPS-1", queue: "OPS", title: "Rotate credentials" }
    api.listTaskQueues.mockResolvedValue({
      queues: [queue, { ...queue, prefix: "OPS", name: "Operations" }],
      count: 2,
    })
    api.listTasks.mockResolvedValue({ tasks: [root, child, ops], sequence: 10 })

    const agent = render(<TasksWorkspace scopeAgent="worker" />)
    await screen.findByText("Ship native tasks")
    await userEvent.click(screen.getByRole("button", { name: "Queue: all" }))
    await userEvent.click(await screen.findByRole("menuitem", { name: /^OPS/ }))
    expect(await screen.findByRole("button", { name: "Queue: OPS" })).toBeInTheDocument()
    agent.unmount()

    render(<TasksWorkspace />)
    expect(await screen.findByRole("button", { name: "Queue: OPS" })).toBeInTheDocument()
    expect(await screen.findByText("Rotate credentials")).toBeInTheDocument()
    expect(screen.queryByText("Ship native tasks")).toBeNull()
  })
})
