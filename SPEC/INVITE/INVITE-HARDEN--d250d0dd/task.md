# INVITE-HARDEN: constant-time invite-secret comparison and ONE indistinguishable failure response for unknown/expired/revoked/already-consumed

| Field | Value |
| --- | --- |
| Public id | `d250d0dd-dd17-4aa0-94ea-eb76a1f72913` |
| Key | INVITE-HARDEN |
| Epic | [INVITE](../epic.md) |
| Status | in_progress |
| Priority | P1 |
| Component | security |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T21:12:48.621700+00:00 |
| Updated | 2026-08-14T23:05:30.178742+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run 'TestInviteGateEveryInviteRefusalIsIndistinguishable|TestInviteSecretComparedInConstantTime' ./internal/httpapi ./internal/invite
```

## Status note

CODE COMPLETE, AWAITING COORDINATED COMMIT -- deliberately NOT done. The feature-runner is a code-only agent; the orchestrator commits. Five files in internal/invite: guard_test.go (NEW, still untracked), doc.go, errors.go, retention.go, secret.go. Reviewer gate PASS; security gate PASS-WITH-FINDINGS with every finding fixed and re-verified by the gate that raised it. Committed != running: this stays in_progress until the commit lands.

PROOF_CMD CORRECTED 2026-08-14 (this was blocking completion). The previously recorded proof was `go test -race -run 'TestInviteRedeemFailuresIndistinguishable|TestInviteSecretComparedInConstantTime' ./internal/httpapi ./internal/invite`. TestInviteRedeemFailuresIndistinguishable HAS NEVER EXISTED anywhere in the repo -- git grep finds the name only inside the generated SPEC/ mirror, i.e. only in this task's own record. `go test -run <nonexistent>` prints ok ... [no tests to run] and EXITS 0, so the httpapi half of that proof was VACUOUS: it would have looked identical to a pass while asserting nothing. This is the exact failure class scripts/proof-check.sh exists to catch, and it is why a proof_cmd must be RUN through proof-check.sh and its verdict quoted rather than merely stored. The real test asserting the property is TestInviteGateEveryInviteRefusalIsIndistinguishable (internal/httpapi/invitegate_test.go:372), which asserts a byte-identical status+body across 8 rows. Both the reviewer and the security gate identified the vacuity independently. The reviewer ran the CORRECTED command in a clean HEAD overlay: verdict=PASS, 5 tests, 2 top-level, 0 skipped.

SCOPE NARROWED BY A MEASURED FINDING: BOTH halves of INVITE-HARDEN were ALREADY IMPLEMENTED at HEAD and needed NO production code change. (a) Constant-time comparison -- internal/invite/secret.go:92-95 VerifySecret hashes first then uses crypto/subtle.ConstantTimeCompare; store.go:729-737 compares an UNKNOWN id against a per-store 32-byte crypto/rand dummyDigest so the work is equal either way, and VerifySecret is the LEFT operand of `||` so it is never short-circuited; store.go:762 and :766 are also constant-time. (b) ONE indistinguishable failure response -- internal/httpapi/auth.go writeInviteError already collapses ErrUnknownInvite / ErrExpired / ErrRevoked / ErrAlreadyRedeemed / ErrInvalidInviteID into a single 403 {"error":"invite not accepted"}, logging the sentinel server-side only. What this increment therefore delivers is the MISSING EVIDENCE plus doc corrections, not new behaviour.

## Description

EPIC: 0b43393e-556b-409a-938a-846be2fb4a75 | DEPENDS ON: INVITE-GATE | BLOCKS: none

Mirrors the existing deliberate indistinguishability of the 401 and 404 surfaces (CONTRACTS-HTTP.md:19, :235-239) -- distinguishing the four invite failure modes is an enumeration oracle. Comparison uses stdlib crypto/subtle.ConstantTimeCompare. INVARIANT 9: do not hand-roll a comparison, a hash, or a token format; if any part of this looks like inventing a scheme, stop and escalate.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [0b43393e-556b-409a-938a-846be2fb4a75](../EPIC-invite-only-enrolment-the-root-fix-for-the-pre-auth--0b43393e/task.md) — EPIC: invite-only enrolment -- the root fix for the pre-auth attack family (needs planner… (superseded)
- [INVITE-GATE](../INVITE-GATE--05a5216d/task.md) — INVITE-GATE: POST /v1/enroll REQUIRES a valid invite and fails closed; invite consumption… (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [0b43393e-556b-409a-938a-846be2fb4a75](../EPIC-invite-only-enrolment-the-root-fix-for-the-pre-auth--0b43393e/task.md) — EPIC: invite-only enrolment -- the root fix for the pre-auth attack family (needs planner… (superseded)
- [INVITE-GATE](../INVITE-GATE--05a5216d/task.md) — INVITE-GATE: POST /v1/enroll REQUIRES a valid invite and fails closed; invite consumption… (done)
- [INVITE-GATE-ENFORCE-FU-INVITEDOCS](../INVITE-GATE-ENFORCE-FU-INVITEDOCS--47c7bae9/task.md) — INVITE-GATE-ENFORCE-FU-INVITEDOCS: correct internal/invite doc comments now the gate is e… (todo)
- [INVITE-HARDEN-FU-GUARDHOLES](../INVITE-HARDEN-FU-GUARDHOLES--fbb31afb/task.md) — INVITE-HARDEN-FU-GUARDHOLES: guard comparator checks must resolve aliased imports (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
