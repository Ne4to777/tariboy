# Workflow UI Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Show workflow tasks in Desktop: their status and category in the list, and in the task drawer the outcomes, artifacts, script runs, and pause decisions; let the customer bind a workflow image, pools, and secrets in queue settings.

**Architecture:** `ui/src/lib/tasks.ts` gains the types and client functions for the routes added by the engine, scripts, and pause plans. Rendering code stops reading `task.status` as a fixed enum: list filters, tones, and "active" logic use `task.category`, and the label shows `task.status`. One new component per concern keeps `TaskDetail.tsx` from growing.

**Tech Stack:** React, TypeScript, Vitest with Testing Library, Playwright (`npm run test:tasks-browser`), the existing UI components under `ui/src/components/ui`.

**Spec:** `docs/superpowers/specs/2026-10-02-workflow-images-design.md`, sections "Status and category", "Transition by the customer", "Artifacts", "Pause", "Operator override", "Runs", "Queue binding", and "Phases", item 6.

**Depends on:** the engine core, scripts, and goal-and-pause plans (all routes exist).

**How to read this plan:** each task gives the files, the exported interface, the behaviors to pin with tests, and the constraints. The implementer writes the tests first, watches them fail, then implements.

## Global Constraints

- A task with a workflow reports `status` (the workflow status ID), `category` (`open`, `in_progress`, `wait_customer`, `done`, `cancelled`), and optional `waiting_on` (`customer`, `script`, `pause`). A flexible task reports `category` equal to `status`.
- Never compare `task.status` with a category constant. Use `task.category` for tone, filters, sorting, "active" and "closed" logic, and drag rules.
- A workflow task has no editable status control and no Done button. Its status changes only through an outcome, an operator move, a cancel, or a pause decision.
- Every request carries the explicit host target the surrounding component already uses (`target?: ApiTarget`); never fall back to the local daemon.
- Secret values are write-only in the UI: an input to set one, a list of keys, never a value read back.
- Use the existing components in `ui/src/components/ui` and the styles in `ui/src/pages/tasks/panelStyles.tsx` and `tasks.css`. Do not add a UI library.
- Destructive actions (cancel a task, move without checks, release a holder, remove a secret, clear a binding) ask for confirmation.
- Run commands directly from the worktree root or `ui/`, without `bash -lc`. Run each `git` command as its own plain command.
- Never point UI tests at the live daemon or `127.0.0.1:9990`. The browser suite starts its own isolated daemon.
- Do not stage `desktop/dist` or `desktop/src-tauri/resources/bin/`. Do not rebuild `internal/storeui/dist` unless a shared component it embeds changed; if one did, say so in the report instead of rebuilding.
- Do not bump the version and do not edit `CHANGELOG.md`. Commit messages carry no attribution trailer.

## Review Focus

- A flexible task must render and behave exactly as before in the list, the drawer, drag and drop, and filters.
- A workflow status whose ID is long or unknown to the UI must render without breaking the row layout.
- An outcome button must be disabled while its request is in flight and must show a rejected or failed result with the script message.
- A paused task must make the pause reason and the three decisions impossible to miss in the drawer and visible in the list.
- A slow or failing workflow request must not blank the drawer: the task fields stay, the workflow panel shows the error and a retry.

---

### Task 1: Client types and functions

**Files:**
- Modify: `ui/src/lib/tasks.ts`
- Test: `ui/src/lib/tasks.test.ts`

**Interfaces:**
- Consumes: the REST routes of the earlier plans.
- Produces, all exported from `ui/src/lib/tasks.ts`:

