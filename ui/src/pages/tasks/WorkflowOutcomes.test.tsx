import { act, fireEvent, render, screen, within } from "@testing-library/react"
import { afterEach, beforeEach, expect, it, vi } from "vitest"
import { ApiError } from "@/lib/api"
import type { TransitionRequest } from "@/lib/tasks"
import WorkflowOutcomes from "./WorkflowOutcomes"
import { customerView, poolView, remoteTarget } from "./workflowFixtures"

const api = vi.hoisted(() => ({ advanceTask: vi.fn(), getTransitionRequest: vi.fn() }))
vi.mock("@/lib/tasks", async (importOriginal) => ({
  ...await importOriginal<typeof import("@/lib/tasks")>(),
  ...api,
}))

const target = remoteTarget
const request = (state: TransitionRequest["state"], extra: Partial<TransitionRequest> = {}): TransitionRequest => ({
  id: 11, task_key: "REL-1", outcome: "approve", actor: "user:owner", state, created_at: "2026-10-01T10:06:00Z", ...extra,
})

beforeEach(() => { vi.clearAllMocks() })
afterEach(() => { vi.useRealTimers() })

function renderOutcomesWithUnmount(view = customerView) {
  const onChanged = vi.fn()
  const onRefresh = vi.fn()
  const { unmount } = render(<WorkflowOutcomes taskKey="REL-1" view={view} target={target} onChanged={onChanged} onRefresh={onRefresh} />)
  return { onChanged, onRefresh, unmount }
}

function renderOutcomes(view = customerView) {
  const onChanged = vi.fn()
  const onRefresh = vi.fn()
  render(<WorkflowOutcomes taskKey="REL-1" view={view} target={target} onChanged={onChanged} onRefresh={onRefresh} />)
  return { onChanged, onRefresh }
}

it("lists a pool status's outcomes read-only, with missing artifacts highlighted and the checks named", () => {
  renderOutcomes(poolView)
  expect(screen.queryByRole("button", { name: /drafted/ })).not.toBeInTheDocument()
  expect(screen.queryByLabelText("Outcome message")).not.toBeInTheDocument()
  const row = screen.getByText("drafted").closest("li")!
  expect(within(row).getByText("review")).toBeInTheDocument()
  expect(within(row).getByText("notes")).not.toHaveAttribute("data-missing")
  expect(within(row).getByText("summary")).toHaveAttribute("data-missing", "true")
  expect(within(row).getByText("./checks/notes.sh")).toBeInTheDocument()
  expect(screen.getByText(/agent decides/i)).toBeInTheDocument()
})

it("offers one button per outcome and a message field for a customer status", () => {
  renderOutcomes()
  expect(screen.getByRole("button", { name: "approve" })).toBeEnabled()
  expect(screen.getByRole("button", { name: "rework" })).toBeEnabled()
  expect(screen.getByLabelText("Outcome message")).toBeInTheDocument()
})

it("advances from the shown status and reports the change when it applies at once", async () => {
  api.advanceTask.mockResolvedValue(request("applied", { outcome: "rework" }))
  const { onChanged } = renderOutcomes()
  fireEvent.change(screen.getByLabelText("Outcome message"), { target: { value: "needs more" } })
  await act(async () => { fireEvent.click(screen.getByRole("button", { name: "rework" })) })
  expect(api.advanceTask).toHaveBeenCalledWith("REL-1", "rework", "needs more", "review", target)
  expect(onChanged).toHaveBeenCalledTimes(1)
  expect(api.getTransitionRequest).not.toHaveBeenCalled()
})

it("polls a pending request every second with the buttons disabled until it applies", async () => {
  vi.useFakeTimers()
  api.advanceTask.mockResolvedValue(request("pending", { wait_seconds: 30 }))
  api.getTransitionRequest
    .mockResolvedValueOnce(request("pending"))
    .mockResolvedValueOnce(request("applied"))
  const { onChanged } = renderOutcomes()
  await act(async () => { fireEvent.click(screen.getByRole("button", { name: "approve" })) })
  expect(screen.getByText("checking…")).toBeInTheDocument()
  expect(screen.getByRole("button", { name: "rework" })).toBeDisabled()
  await act(async () => { await vi.advanceTimersByTimeAsync(1000) })
  expect(api.getTransitionRequest).toHaveBeenCalledWith("REL-1", 11, target)
  expect(onChanged).not.toHaveBeenCalled()
  await act(async () => { await vi.advanceTimersByTimeAsync(1000) })
  expect(api.getTransitionRequest).toHaveBeenCalledTimes(2)
  expect(onChanged).toHaveBeenCalledTimes(1)
  expect(screen.queryByText("checking…")).not.toBeInTheDocument()
})

it("shows a rejection with the script's message as text and re-enables the buttons", async () => {
  vi.useFakeTimers()
  api.advanceTask.mockResolvedValue(request("pending", { wait_seconds: 30 }))
  api.getTransitionRequest.mockResolvedValue(request("rejected", { result_message: "<b>no heading</b>" }))
  const { onChanged, onRefresh } = renderOutcomes()
  await act(async () => { fireEvent.click(screen.getByRole("button", { name: "approve" })) })
  await act(async () => { await vi.advanceTimersByTimeAsync(1000) })
  const notice = screen.getByRole("alert")
  expect(notice).toHaveTextContent("Rejected")
  expect(within(notice).getByText("<b>no heading</b>")).toBeInTheDocument()
  expect(notice.querySelector("b")).toBeNull()
  expect(screen.getByRole("button", { name: "approve" })).toBeEnabled()
  expect(onChanged).not.toHaveBeenCalled()
  expect(onRefresh).toHaveBeenCalled()
})

