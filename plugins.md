---
title: Plugins
description: Understand Tariboy's built-in capabilities and instructions and its separately installed external plugin processes.
sidebar:
  label: Overview
  icon: plug
---

Tariboy uses the word **plugin** for two related but operationally different
extension mechanisms. An image may name either kind, but their installation,
execution, and prompt behavior are not the same.

<CardGroup cols={2}>
  <Card title="Built-in plugins" href="/docs/plugins/built-in" icon="package-check">
    Daemon-owned capabilities such as context, messages, scripts, and Native
    Tasks and Goal, plus instruction-only plugins such as workdir. They do not run as
    separate plugin processes.
  </Card>
  <Card title="External plugins" href="#external-plugins" icon="unplug">
    Explicitly installed bundles that the daemon supervises as subprocesses.
  </Card>
</CardGroup>

## Choose the right model

| Property | Built-in plugin | External plugin |
| --- | --- | --- |
| Implementation | Tariboy daemon, agent API, bundled shim, or prompt instruction | Separately distributed executable |
| Installation | Ships with Tariboy | `tariboy plugin install <directory>` |
| Runtime process | No additional plugin process | One supervised process per active plugin name |
| Image declaration | `plugins: [{name: context}]` | The installed plugin name |
| Version selection | Tariboy release | Active installed version; images select by name |
| Prompt injection in schema v2 | Never implicit | Never implicit |
| Agent Skill packaging | Only when explicitly listed under image `skills` | Only when an explicit image skill directory resolves into the plugin tree |

Schema-v2 images receive exactly the plugin names they declare. Most built-ins
enable a capability; `workdir` is instruction-only. Neither class inserts
instructions into the prompt. List every static instruction and runtime marker
under the image's ordered `prompts` template. See [Images](/docs/images) for the
complete manifest contract.

Prompt assets and Agent Skills are separate declarations. A plugin directory
such as `$PLUGINS/jira/2.5.0/skills/triage` becomes a packaged Agent Skill only
when an image lists it under `skills: [{dir: ...}]`; placing content under a
plugin's own skills directory does not opt it in automatically. At runtime Tariboy
uses a generated local plugin for Claude Code, a bounded prompt catalog for
Codex CLI, and an isolated config overlay for OpenCode. These integrations are
additive to each harness's normal global and CWD discovery. Native duplicate
precedence remains harness-owned; the Codex image entry points to its exact
absolute `SKILL.md` path rather than replacing a native entry.

## External plugins

An external plugin is a subprocess the daemon spawns and supervises. It binds
an AF_UNIX socket, prints a one-line JSON handshake, then serves `/health` and
its type-specific HTTP endpoints.

### Plugin types

Types are defined in `internal/plugins`:

- **`channel-source`** publishes inbound events through
  `POST /api/plugin/publish`.
- **`channel-sink`** receives subscribed bus messages at `POST /deliver`, with
  durable redelivery and a dead-letter queue.
- **`tool`** advertises an agent-facing capability out of band.
- **`harness`** is recognized, but its runtime wiring is deferred.

### Providers and parameterized channels

A provider plugin offers a channel that an agent subscribes to with parameters,
such as a ticket identifier, query, or tick interval. The daemon validates the
parameters against the channel's `params_schema`, fingerprints them into a
**watch**, and drives the plugin through `/watches` push or
`GET /api/plugin/watches` pull. The plugin therefore produces only work that
has an active consumer.

