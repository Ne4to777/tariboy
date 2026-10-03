import { fireEvent, render, screen, waitFor } from "@testing-library/react"
import { beforeEach, expect, it, vi } from "vitest"
import { ApiError } from "@/lib/api"
import WorkflowBuildFromDirectory from "./WorkflowBuildFromDirectory"

const api = vi.hoisted(() => ({ validateWorkflowDirectory: vi.fn(), buildWorkflowDirectory: vi.fn() }))
vi.mock("@/lib/tasks", async (importOriginal) => ({ ...await importOriginal<typeof import("@/lib/tasks")>(), ...api }))
const toast = vi.hoisted(() => ({ success: vi.fn(), error: vi.fn() }))
vi.mock("sonner", () => ({ toast }))

const remote = { id: "remote", label: "Remote", baseURL: "https://remote", token: "t" }

beforeEach(() => vi.clearAllMocks())

function setup() {
  const onBuilt = vi.fn()
  render(<WorkflowBuildFromDirectory target={remote} onBuilt={onBuilt} />)
  fireEvent.change(screen.getByLabelText("Workflow source directory"), { target: { value: " /srv/wf " } })
  return onBuilt
}

it("validates on the target and shows what the source declares", async () => {
  api.validateWorkflowDirectory.mockResolvedValue({ valid: true, name: "dev", version: "1.0.0", pools: ["devs"], files: ["statuses/plan.md"], errors: [] })
  setup()
  fireEvent.click(screen.getByRole("button", { name: "Validate" }))
  expect(await screen.findByText(/dev 1\.0\.0/)).toBeInTheDocument()
  expect(screen.getByText(/devs/)).toBeInTheDocument()
  expect(api.validateWorkflowDirectory).toHaveBeenCalledWith("/srv/wf", remote)
})

it("lists validation errors", async () => {
  api.validateWorkflowDirectory.mockResolvedValue({ valid: false, name: "dev", version: "1.0.0", pools: [], files: [], errors: [{ code: "status_missing", path: "statuses", message: "no statuses" }] })
  setup()
  fireEvent.click(screen.getByRole("button", { name: "Validate" }))
  expect(await screen.findByRole("alert")).toHaveTextContent("status_missing · statuses: no statuses")
})

it("builds, reports a new or an existing build, and refreshes the list", async () => {
  api.buildWorkflowDirectory.mockResolvedValueOnce({ name: "dev", version: "1.0.0", digest: "d", tags: [], created: true })
  const onBuilt = setup()
  fireEvent.click(screen.getByRole("button", { name: "Build" }))
  await waitFor(() => expect(toast.success).toHaveBeenCalledWith("built dev:1.0.0"))
  expect(api.buildWorkflowDirectory).toHaveBeenCalledWith("/srv/wf", remote)
  expect(onBuilt).toHaveBeenCalledTimes(1)

  api.buildWorkflowDirectory.mockResolvedValueOnce({ name: "dev", version: "1.0.0", digest: "d", tags: [], created: false })
  fireEvent.click(screen.getByRole("button", { name: "Build" }))
  await waitFor(() => expect(toast.success).toHaveBeenCalledWith("dev:1.0.0 already built"))
})

it("shows the errors of a refused build and a version hint", async () => {
  api.buildWorkflowDirectory.mockRejectedValueOnce(new ApiError(400, "workflow_invalid", "invalid", { errors: [{ code: "owner_missing", path: "statuses[0].owner", message: "no owner" }] }))
  setup()
  fireEvent.click(screen.getByRole("button", { name: "Build" }))
  expect(await screen.findByRole("alert")).toHaveTextContent("owner_missing · statuses[0].owner: no owner")

  api.buildWorkflowDirectory.mockRejectedValueOnce(new ApiError(409, "workflow_version_published", "dev 1.0.0 is taken"))
  fireEvent.click(screen.getByRole("button", { name: "Build" }))
  expect(await screen.findByText(/dev 1\.0\.0 is taken/)).toBeInTheDocument()
  expect(screen.getByText(/tariboy workflow version update/)).toBeInTheDocument()
})
