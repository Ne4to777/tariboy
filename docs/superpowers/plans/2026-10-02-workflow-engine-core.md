# Workflow Engine Core Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the unused task workflow engine with the simpler one from the spec: a queue binds a workflow image, a task moves through statuses that each have one owner, and the daemon dispatches pool work, stores artifacts, and applies transitions.

**Architecture:** The engine stays in package `internal/tasks`, in rewritten `workflow_*.go` files, because it shares the task transaction, event, and notification code. It reads published manifests from the `task_workflow_images` table that the workflow image plan added. The existing `tasks.status` column keeps holding the category; the workflow status lives in `tasks.workflow_status`; the JSON boundary reports the workflow status as `status` and the category as `category`. Scripts (checks and watch scripts), queue secrets, pause, and the Goal block are later plans: in this plan a transition with checks is refused, and a script-owned status waits until an operator moves it.

**Tech Stack:** Go 1.26, SQLite migrations, `internal/tasks`, `internal/taskcli`, `internal/agentapi`, `internal/commands`, React/TypeScript for the removal of the old UI editor.

**Spec:** `docs/superpowers/specs/2026-10-02-workflow-images-design.md`, sections "Queue binding", "Runtime" (without "Pause" and without scripts), "Persistence", "Removed", "Error codes", and "Phases", item 3.

**Depends on:** `docs/superpowers/plans/2026-10-02-workflow-image.md` (packages `internal/workflowfile` and `internal/workflowimage`, table `task_workflow_images`).

**How to read this plan:** each task gives the files, the exact exported interface, the behaviors to pin with tests, and the constraints. The implementer writes the tests first, watches them fail, then implements. Names and signatures in "Produces" are binding; internal helpers are the implementer's choice.

## Global Constraints

- A task with a workflow has `tasks.workflow_digest` set. A task without one is a flexible task and behaves exactly as before this plan.
- Category values are the existing status constants: `open`, `in_progress`, `wait_customer`, `done`, `cancelled`. They stay in `tasks.status`.
- JSON for a task with a workflow: `status` is the workflow status, `category` is the category, `waiting_on` is `customer`, `script`, or `pause` while the category is `wait_customer` and empty otherwise. JSON for a flexible task: `category` equals `status`, `waiting_on` is empty.
- Category by state: pool status with a holder → `in_progress`, assignee is the holder; pool status without an eligible agent → `open`, assignee empty; customer status → `wait_customer` with an open customer wait, assignee unchanged; script status → `wait_customer` with no customer wait, assignee empty; terminal → `done`, or `cancelled` when the status declares `cancelled: true`.
- The daemon assigns work; there is no claim command for workflow tasks.
- Error codes are the ones in the spec's "Error codes" table, with HTTP status 409 for state conflicts, 403 for `not_holder`, 404 for unknown tasks, 400 for malformed input.
- `tasks.workflow_version_id` and the table `task_workflow_versions` cannot be dropped: the column carries a foreign key, and SQLite refuses to drop such a column without rebuilding `tasks`. Leave the column in place, always `NULL`, and leave the table in place, empty.
- Keep queue triggers (`task_queue_workflow_triggers`, the ingress cursor and sequence tables, and their routes and commands) working unchanged.
- Every workflow mutation, its task event, and any notification intent commit in one SQLite transaction, as existing task mutations do.
- Run commands directly from the worktree root, without `bash -lc`. Run each `git` command as its own plain command.
- Never run tests against the live `~/.tariboy`, `~/.tariboyd`, or `127.0.0.1:9990`.
- Do not bump the version and do not edit `CHANGELOG.md`. Commit messages carry no attribution trailer.

## Review Focus

- A flexible task in a queue with no workflow must behave byte-for-byte as before: same JSON fields plus `category`, same commands, same Goal selection.
- An agent that is not the holder, and any agent in a customer status, must not be able to advance, set artifacts, or move a task.
- Two concurrent `advance` calls for the same task must apply at most one transition.
- A task created in a bound queue while every pool member is busy must wait in `open` and be assigned when a member becomes free, without a daemon restart.
- Rebinding a queue to a new workflow version must not change the status graph of tasks already in flight.

