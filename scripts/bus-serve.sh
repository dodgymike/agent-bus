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
# THE BUS SERVES https AND ONLY https (invariant 11, MTLS-LISTENER, 2026-08-07).
# The probe below is therefore an https request whose certificate is VERIFIED
# against the bus's own self-signed certificate in the data dir -- see the
# HEALTH_URL block. `start` prints the certificate fingerprint an agent must
# pass to `agent-busctl enrol --bus-fingerprint`, because there is deliberately
# no trust-on-first-use. There is no plaintext mode and no flag that asks for
# one; a bus with unusable key material REFUSES TO START rather than degrading.
#
# This script's probe uses curl because it runs on a workstation. The probe
# INSIDE the container image is `agent-bus healthcheck` (a subcommand on the
# server binary), because the runtime image ships no HTTP client that can be
# told to trust one self-signed certificate -- see cmd/agent-bus/healthcheck.go.
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

# ---------------------------------------------------------------------------
# THE HEALTH PROBE IS https, AND ITS CERTIFICATE IS VERIFIED (MTLS-VERIFY).
#
# The bus serves TLS and ONLY TLS (invariant 11, MTLS-LISTENER): there is no
# plaintext listener, so the http:// probe this script used until 2026-08-07
# could no longer succeed against any bus it started. Both halves had to move
# in one commit, or `start` would report a healthy bus as failed and every
# other task's server-startup proof would break with it.
#
# --cacert, NOT -k. The bus certificate is SELF-SIGNED and there is no CA
# anywhere in this design, so the certificate the bus writes into its own data
# directory IS the trust anchor: pointing curl at that exact file is a full
# verification against exactly one certificate. `curl -k` would also "work",
# and would verify nothing at all -- invariant 11 is explicit that certificate
# verification is never disabled to make something work. Because this is a
# real verification it also checks the hostname against the certificate's SANs
# and the validity period, so an expired bus certificate correctly reports
# unhealthy rather than serving quietly to a probe that ignores it.
#
# PROBE_ADDR, not LISTEN: a wildcard bind (":8080", "0.0.0.0:8080",
# "[::]:8080") is a legal -listen but is not an address anything dials, and
# internal/buscert deliberately leaves it out of the certificate's SANs. The
# loopback form is what the certificate always names, and it reaches a
# wildcard-bound bus. This also fixes a latent bug in the old http:// probe,
# which built the unusable URL "http://:8080/healthz" in that case.
# ---------------------------------------------------------------------------
CERT_FILE="${DATA_DIR}/bus-tls.crt"

probe_addr() {
  local host="${LISTEN%:*}" port="${LISTEN##*:}"
  case "$host" in
    ""|"0.0.0.0"|"[::]"|"::") host="127.0.0.1" ;;
  esac
  printf '%s:%s' "$host" "$port"
}

PROBE_ADDR="$(probe_addr)"
HEALTH_URL="https://${PROBE_ADDR}/healthz"

# health_probe — true if the bus answers 200 on /healthz over a VERIFIED TLS
# connection. Quiet: callers decide what to print.
health_probe() {
  [[ -r "$CERT_FILE" ]] || return 1
  curl -fsS --cacert "$CERT_FILE" -o /dev/null "$HEALTH_URL" 2>/dev/null
}

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
  if health_probe; then
    echo "agent-bus: running (pid ${pid}, listen ${LISTEN}, data-dir ${DATA_DIR}, tls ${CERT_FILE})"
    return 0
  fi
  if [[ ! -r "$CERT_FILE" ]]; then
    echo "agent-bus: process running (pid ${pid}) but its certificate ${CERT_FILE} is not readable, so the https probe cannot verify anything. The bus writes it on first start; check AGENT_BUS_DATA_DIR points at the dir this process was started with" >&2
    return 1
  fi
  echo "agent-bus: process running (pid ${pid}) but ${HEALTH_URL} did not answer (verified against ${CERT_FILE})"
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

  mkdir -p "$RUN_DIR" "$(dirname "$BIN_FILE")"

  # DATA_DIR holds agent credentials once ENROL ships (main.go: "0o700: the
  # store holds agent credentials"). Go's os.MkdirAll is a no-op — it does
  # NOT chmod — when the target already exists, so if this wrapper leaves the
  # directory pre-created at the ambient umask, the server's own MkdirAll(...,
  # 0o700) inside run() never actually secures it. Create + chmod it
  # explicitly here rather than relying on the server to fix it up.
  mkdir -p "$DATA_DIR"
  chmod 700 "$DATA_DIR"

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
    if health_probe; then
      # The fingerprint is PUBLIC (it is the digest of a certificate sent to
      # every client on every handshake) and it is the value an agent must pass
      # as --bus-fingerprint, because there is deliberately no
      # trust-on-first-use. Printing it here is what makes `bus-serve.sh start`
      # followed by `agent-busctl enrol` a two-step an agent can actually do
      # without going and reading the log.
      local fp
      fp="$(grep -o 'bus_cert_fingerprint=[0-9a-f]\{64\}' "$LOG_FILE" | tail -1 | cut -d= -f2 || true)"
      echo "agent-bus: started (pid ${new_pid}, listen ${LISTEN}, data-dir ${DATA_DIR})"
      echo "agent-bus: serving https ONLY (invariant 11: there is no plaintext listener)"
      echo "agent-bus:   url         https://${PROBE_ADDR}"
      echo "agent-bus:   certificate ${CERT_FILE}"
      if [[ -n "$fp" ]]; then
        echo "agent-bus:   fingerprint ${fp}"
        echo "agent-bus: enrol with: agent-busctl enrol --bus https://${PROBE_ADDR} --bus-fingerprint ${fp} --name <name>"
      fi
      return 0
    fi
    sleep 0.1
    waited=$((waited + 1))
  done

  echo "agent-bus: pid ${new_pid} started but ${HEALTH_URL} did not answer within 5s; see ${LOG_FILE}" >&2
  if [[ ! -r "$CERT_FILE" ]]; then
    echo "agent-bus: the bus certificate ${CERT_FILE} is missing or unreadable. The bus serves TLS ONLY and refuses to start without usable key material, so it most likely never started; the log will name the file it refused over" >&2
  fi
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
