import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { beforeEach, expect, it, vi } from "vitest"
import { ApiError } from "@/lib/api"
import { WorkflowPanel } from "./WorkflowPanel"
import { customerView, poolView, workflowTask, remoteTarget } from "./workflowFixtures"

const api = vi.hoisted(() => ({
  getTaskWorkflow: vi.fn(),
  advanceTask: vi.fn(),
  moveTaskWorkflow: vi.fn(),
  cancelWorkflowTask: vi.fn(),
  resumeTaskWorkflow: vi.fn(),
}))
vi.mock("@/lib/tasks", async (importOriginal) => ({
  ...await importOriginal<typeof import("@/lib/tasks")>(),
  ...api,
}))

const target = remoteTarget

beforeEach(() => {
  vi.clearAllMocks()
  api.getTaskWorkflow.mockResolvedValue(customerView)
  api.moveTaskWorkflow.mockResolvedValue(workflowTask)
  api.cancelWorkflowTask.mockResolvedValue(workflowTask)
})

function renderPanel(task = workflowTask) {
  const onTaskChanged = vi.fn()
  const view = render(<WorkflowPanel task={task} target={target} onTaskChanged={onTaskChanged} />)
  return { onTaskChanged, ...view }
}

it("renders the header, outcomes, artifacts, runs and visits of the view", async () => {
  renderPanel()
  expect(await screen.findByText("release@1.2.0")).toBeInTheDocument()
  expect(api.getTaskWorkflow).toHaveBeenCalledWith("REL-1", target)
  const header = screen.getByText("release@1.2.0").parentElement!
  expect(header).toHaveTextContent("review")
  expect(header).toHaveTextContent("customer")
  expect(screen.getByRole("button", { name: "approve" })).toBeInTheDocument()
  expect(screen.getByText("Artifacts").closest("section")).toHaveTextContent("# Notes")
  expect(screen.getByText("Script runs").closest("section")).toHaveTextContent("./scripts/ci.sh")
  const visits = screen.getByText("Visits").closest("section")!
  expect(visits).toHaveTextContent("draft")
  expect(visits).toHaveTextContent("drafted")
  expect(visits).toHaveTextContent("first cut")
  expect(within(visits).getAllByRole("listitem")).toHaveLength(2)
  expect(screen.queryByText(/paused/i)).not.toBeInTheDocument()
})

it("names the pool and the holder for a pool status", async () => {
  api.getTaskWorkflow.mockResolvedValue(poolView)
  renderPanel({ ...workflowTask, status: "draft", category: "in_progress", waiting_on: undefined })
  const header = (await screen.findByText("release@1.2.0")).parentElement!
  expect(header).toHaveTextContent("pool:writers")
  expect(header).toHaveTextContent("agent:writer")
})

it("shows the pause banner for a paused task", async () => {
  api.getTaskWorkflow.mockResolvedValue({ ...poolView, waiting_on: "pause", paused_reason: "holder_unavailable" })
  renderPanel({ ...workflowTask, waiting_on: "pause", workflow_paused_reason: "holder_unavailable" })
  expect(await screen.findByText(/holder of this status cannot work/i)).toBeInTheDocument()
  expect(screen.getByRole("button", { name: "Release holder" })).toBeInTheDocument()
})

it("shows a failed view request with a retry", async () => {
  api.getTaskWorkflow.mockRejectedValueOnce(new ApiError(503, "workflow_unavailable", "the image store is not available"))
  renderPanel()
  expect(await screen.findByRole("alert")).toHaveTextContent("the image store is not available")
  await act(async () => { fireEvent.click(screen.getByRole("button", { name: "Retry" })) })
  expect(await screen.findByText("release@1.2.0")).toBeInTheDocument()
  expect(screen.queryByRole("alert")).not.toBeInTheDocument()
})

