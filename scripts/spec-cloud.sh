#!/usr/bin/env bash
# spec-cloud.sh — authenticated curl shim for the CLOUD Spec Keeper
# (spec.elasticninja.com) for the agent-bus project.
#
# Drop-in for the old `curl … http://localhost:8080/api/v1/…` calls: it finds the
# request PATH among its args (the first arg starting with `/`), prepends the cloud
# host, injects a fresh Cognito Bearer (cached ~40 min, auto-refreshed on 401), and
# passes every other arg straight through to curl. Emits the response BODY on stdout
# and exits non-zero on a non-2xx status.
#
# Usage mirrors the old calls after a mechanical rewrite:
#   bash scripts/spec-cloud.sh -s /api/v1/projects/agent-bus/export > SPEC.md
#   bash scripts/spec-cloud.sh -s -X POST /api/v1/projects/agent-bus/tasks/claim-next \
#        -H 'Content-Type: application/json' -d '{"agent":"you"}'
#   bash scripts/spec-cloud.sh -sf /readyz          # health check
#
# Creds live outside the repo (never committed): see the re-auth recipe in the
# cloud-spec-keeper memory. Falls back with a clear error if they're missing.
set -uo pipefail
CREDS="${SPEC_CLOUD_CREDS:-/mnt/sdc/mike/claude-scratch/spec-cloud-creds-agent-bus.env}"
CACHE="${SPEC_CLOUD_TOKENCACHE:-/mnt/sdc/mike/claude-scratch/.spec-cloud-token-agent-bus}"
HOST="${SPEC_CLOUD_HOST:-https://api.spec.elasticninja.com}"

[ -f "$CREDS" ] || { echo "spec-cloud: creds file missing ($CREDS) — re-enrol (see cloud-spec-keeper memory)" >&2; exit 3; }
set -a; . "$CREDS"; set +a

# Mint an access token. The password is passed via --cli-input-json on STDIN, NOT on the
# argv: `--auth-parameters PASSWORD=…` puts the plaintext password in the process table,
# where any local user's `ps` can read it for the life of the call.
# (The v1 aws CLI on this box cannot read --cli-input-json from file:///dev/stdin — it
# reads it as empty — so the request goes through a mode-600 temp file that is removed
# on every exit path.)
_mint() {
  local req rc tok
  req=$(mktemp "${TMPDIR:-/tmp}/.spec-cloud-auth.XXXXXX") || return 1
  chmod 600 "$req"
  trap 'rm -f "$req"' RETURN
  python3 -c '
import json, os, sys
json.dump({"AuthFlow": "USER_PASSWORD_AUTH",
           "ClientId": os.environ["SPEC_CLOUD_CLIENT_ID"],
           "AuthParameters": {"USERNAME": os.environ["SPEC_CLOUD_USERNAME"],
                              "PASSWORD": os.environ["SPEC_CLOUD_PASSWORD"]}}, open(sys.argv[1], "w"))
' "$req" || return 1
  tok=$(aws cognito-idp initiate-auth --region "$SPEC_CLOUD_REGION" \
          --cli-input-json "file://$req" \
          --query 'AuthenticationResult.AccessToken' --output text 2>/dev/null)
  rc=$?
  printf '%s' "$tok"
  return $rc
}
_token() {
  if [ -f "$CACHE" ] && [ $(( $(date +%s) - $(stat -c %Y "$CACHE" 2>/dev/null || echo 0) )) -lt 2400 ]; then
    cat "$CACHE"
  else
    local t; t=$(_mint)
    if [ -n "$t" ] && [ "$t" != "None" ]; then umask 077; printf '%s' "$t" >"$CACHE"; printf '%s' "$t"; fi
  fi
}

# Split args into the request path (first /-prefixed arg) and pass-through curl opts.
#
# A curl option that TAKES A VALUE can be followed by a /-prefixed argument that is a
# local filename, not the API path — `-o /dev/null` and `-d @/tmp/body.json` are the ones
# that bite. Naively grabbing "the first arg starting with /" retargets the request at
# /dev/null and 404s. So skip the argument immediately after any value-taking option.
# Long options are enumerated rather than matched as `--*`, because most of them
# (--silent, --fail, --show-error) take NO value — treating them as if they did would
# swallow the API path that follows.
_takes_value() {
  case "$1" in
    -o | -d | -F | -H | -X | -A | -b | -c | -e | -T | -u | -U | -w | -K | -E | -y | -Y | -z | -m) return 0 ;;
    --output | --data | --data-binary | --data-raw | --data-urlencode | --form | --header) return 0 ;;
    --request | --user-agent | --cookie | --cookie-jar | --referer | --upload-file) return 0 ;;
    --user | --proxy-user | --write-out | --config | --cert | --key | --cacert) return 0 ;;
    --max-time | --connect-timeout | --retry | --url | --resolve | --interface) return 0 ;;
    *) return 1 ;;   # bare paths, and flag clusters like -sS / -sf, take no value
  esac
}
path=""; passthru=(); skip_next=0
for a in "$@"; do
  if [ -z "$path" ] && [ "$skip_next" -eq 0 ] && [ "${a#/}" != "$a" ]; then
    path="$a"
  else
    passthru+=("$a")
  fi
  if [ "$skip_next" -eq 0 ] && _takes_value "$a"; then skip_next=1; else skip_next=0; fi
done
[ -n "$path" ] || { echo "spec-cloud: no /path argument found in: $*" >&2; exit 2; }

_call() {
  local tok="$1"; shift
  curl -sS -w $'\n%{http_code}' -H "Authorization: Bearer $tok" "${passthru[@]}" "$HOST$path"
}

tok=$(_token); [ -n "$tok" ] || { echo "spec-cloud: auth failed (Cognito)" >&2; exit 4; }
resp=$(_call "$tok"); code=$(printf '%s' "$resp" | tail -1)
if [ "$code" = "401" ]; then           # token lapsed mid-run → re-mint once + retry
  rm -f "$CACHE"; tok=$(_token); resp=$(_call "$tok"); code=$(printf '%s' "$resp" | tail -1)
fi
printf '%s' "$resp" | sed '$d'          # body only (strip the trailing status line)
case "$code" in 2*) exit 0;; *) echo "spec-cloud: HTTP $code for $path" >&2; exit 5;; esac
