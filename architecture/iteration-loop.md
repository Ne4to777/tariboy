---
title: Agents & the iteration loop
description: An agent runs a series of supervised iterations, driven by a timer or by messages, on top of a whoami / loop / channels core.
sidebar:
  label: Iteration loop
  icon: repeat
---

An **agent** is a configured, named instance of an image with its own working
directory and its own loop. It does not run continuously — it runs a series of
**iterations**. On each iteration the daemon:

1. prepares a prompt — a platform-owned **Task Processing Order** block groups
   one-shot instructions, messages (including awaiting replies), and Goal in
   that order. Schema v2 preserves the declared order of the remaining image
   layers; self-contained legacy schema-v1 artifacts use the same work block;
2. launches the harness (e.g. Claude) under the [shim](/docs/architecture/shim)
   for exactly one pass;
3. records the outcome, iteration logs, AI usage, and audit timeline.

## What drives the loop

The loop is driven two ways, both requiring the loop to be **enabled**:

- **Timer** — `interval_s`: wake every N seconds.
- **Message** — a publish that matches one of the agent's subscriptions nudges
  the loop to start an iteration now (only if pending deliveries exist).

For a workflow assignment, the wake is only scheduling. The running iteration
must atomically claim work with `tasks work next`; the returned lease-bound work
packet is the execution authority. Assignment completion or an allowed
observation reaction advances durable workflow state, not receipt of the wake.

Loop policy is per-agent: `interval`, `timeout` (soft), `hard-timeout`,
`on_timeout` and `on_error` (e.g. `restart`). An iteration ends when it signals
completion, exits, or reaches an enforced deadline.

### Agent Goals

Goal is per-agent and enabled by default. The reconciler retains one assigned
Native Task selection until it is released: `done` or `cancelled`, lost
assignment or visibility, Goal disabled, a `wait_customer` task whose oldest
unanswered customer wait reaches the agent's positive
`goal_wait_customer_timeout_s` (300 seconds by default), or eligible assigned
work with a strictly higher priority. It orders replacements by priority (`P0`
through `P3`), then `in_progress` before `open`, creation time, and task key.
Equal-priority work does not displace a valid selection. A task's pull request
URL does not affect Goal selection or release.

At startup, relevant task or agent changes, terminal iteration completion, and
the bounded one-minute recovery cadence, the reconciler publishes one durable
`task.goal` inbox message. It follows `Publish -> delivery -> WakeMessage` and
never starts an iteration directly. An unprocessed Goal delivery outside the DLQ
blocks another publication. Dead-lettered deliveries are retained but do not
block a new Goal generation; the positive per-agent delivery cooldown
(60 seconds by default) still blocks rapid repeats. A wait within its grace
period remains the goal but sends no continuation wake. Disabled agents or
disabled Autopilot loops neither select nor receive a goal wake, but preserve a
valid sticky key; only disabling Goal clears it.

Every non-bare agent receives daemon-owned Goal guidance inside **Task Processing
Order**, regardless of image plugins or runtime entries. The block tells the agent to work through the
Native Task workflow, wait for a recorded customer answer while in
`wait_customer`, and set `wait_customer` after recording a PR while monitoring
it rather than merging. When selected, it also contains the task key, title,
priority, status, and description. The inbox message is only a durable wake
hint. Task title and description are untrusted task input, not daemon
instructions or lifecycle authority.

The block starts with `# Task Processing Order` and a fixed instruction to
process one-shot, then messages, then Goal. Its `## One-shot`, `## Messages`,
and `## Goal` subsections always appear. Missing inputs explicitly say that
there is no one-shot instruction, no incoming messages, or no selected Goal
for this iteration; the agent is told to continue without running commands
merely to search for absent input. Outstanding requests remain nested under
Messages even when no incoming messages exist. Message IDs, threading fields,
subject/data, and the explicit `message processed` instruction are retained.
Bare sessions still receive no prompt.

