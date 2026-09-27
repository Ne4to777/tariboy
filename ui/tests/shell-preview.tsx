// Console shell preview fixture: the real App over a stubbed daemon, for
// eyeballing the style layer (and for tests/shell-preview-shot.mjs). Not part
// of any test run — it is a development harness, served by
// vite.fixtures.config.ts like the other fixtures in this directory.
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import App from "@/App";
import "@/index.css";

const AGENTS = [
  { name: "builder", image: "worker:v2", state: "running", harness: "claude", loop_enabled: true, group: "core", interactive: true, current_goal_task_key: "TB-142", cwd: "/home/agent/github/tariboy" },
  { name: "reviewer", image: "worker:v2", state: "running", harness: "claude", loop_enabled: false, group: "core", interactive: true },
  { name: "packager", image: "bare:latest", state: "failed", harness: "codex", loop_enabled: false, group: null, interactive: true },
  { name: "docs-bot", image: "bare:latest", state: "stopped", harness: "codex", loop_enabled: false, group: null, interactive: false },
];

const TASKS = [
  { key: "TB-142", queue: "TB", parent_key: "", position: 1, priority: "P1", title: "Migrate store schema", description: "", status: "in_progress", author: "customer:ops", customer: "customer:ops", group: "", assignee: "agent:builder", manual_block_reason: "", blocked: false, revision: 1, created_at: "2026-09-13T09:10:00Z", updated_at: "2026-09-13T14:02:00Z", completed_at: "" },
  { key: "TB-143", queue: "TB", parent_key: "TB-142", position: 1, priority: "P2", title: "Dump current schema", description: "", status: "done", author: "customer:ops", customer: "customer:ops", group: "", assignee: "agent:builder", manual_block_reason: "", blocked: false, revision: 1, created_at: "2026-09-13T09:12:00Z", updated_at: "2026-09-13T13:44:00Z", completed_at: "2026-09-13T13:44:00Z" },
  { key: "TB-144", queue: "TB", parent_key: "TB-142", position: 2, priority: "P1", title: "Write migration", description: "", status: "wait_customer", author: "customer:ops", customer: "customer:ops", group: "", assignee: "agent:builder", manual_block_reason: "", blocked: false, revision: 1, created_at: "2026-09-13T09:14:00Z", updated_at: "2026-09-13T14:02:00Z", completed_at: "" },
  { key: "TB-146", queue: "TB", parent_key: "TB-144", position: 1, priority: "P2", title: "Generate SQL", description: "", status: "done", author: "customer:ops", customer: "customer:ops", group: "", assignee: "agent:reviewer", manual_block_reason: "", blocked: false, revision: 1, created_at: "2026-09-13T09:20:00Z", updated_at: "2026-09-13T13:58:00Z", completed_at: "2026-09-13T13:58:00Z" },
  { key: "TB-147", queue: "TB", parent_key: "TB-144", position: 2, priority: "P2", title: "Verify rollback", description: "", status: "open", author: "customer:ops", customer: "customer:ops", group: "", assignee: "agent:reviewer", manual_block_reason: "", blocked: true, revision: 1, created_at: "2026-09-13T09:22:00Z", updated_at: "2026-09-12T18:00:00Z", completed_at: "" },
  { key: "TB-145", queue: "TB", parent_key: "TB-142", position: 3, priority: "P3", title: "Update docs", description: "", status: "cancelled", author: "customer:ops", customer: "customer:ops", group: "", assignee: "agent:docs-bot", manual_block_reason: "", blocked: false, revision: 1, created_at: "2026-09-08T09:22:00Z", updated_at: "2026-09-08T11:00:00Z", completed_at: "" },
];

