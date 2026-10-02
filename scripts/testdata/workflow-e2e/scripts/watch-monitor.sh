#!/bin/sh
# Watch of `monitor`: quiet twice, then merged. It counts per visit, the way a
# real watch keeps its state: a new visit starts again from zero.
set -u
snap() { python3 "$TARIBOY_WORKFLOW_DIR/scripts/snapshot.py" "$@"; }

visit="$(snap field visit.id)"
counter="$TARIBOY_TASK_DIR/monitor-$visit.count"
runs=$(( $(cat "$counter" 2>/dev/null || echo 0) + 1 ))
echo "$runs" >"$counter"
echo "monitor run $runs of visit $visit"
if [ "$runs" -lt 3 ]; then
  exit "$TARIBOY_QUIET_EXIT"
fi

echo "$visit" >"$TARIBOY_TASK_DIR/monitor.visit"
# The artifact carries the secret on purpose, to prove artifact redaction.
printf '{"outcome":"merged","message":"merged after %s runs","artifacts":{"verdict":"merged in visit %s with %s"}}\n' \
  "$runs" "$visit" "$E2E_TOKEN" >"$TARIBOY_RESULT_FILE"
