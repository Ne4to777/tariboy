# Workflow Scripts Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the daemon run the scripts of a workflow image: checks that gate a transition an agent requests, and watch scripts that own a status. Add queue secrets and the two run modes, Compose binding, and an end-to-end script.

**Architecture:** `internal/tasks` owns durable run records and decides what a finished run means: it advances a transition request, applies a watch outcome, or records a failure. A new package `internal/workflowrun` knows the script protocol and nothing about tasks: it prepares the files and environment of one run, executes the process, and classifies the result. A daemon worker connects the two, the way the agent script supervisor in `internal/loop/manager.go` connects `internal/script` to processes.

**Tech Stack:** Go 1.26, SQLite migrations, `internal/tasks`, `internal/workflowrun` (new), `internal/loop`, `internal/daemon`, `internal/compose`, `internal/taskcli`, `internal/commands`, Bash for the end-to-end script.

**Spec:** `docs/superpowers/specs/2026-10-02-workflow-images-design.md`, sections "Script protocol", "Secrets", "Transition by an agent", "Transition by a script", "Queue binding" (secrets, Compose), and "Phases", items 3 and 5.

**Depends on:** `docs/superpowers/plans/2026-10-02-workflow-engine-core.md` and `docs/superpowers/plans/2026-10-02-script-exit-codes.md` (constants `script.QuietExit` = 111 and `script.RejectExit` = 112).

**How to read this plan:** each task gives the files, the exact exported interface, the behaviors to pin with tests, and the constraints. The implementer writes the tests first, watches them fail, then implements. Names and signatures in "Produces" are binding.

## Global Constraints

- Exit codes: `0` — a check passed, or a watch script produced an outcome; `111` — a watch script saw no change; `112` — a check's condition does not hold; anything else, a timeout, an unreadable result file, an undeclared outcome, or an undeclared artifact — a failure. `111` from a check and `112` from a watch script are failures.
- A script is an executable file inside the unpacked workflow image. The daemon runs it directly, with no arguments and no shell wrapper.
- Environment variables given to every run: `TARIBOY_TASK_KEY`, `TARIBOY_TASK_QUEUE`, `TARIBOY_WORKFLOW_NAME`, `TARIBOY_WORKFLOW_VERSION`, `TARIBOY_WORKFLOW_STATUS`, `TARIBOY_WORKFLOW_OUTCOME` (checks only), `TARIBOY_WORKFLOW_DIR`, `TARIBOY_TASK_FILE`, `TARIBOY_TASK_DIR`, `TARIBOY_RESULT_FILE`, `TARIBOY_QUIET_EXIT=111`, `TARIBOY_REJECT_EXIT=112`. A run never receives `TARIBOY_TOOLS_SOCKET`.
- Result file: optional JSON object with `outcome`, `message`, and `artifacts`; at most 64 KiB; `message` at most 4 KiB; artifact values at most 64 KiB each.
- Run modes: `queue` — working directory is the task directory; environment is the daemon baseline, then workflow `env`, then queue secrets. `agent` — working directory is the holder's effective working directory; environment is the daemon baseline, the holder's environment and secrets, then workflow `env`, then queue secrets. Watch scripts always run as `queue`.
- Files of a task live under `<base-dir>/tasks/<KEY>/`: `state/` is `TARIBOY_TASK_DIR`; `runs/<run-id>/` holds `task.json`, `result.json`, and `run.log`. Directories are `0700`, files `0600`.
- Secret values are never returned by a read route and never written to a task snapshot, an artifact, a result file, an event, or a log line the daemon writes.
- A failure is never silent: a failed check closes its request as `failed` with the log path; every failure increments the visit's `script_failures`. A check rejection increments `rejected_requests`. This plan only counts; pausing is the next plan.
- Runs of one task never overlap.
- Run commands directly from the worktree root, without `bash -lc`. Run each `git` command as its own plain command.
- Never run tests against the live `~/.tariboy`, `~/.tariboyd`, or `127.0.0.1:9990`. Every test daemon uses its own base and runtime directories and a disabled listener.
- Do not bump the version and do not edit `CHANGELOG.md`. Commit messages carry no attribution trailer.

