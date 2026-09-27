---
title: The channel bus & messaging
description: Messages move over a store-backed fan-out bus — six tables, per-agent delivery queues, and durable ack with redelivery.
sidebar:
  label: Channel bus
  icon: radio
---

Messages move over a **store-backed fan-out bus** (fan-out/query logic in
`internal/bus`, schema in `internal/store/migrations/0003_bus.sql`). This page is
the architectural summary; the [channel bus reference](/docs/reference/channels)
covers the full model, tools, and debugging.

## Six tables

The four transport tables carry every message:

- **channels** — named streams (`agent:<a>:inbox`, `group:<g>:broadcast`, …);
- **messages** — immutable rows published to a channel;
- **subscriptions** — an agent's standing interest, optionally filtered by a
  content `matcher` and `type` globs;
- **deliveries** — one row per matching `(subscription, message)`; this *is* the
  per-agent queue.

Two more make a chat an entity rather than a projection of those four:

- **chats** — one row per chat, owning exactly one transport channel;
- **chat_participants** — who takes part, with each principal's role, read mark
  and mute flag.

The transport four are unaware of the other two: a chat is addressed by
publishing to the channel it owns, so `Publish -> delivery -> WakeMessage` is
the same code path it was before chats existed.

## Publish is a write, not a push

Publishing does not push text into a process. `Publish` writes the message,
creates delivery rows for matching subscriptions, and nudges affected loops.

Publishers may provide a stable idempotency key. A retry returns the original
immutable message and does not recreate deliveries or fire the publish hook a
second time. Native Tasks uses this for its transactional notification outbox.

At iteration prepare time the runner drains pending deliveries — oldest first, up
to `messages_batch` (default 10) — into the prompt. Draining does not acknowledge
them: the agent must explicitly mark each message processed by its id, and a
reply auto-processes the message it answers. Anything left unprocessed stays
pending and is rendered again next iteration, until it reaches the DLQ (max 5
attempts). Publishing and processing are distinct durable operations, so a
harness error, timeout, or kill can never silently acknowledge work.

`messages_max_queue` defaults to 100 for new agents. Reaching it keeps the
existing overflow behavior: later deliveries for that agent are stored in DLQ
with result `queue_limit`, so the limit bounds runnable pending work rather than
all message storage. Agent status derives the distinct pending count and reports
when it is at or above the configured limit.

Operators can physically clear one agent's pending queue with
`tariboy agent inbox clear NAME` or `POST /api/agents/{name}/inbox/clear`.
The bus removes that agent's unacknowledged, non-DLQ deliveries in one SQLite
transaction and deletes only message rows orphaned by that operation. Processed
and DLQ deliveries, shared messages, other agents' deliveries, and messages not
yet consumed by workflow ingress are retained.

## Channel names

Channel name prefixes: `agent`, `group`, `user`, `chat`, `plugin`, `system`.
Provider-declared channels carry their own plugin-owned prefixes and are
accepted through the provider registry rather than this list. Well-known shapes:

- `agent:<a>:inbox` — direct inbox for one agent,
- `agent:<a>:stream` — stream channel for one agent,
- `group:<g>:broadcast` — fan-out to all group members,
- `group:<g>:inbox` — group lead inbox,
- `chat:<id>` — the one transport channel a chat owns,
- `chat:telegram:<agent>` — one bundled Telegram forum topic mapped to an agent,
- `user:<name>` — user-facing channel.

Native Tasks publishes `task.assigned`, `task.question`, `task.answered`,
`task.triage`, and daemon-owned `task.goal`. An agent recipient reads the first
four in its tasks chat, `chat:tasks:<agent>`, and `task.goal` in its service
chat, `chat:service:<agent>`; customer recipients use `user:<login>`. Mentions and unresolved-answer state remain in
the task itself; the channel message is the delivery mechanism, not the source
of truth.

The customer login is a fixed value rather than the account `tariboyd` happens
to run as, so `user:customer` names the same person on every server. It defaults
to `customer` and is overridable through the `customer_login` key in
`daemon_config`. The first start after upgrading adopts the value in one
transaction, carrying existing tasks, comments, waits, notification state and
the old `user:<$USER>` channel — messages and subscriptions included — over to
the new principal instead of orphaning them.

## Chats are an entity over the channel bus

A chat is a row in `chats`: an id, a kind, a title and exactly one transport
channel `chat:<id>` that it owns. Who takes part lives in `chat_participants`,
one row per principal (`user:<login>` or `agent:<name>`) with its role, its read
mark and its mute flag. `messages`, `subscriptions` and `deliveries` are
untouched, so `Publish -> delivery -> WakeMessage` and `HasPending` keep working
exactly as before: the wake path never learns that chats exist.

A chat channel is deliberately never an agent inbox. The bus suppresses an
author's own delivery on every channel except that agent's inbox and its
`service:<agent>` chat, so an inbox-backed chat would echo an agent's own reply
straight back into its own queue. The service chat is the exception because
`script.result` and self-targeted schedule alarms are published there with
`produced_by_agent` set to the recipient; they are messages for that agent, and
suppressing them would leave it asleep.

Startup reconciliation provisions three chats per agent, and agent creation does
the same for a new agent:

| Chat | Kind | Carries |
| --- | --- | --- |
| `dm:<agent>` | `direct` | the conversation between the customer and that agent |
| `tasks:<agent>` | `tasks` | task notifications for that agent |
| `service:<agent>` | `service` | that agent's own wakes — `task.goal`, `script.result`, schedule alarms |

