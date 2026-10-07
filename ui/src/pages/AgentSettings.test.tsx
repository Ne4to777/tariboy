import { it, expect, vi, afterEach } from "vitest";
import {
  render,
  screen,
  fireEvent,
  waitFor,
  within,
} from "@testing-library/react";
import { toast } from "sonner";
import { AgentNameContext } from "@/lib/agent";
import type { Daemon } from "@/lib/daemons";
import AgentSettings from "./AgentSettings";

vi.mock("sonner", () => ({
  toast: { success: vi.fn(), error: vi.fn() },
}));

afterEach(() => {
  vi.restoreAllMocks();
  vi.clearAllMocks();
});

const view = {
  name: "alpha",
  image: "img:1",
  digest: "",
  state: "running",
  cwd: "",
  harness: "claude",
  model: "",
  effort: "",
  interactive: false,
  loop_enabled: true,
  interval_s: 30,
  timeout_s: 60,
  hard_timeout_s: 120,
  on_timeout: "restart",
  on_error: "restart",
  max_idle_iterations: 0,
  ai_stall_timeout_s: 300,
  user_prompt: "hi",
  env: {},
  plugins: [],
  messages_batch: 10,
  messages_max_queue: 100,
  group: null,
  goal_enabled: true,
  goal_wait_customer_timeout_s: 300,
  goal_delivery_cooldown_s: 60,
  current_goal_task_key: "",
  alias: "",
  notes: "",
};

type Call = {
  path: string;
  method?: string;
  body: unknown;
  headers?: HeadersInit;
};

// An accepted write is visible to the next read, exactly as a real daemon
// behaves — otherwise a reload would "reconcile" acknowledged values back to
// their old ones and the tests would be measuring the fixture, not the page.
const SERVER_FIELD: Record<string, string> = {
  "/loop/interval": "interval_s",
  "/loop/timeout": "timeout_s",
  "/loop/hard-timeout": "hard_timeout_s",
  "/loop/on-timeout": "on_timeout",
  "/loop/on-error": "on_error",
  "/loop/max-idle": "max_idle_iterations",
  "/loop/ai-stall-timeout": "ai_stall_timeout_s",
  "/model": "model",
  "/effort": "effort",
  "/messages/batch": "messages_batch",
  "/messages/max-queue": "messages_max_queue",
};

// stubFetch answers every request this page makes from one mutable server-side
// view. `fail` marks the paths that reject, so a test can put a failure exactly
// in the middle of a batched save; `apply` runs after the default write-back so
// a test can model server-side normalization.
function stubFetch(
  calls: Call[],
  opts?: {
    view?: Record<string, unknown>;
    fail?: (path: string) => boolean;
    apply?: (
      server: Record<string, unknown>,
      path: string,
      body: unknown,
    ) => void;
  },
) {
  const server: Record<string, unknown> = { ...view, ...(opts?.view ?? {}) };
  let shellScript = String(server.shell_script ?? "");
  vi.stubGlobal(
    "fetch",
    vi.fn().mockImplementation((path: string, init?: RequestInit) => {
      const body = init?.body ? JSON.parse(init.body as string) : undefined;
      if (init?.method)
        calls.push({ path, method: init.method, body, headers: init.headers });
      if (init?.method === "POST" && opts?.fail?.(path)) {
        return Promise.resolve({
          ok: false,
          status: 400,
          text: async () =>
            JSON.stringify({
              ok: false,
              error: { code: "invalid", message: "server rejected it" },
            }),
        } as Response);
      }
      if (init?.method === "POST") {
        if (path.endsWith("/shell-script")) {
          shellScript = (body as { script: string }).script;
        }
        const key = Object.keys(SERVER_FIELD).find((suffix) =>
          path.endsWith(suffix),
        );
        if (key) server[SERVER_FIELD[key]] = (body as { value: unknown }).value;
        if (path.endsWith("/goal-enabled"))
          server.goal_enabled = (body as { enabled: boolean }).enabled;
        if (path.endsWith("/goal-wait-customer-timeout")) {
          server.goal_wait_customer_timeout_s = (
            body as { seconds: number }
          ).seconds;
        }
        if (path.endsWith("/goal-delivery-cooldown"))
          server.goal_delivery_cooldown_s = (
            body as { seconds: number }
          ).seconds;
        opts?.apply?.(server, path, body);
      }
      let result: unknown = server;
      if (path.endsWith("/shell-script")) result = init?.method === "POST" ? { saved: true } : { script: shellScript };
      else if (path.endsWith("/secrets")) result = { keys: [], count: 0 };
      else if (path.endsWith("/retention"))
        result = {
          keep_iterations: 0,
          keep_days: 0,
          max_bytes: 0,
          archive: false,
        };
      else if (path.endsWith("/prompt"))
        result = {
          name: "alpha",
          prompt: "assembled prompt text",
          layers: [{ name: "system", sha256: "abc123def456" }],
        };
      else if (path.endsWith("/user-prompt"))
        result = { name: "alpha", user_prompt: "hi" };
      else if (path.endsWith("/context"))
        result = { name: "alpha", context: "some context" };
      return Promise.resolve({
        ok: true,
        status: 200,
        text: async () => JSON.stringify({ ok: true, result }),
      } as Response);
    }),
  );
}

