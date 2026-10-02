import { beforeEach, describe, expect, it, vi } from "vitest"

const apiOn = vi.hoisted(() => vi.fn())
vi.mock("./api", () => ({
  apiOn,
  resolveTarget: (target: unknown) => target,
}))

import {
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
