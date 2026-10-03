# Workflow images design

Design agreed in conversation with the customer on 2026-10-02. It also covers
IMPROVE-3khm (fixed quiet exit code for scripts), which becomes phase 1.

This document is the specification. Implementation proceeds phase by phase;
each phase gets its own plan under `docs/superpowers/plans/`.

## Goal and scope

Agents do not reliably follow the process their image describes. The clearest
symptom: the developer agent is told to start a durable pull request monitor and
to resume it after every result, and it sometimes does neither. Nothing in the
daemon notices.

The goal is a process the daemon enforces rather than one the prompt requests:

- A queue defines the process. A task in that queue moves only through declared
  statuses, and leaves a status only when the daemon has verified the exit
  conditions.
- The daemon stays generic. Every concrete check — which VCS, which review
  system, what counts as merged — lives in scripts shipped with the process, not
  in daemon code.
- An agent image keeps describing the agent: its instructions and tools. Any
  agent may work a task from any queue.
- Freedom remains available. A queue without a workflow behaves exactly as
  today, and a workflow may be as loose as one status with no checks.

Out of scope:

- more than one owner per status, parallel branches, and joins;
- OS-level isolation of workflow scripts from the agent;
- changes to the task `pull_request` field;
- automatic Store refresh-and-build for workflow images.

## What the code confirms

All references are to `main` at `6c216ee` (0.71.2).

- The developer process is prompt-only. It is the eight-row Flow table in
  `tariboy-store/images/tariboy-developer/instructions.md`; rows 6 and 7 make the
  agent start one durable monitor and resume it with `rerun`.
- A flexible task gates nothing. Its statuses are `open`, `in_progress`,
  `wait_customer`, `done`, and `cancelled`, and `ttasks done` succeeds at any
  time.
- A durable script belongs to an agent, not a task. `script.Definition`
  (`internal/script/model.go`) has no task key, so the daemon cannot tell whether
  a task has a live monitor.
- A recurring script keeps its schedule only while it stays quiet; any published
  result stops it (`internal/script/store.go:300-306`). The quiet code is a
  per-definition `quiet_exit` value that every caller sets to `2`.
- A workflow engine already exists in `internal/tasks/workflow_*.go` with
  versions, pools, leases, requirements, artifacts, guards, questions, and
  observations (`internal/store/migrations/0026_task_workflows.sql`). No workflow
  definition exists in this repository or in `tariboy-store`.
- That engine cannot express the developer process:
  - Its gates are self-attested. Completion checks only that an artifact with the
    required name exists (`workflow_runtime.go`, `requireAssignmentOutputsTx`),
    and artifact content is validated syntactically (`workflow_artifacts.go`,
    `validateArtifactContent`).
  - Every non-terminal status needs a requirement with an agent pool
    (`workflow_validator.go:245,264`), so customer approval and waiting for a
    merge have no representation.
  - An assignment is a time lease, 30 minutes by default, while a pull request
    waits for hours or days.
- Goal selects by `assignee` and the flexible status
  (`internal/taskgoal/store.go:203-227`), and `validGoal` treats `wait_customer`
  without an open customer wait as not a valid Goal (`store.go:278`). The Goal
  text in the prompt is fixed (`internal/loop/prompt_v2.go:59`).
- Scripts run with the agent's environment and secrets in the agent's working
  directory (`internal/loop/manager.go:1873`). Secrets are stored per agent
  (`internal/store/migrations/0002_agents.sql`, table `secrets`).
- An agent image is a versioned artifact built from a Store source directory;
  tags are pointers to stored content
  (`docs/docs/images/index.mdx`, "Refs, tags and identity").

## Decisions

| Question | Decision |
| --- | --- |
| Where the process lives | In a workflow image: a new versioned Store artifact holding the status graph and its scripts. |
| Existing workflow engine | Replaced in place by a simpler one. Pools stay. |
| Owners per status | Exactly one: a pool, the customer, or a script. |
| Lease | No time lease. An agent holds a task while it is able to work. |
| Stalled work | The daemon pauses the task and asks the customer. |
| Artifacts | Kept, simplified: named values the daemon stores and does not interpret. |
| Status model | One status per task, from the workflow. Each status has a derived category. |
| Who assigns work | The daemon dispatches tasks to pool members. There is no claim command. |
| Script secrets | Declared by the workflow image, supplied by the queue. |
| Quiet exit code | A fixed protocol constant, `111` (IMPROVE-3khm). |