const renderPage = (target?: Daemon | null) =>
  render(
    <AgentNameContext.Provider value="alpha">
      <AgentSettings target={target} />
    </AgentNameContext.Provider>,
  );

// posts narrows a recorded call list to the writes, which is what the batched
// save contract is stated in: which fields were sent, and in which order.
const posts = (calls: Call[]) =>
  calls.filter((c) => c.method === "POST").map((c) => c.path);

it("saves Goal settings serially on the explicit host", async () => {
  const target: Daemon = {
    id: "remote",
    label: "Remote",
    baseURL: "https://remote.test",
    token: "secret",
  };
  const calls: Call[] = [];
  stubFetch(calls, { view: { current_goal_task_key: "TARI-43" } });
  renderPage(target);

  const enabled = await screen.findByRole("switch", { name: "Enable Goal" });
  fireEvent.click(enabled);
  fireEvent.change(screen.getByLabelText("Wait customer timeout seconds"), {
    target: { value: "120" },
  });
  fireEvent.change(screen.getByLabelText("Goal delivery cooldown seconds"), {
    target: { value: "90" },
  });
  fireEvent.click(screen.getByRole("button", { name: "Save Goal settings" }));

  await waitFor(() =>
    expect(posts(calls)).toEqual([
      "https://remote.test/api/agents/alpha/goal-enabled",
      "https://remote.test/api/agents/alpha/goal-wait-customer-timeout",
      "https://remote.test/api/agents/alpha/goal-delivery-cooldown",
    ]),
  );
  expect(
    calls.filter((call) => call.method === "POST").map((call) => call.body),
  ).toEqual([{ enabled: false }, { seconds: 120 }, { seconds: 90 }]);
  expect(calls.find((call) => call.method === "POST")?.headers).toMatchObject({
    Authorization: "Bearer secret",
  });
});

it("loads and saves an agent shell script on the explicit host", async () => {
  const target: Daemon = {
    id: "remote",
    label: "Remote",
    baseURL: "https://remote.test",
    token: "secret",
  };
  const calls: Call[] = [];
  stubFetch(calls, { view: { shell_script: "export OLD=1" } });
  renderPage(target);

  const script = await screen.findByLabelText("Agent Shell Script");
  await waitFor(() => expect(script).toHaveValue("export OLD=1"));
  fireEvent.change(script, { target: { value: "  export NEW=1\n  printf '%s' \"$NEW\"\n" } });
  fireEvent.click(
    screen.getByRole("button", { name: "Save agent shell script" }),
  );

  await waitFor(() =>
    expect(posts(calls)).toContain(
      "https://remote.test/api/agents/alpha/shell-script",
    ),
  );
  expect(
    calls.find(
      (call) => call.method === "POST" && call.path.endsWith("/shell-script"),
    )?.body,
  ).toEqual({
    script: "  export NEW=1\n  printf '%s' \"$NEW\"\n",
  });
});