it("refetches the view when the task's revision changes", async () => {
  const { rerender, onTaskChanged } = renderPanel()
  await screen.findByText("release@1.2.0")
  rerender(<WorkflowPanel task={{ ...workflowTask }} target={target} onTaskChanged={onTaskChanged} />)
  expect(api.getTaskWorkflow).toHaveBeenCalledTimes(1)
  rerender(<WorkflowPanel task={{ ...workflowTask, revision: 5 }} target={target} onTaskChanged={onTaskChanged} />)
  await waitFor(() => expect(api.getTaskWorkflow).toHaveBeenCalledTimes(2))
})

it("refetches the view and reports the change after an outcome applies", async () => {
  api.advanceTask.mockResolvedValue({ id: 1, task_key: "REL-1", outcome: "rework", actor: "user:owner", state: "applied", created_at: "" })
  const { onTaskChanged } = renderPanel()
  const rework = await screen.findByRole("button", { name: "rework" })
  await act(async () => { fireEvent.click(rework) })
  expect(onTaskChanged).toHaveBeenCalledTimes(1)
  expect(api.getTaskWorkflow).toHaveBeenCalledTimes(2)
})

async function openMenuItem(name: string) {
  await userEvent.click(await screen.findByRole("button", { name: "Workflow actions" }))
  await userEvent.click(await screen.findByRole("menuitem", { name }))
}

it("moves to a known status with a required reason after confirmation", async () => {
  const { onTaskChanged } = renderPanel()
  await openMenuItem("Move to status…")
  const select = screen.getByLabelText("Target status")
  expect(within(select).getAllByRole("option").map((option) => option.getAttribute("value")))
    .toEqual(["", "draft", "review", "publish"])
  await userEvent.selectOptions(select, "draft")
  const move = screen.getByRole("button", { name: "Move" })
  expect(move).toBeDisabled()
  await userEvent.type(screen.getByLabelText("Reason"), "found a regression")
  await userEvent.click(move)
  const dialog = await screen.findByRole("alertdialog")
  await userEvent.click(within(dialog).getByRole("button", { name: "Move task" }))
  await waitFor(() => expect(api.moveTaskWorkflow).toHaveBeenCalledWith("REL-1", "draft", "found a regression", target))
  await waitFor(() => expect(onTaskChanged).toHaveBeenCalledTimes(1))
  expect(api.getTaskWorkflow).toHaveBeenCalledTimes(2)
})

it("shows a refused move inline", async () => {
  api.moveTaskWorkflow.mockRejectedValue(new ApiError(400, "status_unknown", "the image does not declare status x"))
  const { onTaskChanged } = renderPanel()
  await openMenuItem("Move to status…")
  await userEvent.selectOptions(screen.getByLabelText("Target status"), "publish")
  await userEvent.type(screen.getByLabelText("Reason"), "why not")
  await userEvent.click(screen.getByRole("button", { name: "Move" }))
  await userEvent.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: "Move task" }))
  expect(await screen.findByRole("alert")).toHaveTextContent("the image does not declare status x")
  expect(onTaskChanged).not.toHaveBeenCalled()
})

it("cancels the task from the menu after confirmation, and not when declined", async () => {
  const { onTaskChanged } = renderPanel()
  await openMenuItem("Cancel task")
  await userEvent.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: "Go back" }))
  expect(api.cancelWorkflowTask).not.toHaveBeenCalled()
  await openMenuItem("Cancel task")
  await userEvent.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: "Cancel task" }))
  await waitFor(() => expect(api.cancelWorkflowTask).toHaveBeenCalledWith("REL-1", target))
  await waitFor(() => expect(onTaskChanged).toHaveBeenCalledTimes(1))
})

it("offers no cancel for a closed task", async () => {
  api.getTaskWorkflow.mockResolvedValue({ ...customerView, status: "publish", category: "done", owner: "", outcomes: [] })
  renderPanel({ ...workflowTask, status: "publish", category: "done", waiting_on: undefined })
  await userEvent.click(await screen.findByRole("button", { name: "Workflow actions" }))
  expect(await screen.findByRole("menuitem", { name: "Move to status…" })).toBeInTheDocument()
  expect(screen.queryByRole("menuitem", { name: "Cancel task" })).not.toBeInTheDocument()
})