## File Structure

| File | Responsibility |
| --- | --- |
| `internal/store/migrations/0054_drop_task_workflow_engine.sql` | Remove old engine tables; turn old managed tasks into flexible ones. |
| `internal/store/migrations/0055_task_workflow_images_engine.sql` | New engine tables and task columns. |
| `internal/tasks/workflow_triggers.go` | Queue triggers and bus ingress, moved out of `workflow_observations.go`. |
| `internal/tasks/workflow_model.go` | Engine types. |
| `internal/tasks/workflow_binding.go` | Queue binding and pools. |
| `internal/tasks/workflow_engine.go` | Entering a status, category, applying a transition. |
| `internal/tasks/workflow_dispatch.go` | Holder selection and the dispatch reconciler. |
| `internal/tasks/workflow_artifacts.go` | Artifacts. |
| `internal/tasks/workflow_requests.go` | `advance`, operator move, cancel. |
| `internal/tasks/workflow_view.go` | The read model for one task. |
| `internal/taskcli/*`, `internal/agentapi/agentapi.go`, `internal/commands/tasks.go` | Commands and routes. |

---

### Task 1: Remove the old engine

**Files:**
- Create: `internal/store/migrations/0054_drop_task_workflow_engine.sql`
- Create: `internal/tasks/workflow_triggers.go`
- Delete: `internal/tasks/workflow_definition.go`, `workflow_validator.go`, `workflow_packet.go`, `workflow_questions.go`, `workflow_reducer.go`, `workflow_runtime.go`, `workflow_artifacts.go`, `workflow_observations.go`, `workflow_model.go`, and their `_test.go` files
- Modify: `internal/tasks/workflow_binding.go` (keep only pools), `internal/tasks/service.go`, `internal/tasks/actions.go`, `internal/tasks/store.go`, `internal/tasks/tree.go`, `internal/tasks/transfer.go`
- Modify: `internal/agentapi/agentapi.go` (remove `workflowGated`, `workflowDirectChannelGated`, and `WorkflowPermissions`), `internal/loop/manager.go` (remove the `WorkflowPermissions` wiring)
- Modify: `internal/taskcli/parse.go`, `routes.go`, `help.go` (remove `work`, `questions`, `answer`, `observe`, `workflows`, the workflow form of `ask`, and `artifacts`)
- Modify: `internal/commands/tasks.go`, `tasks_openapi.go` (remove `/api/workflows` routes and the per-task workflow, packet, assignment, artifact, question, and subscription routes)
- Modify: `internal/compose/file.go`, `reconcile.go`, `cli.go` (remove `workflows:` and the `workflow:` key of `task_queues`; keep `pools`)
- Modify: `internal/daemon/daemon.go` (remove the workflow question reconciler; keep the ingress reconciler for triggers)
- Modify: UI: `ui/src/pages/tasks/QueueSettings.tsx`, `TaskDetail.tsx`, `TaskDrawer.tsx`, `TasksWorkspace.tsx`, `ui/src/lib/tasks.ts`, and their tests (remove the workflow editor and execution panels that call removed routes; keep pools if the UI shows them)
- Delete: `scripts/workflow-e2e.sh` and its `Makefile` target and `full-check` step (a new script arrives with the scripts plan)
- Modify: docs: replace the body of `docs/docs/task-workflows.mdx` with a short page stating that queue workflows are being replaced by workflow images, linking to `/docs/workflow-images`; remove packet, lease, and observation text from `docs/docs/tasks.mdx`, `docs/docs/architecture/index.mdx`, `iteration-loop.mdx`, `state-model.mdx`, `docs/docs/plugins/built-in/tasks.mdx`, `docs/docs/binaries/agent-tools.mdx`, `compose.mdx`, `docs/docs/reference/commands.md`, `docs/docs/development.mdx`

