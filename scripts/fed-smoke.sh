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
# This script describes the supported surface and fails loudly at the first
# unavailable step. Each of the steps below has since landed; the list is kept
# because it names the compiled command each stage depends on:
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

# MIXED-VERSION REHEARSAL (RELAY-51) -- per-bus lifecycle command overrides.
#
# bus-serve.sh builds the server from the repository root it ITSELF lives in, so
# pointing a bus at a bus-serve.sh under a DIFFERENT checkout is what runs that
# bus on a DIFFERENT BUILD. That is the only way to rehearse a rollout, because
# a rollout is by definition the window in which two buses run different
# binaries, and a harness that can only run one build can never enter it.
#
# Default is this checkout for all three, so an unset environment is byte-for-byte
# the single-build behaviour every existing invocation and proof_cmd relies on.
#
#   FED_SMOKE_SERVE_A=/path/to/other/checkout/scripts/bus-serve.sh bash scripts/fed-smoke.sh
#
# The three buses are the SENDER (A), the TRANSIT bus (B) and the RECIPIENT (C),
# so A-new/B-old is the emitter-meets-old-reader case and A-old/B-new is the
# readers-first case. See docs/THREE-BUS-DOCKER.md "Rolling out a wire change".
#
# THESE OVERRIDE THE SERVER BUILD ONLY. The compiled agent CLI is always built
# from THIS checkout (see the `go build ./cmd/agent-busctl` below), so a mixed
# run varies the three bus binaries against ONE client. That is the right shape
# for a bus-to-bus wire change, which is what this exists for -- but it means a
# rehearsal of an AGENT-FACING wire change would go falsely green here, because
# the client half never varies. Do not use these to rehearse one.
readonly SERVE_A="${FED_SMOKE_SERVE_A:-$SERVE}"
readonly SERVE_B="${FED_SMOKE_SERVE_B:-$SERVE}"
readonly SERVE_C="${FED_SMOKE_SERVE_C:-$SERVE}"

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

# serve_for_run_dir maps a bus's run directory to the lifecycle command that
# BUILDS AND RUNS it. It fails closed: an unrecognised run directory is a caller
# bug, and silently falling back to the default checkout would run a bus on the
# wrong build while reporting success -- the exact fail-silent direction a
# mixed-version rehearsal cannot tolerate, since "the message got through" would
# then be evidence about a topology nobody configured.
serve_for_run_dir() {
  case "$1" in
    "$RUN_A") printf '%s' "$SERVE_A" ;;
    "$RUN_B") printf '%s' "$SERVE_B" ;;
    "$RUN_C") printf '%s' "$SERVE_C" ;;
    *) die "serve_for_run_dir: unknown run directory $1" ;;
  esac
}

