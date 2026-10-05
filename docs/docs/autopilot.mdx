---
title: Autopilot
description: Run bounded, observable iterations on timers and durable message triggers.
sidebar:
  label: Autopilot
  icon: repeat
---

Autopilot is the daemon-controlled iteration loop. It is independent from an
interactive terminal: either can run alone, both can be enabled, and stopping
one does not silently change the other.

## Trigger model

With Autopilot enabled, an iteration can start from:

- a configured timer interval; or
- a matching pending message delivery.

Messages are durable. Publishing creates a delivery for each matching
subscription and nudges the loop. The runner includes pending deliveries in the
next prompt. Normal completion acknowledges them; harness errors, timeouts, and
kills preserve them for bounded redelivery.

Disabling Autopilot prevents new scheduled and message-triggered iterations. It
does not delete pending messages and does not terminate an interactive Console.

## Configure conservatively

Start with:

- an image prompt that defines one coherent unit of work;
- a long enough interval to inspect outcomes;
- a soft timeout that fits normal work;
- a hard timeout that bounds the worst case;
- a low cost budget;
- no external subscriptions until the timer path is understood.

The Autopilot tab shows live enablement and policy. Activity shows trigger,
iteration state, result, deadlines, audit, usage, and cost. For Claude and
Codex harnesses, the audit is presented as a readable timeline of messages,
reasoning, commands, skills, tool calls, and results; model and token metadata
remains available on each AI call. A running soft deadline may be extended only
before timeout enforcement starts.

## Controls

- **Exec** (agent header, shown while the agent is enabled) — start one manual
  iteration immediately, whether Autopilot is enabled or paused. It is
  available for both interactive and non-interactive agents; optional one-shot
  text, entered in its dialog, is appended only to that iteration's assembled
  prompt.
- **Pause / Disable Autopilot** — prevent new autonomous iterations.
- **Kill** — immediately stop current iteration or session; use when work is
  unsafe or stuck.
- **Budget** — reject AI calls once the configured spend limit is reached.
- **Audit** — inspect each proxied AI request and tool timeline. Copy readable
  Markdown or export a ZIP for the selected iteration or all retained
  iterations. Exports contain sensitive agent data; review the warning before
  downloading them.

After a kill, verify the Activity outcome and pending message state before
re-enabling. A killed or failed iteration may leave a message eligible for
redelivery.

## Event-driven progression

Channels and subscriptions are the advanced layer behind message triggers. They
allow agents and plugins to wake work without polling. Begin with one explicit
subscription and one observable publisher, then add fan-out and groups after
the single-agent delivery lifecycle is understood.

In Desktop an agent's event triggers are managed in **Chat → Channels**; the
Autopilot card shows how many there are and links there. The delivery queue,
with its Archive and DLQ, is **Chat → Queue**.

See [Messaging architecture](/docs/architecture/messaging) and [Channel
reference](/docs/reference/channels) for the complete contract.
