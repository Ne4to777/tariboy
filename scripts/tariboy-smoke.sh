#!/usr/bin/env bash
# Smoke test for the default tariboy service.
# Starts tariboyd detached via `tariboy daemon start`, then uses bin/tariboy only.
set -euo pipefail
trap 'echo "FAIL: command failed at ${BASH_SOURCE[0]}:${LINENO}" >&2' ERR

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

AGENT="${1:-tariboy-smoke}"
IMAGE="${2:-basic-tariboy-smoke:latest}"
REAL_HARNESS="${TARIBOY_SMOKE_REAL_HARNESS:-codex}"
REQUIRE_REAL="${TARIBOY_SMOKE_REQUIRE_REAL:-0}"
CODEX_BIN="${TARIBOY_SMOKE_CODEX_BIN:-codex}"

# Real Codex smoke must not write workspace-trust entries or session state into
# the operator's Codex home. Seed an ephemeral home with only the credentials
# and configuration needed by the CLI; all trust decisions below are scoped to
# this disposable smoke run.
SOURCE_CODEX_HOME="${CODEX_HOME:-$HOME/.codex}"
source "$ROOT/scripts/smoke-codex-env.sh"
smoke_codex_env_setup "$SOURCE_CODEX_HOME" "$CODEX_BIN"
export TARIBOY_STUB_HARNESS="$ROOT/scripts/stub-harness.sh"
# Fully isolate this run from the user's real daemon: its own data dir (DB,
# agents) AND runtime dir (socket/pid/log). Without base isolation two daemons
# would share ~/.tariboy's SQLite DB and both run loop managers over the same
# agents. Smoke no longer stops the user's daemon, so it must not share its state.
export TARIBOY_BASE_DIR="$(mktemp -d)"
export TARIBOY_RUNTIME_DIR="$(mktemp -d)"
. "$ROOT/scripts/test-image-fixture.sh"
TEST_IMAGE_SOURCE="$TARIBOY_BASE_DIR/test-image"
make_test_image_fixture "$TEST_IMAGE_SOURCE"
# daemonctl appends TARIBOY_HTTP_ADDR as the authoritative --http-addr flag.
# Bind an ephemeral loopback port so the smoke cannot collide with :9990 even
# when the legacy wrapper's empty --web-addr alias is superseded.
export TARIBOY_HTTP_ADDR="127.0.0.1:0"
# Jail the /api/fs/list path-autocomplete at a seeded temp tree (not the real
# $HOME) so the web-api case can assert a known nested dir and a refused escape
# deterministically. Exported before daemon start so the detached daemon inherits
# it (fsbrowser.Root reads TARIBOY_FS_ROOT at request time).
export TARIBOY_FS_ROOT="$(mktemp -d)"
mkdir -p "$TARIBOY_FS_ROOT/projects/app"
# Smoke exercises the daemon/CLI/agent loop, not the web UI. Keep the legacy
# empty web-listener alias in the wrapper while TARIBOY_HTTP_ADDR above provides
# the authoritative isolated listener for current daemons.
TARIBOY_DAEMON_WRAP="$TARIBOY_RUNTIME_DIR/tariboyd-nowebui.sh"
cat > "$TARIBOY_DAEMON_WRAP" <<WRAP
#!/bin/sh
exec env TARIBOY_SHELL_ENV=1 "$ROOT/bin/tariboyd" --web-addr "" "\$@"
WRAP
chmod +x "$TARIBOY_DAEMON_WRAP"
export TARIBOY_DAEMON_BIN="$TARIBOY_DAEMON_WRAP"

# Track every agent the smoke run creates so cleanup can remove them all,
# even when a case fails partway through.
CREATED_AGENTS=()

cleanup() {
  if [ "${#CREATED_AGENTS[@]}" -gt 0 ]; then
    echo "--- cleanup: remove smoke agents"
    local agent
    for agent in "${CREATED_AGENTS[@]}"; do
      "$ROOT/bin/tariboy" agent rm "$agent" --force >/dev/null 2>&1 || true
    done
  fi
  "$ROOT/bin/tariboy" daemon stop >/dev/null 2>&1 || true
  smoke_codex_env_cleanup
}
trap cleanup EXIT

need() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "FAIL: missing required command: $1" >&2
    exit 1
  }
}

need python3
need tmux
need curl

sa() {
  "$ROOT/bin/tariboy" "$@"
}

