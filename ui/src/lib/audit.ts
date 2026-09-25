import type { Call } from "@/lib/transcript";

export interface AuditEvent { seq: number; kind: string; source: string; data: string; at: string; iteration_id: string }

// parseData turns the JSON-string data field back into an object. On any parse
// failure it returns {} so callers never throw on a malformed record.
export function parseData(data: string): Record<string, unknown> {
  try {
    const v = JSON.parse(data);
    return v && typeof v === "object" ? (v as Record<string, unknown>) : {};
  } catch {
    return {};
  }
}

export function truncate(s: string, n = 120): string {
  return s.length > n ? s.slice(0, n) + "…" : s;
}

export type Tone = "neutral" | "success" | "warn" | "error";

// tone maps a record to a semantic accent used for the left bar. Cosmetic only.
export function tone(kind: string, data: string): Tone {
  if (kind === "error" || kind.endsWith("_error")) return "error";
  if (kind === "iteration_finished" || kind === "iteration_done") {
    const st = parseData(data).status;
    return typeof st === "string" && st.endsWith("_error") ? "error" : "success";
  }
  if (kind.startsWith("budget_")) return "warn";
  return "neutral";
}

export const TONE_BAR: Record<Tone, string> = {
  neutral: "border-l-transparent",
  success: "border-l-green-500",
  warn: "border-l-amber-500",
  error: "border-l-red-500",
};

// decodeJSONStrings walks a value and replaces any string that is itself a JSON
// object/array (e.g. harness_output's data.line) with the parsed value, so the
// pretty view shows real nested JSON instead of an escaped string. Non-JSON
// strings, numbers, and plain scalars are left untouched.
export function decodeJSONStrings(v: unknown): unknown {
  if (typeof v === "string") {
    const t = v.trim();
    if (t.startsWith("{") || t.startsWith("[")) {
      try {
        const parsed = JSON.parse(t);
        if (parsed && typeof parsed === "object") return decodeJSONStrings(parsed);
      } catch {
        /* keep the raw string */
      }
    }
    return v;
  }
  if (Array.isArray(v)) return v.map(decodeJSONStrings);
  if (v && typeof v === "object") {
    const out: Record<string, unknown> = {};
    for (const [k, val] of Object.entries(v as Record<string, unknown>)) out[k] = decodeJSONStrings(val);
    return out;
  }
  return v;
}

// pretty renders the full record as indented JSON, with data parsed back into a
// nested object and any JSON-string fields (data.line) decoded recursively so
// harness JSON shows as real nested JSON, not an escaped string.
export function pretty(e: AuditEvent): string {
  const record = { seq: e.seq, at: e.at, kind: e.kind, source: e.source, data: decodeJSONStrings(parseData(e.data)) };
  return JSON.stringify(record, null, 2);
}

// isBoundary marks the iteration start/finish records that get an accent row.
export function isBoundary(kind: string): boolean {
  return kind === "iteration_started" || kind === "iteration_finished";
}

// hhmmss extracts HH:MM:SS from an ISO timestamp; on any parse failure it
// returns the raw value so the row still renders.
export function hhmmss(at: string): string {
  const m = /T(\d{2}:\d{2}:\d{2})/.exec(at);
  return m ? m[1] : at;
}

// salientArg picks the most informative field of a tool_use input, by tool name,
// falling back to the first string field and finally to compact JSON.
export function salientArg(name: string, input: Record<string, unknown>): string {
  const pick = (k: string) => (typeof input[k] === "string" ? (input[k] as string) : "");
  switch (name) {
    case "Bash": return pick("command");
    case "Read": case "Edit": case "Write": case "NotebookEdit": return pick("file_path");
    case "Grep": case "Glob": return pick("pattern");
    case "Task": return pick("description");
    case "Skill": return pick("skill");
  }
  for (const v of Object.values(input)) if (typeof v === "string") return v;
  return JSON.stringify(input);
}

export interface Descriptor { icon: string; label: string; preview: string; tone: Tone }