```ts
export type TaskCategory = "open" | "in_progress" | "wait_customer" | "done" | "cancelled";
export type WaitingOn = "customer" | "script" | "pause";
// Task gains: category: TaskCategory; waiting_on?: WaitingOn; workflow_name?: string;
// workflow_version?: string; workflow_digest?: string; workflow_paused_reason?: string.

export interface WorkflowOutcome { on: string; to: string; requires?: string[]; missing?: string[]; checks?: string[] }
export interface WorkflowArtifact { id: number; name: string; value: string; author: string; created_at: string }
export interface WorkflowVisit { id: number; sequence: number; status: string; entered_at: string; entered_by: string; left_at?: string; outcome?: string; message?: string }
export interface TransitionRequest { id: number; task_key: string; outcome: string; message?: string; actor: string; state: "pending" | "applied" | "rejected" | "failed" | "cancelled"; result_message?: string; created_at: string; finished_at?: string }
export interface WorkflowView { name: string; version: string; digest: string; status: string; category: TaskCategory; waiting_on?: WaitingOn; owner: string; holder?: string; instructions_path?: string; outcomes: WorkflowOutcome[]; artifacts: WorkflowArtifact[]; visits: WorkflowVisit[]; last_request?: TransitionRequest }
export interface ScriptRun { id: number; task_key: string; kind: "check" | "watch"; script: string; run_as: "queue" | "agent"; state: string; verdict?: string; exit_code?: number; message?: string; created_at: string; started_at?: string; finished_at?: string; log_path?: string }
export interface QueueWorkflow { queue: string; name: string; version: string; digest: string; revision: number; updated_at: string }
export interface WorkflowImage { name: string; tag: string; version: string; digest: string; built_at: string }

export function isWorkflowTask(task: Task): boolean;
export function taskStatusLabel(task: Task): string;         // humanized status for either kind
export function taskCategoryLabel(category: TaskCategory): string;
export function isActiveCategory(category: TaskCategory): boolean; // open or in_progress

export function getTaskWorkflow(key: string, target?: ApiTarget): Promise<WorkflowView>;
export function advanceTask(key: string, outcome: string, message: string, target?: ApiTarget): Promise<TransitionRequest>;
export function getTransitionRequest(key: string, id: number, target?: ApiTarget): Promise<TransitionRequest>;
export function setTaskArtifact(key: string, name: string, value: string, target?: ApiTarget): Promise<WorkflowArtifact>;
export function getTaskArtifactHistory(key: string, name: string, target?: ApiTarget): Promise<WorkflowArtifact[]>;
export function listTaskScriptRuns(key: string, target?: ApiTarget): Promise<ScriptRun[]>;
export function getTaskScriptRunLog(key: string, id: number, target?: ApiTarget): Promise<string>;
export function moveTaskWorkflow(key: string, to: string, reason: string, target?: ApiTarget): Promise<Task>;
export function cancelWorkflowTask(key: string, target?: ApiTarget): Promise<Task>;
export function resumeTaskWorkflow(key: string, decision: "continue" | "release", target?: ApiTarget): Promise<Task>;
export function getQueueWorkflow(queue: string, target?: ApiTarget): Promise<QueueWorkflow | null>; // null when unbound
export function setQueueWorkflow(queue: string, ref: string, revision: number, target?: ApiTarget): Promise<QueueWorkflow>;
export function clearQueueWorkflow(queue: string, revision: number, target?: ApiTarget): Promise<void>;
export function listQueueSecretKeys(queue: string, target?: ApiTarget): Promise<string[]>;
export function setQueueSecret(queue: string, key: string, value: string, target?: ApiTarget): Promise<void>;
export function removeQueueSecret(queue: string, key: string, target?: ApiTarget): Promise<void>;
export function listWorkflowImages(target?: ApiTarget): Promise<WorkflowImage[]>;
```

- [ ] **Step 1: Write the failing tests:** each function's method, path, and body against a stubbed `fetch`; `getQueueWorkflow` maps the unbound error to `null`; `taskStatusLabel` for a flexible task, a workflow task, and an unknown long status ID; `isActiveCategory`.
- [ ] **Step 2: Run** `cd ui && npx vitest run src/lib/tasks.test.ts` and confirm the new tests fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `cd ui && npx vitest run src/lib/tasks.test.ts && npx tsc -b` and confirm both pass.
- [ ] **Step 5: Commit** with message `Add workflow task client functions`.

---

### Task 2: Category-based list, row, and filters

**Files:**
- Modify: `ui/src/pages/tasks/TaskRow.tsx`, `TaskTree.tsx`, `TaskFilterBar.tsx`, `TasksWorkspace.tsx`, `AllTasksWorkspace.tsx`, and any other reader of `task.status` under `ui/src` (find them with `rg -n "\.status\b" ui/src/pages/tasks ui/src/lib/tasks.ts ui/src/components`)
- Test: `TaskRow.test.tsx`, `TasksWorkspace.test.tsx`, `AllTasksWorkspace.test.tsx`

**Interfaces:**
- Consumes: Task 1.
- Produces: a row that shows `taskStatusLabel(task)` with the tone of `task.category`; a small `waiting_on` indicator ("customer", "script", "paused") for a waiting workflow task; a distinct paused marker; drag, sort, and "In progress" labelling driven by `category`.

Behaviors to pin: a flexible task row is unchanged (snapshot of its text and classes); a workflow task in `implement` / `in_progress` shows "Implement" with the in-progress tone; a script wait and a pause show their indicators; a 64-character status ID is truncated with the full ID in a title attribute; the status filter lists workflow statuses present in the loaded tasks next to the flexible ones.

- [ ] **Step 1: Write the failing tests.**
- [ ] **Step 2: Run** `cd ui && npx vitest run src/pages/tasks` and confirm the new tests fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `cd ui && npx vitest run src/pages/tasks && npx tsc -b && npm run lint` and confirm all pass.
- [ ] **Step 5: Commit** with message `Render task status and category separately`.

---

### Task 3: The workflow panel in the task drawer

