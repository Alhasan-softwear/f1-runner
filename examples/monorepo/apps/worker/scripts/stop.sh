#!/bin/sh
if [ -f "$F1_SHARED/worker.pid" ]; then
  kill "$(cat "$F1_SHARED/worker.pid")" 2>/dev/null || true
  rm -f "$F1_SHARED/worker.pid"
  echo "worker stopped"
fi