## Concepts

**Workflow image.** A built, immutable artifact with a name, a version, and a
content digest. It contains a manifest, status instructions, and scripts.

**Status.** A node of the process. It has one owner:

- `pool` — one agent from a named pool of the queue does the work;
- `customer` — the task waits for the customer's decision;
- `script` — the task waits for a watch script the daemon runs.

**Outcome and transition.** A status declares transitions, each keyed by an
outcome name. The owner declares the outcome: an agent with a command, the
customer in the task, a watch script through its result file.

**Check.** A script attached to a transition out of a pool status. The daemon
runs it when the agent requests that transition. The transition happens only
when every check passes.

**Artifact.** A named value on a task. The workflow image declares the names
and their meaning. The daemon stores values and verifies presence only.

**Category.** A property of a status, derived from its owner. Generic
mechanisms that must not know status names — Goal, list filters, blocking
relations — use the category.

**Holder.** The agent currently working a pool status. The task remembers the
last holder per pool.

**Pause.** A task state in which the workflow status is unchanged, no work is
dispatched, and the customer has an open question to decide how to continue.

## Workflow image source

A Store holds workflow sources next to image sources:

```text
workflows/development/
  Workflowfile.yaml
  statuses/plan.md
  statuses/implement.md
  statuses/complete.md
  scripts/pr-open.sh
  scripts/pr-monitor.py
  scripts/merged-on-base.sh
```

### `Workflowfile.yaml`

The loader is strict: an unknown field is an error.

```yaml
schema_version: 1
name: development
workflow_version: 0.1.0
initial_status: plan

requires_secrets: [GH_TOKEN]
env:
  PR_POLL_SECONDS: "60"

limits:
  idle_iterations: 3
  rejected_requests: 5
  script_failures: 3
  unavailable_grace: 5m

artifacts:
  - name: plan
    description: The implementation plan, as Markdown.
  - name: pull_request
    description: URL of the pull request for this task.
  - name: merge_commit
    description: Merge commit recorded by the monitor.

statuses:
  - id: plan
    owner: { pool: developers }
    instructions: ./statuses/plan.md
    transitions:
      - on: planned
        to: approval
        requires: [plan]

  - id: approval
    owner: customer
    transitions:
      - { on: approved, to: implement }
      - { on: changes_requested, to: plan }

  - id: implement
    owner: { pool: developers }
    instructions: ./statuses/implement.md
    limits: { idle_iterations: 6 }
    transitions:
      - on: ready
        to: review
        requires: [pull_request]
        checks:
          - script: ./scripts/pr-open.sh
            timeout: 60s

  - id: review
    owner: script
    watch:
      script: ./scripts/pr-monitor.py
      every: 60s
      timeout: 60s
    transitions:
      - { on: merged, to: complete }
      - { on: changes_requested, to: implement }

  - id: complete
    owner: { pool: developers }
    instructions: ./statuses/complete.md
    transitions:
      - on: cleaned
        to: done
        checks:
          - script: ./scripts/merged-on-base.sh
            run_as: agent

  - id: done
    terminal: true
```

### Root fields

