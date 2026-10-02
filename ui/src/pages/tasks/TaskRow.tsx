import { useDraggable, useDroppable } from "@dnd-kit/core"
import { ChevronRight, GripVertical, Plus } from "lucide-react"
import { PriorityTag, StatusPill } from "@/components/ui/status"
import { cn } from "@/lib/utils"
import { taskTone } from "@/lib/statusTone"
import { isWorkflowTask, taskStatusLabel } from "@/lib/tasks"
import type { VisibleTaskRow } from "@/lib/taskTree"
import { INDENT_PX, MAX_INDENT_DEPTH, TASK_COLUMNS, type TaskRowMode } from "./taskColumns"
import { formatTaskDuration, formatTaskTime } from "./taskTime"

const priorityLabels = {
  P0: "Critical",
  P1: "High",
  P2: "Normal",
  P3: "Low",
} as const

/* @dnd-kit intentionally exposes callback refs and live attributes from its
 * hook for render-time spreading; they are not mutable React ref reads. */
/* eslint-disable react-hooks/refs */
export default function TaskRow({
  row,
  mode,
  server,
  hasActiveQuestion,
  expanded,
  selected,
  onToggle,
  onSelect,
  onAddChild,
}: {
  row: VisibleTaskRow
  mode: TaskRowMode
  /** The server the task lives on; shown only in the `servers` table. */
  server?: string
  hasActiveQuestion: boolean
  expanded: boolean
  selected: boolean
  onToggle: () => void
  onSelect: () => void
  onAddChild?: () => void
}) {
  const readOnly = row.task.access === "context" || row.task.access === "respond"
  // The every-server table has no drag and no add-child: neither can cross the
  // server boundary the rows sit on.
  const disabled = readOnly || mode === "servers"
  const drag = useDraggable({ id: row.task.key, disabled })
  const dropBefore = useDroppable({ id: `before:${row.task.key}`, disabled })
  const dropInside = useDroppable({ id: `inside:${row.task.key}`, disabled })
  const dropAfter = useDroppable({ id: `after:${row.task.key}`, disabled })
  const tone = taskTone(row.task.category)
  const workflow = isWorkflowTask(row.task)
  return (
    <div
      ref={drag.setNodeRef}
      data-testid={`task-row-${row.task.key}`}
      style={{ opacity: drag.isDragging ? 0.45 : undefined }}
      className={cn(
        "group/row relative flex min-h-[30px] items-center gap-2.5 px-4 text-[13px]",
        selected ? "bg-accent" : "hover:bg-muted",
        dropInside.isOver && "ring-1 ring-primary ring-inset",
        row.task.access === "context" && "opacity-60",
      )}
    >
      <span
        ref={dropBefore.setNodeRef}
        data-testid={`drop-before-${row.task.key}`}
        className={`task-drop-zone before ${dropBefore.isOver ? "is-over" : ""}`}
      />
      <span
        ref={dropInside.setNodeRef}
        data-testid={`drop-inside-${row.task.key}`}
        className="task-drop-zone inside"
      />
      <span
        ref={dropAfter.setNodeRef}
        data-testid={`drop-after-${row.task.key}`}
        className={`task-drop-zone after ${dropAfter.isOver ? "is-over" : ""}`}
      />
      {/* The grip lives in the row's own left padding: the reference table has
          no column for it, and it is only there under the pointer anyway. */}
      {mode !== "servers" && (
        <button
          type="button"
          ref={drag.setActivatorNodeRef}
          className="task-row-grip absolute top-1/2 left-0 z-2 grid h-6 w-4 -translate-y-1/2 cursor-grab place-items-center text-muted-foreground opacity-0 group-hover/row:opacity-60"
          aria-label={`Move ${row.task.key}`}
          {...drag.listeners}
          {...drag.attributes}
        >
          <GripVertical aria-hidden="true" className="size-3" />
        </button>
      )}
      <span
        style={{ width: TASK_COLUMNS.key }}
        className="z-2 flex shrink-0 items-center gap-1.5 overflow-hidden"
      >
        {Array.from({ length: Math.min(row.depth, MAX_INDENT_DEPTH) }, (_, level) => (
          <span key={level} aria-hidden="true" className="shrink-0" style={{ width: INDENT_PX }} />
        ))}
        <button
          type="button"
          className="grid size-4 shrink-0 place-items-center text-muted-foreground opacity-65 disabled:opacity-0"
          aria-label={`${expanded ? "Collapse" : "Expand"} ${row.task.key}`}
          disabled={!row.hasChildren}
          onClick={onToggle}
        >
          <ChevronRight
            aria-hidden="true"
            className={cn("size-2.5 transition-transform duration-[120ms]", expanded && "rotate-90")}
          />
        </button>
        <span className="min-w-0 truncate font-mono text-[11.5px] text-muted-foreground tabular-nums">
          {row.task.key}
        </span>
      </span>
      <button
        type="button"
        className="task-row-main relative z-2 flex min-w-0 flex-1 items-center gap-[7px] overflow-hidden text-left"
        onClick={onSelect}
      >
        <span className="min-w-0 truncate">{row.task.title}</span>
        {hasActiveQuestion && (
          <span
            className="size-[5px] shrink-0 rounded-full bg-primary"
            role="img"
            aria-label={`Unread question notification for ${row.task.key}`}
          />
        )}
        {row.task.blocked && <StatusPill tone="danger" size="sm">blocked</StatusPill>}
      </button>
      {/* Priority stays in both modes, unlike the reference, which drops it from
          the all-tasks table: in this app priority is what orders the tree, so
          a table without it cannot be read. */}
      <span style={{ width: TASK_COLUMNS.priority }} className="shrink-0">
        <PriorityTag
          className="task-priority"
          priority={row.task.priority}
          aria-label={`${row.task.priority} ${priorityLabels[row.task.priority]}`}
        />
      </span>
      {mode !== "agent" && (
        <span style={{ width: TASK_COLUMNS.agent }} className="shrink-0">
          {row.task.assignee && (
            <span className="inline-flex h-[19px] max-w-full items-center truncate rounded-[6px] bg-muted px-[7px] text-[11.5px]">
              {row.task.assignee.replace(/^agent:/, "")}
            </span>
          )}
        </span>
      )}
      {mode === "servers" && (
        <span style={{ width: TASK_COLUMNS.server }} className="shrink-0 truncate font-mono text-[11.5px] text-muted-foreground">
          {server}
        </span>
      )}
      {workflow ? (
        <span style={{ width: TASK_COLUMNS.status }} className="flex min-w-0 shrink-0 items-center gap-1 overflow-hidden">
          <StatusPill tone={tone} className="min-w-0 shrink">
            {/* A workflow status ID can be 64 characters: CSS clips it, the
                title keeps the whole ID readable. */}
            <span title={row.task.status} className="min-w-0 max-w-[24ch] truncate">{taskStatusLabel(row.task)}</span>
          </StatusPill>
          {row.task.waiting_on === "pause" ? (
            <StatusPill tone="danger" size="sm" title={row.task.workflow_paused_reason || undefined}>paused</StatusPill>
          ) : row.task.waiting_on ? (
            <StatusPill tone="quiet" size="sm">{row.task.waiting_on}</StatusPill>
          ) : null}
        </span>
      ) : (
        <span style={{ width: TASK_COLUMNS.status }} className="shrink-0">
          <StatusPill tone={tone}>{taskStatusLabel(row.task)}</StatusPill>
        </span>
      )}
      {mode === "agent" && (
        <span
          style={{ width: TASK_COLUMNS.duration }}
          className="shrink-0 text-right font-mono text-[11.5px] text-muted-foreground tabular-nums"
        >
          {formatTaskDuration(row.task)}
        </span>
      )}
      <span
        style={{ width: TASK_COLUMNS.updated }}
        className="shrink-0 text-right text-[12px] text-muted-foreground tabular-nums"
      >
        {formatTaskTime(row.task.updated_at)}
      </span>
      {mode !== "servers" && <span style={{ width: TASK_COLUMNS.add }} className="shrink-0">
        {!disabled && (
          <button
            type="button"
            className="relative z-2 grid size-6 place-items-center rounded-[7px] text-muted-foreground opacity-0 hover:bg-accent hover:text-foreground group-hover/row:opacity-100"
            aria-label={`Add child to ${row.task.key}`}
            onClick={onAddChild}
          >
            <Plus aria-hidden="true" className="size-3.5" />
          </button>
        )}
      </span>}
    </div>
  )
}

