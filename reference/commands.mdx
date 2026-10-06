---
title: Command reference
description: The full command reference — operator commands and agent capability scripts.
sidebar:
  label: Commands
  icon: terminal
---

# tariboy command reference

tariboy has three command surfaces:

1. **Operator commands** — `tariboy <group> <command>` (`sa` below is a
   convenience alias you define yourself, e.g. `alias sa=tariboy`; the build
   ships no such symlink), run by a
   human or CI against the daemon. Global flags: `--json`, `--help`,
   `--help-json`, `--version`. This is the authoritative list, generated from the
   binary's registry (`tariboy --help-json`).
2. **Agent capability scripts** — the applicable packaged skill's
   `scripts/*.sh` launcher, run *inside* an agent over `$TARIBOY_TOOLS_SOCKET`.
3. **Native Tasks** — `tariboy-tasks`, installed as `ttasks`, runs shared task
   verbs for an operator or an identity-bound agent. `ttasks --version` reports
   its build and `ttasks --json …` requests JSON output.

## Operator commands

| Command | Summary |
| --- | --- |
| `tariboy agent exec` | Run a manual iteration now, with an optional one-shot prompt |
| `tariboy agent inspect` | Show one agent's full config |
| `tariboy agent kill` | Kill the current iteration via its shim |
| `tariboy agent ps` | List agents |
| `tariboy agent pull` | Read a base64 file from an agent's cwd (used by 'cp') |
| `tariboy agent restart` | Restart an agent and run one iteration now |
| `tariboy agent rm` | Remove an agent (stop it first, or --force) |
| `tariboy agent run` | Create and start an agent from an image (image:tag) |
| `tariboy agent screen` | Capture the interactive screen (tmux capture-pane) |
| `tariboy agent send-keys` | Send keys into the interactive session (tmux send-keys) |
| `tariboy agent start` | Start (enable) an agent's loop |
| `tariboy agent status history` | List newest-first status messages from the audit log, including each event's iteration ID when present |
| `tariboy agent status show` | Show one agent's runtime status |
| `tariboy agent stop` | Stop an agent's loop (current iteration lives) |
| `tariboy backup` | Back up an agent (or 'all') to a portable tar.gz |
| `tariboy budget ls` | List configured budgets |
| `tariboy budget set` | Set a cost budget (scope agent:\<name\>\|group:\<g\>\|global) |
| `tariboy budget status` | Show current spend vs limit for each budget |
| `tariboy channel inspect` | Show a channel's kind and message count |
| `tariboy channel ls` | List channels |
| `tariboy channel tail` | Print recent messages on a channel (-f to follow) |
| `tariboy compose archive` | Create a compose-only portable team archive; transfer runnable images separately |
| `tariboy compose import` | Preview and import compose-only team/runtime configuration |
| `tariboy files upload` | Upload up to 16 MiB of base64 content to the server and return its absolute path |
| `tariboy cp` | Stream a file up to 1 GiB to the server: cp LOCAL_FILE; download: cp AGENT:SRC LOCAL_DST |
| `tariboy daemon config get` | Read daemon config (all keys, or one with --key) |
| `tariboy daemon config set` | Set a daemon config key (runtime-mutable) |
| `tariboy daemon reindex` | Rebuild ai_requests metadata from proxy-transcript.jsonl files |
| `tariboy daemon status` | Show daemon version, uptime and base directory |
| `tariboy group assign` | Assign an agent to a group (empty group leaves) |
| `tariboy group create` | Create (or update) a group with an optional lead |
| `tariboy group inspect` | Show a group's lead, members, channels and shared dir |
| `tariboy group ls` | List groups (name/lead/member count) |
| `tariboy group rm` | Remove a group (detach members, delete channels; --volumes drops the shared dir) |
| `tariboy image build --path DIR --name NAME [--tag TAG] [--repository-id ID --git-commit SHA]` | Build one ref and move every requested tag onto it, plus a frozen source snapshot; repeat explicit tags, or omit them to publish image_version plus latest (only latest when unversioned); an existing tag is moved, not refused; Git provenance must be paired |
| `tariboy image build STORE/IMAGE [--name NAME] [--tag TAG]` | Restore available skill locks and build from the selected daemon's Store; default name is IMAGE and an omitted tag publishes image_version plus latest, or only latest when unversioned |
| `tariboy image validate --path DIR --name NAME [--tag TAG]` | Validate the source and target ref without publishing; tag defaults to `latest`; prints the result and exits 1 when it is invalid |
| `tariboy image version get [--path FILE_OR_DIR]` | Print the local image_version; defaults to ./Tariboyfile.yaml; no daemon required |
| `tariboy image version update <major\|minor\|patch> [--path FILE_OR_DIR]` | Increment the local SemVer, reset lower components and remove suffixes; preserve YAML fields/comments |
| `tariboy image inspect` | Show an image manifest |
| `tariboy image ls` | List built agent images |
| `tariboy image prompt` | Print an image's assembled prompt |
| `tariboy image provenance REF` | Show local source and frozen snapshot Git provenance |
| `tariboy image template` | Show the ordered schema-v2 static/runtime template |
| `tariboy image rm` | Remove a built image |
| `tariboy iteration inspect` | Show one iteration, including its snapshotted image ref, source version, digest, prompt-template hash, and tags |
| `tariboy iteration logs` | Print an iteration's harness logs |
| `tariboy iteration ls` | List an agent's iterations; `--tag`, `--started-after` and `--started-before` narrow the list |
| `tariboy iteration tag add` | Add tags to one or more iterations |
| `tariboy iteration tag rm` | Remove tags from one or more iterations |
| `tariboy iteration tag set` | Replace the tags of one or more iterations |
| `tariboy logs` | Stream or print an agent's events (-f to follow) |
| `tariboy loop disable` | Turn the agent loop disable |
| `tariboy loop enable` | Turn the agent loop enable |
| `tariboy loop hard-timeout` | Get or set loop hard-timeout (seconds); omit value to read |
| `tariboy loop interval` | Get or set loop interval (seconds); omit value to read |
| `tariboy loop on-error` | Get or set loop on-error policy; omit value to read |
| `tariboy loop on-timeout` | Get or set loop on-timeout policy; omit value to read |
| `tariboy loop timeout` | Get or set loop timeout (seconds); omit value to read |
| `tariboy maintenance get` | Show database maintenance settings and the last run |
| `tariboy maintenance run` | Back up the database, delete expired data, and compact it now |
| `tariboy maintenance set` | Change database maintenance settings; unset flags keep their value |
| `tariboy message send` | Publish a message to a channel (operator) |
| `tariboy plugin inspect` | Show one plugin's manifest, state and socket |
| `tariboy plugin install` | Install and start a plugin from a directory with plugin.json |
| `tariboy plugin logs` | Show captured plugin stdout/stderr |
| `tariboy plugin ls` | List installed plugins (name/version/types/state/health) |
| `tariboy plugin rm` | Stop and remove a plugin |
| `tariboy prune` | Prune old iterations for an agent now (or 'all'); --dry-run lists victims |
| `tariboy restore` | Restore an agent from a backup tar.gz (optionally under a new name) |
| `tariboy retention get` | Show the effective retention policy for an agent (or 'default') |
| `tariboy retention set` | Set the retention policy for an agent (or 'default') |
| `tariboy schedule ls` | List an agent's schedules (read-only) |
| `tariboy secret ls` | List secret keys (values are never shown) |
| `tariboy secret rm` | Remove a secret |
| `tariboy secret set` | Set a secret; value from --value or stdin |
| `tariboy store add NAME SOURCE` | Register a Git URL or absolute local directory on the daemon host |
| `tariboy store list` | List this daemon's Store registrations |
| `tariboy store show NAME` | Read current images, versions and diagnostics from disk |
| `tariboy store refresh NAME` | Reset a managed Git clone to its upstream, discarding local changes; fast-forward pull a local Git checkout; or reread a non-Git local directory |
| `tariboy store auto NAME [--interval MINUTES] [--image IMAGE]` | Set the automatic refresh-and-build policy; the daemon refreshes every interval and rebuilds the selected images that need an update, and `--interval 0` disables it |
| `tariboy store remove NAME` | Unregister a Store and remove only its managed clone; preserve local sources and built images |
| `tariboy workflow validate [STORE/NAME] [--path DIR]` | Validate a workflow source directory without building it; reports every error with a stable code and path, and exits 1 when the source is invalid |
| `tariboy workflow build [STORE/NAME] [--path DIR]` | Validate and publish a workflow image under its `workflow_version` and `latest`; an identical rebuild is a no-op, different content for a published version fails with `workflow_version_published` |
| `tariboy workflow ls` | List built workflow images, one row per tag |
| `tariboy workflow inspect NAME [TAG]` | Show a workflow image manifest; TAG is a tag or full digest and defaults to `latest` |
| `tariboy workflow rm NAME TAG` | Remove one tag, and the stored content when no tag remains |
| `tariboy workflow version get [--path FILE_OR_DIR]` | Print the local workflow_version; defaults to the current directory; no daemon required |
| `tariboy workflow version update <major\|minor\|patch> [--path FILE_OR_DIR]` | Increment the local SemVer, reset lower components and remove suffixes; no daemon required |
| `ttasks queue create` | Create a task queue |
| `tariboy usage` | Aggregate AI usage and cost from ai_requests |
| `tariboy user-prompt get` | Read the agent's standing user-prompt |
| `tariboy user-prompt set` | Set the agent's standing user-prompt |
| `tariboy version` | Print the Tariboy version locally without a daemon |

