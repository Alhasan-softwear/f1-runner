#!/bin/sh
# Start scripts must return: daemonize the actual work.
nohup sh -c 'while true; do echo "$(date -u) worker beat ref=$F1_REF" >> "$F1_LOG"; sleep 5; done' >/dev/null 2>&1 &
echo $! > "$F1_SHARED/worker.pid"
echo "worker started (pid $(cat "$F1_SHARED/worker.pid"))"