## Review Focus

- A script that writes a huge or malformed result file, or prints megabytes to stdout, must fail cleanly without exhausting memory.
- A script that forks a child and exits must not leave the child running after a timeout or a cancel: the whole process group dies.
- A transition must apply exactly once when the agent's iteration ends before its checks finish, and when the daemon restarts between a passing check and the transition.
- Leaving a script status by an operator move, a cancel, or an outcome must stop the watch script and never start another run for that visit.
- A queue secret must not appear in `task.json`, events, API responses, or the support bundle.

## File Structure

| File | Responsibility |
| --- | --- |
| `internal/store/migrations/0056_task_script_runs.sql` | Run records, watch schedule column, queue secrets. |
| `internal/workflowrun/protocol.go` | Environment names, result parsing, verdict classification. |
| `internal/workflowrun/execute.go` | One process: files, process group, timeout, log. |
| `internal/workflowrun/supervisor.go` | The daemon worker loop. |
| `internal/tasks/workflow_runs.go` | Run records and what a finished run means. |
| `internal/tasks/workflow_secrets.go` | Queue secrets. |
| `internal/tasks/workflow_snapshot.go` | The task snapshot given to scripts. |
| `scripts/workflow-e2e.sh` | End-to-end proof on an isolated daemon. |

---

### Task 1: The script protocol

**Files:**
- Create: `internal/workflowrun/protocol.go`
- Test: `internal/workflowrun/protocol_test.go`

**Interfaces:**
- Consumes: `script.QuietExit`, `script.RejectExit`.
- Produces:

```go
package workflowrun

const (
	KindCheck = "check"
	KindWatch = "watch"

	VerdictPass    = "pass"    // check: condition holds
	VerdictReject  = "reject"  // check: condition does not hold
	VerdictOutcome = "outcome" // watch: an outcome is ready
	VerdictQuiet   = "quiet"   // watch: nothing changed
	VerdictFailure = "failure" // anything else
)

const (
	MaxResultBytes   = 64 << 10
	MaxMessageBytes  = 4 << 10
	MaxArtifactBytes = 64 << 10
)

type Declared struct {
	Outcomes  []string // outcomes of the current status; watch only
	Artifacts []string // artifact names the workflow declares
}

type Verdict struct {
	Kind      string            // one of the Verdict* constants
	Outcome   string            // set for VerdictOutcome
	Message   string            // script message, or the failure reason
	Artifacts map[string]string // set for VerdictPass and VerdictOutcome
}

// Classify turns a finished process into a verdict. exit is nil when the
// process did not exit normally; timedOut and rawResult describe the rest.
func Classify(kind string, exit *int, timedOut bool, rawResult []byte, resultErr error, declared Declared) Verdict

// ProtocolEnv returns the TARIBOY_* entries for one run.
func ProtocolEnv(v EnvValues) []string

type EnvValues struct {
	TaskKey, Queue, WorkflowName, WorkflowVersion, Status, Outcome string
	WorkflowDir, TaskFile, TaskDir, ResultFile                      string
}
```

- [ ] **Step 1: Write the failing tests:** a table over kind × exit code covering every row of the spec's exit code table; a missing result file with exit `0` is a pass for a check and a failure for a watch script (no outcome); malformed JSON, an oversized file, an oversized message, an undeclared outcome, an undeclared artifact, and an oversized artifact value are failures with a reason; a rejected check keeps its message; `ProtocolEnv` omits `TARIBOY_WORKFLOW_OUTCOME` when empty and always carries both exit code variables.
- [ ] **Step 2: Run** `go test ./internal/workflowrun/ -count=1` and confirm it fails to compile.
- [ ] **Step 3: Implement** `protocol.go`.
- [ ] **Step 4: Run** the same command and confirm it passes.
- [ ] **Step 5: Commit** with message `Add the workflow script protocol`.