it.each(["0", "1.5"])(
  "rejects Goal timeout %s before saving",
  async (value) => {
    const calls: Call[] = [];
    stubFetch(calls);
    renderPage();

    const timeout = await screen.findByLabelText(
      "Wait customer timeout seconds",
    );
    fireEvent.change(timeout, { target: { value } });
    fireEvent.click(screen.getByRole("button", { name: "Save Goal settings" }));

    expect(await screen.findByRole("alert")).toHaveTextContent(
      "Enter a positive whole number of seconds.",
    );
    expect(posts(calls)).toEqual([]);
  },
);

it.each(["0", "1.5"])(
  "rejects Goal delivery cooldown %s before saving",
  async (value) => {
    const calls: Call[] = [];
    stubFetch(calls);
    renderPage();

    const cooldown = await screen.findByLabelText(
      "Goal delivery cooldown seconds",
    );
    fireEvent.change(cooldown, { target: { value } });
    fireEvent.click(screen.getByRole("button", { name: "Save Goal settings" }));

    expect(await screen.findByRole("alert")).toHaveTextContent(
      "Enter a positive whole number of seconds.",
    );
    expect(posts(calls)).toEqual([]);
  },
);

it("discards a Goal timeout edit back to the loaded value", async () => {
  const calls: Call[] = [];
  stubFetch(calls);
  renderPage();

  const timeout = await screen.findByLabelText("Wait customer timeout seconds");
  fireEvent.change(timeout, { target: { value: "120" } });
  fireEvent.click(screen.getByRole("button", { name: "Discard changes" }));

  expect(timeout).toHaveValue(300);
  expect(
    screen.queryByRole("button", { name: "Save Goal settings" }),
  ).not.toBeInTheDocument();
  expect(posts(calls)).toEqual([]);
});

it("keeps only the failed Goal field dirty after the second save fails", async () => {
  const calls: Call[] = [];
  stubFetch(calls, {
    fail: (path) => path.endsWith("/goal-wait-customer-timeout"),
  });
  render(
    <AgentNameContext.Provider value="alpha">
      <AgentSettings />
    </AgentNameContext.Provider>,
  );

  fireEvent.click(await screen.findByRole("switch", { name: "Enable Goal" }));
  const timeout = screen.getByLabelText("Wait customer timeout seconds");
  fireEvent.change(timeout, { target: { value: "120" } });
  fireEvent.click(screen.getByRole("button", { name: "Save Goal settings" }));

  expect(
    await screen.findByText(
      "Some changes were not saved. Review the highlighted fields and try again.",
    ),
  ).toBeInTheDocument();
  expect(posts(calls)).toEqual([
    "/api/agents/alpha/goal-enabled",
    "/api/agents/alpha/goal-wait-customer-timeout",
  ]);
  expect(screen.getByRole("switch", { name: "Enable Goal" })).toHaveAttribute(
    "aria-checked",
    "false",
  );
  expect(timeout).toHaveAttribute("aria-invalid", "true");
  fireEvent.change(timeout, { target: { value: "300" } });
  expect(
    screen.queryByRole("button", { name: "Save Goal settings" }),
  ).not.toBeInTheDocument();
});

it("renders the current Goal task key in a disabled input", async () => {
  const calls: Call[] = [];
  stubFetch(calls, { view: { current_goal_task_key: "TARI-43" } });
  renderPage();

  expect(await screen.findByLabelText("Current goal task")).toHaveValue(
    "TARI-43",
  );
  expect(screen.getByLabelText("Current goal task")).toBeDisabled();
});

it("explains how Goal is selected from the Goal section", async () => {
  const calls: Call[] = [];
  stubFetch(calls);
  renderPage();

  fireEvent.click(await screen.findByRole("button", { name: "How Goal is selected" }));
  expect(await screen.findByRole("dialog")).toHaveTextContent(
    "Tariboy keeps the selected task until it is released",
  );
});

