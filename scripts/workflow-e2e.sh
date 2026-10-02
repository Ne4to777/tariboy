#!/usr/bin/env bash
# Isolated end-to-end proof of workflow scripts: a workflow image with checks,
# watch scripts, and a queue secret drives a task through a pool status, a
# customer status, and two script statuses on a real daemon. Operator calls go
# through ttasks on the daemon's Unix socket and agent calls through each
# agent's identity-bound tools socket. The daemon owns its base and runtime
# directories and has no HTTP listener: it never touches ~/.tariboy,
# ~/.tariboyd, or 127.0.0.1:9990.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
source "$ROOT/scripts/test-image-fixture.sh"
BIN="$ROOT/bin"
FIXTURE="$ROOT/scripts/testdata/workflow-e2e"
SANDBOX="$(mktemp -d)"
BASE="$SANDBOX/base"
RUNTIME="$SANDBOX/runtime"
mkdir -p "$BASE" "$RUNTIME" "$SANDBOX/builder-work"
export TARIBOY_BASE_DIR="$BASE"
export TARIBOY_RUNTIME_DIR="$RUNTIME"
export TARIBOY_SHIM_BIN="$BIN/tariboy-shim"
export TARIBOY_STUB_HARNESS="$ROOT/scripts/stub-harness.sh"
unset TARIBOY_TOOLS_SOCKET TARIBOY_DAEMON_SOCKET E2E_TOKEN
SOCK="$RUNTIME/tariboyd.sock"
DB="$BASE/tariboyd.db"
DAEMON_LOG="$SANDBOX/daemon.log"
DPID=""
STARTED_AT=$SECONDS

# The secret every read is searched for, and the agent's own value of the
# same name, which the queue value must override in a run_as agent check.
TOKEN="e2e-queue-secret-$$-$RANDOM-value"
AGENT_TOKEN="agent-own-value-$$-$RANDOM"
# An agent secret no queue secret shadows: the run_as agent check prints it.
AGENT_SECRET="agent-only-secret-$$-$RANDOM"

# kill_sandbox_processes kills, as a last resort, every process that still
# belongs to this sandbox: the PIDs recorded under the runtime directory and
# every process whose command line or environment names the sandbox.
kill_sandbox_processes() {
  local pids=() pid file
  while IFS= read -r file; do
    pid="$(tr -dc '0-9' <"$file" 2>/dev/null)"
    [ -n "$pid" ] && pids+=("$pid")
  done < <(find "$RUNTIME" -name '*.pid' -type f 2>/dev/null)
  while IFS= read -r pid; do
    pids+=("$pid")
  done < <(pgrep -f "$SANDBOX" 2>/dev/null)
  for file in /proc/[0-9]*/environ; do
    [ -r "$file" ] && grep -qsF "$SANDBOX" "$file" && pids+=("$(basename "$(dirname "$file")")")
  done
  for pid in "${pids[@]}"; do
    [ "$pid" = "$$" ] || [ "$pid" = "${BASHPID:-}" ] && continue
    kill -9 "$pid" 2>/dev/null
  done
  return 0
}

cleanup() {
  local code=$?
  set +e
  # Agent teardown does not depend on the daemon PID: a daemon started by a
  # step that failed may still answer on its socket.
  if [ -S "$SOCK" ]; then
    "$BIN/tariboy" --socket "$SOCK" agent kill builder >/dev/null 2>&1
    "$BIN/tariboy" --socket "$SOCK" agent kill outsider >/dev/null 2>&1
  fi
  if [ -n "${DPID:-}" ]; then
    kill "$DPID" 2>/dev/null
    for _ in $(seq 1 100); do
      kill -0 "$DPID" 2>/dev/null || break
      sleep 0.1
    done
    kill -9 "$DPID" 2>/dev/null
    wait "$DPID" 2>/dev/null
  fi
  kill_sandbox_processes
  if [ "$code" -ne 0 ] && [ -s "$DAEMON_LOG" ]; then
    echo "--- last daemon log lines" >&2
    tail -n 40 "$DAEMON_LOG" >&2
  fi
  chmod -R u+w "$SANDBOX" 2>/dev/null
  rm -rf "$SANDBOX"
}
trap cleanup EXIT
fail() { echo "FAIL [$STEP]: $*" >&2; exit 1; }
STEP="setup"
step() { STEP="$1"; echo "--- $1"; }

