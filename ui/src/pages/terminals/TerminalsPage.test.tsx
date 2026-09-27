import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, render, screen, waitFor, fireEvent, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";
import { DaemonProvider } from "@/components/DaemonProvider";
import TerminalsPage from "./TerminalsPage";
import { UI_MODE_KEY } from "@/lib/uiMode";
import { fetchAllAgents } from "@/lib/aggregate";
import {
  agentGetOn,
  apiOn,
  createAgent,
  imageManifestGet,
  listImages,
  startAgent,
} from "@/lib/api";
import { addDaemon, listDaemons, getDaemonToken } from "@/lib/daemons";
import * as desktop from "@/lib/desktop";
import { SidebarStateProvider } from "./SidebarStateProvider";
import { CustomerQuestionNotificationsContext } from "@/components/customerQuestionNotificationsContext";
import { targetFor } from "@/lib/terminalsHost";
import { TitlebarSlotContext } from "./titlebarSlotContext";
import { createWorkspace, moveHost, resetWorkspacesForTest } from "@/lib/workspaces";
import userEvent from "@testing-library/user-event";

vi.mock("@/lib/aggregate", () => ({
  fetchAllAgents: vi.fn(),
}));

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    agentGetOn: vi.fn(),
    apiOn: vi.fn(),
    createAgent: vi.fn(),
    imageManifestGet: vi.fn(),
    listImages: vi.fn(),
    startAgent: vi.fn(),
  };
});

// Selecting an agent mounts TerminalPane, which dials a websocket and renders
// xterm.js — neither works in jsdom. Stub both, same approach as
// TerminalPane.test.tsx.
vi.mock("@/hooks/useTerminalSocket", () => ({
  useTerminalSocket: vi.fn(() => ({
    status: "open",
    absent: false,
    send: vi.fn(),
    sendResize: vi.fn(),
    attachTerm: vi.fn(),
    name: "a1",
    reconnect: vi.fn(),
  })),
}));
vi.mock("@/components/TuiScreen", () => ({
  TuiScreen: () => <div data-testid="tui-screen" />,
}));

const cloneProjection = {
  name: "source",
  image: "worker:v1",
  digest: "sha256",
  state: "stopped",
  cwd: "/managed/source",
  configured_cwd: "",
  harness: "codex",
  model: "gpt-5",
  effort: "high",
  interactive: true,
  loop_enabled: false,
  enabled: false,
  interval_s: 0,
  timeout_s: 0,
  hard_timeout_s: 0,
  on_timeout: "restart",
  on_error: "restart",
  max_idle_iterations: 0,
  user_prompt: "",
  env: {},
  plugins: [],
  messages_batch: 10,
  messages_max_queue: 1000,
  group: null,
  alias: "",
  notes: "",
  color: "",
} as const;

beforeEach(() => {
  localStorage.clear();
  sessionStorage.clear();
  resetWorkspacesForTest();
  // Console navigation is Expert behaviour; Simple opens Chat.
  localStorage.setItem(UI_MODE_KEY, "expert");
  vi.mocked(apiOn).mockReset().mockResolvedValue(undefined);
  vi.mocked(fetchAllAgents).mockResolvedValue([
    {
      host: { id: "", label: "This daemon (local)" },
      agents: [
        { name: "a1", image: "bare:latest", state: "running", harness: "claude", loop_enabled: false, group: null, interactive: true },
        { name: "a2", image: "img", state: "running", harness: "claude", loop_enabled: false, group: null, interactive: false },
      ],
    },
    { host: { id: "d1", label: "prod" }, agents: [], error: "unauthorized: nope" },
  ]);
  vi.mocked(listImages).mockResolvedValue({
    images: [{ name: "worker", tag: "v1", bare: false }],
    count: 1,
  });
  vi.mocked(imageManifestGet).mockResolvedValue({
    schema_version: 1,
    name: "worker",
    tag: "v1",
    built_at: "2026-07-29T00:00:00Z",
    parents: null,
    plugins: null,
	 skills: null,
    requires_secrets: null,
    harness: { type: "codex", interactive: false },
    env: null,
    policy: {},
    layers: null,
  });
  vi.mocked(startAgent).mockResolvedValue({ name: "new-agent", action: "start" });
  vi.mocked(agentGetOn).mockResolvedValue(cloneProjection);
});
afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

function renderAt(path: string, attention: ReadonlyMap<string, number> = new Map()) {
  // Stands in for the App titlebar slot the workspace switcher portals into.
  const slot = document.body.appendChild(document.createElement("div"));
  return render(
    <TitlebarSlotContext.Provider value={slot}>
    <DaemonProvider>
      <CustomerQuestionNotificationsContext.Provider value={{ attention, refreshHost: async () => {} }}>
        <SidebarStateProvider>
          <MemoryRouter initialEntries={[path]}>
            <RoutedLocation />
            <Routes>
              <Route path="/" element={<TerminalsPage />} />
              <Route path="/terminals" element={<TerminalsPage />} />
              <Route path="/terminals/:hostId/:agent" element={<TerminalsPage />} />
              <Route path="/agents/:hostId/:agent/:tab" element={<TerminalsPage />} />
              <Route path="/agents/:hostId/teams/:team" element={<TerminalsPage />} />
              <Route path="/servers/:hostId/tasks" element={<TerminalsPage />} />
            </Routes>
          </MemoryRouter>
        </SidebarStateProvider>
      </CustomerQuestionNotificationsContext.Provider>
    </DaemonProvider>
    </TitlebarSlotContext.Provider>,
  );
}

function RoutedLocation() {
  const location = useLocation();
  return <output data-testid="location">{location.pathname + location.search}</output>;
}

function mockRowRects(tops: Record<string, number>) {
  vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (this: HTMLElement) {
    const top = tops[this.getAttribute("aria-label") ?? ""] ?? 0;
    return {
      left: 0, top, width: 100, height: 20, right: 100, bottom: top + 20, x: 0, y: top,
      toJSON: () => ({}),
    };
  });
}

async function keyboardMove(label: string, direction: "ArrowUp" | "ArrowDown") {
  const row = screen.getByRole("button", { name: label });
  fireEvent.keyDown(row, { code: "Space" });
  await waitFor(() => expect(row).toHaveAttribute("aria-pressed", "true"));
  fireEvent.keyDown(document, { code: direction });
  fireEvent.keyDown(document, { code: "Space" });
  await waitFor(() => expect(row).not.toHaveAttribute("aria-pressed"));
}

