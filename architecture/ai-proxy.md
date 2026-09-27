---
title: The AI proxy & the audit log
description: Every LLM call routes through the in-process AI proxy, which enforces budgets and writes a per-iteration audit transcript.
sidebar:
  label: AI proxy & audit
  icon: activity
---

Every LLM call an agent or plugin makes is routed
through the daemon's **in-process AI proxy** (`internal/aiproxy`). The proxy:

- **routes** to the configured provider and model;
- enforces **cost budgets** (`tariboy budget …`) and records usage and cost
  into `ai_requests` (`tariboy usage`);
- writes a **per-iteration audit transcript** (`proxy-transcript.jsonl`) — the
  real request/response including thinking and tool calls — so an iteration can
  be replayed and inspected after the fact. `tariboy daemon reindex` rebuilds
  the `ai_requests` metadata from these transcripts.

The proxy drops the client's `Accept-Encoding` header when it forwards a
request. The upstream response still travels compressed, but the proxy
decompresses it, so the usage parser and transcripts see plain SSE or JSON and
the client receives the response uncompressed over loopback. Passing the
client's header through would leave compressed bytes that the usage parser
cannot read, and requests would be recorded with no model, tokens or cost.

Every agent may also have four independent USD limits for the local calendar
hour, day, ISO week (Monday start), and month. Each defaults to `0` (unlimited)
and is separate from the legacy global/group rolling budgets. When one or more
non-zero limits are exhausted from immutable `ai_requests.cost_usd` history,
the proxy denies the next request before upstream provider access.

After token authentication establishes both agent and iteration attribution,
the proxy records that iteration's activity timestamp before forwarding the
request. This drives the non-blocking stalled-agent signal described in the
[state model](/docs/architecture/state-model): it never rejects, cancels, or
otherwise changes the request or iteration lifecycle, and a recording failure
is diagnostic-only.

Before starting a harness, the daemon reads its selected Goal once, resolves
the task's top-level root through Native Tasks as that agent, and stamps the
AI-proxy lease with `task_id` and `epic_id`. All requests in the iteration are
therefore attributed without an agent command. An iteration without a selected
Goal remains unattributed.

An agent that creates or claims work after its iteration starts may use
`scripts/goal.sh set <TASK-KEY>`. The daemon validates and persists that Goal,
resolves its top-level root, and updates the live lease. Only later requests use
the new attribution; already recorded requests remain unchanged. A tagged live
lease rejects a different Goal.

## Pricing catalog and request costs

The daemon owns a last-known-good model-price catalog from LiteLLM's fixed
published source:

`https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json`

At startup it loads and validates the bounded local
`model-prices-litellm.json` cache before proxy requests begin. A missing or
invalid cache is non-fatal. If the cache is absent or at least 24 hours old, an
awaited daemon worker refreshes it after startup without blocking proxy
readiness, then refreshes every 24 hours until shutdown. Downloads have a
ten-second timeout, an eight-MiB limit, no redirect following, and complete
catalog validation before publication.

Runtime price resolution uses this precedence:

1. an operator-managed `manual` database row;
2. the latest valid `litellm` database row;
3. the built-in fallback for selected models.

LiteLLM per-token values are converted to USD per million tokens. When a model
does not publish a cache-write or cache-read price, ordinary input pricing is
used for that bucket. Provider parsers make the four token buckets exclusive:
OpenAI cache-read and cache-write tokens are subtracted from total input, while
Anthropic's already-separated values remain separate. Negative values are
clamped to zero.

One request looks up one complete in-memory price generation and stores the
resulting `cost_usd` in `ai_requests`. A later catalog publication cannot mix
prices within that request or recalculate its historical cost. Unknown models
remain proxyable and are recorded at zero cost with a bounded warning.

A successful refresh writes and syncs an owner-only temporary cache, atomically
renames it, replaces all `litellm` database rows in one transaction, and then
swaps the complete runtime map. If any candidate, download, cache, or database
step fails, the last valid runtime generation remains active; when database
publication fails after cache replacement, the next startup can reconcile the
valid cache. `pricing_catalog_loaded`, `pricing_catalog_refreshed`, and
`pricing_catalog_error` logs/events expose only source, generation time,
accepted model count, and a stable error class. They never include the catalog
body, credentials, or model request data.

## Group snapshots and Usage

After request attribution is known, the proxy resolves the agent's current
group once and stores its identifier and display name with the Usage row. A
lookup failure records an explicitly ungrouped request rather than dropping
usage. Current groups use their name as both identifier and display name, but
the stored fields remain request-time snapshots: later group or membership
changes do not rewrite them.

The server `usage` command and `/api/usage` route apply one group predicate to
the aggregate rows, daily series, summary totals, and newest-first request list.
An absent group returns all snapshots, a group name selects that saved
identifier, and `__ungrouped__` selects null snapshots. Recent requests are
bounded to 200 rows. The Desktop behavior is described in [Web UI
architecture](/docs/architecture/web-ui#server-usage).

## Keys never reach plugins

Because the proxy is the only path to an LLM, plugins never receive the upstream
provider key: the daemon **scrubs `ANTHROPIC_API_KEY` / `OPENAI_API_KEY` from
plugin environments** and hands out short-lived scoped proxy tokens instead.

## Codex harness

Codex harness iterations are configured automatically with a per-invocation
custom provider that uses this tokenized proxy (including Codex's Responses API).
Operators do not need to set `OPENAI_BASE_URL` or place an OpenAI provider key in
an agent environment for audit attribution. The transcript reader normalizes
Responses API messages, reasoning summaries, local shell actions, function and
MCP calls, and their results into the same readable Activity timeline used for
other harnesses.

Activity can copy that readable timeline as Markdown for one iteration or all
retained iterations. Its explicit ZIP export contains `audit.md` for human
review and lossless `audit.jsonl` records for downstream analysis. The JSONL
records preserve the raw proxied request and response bodies.

## Daemon restart continuity

The proxy listener remains in-process, so a daemon restart creates a brief
loopback outage. Its concrete `127.0.0.1:PORT` and active per-iteration token
leases are atomically stored in the owner-only
`~/.tariboy/aiproxy-handoff.json`. The replacement daemon prunes leases whose
iteration is no longer `running`, binds the same address, and accepts the
surviving harness's existing token. If that address is occupied while a live
lease exists, startup fails instead of silently moving the proxy and marooning
the harness on its old URL.

Adoption removes the carried lease after the shim produces a terminal result.
The handoff file is runtime-sensitive state: it is not part of SQLite, audit
logs, transcripts, or support bundles.

The pricing catalog cache has a different purpose and lifecycle from this
handoff file. It contains external model-price data, not active tokens, but is
also owner-only and excluded from support bundles. See [Security and
controls](/docs/security-controls#pricing-catalog-boundary).
