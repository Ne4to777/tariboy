import { fireEvent, render, screen } from "@testing-library/react"
import { beforeEach, expect, it, vi } from "vitest"
import QueueSettings from "./QueueSettings"
import { remoteTarget } from "./workflowFixtures"

const api = vi.hoisted(() => ({
  getQueueWorkflow: vi.fn(), listWorkflowImages: vi.fn(), listQueueSecrets: vi.fn(), listAgentPools: vi.fn(),
}))
vi.mock("@/lib/tasks", async (importOriginal) => ({ ...await importOriginal<typeof import("@/lib/tasks")>(), ...api }))

const queue = { prefix: "REL", name: "Release", description: "", owners: [], responsible_agent: "", revision: 1 }

beforeEach(() => {
  vi.resetAllMocks()
  api.getQueueWorkflow.mockResolvedValue(null)
  api.listWorkflowImages.mockResolvedValue([])
  api.listQueueSecrets.mockResolvedValue([])
  api.listAgentPools.mockResolvedValue({ items: [] })
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
