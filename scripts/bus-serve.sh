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
# no trust-on-first-use -- and it computes that value FROM THE CERTIFICATE FILE,
# never from the log (see cert_fingerprint). There is no plaintext mode and no
# flag that asks for one; a bus with unusable key material REFUSES TO START
# rather than degrading.
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
#           3 not running (no usable pidfile: absent, stale, or not holding a
#           pid); 2 bad usage
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

# ---------------------------------------------------------------------------
# THE FINGERPRINT IS DERIVED FROM THE CERTIFICATE. NEVER FROM THE LOG.
#
# This function used to be a `grep bus_cert_fingerprint= "$LOG_FILE" | tail -1`.
# That was a P1: the log is a MUTABLE, world-writable-by-construction artefact
# living in RUN_DIR (default /tmp/agent-bus), so a local attacker who owns that
# directory — or who can simply append to the file while `start` is polling —
# plants a line of their own, wins `tail -1`, and the wrapper hands the operator
# a confident, paste-ready `--bus-fingerprint` naming the ATTACKER's
# certificate. That is precisely the MITM that "there is deliberately no
# trust-on-first-use" (invariant 11) exists to prevent, achieved without ever
# touching the bus. A wrong trust anchor delivered confidently is worse than no
# trust anchor at all.
#
# $CERT_FILE is the authority and the only authority: it is the same file
# health_probe passes to `curl --cacert`, it is what the bus actually serves,
# and internal/buscert defines the published fingerprint as sha256 over the DER
# of that leaf (buscert.FingerprintOf). Computing it here from the certificate
# means the value printed is true by construction — an attacker who could change
# it would already have had to replace the certificate the bus is serving.
#
# A log line is a convenience artefact. It is never a trust root, and nothing in
# this script may reconstruct that shortcut: if the certificate cannot be read,
# the correct output is a REFUSAL that names the remedy, not a fallback.
#
# openssl is the primary path; the awk/base64/sha256sum path is a coreutils
# fallback for a box WITHOUT openssl — not for a box where openssl ran and said
# no. Both compute the identical thing — sha256 of the DER of the first (leaf)
# CERTIFICATE block in the file — and both read only $CERT_FILE. The result is
# validated as 64 lowercase hex before it is printed, because a half-parsed
# digest pasted into `--bus-fingerprint` must fail loudly here rather than at a
# handshake.
#
# THE `curl --cacert` GATE IN health_probe IS LOAD-BEARING FOR THE FALLBACK, and
# a future caller must not remove it by accident. openssl PARSES the certificate
# and so rejects a non-certificate outright; the coreutils fallback does no X.509
# parsing at all, so a syntactically valid PEM block wrapping arbitrary base64
# would yield a well-formed but WRONG 64-hex value. On a box WITH openssl that is
# now unreachable twice over — the fallback is not entered at all, and this
# function is only ever called after health_probe completed a real TLS
# verification against this exact file (curl exits 77 on anything else). On a box
# WITHOUT openssl only the second guard remains, so calling cert_fingerprint from
# a path that has NOT verified $CERT_FILE — from cmd_status, say — reopens it.
# ---------------------------------------------------------------------------
cert_fingerprint() {
  [[ -r "$CERT_FILE" ]] || return 1
  local raw="" fp="" have_openssl=0
  if command -v openssl >/dev/null 2>&1; then
    have_openssl=1
    raw="$(openssl x509 -in "$CERT_FILE" -noout -fingerprint -sha256 2>/dev/null)" || raw=""
    fp="${raw##*=}"   # "SHA256 Fingerprint=AB:CD:..." -> "AB:CD:..."
  fi
  # The fallback runs ONLY when openssl is ABSENT, never when it is present and
  # FAILED. Both gates caught the earlier `if [[ -z "$fp" ]]` here: openssl
  # parses the certificate, so its refusal is a real verdict, and falling back
  # from it would answer a question openssl had just answered "no" to — with a
  # weaker method that cannot parse anything. A PEM block wrapping 200 'A's was
  # shown to yield a well-formed, WRONG 64-hex value that way.
  if (( have_openssl == 0 )) && [[ -z "$fp" ]]; then
    # The fallback must fail CLOSED, and getting this wrong reproduces the very
    # defect this function exists to fix. A first cut piped the extraction
    # straight into sha256sum, so an empty or non-PEM $CERT_FILE produced
    # e3b0c44298fc1c14... -- the sha256 of the EMPTY STRING -- which is a
    # perfectly well-formed 64-hex value that passes the check below and would
    # have been printed as a trust anchor. A truncated block (BEGIN, no END)
    # likewise digested a partial certificate. So the block must be COMPLETE
    # (awk exits non-zero otherwise), non-trivial, and valid base64 before
    # anything is hashed.
    local body=""
    body="$(awk '
      /-----BEGIN CERTIFICATE-----/ { inblock = 1; next }
      /-----END CERTIFICATE-----/   { if (inblock) complete = 1; exit }
      inblock                       { print }
      END                           { exit !complete }
    ' "$CERT_FILE" 2>/dev/null)" || body=""
    if (( ${#body} >= 100 )); then
      fp="$(printf '%s\n' "$body" | base64 -d 2>/dev/null | sha256sum 2>/dev/null | cut -d' ' -f1)" || fp=""
    fi
  fi
  fp="$(printf '%s' "$fp" | tr -d ':[:space:]' | tr 'A-F' 'a-f')"
  [[ "$fp" =~ ^[0-9a-f]{64}$ ]] || return 1
  printf '%s' "$fp"
}

# pid_running PID — true if a process with that pid exists. The numeric guard is
# belt-and-braces behind read_pid: nothing in this script may reach a `kill`
# with a value it has not proved is a plain positive decimal.
pid_running() {
  local pid="$1"
  [[ "$pid" =~ ^[1-9][0-9]*$ ]] && kill -0 "$pid" 2>/dev/null
}

# read_pid — echoes the pid recorded in PID_FILE, or nothing if it is absent or
# does not hold a plain positive decimal.
#
# THE VALIDATION IS THE POINT, and it is the single choke point for it. Every
# `kill` in this script signals whatever this function returned, so an
# unvalidated pidfile is an arbitrary-signal primitive: a file containing "-1"
# reaches `kill -TERM -1`, which signals EVERY PROCESS THE INVOKING USER OWNS,
# and "0" would signal the whole process group. RUN_DIR defaults to
# /tmp/agent-bus, so the contents of that file are not necessarily ours. Leading
# zeros, "+5", "abc", "1 2", "$(...)" and empty files are all refused for the
# same reason: a pid is a positive decimal or it is not a pid.
#
# The file is read with `cat`, NOT `tr -d '[:space:]'`, and the difference is a
# real one the reviewer gate caught. Stripping whitespace happens BEFORE the
# regex, so it does not sanitise the input — it CONSTRUCTS a new one: "1 2" was
# silently coerced to the pid 12, and a torn or interleaved pidfile ("123\n456")
# to 123456. `stop` would then have signalled a pid that never appeared in the
# file at all, which is exactly what this function claims cannot happen. Command
# substitution already strips the trailing newline the script itself writes, so
# nothing is lost: "4242\n" is still accepted, and anything else fails closed.
#
# Refusing means printing NOTHING, which every caller already handles as "not
# running" — `stop` then clears the bad file and `start` overwrites it. The
# warning goes to stderr so the operator learns why their bus looked stopped.
# Reading with `cat` also keeps an unreadable pidfile's "Permission denied" from
# leaking past the redirection, which `< "$PID_FILE"` did: the redirection is
# applied by the shell before the `2>/dev/null` on the command it feeds.
read_pid() {
  # -f also excludes a FIFO, so `cat` below cannot block `status` forever on a
  # pidfile an attacker replaced with a named pipe.
  [[ -f "$PID_FILE" ]] || return 0
  local raw sz
  raw="$(cat "$PID_FILE" 2>/dev/null)" || raw=""
  # The empty check comes BEFORE the size probe, and the order is the fix rather
  # than a style choice: an unreadable pidfile yields an empty `raw` and returns
  # here, so `wc` never opens it. Both gates caught the previous arrangement
  # reintroducing the very "Permission denied" leak this comment claims to have
  # closed. The redirection is also written `2>/dev/null <` rather than
  # `< ... 2>/dev/null`, because the shell applies redirections left to right and
  # the later form reports the failed open on a stderr it has not silenced yet.
  if [[ -z "$raw" ]]; then
    return 0
  fi
  sz="$(wc -c 2>/dev/null < "$PID_FILE")" || sz=0
  sz="${sz//[^0-9]/}"
  # If exactly one byte is unaccounted for, it must actually BE the trailing
  # newline. Both gates found the earlier "one byte missing, anywhere" allowance
  # too loose: an interior NUL spends it, so "1<NUL>2" with no trailing newline
  # came back as pid 12 — a value not in the file, which is the defect this
  # function exists to prevent, in its fourth costume. `tail -c 1` through a
  # command substitution is empty exactly when that last byte is a newline
  # (which the substitution strips), so a non-empty trailer means the missing
  # byte was somewhere inside the number and the file is refused.
  #
  # The `|| trailer='?'` is the sentinel both gates asked for, and it matters
  # because this is the one external read here that would otherwise fail OPEN:
  # `cat` falls back to "" and `wc` to 0, which both REFUSE, but an empty trailer
  # ACCEPTS. A `tail` that failed for any reason would therefore wave through the
  # very input this check exists to catch. '?' is not a newline, so it refuses.
  local trailer=""
  if (( sz == ${#raw} + 1 )); then
    trailer="$(tail -c 1 "$PID_FILE" 2>/dev/null)" || trailer='?'
  fi
  # THE BYTE COUNT MUST MATCH, and this is the third form of one bug rather than
  # a new one. Command substitution SILENTLY DISCARDS NUL BYTES, so a pidfile
  # holding "1<NUL>2" arrives here as "12" — a pid that is not in the file, which
  # is precisely what removing the `tr` was supposed to end. The bytes the shell
  # dropped are invisible in `raw`, so the only way to notice is to compare
  # against the file's real size. Exactly ONE byte may be absent from `raw`, and
  # the trailer check below requires it to be the trailing NEWLINE this script
  # writes. Stated exactly, because a comment that overstates its guarantee is
  # the defect this whole task is about: a single TRAILING NUL is still tolerated
  # (a substitution strips it indistinguishably from a newline), and that is
  # harmless, because the digits returned are then still literally the ones in
  # the file. What this stops is a byte vanishing from INSIDE the number —
  # "1<NUL>2", a second line, trailing whitespace — all of which fail closed. The
  # length bound is belt-and-braces: no pid_max on any Linux reaches ten digits.
  if (( sz != ${#raw} && sz != ${#raw} + 1 )) || [[ -n "$trailer" ]] || (( ${#raw} > 10 )); then
    echo "agent-bus: ignoring pidfile ${PID_FILE}: it does not hold a bare pid (${sz} bytes on disk), and this script will not pass its contents to kill. Remove the file if the bus is not running" >&2
    return 0
  fi
  if [[ ! "$raw" =~ ^[1-9][0-9]*$ ]]; then
    echo "agent-bus: ignoring pidfile ${PID_FILE}: its contents are not a pid, and this script will not pass them to kill. Remove the file if the bus is not running" >&2
    return 0
  fi
  printf '%s' "$raw"
}

cmd_status() {
  local pid
  pid="$(read_pid)"
  if [[ -z "$pid" ]]; then
    # "no USABLE pidfile": read_pid also returns empty for a pidfile that exists
    # but does not hold a pid, and it has already said so on stderr.
    echo "agent-bus: not running (no usable pidfile at ${PID_FILE})"
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
      # without going and reading anything else -- and cert_fingerprint reads
      # the CERTIFICATE, never the log, for the reason given at its definition.
      local fp
      fp="$(cert_fingerprint)" || fp=""
      echo "agent-bus: started (pid ${new_pid}, listen ${LISTEN}, data-dir ${DATA_DIR})"
      echo "agent-bus: serving https ONLY (invariant 11: there is no plaintext listener)"
      echo "agent-bus:   url         https://${PROBE_ADDR}"
      echo "agent-bus:   certificate ${CERT_FILE}"
      if [[ -n "$fp" ]]; then
        echo "agent-bus:   fingerprint ${fp}"
        # Enrolment is invite-only (invariant 3, enrolmentInviteRequired = true
        # since 3cedcb7, 2026-08-15): the --bus/--bus-fingerprint form this
        # used to print here now gets a 403 every time, since it presents no
        # invite. Minting one takes the data dir's exclusive lock, which this
        # running bus already holds, so it cannot be done while the bus is up
        # -- print the two-step (stop, mint, start again) rather than a
        # command that fails as printed.
        echo "agent-bus: enrolling requires an invite. Already have one? agent-busctl enrol --invite-file <path> --name <name>"
        echo "agent-bus: need one? mint takes this bus's data-dir lock, so stop it first:"
        echo "agent-bus:   scripts/bus-serve.sh stop"
        echo "agent-bus:   ${BIN_FILE} invite mint -data-dir ${DATA_DIR} -bus-address https://${PROBE_ADDR} -ttl 1h -json > invite.json"
        echo "agent-bus:   chmod 0600 invite.json   # it holds a bearer credential; agent-busctl refuses a wider mode"
        echo "agent-bus:   scripts/bus-serve.sh start"
        echo "agent-bus:   agent-busctl enrol --invite-file invite.json --name <name>"
      else
        # No guess, and specifically no falling back to the log: an agent
        # enrolling against a fingerprint from an untrusted source is the whole
        # attack this refusal exists to prevent.
        echo "agent-bus: WARNING could not compute the fingerprint of ${CERT_FILE} (openssl, or base64+sha256sum, is needed). The bus is up; get the value from the certificate itself before enrolling: openssl x509 -in ${CERT_FILE} -noout -fingerprint -sha256. Do NOT take it from ${LOG_FILE}: that file lives in AGENT_BUS_RUN_DIR (default /tmp/agent-bus) and anyone who can write there can plant a bus_cert_fingerprint= line in it. (Reading the BUS's own startup log ON THE BUS HOST is the out-of-band confirmation the docs recommend -- this wrapper's run-dir log is not that.)" >&2
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
