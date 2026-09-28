import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, useLocation } from "react-router-dom";
import type { ReactNode } from "react";
import { fetchAllAgents } from "@/lib/aggregate";
import App from "./App";
import { UI_MODE_KEY } from "@/lib/uiMode";

vi.mock("@/lib/aggregate", () => ({
  fetchAllAgents: vi.fn(),
}));

vi.mock("@/components/CustomerQuestionNotifications", () => ({
  CustomerQuestionNotifications: ({ children }: { children: ReactNode }) => children,
}));

function LocationProbe() {
  const location = useLocation();
  return <output data-testid="location">{location.pathname + location.search}</output>;
}

function renderAt(path: string) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <App />
      <LocationProbe />
    </MemoryRouter>,
  );
}

beforeEach(() => {
  localStorage.clear();
  sessionStorage.clear();
  vi.mocked(fetchAllAgents).mockResolvedValue([
    { host: { id: "", label: "This daemon (local)" }, agents: [] },
  ]);
  vi.stubGlobal(
    "fetch",
    vi.fn().mockImplementation(async (input: RequestInfo | URL) => {
      const url = String(input);
      const result = url.includes("/api/usage") ? {
          total_requests: 0, total_cost_usd: 0, total_input_tokens: 0,
          total_output_tokens: 0, total_cache_write_tokens: 0, total_cache_read_tokens: 0,
          rows: [], series: [], requests: [],
        }
        : url.includes("/api/groups") ? { groups: [], count: 0 }
        : url.includes("/api/channels") ? { channels: [] }
        : { agents: [], count: 0 };
      return {
        ok: true,
        status: 200,
        text: async () => JSON.stringify({ ok: true, result }),
      } as Response;
    }),
  );
});

afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