An operator queue trigger can turn a provider event into a new task without
giving the plugin authority over any existing task. The committed bus message is
matched once and creates an ordinary Native Task. See [Task
workflows](/docs/task-workflows#queue-triggers).

### Installation and versioning

Install an external bundle from a daemon-accessible directory:

```bash
tariboy plugin install ./my-plugin
tariboy plugin ls
tariboy plugin inspect my-plugin
```

Third-party external plugins are optional and installed separately. A
first-party plugin may instead ship as a sibling Tariboy executable; the daemon
ensures its reserved manifest at startup through the same supervised-process
and versioned-directory path. Immutable versions live side by side under
`$PLUGINS/<name>/<version>`. Exactly one
version is active and one process is supervised per plugin name. Reinstalling
identical bytes is idempotent; different bytes at an existing name/version are
rejected.

An image selects an external capability by name, not by installed version.
Activating another installed version therefore changes what future launches of
that named plugin run without changing the image manifest. Keep any static
guidance explicit in the image template, normally with a `$PLUGINS/...` prompt
path.

### Operator CLI and settings contributions

An external manifest may declare `operator_commands` and `settings`. Command
paths are relative to the plugin name: a `telegram` manifest command named
`chat.setup` becomes `tariboy telegram chat setup`. The CLI obtains these
descriptors from the selected running daemon and invokes them through the
existing generic plugin action endpoint. Core commands remain available when
the daemon is down; contributed commands require the daemon that supplied
their schema. Secret command arguments are file-or-stdin inputs and are never
accepted as token values in argv.

Settings descriptors use only Tariboy's built-in string, password,
integer-list, status, and action controls. Desktop renders them under the
route-selected server's **Settings → Integrations** section. Passwords are
write-only. A plugin cannot inject JavaScript, HTML, CSS, React components,
routes, or assets into the CLI or Desktop process. The daemon validates command
names, field types, required values, action fields, and namespace ownership
before forwarding an action to the supervised plugin.

## Bundled Telegram plugin

`tariboy-plugin-telegram` is installed and enabled automatically beside each
packaged daemon. The operator must first create a Telegram supergroup, enable
topics, add the bot, and give the bot permission to manage topics. Bots cannot
create a group, enable forum mode, or grant administrator rights, and Tariboy
never changes user or bot permissions.

Configure the bot with an owner-only token file (or stdin), then bind the
existing forum supergroup:

```bash
tariboy telegram configure --token-file ./telegram-token --allowed-uids 123,456
tariboy telegram chat setup --chat-id -1001234567890
tariboy telegram status
```

The same fields and actions are available for the selected server under
**Settings → Integrations → Telegram**. The token is write-only. An empty
allowed UID list is deny-all: no message or command is accepted, including from
the configured chat, and discarded update offsets still advance so old
messages cannot run after access is enabled later. Allowed users do not need to
be chat administrators.

Setup accepts only a forum `supergroup` and verifies the bot can manage topics.
It creates a `tariboyd` management topic and one retained topic per current
agent; reconciliation creates topics for later agents. Deleting an agent does
not delete its topic. If a mapped topic is removed in Telegram, the next agent
reply creates and records a replacement.

The management topic supports:

```text
/help
/agents
/agent create NAME IMAGE
/agent show NAME
/agent set NAME FIELD VALUE
/start NAME
/stop NAME
/kill NAME
/tasks [NAME]
/task show KEY
/task create QUEUE TITLE
/task assign KEY AGENT
/task status KEY open|in_progress|done|cancelled
/task comment KEY TEXT
```

Each agent topic supports the same lifecycle and task operations with the
agent implied:

```text
/help
/start
/stop
/kill
/tasks
/task show KEY
/task create QUEUE TITLE
/task assign KEY
/task status KEY open|in_progress|done|cancelled
/task comment KEY TEXT
```

`/help` prints the complete list for its topic. `/agent set` permits only
alias, image, harness, model, effort, working directory, interactive mode, and
Autopilot settings; it cannot set environment variables or secrets. Ordinary
text in a live agent topic travels through `chat:telegram:<agent>` and the
existing channel/message delivery and acknowledgement path; its Telegram update
is marked read only after the mapped agent has a non-DLQ delivery. Private chats,
other groups, the general topic, anonymous posts, unknown topics, media,
reactions, edits, and webhooks are not supported.

## Related reference

- [Built-in plugins](/docs/plugins/built-in) lists every daemon-owned
  capability and its command surface.
- [Agent capability scripts](/docs/binaries/agent-tools) explains skill-local
  launchers and the `tasks` compatibility shim used inside an agent.
- [Channels](/docs/reference/channels) defines delivery, subscriptions,
  provider watches, schedules, and script results.
- [Command reference](/docs/reference/commands) lists operator and agent
  commands.
