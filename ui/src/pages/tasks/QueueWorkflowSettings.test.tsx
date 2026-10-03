import { fireEvent, render, screen, waitFor } from "@testing-library/react"
import { MemoryRouter, Route, Routes } from "react-router-dom"
import { beforeEach, expect, it, vi } from "vitest"
import { ApiError } from "@/lib/api"
import QueueWorkflowSettings from "./QueueWorkflowSettings"
import { remoteTarget } from "./workflowFixtures"

const api = vi.hoisted(() => ({
  getQueueWorkflow: vi.fn(), setQueueWorkflow: vi.fn(), clearQueueWorkflow: vi.fn(),
  listWorkflowImages: vi.fn(), listQueueSecrets: vi.fn(), setQueueSecret: vi.fn(), removeQueueSecret: vi.fn(),
}))
vi.mock("@/lib/tasks", async (importOriginal) => ({ ...await importOriginal<typeof import("@/lib/tasks")>(), ...api }))

const digest = "0123456789abcdef0123456789abcdef"
const binding = { queue: "REL", name: "release", version: "1.2.0", digest, revision: 3, updated_at: "2026-10-02T10:00:00Z" }
const images = [
  { name: "release", tag: "1.2.0", version: "1.2.0", digest, built_at: "" },
  { name: "release", tag: "latest", version: "1.2.0", digest, built_at: "" },
  { name: "audit", tag: "2", version: "2", digest: "ff", built_at: "" },
]
const pool = (name: string, agents: string[]) => ({ id: 1, queue: "REL", name, agents, revision: 1, created_at: "", updated_at: "" })
/** A stand-in for the pools editor: each button reports one saved pool. */
const renderIt = () => render(<MemoryRouter initialEntries={["/servers/h1/tasks"]}><Routes><Route path="/servers/:hostId/tasks" element={<QueueWorkflowSettings queue="REL" target={remoteTarget} pools={(onPoolSaved) => <div>
  pools editor
  <button type="button" onClick={() => onPoolSaved(pool("dev", ["dev-a"]))}>save dev</button>
  <button type="button" onClick={() => onPoolSaved(pool("qa", []))}>empty qa</button>
  <button type="button" onClick={() => onPoolSaved(pool("qa", ["qa-a"]))}>save qa</button>
</div>} />} /></Routes></MemoryRouter>)

beforeEach(() => {
  vi.resetAllMocks()
  api.listWorkflowImages.mockResolvedValue(images)
  api.listQueueSecrets.mockResolvedValue([])
})

it("renders No workflow when unbound and lists images sorted with latest first", async () => {
  api.getQueueWorkflow.mockResolvedValue(null)
  renderIt()
  expect(await screen.findByText("No workflow")).toBeInTheDocument()
  await screen.findByRole("option", { name: "audit:2" })
  const options = screen.getAllByRole("option").map((option) => option.textContent)
  expect(options).toEqual(["Choose an image", "audit:2", "release:latest", "release:1.2.0"])
  expect(screen.getByRole("button", { name: "Clear" })).toBeDisabled()
  expect(api.getQueueWorkflow).toHaveBeenCalledWith("REL", remoteTarget)
})

it("renders a binding with a short digest and the full digest in a title", async () => {
  api.getQueueWorkflow.mockResolvedValue(binding)
  renderIt()
  const short = await screen.findByText(digest.slice(0, 12))
  expect(short).toHaveAttribute("title", digest)
  expect(screen.getByTestId("binding")).toHaveTextContent("release 1.2.0")
})

it("binds NAME:TAG with revision 0 when unbound", async () => {
  api.getQueueWorkflow.mockResolvedValue(null)
  api.setQueueWorkflow.mockResolvedValue(binding)
  renderIt()
  await screen.findByText("No workflow")
  await screen.findByRole("option", { name: "audit:2" })
  fireEvent.change(screen.getByLabelText("Workflow image"), { target: { value: "release:latest" } })
  fireEvent.click(screen.getByRole("button", { name: "Bind" }))
  await waitFor(() => expect(api.setQueueWorkflow).toHaveBeenCalledWith("REL", "release:latest", 0, remoteTarget))
  expect(await screen.findByText(digest.slice(0, 12))).toBeInTheDocument()
})