describe("product routing", () => {
  it("hides Workspace and keeps the sidebar control in the titlebar", () => {
    renderAt("/");

    const titlebar = screen.getByTestId("app-titlebar");
    for (const label of ["Workspace", "Agents", "Tasks", "Images", "Settings"]) {
      expect(within(titlebar).queryByRole("link", { name: label })).toBeNull();
    }
    expect(within(titlebar).getByRole("button", { name: "Hide agents" }))
      .toBeInTheDocument();
    expect(within(titlebar).getByRole("button", { name: "Toggle theme" }))
      .toBeInTheDocument();
  });

  it("switches between Agents and All tasks through ?view=all on the current route", () => {
    renderAt("/agents/local/builder/console?task=X-1");

    const titlebar = screen.getByTestId("app-titlebar");
    const view = within(titlebar).getByRole("radiogroup", { name: "View" });
    expect(within(view).getByRole("radio", { name: "Agents" })).toHaveAttribute("aria-checked", "true");

    fireEvent.click(within(view).getByRole("radio", { name: "All tasks" }));
    expect(screen.getByTestId("location")).toHaveTextContent("/agents/local/builder/console?task=X-1&view=all");
    expect(within(view).getByRole("radio", { name: "All tasks" })).toHaveAttribute("aria-checked", "true");

    fireEvent.click(within(view).getByRole("radio", { name: "Agents" }));
    expect(screen.getByTestId("location").textContent).toBe("/agents/local/builder/console?task=X-1");
  });

  it("hides the agents sidebar control in All tasks", () => {
    renderAt("/agents/local/builder/console?view=all");

    const titlebar = screen.getByTestId("app-titlebar");
    expect(within(titlebar).queryByRole("button", { name: "Hide agents" })).toBeNull();
    expect(within(titlebar).queryByRole("button", { name: "Show agents" })).toBeNull();

    fireEvent.click(within(titlebar).getByRole("radio", { name: "Agents" }));
    expect(within(titlebar).getByRole("button", { name: "Hide agents" })).toBeInTheDocument();
  });

  it("opens daemon-independent application settings from the titlebar", async () => {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new Error("daemon down")));
    renderAt("/");

    const titlebar = screen.getByTestId("app-titlebar");
    const settings = within(titlebar).getByRole("link", { name: "Application settings" });
    expect(settings).toHaveAttribute("href", "/app-settings");
    fireEvent.click(settings);

    await waitFor(() => expect(screen.getByTestId("location")).toHaveTextContent("/app-settings"));
    expect(screen.getByRole("heading", { name: "Application settings" })).toBeInTheDocument();
    expect(screen.getByText("Updates are only available in the Desktop app."))
      .toBeInTheDocument();
  });

  it("renders Tasks inside an explicit server workspace", async () => {
    renderAt("/servers/local/tasks");

    expect(await screen.findByLabelText("Search tasks")).toBeInTheDocument();
    expect(screen.getByTestId("tasks-workspace")).toHaveAttribute("data-scope-agent", "");
    expect(screen.getByRole("navigation", { name: "Server workspace" }))
      .toBeInTheDocument();
  });

  it.each([
    ["/tasks?queue=ops", "/servers/local/tasks?queue=ops"],
    ["/settings/advanced/usage?range=7d", "/servers/local/settings/advanced/usage?range=7d"],
  ])("redirects legacy server surface %s without losing its tail", async (from, to) => {
    renderAt(from);
    await waitFor(() => expect(screen.getByTestId("location")).toHaveTextContent(to));
  });

  it("preserves the active remote host when redirecting an image detail", async () => {
    localStorage.setItem("tariboy_active_daemon", "remote-1");
    renderAt("/images/worker/v1/template?raw=1");
    await waitFor(() =>
      expect(screen.getByTestId("location")).toHaveTextContent(
        "/servers/remote-1/images/worker/v1/template?raw=1",
      ),
    );
  });

  it("toggles the Agents sidebar from the left side of the window toolbar", () => {
    renderAt("/");

    const toggle = screen.getByRole("button", { name: "Hide agents" });

    fireEvent.click(toggle);

    expect(screen.getByRole("button", { name: "Show agents" })).toBeInTheDocument();
    expect(JSON.parse(localStorage.getItem("terminals:sidebar:v1")!))
      .toMatchObject({ hidden: true });
  });

  it("keeps the macOS titlebar safe and draggable when the daemon banner is visible", async () => {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new Error("daemon down")));
    renderAt("/");

    const header = screen.getByTestId("app-titlebar");
    const daemonBanner = await screen.findByText(
      "daemon unreachable — is tariboyd running?",
    );
    expect(header).toHaveAttribute("data-tauri-drag-region", "deep");
    expect(screen.getByRole("button", { name: "Hide agents" }))
      .not.toHaveAttribute("data-tauri-drag-region");
    for (const link of header.querySelectorAll("a")) {
      expect(link).not.toHaveAttribute("data-tauri-drag-region");
    }
    expect(
      header.compareDocumentPosition(daemonBanner) & Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
  });

  it("uses Agents as the default route", () => {
    renderAt("/");
    expect(screen.getByTestId("location")).toHaveTextContent(/^\/$/);
    expect(screen.getByRole("complementary", { name: "Agents" })).toBeInTheDocument();
  });

  it("redirects a legacy terminal route to the Console tab", async () => {
    localStorage.setItem(UI_MODE_KEY, "expert");
    renderAt("/terminals/local/worker");
    await waitFor(() =>
      expect(screen.getByTestId("location")).toHaveTextContent(
        "/agents/local/worker/console",
      ),
    );
  });

  it("redirects a legacy terminal route to Chat in Simple mode", async () => {
    renderAt("/terminals/local/worker");
    await waitFor(() =>
      expect(screen.getByTestId("location")).toHaveTextContent(
        "/agents/local/worker/chat",
      ),
    );
  });

  it("preserves create preselection while canonicalizing old entry points", async () => {
    renderAt("/terminals?new=1&host=remote-1&image=worker%3Av1");
    await waitFor(() =>
      expect(screen.getByTestId("location")).toHaveTextContent(
        "/?new=1&host=remote-1&image=worker%3Av1",
      ),
    );
  });

  it("preserves image and host parameters from /agents/new", async () => {
    renderAt("/agents/new?image=worker%3Av1&host=remote-1");
    await waitFor(() =>
      expect(screen.getByTestId("location")).toHaveTextContent(
        "/?image=worker%3Av1&host=remote-1&new=1",
      ),
    );
  });

  it("resolves legacy agent tabs against the selected host", async () => {
    localStorage.setItem("tariboy_active_daemon", "remote-1");
    renderAt("/agent/worker/logs");
    await waitFor(() =>
      expect(screen.getByTestId("location")).toHaveTextContent(
        "/agents/remote-1/worker/activity",
      ),
    );
  });

  it.each([
    ["/usage", "/settings/advanced/usage"],
    ["/groups", "/settings/advanced/groups"],
    ["/ops", "/settings/advanced/ops"],
    ["/channels", "/settings/advanced/channels"],
  ])("redirects operator route %s under Settings", async (from, to) => {
    renderAt(from);
    await waitFor(() => expect(screen.getByTestId("location")).toHaveTextContent(to));
  });

  it("keeps an unknown host id fail-closed instead of rewriting it to local", async () => {
    renderAt("/agents/removed-host/worker/console");
    await waitFor(() =>
      expect(screen.getByTestId("location")).toHaveTextContent(
        "/agents/removed-host/worker/console",
      ),
    );
    expect(screen.getByTestId("location")).not.toHaveTextContent("/agents/local/");
  });
});