codex_smoke_supported() {
  command -v "$CODEX_BIN" >/dev/null 2>&1 || {
    if [ "$REQUIRE_REAL" = "1" ]; then
      echo "FAIL: required Codex smoke harness not found: ${CODEX_BIN}" >&2
      return 2
    fi
    echo "--- codex: skip optional smoke (command not found: ${CODEX_BIN})"
    return 1
  }

  local out
  if ! out="$("$CODEX_BIN" exec --dangerously-bypass-approvals-and-sandbox --skip-git-repo-check --cd "$ROOT" "Reply with ok only." 2>&1)"; then
    if [ "$REQUIRE_REAL" = "1" ]; then
      echo "FAIL: required Codex capability probe failed" >&2
      printf '%s\n' "$out" >&2
      return 2
    fi
    echo "--- codex: skip optional smoke (capability probe failed)"
    printf '%s\n' "$out"
    return 1
  fi
  if printf '%s\n' "$out" | grep -q "approval_policy.*disallowed by requirements"; then
    if [ "$REQUIRE_REAL" = "1" ]; then
      echo "FAIL: managed requirements disallow the Codex smoke capability" >&2
      return 2
    fi
    echo "--- codex: skip optional smoke (managed requirements disallow approval_policy=Never)"
    return 1
  fi
  return 0
}

wait_daemon() {
  for _ in $(seq 1 100); do
    if sa daemon status >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.1
  done
  return 1
}

iteration_field() {
  local agent="$1"
  local field="$2"
  local data
  data="$(sa --json iteration ls "$agent")"
  FIELD="$field" DATA="$data" python3 - <<'PY'
import json
import os

data = json.loads(os.environ["DATA"])
items = data.get("iterations") or []
if not items:
    raise SystemExit(1)
print(items[-1].get(os.environ["FIELD"], ""))
PY
}

run_stub_case() {
  local agent="$1"
  local interactive="$2"
  local hello="hello from ${agent}"

  echo "--- fake harness: recreate ${agent} interactive=${interactive}"
  CREATED_AGENTS+=("$agent")
  sa agent rm "$agent" --force >/dev/null 2>&1 || true
  sa agent run "$IMAGE" --name "$agent" --harness stub --interactive "$interactive" --env "STUB_STDOUT=${hello}" --loop false | grep -q "name: ${agent}" \
    || { echo "FAIL: agent run ${agent}" >&2; exit 1; }

  run_iteration_and_check_logs "$agent" "$hello" "fake harness smoke" "$interactive"
}

run_codex_case() {
  local agent="$1"
  local interactive="$2"
  local hello="hello from ${agent}"

  echo "--- agent: recreate ${agent} with harness codex interactive=${interactive}"
  CREATED_AGENTS+=("$agent")
  sa agent rm "$agent" --force >/dev/null 2>&1 || true
  sa agent run "$IMAGE" --name "$agent" --harness codex --interactive "$interactive" --loop false | grep -q "name: ${agent}" \
    || { echo "FAIL: agent run ${agent}" >&2; exit 1; }

  if [ "$interactive" = "true" ]; then
    local workdir="$TARIBOY_BASE_DIR/agents/$agent/workdir"
    printf '\n[projects."%s"]\ntrust_level = "trusted"\n' "$workdir" >>"$CODEX_HOME/config.toml"
  fi

  local prompt
  prompt="Run this exact shell command now, then exit:

printf '%s\n' '${hello}' && i-am-done

Do not edit files. Do not explain. Do not do anything else."

  run_iteration_and_check_logs "$agent" "$hello" "$prompt" "$interactive"
}

run_iteration_and_check_logs() {
  local agent="$1"
  local hello="$2"
  local prompt="$3"
  local interactive="${4:-false}"

  echo "--- iteration: run one manual iteration (${agent})"
  sa agent exec "$agent" "$prompt" >/dev/null \
    || { echo "FAIL: agent exec ${agent}" >&2; exit 1; }

  local iter_id=""
  local status=""
  for _ in $(seq 1 180); do
    iter_id="$(iteration_field "$agent" id 2>/dev/null || true)"
    status="$(iteration_field "$agent" status 2>/dev/null || true)"
    case "$status" in
      done|no_i_am_done|harness_error|timeout|killed)
        break
        ;;
    esac
    sleep 1
  done

  [ -n "$iter_id" ] || { echo "FAIL: no iteration recorded for ${agent}" >&2; exit 1; }
  [ "$status" = "done" ] || {
    echo "FAIL: iteration ${iter_id} status=${status}, want done" >&2
    sa --json iteration logs "$agent" "$iter_id" >&2 || true
    exit 1
  }

  # Interactive harnesses run inside a tmux TUI whose pane capture
  # (harness.stdout.log) shreds any output across ANSI cursor-positioning escapes,
  # so there is no clean marker line to grep and no other log holds a clean copy.
  # status=done already requires the agent to have run i-am-done, so that terminal
  # status is the verifiable signal for interactive cases.
  if [ "$interactive" = "true" ]; then
    echo "OK: ${agent} ${iter_id} finished done (interactive; status-only check)"
    return 0
  fi

  echo "--- iteration: check logs (${agent})"
  local logs
  logs="$(sa --json iteration logs "$agent" "$iter_id")"
  if ! printf '%s' "$logs" | grep -q "$hello"; then
    echo "FAIL: iteration logs do not contain '${hello}'" >&2
    printf '%s\n' "$logs" >&2
    exit 1
  fi

  echo "OK: ${agent} ${iter_id} finished done and logged '${hello}'"
}

