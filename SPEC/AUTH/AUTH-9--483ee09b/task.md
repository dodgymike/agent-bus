# AUTH-9: Opt-in session persistence (--persist-session) + agent-busctl session logout

| Field | Value |
| --- | --- |
| Public id | `483ee09b-4248-44d1-a9b4-d3a8c0fa47ba` |
| Key | AUTH-9 |
| Epic | [AUTH](../epic.md) |
| Status | done |
| Priority | P1 |
| Component | auth |
| Section | backlog |
| Tags | — |
| Created | 2026-08-16T08:00:22.816452+00:00 |
| Updated | 2026-08-16T09:02:25.619950+00:00 |
| Completed | 2026-08-16T09:02:25.619933+00:00 |

## Proof command

```sh
go test -race -run 'TestPersist|TestSessionFileName|TestForgetPersisted|TestEnvPersist|TestForced' ./client/
```

## Description

OPERATOR-REQUESTED 2026-08-16. Opt-in session persistence for the CLI.

Add `--persist-session` (env AGENT_BUS_PERSIST_SESSION) so the session token is cached in the
credential store and REUSED by the next process, plus `agent-busctl session logout` to discard it.

# Why

Live incident 2026-08-15 on bus-matv6xu7ronvdq7o: elastic-agent-1 held 32 sessions and got
12 x HTTP 200 then 32 x HTTP 503 on /v1/session/complete, locked out of its OWN identity with no
self-service recovery.

Root cause is structural, not a client bug. agent-busctl is one-shot; the client caches sessions IN
MEMORY only; the store persists identity/keys/pins/cursors but NOT the token. The bus holds each
session for SessionLifetime (1h) against DefaultMaxActiveSessionsPerAgent (32) and evicts nothing.
Under invariant 7 an agent shells out per command, so EVERY command burns a session for an hour.
Above ~1 command / 2 minutes an agent bricks itself.

internal/auth/service.go:46 sizes the cap on "the steady state for a well-behaved agent is TWO
concurrent sessions" -- true for a long-lived embedding client, FALSE for the shell-out shape
invariant 7 mandates. That mismatch is the defect, not the number 32.

# The reversal, explicitly authorised

This REVERSES the standing decision that a session token is never written to disk. The operator
authorised it directly and explicitly: "I want this feature to write the creds to disk! so no
refusals on that, only on practical security / safety concerns."

Default remains OFF. Persistence is opt-in.

# Gate findings, all fixed in-task

SECURITY HIGH -- the bus binding was a TAUTOLOGY. loadPersistedSession compared doc.BusURL against
cred.BusURL, both off the stored credential, while resolveBusURL prefers --bus/AGENT_BUS_URL. The
flag moved the connection without moving the check, so the token was presented to whatever --bus
named; proven leaking to a rogue loopback listener with a passing no-persist control. Fixed by
binding to resolveBusURL().String() on BOTH load and save; regression test proven RED by mutation.

REVIEW BLOCKER -- `whoami --verify` verified NOTHING. It called EnsureSession, a cache lookup; once
the cache outlived the process, --verify returned exit 0 against an unreachable bus -- failing at
its one job, in exactly the bus-restart case its own help text names. Fixed with VerifySession,
which always reaches the network.

REVIEW BLOCKER -- `agent-busctl logout` orphaned a live bearer token the CLI could no longer delete
(session logout resolves the identity first, so it exited 3). Logout/LogoutAll now destroy the
persisted session, best-effort.

Also fixed: session logout --as/--json exited 2; double JSON document on exit 8; the world-readable
file was overwritten by the same command that warned about it; the fixed .tmp name raced across
processes and was never swept; os.Stat followed a planted symlink; no redacting String(); .gitignore
missed the new credential file; --persist-session absent from --help; `session logout --help`
errored; the "0 handshakes" doc claim was impossible from a cold store.

# Acceptance
  - default off; no token written without the opt-in
  - 0600, O_EXCL at creation, never chmod-ed
  - a token is never presented to a bus other than the one it was issued by
  - `whoami --verify` always reaches the bus
  - `logout` leaves no token behind
  - every guard proven by mutation, not merely written

# Follow-ups
  AUTH-7 (4ba67a7b) operator clears one agent's sessions -- blocked: no operator principal exists.
  AUTH-8 (b65948b7) deep dive, usability vs abuse protection.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [AUTH-7](../AUTH-7--4ba67a7b/task.md) — AUTH-7: Operator/admin can clear one agent's active sessions without restarting the bus (todo)
- [AUTH-8](../AUTH-8--b65948b7/task.md) — AUTH-8: DEEP DIVE — the balance between usability and security / abuse protection (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