/** Register "prod" in the real (jsdom) registry under the id the mocked
 *  fetchAllAgents reports, so the manage menu operates on a real entry. */
async function registerProd(): Promise<string> {
  const meta = await addDaemon({ label: "prod", baseURL: "http://127.0.0.1:9992", token: "tok" });
  vi.mocked(fetchAllAgents).mockResolvedValue([
    { host: { id: "", label: "This daemon (local)" }, agents: [] },
    { host: { id: meta.id, label: "prod" }, agents: [] },
  ]);
  return meta.id;
}

// Radix opens a DropdownMenu on pointerdown, which jsdom's fireEvent.click
// does not synthesize — drive it from the keyboard instead.
async function openManageMenu(): Promise<void> {
  fireEvent.keyDown(await screen.findByRole("button", { name: "manage prod" }), { key: "Enter" });
}


/** Open the Servers tab before mounting — the tab the sidebar remembers is the
 *  one the server sections, host status and drag-and-drop live in. */
function serversTab() {
  localStorage.setItem("terminals:sidebar-tab:v1", "servers");
}

/** Radix activates a tab on mousedown, which fireEvent.click does not send. */
function pickSidebarTab(name: string) {
  fireEvent.mouseDown(screen.getByRole("tab", { name }), { button: 0 });
}

