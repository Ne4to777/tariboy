#!/bin/sh
# Check of `shipped`, run as the queue: it passes, so the transition moves the
# task from the pool status build into the script status finish.
set -u
[ "${TARIBOY_WORKFLOW_OUTCOME:-}" = shipped ] || { echo "TARIBOY_WORKFLOW_OUTCOME is not shipped" >&2; exit 1; }
echo "ship check passed"
