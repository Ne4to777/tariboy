import { fireEvent, render, screen, waitFor, within } from "@testing-library/react"
import { MemoryRouter, Route, Routes } from "react-router-dom"
import { beforeEach, expect, it, vi } from "vitest"
import { ApiError } from "@/lib/api"
import type { WorkflowManifest } from "@/lib/tasks"
import WorkflowImageDetail from "./WorkflowImageDetail"

const api = vi.hoisted(() => ({
  getWorkflowImage: vi.fn(), removeWorkflowImage: vi.fn(), listTaskQueues: vi.fn(), getQueueWorkflow: vi.fn(),
}))
vi.mock("@/lib/tasks", async (importOriginal) => ({ ...await importOriginal<typeof import("@/lib/tasks")>(), ...api }))
vi.mock("@/components/DaemonProvider", () => ({ useOptionalDaemons: () => ({ daemons: [] }) }))

const remote = { id: "remote", label: "Remote", baseURL: "https://remote", token: "t" }
const digest = "c".repeat(64)
const manifest: WorkflowManifest = {
  schema_version: 1, name: "development", version: "0.1.0", digest, built_at: "2026-10-01T00:00:00Z",
  definition: {
    schema_version: 1, name: "development", workflow_version: "0.1.0", initial_status: "plan",
    requires_secrets: ["GH_TOKEN"], env: { PR_POLL_SECONDS: "60" }, limits: { idle_iterations: 4 },
    artifacts: [{ name: "plan", description: "The plan, as Markdown." }],
    statuses: [
      { id: "plan", owner: { kind: "pool", pool: "developers" }, instructions: "./statuses/plan.md",
        transitions: [{ on: "planned", to: "review", requires: ["plan"], checks: [{ script: "./scripts/ok.sh" }, { script: "./scripts/slow.sh", run_as: "agent", timeout: "5m" }] }] },
      { id: "review", owner: { kind: "script" }, watch: { script: "./scripts/watch.py", every: "60s" }, limits: { rejected_requests: 9 },
        transitions: [{ on: "merged", to: "done" }, { on: "dropped", to: "cancelled" }] },
      { id: "done", owner: { kind: "" }, terminal: true },
      { id: "cancelled", owner: { kind: "" }, terminal: true, cancelled: true },
    ],
  },
  files: [{ path: "scripts/ok.sh", sha256: "f".repeat(64), executable: true, size: 42 }],
}

beforeEach(() => {
  vi.clearAllMocks()
  api.getWorkflowImage.mockResolvedValue(manifest)
  api.listTaskQueues.mockResolvedValue({ queues: [{ prefix: "DEV" }], count: 1 })
  api.getQueueWorkflow.mockResolvedValue({ queue: "DEV", digest })
})

function renderDetail(tag = "0.1.0") {
  return render(<MemoryRouter initialEntries={[`/w/development/${tag}`]}>
    <Routes>
      <Route path="/w/:name/:tag" element={<WorkflowImageDetail target={remote} name="development" tag={tag} basePath="/w" />} />
      <Route path="/w" element={<p>list page</p>} />
    </Routes>
  </MemoryRouter>)
}

it("shows the status graph, limits, and declared parts of the manifest", async () => {
  renderDetail()
  const statuses = await screen.findByRole("table", { name: "Statuses" })
  expect(api.getWorkflowImage).toHaveBeenCalledWith("development", "0.1.0", remote)
  const rows = within(statuses).getAllByRole("row").slice(1)
  expect(rows[0]).toHaveTextContent("plan")
  expect(rows[0]).toHaveTextContent("initial")
  expect(rows[0]).toHaveTextContent("pool:developers")
  expect(rows[0]).toHaveTextContent("./statuses/plan.md")
  expect(rows[0]).toHaveTextContent("planned → review")
  expect(rows[0]).toHaveTextContent("requires plan")
  expect(rows[0]).toHaveTextContent("./scripts/ok.sh as queue, 60s")
  expect(rows[0]).toHaveTextContent("./scripts/slow.sh as agent, 5m")
  expect(rows[1]).toHaveTextContent("script")
  expect(rows[1]).toHaveTextContent("./scripts/watch.py every 60s, timeout 60s")
  expect(rows[2]).toHaveTextContent("terminal")
  expect(rows[3]).toHaveTextContent("cancelled")

  const limits = screen.getByRole("table", { name: "Limits" })
  expect(within(limits).getByRole("row", { name: /^review/ })).toHaveTextContent("9 (status)")
  expect(within(limits).getByRole("row", { name: /^workflow/ })).toHaveTextContent("4 (workflow)")
  expect(within(limits).queryByRole("row", { name: /^done/ })).toBeNull()

  expect(screen.getByText(/The plan, as Markdown\./)).toBeInTheDocument()
  expect(screen.getByText("GH_TOKEN")).toBeInTheDocument()
  expect(screen.getByText("PR_POLL_SECONDS=60")).toBeInTheDocument()
  expect(screen.getByText("scripts/ok.sh")).toHaveAttribute("title", "f".repeat(64))
  await waitFor(() => expect(screen.getByTestId("bound-to")).toHaveTextContent("DEV"))
})

it("removes nothing when the confirmation is declined", async () => {
  renderDetail()
  fireEvent.click(await screen.findByRole("button", { name: "Remove tag" }))
  fireEvent.click(await screen.findByRole("button", { name: "Go back" }))
  await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull())
  expect(api.removeWorkflowImage).not.toHaveBeenCalled()
})

it("removes the tag on the target and returns to the list", async () => {
  api.removeWorkflowImage.mockResolvedValue({ removed: true })
  renderDetail()
  fireEvent.click(await screen.findByRole("button", { name: "Remove tag" }))
  fireEvent.click(await screen.findByRole("button", { name: "Remove development:0.1.0" }))
  expect(await screen.findByText("list page")).toBeInTheDocument()
  expect(api.removeWorkflowImage).toHaveBeenCalledWith("development", "0.1.0", remote)
})

it("explains workflow_in_use inline with the bound queues", async () => {
  api.removeWorkflowImage.mockRejectedValue(new ApiError(409, "workflow_in_use", "workflow image is in use: 1 queue is bound to it"))
  renderDetail()
  await waitFor(() => expect(screen.getByTestId("bound-to")).toHaveTextContent("DEV"))
  fireEvent.click(screen.getByRole("button", { name: "Remove tag" }))
  fireEvent.click(await screen.findByRole("button", { name: "Remove development:0.1.0" }))
  const alert = await screen.findByRole("alert")
  expect(alert).toHaveTextContent("workflow image is in use: 1 queue is bound to it")
  expect(alert).toHaveTextContent("Bound to: DEV")
})

it("hides Remove tag for a page opened by digest and opens the upload dialog", async () => {
  renderDetail(digest)
  fireEvent.click(await screen.findByRole("button", { name: "Upload to servers" }))
  expect(screen.queryByRole("button", { name: "Remove tag" })).toBeNull()
  expect(await screen.findByText(`Upload workflow development:${digest}`)).toBeInTheDocument()
})

it("shows a load failure", async () => {
  api.getWorkflowImage.mockRejectedValue(new ApiError(404, "not_found", "workflow image not found"))
  renderDetail()
  expect(await screen.findByRole("alert")).toHaveTextContent("workflow image not found")
})
