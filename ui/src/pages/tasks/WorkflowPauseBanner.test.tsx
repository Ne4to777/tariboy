import { act, fireEvent, render, screen, within } from "@testing-library/react"
import { beforeEach, expect, it, vi } from "vitest"
import { ApiError } from "@/lib/api"
import WorkflowPauseBanner from "./WorkflowPauseBanner"
import { remoteTarget } from "./workflowFixtures"

const api = vi.hoisted(() => ({ resumeTaskWorkflow: vi.fn(), cancelWorkflowTask: vi.fn() }))
vi.mock("@/lib/tasks", async (importOriginal) => ({
  ...await importOriginal<typeof import("@/lib/tasks")>(),
  ...api,
}))

const target = remoteTarget

beforeEach(() => {
  vi.clearAllMocks()
  api.resumeTaskWorkflow.mockResolvedValue({})
  api.cancelWorkflowTask.mockResolvedValue({})
})

function renderBanner(reason = "rejected_requests", pool = true) {
  const onChanged = vi.fn()
  render(<WorkflowPauseBanner taskKey="REL-1" reason={reason} pool={pool} target={target} onChanged={onChanged} />)
  return { onChanged }
}

async function confirm(action: string) {
  const dialog = await screen.findByRole("alertdialog")
  await act(async () => { fireEvent.click(within(dialog).getByRole("button", { name: action })) })
}

it("says why the task is paused in words", () => {
  renderBanner("rejected_requests")
  expect(screen.getByRole("alert")).toHaveTextContent(/checks rejected too many transition requests/i)
})

it("shows an unknown reason as it came", () => {
  renderBanner("cosmic_rays")
  expect(screen.getByText(/cosmic_rays/)).toBeInTheDocument()
})

it("offers Release holder only for a pool status", () => {
  renderBanner("script_failures", false)
  expect(screen.getByRole("button", { name: "Continue" })).toBeInTheDocument()
  expect(screen.getByRole("button", { name: "Cancel task" })).toBeInTheDocument()
  expect(screen.queryByRole("button", { name: "Release holder" })).not.toBeInTheDocument()
})

it("continues after confirmation and reports the change", async () => {
  const { onChanged } = renderBanner()
  fireEvent.click(screen.getByRole("button", { name: "Continue" }))
  await confirm("Continue")
  expect(api.resumeTaskWorkflow).toHaveBeenCalledWith("REL-1", "continue", target)
  expect(onChanged).toHaveBeenCalled()
})

it("releases the holder after confirmation", async () => {
  const { onChanged } = renderBanner()
  fireEvent.click(screen.getByRole("button", { name: "Release holder" }))
  await confirm("Release holder")
  expect(api.resumeTaskWorkflow).toHaveBeenCalledWith("REL-1", "release", target)
  expect(onChanged).toHaveBeenCalled()
})

it("cancels the task after confirmation", async () => {
  const { onChanged } = renderBanner()
  fireEvent.click(screen.getByRole("button", { name: "Cancel task" }))
  await confirm("Cancel task")
  expect(api.cancelWorkflowTask).toHaveBeenCalledWith("REL-1", target)
  expect(api.resumeTaskWorkflow).not.toHaveBeenCalled()
  expect(onChanged).toHaveBeenCalled()
})

it("does nothing when the confirmation is declined", async () => {
  const { onChanged } = renderBanner()
  fireEvent.click(screen.getByRole("button", { name: "Cancel task" }))
  await confirm("Go back")
  expect(screen.queryByRole("alertdialog")).not.toBeInTheDocument()
  expect(api.cancelWorkflowTask).not.toHaveBeenCalled()
  expect(onChanged).not.toHaveBeenCalled()
})

it("shows a refused decision inline", async () => {
  api.resumeTaskWorkflow.mockRejectedValue(new ApiError(409, "workflow_not_paused", "the task is not paused"))
  const { onChanged } = renderBanner()
  fireEvent.click(screen.getByRole("button", { name: "Continue" }))
  await confirm("Continue")
  expect(screen.getAllByRole("alert").some((node) => node.textContent === "the task is not paused")).toBe(true)
  expect(onChanged).not.toHaveBeenCalled()
})