// CodexHarnessPresentation is the small, user-facing subset of a Codex JSONL
// item. The original event stays available through pretty(); this only avoids
// making routine commands and messages look like an escaped protocol envelope.
export interface CodexHarnessPresentation {
  type: "command" | "message" | "tool" | "skill" | "thinking" | "file";
  label: string;
  icon: string;
  text: string;
  status?: string;
  output?: string;
}

function stringField(value: unknown): string | undefined {
  return typeof value === "string" && value !== "" ? value : undefined;
}

// codexHarnessPresentation recognizes the item.started/item.completed events
// emitted by the Codex harness. It deliberately returns undefined for every
// other line so generic harness and audit rendering remains unchanged.
export function codexHarnessPresentation(e: AuditEvent): CodexHarnessPresentation | undefined {
  if (e.kind !== "harness_output") return undefined;
  const line = parseData(e.data).line;
  if (typeof line !== "string") return undefined;
  try {
    const outer = JSON.parse(line) as Record<string, unknown>;
    if (outer.type !== "item.started" && outer.type !== "item.completed") return undefined;
    const item = outer.item;
    if (!item || typeof item !== "object") return undefined;
    const record = item as Record<string, unknown>;
    if (record.type === "command_execution") {
      const text = stringField(record.command) ?? stringField(record.cmd);
      if (!text) return undefined;
      const exitCode = record.exit_code;
      const status = stringField(record.status)
        ?? (outer.type === "item.started" ? "running" : typeof exitCode === "number" && exitCode !== 0 ? "failed" : "completed");
      const output = stringField(record.aggregated_output) ?? stringField(record.output) ?? stringField(record.result);
      return { type: "command", label: "command", icon: "⌨", text, status, output };
    }
    if (record.type === "agent_message") {
      const text = stringField(record.text) ?? stringField(record.message);
      return text ? { type: "message", label: "message", icon: "💬", text, status: stringField(record.status) } : undefined;
    }
    if (record.type === "mcp_tool_call") {
      const server = stringField(record.server) ?? stringField(record.server_name) ?? "MCP";
      const tool = stringField(record.tool) ?? stringField(record.name) ?? "tool";
      const args = record.arguments && typeof record.arguments === "object" ? record.arguments as Record<string, unknown> : {};
      const detail = salientArg(tool, args);
      return { type: "tool", label: "Tool", icon: "🔧", text: `${server} · ${tool}${detail && detail !== "{}" ? ` — ${detail}` : ""}`, status: stringField(record.status), output: stringField(record.result) ?? stringField(record.error) };
    }
    if (record.type === "skill") {
      const text = stringField(record.name) ?? stringField(record.skill);
      return text ? { type: "skill", label: "Skill", icon: "✦", text, status: stringField(record.status) } : undefined;
    }
    if (record.type === "reasoning") {
      const text = stringField(record.text) ?? stringField(record.summary);
      return text ? { type: "thinking", label: "Thinking", icon: "🧠", text } : undefined;
    }
    if (record.type === "file_change") {
      const changes = Array.isArray(record.changes) ? record.changes : [];
      const text = changes.map((change) => {
        if (!change || typeof change !== "object") return "";
        const item = change as Record<string, unknown>;
        const path = stringField(item.path) ?? stringField(item.file_path) ?? "";
        const kind = stringField(item.kind) ?? stringField(item.type) ?? "";
        return path ? `${path}${kind ? ` (${kind})` : ""}` : "";
      }).filter(Boolean).join(", ");
      return text ? { type: "file", label: "File change", icon: "📝", text, status: stringField(record.status) } : undefined;
    }
  } catch {
    // Malformed protocol lines retain the existing generic rendering.
  }
  return undefined;
}

// resultText flattens a tool_result content (string, or an array of {text}
// blocks) into a single preview string.
function resultText(content: unknown): string {
  if (typeof content === "string") return content;
  if (Array.isArray(content))
    return content
      .map((b) => (b && typeof b === "object" && typeof (b as Record<string, unknown>).text === "string" ? ((b as Record<string, unknown>).text as string) : ""))
      .join(" ")
      .trim();
  return "";
}

