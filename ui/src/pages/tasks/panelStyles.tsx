import { ChevronDown } from "lucide-react"
import type { ComponentProps, ReactNode } from "react"

import { cn } from "@/lib/utils"

/**
 * The vocabulary of the task detail panel, in one place so the panel and its
 * comment section cannot drift apart. Nothing here decides what a thing means —
 * these are the sizes and fills the style layer gives a label, a field, a row
 * and a quiet action.
 */

/** Every section label and field label reads the same. */
export const LABEL = "text-[11.5px] font-medium tracking-[.02em] text-muted-foreground"
/** A field is a fill, not a box: 30px of `--muted` at radius 8, no border. */
export const FIELD = "h-[30px] w-full min-w-0 rounded-[8px] border-0 bg-muted px-2.5 text-[12.5px] md:text-[12.5px] text-foreground shadow-none outline-none focus-visible:ring-2 focus-visible:ring-ring"
/** A field carrying an identifier rather than prose. */
export const FIELD_MONO = "font-mono text-[11.5px] md:text-[11.5px]"
/** Keys, times, durations and digests line up in one column. */
export const MONO = "font-mono text-[11.5px] tabular-nums"
/** The list row shared by dependencies and history. */
export const ROW = "-mx-1.5 flex min-h-[30px] items-center gap-2.5 rounded-[8px] px-2 hover:bg-muted"
/** The counter that follows a section label. */
export const COUNT = "font-mono text-[11px] tabular-nums text-muted-foreground opacity-75"
/** A quiet 24px action sitting on the right of a section header. */
export const QUIET_ACTION = "h-6 rounded-[7px] px-2.5 text-[12px] font-normal text-muted-foreground hover:bg-accent hover:text-foreground"
/** The one empty-state voice: a single quiet line where the list would be. */
export const EMPTY = "text-[12px] text-muted-foreground opacity-80"
/** The two banner fills. Translucent, so they sit on any surface. */
export const DANGER_FILL = "bg-[color-mix(in_oklab,var(--status-failed)_12%,transparent)] text-status-failed"
export const PRIMARY_FILL = "bg-[color-mix(in_oklab,var(--primary)_12%,transparent)] text-primary"

/**
 * A native select wearing the field fill, with the chevron drawn over it. The
 * shell exists because four selects in this panel need the same overlay at
 * three different heights.
 */
export function SelectShell({ className, children, ...props }: ComponentProps<"select"> & { children: ReactNode }) {
  return <span className="relative inline-flex min-w-0">
    <select className={cn(FIELD, "appearance-none pr-7", className)} {...props}>{children}</select>
    <ChevronDown aria-hidden="true" className="pointer-events-none absolute top-1/2 right-2.5 size-2.5 -translate-y-1/2 opacity-45" />
  </span>
}
