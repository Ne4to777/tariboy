import { Fragment, type FormEvent, type ReactNode, useCallback, useEffect, useRef, useState } from "react";
import { toast } from "sonner";
import { useAgentName } from "@/lib/agent";
import {
  ApiError, cancelAgentScript, downloadAgentScriptLog, getAgentScriptLog,
  listAgentScriptRuns, listAgentScripts, removeAgentScript, rerunAgentScript,
  runAgentScriptOnce, scheduleAgentScript, type ScriptDefinition, type ScriptMode, type ScriptRun,
} from "@/lib/api";
import { fmtDateTime } from "@/lib/time";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import {
  AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent,
  AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle,
} from "@/components/ui/alert-dialog";

const activeRunStatuses = new Set(["pending", "running"]);
const emptyForm = { script_name: "", description: "", command: "", mode: "once" as ScriptMode, interval_seconds: "" };

function when(value?: string) { return value ? fmtDateTime(value) : "—"; }
function errorText(error: unknown) { return error instanceof ApiError ? error.message : String(error); }
function duration(run: ScriptRun) {
  if (!run.started_at || !run.finished_at) return "—";
  const milliseconds = Date.parse(run.finished_at) - Date.parse(run.started_at);
  return milliseconds >= 0 ? `${(milliseconds / 1000).toFixed(1)}s` : "—";
}
function nextGeneration(generations: Record<string, number>, key: string) {
  const generation = (generations[key] ?? 0) + 1;
  generations[key] = generation;
  return generation;
}

