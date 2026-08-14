# CORE-1: Repo skeleton: go.mod, internal/ package layout, .gitignore

| Field | Value |
| --- | --- |
| Public id | `eea035e4-92de-4ca3-95ed-fa8073cd6a81` |
| Key | CORE-1 |
| Epic | [CORE](../epic.md) |
| Status | done |
| Priority | P0 |
| Component | core |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T09:05:41.062293+00:00 |
| Updated | 2026-08-02T18:05:37.288542+00:00 |
| Completed | 2026-08-02T09:51:21.154268+00:00 |

## Proof command

```sh
go build ./... && test -z "$("$(go env GOROOT)/bin/gofmt" -l .)"
```

## Description

PROOF_CMD CORRECTED 2026-08-02 (spec-keeper). The recorded proof was
`go build ./... && test -z "$(gofmt -l .)"`. **On this box that never ran gofmt at all**: `gofmt` is
NOT on PATH (only `$(go env GOROOT)/bin/gofmt` is), a command substitution whose command fails to exec
produces EMPTY stdout, and `test -z ""` is TRUE -- so the clause PASSED by failing to launch. That is
the same vacuity class scripts/proof-check.sh exists to catch, one level up the stack: a whole TOOL
silently absent rather than a test silently unmatched.

NOT REOPENED, and here is the evidence for that call rather than an assertion. The CORRECTED command
was RE-RUN against the current tree on 2026-08-02 through scripts/proof-check.sh:
`verdict=PASS class=file-assertion,toolchain exit=0` -- `go build ./...` succeeds and the REAL gofmt
binary reports zero files needing formatting. CORE-1's substance was fine; only its evidence was
worthless. Reviewer and security notes on this task independently recorded running gofmt via
$GOROOT/bin and finding the repo clean, which agrees.

STANDING RULE, now in CLAUDE.md: never use a bare `gofmt`; use `go fmt ./...` or
`"$(go env GOROOT)/bin/gofmt" -l .`. See task c0a5bdb6 for the full write-up and fc8cd234 for the
sweep of every other proof_cmd containing a bare gofmt call.

--- ORIGINAL DESCRIPTION ---
Initialize go.mod (module github.com/dodgymike/agent-bus, go1.19 toolchain pin), create the internal/ package layout (ids, store, wal, hub, auth, httpapi, relay) as packages with doc.go stubs, and the cmd/agent-bus/ dir. The HTTP package is named `httpapi`, NOT `http`: naming it `http` would shadow stdlib net/http in every file that imports both, which is a needless papercut. .gitignore already covers build artifacts and /data/ -- verify, do not duplicate. No server logic yet -- this is the scaffold every other task builds on.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


_None recorded._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [c0a5bdb6-8b57-4382-adb1-db6657850818](../Re-verify-CORE-1-s-gofmt-proof-with-the-corrected-go-env--c0a5bdb6/task.md) — Re-verify CORE-1's gofmt proof with the corrected ($(go env GOROOT)/bin/gofmt) invocation… (done)
- [fc8cd234-d275-43a1-9cb0-d10bca4a4086](../../PROCESS/Backfill-non-vacuous-proof_cmd-across-the-14-actionable--fc8cd234/task.md) — Backfill non-vacuous proof_cmd across the 14 actionable tasks that have none (CLI-1..9 +… (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [AUTH-6](../../AUTH/AUTH-6--1640e0b4/task.md) — AUTH-6: Auth FAIL-OPEN risk -- wrap the mux with auth + an explicit unauthenticated allow… (superseded)
- [CORE-10](../CORE-10--27ad23ef/task.md) — CORE-10: .gitignore has no secret patterns while the stop hook stages with \`git add -A\` (done)
- [CORE-11](../CORE-11--fd330606/task.md) — CORE-11: shutdownGrace (10s) &lt; defaultPollTimeout (30s) -- record the ctx.Done() contract… (done)
- [CORE-12](../CORE-12--ae000d92/task.md) — CORE-12: defaultListen=":8080" binds all interfaces -- prefer 127.0.0.1:8080 (superseded)
- [CORE-13](../CORE-13--42cb8a90/task.md) — CORE-13: Middleware implements Flusher/Hijacker unconditionally and drops io.ReaderFrom (… (done)
- [CORE-14](../CORE-14--13488041/task.md) — CORE-14: A handler that writes then panics logs status=200 -- the audit trail is wrong (done)
- [CORE-15](../CORE-15--68ad525c/task.md) — CORE-15: logging.format() calls .String()/.Error() on a typed-nil -&gt; panic inside the log… (done)
- [CORE-6](../CORE-6--f4ce3f6a/task.md) — CORE-6: logging maxValueLen=1024 truncates panic stack traces (exempt \`stack\` or raise to… (done)
- [CORE-7](../CORE-7--60f49022/task.md) — CORE-7: HEAD is 405'd by requireGET while writeJSON still guards MethodHead -- dead code,… (done)
- [CORE-8](../CORE-8--1e9dae04/task.md) — CORE-8: Unmatched paths return ServeMux's text/plain 404, breaking the JSON error contract (done)
- [CORE-9](../CORE-9--a1f74fcc/task.md) — CORE-9: Set IdleTimeout + MaxHeaderBytes on http.Server -- and deliberately leave Read/Wr… (done)
- [CRYPTO-1](../../CRYPTO/CRYPTO-1--30570fb9/task.md) — CRYPTO-1: DESIGN SPIKE -- Signal-style E2E crypto for agent-bus (CRYPTO_DEEPDIVE.md + DEC… (done)
- [CRYPTO-2](../../CRYPTO/CRYPTO-2--0ad37da2/task.md) — CRYPTO-2: Adopt the crypto primitive layer chosen by the spike (internal/cryptobox + go.m… (superseded)
- [SPEC-API-LIST-SILENT-TRUNCATION](../../UNASSIGNED/SPEC-API-LIST-SILENT-TRUNCATION--82f35b73/task.md) — Task-list API silently truncates at 200 with no total, no next and no working pagination… (todo)
- [c0a5bdb6-8b57-4382-adb1-db6657850818](../Re-verify-CORE-1-s-gofmt-proof-with-the-corrected-go-env--c0a5bdb6/task.md) — Re-verify CORE-1's gofmt proof with the corrected ($(go env GOROOT)/bin/gofmt) invocation… (done)
- [cf886bb9-1921-4b17-826b-1ce2f8ef987f](../../TOOLING/scripts-spec-cloud.sh-leaks-SPEC_CLOUD_PASSWORD-on-the-a--cf886bb9/task.md) — scripts/spec-cloud.sh leaks SPEC_CLOUD_PASSWORD on the \`aws\` argv (readable via /proc/*/c… (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