// harnessDescriptor maps a decoded stream-json line to a descriptor.
function harnessDescriptor(inner: Record<string, unknown>): Descriptor {
  const t = inner.type;
  const st = inner.subtype;
  if (t === "assistant") {
    const content = (inner.message as Record<string, unknown> | undefined)?.content;
    const block = Array.isArray(content) ? (content[0] as Record<string, unknown>) : undefined;
    if (block?.type === "text") return { icon: "💬", label: "assistant", preview: truncate(String(block.text ?? "")), tone: "neutral" };
    if (block?.type === "tool_use") {
      const name = String(block.name ?? "tool");
      const input = (block.input && typeof block.input === "object" ? block.input : {}) as Record<string, unknown>;
      return { icon: "🔧", label: name, preview: truncate(salientArg(name, input)), tone: "neutral" };
    }
    // thinking or unknown assistant block — collapseThinking folds thinking away.
    return { icon: "🧠", label: "thinking", preview: "", tone: "neutral" };
  }
  if (t === "user") {
    const content = (inner.message as Record<string, unknown> | undefined)?.content;
    const block = Array.isArray(content) ? (content[0] as Record<string, unknown>) : undefined;
    const isErr = block?.is_error === true;
    return { icon: isErr ? "❌" : "✅", label: "result", preview: truncate(resultText(block?.content)), tone: isErr ? "error" : "neutral" };
  }
  if (t === "system") {
    if (st === "init") return { icon: "⚙", label: "session", preview: truncate(`${inner.model ?? "?"} · ${inner.cwd ?? "?"}`), tone: "neutral" };
    return { icon: "🪝", label: "hook", preview: truncate(String(inner.hook_name ?? st ?? "")), tone: "neutral" };
  }
  if (t === "rate_limit_event") {
    const status = String((inner.rate_limit_info as Record<string, unknown> | undefined)?.status ?? "");
    return { icon: "⏳", label: "rate limit", preview: status, tone: status === "allowed" ? "neutral" : "warn" };
  }
  return { icon: "·", label: String(t ?? "harness"), preview: truncate(JSON.stringify(inner)), tone: "neutral" };
}

// descriptor maps one audit event to its chat-line display form. Never throws.
export function descriptor(e: AuditEvent): Descriptor {
  const d = parseData(e.data);
  const s = (k: string) => (typeof d[k] === "string" ? (d[k] as string) : "");
  switch (e.kind) {
    case "harness_output": {
      try {
        const inner = JSON.parse(s("line")) as Record<string, unknown>;
        return harnessDescriptor(inner);
      } catch {
        return { icon: "·", label: "harness", preview: truncate(s("line")), tone: "neutral" };
      }
    }
    case "iteration_started": return { icon: "▶", label: "iteration", preview: s("trigger"), tone: "neutral" };
    case "iteration_finished": case "iteration_done": return { icon: "⏹", label: "iteration", preview: s("error") ? `${s("status")}: ${truncate(s("error"))}` : s("status"), tone: tone(e.kind, e.data) };
    case "launching_harness": return { icon: "🚀", label: "harness", preview: `${s("harness")} (${d.interactive ? "interactive" : "batch"})`, tone: "neutral" };
    case "harness_spawned": return { icon: "🚀", label: "harness", preview: "spawned", tone: "neutral" };
    case "status": return { icon: "📍", label: "status", preview: s("message"), tone: "neutral" };
    case "shim": case "shim_error": return { icon: "·", label: "shim", preview: truncate(s("line") || s("error")), tone: tone(e.kind, e.data) };
    default: return { icon: "·", label: e.kind, preview: truncate(s("reason") || s("message") || JSON.stringify(d)), tone: tone(e.kind, e.data) };
  }
}

