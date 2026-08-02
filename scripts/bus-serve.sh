#!/usr/bin/env bash
# scripts/bus-serve.sh — start/stop/status a LOCAL agent-bus server.
#
# This is the ONLY sanctioned way to bring up a local agent-bus server: an
# agent should never hand-write a `go run`/`go build` line or construct the
# HTTP call itself (repo invariant 7). It builds the cmd/agent-bus binary,
# runs it backgrounded with a pidfile by default (or attached in the
# foreground with --foreground), and polls GET /healthz to confirm it is
# actually serving before `start` returns.
#
# Nothing this script does touches the repo tree: the built binary, pidfile,
# log, and default data dir all live under a run dir outside the repo. The
# tracked ./data directory is never used as a default — override
# AGENT_BUS_DATA_DIR explicitly if you really want that path.
#
# Usage:
#   scripts/bus-serve.sh start [--foreground|-f]
#   scripts/bus-serve.sh status
#   scripts/bus-serve.sh stop
#
# Env overrides (all optional):
#   AGENT_BUS_RUN_DIR      default /tmp/agent-bus         — pidfile, log, built binary
#   AGENT_BUS_DATA_DIR     default $AGENT_BUS_RUN_DIR/data — durable store + WAL (-data-dir)
#   AGENT_BUS_LISTEN       default 127.0.0.1:8080          — LOOPBACK by default (-listen)
#   AGENT_BUS_LOG_LEVEL    default info                    (-log-level)
#   AGENT_BUS_POLL_TIMEOUT default 30s                     (-poll-timeout)
#
# Exit codes:
#   start:  0 started and healthy; 1 already running / failed to become healthy; 2 bad usage
#   status: 0 running and healthy; 1 process alive but /healthz not answering;
#           3 not running (no pidfile, or stale pidfile); 2 bad usage
#   stop:   0 stopped (or was already stopped); 2 bad usage
set -euo pipefail

usage() {
  cat >&2 <<'EOF'
usage: bus-serve.sh {start [--foreground|-f]|status|stop}
EOF
}

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." >/dev/null 2>&1 && pwd)"

RUN_DIR="${AGENT_BUS_RUN_DIR:-/tmp/agent-bus}"
DATA_DIR="${AGENT_BUS_DATA_DIR:-${RUN_DIR}/data}"
LISTEN="${AGENT_BUS_LISTEN:-127.0.0.1:8080}"
LOG_LEVEL="${AGENT_BUS_LOG_LEVEL:-info}"
POLL_TIMEOUT="${AGENT_BUS_POLL_TIMEOUT:-30s}"

PID_FILE="${RUN_DIR}/agent-bus.pid"
LOG_FILE="${RUN_DIR}/agent-bus.log"
BIN_FILE="${RUN_DIR}/bin/agent-bus"

HEALTH_URL="http://${LISTEN}/healthz"

# pid_running PID — true if a process with that pid exists.
pid_running() {
  local pid="$1"
  [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null
}

# read_pid — echoes the pid recorded in PID_FILE, or nothing if absent.
read_pid() {
  if [[ -f "$PID_FILE" ]]; then
    tr -d '[:space:]' < "$PID_FILE"
  fi
}

cmd_status() {
  local pid
  pid="$(read_pid)"
  if [[ -z "$pid" ]]; then
    echo "agent-bus: not running (no pidfile at ${PID_FILE})"
    return 3
  fi
  if ! pid_running "$pid"; then
    echo "agent-bus: not running (stale pidfile ${PID_FILE}, pid ${pid} gone)"
    return 3
  fi
  if curl -fsS -o /dev/null "$HEALTH_URL"; then
    echo "agent-bus: running (pid ${pid}, listen ${LISTEN}, data-dir ${DATA_DIR})"
    return 0
  fi
  echo "agent-bus: process running (pid ${pid}) but ${HEALTH_URL} did not answer"
  return 1
}

cmd_stop() {
  local pid
  pid="$(read_pid)"
  if [[ -z "$pid" ]] || ! pid_running "$pid"; then
    echo "agent-bus: not running, nothing to stop"
    rm -f "$PID_FILE"
    return 0
  fi
  echo "agent-bus: stopping pid ${pid}..."
  kill -TERM "$pid" 2>/dev/null || true
  local waited=0
  while pid_running "$pid" && (( waited < 100 )); do
    sleep 0.1
    waited=$((waited + 1))
  done
  if pid_running "$pid"; then
    echo "agent-bus: pid ${pid} did not exit after 10s, sending SIGKILL" >&2
    kill -KILL "$pid" 2>/dev/null || true
  fi
  rm -f "$PID_FILE"
  echo "agent-bus: stopped"
  return 0
}

cmd_start() {
  local foreground=0
  if [[ "${1:-}" == "--foreground" || "${1:-}" == "-f" ]]; then
    foreground=1
  fi

  local pid
  pid="$(read_pid)"
  if [[ -n "$pid" ]] && pid_running "$pid"; then
    echo "agent-bus: already running (pid ${pid}); refusing to start a second instance" >&2
    return 1
  fi
  if [[ -n "$pid" ]]; then
    echo "agent-bus: clearing stale pidfile (pid ${pid} gone)" >&2
    rm -f "$PID_FILE"
  fi

  mkdir -p "$RUN_DIR" "$DATA_DIR" "$(dirname "$BIN_FILE")"

  echo "agent-bus: building..." >&2
  ( cd "$REPO_ROOT" && go build -o "$BIN_FILE" ./cmd/agent-bus )

  if [[ "$foreground" -eq 1 ]]; then
    exec "$BIN_FILE" -listen "$LISTEN" -data-dir "$DATA_DIR" -log-level "$LOG_LEVEL" -poll-timeout "$POLL_TIMEOUT"
  fi

  : > "$LOG_FILE"
  nohup "$BIN_FILE" -listen "$LISTEN" -data-dir "$DATA_DIR" -log-level "$LOG_LEVEL" -poll-timeout "$POLL_TIMEOUT" \
    >> "$LOG_FILE" 2>&1 &
  local new_pid=$!
  disown "$new_pid" 2>/dev/null || true
  echo "$new_pid" > "$PID_FILE"

  local waited=0
  while (( waited < 50 )); do
    if ! pid_running "$new_pid"; then
      echo "agent-bus: server exited immediately; see ${LOG_FILE}" >&2
      rm -f "$PID_FILE"
      return 1
    fi
    if curl -fsS -o /dev/null "$HEALTH_URL" 2>/dev/null; then
      echo "agent-bus: started (pid ${new_pid}, listen ${LISTEN}, data-dir ${DATA_DIR})"
      return 0
    fi
    sleep 0.1
    waited=$((waited + 1))
  done

  echo "agent-bus: pid ${new_pid} started but ${HEALTH_URL} did not answer within 5s; see ${LOG_FILE}" >&2
  return 1
}

main() {
  local sub="${1:-}"
  if [[ -z "$sub" ]]; then
    usage
    return 2
  fi
  shift
  case "$sub" in
    start) cmd_start "$@" ;;
    status) cmd_status ;;
    stop) cmd_stop ;;
    *)
      usage
      return 2
      ;;
  esac
}

main "$@"