// One task's detail, so the right-side panel can be eyeballed the way the
// console shell is. TB-144 is the interesting one: it waits on the customer,
// carries a comment thread, dependencies and history.
const DETAIL_KEY = "TB-144";
const COMMENTS = [
  { id: 1, task_key: DETAIL_KEY, author: "agent:builder", body: "Two safe rollouts are possible. Behind a flag we can ship today; a staged migration needs a maintenance window.\n\nWhich do you want?", revision: 1, created_at: "2026-09-13T13:20:00Z", updated_at: "2026-09-13T13:20:00Z" },
  { id: 2, task_key: DETAIL_KEY, author: "customer:ops", body: "Checking with the on-call team.", revision: 1, created_at: "2026-09-13T13:52:00Z", updated_at: "2026-09-13T13:52:00Z" },
];
const WAITS = [
  { id: 1, task_key: DETAIL_KEY, expected_principal: "customer:ops", requesting_principal: "agent:builder", requesting_comment_id: 1, requested_at: "2026-09-13T13:20:00Z" },
];
const RELATIONS = [
  { id: 1, source_key: DETAIL_KEY, source_title: "Write migration for the skills column", source_status: "wait_customer", target_key: "TB-142", target_title: "Packager drops skills when the template declares no plugins", target_status: "in_progress", type: "blocks", created_by: "agent:builder", created_at: "2026-09-13T09:30:00Z" },
  { id: 2, source_key: DETAIL_KEY, source_title: "Write migration for the skills column", source_status: "wait_customer", target_key: "TB-133", target_title: "Nightly bundle fails to sign artifacts", target_status: "open", type: "related", created_by: "agent:builder", created_at: "2026-09-13T09:31:00Z" },
];
const EVENTS = [
  { sequence: 4, event_id: "e4", task_key: DETAIL_KEY, queue: "TB", kind: "task.comment.added", actor: "customer:ops", task_revision: 4, payload: { comment_id: 2 }, created_at: "2026-09-13T13:52:00Z" },
  { sequence: 3, event_id: "e3", task_key: DETAIL_KEY, queue: "TB", kind: "task.status.changed", actor: "agent:builder", task_revision: 3, payload: { from: "in_progress", to: "wait_customer" }, created_at: "2026-09-13T13:20:00Z" },
  { sequence: 2, event_id: "e2", task_key: DETAIL_KEY, queue: "TB", kind: "workflow.assignment.started", actor: "agent:builder", task_revision: 2, payload: { attempt: 1, node: "write_migration" }, created_at: "2026-09-13T09:40:00Z" },
  { sequence: 1, event_id: "e1", task_key: DETAIL_KEY, queue: "TB", kind: "task.created", actor: "customer:ops", task_revision: 1, payload: {}, created_at: "2026-09-13T09:14:00Z" },
];
const DESCRIPTION = "The migration has to run without a maintenance window on hosts that are already serving.\n\n### Repro\n\n1. Start the daemon on a store built before the schema change.\n2. Apply the migration while a session is open.\n3. The session is reaped instead of resumed.";

// Dark mode is a class on <html> (ThemeProvider does this in the real entry).
if (new URLSearchParams(location.search).get("theme") === "dark") {
  document.documentElement.classList.add("dark");
}

const real = window.fetch;
window.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
  const url = String(typeof input === "string" ? input : input instanceof URL ? input : input.url);
  const json = (result: unknown) => new Response(JSON.stringify({ ok: true, result }), { status: 200, headers: { "content-type": "application/json" } });
  if (url.includes("/api/agents")) return json({ agents: AGENTS, count: AGENTS.length });
  if (url.includes("/api/groups")) return json({ groups: [{ name: "core", lead: "builder", members: 2 }], count: 1 });
  if (url.includes("/api/daemon/config")) return json({});
  if (url.includes("/api/task-queues")) return json({ queues: [{ prefix: "TB", title: "Tariboy", description: "" }], count: 1 });
  if (url.includes("/api/task-notifications")) return json({ notifications: [], count: 0 });
  if (url.includes("/api/task-principals")) return json({ customer: "customer:ops", agents: AGENTS.map((agent) => agent.name) });
  if (url.includes(`/api/tasks/${DETAIL_KEY}/artifacts`)) return json({ items: [], count: 0 });
  if (url.includes(`/api/tasks/${DETAIL_KEY}/questions`)) return json({ items: [], count: 0 });
  if (url.includes(`/api/tasks/${DETAIL_KEY}/events`)) return json({ events: EVENTS, count: EVENTS.length });
  if (url.includes(`/api/tasks/${DETAIL_KEY}/workflow`)) return json(null);
  if (url.includes(`/api/tasks/${DETAIL_KEY}`)) {
    const task = { ...TASKS.find((candidate) => candidate.key === DETAIL_KEY)!, description: DESCRIPTION, access: "write" };
    return json({ task, comments: COMMENTS, waiting_for: WAITS, relations: RELATIONS });
  }
  if (url.includes("/api/tasks")) return json({ tasks: TASKS, count: TASKS.length, sequence: 10 });
  if (url.includes("/status")) return json({ state: "running", messages_pending: 0 });
  if (url.includes("/api/")) return json({});
  return real(input, init);
}) as typeof fetch;

// The preview shows the full Expert workspace.
localStorage.setItem("app:ui-mode:v1", "expert");

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <MemoryRouter initialEntries={[new URLSearchParams(location.search).get("route") ?? "/agents/local/builder/console"]}>
      <Routes><Route path="*" element={<App />} /></Routes>
    </MemoryRouter>
  </StrictMode>,
);