**Interfaces:**
- Consumes: nothing.
- Produces: a daemon with no workflow engine. `Task.WorkflowVersionID`, `WorkflowVersion`, `WorkflowStatus`, and `WorkflowRevision` are removed from the struct for now; Task 2 adds the new fields. Queue triggers keep their routes, commands, and behavior: `CreateQueueWorkflowTrigger`, `ListQueueWorkflowTriggers`, `DeleteQueueWorkflowTrigger`, `ApplyWorkflowObservation` reduced to trigger handling, `ReconcileWorkflowObservations`, and the ingress helpers live in `workflow_triggers.go`.

Migration `0054`:
- for every task with `workflow_version_id IS NOT NULL`: set `workflow_version_id`, `workflow_status`, and `workflow_revision` to `NULL`, leave `status` as it is, and insert one `task_events` row of kind `workflow.removed` with the old workflow status in the payload;
- drop, children first: `task_observations`, `task_workflow_subscriptions`, `task_workflow_holds`, `task_workflow_questions`, `task_artifacts`, `task_assignments`, `task_requirement_executions`, `task_status_executions`, `task_queue_workflows`, `task_workflow_outbox` if nothing else uses it;
- delete every row of `task_workflow_versions` and keep the table.

- [ ] **Step 1: Write the failing test** `TestWorkflowEngineRemovalMigration` in `internal/store/store_test.go`: seed, before `0054`, a queue, a published old workflow version, one managed task in `in_progress`, and one trigger; after `Open`, assert the task has `NULL` workflow columns and unchanged `status`, a `workflow.removed` event exists, the dropped tables are gone, `task_workflow_versions` is empty, and the trigger row survives.
- [ ] **Step 2: Run** `go test ./internal/store/ -run TestWorkflowEngineRemovalMigration -count=1` and confirm it fails.
- [ ] **Step 3: Write the migration.** If a kept table references a dropped one, report it instead of dropping the kept table.
- [ ] **Step 4: Remove the Go code.** Move trigger and ingress code to `workflow_triggers.go` first and get `go build ./...` green, then delete. Keep `workflowManagedError` for later tasks. Delete tests of removed behavior; keep and fix tests of triggers and pools.
- [ ] **Step 5: Remove the CLI, routes, compose keys, UI panels, e2e script, and documentation** listed above.
- [ ] **Step 6: Run** `make check` and confirm every step reports success.
- [ ] **Step 7: Commit** with message `Remove the unused task workflow engine`.

---

### Task 2: Schema and model of the new engine

**Files:**
- Create: `internal/store/migrations/0055_task_workflow_images_engine.sql`
- Create: `internal/tasks/workflow_model.go`
- Modify: `internal/tasks/model.go`, `internal/tasks/store.go` (task select and scan), `internal/taskgoal/store.go` (its own task select)
- Test: `internal/store/store_test.go`, `internal/tasks/workflow_model_test.go`

**Interfaces:**
- Consumes: table `task_workflow_images` from the workflow image plan.
- Produces:

