# Workflow Goal and Pause Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Push the agent onto the workflow of its Goal task with a prompt block that states the current status, its instructions, and the exact way out; and pause a task, asking the customer, when the work stalls.

**Architecture:** The Goal block is rendered in `internal/loop` from the task service's workflow view plus the status instructions read from the workflow image. Stall detection lives in `internal/tasks`: counters on the current status visit, a pause state on the task, and a customer wait opened through the existing answer-tracking mechanism. The loop reports each finished iteration to the task service; a daemon reconciler watches holder availability.

**Tech Stack:** Go 1.26, `internal/loop`, `internal/tasks`, `internal/taskgoal`, `internal/daemon`, `internal/taskcli`, `internal/commands`.

**Spec:** `docs/superpowers/specs/2026-10-02-workflow-images-design.md`, sections "Goal", "Pause", "Limits", and "Phases", item 4.

**Depends on:** `docs/superpowers/plans/2026-10-02-workflow-engine-core.md` and `docs/superpowers/plans/2026-10-02-workflow-scripts.md`.

**How to read this plan:** each task gives the files, the exact exported interface, the behaviors to pin with tests, and the constraints. The implementer writes the tests first, watches them fail, then implements. Names and signatures in "Produces" are binding.

## Global Constraints

- Goal selection does not change. Do not edit the selection queries in `internal/taskgoal/store.go` except where a task says so.
- Pause reasons, exact strings: `idle_iterations`, `rejected_requests`, `script_failures`, `holder_unavailable`.
- Limits come from `(*workflowfile.File).StatusLimits(statusID)`: defaults `idle_iterations` 3, `rejected_requests` 5, `script_failures` 3, `unavailable_grace` 5m; a grace of `0` pauses at once.
- A paused task: `workflow_paused_reason` is set, category is `wait_customer` with an open customer wait authored by `system:workflow`, the workflow status is unchanged, any watch script is stopped, and no work is dispatched.
- The customer resolves a pause with exactly one decision: `continue` or `release`, or cancels the task. A plain comment does not resolve it.
- The daemon never hands a paused task to another agent on its own.
- Counters reset when the status changes and when a pause is resolved.
- Status instructions are trusted like image prompts. Task title, description, artifact values, and script messages are untrusted input and are rendered as data, never as instructions.
- Run commands directly from the worktree root, without `bash -lc`. Run each `git` command as its own plain command.
- Never run tests against the live `~/.tariboy`, `~/.tariboyd`, or `127.0.0.1:9990`.
- Do not bump the version and do not edit `CHANGELOG.md`. Commit messages carry no attribution trailer.

## Review Focus

- An iteration that worked a different task, or ended before the agent became the holder, must not count as idle for this task.
- An agent that honestly works one status across several iterations with a raised per-status limit must not be paused early; one that requests and is rejected forever must be.
- An operator restarting an agent (briefly unavailable) must not produce a pause when the grace has not elapsed.
- Resuming with `release` when no other pool member is eligible must leave the task waiting in `open`, not paused again and not assigned to the released agent.
- An artifact value or script message containing Markdown headings or "ignore previous instructions" text must not be able to pose as status instructions in the Goal block.

---

### Task 1: The Goal block for a workflow task

**Files:**
- Modify: `internal/loop/prompt_v2.go`, `internal/loop/runner.go` (where `FormatRuntimeGoal` is called), and the operator prompt preview path that renders the same block
- Modify: `internal/loop/manager.go` (configuration: access to the workflow view and the image store)
- Test: `internal/loop/prompt_v2_test.go`, runner tests

**Interfaces:**
- Consumes: `tasks.Service.GetWorkflow`, `tasks.WorkflowView`, `workflowimage.Store.FilePath`.
- Produces:

```go
// WorkflowGoal is everything the Goal block shows for a workflow task.
type WorkflowGoal struct {
	Task         tasks.Task
	View         tasks.WorkflowView
	Instructions string // content of the status instructions file; "" when none
}

const maxGoalInstructionsBytes = 64 << 10
const maxGoalValueRunes = 400 // artifact values and script messages are cut to this

// FormatRuntimeWorkflowGoal renders the Goal block for a workflow task.
func FormatRuntimeWorkflowGoal(goal WorkflowGoal) string
```