it("edits a loop interval via POST loop/interval", async () => {
  const calls: Call[] = [];
  stubFetch(calls);
  renderPage();

  await waitFor(() =>
    expect(screen.getByLabelText("Interval")).toBeInTheDocument(),
  );
  fireEvent.change(screen.getByLabelText("Interval"), {
    target: { value: "45" },
  });
  fireEvent.click(screen.getByText("Save loop settings"));
  await waitFor(() =>
    expect(posts(calls)).toEqual(["/api/agents/alpha/loop/interval"]),
  );
  expect(
    (calls.find((c) => c.method === "POST")?.body as { value?: number })?.value,
  ).toBe(45);
  // The per-field commit points are gone: the section has one save, not six.
  expect(screen.queryByText("Set")).not.toBeInTheDocument();
});

it("saves Messages and Channels settings serially", async () => {
  const calls: Call[] = [];
  stubFetch(calls);
  renderPage();

  fireEvent.change(await screen.findByLabelText("Messages per iteration"), {
    target: { value: "12" },
  });
  fireEvent.change(screen.getByLabelText("Maximum pending queue"), {
    target: { value: "80" },
  });
  fireEvent.click(screen.getByRole("button", { name: "Save message settings" }));

  await waitFor(() => expect(posts(calls)).toEqual([
    "/api/agents/alpha/messages/batch",
    "/api/agents/alpha/messages/max-queue",
  ]));
  expect(screen.queryByRole("button", { name: "Save message settings" })).not.toBeInTheDocument();
});

it("rejects non-positive message settings before saving", async () => {
  const calls: Call[] = [];
  stubFetch(calls);
  renderPage();

  fireEvent.change(await screen.findByLabelText("Maximum pending queue"), {
    target: { value: "0" },
  });
  fireEvent.click(screen.getByRole("button", { name: "Save message settings" }));

  expect(await screen.findByText("Enter a positive whole number.")).toBeInTheDocument();
  expect(posts(calls)).toEqual([]);
});

it("sets max idle iterations via POST loop/max-idle", async () => {
  const calls: Call[] = [];
  stubFetch(calls);
  renderPage();

  const input = await screen.findByLabelText("Maximum idle iterations");
  fireEvent.change(input, { target: { value: "3" } });
  fireEvent.click(screen.getByText("Save loop settings"));
  await waitFor(() =>
    expect(posts(calls)).toEqual(["/api/agents/alpha/loop/max-idle"]),
  );
  expect(
    (calls.find((c) => c.method === "POST")?.body as { value?: number })?.value,
  ).toBe(3);
});

it("saves model via POST /model", async () => {
  const calls: Call[] = [];
  stubFetch(calls);
  renderPage();

  await waitFor(() =>
    expect(screen.getByLabelText("Model")).toBeInTheDocument(),
  );
  fireEvent.change(screen.getByLabelText("Model"), {
    target: { value: "opus" },
  });
  fireEvent.click(screen.getByText("Save runtime settings"));
  await waitFor(() =>
    expect(posts(calls)).toEqual(["/api/agents/alpha/model"]),
  );
  expect(
    (calls.find((c) => c.method === "POST")?.body as { value?: string })?.value,
  ).toBe("opus");
});

it("shows no unsaved footer until a section is dirty, and only for that section", async () => {
  const calls: Call[] = [];
  stubFetch(calls);
  renderPage();

  await waitFor(() =>
    expect(screen.getByLabelText("Interval")).toBeInTheDocument(),
  );
  expect(screen.queryByText("Unsaved changes")).not.toBeInTheDocument();
  expect(screen.queryByText("Save loop settings")).not.toBeInTheDocument();
  expect(screen.queryByText("Save runtime settings")).not.toBeInTheDocument();

  fireEvent.change(screen.getByLabelText("Interval"), {
    target: { value: "45" },
  });
  expect(screen.getAllByText("Unsaved changes")).toHaveLength(1);
  expect(screen.getByText("Save loop settings")).toBeEnabled();
  // Runtime owns a separate draft, so a Loop edit leaves it clean.
  expect(screen.queryByText("Save runtime settings")).not.toBeInTheDocument();

  // Typing the loaded value back is not a change: the section goes clean again.
  fireEvent.change(screen.getByLabelText("Interval"), {
    target: { value: "30" },
  });
  expect(screen.queryByText("Unsaved changes")).not.toBeInTheDocument();
  expect(calls.some((c) => c.method === "POST")).toBe(false);
});