export default function AgentScripts() {
  const name = useAgentName();
  const [scripts, setScripts] = useState<ScriptDefinition[]>([]);
  const [runs, setRuns] = useState<Record<string, ScriptRun[]>>({});
  const [expandedScripts, setExpandedScripts] = useState<Set<string>>(new Set());
  const [expandedRuns, setExpandedRuns] = useState<Set<string>>(new Set());
  const [logs, setLogs] = useState<Record<string, string>>({});
  const [form, setForm] = useState(emptyForm);
  const [busy, setBusy] = useState(false);
  const [remove, setRemove] = useState<ScriptDefinition | null>(null);
  const wasPolling = useRef(false);
  const runsGenerations = useRef<Record<string, number>>({});
  const logGenerations = useRef<Record<string, number>>({});

  const load = useCallback(async () => {
    try { setScripts((await listAgentScripts(name)).scripts ?? []); }
    catch (error) { toast.error(`Could not load scripts: ${errorText(error)}`); }
  }, [name]);

  const refreshExpandedRuns = useCallback(async (forceLogs = false) => {
    await Promise.all([...expandedScripts].map(async (scriptID) => {
      const runsGeneration = nextGeneration(runsGenerations.current, scriptID);
      try {
        const result = await listAgentScriptRuns(name, scriptID);
        if (runsGeneration !== runsGenerations.current[scriptID]) return;
        const nextRuns = result.runs ?? [];
        const previousRuns = new Map((runs[scriptID] ?? []).map((run) => [run.id, run]));
        setRuns((current) => ({ ...current, [scriptID]: nextRuns }));
        await Promise.all(nextRuns.filter((run) => expandedRuns.has(run.id) && (forceLogs || activeRunStatuses.has(run.status) || activeRunStatuses.has(previousRuns.get(run.id)?.status ?? ""))).map(async (run) => {
          const logGeneration = nextGeneration(logGenerations.current, run.id);
          const log = await getAgentScriptLog(name, run.id);
          if (logGeneration !== logGenerations.current[run.id]) return;
          setLogs((current) => ({ ...current, [run.id]: log.log }));
        }));
      } catch {
        // Polling is best-effort. An explicit expand still reports load errors.
      }
    }));
  }, [expandedRuns, expandedScripts, name, runs]);

  useEffect(() => {
    const timer = window.setTimeout(() => void load(), 0);
    return () => window.clearTimeout(timer);
  }, [load]);
  const active = scripts.some((definition) => definition.state === "active" || (definition.latest_run && activeRunStatuses.has(definition.latest_run.status)));
  useEffect(() => {
    if (!active) {
      if (wasPolling.current) {
        wasPolling.current = false;
        void refreshExpandedRuns(true);
      }
      return;
    }
    wasPolling.current = true;
    const timer = window.setInterval(() => {
      void load();
      void refreshExpandedRuns();
    }, 3000);
    return () => window.clearInterval(timer);
  }, [active, load, refreshExpandedRuns]);

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    const interval = Number(form.interval_seconds);
    if (form.mode === "every" && (!Number.isInteger(interval) || interval <= 0)) {
      toast.error("Scheduled scripts need a positive interval in seconds"); return;
    }
    setBusy(true);
    try {
      const common = { script_name: form.script_name, description: form.description, command: form.command };
      if (form.mode === "once") await runAgentScriptOnce(name, common);
      else await scheduleAgentScript(name, { ...common, interval_seconds: interval });
      setForm(emptyForm);
      toast.success(form.mode === "once" ? "One-shot run queued" : "Scheduled script started");
      await load();
    } catch (error) { toast.error(`Could not start script: ${errorText(error)}`); }
    finally { setBusy(false); }
  };

  const toggleScript = async (definition: ScriptDefinition) => {
    const open = expandedScripts.has(definition.id);
    setExpandedScripts((current) => { const next = new Set(current); if (open) next.delete(definition.id); else next.add(definition.id); return next; });
    if (!open && !runs[definition.id]) {
      const generation = nextGeneration(runsGenerations.current, definition.id);
      try {
        const result = await listAgentScriptRuns(name, definition.id);
        if (generation !== runsGenerations.current[definition.id]) return;
        setRuns((current) => ({ ...current, [definition.id]: result.runs ?? [] }));
      }
      catch (error) { toast.error(`Could not load runs: ${errorText(error)}`); }
    }
  };

  const toggleRun = async (run: ScriptRun) => {
    const open = expandedRuns.has(run.id);
    setExpandedRuns((current) => { const next = new Set(current); if (open) next.delete(run.id); else next.add(run.id); return next; });
    if (!open && logs[run.id] === undefined) {
      const logGeneration = nextGeneration(logGenerations.current, run.id);
      const runsGeneration = nextGeneration(runsGenerations.current, run.script_id);
      try {
        const result = await getAgentScriptLog(name, run.id);
        if (logGeneration === logGenerations.current[run.id]) {
          setLogs((current) => ({ ...current, [run.id]: result.log }));
        }
        if (runsGeneration === runsGenerations.current[run.script_id]) {
          setRuns((current) => ({ ...current, [run.script_id]: (current[run.script_id] ?? []).map((item) => item.id === run.id ? result.run : item) }));
        }
      } catch (error) { toast.error(`Could not load run log: ${errorText(error)}`); }
    }
  };

  const cancel = async (id: string) => {
    try { await cancelAgentScript(name, id); toast.success("Cancellation requested"); await load(); }
    catch (error) { toast.error(`Could not cancel script: ${errorText(error)}`); }
  };
  const rerun = async (definition: ScriptDefinition) => {
    try { await rerunAgentScript(name, definition.id); nextGeneration(runsGenerations.current, definition.id); toast.success("Run queued"); setRuns((current) => { const next = { ...current }; delete next[definition.id]; return next; }); await load(); }
    catch (error) { toast.error(`Could not execute script: ${errorText(error)}`); }
  };
  const copyPath = async (path: string) => {
    try { await navigator.clipboard.writeText(path); toast.success("Log path copied"); }
    catch (error) { toast.error(`Could not copy path: ${errorText(error)}`); }
  };
  const download = async (run: ScriptRun) => {
    try {
      const blob = await downloadAgentScriptLog(name, run.id);
      const url = URL.createObjectURL(blob);
      const anchor = document.createElement("a");
      anchor.href = url; anchor.download = `${run.id}.log`; anchor.click();
      URL.revokeObjectURL(url);
    } catch (error) { toast.error(`Could not download log: ${errorText(error)}`); }
  };
  const confirmRemove = async () => {
    if (!remove) return;
    try { await removeAgentScript(name, remove.id); toast.success("Script removed"); setRemove(null); await load(); }
    catch (error) { toast.error(`Could not remove script: ${errorText(error)}`); }
  };

  return <div className="mx-auto flex max-w-6xl flex-col gap-6">
    <section className="rounded-lg border p-4">
      <h2 className="text-lg font-semibold">Local scripts</h2>
      <div className="mt-3 flex gap-2" aria-label="Script kind">
        <Button type="button" variant={form.mode === "once" ? "default" : "outline"} aria-pressed={form.mode === "once"} onClick={() => setForm({ ...form, mode: "once" })}>Run once</Button>
        <Button type="button" variant={form.mode === "every" ? "default" : "outline"} aria-pressed={form.mode === "every"} onClick={() => setForm({ ...form, mode: "every" })}>Schedule</Button>
      </div>
      <form className="mt-4 grid gap-3 sm:grid-cols-2" onSubmit={submit}>
        <Field label="Name"><Input required value={form.script_name} onChange={(event) => setForm({ ...form, script_name: event.target.value })} /></Field>
        <Field label="Description"><Input required value={form.description} onChange={(event) => setForm({ ...form, description: event.target.value })} /></Field>
        <Field label="Command" className="sm:col-span-2"><Textarea required value={form.command} onChange={(event) => setForm({ ...form, command: event.target.value })} /></Field>
        {form.mode === "every" && <><Field label="Every (seconds)"><Input required min="1" type="number" value={form.interval_seconds} onChange={(event) => setForm({ ...form, interval_seconds: event.target.value })} /></Field><p className="self-end text-xs text-muted-foreground">A scheduled run that exits 111 publishes nothing and keeps the schedule running.</p></>}
        <div className="sm:col-span-2"><Button disabled={busy} type="submit">{form.mode === "once" ? "Run once" : "Schedule"}</Button></div>
      </form>
    </section>

    <section>
      <h2 className="mb-3 text-lg font-semibold">Scripts</h2>
      {scripts.length === 0 ? <p className="text-sm text-muted-foreground">No scripts yet.</p> : <div className="overflow-x-auto rounded-lg border"><table className="w-full text-left text-sm"><thead className="bg-muted/50 text-muted-foreground"><tr><th className="p-2">Script</th><th className="p-2">Mode</th><th className="p-2">State</th><th className="p-2">Next run</th><th className="p-2">Actions</th></tr></thead><tbody>{scripts.map((definition) => {
        const open = expandedScripts.has(definition.id);
        return <Fragment key={definition.id}><tr className="border-t align-top"><td className="p-2"><Button variant="ghost" className="h-auto px-1 font-medium" aria-expanded={open} aria-controls={`runs-${definition.id}`} onClick={() => void toggleScript(definition)}>{open ? "▾" : "▸"} {definition.name}</Button><div className="max-w-md whitespace-pre-wrap pl-7 text-muted-foreground">{definition.description}</div><div className="max-w-md whitespace-pre-wrap break-words pl-7 text-xs text-muted-foreground"><span className="font-medium text-foreground">Command: </span><code className="font-mono">{definition.command}</code></div></td><td className="p-2">{definition.mode === "every" ? `Every ${definition.interval_seconds}s` : "Once"}</td><td className="p-2">{definition.state}<div className="text-xs text-muted-foreground">latest {definition.latest_run?.status ?? "—"}</div></td><td className="p-2 text-xs text-muted-foreground">{when(definition.next_run_at)}</td><td className="flex flex-wrap gap-1 p-2">{definition.state === "active" && <Button size="sm" variant="outline" onClick={() => void cancel(definition.id)}>Cancel</Button>}{(definition.state === "completed" || (definition.mode === "every" && definition.state === "active" && !activeRunStatuses.has(definition.latest_run?.status ?? ""))) && <Button size="sm" variant="outline" onClick={() => void rerun(definition)}>Exec</Button>}{definition.state !== "active" && <Button size="sm" variant="destructive" onClick={() => setRemove(definition)}>Remove</Button>}</td></tr>{open && <tr className="border-t bg-muted/10"><td colSpan={5} className="p-3"><RunList id={`runs-${definition.id}`} runs={runs[definition.id]} expanded={expandedRuns} logs={logs} onToggle={toggleRun} onCancel={cancel} onCopy={copyPath} onDownload={download} /></td></tr>}</Fragment>;
      })}</tbody></table></div>}
    </section>

    <AlertDialog open={remove !== null} onOpenChange={(open) => { if (!open) setRemove(null); }}><AlertDialogContent><AlertDialogHeader><AlertDialogTitle>Remove script?</AlertDialogTitle><AlertDialogDescription>Remove {remove?.name} and all of its run history. This cannot be undone.</AlertDialogDescription></AlertDialogHeader><AlertDialogFooter><AlertDialogCancel>Keep script</AlertDialogCancel><AlertDialogAction onClick={() => void confirmRemove()}>Remove</AlertDialogAction></AlertDialogFooter></AlertDialogContent></AlertDialog>
  </div>;
}

