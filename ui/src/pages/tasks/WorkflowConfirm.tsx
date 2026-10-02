import { useState } from "react"
import {
  AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent,
  AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle,
} from "@/components/ui/alert-dialog"

/** What a workflow action asks before it runs. */
export interface ConfirmRequest {
  title: string
  description: string
  action: string
  run: () => void
}

/**
 * The in-app confirmation the console uses for destructive actions (see
 * `AgentControls`), shared by the workflow panel's decisions. Declining runs
 * nothing; "Go back" avoids a second "Cancel" next to "Cancel task".
 */
export function useWorkflowConfirm() {
  const [request, setRequest] = useState<ConfirmRequest | null>(null)
  const dialog = (
    <AlertDialog open={request !== null} onOpenChange={(open) => { if (!open) setRequest(null) }}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>{request?.title}</AlertDialogTitle>
          <AlertDialogDescription>{request?.description}</AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel>Go back</AlertDialogCancel>
          <AlertDialogAction variant="destructive" onClick={() => {
            const run = request?.run
            setRequest(null)
            run?.()
          }}>{request?.action}</AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
  return { confirm: setRequest, dialog }
}

export function errorText(error: unknown): string {
  return error instanceof Error ? error.message : String(error)
}