```sql
ALTER TABLE tasks ADD COLUMN workflow_digest TEXT;
ALTER TABLE tasks ADD COLUMN workflow_paused_reason TEXT NOT NULL DEFAULT '';
-- tasks.workflow_status (existing, nullable) holds the current status ID.
CREATE INDEX idx_tasks_workflow_digest ON tasks(workflow_digest, workflow_status);

CREATE TABLE task_queue_workflows (
    queue_prefix     TEXT PRIMARY KEY REFERENCES task_queues(prefix) ON DELETE CASCADE,
    workflow_digest  TEXT NOT NULL REFERENCES task_workflow_images(digest) ON DELETE RESTRICT,
    revision         INTEGER NOT NULL DEFAULT 1,
    updated_at       TEXT NOT NULL
);
CREATE TABLE task_workflow_holders (
    task_id       INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    pool          TEXT NOT NULL,
    agent         TEXT NOT NULL,
    dispatched_at TEXT NOT NULL,
    PRIMARY KEY (task_id, pool)
);
CREATE TABLE task_status_visits (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id           INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    sequence          INTEGER NOT NULL,
    status_id         TEXT NOT NULL,
    entered_at        TEXT NOT NULL,
    entered_by        TEXT NOT NULL,
    left_at           TEXT NOT NULL DEFAULT '',
    outcome           TEXT NOT NULL DEFAULT '',
    message           TEXT NOT NULL DEFAULT '',
    idle_iterations   INTEGER NOT NULL DEFAULT 0,
    rejected_requests INTEGER NOT NULL DEFAULT 0,
    script_failures   INTEGER NOT NULL DEFAULT 0,
    UNIQUE (task_id, sequence)
);
CREATE TABLE task_transition_requests (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id        INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    visit_id       INTEGER NOT NULL REFERENCES task_status_visits(id) ON DELETE CASCADE,
    outcome        TEXT NOT NULL,
    message        TEXT NOT NULL DEFAULT '',
    actor          TEXT NOT NULL,
    state          TEXT NOT NULL CHECK(state IN ('pending','applied','rejected','failed','cancelled')),
    result_message TEXT NOT NULL DEFAULT '',
    created_at     TEXT NOT NULL,
    finished_at    TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX idx_task_transition_requests_one_pending
    ON task_transition_requests(task_id) WHERE state = 'pending';
CREATE TABLE task_artifacts (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id    INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    value      TEXT NOT NULL,
    author     TEXT NOT NULL,
    created_at TEXT NOT NULL
);
CREATE INDEX idx_task_artifacts_current ON task_artifacts(task_id, name, id DESC);
```

```go
// model.go: fields added to Task
WorkflowDigest       string `json:"workflow_digest,omitempty"`
WorkflowName         string `json:"workflow_name,omitempty"`
WorkflowVersion      string `json:"workflow_version,omitempty"`
WorkflowStatus       string `json:"-"`
WorkflowPausedReason string `json:"workflow_paused_reason,omitempty"`
Category             string `json:"category"`
WaitingOn            string `json:"waiting_on,omitempty"`

// Task.MarshalJSON reports the workflow status as "status" and the stored
// status as "category" for a task with a workflow; for a flexible task both
// are the stored status.
func (t Task) MarshalJSON() ([]byte, error)

const (
	WaitingOnCustomer = "customer"
	WaitingOnScript   = "script"
	WaitingOnPause    = "pause"
)

// workflow_model.go
type StatusVisit struct {
	ID        int64  `json:"id"`
	Sequence  int64  `json:"sequence"`
	Status    string `json:"status"`
	EnteredAt string `json:"entered_at"`
	EnteredBy string `json:"entered_by"`
	LeftAt    string `json:"left_at,omitempty"`
	Outcome   string `json:"outcome,omitempty"`
	Message   string `json:"message,omitempty"`
}
type TransitionRequest struct {
	ID            int64  `json:"id"`
	TaskKey       string `json:"task_key"`
	Outcome       string `json:"outcome"`
	Message       string `json:"message,omitempty"`
	Actor         string `json:"actor"`
	State         string `json:"state"`
	ResultMessage string `json:"result_message,omitempty"`
	CreatedAt     string `json:"created_at"`
	FinishedAt    string `json:"finished_at,omitempty"`
}
type Artifact struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Value     string `json:"value"`
	Author    string `json:"author"`
	CreatedAt string `json:"created_at"`
}
type QueueWorkflow struct {
	Queue     string `json:"queue"`
	Name      string `json:"name"`
	Version   string `json:"version"`
	Digest    string `json:"digest"`
	Revision  int64  `json:"revision"`
	UpdatedAt string `json:"updated_at"`
}

// loadManifestTx reads a published manifest by digest from task_workflow_images.
func loadManifestTx(ctx context.Context, q queryer, digest string) (workflowimage.Manifest, error)
```

