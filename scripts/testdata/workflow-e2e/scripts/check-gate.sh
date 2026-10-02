#!/bin/sh
# Second check of `built`, run as the holder (run_as: agent). It records where
# and with what it ran, then follows the `gate` artifact.
set -u
snap() { python3 "$TARIBOY_WORKFLOW_DIR/scripts/snapshot.py" "$@"; }

pwd -P >"$TARIBOY_TASK_DIR/agent-check.cwd"
printf '%s\n' "${AGENT_MARK:-}" >"$TARIBOY_TASK_DIR/agent-check.mark"
snap sha256 E2E_TOKEN >"$TARIBOY_TASK_DIR/agent-check.sha"
# Deliberately careless: the holder's own secret goes to the log, which only
# the customer and this agent may read; no other principal's read may show it.
echo "agent secret ${E2E_AGENT_SECRET:-}"

gate="$(snap artifact gate)"
case "$gate" in
  pass)
    exit 0
    ;;
  reject)
    printf '{"message":"gate says no: set gate to pass"}\n' >"$TARIBOY_RESULT_FILE"
    exit "$TARIBOY_REJECT_EXIT"
    ;;
  slow)
    sleep 3
    printf '{"message":"gate was slow and says no"}\n' >"$TARIBOY_RESULT_FILE"
    exit "$TARIBOY_REJECT_EXIT"
    ;;
  *)
    echo "gate check broke on purpose: gate=$gate" >&2
    exit 3
    ;;
esac
