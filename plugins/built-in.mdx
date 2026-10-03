---
title: Built-in plugins
description: Reference for daemon-owned capabilities and instruction plugins shipped with Tariboy.
sidebar:
  label: All built-in plugins
  icon: list
---

Built-in plugins are implemented by Tariboy itself. Capability plugins gate
agent API routes, commands, or shims. Instruction-only plugins contribute no
tool surface and exist so a schema-v2 image can declare related static and
runtime prompt entries explicitly. Neither kind starts a supervised subprocess.

## Complete list

| Plugin | Class | In official `basic` | Agent surface | Durable state |
| --- | --- | :---: | --- | --- |
| [`whoami`](/docs/plugins/built-in/whoami) | Historical core | Yes | `scripts/whoami.sh` | None |
| [`loop`](/docs/plugins/built-in/loop) | Historical core | Yes | `i-am-done`, `scripts/loop.sh …` | Iteration and loop state |
| [`messages`](/docs/plugins/built-in/messages) | Historical core | Yes | `scripts/messages.sh message …`, `request`, channels | Bus messages, deliveries, subscriptions |
| [`context`](/docs/plugins/built-in/context) | Optional | Yes | `scripts/context.sh get/set` | Per-agent `CONTEXT.md` |
| [`status`](/docs/plugins/built-in/status) | Optional | Yes | `scripts/status.sh [set]` | Agent row and audit timeline |
| [`workdir`](/docs/plugins/built-in/workdir) | Instruction-only | Yes | Prompt only | None |
| [`goal`](/docs/plugins/built-in/goal) | Optional | Yes | `scripts/goal.sh set <TASK-KEY>` | Selected task and AI usage attribution on the iteration lease |
| [`schedule`](/docs/plugins/built-in/schedule) | Optional | No | `scripts/schedule.sh …` | Schedules and resulting bus messages |
| [`scripts`](/docs/plugins/built-in/scripts) | Optional | Yes | `scripts/scripts.sh …` | Script records and logs |
| [`image-creator`](/docs/plugins/built-in/image-creator) | Optional | No | `scripts/image_creator.sh build` | Built image in the host image store |
| [`tasks`](/docs/plugins/built-in/tasks) | Optional | Yes | Bare `tasks` command | Native Tasks in SQLite |

“Historical core” describes schema-v1 resolution, where `whoami`, `loop`, and
`messages` were added automatically. It does **not** mean schema-v2 images
receive them automatically.

## Enable a capability

Schema v2 accepts an explicit, ordered plugin list:

```yaml Tariboyfile.yaml
schema_version: 2
plugins:
  - name: whoami
  - name: loop
  - name: messages
  - name: context
  - name: workdir
skills:
  - dir: ../../skills/whoami
  - dir: ../../skills/loop
  - dir: ../../skills/messages
  - dir: ../../skills/context
  - dir: ../../skills/workdir
prompts:
  - runtime: identity
  - runtime: messages
  - runtime: context
  - runtime: workdir
  - runtime: user-prompt
  - file: ../../skills/loop/finish-iteration.md
```

Plugin order is preserved, duplicate names are rejected, and an unknown name is
accepted only when an installed external plugin manifest resolves it. Static
prompt files and runtime placeholders remain separate from the plugin list.

:::warning[Plugins do not inject schema-v2 prompts]
Declaring `context` enables its route but does not package its skill or add the
live context text. List the image skill and `runtime: context` explicitly when
the agent needs both. The same transparency rule applies to instruction-only
`workdir` and `goal`.
:::

## The default image

The official Store's `basic` source declares nine built-ins:

```text
whoami, loop, messages, context, status, workdir, scripts, goal, tasks
```

It deliberately excludes `schedule` and `image-creator`. Register and refresh
the Store, then build it. Existing agents
remain pinned to their assigned image digest until an image change is activated
for a future iteration.

## Capability enforcement

The per-agent API checks the agent's currently active plugin set on each gated
request. A missing capability returns `404 plugin_disabled`; it does not invoke
the underlying operation. Image activation rewrites image-owned shims before
promoting the new plugin set, so the bare `tasks` and `i-am-done` commands match
the active image.

## Prompt and state ownership

Canonical skills and the mandatory finish prompt live in the image source. A
schema-v2 image packages each enabled skill and chooses the
exact position of runtime values and mandatory prompt layers. Changing an image does not
delete daemon-owned agent data such as context, audit, messages, schedules,
scripts, or Tasks.

For the image schema and runtime marker list, see [Images](/docs/images). For
external process plugins, see [Plugins](/docs/plugins#external-plugins).