it("discards loop edits back to the loaded baseline without writing", async () => {
  const calls: Call[] = [];
  stubFetch(calls);
  renderPage();

  await waitFor(() =>
    expect(screen.getByLabelText("Interval")).toBeInTheDocument(),
  );
  fireEvent.change(screen.getByLabelText("Interval"), {
    target: { value: "45" },
  });
  fireEvent.change(screen.getByLabelText("On error"), {
    target: { value: "stop" },
  });
  fireEvent.click(screen.getByText("Discard changes"));

  expect(screen.getByLabelText("Interval")).toHaveValue("30");
  expect(screen.getByLabelText("On error")).toHaveValue("restart");
  expect(screen.queryByText("Unsaved changes")).not.toBeInTheDocument();
  expect(await screen.findByText("Changes discarded")).toBeInTheDocument();
  expect(calls.some((c) => c.method === "POST")).toBe(false);
});

it("sends only the changed loop fields, serially, in the section's render order", async () => {
  const calls: Call[] = [];
  stubFetch(calls);
  renderPage();

  await waitFor(() =>
    expect(screen.getByLabelText("Interval")).toBeInTheDocument(),
  );
  // Edited out of render order on purpose: the request order is the section's,
  // not the operator's.
  fireEvent.change(screen.getByLabelText("Maximum idle iterations"), {
    target: { value: "4" },
  });
  fireEvent.change(screen.getByLabelText("On error"), {
    target: { value: "stop" },
  });
  fireEvent.change(screen.getByLabelText("Interval"), {
    target: { value: "45" },
  });
  fireEvent.click(screen.getByText("Save loop settings"));

  await waitFor(() =>
    expect(screen.queryByText("Unsaved changes")).not.toBeInTheDocument(),
  );
  expect(posts(calls)).toEqual([
    "/api/agents/alpha/loop/interval",
    "/api/agents/alpha/loop/on-error",
    "/api/agents/alpha/loop/max-idle",
  ]);
});

it("saves the per-agent AI stall timeout with the Loop settings", async () => {
  const calls: Call[] = [];
  stubFetch(calls);
  renderPage();

  const input = await screen.findByLabelText("AI inactivity timeout");
  expect(input).toHaveValue(300);
  fireEvent.change(input, { target: { value: "420" } });
  fireEvent.click(screen.getByText("Save loop settings"));

  await waitFor(() =>
    expect(screen.queryByText("Unsaved changes")).not.toBeInTheDocument(),
  );
  expect(posts(calls)).toEqual(["/api/agents/alpha/loop/ai-stall-timeout"]);
});

it("reloads after a successful loop save and adopts the server's canonical values", async () => {
  const calls: Call[] = [];
  // The server clamps interval to a 60s floor, so the reload must win over the
  // draft the operator typed.
  stubFetch(calls, {
    apply: (server, path, body) => {
      if (path.endsWith("/loop/interval")) {
        server.interval_s = Math.max(
          60,
          Number((body as { value: number }).value),
        );
      }
    },
  });
  renderPage();

  await waitFor(() =>
    expect(screen.getByLabelText("Interval")).toBeInTheDocument(),
  );
  fireEvent.change(screen.getByLabelText("Interval"), {
    target: { value: "45" },
  });
  fireEvent.click(screen.getByText("Save loop settings"));

  expect(await screen.findByText("Loop settings saved")).toBeInTheDocument();
  await waitFor(() =>
    expect(screen.getByLabelText("Interval")).toHaveValue("60"),
  );
  // Baseline moved with it: the adopted value is not reported as unsaved work.
  expect(screen.queryByText("Unsaved changes")).not.toBeInTheDocument();
  expect(
    calls.filter((c) => c.method === "GET" && c.path === "/api/agents/alpha")
      .length,
  ).toBeGreaterThan(0);
});