| Field | Required | Meaning |
| --- | --- | --- |
| `schema_version` | yes | `1`. |
| `name` | yes | Workflow name; a safe route segment. |
| `workflow_version` | yes | SemVer 2.0.0 without a `v` prefix. |
| `initial_status` | yes | ID of a non-terminal status. |
| `statuses` | yes | The status graph. |
| `artifacts` | no | Declared artifact names with descriptions. |
| `requires_secrets` | no | Names of secrets the scripts need. |
| `env` | no | Non-secret environment defaults for scripts. |
| `limits` | no | Workflow-wide defaults; see [Limits](#limits). |

### Status fields

| Field | Applies to | Meaning |
| --- | --- | --- |
| `id` | all | Unique status ID. |
| `owner` | non-terminal | `{pool: NAME}`, `customer`, or `script`. |
| `instructions` | pool, customer | Source-relative Markdown file shown to the owner. |
| `watch` | script | `script`, `every`, and `timeout` of the watch script. |
| `transitions` | non-terminal | List of `on`, `to`, and, for pool statuses, `requires` and `checks`. |
| `limits` | pool, script | Per-status overrides of the workflow limits. |
| `terminal` | terminal | `true`. A terminal status has no owner and no transitions. |
| `cancelled` | terminal | `true` when the result counts as cancelled rather than done. |

A check has `script`, an optional `run_as` (`queue`, the default, or `agent`),
and an optional `timeout` (default `60s`, maximum `30m`).

### Limits

| Limit | Default | Meaning |
| --- | --- | --- |
| `idle_iterations` | `3` | Consecutive iterations a holder may finish without requesting a transition before the task pauses. |
| `rejected_requests` | `5` | Consecutive transition requests a check may reject before the task pauses. |
| `script_failures` | `3` | Consecutive script failures before the task pauses. |
| `unavailable_grace` | `5m` | How long a holder may be unable to work before the task pauses. `0` pauses at once. |

### Validation

Build and `validate` report every independent error with a stable code and
path. The rules:

- status IDs are unique; `initial_status` exists and is non-terminal;
- every status is reachable from `initial_status`, and a terminal status is
  reachable;
- every `to` names a declared status; outcome names are unique within a status;
- a non-terminal status has an owner and at least one transition;
- `requires` and `checks` appear only on transitions out of a pool status;
- `watch` appears on every `script` status and nowhere else;
- every name in `requires` is a declared artifact;
- every script and instruction path is source-relative, stays inside the source
  directory, and names a regular file; scripts are executable;
- symlinks inside the source are rejected, as for image sources.

## Build, storage, and versions

`tariboy workflow build` validates the source and publishes it. The built
artifact is stored once by content digest under
`<base-dir>/workflows/<name>/refs/<digest>/` as an owner-only, read-only tree.
Tags are pointer files under `<base-dir>/workflows/<name>/tags/`. A build
publishes the `workflow_version` tag and `latest`.

A published version is immutable. Rebuilding the same `workflow_version` with
identical content is a no-op; with different content it fails with
`workflow_version_published`. This is stricter than agent images, where a
rebuild moves the ref, because a task in flight must keep the exact scripts it
started with.

The manifest of every published digest is also stored in SQLite, so the reducer
does not read the filesystem to decide a transition.

The daemon refuses to remove a digest that an open task pins.

Operator commands:

```bash
tariboy workflow validate --path DIR
tariboy workflow build --path DIR
tariboy workflow build STORE/NAME
tariboy workflow ls
tariboy workflow inspect NAME[:TAG]
tariboy workflow rm NAME:TAG
tariboy workflow version get [--path DIR]
tariboy workflow version update major|minor|patch [--path DIR]
```

## Queue binding

A queue binds one workflow digest, its pools, and its secrets:

```bash
ttasks queue workflow set DEV development:0.1.0
ttasks queue workflow clear DEV
ttasks queue pool set DEV developers dev-a dev-b
ttasks queue secret set DEV GH_TOKEN        # value from --value or stdin
ttasks queue secret ls DEV
ttasks queue secret rm DEV GH_TOKEN
```

The binding resolves the tag to a digest when it is set. Moving a tag later does
not change the queue; the operator binds the new version explicitly.

Binding fails when a pool named by the manifest is missing or empty
(`workflow_pool_empty`) or a required secret has no value
(`workflow_secret_missing`). Removing the last member of a bound pool or a bound
secret is rejected for the same reason.

A new task in a bound queue pins the queue's current digest in the transaction
that creates it and enters `initial_status`. An existing task keeps its pinned
digest. A task is never migrated to another workflow version.

Compose declares the same binding:

```yaml
task_queues:
  DEV:
    name: Development
    workflow: official/development:0.1.0
    pools:
      developers: [dev-a, dev-b]
```

Compose never carries secret values. Queue triggers, which create a task from a
plugin-produced channel message, are unchanged.

## Runtime

### Status and category

A task with a workflow has one status: the workflow status. Its category is
derived:

| State | Category | `assignee` |
| --- | --- | --- |
| pool status, holder assigned | `in_progress` | the holder |
| pool status, no eligible agent | `open` | empty |
| customer status | `wait_customer`, with an open customer wait | unchanged |
| script status | `wait_customer`, with no customer wait | empty |
| paused | `wait_customer`, with an open customer wait | unchanged |
| terminal | `done`, or `cancelled` when the status says so | unchanged |

Nobody sets a category. In storage, the existing `tasks.status` column holds the
category and `tasks.workflow_status` holds the status. In the API, a task with a
workflow reports the workflow status as `status`, the category as `category`,
and, while waiting, `waiting_on` as `customer`, `script`, or `pause`.

A task without a workflow keeps its five statuses; each equals its own
category. `status_view` filters, blocking relations, and the active-descendants
check use the category for every task.

On a task with a workflow, `ttasks done`, `ttasks update --status`, and
`ttasks ready --claim` return `workflow_managed` with the available outcomes and
the `advance` command.

### Dispatch

When a task enters a pool status the daemon assigns a holder:

1. If the task already had a holder for that pool and that agent is still a
   member, the same agent is assigned. It keeps the worktree and the context.
2. Otherwise the daemon picks an eligible member: enabled, loop enabled, not
   halted, Goal enabled, and with no current Goal. Ties go to the member
   dispatched least recently, then to pool order.
3. If no member is eligible, the task waits in category `open`. The daemon
   retries when a member finishes its Goal, when pool membership changes, and on
   the one-minute reconciler cadence.

Assignment sets `assignee`, records the holder, and publishes the usual
assignment notification. From there Goal selects and wakes the agent as today.
Pool membership is read at dispatch time; there is no frozen snapshot.

### Transition by an agent

```bash
ttasks artifacts set DEV-ab12 pull_request https://github.com/org/repo/pull/42
ttasks advance DEV-ab12 --outcome ready --message "PR opened, suite green"
```

`advance` is accepted only from the holder, for an outcome the current status
declares. The daemon then:

1. Rejects the request with `artifact_missing` when a required artifact has no
   value.
2. Records a durable transition request. A second request while one is pending
   returns `transition_pending`.
3. Runs the checks in declared order and stops at the first that does not pass.
4. Applies the transition when all pass, or closes the request as `rejected` or
   `failed` with the script's message.

A transition without checks applies in the request's own transaction. With
checks, the command waits for the result and prints it; if the iteration ends
first, the checks still finish and the result still applies. A transition that
applies after the agent has gone reaches it through the next Goal wake.

### Transition by the customer

In a customer status the task shows the status instructions and one action per
transition. The same `advance` command works for the customer and the operator:

```bash
ttasks advance DEV-ab12 --outcome approved
```

An agent cannot declare an outcome in a customer status.

### Transition by a script

Entering a script status starts its watch script: once immediately, then
`every` after each run finishes. Runs never overlap. Leaving the status, pausing,
or cancelling the task stops it. A run that exits `0` names an outcome in its
result file, and the daemon applies that transition.

### Artifacts

```bash
ttasks artifacts set KEY NAME VALUE     # value from the argument or stdin
ttasks artifacts ls KEY
ttasks artifacts show KEY NAME
```

- Only declared names are accepted. A value is UTF-8 text up to 64 KiB.
- The holder may set artifacts while it holds the task. A script sets them
  through its result file.
- Each write records its author (`agent:<name>` or `script:<path>`) and time. The
  newest value is current; earlier values remain as history.
- The daemon checks presence for `requires`. It never parses a value.

### Pause

The daemon pauses a task when:

- the holder cannot work — the agent is disabled, its loop is disabled, it is
  halted (`halt_kind` `error` or `idle_limit`), it left the pool, or it was
  deleted — and that has lasted `unavailable_grace`;
- the holder finished `idle_iterations` consecutive iterations with this task
  as its Goal, in the same status, without an `advance` request;
- checks rejected `rejected_requests` consecutive requests in the same status;
- scripts failed `script_failures` consecutive times in the same status.

Pausing records the reason, stops any watch script, and opens a customer
question on the task through the existing answer-tracking mechanism, so the
customer gets the usual notification and the task appears under "Waiting for
me". The counters reset when the status changes and when a pause is resolved.

The customer resolves a pause with one decision:

```bash
ttasks workflow resume KEY --decision continue   # same holder, counters reset
ttasks workflow resume KEY --decision release    # clear the holder, dispatch again
ttasks cancel KEY                                # close the task as cancelled
```

A plain comment does not resolve a pause. The daemon never hands a paused task
to another agent on its own, because the worktree stays with the previous
holder.

### Operator override

The customer and the operator may move a task without checks:

```bash
ttasks workflow move KEY --to STATUS --reason "TEXT"
```

The move is recorded as an event with the actor and the reason. Agents cannot
call it. This is the escape hatch for a process that does not fit one task.

`ttasks cancel KEY` is a new command for the customer and the operator. It works
in any status of a task with a workflow: it stops scripts, sets category
`cancelled`, and leaves the workflow status as the record of where the task
stopped. A flexible task is still cancelled with `ttasks update --status
cancelled`.

### Agent questions

`ttasks ask KEY user:LOGIN TEXT` works as on a flexible task. While the holder's
question to the customer is open, the category is `wait_customer`; the answer
returns it to `in_progress`. The workflow status does not change.

## Script protocol

A script is an executable file from the workflow image. The daemon runs it
directly, without arguments and without a shell wrapper. Scripts may call any
external tool. They get no daemon API and no agent tools socket; the only
channel back is the result file.

### Input

| Variable | Value |
| --- | --- |
| `TARIBOY_TASK_KEY`, `TARIBOY_TASK_QUEUE` | The task. |
| `TARIBOY_WORKFLOW_NAME`, `TARIBOY_WORKFLOW_VERSION` | The pinned workflow. |
| `TARIBOY_WORKFLOW_STATUS` | The current status. |
| `TARIBOY_WORKFLOW_OUTCOME` | The requested outcome; checks only. |
| `TARIBOY_WORKFLOW_DIR` | Root of the unpacked workflow image. |
| `TARIBOY_TASK_FILE` | Path to a JSON snapshot of the task. |
| `TARIBOY_TASK_DIR` | Owner-only directory that persists for the life of the task. |
| `TARIBOY_RESULT_FILE` | Path the script writes its result to. |
| `TARIBOY_QUIET_EXIT` | `111`. |
| `TARIBOY_REJECT_EXIT` | `112`. |

The task snapshot contains the key, queue, title, description, priority,
customer, the current status, the requested outcome and message, the holder per
pool, and the current artifacts with their authors. It contains no secrets.

### Output

The result file is optional JSON, at most 64 KiB:

```json
{
  "outcome": "merged",
  "message": "Pull request #42 merged as 9f2c1e7.",
  "artifacts": { "merge_commit": "9f2c1e7" }
}
```

`message` is at most 4 KiB. `artifacts` may name only declared artifacts.
`outcome` is required from a watch script that exits `0` and ignored from a
check.

### Exit codes

| Code | Check | Watch script |
| --- | --- | --- |
| `0` | The condition holds. | An outcome is ready; apply the transition. |
| `111` | Invalid; treated as a failure. | Nothing changed; stay quiet. |
| `112` | The condition does not hold; `message` goes to the agent. | Invalid; treated as a failure. |
| any other | The script failed. | The script failed. |

A timeout, a result file that is not valid JSON, an undeclared outcome, and an
undeclared artifact are failures too.

`111` is the constant from IMPROVE-3khm. `112` separates "the condition does not
hold", which the agent can fix, from a broken script, which it cannot. Both lie
in the range 100–113 that neither the shell, `sysexits`, nor Go's `flag` package
uses.

A failure is never silent. A failed check closes the request as `failed` with
the path of the run log. Every failure counts toward `script_failures`.

### Environment and working directory

| `run_as` | Working directory | Environment |
| --- | --- | --- |
| `queue` | `TARIBOY_TASK_DIR` | daemon baseline, then workflow `env`, then queue secrets |
| `agent` | the holder's effective working directory | daemon baseline, the holder's environment and secrets, then workflow `env`, then queue secrets |

Watch scripts always run as `queue`, so they do not depend on which agent held
the task. `agent` is available only to checks, to inspect a worktree or a local
branch.

The same secret name may exist on an agent and on a queue. They are independent
values; in `agent` mode the queue value wins.

### Runs

Every run is a durable record with its kind, script, state, exit code,
timestamps, and an owner-only log holding combined stdout and stderr. Records
and logs are visible on the task.

Runs follow the pattern of durable agent scripts: a claimed run records its PID
and runs in its own process group, cancellation is durable, and after a daemon
restart a run that was in progress is recorded as `interrupted` and not
repeated blindly. An interrupted check closes its request as `failed`; an
interrupted watch run is followed by the next scheduled run.

Files for a task live under `<base-dir>/tasks/<KEY>/`: `state/` is
`TARIBOY_TASK_DIR`, and `runs/<run-id>/` holds that run's task snapshot, result
file, and log.

## Goal

Goal selection does not change. It already selects by `assignee` and category,
and it already releases a `wait_customer` task that has no customer wait. So:

- in a pool status the holder has the task as its Goal;
- in a script status the agent is free at once;
- in a customer status or a pause the existing wait grace applies, then Goal
  releases the task;
- when a watch script or the customer returns the task to a pool status, the
  daemon assigns the holder again and Goal wakes it with `task.goal`.

The agent no longer handles `script.result` for a workflow task.

### Goal block

For a task with a workflow, the `## Goal` section of **Task Processing Order**
replaces the fixed flexible guidance with:

- the workflow name and version, the current status, and how the task reached
  it (the last transition and its message);
- the status instructions from the workflow image;
- each available outcome with its required artifacts and checks;
- the current artifacts;
- the result of the last transition request, when it was rejected or failed;
- the exact `ttasks artifacts set` and `ttasks advance` commands.

Status instructions are trusted like image prompts. The task title,
description, artifact values, and script messages remain untrusted input.

### Idle iterations

When an iteration of the holder ends, the daemon counts it as idle for the task
if the task was the iteration's Goal, the status is unchanged, and no `advance`
request was made during the iteration. A request, a status change, or a resolved
pause resets the count.

## Secrets

Queue secrets are stored per queue, like agent secrets. Values are never
returned by a read route, never written to the task snapshot, an artifact, a
result file, or an event, and never included in a support bundle. Nightly
database backups contain them in plaintext, as they contain agent secrets
today; the security documentation will say so.

## Boundary

Workflow scripts and agents run as the same OS user. A gate protects against a
skipped step, not against an agent that deliberately works around the process.

The old engine narrowed an agent's messaging tools during a managed iteration.
That mechanism is removed with work packets. An agent on a workflow task has
its normal tools.

## Persistence

New or reshaped:

- `task_workflow_images` — digest, name, version, manifest, build time;
- `task_queue_workflows` — queue, digest, revision;
- `task_queue_secrets` — queue, key, value;
- `tasks` — `workflow_digest` replaces `workflow_version_id`; pause reason and
  counters;
- `task_workflow_holders` — task, pool, agent, last dispatch time;
- `task_status_visits` — task, status, entered and left times, outcome, actor;
- `task_transition_requests` — task, visit, outcome, message, state, result;
- `task_artifacts` — task, name, value, author, time;
- `task_script_runs` — task, visit, request, kind, script, state, exit code,
  times, log path.

Kept as they are: `task_agent_pools`, `task_agent_pool_members`,
`task_queue_workflow_triggers`, and the ingress tables the triggers use.

Dropped: `task_workflow_versions`, `task_status_executions`,
`task_requirement_executions`, `task_assignments`, `task_workflow_questions`,
`task_workflow_holds`, `task_workflow_subscriptions`, `task_observations`.

The old engine is unused, so no data moves. The migration removes old queue
bindings. A task that pinned an old workflow version becomes a flexible task
with its current status, and the migration records that as a task event.

Every workflow mutation, its event, and any notification intent commit in one
transaction, as they do today. New task events: `workflow.transition_requested`,
`workflow.transition_rejected`, `workflow.transitioned`, `workflow.paused`,
`workflow.resumed`, `workflow.moved`, `workflow.script_run`, and `artifact.set`.

Nightly retention deletes the new rows together with their closed task tree and
removes the task's directory.

## Removed

- Requirements, `join`, `require_all`, guard expressions, budgets, retries, and
  time leases.
- Work packets and the `ttasks work`, `ttasks questions`, `ttasks answer`, and
  `ttasks observe` commands.
- Workflow questions and holds; the ordinary `ttasks ask` replaces them.
- Assignment-scoped channel subscriptions and observations; watch scripts
  replace them.
- The `ttasks workflows` definition commands and the `/api/workflows` routes;
  `tariboy workflow` replaces them.
- Workflow tool gating in the agent capability server.

## Error codes

| Code | Meaning |
| --- | --- |
| `workflow_invalid` | The source fails validation; details list every error. |
| `workflow_version_published` | The version exists with different content. |
| `workflow_in_use` | An open task pins the digest. |
| `workflow_pool_empty` | A bound pool is missing or empty. |
| `workflow_secret_missing` | A required secret has no value on the queue. |
| `workflow_managed` | The command does not apply to a task with a workflow. |
| `outcome_unknown` | The status does not declare that outcome. |
| `not_holder` | The caller is not the owner of the current status. |
| `artifact_missing` | A required artifact has no value. |
| `artifact_unknown` | The workflow does not declare that artifact. |
| `transition_pending` | Another request is still running its checks. |
| `transition_rejected` | A check exited `112`. |
| `transition_failed` | A check failed to run. |
| `workflow_paused` | The task is paused and waits for the customer. |

## Phases

Each phase leaves `main` working and gets its own plan.

1. **Script exit codes (IMPROVE-3khm).** Constants `111` and `112`. New
   schedules use `111` only; `TARIBOY_QUIET_EXIT` is exported to every run. The
   Desktop "Quiet exit" field and the `quiet_exit` field of the definition model
   and API response are removed.

   Existing recurring definitions with a stored quiet code are rewritten by the
   migration: the command is wrapped in a nested `sh -c` whose stored code maps
   to `111`, and the column is cleared. The column itself stays in the schema,
   always `NULL`: dropping it needs a rebuild of `scripts`, and a rebuild under
   enforced foreign keys would cascade into `script_runs`. For one release the
   `quiet_exit` request parameter,
   which the Store skill's `--quiet-exit N` flag sends, stays accepted as a
   deprecated alias that applies the same wrapping. Agents on images that still
   pass it keep working until their skills are updated. The parameter is removed
   after phase 7.
2. **Workflow image.** Manifest, validation, build, storage, the SQLite copy of
   the manifest, `tariboy workflow` commands, Store discovery under
   `workflows/`.
3. **Engine.** Statuses and owners, categories, artifacts, `advance`, transition
   requests, check and watch runs, dispatch, queue binding and pinning, the
   override and cancel commands, the migration that removes the old engine.
4. **Goal and pause.** Goal block, idle counting, holder availability, pause
   and resume.
5. **Queue secrets and run modes.** Secret storage and commands, `queue` and
   `agent` environments.
6. **UI.** Status and category in the task list, outcome actions, artifacts,
   script runs and logs on the task, pause decisions, queue workflow settings.
7. **Store content.** Two workflow images. `development` is the example in this
   document, with the pull request monitor moved into it from
   `tariboy-developer`. `research` is the loose one: a single pool status whose
   only transition requires a `report` artifact and has no checks, then a
   terminal status. The `tariboy-developer` instructions shrink to tools and
   working method, and every skill script in the Store switches to
   `TARIBOY_QUIET_EXIT`.

Phases 2 through 6 ship behind no flag: a queue has no workflow until an
operator binds one.

## Testing

- Unit tests per package, written first: manifest validation, the reducer,
  dispatch, the pause conditions, artifact rules, queue binding.
- Protocol contract tests with stub scripts: exits `0`, `111`, `112`, and
  others; a timeout; an oversized, malformed, or missing result file; an
  undeclared outcome and artifact; a daemon restart during a run.
- `scripts/workflow-e2e.sh` is rewritten for the new engine on an isolated
  daemon: build a workflow image, bind a queue, drive a task through pool,
  customer, and script statuses, then exercise a rejected check, a failed
  script, a pause, a resume, and an override.
- Goal tests: the Goal block for a workflow task, release in a script status,
  re-wake on return to a pool status, idle counting.
- UI unit tests and the Tasks browser suite for outcome actions and pause
  decisions.
- In `tariboy-store`: workflow image validation in `make check` and tests for
  the moved monitor script.

Every test daemon uses its own base and runtime directories and a disabled or
isolated listener.

## Documentation

Updated with the phase that changes the behavior:

- `docs/docs/task-workflows.mdx` — rewritten for workflow images;
- `docs/docs/tasks.mdx` — status and category, the `advance` path;
- `docs/docs/architecture/index.mdx`, `state-model.mdx`, `iteration-loop.mdx` —
  ownership, tables, Goal block, removal of work packets;
- `docs/docs/plugins/built-in/scripts.mdx`, `tasks.mdx`,
  `docs/docs/binaries/agent-tools.mdx`, `docs/docs/reference/commands.md` —
  exit codes and commands;
- `docs/docs/images/index.mdx` — workflow sources in a Store;
- `docs/docs/binaries/compose.mdx` — queue workflow binding;
- `docs/docs/security-controls.mdx` — queue secrets, the script boundary;
- `docs/docs/development.mdx` — the workflow test paragraph;
- `tariboy-store/README.md` — the `workflows/` layout.

## Implementation record

All seven phases were implemented on the branch `worktree-workflow-images-design`
(Tariboy) and `workflow-images` (tariboy-store), 2026-10-02/03. Where the
implementation deliberately departs from the text above, the code and the
product documentation under `docs/docs/` are authoritative. The departures:

- **Script exit codes.** `scripts.quiet_exit` stays in the schema, always
  `NULL`, instead of being dropped (a rebuild of `scripts` would cascade into
  `script_runs`). The deprecated `quiet_exit` request parameter is **not**
  removed in this branch: all phases ship together, so removing it would give
  agents on already-built images no release of grace. The nested legacy wrapper
  calls `sh` through the agent's `PATH`.
- **Workflow image.** The digest covers the stored file bytes only. A version is
  immutable even after its tag is removed. `workflow inspect` and `workflow rm`
  take `NAME` and `TAG` as separate arguments. `tariboy workflow validate` runs
  the same source scan as `build` and exits `1` when the source is invalid.
- **Engine.** The old engine's `tasks.workflow_version_id` column and the
  `task_workflow_versions` table stay in the schema, unused. A new holder is
  eligible only when it has no assigned open task (the Goal reconciler's own
  predicate). `advance` takes `--from STATUS`; a mismatch is `status_changed`.
  Leaving a pool status resolves only the holder's own questions; waits authored
  by `system:workflow` are ignored by Goal selection, so a task in a customer
  status (or paused) releases its agent at once — the "existing wait grace" in
  the Goal section does not apply. An image cannot be removed while any task,
  open or closed, pins it. Emptying a pool is refused while a bound image or an
  open task's image names it. A holder keeps read access to its task through its
  holder row even after the assignee is cleared.
- **Scripts.** The task snapshot carries `visit: {id, entered_at}` so a watch
  script can tell "my outcome was applied and the task came back" from "the
  daemon restarted before applying it". `transition_rejected` and
  `transition_failed` are request states and events, not error codes; new codes
  are `script_running`, `run_log_invalid`, `source_invalid`, and the secret
  codes `invalid_secret`, `invalid_secret_key`, `secret_too_large`. A pending
  request carries `wait_seconds`. Queue secret values of at least six bytes are
  replaced with `[redacted]` in a script's message, in artifact values it
  returns, and in the served log tail (a filter of current values only; the
  file on disk is not redacted). A run log is readable by the customer and by
  the task's holders; the log of a `run_as: agent` run only by the customer and
  the holder recorded on the run. After a crash, an orphaned script is
  terminated only with proof of identity from `/proc/<pid>/environ`, and an
  interrupted watch is rescheduled after `every`. Quiet watch runs leave no
  event; only the newest 20 per visit are kept, and only the task's newest 20
  quiet run directories survive. Checks start before watches. Nightly retention
  removes `<base-dir>/tasks/<KEY>/`.
- **Goal and pause.** `release` keeps the pool's holder row marked `released`:
  the released agent is excluded from dispatch for that task and pool until
  another agent takes it or an operator move deletes the released rows; with no
  other member the task stays `open`. The pause takes over the customer's single
  open wait; a holder's open question is absorbed and named in the pause comment.
  Count limits cannot be `0` (only `unavailable_grace: 0` is allowed).
  `rejected_requests` counts every rejection in the visit, not only consecutive
  ones. An iteration does not count as idle when a request of the visit is
  pending or was cancelled/applied during it, when the holder's own question is
  open or answered during it, when it ended as `harness_error`, or when it
  started before the visit's `resumed_at`. The Goal an iteration ran with lives
  in memory, so an adopted iteration after a restart is never counted.
- **UI.** No per-status filter; the Active/Closed/All views select by category.
  `WorkflowView` exposes `declared_artifacts` and `statuses`. The panel
  refetches on workflow events, not only on the task revision.
- **Store.** The `development` scripts are Python (`pr-open.py`,
  `pr-monitor.py`, `merged-on-base.py`, shared `pr_lib.py`); `github-pr.py
  monitor` stays in the developer image for tasks without a workflow, quiet
  with `111`.
