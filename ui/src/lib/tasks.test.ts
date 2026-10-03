import { beforeEach, describe, expect, it, vi } from "vitest"

const apiOn = vi.hoisted(() => vi.fn())
vi.mock("./api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("./api")>()),
  apiOn,
  resolveTarget: (target: unknown) => target,
}))
import { ApiError } from "./api"

import {
  advanceTask,
  cancelWorkflowTask,
  clearQueueWorkflow,
  getQueueWorkflow,
  getTaskArtifactHistory,
  getTaskScriptRunLog,
  getTaskWorkflow,
  getTransitionRequest,
  isActiveCategory,
  isWorkflowTask,
  listQueueSecrets,
  listTaskScriptRuns,
  listWorkflowImages,
  getWorkflowImage,
  removeWorkflowImage,
  validateWorkflowDirectory,
  buildWorkflowDirectory,
  moveTaskWorkflow,
  removeQueueSecret,
  resumeTaskWorkflow,
  setQueueSecret,
  setQueueWorkflow,
  setTaskArtifact,
  taskCategoryLabel,
  taskStatusLabel,
  type Task,
  listAgentPools,
  rebindAgentPool,
  updateTask,
} from "./tasks"

describe("task client", () => {
  beforeEach(() => apiOn.mockReset().mockResolvedValue({}))

  it("uses revisioned explicit pool mutations", async () => {
    await rebindAgentPool("DEV", "developers", ["dev-a", "dev-b"], 4, "pool-4")
    expect(apiOn).toHaveBeenCalledWith(undefined, "PATCH", "/api/task-queues/DEV/pools/developers", {
      agents: ["dev-a", "dev-b"], revision: 4, idempotency_key: "pool-4",
    })
  })

  it("lists the agent pools of a queue", async () => {
    await listAgentPools("DEV")
    expect(apiOn.mock.calls.map((call) => call.slice(1, 3))).toEqual([
      ["GET", "/api/task-queues/DEV/pools"],
    ])
  })

  it("keeps wait-customer, pull request, revision, and explicit target in task updates", async () => {
    const target = { id: "remote", label: "Remote", baseURL: "https://remote.test", token: "secret" }
    await updateTask("TARI-43", {
      status: "wait_customer",
      pull_request: "https://example.test/pull/7",
      revision: 7,
    }, target)
    expect(apiOn).toHaveBeenCalledWith(target, "PATCH", "/api/tasks/TARI-43", {
      status: "wait_customer",
      pull_request: "https://example.test/pull/7",
      revision: 7,
    })
  })
})

const remote = { id: "remote", label: "Remote", baseURL: "https://remote.test", token: "secret" }
const lastCall = () => apiOn.mock.calls.at(-1)