---

### Task 2: Executing one run

**Files:**
- Create: `internal/workflowrun/execute.go`
- Test: `internal/workflowrun/execute_test.go`

**Interfaces:**
- Consumes: Task 1.
- Produces:

```go
type Spec struct {
	RunID      string
	Kind       string
	ScriptPath string        // absolute path inside the unpacked image
	Cwd        string
	Env        []string      // complete environment, protocol entries included
	Timeout    time.Duration
	RunDir     string        // <base>/tasks/<KEY>/runs/<run-id>
	TaskDir    string        // <base>/tasks/<KEY>/state
	Snapshot   []byte        // written to RunDir/task.json
	Declared   Declared
	OnStart    func(pid int) // called once the process has a PID
}

type Result struct {
	Verdict  Verdict
	ExitCode *int
	TimedOut bool
	LogPath  string
	Started  time.Time
	Finished time.Time
}

// Execute prepares the directories and task.json, runs the script in its own
// process group with combined output in RunDir/run.log, enforces the timeout,
// reads RunDir/result.json, and classifies. Cancelling ctx kills the group.
func Execute(ctx context.Context, spec Spec) Result
```

- [ ] **Step 1: Write the failing tests** with real stub scripts written into `t.TempDir()`: pass, reject with a message, watch outcome with an artifact, quiet, crash, timeout, a script that forks a sleeping child and exits (the child is gone after `Execute` returns), a script that prints 8 MiB (the log is on disk, memory use is bounded, the verdict is unaffected), a result file larger than the limit, context cancellation, a missing or non-executable script (failure, no panic), directory and file modes.
- [ ] **Step 2: Run** `go test ./internal/workflowrun/ -count=1` and confirm the new tests fail.
- [ ] **Step 3: Implement.** Follow the process handling of the agent script supervisor in `internal/loop/manager.go` (`Setpgid`, group kill, `WaitDelay`). Read the result file through `io.LimitReader`.
- [ ] **Step 4: Run** the same command and confirm it passes.
- [ ] **Step 5: Commit** with message `Execute workflow scripts in their own process group`.

---

### Task 3: Queue secrets

**Files:**
- Create: `internal/store/migrations/0056_task_script_runs.sql` (this task adds the `task_queue_secrets` part; Task 4 adds the rest to the same file before either is committed, or uses `0057` if this task is committed first — one migration per commit)
- Create: `internal/tasks/workflow_secrets.go`
- Modify: `internal/tasks/workflow_binding.go`, `internal/taskcli/*`, `internal/commands/tasks.go`, `tasks_openapi.go`
- Test: `internal/tasks/workflow_secrets_test.go`, command and parser tests

**Interfaces:**
- Consumes: the engine core plan.
- Produces:

```sql
CREATE TABLE task_queue_secrets (
    queue_prefix TEXT NOT NULL REFERENCES task_queues(prefix) ON DELETE CASCADE,
    key          TEXT NOT NULL,
    value        TEXT NOT NULL,
    updated_at   TEXT NOT NULL,
    PRIMARY KEY (queue_prefix, key)
);
```

```go
func (s *Service) SetQueueSecret(ctx context.Context, actor Actor, queue, key, value string) error
func (s *Service) ListQueueSecretKeys(ctx context.Context, actor Actor, queue string) ([]string, error)
func (s *Service) RemoveQueueSecret(ctx context.Context, actor Actor, queue, key string) error
// queueSecrets is internal: values for the run environment only.
func (s *Service) queueSecrets(ctx context.Context, queue string) (map[string]string, error)
```

