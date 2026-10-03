---
title: tariboy (operator CLI)
description: The human/CI client that talks to the daemon over its socket, grouped by noun.
sidebar:
  label: Operator CLI
  icon: user-cog
---

The human/CI client (`cmd/tariboy`) talks to the daemon over its socket.
Commands are grouped by noun. The full, authoritative list — generated from the
binary — is in the [command reference](/docs/reference/commands).

`daemon` subcommands (`start` / `stop` / `restart` / `status` / `logs`) work even
when the daemon is down.

| Group | What it does |
| --- | --- |
| `version` | print the canonical Tariboy version locally, without a daemon |
| `daemon` | `start` / `stop` / `restart` / `status` / `logs`, `config get`/`set`, `reindex` |
| `image` | `build`, `ls`, `inspect`, `prompt`, `rm` |
| `agent` | `run`, `ps`, `inspect`, `start`, `stop`, `restart`, `kill`, `rm`, `exec`, `screen`, `send-keys`, `status show`/`history`, `pull` |
| `cp` | Upload `LOCAL_FILE` to the shared server directory, or download `AGENT:SRC LOCAL_DST` |
| `files` | `upload` base64 content to the shared server directory |
| `loop` | `enable`, `disable`, `interval`, `timeout`, `hard-timeout`, `on-timeout`, `on-error` |
| `channel` / `message` | `channel ls`/`inspect`/`tail`, `message send` |
| `group` | `create`, `assign`, `inspect`, `ls`, `rm` |
| `plugin` | `install`, `ls`, `inspect`, `logs`, `rm` |
| `telegram` | contributed by the running bundled plugin: `configure`, `chat setup`, `status` |
| `schedule` | `ls` |
| `tasks queue create` | Create a native task queue with owners and an optional responsible agent |
| `secret` | `set`, `ls`, `rm` |
| `budget` | `set`, `ls`, `status` |
| `usage` / `logs` / `iteration` | AI usage/cost, event stream, iteration inspect/logs/ls |
| `retention` / `prune` | retention policy, prune old iterations |
| `backup` / `restore` | portable per-agent `tar.gz` |
| `user-prompt` | get/set the agent's standing user prompt |
| registry: `push` / `pull` / `login` | interact with a [`tariboy-store`](/docs/binaries/store) registry |

See the [full command reference](/docs/reference/commands#operator-commands) for
every command and a one-line summary of each.

`tariboy version` and `tariboy --version` are equivalent plain-text
forms. Both work without a running daemon.

Plugin command groups are declarative and namespaced. The CLI merges validated
contributions from the selected running daemon; core help remains usable when
that daemon is unavailable, while contributed commands require it. Secret
arguments are accepted only through an owner-only file or stdin, never as a
token value in argv. See the [Telegram setup workflow](/docs/plugins#bundled-telegram-plugin).

Queue pools and triggers are managed with the operator form of `ttasks queue
pool` and `ttasks queue trigger`, or through the matching REST routes; Compose
is the declarative client for pools and the generated OpenAPI describes the raw
API. Workflow images are managed with `tariboy workflow`. See
[Task workflows](/docs/task-workflows) and [Workflow images](/docs/workflow-images).

Agent create and update accept `--goal-enabled` and
`--goal-wait-customer-timeout-s`; their defaults are enabled and 300 seconds.
Inspect and list output also reports the daemon-selected read-only
`current_goal_task_key`.

## File transfer

`tariboy cp LOCAL_FILE` streams a local file through `PUT /api/files/raw` and
prints the returned absolute server path. Files are stored beneath
`$TARIBOY_BASE_DIR/files`, with a unique directory for each upload and a
1 GiB per-file limit. Agents on that server can read the same uploaded file;
uploading does not require an agent or write into an agent's working directory.

The lower-level `tariboy files upload` command remains a JSON/base64
compatibility surface with a 16 MiB decoded-file limit.

`tariboy cp AGENT:SRC LOCAL_DST` still downloads from the agent's working
directory. The former upload syntax `cp SRC AGENT:DST` and `agent push`
have been removed; use `cp LOCAL_FILE` and the returned shared path instead.