/** The table header. Same widths, same order, same gaps as the rows under it,
 *  read from `TASK_COLUMNS` rather than repeated. */
export function TaskTableHeader({ mode, sticky = false }: { mode: TaskRowMode; sticky?: boolean }) {
  return (
    <div
      data-testid="task-table-header"
      className={cn(
        "flex h-[30px] items-center gap-2.5 px-4 text-[11.5px] text-muted-foreground",
        sticky && "sticky top-0 z-2 bg-card",
      )}
    >
      <span style={{ width: TASK_COLUMNS.key }} className="shrink-0">Key</span>
      <span className="min-w-0 flex-1">Task</span>
      <span style={{ width: TASK_COLUMNS.priority }} className="shrink-0">Pri</span>
      {mode !== "agent" && (
        <span style={{ width: TASK_COLUMNS.agent }} className="shrink-0">Agent</span>
      )}
      {mode === "servers" && (
        <span style={{ width: TASK_COLUMNS.server }} className="shrink-0">Server</span>
      )}
      <span style={{ width: TASK_COLUMNS.status }} className="shrink-0">Status</span>
      {mode === "agent" && (
        <span style={{ width: TASK_COLUMNS.duration }} className="shrink-0 text-right">Duration</span>
      )}
      <span style={{ width: TASK_COLUMNS.updated }} className="shrink-0 text-right">Updated</span>
      {mode !== "servers" && <span aria-hidden="true" style={{ width: TASK_COLUMNS.add }} className="shrink-0" />}
    </div>
  )
}