Commands: `ttasks queue secret set QUEUE KEY [--value V]` (stdin when `--value` is absent), `ttasks queue secret ls QUEUE`, `ttasks queue secret rm QUEUE KEY`; routes `PUT`, `GET`, and `DELETE` under `/api/task-queues/{queue}/secrets`. All customer only.

Behaviors to pin: key rule `^[A-Za-z_][A-Za-z0-9_]*$`; no route or command output ever contains a value; `SetQueueWorkflow` fails with `workflow_secret_missing` (409) listing every `requires_secrets` name without a value; `RemoveQueueSecret` is rejected with the same code while the bound manifest requires that key; an agent actor is `forbidden`; the event recorded for a set or remove names the key only.

- [ ] **Step 1: Write the failing tests.**
- [ ] **Step 2: Run** `go test ./internal/tasks/ ./internal/taskcli/ ./internal/commands/ ./internal/store/ -count=1` and confirm the new tests fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** the same command and confirm it passes.
- [ ] **Step 5: Commit** with message `Add queue secrets for workflow scripts`.

---

### Task 4: Run records, checks, and watch scripts in the task service

**Files:**
- Create or extend: the script runs migration (`task_script_runs`, `task_status_visits.next_watch_at`)
- Create: `internal/tasks/workflow_runs.go`, `internal/tasks/workflow_snapshot.go`
- Modify: `internal/tasks/workflow_requests.go` (`Advance` with checks), `internal/tasks/workflow_engine.go` (entering and leaving a script status), `internal/tasks/workflow_view.go` (runs in the view)
- Test: `internal/tasks/workflow_runs_test.go`

**Interfaces:**
- Consumes: Task 1's verdict type, the engine core plan.
- Produces:

```sql
CREATE TABLE task_script_runs (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id          INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    visit_id         INTEGER NOT NULL REFERENCES task_status_visits(id) ON DELETE CASCADE,
    request_id       INTEGER REFERENCES task_transition_requests(id) ON DELETE CASCADE,
    kind             TEXT NOT NULL CHECK(kind IN ('check','watch')),
    script           TEXT NOT NULL,
    run_as           TEXT NOT NULL CHECK(run_as IN ('queue','agent')),
    check_index      INTEGER NOT NULL DEFAULT 0,
    state            TEXT NOT NULL CHECK(state IN ('pending','running','finished','interrupted','cancelled')),
    verdict          TEXT NOT NULL DEFAULT '',
    exit_code        INTEGER,
    message          TEXT NOT NULL DEFAULT '',
    cancel_requested INTEGER NOT NULL DEFAULT 0,
    pid              INTEGER,
    created_at       TEXT NOT NULL,
    started_at       TEXT NOT NULL DEFAULT '',
    finished_at      TEXT NOT NULL DEFAULT '',
    log_path         TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX idx_task_script_runs_one_active
    ON task_script_runs(task_id) WHERE state IN ('pending','running');
ALTER TABLE task_status_visits ADD COLUMN next_watch_at TEXT NOT NULL DEFAULT '';
```

```go
type ScriptRun struct {
	ID         int64  `json:"id"`
	TaskKey    string `json:"task_key"`
	Kind       string `json:"kind"`
	Script     string `json:"script"`
	RunAs      string `json:"run_as"`
	State      string `json:"state"`
	Verdict    string `json:"verdict,omitempty"`
	ExitCode   *int   `json:"exit_code,omitempty"`
	Message    string `json:"message,omitempty"`
	CreatedAt  string `json:"created_at"`
	StartedAt  string `json:"started_at,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`
	LogPath    string `json:"log_path,omitempty"`
}

// RunJob is everything the worker needs to execute one pending run.
type RunJob struct {
	Run             ScriptRun
	Queue           string
	WorkflowName    string
	WorkflowVersion string
	WorkflowDigest  string
	Status          string
	Outcome         string            // requested outcome; checks only
	Timeout         time.Duration
	Holder          string            // agent name for run_as agent, else ""
	WorkflowEnv     map[string]string
	QueueSecrets    map[string]string
	Snapshot        []byte            // task.json content, no secrets
	Outcomes        []string          // declared outcomes of the status
	Artifacts       []string          // declared artifact names
}