it("stops a loop save at the failing field and keeps the rest dirty", async () => {
  const calls: Call[] = [];
  stubFetch(calls, { fail: (path) => path.endsWith("/loop/timeout") });
  renderPage();

  await waitFor(() =>
    expect(screen.getByLabelText("Interval")).toBeInTheDocument(),
  );
  fireEvent.change(screen.getByLabelText("Interval"), {
    target: { value: "45" },
  });
  fireEvent.change(screen.getByLabelText("Timeout"), {
    target: { value: "90" },
  });
  fireEvent.change(screen.getByLabelText("Maximum idle iterations"), {
    target: { value: "4" },
  });
  fireEvent.click(screen.getByText("Save loop settings"));

  expect(
    await screen.findByText(
      "Some changes were not saved. Review the highlighted fields and try again.",
    ),
  ).toBeInTheDocument();
  // The third field is NEVER attempted: the fan-out stops at the failure.
  expect(posts(calls)).toEqual([
    "/api/agents/alpha/loop/interval",
    "/api/agents/alpha/loop/timeout",
  ]);
  // The acknowledged field keeps its saved value; the failed and unattempted
  // fields keep the operator's drafts and the footer stays up for a retry.
  expect(screen.getByLabelText("Interval")).toHaveValue("45");
  expect(screen.getByLabelText("Timeout")).toHaveValue("90");
  expect(screen.getByLabelText("Maximum idle iterations")).toHaveValue(4);
  expect(screen.getByText("Unsaved changes")).toBeInTheDocument();
  // The failed field is identified accessibly, and carries the server's reason.
  const failed = screen.getByLabelText("Timeout");
  const described = failed.getAttribute("aria-describedby") ?? "";
  expect(described).not.toBe("");
  const errorNode = described
    .split(" ")
    .map((id) => document.getElementById(id))
    .find((n) => n?.textContent?.includes("server rejected it"));
  expect(errorNode).toBeTruthy();
  expect(screen.getByLabelText("Interval")).not.toHaveAttribute(
    "aria-invalid",
    "true",
  );
});

it("retries a partially failed loop save with only the remaining dirty fields", async () => {
  const calls: Call[] = [];
  let failing = true;
  stubFetch(calls, {
    fail: (path) => failing && path.endsWith("/loop/timeout"),
  });
  renderPage();

  await waitFor(() =>
    expect(screen.getByLabelText("Interval")).toBeInTheDocument(),
  );
  fireEvent.change(screen.getByLabelText("Interval"), {
    target: { value: "45" },
  });
  fireEvent.change(screen.getByLabelText("Timeout"), {
    target: { value: "90" },
  });
  fireEvent.change(screen.getByLabelText("Maximum idle iterations"), {
    target: { value: "4" },
  });
  fireEvent.click(screen.getByText("Save loop settings"));
  await waitFor(() => expect(posts(calls)).toHaveLength(2));

  failing = false;
  calls.length = 0;
  fireEvent.click(screen.getByText("Save loop settings"));
  await waitFor(() =>
    expect(posts(calls)).toEqual([
      "/api/agents/alpha/loop/timeout",
      "/api/agents/alpha/loop/max-idle",
    ]),
  );
});

it("saves only the changed runtime field and leaves loop alone", async () => {
  const calls: Call[] = [];
  stubFetch(calls, { view: { model: "opus", effort: "low" } });
  renderPage();

  await waitFor(() =>
    expect(screen.getByLabelText("Effort")).toBeInTheDocument(),
  );
  fireEvent.change(screen.getByLabelText("Effort"), {
    target: { value: "high" },
  });
  fireEvent.click(screen.getByText("Save runtime settings"));

  expect(await screen.findByText("Runtime settings saved")).toBeInTheDocument();
  expect(posts(calls)).toEqual(["/api/agents/alpha/effort"]);
  expect(
    (calls.find((c) => c.method === "POST")?.body as { value?: string })?.value,
  ).toBe("high");
});