it("stops waiting after wait_seconds and offers a refresh", async () => {
  vi.useFakeTimers()
  api.advanceTask.mockResolvedValue(request("pending", { wait_seconds: 2 }))
  api.getTransitionRequest.mockResolvedValue(request("pending"))
  const { onChanged, onRefresh } = renderOutcomes()
  await act(async () => { fireEvent.click(screen.getByRole("button", { name: "approve" })) })
  await act(async () => { await vi.advanceTimersByTimeAsync(5000) })
  expect(api.getTransitionRequest).toHaveBeenCalledTimes(2)
  expect(screen.getByText(/still checking/i)).toBeInTheDocument()
  expect(screen.getByRole("button", { name: "approve" })).toBeEnabled()
  expect(onChanged).not.toHaveBeenCalled()
  fireEvent.click(screen.getByRole("button", { name: "Refresh" }))
  expect(onRefresh).toHaveBeenCalled()
})

it("refetches the view and shows the message when the status changed meanwhile", async () => {
  api.advanceTask.mockRejectedValue(new ApiError(409, "status_changed", "the task is in status publish"))
  const { onRefresh } = renderOutcomes()
  await act(async () => { fireEvent.click(screen.getByRole("button", { name: "approve" })) })
  expect(onRefresh).toHaveBeenCalled()
  expect(screen.getByRole("alert")).toHaveTextContent("the task is in status publish")
})

it("shows the last request's failure from the view", () => {
  renderOutcomes({ ...customerView, last_request: request("failed", { result_message: "check timed out\nlog: /x/run.log" }) })
  const notice = screen.getByRole("alert")
  expect(notice).toHaveTextContent("Failed")
  expect(notice).toHaveTextContent("check timed out")
})

it("refetches the view when another request is still pending, and when the task is paused or lacks an artifact", async () => {
  for (const code of ["transition_pending", "workflow_paused", "artifact_missing"]) {
    api.advanceTask.mockRejectedValueOnce(new ApiError(409, code, `refused: ${code}`))
    const { onRefresh, unmount } = renderOutcomesWithUnmount()
    await act(async () => { fireEvent.click(screen.getByRole("button", { name: "approve" })) })
    expect(onRefresh).toHaveBeenCalledTimes(1)
    expect(screen.getByRole("alert")).toHaveTextContent(`refused: ${code}`)
    unmount()
  }
})

it("stops polling and sets no state once unmounted", async () => {
  vi.useFakeTimers()
  const errors = vi.spyOn(console, "error").mockImplementation(() => {})
  api.advanceTask.mockResolvedValue(request("pending", { wait_seconds: 30 }))
  api.getTransitionRequest.mockResolvedValue(request("pending"))
  const { onChanged, onRefresh, unmount } = renderOutcomesWithUnmount()
  await act(async () => { fireEvent.click(screen.getByRole("button", { name: "approve" })) })
  unmount()
  await act(async () => { await vi.advanceTimersByTimeAsync(5000) })
  expect(api.getTransitionRequest).not.toHaveBeenCalled()
  expect(onChanged).not.toHaveBeenCalled()
  expect(onRefresh).not.toHaveBeenCalled()
  expect(errors).not.toHaveBeenCalled()
  errors.mockRestore()
})

it("labels a failed poll as an unreadable request, not a refusal", async () => {
  vi.useFakeTimers()
  api.advanceTask.mockResolvedValue(request("pending", { wait_seconds: 30 }))
  api.getTransitionRequest.mockRejectedValue(new Error("network down"))
  renderOutcomes()
  await act(async () => { fireEvent.click(screen.getByRole("button", { name: "approve" })) })
  await act(async () => { await vi.advanceTimersByTimeAsync(1000) })
  const notice = screen.getByRole("alert")
  expect(notice).toHaveTextContent("Could not read the request")
  expect(notice).toHaveTextContent("network down")
  expect(notice).not.toHaveTextContent("Refused")
})

it("drops its notice when the view moves to another status", async () => {
  api.advanceTask.mockRejectedValue(new ApiError(409, "status_changed", "the task is in status publish"))
  const onChanged = vi.fn()
  const onRefresh = vi.fn()
  const { rerender } = render(<WorkflowOutcomes taskKey="REL-1" view={customerView} target={target} onChanged={onChanged} onRefresh={onRefresh} />)
  await act(async () => { fireEvent.click(screen.getByRole("button", { name: "approve" })) })
  expect(screen.getByRole("alert")).toBeInTheDocument()
  rerender(<WorkflowOutcomes taskKey="REL-1" view={{ ...customerView, status: "publish", outcomes: [] }} target={target}
    onChanged={onChanged} onRefresh={onRefresh} />)
  expect(screen.queryByRole("alert")).not.toBeInTheDocument()
})

it("offers a refresh while a request from elsewhere is pending", () => {
  const { onRefresh } = renderOutcomes({ ...customerView, last_request: request("pending", { wait_seconds: 60 }) })
  expect(screen.getByText("checking…")).toBeInTheDocument()
  expect(screen.getByRole("button", { name: "approve" })).toBeDisabled()
  fireEvent.click(screen.getByRole("button", { name: "Refresh" }))
  expect(onRefresh).toHaveBeenCalled()
})

it("says what a disabled outcome still needs", () => {
  renderOutcomes({ ...customerView, outcomes: [{ on: "approve", to: "publish", requires: ["notes"], missing: ["notes"] }] })
  expect(screen.getByRole("button", { name: "approve" })).toBeDisabled()
  expect(screen.getByText("needs: notes")).toBeInTheDocument()
})
