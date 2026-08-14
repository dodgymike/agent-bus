#!/usr/bin/env bash
# RELAY-25: deterministic three-bus federation smoke test.
#
# This runs a direct sender-agent -> recipient-agent message A -> B -> C entirely
# on loopback (not broadcast). It proves the compiled CLI and server wiring,
# durable/idempotent send, exactly-once delivery, and progressive audit bus_path
# values. It does NOT prove SSH tunnel bring-up or flap recovery,
# NAT/keepalive behaviour, latency against RetryHorizonCeiling, or certificate
# pinning across a real tunnel. RELAY-25-FU-REALHOST owns that proof.
#
# This script intentionally describes the supported surface as it SHOULD be and
# therefore cannot pass yet. It fails loudly at the first unavailable step:
#
#   * CLI-11 must add `agent-bus key export-public --data-dir ... --json`.
#   * INVITE-CLIENT/INVITE-GATE must make `agent-busctl enrol --invite-file ...`
#     redeem an operator-minted JSON invite without exposing its secret in argv.
#   * CLI-6 must add `agent-bus log --data-dir ... --json`, with ordered
#     `bus_path` on every audit record.
#   * RELAY-20, RELAY-21, and RELAY-24 must mount, accept, and compose the relay
#     runtime after RELAY-41 supplies next-hop certificate pins.
#
# The intended compiled surfaces are `invite mint`, `key export-public`,
# `peer add`, `enrol`, `send`, `watch`, and `log`; the script stops at the first
# unavailable one. On success it removes its owned /tmp roots. On failure it
# stops only its owned buses and preserves those roots and artifacts for review;
# a rerun refuses to reuse them.
#
# There are deliberately no direct HTTP calls, certificate/key-file scraping,
# raw WAL/audit parsing, ad-hoc helper binaries, or tracked data-directory use.
# Server lifecycle goes only through the sanctioned scripts/bus-serve.sh, and
# every bus/client capability goes through a compiled project command.
set -euo pipefail

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)"
readonly REPO_ROOT="$(cd "${SCRIPT_DIR}/.." >/dev/null 2>&1 && pwd)"
readonly SERVE="${REPO_ROOT}/scripts/bus-serve.sh"

readonly ROOT_A=/tmp/fed-smoke-a
readonly ROOT_B=/tmp/fed-smoke-b
readonly ROOT_C=/tmp/fed-smoke-c
readonly SENTINEL=.fed-smoke-owner
readonly OWNER_TOKEN="fed-smoke:$$:$(date +%s)"

readonly RUN_A="${ROOT_A}/run"
readonly RUN_B="${ROOT_B}/run"
readonly RUN_C="${ROOT_C}/run"
readonly DATA_A="${ROOT_A}/data"
readonly DATA_B="${ROOT_B}/data"
readonly DATA_C="${ROOT_C}/data"
readonly IDENTITY_A="${ROOT_A}/identity"
readonly IDENTITY_C="${ROOT_C}/identity"
readonly URL_A=https://127.0.0.1:9101
readonly URL_B=https://127.0.0.1:9102
readonly URL_C=https://127.0.0.1:9103
readonly CTL="${ROOT_A}/bin/agent-busctl"
readonly SERVER_A="${RUN_A}/bin/agent-bus"
readonly SERVER_B="${RUN_B}/bin/agent-bus"
readonly SERVER_C="${RUN_C}/bin/agent-bus"

die() {
  printf 'fed-smoke: ERROR: %s\n' "$*" >&2
  exit 1
}

note() {
  printf 'fed-smoke: %s\n' "$*" >&2
}

claim_root() {
  local root="$1"
  if [[ -e "$root" ]]; then
    die "$root already exists; refusing to reuse or remove data this run does not own"
  fi
  mkdir -m 0700 -- "$root"
  printf '%s\n' "$OWNER_TOKEN" >"${root}/${SENTINEL}"
  chmod 0600 "${root}/${SENTINEL}"
}