- [ ] **Step 1: Write the failing tests:** the migration creates the tables, columns, and the one-pending unique index; `Task.MarshalJSON` output for a flexible task (`status` and `category` equal, no `waiting_on`) and for a workflow task in each category, including `waiting_on` for a customer status, a script status, and a paused task; the task scan fills `WorkflowName` and `WorkflowVersion` from the joined manifest row; `taskgoal` still selects by the stored status.
- [ ] **Step 2: Run** `go test ./internal/store/ ./internal/tasks/ ./internal/taskgoal/ -count=1` and confirm the new tests fail.
- [ ] **Step 3: Implement.** `WaitingOn` is derived at scan time: `pause` when `workflow_paused_reason` is not empty, else from the owner of the current status in the manifest. Keep `Task.Status` holding the stored category everywhere in Go code.
- [ ] **Step 4: Run** the same command plus `go build ./...`; confirm both pass.
- [ ] **Step 5: Commit** with message `Add the workflow image engine schema and task fields`.

---

### Task 3: Queue binding and pools

**Files:**
- Modify: `internal/tasks/workflow_binding.go`
- Modify: `internal/tasks/service.go` (a resolver hook), `internal/daemon/daemon.go` (wiring)
- Test: `internal/tasks/workflow_binding_test.go`

**Interfaces:**
- Consumes: `workflowimage.Manifest`, `(*workflowfile.File).Pools()`, `loadManifestTx`.
- Produces:

```go
// WorkflowResolver turns "name:tag" or a digest into a published digest.
type WorkflowResolver func(ref string) (digest string, err error)

func (s *Service) SetWorkflowResolver(resolve WorkflowResolver)

// SetQueueWorkflow binds a queue. revision is the current binding revision,
// 0 for a first binding. Customer only.
func (s *Service) SetQueueWorkflow(ctx context.Context, actor Actor, queue, ref string, revision int64) (QueueWorkflow, error)
func (s *Service) ClearQueueWorkflow(ctx context.Context, actor Actor, queue string, revision int64) error
func (s *Service) GetQueueWorkflow(ctx context.Context, actor Actor, queue string) (QueueWorkflow, error)
```

`RebindAgentPool`, `GetAgentPool`, and `ListAgentPools` keep their signatures.

Behaviors to pin:
- binding fails with `workflow_pool_empty` (409) naming every pool of the manifest that is missing or empty in the queue;
- binding with a stale revision fails with `revision_conflict`; binding the digest already bound is a no-op that keeps the revision;
- an unknown ref fails with `not_found`;
- `RebindAgentPool` to an empty list is rejected while the bound manifest uses that pool, and allowed otherwise;
- `ClearQueueWorkflow` leaves tasks in flight on their pinned digest;
- an agent actor is `forbidden` for all three.

- [ ] **Step 1: Write the failing tests** for the behaviors above. Seed manifests by inserting into `task_workflow_images` through a test helper that builds a `workflowimage.Manifest` from a small in-memory `workflowfile.File`.
- [ ] **Step 2: Run** `go test ./internal/tasks/ -run "QueueWorkflow|AgentPool" -count=1` and confirm failure.
- [ ] **Step 3: Implement.** Wire the resolver in the daemon to the workflow image store's `Resolve`.
- [ ] **Step 4: Run** `go test ./internal/tasks/ -count=1` and confirm it passes.
- [ ] **Step 5: Commit** with message `Bind queues to workflow images`.

---

### Task 4: Entering a status, pinning, and dispatch

**Files:**
- Create: `internal/tasks/workflow_engine.go`, `internal/tasks/workflow_dispatch.go`
- Modify: `internal/tasks/service.go` (`CreateTask`), `internal/tasks/workflow.go` (`UpdateTask`, `CompleteTask`, `ClaimTask` guards), `internal/tasks/tree.go` (ready filter), `internal/tasks/transfer.go`, `internal/daemon/daemon.go` (reconciler)
- Test: `internal/tasks/workflow_engine_test.go`, `internal/tasks/workflow_dispatch_test.go`