it("sends the current revision and reloads on a revision conflict", async () => {
  api.getQueueWorkflow.mockResolvedValueOnce(binding).mockResolvedValue({ ...binding, revision: 4, name: "audit" })
  api.setQueueWorkflow.mockRejectedValue(new ApiError(409, "revision_conflict", "changed", { current_revision: 4 }))
  renderIt()
  await screen.findByText(digest.slice(0, 12))
  await screen.findByRole("option", { name: "audit:2" })
  fireEvent.change(screen.getByLabelText("Workflow image"), { target: { value: "audit:2" } })
  fireEvent.click(screen.getByRole("button", { name: "Bind" }))
  await waitFor(() => expect(api.setQueueWorkflow).toHaveBeenCalledWith("REL", "audit:2", 3, remoteTarget))
  expect(await screen.findByText(/changed elsewhere/i)).toBeInTheDocument()
  expect(api.getQueueWorkflow).toHaveBeenCalledTimes(2)
  expect(screen.getByTestId("binding")).toHaveTextContent("audit")
})

it("lists empty pools next to the pools control", async () => {
  api.getQueueWorkflow.mockResolvedValue(null)
  api.setQueueWorkflow.mockRejectedValue(new ApiError(409, "workflow_pool_empty", "workflow pools are missing or have no agents: dev, qa", { pools: ["dev", "qa"] }))
  renderIt()
  await screen.findByRole("option", { name: "audit:2" })
  fireEvent.change(screen.getByLabelText("Workflow image"), { target: { value: "audit:2" } })
  fireEvent.click(screen.getByRole("button", { name: "Bind" }))
  const alert = await screen.findByRole("alert", { name: "Pools needed" })
  expect(alert).toHaveTextContent("dev")
  expect(alert).toHaveTextContent("qa")
  expect(alert).toHaveTextContent("workflow pools are missing or have no agents")
  expect(screen.getByText("pools editor")).toBeInTheDocument()
})

it("clears Pools needed once the editor saved members for every listed pool", async () => {
  api.getQueueWorkflow.mockResolvedValue(null)
  api.setQueueWorkflow.mockRejectedValue(new ApiError(409, "workflow_pool_empty", "workflow pools are missing or have no agents: dev, qa", { pools: ["dev", "qa"] }))
  renderIt()
  await screen.findByRole("option", { name: "audit:2" })
  fireEvent.change(screen.getByLabelText("Workflow image"), { target: { value: "audit:2" } })
  fireEvent.click(screen.getByRole("button", { name: "Bind" }))
  await screen.findByRole("alert", { name: "Pools needed" })
  fireEvent.click(screen.getByRole("button", { name: "save dev" }))
  // The daemon's message stays; the list names only what is still empty.
  expect(screen.getByRole("alert", { name: "Pools needed" }).querySelector(".font-mono")).toHaveTextContent(/^qa$/)
  fireEvent.click(screen.getByRole("button", { name: "empty qa" }))
  expect(screen.getByRole("alert", { name: "Pools needed" })).toBeInTheDocument()
  fireEvent.click(screen.getByRole("button", { name: "save qa" }))
  expect(screen.queryByRole("alert", { name: "Pools needed" })).not.toBeInTheDocument()
})

it("lists missing secrets next to the secrets control", async () => {
  api.getQueueWorkflow.mockResolvedValue(null)
  api.setQueueWorkflow.mockRejectedValue(new ApiError(409, "workflow_secret_missing", "workflow secrets have no value: TOKEN", { secrets: ["TOKEN"] }))
  renderIt()
  await screen.findByRole("option", { name: "audit:2" })
  fireEvent.change(screen.getByLabelText("Workflow image"), { target: { value: "audit:2" } })
  fireEvent.click(screen.getByRole("button", { name: "Bind" }))
  const alert = await screen.findByRole("alert", { name: "Secrets needed" })
  expect(alert).toHaveTextContent("TOKEN")
})

