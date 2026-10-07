import { act, fireEvent, render, screen, within } from "@testing-library/react"
import { beforeEach, expect, it, vi } from "vitest"
import { ApiError } from "@/lib/api"
import type { QueueSource, SourceRun } from "@/lib/tasks"
import QueueSources from "./QueueSources"
import { remoteTarget } from "./workflowFixtures"

const api = vi.hoisted(() => ({ listQueueSources: vi.fn(), getQueueSourceRunLog: vi.fn() }))
vi.mock("@/lib/tasks", async (importOriginal) => ({ ...await importOriginal<typeof import("@/lib/tasks")>(), ...api }))

const finished: SourceRun = {
  id: 4, queue: "DEV", source: "pull-requests", script: "./sources/prs.sh", state: "finished", verdict: "failure",
  exit_code: 1, message: "gh: rate limited", tasks_created: 0, started_at: "2026-10-05T12:00:00Z", finished_at: "2026-10-05T12:00:03Z",
}
const running: SourceRun = { ...finished, id: 5, state: "running", verdict: undefined, exit_code: undefined, message: undefined,
  finished_at: undefined, started_at: "2026-10-05T12:02:00Z" }
const source: QueueSource = {
  queue: "DEV", name: "pull-requests", script: "./sources/prs.sh", every: "2m", failures: 1,
  last_run: running, runs: [running, finished],
}

beforeEach(() => {
  vi.resetAllMocks()
  api.listQueueSources.mockResolvedValue([source])
})

it("lists each source with its schedule and its runs, newest first", async () => {
  render(<QueueSources queue="DEV" target={remoteTarget} />)
  const item = await screen.findByRole("group", { name: "Source pull-requests" })
  expect(api.listQueueSources).toHaveBeenCalledWith("DEV", remoteTarget)
  expect(item).toHaveTextContent("./sources/prs.sh")
  expect(item).toHaveTextContent("every 2m")
  expect(item).toHaveTextContent("1 failed in a row")
  const rows = within(item).getAllByRole("listitem")
  expect(rows).toHaveLength(2)
  expect(rows[0]).toHaveTextContent("running")
  expect(rows[1]).toHaveTextContent("failure")
  expect(rows[1]).toHaveTextContent("exit 1")
  expect(rows[1]).toHaveTextContent("gh: rate limited")
})

it("says so when the workflow has no sources", async () => {
  api.listQueueSources.mockResolvedValue([])
  render(<QueueSources queue="DEV" target={remoteTarget} />)
  expect(await screen.findByText("No sources")).toBeInTheDocument()
})

it("shows a failed load with Retry instead of claiming there are no sources", async () => {
  api.listQueueSources.mockRejectedValueOnce(new ApiError(403, "forbidden", "only the customer may read sources"))
  render(<QueueSources queue="DEV" target={remoteTarget} />)
  expect(await screen.findByRole("alert")).toHaveTextContent("only the customer may read sources")
  expect(screen.queryByText("No sources")).not.toBeInTheDocument()
  await act(async () => { fireEvent.click(screen.getByRole("button", { name: "Retry" })) })
  expect(await screen.findByRole("group", { name: "Source pull-requests" })).toBeInTheDocument()
})

it("reads a run's log on demand and refreshes the log of a running run", async () => {
  api.getQueueSourceRunLog.mockResolvedValueOnce({ text: "polling", truncated: false })
    .mockResolvedValueOnce({ text: "polling\nfound 2", truncated: false })
  render(<QueueSources queue="DEV" target={remoteTarget} />)
  const item = await screen.findByRole("group", { name: "Source pull-requests" })
  const row = within(item).getAllByRole("listitem")[0]
  expect(api.getQueueSourceRunLog).not.toHaveBeenCalled()
  await act(async () => { fireEvent.click(within(row).getByRole("button", { name: "Log" })) })
  expect(api.getQueueSourceRunLog).toHaveBeenCalledWith("DEV", 5, undefined, remoteTarget)
  expect(row.querySelector("pre")!.textContent).toBe("polling")
  await act(async () => { fireEvent.click(within(row).getByRole("button", { name: "Refresh log" })) })
  expect(row.querySelector("pre")!.textContent).toBe("polling\nfound 2")
})

it("reloads the list on demand", async () => {
  render(<QueueSources queue="DEV" target={remoteTarget} />)
  await screen.findByRole("group", { name: "Source pull-requests" })
  await act(async () => { fireEvent.click(screen.getByRole("button", { name: "Reload sources" })) })
  expect(api.listQueueSources).toHaveBeenCalledTimes(2)
})
