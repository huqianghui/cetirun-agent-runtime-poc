#!/bin/sh
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# session-logger — Tail agent session/audit logs to stdout.
#
# Each JSONL line is a complete audit record (user input, assistant output,
# tool calls, tool results). Writing to stdout lets Container Insights
# (Azure Monitor) capture everything via ContainerLogV2.
#
# Runs as a sidecar container with the PVC mounted read-only.

set -e

AGENT_TYPE="${AGENT_TYPE:-hermes}"
SCAN_INTERVAL="${SCAN_INTERVAL:-30}"
MAX_FILES="${MAX_FILES:-50}"

if [ "$AGENT_TYPE" = "hermes" ]; then
  LOG_FILE="/home/vibe/.hermes/logs/gateway.log"
  echo "[session-logger] Starting Hermes gateway log tail (scan interval: ${SCAN_INTERVAL}s)"
else
  SESSIONS_BASE="/home/vibe/.openclaw/agents"
  echo "[session-logger] Starting OpenClaw session JSONL logger (scan interval: ${SCAN_INTERVAL}s, max files: ${MAX_FILES})"
fi

# Clean up all child processes on exit to prevent orphaned tail processes
# that would keep consuming memory after the main loop restarts.
cleanup() {
  kill 0 2>/dev/null || true
}
trap cleanup EXIT TERM INT

while true; do
  if [ "$AGENT_TYPE" = "hermes" ]; then
    if [ ! -f "$LOG_FILE" ]; then
      sleep "$SCAN_INTERVAL"
      continue
    fi

    tail -n 0 -F -- "$LOG_FILE" &
    TAIL_PID=$!
    while kill -0 "$TAIL_PID" 2>/dev/null; do
      sleep "$SCAN_INTERVAL"
    done
    wait "$TAIL_PID" 2>/dev/null || true
    sleep "$SCAN_INTERVAL"
    continue
  fi

  # Find active session JSONL files modified in the last 24 hours.
  # Skips archived .reset.* files and stale sessions to limit open file
  # descriptors and polling overhead as session history grows.
  # Cap at MAX_FILES most-recently-modified files to bound memory usage from
  # tail's inotify watches while ensuring new sessions are always picked up.
  files=$(find "$SESSIONS_BASE" -path "*/sessions/*.jsonl" ! -name "*.reset.*" -mtime -1 -printf '%T@ %p\n' 2>/dev/null | sort -rn | head -n "$MAX_FILES" | cut -d' ' -f2- | sort)

  if [ -z "$files" ]; then
    sleep "$SCAN_INTERVAL"
    continue
  fi

  file_count=$(echo "$files" | wc -l)
  echo "[session-logger] Tailing ${file_count} session file(s)"

  # Follow from current end of file — no replay of history on (re)start.
  # -q suppresses "==> filename <==" headers that would break the JSONL stream.
  # -- prevents filenames starting with - from being parsed as options.
  # Restrict splitting to newlines and disable globbing so paths with spaces
  # or glob characters are handled safely while still running a single tail.
  old_ifs=$IFS
  IFS='
'
  set -f
  # shellcheck disable=SC2086
  set -- $files
  tail -q -n 0 -F -- "$@" &
  TAIL_PID=$!
  set +f
  IFS=$old_ifs

  # Periodically check for new session files; restart tail when set changes.
  while kill -0 "$TAIL_PID" 2>/dev/null; do
    sleep "$SCAN_INTERVAL"
    new_files=$(find "$SESSIONS_BASE" -path "*/sessions/*.jsonl" ! -name "*.reset.*" -mtime -1 -printf '%T@ %p\n' 2>/dev/null | sort -rn | head -n "$MAX_FILES" | cut -d' ' -f2- | sort)
    if [ "$new_files" != "$files" ]; then
      echo "[session-logger] Session file set changed, restarting tail"
      kill "$TAIL_PID" 2>/dev/null || true
      break
    fi
  done

  wait "$TAIL_PID" 2>/dev/null || true

  # Backoff before restarting to avoid tight loop if tail fails immediately
  # (e.g., too many open file descriptors or transient FS issues).
  sleep "$SCAN_INTERVAL"
done