serve_bus() {
  local run_dir="$1" data_dir="$2" listen="$3" action="$4" serve=""
  serve="$(serve_for_run_dir "$run_dir")"
  AGENT_BUS_RUN_DIR="$run_dir" \
    AGENT_BUS_DATA_DIR="$data_dir" \
    AGENT_BUS_LISTEN="$listen" \
    "$serve" "$action"
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

# add_trust writes a TRUST record: the pinned bus signing key, and — for an
# ADJACENT bus only — the INBOUND client-certificate binding.
#
# The optional 5th argument is that binding, and the two fingerprints here are
# OPPOSITE DIRECTIONS. Do not collapse them (internal/relay/peerstore.go:752):
#
#   -tls-fingerprint          OUTBOUND, on the ROUTE record. The SERVER
#   (see add_route)           certificate the hop at -url presents when WE dial
#                             IT. Keyed to an ADDRESS.
#
#   -peer-client-fingerprint  INBOUND, on the TRUST record. The CLIENT
#                             certificate the bus at -bus-id presents when IT
#                             dials US. Keyed to a BUS PRINCIPAL.
#
# The value is the peer's own `invite mint -json` bus_cert_fingerprint, which is
# sha256 over that bus's single leaf certificate — a bus holds exactly ONE
# cert/key pair (internal/buscert: bus-tls.crt), so the same fingerprint is
# correct in both roles. It comes from a compiled command, not from scraping
# bus-tls.crt, which this script's header forbids.
#
# It is passed ONLY for an adjacent bus. A trust record for a bus that never
# opens a connection to us (A<->C here, which is trust-only by design) gets a
# signing pin and no transport binding, because binding one would assert an
# inbound connection this topology never makes.
add_trust() {
  local server="$1" data_dir="$2" origin_id="$3" signing_key="$4"
  local args=(peer add -data-dir "$data_dir" -bus-id "$origin_id" -signing-key "$signing_key" -json)
  # PRESENCE, NOT EMPTINESS — the same rule the CLI itself applies to this flag
  # via fs.Visit (cmd/agent-bus/peer.go:653-676). Treating an EMPTY 5th argument
  # as "not adjacent" is the fail-silent direction: it would write a trust record
  # with NO transport binding while reporting success, losing an admission
  # credential to an unset shell variable. Unreachable today (json_string dies on
  # an empty field), but this script is the worked example operators copy.
  if (($# >= 5)); then
    [[ -n "$5" ]] ||
      die "add_trust: an inbound client-certificate fingerprint was passed for $origin_id but is EMPTY; refusing to write a trust record with no transport binding"
    args+=(-peer-client-fingerprint "$5")
  fi
  "$server" "${args[@]}" >/dev/null
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

# ---------------------------------------------------------------------------
# CROSS-BUS CORRELATION -- READ THIS BEFORE CHANGING AN ASSERTION BELOW.
#
# A MESSAGE ID IS NOT A CROSS-BUS CORRELATOR. Every bus MINTS ITS OWN ids and
# never adopts a peer's (invariant 1), so one logical message is `bus-A-11` on
# A, `bus-B-11` on B and `bus-C-9` on C. Asserting that the SAME id string
# appears in all three audits is UNSATISFIABLE BY CONSTRUCTION, not merely
# unmet: it asserts an invariant VIOLATION. This script made exactly that
# assertion and could never pass, which is the whole of
# RELAY-25-FU-CORRELATION. Do not reintroduce it.
#
# What IS stable across hops is the audit record's `content_sha256`. Per
# PROTOCOL.md 8.6 it is the SHA-256 over the CANONICAL SIGNING BYTES -- the
# exact bytes the sender's Ed25519 signature covers -- and internal/hub/audit.go
# hashes a RELAYED message under the ORIGIN's assignment (see `signedAs`, gated
# on req.relayed), so A, B and C all record the digest of the message AS A
# MINTED IT AND THE SENDER SIGNED IT.
#
# WHAT THIS CORRELATOR PROVES: the record on B and on C is the audit of the very
# bytes the sender signed on A. The canonical bytes cover the origin message id,
# the origin sequence, the sender, the recipient SET, the sender's timestamp and
# the body, so a relay that carried a different message, a different audience or
# a different body yields a different digest and matches NOTHING.
#
# WHAT IT DOES NOT PROVE, stated so it is known rather than assumed:
#   * It is a CORRELATION KEY, not a signature check. This script never verifies
#     the Ed25519 signature, so it shows B and C recorded the same signed BYTES,
#     not that those bytes were validly signed. SIGN-6 owns receive-path
#     verification.
#   * It cannot distinguish two BYTE-IDENTICAL logical messages. This run sends
#     ONE payload under ONE idempotency key, and the "exactly one record" counts
#     below are what make a second copy visible.
#   * A digest is not an ORDERING. The ordered `bus_path` assertions are what
#     prove the A->B->C traversal; the digest only says WHICH message.
#
# THE TWO `content_sha256` FIELDS IN THIS SYSTEM ARE DIFFERENT HASHES AND MUST
# NEVER BE COMPARED TO EACH OTHER. The AUDIT's is the canonical signing digest;
# the one on `agent-busctl watch` output is store.ContentHash(body) -- the BARE
# BODY. Both are 64 lowercase hex characters, so mixing them would fail silently
# at the shell with no type error anywhere. The watch stream is therefore
# correlated on its OWN terms (body text plus audience plus path), never against
# an audit digest.
# ---------------------------------------------------------------------------

# audit_origin_digest returns the canonical content digest the ORIGIN bus
# recorded for the message it minted as $message_id.
#
# This is the ONLY place a message id is used to find a record, and it is sound
# precisely because it is applied to THE BUS THAT MINTED THAT ID. The digest it
# returns is what every downstream bus is then correlated by.
audit_origin_digest() {
  local file="$1" bus_name="$2" message_id="$3" summary="" count=""
  summary="$(jq -s --arg id "$message_id" '
    [.[] | if type == "array" then .[] else . end |
      select(.message_id == $id) | .content_sha256 | strings |
      select(test("^[0-9a-f]{64}$"))] | unique
    | {count: length, digest: (.[0] // "")}
  ' "$file")" || die "$bus_name audit output is not valid JSON/NDJSON"
  count="$(jq -r '.count' <<<"$summary")"
  [[ "$count" == 1 ]] ||
    die "$bus_name audit holds $count distinct canonical content digests for the message it minted as $message_id; want exactly 1"
  json_string "$summary" digest
}

# assert_audit_hop asserts, on ONE bus, the property this smoke test exists to
# prove: that this bus's audit holds EXACTLY ONE record of the correlated
# logical message, that it names the right sender and audience, and that its
# ORDERED bus_path is exactly the hops traversed so far.
#
# It deliberately does NOT constrain the LOCAL message id: that id is this bus's
# own mint and differs on every bus (invariant 1). Correlation is by digest.
assert_audit_hop() {
  local file="$1" bus_name="$2" digest="$3" sender="$4" recipient="$5" expected_path="$6"
  local total_count="" match_count=""
  total_count="$(jq -s --arg d "$digest" '
    [.[] | if type == "array" then .[] else . end |
      select(.content_sha256 == $d)] | length
  ' "$file")" || die "$bus_name audit output is not valid JSON/NDJSON"
  [[ "$total_count" == 1 ]] ||
    die "$bus_name audit holds $total_count records correlated to content digest $digest; want exactly 1 (more than one means the logical message was DUPLICATED on this bus)"
  match_count="$(jq -s --arg d "$digest" --arg from "$sender" --arg to "$recipient" \
    --argjson path "$expected_path" '
    [.[] | if type == "array" then .[] else . end |
      select(.content_sha256 == $d and .sender == $from and .broadcast == false
        and .recipients == [$to] and .bus_path == $path)] | length
  ' "$file")" || die "$bus_name audit output is not valid JSON/NDJSON"
  [[ "$match_count" == 1 ]] ||
    die "$bus_name audit record for content digest $digest is wrong: want sender=$sender recipients=[$recipient] broadcast=false bus_path=$expected_path; got $(jq -c -s --arg d "$digest" '[.[] | if type == "array" then .[] else . end | select(.content_sha256 == $d) | {message_id, sender, broadcast, recipients, bus_path}]' "$file")"
}

# classify_zero_delivery attributes a bounded-watch miss, correlating C's audit
# by the ORIGIN's canonical content digest -- never by the origin's message id,
# which C never adopts (invariant 1; see the correlation note above).
classify_zero_delivery() {
  local file="$1" digest="$2" expected_path="$3"
  local path_count="" message_count=""
  path_count="$(jq -s --arg d "$digest" --argjson path "$expected_path" '
    [.[] | if type == "array" then .[] else . end |
      select(.content_sha256 == $d and .bus_path == $path)] | length
  ' "$file" 2>/dev/null)" || return 2
  message_count="$(jq -s --arg d "$digest" '
    [.[] | if type == "array" then .[] else . end |
      select(.content_sha256 == $d)] | length
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

# audit_row renders one audit record in the shape `agent-bus log --json` emits,
# so the self-test fixtures below are the real shape rather than an approximation
# of it.
audit_row() {
  local id="$1" sender="$2" recipients="$3" broadcast="$4" path="$5" digest="$6"
  jq -nc --arg id "$id" --arg sender "$sender" --argjson recipients "$recipients" \
    --argjson broadcast "$broadcast" --argjson path "$path" --arg digest "$digest" \
    '{message_id:$id, seq:1, sender:$sender, broadcast:$broadcast,
      recipients:$recipients, bus_path:$path, sent_at:"2026-08-15T00:00:00Z",
      size:18, content_sha256:$digest}'
}

# assert_hop_rejects runs assert_audit_hop in a SUBSHELL, so its die() is caught
# instead of ending this process, and REQUIRES it to fail.
#
# This is what keeps the correlation assertions falsifiable. Every fixture it is
# given below is a shape a genuinely broken relay would produce, and an
# assertion that accepted one would be worse than the unsatisfiable id
# comparison it replaced -- that one at least failed honestly.
assert_hop_rejects() {
  local label="$1"
  shift
  if (assert_audit_hop "$@") >/dev/null 2>&1; then
    die "self-test: assert_audit_hop ACCEPTED a broken fixture ($label); the assertion cannot fail and proves nothing"
  fi
}

assert_hop_accepts() {
  local label="$1"
  shift
  (assert_audit_hop "$@") >/dev/null 2>&1 ||
    die "self-test: assert_audit_hop REJECTED the correct fixture ($label)"
}

run_classifier_self_test() {
  local work
  work="$(mktemp -d)"
  trap 'rm -rf -- "$work"' RETURN

  # Two distinct, well-formed canonical digests: D1 is the logical message under
  # test, D2 is any other message that happens to share the bus.
  local d1="1111111111111111111111111111111111111111111111111111111111111111"
  local d2="2222222222222222222222222222222222222222222222222222222222222222"
  local from="bus-a.sender" to="bus-c.recipient"
  local full='["a","b","c"]' short='["a","b"]'

  # --- zero-delivery classifier, correlated by content digest ---------------
  audit_row m-c-9 "$from" "[\"$to\"]" false "$full" "$d1" >"$work/complete"
  {
    audit_row m-c-9 "$from" "[\"$to\"]" false "$full" "$d1"
    audit_row m-c-10 "$from" "[\"$to\"]" false "$full" "$d1"
  } >"$work/duplicate"
  {
    audit_row m-c-9 "$from" "[\"$to\"]" false "$full" "$d1"
    audit_row m-c-10 "$from" "[\"$to\"]" false "$short" "$d1"
  } >"$work/mixed-duplicate"
  audit_row m-c-9 "$from" "[\"$to\"]" false "$short" "$d1" >"$work/partial"
  printf '%s\n' '{not-json' >"$work/malformed"

  classify_zero_delivery "$work/complete" "$d1" "$full" || die "classifier self-test: complete path"
  if classify_zero_delivery "$work/duplicate" "$d1" "$full"; then
    die "classifier self-test: duplicate complete paths classified as watch timeout"
  else
    [[ $? == 3 ]] || die "classifier self-test: duplicate path classification"
  fi
  if classify_zero_delivery "$work/mixed-duplicate" "$d1" "$full"; then
    die "classifier self-test: mixed duplicate paths classified as watch timeout"
  else
    [[ $? == 3 ]] || die "classifier self-test: mixed duplicate classification"
  fi
  if classify_zero_delivery "$work/partial" "$d1" "$full"; then
    die "classifier self-test: partial path classified as complete"
  else
    [[ $? == 1 ]] || die "classifier self-test: partial path classification"
  fi
  if classify_zero_delivery "$work/malformed" "$d1" "$full"; then
    die "classifier self-test: malformed audit classified as complete"
  else
    [[ $? == 2 ]] || die "classifier self-test: malformed audit classification"
  fi
  note "PASS: zero-delivery audit classifier fixtures"

  # --- audit_origin_digest -------------------------------------------------
  # It resolves the correlator from the ORIGIN bus by the id that bus minted,
  # and must refuse anything ambiguous rather than return a guess.
  audit_row m-a-11 "$from" "[\"$to\"]" false '["a"]' "$d1" >"$work/origin-one"
  {
    audit_row m-a-11 "$from" "[\"$to\"]" false '["a"]' "$d1"
    audit_row m-a-11 "$from" "[\"$to\"]" false '["a"]' "$d2"
  } >"$work/origin-ambiguous"
  audit_row m-a-12 "$from" "[\"$to\"]" false '["a"]' "$d1" >"$work/origin-absent"

  [[ "$(audit_origin_digest "$work/origin-one" A m-a-11)" == "$d1" ]] ||
    die "self-test: audit_origin_digest did not return the recorded digest"
  if (audit_origin_digest "$work/origin-ambiguous" A m-a-11) >/dev/null 2>&1; then
    die "self-test: audit_origin_digest accepted TWO different digests for one minted id"
  fi
  if (audit_origin_digest "$work/origin-absent" A m-a-11) >/dev/null 2>&1; then
    die "self-test: audit_origin_digest invented a digest for a message the origin never recorded"
  fi
  note "PASS: origin-digest resolution fixtures"

  # --- assert_audit_hop ----------------------------------------------------
  # The correct terminal shape, then every way a broken relay can deviate.
  audit_row m-c-9 "$from" "[\"$to\"]" false "$full" "$d1" >"$work/hop-good"
  assert_hop_accepts "correct three-hop terminal record" \
    "$work/hop-good" C "$d1" "$from" "$to" "$full"

  # A relay that never delivered: C's audit holds only OTHER traffic. This is
  # the case the replaced assertion could never distinguish from a working one,
  # because it was looking for an id C is forbidden to hold.
  audit_row m-c-9 "$from" "[\"$to\"]" false "$full" "$d2" >"$work/hop-absent"
  assert_hop_rejects "message absent from C" \
    "$work/hop-absent" C "$d1" "$from" "$to" "$full"

  # An empty audit at C.
  : >"$work/hop-empty"
  assert_hop_rejects "C audit empty" \
    "$work/hop-empty" C "$d1" "$from" "$to" "$full"

  # Truncated traversal: the message reached C but the recorded path is short,
  # so the three-hop claim this epic makes is unproven.
  audit_row m-c-9 "$from" "[\"$to\"]" false "$short" "$d1" >"$work/hop-short"
  assert_hop_rejects "bus_path truncated to two hops" \
    "$work/hop-short" C "$d1" "$from" "$to" "$full"

  # Out-of-order traversal: the same three buses, wrong order. bus_path is an
  # ORDERED list and a set comparison would pass this.
  audit_row m-c-9 "$from" "[\"$to\"]" false '["b","a","c"]' "$d1" >"$work/hop-misordered"
  assert_hop_rejects "bus_path out of order" \
    "$work/hop-misordered" C "$d1" "$from" "$to" "$full"

  # Duplicated: one logical message became two records on C.
  {
    audit_row m-c-9 "$from" "[\"$to\"]" false "$full" "$d1"
    audit_row m-c-10 "$from" "[\"$to\"]" false "$full" "$d1"
  } >"$work/hop-duplicate"
  assert_hop_rejects "logical message duplicated on C" \
    "$work/hop-duplicate" C "$d1" "$from" "$to" "$full"

  # Duplicated with ONE well-formed copy: exactly one record satisfies the full
  # predicate, so only the correlated-record COUNT can catch this. It is what
  # makes that count load-bearing rather than redundant, and it is the shape a
  # relay that delivered twice by two different paths would leave.
  {
    audit_row m-c-9 "$from" "[\"$to\"]" false "$full" "$d1"
    audit_row m-c-10 "$from" "[\"$to\"]" false "$short" "$d1"
  } >"$work/hop-duplicate-mixed"
  assert_hop_rejects "logical message duplicated on C with one partial path" \
    "$work/hop-duplicate-mixed" C "$d1" "$from" "$to" "$full"

  # Misdelivered or misattributed.
  audit_row m-c-9 "$from" '["bus-c.someone-else"]' false "$full" "$d1" >"$work/hop-wrong-to"
  assert_hop_rejects "wrong recipient" \
    "$work/hop-wrong-to" C "$d1" "$from" "$to" "$full"
  audit_row m-c-9 "bus-a.someone-else" "[\"$to\"]" false "$full" "$d1" >"$work/hop-wrong-from"
  assert_hop_rejects "wrong sender" \
    "$work/hop-wrong-from" C "$d1" "$from" "$to" "$full"
  # A broadcast carries the flag AND an empty recipient list -- wal's
  # AuditRecord.validate REQUIRES that pairing -- so this fixture is caught by
  # the recipients comparison whether or not the flag is also checked. The
  # `broadcast == false` clause in assert_audit_hop is therefore deliberate
  # defence in depth that NO legal fixture can make independently load-bearing:
  # a record with broadcast=true AND a non-empty recipient list is a shape the
  # writer refuses to produce. Do not "prove" it with an illegal fixture.
  audit_row m-c-9 "$from" '[]' true "$full" "$d1" >"$work/hop-broadcast"
  assert_hop_rejects "directed message recorded as a broadcast" \
    "$work/hop-broadcast" C "$d1" "$from" "$to" "$full"

  note "PASS: three-hop audit correlation fixtures"
}

if [[ "${1:-}" == "--self-test" ]]; then
  command -v jq >/dev/null 2>&1 || die "jq is required for classifier self-test"
  run_classifier_self_test
  exit 0
fi

command -v jq >/dev/null 2>&1 || die "jq is required to validate compiled --json output"
for candidate in "$SERVE_A" "$SERVE_B" "$SERVE_C"; do
  # -f as well as -x: a DIRECTORY is executable, and naming one would otherwise
  # pass this check and fail later as an opaque exec error.
  [[ -f "$candidate" && -x "$candidate" ]] ||
    die "sanctioned lifecycle command is not an executable file: $candidate"
done
unset candidate

# STATE WHICH BUILD EACH BUS RAN, ON EVERY RUN -- never only when an override is
# detected. A run that announced itself only when it saw an override would make
# the operator's evidence the ABSENCE of a line, and absence is indistinguishable
# from a typo: FED_SMOKE_SERVE_1=... or a lowercased name is silently ignored, all
# three buses quietly default to this checkout, and the run PASSES while the
# operator believes a mixed-version rehearsal happened. A passing run then deletes
# its roots, so this banner is the only provenance that survives it.
if [[ "$SERVE_A" == "$SERVE" && "$SERVE_B" == "$SERVE" && "$SERVE_C" == "$SERVE" ]]; then
  note "single-build run -- all three buses build from this checkout"
else
  note "MIXED-VERSION RUN -- the buses are NOT all on this checkout's build"
fi
note "  A (sender)    $SERVE_A"
note "  B (transit)   $SERVE_B"
note "  C (recipient) $SERVE_C"

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
# B is A's only adjacent bus, so B is the only trust record here that carries an
# inbound binding; C's is signing-only.
add_route "$SERVER_A" "$DATA_A" "$bus_b" "$URL_B" "$fp_b" "$bus_c"
add_trust "$SERVER_A" "$DATA_A" "$bus_b" "$signing_b" "$fp_b"
add_trust "$SERVER_A" "$DATA_A" "$bus_c" "$signing_c"

# B is the only adjacent hop to both endpoint buses, so both of its trust
# records bind an inbound client certificate.
add_route "$SERVER_B" "$DATA_B" "$bus_a" "$URL_A" "$fp_a"
add_trust "$SERVER_B" "$DATA_B" "$bus_a" "$signing_a" "$fp_a"
add_route "$SERVER_B" "$DATA_B" "$bus_c" "$URL_C" "$fp_c"
add_trust "$SERVER_B" "$DATA_B" "$bus_c" "$signing_c" "$fp_c"

# C has a route to B but deliberately NO route to A: A is trust-only here, and
# so is signing-only — A never dials C, so there is no inbound binding to make.
add_route "$SERVER_C" "$DATA_C" "$bus_b" "$URL_B" "$fp_b"
add_trust "$SERVER_C" "$DATA_C" "$bus_b" "$signing_b" "$fp_b"
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

# The recipient's stream is on C, so every record carries C's OWN minted id, not
# the id A returned to the sender (invariant 1). Correlate the delivery by what
# is actually stable on this surface: the body the sender sent, the sender's
# fully-qualified id, and the audience. `--replay --no-cursor` means a second
# copy of the logical message WOULD appear here, so this count is the
# exactly-once check for the recipient-visible surface.
delivery_count="$(jq -s --arg text "$PAYLOAD" --arg from "$sender_id" --arg to "$recipient_id" '
  [.[] | select(.text == $text and .from == $from and .broadcast == false and .to == [$to])] | length
' "$watch_file")" ||
  die "recipient watch output is not valid NDJSON"
if [[ "$delivery_count" == 0 ]]; then
  # Stabilize C's durable audit before attributing a bounded-watch miss. An
  # exact terminal path proves relay delivery and makes this an environmental
  # watch timeout, not a relay failure.
  stop_owned_bus "$ROOT_A" "$RUN_A" "$DATA_A" 127.0.0.1:9101
  stop_owned_bus "$ROOT_B" "$RUN_B" "$DATA_B" 127.0.0.1:9102
  stop_owned_bus "$ROOT_C" "$RUN_C" "$DATA_C" 127.0.0.1:9103
  audit_a="${ROOT_A}/audit.ndjson"
  audit_c="${ROOT_C}/audit.ndjson"
  # A's OWN audit supplies the correlator: the canonical digest it recorded for
  # the id it minted. Reading A is not optional here -- without it there is
  # nothing to look for in C.
  read_audit "$SERVER_A" "$DATA_A" "$audit_a" A
  read_audit "$SERVER_C" "$DATA_C" "$audit_c" C
  origin_digest="$(audit_origin_digest "$audit_a" A "$message_id")"
  if classify_zero_delivery "$audit_c" "$origin_digest" "[\"$bus_a\",\"$bus_b\",\"$bus_c\"]"; then
    die "WATCH TIMEOUT/environmental: C audit contains content digest $origin_digest with bus_path=[$bus_a,$bus_b,$bus_c]; relay delivery succeeded but watch observed zero deliveries"
  else
    case $? in
      1) die "relay not established: C audit lacks content digest $origin_digest with complete bus_path=[$bus_a,$bus_b,$bus_c]" ;;
      2) die "unattributable zero-delivery result: C audit output is malformed" ;;
      3) die "relay delivery invariant failed: C audit contains duplicate complete paths for content digest $origin_digest" ;;
      *) die "unattributable zero-delivery result: classifier failed unexpectedly" ;;
    esac
  fi
elif [[ "$delivery_count" != 1 ]]; then
  die "recipient observed $delivery_count deliveries of the logical message; want exactly 1"
fi
# FAIL-CLOSED: `jq -e` exits 4 when select() yields NO output at all, so a
# recipient record that matches nothing fails here rather than passing quietly.
# Do not rewrite this as a grep/echo pipeline.
jq -e --arg text "$PAYLOAD" --arg from "$sender_id" \
  --arg to "$recipient_id" \
  --argjson path "[\"$bus_a\",\"$bus_b\",\"$bus_c\"]" \
  'select(.text == $text and .from == $from and
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

# A minted the message, so A -- and ONLY A -- is asserted by the id A returned
# to the sender. That record's canonical content digest is then the correlator
# for the two downstream buses, each of which minted an id of its own.
origin_digest="$(audit_origin_digest "$audit_a" A "$message_id")"

assert_audit_hop "$audit_a" A "$origin_digest" "$sender_id" "$recipient_id" "[\"$bus_a\"]"
assert_audit_hop "$audit_b" B "$origin_digest" "$sender_id" "$recipient_id" "[\"$bus_a\",\"$bus_b\"]"
assert_audit_hop "$audit_c" C "$origin_digest" "$sender_id" "$recipient_id" "[\"$bus_a\",\"$bus_b\",\"$bus_c\"]"

note "PASS: one logical message (origin id $message_id, content digest $origin_digest)"
note "PASS: delivered exactly once over $bus_a -> $bus_b -> $bus_c"