# --- web-api helpers -----------------------------------------------------------
# The registry HTTP routes are served on the daemon's unix socket (same handler
# as the web listener, which smoke disables with --web-addr ""), so the web-api
# case reaches them over the socket with curl — no TCP port, no collision with
# the user's live daemon on 9990.
API_SOCK="$TARIBOY_RUNTIME_DIR/tariboyd.sock"

# api METHOD PATH [json-body] -> prints the JSON response body.
api() {
  local method="$1" path="$2" body="${3:-}"
  if [ -n "$body" ]; then
    curl -sS --unix-socket "$API_SOCK" -X "$method" \
      -H 'Content-Type: application/json' -d "$body" \
      "http://localhost${path}"
  else
    curl -sS --unix-socket "$API_SOCK" -X "$method" "http://localhost${path}"
  fi
}

# api_code METHOD PATH -> prints only the HTTP status code.
api_code() {
  local method="$1" path="$2"
  curl -sS -o /dev/null -w '%{http_code}' --unix-socket "$API_SOCK" \
    -X "$method" "http://localhost${path}"
}

# assert_json reads a JSON body on stdin and runs a python assertion snippet with
# the decoded object bound to `d`; the snippet must print an OK line on success.
assert_json() {
  python3 -c "$1" || { echo "FAIL: $2" >&2; exit 1; }
}

run_webapi_case() {
  echo "--- webapi: /api/fs/list + create agent/group flows (isolated socket)"

  # (1) GET /api/fs/list — root listing exposes the seeded top-level dir.
  api GET "/api/fs/list" | assert_json '
import sys, json
d = json.load(sys.stdin)
assert d.get("ok"), d
names = [e["name"] for e in d["result"]["entries"]]
assert "projects" in names, names
print("OK fs root listing:", names)
' "fs.list root listing missing seeded dir"

  # (2) GET /api/fs/list?path=projects — nested listing exposes the child dir.
  api GET "/api/fs/list?path=projects" | assert_json '
import sys, json
d = json.load(sys.stdin)
assert d.get("ok"), d
names = [e["name"] for e in d["result"]["entries"]]
assert "app" in names, names
print("OK fs nested listing:", names)
' "fs.list nested listing missing child dir"

  # (3) GET /api/fs/list?path=/etc — an out-of-root absolute path is refused 403.
  local code
  code="$(api_code GET "/api/fs/list?path=/etc")"
  [ "$code" = "403" ] || { echo "FAIL: fs.list escape want 403, got $code" >&2; exit 1; }
  echo "OK fs escape refused: HTTP $code"

  # (4) POST /api/agents — create a standalone agent with a chosen cwd.
  local solo="${AGENT}-webapi-solo"
  local solo_cwd="$TARIBOY_FS_ROOT/projects/app"
  CREATED_AGENTS+=("$solo")
  api POST "/api/agents" \
    "{\"image\":\"${IMAGE}\",\"name\":\"${solo}\",\"cwd\":\"${solo_cwd}\",\"harness\":\"stub\",\"loop\":false}" \
    | assert_json '
import sys, json
d = json.load(sys.stdin)
assert d.get("ok"), d
print("OK created agent:", d["result"]["name"])
' "POST /api/agents (solo) failed"

  api GET "/api/agents/${solo}" | CWD="$solo_cwd" assert_json '
import sys, json, os
d = json.load(sys.stdin)
assert d.get("ok"), d
got = d["result"].get("cwd")
want = os.environ["CWD"]
assert got == want, (got, want)
print("OK agent cwd:", got)
' "GET /api/agents/${solo} cwd mismatch"

  # (5) POST /api/groups + 2x POST /api/agents{group} — group with two members.
  local grp="${AGENT}-webapi-grp"
  local m1="${grp}-a" m2="${grp}-b"
  local grp_cwd="$TARIBOY_FS_ROOT/projects"
  CREATED_AGENTS+=("$m1" "$m2")
  api POST "/api/groups" "{\"name\":\"${grp}\",\"lead\":\"${m1}\"}" | assert_json '
import sys, json
d = json.load(sys.stdin)
assert d.get("ok"), d
print("OK created group:", '"\"${grp}\""')
' "POST /api/groups failed"

  local member
  for member in "$m1" "$m2"; do
    api POST "/api/agents" \
      "{\"image\":\"${IMAGE}\",\"name\":\"${member}\",\"cwd\":\"${grp_cwd}\",\"harness\":\"stub\",\"group\":\"${grp}\",\"loop\":false}" \
      | assert_json '
import sys, json
d = json.load(sys.stdin)
assert d.get("ok"), d
print("OK created member:", d["result"]["name"])
' "POST /api/agents (member ${member}) failed"
  done

  # Assert group membership and each member's cwd landed as sent.
  api GET "/api/groups/${grp}" | M1="$m1" M2="$m2" assert_json '
import sys, json, os
d = json.load(sys.stdin)
assert d.get("ok"), d
members = set(d["result"]["members"])
want = {os.environ["M1"], os.environ["M2"]}
assert want <= members, (members, want)
print("OK group members:", sorted(members))
' "GET /api/groups/${grp} membership mismatch"

  for member in "$m1" "$m2"; do
    api GET "/api/agents/${member}" | CWD="$grp_cwd" GRP="$grp" assert_json '
import sys, json, os
d = json.load(sys.stdin)
assert d.get("ok"), d
r = d["result"]
assert r.get("cwd") == os.environ["CWD"], (r.get("cwd"), os.environ["CWD"])
assert r.get("group") == os.environ["GRP"], (r.get("group"), os.environ["GRP"])
print("OK member", r["name"], "cwd/group:", r.get("cwd"), r.get("group"))
' "GET /api/agents/${member} cwd/group mismatch"
  done

  echo "OK: webapi case passed (/api/fs/list + agent + group create flows)"
}

