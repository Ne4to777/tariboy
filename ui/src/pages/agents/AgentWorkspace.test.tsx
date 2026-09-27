import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";
import { DaemonProvider } from "@/components/DaemonProvider";
import { addDaemon } from "@/lib/daemons";
import AgentWorkspace from "./AgentWorkspace";
import { canOpenAgentCwdInVSCode } from "./agentCwdVSCode";
import { agentGetOn } from "@/lib/api";
import * as desktop from "@/lib/desktop";
import { UI_MODE_KEY } from "@/lib/uiMode";

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return { ...actual, agentGetOn: vi.fn() };
});
vi.mock("@/hooks/useTerminalSocket", () => ({
  useTerminalSocket: vi.fn(() => ({
    status: "closed", absent: false, send: vi.fn(), sendResize: vi.fn(),
    attachTerm: vi.fn(), name: "worker", reconnect: vi.fn(),
  })),
}));
vi.mock("@/components/TuiScreen", () => ({ TuiScreen: () => <div>terminal</div> }));

const agent = {
  name: "worker",
  image: "worker:v1",
  state: "running",
  harness: "codex",
  loop_enabled: false,
  group: null,
  interactive: false,
  cwd: "/srv/worker project",
  current_goal_task_key: "IMPROVE-37",
};

beforeEach(() => {
  localStorage.clear();
  sessionStorage.clear();
  // These cases cover the full workspace; Simple mode has its own block below.
  localStorage.setItem(UI_MODE_KEY, "expert");
  vi.mocked(agentGetOn).mockResolvedValue({
    name: "worker",
    state: "running",
    loop_enabled: false,
    iterations: 0,
    last_iteration: null,
    last_iteration_id: null,
    status_message: "",
    status_updated: "",
  });
});
afterEach(() => {
  vi.restoreAllMocks();
  vi.useRealTimers();
});

