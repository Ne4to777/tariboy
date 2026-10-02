import { render, screen, waitFor, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { beforeEach, expect, it, vi } from "vitest"
import type { Task, TaskDetail as Detail } from "@/lib/tasks"
import TaskDetail from "./TaskDetail"
import { customerView, poolView, workflowTask } from "./workflowFixtures"

const api = vi.hoisted(() => ({ getTaskWorkflow: vi.fn(), resumeTaskWorkflow: vi.fn() }))
vi.mock("@/lib/tasks", async (importOriginal) => ({
  ...await importOriginal<typeof import("@/lib/tasks")>(),
  ...api,
}))

const flexible: Task = {
  ...workflowTask,
  key: "TEST-1",
  queue: "TEST",
  title: "Flexible task",
  status: "in_progress",
  category: "in_progress",
  waiting_on: undefined,
  workflow_name: undefined,
  workflow_version: undefined,
  workflow_digest: undefined,
  assignee: "agent:worker",
}

beforeEach(() => {
  vi.clearAllMocks()
  api.getTaskWorkflow.mockResolvedValue(customerView)
})

function renderDetail(task: Task) {
  const detail: Detail = { task, comments: [], waiting_for: [], relations: [] }
  const onSave = vi.fn().mockResolvedValue(task)
  const onTaskChanged = vi.fn()
  const onClose = vi.fn()
  render(<TaskDetail detail={detail} events={[]} principals={null} width={600} resizeHandle={null}
    onClose={onClose} onSave={onSave} onComment={vi.fn()} onAddRelation={vi.fn()} onDeleteRelation={vi.fn()}
    onTransfer={vi.fn()} onTaskChanged={onTaskChanged} />)
  return { onSave, onTaskChanged, onClose }
}

it("keeps a flexible task's status select, fields and sections as they were", async () => {
  const { onSave } = renderDetail(flexible)
  const status = screen.getByLabelText("Status")
  expect(within(status).getAllByRole("option").map((option) => option.textContent))
    .toEqual(["Open", "In progress", "Wait customer", "Done", "Cancelled"])
  expect(status).toHaveValue("in_progress")
  for (const label of ["Title", "Priority", "Assignee", "Pull request", "Manual block reason"]) {
    expect(screen.getByLabelText(label)).toBeInTheDocument()
  }
  expect(screen.getByText("Dependencies")).toBeInTheDocument()
  expect(screen.getByText("History")).toBeInTheDocument()
  expect(screen.getByRole("button", { name: "Save task" })).toBeInTheDocument()
  expect(screen.queryByText("release@1.2.0")).not.toBeInTheDocument()
  expect(api.getTaskWorkflow).not.toHaveBeenCalled()
  await userEvent.selectOptions(status, "done")
  await userEvent.click(screen.getByRole("button", { name: "Save task" }))
  expect(onSave).toHaveBeenCalledWith(expect.objectContaining({ status: "done" }))
})

it("no longer shows the old unmanaged line for either kind of task", () => {
  renderDetail(flexible)
  expect(screen.queryByText(/unmanaged/)).not.toBeInTheDocument()
})

it("replaces the status select with the workflow panel for a workflow task", async () => {
  const { onSave } = renderDetail(workflowTask)
  expect(await screen.findByText("release@1.2.0")).toBeInTheDocument()
  expect(screen.queryByLabelText("Status")).not.toBeInTheDocument()
  expect(screen.queryByText(/unmanaged/)).not.toBeInTheDocument()
  expect(screen.getByLabelText("Title")).toBeInTheDocument()
  expect(screen.getByLabelText("Priority")).toBeInTheDocument()
  expect(screen.getByLabelText("Pull request")).toBeInTheDocument()
  expect(screen.getByText("Dependencies")).toBeInTheDocument()
  await userEvent.clear(screen.getByLabelText("Title"))
  await userEvent.type(screen.getByLabelText("Title"), "Renamed")
  await userEvent.click(screen.getByRole("button", { name: "Save task" }))
  expect(onSave).toHaveBeenCalledWith(expect.objectContaining({ title: "Renamed", status: undefined }))
})

it("confirms a pause decision inside the open sheet without closing it", async () => {
  api.getTaskWorkflow.mockResolvedValue({ ...poolView, waiting_on: "pause", paused_reason: "idle_iterations" })
  api.resumeTaskWorkflow.mockResolvedValue(workflowTask)
  const { onTaskChanged, onClose } = renderDetail({ ...workflowTask, waiting_on: "pause" })
  await userEvent.click(await screen.findByRole("button", { name: "Continue" }))
  await userEvent.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: "Continue" }))
  await waitFor(() => expect(api.resumeTaskWorkflow).toHaveBeenCalledWith("REL-1", "continue", undefined))
  await waitFor(() => expect(onTaskChanged).toHaveBeenCalled())
  expect(onClose).not.toHaveBeenCalled()
  expect(screen.getByText("Release notes")).toBeInTheDocument()
})