# run_restart_case proves EPIC dev-t-216 (`tariboy daemon restart`) end-to-end
# against the REAL isolated daemon: restart from a running state spawns a fresh
# process (new pid), and restart from a stopped state just starts. Runs first,
# before any agents exist, so it exercises pure daemon lifecycle without
# perturbing the agent-loop cases that follow. Fully isolated (base+runtime+web
# off) like the rest of this script — the live daemon on 9990 is untouched.
daemon_status_pid() {
  # Print the pid from `daemon status` ("running (pid N, version V)"), or nothing
  # if stopped. Caller decides how to treat an empty result.
  sa daemon status 2>/dev/null | sed -n 's/.*pid \([0-9][0-9]*\).*/\1/p'
}

run_restart_case() {
  echo "--- restart: running -> restart -> new pid, stopped -> restart -> running (EPIC dev-t-216)"

  # 1. Daemon is already up (started at service boot). Capture pid P1.
  local p1
  p1="$(daemon_status_pid)"
  [ -n "$p1" ] || { echo "FAIL: daemon not running before restart" >&2; exit 1; }
  echo "OK running before restart: pid $p1"

  # 2. restart exits 0; status running with a fresh pid P2 != P1.
  sa daemon restart || { echo "FAIL: daemon restart exit nonzero" >&2; exit 1; }
  wait_daemon || { echo "FAIL: daemon not ready after restart" >&2; exit 1; }
  local p2
  p2="$(daemon_status_pid)"
  [ -n "$p2" ] || { echo "FAIL: daemon not running after restart" >&2; exit 1; }
  [ "$p2" != "$p1" ] || { echo "FAIL: pid unchanged after restart (still $p1)" >&2; exit 1; }
  echo "OK restart -> fresh process: $p1 -> $p2"

  # 3. From a STOPPED state: stop, then restart just starts (exits 0, running).
  sa daemon stop || { echo "FAIL: daemon stop exit nonzero" >&2; exit 1; }
  if sa daemon status >/dev/null 2>&1; then
    echo "FAIL: daemon still running after stop" >&2
    exit 1
  fi
  echo "OK stopped"
  sa daemon restart || { echo "FAIL: restart-from-stopped exit nonzero" >&2; exit 1; }
  wait_daemon || { echo "FAIL: daemon not ready after restart-from-stopped" >&2; exit 1; }
  local p3
  p3="$(daemon_status_pid)"
  [ -n "$p3" ] || { echo "FAIL: daemon not running after restart-from-stopped" >&2; exit 1; }
  echo "OK restart-from-stopped -> running: pid $p3"

  echo "OK: restart case passed (running->restart->new-pid->running, stopped->restart->running)"
}