**Interfaces:**
- Consumes: Tasks 2 and 3.
- Produces:

```go
// enterStatusTx closes the current visit, opens a visit for statusID, sets
// tasks.workflow_status, derives the category and assignee, and records the
// workflow.transitioned event and notification intents. via is the actor.
func (s *Service) enterStatusTx(ctx context.Context, tx *sql.Tx, task *Task, manifest workflowimage.Manifest, statusID, via, outcome, message string) error

// DispatchPending assigns holders to workflow tasks waiting in a pool status
// without one. It returns how many tasks it assigned.
func (s *Service) DispatchPending(ctx context.Context) (int, error)
```

Behaviors to pin:
- a task created in a bound queue pins the queue's digest and enters `initial_status` in the creating transaction; a task in an unbound queue is flexible;
- entering a pool status assigns the previous holder of that pool when one is recorded and is still a pool member, even if that agent has another Goal;
- otherwise it assigns an eligible member: agent enabled, loop enabled, Goal enabled, not halted, and with an empty `current_goal_task_key`; ties go to the member with the oldest `dispatched_at` across the queue's holders, then to pool order;
- with no eligible member the task is `open` and unassigned, and `DispatchPending` assigns it once a member becomes eligible;
- entering a customer status sets `wait_customer` and opens a customer wait through the existing wait mechanism, authored by `system:workflow`, with a comment that names the status and its outcomes;
- entering a script status sets `wait_customer`, clears the assignee, and opens no wait;
- entering a terminal status sets `done` or `cancelled` and `completed_at`;
- each entry writes one `task_status_visits` row and one `workflow.transitioned` event carrying `from`, `to`, `outcome`, and `actor`;
- `UpdateTask` with a status or assignee, `CompleteTask`, and `ClaimTask` on a workflow task return `workflow_managed` whose `Data` lists the available outcomes; title, description, priority, and pull request updates still work;
- the ready list excludes workflow tasks; a transferred task cannot enter a bound queue;
- after an assignment the goal signal fires.

- [ ] **Step 1: Write the failing tests** for the behaviors above. Use real agent rows; read `internal/agent/agent.go` for the columns that mean enabled, loop enabled, Goal enabled, and halted.
- [ ] **Step 2: Run** `go test ./internal/tasks/ -count=1` and confirm the new tests fail.
- [ ] **Step 3: Implement.** Start a `DispatchPending` loop in the daemon next to the goal reconciler: on the task service's change signal and every minute.
- [ ] **Step 4: Run** `go test ./internal/tasks/ ./internal/daemon/ ./internal/taskgoal/ -count=1` and confirm it passes.
- [ ] **Step 5: Commit** with message `Run workflow tasks through statuses and dispatch pool work`.

---

### Task 5: Artifacts

**Files:**
- Create: `internal/tasks/workflow_artifacts.go`
- Test: `internal/tasks/workflow_artifacts_test.go`

**Interfaces:**
- Consumes: Task 4.
- Produces:

```go
const maxArtifactBytes = 64 << 10

func (s *Service) SetArtifact(ctx context.Context, actor Actor, key, name, value string) (Artifact, error)
func (s *Service) ListArtifacts(ctx context.Context, actor Actor, key string) ([]Artifact, error) // current value per name, sorted by name
func (s *Service) GetArtifact(ctx context.Context, actor Actor, key, name string) (Artifact, []Artifact, error) // current, then history newest first

// setArtifactTx is the transactional core, also used by script results later.
func setArtifactTx(ctx context.Context, tx *sql.Tx, task Task, manifest workflowimage.Manifest, name, value, author, now string) (Artifact, error)
```

Behaviors to pin: only declared names (`artifact_unknown`); valid UTF-8 up to 64 KiB (`artifact_too_large`, `invalid_artifact`); only the current holder in a pool status, or the customer, may set (`not_holder`); a flexible task returns `workflow_not_bound`; a second value becomes current and the first stays in history; each write records an `artifact.set` event without the value; readers need ordinary task read access.

