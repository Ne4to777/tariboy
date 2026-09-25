/** Timestamps in the task table are read down a column, so they are short and
 *  tabular: the clock for today, the weekday for the last week, and a date
 *  before that. An unparseable or empty timestamp reads as an em dash rather
 *  than "Invalid Date". */
export function formatTaskTime(iso: string, now = new Date()): string {
  if (!iso) return "—"
  const at = new Date(iso)
  if (Number.isNaN(at.getTime())) return "—"
  const sameDay = at.getFullYear() === now.getFullYear()
    && at.getMonth() === now.getMonth()
    && at.getDate() === now.getDate()
  if (sameDay) {
    return `${String(at.getHours()).padStart(2, "0")}:${String(at.getMinutes()).padStart(2, "0")}`
  }
  const days = (now.getTime() - at.getTime()) / 86_400_000
  if (days >= 0 && days < 7) return at.toLocaleDateString("en-US", { weekday: "short" })
  return at.toLocaleDateString("en-US", { day: "numeric", month: "short" })
}

/** How long a task has been worked: its first move to in_progress to
 *  completion, or to now while it is still open. A task never started reads as
 *  an em dash. Read down a column beside the clock, so it is compact and never
 *  longer than two units. */
export function formatTaskDuration(
  task: { started_at?: string; completed_at: string },
  now = new Date(),
): string {
  if (!task.started_at) return "—"
  const from = new Date(task.started_at)
  const to = task.completed_at ? new Date(task.completed_at) : now
  if (Number.isNaN(from.getTime()) || Number.isNaN(to.getTime())) return "—"
  const seconds = Math.max(0, Math.round((to.getTime() - from.getTime()) / 1000))
  const minutes = Math.floor(seconds / 60)
  if (minutes < 1) return `${seconds}s`
  const hours = Math.floor(minutes / 60)
  if (hours < 1) return `${minutes}m`
  const days = Math.floor(hours / 24)
  if (days < 1) return minutes % 60 === 0 ? `${hours}h` : `${hours}h ${minutes % 60}m`
  return hours % 24 === 0 ? `${days}d` : `${days}d ${hours % 24}h`
}