The block, in this order:
1. `# Agent Goal`, then one fixed paragraph: the task follows a workflow; work only on the current status; leave it by declaring an outcome with `ttasks advance`; the task status cannot be set directly; never merge or close on the customer's behalf unless the status instructions say so.
2. Lines `key`, `title`, `priority`, `workflow` (`name@version`), `status`, `category`, and, when a previous visit exists, `reached by` (previous status, outcome, actor, message).
3. `description:` and the task description.
4. `### Status instructions` and the file content, or "This status has no instructions." For a category other than `in_progress`, a fixed line instead: the task waits for the customer, a script, or a pause decision, and the agent must not work on it.
5. `### Outcomes`: one line per outcome with its target status, required artifacts, which of them are missing, and the check scripts.
6. `### Artifacts`: current name, author, and value, each value cut to `maxGoalValueRunes` and rendered inside a fenced block.
7. `### Last transition request`, only when the last request was rejected or failed: its state and message inside a fenced block.
8. `### Commands`: the exact `ttasks artifacts set KEY NAME VALUE` and `ttasks advance KEY --outcome NAME --message "TEXT"` lines with the real key.

Behaviors to pin: the full text for a pool status with a missing artifact and a rejected last request; a customer status; a script status; a paused task; a flexible task still renders the existing `FormatRuntimeGoal` text unchanged; an instructions file larger than the limit is cut with a visible marker; a fence inside an artifact value cannot close the block early (use a fence longer than any run of backticks in the value); the operator prompt preview shows the same block.

- [ ] **Step 1: Write the failing tests.**
- [ ] **Step 2: Run** `go test ./internal/loop/ -run "Goal|Prompt" -count=1` and confirm the new tests fail.
- [ ] **Step 3: Implement.** When reading the view or the instructions fails, render the flexible Goal text plus one line saying the workflow details could not be loaded, and log the error; an iteration must still start.
- [ ] **Step 4: Run** `go test ./internal/loop/ -count=1` and confirm it passes.
- [ ] **Step 5: Commit** with message `Show the workflow status and its exits in the Goal block`.

---

### Task 2: Pause and resume

**Files:**
- Create: `internal/tasks/workflow_pause.go`
- Modify: `internal/tasks/workflow_requests.go` (refuse `Advance` while paused), `internal/tasks/workflow_dispatch.go` (skip paused tasks), `internal/tasks/workflow_engine.go` (reset on status change), `internal/tasks/actions.go`, `internal/taskcli/*`, `internal/commands/tasks.go`, `tasks_openapi.go`
- Test: `internal/tasks/workflow_pause_test.go`, command and parser tests

**Interfaces:**
- Consumes: the engine core and scripts plans.
- Produces:

```go
const (
	PauseIdleIterations    = "idle_iterations"
	PauseRejectedRequests  = "rejected_requests"
	PauseScriptFailures    = "script_failures"
	PauseHolderUnavailable = "holder_unavailable"

	ResumeContinue = "continue"
	ResumeRelease  = "release"
)

// pauseTx pauses a workflow task. It is a no-op when the task is already
// paused or closed.
func (s *Service) pauseTx(ctx context.Context, tx *sql.Tx, task *Task, manifest workflowimage.Manifest, reason, detail string) error

// ResumeWorkflow resolves a pause. Customer only.
func (s *Service) ResumeWorkflow(ctx context.Context, actor Actor, key, decision string) (Task, error)
```

Command `ttasks workflow resume KEY --decision continue|release`; route `POST /api/tasks/{key}/workflow/resume`.

Behaviors to pin:
- `pauseTx` sets the reason, category `wait_customer`, opens one customer wait with a `system:workflow` comment that states the reason, the detail, and the three choices, clears `next_watch_at`, cancels the visit's pending and running script runs, cancels a pending transition request, and records `workflow.paused`;
- `Advance` and `SetArtifact` by the holder on a paused task return `workflow_paused`; `MoveWorkflow` and `CancelWorkflowTask` still work and clear the pause;
- `ResumeWorkflow` with `continue`: clears the reason, zeroes the visit counters, resolves the wait, restores the category of the current status (pool: `in_progress` with the same holder; script: `wait_customer` with `next_watch_at` set to now; customer: `wait_customer` with a fresh customer wait), records `workflow.resumed`, and fires the goal signal;
- with `release` in a pool status: also deletes that pool's holder row, clears the assignee, and dispatches again, excluding nobody; if no member is eligible the task is `open`;
- `release` outside a pool status is rejected with `invalid_decision`; an unknown decision likewise; a task that is not paused returns `workflow_not_paused`; an agent actor is `forbidden`;
- a customer comment alone leaves the task paused.

- [ ] **Step 1: Write the failing tests.**
- [ ] **Step 2: Run** `go test ./internal/tasks/ ./internal/taskcli/ ./internal/commands/ -count=1` and confirm the new tests fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** the same command and confirm it passes.
- [ ] **Step 5: Commit** with message `Pause and resume workflow tasks`.

