---
title: messages
description: Publish, receive, acknowledge, reply to, and subscribe to durable channel messages.
sidebar:
  label: messages
  icon: messages-square
---

`messages` connects an agent to Tariboy's durable channel bus. It is a
historical core capability included in the official Store's `basic` source;
schema-v2 images
must declare it explicitly.

## Capability surface

| Operation | Commands |
| --- | --- |
| Publish | `scripts/messages.sh message send`, `scripts/messages.sh request` |
| Inbox | `scripts/messages.sh message ls`, `processed`, `reply` |
| Recovery | `scripts/messages.sh message dlq`, `scripts/messages.sh message dlq requeue` |
| Answering | `GET /tools/chat/unanswered` — the chat messages this agent has not replied to yet |
| Subscriptions | `scripts/messages.sh sources`, `scripts/messages.sh channel subscribe`, `ls`, `unsubscribe` |

Messages are published to named channels and routed to durable per-agent
deliveries. Pending messages can wake an enabled loop and are inserted into the
next iteration in a bounded batch. A message remains pending until the agent
marks it processed; a reply also processes the source message.

`scripts/messages.sh request` adds correlation metadata. With `--deadline`, the scheduler
publishes a timeout event to the requester's inbox if no correlated reply
arrives before the deadline.

## Subscriptions and providers

A subscription can narrow existing channel traffic with type globs and a JSON
matcher. Provider-backed channels additionally accept `--params`; the daemon
validates and fingerprints those parameters into a shared watch. Use matcher
fields to filter events already flowing and provider parameters to request work
that must be produced.

```bash
scripts/messages.sh channel subscribe issue-provider:issues \
  --type 'issue.*' \
  --matcher '{"data.priority":"high"}' \
  --params '{"project":"checkout"}'
```

## Prompt integration

Package the skill and use the messages runtime marker:

```yaml Tariboyfile.yaml
plugins:
  - name: messages
skills:
  - dir: ../../skills/messages
prompts:
  - runtime: messages
```

The runtime entry renders one `# Messages` group containing the current inbox
batch and outstanding request state. The database and bus remain authoritative
for delivery state. The older `awaiting-replies` marker remains accepted for
compatibility with existing built images.

## Failure and recovery

Unprocessed messages are redelivered. After the delivery attempt limit they
move to the agent's dead-letter queue, where the agent can inspect and requeue
them. Publishing and processing are distinct durable operations, so a harness
failure does not silently acknowledge work.

## Related reference

- [Messaging architecture](/docs/architecture/messaging)
- [Channels reference](/docs/reference/channels)