- [ ] **Step 1: Write the failing tests.**
- [ ] **Step 2: Run** `go test ./internal/tasks/ -run Artifact -count=1` and confirm failure.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test ./internal/tasks/ -count=1` and confirm it passes.
- [ ] **Step 5: Commit** with message `Store workflow artifacts on tasks`.

---

### Task 6: Advance, move, cancel, and the task view

**Files:**
- Create: `internal/tasks/workflow_requests.go`, `internal/tasks/workflow_view.go`
- Test: `internal/tasks/workflow_requests_test.go`, `internal/tasks/workflow_view_test.go`

**Interfaces:**
- Consumes: Tasks 4 and 5.
- Produces:

```go
type AdvanceInput struct {
	Outcome string `json:"outcome"`
	Message string `json:"message"`
}

// Advance declares an outcome for the current status. A pool status accepts
// it from the holder; a customer status from the customer. A transition
// without checks applies in this call. A transition with checks is refused
// with checks_unavailable until the scripts plan lands.
func (s *Service) Advance(ctx context.Context, actor Actor, key string, in AdvanceInput) (TransitionRequest, error)

// MoveWorkflow moves a task to any status without checks. Customer only.
func (s *Service) MoveWorkflow(ctx context.Context, actor Actor, key, to, reason string) (Task, error)

// CancelWorkflowTask closes a workflow task as cancelled. Customer only.
func (s *Service) CancelWorkflowTask(ctx context.Context, actor Actor, key string) (Task, error)

type OutcomeView struct {
	On       string   `json:"on"`
	To       string   `json:"to"`
	Requires []string `json:"requires,omitempty"`
	Missing  []string `json:"missing,omitempty"` // required artifacts with no value
	Checks   []string `json:"checks,omitempty"`  // script paths
}
type WorkflowView struct {
	Name             string             `json:"name"`
	Version          string             `json:"version"`
	Digest           string             `json:"digest"`
	Status           string             `json:"status"`
	Category         string             `json:"category"`
	WaitingOn        string             `json:"waiting_on,omitempty"`
	Owner            string             `json:"owner"`            // "pool:<name>", "customer", "script", or "" for terminal
	Holder           string             `json:"holder,omitempty"` // "agent:<name>"
	InstructionsPath string             `json:"instructions_path,omitempty"` // source-relative path inside the image
	Outcomes         []OutcomeView      `json:"outcomes"`
	Artifacts        []Artifact         `json:"artifacts"`
	Visits           []StatusVisit      `json:"visits"`
	LastRequest      *TransitionRequest `json:"last_request,omitempty"`
}
func (s *Service) GetWorkflow(ctx context.Context, actor Actor, key string) (WorkflowView, error)
```

Behaviors to pin:
- `outcome_unknown` for an outcome the status does not declare; `not_holder` for a non-holder agent, for any agent in a customer status, and for anyone in a script status; `artifact_missing` with `Data["missing"]` when a required artifact has no value;
- a successful advance records an `applied` request, leaves the visit with the outcome and message, enters the target status, and resets nothing outside the new visit;
- the customer's advance in a customer status resolves the open customer wait;
- two advances racing on one task: exactly one applies, the other gets `revision_conflict` or `transition_pending`;
- `MoveWorkflow` requires a non-empty reason, records `workflow.moved` with actor and reason, and works out of any status including terminal ones;
- `CancelWorkflowTask` sets category `cancelled` and `completed_at`, leaves `workflow_status`, cancels a pending request, and records an event;
- `GetWorkflow` on a flexible task returns `workflow_not_bound`; on a workflow task it lists outcomes with `Missing` computed from current artifacts.

- [ ] **Step 1: Write the failing tests.**
- [ ] **Step 2: Run** `go test ./internal/tasks/ -run "Advance|MoveWorkflow|CancelWorkflow|GetWorkflow" -count=1` and confirm failure.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test ./internal/tasks/ -count=1` and confirm it passes.
- [ ] **Step 5: Commit** with message `Advance, move, and cancel workflow tasks`.