owns_root() {
  local root="$1" token=""
  [[ "$root" == /tmp/fed-smoke-a || "$root" == /tmp/fed-smoke-b || "$root" == /tmp/fed-smoke-c ]] || return 1
  [[ -f "${root}/${SENTINEL}" ]] || return 1
  IFS= read -r token <"${root}/${SENTINEL}" || return 1
  [[ "$token" == "$OWNER_TOKEN" ]]
}

serve_bus() {
  local run_dir="$1" data_dir="$2" listen="$3" action="$4"
  AGENT_BUS_RUN_DIR="$run_dir" \
    AGENT_BUS_DATA_DIR="$data_dir" \
    AGENT_BUS_LISTEN="$listen" \
    "$SERVE" "$action"
}

stop_owned_bus() {
  local root="$1" run_dir="$2" data_dir="$3" listen="$4"
  owns_root "$root" || return 0
  serve_bus "$run_dir" "$data_dir" "$listen" stop >/dev/null 2>&1 || true
}

remove_owned_root() {
  local root="$1"
  owns_root "$root" || die "cleanup refused to remove unowned path $root"
  rm -rf -- "$root"
}

cleanup() {
  local status=$?
  trap - EXIT INT TERM
  set +e

  # Reverse the configured start order. These are the only processes this
  # script may signal, and bus-serve validates its own pidfile before doing so.
  stop_owned_bus "$ROOT_A" "$RUN_A" "$DATA_A" 127.0.0.1:9101
  stop_owned_bus "$ROOT_B" "$RUN_B" "$DATA_B" 127.0.0.1:9102
  stop_owned_bus "$ROOT_C" "$RUN_C" "$DATA_C" 127.0.0.1:9103

  if (( status == 0 )); then
    owns_root "$ROOT_A" && remove_owned_root "$ROOT_A"
    owns_root "$ROOT_B" && remove_owned_root "$ROOT_B"
    owns_root "$ROOT_C" && remove_owned_root "$ROOT_C"
  else
    note "FAILED (exit $status); stopped owned buses and preserved artifacts:"
    note "  $ROOT_A"
    note "  $ROOT_B"
    note "  $ROOT_C"
  fi
  exit "$status"
}

