import { useCallback, useEffect, useState } from "react"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import type { ApiTarget } from "@/lib/api"
import { listQueueSecrets, removeQueueSecret, setQueueSecret, type QueueSecretInfo } from "@/lib/tasks"
import { COUNT, EMPTY, FIELD, LABEL, QUIET_ACTION } from "./panelStyles"
import { formatTaskTime } from "./taskTime"
import { errorText, useWorkflowConfirm } from "./WorkflowConfirm"

// The daemon's rule (internal/tasks/workflow_secrets.go).
const KEY_PATTERN = /^[A-Za-z_][A-Za-z0-9_]*$/

function keyProblem(key: string): string {
  if (!KEY_PATTERN.test(key)) return "A secret key starts with a letter or underscore and holds letters, digits, and underscores."
  if (key.startsWith("TARIBOY_")) return "A secret key must not start with TARIBOY_."
  return ""
}

/**
 * A queue's secrets: keys with their time, Set and Remove. A value is
 * write-only: it lives in the password input until submit, then is cleared,
 * and nothing here ever reads or renders one.
 */
export default function QueueSecrets({ queue, target, missing = [], missingMessage = "" }: {
  queue: string
  target?: ApiTarget
  /** Names a failed bind reported without a value. */
  missing?: string[]
  missingMessage?: string
}) {
  const [secrets, setSecrets] = useState<QueueSecretInfo[]>([])
  const [key, setKey] = useState("")
  const [value, setValue] = useState("")
  const [error, setError] = useState("")
  const [busy, setBusy] = useState(false)
  const { confirm, dialog } = useWorkflowConfirm()

  const load = useCallback(async () => {
    try { setSecrets(await listQueueSecrets(queue, target)) } catch (err) { setError(errorText(err)) }
  }, [queue, target])
  useEffect(() => { void Promise.resolve().then(load) }, [load])

  const submit = async (event: React.FormEvent) => {
    event.preventDefault()
    const name = key.trim()
    const problem = keyProblem(name)
    if (problem) { setError(problem); return }
    const secret = value
    setValue("")
    setError("")
    setBusy(true)
    try {
      await setQueueSecret(queue, name, secret, target)
      setKey("")
      await load()
    } catch (err) { setError(errorText(err)) } finally { setBusy(false) }
  }

  const remove = (name: string) => confirm({
    title: `Remove secret ${name}?`,
    description: "Scripts that read it will fail until it is set again. The value cannot be recovered.",
    action: "Remove secret",
    run: () => {
      setError("")
      setBusy(true)
      removeQueueSecret(queue, name, target).then(load, (err) => setError(errorText(err))).finally(() => setBusy(false))
    },
  })
  const stillMissing = missing.filter((name) => !secrets.some((secret) => secret.key === name))

  return (
    <section className="flex min-w-0 flex-col gap-1.5" aria-label={`Secrets ${queue}`}>
      <div className="flex items-center gap-1.5"><span className={LABEL}>Secrets</span><span className={COUNT}>{secrets.length}</span></div>
      {stillMissing.length > 0 && (
        <div role="alert" aria-label="Secrets needed" className="text-[12px] text-destructive">
          <p>{missingMessage || "These secrets have no value:"}</p>
          <p className="font-mono">{stillMissing.join(", ")}</p>
        </div>
      )}
      {secrets.length === 0 ? <span className={EMPTY}>No secrets.</span> : (
        <ul className="flex min-w-0 flex-col gap-1">
          {secrets.map((secret) => (
            <li key={secret.key} className="flex min-w-0 items-center gap-2 text-[12px]">
              <span className="min-w-0 flex-1 truncate font-mono">{secret.key}</span>
              <span className="text-muted-foreground">{formatTaskTime(secret.updated_at)}</span>
              <button type="button" className={QUIET_ACTION} aria-label={`Remove ${secret.key}`} disabled={busy} onClick={() => remove(secret.key)}>Remove</button>
            </li>
          ))}
        </ul>
      )}
      <form className="flex min-w-0 flex-wrap items-center gap-1.5" autoComplete="off" onSubmit={(event) => void submit(event)}>
        <Input aria-label="Secret key" placeholder="KEY" value={key} className={`${FIELD} h-[26px] min-w-[100px] flex-1`} onChange={(event) => setKey(event.target.value)} />
        <Input aria-label="Secret value" type="password" autoComplete="new-password" placeholder="Value" value={value} className={`${FIELD} h-[26px] min-w-[100px] flex-1`} onChange={(event) => setValue(event.target.value)} />
        <Button type="submit" size="sm" className="h-[26px]" disabled={busy || !key.trim() || !value}>Set</Button>
      </form>
      {error && <p role="alert" className="text-[12px] text-destructive">{error}</p>}
      {dialog}
    </section>
  )
}
