---
title: Binaries & commands
description: Core binaries and the Telegram plugin built by make build.
sidebar:
  label: Overview
  icon: terminal
---

Six real binaries are built by `make build`:

| Binary | Role |
| --- | --- |
| `tariboyd` | The [daemon](/docs/architecture) — long-running, owns all durable state. |
| `tariboy` | The [operator CLI](/docs/binaries/operator-cli) — human/CI client over the socket. |
| `tariboy-tasks` | The Native Tasks client; installers also provide its `ttasks` alias. |
| `tariboy-shim` | Runs [one harness iteration under a watchdog](/docs/architecture/shim). |
| `tariboy-store` | A standalone [image registry](/docs/binaries/store) server. |
| `tariboy-plugin-telegram` | The bundled Telegram channel and operator-surface plugin. |

The daemon installs or refreshes the Telegram plugin from its sibling binary at
startup. Optional external plugins are built and distributed independently.

`make build` does not embed agent image sources or skills. Register a Store such
as `git@github.com:alekzonder/tariboy-store.git`, refresh it, and build the
desired image on each daemon that needs it. The daemon synthesizes only the
reserved empty `bare:latest` image.

## Client/daemon version drift

`tariboy` and the [agent tool scripts](/docs/binaries/agent-tools) read the
`X-Tariboy-Version` header the daemon stamps on
[every response](/docs/architecture#version-reporting). When that version differs
from the client's own build, the client prints a warning naming both versions and
its own path, strictly on **stderr**. It does not
change stdout or the exit code, so output parsing is unaffected. A daemon old
enough to send no header produces no warning.

This matters because packaged skill scripts and the agent `tasks` / `i-am-done`
compatibility shims execute from the active image bridge: without the warning,
a script too old to know a newer flag looks like it simply did nothing.
`scripts/whoami.sh` prints both `client_version` and `daemon_version` for exactly this
reason — it is the first command to run when the tools behave strangely.

The authoritative operator command list is generated from the binary
(`tariboy --help-json`) and documented in the
[command reference](/docs/reference/commands).

Queue agent pools and triggers are configured with the operator `ttasks` client
or operator REST, and pools also through [`tariboy compose`](/docs/binaries/compose).
Workflow images are built and stored with `tariboy workflow`; see
[Workflow images](/docs/workflow-images) and
[Task workflows](/docs/task-workflows).

## The three command surfaces

<CardGroup>
  <Card title="Operator commands" href="/docs/binaries/operator-cli" icon="user-cog">
    `tariboy <group> <command>` — run by a human or CI against the daemon.
  </Card>
  <Card title="Agent capability scripts" href="/docs/binaries/agent-tools" icon="wrench">
    Packaged skill-local scripts — run *inside* an agent over its per-agent socket.
  </Card>
  <Card title="Native Tasks" href="/docs/tasks" icon="list-tree">
    `ttasks <verb>` — Native Tasks for operators and identity-bound agents.
  </Card>
</CardGroup>
