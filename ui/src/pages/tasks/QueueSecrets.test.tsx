import { fireEvent, render, screen, waitFor } from "@testing-library/react"
import { beforeEach, expect, it, vi } from "vitest"
import { ApiError } from "@/lib/api"
import QueueSecrets from "./QueueSecrets"
import { remoteTarget } from "./workflowFixtures"

const api = vi.hoisted(() => ({ listQueueSecrets: vi.fn(), setQueueSecret: vi.fn(), removeQueueSecret: vi.fn() }))
vi.mock("@/lib/tasks", async (importOriginal) => ({ ...await importOriginal<typeof import("@/lib/tasks")>(), ...api }))

const renderIt = () => render(<QueueSecrets queue="REL" target={remoteTarget} />)
const fill = (key: string, value: string) => {
  fireEvent.change(screen.getByLabelText("Secret key"), { target: { value: key } })
  fireEvent.change(screen.getByLabelText("Secret value"), { target: { value } })
}

beforeEach(() => {
  vi.resetAllMocks()
  api.listQueueSecrets.mockResolvedValue([{ key: "TOKEN", updated_at: "2026-10-02T10:00:00Z" }])
})

it("lists keys only and uses a password input for the value", async () => {
  renderIt()
  expect(await screen.findByText("TOKEN")).toBeInTheDocument()
  expect(screen.getByLabelText("Secret value")).toHaveAttribute("type", "password")
  expect(api.listQueueSecrets).toHaveBeenCalledWith("REL", remoteTarget)
})

it("sets a secret, clears the value input, and never renders the value", async () => {
  api.setQueueSecret.mockResolvedValue({ queue: "REL", key: "NEW_KEY", updated_at: "2026-10-02T11:00:00Z" })
  api.listQueueSecrets.mockResolvedValueOnce([]).mockResolvedValue([{ key: "NEW_KEY", updated_at: "2026-10-02T11:00:00Z" }])
  const { container } = renderIt()
  await screen.findByLabelText("Secret key")
  fill("NEW_KEY", "s3cr3t-value")
  fireEvent.click(screen.getByRole("button", { name: "Set" }))
  await waitFor(() => expect(api.setQueueSecret).toHaveBeenCalledWith("REL", "NEW_KEY", "s3cr3t-value", remoteTarget))
  await screen.findByRole("button", { name: "Remove NEW_KEY" })
  expect(screen.getByLabelText("Secret value")).toHaveValue("")
  expect(container.innerHTML).not.toContain("s3cr3t-value")
})

it("refuses an invalid key before any request", async () => {
  renderIt()
  await screen.findByText("TOKEN")
  for (const key of ["1bad", "has-dash", "TARIBOY_X"]) {
    fill(key, "v")
    fireEvent.click(screen.getByRole("button", { name: "Set" }))
    expect(await screen.findByRole("alert")).toHaveTextContent(/key/i)
  }
  expect(api.setQueueSecret).not.toHaveBeenCalled()
})

it("shows daemon errors inline without echoing the value", async () => {
  api.setQueueSecret.mockRejectedValue(new ApiError(413, "secret_too_large", "a secret value is at most 64 KiB"))
  const { container } = renderIt()
  await screen.findByText("TOKEN")
  fill("BIG", "hunter2")
  fireEvent.click(screen.getByRole("button", { name: "Set" }))
  expect(await screen.findByRole("alert")).toHaveTextContent("at most 64 KiB")
  expect(container.innerHTML).not.toContain("hunter2")
  expect(screen.getByLabelText("Secret value")).toHaveValue("")
})

it("asks before removing and does nothing when declined", async () => {
  api.removeQueueSecret.mockResolvedValue(undefined)
  renderIt()
  await screen.findByText("TOKEN")
  fireEvent.click(screen.getByRole("button", { name: "Remove TOKEN" }))
  fireEvent.click(await screen.findByRole("button", { name: "Go back" }))
  expect(api.removeQueueSecret).not.toHaveBeenCalled()
  fireEvent.click(screen.getByRole("button", { name: "Remove TOKEN" }))
  fireEvent.click(await screen.findByRole("button", { name: "Remove secret" }))
  await waitFor(() => expect(api.removeQueueSecret).toHaveBeenCalledWith("REL", "TOKEN", remoteTarget))
})

it("shows a refused remove inline", async () => {
  api.removeQueueSecret.mockRejectedValue(new ApiError(409, "workflow_secret_missing", "the bound workflow requires TOKEN"))
  renderIt()
  await screen.findByText("TOKEN")
  fireEvent.click(screen.getByRole("button", { name: "Remove TOKEN" }))
  fireEvent.click(await screen.findByRole("button", { name: "Remove secret" }))
  expect(await screen.findByRole("alert")).toHaveTextContent("requires TOKEN")
})

it("disables Set and every Remove while a request runs", async () => {
  let finish: () => void = () => {}
  api.removeQueueSecret.mockReturnValue(new Promise<void>((resolve) => { finish = resolve }))
  renderIt()
  await screen.findByText("TOKEN")
  fill("OTHER", "v")
  fireEvent.click(screen.getByRole("button", { name: "Remove TOKEN" }))
  fireEvent.click(await screen.findByRole("button", { name: "Remove secret" }))
  await waitFor(() => expect(screen.getByRole("button", { name: "Remove TOKEN" })).toBeDisabled())
  expect(screen.getByRole("button", { name: "Set" })).toBeDisabled()
  finish()
  await waitFor(() => expect(screen.getByRole("button", { name: "Remove TOKEN" })).toBeEnabled())
  expect(screen.getByRole("button", { name: "Set" })).toBeEnabled()
})

it("drops a needed name once that key is in the list", async () => {
  api.setQueueSecret.mockResolvedValue({ queue: "REL", key: "API_KEY", updated_at: "2026-10-02T11:00:00Z" })
  api.listQueueSecrets.mockResolvedValueOnce([]).mockResolvedValue([{ key: "API_KEY", updated_at: "2026-10-02T11:00:00Z" }])
  render(<QueueSecrets queue="REL" target={remoteTarget} missing={["API_KEY", "OTHER"]} />)
  expect(await screen.findByRole("alert", { name: "Secrets needed" })).toHaveTextContent("API_KEY, OTHER")
  fill("API_KEY", "v")
  fireEvent.click(screen.getByRole("button", { name: "Set" }))
  await waitFor(() => expect(screen.getByRole("alert", { name: "Secrets needed" })).not.toHaveTextContent("API_KEY"))
  expect(screen.getByRole("alert", { name: "Secrets needed" })).toHaveTextContent("OTHER")
})

it("renders the names a bind needs next to the control", async () => {
  render(<QueueSecrets queue="REL" target={remoteTarget} missing={["API_KEY"]} missingMessage="workflow secrets have no value: API_KEY" />)
  expect(await screen.findByRole("alert", { name: "Secrets needed" })).toHaveTextContent("API_KEY")
})