---

### Task 7: Commands and routes

**Files:**
- Modify: `internal/tasks/actions.go` (agent actions), `internal/taskcli/parse.go`, `routes.go`, `help.go`, `internal/commands/tasks.go`, `tasks_openapi.go`
- Test: `internal/taskcli/*_test.go`, `internal/commands/tasks*_test.go`, `internal/tasks/actions_test.go`
- Modify: `scripts/tariboy-tasks-e2e.sh` only if it lists commands that changed

**Interfaces:**
- Consumes: Tasks 3–6.
- Produces:

| Command | Mode | Route |
| --- | --- | --- |
| `ttasks advance KEY --outcome NAME [--message TEXT]` | agent and customer | `POST /api/tasks/{key}/advance` |
| `ttasks artifacts set KEY NAME [VALUE]` (value from stdin when absent) | agent and customer | `PUT /api/tasks/{key}/artifacts/{name}` |
| `ttasks artifacts ls KEY` | agent and customer | `GET /api/tasks/{key}/artifacts` |
| `ttasks artifacts show KEY NAME` | agent and customer | `GET /api/tasks/{key}/artifacts/{name}` |
| `ttasks workflow get KEY` | agent and customer | `GET /api/tasks/{key}/workflow` |
| `ttasks workflow move KEY --to STATUS --reason TEXT` | customer | `POST /api/tasks/{key}/workflow/move` |
| `ttasks cancel KEY` | customer | `POST /api/tasks/{key}/cancel` |
| `ttasks queue workflow set QUEUE REF [--revision N]` | customer | `PUT /api/task-queues/{queue}/workflow` |
| `ttasks queue workflow get QUEUE` | customer | `GET /api/task-queues/{queue}/workflow` |
| `ttasks queue workflow clear QUEUE --revision N` | customer | `DELETE /api/task-queues/{queue}/workflow` |

Agent mode goes through the identity-bound tools socket and `Service.AgentAction` with actions `advance`, `artifact_set`, `artifact_ls`, `artifact_show`, and `workflow_get`. `ttasks done`, `ttasks update --status`, and `ttasks ready --claim` on a workflow task print the `workflow_managed` message with the available outcomes and the `advance` command. `ttasks show` prints `status`, `category`, and `waiting_on`.

- [ ] **Step 1: Write the failing tests:** parser and help tests for every command above, including `--help-json`; route tests for each REST route and its error mapping; agent action tests proving the daemon ignores a caller-supplied identity.
- [ ] **Step 2: Run** `go test ./internal/taskcli/ ./internal/commands/ ./internal/tasks/ -count=1` and confirm the new tests fail.
- [ ] **Step 3: Implement.** Follow the existing patterns in `parse.go` and `routes.go`; keep the OpenAPI registry in sync.
- [ ] **Step 4: Run** `make backend-check` and confirm every step reports success.
- [ ] **Step 5: Commit** with message `Add workflow task commands and routes`.

---

### Task 8: Documentation and checks

**Files:**
- Modify: `docs/docs/task-workflows.mdx`, `docs/docs/tasks.mdx`, `docs/docs/workflow-images.mdx`, `docs/docs/architecture/state-model.mdx`, `docs/docs/plugins/built-in/tasks.mdx`, `docs/docs/binaries/agent-tools.mdx`, `docs/docs/reference/commands.md`

**Interfaces:**
- Consumes: Tasks 1–7.
- Produces: documentation of queue binding, pinning, status and category, dispatch, artifacts, `advance`, operator move and cancel, the new tables, and the error codes. State plainly that checks and watch scripts are not executed yet: a transition with checks is refused and a script status waits for an operator move.

- [ ] **Step 1: Write** the documentation.
- [ ] **Step 2: Run** `make check` and confirm every step reports success.
- [ ] **Step 3: Run** `git diff --check main...HEAD` and confirm no output.
- [ ] **Step 4: Commit** with message `Document the workflow engine core`.
