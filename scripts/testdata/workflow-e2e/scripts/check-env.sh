#!/bin/sh
# First check of `built`, run as the queue. It proves what a queue run
# receives, leaves evidence in TARIBOY_TASK_DIR, and passes.
set -u
snap() { python3 "$TARIBOY_WORKFLOW_DIR/scripts/snapshot.py" "$@"; }
fail() { printf '%s\n' "$*" >&2; exit 1; }

[ -n "${E2E_TOKEN:-}" ] || fail "the queue secret E2E_TOKEN is missing"
[ -z "${TARIBOY_TOOLS_SOCKET:-}" ] || fail "TARIBOY_TOOLS_SOCKET reached a workflow script"
[ "$(pwd -P)" = "$(cd "$TARIBOY_TASK_DIR" && pwd -P)" ] || fail "the working directory is not TARIBOY_TASK_DIR"
[ "${E2E_MODE:-}" = fixture ] || fail "the workflow env E2E_MODE is missing"
[ "${TARIBOY_WORKFLOW_OUTCOME:-}" = built ] || fail "TARIBOY_WORKFLOW_OUTCOME is not built"
[ "$TARIBOY_WORKFLOW_STATUS" = build ] || fail "TARIBOY_WORKFLOW_STATUS is not build"
[ "$TARIBOY_QUIET_EXIT" = 111 ] && [ "$TARIBOY_REJECT_EXIT" = 112 ] || fail "unexpected exit code constants"

# Evidence the test compares without seeing the secret.
snap sha256 E2E_TOKEN >"$TARIBOY_TASK_DIR/queue-check.sha"
cp "$TARIBOY_TASK_FILE" "$TARIBOY_TASK_DIR/queue-check.task.json"

# Deliberately careless: the secret goes to the log and into the message so
# the test can prove that reads redact it. A real script must never do this.
echo "checking with token $E2E_TOKEN"
printf '{"message":"checked with token %s"}\n' "$E2E_TOKEN" >"$TARIBOY_RESULT_FILE"
