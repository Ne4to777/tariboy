import type { Daemon } from "@/lib/daemons"
import type { ScriptRun, Task, WorkflowView } from "@/lib/tasks"

/** Test fixtures for the workflow panel and its sections. */

export const remoteTarget: Daemon = { id: "remote", label: "remote", baseURL: "http://remote", token: "t" }

export const workflowTask: Task = {
  key: "REL-1",
  queue: "REL",
  parent_key: "",
  position: 0,
  priority: "P2",
  title: "Release notes",
  description: "Write them",
  status: "review",
  category: "wait_customer",
  waiting_on: "customer",
  workflow_name: "release",
  workflow_version: "1.2.0",
  workflow_digest: "sha256:abc",
  pull_request: "",
  author: "user:owner",
  customer: "user:owner",
  group: "",
  assignee: "agent:writer",
  manual_block_reason: "",
  blocked: false,
  revision: 4,
  created_at: "2026-10-01T10:00:00Z",
  updated_at: "2026-10-01T10:00:00Z",
  completed_at: "",
}

export const checkRun: ScriptRun = {
  id: 7,
  task_key: "REL-1",
  kind: "check",
  script: "./checks/notes.sh",
  run_as: "queue",
  state: "finished",
  verdict: "reject",
  exit_code: 112,
  message: "the notes must start with a heading",
  created_at: "2026-10-01T10:05:00Z",
  started_at: "2026-10-01T10:05:01Z",
  finished_at: "2026-10-01T10:05:03Z",
}

export const watchRun: ScriptRun = {
  id: 9,
  task_key: "REL-1",
  kind: "watch",
  script: "./scripts/ci.sh",
  run_as: "queue",
  state: "running",
  created_at: "2026-10-01T10:09:00Z",
  started_at: "2026-10-01T10:09:01Z",
}

/** A task in the customer status `review`, with two outcomes. */
export const customerView: WorkflowView = {
  name: "release",
  version: "1.2.0",
  digest: "sha256:abc",
  status: "review",
  category: "wait_customer",
  waiting_on: "customer",
  owner: "customer",
  outcomes: [
    { on: "approve", to: "publish", requires: ["notes"], missing: [], checks: ["./checks/notes.sh"] },
    { on: "rework", to: "draft" },
  ],
  artifacts: [
    { id: 3, name: "notes", value: "# Notes\nline 2", author: "agent:writer", created_at: "2026-10-01T10:04:00Z" },
  ],
  visits: [
    { id: 1, sequence: 1, status: "draft", entered_at: "2026-10-01T10:00:00Z", entered_by: "user:owner",
      left_at: "2026-10-01T10:04:30Z", outcome: "drafted", message: "first cut" },
    { id: 2, sequence: 2, status: "review", entered_at: "2026-10-01T10:04:30Z", entered_by: "agent:writer" },
  ],
  runs: [watchRun, checkRun],
}

/** The same task in the pool status `draft`, held by an agent. */
export const poolView: WorkflowView = {
  ...customerView,
  status: "draft",
  category: "in_progress",
  waiting_on: undefined,
  owner: "pool:writers",
  holder: "agent:writer",
  outcomes: [
    { on: "drafted", to: "review", requires: ["notes", "summary"], missing: ["summary"], checks: ["./checks/notes.sh"] },
  ],
}