describe("TerminalsPage", () => {
  it("retains the selected agent shell when its next aggregate refresh is unavailable", async () => {
    vi.useFakeTimers();
    const remote = await addDaemon({ label: "prod", baseURL: "http://127.0.0.1:19992", token: "tok" });
    let state: "ready" | "unavailable" | "recovered" = "ready";
    vi.mocked(fetchAllAgents).mockImplementation(async () => [{
      host: { id: remote.id, label: "prod" },
      agents: state === "unavailable" ? [] : [{
        name: "worker", image: state === "recovered" ? "worker:v2" : "worker:v1", state: "running", harness: "codex",
        loop_enabled: false, group: null, interactive: false,
      }],
      error: state === "unavailable" ? "unreachable" : undefined,
    }]);

    renderAt(`/agents/${remote.id}/worker/console`);
    await act(async () => { await vi.advanceTimersByTimeAsync(0); });
    expect(screen.getByRole("heading", { name: "worker" })).toBeInTheDocument();

    state = "unavailable";
    await act(async () => { await vi.advanceTimersByTimeAsync(3000); });
    expect(vi.mocked(fetchAllAgents).mock.calls.length).toBeGreaterThan(1);

    expect(screen.getByRole("heading", { name: "worker" })).toBeInTheDocument();
    expect(screen.getByText(/temporarily unavailable/i)).toHaveAttribute("role", "status");
    expect(screen.getByTestId("agent-workspace-content")).toHaveAttribute("inert", "");
    expect(screen.getByRole("button", { name: "Open worker" })).toBeDisabled();

    state = "recovered";
    await act(async () => { await vi.advanceTimersByTimeAsync(3000); });
    expect(vi.mocked(fetchAllAgents).mock.calls.length).toBeGreaterThan(2);
    expect(screen.getByRole("heading", { name: "worker" })).toBeInTheDocument();
    expect(screen.getByText("worker:v2")).toBeInTheDocument();
    expect(screen.queryByText(/temporarily unavailable/i)).toBeNull();
    expect(screen.getByTestId("agent-workspace-content")).not.toHaveAttribute("inert");
  }, 10_000);

  it("opens a server workspace from a selectable server heading", async () => {
    serversTab();
    renderAt("/");

    fireEvent.click(await screen.findByRole("button", {
      name: "Open server This daemon (local)",
    }));

    await waitFor(() =>
      expect(screen.getByTestId("location")).toHaveTextContent("/servers/local/tasks"),
    );
    expect(screen.getByRole("navigation", { name: "Server workspace" }))
      .toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Open server This daemon (local)" }))
      .toHaveAttribute("aria-current", "page");
  });

  it("shows server context above agent context with distinct selected states", async () => {
    serversTab();
    renderAt("/agents/local/a1/console");

    expect(await screen.findByRole("navigation", { name: "Server workspace" }))
      .toBeInTheDocument();
    expect(await screen.findByRole("navigation", { name: "Agent workspace" }))
      .toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Open server This daemon (local)" }))
      .toHaveAttribute("aria-current", "page");
    expect(screen.getByRole("button", { name: "Open a1" }))
      .toHaveAttribute("aria-current", "page");
  });

  it("counts the agent's tasks with unread questions on the Tasks tab", async () => {
    renderAt("/agents/local/a1/console", new Map([[JSON.stringify(["", "a1"]), 2]]));

    const count = await screen.findByLabelText("2 tasks with unread agent questions");
    expect(count).toHaveTextContent("2");
    const nav = screen.getByRole("navigation", { name: "Agent workspace" });
    expect(within(nav).getByRole("link", { name: /Tasks/ })).toContainElement(count);
  });

  it("shows no Tasks count without unread agent questions", async () => {
    renderAt("/agents/local/a1/console");

    expect(await screen.findByRole("navigation", { name: "Agent workspace" })).toBeInTheDocument();
    expect(screen.queryByLabelText(/unread agent question/)).toBeNull();
  });

  it("opens a sidebar agent in Console", async () => {
    renderAt("/");
    await waitFor(() => expect(screen.getByText("a1")).toBeInTheDocument());

    fireEvent.click(screen.getByRole("button", { name: "Open a1" }));

    await waitFor(() =>
      expect(screen.getByTestId("location")).toHaveTextContent("/agents/local/a1/console"),
    );
  });

  it("opens a non-interactive sidebar agent in Console", async () => {
    renderAt("/");
    await waitFor(() => expect(screen.getByText("a2")).toBeInTheDocument());

    fireEvent.click(screen.getByRole("button", { name: "Open a2" }));

    await waitFor(() =>
      expect(screen.getByTestId("location"))
        .toHaveTextContent("/agents/local/a2/console"),
    );
    expect(await screen.findByRole("navigation", { name: "Agent workspace" }))
      .toBeInTheDocument();
  });

  it("keeps the current agent tab when switching to another agent", async () => {
    renderAt("/agents/local/a1/activity");
    await waitFor(() => expect(screen.getByText("a2")).toBeInTheDocument());

    fireEvent.click(screen.getByRole("button", { name: "Open a2" }));

    await waitFor(() =>
      expect(screen.getByTestId("location"))
        .toHaveTextContent("/agents/local/a2/activity"),
    );
  });

  it("shows All tasks in the island, narrowed to the selected agent, and keeps the view when another agent is picked", async () => {
    renderAt("/agents/local/a1/console?view=all");
    const workspace = await screen.findByTestId("all-tasks-workspace");
    expect(within(workspace).getByRole("button", { name: "Clear agent filter" }).parentElement)
      .toHaveTextContent("a1");
    expect(screen.queryByTestId("tui-screen")).toBeNull();

    fireEvent.click(within(workspace).getByRole("button", { name: "Clear agent filter" }));
    await waitFor(() => expect(within(workspace).queryByRole("button", { name: "Clear agent filter" })).toBeNull());
    // The agent stays selected in the sidebar; only the filter went.
    expect(screen.getByTestId("location").textContent).toBe("/agents/local/a1/console?view=all");

    fireEvent.click(screen.getByRole("button", { name: "Open a2" }));
    await waitFor(() =>
      expect(screen.getByTestId("location").textContent).toBe("/agents/local/a2/console?view=all"),
    );
    expect(within(await screen.findByTestId("all-tasks-workspace"))
      .getByRole("button", { name: "Clear agent filter" }).parentElement).toHaveTextContent("a2");
  });

  it("drops the selected task when switching agents on the Tasks tab", async () => {
    renderAt("/agents/local/a1/tasks?task=DEV-1");
    await waitFor(() => expect(screen.getByText("a2")).toBeInTheDocument());

    fireEvent.click(screen.getByRole("button", { name: "Open a2" }));

    await waitFor(() =>
      expect(screen.getByTestId("location").textContent).toBe("/agents/local/a2/tasks"),
    );
  });

  it("restores server, team, and agent order", async () => {
    serversTab();
    localStorage.setItem("terminals:server-order:v1", JSON.stringify({
      version: 1,
      ids: ["d1", ""],
    }));
    vi.mocked(fetchAllAgents).mockResolvedValue([
      {
        host: { id: "", label: "local" },
        groups: [{ name: "alpha", lead: "", members: 1 }, { name: "beta", lead: "", members: 1 }],
        agents: [
          { name: "a1", image: "img", state: "running", harness: "codex", loop_enabled: true, group: "alpha", interactive: true },
          { name: "b1", image: "img", state: "running", harness: "codex", loop_enabled: true, group: "beta", interactive: true },
          { name: "solo1", image: "img", state: "running", harness: "codex", loop_enabled: true, group: null, interactive: true },
          { name: "solo2", image: "img", state: "running", harness: "codex", loop_enabled: true, group: null, interactive: true },
        ],
        sidebarOrder: { version: 1, groups: ["beta", "alpha"], agents: ["solo2", "solo1"] },
      },
      { host: { id: "d1", label: "prod" }, agents: [], groups: [] },
    ] as unknown as Awaited<ReturnType<typeof fetchAllAgents>>);

    renderAt("/");

    const prod = await screen.findByRole("button", { name: "Open server prod" });
    const local = screen.getByRole("button", { name: "Open server local" });
    expect(prod.compareDocumentPosition(local) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    const beta = screen.getByRole("button", { name: "Open team beta" });
    const alpha = screen.getByRole("button", { name: "Open team alpha" });
    expect(beta.compareDocumentPosition(alpha) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    const solo2 = screen.getByRole("button", { name: "Open solo2" });
    const solo1 = screen.getByRole("button", { name: "Open solo1" });
    expect(solo2.compareDocumentPosition(solo1) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("uses the whole server row as an accessible drag surface", async () => {
    serversTab();
    vi.mocked(fetchAllAgents).mockResolvedValue([
      { host: { id: "", label: "local" }, agents: [], groups: [] },
      { host: { id: "d1", label: "prod" }, agents: [], groups: [] },
    ]);
    renderAt("/");

    const local = await screen.findByRole("button", { name: "Open server local" });
    expect(local).toHaveAttribute("aria-roledescription", "draggable");
    expect(screen.queryByRole("button", { name: /Move server/ })).toBeNull();
  });

  it("persists server order after a keyboard drag", async () => {
    serversTab();
    vi.mocked(fetchAllAgents).mockResolvedValue([
      { host: { id: "", label: "local" }, agents: [], groups: [] },
      { host: { id: "d1", label: "prod" }, agents: [], groups: [] },
    ]);
    mockRowRects({ "Open server local": 0, "Open server prod": 30 });
    renderAt("/");
    await screen.findByRole("button", { name: "Open server local" });

    await keyboardMove("Open server local", "ArrowDown");

    expect(JSON.parse(localStorage.getItem("terminals:server-order:v1")!))
      .toEqual({ version: 1, ids: ["d1", ""] });
  });

  it("keeps the saved position of hosts in other workspaces when reordering servers", async () => {
    serversTab();
    vi.mocked(fetchAllAgents).mockResolvedValue([
      { host: { id: "", label: "local" }, agents: [], groups: [] },
      { host: { id: "d1", label: "prod" }, agents: [], groups: [] },
      { host: { id: "d2", label: "lab" }, agents: [], groups: [] },
    ]);
    localStorage.setItem("terminals:server-order:v1", JSON.stringify({ version: 1, ids: ["", "d2", "d1"] }));
    moveHost("d2", createWorkspace("Lab")!);
    mockRowRects({ "Open server local": 0, "Open server prod": 30 });
    renderAt("/");
    await screen.findByRole("button", { name: "Open server local" });

    await keyboardMove("Open server local", "ArrowDown");

    expect(JSON.parse(localStorage.getItem("terminals:server-order:v1")!))
      .toEqual({ version: 1, ids: ["d1", "d2", ""] });
  });

  it("persists team order after a keyboard drag", async () => {
    serversTab();
    vi.mocked(fetchAllAgents).mockResolvedValue([{
      host: { id: "", label: "local" },
      agents: [],
      groups: [{ name: "a", lead: "", members: 0 }, { name: "b", lead: "", members: 0 }],
    }]);
    mockRowRects({ "Open team a": 0, "Open team b": 30 });
    vi.mocked(apiOn).mockResolvedValueOnce(undefined);
    renderAt("/");
    await screen.findByRole("button", { name: "Open team a" });

    await keyboardMove("Open team a", "ArrowDown");

    await waitFor(() => expect(apiOn).toHaveBeenCalledWith(
      null,
      "POST",
      "/api/daemon/config",
      { key: "sidebar_order_v1", value: JSON.stringify({ version: 1, groups: ["b", "a"], agents: [] }) },
    ));
  });

  it("rolls back agent order when daemon persistence fails", async () => {
    serversTab();
    vi.mocked(fetchAllAgents).mockResolvedValue([{
      host: { id: "", label: "local" },
      agents: [
        { name: "a1", image: "img", state: "running", harness: "codex", loop_enabled: true, group: null, interactive: true },
        { name: "a2", image: "img", state: "running", harness: "codex", loop_enabled: true, group: null, interactive: true },
      ],
      groups: [],
    }]);
    mockRowRects({ "Open a1": 0, "Open a2": 30 });
    let rejectSave!: (cause: Error) => void;
    vi.mocked(apiOn).mockImplementationOnce(() => new Promise<void>((_, reject) => {
      rejectSave = reject;
    }));
    renderAt("/");
    const a1 = await screen.findByRole("button", { name: "Open a1" });
    const a2 = screen.getByRole("button", { name: "Open a2" });

    await keyboardMove("Open a1", "ArrowDown");
    expect(a2.compareDocumentPosition(a1) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    rejectSave(new Error("offline"));

    expect(await screen.findByRole("alert")).toHaveTextContent("Could not save agents order");
    expect(a1.compareDocumentPosition(a2) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("serializes daemon order writes so an older request cannot finish last", async () => {
    serversTab();
    vi.mocked(fetchAllAgents).mockResolvedValue([{
      host: { id: "", label: "local" },
      agents: [
        { name: "a1", image: "img", state: "running", harness: "codex", loop_enabled: true, group: "a", interactive: true },
        { name: "a2", image: "img", state: "running", harness: "codex", loop_enabled: true, group: "a", interactive: true },
        { name: "b1", image: "img", state: "running", harness: "codex", loop_enabled: true, group: "b", interactive: true },
        { name: "b2", image: "img", state: "running", harness: "codex", loop_enabled: true, group: "b", interactive: true },
      ],
      groups: [{ name: "a", lead: "", members: 2 }, { name: "b", lead: "", members: 2 }],
    }]);
    let finishFirst!: () => void;
    let failSecond!: (cause: Error) => void;
    vi.mocked(apiOn)
      .mockImplementationOnce(() => new Promise<void>((resolve) => { finishFirst = resolve; }))
      .mockImplementationOnce(() => new Promise<void>((_, reject) => { failSecond = reject; }));
    mockRowRects({
      "Open a1": 0,
      "Open a2": 30,
      "Open b1": 60,
      "Open b2": 90,
      "Open team a": 120,
      "Open team b": 150,
    });
    renderAt("/");
    const a1 = await screen.findByRole("button", { name: "Open a1" });

    fireEvent.keyDown(a1, { code: "Space" });
    await waitFor(() => expect(a1).toHaveAttribute("aria-pressed", "true"));
    fireEvent.keyDown(document, { code: "ArrowDown" });
    fireEvent.keyDown(document, { code: "Space" });
    await waitFor(() => expect(apiOn).toHaveBeenCalledTimes(1));

    const b1 = screen.getByRole("button", { name: "Open b1" });
    fireEvent.keyDown(b1, { code: "Space" });
    await waitFor(() => expect(b1).toHaveAttribute("aria-pressed", "true"));
    fireEvent.keyDown(document, { code: "ArrowDown" });
    fireEvent.keyDown(document, { code: "Space" });
    await waitFor(() => expect(b1).not.toHaveAttribute("aria-pressed"));
    await act(async () => {});

    expect(apiOn).toHaveBeenCalledTimes(1);
    finishFirst();
    await waitFor(() => expect(apiOn).toHaveBeenCalledTimes(2));
    expect(screen.getByRole("button", { name: "Open a2" })
      .compareDocumentPosition(screen.getByRole("button", { name: "Open a1" }))
      & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(screen.getByRole("button", { name: "Open b2" })
      .compareDocumentPosition(screen.getByRole("button", { name: "Open b1" }))
      & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    failSecond(new Error("offline"));
    expect(await screen.findByRole("alert")).toHaveTextContent("Could not save agents order");
    expect(screen.getByRole("button", { name: "Open a2" })
      .compareDocumentPosition(screen.getByRole("button", { name: "Open a1" }))
      & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(screen.getByRole("button", { name: "Open b1" })
      .compareDocumentPosition(screen.getByRole("button", { name: "Open b2" }))
      & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();

    let failThird!: (cause: Error) => void;
    let failFourth!: (cause: Error) => void;
    vi.mocked(apiOn)
      .mockImplementationOnce(() => new Promise<void>((_, reject) => { failThird = reject; }))
      .mockImplementationOnce(() => new Promise<void>((_, reject) => { failFourth = reject; }));
    await keyboardMove("Open b1", "ArrowDown");
    await waitFor(() => expect(apiOn).toHaveBeenCalledTimes(3));
    await keyboardMove("Open team a", "ArrowDown");
    expect(apiOn).toHaveBeenCalledTimes(3);
    failThird(new Error("offline"));
    await waitFor(() => expect(apiOn).toHaveBeenCalledTimes(4));
    failFourth(new Error("offline"));
    await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent("Could not save groups order"));
    expect(screen.getByRole("button", { name: "Open a2" })
      .compareDocumentPosition(screen.getByRole("button", { name: "Open a1" }))
      & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(screen.getByRole("button", { name: "Open b1" })
      .compareDocumentPosition(screen.getByRole("button", { name: "Open b2" }))
      & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("honors a hidden sidebar from the shared persisted state", async () => {
    localStorage.setItem("terminals:sidebar:v1", JSON.stringify({ width: 268, hidden: true }));
    renderAt("/");

    expect(await screen.findByText("Pick an agent on the left, or create one.")).toBeInTheDocument();
    expect(screen.queryByText("Agents")).toBeNull();
    expect(screen.queryByRole("separator", { name: "resize sidebar" })).toBeNull();
  });

  it("opens a sidebar agent in the product Console route", async () => {
    renderAt("/");
    fireEvent.click(await screen.findByText("a1"));
    await waitFor(() =>
      expect(screen.getByTestId("location")).toHaveTextContent("/agents/local/a1/console"),
    );
  });

  it("opens a sidebar agent on Chat in Simple mode", async () => {
    localStorage.setItem(UI_MODE_KEY, "simple");
    renderAt("/");
    fireEvent.click(await screen.findByText("a1"));
    await waitFor(() =>
      expect(screen.getByTestId("location")).toHaveTextContent("/agents/local/a1/chat"),
    );
  });

  it("groups agents under server sections and shows host errors", async () => {
    serversTab();
    renderAt("/terminals");
    await waitFor(() => expect(screen.getByText("This daemon (local)")).toBeInTheDocument());
    expect(screen.getByText("prod")).toBeInTheDocument();
    expect(screen.getByText("a1")).toBeInTheDocument();
    expect(screen.getByText("unauthorized: nope")).toBeInTheDocument();
  });

  it("separates team members from individual agents", async () => {
    serversTab();
    vi.mocked(fetchAllAgents).mockResolvedValue([{
      host: { id: "", label: "This daemon (local)" },
      groups: [{ name: "empty-team", lead: "", members: 0 }],
      agents: [
        { name: "lead", image: "img", state: "running", harness: "claude", loop_enabled: true, group: "dev-team", interactive: true },
        { name: "solo", image: "img", state: "stopped", harness: "codex", loop_enabled: false, group: null, interactive: true },
      ],
    }]);
    renderAt("/");
    expect(await screen.findByText("Teams")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Open team empty-team" })).toBeInTheDocument();
    expect(screen.getByText("dev-team")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Open team dev-team" }));
    expect(await screen.findByRole("heading", { name: "dev-team" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Open team member lead" })).toBeInTheDocument();
    expect(screen.getByText("Individual agents")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Open lead" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Open solo" })).toBeInTheDocument();
  });

  it.each([
    { agentName: "lead", group: "dev-team" },
    { agentName: "solo", group: null },
  ])("opens Clone for the $agentName sidebar row without navigating", async ({ agentName, group }) => {
    vi.mocked(fetchAllAgents).mockResolvedValue([{
      host: { id: "", label: "This daemon (local)" },
      agents: [
        { name: "lead", image: "worker:v1", state: "running", harness: "codex", loop_enabled: true, group: "dev-team", interactive: true },
        { name: "solo", image: "worker:v1", state: "stopped", harness: "codex", loop_enabled: false, group: null, interactive: true },
      ],
    }]);
    vi.mocked(agentGetOn).mockResolvedValueOnce({ ...cloneProjection, name: agentName, group });
    renderAt("/");

    const row = await screen.findByRole("button", { name: `Open ${agentName}` });
    fireEvent.contextMenu(row);
    fireEvent.click(await screen.findByRole("menuitem", { name: "Clone" }));

    expect(await screen.findByRole("heading", { name: "Clone agent" })).toBeInTheDocument();
    expect(agentGetOn).toHaveBeenCalledWith(targetFor(""), agentName, "");
    expect(screen.getByTestId("location")).toHaveTextContent("/");
  });

  it("does not navigate for a context-menu gesture", async () => {
    renderAt("/");
    const row = await screen.findByRole("button", { name: "Open a1" });

    fireEvent.pointerDown(row, { button: 2, pointerId: 12, clientX: 10, clientY: 10 });
    fireEvent.pointerMove(window, { pointerId: 12, clientX: 200, clientY: 200 });
    fireEvent.contextMenu(row);

    expect(await screen.findByRole("menuitem", { name: "Clone" })).toBeInTheDocument();
    expect(screen.getByTestId("location").textContent).toBe("/");
  });

  it("keeps the individual agents subsection visible when a host only has an empty team", async () => {
    serversTab();
    vi.mocked(fetchAllAgents).mockResolvedValue([{
      host: { id: "", label: "This daemon (local)" },
      groups: [{ name: "empty-team", lead: "", members: 0 }],
      agents: [],
    }]);
    renderAt("/");
    expect(await screen.findByRole("button", { name: "Open team empty-team" })).toBeInTheDocument();
    expect(screen.getByText("Individual agents")).toBeInTheDocument();
  });

  it("shows native SSH connection failures in the terminal workspace", async () => {
    serversTab();
    vi.stubGlobal("__TAURI_INTERNALS__", {});
    vi.spyOn(desktop, "hostsList").mockResolvedValue([{
      id: "d1",
      label: "prod",
      kind: "ssh",
      ssh_alias: "prod-box",
      remote_install_dir: "~/.local/lib/tariboy",
      remote_port: 9990,
      https_base_url: "",
      last_daemon_version: "",
      state: "disconnected",
      base_url: "",
      local_port: 0,
      phase: "",
      platform: "",
      arch: "",
      prerequisites: [],
      message: "",
    }]);
    vi.spyOn(desktop, "hostConnect").mockRejectedValue(new Error("SSH timed out"));

    renderAt("/terminals");
    fireEvent.click(await screen.findByRole("button", { name: "Connect prod" }));

    expect(await screen.findByRole("alert")).toHaveTextContent(
      "Could not connect to prod: Error: SSH timed out",
    );
  });

  it("hides Update in the agent sidebar when the remote version is current", async () => {
    serversTab();
    vi.stubGlobal("__TAURI_INTERNALS__", {});
    vi.spyOn(desktop, "daemonState").mockResolvedValue({
      state: "ready",
      base_url: "http://127.0.0.1:9990",
      daemon_version: "0.11.5",
      app_version: "0.11.5",
      base_dir: "/tmp/tariboy",
      pid: 42,
      adopted: false,
      message: "",
    });
    vi.spyOn(desktop, "hostsList").mockResolvedValue([{
      id: "d1",
      label: "prod",
      kind: "ssh",
      ssh_alias: "prod-box",
      remote_install_dir: "~/.local/lib/tariboy",
      remote_port: 9990,
      https_base_url: "",
      last_daemon_version: "0.11.5",
      state: "ready",
      base_url: "http://127.0.0.1:18444",
      local_port: 18444,
      phase: "connect",
      platform: "Linux",
      arch: "x86_64",
      prerequisites: [],
      message: "",
    }]);

    renderAt("/terminals");

    expect(await screen.findByText("ready")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Update prod" })).toBeNull();
  });

  /** Two SSH hosts behind the app version: the state "Update all (N)" renders for. */
  async function staleHosts() {
    serversTab();
    vi.stubGlobal("__TAURI_INTERNALS__", {});
    vi.spyOn(desktop, "daemonState").mockResolvedValue({
      state: "ready",
      base_url: "http://127.0.0.1:9990",
      daemon_version: "0.12.0",
      app_version: "0.12.0",
      base_dir: "/tmp/tariboy",
      pid: 42,
      adopted: false,
      message: "",
    });
    const host = (id: string, label: string) => ({
      id,
      label,
      kind: "ssh" as const,
      ssh_alias: `${label}-box`,
      remote_install_dir: "~/.local/lib/tariboy",
      remote_port: 9990,
      https_base_url: "",
      last_daemon_version: "0.11.5",
      state: "ready" as const,
      base_url: "http://127.0.0.1:18444",
      local_port: 18444,
      phase: "connect",
      platform: "Linux",
      arch: "x86_64",
      prerequisites: [],
      message: "",
    });
    vi.spyOn(desktop, "hostsList").mockResolvedValue([host("d1", "alpha"), host("d2", "beta")]);
    vi.mocked(fetchAllAgents).mockResolvedValue([
      { host: { id: "d1", label: "alpha" }, agents: [] },
      { host: { id: "d2", label: "beta" }, agents: [] },
    ]);
    renderAt("/terminals");
    return screen.findByRole("button", { name: "Update all (2)" });
  }

  it("updates every outdated server once Update all is confirmed", async () => {
    const update = vi.spyOn(desktop, "hostUpdate").mockResolvedValue({ operation_id: "op" });
    vi.stubGlobal("confirm", vi.fn(() => true));

    fireEvent.click(await staleHosts());

    await waitFor(() => expect(update).toHaveBeenCalledTimes(2));
    expect(update.mock.calls.map(([id]) => id)).toEqual(["d1", "d2"]);
  });

  it("updates nothing when Update all is cancelled", async () => {
    const update = vi.spyOn(desktop, "hostUpdate").mockResolvedValue({ operation_id: "op" });
    vi.stubGlobal("confirm", vi.fn(() => false));

    fireEvent.click(await staleHosts());

    await waitFor(() => expect(window.confirm).toHaveBeenCalled());
    expect(update).not.toHaveBeenCalled();
  });

  it("reports a failed server and still updates the rest", async () => {
    const update = vi.spyOn(desktop, "hostUpdate")
      .mockRejectedValueOnce(new Error("ssh closed"))
      .mockResolvedValue({ operation_id: "op" });
    vi.stubGlobal("confirm", vi.fn(() => true));

    fireEvent.click(await staleHosts());

    expect(await screen.findByRole("alert")).toHaveTextContent("alpha");
    expect(update).toHaveBeenCalledTimes(2);
  });

  it("empty state prompts to pick or create an agent when none selected", async () => {
    serversTab();
    renderAt("/terminals");
    await waitFor(() => expect(screen.getByText("a1")).toBeInTheDocument());
    expect(screen.getByRole("button", { name: "New agent" })).toBeInTheDocument();
  });

  it("marks non-interactive agents in the sidebar and leaves interactive ones unmarked", async () => {
    renderAt("/terminals");
    await waitFor(() => expect(screen.getByText("a2")).toBeInTheDocument());
    const a1Row = screen.getByText("a1").closest("button")!;
    const a2Row = screen.getByText("a2").closest("button")!;
    expect(a2Row.textContent).toMatch(/non-tty/);
    expect(a1Row.textContent).not.toMatch(/non-tty/);
  });

  it("carries only the unread message count in an agent row, never a question dot", async () => {
    vi.mocked(fetchAllAgents).mockResolvedValue([
      {
        host: { id: "remote-1", label: "prod" },
        agents: [{ name: "alice", image: "bare:latest", state: "running", harness: "claude", loop_enabled: false, group: null, interactive: true }],
        chats: [{ id: "dm:alice", kind: "direct", title: "alice", agent: "alice", last_ts: "2026-09-15T10:00:00Z", last_from: "agent:alice", last_type: "message", last_text: "hi", unread: 3 }],
      },
    ])

    renderAt("/", new Map([[JSON.stringify(["remote-1", "alice"]), 1]]))

    expect(await screen.findByLabelText("3 unread messages from alice")).toBeInTheDocument()
    expect(screen.queryByRole("img", { name: /customer question/ })).toBeNull()
  });

  it("re-polls immediately after creating an agent instead of waiting for the next tick", async () => {
    serversTab();
    vi.mocked(createAgent).mockResolvedValue({ name: "new-agent", state: "running" });
    renderAt("/terminals");
    await waitFor(() => expect(screen.getByText("a1")).toBeInTheDocument());
    const callsBefore = vi.mocked(fetchAllAgents).mock.calls.length;

    fireEvent.click(screen.getByRole("button", { name: "New agent" }));
    fireEvent.focus(await screen.findByLabelText("image"));
    fireEvent.click(await screen.findByRole("option", { name: "worker:v1" }));
    const create = screen.getByRole("button", { name: "Create agent" });
    await waitFor(() => expect(create).toBeEnabled());
    fireEvent.click(create);

    await waitFor(() => expect(createAgent).toHaveBeenCalled());
    await waitFor(() =>
      expect(vi.mocked(fetchAllAgents).mock.calls.length).toBeGreaterThan(callsBefore),
    );
  });

  it("opens Run Agent with both host and image preselected", async () => {
    const id = await registerProd();
    renderAt(`/terminals?new=1&host=${id}&image=worker%3Av1`);

    expect(await screen.findByRole("dialog")).toBeInTheDocument();
    expect(screen.getByRole("combobox", { name: "Host" })).toHaveTextContent("prod");
    expect(screen.getByLabelText("image")).toHaveValue("worker:v1");
    await waitFor(() =>
      expect(listImages).toHaveBeenCalledWith(expect.objectContaining({ id })),
    );
    expect(imageManifestGet).toHaveBeenCalledWith(
      "worker:v1",
      expect.objectContaining({ id }),
    );
  });

  it("offers a manage menu for remote servers but not for the local daemon", async () => {
    serversTab();
    await registerProd();
    renderAt("/terminals");
    await waitFor(() => expect(screen.getByText("prod")).toBeInTheDocument());
    expect(screen.getByRole("button", { name: "manage prod" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "manage This daemon (local)" })).toBeNull();
  });

  it("edits a server and re-polls immediately with the new values", async () => {
    serversTab();
    const id = await registerProd();
    renderAt("/terminals");
    await waitFor(() => expect(screen.getByText("prod")).toBeInTheDocument());
    const callsBefore = vi.mocked(fetchAllAgents).mock.calls.length;

    await openManageMenu();
    fireEvent.click(await screen.findByText("Edit host"));

    const label = (await screen.findByLabelText("label")) as HTMLInputElement;
    const url = screen.getByLabelText("base URL") as HTMLInputElement;
    await waitFor(() => {
      expect(label).toHaveValue("prod");
      expect(url).toHaveValue("http://127.0.0.1:9992");
    });

    fireEvent.change(label, { target: { value: "prod-renamed" } });
    fireEvent.change(url, { target: { value: "http://127.0.0.1:9993/" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(async () =>
      expect((await listDaemons()).find((m) => m.id === id)?.label).toBe("prod-renamed"),
    );
    const meta = (await listDaemons()).find((m) => m.id === id)!;
    expect(meta.label).toBe("prod-renamed");
    expect(meta.baseURL).toBe("http://127.0.0.1:9993");
    // Blank token field must not wipe the stored token.
    expect(await getDaemonToken(id)).toBe("tok");
    await waitFor(() =>
      expect(vi.mocked(fetchAllAgents).mock.calls.length).toBeGreaterThan(callsBefore),
    );
  });

  it("removes a server after confirmation and re-polls", async () => {
    serversTab();
    const id = await registerProd();
    const confirm = vi.spyOn(window, "confirm").mockReturnValue(true);
    renderAt("/terminals");
    await waitFor(() => expect(screen.getByText("prod")).toBeInTheDocument());
    const callsBefore = vi.mocked(fetchAllAgents).mock.calls.length;

    await openManageMenu();
    fireEvent.click(await screen.findByText("Remove host"));

    await waitFor(() => expect(confirm).toHaveBeenCalled());
    expect(confirm).toHaveBeenCalledWith(expect.stringMatching(/remote daemon and data remain/i));
    await waitFor(async () =>
      expect((await listDaemons()).find((m) => m.id === id)).toBeUndefined(),
    );
    expect(await getDaemonToken(id)).toBe("");
    await waitFor(() =>
      expect(vi.mocked(fetchAllAgents).mock.calls.length).toBeGreaterThan(callsBefore),
    );
  });

  it("keeps the server when the confirm is declined", async () => {
    serversTab();
    const id = await registerProd();
    vi.spyOn(window, "confirm").mockReturnValue(false);
    renderAt("/terminals");
    await waitFor(() => expect(screen.getByText("prod")).toBeInTheDocument());

    await openManageMenu();
    fireEvent.click(await screen.findByText("Remove host"));

    await waitFor(() => expect(window.confirm).toHaveBeenCalled());
    expect((await listDaemons()).find((m) => m.id === id)).toBeDefined();
  });

  it("navigates away when the server open in the route is removed", async () => {
    serversTab();
    const id = (await addDaemon({
      label: "prod", baseURL: "http://127.0.0.1:9992", token: "tok",
    })).id;
    vi.mocked(fetchAllAgents).mockResolvedValue([
      { host: { id: "", label: "This daemon (local)" }, agents: [] },
      {
        host: { id, label: "prod" },
        agents: [
          { name: "a1", image: "bare:latest", state: "running", harness: "claude", loop_enabled: false, group: null, interactive: true },
        ],
      },
    ]);
    vi.spyOn(window, "confirm").mockReturnValue(true);

    renderAt(`/agents/${id}/a1/console`);
    // The pane is up: "prod" is both the sidebar heading and the pane's host label.
    await waitFor(() => expect(screen.getByTestId("tui-screen")).toBeInTheDocument());

    await openManageMenu();
    fireEvent.click(await screen.findByText("Remove host"));

    // Back on the empty state — the terminal pane must not linger on a host id
    // that now resolves to the local daemon.
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "New agent" })).toBeInTheDocument(),
    );
  });
});

describe("host workspaces", () => {
  beforeEach(() => {
    vi.mocked(fetchAllAgents).mockResolvedValue([
      {
        host: { id: "", label: "This daemon (local)" },
        agents: [
          { name: "a1", image: "img", state: "running", harness: "claude", loop_enabled: false, group: null, interactive: true },
        ],
      },
      {
        host: { id: "d1", label: "prod" },
        agents: [
          { name: "b1", image: "img", state: "running", harness: "claude", loop_enabled: false, group: null, interactive: true },
        ],
      },
    ]);
  });

  it("filters the sidebar and its counts to the current workspace and follows the switch", async () => {
    moveHost("d1", createWorkspace("Production")!);
    renderAt("/agents/local/a1/activity");
    expect(await screen.findByRole("button", { name: "Open a1" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Open b1" })).toBeNull();
    expect(screen.getByText("1 server · 1 agent")).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "Workspace: Default" }));
    await userEvent.click(await screen.findByRole("menuitemradio", { name: /Production/ }));

    // The open agent is not in Production, so its first agent opens in the same tab.
    await waitFor(() =>
      expect(screen.getByTestId("location").textContent).toBe("/agents/d1/b1/activity"),
    );
    expect(await screen.findByRole("button", { name: "Open b1" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Open a1" })).toBeNull();
  });

  it("shows an empty workspace in the island with a way to manage it", async () => {
    createWorkspace("Lab");
    renderAt("/agents/local/a1/console");
    await screen.findByRole("button", { name: "Open a1" });

    await userEvent.click(screen.getByRole("button", { name: "Workspace: Default" }));
    await userEvent.click(await screen.findByRole("menuitemradio", { name: /Lab/ }));

    await waitFor(() => expect(screen.getByTestId("location").textContent).toBe("/"));
    expect(await screen.findByText("Lab has no agents yet")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Manage workspaces" }));
    const dialog = await screen.findByRole("dialog", { name: "Manage workspaces" });
    expect(within(dialog).getByLabelText("Workspace name")).toHaveValue("Lab");

    // Moving a host in updates the island without a reload.
    await userEvent.click(within(dialog).getByRole("button", { name: /^Default/ }));
    await userEvent.click(within(dialog).getByRole("button", { name: "Move prod to…" }));
    await userEvent.click(within(dialog).getByRole("button", { name: "Lab" }));
    expect(await screen.findByRole("button", { name: "Open b1", hidden: true })).toBeInTheDocument();
  });
});

describe("sidebar width", () => {
  it("starts at the persisted width", async () => {
    localStorage.setItem("terminals:sidebar:v1", JSON.stringify({ width: 380, hidden: false }));
    renderAt("/terminals");
    await waitFor(() => expect(screen.getByText("a1")).toBeInTheDocument());
    expect(screen.getByTestId("agents-sidebar")).toHaveStyle({ width: "380px" });
  });

  it("drags to a new width and persists it", async () => {
    renderAt("/terminals");
    await waitFor(() => expect(screen.getByText("a1")).toBeInTheDocument());
    const handle = screen.getByRole("separator", { name: "resize sidebar" });

    // jsdom has no layout, so the aside's rect.left is 0 and clientX is the width.
    fireEvent.pointerDown(handle);
    fireEvent.pointerMove(window, { clientX: 400 });
    fireEvent.pointerUp(window);

    expect(screen.getByTestId("agents-sidebar")).toHaveStyle({ width: "400px" });
    expect(JSON.parse(localStorage.getItem("terminals:sidebar:v1")!))
      .toMatchObject({ width: 400, hidden: false });

    // Drag ended: further pointer moves must not keep resizing.
    fireEvent.pointerMove(window, { clientX: 200 });
    expect(JSON.parse(localStorage.getItem("terminals:sidebar:v1")!))
      .toMatchObject({ width: 400, hidden: false });
  });

  it("clamps a drag past the maximum", async () => {
    renderAt("/terminals");
    await waitFor(() => expect(screen.getByText("a1")).toBeInTheDocument());
    fireEvent.pointerDown(screen.getByRole("separator", { name: "resize sidebar" }));
    fireEvent.pointerMove(window, { clientX: 5000 });
    fireEvent.pointerUp(window);
    expect(JSON.parse(localStorage.getItem("terminals:sidebar:v1")!))
      .toMatchObject({ width: 640, hidden: false });
  });

  it("resets to the default on double-click", async () => {
    localStorage.setItem("terminals:sidebar:v1", JSON.stringify({ width: 500, hidden: false }));
    renderAt("/terminals");
    await waitFor(() => expect(screen.getByText("a1")).toBeInTheDocument());
    fireEvent.doubleClick(screen.getByRole("separator", { name: "resize sidebar" }));
    expect(JSON.parse(localStorage.getItem("terminals:sidebar:v1")!))
      .toMatchObject({ width: 268, hidden: false });
  });

  it("resizes with arrow keys", async () => {
    localStorage.setItem("terminals:sidebar:v1", JSON.stringify({ width: 300, hidden: false }));
    renderAt("/terminals");
    await waitFor(() => expect(screen.getByText("a1")).toBeInTheDocument());
    const handle = screen.getByRole("separator", { name: "resize sidebar" });
    fireEvent.keyDown(handle, { key: "ArrowRight" });
    expect(JSON.parse(localStorage.getItem("terminals:sidebar:v1")!))
      .toMatchObject({ width: 308, hidden: false });
    fireEvent.keyDown(handle, { key: "ArrowLeft", shiftKey: true });
    expect(JSON.parse(localStorage.getItem("terminals:sidebar:v1")!))
      .toMatchObject({ width: 276, hidden: false });
  });
});

describe("sidebar tabs", () => {
  const twoHosts = [
    {
      host: { id: "", label: "This daemon (local)" },
      groups: [{ name: "release", lead: "alpha", members: 1 }],
      agents: [
        { name: "alpha", image: "img", state: "running", harness: "claude", loop_enabled: false, group: "release", interactive: true },
        { name: "beta", image: "img", state: "stopped", harness: "claude", loop_enabled: false, group: null, interactive: true },
      ],
    },
    {
      host: { id: "prod", label: "prod" },
      agents: [
        { name: "gamma", image: "img", state: "running", harness: "claude", loop_enabled: false, group: "release", interactive: true },
      ],
    },
  ];

  it("opens on Agents and lists every server's agents without server sections", async () => {
    vi.mocked(fetchAllAgents).mockResolvedValue(twoHosts);
    renderAt("/");

    expect(await screen.findByRole("button", { name: "Open alpha" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Open gamma" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Open server prod" })).toBeNull();
  });

  it("remembers the chosen tab across mounts", async () => {
    vi.mocked(fetchAllAgents).mockResolvedValue(twoHosts);
    const first = renderAt("/");
    await screen.findByRole("tab", { name: "Servers" });
    pickSidebarTab("Servers");
    expect(await screen.findByRole("button", { name: "Open server prod" })).toBeInTheDocument();
    first.unmount();

    renderAt("/");
    expect(await screen.findByRole("button", { name: "Open server prod" })).toBeInTheDocument();
  });

  it("puts a called-for agent above the rest, and a pinned one above that", async () => {
    vi.mocked(fetchAllAgents).mockResolvedValue(twoHosts);
    renderAt("/", new Map([[JSON.stringify(["prod", "gamma"]), 1]]));

    const names = () => screen.getAllByTestId("sidebar-agent").map((row) => row.textContent);
    await waitFor(() => expect(names()).toHaveLength(3));
    expect(names()[0]).toMatch(/gamma/);

    fireEvent.contextMenu(screen.getByRole("button", { name: "Open beta" }));
    fireEvent.click(await screen.findByRole("menuitem", { name: "Pin" }));
    await waitFor(() => expect(names()[0]).toMatch(/beta/));
    expect(names()[1]).toMatch(/gamma/);
    expect(JSON.parse(localStorage.getItem("terminals:sidebar-pinned:v1")!))
      .toEqual({ version: 1, keys: [JSON.stringify(["", "beta"])] });
  });

  it("groups agents from every server under one group heading", async () => {
    vi.mocked(fetchAllAgents).mockResolvedValue(twoHosts);
    renderAt("/");
    await screen.findByRole("tab", { name: "Groups" });
    pickSidebarTab("Groups");

    const release = await screen.findByRole("button", { name: "Open team release" });
    expect(release).toBeInTheDocument();
    const section = release.closest("section")!;
    expect(section.textContent).toMatch(/alpha/);
    expect(section.textContent).toMatch(/gamma/);
    expect(section.textContent).not.toMatch(/beta/);
  });
});