it("asks before clearing and does nothing when declined", async () => {
  api.getQueueWorkflow.mockResolvedValue(binding)
  api.clearQueueWorkflow.mockResolvedValue(undefined)
  renderIt()
  await screen.findByText(digest.slice(0, 12))
  fireEvent.click(screen.getByRole("button", { name: "Clear" }))
  fireEvent.click(await screen.findByRole("button", { name: "Go back" }))
  expect(api.clearQueueWorkflow).not.toHaveBeenCalled()
  fireEvent.click(screen.getByRole("button", { name: "Clear" }))
  fireEvent.click(await screen.findByRole("button", { name: "Clear binding" }))
  await waitFor(() => expect(api.clearQueueWorkflow).toHaveBeenCalledWith("REL", 3, remoteTarget))
  expect(await screen.findByText("No workflow")).toBeInTheDocument()
})

it("disables Bind and Clear while a clear is running and enables them afterwards", async () => {
  api.getQueueWorkflow.mockResolvedValue(binding)
  let finish: () => void = () => {}
  api.clearQueueWorkflow.mockReturnValue(new Promise<void>((resolve) => { finish = resolve }))
  renderIt()
  await screen.findByText(digest.slice(0, 12))
  await screen.findByRole("option", { name: "audit:2" })
  fireEvent.change(screen.getByLabelText("Workflow image"), { target: { value: "audit:2" } })
  fireEvent.click(screen.getByRole("button", { name: "Clear" }))
  fireEvent.click(await screen.findByRole("button", { name: "Clear binding" }))
  await waitFor(() => expect(screen.getByRole("button", { name: "Bind" })).toBeDisabled())
  expect(screen.getByRole("button", { name: "Clear" })).toBeDisabled()
  finish()
  await waitFor(() => expect(screen.getByRole("button", { name: "Bind" })).toBeEnabled())
})

it("reloads and shows the notice when a clear hits a revision conflict", async () => {
  api.getQueueWorkflow.mockResolvedValueOnce(binding).mockResolvedValue({ ...binding, revision: 4 })
  api.clearQueueWorkflow.mockRejectedValue(new ApiError(409, "revision_conflict", "changed", { current_revision: 4 }))
  renderIt()
  await screen.findByText(digest.slice(0, 12))
  fireEvent.click(screen.getByRole("button", { name: "Clear" }))
  fireEvent.click(await screen.findByRole("button", { name: "Clear binding" }))
  expect(await screen.findByText(/changed elsewhere/i)).toBeInTheDocument()
  expect(api.getQueueWorkflow).toHaveBeenCalledTimes(2)
  expect(screen.getByTestId("binding")).toBeInTheDocument()
})

it("does not claim No workflow when the binding failed to load, and retries", async () => {
  api.getQueueWorkflow.mockRejectedValueOnce(new ApiError(500, "internal", "boom")).mockResolvedValue(binding)
  renderIt()
  expect(await screen.findByRole("alert")).toHaveTextContent("boom")
  expect(screen.queryByText("No workflow")).not.toBeInTheDocument()
  expect(screen.getByRole("button", { name: "Bind" })).toBeDisabled()
  expect(screen.getByRole("button", { name: "Clear" })).toBeDisabled()
  fireEvent.click(screen.getByRole("button", { name: "Retry" }))
  expect(await screen.findByText(digest.slice(0, 12))).toBeInTheDocument()
  expect(screen.queryByText("boom")).not.toBeInTheDocument()
})

it("links the bound image to its detail page by its version tag, else by digest", async () => {
  api.getQueueWorkflow.mockResolvedValue(binding)
  const view = renderIt()
  await waitFor(() => expect(screen.getByRole("link", { name: "release" }))
    .toHaveAttribute("href", "/servers/h1/workflows/release/1.2.0"))
  view.unmount()

  api.listWorkflowImages.mockResolvedValue([{ name: "release", tag: "latest", version: "1.2.0", digest: "other", built_at: "" }])
  renderIt()
  await waitFor(() => expect(screen.getByRole("link", { name: "release" }))
    .toHaveAttribute("href", `/servers/h1/workflows/release/${digest}`))
})
