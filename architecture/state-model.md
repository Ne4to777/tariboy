---
title: State model — the DB is the source of truth
description: The SQLite DB is the single source of configuration; live state is computed, not stored, which is why test daemons must be isolated.
sidebar:
  label: State model
  icon: database
---

The **SQLite DB is the single source of configuration.** There is no
`config.json` per agent and no stored `state` column; an agent's **live state**
(`running` / `idle` / `stopped` / `error`) is **computed** from the DB config
plus what is actually running, not persisted.

A running iteration also has persisted AI-proxy liveness evidence. Its
`started_at` time is the initial activity point, and every authenticated request
for that exact agent and iteration updates `last_ai_request_at` before upstream
work begins. When the newest point is at least the agent's positive
`ai_stall_timeout_s` old (1800 seconds, 30 minutes, by default), live state is derived as
`error` with an informational `error_reason`; the loop and iteration continue.
The next attributed request updates the timestamp and live state derives as
`running` again. Stale requests for older or completed iterations are ignored.

Reconciliation and reaping are therefore flag-independent: the daemon derives
what *should* be running from the DB and converges to it.

## Filesystem-backed shell scripts

The global pre-iteration script at `<base-dir>/global-agent-shell.sh` and an
agent script at `<base-dir>/agents/<name>/agent-shell.sh` are intentionally not
SQLite fields. The daemon validates a complete submitted value with `bash -n`
before atomically replacing an owner-only file. A rejected value leaves the
previous file unchanged; a missing or empty file is a no-op at launch.

## Pricing and Usage snapshots

Model-price state has three layers with distinct ownership. Operator-managed
and LiteLLM-managed rows live in `ai_pricing`; built-in prices are runtime
fallbacks rather than persisted manual overrides. The resolved in-memory map is
derived with `manual` rows first, then `litellm` rows, then the built-in
fallback. `model-prices-litellm.json` is a rebuildable cache of the external
source, not a second editable configuration store. A valid cache is reconciled
into the managed database rows during startup before its complete generation is
published to the proxy.

Each `ai_requests` row is historical evidence, not a projection of current
configuration. The proxy calculates `cost_usd` once from one complete price
generation and persists that value with mutually exclusive input, output,
cache-write, and cache-read token buckets. Later price refreshes do not
recalculate Usage or budget history.