it("gives the eight batched fields and Secrets the next-iteration timing helper", async () => {
  const calls: Call[] = [];
  stubFetch(calls);
  renderPage();

  await waitFor(() =>
    expect(screen.getByLabelText("Interval")).toBeInTheDocument(),
  );
  // Eight batched fields plus the Secrets store/removal helper.
  expect(
    screen.getAllByText(/Takes effect on the next iteration\.$/),
  ).toHaveLength(9);
});

it("changing harness saves it without restarting and explains when it takes effect", async () => {
  const calls: Call[] = [];
  stubFetch(calls);
  renderPage();

  const select = await screen.findByLabelText("Harness");
  expect((select as HTMLSelectElement).value).toBe("claude");
  fireEvent.change(select, { target: { value: "codex" } });
  await waitFor(() =>
    expect(
      calls.some(
        (c) =>
          c.path === "/api/agents/alpha/harness" &&
          (c.body as { value?: string })?.value === "codex",
      ),
    ).toBe(true),
  );
  expect(calls.some((c) => c.path === "/api/agents/alpha/restart")).toBe(false);
  expect(
    screen.getAllByText(
      "Saved immediately. Takes effect the next time the agent starts.",
    ),
  ).toHaveLength(2);
  expect(
    screen.getByText("Restart the agent yourself when you're ready."),
  ).toBeInTheDocument();
});

it("a failed harness save keeps the existing error behavior and does not restart", async () => {
  const calls: Call[] = [];
  stubFetch(calls, { fail: (path) => path.endsWith("/harness") });
  renderPage();

  const select = await screen.findByLabelText("Harness");
  fireEvent.change(select, { target: { value: "codex" } });

  await waitFor(() =>
    expect(toast.error).toHaveBeenCalledWith(
      "harness failed: server rejected it",
    ),
  );
  expect(calls.some((c) => c.path === "/api/agents/alpha/harness")).toBe(true);
  expect(calls.some((c) => c.path === "/api/agents/alpha/restart")).toBe(false);
});

it("harness dropdown offers exactly claude/codex/opencode/cursor (no test-only stub)", async () => {
  const calls: Call[] = [];
  stubFetch(calls);
  renderPage();

  const select = await screen.findByLabelText("Harness");
  const opts = Array.from((select as HTMLSelectElement).options).map(
    (o) => o.value,
  );
  expect(opts).toEqual(["claude", "codex", "opencode", "cursor"]);
});

it("harness dropdown still renders an agent's current out-of-list harness (stub)", async () => {
  const calls: Call[] = [];
  stubFetch(calls, { view: { harness: "stub" } });
  renderPage();

  const select = await screen.findByLabelText("Harness");
  expect((select as HTMLSelectElement).value).toBe("stub");
  const opts = Array.from((select as HTMLSelectElement).options).map(
    (o) => o.value,
  );
  expect(opts).toEqual(["claude", "codex", "opencode", "cursor", "stub"]);
});

it("toggling interactive saves the boolean without restarting and explains when it takes effect", async () => {
  const calls: Call[] = [];
  stubFetch(calls);
  renderPage();

  await waitFor(() =>
    expect(screen.getByLabelText("Interactive (tmux TUI)")).toBeInTheDocument(),
  );
  fireEvent.click(screen.getByLabelText("Interactive (tmux TUI)"));
  await waitFor(() =>
    expect(
      calls.some(
        (c) =>
          c.path === "/api/agents/alpha/interactive" &&
          (c.body as { value?: boolean })?.value === true,
      ),
    ).toBe(true),
  );
  expect(calls.some((c) => c.path === "/api/agents/alpha/restart")).toBe(false);
  expect(
    screen.getAllByText(
      "Saved immediately. Takes effect the next time the agent starts.",
    ),
  ).toHaveLength(2);
  expect(
    screen.getByText("Restart the agent yourself when you're ready."),
  ).toBeInTheDocument();
});