A chat the customer creates belongs to no agent and may hold several, which is
how a multi-agent conversation exists at all. Its id is a channel segment and is
refused when it is already in use or falls in a namespace the daemon provisions
itself (`dm:`, `tasks:`, `service:`, `group:`, `telegram:`, `plugin:`), so two
chats can never share one channel and quietly interleave their history.
Membership is what carries delivery: adding an agent subscribes it to the chat
channel, removing one revokes that subscription and leaves the deliveries it
already holds in its queue, because leaving a chat ends membership, not work
already handed out.

The personal chat carries a `legacy_agent`, which is how history written before
the chats migration stays readable: that history is the two inboxes merged by
time — the customer's messages in `agent:<a>:inbox` and the agent's messages in
`user:<customer>` — and no message is ever rewritten onto the new channel. The
feed reads the chat channel and, for a chat with a `legacy_agent`, unions that
projection.

The sender is `data.from` when a producer wrote one, else a principal-shaped
`source`, else `produced_by_agent`, else `system`. It is resolved once, when the
message is written, into the indexed `messages.sender` column, so the chat list
and a chat's history seek `(channel, sender)` instead of parsing every message
body on each read. Task notifications carry the
principal that caused them in `data.from` and keep `source` as `system:tasks`,
because moving the author into `source` would exclude an agent from a
notification it addressed to itself. An operator publish is attributed to the
customer principal, and carries `reply_to: user:<customer>` when it is sent from
a chat.

Processing a delivery and answering a message are separate obligations, and
neither implies the other. Processing satisfies the transport — every delivered
message is marked processed with a text result, or it is redelivered and
eventually dead-lettered. Answering satisfies the conversation: a message counts
as answered only when a reply of that principal points at it through
`in_reply_to`, which a reply fills and a plain send does not.
`GET /api/chats/{chat}/unanswered`, and for an agent `GET /tools/chat/unanswered`
across every chat it takes part in, is that queue — the messages another
participant sent at or after this principal joined with no reply of its own
pointing at them. The answer is published into the chat as an ordinary message,
so the conversation carries both halves.

`GET /api/chats` ranks every chat the customer takes part in by its last message
and counts what that participant has not read; `GET /api/chats/{chat}` returns
one chat and the customer's current read mark, so a reader can separate what it
has already seen; `POST /api/chats/{chat}/read` moves that mark forward. Both
take a chat id — `dm:worker`, `tasks:worker`, `service:worker` — and still
accept a bare agent name for that agent's conversation, which is what a caller
holding only a name can address. An existing chat wins over the name reading,
so an agent can never shadow a chat. Read marks are `chat_participants.read_ts`, one per participant; the
single pre-chats `chat_read_v1` value in `daemon_config` is carried into them by
the migration. Both read endpoints take a `types` filter of globs and default to
conversation types only, so an agent's own `task.goal`, `script.result` and
schedule wakes neither appear in the conversation nor move it up the list.

`GET /api/messages/ws` streams one hint per publication for every agent on the
host. A frame carries `{chat, agent, id, channel, type, from, ts}` — `chat` is
the channel's own chat and is absent for a channel no chat owns, so a client can
refetch exactly one conversation — and is only a refetch hint — the HTTP responses stay authoritative. Nothing is replayed,
because a client refetches on connect and on every reconnect.

The bundled Telegram process remains outside the daemon. It authorizes and
maps forum updates, then publishes ordinary text through the authenticated
plugin API onto `chat:telegram:<agent>`. Agent replies return through the
existing channel-sink outbox, so bus persistence, wake coalescing,
acknowledgement, redelivery, and DLQ behavior remain authoritative; Telegram
does not introduce a second agent-delivery path.

## Agent goal delivery

The per-agent Goal reconciler publishes `task.goal` to the agent's service
chat, `chat:service:<name>`, with the selected task key and `selected` or
`iteration_completed` reason. Its idempotency key includes the agent, task key,
task revision, and terminal iteration identity, so recovery and repeated scans
do not duplicate a generation. An unprocessed Goal delivery outside the DLQ
suppresses another publication. Dead-lettered deliveries are retained but do
not block a new Goal generation; a positive per-agent cooldown still suppresses
rapid repeats (60 seconds by default). Delivery remains strictly
`Publish -> delivery -> WakeMessage`; the reconciler never starts an iteration
directly. Disabled agents or disabled loops receive no new goal wake.

An agent's unfiltered subscription to its own inbox is protected system state.
Agent creation provisions it, and daemon startup reconciles all persisted
agents before starting their loops. The same startup step provisions each
agent's chats and the participant subscription that carries `chat:tasks:<a>`
and `chat:service:<a>`, so a chat-addressed wake exists before any publisher
runs. Task notifications therefore follow the ordinary
`Publish -> delivery -> WakeMessage` path unchanged, whichever channel carries
them: enabled loops wake for pending deliveries, while disabled loops leave
them queued. `agent:<a>:inbox` remains the direct inbox for group sends,
request replies and system events.

Workflow runtime wakes use the same bus/outbox path. An incoming message never
changes a workflow status directly. An operator-declared external trigger may
create a new task; an assignment-scoped, policy-allowed subscription may append
an observation and apply only its declared reaction (`record_only`, wake, hold,
or optional acknowledgement work). Late events degrade to record-only. See
[Configurable task workflows](/docs/task-workflows#channels-triggers-subscriptions-and-observations).

See the [channel bus reference](/docs/reference/channels) for message fields,
subscription matchers, schedules and scripts, groups, and failure-mode
debugging.