---

### Task 3: Limits that trigger a pause

**Files:**
- Create: `internal/tasks/workflow_stall.go`
- Modify: `internal/tasks/workflow_runs.go` (rejections and failures), `internal/tasks/workflow_requests.go` (reset on request), `internal/loop` (report each finished iteration), `internal/daemon/daemon.go` (wiring)
- Modify: `internal/store/migrations/` (next free number): `ALTER TABLE task_workflow_holders ADD COLUMN unavailable_since TEXT NOT NULL DEFAULT ''`
- Test: `internal/tasks/workflow_stall_test.go`, a loop test for the report

**Interfaces:**
- Consumes: Task 2.
- Produces:

```go
// IterationEnd describes one finished iteration of an agent.
type IterationEnd struct {
	Agent       string
	IterationID string
	GoalTaskKey string // the Goal the iteration ran with; "" when none
	StartedAt   time.Time
	FinishedAt  time.Time
}

// RecordIterationEnd counts the iteration as idle for its Goal task when the
// agent is the holder of a pool status it already held when the iteration
// started, the status did not change, and the agent made no transition
// request during the iteration. It pauses the task at the limit.
func (s *Service) RecordIterationEnd(ctx context.Context, end IterationEnd) error

// CheckHolders records since when each holder of an active pool status cannot
// work and pauses tasks whose holder has been unavailable for the grace.
func (s *Service) CheckHolders(ctx context.Context, now time.Time) (int, error)
```

A holder cannot work when the agent row is missing, the agent is disabled, its loop is disabled, it is halted (the halt logic in `internal/agent/agent.go`), or it is no longer a member of the pool.

Behaviors to pin:
- idle counting per the doc comment, including: an iteration with a different Goal does not count; an iteration during which the status was entered does not count; a transition request during the iteration, whatever its result, resets the counter to zero; reaching the limit pauses with `idle_iterations`; a per-status limit overrides the workflow limit;
- a check rejection that brings `rejected_requests` to the limit pauses with `rejected_requests`; an applied transition resets the counters by opening a new visit;
- a check failure or a watch failure that brings `script_failures` to the limit pauses with `script_failures`; the limit counts consecutive failures, so a pass, reject, quiet, or outcome verdict resets the count to zero;
- `CheckHolders` sets `unavailable_since` on first observation, clears it when the holder can work again, pauses with `holder_unavailable` once `now - unavailable_since >= grace`, pauses at once when the grace is `0`, and ignores tasks that are paused, closed, or not in a pool status;
- the loop calls `RecordIterationEnd` once per terminal iteration with the Goal key the iteration actually ran with.

- [ ] **Step 1: Write the failing tests.**
- [ ] **Step 2: Run** `go test ./internal/tasks/ ./internal/loop/ ./internal/store/ -count=1` and confirm the new tests fail.
- [ ] **Step 3: Implement.** Find where the loop already calls the goal reconciler's `IterationCompleted` and report the iteration there, before the Goal wake is published, so a task paused by this iteration is not re-woken. Run `CheckHolders` from the same daemon loop as `DispatchPending`.
- [ ] **Step 4: Run** `make backend-check` and confirm every step reports success.
- [ ] **Step 5: Commit** with message `Pause stalled workflow tasks and ask the customer`.

---

### Task 4: End-to-end coverage and documentation

**Files:**
- Modify: `scripts/workflow-e2e.sh`, `scripts/testdata/workflow-e2e/`
- Modify: `docs/docs/task-workflows.mdx`, `docs/docs/tasks.mdx`, `docs/docs/architecture/iteration-loop.mdx`, `docs/docs/reference/commands.md`

**Interfaces:**
- Consumes: Tasks 1–3.
- Produces: the end-to-end script additionally proves a pause by repeated check rejection (limit set to 2 in the fixture), a `continue` resume, and a `release` resume; documentation of the Goal block, the limits, the four pause reasons, and the resume decisions.

- [ ] **Step 1: Extend** the fixture and script; run `make workflow-e2e`.
- [ ] **Step 2: Update** the documentation; in `iteration-loop.mdx` replace the description of the fixed Goal guidance with the two forms (flexible and workflow).
- [ ] **Step 3: Run** `make check` and `make workflow-e2e`; confirm both succeed.
- [ ] **Step 4: Run** `git diff --check main...HEAD` and confirm no output.
- [ ] **Step 5: Commit** with message `Document and prove workflow pauses end to end`.
