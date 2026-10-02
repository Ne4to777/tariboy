#!/bin/sh
# Watch of `finish`, driven by the `finish_mode` artifact: done reports
# finished, sleep runs long so the test can stop it, anything else fails.
set -u
snap() { python3 "$TARIBOY_WORKFLOW_DIR/scripts/snapshot.py" "$@"; }

mode="$(snap artifact finish_mode)"
case "$mode" in
  done)
    printf '{"outcome":"finished","message":"finish watch saw done"}\n' >"$TARIBOY_RESULT_FILE"
    ;;
  sleep)
    echo "$$" >>"$TARIBOY_TASK_DIR/finish.pids"
    echo "finish watch sleeping as pid $$"
    exec sleep 45
    ;;
  *)
    echo "finish watch failed on purpose: mode=$mode" >&2
    exit 7
    ;;
esac