**Files:**
- Create: `ui/src/pages/tasks/WorkflowPanel.tsx`, `WorkflowOutcomes.tsx`, `WorkflowArtifacts.tsx`, `WorkflowRuns.tsx`, `WorkflowPauseBanner.tsx`
- Modify: `ui/src/pages/tasks/TaskDetail.tsx`, `TaskDrawer.tsx`
- Test: `WorkflowPanel.test.tsx` and one test file per new component

**Interfaces:**
- Consumes: Tasks 1 and 2.
- Produces:

```tsx
export function WorkflowPanel(props: { task: Task; target?: ApiTarget; onTaskChanged: () => void }): JSX.Element;
```

`TaskDetail` renders `WorkflowPanel` for a workflow task in place of the status select and the Done action; everything else in the detail (title, description, priority, pull request, comments, relations) stays.

The panel shows, top to bottom:
1. the pause banner when `waiting_on` is `pause`: the reason in words, and buttons **Continue**, **Release holder** (only for a pool status), and **Cancel task**;
2. a header line: workflow `name@version`, the status, the owner, and the holder;
3. outcomes: for a customer status one button per outcome with an optional message field; for a pool or script status a read-only list with required artifacts, missing ones highlighted, and check scripts; the last request's result when rejected or failed, with the script message;
4. artifacts: name, author, time, value (collapsed when long), a history expander, and for the customer an edit action;
5. script runs: kind, script, state, verdict, exit code, times, and a log viewer loaded on demand;
6. the visit timeline;
7. an overflow menu for the customer: **Move to status…** (target select and a required reason) and **Cancel task**.

Behaviors to pin: each section's rendering from a fixture view; clicking an outcome calls `advanceTask`, disables the buttons while pending, polls `getTransitionRequest` until it leaves `pending`, then refreshes the view and calls `onTaskChanged`; a rejected result is shown with its message and the buttons re-enable; the three pause decisions call the right functions after confirmation; a failed view request shows an error and a retry without unmounting the task detail; the panel refetches when the task's revision changes (the realtime path already refetches the task).

- [ ] **Step 1: Write the failing tests.**
- [ ] **Step 2: Run** `cd ui && npx vitest run src/pages/tasks` and confirm the new tests fail.
- [ ] **Step 3: Implement.** Keep each new file under about 200 lines.
- [ ] **Step 4: Run** `cd ui && npx vitest run src/pages/tasks && npx tsc -b && npm run lint` and confirm all pass.
- [ ] **Step 5: Commit** with message `Show workflow outcomes, artifacts, runs, and pauses on a task`.

---

### Task 4: Queue workflow settings

**Files:**
- Create: `ui/src/pages/tasks/QueueWorkflowSettings.tsx`, `QueueSecrets.tsx`
- Modify: `ui/src/pages/tasks/QueueSettings.tsx`
- Test: one test file per new component, `QueueSettings` tests

**Interfaces:**
- Consumes: Task 1.
- Produces: in queue settings, a **Workflow** section: the current binding (name, version, short digest) or "No workflow"; a select of published workflow images (`name:tag`) with **Bind** and **Clear**; the pools the bound manifest needs with their members, editable through the existing pool route; a **Secrets** list of keys with **Set** (key and value inputs) and **Remove**. A bind error `workflow_pool_empty` or `workflow_secret_missing` lists the missing pools or secrets next to the control that fixes them.

Behaviors to pin: unbound and bound rendering; bind sends the current revision and surfaces a revision conflict by reloading; the two bind errors render their lists; secret values are never rendered after submission and the value input clears; remove and clear ask for confirmation.

- [ ] **Step 1: Write the failing tests.**
- [ ] **Step 2: Run** `cd ui && npx vitest run src/pages/tasks` and confirm the new tests fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `cd ui && npx vitest run src/pages/tasks && npx tsc -b && npm run lint` and confirm all pass.
- [ ] **Step 5: Commit** with message `Bind workflow images, pools, and secrets in queue settings`.

---

### Task 5: Browser test, documentation, and checks

**Files:**
- Modify: `ui/tests/tasks-e2e.pw.ts` and its fixture setup
- Modify: `docs/docs/tasks.mdx` (the Desktop section), `docs/docs/task-workflows.mdx`, `docs/docs/architecture/web-ui.mdx`

**Interfaces:**
- Consumes: Tasks 1–4.
- Produces: a browser scenario on the suite's isolated daemon: publish a small workflow image with one customer status, bind a queue, create a task, see its status in the list, approve it with the outcome button in the drawer, and see it closed; documentation of the list, the drawer panel, the pause banner, and queue settings.

- [ ] **Step 1: Add** the browser scenario; run `cd ui && npm run test:tasks-browser`.
- [ ] **Step 2: Update** the documentation.
- [ ] **Step 3: Run** `make check`; confirm every step reports success.
- [ ] **Step 4: Run** `git diff --check main...HEAD` and confirm no output.
- [ ] **Step 5: Commit** with message `Cover the workflow UI in the browser suite and document it`.
