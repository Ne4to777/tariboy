import { describe, it, expect } from "vitest";
import { tone, pretty, isBoundary, descriptor, salientArg, hhmmss, collapseThinking, fmtTokens, hasHarnessOutput, mergeProxyCalls, type AuditEvent, type DisplayRow } from "./audit";
import type { Call } from "@/lib/transcript";

const ev = (over: Partial<AuditEvent>): AuditEvent =>
  ({ seq: 1, kind: "iteration_started", source: "system", at: "t", data: "{}", iteration_id: "it-1", ...over });

const ho = (inner: Record<string, unknown>): string => JSON.stringify({ line: JSON.stringify(inner) });
const asst = (block: Record<string, unknown>): string => ho({ type: "assistant", message: { content: [block] } });
const usr = (block: Record<string, unknown>): string => ho({ type: "user", message: { content: [block] } });

describe("audit helpers", () => {
  it("tone is error for *_error and success for finished/done", () => {
    expect(tone("iteration_finished", JSON.stringify({ status: "done" }))).toBe("success");
    expect(tone("iteration_finished", JSON.stringify({ status: "harness_error" }))).toBe("error");
    expect(tone("shim_error", "{}")).toBe("error");
  });

  it("isBoundary marks iteration start/finish only", () => {
    expect(isBoundary("iteration_started")).toBe(true);
    expect(isBoundary("iteration_finished")).toBe(true);
    expect(isBoundary("harness_output")).toBe(false);
  });

  it("pretty decodes nested JSON-string fields", () => {
    const out = pretty(ev({ kind: "harness_output", data: JSON.stringify({ line: JSON.stringify({ type: "assistant" }) }) }));
    expect(out).toContain('"type": "assistant"');
    expect(out).not.toContain('\\"type\\"');
  });
});

describe("descriptor", () => {
  it("assistant text → 💬 assistant + text", () => {
    const d = descriptor(ev({ kind: "harness_output", data: asst({ type: "text", text: "Let me check the state" }) }));
    expect(d.icon).toBe("💬");
    expect(d.label).toBe("assistant");
    expect(d.preview).toBe("Let me check the state");
  });

  it("assistant tool_use → 🔧 <ToolName> + salient arg", () => {
    const d = descriptor(ev({ kind: "harness_output", data: asst({ type: "tool_use", name: "Bash", input: { command: "git status" } }) }));
    expect(d.icon).toBe("🔧");
    expect(d.label).toBe("Bash");
    expect(d.preview).toBe("git status");
  });

  it("user tool_result success → ✅ result, neutral", () => {
    const d = descriptor(ev({ kind: "harness_output", data: usr({ type: "tool_result", content: "(Bash completed with no output)", is_error: false }) }));
    expect(d.icon).toBe("✅");
    expect(d.label).toBe("result");
    expect(d.preview).toBe("(Bash completed with no output)");
    expect(d.tone).toBe("neutral");
  });

  it("user tool_result error → ❌ result, error tone", () => {
    const d = descriptor(ev({ kind: "harness_output", data: usr({ type: "tool_result", content: "boom", is_error: true }) }));
    expect(d.icon).toBe("❌");
    expect(d.tone).toBe("error");
  });

  it("tool_result with content array → joins text blocks", () => {
    const d = descriptor(ev({ kind: "harness_output", data: usr({ type: "tool_result", content: [{ type: "text", text: "hello" }] }) }));
    expect(d.preview).toBe("hello");
  });

  it("system/init → ⚙ session with model and cwd", () => {
    const d = descriptor(ev({ kind: "harness_output", data: ho({ type: "system", subtype: "init", model: "claude-opus-4-8", cwd: "/repo" }) }));
    expect(d.icon).toBe("⚙");
    expect(d.label).toBe("session");
    expect(d.preview).toContain("claude-opus-4-8");
    expect(d.preview).toContain("/repo");
  });

  it("system/hook_started → 🪝 hook + hook_name", () => {
    const d = descriptor(ev({ kind: "harness_output", data: ho({ type: "system", subtype: "hook_started", hook_name: "SessionStart:startup" }) }));
    expect(d.icon).toBe("🪝");
    expect(d.preview).toBe("SessionStart:startup");
  });

  it("rate_limit_event allowed → ⏳ neutral, limited → warn", () => {
    const ok = descriptor(ev({ kind: "harness_output", data: ho({ type: "rate_limit_event", rate_limit_info: { status: "allowed" } }) }));
    expect(ok.icon).toBe("⏳");
    expect(ok.tone).toBe("neutral");
    const lim = descriptor(ev({ kind: "harness_output", data: ho({ type: "rate_limit_event", rate_limit_info: { status: "rejected" } }) }));
    expect(lim.tone).toBe("warn");
  });

  it("iteration_started → ▶, iteration_finished success/error tone", () => {
    expect(descriptor(ev({ kind: "iteration_started", data: JSON.stringify({ trigger: "manual" }) })).icon).toBe("▶");
    expect(descriptor(ev({ kind: "iteration_finished", data: JSON.stringify({ status: "done" }) })).tone).toBe("success");
    expect(descriptor(ev({ kind: "iteration_finished", data: JSON.stringify({ status: "harness_error" }) })).tone).toBe("error");
  });

  it("launching_harness / status / shim_error", () => {
    expect(descriptor(ev({ kind: "launching_harness", data: JSON.stringify({ harness: "claude", interactive: false }) })).preview).toBe("claude (batch)");
    expect(descriptor(ev({ kind: "status", data: JSON.stringify({ message: "checking state" }) })).preview).toBe("checking state");
    expect(descriptor(ev({ kind: "shim_error", data: JSON.stringify({ line: "oops" }) })).tone).toBe("error");
  });

  it("malformed data never throws → neutral fallback", () => {
    const d = descriptor(ev({ kind: "harness_output", data: "not json" }));
    expect(d.icon).toBe("·");
    expect(d.tone).toBe("neutral");
  });
});

