import { act, fireEvent, render, screen, within } from "@testing-library/react"
import { beforeEach, expect, it, vi } from "vitest"
import { ApiError } from "@/lib/api"
import type { WorkflowArtifact } from "@/lib/tasks"
import WorkflowArtifacts from "./WorkflowArtifacts"
import { remoteTarget } from "./workflowFixtures"

const api = vi.hoisted(() => ({ setTaskArtifact: vi.fn(), getTaskArtifactHistory: vi.fn() }))
vi.mock("@/lib/tasks", async (importOriginal) => ({
  ...await importOriginal<typeof import("@/lib/tasks")>(),
  ...api,
}))

const target = remoteTarget
const notes: WorkflowArtifact = {
  id: 3, name: "notes", value: "<i>one</i>\n2\n3\n4\n5\n6\n7\n8", author: "agent:writer", created_at: "2026-10-01T10:04:00Z",
}

beforeEach(() => { vi.clearAllMocks() })

function renderArtifacts(props: Partial<Parameters<typeof WorkflowArtifacts>[0]> = {}) {
  const onChanged = vi.fn()
  render(<WorkflowArtifacts taskKey="REL-1" artifacts={[notes]} missing={["summary"]} editable target={target}
    onChanged={onChanged} {...props} />)
  return { onChanged }
}

it("shows each artifact with its author and a long value collapsed to six lines, as text", () => {
  renderArtifacts()
  const item = screen.getByText("notes").closest("li")!
  expect(within(item).getByText("agent:writer")).toBeInTheDocument()
  const value = item.querySelector("pre")!
  expect(value.textContent).toBe("<i>one</i>\n2\n3\n4\n5\n6")
  expect(value.querySelector("i")).toBeNull()
  const expand = within(item).getByRole("button", { name: "Show all 8 lines" })
  expect(expand).toHaveAttribute("aria-expanded", "false")
  fireEvent.click(expand)
  expect(item.querySelector("pre")!.textContent).toBe(notes.value)
  expect(within(item).getByRole("button", { name: "Show less" })).toHaveAttribute("aria-expanded", "true")
})

it("lists a required artifact that has no value yet", () => {
  renderArtifacts()
  const item = screen.getByText("summary").closest("li")!
  expect(within(item).getByText("no value")).toBeInTheDocument()
})

it("loads the history only when asked", async () => {
  api.getTaskArtifactHistory.mockResolvedValue([
    notes, { ...notes, id: 2, value: "older value", author: "user:owner" },
  ])
  renderArtifacts()
  expect(api.getTaskArtifactHistory).not.toHaveBeenCalled()
  const item = screen.getByText("notes").closest("li")!
  await act(async () => { fireEvent.click(within(item).getByRole("button", { name: "History" })) })
  expect(api.getTaskArtifactHistory).toHaveBeenCalledWith("REL-1", "notes", target)
  expect(within(item).getByText("older value")).toBeInTheDocument()
})

it("saves an edited value and reports the change", async () => {
  api.setTaskArtifact.mockResolvedValue({ ...notes, value: "new" })
  const { onChanged } = renderArtifacts()
  const item = screen.getByText("notes").closest("li")!
  fireEvent.click(within(item).getByRole("button", { name: "Edit" }))
  fireEvent.change(within(item).getByLabelText("Value of notes"), { target: { value: "new" } })
  await act(async () => { fireEvent.click(within(item).getByRole("button", { name: "Save" })) })
  expect(api.setTaskArtifact).toHaveBeenCalledWith("REL-1", "notes", "new", target)
  expect(onChanged).toHaveBeenCalled()
  expect(within(item).queryByLabelText("Value of notes")).not.toBeInTheDocument()
})

it("keeps the editor and shows the daemon's error inline", async () => {
  api.setTaskArtifact.mockRejectedValue(new ApiError(413, "artifact_too_large", "the value exceeds 64 KiB"))
  const { onChanged } = renderArtifacts()
  const item = screen.getByText("summary").closest("li")!
  fireEvent.click(within(item).getByRole("button", { name: "Set" }))
  fireEvent.change(within(item).getByLabelText("Value of summary"), { target: { value: "x" } })
  await act(async () => { fireEvent.click(within(item).getByRole("button", { name: "Save" })) })
  expect(within(item).getByRole("alert")).toHaveTextContent("the value exceeds 64 KiB")
  expect(within(item).getByLabelText("Value of summary")).toBeInTheDocument()
  expect(onChanged).not.toHaveBeenCalled()
})

it("offers no edit action when the task cannot be edited", () => {
  renderArtifacts({ editable: false })
  expect(screen.queryByRole("button", { name: "Edit" })).not.toBeInTheDocument()
  expect(screen.queryByRole("button", { name: "Set" })).not.toBeInTheDocument()
})

it("collapses one very long line by length and keeps every value in a focusable scroll box", async () => {
  const long = "x".repeat(5000)
  api.getTaskArtifactHistory.mockResolvedValue([{ ...notes, id: 2, value: long }])
  renderArtifacts({ artifacts: [{ ...notes, value: long }] })
  const item = screen.getByText("notes").closest("li")!
  const value = item.querySelector("pre")!
  expect(value.textContent).toHaveLength(600)
  expect(value).toHaveClass("max-h-60", "overflow-auto")
  expect(value).toHaveAttribute("tabindex", "0")
  fireEvent.click(within(item).getByRole("button", { name: "Show all 5000 characters" }))
  expect(item.querySelector("pre")!.textContent).toBe(long)
  expect(item.querySelector("pre")).toHaveClass("max-h-60", "overflow-auto")
  await act(async () => { fireEvent.click(within(item).getByRole("button", { name: "History" })) })
  const entry = item.querySelector("ol pre")!
  expect(entry).toHaveClass("max-h-60", "overflow-auto")
  expect(entry).toHaveAttribute("tabindex", "0")
})