function RunList({ id, runs, expanded, logs, onToggle, onCancel, onCopy, onDownload }: { id: string; runs?: ScriptRun[]; expanded: Set<string>; logs: Record<string, string>; onToggle: (run: ScriptRun) => Promise<void>; onCancel: (id: string) => Promise<void>; onCopy: (path: string) => Promise<void>; onDownload: (run: ScriptRun) => Promise<void> }) {
  if (!runs) return <p id={id} className="text-sm text-muted-foreground">Loading runs…</p>;
  if (runs.length === 0) return <p id={id} className="text-sm text-muted-foreground">No runs.</p>;
  return <div id={id} className="grid gap-2">{runs.map((run) => { const open = expanded.has(run.id); return <div key={run.id} className="rounded border bg-background"><div className="flex flex-wrap items-center gap-2 p-2"><Button size="sm" variant="ghost" aria-expanded={open} aria-controls={`run-${run.id}`} onClick={() => void onToggle(run)}>{open ? "▾" : "▸"} {run.id}</Button><span>{run.status}{run.cancel_requested ? " (cancelling)" : ""}</span>{run.exit_code !== undefined && <span className="text-muted-foreground">exit {run.exit_code}</span>}<span className="ml-auto text-xs text-muted-foreground">{when(run.finished_at || run.started_at || run.created_at)}</span>{activeRunStatuses.has(run.status) && !run.cancel_requested && <Button size="sm" variant="outline" onClick={() => void onCancel(run.id)}>Cancel run</Button>}</div>{open && <div id={`run-${run.id}`} className="grid gap-3 border-t p-3"><dl className="grid grid-cols-[max-content_1fr] gap-x-3 gap-y-1 text-xs"><dt>Status</dt><dd>{run.status}{run.cancel_requested ? " (cancelling)" : ""}</dd><dt>Exit code</dt><dd>{run.exit_code ?? "—"}</dd><dt>Queued</dt><dd>{when(run.created_at)}</dd><dt>Started</dt><dd>{when(run.started_at)}</dd><dt>Finished</dt><dd>{when(run.finished_at)}</dd><dt>Duration</dt><dd>{duration(run)}</dd><dt>Log path</dt><dd className="break-all font-mono">{run.log_path || "—"}</dd></dl><div className="flex gap-2"><Button size="sm" variant="outline" disabled={!run.log_path} onClick={() => void onCopy(run.log_path ?? "")}>Copy path</Button><Button size="sm" variant="outline" disabled={!run.log_path} onClick={() => void onDownload(run)}>Download log</Button></div><pre className="max-h-96 overflow-auto whitespace-pre-wrap rounded bg-muted p-3 text-xs">{logs[run.id] ?? "Loading log…"}</pre></div>}</div>; })}</div>;
}

function Field({ label, className = "", children }: { label: string; className?: string; children: ReactNode }) {
  return <label className={`grid gap-1 text-sm ${className}`}><Label>{label}</Label>{children}</label>;
}