describe("salientArg", () => {
  it("picks the per-tool field", () => {
    expect(salientArg("Bash", { command: "ls" })).toBe("ls");
    expect(salientArg("Read", { file_path: "/a/b.ts" })).toBe("/a/b.ts");
    expect(salientArg("Grep", { pattern: "foo" })).toBe("foo");
    expect(salientArg("Task", { description: "do it" })).toBe("do it");
    expect(salientArg("Skill", { skill: "brainstorming" })).toBe("brainstorming");
  });
  it("falls back to first string field, then JSON", () => {
    expect(salientArg("Whatever", { x: 1, y: "hi" })).toBe("hi");
    expect(salientArg("Whatever", { n: 5 })).toBe('{"n":5}');
  });
});

describe("hhmmss", () => {
  it("slices the time from an ISO string", () => {
    expect(hhmmss("2026-07-09T09:50:01.170Z")).toBe("09:50:01");
  });
  it("returns the raw value when unparseable", () => {
    expect(hhmmss("t")).toBe("t");
  });
});

const thinkTokens = (n: number): string => JSON.stringify({ line: JSON.stringify({ type: "system", subtype: "thinking_tokens", estimated_tokens: n }) });
const thinkBlock = (): string => JSON.stringify({ line: JSON.stringify({ type: "assistant", message: { content: [{ type: "thinking", thinking: "" }] } }) });

describe("collapseThinking", () => {
  it("empty → []", () => {
    expect(collapseThinking([])).toEqual([]);
  });

  it("passes non-thinking events through as event rows", () => {
    const rows = collapseThinking([ev({ seq: 1, kind: "status", data: JSON.stringify({ message: "hi" }) })]);
    expect(rows).toEqual([{ kind: "event", key: 1, event: expect.objectContaining({ seq: 1 }) }]);
  });

  it("folds a consecutive thinking run into one marker summing tokens", () => {
    const rows = collapseThinking([
      ev({ seq: 1, kind: "status" }),
      ev({ seq: 2, kind: "harness_output", data: thinkTokens(50) }),
      ev({ seq: 3, kind: "harness_output", data: thinkBlock() }),
      ev({ seq: 4, kind: "harness_output", data: thinkTokens(2000) }),
      ev({ seq: 5, kind: "status" }),
    ]);
    expect(rows.map((r) => r.kind)).toEqual(["event", "thinking", "event"]);
    const marker = rows[1] as Extract<DisplayRow, { kind: "thinking" }>;
    expect(marker.key).toBe(2);
    expect(marker.events).toHaveLength(3);
    expect(marker.tokens).toBe(2050);
  });

  it("handles a run at the start and end of the list", () => {
    const rows = collapseThinking([
      ev({ seq: 1, kind: "harness_output", data: thinkTokens(10) }),
      ev({ seq: 2, kind: "status" }),
      ev({ seq: 3, kind: "harness_output", data: thinkTokens(20) }),
    ]);
    expect(rows.map((r) => r.kind)).toEqual(["thinking", "event", "thinking"]);
  });
});

describe("fmtTokens", () => {
  it("compacts thousands", () => {
    expect(fmtTokens(50)).toBe("50");
    expect(fmtTokens(2100)).toBe("2.1k");
  });
});