# db_leaked SEED|COUNT <agent>: seed or count the seven agent-keyed side-table rows
# that Store.Delete leaves behind and PurgeAgentData is meant to clean (subscriptions
# + their deliveries, schedules, scripts, ai_requests, retention_policies, and
# budgets). SEED plants exactly one sentinel row per predicate (7 total);
# COUNT prints how many still match the agent. Both talk to the isolated smoke DB
# directly via python3's stdlib sqlite3 (WAL + busy_timeout, so it coexists with the
# stopped daemon's own connection). This lets the --volumes wipe assert the leaked rows
# are actually deleted, not merely absent.
db_leaked() {
  local mode="$1" agent="$2"
  MODE="$mode" AGENT="$agent" DB="$TARIBOY_BASE_DIR/tariboyd.db" python3 - <<'PY'
import os, sqlite3
db, agent, mode = os.environ["DB"], os.environ["AGENT"], os.environ["MODE"]
scope = "agent:" + agent
c = sqlite3.connect(db, timeout=10)
c.execute("PRAGMA busy_timeout=10000")
if mode == "SEED":
    c.executescript("BEGIN;")
    c.execute("INSERT INTO subscriptions(id,agent,channel) VALUES(?,?,?)",
              ("sub-sentinel", agent, "chan-sentinel"))
    c.execute("INSERT INTO deliveries(subscription_id,message_id) VALUES(?,?)",
              ("sub-sentinel", "msg-sentinel"))
    c.execute("INSERT INTO schedules(id,agent,kind,spec,channel,next_fire_at) VALUES(?,?,?,?,?,?)",
              ("sch-sentinel", agent, "oneshot", "2099-01-01T00:00:00Z",
               "inbox:" + agent, "2099-01-01T00:00:00Z"))
    c.execute("INSERT INTO scripts(id,agent,name,description,command,mode,state,created_at) VALUES(?,?,?,?,?,?,?,?)",
              ("scr-sentinel", agent, "sentinel", "purge sentinel", "echo hi", "once",
               "completed", "2099-01-01T00:00:00Z"))
    c.execute("INSERT INTO ai_requests(id,ts,agent) VALUES(?,?,?)",
              ("air-sentinel", "2099-01-01T00:00:00Z", agent))
    c.execute("INSERT INTO retention_policies(agent) VALUES(?)", (agent,))
    c.execute("INSERT INTO budgets(scope) VALUES(?)", (scope,))
    c.commit()
    print("seeded")
else:
    total = 0
    for sql, arg in [
        # The agent already owns its protected inbox subscription. Count only
        # this fixture's row so SEED remains exactly seven sentinel records.
        ("SELECT COUNT(*) FROM subscriptions WHERE id=?", "sub-sentinel"),
        # Count the seeded delivery directly by its subscription_id (stable across
        # purge): the subscriptions row is gone after purge, so a subquery join
        # would miss a leaked delivery there.
        ("SELECT COUNT(*) FROM deliveries WHERE subscription_id=?", "sub-sentinel"),
        ("SELECT COUNT(*) FROM schedules WHERE agent=?", agent),
        ("SELECT COUNT(*) FROM scripts WHERE agent=?", agent),
        ("SELECT COUNT(*) FROM ai_requests WHERE agent=?", agent),
        ("SELECT COUNT(*) FROM retention_policies WHERE agent=?", agent),
        ("SELECT COUNT(*) FROM budgets WHERE scope=?", scope),
    ]:
        total += c.execute(sql, (arg,)).fetchone()[0]
    print(total)
c.close()
PY
}

# agent_row_state <agent>: print the agent's DB `state` via the inspect API, or
# empty if the row is gone (inspect fails). Used to assert preserve leaves the row
# stopped and --volumes deletes it.
agent_row_state() {
  sa --json agent inspect "$1" 2>/dev/null \
    | python3 -c 'import sys,json;
try:
    d=json.load(sys.stdin); print(d.get("state",""))
except Exception:
    print("")' 2>/dev/null || true
}