it("puts harness and interactive in a labelled next-start subregion", async () => {
  const calls: Call[] = [];
  stubFetch(calls);
  renderPage();

  const region = await screen.findByRole("region", {
    name: "Next-start settings",
  });
  expect(
    within(region).getByText("Restart the agent yourself when you're ready."),
  ).toBeInTheDocument();
  expect(within(region).getByLabelText("Harness")).toBeInTheDocument();
  expect(
    within(region).getByLabelText("Interactive (tmux TUI)"),
  ).toBeInTheDocument();
  expect(
    within(region).getAllByText(
      "Saved immediately. Takes effect the next time the agent starts.",
    ),
  ).toHaveLength(2);
  // They are NOT part of the batched Runtime draft: touching them raises no
  // unsaved footer.
  expect(screen.queryByText("Unsaved changes")).not.toBeInTheDocument();
});

it("a failed interactive save keeps the existing error behavior and does not restart", async () => {
  const calls: Call[] = [];
  stubFetch(calls, { fail: (path) => path.endsWith("/interactive") });
  renderPage();

  const toggle = await screen.findByLabelText("Interactive (tmux TUI)");
  expect(toggle).toHaveAttribute("aria-checked", "false");
  fireEvent.click(toggle);

  await waitFor(() =>
    expect(toast.error).toHaveBeenCalledWith(
      "interactive failed: server rejected it",
    ),
  );
  expect(calls.some((c) => c.path === "/api/agents/alpha/interactive")).toBe(
    true,
  );
  expect(calls.some((c) => c.path === "/api/agents/alpha/restart")).toBe(false);
  expect(screen.getByLabelText("Interactive (tmux TUI)")).toHaveAttribute(
    "aria-checked",
    "false",
  );
});

it("renders one section per row in the design's order, with no second column", async () => {
  const calls: Call[] = [];
  stubFetch(calls);
  const { container } = renderPage();

  await screen.findByLabelText("Interval");
  const order = [
    "Goal",
    "Loop",
    "Messages & Channels",
    "Runtime",
    "Secrets (write-only)",
    "Retention and cleanup",
  ].map((t) => screen.getByText(t));
  for (let i = 0; i + 1 < order.length; i++) {
    // DOM order is visual order, so Tab moves through the sections as read.
    expect(
      order[i].compareDocumentPosition(order[i + 1]) &
        Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
  }
  expect(container.querySelector('[class*="lg:grid-cols-2"]')).toBeNull();
});

it("keeps discard and save keyboard reachable, in that order", async () => {
  const calls: Call[] = [];
  stubFetch(calls);
  renderPage();

  await waitFor(() =>
    expect(screen.getByLabelText("Interval")).toBeInTheDocument(),
  );
  fireEvent.change(screen.getByLabelText("Interval"), {
    target: { value: "45" },
  });

  const discard = screen.getByText("Discard changes");
  const save = screen.getByText("Save loop settings");
  expect(
    discard.compareDocumentPosition(save) & Node.DOCUMENT_POSITION_FOLLOWING,
  ).toBeTruthy();
  for (const el of [discard, save]) {
    expect(el).toBeEnabled();
    expect(el).not.toHaveAttribute("tabindex", "-1");
    el.focus();
    expect(document.activeElement).toBe(el);
  }
});

it("mounts no prune dialog buttons while its confirmation is closed", async () => {
  const calls: Call[] = [];
  stubFetch(calls);
  renderPage();

  await screen.findByText("Prune now");
  // A mounted-but-hidden dialog would put a second Cancel in the page and the
  // open prune dialog below would silently find the wrong one.
  expect(screen.queryByText("Prune retained data")).not.toBeInTheDocument();
  expect(screen.queryAllByText("Cancel")).toHaveLength(0);

  fireEvent.click(screen.getByText("Prune now"));
  expect(await screen.findByText("Cancel")).toBeInTheDocument();
  expect(screen.queryAllByText("Cancel")).toHaveLength(1);
});