export type DisplayRow =
  | { kind: "event"; key: number; event: AuditEvent }
  | { kind: "thinking"; key: number; events: AuditEvent[]; tokens: number }
  | { kind: "proxycall"; key: number; call: Call; mode: "enrich" | "full" };

// isThinking marks the low-signal reasoning events collapseThinking folds:
// thinking_tokens counters and the redacted (empty-text) assistant thinking
// blocks this harness emits.
function isThinking(e: AuditEvent): boolean {
  if (e.kind !== "harness_output") return false;
  const line = parseData(e.data).line;
  if (typeof line !== "string") return false;
  try {
    const inner = JSON.parse(line) as Record<string, unknown>;
    if (inner.type === "system" && inner.subtype === "thinking_tokens") return true;
    if (inner.type === "assistant") {
      const content = (inner.message as Record<string, unknown> | undefined)?.content;
      const block = Array.isArray(content) ? (content[0] as Record<string, unknown>) : undefined;
      return block?.type === "thinking";
    }
  } catch {
    /* not thinking */
  }
  return false;
}

function thinkingTokens(e: AuditEvent): number {
  try {
    const inner = JSON.parse(String(parseData(e.data).line)) as Record<string, unknown>;
    const n = inner.estimated_tokens;
    return typeof n === "number" && Number.isFinite(n) ? n : 0;
  } catch {
    return 0;
  }
}

// collapseThinking folds each maximal run of consecutive thinking events into a
// single marker row (keyed by the run's first seq, tokens summed). Order is
// preserved; non-thinking events pass through as event rows.
export function collapseThinking(events: AuditEvent[]): DisplayRow[] {
  const out: DisplayRow[] = [];
  let run: AuditEvent[] = [];
  const flush = () => {
    if (run.length) {
      out.push({ kind: "thinking", key: run[0].seq, events: run, tokens: run.reduce((a, e) => a + thinkingTokens(e), 0) });
      run = [];
    }
  };
  for (const e of events) {
    if (isThinking(e)) run.push(e);
    else {
      flush();
      out.push({ kind: "event", key: e.seq, event: e });
    }
  }
  flush();
  return out;
}

// fmtTokens renders a token count compactly: 2100 → "2.1k", 50 → "50".
export function fmtTokens(n: number): string {
  return n >= 1000 ? `${(n / 1000).toFixed(1)}k` : String(n);
}

// hasHarnessOutput reports whether the iteration produced harness stream-json
// (batch agents). Absent for interactive agents, which decides render mode.
export function hasHarnessOutput(events: AuditEvent[]): boolean {
  return events.some((e) => e.kind === "harness_output");
}

// rowTime returns the sort timestamp of a display row.
function rowTime(r: DisplayRow): string {
  if (r.kind === "event") return r.event.at;
  if (r.kind === "thinking") return r.events[0]?.at ?? "";
  return r.call.ts;
}

// mergeProxyCalls interleaves one proxycall row per call into the existing rows,
// ordered by timestamp (stable). Proxycall keys are negative to never collide
// with event seq keys.
export function mergeProxyCalls(rows: DisplayRow[], calls: Call[], mode: "enrich" | "full"): DisplayRow[] {
  const proxyRows: DisplayRow[] = calls.map((call) => ({ kind: "proxycall", key: -(call.seq + 1), call, mode }));
  const all = [...rows, ...proxyRows];
  return all
    .map((r, i) => ({ r, i }))
    .sort((a, b) => {
      const ta = rowTime(a.r), tb = rowTime(b.r);
      const pa = Date.parse(ta), pb = Date.parse(tb);
      if (!Number.isNaN(pa) && !Number.isNaN(pb)) {
        if (pa < pb) return -1;
        if (pa > pb) return 1;
        return a.i - b.i; // stable: preserve original relative order on ties
      }
      if (ta < tb) return -1;
      if (ta > tb) return 1;
      return a.i - b.i; // stable: preserve original relative order on ties
    })
    .map(({ r }) => r);
}