# run_enabled_lifecycle_case proves the master `enabled` flag lifecycle: a freshly
# created agent starts out disabled (enabled=false -> state "stopped"); `agent
# start` enables it (state becomes idle/running, i.e. anything but "stopped");
# `agent stop` disables it again (back to "stopped"). Non-interactive (stub
# harness, no tmux), so it runs under the default `make smoke`. Reuses the
# agent_row_state helper above (bare `sa --json agent inspect` object, NOT the
# {ok,result} wrapper the `api` helper returns).
run_enabled_lifecycle_case() {
  echo "--- enabled: created agent is stopped until started, and stop disables it again"
  local agent="${AGENT}-enabled"
  CREATED_AGENTS+=("$agent")
  sa agent rm "$agent" --force >/dev/null 2>&1 || true
  sa agent run "$IMAGE" --name "$agent" --harness stub --interactive false --loop false | grep -q "name: ${agent}" \
    || { echo "FAIL: agent run ${agent}" >&2; exit 1; }

  local state
  state="$(agent_row_state "$agent")"
  [ "$state" = "stopped" ] || { echo "FAIL: fresh agent state=$state, want stopped (enabled=false by default)" >&2; exit 1; }

  sa agent start "$agent" || { echo "FAIL: agent start ${agent}" >&2; exit 1; }
  state="$(agent_row_state "$agent")"
  case "$state" in
    idle|running) ;;
    *) echo "FAIL: after start state=$state, want idle or running" >&2; exit 1 ;;
  esac

  sa agent stop "$agent" || { echo "FAIL: agent stop ${agent}" >&2; exit 1; }
  state="$(agent_row_state "$agent")"
  [ "$state" = "stopped" ] || { echo "FAIL: after stop state=$state, want stopped" >&2; exit 1; }

  echo "OK: enabled lifecycle (created stopped -> start -> non-stopped -> stop -> stopped)"
}

# has_iteration <agent> <id>: exit 0 if <id> is still among the agent iterations.
has_iteration() {
  sa --json iteration ls "$1" 2>/dev/null \
    | ID="$2" python3 -c 'import sys,json,os;
d=json.load(sys.stdin); items=d.get("iterations") or [];
sys.exit(0 if any(i.get("id")==os.environ["ID"] for i in items) else 1)'
}

