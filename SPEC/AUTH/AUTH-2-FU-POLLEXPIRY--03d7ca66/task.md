# AUTH-2-FU-POLLEXPIRY: A long-poll can outlive its session, quietly contradicting immediate revocation

| Field | Value |
| --- | --- |
| Public id | `03d7ca66-110e-4560-803e-1a7825d1accc` |
| Key | _(null in the export)_ |
| Epic | [AUTH](../epic.md) |
| Status | todo |
| Priority | P1 |
| Component | auth |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T19:40:29.145295+00:00 |
| Updated | 2026-08-14T22:33:56.320388+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run 'TestWaitCapsAtSessionExpiryNotJustPollTimeout|TestWaitReAuthenticatesBeforeDelivering|TestWaitReChecksCertBindingBeforeDelivering' ./internal/hub/... ./internal/httpapi/...
```

## Status note

Added a proof_cmd 2026-08-14 -- this task had NONE (confirmed via a fresh API fetch, not the cached export), which under CLAUDE.md means it could not be completed at all regardless of code state. Named three future acceptance tests matching this task's widened definition of done: the poll-timeout cap against session expiry, re-authentication before delivering, and the cert-cross-check re-verification folded in from MTLS-CROSSCHECK-FU-POLLRECHECK. None exist yet -- this task is still todo; the names are the acceptance target for whoever implements it, following this backlog's existing convention of naming not-yet-written tests as proof_cmds.

## Description

Origin: security audit of AUTH-2, 2026-08-02, found forward-looking. Authentication is evaluated ONCE at request entry (handleWait does not re-authenticate; it reads the principal authMiddleware already attached via messagingPrincipal, which checks nothing). CORRECTED 2026-08-08 (was: '-poll-timeout is validated only as > 0 with no ceiling against the 1h auth.SessionLifetime' -- that was WRONG, see kind=report note): -poll-timeout IS ceilinged, at hub.MaxPollTimeout (5 minutes), on all three paths -- hub.Wait clamps, hub.Open clamps the operator's -poll-timeout flag, and readTimeoutParam refuses rather than clamps an over-ceiling client request (400). So the pre-fix exposure was always <=5 minutes, never <=1 hour. A poll parked at entry still keeps serving after its session expires or is revoked, up to that 5-minute bound, and up to hub.MaxWaitersPerAgent (32) parked polls per agent, so the lag can cover up to 32 batches, not one. auth.Principal already carries ExpiresAt, so a handler CAN enforce it but nothing requires it. The POLL epic must cap the wait at min(PollTimeout, time.Until(principal.ExpiresAt)) and re-Authenticate before delivering. Re-polling does NOT chain past a revoke: /v1/wait is not on unauthenticatedRoutes, so the next poll is refused. P1 because 'revocation is immediate' (DECISIONS.md 2026-08-02) is otherwise false for any poll already in flight -- STAYS P1 today (no reachable revocation surface exists yet; only expiry is reachable, a bounded <=5min overrun by a principal that legitimately held the credential moments earlier) but becomes P0 the day AUTH-4 (/v1/leave, a853261d, still todo as of this update) ships, since that is the day an operator starts relying on immediate revocation. ORDERING CONSTRAINT: this task MUST land before AUTH-4 -- recorded as a `blocks` relation -- so revocation never ships without covering the parked-poll case. STATUS SPLIT (2026-08-08): the DOC-ACCURACY half of the underlying audit finding (F8/S8b, overstated 'revocation and expiry immediate' comment in internal/httpapi/authmw.go) has landed as a comment-only change (see task notes for the diff/verification) -- reviewer PASS, security PASS. The BEHAVIOUR half -- the min(pollTimeout, time.Until(ExpiresAt)) cap and re-authenticate-before-delivering in handleWait/hub.Wait -- is UNTOUCHED. This task's definition-of-done is the behaviour fix; it stays `todo` until that lands, not merely documented.

UPDATED 2026-08-14 (spec-keeper, coordinator-directed AUTH audit against a clean ec14bb8 overlay) -- TWO changes, both verified directly, not taken on trust.

(1) SCOPE WIDENED, TRIGGER FIRED. The original text said MTLS-CROSSCHECK "has NOT shipped either (no reachable exposure yet)... revisit if MTLS-CROSSCHECK lands first." That premise is now FALSE: MTLS-CROSSCHECK shipped at commit 2ea7dfb ("Cross-check mTLS against the session token on the agent plane"). Its enforcement, enforceCertBinding (internal/httpapi/crosscheck.go:212), runs at THREE call sites -- internal/httpapi/authmw.go:431, internal/httpapi/auth.go:662, internal/httpapi/auth.go:743 -- all at REQUEST ADMISSION ONLY, confirmed by crosscheck.go's own doc comment (:38-46): "THE CHECK RUNS AT REQUEST ADMISSION ONLY. It is a gate, not a supervisor, exactly as the session check is. A long poll admitted the instant before an operator retires a binding runs to the end of its poll timeout... precisely as it outlives a revoked session." That same comment already names THIS task as the closing one: "Closing it is AUTH-2-FU-POLLEXPIRY (03d7ca66-110e-4560-803e-1a7825d1accc), which must now re-evaluate BOTH the session AND this cross-check before delivering, not the session alone." So a parked poll now outlives the cert binding as well as the session -- this task's definition-of-done widens accordingly: handleWait/hub.Wait must re-check BOTH principal.ExpiresAt AND the cert-to-agent binding before delivering a batch off a parked poll, not the session alone. This does not change the P1 priority (that escalation trigger is tied to AUTH-4 shipping, still todo, not to MTLS-CROSSCHECK) but does change what "done" requires.

(2) LINE NUMBERS CORRECTED (all four cited in this description had drifted): hub.Wait's clamp is now at internal/hub/wait.go:230-231 (was :165-166 -- that line NOW lands inside the unrelated test-only SetWaiterParkedHook, which would have sent an implementer to the wrong function entirely). hub.Open's clamp of the operator's -poll-timeout flag is now at internal/hub/hub.go:524-525 (was :498-499). readTimeoutParam's refusal is now at internal/httpapi/messages.go:949-951 (was :947-949).

(3) OVERLAP WITH MTLS-CROSSCHECK-FU-POLLRECHECK (665694e0) -- that task exists solely to restate item (1) above as a safety net "so the requirement is not lost if [this task] is re-scoped," by its own description. Since item (1) is now folded directly into THIS task's scope, MTLS-CROSSCHECK-FU-POLLRECHECK is superseded against this task (see that task's own record) rather than left to be built twice.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [AUTH-2](../AUTH-2--4b45a6d8/task.md) — AUTH-2: Token verification middleware (done)
- [AUTH-4](../AUTH-4--a853261d/task.md) — AUTH-4: POST /v1/leave -- leave / revocation (done)
- [MTLS-CROSSCHECK](../../MTLS/MTLS-CROSSCHECK--2b2af075/task.md) — MTLS-CROSSCHECK: reject a session token presented over a connection whose client certific… (done)
- [MTLS-CROSSCHECK-FU-POLLRECHECK](../../MTLS/MTLS-CROSSCHECK-FU-POLLRECHECK--665694e0/task.md) — AUTH-2-FU-POLLEXPIRY must re-evaluate the certificate cross-check mid-poll, not only the… (superseded)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [MTLS-CROSSCHECK](../../MTLS/MTLS-CROSSCHECK--2b2af075/task.md) — MTLS-CROSSCHECK: reject a session token presented over a connection whose client certific… (done)
- [MTLS-CROSSCHECK-FU-POLLRECHECK](../../MTLS/MTLS-CROSSCHECK-FU-POLLRECHECK--665694e0/task.md) — AUTH-2-FU-POLLEXPIRY must re-evaluate the certificate cross-check mid-poll, not only the… (superseded)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