type RunCompletion struct {
	Verdict    string
	Outcome    string
	Message    string
	Artifacts  map[string]string
	ExitCode   *int
	LogPath    string
	FinishedAt string
}

func (s *Service) PendingRunJobs(ctx context.Context) ([]RunJob, error)
func (s *Service) ClaimScriptRun(ctx context.Context, id int64, startedAt, logPath string) (bool, error)
func (s *Service) SetScriptRunPID(ctx context.Context, id int64, pid int) error
func (s *Service) CompleteScriptRun(ctx context.Context, id int64, done RunCompletion) error
func (s *Service) ScheduleDueWatches(ctx context.Context, now time.Time) (int, error)
func (s *Service) RecoverScriptRuns(ctx context.Context) error
func (s *Service) CancelRequestedRuns(ctx context.Context) ([]ScriptRun, error) // running runs to kill
func (s *Service) ListScriptRuns(ctx context.Context, actor Actor, key string) ([]ScriptRun, error)
func (s *Service) GetScriptRun(ctx context.Context, actor Actor, key string, id int64) (ScriptRun, error)
```

Behaviors to pin:
- `Advance` on a transition with checks records a `pending` request and one pending run for the first check, and returns the pending request; a second `Advance` meanwhile is `transition_pending`;
- `CompleteScriptRun` with `pass` creates the next check's run, or, after the last check, applies the transition, stores the artifacts the checks returned with author `script:<path>`, and closes the request as `applied`;
- with `reject`: the request is `rejected` with the script message, `rejected_requests` on the visit increases, the status is unchanged, and the holder gets a Goal wake through the goal signal;
- with `failure`: the request is `failed` with the message and log path, and `script_failures` increases;
- entering a script status sets `next_watch_at` to now; `ScheduleDueWatches` creates one pending watch run per due visit and never a second while one is active;
- a watch `quiet` sets `next_watch_at` to finish time plus `every`; `outcome` stores artifacts and applies the transition for that outcome with the script message; `failure` increases `script_failures` and reschedules after `every`;
- leaving the status (transition, operator move, cancel) clears `next_watch_at`, cancels a pending run, and sets `cancel_requested` on a running one; a run that completes after its visit was left changes nothing but its own row;
- `RecoverScriptRuns` marks running runs `interrupted`; an interrupted check closes its request as `failed`; an interrupted watch run reschedules;
- completing a run twice is a no-op;
- the snapshot contains the key, queue, title, description, priority, customer, current status, requested outcome and message, holders per pool, and current artifacts with authors, and no secret value.

- [ ] **Step 1: Write the failing tests** for the behaviors above. Drive the service directly; no processes are needed.
- [ ] **Step 2: Run** `go test ./internal/tasks/ ./internal/store/ -count=1` and confirm the new tests fail.
- [ ] **Step 3: Implement.** Replace the `checks_unavailable` refusal from the engine core plan.
- [ ] **Step 4: Run** the same command and confirm it passes.
- [ ] **Step 5: Commit** with message `Record workflow script runs and apply their verdicts`.

---

### Task 5: The daemon worker and run modes

**Files:**
- Create: `internal/workflowrun/supervisor.go`
- Modify: `internal/loop/manager.go` (a method that returns an agent's script runtime), `internal/daemon/daemon.go` (start the worker, call `RecoverScriptRuns` at startup)
- Test: `internal/workflowrun/supervisor_test.go`, `internal/loop/manager_test.go`

**Interfaces:**
- Consumes: Tasks 1, 2, and 4; `workflowimage.Store.FilePath` and `ContentDir`.
- Produces:

```go
// internal/loop
// ScriptRuntime returns the working directory and environment a script run
// "as the agent" uses: the daemon baseline, the agent's environment and
// secrets, the agent bin directory on PATH, and no tools socket.
func (m *Manager) ScriptRuntime(agentName string) (cwd string, env []string, err error)

