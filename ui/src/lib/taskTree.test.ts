import { describe, expect, it } from "vitest"
import type { Task } from "./tasks"
import {
  buildTaskForest,
  canReorderTaskBeside,
  canDropTaskInside,
  flattenVisible,
} from "./taskTree"

function task(
  key: string,
  parentKey = "",
  position = 0,
  priority: "P0" | "P1" | "P2" | "P3" = "P2",
): Task {
  return {
    key,
    category: "open",
    queue: "TEST",
    parent_key: parentKey,
    position,
    priority,
    title: key,
    description: "",
    status: "open",
    author: "user:owner",
    customer: "user:owner",
    group: "",
    assignee: "",
    manual_block_reason: "",
    blocked: false,
    revision: 1,
    created_at: "2026-07-31T10:00:00Z",
    updated_at: "2026-07-31T10:00:00Z",
    completed_at: "",
  }
}

describe("task tree model", () => {
  it("keeps arbitrary-depth descendants in a five-level forest", () => {
    const rows = [
      task("TEST-5", "TEST-4"),
      task("TEST-3", "TEST-2"),
      task("TEST-1"),
      task("TEST-4", "TEST-3"),
      task("TEST-2", "TEST-1"),
    ]

    const forest = buildTaskForest(rows)

    expect(forest).toHaveLength(1)
    expect(forest[0].children[0].children[0].children[0].children[0].task.key)
      .toBe("TEST-5")
  })

  it("keeps context ancestors and promotes an orphan with a missing parent", () => {
    const forest = buildTaskForest([
      { ...task("TEST-1"), access: "context" },
      task("TEST-2", "TEST-1"),
      task("TEST-9", "TEST-404"),
    ])

    expect(forest.map((node) => node.task.key)).toEqual(["TEST-1", "TEST-9"])
    expect(forest[0].children[0].task.key).toBe("TEST-2")
    expect(forest[1].orphaned).toBe(true)
  })

  it("sorts root and nested siblings by priority, position, and key", () => {
    const forest = buildTaskForest([
      task("TEST-4", "", 1, "P3"),
      task("TEST-3", "", 20, "P0"),
      task("TEST-2", "", 10, "P0"),
      task("TEST-1", "", 1, "P2"),
      task("TEST-8", "TEST-1", 1, "P2"),
      task("TEST-7", "TEST-1", 20, "P1"),
      task("TEST-6", "TEST-1", 10, "P1"),
      task("TEST-5", "TEST-1", 1, "P0"),
    ])

    expect(forest.map((node) => node.task.key)).toEqual([
      "TEST-2",
      "TEST-3",
      "TEST-1",
      "TEST-4",
    ])
    expect(forest[2].children.map((node) => node.task.key)).toEqual([
      "TEST-5",
      "TEST-6",
      "TEST-7",
      "TEST-8",
    ])
  })

  it("only reorders siblings within the same priority", () => {
    expect(canReorderTaskBeside(task("TEST-1", "", 1, "P0"), task("TEST-2", "", 2, "P0"))).toBe(true)
    expect(canReorderTaskBeside(task("TEST-1", "", 1, "P3"), task("TEST-2", "", 2, "P0"))).toBe(false)
    expect(canReorderTaskBeside(undefined, task("TEST-2"))).toBe(false)
  })

  it("flattens only expanded branches", () => {
    const forest = buildTaskForest([
      task("TEST-1"),
      task("TEST-2", "TEST-1"),
      task("TEST-3", "TEST-2"),
    ])

    expect(flattenVisible(forest, new Set()).map((row) => row.task.key))
      .toEqual(["TEST-1"])
    expect(flattenVisible(forest, new Set(["TEST-1"])).map((row) => [row.task.key, row.depth]))
      .toEqual([["TEST-1", 0], ["TEST-2", 1]])
  })

  it("rejects an inside drop onto the dragged task or its own subtree", () => {
    const forest = buildTaskForest([
      task("TEST-1"),
      task("TEST-2", "TEST-1"),
      task("TEST-3", "TEST-2"),
      task("TEST-4"),
    ])

    expect(canDropTaskInside(forest, "TEST-1", "TEST-1")).toBe(false)
    expect(canDropTaskInside(forest, "TEST-1", "TEST-3")).toBe(false)
    expect(canDropTaskInside(forest, "TEST-1", "TEST-4")).toBe(true)
  })
})