# Compose down/up data preservation (EPIC dev-t-gaa, verifies gaa.3 + gaa.6).
# Non-interactive (stub harness), so it runs under the default `make smoke`.
#   1. create a stub agent on image v1 + run one iteration -> real history +
#      audit.jsonl + a seeded CONTEXT.md sentinel.
#   2. compose down (no --volumes) -> row kept stopped, durable tree + CONTEXT.md +
#      audit.jsonl + the iteration all survive.
#   3. compose up (image v2, a DIFFERENT image) -> reprovisions in place: history +
#      context + audit survive AND the new image (ref + digest) is live.
#   4. seed the seven leaked side-table rows, compose down --volumes -> full wipe:
#      durable tree gone, row gone, every leaked row purged.
#
# The agent is CREATED out of band via `sa agent run --harness stub` (compose's
# file validator rejects the stub harness, wanting claude|codex|opencode), and the
# compose files below carry no harness block so they validate. Compose then drives
# the actual down/up/down --volumes converge against that existing agent — which is
# exactly the data-preservation path under test.
run_compose_preserve_case() {
  echo "--- compose: down preserves agent data across an image swap; --volumes purges (EPIC dev-t-gaa)"
  local agent="${AGENT}-compose"
  CREATED_AGENTS+=("$agent")
  # Full clean start, including any prior durable data, so a rerun is deterministic.
  sa agent rm "$agent" --force --purge >/dev/null 2>&1 || true

  local base="$TARIBOY_BASE_DIR"
  local adir="$base/agents/$agent"

  # Two DISTINCT images: v2 drops one plugin from `basic`, so its manifest digest
  # differs from v1's (verified: distinct ref AND distinct digest). This makes the
  # up-side image swap observable.
  local ctx; ctx="$(mktemp -d)"
  mkdir -p "$ctx/v1" "$ctx/v2"
  cp -R "$TEST_IMAGE_SOURCE/." "$ctx/v1/"
  cp -R "$TEST_IMAGE_SOURCE/." "$ctx/v2/"
  grep -v 'name: status' "$ctx/v1/Tariboyfile.yaml" > "$ctx/v2/Tariboyfile.yaml.tmp"
  mv "$ctx/v2/Tariboyfile.yaml.tmp" "$ctx/v2/Tariboyfile.yaml"
  sa image build --name smoke-compose-v1 --tag latest --path "$ctx/v1" | grep -q "digest:" \
    || { echo "FAIL: build smoke-compose-v1" >&2; exit 1; }
  sa image build --name smoke-compose-v2 --tag latest --path "$ctx/v2" | grep -q "digest:" \
    || { echo "FAIL: build smoke-compose-v2" >&2; exit 1; }

  # Compose files (no harness block, so they validate). loop.enabled:false so the
  # up-side converge quiesces the loop reprovision re-enables, keeping background
  # iterations from racing the assertions. Daemon is already up, so --no-start
  # (below) skips the auto-start seam. `down` ignores the image ref, so fv1 vs fv2
  # only matters for the up-side swap.
  local fv1="$ctx/compose-v1.yaml" fv2="$ctx/compose-v2.yaml"
  cat > "$fv1" <<YAML
version: 1
agents:
  ${agent}:
    image: smoke-compose-v1:latest
    loop: { enabled: false }
YAML
  sed 's/smoke-compose-v1/smoke-compose-v2/' "$fv1" > "$fv2"

  # (1) create the stub agent on v1 (out of band), then one manual iteration so it
  # has real history + audit.
  sa agent run smoke-compose-v1:latest --name "$agent" --harness stub --loop false \
    | grep -q "name: ${agent}" || { echo "FAIL: agent run ${agent}" >&2; exit 1; }
  sa agent exec "$agent" "stub run" >/dev/null \
    || { echo "FAIL: agent exec ${agent}" >&2; exit 1; }
  local iter_id="" status=""
  for _ in $(seq 1 60); do
    iter_id="$(iteration_field "$agent" id 2>/dev/null || true)"
    status="$(iteration_field "$agent" status 2>/dev/null || true)"
    [ "$status" = "done" ] && break
    sleep 0.5
  done
  [ "$status" = "done" ] || { echo "FAIL: iteration status=${status}, want done" >&2; exit 1; }
  [ -n "$iter_id" ] || { echo "FAIL: no iteration recorded" >&2; exit 1; }
  [ -f "$adir/audit.jsonl" ] || { echo "FAIL: no audit.jsonl after iteration" >&2; exit 1; }

  # Seed a known CONTEXT.md sentinel (the durable working memory a real agent would
  # write). Preserve must keep it byte-for-byte; Provision never rewrites it.
  local sentinel="compose-preserve sentinel ${iter_id}"
  printf '%s\n' "$sentinel" > "$adir/CONTEXT.md"
  local audit_sum; audit_sum="$(sha256sum "$adir/audit.jsonl" | cut -d' ' -f1)"
  echo "OK provisioned on v1 + one iteration ($iter_id); context+audit written"

  # (2) plain down: preserve. Row kept (stopped), durable artifacts survive.
  sa compose -f "$fv1" --no-start down >/dev/null \
    || { echo "FAIL: compose down (preserve)" >&2; exit 1; }
  [ "$(agent_row_state "$agent")" = "stopped" ] \
    || { echo "FAIL: agent row not kept-stopped after preserving down (state=$(agent_row_state "$agent"))" >&2; exit 1; }
  [ -d "$adir/iterations" ] || { echo "FAIL: iterations dir dropped by preserving down" >&2; exit 1; }
  [ "$(cat "$adir/CONTEXT.md" 2>/dev/null)" = "$sentinel" ] \
    || { echo "FAIL: CONTEXT.md not preserved across down" >&2; exit 1; }
  [ "$(sha256sum "$adir/audit.jsonl" 2>/dev/null | cut -d' ' -f1)" = "$audit_sum" ] \
    || { echo "FAIL: audit.jsonl not preserved across down" >&2; exit 1; }
  has_iteration "$agent" "$iter_id" || { echo "FAIL: iteration ${iter_id} lost across down" >&2; exit 1; }
  # Rebuildable tree is dropped (only image/bin/workdir).
  [ ! -d "$adir/image" ] || { echo "FAIL: image dir survived preserving down (should be dropped)" >&2; exit 1; }
  echo "OK preserving down kept row(stopped)+context+audit+iteration; dropped image tree"

  # (3) up on v2: reprovision in place. History survives AND the new image is live.
  sa compose -f "$fv2" --no-start up >/dev/null \
    || { echo "FAIL: compose up (v2 swap)" >&2; exit 1; }
  # Quiesce the loop reprovision re-enabled so it can't race the wipe assertions.
  sa agent stop "$agent" >/dev/null 2>&1 || true
  local now_ref now_dig
  now_ref="$(sa --json agent inspect "$agent" | python3 -c 'import sys,json;print(json.load(sys.stdin).get("image",""))')"
  now_dig="$(sa --json agent inspect "$agent" | python3 -c 'import sys,json;print(json.load(sys.stdin).get("digest",""))')"
  [ "$now_ref" = "smoke-compose-v2:latest" ] \
    || { echo "FAIL: image not swapped to v2 (ref=${now_ref})" >&2; exit 1; }
  [ -n "$now_dig" ] || { echo "FAIL: agent has no image digest after swap" >&2; exit 1; }
  has_iteration "$agent" "$iter_id" || { echo "FAIL: iteration ${iter_id} lost across image swap" >&2; exit 1; }
  [ "$(cat "$adir/CONTEXT.md" 2>/dev/null)" = "$sentinel" ] \
    || { echo "FAIL: CONTEXT.md lost across image swap" >&2; exit 1; }
  [ "$(sha256sum "$adir/audit.jsonl" 2>/dev/null | cut -d' ' -f1)" = "$audit_sum" ] \
    || { echo "FAIL: audit.jsonl lost across image swap" >&2; exit 1; }
  [ -d "$adir/image" ] || { echo "FAIL: image tree not re-unpacked by up (v2)" >&2; exit 1; }
  echo "OK up(v2) swapped image ($now_ref) in place; history+context+audit survived"

  # (4) seed the seven leaked side-table rows, then down --volumes -> full wipe.
  [ "$(db_leaked SEED "$agent")" = "seeded" ] || { echo "FAIL: could not seed leaked rows" >&2; exit 1; }
  local seeded; seeded="$(db_leaked COUNT "$agent")"
  [ "$seeded" = "7" ] || { echo "FAIL: expected 7 seeded leaked rows, got ${seeded}" >&2; exit 1; }
  sa compose -f "$fv2" --no-start down --volumes >/dev/null \
    || { echo "FAIL: compose down --volumes" >&2; exit 1; }
  [ ! -d "$adir" ] || { echo "FAIL: durable tree survived --volumes wipe" >&2; exit 1; }
  [ -z "$(agent_row_state "$agent")" ] \
    || { echo "FAIL: agent row survived --volumes wipe (state=$(agent_row_state "$agent"))" >&2; exit 1; }
  local left; left="$(db_leaked COUNT "$agent")"
  [ "$left" = "0" ] || { echo "FAIL: ${left} leaked rows survived --volumes purge (want 0)" >&2; exit 1; }
  echo "OK down --volumes wiped durable tree + row + all 7 leaked side-table rows"
  echo "OK: compose data-preservation case passed"
}

