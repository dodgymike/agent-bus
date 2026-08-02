#!/usr/bin/env bash
# spec-cloud.sh — authenticated curl shim for the CLOUD Spec Keeper
# (spec.elasticninja.com), the primary task-state store as of 2026-07-23.
#
# Drop-in for the old `curl … http://localhost:8080/api/v1/…` calls: it finds the
# request PATH among its args (the first arg starting with `/`), prepends the cloud
# host, injects a fresh Cognito Bearer (cached ~40 min, auto-refreshed on 401), and
# passes every other arg straight through to curl. Emits the response BODY on stdout
# and exits non-zero on a non-2xx status.
#
# Usage mirrors the old calls after a mechanical rewrite:
#   bash scripts/spec-cloud.sh -s /api/v1/projects/bird-song/export > SPEC.md
#   bash scripts/spec-cloud.sh -s -X POST /api/v1/projects/bird-song/tasks/claim-next \
#        -H 'Content-Type: application/json' -d '{"agent":"you"}'
#   bash scripts/spec-cloud.sh -sf /readyz          # health check
#
# Creds live outside the repo (never committed): see the re-auth recipe in the
# cloud-spec-keeper memory. Falls back with a clear error if they're missing.
set -uo pipefail
CREDS="${SPEC_CLOUD_CREDS:-/mnt/sdc/mike/claude-scratch/spec-cloud-creds.env}"
CACHE="${SPEC_CLOUD_TOKENCACHE:-/mnt/sdc/mike/claude-scratch/.spec-cloud-token}"
HOST="${SPEC_CLOUD_HOST:-https://api.spec.elasticninja.com}"

[ -f "$CREDS" ] || { echo "spec-cloud: creds file missing ($CREDS) — re-enrol (see cloud-spec-keeper memory)" >&2; exit 3; }
set -a; . "$CREDS"; set +a

_mint() {
  aws cognito-idp initiate-auth --region "$SPEC_CLOUD_REGION" --auth-flow USER_PASSWORD_AUTH \
    --client-id "$SPEC_CLOUD_CLIENT_ID" \
    --auth-parameters USERNAME="$SPEC_CLOUD_USERNAME",PASSWORD="$SPEC_CLOUD_PASSWORD" \
    --query 'AuthenticationResult.AccessToken' --output text 2>/dev/null
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
path=""; passthru=()
for a in "$@"; do
  if [ -z "$path" ] && [ "${a#/}" != "$a" ]; then path="$a"; else passthru+=("$a"); fi
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
