import { useEffect, useRef } from "react"

/** Helpers shared by the workflow panel and the queue workflow settings. */

/** The message of a failed request, as a person reads it. */
export function errorText(error: unknown): string {
  return error instanceof Error ? error.message : String(error)
}

/** A ref that is true while the component is mounted, for async callbacks. */
export function useMounted() {
  const mounted = useRef(true)
  useEffect(() => {
    mounted.current = true
    return () => { mounted.current = false }
  }, [])
  return mounted
}
