import { renderHook, waitFor } from "@testing-library/react"
import { beforeEach, expect, it, vi } from "vitest"
import type { WorkflowDefinition, WorkflowImage } from "@/lib/tasks"
import { boundTo, effectiveLimits, groupWorkflowImages, ownerLabel, useQueueBindings } from "./workflowImages"

const api = vi.hoisted(() => ({ listTaskQueues: vi.fn(), getQueueWorkflow: vi.fn() }))
vi.mock("@/lib/tasks", async (importOriginal) => ({
  ...await importOriginal<typeof import("@/lib/tasks")>(),
  ...api,
}))

const remote = { id: "remote", label: "Remote", baseURL: "https://remote", token: "t" }
const image = (name: string, tag: string, digest = "d"): WorkflowImage =>
  ({ name, tag, version: tag, digest, built_at: "2026-10-01T00:00:00Z" })

beforeEach(() => {
  api.listTaskQueues.mockReset()
  api.getQueueWorkflow.mockReset()
})

it("groups by name and puts latest first, then versions newest first", () => {
  const groups = groupWorkflowImages([
    image("research", "0.1.0"), image("development", "1.9.0"), image("development", "latest"),
    image("development", "1.10.0"), image("development", "0.2.0"),
  ])
  expect(groups.map((group) => [group.name, group.tags.map((tag) => tag.tag)])).toEqual([
    ["development", ["latest", "1.10.0", "1.9.0", "0.2.0"]],
    ["research", ["0.1.0"]],
  ])
})

it("collects the queues bound to each digest and marks the unreadable ones", async () => {
  api.listTaskQueues.mockResolvedValue({ queues: [{ prefix: "DEV" }, { prefix: "OPS" }, { prefix: "RES" }, { prefix: "BAD" }], count: 4 })
  api.getQueueWorkflow.mockImplementation(async (queue: string) => {
    if (queue === "BAD") throw new Error("boom")
    if (queue === "RES") return null
    return { queue, digest: "aaa" }
  })
  const { result } = renderHook(() => useQueueBindings(remote))
  await waitFor(() => expect(result.current.loaded).toBe(true))
  expect(api.listTaskQueues).toHaveBeenCalledWith(remote)
  for (const queue of ["DEV", "OPS", "RES", "BAD"]) expect(api.getQueueWorkflow).toHaveBeenCalledWith(queue, remote)
  expect(boundTo(result.current, "aaa")).toEqual({ text: "DEV, OPS, unknown", title: "Could not read the binding of BAD" })
  expect(boundTo(result.current, "bbb")).toEqual({ text: "unknown", title: "Could not read the binding of BAD" })
})

it("shows unknown everywhere when the queue list fails", async () => {
  api.listTaskQueues.mockRejectedValue(new Error("offline"))
  const { result } = renderHook(() => useQueueBindings(null))
  await waitFor(() => expect(result.current.loaded).toBe(true))
  expect(boundTo(result.current, "aaa")).toEqual({ text: "unknown", title: "Could not list queues: offline" })
})

it("shows a dash for an image no queue binds", async () => {
  api.listTaskQueues.mockResolvedValue({ queues: [], count: 0 })
  const { result } = renderHook(() => useQueueBindings(null))
  await waitFor(() => expect(result.current.loaded).toBe(true))
  expect(boundTo(result.current, "aaa")).toEqual({ text: "—", title: undefined })
})

it("labels owners", () => {
  expect(ownerLabel({ owner: { kind: "pool", pool: "devs" } })).toBe("pool:devs")
  expect(ownerLabel({ owner: { kind: "customer" } })).toBe("customer")
  expect(ownerLabel({ owner: { kind: "script" } })).toBe("script")
  expect(ownerLabel({ owner: { kind: "" }, terminal: true })).toBe("terminal")
})

it("resolves limits status over workflow over default", () => {
  const definition = {
    limits: { idle_iterations: 4 },
    statuses: [
      { id: "plan", owner: { kind: "pool", pool: "devs" }, limits: { rejected_requests: 9, unavailable_grace: "0" } },
      { id: "done", owner: { kind: "" }, terminal: true },
    ],
  } as unknown as WorkflowDefinition
  expect(effectiveLimits(definition)).toEqual([
    {
      scope: "workflow",
      values: [
        { key: "idle_iterations", value: "4", source: "workflow" },
        { key: "rejected_requests", value: "5", source: "default" },
        { key: "script_failures", value: "3", source: "default" },
        { key: "unavailable_grace", value: "5m", source: "default" },
      ],
    },
    {
      scope: "plan",
      values: [
        { key: "idle_iterations", value: "4", source: "workflow" },
        { key: "rejected_requests", value: "9", source: "status" },
        { key: "script_failures", value: "3", source: "default" },
        { key: "unavailable_grace", value: "0", source: "status" },
      ],
    },
  ])
})