start_daemon() {
  TARIBOY_SHELL_ENV=1 "$BIN/tariboyd" --base-dir "$BASE" --http-addr "" --log-level warn >>"$DAEMON_LOG" 2>&1 &
  DPID=$!
  for _ in $(seq 1 200); do
    if [ -S "$SOCK" ] && "$BIN/tariboy" --socket "$SOCK" agent ps >/dev/null 2>&1; then return; fi
    sleep 0.05
  done
  fail "isolated daemon did not become ready"
}
stop_daemon() {
  kill "$DPID" 2>/dev/null || true
  wait "$DPID" 2>/dev/null || true
  DPID=""
}

sa() { "$BIN/tariboy" --socket "$SOCK" "$@"; }
# op runs ttasks in operator mode (the daemon customer); as_agent runs it as an agent.
op() { env -u TARIBOY_TOOLS_SOCKET "$BIN/tariboy-tasks" "$@"; }
as_agent() { local agent="$1"; shift; TARIBOY_TOOLS_SOCKET="$RUNTIME/$agent.sock" "$BIN/tariboy-tasks" "$@"; }

# capture CMD... runs a command that may fail and keeps OUT, ERR, and CODE.
capture() {
  local out="$SANDBOX/capture.out" err="$SANDBOX/capture.err"
  set +e
  "$@" >"$out" 2>"$err"
  CODE=$?
  set -e
  OUT="$(cat "$out")"
  ERR="$(cat "$err")"
}
# contains TEXT NEEDLE succeeds when TEXT holds NEEDLE. Producers are captured
# first: piping into grep -q may cut the producer off under pipefail.
contains() { [[ "$1" == *"$2"* ]]; }
# has_line TEXT PREFIX succeeds when a line of TEXT starts with PREFIX.
has_line() {
  local line
  while IFS= read -r line; do
    [[ "$line" == "$2"* ]] && return 0
  done <<<"$1"
  return 1
}
# field PATH reads a dotted path (list indexes allowed) from JSON on stdin.
field() {
  python3 -c 'import json,sys
x=json.load(sys.stdin)
for part in sys.argv[1].split("."):
    x=x[int(part)] if isinstance(x,list) else x[part]
print(json.dumps(x,separators=(",",":")) if isinstance(x,(dict,list)) else str(x).lower() if isinstance(x,bool) else x)' "$1"
}
db() {
  python3 -c 'import sqlite3,sys
db=sqlite3.connect("file:"+sys.argv[1]+"?mode=ro",uri=True)
row=db.execute(sys.argv[2]).fetchone()
print("" if row is None else "|".join(str(v) for v in row))' "$DB" "$1"
}
# wait_for SECONDS DESCRIPTION CMD... polls CMD every 0.2s until it succeeds.
wait_for() {
  local seconds="$1" what="$2"; shift 2
  local deadline=$((SECONDS + seconds))
  until "$@"; do
    [ "$SECONDS" -lt "$deadline" ] || fail "timed out after ${seconds}s waiting for $what"
    sleep 0.2
  done
}
view() { op workflow get "$1" --json; }
status_is() { [ "$(view "$1" | field status)" = "$2" ]; }
holder_is() { [ "$(view "$1" | field holder 2>/dev/null)" = "agent:$2" ]; }
open_visit() { db "SELECT id FROM task_status_visits WHERE task_id=(SELECT id FROM tasks WHERE task_key='$1') AND left_at=''"; }
visit_counters() {
  db "SELECT rejected_requests, script_failures, idle_iterations FROM task_status_visits WHERE id=$1"
}
runs_json() { op workflow runs "$1" --json; }
# run_where KEY PYTHON-EXPR prints the newest run id matching the expression
# over a run object r, or nothing.
run_where() {
  runs_json "$1" | python3 -c 'import json,sys
for r in json.load(sys.stdin)["runs"]:
    if eval(sys.argv[1]):
        print(r["id"]); break' "$2"
}
has_run() { [ -n "$(run_where "$1" "$2")" ]; }
run_field() { op workflow runs "$1" --json | python3 -c 'import json,sys
print(next(str(r.get(sys.argv[2],"")) for r in json.load(sys.stdin)["runs"] if r["id"]==int(sys.argv[1])))' "$2" "$3"; }
last_request_state() { view "$1" | python3 -c 'import json,sys; print((json.load(sys.stdin).get("last_request") or {}).get("state",""))'; }
pid_gone() { ! kill -0 "$1" 2>/dev/null; }
# logged_runs KEY lists the runs of KEY whose log file is on disk: a run that
# never started has none, and only the latest quiet watch run keeps its files.
logged_runs() {
  local id
  for id in $(runs_json "$1" | python3 -c 'import json,sys; print(" ".join(str(r["id"]) for r in json.load(sys.stdin)["runs"]))'); do
    [ -f "$BASE/tasks/$1/runs/$id/run.log" ] && echo "$id"
  done
  return 0
}

[ -x "$BIN/tariboyd" ] && [ -x "$BIN/tariboy-tasks" ] || fail "build the binaries first: make build"

step "start an isolated daemon (base=$BASE, no HTTP listener)"
start_daemon

step "create a pool agent and a non-holder agent with long-running stub iterations"
make_test_image_fixture "$SANDBOX/image"
sa image build --name wf-e2e-agent --tag latest --path "$SANDBOX/image" >/dev/null || fail "build the agent image"
# builder runs its loop so dispatch may pick it; its own E2E_TOKEN must lose
# to the queue secret of the same name.
sa agent run wf-e2e-agent:latest --name builder --harness stub --loop true --plugins tasks \
  --cwd "$SANDBOX/builder-work" --env 'STUB_SLEEP=600,STUB_CALL_DONE=0,AGENT_MARK=from-builder' >/dev/null \
  || fail "create builder"
sa secret set builder E2E_TOKEN --value "$AGENT_TOKEN" >/dev/null || fail "set the agent secret"
sa secret set builder E2E_AGENT_SECRET --value "$AGENT_SECRET" >/dev/null || fail "set the agent-only secret"
sa agent start builder >/dev/null || fail "start builder"
sa agent run wf-e2e-agent:latest --name outsider --harness stub --loop false --plugins tasks \
  --env 'STUB_SLEEP=600,STUB_CALL_DONE=0' >/dev/null || fail "create outsider"
sa agent exec outsider >/dev/null || fail "start an outsider iteration"
wait_for 20 "the builder tools socket" test -S "$RUNTIME/builder.sock"
wait_for 20 "the outsider tools socket" test -S "$RUNTIME/outsider.sock"

step "build the fixture workflow image"
sa workflow build --path "$FIXTURE" >/dev/null || fail "build the fixture workflow"

step "a queue with no workflow still works the old way: claim, done"
op queue create --prefix LEG --name "Flexible queue" --owners outsider >/dev/null
LEG_KEY="$(op create --queue LEG --title "flexible task" --json | field key)"
CLAIMED="$(as_agent outsider ready --queue LEG --claim --idempotency-key leg-claim --json)"
contains "$CLAIMED" "\"key\":\"$LEG_KEY\"" || fail "outsider did not claim $LEG_KEY: $CLAIMED"
[ "$(as_agent outsider show "$LEG_KEY" --json | field task.status)" = in_progress ] || fail "the claimed task is not in_progress"
LEG_REVISION="$(as_agent outsider show "$LEG_KEY" --json | field task.revision)"
as_agent outsider done "$LEG_KEY" --revision "$LEG_REVISION" >/dev/null || fail "done on a flexible task"
[ "$(op show "$LEG_KEY" --json | field task.status)" = done ] || fail "the flexible task is not done"

step "binding is refused with workflow_secret_missing before the secret is set"
op queue create --prefix E2E --name "Workflow E2E" --owners outsider >/dev/null
op queue pool set E2E builders --agents builder --revision 0 --idempotency-key e2e-pool >/dev/null
capture op queue workflow set E2E e2e-flow:0.1.0 --revision 0
[ "$CODE" -ne 0 ] || fail "binding without the secret succeeded"
contains "$ERR$OUT" workflow_secret_missing || fail "want workflow_secret_missing, got: $ERR$OUT"

step "set the queue secret from stdin, bind, and never read the value back"
printf '%s\n' "$TOKEN" | op queue secret set E2E E2E_TOKEN >/dev/null
op queue workflow set E2E e2e-flow:0.1.0 --revision 0 >/dev/null || fail "binding with the secret failed"
SECRETS="$(op queue secret ls E2E --json)"
contains "$SECRETS" '"key":"E2E_TOKEN"' || fail "secret ls does not list E2E_TOKEN: $SECRETS"
capture op queue secret rm E2E E2E_TOKEN
contains "$ERR$OUT" workflow_secret_missing || fail "removing a required secret was not refused: $ERR$OUT"

step "a new task pins the workflow and is dispatched to the pool member"
KEY="$(op create --queue E2E --title "workflow task" --description "drive me" --json | field key)"
wait_for 30 "dispatch of $KEY to builder" holder_is "$KEY" builder
V="$(view "$KEY")"
[ "$(printf '%s' "$V" | field status)" = build ] || fail "status is not build: $V"
[ "$(printf '%s' "$V" | field category)" = in_progress ] || fail "category is not in_progress: $V"
BUILD_VISIT="$(open_visit "$KEY")"

step "advance without the required artifact is refused with artifact_missing"
capture as_agent builder advance "$KEY" --outcome built --from build
[ "$CODE" -ne 0 ] && contains "$ERR" artifact_missing || fail "want artifact_missing, got code=$CODE: $ERR"
status_is "$KEY" build || fail "a refused advance moved the task"

step "a rejected check leaves the status, returns its message, and counts only rejected_requests"
as_agent builder artifacts set "$KEY" report "built it" >/dev/null
as_agent builder artifacts set "$KEY" gate reject >/dev/null
capture as_agent builder advance "$KEY" --outcome built --from build
[ "$CODE" -eq 1 ] || fail "a rejected advance exited $CODE: $OUT $ERR"
has_line "$ERR" 'rejected:' || fail "no rejected line: $ERR"
contains "$ERR" 'gate says no: set gate to pass' || fail "the script message did not reach the agent: $ERR"
contains "$ERR" "hint: repeat with ttasks advance $KEY --outcome built --from build" || fail "no repeat hint: $ERR"
status_is "$KEY" build || fail "a rejected check moved the task"
[ "$(visit_counters "$BUILD_VISIT")" = "1|0|0" ] || fail "counters after a rejection: $(visit_counters "$BUILD_VISIT"), want 1|0|0"

step "--no-wait returns a pending request, and a second advance meanwhile is transition_pending"
as_agent builder artifacts set "$KEY" gate slow >/dev/null
PENDING="$(as_agent builder advance "$KEY" --outcome built --no-wait --json)"
[ "$(printf '%s' "$PENDING" | field state)" = pending ] || fail "--no-wait did not return a pending request: $PENDING"
[ "$(printf '%s' "$PENDING" | field wait_seconds)" = 70 ] || fail "wait_seconds is not the check timeouts plus 30: $PENDING"
capture as_agent builder advance "$KEY" --outcome built
contains "$ERR" transition_pending || fail "want transition_pending, got code=$CODE: $ERR"
request_done() { [ "$(last_request_state "$KEY")" = rejected ]; }
wait_for 30 "the slow check to reject" request_done
[ "$(visit_counters "$BUILD_VISIT")" = "2|0|0" ] || fail "counters after two rejections: $(visit_counters "$BUILD_VISIT")"

step "a failed check reports failed with a log path; the log is readable by the customer and the holder"
as_agent builder artifacts set "$KEY" gate fail >/dev/null
capture as_agent builder advance "$KEY" --outcome built --from build
[ "$CODE" -eq 1 ] || fail "a failed advance exited $CODE: $OUT $ERR"
has_line "$ERR" 'failed:' || fail "no failed line: $ERR"
FAILED_RUN="$(printf '%s\n' "$ERR" | sed -n "s|^hint: ttasks workflow log $KEY \([0-9]*\)\$|\1|p")"
[ -n "$FAILED_RUN" ] || fail "no workflow log hint: $ERR"
contains "$ERR" "log: $BASE/tasks/$KEY/runs/$FAILED_RUN/run.log" || fail "no log path in the failure: $ERR"
LOG="$(op workflow log "$KEY" "$FAILED_RUN")"
contains "$LOG" 'gate check broke on purpose: gate=fail' || fail "the operator cannot read the failed check's log: $LOG"
LOG="$(as_agent builder workflow log "$KEY" "$FAILED_RUN")"
contains "$LOG" 'gate check broke on purpose' || fail "the holder cannot read the log of its run_as agent check: $LOG"
[ "$(run_field "$KEY" "$FAILED_RUN" verdict)" = failure ] || fail "the failed run's verdict is not failure"
[ "$(run_field "$KEY" "$FAILED_RUN" exit_code)" = 3 ] || fail "the failed run's exit code is not 3"
[ "$(visit_counters "$BUILD_VISIT")" = "2|1|0" ] || fail "counters after a failure: $(visit_counters "$BUILD_VISIT"), want 2|1|0"
status_is "$KEY" build || fail "a failed check moved the task"

step "an agent that can read the task but is not a holder is refused the log"
OUTSIDER_RUNS="$(as_agent outsider workflow runs "$KEY" --json)"
contains "$OUTSIDER_RUNS" "\"id\":$FAILED_RUN" || fail "the queue owner cannot list the runs: $OUTSIDER_RUNS"
capture as_agent outsider workflow log "$KEY" "$FAILED_RUN"
[ "$CODE" -ne 0 ] && contains "$ERR" forbidden || fail "a non-holder read the log: code=$CODE $OUT $ERR"
QUEUE_RUN="$(run_where "$KEY" 'r["script"]=="./scripts/check-env.sh"')"
capture as_agent outsider workflow log "$KEY" "$QUEUE_RUN"
[ "$CODE" -ne 0 ] && contains "$ERR" forbidden || fail "a non-holder read a queue run's log: code=$CODE"

step "passing checks apply the transition: pool -> customer"
as_agent builder artifacts set "$KEY" gate pass >/dev/null
capture as_agent builder advance "$KEY" --outcome built --from build --message "ready for review"
[ "$CODE" -eq 0 ] || fail "a passing advance exited $CODE: $OUT $ERR"
V="$(view "$KEY")"
[ "$(printf '%s' "$V" | field status)" = approval ] || fail "status is not approval: $V"
[ "$(printf '%s' "$V" | field waiting_on)" = customer ] || fail "waiting_on is not customer: $V"
[ "$(printf '%s' "$V" | field last_request.state)" = applied ] || fail "the request is not applied: $V"

step "the scripts received the snapshot with the visit id, the queue secret, and the right working directories"
STATE="$BASE/tasks/$KEY/state"
[ "$(python3 -c 'import os,sys; print(oct(os.stat(sys.argv[1]).st_mode & 0o777))' "$STATE")" = 0o700 ] \
  || fail "the task state directory is not 0700"
SNAP="$(cat "$STATE/queue-check.task.json")"
[ "$(printf '%s' "$SNAP" | field visit.id)" = "$BUILD_VISIT" ] || fail "snapshot visit id is not the build visit $BUILD_VISIT: $SNAP"
[ "$(printf '%s' "$SNAP" | field key)" = "$KEY" ] && [ "$(printf '%s' "$SNAP" | field outcome)" = built ] \
  && [ "$(printf '%s' "$SNAP" | field holders.builders)" = builder ] || fail "unexpected snapshot: $SNAP"
contains "$SNAP" "$TOKEN" && fail "the snapshot holds the secret"
WANT_SHA="$(printf '%s' "$TOKEN" | python3 -c 'import hashlib,sys; print(hashlib.sha256(sys.stdin.buffer.read()).hexdigest())')"
[ "$(cat "$STATE/queue-check.sha")" = "$WANT_SHA" ] || fail "the queue check did not receive the queue secret"
[ "$(cat "$STATE/agent-check.cwd")" = "$(cd "$SANDBOX/builder-work" && pwd -P)" ] || fail "the run_as agent check did not run in the holder's working directory: $(cat "$STATE/agent-check.cwd")"
[ "$(cat "$STATE/agent-check.mark")" = from-builder ] || fail "the run_as agent check did not get the holder's environment"
[ "$(cat "$STATE/agent-check.sha")" = "$WANT_SHA" ] || fail "the queue secret did not override the agent's value"

step "the customer advances: customer -> script status; the watch is quiet twice, then reports merged"
op advance "$KEY" --outcome approved --from approval >/dev/null || fail "the customer could not advance"
wait_for 60 "the monitor watch to move the task to finish" status_is "$KEY" finish
MONITOR="$(runs_json "$KEY" | python3 -c 'import json,sys
print(" ".join(r["verdict"] for r in reversed(json.load(sys.stdin)["runs"]) if r["script"]=="./scripts/watch-monitor.sh"))')"
[ "$MONITOR" = "quiet quiet outcome" ] || fail "monitor verdicts were '$MONITOR', want 'quiet quiet outcome'"
MONITOR_VISIT="$(cat "$STATE/monitor.visit")"
DB_MONITOR_VISIT="$(db "SELECT id FROM task_status_visits WHERE task_id=(SELECT id FROM tasks WHERE task_key='$KEY') AND status_id='monitor'")"
[ "$MONITOR_VISIT" = "$DB_MONITOR_VISIT" ] || fail "the monitor saw visit $MONITOR_VISIT, the database's monitor visit is $DB_MONITOR_VISIT"
[ "$(cat "$STATE/monitor-$MONITOR_VISIT.count")" = 3 ] || fail "the monitor watch did not run three times in its visit"
VERDICT="$(op artifacts show "$KEY" verdict --json | field artifact.value)"
[ "$VERDICT" = "merged in visit $MONITOR_VISIT with [redacted]" ] || fail "verdict artifact is '$VERDICT'"
[ "$(op artifacts show "$KEY" verdict --json | field artifact.author)" = script:./scripts/watch-monitor.sh ] || fail "the verdict artifact is not authored by the script"
EVENTS="$(op events "$KEY" --limit 500 --json)"
[ "$(printf '%s' "$EVENTS" | python3 -c 'import json,sys
print(sum(1 for e in json.load(sys.stdin)["events"] if e["kind"]=="workflow.script_run" and (e.get("payload") or {}).get("verdict")=="quiet"))')" = 0 ] \
  || fail "a quiet watch run left an event"

step "a failing watch script is recorded as a failure and counted"
FINISH_VISIT="$(open_visit "$KEY")"
failed_finish() { has_run "$KEY" 'r["script"]=="./scripts/watch-finish.sh" and r.get("verdict")=="failure"'; }
wait_for 30 "a failed finish watch run" failed_finish
FINISH_FAIL="$(run_where "$KEY" 'r["script"]=="./scripts/watch-finish.sh" and r.get("verdict")=="failure"')"
[ "$(run_field "$KEY" "$FINISH_FAIL" exit_code)" = 7 ] || fail "the failed watch run's exit code is not 7"
LOG="$(op workflow log "$KEY" "$FINISH_FAIL")"
contains "$LOG" 'finish watch failed on purpose' || fail "the failed watch log is not readable: $LOG"
FAILURES="$(visit_counters "$FINISH_VISIT" | cut -d'|' -f2)"
[ "$FAILURES" -ge 1 ] || fail "script_failures was not counted for the finish visit"
EVENTS="$(op events "$KEY" --limit 500 --json)"
contains "$EVENTS" '"workflow.script_failed"' || fail "no workflow.script_failed event"
status_is "$KEY" finish || fail "a failed watch moved the task"

step "an operator move and a cancel stop a running watch"
KEY_B="$(op create --queue E2E --title "move and cancel" --json | field key)"
wait_for 30 "dispatch of $KEY_B to builder" holder_is "$KEY_B" builder
op artifacts set "$KEY_B" finish_mode sleep >/dev/null
op workflow move "$KEY_B" --to finish --reason "e2e: start a long watch" >/dev/null
sleeping() { [ -s "$BASE/tasks/$1/state/finish.pids" ] && has_run "$1" 'r["state"]=="running"'; }
wait_for 30 "a running finish watch on $KEY_B" sleeping "$KEY_B"
SLEEP_RUN="$(run_where "$KEY_B" 'r["state"]=="running"')"
SLEEP_PID="$(tail -n1 "$BASE/tasks/$KEY_B/state/finish.pids")"
op workflow move "$KEY_B" --to approval --reason "e2e: operator override" >/dev/null || fail "workflow move failed"
status_is "$KEY_B" approval || fail "the move did not reach approval"
run_cancelled() { [ "$(run_field "$1" "$2" state)" = cancelled ]; }
wait_for 20 "the moved-away watch run to be cancelled" run_cancelled "$KEY_B" "$SLEEP_RUN"
wait_for 10 "the moved-away watch process to exit" pid_gone "$SLEEP_PID"
op workflow move "$KEY_B" --to finish --reason "e2e: start it again" >/dev/null
second_sleep() { [ "$(wc -l <"$BASE/tasks/$KEY_B/state/finish.pids")" -ge 2 ] && has_run "$KEY_B" 'r["state"]=="running"'; }
wait_for 30 "a second running finish watch on $KEY_B" second_sleep
SLEEP_RUN="$(run_where "$KEY_B" 'r["state"]=="running"')"
SLEEP_PID="$(tail -n1 "$BASE/tasks/$KEY_B/state/finish.pids")"
op cancel "$KEY_B" >/dev/null || fail "cancel failed"
VB="$(op show "$KEY_B" --json)"
[ "$(printf '%s' "$VB" | field task.category)" = cancelled ] || fail "the cancelled task's category is not cancelled: $VB"
[ "$(printf '%s' "$VB" | field task.status)" = finish ] || fail "cancel did not keep the workflow status: $VB"
wait_for 20 "the cancelled task's watch run to be cancelled" run_cancelled "$KEY_B" "$SLEEP_RUN"
wait_for 10 "the cancelled task's watch process to exit" pid_gone "$SLEEP_PID"

step "a daemon restart during a long watch records it as interrupted, and the watch runs again after every"
op artifacts set "$KEY" finish_mode sleep >/dev/null
wait_for 30 "a running finish watch on $KEY" sleeping "$KEY"
LONG_RUN="$(run_where "$KEY" 'r["state"]=="running"')"
LONG_PID="$(tail -n1 "$STATE/finish.pids")"
# The next run, after the restart, finds done and reports finished.
op artifacts set "$KEY" finish_mode done >/dev/null
stop_daemon
pid_gone "$LONG_PID" || fail "the watch process outlived the daemon"
[ "$(db "SELECT state FROM task_script_runs WHERE id=$LONG_RUN")" = running ] || fail "the run was not left running for recovery"
start_daemon
interrupted() { [ "$(run_field "$KEY" "$LONG_RUN" state)" = interrupted ]; }
wait_for 30 "the run in progress to be recorded as interrupted" interrupted
wait_for 30 "the rescheduled watch to finish the task" status_is "$KEY" done
[ "$(op show "$KEY" --json | field task.category)" = done ] || fail "the finished task's category is not done"
has_run "$KEY" "r[\"id\"]>$LONG_RUN and r[\"script\"]==\"./scripts/watch-finish.sh\" and r.get(\"verdict\")==\"outcome\"" \
  || fail "no watch run after the interrupted one reported the outcome"

step "a pool member that owns no queue advances from the pool into a script status and sees the applied request"
KEY_C="$(op create --queue E2E --title "ship straight" --json | field key)"
wait_for 30 "dispatch of $KEY_C to builder" holder_is "$KEY_C" builder
as_agent builder artifacts set "$KEY_C" finish_mode done >/dev/null
capture as_agent builder advance "$KEY_C" --outcome shipped --from build --json
[ "$CODE" -eq 0 ] || fail "the advance into the script status exited $CODE: $OUT $ERR"
[ "$(printf '%s' "$OUT" | field state)" = applied ] || fail "the advance did not print the applied request: $OUT"
VC="$(as_agent builder workflow get "$KEY_C" --json)"
contains "$VC" "\"name\":\"e2e-flow\"" || fail "the former holder cannot read its task's workflow: $VC"

step "the secret value appears in no read, while [redacted] does; the agent's secret only in the logs it may read"
QUEUE_LOG_FILE="$(cat "$BASE/tasks/$KEY/runs/$QUEUE_RUN/run.log")"
contains "$QUEUE_LOG_FILE" "$TOKEN" || fail "the queue check did not print the secret into its log file"
AGENT_LOG_FILE="$(cat "$BASE/tasks/$KEY/runs/$FAILED_RUN/run.log")"
contains "$AGENT_LOG_FILE" "$AGENT_SECRET" || fail "the run_as agent check did not print the agent's secret into its log file"
# READS gathers every read of every principal; OTHERS only the reads of a
# principal that is neither the customer reading a log nor the recorded holder.
READS="$SANDBOX/reads.txt"
OTHERS="$SANDBOX/others.txt"
HOLDER_LOGS="$SANDBOX/holder-logs.txt"
: >"$READS"
: >"$OTHERS"
: >"$HOLDER_LOGS"
for k in "$KEY" "$KEY_B"; do
  {
    op show "$k" --json
    op workflow get "$k" --json
    op workflow get "$k"
    op workflow runs "$k" --json
    op workflow runs "$k"
    op events "$k" --limit 500 --json
    op artifacts ls "$k" --json
    as_agent outsider show "$k" --json
    as_agent outsider workflow get "$k" --json
    as_agent outsider workflow runs "$k" --json
  } >>"$OTHERS"
  {
    as_agent builder show "$k" --json
    as_agent builder workflow get "$k" --json
    as_agent builder workflow runs "$k" --json
  } >>"$READS"
  for run in $(logged_runs "$k"); do
    op workflow log "$k" "$run" --json >>"$READS"
    op workflow log "$k" "$run" >>"$READS"
    as_agent builder workflow log "$k" "$run" --json >>"$HOLDER_LOGS"
    as_agent builder workflow log "$k" "$run" >>"$HOLDER_LOGS"
    capture as_agent outsider workflow log "$k" "$run"
    [ "$CODE" -ne 0 ] && contains "$ERR" forbidden || fail "outsider read the log of run $run of $k: code=$CODE"
    printf '%s\n%s\n' "$OUT" "$ERR" >>"$OTHERS"
  done
done
op artifacts show "$KEY" verdict --json >>"$OTHERS"
op queue secret ls E2E --json >>"$OTHERS"
op queue workflow get E2E --json >>"$OTHERS"
cat "$OTHERS" "$HOLDER_LOGS" >>"$READS"
ALL_READS="$(cat "$READS")"
OTHER_READS="$(cat "$OTHERS")"
HOLDER_READS="$(cat "$HOLDER_LOGS")"
contains "$ALL_READS" "$TOKEN" && fail "the secret value appears in a read: $(grep -F "$TOKEN" "$READS" | head -n 3)"
contains "$ALL_READS" "$AGENT_TOKEN" && fail "the agent's shadowed secret value appears in a read"
contains "$OTHER_READS" "$AGENT_SECRET" && fail "the agent's secret appears in a read by another principal: $(grep -F "$AGENT_SECRET" "$OTHERS" | head -n 3)"
contains "$HOLDER_READS" "$AGENT_SECRET" || fail "the holder's log read lacks its own secret, so the check above proves nothing"
contains "$HOLDER_READS" '[redacted]' || fail "the holder's log read shows no [redacted]"
contains "$ALL_READS" '[redacted]' || fail "no read shows [redacted]"
[ "$(run_field "$KEY" "$QUEUE_RUN" message)" = "checked with token [redacted]" ] || fail "the check message was not redacted: $(run_field "$KEY" "$QUEUE_RUN" message)"
LOG="$(op workflow log "$KEY" "$QUEUE_RUN")"
contains "$LOG" 'checking with token [redacted]' || fail "the log route did not redact the secret"

echo "PASS: workflow e2e in $((SECONDS - STARTED_AT))s"