echo "--- service: start tariboyd (detached)"
make build
# Best-effort reset: stop any daemon left over from a previous run in this runtime dir.
sa daemon stop >/dev/null 2>&1 || true
sa daemon start || { echo "FAIL: daemon start failed" >&2; exit 1; }
wait_daemon || { echo "FAIL: tariboyd did not become ready" >&2; exit 1; }

# Daemon lifecycle: `tariboy daemon restart` (EPIC dev-t-216). Runs first,
# before any agents/images exist, so it is pure lifecycle and leaves the daemon
# running for the cases below.
run_restart_case

echo "--- image: build ${IMAGE}"
sa image build --name "${IMAGE%%:*}" --tag "${IMAGE#*:}" --path "$TEST_IMAGE_SOURCE" | grep -q "digest:" \
  || { echo "FAIL: image build did not print digest" >&2; exit 1; }

# Web-api surface (EPIC V): /api/fs/list path-autocomplete + agent/group create
# flows over the socket. Non-interactive, so it runs under the default `make smoke`.
run_webapi_case

# Compose down/up data preservation (EPIC dev-t-gaa): preserve across an image
# swap, then --volumes full wipe incl. leaked side-table rows. Non-interactive
# (stub harness), so it runs under the default `make smoke`.
run_compose_preserve_case

# Master `enabled` flag lifecycle: created stopped -> start -> stop -> stopped.
# Non-interactive (stub harness), so it runs under the default `make smoke`.
run_enabled_lifecycle_case

# Interactive cases (tmux TUI) only run in full mode (`make full-smoke` sets
# TARIBOY_SMOKE_FULL=1). The default `make smoke` runs non-interactive only:
# it is faster and, because the base dir is isolated (fresh, untrusted workdir),
# a real harness's interactive TUI can block on its workspace-trust dialog.
run_stub_case "${AGENT}-stub" false
if [ "${TARIBOY_SMOKE_FULL:-}" = "1" ]; then
  run_stub_case "${AGENT}-stub-interactive" true
fi

case "$REAL_HARNESS" in
  none)
    [ "$REQUIRE_REAL" != "1" ] || {
      echo "FAIL: TARIBOY_SMOKE_REQUIRE_REAL=1 conflicts with TARIBOY_SMOKE_REAL_HARNESS=none" >&2
      exit 1
    }
    echo "--- real harness: disabled explicitly"
    ;;
  codex)
    if codex_smoke_supported; then
      run_codex_case "${AGENT}-codex" false
      if [ "${TARIBOY_SMOKE_FULL:-}" = "1" ]; then
        run_codex_case "${AGENT}-codex-interactive" true
      fi
    else
      rc=$?
      [ "$rc" -ne 2 ] || exit 1
    fi
    ;;
  *)
    echo "FAIL: unsupported TARIBOY_SMOKE_REAL_HARNESS=${REAL_HARNESS}; want codex or none" >&2
    exit 1
    ;;
esac

if [ -n "${TARIBOY_IMAGE_SKILLS_REAL_HARNESS:-}" ]; then
  "$ROOT/scripts/image-skills-harness-smoke.sh"
fi