describe("AgentWorkspace", () => {
  it("links the current Goal task and explains how Goal is selected", async () => {
    render(
      <DaemonProvider>
        <MemoryRouter initialEntries={["/agents/local/worker/console"]}>
          <Routes>
            <Route path="/agents/:hostId/:agent/:tab" element={<AgentWorkspace hostId="" hostLabel="Local" agent={agent} refresh={vi.fn()} />} />
          </Routes>
        </MemoryRouter>
      </DaemonProvider>,
    );

    expect(await screen.findByRole("link", { name: "IMPROVE-37" })).toHaveAttribute(
      "href",
      "/agents/local/worker/tasks?task=IMPROVE-37",
    );
    await userEvent.click(screen.getByRole("button", { name: "How Goal is selected" }));
    expect(await screen.findByRole("dialog")).toHaveTextContent(
      "Tariboy keeps the selected task until it is released",
    );
  });

  it("shows Goal help when no task is currently selected", async () => {
    render(
      <DaemonProvider>
        <MemoryRouter initialEntries={["/agents/local/worker/console"]}>
          <Routes>
            <Route path="/agents/:hostId/:agent/:tab" element={<AgentWorkspace hostId="" hostLabel="Local" agent={{ ...agent, current_goal_task_key: "" }} refresh={vi.fn()} />} />
          </Routes>
        </MemoryRouter>
      </DaemonProvider>,
    );

    expect(await screen.findByText("No current goal")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "How Goal is selected" })).toBeInTheDocument();
  });

  it("shows the configured harness, model, and effort before Goal", async () => {
    vi.mocked(agentGetOn).mockImplementation(async (_target, _name, action) => {
      if (action === "") return { ...agent, harness: "codex", model: "gpt-5-codex", effort: "high" };
      return {
        name: "worker", state: "running", loop_enabled: false, iterations: 0,
        last_iteration: null, last_iteration_id: null, status_message: "", status_updated: "",
      };
    });

    render(
      <DaemonProvider>
        <MemoryRouter initialEntries={["/agents/local/worker/console"]}>
          <Routes>
            <Route path="/agents/:hostId/:agent/:tab" element={<AgentWorkspace hostId="" hostLabel="Local" agent={agent} refresh={vi.fn()} />} />
          </Routes>
        </MemoryRouter>
      </DaemonProvider>,
    );

    await waitFor(() => expect(screen.getByTestId("agent-runtime")).toHaveTextContent("codex · gpt-5-codex · high"));
    const runtime = screen.getByTestId("agent-runtime");
    const goal = screen.getByText("Goal:");
    expect(runtime.compareDocumentPosition(goal) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("omits model and effort the harness does not report", async () => {
    vi.mocked(agentGetOn).mockImplementation(async (_target, _name, action) => {
      if (action === "") return { ...agent, harness: "opencode", model: "", effort: "" };
      return {
        name: "worker", state: "running", loop_enabled: false, iterations: 0,
        last_iteration: null, last_iteration_id: null, status_message: "", status_updated: "",
      };
    });

    render(
      <DaemonProvider>
        <MemoryRouter initialEntries={["/agents/local/worker/console"]}>
          <Routes>
            <Route path="/agents/:hostId/:agent/:tab" element={<AgentWorkspace hostId="" hostLabel="Local" agent={agent} refresh={vi.fn()} />} />
          </Routes>
        </MemoryRouter>
      </DaemonProvider>,
    );

    await waitFor(() => expect(screen.getByTestId("agent-runtime")).toHaveTextContent("opencode"));
    expect(screen.getByTestId("agent-runtime")).not.toHaveTextContent("·");
  });

  it("shows daemon-authoritative message queue saturation", async () => {
    vi.mocked(agentGetOn).mockResolvedValue({
      name: "worker", state: "running", loop_enabled: false, iterations: 0,
      last_iteration: null, last_iteration_id: null, status_message: "", status_updated: "",
      messages_pending: 100, messages_max_queue: 100, messages_queue_full: true,
    });

    render(
      <DaemonProvider>
        <MemoryRouter initialEntries={["/agents/local/worker/console"]}>
          <Routes>
            <Route path="/agents/:hostId/:agent/:tab" element={<AgentWorkspace hostId="" hostLabel="Local" agent={agent} refresh={vi.fn()} />} />
          </Routes>
        </MemoryRouter>
      </DaemonProvider>,
    );

    expect(await screen.findByText("Message queue full: 100 / 100")).toHaveClass("text-destructive");
  });

  it("shows why the loop halted, so a failed iteration launch is visible on every tab", async () => {
    vi.mocked(agentGetOn).mockResolvedValue({
      name: "worker", state: "error", loop_enabled: false, iterations: 0,
      last_iteration: null, last_iteration_id: null, status_message: "", status_updated: "",
      halt_kind: "error",
      halt_reason: "halted: image activation failed: prepare claude image skill bridge: skills missing",
      error_reason: "halted: image activation failed: prepare claude image skill bridge: skills missing",
    });

    render(
      <DaemonProvider>
        <MemoryRouter initialEntries={["/agents/local/worker/console"]}>
          <Routes>
            <Route path="/agents/:hostId/:agent/:tab" element={<AgentWorkspace hostId="" hostLabel="Local" agent={agent} refresh={vi.fn()} />} />
          </Routes>
        </MemoryRouter>
      </DaemonProvider>,
    );

    expect(await screen.findByTestId("agent-halt-reason")).toHaveTextContent(
      "halted: image activation failed: prepare claude image skill bridge: skills missing",
    );
    expect(screen.getByTestId("agent-halt-reason")).toHaveClass("text-destructive");
  });

  it("renders configured budget rows when an older daemon omits exhausted periods", async () => {
    vi.mocked(agentGetOn).mockResolvedValue({
      name: "worker", state: "running", loop_enabled: false, iterations: 0,
      last_iteration: null, last_iteration_id: null, status_message: "", status_updated: "",
      budget: {
        hour_usd: 1, day_usd: 0, week_usd: 0, month_usd: 0,
        hour_spent_usd: 0.25, day_spent_usd: 0.25, week_spent_usd: 0.25, month_spent_usd: 0.25,
      },
    });

    render(
      <DaemonProvider>
        <MemoryRouter initialEntries={["/agents/local/worker/console"]}>
          <Routes>
            <Route path="/agents/:hostId/:agent/:tab" element={<AgentWorkspace hostId="" hostLabel="Local" agent={agent} refresh={vi.fn()} />} />
          </Routes>
        </MemoryRouter>
      </DaemonProvider>,
    );

    expect(await screen.findByTestId("agent-budget-header")).toHaveTextContent("Hour 0.25 / 1");
    expect(screen.queryByText(/Out of budget:/)).not.toBeInTheDocument();
  });

  it("offers the action only for local and resolved SSH hosts", () => {
    const daemons = [
      { id: "ssh-1", label: "ssh", baseURL: "http://127.0.0.1:1", kind: "ssh" as const },
      { id: "https-1", label: "https", baseURL: "https://agents.example", kind: "https" as const },
    ];

    expect(canOpenAgentCwdInVSCode("", daemons)).toBe(true);
    expect(canOpenAgentCwdInVSCode("ssh-1", daemons)).toBe(true);
    expect(canOpenAgentCwdInVSCode("https-1", daemons)).toBe(false);
    expect(canOpenAgentCwdInVSCode("missing", daemons)).toBe(false);
  });

  it("shows the complete effective cwd and opens the local directory in VS Code", async () => {
	const open = vi.spyOn(desktop, "openHostPathInVSCode").mockResolvedValue(null);
    render(
      <DaemonProvider>
        <MemoryRouter initialEntries={["/agents/local/worker/console"]}>
          <Routes>
            <Route
              path="/agents/:hostId/:agent/:tab"
              element={<AgentWorkspace hostId="" hostLabel="Local" agent={agent} refresh={vi.fn()} />}
            />
          </Routes>
        </MemoryRouter>
      </DaemonProvider>,
    );

    const cwd = await screen.findByTestId("agent-cwd");
    expect(cwd).toHaveTextContent("/srv/worker project");
    expect(cwd).not.toHaveClass("truncate");
    await userEvent.click(screen.getByRole("button", { name: "Open in VS Code" }));
    expect(open).toHaveBeenCalledWith("", "/srv/worker project");
  });

  it("disables the VS Code action while the host is unavailable", async () => {
    render(
      <DaemonProvider>
        <MemoryRouter initialEntries={["/agents/local/worker/console"]}>
          <Routes>
            <Route path="/agents/:hostId/:agent/:tab" element={<AgentWorkspace hostId="" hostLabel="Local" agent={agent} refresh={vi.fn()} unavailable />} />
          </Routes>
        </MemoryRouter>
      </DaemonProvider>,
    );

    expect(await screen.findByRole("button", { name: "Open in VS Code" })).toBeDisabled();
  });

  it("retries effective cwd inspection after a transient failure", async () => {
    vi.useFakeTimers();
    let inspections = 0;
    vi.mocked(agentGetOn).mockImplementation(async (_target, _name, action) => {
      if (action !== "") {
        return {
          name: "worker", state: "running", loop_enabled: false, iterations: 0,
          last_iteration: null, last_iteration_id: null, status_message: "", status_updated: "",
        };
      }
      inspections += 1;
      if (inspections === 1) throw new Error("temporary inspect failure");
      return { ...agent, cwd: "/managed/worker" };
    });

    render(
      <DaemonProvider>
        <MemoryRouter initialEntries={["/agents/local/worker/console"]}>
          <Routes>
            <Route
              path="/agents/:hostId/:agent/:tab"
              element={<AgentWorkspace hostId="" hostLabel="Local" agent={{ ...agent, cwd: "" }} refresh={vi.fn()} />}
            />
          </Routes>
        </MemoryRouter>
      </DaemonProvider>,
    );

    await act(async () => { await vi.advanceTimersByTimeAsync(0); });
    expect(inspections).toBe(1);
    expect(screen.getByTestId("agent-cwd")).toHaveTextContent("…");
    await act(async () => { await vi.advanceTimersByTimeAsync(3000); });
    expect(screen.getByTestId("agent-cwd")).toHaveTextContent("/managed/worker");
    expect(inspections).toBeGreaterThanOrEqual(2);
  });

  it("orders the workspace tabs with Chat first and Tasks second", async () => {
    render(
      <DaemonProvider>
        <MemoryRouter initialEntries={["/agents/local/worker/console"]}>
          <Routes>
            <Route
              path="/agents/:hostId/:agent/:tab"
              element={<AgentWorkspace hostId="" hostLabel="Local" agent={agent} refresh={vi.fn()} />}
            />
          </Routes>
        </MemoryRouter>
      </DaemonProvider>,
    );

    const nav = await screen.findByRole("navigation", { name: "Agent workspace" });
    expect(within(nav).getAllByRole("link").map((link) => link.textContent)).toEqual([
      "Chat",
      "Tasks",
      "Console",
      "Autopilot",
      "Activity",
      "Configuration",
      "Advanced",
    ]);
  });

  it("keeps host and agent identity in every tab link", async () => {
    render(
      <DaemonProvider>
        <MemoryRouter initialEntries={["/agents/local/worker/console"]}>
          <Routes>
            <Route
              path="/agents/:hostId/:agent/:tab"
              element={<AgentWorkspace hostId="" hostLabel="Local" agent={agent} refresh={vi.fn()} />}
            />
          </Routes>
        </MemoryRouter>
      </DaemonProvider>,
    );

    await waitFor(() => expect(screen.getByRole("navigation", { name: "Agent workspace" })).toBeInTheDocument());
    expect(screen.getByRole("link", { name: "Console" })).toHaveAttribute("href", "/agents/local/worker/console");
    expect(screen.getByRole("link", { name: "Autopilot" })).toHaveAttribute("href", "/agents/local/worker/autopilot");
    expect(screen.getByRole("link", { name: "Activity" })).toHaveAttribute("href", "/agents/local/worker/activity");
    expect(screen.getByRole("link", { name: "Tasks" })).toHaveAttribute("href", "/agents/local/worker/tasks");
    expect(screen.getByRole("link", { name: "Configuration" })).toHaveAttribute("href", "/agents/local/worker/configuration");
    expect(screen.getByRole("link", { name: "Advanced" })).toHaveAttribute("href", "/agents/local/worker/advanced");
    expect(screen.getByText("This agent has no interactive terminal.")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Open Configuration" })).toHaveAttribute(
      "href",
      "/agents/local/worker/configuration",
    );
  });

  it("reuses the Tasks workspace scoped to the route agent", async () => {
    render(
      <DaemonProvider>
        <MemoryRouter initialEntries={["/agents/local/worker/tasks"]}>
          <Routes>
            <Route
              path="/agents/:hostId/:agent/:tab"
              element={<AgentWorkspace hostId="" hostLabel="Local" agent={agent} refresh={vi.fn()} />}
            />
          </Routes>
        </MemoryRouter>
      </DaemonProvider>,
    );

    const workspace = await screen.findByTestId("tasks-workspace");
    expect(workspace).toHaveAttribute("data-scope-agent", "worker");
  });

  it("loads Configuration from the host identified by the route", async () => {
    const host = await addDaemon({
      label: "Remote",
      baseURL: "https://remote.example",
      token: "secret",
    });

    render(
      <DaemonProvider>
        <MemoryRouter initialEntries={[`/agents/${host.id}/worker/configuration`]}>
          <Routes>
            <Route
              path="/agents/:hostId/:agent/:tab"
              element={<AgentWorkspace hostId={host.id} hostLabel="Remote" agent={agent} refresh={vi.fn()} />}
            />
          </Routes>
        </MemoryRouter>
      </DaemonProvider>,
    );

    await waitFor(() =>
      expect(vi.mocked(agentGetOn).mock.calls).toContainEqual([
        expect.objectContaining({ id: host.id }),
        "worker",
        "",
      ]),
    );
  });
});

describe("AgentWorkspace in Simple mode", () => {
  function Location() {
    return <p data-testid="location">{useLocation().pathname}</p>;
  }
  function renderAt(path: string) {
    render(
      <DaemonProvider>
        <MemoryRouter initialEntries={[path]}>
          <Routes>
            <Route
              path="/agents/:hostId/:agent/:tab"
              element={<><AgentWorkspace hostId="" hostLabel="Local" agent={agent} refresh={vi.fn()} /><Location /></>}
            />
          </Routes>
        </MemoryRouter>
      </DaemonProvider>,
    );
  }

  beforeEach(() => localStorage.removeItem(UI_MODE_KEY));

  it("shows only Chat, Tasks, Console and Configuration with the full header", async () => {
    renderAt("/agents/local/worker/tasks");

    const nav = await screen.findByRole("navigation", { name: "Agent workspace" });
    expect(within(nav).getAllByRole("link").map((link) => link.textContent)).toEqual([
      "Chat",
      "Tasks",
      "Console",
      "Configuration",
    ]);
    expect(screen.getByRole("button", { name: "Stop" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "IMPROVE-37" })).toBeInTheDocument();
  });

  it("keeps the Console tab open", async () => {
    renderAt("/agents/local/worker/console");

    const nav = await screen.findByRole("navigation", { name: "Agent workspace" });
    expect(within(nav).getByRole("link", { name: "Console" })).toHaveAttribute("aria-current", "page");
    expect(screen.getByTestId("location")).toHaveTextContent("/agents/local/worker/console");
  });

  it.each(["autopilot", "activity", "advanced"])("sends a hidden %s tab to Chat", async (tab) => {
    renderAt(`/agents/local/worker/${tab}`);

    await waitFor(() => expect(screen.getByTestId("location")).toHaveTextContent("/agents/local/worker/chat"));
  });
});
