import { fireEvent, render, screen, waitFor } from "@testing-library/react"
import { beforeEach, expect, it, vi } from "vitest"
import { ApiError } from "@/lib/api"
import QueueSettings from "./QueueSettings"
import { remoteTarget } from "./workflowFixtures"

const api = vi.hoisted(() => ({
  getQueueWorkflow: vi.fn(), listWorkflowImages: vi.fn(), listQueueSecrets: vi.fn(), listAgentPools: vi.fn(),
  setQueueWorkflow: vi.fn(), rebindAgentPool: vi.fn(),
}))
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }))
vi.mock("@/lib/tasks", async (importOriginal) => ({ ...await importOriginal<typeof import("@/lib/tasks")>(), ...api }))

const queue = { prefix: "REL", name: "Release", description: "", owners: [], responsible_agent: "", revision: 1 }

beforeEach(() => {
  vi.resetAllMocks()
  api.getQueueWorkflow.mockResolvedValue(null)
  api.listWorkflowImages.mockResolvedValue([])
  api.listQueueSecrets.mockResolvedValue([])
  api.listAgentPools.mockResolvedValue({ items: [] })
})

it("clears Pools needed once the real pools editor saves members for the pool", async () => {
  api.listWorkflowImages.mockResolvedValue([{ name: "release", tag: "latest", version: "1", digest: "d", built_at: "" }])
  api.setQueueWorkflow.mockRejectedValue(new ApiError(409, "workflow_pool_empty", "workflow pools are missing or have no agents: dev", { pools: ["dev"] }))
  api.rebindAgentPool.mockResolvedValue({ id: 1, queue: "REL", name: "dev", agents: ["dev-a"], revision: 1, created_at: "", updated_at: "" })
  render(<QueueSettings queues={[queue as never]} onCreate={vi.fn()} onUpdate={vi.fn()} target={remoteTarget} />)
  fireEvent.click(screen.getByRole("button", { name: "Workflow REL" }))
  await screen.findByRole("option", { name: "release:latest" })
  fireEvent.change(screen.getByLabelText("Workflow image"), { target: { value: "release:latest" } })
  fireEvent.click(screen.getByRole("button", { name: "Bind" }))
  await screen.findByRole("alert", { name: "Pools needed" })
  fireEvent.change(await screen.findByLabelText("Pool name REL"), { target: { value: "dev" } })
  fireEvent.change(screen.getByLabelText("Pool agents REL"), { target: { value: "dev-a" } })
  fireEvent.click(screen.getByRole("button", { name: "Save pool" }))
  await waitFor(() => expect(screen.queryByRole("alert", { name: "Pools needed" })).not.toBeInTheDocument())
  expect(api.rebindAgentPool).toHaveBeenCalledWith("REL", "dev", ["dev-a"], 0, expect.any(String), remoteTarget)
})

it("opens the workflow section with the pools editor and secrets, carrying the target", async () => {
  render(<QueueSettings queues={[queue as never]} onCreate={vi.fn()} onUpdate={vi.fn()} target={remoteTarget} />)
  expect(screen.queryByText("No workflow")).not.toBeInTheDocument()
  fireEvent.click(screen.getByRole("button", { name: "Workflow REL" }))
  expect(await screen.findByText("No workflow")).toBeInTheDocument()
  expect(await screen.findByLabelText("Pool name REL")).toBeInTheDocument()
  expect(screen.getByLabelText("Secret key")).toBeInTheDocument()
  expect(api.getQueueWorkflow).toHaveBeenCalledWith("REL", remoteTarget)
  expect(api.listAgentPools).toHaveBeenCalledWith("REL", remoteTarget)
})
