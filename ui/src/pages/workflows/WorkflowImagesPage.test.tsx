import { fireEvent, render, screen, waitFor, within } from "@testing-library/react"
import { MemoryRouter } from "react-router-dom"
import { beforeEach, expect, it, vi } from "vitest"
import WorkflowImagesPage from "./WorkflowImagesPage"

const api = vi.hoisted(() => ({ listWorkflowImages: vi.fn(), listTaskQueues: vi.fn(), getQueueWorkflow: vi.fn() }))
vi.mock("@/lib/tasks", async (importOriginal) => ({ ...await importOriginal<typeof import("@/lib/tasks")>(), ...api }))
vi.mock("@/components/DaemonProvider", () => ({ useOptionalDaemons: () => ({ daemons: [] }) }))

const remote = { id: "remote", label: "Remote", baseURL: "https://remote", token: "t" }
const digestA = "a".repeat(64)
const digestB = "b".repeat(64)

beforeEach(() => {
  vi.clearAllMocks()
  api.listWorkflowImages.mockResolvedValue([
    { name: "development", tag: "0.9.0", version: "0.9.0", digest: digestB, built_at: "2026-09-01T00:00:00Z" },
    { name: "development", tag: "latest", version: "0.10.0", digest: digestA, built_at: "2026-10-01T00:00:00Z" },
    { name: "development", tag: "0.10.0", version: "0.10.0", digest: digestA, built_at: "2026-10-01T00:00:00Z" },
    { name: "research", tag: "0.1.0", version: "0.1.0", digest: digestB, built_at: "2026-09-02T00:00:00Z" },
  ])
  api.listTaskQueues.mockResolvedValue({ queues: [{ prefix: "DEV" }, { prefix: "BAD" }], count: 2 })
  api.getQueueWorkflow.mockImplementation(async (queue: string) => {
    if (queue === "BAD") throw new Error("boom")
    return { queue, digest: digestA }
  })
})

const renderPage = () => render(<MemoryRouter><WorkflowImagesPage target={remote} basePath="/servers/remote/workflows" /></MemoryRouter>)

it("lists tags grouped by name with latest first, short digests, and bindings", async () => {
  renderPage()
  const table = await screen.findByRole("table")
  await waitFor(() => expect(within(table).getAllByText("DEV, unknown")).toHaveLength(2))
  const rows = within(table).getAllByRole("row").slice(1)
  expect(rows.map((row) => within(row).getAllByRole("cell")[1].textContent)).toEqual(["latest", "0.10.0", "0.9.0", "0.1.0"])
  expect(within(rows[0]).getByText(digestA.slice(0, 12))).toHaveAttribute("title", digestA)
  expect(within(rows[0]).getByRole("link", { name: "latest" })).toHaveAttribute("href", "/servers/remote/workflows/development/latest")
  expect(within(rows[2]).getByText("unknown")).toHaveAttribute("title", "Could not read the binding of BAD")
  expect(api.listWorkflowImages).toHaveBeenCalledWith(remote)
  expect(api.listTaskQueues).toHaveBeenCalledWith(remote)
  expect(within(rows[0]).getByRole("button", { name: "Upload to servers development:latest" })).toBeInTheDocument()
})

it("shows an empty registry", async () => {
  api.listWorkflowImages.mockResolvedValue([])
  renderPage()
  expect(await screen.findByText("No workflow images")).toBeInTheDocument()
})

it("shows a list failure with Retry", async () => {
  api.listWorkflowImages.mockRejectedValueOnce(new Error("daemon down"))
  renderPage()
  expect(await screen.findByRole("alert")).toHaveTextContent("daemon down")
  fireEvent.click(screen.getByRole("button", { name: "Retry" }))
  expect(await screen.findByRole("table")).toBeInTheDocument()
})