const call = (seq: number, ts: string): Call =>
  ({ seq, ts, provider: "anthropic", model: "m", instructions: "S", instructions_changed: seq === 0, delta: [], response: { blocks: [] } });

describe("hasHarnessOutput", () => {
  it("true when a harness_output event exists", () => {
    expect(hasHarnessOutput([ev({ seq: 1, kind: "harness_output", at: "2026-07-10T00:00:01Z" })])).toBe(true);
  });
  it("false for interactive (no harness_output)", () => {
    expect(hasHarnessOutput([ev({ seq: 1, kind: "shim", at: "t" })])).toBe(false);
  });
});

describe("mergeProxyCalls", () => {
  it("interleaves proxy calls by timestamp", () => {
    const rows: DisplayRow[] = [
      { kind: "event", key: 1, event: ev({ seq: 1, at: "2026-07-10T00:00:01Z" }) },
      { kind: "event", key: 2, event: ev({ seq: 2, at: "2026-07-10T00:00:03Z" }) },
    ];
    const merged = mergeProxyCalls(rows, [call(0, "2026-07-10T00:00:02Z")], "enrich");
    expect(merged.map((r) => r.kind)).toEqual(["event", "proxycall", "event"]);
    const pc = merged[1];
    if (pc.kind !== "proxycall") throw new Error("expected proxycall");
    expect(pc.mode).toBe("enrich");
    expect(pc.key).toBeLessThan(0);
  });

  it("is stable on ties, preserving original relative order", () => {
    const rows: DisplayRow[] = [
      { kind: "event", key: 1, event: ev({ seq: 1, at: "2026-07-10T00:00:01Z" }) },
      { kind: "event", key: 2, event: ev({ seq: 2, at: "2026-07-10T00:00:01Z" }) },
    ];
    const merged = mergeProxyCalls(rows, [call(0, "2026-07-10T00:00:01Z")], "full");
    expect(merged.map((r) => r.key)).toEqual([1, 2, -1]);
  });

  it("uses events[0].at for thinking rows when sorting", () => {
    const rows: DisplayRow[] = [
      { kind: "thinking", key: 1, events: [ev({ seq: 1, at: "2026-07-10T00:00:01Z" })], tokens: 10 },
    ];
    const merged = mergeProxyCalls(rows, [call(5, "2026-07-10T00:00:00Z")], "full");
    expect(merged.map((r) => r.kind)).toEqual(["proxycall", "thinking"]);
    expect(merged[0].key).toBe(-6);
  });

  it("orders by parsed instant, not lexicographic string, across trimmed vs millis precision", () => {
    // Audit event at exactly the same instant as a proxy call whose timestamp has
    // trimmed (no-fraction) precision: "...02Z" < "...02.000Z" lexicographically
    // even though they're the same instant, so a pure string compare would be
    // order-agnostic between them (fine) but would misplace the .500Z call below
    // "...02Z" since "1" < "2" for the seconds digit only when digits align —
    // here we specifically prove the earlier proxy call (01.500Z) sorts first.
    const rows: DisplayRow[] = [
      { kind: "event", key: 1, event: ev({ seq: 1, at: "2026-07-10T00:00:02.000Z" }) },
    ];
    const merged = mergeProxyCalls(
      rows,
      [call(0, "2026-07-10T00:00:02Z"), call(1, "2026-07-10T00:00:01.500Z")],
      "full",
    );
    // The 01.5s proxy call must sort before both 02s rows.
    expect(merged[0].kind).toBe("proxycall");
    if (merged[0].kind !== "proxycall") throw new Error("expected proxycall");
    expect(merged[0].call.seq).toBe(1);
    // The remaining two (same instant, 02s) keep their original relative order (stable tiebreak).
    expect(merged.slice(1).map((r) => r.kind)).toEqual(["event", "proxycall"]);
  });
});

describe("harness error reasons", () => {
  it("shows the iteration_finished error next to its status", () => {
    const d = descriptor(ev({ kind: "iteration_finished", data: JSON.stringify({ status: "harness_error", error: "spawn shim: chdir /x: no such file or directory" }) }));
    expect(d.preview).toBe("harness_error: spawn shim: chdir /x: no such file or directory");
  });
  it("shows the shim_error error field", () => {
    const d = descriptor(ev({ kind: "shim_error", data: JSON.stringify({ error: "chdir /x: no such file or directory" }) }));
    expect(d.preview).toBe("chdir /x: no such file or directory");
  });
});