describe("workflow task client", () => {
  beforeEach(() => apiOn.mockReset().mockResolvedValue({}))

  it("reads the workflow of a task", async () => {
    const view = { name: "flow", status: "review", statuses: [{ id: "review", owner: "customer", terminal: false }], declared_artifacts: [] }
    apiOn.mockResolvedValue(view)
    await expect(getTaskWorkflow("TARI 1", remote)).resolves.toEqual(view)
    expect(lastCall()).toEqual([remote, "GET", "/api/tasks/TARI%201/workflow", undefined])
  })

  it("advances with and without the status the caller saw", async () => {
    const pending = { id: 4, outcome: "approve", state: "pending", wait_seconds: 30 }
    apiOn.mockResolvedValue(pending)
    await expect(advanceTask("TARI-1", "approve", "ok", undefined, remote)).resolves.toEqual(pending)
    expect(lastCall()).toEqual([remote, "POST", "/api/tasks/TARI-1/advance", { outcome: "approve", message: "ok" }])
    const request = await advanceTask("TARI-1", "approve", "ok", "review", remote)
    expect(request.wait_seconds).toBe(30)
    expect(lastCall()).toEqual([remote, "POST", "/api/tasks/TARI-1/advance", { outcome: "approve", message: "ok", from: "review" }])
  })

  it("reads a transition request", async () => {
    await getTransitionRequest("TARI-1", 7, remote)
    expect(lastCall()).toEqual([remote, "GET", "/api/tasks/TARI-1/workflow/requests/7", undefined])
  })

  it("sets an artifact and reads its history", async () => {
    await setTaskArtifact("TARI-1", "pr url", "https://x", remote)
    expect(lastCall()).toEqual([remote, "PUT", "/api/tasks/TARI-1/artifacts/pr%20url", { value: "https://x" }])
    apiOn.mockResolvedValue({ artifact: { id: 2 }, history: [{ id: 1 }, { id: 2 }] })
    expect(await getTaskArtifactHistory("TARI-1", "pr url", remote)).toEqual([{ id: 1 }, { id: 2 }])
    expect(lastCall()).toEqual([remote, "GET", "/api/tasks/TARI-1/artifacts/pr%20url", undefined])
  })

  it("lists script runs and unwraps them", async () => {
    apiOn.mockResolvedValue({ runs: [{ id: 3 }], count: 1 })
    expect(await listTaskScriptRuns("TARI-1", remote)).toEqual([{ id: 3 }])
    expect(lastCall()).toEqual([remote, "GET", "/api/tasks/TARI-1/workflow/runs", undefined])
  })

  it("returns the log text and truncation, with an optional byte limit", async () => {
    apiOn.mockResolvedValue({ run_id: 3, text: "line", truncated: true })
    expect(await getTaskScriptRunLog("TARI-1", 3, undefined, remote)).toEqual({ text: "line", truncated: true })
    expect(lastCall()).toEqual([remote, "GET", "/api/tasks/TARI-1/workflow/runs/3/log", undefined])
    await getTaskScriptRunLog("TARI-1", 3, 4096, remote)
    expect(lastCall()).toEqual([remote, "GET", "/api/tasks/TARI-1/workflow/runs/3/log?max_bytes=4096", undefined])
  })

  it("moves, cancels, and resumes", async () => {
    await moveTaskWorkflow("TARI-1", "review", "stuck", remote)
    expect(lastCall()).toEqual([remote, "POST", "/api/tasks/TARI-1/workflow/move", { to: "review", reason: "stuck" }])
    await cancelWorkflowTask("TARI-1", remote)
    expect(lastCall()).toEqual([remote, "POST", "/api/tasks/TARI-1/cancel", undefined])
    await resumeTaskWorkflow("TARI-1", "release", remote)
    expect(lastCall()).toEqual([remote, "POST", "/api/tasks/TARI-1/workflow/resume", { decision: "release" }])
  })

  it("maps only an unbound queue to null", async () => {
    apiOn.mockRejectedValueOnce(new ApiError(404, "queue_workflow_not_found", "none"))
    expect(await getQueueWorkflow("DEV", remote)).toBeNull()
    expect(lastCall()).toEqual([remote, "GET", "/api/task-queues/DEV/workflow", undefined])
    const boom = new ApiError(500, "internal", "boom")
    apiOn.mockRejectedValueOnce(boom)
    await expect(getQueueWorkflow("DEV", remote)).rejects.toBe(boom)
    apiOn.mockResolvedValueOnce({ queue: "DEV", revision: 2 })
    expect(await getQueueWorkflow("DEV", remote)).toEqual({ queue: "DEV", revision: 2 })
  })

  it("binds and clears a queue workflow with the revision", async () => {
    await setQueueWorkflow("DEV", "development:latest", 0, remote)
    expect(lastCall()).toEqual([remote, "PUT", "/api/task-queues/DEV/workflow", { ref: "development:latest", revision: 0 }])
    await clearQueueWorkflow("DEV", 3, remote)
    expect(lastCall()).toEqual([remote, "DELETE", "/api/task-queues/DEV/workflow?revision=3", undefined])
  })

  it("lists, sets, and removes queue secrets without reading values", async () => {
    apiOn.mockResolvedValue({ secrets: [{ key: "TOKEN", updated_at: "t" }], count: 1 })
    expect(await listQueueSecrets("DEV", remote)).toEqual([{ key: "TOKEN", updated_at: "t" }])
    expect(lastCall()).toEqual([remote, "GET", "/api/task-queues/DEV/secrets", undefined])
    apiOn.mockResolvedValue({ queue: "DEV", key: "A B", updated_at: "t" })
    expect(await setQueueSecret("DEV", "A B", "s3cret", remote)).toEqual({ queue: "DEV", key: "A B", updated_at: "t" })
    expect(lastCall()).toEqual([remote, "PUT", "/api/task-queues/DEV/secrets/A%20B", { value: "s3cret" }])
    await removeQueueSecret("DEV", "A B", remote)
    expect(lastCall()).toEqual([remote, "DELETE", "/api/task-queues/DEV/secrets/A%20B", undefined])
  })

  it("lists workflow images", async () => {
    apiOn.mockResolvedValue({ workflows: [{ name: "development", tag: "latest" }], count: 1 })
    expect(await listWorkflowImages(remote)).toEqual([{ name: "development", tag: "latest" }])
    expect(lastCall()).toEqual([remote, "GET", "/api/workflow-images", undefined])
  })

  it("reads, removes, validates, and builds workflow images on the target", async () => {
    await getWorkflowImage("dev flow", "1.0.0", remote)
    expect(lastCall()).toEqual([remote, "GET", "/api/workflow-images/dev%20flow/1.0.0", undefined])
    await removeWorkflowImage("dev", "latest", remote)
    expect(lastCall()).toEqual([remote, "DELETE", "/api/workflow-images/dev/latest", undefined])
    await validateWorkflowDirectory("/src/wf", remote)
    expect(lastCall()).toEqual([remote, "POST", "/api/workflow-images/validate", { path: "/src/wf" }])
    await buildWorkflowDirectory("/src/wf", remote)
    expect(lastCall()).toEqual([remote, "POST", "/api/workflow-images/build", { path: "/src/wf" }])
  })
})

describe("workflow task helpers", () => {
  const task = (over: Partial<Task>): Task => ({ key: "T-1", status: "open", category: "open", ...over }) as Task

  it("tells workflow tasks from flexible ones", () => {
    expect(isWorkflowTask(task({}))).toBe(false)
    expect(isWorkflowTask(task({ workflow_name: "development" }))).toBe(true)
    // The daemon marks a workflow task by its digest; the name is a fallback.
    expect(isWorkflowTask(task({ workflow_digest: "abc" }))).toBe(true)
  })

  it("labels the status of either kind", () => {
    expect(taskStatusLabel(task({ status: "wait_customer", category: "wait_customer" }))).toBe("Wait customer")
    expect(taskStatusLabel(task({ status: "code-review_pending", category: "in_progress", workflow_name: "d" }))).toBe("Code review pending")
    const long = "x".repeat(64)
    expect(taskStatusLabel(task({ status: long, workflow_name: "d" }))).toBe("X".concat("x".repeat(63)))
  })

  it("labels categories and tells active ones", () => {
    expect(taskCategoryLabel("in_progress")).toBe("In progress")
    expect(["open", "in_progress"].every((c) => isActiveCategory(c as never))).toBe(true)
    expect(["wait_customer", "done", "cancelled"].some((c) => isActiveCategory(c as never))).toBe(false)
  })
})