The same row stores nullable request-time `group_id` and `group_name` snapshots.
Moving an agent, renaming or deleting a group, or changing current membership
does not rewrite those columns. Rows created before snapshots existed remain
null and appear as Ungrouped; reindexing an old transcript does not guess a
group from current state. See [AI proxy and audit](/docs/architecture/ai-proxy#group-snapshots-and-usage)
for the recording and filtering flow and [Images and groups](/docs/images-and-groups#historical-group-usage)
for the operator-visible consequences.

## Recorded halt reasons

The loop can be turned off automatically as well as by hand: a stop policy
(`on_error` or `on_timeout` set to `stop`) halts it after a failed or timed-out
iteration, and the idle limit halts it after enough consecutive self-declared
idle iterations. The reason is already persisted in two existing columns — a
policy halt in `error_reason`, an idle stop as a `status_message` carrying the
shared `idle_limit` prefix — so no new column and no migration stand behind it.
A derived accessor computes a halt kind and reason from those two fields,
because both can be populated at once; an error halt wins over an idle stop,
being the more urgent explanation. All three agent read surfaces — `agent
inspect`, the `agent ps` rows, and the agent status endpoint — report it as
`halt_kind` (`error` or `idle_limit`) and `halt_reason`, and only when there is
a reason: with none, both keys are omitted entirely rather than emitted empty.
An agent-authored status line is never reported as a halt reason, since only
the idle prefix qualifies.

## Iteration tags

An iteration row is immutable evidence of an execution, so tags are not a column
on it. They are rows in `iteration_tags`, keyed by `(iteration_id, tag)`, with an
`ON DELETE CASCADE` reference to `iterations`. Retention pruning and agent
deletion therefore retire an iteration's tags with the iteration itself, and no
tag write can touch the iteration row.

A batch of tag changes over several iterations of one agent commits in one
transaction, and an id belonging to another agent fails the whole batch rather
than writing part of it. Tags are a property of the iteration, not of the agent
that applied them: who tagged an iteration is not recorded.

## Chats and participants

A chat is persisted state, not a view over the bus. `chats` holds one row per
chat — `id`, `kind`, `title`, the one transport `channel` it owns, an optional
`legacy_agent`, `created_at` and `archived_at` — and `chat_participants` holds
one row per principal in it, keyed by `(chat_id, principal)`, with its `role`,
`joined_at`, `read_ts` and `muted` flag. The `channel` column is unique, so two
chats can never address the same transport channel.

Ownership follows how the chat came to exist. The daemon owns the three chats
it provisions for every agent — `dm:<agent>`, `tasks:<agent>` and
`service:<agent>` — reconciling them at startup and at agent creation, which
makes them self-healing: a missing row is recreated, and the namespaces they
live in are refused to anyone else. A chat the customer creates is owned by the
customer: nothing reconciles it, and its lifetime is exactly its row.

Membership is durable and revocable, and its lifetime is not the delivery's.
Adding an agent participant materializes its locked subscription to the chat
channel; removing one sets `subscriptions.revoked_at` instead of deleting the
row, because `deliveries` joins `subscriptions` and a delete would make the
agent's already-created, unacknowledged deliveries vanish from its queue.
A revoked subscription is skipped by fan-out and still owns its past deliveries.

The invariant underneath all of it: **`messages` rows are never rewritten.**
No chat operation re-channels, edits or deletes a published message; only
[nightly retention](#nightly-backup-and-data-retention) deletes fully delivered
messages older than the retention period. That is
what lets pre-chats history stay readable without a data migration — a personal
chat carries `legacy_agent`, and its feed unions the chat channel with the old
projection of `agent:<a>:inbox` and `user:<customer>` merged by time — and it is
why a chat can be created, joined and left without risking history.

`messages.sender` is the one derived column: the effective sender (`data.from`,
else a principal-shaped `source`, else `produced_by_agent`, else `system`),
stored in the same transaction that inserts the message and indexed with
`channel` and `ts`. Migration `0048_message_sender.sql` backfilled it for
existing rows with the same expression. It never changes after the insert, and
chat reads filter on it instead of parsing `data`.

## Nightly backup and data retention

Maintenance settings are one JSON value under `maintenance` in `daemon_config`;
a missing value means the defaults: enabled, 03:00 daemon-host local time, 7
backups, 90 days of retention, compaction on at 10% free pages. The last run's
result (times, trigger, backup path, rows deleted per category, logical size
before and after, compaction, error) is stored under `maintenance_last_run`.
Operators change both with **Settings → General → Backup & data retention**,
`tariboy maintenance get|set|run`, or `/api/maintenance`.

A run, scheduled or manual, is serialized and always starts with a backup:
`VACUUM INTO` writes a consistent copy while the daemon keeps serving, to
`<base-dir>/backups/db/tariboyd-<UTC time>.db`, and only the newest configured
number of those exact names are kept; other files in the directory are never
rotated. **If the backup fails, nothing is deleted.** Cleanup then
removes data older than the retention period, each category in its own
transaction:

- **Native Tasks** — only whole trees whose every task is `done` or
  `cancelled` with `completed_at` before the cutoff, with their comments,
  waits, events, notifications and read state, relations, and outbox rows. A tree related to a task that stays is kept.
  After the deletion commits, each deleted task's `<base-dir>/tasks/<KEY>/`
  directory is removed; a key that is not a single path element is skipped.
- **AI proxy Usage** — `ai_requests` rows. Retention is `0` (keep everything)
  or at least 31 days, so the calendar-month budget window never loses rows,
  and rows inside the longest rolling `budgets` period are kept even when it
  exceeds the retention period. `tariboy daemon reindex` can restore deleted
  rows from retained transcripts; the next run deletes them again.
- **Messages** — messages whose every delivery is acknowledged or
  dead-lettered, with those deliveries. Unacknowledged work, and messages that
  queue-trigger ingress has not consumed yet, are never deleted.
  This is the only path that removes `messages` rows; old chat history
  therefore ends at the retention period.
- **Daemon events** (`events`) and task mutation idempotency records.

When compaction is on and at least the threshold share of pages is free, a
full `VACUUM` plus `wal_checkpoint(TRUNCATE)` returns the space to the file
system. The daemon uses one SQLite connection, so every other database request
waits for the few seconds this takes, and the disk needs free space about the
size of the database. A failed `VACUUM` is rolled back by SQLite and recorded in
the run result. The scheduler checks the wall clock every minute, so the slot
holds across system sleep; a slot noticed more than 15 minutes late — the
daemon was down or the machine asleep — is skipped, not caught up.

To restore, stop the daemon, replace `tariboyd.db` with a backup file (and
remove any `tariboyd.db-wal` and `tariboyd.db-shm`), then start it again.

The per-agent iteration pruner (`tariboy retention`) is separate and unchanged.

## Customer identity and read marks

`customer_login` in `daemon_config` is the fixed customer login, defaulted to
`customer` and adopted once — with existing tasks, comments, waits, notification
state and the old `user:<$USER>` channel carried over in the same transaction —
instead of following whichever account runs `tariboyd`. Read marks are
`chat_participants.read_ts`, one per participant, and only ever move forward;
the single pre-chats `chat_read_v1` JSON object in `daemon_config` was carried
into those rows by the chats migration. A read mark is UI state rather than
authority: losing one costs an unread count, not a message.

## Message queue state

Queue saturation is derived rather than persisted. The agent status endpoint
counts distinct message IDs across that agent's unacknowledged, non-DLQ
deliveries and returns `messages_pending`, the configured
`messages_max_queue`, and `messages_queue_full`. The flag is true when the
pending count is at or above the limit. Publish limiting, status, and physical
queue clearing use the same delivery predicate so the UI never maintains a
second estimate.

## Native task state

Each agent row persists `goal_enabled` (default true), a positive
`goal_wait_customer_timeout_s` (default 300), a positive
`goal_delivery_cooldown_s` (default 60), daemon-owned
`last_goal_delivery_at`, and its read-only `current_goal_task_key`. Disabling
Goal clears the selected key; re-enabling it selects from current task state
rather than restoring an old choice. The goal reconciler normally owns
selection; an agent may explicitly select an active task assigned to itself
during a live iteration. Manually blocked tasks and tasks with an active
incoming `blocks` relation are not eligible; a selected task is released if it
becomes blocked. Only the reconciler writes the delivery timestamp.

Task queues, the unlimited parent tree, comments, waits, relations, events,
notification outbox, customer notification state, and mutation idempotency
records are normalized tables in the same `tariboyd.db`. A task key is its
queue prefix plus four random characters (`TEST-fkt3`), minted against the
`task_key` UNIQUE constraint rather than a counter, and it never changes when a
task moves between parents or between daemons. `task_key_aliases` holds the
numeric keys retired by that migration so they keep resolving; the daemon
rewrites any remaining numeric key on its first start after the upgrade, in one
transaction that also rewrites `agents.current_goal_task_key` and records the
rewrite in `task_legacy_keys_migrated`. Later starts skip it, because a random
suffix can be all digits too and must not be mistaken for a legacy key.
`task_queues.next_number` is left in the schema but is no longer read or
advanced. After that migration the daemon
inserts the default `TASK` queue when no queue has that prefix. Recursive
CTEs derive descendants, inherited access, blocking cycles, and active
descendants without a configured depth limit.

Priority is persisted as a constrained `P0` through `P3` value and defaults to
`P2`. Every root or nested sibling set has canonical order `(priority,
position, task key)`. Manual positions are normalized within a priority bucket,
so reparenting preserves priority without disturbing other buckets.

`started_at` records the first time a task's status became `in_progress` and
never moves afterward: a task paused in `wait_customer`, reopened, or started
again keeps it. The `tasks_started_at` trigger stamps it on every status write
path — update, claim, and a resolved customer wait — with
that write's `updated_at`. Migration `0049` backfilled existing tasks from the
earliest `task.updated` event with status `in_progress`, or `task.claimed`
still in `task_events`; a task with no such event keeps
an empty value until its next real move into `in_progress`, rather than taking
the time of an unrelated edit. Export and import carry the field.

Every task mutation, its event, and any notification intent commit in one
SQLite transaction. The channel publisher runs afterward from the durable
outbox with a stable message idempotency key. The WebSocket is only a delivery
optimization: `task_events.sequence` is the resume authority and HTTP queries
remain authoritative. See [Native Tasks](/docs/tasks).

Queues also own agent pools and queue triggers, and the daemon keeps a
persisted bus-ingress cursor that startup reconciles so a trigger never creates
a task twice. The earlier task workflow engine tables were dropped by migration
`0054`; tasks that had been managed became ordinary tasks and keep a
`workflow.removed` event that records what they ran on. The unused
`tasks.workflow_version_id` and `workflow_revision` columns and the empty
`task_workflow_versions` table stay in place, always `NULL` or empty, because
SQLite cannot drop a column with a foreign key without rebuilding `tasks`.

Migration `0055` adds the workflow image engine. `tasks.workflow_digest` pins
the image a task follows, set in the transaction that creates the task from the
queue's binding; the task is never migrated to another version, and the image
row (`task_workflow_images`) cannot be removed while a queue binds it or an
unfinished task pins it. `tasks.workflow_status` holds the workflow status
(and `tasks.workflow_paused_reason`, the pause reason, empty unless the task
is paused), while `tasks.status` keeps holding the category for every task.
Task JSON shows the
workflow status as `status` and the stored value as `category`; for a task
without a workflow both are the stored status. New tables: `task_queue_workflows`
(one binding per queue, with a revision and an `ON DELETE RESTRICT` reference to
the image), `task_workflow_holders` (the last holder per task and pool),
`task_status_visits`, `task_transition_requests` (at most one `pending` per
task), and `task_artifacts`. Workflow mutations append `workflow.transitioned`,
`workflow.transition_requested`, `workflow.moved`, `workflow.cancelled`, and
`artifact.set` task events, and queue bindings append `queue.workflow_bound` and
`queue.workflow_cleared`, in the same transaction as the change. See
[Task workflows](/docs/task-workflows).

Migration `0056` adds `task_queue_secrets`: per-queue secret values for
workflow scripts, in plaintext like agent secrets, deleted with their queue. No
read route returns a value; setting and removing a key append
`queue.secret_set` and `queue.secret_removed` with the key only. Migration
`0057` adds `task_script_runs`, the durable record of every check and watch run
(kind, script, mode, state, verdict, exit code, message, process id, times, and
log path), with a unique partial index that allows one `pending` or `running`
run per task, and `task_status_visits.next_watch_at`, when the open visit of a
script status runs its watch next. Migration `0058` adds
`task_script_runs.holder`, the agent a `run_as: agent` check ran as, which
decides who may read its log. Migration `0059` adds only indexes: runs by task,
visit, and request, the `pending` and `running` runs, and the open visits with a
scheduled watch. A check moves its transition request to `applied`,
`rejected`, or `failed` in the transaction that records the run; rejections and
failures increment the visit's `rejected_requests` and `script_failures`
counters. Script runs append `workflow.script_run`,
`workflow.transition_rejected`, `workflow.transition_failed`, and
`workflow.script_failed` events; a quiet watch run appends none, and only the
newest 20 quiet runs of a visit are kept. Closing a visit by any route cancels
its pending transition request. When the worker starts, before it starts any
run, it terminates a script a crashed daemon left running when `/proc` proves
the recorded PID is that run's script, then records every run still `running`
as `interrupted`. Run files live under `<base-dir>/tasks/<KEY>/` (`state/` and
`runs/<run-id>/`), owner-only; only the newest 20 quiet watch runs of a task
keep their run directories, counted by the running worker, and nightly
retention removes the directory of every task it deletes.

Migration `0060` adds `task_workflow_holders.released` (default `0`): the
customer's `release` decision sets it on the holder's row, which makes that
agent neither sticky nor eligible for the task in that pool until another agent
replaces the row or an operator move deletes it. Migration `0061` adds
`task_workflow_holders.unavailable_since` (default empty): when the holder was
first seen unable to work, cleared by every dispatch, which the daemon compares
with the `unavailable_grace` limit. `tasks.workflow_paused_reason` is now set: a
pause writes one of `idle_iterations`, `rejected_requests`, `script_failures`,
or `holder_unavailable`, together with category `wait_customer`, an open
customer wait authored by `system:workflow`, and a `workflow.paused` event; a
resume decision, an operator move, or a cancel clears it, and a decision or move
appends `workflow.resumed`. Both are written in the transaction that changes the
task. Migration `0062` adds an index on `task_transition_requests(visit_id,
created_at)`, which the idle-iteration count reads, and
`task_status_visits.resumed_at` (default empty): a resume decision writes the
time on the open visit, and an iteration that started before it does not count
as idle. See [Task workflows](/docs/task-workflows#pauses).

Migration `0063` adds `task_queue_source_runs`, the durable record of every run
of a workflow source (queue, source, script, image digest, state, verdict, exit
code, message, number of tasks created, process id, times, and log path), with a
unique partial index that allows one `running` run per queue and source, and
`task_queue_source_items`, every item key a source turned into a task, keyed by
queue, source, and key. A source run creates its tasks, their artifacts, and
their keys in the transaction that records it, so a crash never repeats a task;
keys are never deleted, so a removed task is not created again. A quiet run
leaves no event and only the newest 20 quiet runs of a source are kept; a failed
run appends the queue event `queue.source_failed`. At startup the worker
terminates a proven orphan of a source run as for a task run, then records
every `running` source run as `interrupted`. See
[Task workflows](/docs/task-workflows#sources).

## Image assignment state

User Store registrations belong to each daemon's SQLite database and persist
only their unique name and source. Git clones live under
`<base-dir>/stores/<name>/`; local absolute sources stay in place. Image names,
versions, and diagnostics are read from `images/` on disk for every view, never
stored as a second inventory. Store refresh and build preparation are
serialized so a pull cannot race source freezing. Store builds then use the
ordinary image snapshot and publication path below.

Workflow images live on disk under `<base-dir>/workflows/<name>/`. Content is
addressed by SHA-256 digest in `refs/<digest>/`, and tags are pointer files in
`tags/` that hold a digest. The `task_workflow_images` table records the
manifest of each published digest, so a manifest can be read from SQLite
without touching the filesystem; a unique index on name and version keeps one
digest per published version. The daemon reconciles the table with the stored
content at startup: it inserts missing rows and deletes rows whose content is
gone. See [Workflow images](/docs/workflow-images).

Directories left under `<base-dir>/store/versions/` by an older release are
untouched legacy data. Current daemons neither read nor refresh them.

Each agent row owns an active image ref/digest and a separate pending
ref/digest/error. Selecting another image changes only the pending fields. The
iteration launch gate validates and stages that artifact, reconciles image
capabilities, and promotes active plus pending state in one guarded transaction.
Every new iteration snapshots its image ref, source `image_version` when
present, digest, and prompt-template hash, so later switches cannot rewrite
historical execution identity. Historical iterations and legacy images keep an
empty version instead of inheriting the ref's current value.

The agent row remains authoritative during crash recovery. An incomplete local
image swap is either rolled back to the DB-active backup or completed when the
already-promoted image digest matches the row. Each unpacked image records the
ref it was materialized for and a daemon-owned id marker, so two generations of
one ref cannot be confused. Generations are kept by id under
`images/<name>/refs/`, so active and pending assignments survive a moved tag.
Harness, model, effort, environment, CWD, context, workdir, messages, history,
group, and subscriptions are not image assignment fields.

Image content is addressed by ref id — derived from the image name and the
source `image_version`, or from the archive bytes when no version is declared —
and a tag is a pointer file naming that id. One build publishes one ref and
moves every requested tag onto it, so a versioned tag and `latest` always agree.
The build records where those tags pointed, then commits every source snapshot
and provenance row in one SQLite transaction; a failure restores the tags and
deletes only content the build introduced, never a generation an agent may be
pinned to. There is no separate immutable mode and no publication journal:
rebuilding any non-reserved ref is always allowed, so no build path can be
refused for a ref that already exists. A store written before this model is
migrated once at daemon startup, keeping each archive's content digest as its
ref id so pinned digests keep resolving.
One publication gate spans ordinary, editable-source, team-import,
agent-authored, compose, and controlled-release publication plus assignment,
activation, provisioning, reprovisioning, import, and removal, so none can
persist an uncommitted generation. Compose publishes through the daemon rather
than writing its image store from the client process.

## Restart handoff

A running iteration is owned by `tariboy-shim`, not by the lifetime of the
daemon process. During a graceful daemon restart, cancellation detaches the old
engine observer without changing the durable iteration row from `running`.
Before any of that, the replacement daemon reconciles each stored agent's bin
shims against its active image (see
[Agent bin shims](/docs/architecture#agent-bin-shims)), so adopted work
continues with the launchers pinned by that image.
The replacement daemon enumerates live shim sockets before starting loop
engines, adopts each matching iteration, and finalizes the existing row when
`result.json` appears. It never launches a duplicate shim while adoption is in
progress.

Pending adoptions are also registered as current live iterations inside the
replacement manager. Terminal attach, resize, input, screen, and Kill RPCs
therefore continue to reach the original shim before the normal loop engine is
created. Start remains idempotent during this window, direct Exec cannot bypass
the adoption barrier, and post-adoption startup reloads the current persisted
Enabled value instead of using a pre-restart snapshot. Restart records one
replacement interactive-run intent until handoff completes, even when
Autopilot is disabled; the engine consumes that intent through a launch gate
shared with Stop, so a later Stop can cancel work that has not started. The gate
is acquired after external tmux checks and covers the durable launch or
manual-collision outcome. If Stop then races detached process creation, the
runner uses bounded cooperative shim Kill so the shim stops its owned harness
process group or tmux session. An unreachable interactive shim receives an
explicit tmux kill. The final managed fallback signals the spawned shim to
force-kill its separately owned non-interactive harness group, then escalates
against the outer shim group after a short bound. This cleanup is skipped when
the row is still `running` for daemon handoff. Shim connections are acquired
under the handoff lock with a finite dial timeout, preventing a pending control
operation from crossing into a replacement iteration when the stable socket
pathname is reused without allowing a wedged listener to block lifecycle
operations indefinitely.

The durable completion flag follows the same lifecycle on both sides of a
restart. Once an adopted iteration calls `i-am-done`, the replacement manager
allows the harness a two-second grace period to exit naturally, then sends the
shim exactly one cooperative Kill request if no `result.json` has appeared.
The shim writes the result and adoption finalizes the original row, preserving
the productive or `--idle` declaration recorded with the completion flag.

The AI-proxy endpoint and active per-iteration leases are carried through the
same restart, so harness retries reconnect after the short listener outage.
If the old shim is no longer reachable, adoption is the component that
terminally classifies the stale row as `harness_error`.

:::danger[This is why test daemons must be isolated]
Two daemons pointed at the same base dir share the same DB and loop managers and
will double-run each other's agents — double-executed iterations and reaped
sessions. Always isolate a test daemon with its own base dir, runtime dir, and
the web UI disabled. See [Development → Test isolation](/docs/development#test-isolation).
:::