The operator prompt preview uses the same grouping and Goal selection. It
keeps placeholders for messages and the one-shot supplied at execution time;
viewing the preview does not drain messages or increment delivery attempts.

## Harness environment and preflight

The daemon's own environment is the baseline every iteration starts from, so
agents see what the account sees. Desktop starts the daemon inside the account's
login shell, which supplies that environment directly; a daemon started without a
usable shell instead resolves the account's `PATH` once, before loop startup,
from a bounded interactive-login-shell probe. Per-iteration construction begins
with that daemon environment, applies harness-required values, then explicit
agent environment and secrets. An explicit agent `PATH` is therefore an
intentional override. Non-bare images prepend the agent bin directory after the
override; bare images omit that prefix and otherwise keep the daemon baseline.

Before starting the shim, the runner checks the adapter's actual executable
against this final environment. For a non-bare image it also resolves `python3`
there and supplies that absolute interpreter path to the packaged skill
launchers. A missing or non-executable harness or Python interpreter fails
preparation through the normal `harness_error` path without including
environment values, prompts, secrets, working directories, or user files. This
preflight improves the diagnosis but cannot remove the normal race in which an
executable disappears before process launch.

Immediately before the unchanged harness command is executed, Bash sources
`<base-dir>/global-agent-shell.sh`, then
`<base-dir>/agents/<name>/agent-shell.sh`. Missing or empty files do nothing;
exports and filesystem effects from either script are available to that
iteration. A source-time failure prevents the harness from starting and follows
the existing `harness_error` lifecycle.

Every `harness_error` iteration writes an `iteration_finished` audit event whose
`error` field states the cause, which the audit timeline shows next to the
status. A failure before or during launch, such as a configured agent CWD that
does not exist, records the launch error. A harness that exits non-zero records
`harness exited with code N`, followed for non-interactive agents by up to 20
trailing lines (2 KiB) of `harness.stderr.log`; the same lines are already teed
into the audit as `harness_output`.

For a schema-v2 image with skills, the launch gate prepares
`agents/<agent>/image-bridges/<image-digest>/<adapter-contract>/<harness>` before
an image is promoted or an iteration row is created. The bridge contains copied
skill bytes and generated adapter metadata; it never links back to the image or
source tree. Publication is owner-only and atomic. The current adapter contract
version is `2`. A complete bridge is reused across iterations and daemon
restarts, while an incomplete or modified cache is rebuilt. A preparation or
harness-support failure leaves the previous image active and records the
pending-image error. Because no iteration row exists yet, such a failure also
halts the loop and records its cause in the agent's `error_reason`, which the
agent status surfaces as `halt_reason` and the workspace header displays; the
audit timeline records one `iteration_failed` event with the
`image_activation_failed` reason. `Start` or `Restart` clears it and resumes the
loop.

The adapters attach the bridge additively:

| Harness | Projection |
| --- | --- |
| Claude Code | `--plugin-dir <absolute bridge>` |
| Codex CLI | At-most-8,000-character prompt catalog with absolute bridge `SKILL.md` paths |
| OpenCode | `OPENCODE_CONFIG_DIR=<absolute bridge>` with an absolute `skills.paths` entry |

Claude Code requires version `2.1.227` or newer. OpenCode is capability-probed
with `opencode debug config`; its effective configuration must report the
generated absolute skill path. Codex needs no image-skill version probe and no
plugin installation: Tariboy prepends escaped name, description, and path
metadata to the shared prompt file, and Codex reads the selected skill on
demand. Tariboy does not change CWD, `HOME`, or `CODEX_HOME`, and it does not
disable normal global or CWD discovery. Schema-v1 images retain their legacy
CWD-relative skill materialization.

Harness adapters select their native execution mode from the persisted
`interactive` setting. Batch Codex iterations use `codex exec --json` with
stdin disconnected so the loop receives machine-readable output and cannot
wait for terminal input. Interactive Codex iterations instead launch the
top-level Codex TUI with the assembled prompt and an open terminal. Both modes
consume the same catalog-prefixed prompt; Codex image skills do not appear in
native `/skills`, `$` completion, or plugin inventory. Claude
similarly omits its print-mode flags. Model, effective environment, working
directory, and AI-proxy routing still apply in both modes.