// internal/workflowrun
type Jobs interface {
	PendingRunJobs(ctx context.Context) ([]tasks.RunJob, error)
	ClaimScriptRun(ctx context.Context, id int64, startedAt, logPath string) (bool, error)
	SetScriptRunPID(ctx context.Context, id int64, pid int) error
	CompleteScriptRun(ctx context.Context, id int64, done tasks.RunCompletion) error
	ScheduleDueWatches(ctx context.Context, now time.Time) (int, error)
	CancelRequestedRuns(ctx context.Context) ([]tasks.ScriptRun, error)
}

type Supervisor struct {
	Jobs         Jobs
	Images       *workflowimage.Store
	BaseDir      string
	BaseEnv      func() []string // daemon baseline
	AgentRuntime func(agent string) (cwd string, env []string, err error)
	Clock        func() time.Time
	Wake         <-chan struct{} // optional nudge
	Interval     time.Duration   // poll fallback, default 2s
	Parallel     int             // concurrent runs, default 4
	Log          *slog.Logger
}

func (s *Supervisor) Run(ctx context.Context)
```

Behaviors to pin:
- a pending check run is claimed, executed, and completed with its verdict; two runs of different tasks execute concurrently; at most `Parallel` at once;
- `queue` mode: cwd is `<base>/tasks/<KEY>/state`, environment order is baseline, workflow `env`, queue secrets, protocol entries; `agent` mode: cwd and base environment come from `AgentRuntime`, then workflow `env`, queue secrets, protocol entries; a queue secret overrides an agent value of the same name; the tools socket variable is absent in both;
- an `agent` run whose holder is unknown, or whose `AgentRuntime` errors, completes as a failure with a reason and runs nothing;
- a script path that leaves the image, or a missing image, completes as a failure;
- a run with `cancel_requested` set while running is killed and completed as cancelled;
- shutting down the context kills running scripts and leaves their rows `running` for `RecoverScriptRuns`.

- [ ] **Step 1: Write the failing tests** with a fake `Jobs` and real stub scripts published through a real `workflowimage.Store` in a temp directory.
- [ ] **Step 2: Run** `go test ./internal/workflowrun/ ./internal/loop/ -run "Supervisor|ScriptRuntime" -count=1` and confirm failure.
- [ ] **Step 3: Implement** and wire into the daemon. The worker starts after `RecoverScriptRuns` and stops before the database closes.
- [ ] **Step 4: Run** `make backend-check` and confirm every step reports success.
- [ ] **Step 5: Commit** with message `Run workflow scripts from the daemon`.

---

### Task 6: Commands for requests and runs

**Files:**
- Modify: `internal/tasks/actions.go`, `internal/taskcli/*`, `internal/commands/tasks.go`, `tasks_openapi.go`
- Test: the matching test files

**Interfaces:**
- Consumes: Task 4.
- Produces:
  - `ttasks advance` waits for a pending request: it polls the request until it leaves `pending`, bounded by the sum of the check timeouts plus 30 seconds, then prints `applied`, or `rejected` or `failed` with the script message and log path, and exits non-zero for the last two. `--no-wait` returns the pending request at once.
  - `ttasks workflow runs KEY` and `ttasks workflow log KEY RUN` for the holder and the customer; routes `GET /api/tasks/{key}/workflow/runs`, `GET /api/tasks/{key}/workflow/runs/{id}`, and `GET /api/tasks/{key}/workflow/runs/{id}/log` (bounded tail, owner-scoped path checks like the agent script log route).
  - `GET /api/tasks/{key}/workflow/requests/{id}` and agent action `request_get`.

- [ ] **Step 1: Write the failing tests:** the wait loop against a fake transport for applied, rejected, failed, and timeout; parser and help tests; route tests; the log route refuses a path outside the task's run directory.
- [ ] **Step 2: Run** `go test ./internal/taskcli/ ./internal/commands/ ./internal/tasks/ -count=1` and confirm the new tests fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** the same command and confirm it passes.
- [ ] **Step 5: Commit** with message `Show workflow requests and script runs`.

---

### Task 7: Compose binding

**Files:**
- Modify: `internal/compose/file.go`, `reconcile.go`, `cli.go`
- Test: `internal/compose/*_test.go`

**Interfaces:**
- Consumes: `workflow.build` and `SetQueueWorkflow`.
- Produces: `task_queues.<PREFIX>.workflow: "[STORE/]NAME[:TAG]"`. With a Store prefix, `compose up` builds that Store source first (a no-op when the version is already published) and binds `NAME:TAG`, where `TAG` defaults to the source's `workflow_version`. Without a prefix it binds an already published ref, `TAG` defaulting to `latest`. Pools are bound before the workflow. An absent `workflow` key leaves an existing binding untouched. Compose never reads or writes secrets; a missing secret surfaces as the `workflow_secret_missing` error of the bind step. `compose status` shows the bound name, version, and digest.

- [ ] **Step 1: Write the failing tests:** parsing of the four ref forms and rejection of malformed ones; reconcile order (agents, pools, build, bind); idempotent second run; the secret error passes through.
- [ ] **Step 2: Run** `go test ./internal/compose/ -count=1` and confirm the new tests fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** the same command and confirm it passes.
- [ ] **Step 5: Commit** with message `Bind queue workflows from Compose`.

---

### Task 8: End-to-end script and documentation

**Files:**
- Create: `scripts/workflow-e2e.sh`, `scripts/testdata/workflow-e2e/` (a workflow source with stub scripts)
- Modify: `Makefile` (target `workflow-e2e`, the `full-check` step)
- Modify: `docs/docs/task-workflows.mdx` (full rewrite), `docs/docs/workflow-images.mdx`, `docs/docs/tasks.mdx`, `docs/docs/security-controls.mdx`, `docs/docs/binaries/compose.mdx`, `docs/docs/architecture/index.mdx`, `state-model.mdx`, `docs/docs/reference/commands.md`, `docs/docs/development.mdx`

**Interfaces:**
- Consumes: everything above.
- Produces: `make workflow-e2e`, which on an isolated daemon (own `TARIBOY_BASE_DIR` and `TARIBOY_RUNTIME_DIR`, HTTP listener disabled) builds the fixture workflow, sets a queue secret, binds the queue, creates a task, and drives it through a pool status, a customer status, and a script status, proving: a rejected check leaves the status and reports the script message; a passing check applies the transition; a watch script that exits `111` twice and then reports an outcome moves the task; a failing script is recorded as a failure; an operator move and a cancel work; the secret reaches the script and appears in no API output. Use `git show main:scripts/workflow-e2e.sh` as the reference for daemon isolation, agent setup with the stub harness, and cleanup.

- [ ] **Step 1: Write** the fixture and the script; run it and fix what it finds.
- [ ] **Step 2: Rewrite** `docs/docs/task-workflows.mdx` as the operator guide: mental model, one complete example, queue binding and secrets, runtime, the script protocol with the exit code table, run modes, commands, routes, persistence, errors and troubleshooting. Update the other pages listed above; `security-controls.mdx` gains queue secrets (including that database backups hold them in plaintext) and the statement that a gate protects against a skipped step, not against an agent that works around it.
- [ ] **Step 3: Run** `make check` and `make workflow-e2e`; confirm both succeed.
- [ ] **Step 4: Run** `git diff --check main...HEAD` and confirm no output.
- [ ] **Step 5: Commit** with message `Add the workflow end-to-end script and documentation`.