preflight() {
  local -a stale_roots=() occupied_ports=()
  local root port

  for root in "$ROOT_A" "$ROOT_B" "$ROOT_C"; do
    [[ ! -e "$root" && ! -L "$root" ]] || stale_roots+=("$root")
  done
  command -v ss >/dev/null 2>&1 ||
    die "preflight requires ss to verify that loopback ports 9101-9103 are free"
  for port in 9101 9102 9103; do
    [[ -z "$(ss -H -ltn "sport = :$port")" ]] || occupied_ports+=("$port")
  done

  if (( ${#stale_roots[@]} == 0 && ${#occupied_ports[@]} == 0 )); then
    return 0
  fi
  note "preflight refused to alter existing resources"
  if (( ${#stale_roots[@]} > 0 )); then
    note "stale smoke roots: ${stale_roots[*]}"
    note "manual remediation: inspect them, then remove only confirmed stale roots with: rm -rf -- ${stale_roots[*]}"
  fi
  if (( ${#occupied_ports[@]} > 0 )); then
    note "occupied loopback ports: ${occupied_ports[*]}"
    note "manual remediation: identify their owners with: ss -ltnp 'sport = :9101 or sport = :9102 or sport = :9103'"
    note "then stop those owner processes through their normal lifecycle command; this script will not kill them"
  fi
  die "resolve every preflight finding above, then rerun"
}

json_string() {
  local document="$1" field="$2" value=""
  value="$(jq -er --arg field "$field" '.[$field] | strings | select(length > 0)' <<<"$document")" ||
    die "compiled command did not return a non-empty JSON string field '$field'"
  printf '%s' "$value"
}

require_ok() {
  local document="$1" operation="$2"
  jq -e '.ok == true' >/dev/null <<<"$document" ||
    die "$operation did not return {\"ok\":true}"
}

mint_invite() {
  local server="$1" data_dir="$2" url="$3" label="$4" result=""
  result="$("$server" invite mint -data-dir "$data_dir" -bus-address "$url" -label "$label" -json)" ||
    die "invite mint failed for $label"
  require_ok "$result" "invite mint for $label"
  printf '%s' "$result"
}

export_signing_key() {
  local server="$1" data_dir="$2" bus_name="$3" result=""
  note "exporting $bus_name signing public key through CLI-11"
  result="$("$server" key export-public --data-dir "$data_dir" --json)" ||
    die "BLOCKED: CLI-11 agent-bus key export-public is unavailable for $bus_name"
  require_ok "$result" "signing-key export for $bus_name"
  json_string "$result" public_key
}

add_route() {
  local server="$1" data_dir="$2" peer_id="$3" peer_url="$4" peer_fp="$5"
  shift 5
  local args=(peer add -data-dir "$data_dir" -bus-id "$peer_id" -url "$peer_url" -tls-fingerprint "$peer_fp" -json)
  local destination
  for destination in "$@"; do
    args+=(-route-for "$destination")
  done
  "$server" "${args[@]}" >/dev/null
}

add_trust() {
  local server="$1" data_dir="$2" origin_id="$3" signing_key="$4"
  "$server" peer add -data-dir "$data_dir" -bus-id "$origin_id" \
    -signing-key "$signing_key" -json >/dev/null
}

enrol_agent() {
  local identity_dir="$1" invite_file="$2" name="$3" result=""
  result="$("$CTL" --identity "$identity_dir" --json enrol --invite-file "$invite_file" --name "$name")" ||
    die "BLOCKED: invite redemption is unavailable while enrolling $name"
  require_ok "$result" "enrol $name"
  json_string "$result" agent_id
}

read_audit() {
  local server="$1" data_dir="$2" output="$3" bus_name="$4"
  "$server" log --data-dir "$data_dir" --json >"$output" ||
    die "BLOCKED: CLI-6 agent-bus log is unavailable for $bus_name"
}

assert_audit_path() {
  local file="$1" bus_name="$2" message_id="$3" expected_path="$4"
  local total_count="" path_count=""
  total_count="$(jq -s --arg id "$message_id" '
    [.[] | if type == "array" then .[] else . end |
      select(.message_id == $id)] | length
  ' "$file")" || die "$bus_name audit output is not valid JSON/NDJSON"
  [[ "$total_count" == 1 ]] ||
    die "$bus_name audit has $total_count total records for $message_id; want exactly 1"
  path_count="$(jq -s --arg id "$message_id" --argjson path "$expected_path" '
    [.[] | if type == "array" then .[] else . end |
      select(.message_id == $id and .bus_path == $path)] | length
  ' "$file")" || die "$bus_name audit output is not valid JSON/NDJSON"
  [[ "$path_count" == 1 ]] ||
    die "$bus_name audit record for $message_id does not have bus_path=$expected_path"
}

classify_zero_delivery() {
  local file="$1" message_id="$2" expected_path="$3"
  local path_count="" message_count=""
  path_count="$(jq -s --arg id "$message_id" --argjson path "$expected_path" '
    [.[] | if type == "array" then .[] else . end |
      select(.message_id == $id and .bus_path == $path)] | length
  ' "$file" 2>/dev/null)" || return 2
  message_count="$(jq -s --arg id "$message_id" '
    [.[] | if type == "array" then .[] else . end |
      select(.message_id == $id)] | length
  ' "$file" 2>/dev/null)" || return 2
  if (( path_count == 1 && message_count == 1 )); then
    return 0
  elif (( message_count > 1 )); then
    return 3
  fi
  # A record without the complete path is evidence that the end-to-end relay
  # was not established, whether the message is absent or only partially seen.
  [[ "$message_count" =~ ^[0-9]+$ ]] || return 2
  return 1
}

run_classifier_self_test() {
  local work
  work="$(mktemp -d)"
  trap 'rm -rf -- "$work"' RETURN
  printf '%s\n' '{"message_id":"m1","bus_path":["a","b","c"]}' >"$work/complete"
  printf '%s\n%s\n' \
    '{"message_id":"m1","bus_path":["a","b","c"]}' \
    '{"message_id":"m1","bus_path":["a","b","c"]}' >"$work/duplicate"
  printf '%s\n%s\n' \
    '{"message_id":"m1","bus_path":["a","b","c"]}' \
    '{"message_id":"m1","bus_path":["a","b"]}' >"$work/mixed-duplicate"
  printf '%s\n' '{"message_id":"m1","bus_path":["a","b"]}' >"$work/partial"
  printf '%s\n' '{not-json' >"$work/malformed"
  classify_zero_delivery "$work/complete" m1 '["a","b","c"]' || die "classifier self-test: complete path"
  if classify_zero_delivery "$work/duplicate" m1 '["a","b","c"]'; then
    die "classifier self-test: duplicate complete paths classified as watch timeout"
  else
    [[ $? == 3 ]] || die "classifier self-test: duplicate path classification"
  fi
  if classify_zero_delivery "$work/mixed-duplicate" m1 '["a","b","c"]'; then
    die "classifier self-test: mixed duplicate paths classified as watch timeout"
  else
    [[ $? == 3 ]] || die "classifier self-test: mixed duplicate classification"
  fi
  if classify_zero_delivery "$work/partial" m1 '["a","b","c"]'; then
    die "classifier self-test: partial path classified as complete"
  else
    [[ $? == 1 ]] || die "classifier self-test: partial path classification"
  fi
  if classify_zero_delivery "$work/malformed" m1 '["a","b","c"]'; then
    die "classifier self-test: malformed audit classified as complete"
  else
    [[ $? == 2 ]] || die "classifier self-test: malformed audit classification"
  fi
  note "PASS: zero-delivery audit classifier fixtures"
}

if [[ "${1:-}" == "--self-test" ]]; then
  command -v jq >/dev/null 2>&1 || die "jq is required for classifier self-test"
  run_classifier_self_test
  exit 0
fi

command -v jq >/dev/null 2>&1 || die "jq is required to validate compiled --json output"
[[ -x "$SERVE" ]] || die "sanctioned lifecycle command is not executable: $SERVE"

preflight
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
claim_root "$ROOT_A"
claim_root "$ROOT_B"
claim_root "$ROOT_C"
mkdir -m 0700 -- "$IDENTITY_A" "$IDENTITY_C" "${ROOT_A}/bin"

note "building the compiled agent CLI into an owned isolated directory"
(cd "$REPO_ROOT" && go build -o "$CTL" ./cmd/agent-busctl)

# First start creates each server-authoritative bus identity, TLS certificate,
# and signing key. Each server binary is built into its own run directory.
note "creating three isolated bus identities on loopback ports 9101-9103"
serve_bus "$RUN_A" "$DATA_A" 127.0.0.1:9101 start >/dev/null
serve_bus "$RUN_B" "$DATA_B" 127.0.0.1:9102 start >/dev/null
serve_bus "$RUN_C" "$DATA_C" 127.0.0.1:9103 start >/dev/null
serve_bus "$RUN_A" "$DATA_A" 127.0.0.1:9101 stop >/dev/null
serve_bus "$RUN_B" "$DATA_B" 127.0.0.1:9102 stop >/dev/null
serve_bus "$RUN_C" "$DATA_C" 127.0.0.1:9103 stop >/dev/null

# Invite minting and peer configuration are offline operations protected by the
# data-directory lock. The invite JSON is the trust anchor passed intact to the
# compiled client; this script never reconstructs it from server files.
invite_a="$(mint_invite "$SERVER_A" "$DATA_A" "$URL_A" fed-smoke-sender)"
invite_c="$(mint_invite "$SERVER_C" "$DATA_C" "$URL_C" fed-smoke-recipient)"

bus_a="$(json_string "$invite_a" bus_id)"
bus_c="$(json_string "$invite_c" bus_id)"
fp_a="$(json_string "$invite_a" bus_cert_fingerprint)"
fp_c="$(json_string "$invite_c" bus_cert_fingerprint)"

# B needs an invite only as a compiled, trustworthy way to obtain its
# server-minted id and certificate fingerprint; no agent redeems it.
invite_b="$(mint_invite "$SERVER_B" "$DATA_B" "$URL_B" fed-smoke-peer-metadata)"
bus_b="$(json_string "$invite_b" bus_id)"
fp_b="$(json_string "$invite_b" bus_cert_fingerprint)"

# Invite blobs contain bearer secrets. Keep them out of argv and preserve them
# only in the sentinel-owned 0700 roots on failure.
invite_file_a="${IDENTITY_A}/invite.json"
invite_file_c="${IDENTITY_C}/invite.json"
printf '%s\n' "$invite_a" >"$invite_file_a"
printf '%s\n' "$invite_c" >"$invite_file_c"
chmod 0600 "$invite_file_a" "$invite_file_c"

signing_a="$(export_signing_key "$SERVER_A" "$DATA_A" A)"
signing_b="$(export_signing_key "$SERVER_B" "$DATA_B" B)"
signing_c="$(export_signing_key "$SERVER_C" "$DATA_C" C)"

note "configuring offline routes and trust as independent records"
# A reaches C through B. A also pins C independently of that next-hop route.
add_route "$SERVER_A" "$DATA_A" "$bus_b" "$URL_B" "$fp_b" "$bus_c"
add_trust "$SERVER_A" "$DATA_A" "$bus_b" "$signing_b"
add_trust "$SERVER_A" "$DATA_A" "$bus_c" "$signing_c"

# B is the only adjacent hop to both endpoint buses.
add_route "$SERVER_B" "$DATA_B" "$bus_a" "$URL_A" "$fp_a"
add_trust "$SERVER_B" "$DATA_B" "$bus_a" "$signing_a"
add_route "$SERVER_B" "$DATA_B" "$bus_c" "$URL_C" "$fp_c"
add_trust "$SERVER_B" "$DATA_B" "$bus_c" "$signing_c"

# C has a route to B but deliberately NO route to A: A is trust-only here.
add_route "$SERVER_C" "$DATA_C" "$bus_b" "$URL_B" "$fp_b"
add_trust "$SERVER_C" "$DATA_C" "$bus_b" "$signing_b"
add_trust "$SERVER_C" "$DATA_C" "$bus_a" "$signing_a"

# Downstream first prevents an upstream forwarder from racing an unavailable
# next hop during startup.
note "restarting configured buses in C, B, A order"
serve_bus "$RUN_C" "$DATA_C" 127.0.0.1:9103 start >/dev/null
serve_bus "$RUN_B" "$DATA_B" 127.0.0.1:9102 start >/dev/null
serve_bus "$RUN_A" "$DATA_A" 127.0.0.1:9101 start >/dev/null

sender_id="$(enrol_agent "$IDENTITY_A" "$invite_file_a" fed-smoke-sender)"
recipient_id="$(enrol_agent "$IDENTITY_C" "$invite_file_c" fed-smoke-recipient)"

readonly PAYLOAD='relay-25:a-to-c:v1'
readonly IDEMPOTENCY_KEY='fed-smoke-relay-25-a-to-c-v1'

note "sending the same logical message twice with one deterministic idempotency key"
send_one="$("$CTL" --identity "$IDENTITY_A" --as "$sender_id" --json send \
  --idempotency-key "$IDEMPOTENCY_KEY" "$recipient_id" "$PAYLOAD")"
require_ok "$send_one" "first send"
send_two="$("$CTL" --identity "$IDENTITY_A" --as "$sender_id" --json send \
  --idempotency-key "$IDEMPOTENCY_KEY" "$recipient_id" "$PAYLOAD")"
require_ok "$send_two" "idempotent retry"

message_id="$(json_string "$send_one" message_id)"
[[ "$(json_string "$send_two" message_id)" == "$message_id" ]] ||
  die "idempotent retry returned a different message_id"
jq -e '.replayed == false' >/dev/null <<<"$send_one" || die "first send unexpectedly reported replayed"
jq -e '.replayed == true' >/dev/null <<<"$send_two" || die "second send did not report replayed"

watch_file="${ROOT_C}/recipient-watch.ndjson"
note "bounded replay-watch on C; waiting for one retained logical message"
set +e
"$CTL" --identity "$IDENTITY_C" --as "$recipient_id" --json watch \
  --replay --no-cursor --for 15s --poll-timeout 1s >"$watch_file"
watch_status=$?
set -e
[[ "$watch_status" == 0 || "$watch_status" == 8 ]] ||
  die "recipient watch failed with exit $watch_status (expected success or bounded-timeout exit 8)"

delivery_count="$(jq -s --arg id "$message_id" '[.[] | select(.message_id == $id)] | length' "$watch_file")" ||
  die "recipient watch output is not valid NDJSON"
if [[ "$delivery_count" == 0 ]]; then
  # Stabilize C's durable audit before attributing a bounded-watch miss. An
  # exact terminal path proves relay delivery and makes this an environmental
  # watch timeout, not a relay failure.
  stop_owned_bus "$ROOT_A" "$RUN_A" "$DATA_A" 127.0.0.1:9101
  stop_owned_bus "$ROOT_B" "$RUN_B" "$DATA_B" 127.0.0.1:9102
  stop_owned_bus "$ROOT_C" "$RUN_C" "$DATA_C" 127.0.0.1:9103
  audit_c="${ROOT_C}/audit.ndjson"
  read_audit "$SERVER_C" "$DATA_C" "$audit_c" C
  if classify_zero_delivery "$audit_c" "$message_id" "[\"$bus_a\",\"$bus_b\",\"$bus_c\"]"; then
    die "WATCH TIMEOUT/environmental: C audit contains $message_id with bus_path=[$bus_a,$bus_b,$bus_c]; relay delivery succeeded but watch observed zero deliveries"
  else
    case $? in
      1) die "relay not established: C audit lacks $message_id with complete bus_path=[$bus_a,$bus_b,$bus_c]" ;;
      2) die "unattributable zero-delivery result: C audit output is malformed" ;;
      3) die "relay delivery invariant failed: C audit contains duplicate complete paths for $message_id" ;;
      *) die "unattributable zero-delivery result: classifier failed unexpectedly" ;;
    esac
  fi
elif [[ "$delivery_count" != 1 ]]; then
  die "recipient observed $delivery_count deliveries of $message_id; want exactly 1"
fi
jq -e --arg id "$message_id" --arg text "$PAYLOAD" --arg from "$sender_id" \
  --arg to "$recipient_id" \
  --argjson path "[\"$bus_a\",\"$bus_b\",\"$bus_c\"]" \
  'select(.message_id == $id and .text == $text and .from == $from and
    .broadcast == false and .to == [$to] and .bus_path == $path)' \
  "$watch_file" >/dev/null ||
  die "recipient message identity, audience, body, or three-hop bus_path is wrong"

# Read stable offline audit files only through the compiled reader. Stopping
# first avoids racing an append and does not weaken the end-to-end assertion.
serve_bus "$RUN_A" "$DATA_A" 127.0.0.1:9101 stop >/dev/null
serve_bus "$RUN_B" "$DATA_B" 127.0.0.1:9102 stop >/dev/null
serve_bus "$RUN_C" "$DATA_C" 127.0.0.1:9103 stop >/dev/null

audit_a="${ROOT_A}/audit.ndjson"
audit_b="${ROOT_B}/audit.ndjson"
audit_c="${ROOT_C}/audit.ndjson"
read_audit "$SERVER_A" "$DATA_A" "$audit_a" A
read_audit "$SERVER_B" "$DATA_B" "$audit_b" B
read_audit "$SERVER_C" "$DATA_C" "$audit_c" C

assert_audit_path "$audit_a" A "$message_id" "[\"$bus_a\"]"
assert_audit_path "$audit_b" B "$message_id" "[\"$bus_a\",\"$bus_b\"]"
assert_audit_path "$audit_c" C "$message_id" "[\"$bus_a\",\"$bus_b\",\"$bus_c\"]"

note "PASS: $message_id delivered exactly once over $bus_a -> $bus_b -> $bus_c"
