import { afterEach, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { AgentNameContext } from "@/lib/agent";
import type { ScriptDefinition, ScriptRun } from "@/lib/api";
import AgentScripts from "./AgentScripts";

afterEach(() => vi.restoreAllMocks());

const latestRun: ScriptRun = {
  id: "srun-alpha-2", script_id: "scr-alpha-1", agent: "alpha", status: "failed", exit_code: 2,
  created_at: "2026-08-20T10:01:00Z", started_at: "2026-08-20T10:01:01Z", finished_at: "2026-08-20T10:01:03Z", log_path: "/data/agents/alpha/scripts/srun-alpha-2.log",
};
const definition: ScriptDefinition = {
  id: "scr-alpha-1", agent: "alpha", name: "nightly", description: "backup database",
  command: "backup --database primary --destination /mnt/archive/nightly", mode: "every", interval_seconds: 60, state: "active",
  created_at: "2026-08-20T10:00:00Z", next_run_at: "2026-08-20T10:02:00Z",
  latest_run: latestRun,
};
const olderRun = { ...definition.latest_run, id: "srun-alpha-1", status: "succeeded", exit_code: 0, created_at: "2026-08-20T10:00:00Z" };

function stubFetch(calls: Array<{ path: string; method: string; body?: unknown }>, definitions: ScriptDefinition[] = [definition]) {
  vi.stubGlobal("fetch", vi.fn().mockImplementation((path: string, init?: RequestInit) => {
    const method = init?.method ?? "GET";
    calls.push({ path, method, body: init?.body ? JSON.parse(init.body as string) : undefined });
    if (path.endsWith("/download")) return Promise.resolve({ ok: true, status: 200, blob: async () => new Blob(["full log"]) } as Response);
    let result: unknown = { scripts: definitions, count: definitions.length };
    if (path.endsWith("/runs")) result = { runs: [definition.latest_run, olderRun], count: 2 };
    if (path.endsWith("/logs")) result = { run: definition.latest_run, log: "make: checks failed" };
    return Promise.resolve({ ok: true, status: 200, text: async () => JSON.stringify({ ok: true, result }) } as Response);
  }));
}

function renderPage() { return render(<AgentNameContext.Provider value="alpha"><AgentScripts /></AgentNameContext.Provider>); }

it("queues exactly one one-shot run", async () => {
  const calls: Array<{ path: string; method: string; body?: unknown }> = [];
  stubFetch(calls); renderPage();
  await screen.findByRole("button", { name: /nightly/ });
  expect(screen.queryByText("Add script")).not.toBeInTheDocument();
  fireEvent.change(screen.getByLabelText("Name"), { target: { value: "check" } });
  fireEvent.change(screen.getByLabelText("Description"), { target: { value: "check repo" } });
  fireEvent.change(screen.getByLabelText("Command"), { target: { value: "make check" } });
  fireEvent.click(screen.getAllByRole("button", { name: "Run once" }).at(-1)!);
  await waitFor(() => expect(calls.some((call) => call.path === "/api/agents/alpha/scripts/run" && call.method === "POST" && (call.body as { command?: string }).command === "make check")).toBe(true));
});

it("starts a fixed-interval script without a quiet exit setting", async () => {
  const calls: Array<{ path: string; method: string; body?: unknown }> = [];
  stubFetch(calls); renderPage(); await screen.findByRole("button", { name: /nightly/ });
  fireEvent.click(screen.getAllByRole("button", { name: "Schedule" })[0]);
  expect(screen.queryByLabelText("Quiet exit (optional)")).not.toBeInTheDocument();
  expect(screen.getByText("A scheduled run that exits 111 publishes nothing and keeps the schedule running.")).toBeInTheDocument();
  fireEvent.change(screen.getByLabelText("Name"), { target: { value: "watch" } });
  fireEvent.change(screen.getByLabelText("Description"), { target: { value: "watch build" } });
  fireEvent.change(screen.getByLabelText("Command"), { target: { value: "./check-build" } });
  fireEvent.change(screen.getByLabelText("Every (seconds)"), { target: { value: "30" } });
  fireEvent.click(screen.getAllByRole("button", { name: "Schedule" }).at(-1)!);
  await waitFor(() => expect(calls.some((call) => call.path === "/api/agents/alpha/scripts/schedule")).toBe(true));
  const body = calls.find((call) => call.path === "/api/agents/alpha/scripts/schedule")!.body;
  expect(body).toEqual({ script_name: "watch", description: "watch build", command: "./check-build", interval_seconds: 30 });
});

it("shows the complete stored launch command in the script definition", async () => {
  const calls: Array<{ path: string; method: string; body?: unknown }> = [];
  stubFetch(calls); renderPage();

  expect(await screen.findByText("backup --database primary --destination /mnt/archive/nightly")).toBeInTheDocument();
});

it("executes an idle recurring script immediately", async () => {
  const calls: Array<{ path: string; method: string; body?: unknown }> = [];
  stubFetch(calls); renderPage();

  fireEvent.click(await screen.findByRole("button", { name: "Exec" }));

  await waitFor(() => expect(calls.some((call) => call.path === "/api/agents/alpha/scripts/scr-alpha-1/rerun" && call.method === "POST")).toBe(true));
});

it.each([
  ["a completed one-shot", { ...definition, mode: "once", state: "completed", next_run_at: undefined }, true],
  ["a recurring script with a pending run", { ...definition, latest_run: { ...latestRun, status: "pending" } }, false],
  ["a cancelled recurring script", { ...definition, state: "cancelled", next_run_at: undefined }, false],
  ["a stopped recurring script", { ...definition, state: "completed", next_run_at: undefined }, true],
] as const)("shows Exec eligibility for %s", async (_label, candidate, expected) => {
  const calls: Array<{ path: string; method: string; body?: unknown }> = [];
  stubFetch(calls, [candidate]); renderPage();

  await screen.findByRole("button", { name: /nightly/ });

  expect(screen.queryByRole("button", { name: "Exec" }) !== null).toBe(expected);
});

it("lazy-loads runs and expands run metadata and log inline", async () => {
  const calls: Array<{ path: string; method: string; body?: unknown }> = [];
  stubFetch(calls); renderPage();
  const scriptButton = await screen.findByRole("button", { name: /nightly/ });
  expect(calls.filter((call) => call.path.endsWith("/runs"))).toHaveLength(0);
  fireEvent.click(scriptButton);
  await screen.findByRole("button", { name: /srun-alpha-2/ });
  expect(calls.filter((call) => call.path === "/api/agents/alpha/scripts/scr-alpha-1/runs")).toHaveLength(1);
  fireEvent.click(screen.getByRole("button", { name: /srun-alpha-2/ }));
  await screen.findByText("make: checks failed");
  const details = screen.getByText("make: checks failed").parentElement!;
  expect(within(details).getByText("/data/agents/alpha/scripts/srun-alpha-2.log")).toBeInTheDocument();
  expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
});

it("copies the absolute path from the expanded run", async () => {
  const calls: Array<{ path: string; method: string; body?: unknown }> = [];
  const writeText = vi.fn().mockResolvedValue(undefined);
  Object.assign(navigator, { clipboard: { writeText } });
  stubFetch(calls); renderPage();
  fireEvent.click(await screen.findByRole("button", { name: /nightly/ }));
  fireEvent.click(await screen.findByRole("button", { name: /srun-alpha-2/ }));
  await screen.findByText("make: checks failed");
  fireEvent.click(screen.getByRole("button", { name: "Copy path" }));
  await waitFor(() => expect(writeText).toHaveBeenCalledWith("/data/agents/alpha/scripts/srun-alpha-2.log"));
});

it("downloads the complete run log attachment", async () => {
  const calls: Array<{ path: string; method: string; body?: unknown }> = [];
  stubFetch(calls); renderPage();
  fireEvent.click(await screen.findByRole("button", { name: /nightly/ }));
  fireEvent.click(await screen.findByRole("button", { name: /srun-alpha-2/ }));
  await screen.findByText("make: checks failed");
  fireEvent.click(screen.getByRole("button", { name: "Download log" }));
  await waitFor(() => expect(calls.some((call) => call.path === "/api/agents/alpha/script-runs/srun-alpha-2/download")).toBe(true));
});

it("refreshes runs for expanded active scripts", async () => {
  vi.useFakeTimers();
  try {
    const calls: Array<{ path: string; method: string; body?: unknown }> = [];
    stubFetch(calls); renderPage();
    await act(() => vi.advanceTimersByTimeAsync(0));
    fireEvent.click(screen.getByRole("button", { name: /nightly/ }));
    await act(async () => {});
    const runsPath = "/api/agents/alpha/scripts/scr-alpha-1/runs";
    expect(calls.filter((call) => call.path === runsPath)).toHaveLength(1);
    await act(() => vi.advanceTimersByTimeAsync(3000));
    expect(calls.filter((call) => call.path === runsPath)).toHaveLength(2);
  } finally {
    vi.useRealTimers();
  }
});

it("fetches the final log when an expanded active run becomes terminal", async () => {
  vi.useFakeTimers();
  try {
    let runsLoads = 0;
    let logLoads = 0;
    vi.stubGlobal("fetch", vi.fn().mockImplementation((path: string) => {
      let result: unknown = { scripts: [definition], count: 1 };
      if (path.endsWith("/runs")) {
        runsLoads += 1;
        result = { runs: [{ ...definition.latest_run, status: runsLoads === 1 ? "running" : "succeeded", exit_code: runsLoads === 1 ? undefined : 0, finished_at: runsLoads === 1 ? undefined : "2026-08-20T10:01:03Z" }], count: 1 };
      }
      if (path.endsWith("/logs")) {
        logLoads += 1;
        result = { run: { ...definition.latest_run, status: logLoads === 1 ? "running" : "succeeded" }, log: logLoads === 1 ? "partial output" : "partial output\nfinal line" };
      }
      return Promise.resolve({ ok: true, status: 200, text: async () => JSON.stringify({ ok: true, result }) } as Response);
    }));
    renderPage();
    await act(() => vi.advanceTimersByTimeAsync(0));
    fireEvent.click(screen.getByRole("button", { name: /nightly/ }));
    await act(async () => {});
    fireEvent.click(screen.getByRole("button", { name: /srun-alpha-2/ }));
    await act(async () => {});
    expect(screen.getByText("partial output")).toBeInTheDocument();
    await act(() => vi.advanceTimersByTimeAsync(3000));
    expect(screen.getByText(/final line/)).toBeInTheDocument();
    expect(logLoads).toBe(2);
  } finally {
    vi.useRealTimers();
  }
});

it("keeps the trailing terminal refresh when an older poll finishes later", async () => {
  vi.useFakeTimers();
  try {
    let scriptLoads = 0;
    let runsLoads = 0;
    let logLoads = 0;
    let resolveStaleRuns!: (response: Response) => void;
    const staleRuns = new Promise<Response>((resolve) => { resolveStaleRuns = resolve; });
    vi.stubGlobal("fetch", vi.fn().mockImplementation((path: string) => {
      let result: unknown;
      if (path.endsWith("/scripts")) {
        scriptLoads += 1;
        result = { scripts: [{ ...definition, state: scriptLoads === 1 ? "active" : "completed" }], count: 1 };
      } else if (path.endsWith("/runs")) {
        runsLoads += 1;
        if (runsLoads === 2) return staleRuns;
        const running = runsLoads === 1;
        result = { runs: [{ ...definition.latest_run, status: running ? "running" : "succeeded", exit_code: running ? undefined : 0, finished_at: running ? undefined : "2026-08-20T10:01:03Z" }], count: 1 };
      } else if (path.endsWith("/logs")) {
        logLoads += 1;
        result = { run: definition.latest_run, log: logLoads === 1 || logLoads === 3 ? "partial output" : "partial output\nfinal line" };
      }
      return Promise.resolve({ ok: true, status: 200, text: async () => JSON.stringify({ ok: true, result }) } as Response);
    }));
    renderPage();
    await act(() => vi.advanceTimersByTimeAsync(0));
    fireEvent.click(screen.getByRole("button", { name: /nightly/ }));
    await act(async () => {});
    fireEvent.click(screen.getByRole("button", { name: /srun-alpha-2/ }));
    await act(async () => {});
    await act(() => vi.advanceTimersByTimeAsync(3000));
    await vi.waitFor(() => expect(runsLoads).toBe(3));
    await vi.waitFor(() => expect(logLoads).toBe(2));
    await act(async () => {});
    expect(screen.getByText(/final line/)).toBeInTheDocument();
    await act(async () => resolveStaleRuns({ ok: true, status: 200, text: async () => JSON.stringify({ ok: true, result: { runs: [{ ...definition.latest_run, status: "running", exit_code: undefined, finished_at: undefined }], count: 1 } }) } as Response));
    await act(async () => {});
    expect(screen.getByText(/final line/)).toBeInTheDocument();
    expect(runsLoads).toBe(3);
    expect(logLoads).toBe(2);
  } finally {
    vi.useRealTimers();
  }
});

it("keeps the final log when the initial expanded-log request finishes late", async () => {
  vi.useFakeTimers();
  try {
    let scriptLoads = 0;
    let runsLoads = 0;
    let logLoads = 0;
    let resolveInitialLog!: (response: Response) => void;
    const initialLog = new Promise<Response>((resolve) => { resolveInitialLog = resolve; });
    vi.stubGlobal("fetch", vi.fn().mockImplementation((path: string) => {
      let result: unknown;
      if (path.endsWith("/scripts")) {
        scriptLoads += 1;
        result = { scripts: [{ ...definition, state: scriptLoads === 1 ? "active" : "completed" }], count: 1 };
      } else if (path.endsWith("/runs")) {
        runsLoads += 1;
        const running = runsLoads === 1;
        result = { runs: [{ ...definition.latest_run, status: running ? "running" : "succeeded", exit_code: running ? undefined : 0, finished_at: running ? undefined : "2026-08-20T10:01:03Z" }], count: 1 };
      } else if (path.endsWith("/logs")) {
        logLoads += 1;
        if (logLoads === 1) return initialLog;
        result = { run: { ...definition.latest_run, status: "succeeded" }, log: "partial output\nfinal line" };
      }
      return Promise.resolve({ ok: true, status: 200, text: async () => JSON.stringify({ ok: true, result }) } as Response);
    }));
    renderPage();
    await act(() => vi.advanceTimersByTimeAsync(0));
    fireEvent.click(screen.getByRole("button", { name: /nightly/ }));
    await act(async () => {});
    fireEvent.click(screen.getByRole("button", { name: /srun-alpha-2/ }));
    await act(() => vi.advanceTimersByTimeAsync(3000));
    await vi.waitFor(() => expect(logLoads).toBeGreaterThanOrEqual(2));
    await act(async () => {});
    expect(screen.getByText(/final line/)).toBeInTheDocument();
    await act(async () => resolveInitialLog({ ok: true, status: 200, text: async () => JSON.stringify({ ok: true, result: { run: { ...definition.latest_run, status: "running" }, log: "partial output" } }) } as Response));
    await act(async () => {});
    expect(screen.getByText(/final line/)).toBeInTheDocument();
  } finally {
    vi.useRealTimers();
  }
});