While a timed iteration is running, Agent Overview shows its deadline and can
extend it by one configured soft-timeout period. Each extension is persisted
before the watchdog is updated, so a daemon restart can adopt the running
iteration with the same deadline and extension count. Extensions are unavailable
after the timeout starts firing or for agents with no soft timeout.

Agents signal completion with `scripts/loop.sh done` (surfaced as `i-am-done`).

When a schema-v2 template declares `runtime: workdir`, preparation resolves the
agent's managed `agents/<agent>/workdir` to an absolute path and renders it
separately from the effective CWD in `runtime: identity`. The two paths are the
same for a default-CWD agent and differ when the operator configured an
external repository CWD. Rendering the path grants no new filesystem access;
the harness already runs with the daemon account's permissions.

:::note
Loop policy is set with the `tariboy loop …` subcommands — `enable`,
`disable`, `interval`, `timeout`, `hard-timeout`, `on-timeout`, `on-error`. See
[the command reference](/docs/reference/commands#operator-commands).
:::

## Image capabilities

Schema-v2 agents receive exactly the plugins declared by their image, including
an empty list. No core union is implicit. The familiar built-in capabilities
are:

- **whoami** — identity: which agent, cwd, current iteration, and client/daemon versions.
- **loop** — iteration control: signal done, start/stop the loop.
- **messages** — publish to and receive from channels.

During a managed workflow iteration, the active work packet narrows this core:
direct send/reply/group coordination and raw channel management are denied by
default. Workflow-defined tools may be allowed explicitly; dynamic observation
subscriptions use `tasks observe` and must match the packet's channel policy.

On top of the core, an image opts into **optional capabilities**:

- `context` — durable working memory,
- `status` — a one-line "what I'm doing",
- `schedule`, `scripts`, `tasks`,
- `image-creator` and any validated, explicitly
  installed external plugin capability.

The built-in `workdir` plugin is instruction-only. The `goal` capability adds
only `scripts/goal.sh set <TASK-KEY>`, allowing an agent that started without a
Goal to select an active task assigned to itself and attribute subsequent AI
requests. It cannot replace a different Goal already attached to that
iteration. A schema-v2 image explicitly packages their skills and composes the
matching runtime value. See
[built-in plugins](/docs/plugins/built-in).

See [Built-in plugins](/docs/plugins/built-in) for the complete capability,
command, prompt, and persistence reference.

Capabilities gate routes and compatibility shims, but do not package skills or
contribute prompt text automatically. Schema-v1 images retain the historical
core union and render the matching skill instructions for compatibility.

Before each iteration's prompt or message preparation, the launch gate first
honors a pending image assignment. Without one, it checks whether the active
ordinary mutable-build ref now resolves to a different digest and stages that
digest as pending. It then validates the new artifact, prepares and verifies
any native skill bridge, reconciles image-owned shims, atomically promotes the
DB assignment, and records the iteration's image ref, source version, digest,
and template hash. The `runtime: identity` value renders that immutable source
version when present. A running iteration is
never interrupted. Backup and staging markers make an interrupted swap
recoverable without changing durable agent state.
Recovery compares the pinned digest, not only the ref. For daemon-managed and
ordinary mutable-build refs, both inspection and staging resolve the exact
retained digest recorded on the agent, including assignments that were pending
when the daemon was upgraded or the ordinary ref was rebuilt.

The agent image read API projects the same selection order under the image
publication gate: explicit pending digest, ordinary mutable active ref, then
current pinned image. Current version metadata always comes from pinned
inspection. This read does not stage or promote an assignment; next is a
preview at read time and can change before the launch gate runs. A changed
digest triggers activation even if the source version stays the same. A failed
inspection is returned explicitly, and the separate pending error preserves
activation failures even without a pending ref.