> Regenerate after adding/removing a command: `make build && ./bin/tariboy --help-json`.

### Host-local commands

These verbs never route through the daemon, so they are absent from the
registry listing above. They act on the host the CLI runs on and work while the
daemon is down.

| Command | Summary |
| --- | --- |
| `tariboy daemon start\|stop\|restart\|status\|logs` | Control and inspect the local daemon process |
| `tariboy update [VERSION\|latest] [--force]` | Install a published release over this installer-managed installation and restart a running daemon |

`tariboy update` updates only an installer-managed `linux-x86_64` installation
and is described in [Remote hosts](/docs/remote-hosts#update-a-host-from-its-own-shell).

### Iteration tags

Tags annotate an iteration without changing it: one agent marks the iterations
of another agent it has already handled. An iteration can carry several tags.
A tag is at most 64 characters and contains no comma, whitespace, or control
character.

The three mutations take a batch of iteration ids and a batch of tags, and apply
in one transaction — the whole batch succeeds or nothing is written. An id that
belongs to another agent fails the batch with `not_found`.

```bash
tariboy iteration tag add worker --id worker-20260918T10-1 --id worker-20260918T11-2 --tag processed
tariboy iteration tag rm worker --id worker-20260918T10-1 --tag processed
tariboy iteration tag set worker --id worker-20260918T10-1 --tag processed --tag reviewed
tariboy iteration tag set worker --id worker-20260918T10-1   # no --tag clears every tag
```

`tariboy iteration ls` searches by tag and by iteration start date. `--tag`
takes a comma-separated list and matches an iteration carrying **any** of them.
Both date bounds are inclusive and accept RFC3339 or a bare `YYYY-MM-DD` day:

```bash
tariboy iteration ls worker --tag processed,reviewed
tariboy iteration ls worker --started-after 2026-09-01 --started-before 2026-09-18
```

`iteration ls` rows and `iteration inspect` both report a sorted `tags` list.

## Agent capability scripts

Run inside an agent; the socket comes from `$TARIBOY_TOOLS_SOCKET`.

| Tool | Purpose |
| --- | --- |
| `scripts/whoami.sh` | Print agent, cwd and current iteration |
| `scripts/status.sh` | Print the agent status |
| `scripts/loop.sh done` | Signal this iteration is finished (i-am-done) |
| `scripts/context.sh get` | Print the durable working memory (CONTEXT.md) |
| `scripts/context.sh set <text>` | Overwrite the durable working memory |
| `scripts/messages.sh message send --channel C [--type T] [--subject k=v,…] [--text … \| --data JSON]` | Publish a message to a channel |
| `scripts/messages.sh channel subscribe C [--matcher JSON] [--type globs]` | Subscribe to a channel |
| `scripts/messages.sh channel unsubscribe ID` | Remove a subscription |
| `scripts/messages.sh channel ls` | List your subscriptions |
| `scripts/messages.sh sources` | List available channels |
| `scripts/schedule.sh add --kind cron\|oneshot --spec S [--channel C] [--message JSON]` | Schedule a future wake-up |
| `scripts/schedule.sh ls` | List your schedules |
| `scripts/schedule.sh cancel ID` | Cancel a schedule |
| `scripts/scripts.sh ls` | List your scripts |
| `scripts/scripts.sh run NAME [--description TEXT] -- COMMAND` | Queue exactly one local run |
| `scripts/scripts.sh schedule NAME --every SECONDS -- COMMAND` | Run now and repeat after each completion; a run that exits `111` (`TARIBOY_QUIET_EXIT`) stays quiet |
| `scripts/scripts.sh runs SCRIPT_ID` / `logs RUN_ID` | Inspect run history and bounded logs |
| `scripts/scripts.sh rerun SCRIPT_ID` | Run a completed one-shot or idle recurring definition immediately |
| `scripts/scripts.sh cancel SCRIPT_OR_RUN_ID` / `rm SCRIPT_ID` | Cancel work or remove inactive history |
| `scripts/image_creator.sh build --name NAME [--tag TAG] --path DIR` | Build a schema-v2 image from a daemon-readable source directory (`image-creator` only); relative paths start at the managed workdir |

## Native Tasks (`ttasks …`)

`tariboy-tasks` is the real executable and `ttasks` its managed alias. A
non-empty `TARIBOY_TOOLS_SOCKET` selects agent mode; it uses only that socket
and fails closed if it cannot connect. Without the variable, the client uses
the host Unix daemon socket as the customer actor. The bare `tasks` command is
only the optional capability-controlled legacy agent shim.

These shared verbs are available in both modes; in agent mode every mutation
derives identity from the socket.

| Command | Purpose |
| --- | --- |
| `ttasks mine` | List visible tasks |
| `ttasks ready [--queue Q] [--claim]` | List ready work; `--claim` requires agent mode |
| `ttasks show KEY` | Show task detail, comments, waits, and relations |
| `ttasks create --queue Q --title T [--assignee A]` | Create a queue-root task |
| `ttasks create --parent KEY --title T` | Create a child inheriting queue/context |
| `ttasks update KEY [--title T] [--description D] [--status S]` | Update fields with optimistic revision |
| `ttasks assign KEY ASSIGNEE` | Hand work to an agent name |
| `ttasks comment KEY TEXT` | Add a task comment |
| `ttasks ask KEY agent:name\|user:login TEXT` | Mention a principal and record an open answer wait |
| `ttasks move KEY [--parent KEY] [--before KEY] [--to-root]` | Reparent/reorder in the same queue |
| `ttasks block KEY --by BLOCKER` | Add a directed, cycle-checked blocking relation |
| `ttasks relate KEY OTHER` | Add a symmetric related link |
| `ttasks done KEY [--complete-anyway]` | Complete, optionally overriding active descendants |

These verbs drive a task that follows a queue's workflow image. Agent mode
sends `advance`, `request_get`, `artifact_set`, `artifact_ls`, `artifact_show`,
`workflow_get`, `workflow_runs`, and `workflow_run_log` over the identity-bound
socket; operator mode calls the REST routes as the customer.

| Command | Modes | REST route |
| --- | --- | --- |
| `ttasks advance KEY --outcome NAME [--from STATUS] [--message TEXT] [--no-wait]` (`--from` refuses with `status_changed` when the task has moved on; waits for the transition's checks, polling `GET /api/tasks/{key}/workflow/requests/{id}`, unless `--no-wait`) | agent, operator | `POST /api/tasks/{key}/advance` |
| `ttasks artifacts set KEY NAME [VALUE \| --file PATH]` (stdin when absent; stored as given, up to 64 KiB) | agent, operator | `PUT /api/tasks/{key}/artifacts/{name}` |
| `ttasks artifacts ls KEY` | agent, operator | `GET /api/tasks/{key}/artifacts` |
| `ttasks artifacts show KEY NAME` | agent, operator | `GET /api/tasks/{key}/artifacts/{name}` |
| `ttasks workflow get KEY` | agent, operator | `GET /api/tasks/{key}/workflow` |
| `ttasks workflow runs KEY` (newest first) | agent, operator | `GET /api/tasks/{key}/workflow/runs` |
| `ttasks workflow log KEY RUN [--max-bytes N]` (last 64 KiB by default, at most 1 MiB; queue secrets redacted; customer and the task's holders only) | agent, operator | `GET /api/tasks/{key}/workflow/runs/{id}/log` |
| `ttasks workflow move KEY --to STATUS --reason TEXT` | operator only | `POST /api/tasks/{key}/workflow/move` |
| `ttasks workflow resume KEY --decision continue\|release` (resolves the pause of a task the daemon paused: `continue` keeps the holder and zeroes the counters; `release` takes a pool task from its holder and dispatches it to another member, or leaves it `open` and unassigned; errors `invalid_decision`, `workflow_not_paused`, `forbidden`) | operator only | `POST /api/tasks/{key}/workflow/resume` |
| `ttasks cancel KEY` | operator only | `POST /api/tasks/{key}/cancel` |
| `ttasks queue workflow set QUEUE REF [--revision N]` | operator only | `PUT /api/task-queues/{queue}/workflow` |
| `ttasks queue workflow get QUEUE` | operator only | `GET /api/task-queues/{queue}/workflow` |
| `ttasks queue workflow clear QUEUE --revision N` | operator only | `DELETE /api/task-queues/{queue}/workflow` |
| `ttasks queue secret set QUEUE KEY [--value V]` (stdin when absent, one trailing newline stripped; up to 64 KiB) | operator only | `PUT /api/task-queues/{queue}/secrets/{key}` |
| `ttasks queue secret ls QUEUE` (keys only, never values) | operator only | `GET /api/task-queues/{queue}/secrets` |
| `ttasks queue secret rm QUEUE KEY` | operator only | `DELETE /api/task-queues/{queue}/secrets/{key}` |
| `ttasks queue source ls QUEUE` | operator only | `GET /api/task-queues/{queue}/sources` |
| `ttasks queue source log QUEUE RUN [--max-bytes N]` (redacted tail, 64 KiB by default) | operator only | `GET /api/task-queues/{queue}/source-runs/{id}/log` |

On a workflow task `ttasks done`, `ttasks update --status`, and
`ttasks ready --claim` are refused with `workflow_managed`; the error lists the
status's outcomes and the `ttasks advance` form to use, with `--from` set to the current status. For an outcome that
declares checks, `ttasks advance` exits `0` when the transition applied and `1`
when a check rejected it, a check failed, the request was cancelled, or the wait
ran out; the output names the next command. `ttasks show` prints
`status` (the workflow status), `category`, and `waiting_on`. See
[Task workflows](/docs/task-workflows#script-protocol) for the scripts behind
checks and watch statuses.

The following administration roots are operator-only and are documented by
`ttasks --help-json`: `queue` (including pools and triggers; see
[Task workflows](/docs/task-workflows)), `events`, `principals`, and
`notifications`. They are unavailable to agent mode.

Resource identifiers are positional (or named flags), for example:

```bash
ttasks queue get OPS
ttasks queue update OPS --name Operations --revision 2
ttasks queue pool list OPS
ttasks notifications read 1
ttasks events OPS-1 --after 7 --limit 10
```

Use each administration command's `--help` for its required fields.
