import { render, screen, within } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"
import type { Task } from "@/lib/tasks"
import { TASK_COLUMNS, type TaskRowMode } from "./taskColumns"
import TaskRow, { TaskTableHeader } from "./TaskRow"
import { formatTaskDuration } from "./taskTime"

const task: Task = {
  key: "IMPROVE-136",
  queue: "IMPROVE",
  parent_key: "",
  position: 1,
  priority: "P1",
  title: "Migrate store schema",
  description: "",
  status: "in_progress",
  author: "user:owner",
  customer: "user:owner",
  group: "",
  assignee: "agent:builder",
  manual_block_reason: "",
  blocked: false,
  revision: 1,
  created_at: "2026-09-19T08:00:00Z",
  started_at: "2026-09-20T10:00:00Z",
  updated_at: "2026-09-20T10:18:00Z",
  completed_at: "2026-09-20T10:18:00Z",
}

function widths(scope: HTMLElement): (string | null)[] {
  return [...scope.children]
    .filter((child) => !child.classList.contains("task-drop-zone") && !child.classList.contains("task-row-grip"))
    .map((child) => (child as HTMLElement).style.width || null)
}

function renderRow(mode: TaskRowMode) {
  return render(
    <>
      <TaskTableHeader mode={mode} />
      <TaskRow
        row={{ task, depth: 0, hasChildren: true, orphaned: false }}
        mode={mode}
        hasActiveQuestion={false}
        expanded={false}
        selected={false}
        onToggle={vi.fn()}
        onSelect={vi.fn()}
        onAddChild={vi.fn()}
      />
    </>,
  )
}

describe("task table columns", () => {
  it.each(["agent", "all"] as const)("lays the %s header and row out on one set of widths", (mode) => {
    renderRow(mode)

    const header = screen.getByTestId("task-table-header")
    const row = screen.getByTestId(`task-row-${task.key}`)
    expect(widths(row)).toEqual(widths(header))
    expect(widths(header)[0]).toBe(`${TASK_COLUMNS.key}px`)
  })

  it("keeps the key on one line inside a fixed 168px column", () => {
    renderRow("agent")

    expect(TASK_COLUMNS.key).toBe(168)
    const key = screen.getByText("IMPROVE-136")
    expect(key.className).toContain("truncate")
    expect(key.className).toContain("min-w-0")
    expect(key.parentElement!.className).toContain("overflow-hidden")
    expect(key.parentElement!.style.width).toBe("168px")
  })

  it("holds a deeply indented key inside the same column", () => {
    render(
      <TaskRow
        row={{ task, depth: 9, hasChildren: true, orphaned: false }}
        mode="agent"
        hasActiveQuestion={false}
        expanded={false}
        selected={false}
        onToggle={vi.fn()}
        onSelect={vi.fn()}
        onAddChild={vi.fn()}
      />,
    )

    const column = screen.getByText("IMPROVE-136").parentElement!
    expect(column.style.width).toBe(`${TASK_COLUMNS.key}px`)
    // The indents are capped, so they cannot push the key out of the column.
    const indents = [...column.children].filter((child) => (child as HTMLElement).style.width === "14px")
    expect(indents).toHaveLength(6)
  })

  it("carries a duration in the agent table and an agent instead of a queue in the all table", () => {
    const agent = renderRow("agent")
    expect(screen.getByTestId("task-table-header")).toHaveTextContent("Duration")
    expect(screen.getByText(formatTaskDuration(task))).toBeInTheDocument()
    agent.unmount()

    renderRow("all")
    const header = screen.getByTestId("task-table-header")
    expect(header).not.toHaveTextContent("Queue")
    expect(header).not.toHaveTextContent("Duration")
    expect(within(screen.getByTestId(`task-row-${task.key}`)).getByText("builder")).toBeInTheDocument()
    expect(screen.queryByText("IMPROVE", { exact: true })).toBeNull()
  })
})

describe("task duration", () => {
  it("measures a closed task from its first start to completion and an open one until now", () => {
    expect(formatTaskDuration(task)).toBe("18m")
    expect(formatTaskDuration(
      { ...task, completed_at: "" },
      new Date("2026-09-20T13:30:00Z"),
    )).toBe("3h 30m")
    expect(formatTaskDuration({ ...task, started_at: "" })).toBe("—")
    expect(formatTaskDuration({ ...task, started_at: undefined })).toBe("—")
  })
})
