---
title: Workflow images
description: Build, validate, store, and inspect versioned, immutable workflow images from a Workflowfile.yaml source directory.
sidebar:
  label: Workflow images
  icon: package
---

A workflow image is a versioned, immutable artifact built from a source
directory that holds a `Workflowfile.yaml`, status instructions, and scripts.
The manifest declares a status graph: who owns each status, which outcomes lead
to which next status, and which artifacts and checks a transition needs.

A queue binds a workflow image, and its tasks then follow the image's statuses;
see [Task workflows](/docs/task-workflows) for binding, the runtime, and the
script protocol.

Everything in the manifest drives queue tasks. `owner`,
`transitions`, `requires`, and `artifacts` shape the status graph; the daemon
runs `checks` when a holder requests a transition and runs each `watch` while a
task is in its script status; `requires_secrets` names queue secrets that must
be set before the image can be bound and that the scripts receive; `env` sets
non-secret variables for every script; `limits` set when the daemon pauses a
stalled task (see [Limits](/docs/task-workflows#limits)).

## Source layout

A [Store](/docs/images#stores-on-a-server) holds workflow sources next to image
sources. Any source directory works with `--path`.

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

The whole directory is the image: every regular file in it is stored, not only
the files the manifest names. A `.git` directory is skipped. A file named
`manifest.json` at the source root is rejected because that name is reserved
for the stored manifest.

## `Workflowfile.yaml`

The loader is strict: an unknown field is an error, and a second YAML document
is an error.

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
| `name` | yes | Workflow name; matches `^[a-z0-9][a-z0-9._-]{0,63}$`. |
| `workflow_version` | yes | SemVer 2.0.0 without a `v` prefix. |
| `initial_status` | yes | ID of a non-terminal status. |
| `statuses` | yes | The status graph; at least one status. |
| `artifacts` | no | Declared artifact names with descriptions. |
| `requires_secrets` | no | Names of secrets the scripts need; each matches `^[A-Za-z_][A-Za-z0-9_]*$`, must not start with `TARIBOY_`, and is listed once. |
| `env` | no | Non-secret environment defaults for scripts. A name matches `^[A-Za-z_][A-Za-z0-9_]*$` and must not start with `TARIBOY_`. |
| `limits` | no | Workflow-wide defaults; see [Limits](#limits). |
| `sources` | no | Queue-level scripts that create tasks; see [Sources](#sources). |

Status IDs, outcome names (`on`), artifact names, and pool names match
`^[a-z][a-z0-9_-]{0,63}$`.

### Status fields

| Field | Applies to | Meaning |
| --- | --- | --- |
| `id` | all | Unique status ID. |
| `owner` | non-terminal | `{pool: NAME}`, `customer`, or `script`. |
| `instructions` | pool, customer | Source-relative Markdown file shown to the owner. Not allowed on a `script` status. |
| `watch` | script | `script`, `every`, and `timeout` of the watch script. |
| `transitions` | non-terminal | List of `on`, `to`, and, for pool statuses, `requires` and `checks`. |
| `limits` | non-terminal | Per-status overrides of the workflow limits. |
| `terminal` | terminal | `true`. A terminal status has no owner, transitions, watch, instructions, or limits. |
| `cancelled` | terminal | `true` when the result counts as cancelled rather than done. Allowed only on a terminal status. |

A watch has a required `script`, a required `every` duration of at least `1s`,
and an optional positive `timeout` (default `60s`, maximum `30m`). The daemon
runs it once when a task enters the status, then `every` after each run
finishes.

A check has a required `script`, an optional `run_as` (`queue`, the default, or
`agent`), and an optional `timeout` (default `60s`, maximum `30m`). The checks of
a transition run in declared order when the holder requests it.

How scripts are run, their exit codes, result file, environment, and run modes
are described under [Script protocol](/docs/task-workflows#script-protocol).

### Sources

A source is a script the daemon runs on a schedule for every queue bound to the
workflow, outside any task. Each item it reports with a key the source has not
reported before becomes a task in the queue, in `initial_status`; the task then
moves through the statuses like any other. A reviewer that waits for pull
requests, an alert, an issue, or an email are examples.

```yaml
sources:
  - name: pull-requests
    script: ./scripts/incoming-prs.sh
    every: 2m
    timeout: 60s
```

| Field | Required | Meaning |
| --- | --- | --- |
| `name` | yes | Matches `^[a-z][a-z0-9_-]{0,63}$`; unique among the sources. It keys the items already seen, so keep it across versions. |
| `script` | yes | Source-relative executable; follows the [path rule](#paths). |
| `every` | yes | Duration of at least `10s` between the end of one run and the start of the next. |
| `timeout` | no | Positive duration, default `60s`, maximum `30m`. |

The result file, the environment, and the commands that show a source's runs
are described under [Sources](/docs/task-workflows#sources).

### Paths

A path in `instructions`, `watch.script`, `checks[].script`, or
`sources[].script`:

- starts with `./`, is not absolute, and stays inside the source directory;
- is not inside the top-level `.git` directory, which a build does not store;
- names a regular file; a symlink anywhere on the path is rejected;
- for a script, has an executable bit.

### Limits

| Limit | Default | Value |
| --- | --- | --- |
| `idle_iterations` | `3` | Positive integer. |
| `rejected_requests` | `5` | Positive integer. |
| `script_failures` | `3` | Positive integer. |
| `unavailable_grace` | `5m` | Non-negative duration such as `5m`; `0` is allowed. |

A status `limits` block overrides the workflow-wide block, which overrides the
defaults. The daemon counts idle iterations, rejected requests, and script
failures per visit of a status and pauses the task when a limit is reached; see
[Limits](/docs/task-workflows#limits) and [Pauses](/docs/task-workflows#pauses).

### Validation

`validate` and `build` report every independent error as an object with a
stable `code`, a `path` that uses YAML field names and indexes (for example
`statuses[2].transitions[0].to`), and a `message`. Errors are ordered by path,
then code.

| Code | Reported when |
| --- | --- |
| `schema_version_unsupported` | `schema_version` is not `1`. |
| `name_invalid` | `name` does not match the workflow name rule. |
| `version_invalid` | `workflow_version` is missing or not SemVer 2.0.0 without a `v`. |
| `secret_name_invalid` | A `requires_secrets` name is malformed, starts with `TARIBOY_`, or is repeated. |
| `env_name_invalid` | An `env` name is malformed or starts with `TARIBOY_`. |
| `limit_invalid` | A count limit is zero or negative. |
| `duration_invalid` | `unavailable_grace`, `watch.timeout`, a check `timeout`, or a source `timeout` is not a valid duration of the required sign, `watch.every` is not a duration of at least `1s`, or a source `every` is not a duration of at least `10s`. |
| `artifact_name_invalid` | An artifact name does not match the identifier rule. |
| `artifact_duplicate` | An artifact is declared twice. |
| `artifact_unknown` | A `requires` entry names an undeclared artifact. |
| `status_missing` | There are no statuses. |
| `status_id_invalid` | A status ID does not match the identifier rule. |
| `status_duplicate` | A status ID is used twice. |
| `initial_status_unknown` | `initial_status` names no status. |
| `initial_status_terminal` | `initial_status` is a terminal status. |
| `status_unreachable` | No path from `initial_status` reaches the status. |
| `terminal_unreachable` | No terminal status is reachable. |
| `terminal_has_work` | A terminal status has an owner, transitions, watch, instructions, or limits. |
| `cancelled_not_terminal` | `cancelled` is set on a non-terminal status. |
| `owner_missing` | A non-terminal status has no owner. |
| `owner_invalid` | A pool owner's name does not match the identifier rule. |
| `watch_missing` | A `script` status has no `watch`. |
| `watch_not_allowed` | A status that is not `script` has a `watch`. |
| `instructions_not_allowed` | A `script` status has `instructions`. |
| `transitions_missing` | A non-terminal status has no transition. |
| `outcome_invalid` | An outcome name does not match the identifier rule. |
| `outcome_duplicate` | An outcome is used twice in one status. |
| `transition_target_unknown` | A `to` names no status. |
| `requires_not_allowed` | `requires` appears on a transition out of a `customer` or `script` status. |
| `checks_not_allowed` | `checks` appear on a transition out of a `customer` or `script` status. |
| `run_as_invalid` | A check's `run_as` is neither `queue` nor `agent`. |
| `timeout_too_long` | A check `timeout`, `watch.timeout`, or source `timeout` exceeds `30m`. |
| `source_name_invalid` | A source name does not match the identifier rule. |
| `source_duplicate` | A source name is used twice. |
| `path_invalid` | A path breaks the [path rule](#paths). |
| `file_missing` | A named file does not exist or cannot be read. |
| `file_not_regular` | A named path, or a component of it, is a symlink or not a regular file. |
| `script_not_executable` | A named script has no executable bit. |
| `source_invalid` | The source directory itself is refused: a symlink or special file anywhere in it, a root `manifest.json`, or a file, file-count, or total-size limit exceeded. Checked only once the manifest has no error. `validate` reports it with `path` `source`; `build` refuses with `workflow_invalid` and the same message. |

`validate` runs the same checks as `build`, so a source `validate` accepts is
one `build` accepts, apart from a version already published with other content.
`tariboy workflow validate` prints the whole result and exits 1 when the source
is invalid, so a script can gate on it.

A manifest that cannot be read or decoded (including an unknown field) is
reported as `workflow_invalid` without a per-field list.

## Build, storage, and versions

`tariboy workflow build` validates the source and publishes it. The built
artifact is stored once, addressed by a SHA-256 digest over one line per
stored file, sorted by path: `<path>\0<executable 0|1>\0<SHA-256 of the
content>\n`. `Workflowfile.yaml` is one of those files, so the definition is
covered through its exact bytes, and the digest of an unchanged source does not
change between releases. Timestamps, ownership, and other permission bits do
not change the digest.

```text
<base-dir>/workflows/<name>/
  refs/<digest>/        copied source tree plus manifest.json
  tags/<tag>            file holding a digest
```

A build publishes two tags that point at the digest: the `workflow_version`
and `latest`. The stored tree is owner-only and read-only: directories are
`0500`, regular files `0400`, and executable files `0500`. `manifest.json`
holds the schema version, name, version, digest, build time, the definition
as parsed, and the path, SHA-256, executable flag, and size of every file.

A source may hold at most 256 files, at most 4 MiB per file, and at most 32 MiB
in total. Symlinks and special files anywhere in the source are rejected.

A published `workflow_version` is immutable. Rebuilding the same version with
identical content is a no-op and reports `created: false`. Rebuilding it with
different content fails with `workflow_version_published`. This is stricter
than agent images, where a rebuild moves the ref, because a workflow must stay
reproducible. To change a workflow, bump the version with
`tariboy workflow version update` and build again; `latest` then moves to the
new digest.

Removing the version tag does not free the version. While any tag, such as
`latest`, still names content of that version, a build of the same version with
different content fails with `workflow_version_published`. A version may be
reused only after its content is gone: every tag that named it is removed,
which deletes the content and its database row.

The manifest of every published digest is also recorded in the
`task_workflow_images` table of the daemon's SQLite database. Removing the
last tag of a digest deletes its row in a transaction that commits only after
the content is deleted. At startup the daemon inserts any missing row for
stored content and deletes any row whose content is gone, unless a queue binds
or a task pins its digest, so a crash between a disk write and its row heals. See [State model](/docs/architecture/state-model).

## Commands

The daemon commands act on the selected daemon. `STORE/NAME` selects a
workflow source in a registered Store; `--path` selects a `Workflowfile.yaml` or
its directory. A manifest file under any other name is refused. The two are
mutually exclusive.

```bash
tariboy workflow validate --path DIR
tariboy workflow validate team/development
tariboy workflow build --path DIR
tariboy workflow build team/development
tariboy workflow ls
tariboy workflow inspect development
tariboy workflow inspect development 0.1.0
tariboy workflow rm development 0.1.0
tariboy workflow version get [--path DIR]
tariboy workflow version update major|minor|patch [--path DIR]
```

| Command | HTTP route | Result |
| --- | --- | --- |
| `workflow validate` | `POST /api/workflow-images/validate` | `valid`, `name`, `version`, `pools`, `files`, and `errors` (empty when valid). Validation errors are returned in the result, not as an HTTP failure; the CLI prints them and exits 1. |
| `workflow build` | `POST /api/workflow-images/build` | `name`, `version`, `digest`, `tags`, and `created`. |
| `workflow ls` | `GET /api/workflow-images` | `workflows` (one row per tag with `name`, `tag`, `version`, `digest`, `built_at`) and `count`. |
| `workflow inspect NAME [TAG]` | `GET /api/workflow-images/{name}/{tag}` | The stored manifest. `TAG` is a tag or a full digest and defaults to `latest`. |
| `workflow rm NAME TAG` | `DELETE /api/workflow-images/{name}/{tag}` | `name`, `tag`, `removed`, and `content_removed`. Removing the last tag that names a digest also deletes its content and database row. |
| (Desktop) | `GET /api/workflow-images/{name}/{tag}/export` | A portable archive of the image; see [Copying to another server](#copying-to-another-server). |
| (Desktop) | `POST /api/workflow-image-imports` | Imports such an archive; the same result as `workflow build`. |

Removing the last tag that names a digest is refused with `workflow_in_use`
(HTTP 409) while a queue is bound to that digest or any task, open or closed,
is pinned to it; the error says which holds it and how many. Removing one of
several tags is not guarded, because the content stays and tasks keep running
from it. Unbind the queue (`ttasks queue workflow clear`) and finish or cancel
its tasks first. A closed task keeps reading its workflow and artifacts through
the image, so the image stays until nightly task retention removes the closed
task trees that pin it.

`workflow version get` and `workflow version update` read and rewrite the local
`workflow_version`, default to the current directory, and need no daemon.
`update` increments the SemVer component, resets the lower ones, removes any
suffix, and preserves the rest of the file.

Errors:

| Code | Meaning |
| --- | --- |
| `workflow_invalid` | The manifest failed validation or could not be parsed, or the source breaks a source limit. Validation failures carry the error list in `error.details.errors`. |
| `workflow_version_published` | The `workflow_version` is already published with different content. |
| `workflow_in_use` | `workflow rm` would delete a digest that a queue binds or any task, open or closed, pins. |
| `not_found` | No workflow image has that name and tag or digest. |
| `bad_source` | Both a Store selector and `--path` were given. |
| `missing_path` | Neither a Store selector nor `--path` was given. |
| `bad_archive` | An import is not a readable `workflow-image` portable archive. |
| `archive_too_large` | An import has no `Content-Length` or is larger than 64 MiB. |
| `workflow_digest_mismatch` | An imported archive does not hold the content its digest names. |

## Copying to another server

An image moves between daemons as content, not as tags.
`GET /api/workflow-images/{name}/{tag}/export` takes a tag or a full digest
and returns `tariboy-workflow-image.tar.gz`: a portable archive of kind
`workflow-image` holding every stored file (not `manifest.json`), with the
name, version, digest, and the paths of the executable files in its metadata,
because the archive format does not keep permission bits.

`POST /api/workflow-image-imports` takes that archive as the request body,
with a `Content-Length` of at most 64 MiB. The daemon stages it under
`<base-dir>/workflow-image-imports/`, which the archive limits and path checks
of the portable format guard, restores the executable bits, and publishes it
like `workflow build`: the same validation, the version tag and `latest` point
at it, and `latest` moves if it named other content. Before anything is
written it computes the digest of the staged files and refuses the import with
`workflow_digest_mismatch` when it differs from the one the archive names. The
staged copy is always removed. The result is the `workflow build` result; an
archive whose digest the daemon already holds reports `created: false`. A
version already published there with other content is refused with
`workflow_version_published`. `built_at` on the receiving daemon is the import
time, and queue bindings do not move.

## Desktop

Each server's workspace has a **Workflow images** section beside **Images**
(`/servers/:hostId/workflows`). Every request goes to that server.

- **Build from directory** takes a `Workflowfile.yaml`, or the directory
  holding it, on that server. **Validate** shows the name, version, pools, and
  files, or every validation error as `code · path: message`. **Build**
  reports `built NAME:VERSION`, or `NAME:VERSION already built` when nothing
  changed, and lists the errors of a refused build; a version already
  published with other content suggests bumping it.
- The list groups tags by name, with `latest` first and then the other tags
  newest version first, and shows the version, the first 12 characters of the
  digest (the full digest on hover), the build time, and **Bound to**: the
  queues bound to that digest. A queue whose binding cannot be read adds
  `unknown`, with the queue named on hover; the rest of the page still works.
- **Upload to servers** copies the image to other ready servers through the
  export and import routes: the archive is downloaded once and imported on
  each selected server in turn. Each server reports **Completed**, **Already
  present**, or **Failed** with the daemon's message, and a failure does not
  stop the others. Cancelling skips the servers not yet started.
- A tag's page (`/servers/:hostId/workflows/:name/:tag`, where the tag may also
  be a full digest) shows the status graph as a table (owner, instruction
  path, outcomes with their required artifacts and checks with `run_as` and
  timeout, watch), the effective [limits](#limits) of the workflow and of each
  non-terminal status with where each value comes from, the declared
  artifacts, required secrets, `env`, and the stored files. Instruction files
  are shown by path only. **Remove tag** asks for confirmation; a
  `workflow_in_use` refusal is shown in place with the bound queues. A page
  opened by digest has no **Remove tag**.

Building from a Store selector stays in the CLI and Compose.

## Official workflow images

A Store holds workflow sources at `workflows/<name>/` beside its `images/`.
The official Store provides two:

- `development`: the pull request flow with the statuses `plan`, `approval`,
  `implement`, `review` (watched by a script), `complete`, and `done`. It
  requires the `GH_TOKEN` queue secret and a pool named `developers`.
- `research`: one pool status, `research`, for a pool named `researchers`; the
  agent stores a `report` artifact and the task is `done`.

The graphs are in [Task workflows](/docs/task-workflows#official-workflows).
To use `development` on a queue `DEV`:

```bash
tariboy store refresh official
tariboy workflow build official/development
ttasks queue pool set DEV developers --agents dev-1,dev-2 --revision 0 --idempotency-key dev-pool
printf '%s' "$GH_TOKEN" | ttasks queue secret set DEV GH_TOKEN
ttasks queue workflow set DEV development:0.1.0 --revision 0
```

The secret is read from standard input so it never shows in the process list.
The bind is refused with `workflow_pool_empty` or `workflow_secret_missing`
until the pool and the secret exist. For `research`, build `official/research`,
create a `researchers` pool, and bind `research:0.1.0`; it needs no secret and
runs on any image that enables the `tasks` plugin, such as `official/basic`.

The queue secret and the agents' tokens are independent. The queue secret
`GH_TOKEN` is what the workflow's scripts (`pr-monitor.py`, `pr-open.py`) read.
It needs read access to pull requests, issues, checks, and commit statuses of
the repository. Each agent of the `developers` pool needs its own `GH_TOKEN`
agent secret as well: the skill's `ensure` and `preflight` commands run in the
agent's environment and use the agent's token, which also creates the pull
request. Only github.com is supported.

Every agent of the `developers` pool must:

- run the image `official/tariboy-developer`;
- have its own `GH_TOKEN` agent secret, as above;
- have git push credentials for the repository;
- have the repository checkout as its working directory;
- have the loop and the Goal block enabled, because the Goal block carries the
  status instructions.

## Using an image

A published image is bound to a queue with `ttasks queue workflow set`, or from
Compose with `task_queues.<PREFIX>.workflow`; tasks created in that queue pin
the version and follow its statuses. See [Task workflows](/docs/task-workflows)
for binding, queue secrets, the [Desktop](/docs/task-workflows#desktop) panel,
dispatch, artifacts, outcomes, scripts, and the operator commands, and
[Compose](/docs/binaries/compose#queue-workflows) for the Compose form.
