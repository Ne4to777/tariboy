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
  category: "in_progress",
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

function renderTask(next: Task) {
  return render(
    <TaskRow
      row={{ task: next, depth: 0, hasChildren: false, orphaned: false }}
      mode="agent"
      hasActiveQuestion={false}
      expanded={false}
      selected={false}
      onToggle={vi.fn()}
      onSelect={vi.fn()}
      onAddChild={vi.fn()}
    />,
  )
}

const workflowTask: Task = {
  ...task,
  status: "implement",
  category: "in_progress",
  workflow_name: "delivery",
}

describe("task row status", () => {
  it("renders a flexible task exactly as before", () => {
    renderTask(task)
    const pill = screen.getByText("In progress")
    expect(pill.tagName).toBe("SPAN")
    expect(pill.dataset.tone).toBe("live")
    expect(pill.className).toBe(
      "inline-flex w-fit shrink-0 items-center gap-1.5 whitespace-nowrap h-5 rounded-[6px] px-2 text-[11.5px] bg-status-running/13 text-status-running font-medium",
    )
    expect(pill.parentElement!.className).toBe("shrink-0")
    expect(pill.parentElement!.children).toHaveLength(1)
    expect(pill).not.toHaveAttribute("title")
  })

  it("shows a workflow status as its label with the tone of its category", () => {
    renderTask(workflowTask)
    const pill = screen.getByText("Implement")
    expect(pill.closest("[data-slot='status-pill']")).toHaveAttribute("data-tone", "live")
    expect(screen.queryByText("In progress")).toBeNull()
  })

  it("tones a workflow task by category, not by status", () => {
    renderTask({ ...workflowTask, status: "review", category: "wait_customer" })
    expect(screen.getByText("Review").closest("[data-slot='status-pill']")).toHaveAttribute("data-tone", "attention")
  })

  it("names the wait of a workflow task: customer, script, or paused", () => {
    const customer = renderTask({ ...workflowTask, category: "wait_customer", waiting_on: "customer" })
    expect(screen.getByText("customer")).toBeInTheDocument()
    customer.unmount()

    const script = renderTask({ ...workflowTask, waiting_on: "script" })
    expect(screen.getByText("script")).toBeInTheDocument()
    script.unmount()

    renderTask({ ...workflowTask, waiting_on: "pause", workflow_paused_reason: "stalled" })
    const paused = screen.getByText("paused")
    expect(paused.closest("[data-slot='status-pill']")).toHaveAttribute("data-tone", "danger")
  })

  it("shows no wait indicator when nothing is waited on", () => {
    renderTask(workflowTask)
    expect(screen.queryByText("customer")).toBeNull()
    expect(screen.queryByText("script")).toBeNull()
    expect(screen.queryByText("paused")).toBeNull()
  })

  it("truncates a 64-character status ID with CSS and keeps the full ID in a title", () => {
    const id = `s${"a".repeat(63)}`
    renderTask({ ...workflowTask, status: id })
    const label = screen.getByTitle(id)
    expect(label.className).toContain("truncate")
    expect(label.textContent!.toLowerCase()).toBe(id)
  })

  it("keeps the drag handle for a workflow task, which only reparents", () => {
    renderTask(workflowTask)
    expect(screen.getByRole("button", { name: `Move ${task.key}` })).toBeInTheDocument()
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
