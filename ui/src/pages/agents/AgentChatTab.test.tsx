import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import AgentChatTab from "./AgentChatTab";
import { AgentNameContext } from "@/lib/agent";
import { UI_MODE_KEY } from "@/lib/uiMode";

beforeEach(() => {
  localStorage.clear();
  vi.stubGlobal("WebSocket", class { close = vi.fn(); });
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue({
    ok: true, status: 200,
    text: async () => JSON.stringify({ ok: true, result: { messages: [], count: 0, read_ts: "", channels: [] } }),
  } as unknown as Response));
});
afterEach(() => {
  vi.unstubAllGlobals();
  localStorage.clear();
});

function renderTab(path = "/") {
  render(
    <MemoryRouter initialEntries={[path]}>
      <AgentNameContext.Provider value="worker">
        <AgentChatTab />
      </AgentNameContext.Provider>
    </MemoryRouter>,
  );
}

it("in Simple shows only the personal chat, its toolbar starting at search", async () => {
  renderTab("/?view=channels");

  expect(await screen.findByRole("searchbox", { name: "Search messages" })).toBeInTheDocument();
  expect(screen.queryByRole("group", { name: "Messaging view" })).not.toBeInTheDocument();
  expect(screen.queryByRole("tablist", { name: "Agent chats" })).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "New chat" })).not.toBeInTheDocument();
});

it("in Expert keeps the Chat/Channels switch and the agent chats", async () => {
  localStorage.setItem(UI_MODE_KEY, "expert");
  renderTab();

  expect(await screen.findByRole("group", { name: "Messaging view" })).toBeInTheDocument();
  expect(screen.getByRole("tablist", { name: "Agent chats" })).toBeInTheDocument();
  expect(screen.getByRole("searchbox", { name: "Search messages" })).toBeInTheDocument();
});

it("in Expert offers Chat, Channels and Queue in one switch", async () => {
  localStorage.setItem(UI_MODE_KEY, "expert");
  renderTab();

  const group = await screen.findByRole("group", { name: "Messaging view" });
  expect(within(group).getAllByRole("button").map((button) => button.textContent))
    .toEqual(["Chat", "Channels", "Queue"]);
});

it("opens the delivery queue with its Queue, Archive and DLQ views under ?view=queue", async () => {
  localStorage.setItem(UI_MODE_KEY, "expert");
  renderTab("/?view=queue");

  const views = await screen.findByRole("tablist", { name: "Queue view" });
  expect(within(views).getAllByRole("tab").map((tab) => tab.textContent))
    .toEqual(["Queue", "Archive", "DLQ"]);
  expect(screen.getByRole("button", { name: "Queue", pressed: true })).toBeInTheDocument();
});

it("manages subscriptions under ?view=channels", async () => {
  localStorage.setItem(UI_MODE_KEY, "expert");
  renderTab("/?view=channels");

  expect(await screen.findByRole("combobox")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Subscribe" })).toBeInTheDocument();
  expect(screen.getByText(/wakes this agent in Autopilot/)).toBeInTheDocument();
});

it("in Simple never opens the queue", async () => {
  renderTab("/?view=queue");

  expect(await screen.findByRole("searchbox", { name: "Search messages" })).toBeInTheDocument();
  expect(screen.queryByRole("tablist", { name: "Queue view" })).not.toBeInTheDocument();
});
