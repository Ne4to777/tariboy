import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";
import AgentAdvancedTab from "./AgentAdvancedTab";
import { AgentNameContext } from "@/lib/agent";

// Each view owns its own behaviour and tests; Advanced only routes between them.
vi.mock("@/pages/AgentPrompt", () => ({ default: () => <div data-testid="prompt-view" /> }));

beforeEach(() => {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue({
    ok: true, status: 200, text: async () => JSON.stringify({ ok: true, result: {} }),
  } as unknown as Response));
});
afterEach(() => vi.unstubAllGlobals());

function Location() {
  const location = useLocation();
  return <div data-testid="location">{location.pathname + location.search}</div>;
}

function renderAdvanced(path: string) {
  render(
    <MemoryRouter initialEntries={[path]}>
      <AgentNameContext.Provider value="worker">
        <Routes>
          <Route path="/agents/:hostId/:agent/advanced/*" element={<AgentAdvancedTab />} />
          <Route path="/agents/:hostId/:agent/chat/*" element={<Location />} />
        </Routes>
      </AgentNameContext.Provider>
    </MemoryRouter>,
  );
}

it("opens on Prompt and no longer lists Channels or Messages", async () => {
  renderAdvanced("/agents/local/worker/advanced");

  expect(await screen.findByTestId("prompt-view")).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Channels" })).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Messages" })).not.toBeInTheDocument();
});

it("sends an old Channels link to Chat → Channels", async () => {
  renderAdvanced("/agents/local/worker/advanced?view=channels");

  expect(await screen.findByTestId("location")).toHaveTextContent("/agents/local/worker/chat?view=channels");
});

it("sends an old Messages link to Chat → Queue", async () => {
  renderAdvanced("/agents/local/worker/advanced?view=messages");

  expect(await screen.findByTestId("location")).toHaveTextContent("/agents/local/worker/chat?view=queue");
});
