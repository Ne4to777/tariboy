import { afterEach, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { AgentNameContext, AgentStatusContext } from "@/lib/agent";
import type { AgentStatus } from "@/lib/types";
import AgentAutopilotTab from "./AgentAutopilotTab";

// The tab's own halt-reason line is what these cases assert. Its children own
// their behaviour and their own tests, so they are stubbed to keep the render
// free of loop controls and subscription fetches.
vi.mock("@/components/LoopControls", () => ({
  LoopToggle: () => <div data-testid="loop-toggle" />,
}));
vi.mock("@/components/IterationTimeoutControl", () => ({
  IterationTimeoutControl: () => <div data-testid="iteration-timeout" />,
}));

afterEach(() => vi.restoreAllMocks());

const stopped: AgentStatus = {
  name: "worker",
  state: "stopped",
  loop_enabled: false,
  iterations: 3,
  last_iteration: null,
  last_iteration_id: null,
  status_message: "",
  status_updated: "",
};

function response(result: unknown, ok = true, status = 200) {
  const body = ok
    ? { ok: true, result }
    : { ok: false, error: result };
  return Promise.resolve({
    ok,
    status,
    text: async () => JSON.stringify(body),
  } as Response);
}

// AgentAutopilotTab fires apiGet("/api/budgets/status") and the subscription
// count in mount effects; stub both so the render is not waiting on a real
// request.
function renderAutopilot(status: AgentStatus, subscriptions: Promise<Response> = response({ channels: [] })) {
  vi.stubGlobal("fetch", vi.fn((path: string) =>
    path === "/api/agents/worker/subscriptions" ? subscriptions : response({ budgets: [] })));
  return render(
    <MemoryRouter initialEntries={["/agents/local/worker/autopilot"]}>
      <AgentNameContext.Provider value="worker">
        <AgentStatusContext.Provider value={{ status, refresh: async () => {} }}>
          <Routes>
            <Route path="/agents/:hostId/:agent/:tab/*" element={<AgentAutopilotTab />} />
          </Routes>
        </AgentStatusContext.Provider>
      </AgentNameContext.Provider>
    </MemoryRouter>,
  );
}

it("shows the idle-limit halt reason under the Running/Stopped line", async () => {
  renderAutopilot({
    ...stopped,
    halt_kind: "idle_limit",
    halt_reason: "idle_limit (3 idle iterations)",
  });
  expect(await screen.findByText("idle_limit (3 idle iterations)")).toBeInTheDocument();
  expect(screen.getByTestId("halt-reason")).toHaveClass("text-muted-foreground");
  expect(screen.getByTestId("halt-reason")).not.toHaveClass("text-destructive");
});

// The "error" halt_kind compared here is produced by (*Agent).HaltReason in
// internal/agent/agent.go (declared at line 208), whose first case returns the
// bare literal at line 211: `return "error", a.ErrorReason`. TypeScript cannot
// import that constant, so this comment is the only coupling: if the two
// literals ever diverge the line still renders, in the wrong tone, silently.
it("shows the error halt reason under the Running/Stopped line", async () => {
  renderAutopilot({ ...stopped, halt_kind: "error", halt_reason: "halted: boom" });
  expect(await screen.findByText("halted: boom")).toBeInTheDocument();
  expect(screen.getByTestId("halt-reason")).toHaveClass("text-destructive");
  expect(screen.getByTestId("halt-reason")).not.toHaveClass("text-muted-foreground");
});

it("renders no halt-reason element when the daemon reports no reason", async () => {
  renderAutopilot(stopped);
  expect(await screen.findByText("Stopped")).toBeInTheDocument();
  await waitFor(() => expect(screen.queryByTestId("halt-reason")).not.toBeInTheDocument());
});

it("renders no halt-reason element when the reason is an empty string", async () => {
  renderAutopilot({ ...stopped, halt_kind: "", halt_reason: "" });
  expect(await screen.findByText("Stopped")).toBeInTheDocument();
  await waitFor(() => expect(screen.queryByTestId("halt-reason")).not.toBeInTheDocument());
});

it("shows an informational AI stall reason without reporting a halt", async () => {
  renderAutopilot({
    ...stopped,
    state: "error",
    loop_enabled: true,
    error_reason: "agent stalled - no AI-proxy requests for 300 seconds",
  });
  expect(
    await screen.findByText("agent stalled - no AI-proxy requests for 300 seconds"),
  ).toHaveClass("text-destructive");
  expect(screen.queryByTestId("halt-reason")).not.toBeInTheDocument();
});

it("counts the event triggers and links to Chat → Channels instead of editing them", async () => {
  renderAutopilot(stopped, response({ channels: [
    { name: "agent:worker", kind: "agent", protected: true },
    { name: "chat:release", kind: "chat", protected: false },
  ] }));

  expect(await screen.findByText("Event triggers: 2 channels")).toBeInTheDocument();
  expect(screen.getByRole("link", { name: "Manage in Chat → Channels" }))
    .toHaveAttribute("href", "/agents/local/worker/chat?view=channels");
  expect(screen.queryByRole("button", { name: "Subscribe" })).not.toBeInTheDocument();
  expect(screen.queryByRole("combobox")).not.toBeInTheDocument();
});

it("keeps the event-trigger link when the count cannot be loaded", async () => {
  renderAutopilot(stopped, response("boom", false, 500));

  expect(await screen.findByRole("link", { name: "Manage in Chat → Channels" })).toBeInTheDocument();
  await waitFor(() => expect(screen.getByText("Event triggers")).toBeInTheDocument());
});
