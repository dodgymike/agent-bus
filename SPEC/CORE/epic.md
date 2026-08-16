# EPIC CORE — Repo skeleton & server bootstrap

[← all epics](../../SPEC.md)

**4 open / 22 total.** Full records live in `SPEC/CORE/<task>/task.md`.

_Relations (real)_ are authoritative edges from the Spec Server. _Referenced (derived)_ are guesses parsed out of description free text — useful, but not a dependency list.

## Open tasks (4)

| Key | Title | Status | Prio | Task | Relations (real) | Referenced (derived) |
| --- | --- | --- | --- | --- | --- | --- |
| c47379ae-9873-4800-a442-03e34a7f1294 | invite mint bypasses the data-directory permission gate entirely -- the invite blob is th… | deferred | P0 | [task.md](invite-mint-bypasses-the-data-directory-permission-gate--c47379ae/task.md) | _not fetched_ | [be447589-6583-4d5c-a9d4-ec9d9fef0f1c](Enforce-data-directory-permissions-at-startup-and-bound--be447589/task.md) |
| 3e43c52c-ae62-4b8c-aabb-1b9f7f62d82f | The data-dir permission gate checks MODE but not OWNERSHIP, and follows symlinks -- and i… | deferred | P1 | [task.md](The-data-dir-permission-gate-checks-MODE-but-not-OWNERSH--3e43c52c/task.md) | _not fetched_ | [be447589-6583-4d5c-a9d4-ec9d9fef0f1c](Enforce-data-directory-permissions-at-startup-and-bound--be447589/task.md) [6c482cc0-ce83-49e9-a7ff-f8575795cb39](../DUR/wal.OpenWriter-RepairTail-open-bus.wal-without-O_NOFOLLO--6c482cc0/task.md) [ae594fa8-03bb-4d51-aa31-641f5ddcae66](../AGENTIF/RUN_DIR-created-with-no-ownership-check-enables-binary-s--ae594fa8/task.md) |
| DISCOVERY-DOC | DISCOVERY-DOC: self-describing unauthenticated discovery document so an agent with only a… | in_progress | P1 | [task.md](DISCOVERY-DOC--2d7ce37b/task.md) | _not fetched_ | [INVITE-GATE](../INVITE/INVITE-GATE--05a5216d/task.md) [CRYPTO-4](../CRYPTO/CRYPTO-4--13f3947e/task.md) |
| MSG-FU-MAINWIRING | MSG-FU-MAINWIRING: main should construct the hub and pass it as BOTH httpapi.Options.Hub… | todo | P1 | [task.md](MSG-FU-MAINWIRING--221f89c0/task.md) | _not fetched_ | — |

## Closed tasks (18) — done, cancelled, superseded

| Key | Title | Status | Prio | Task | Relations (real) | Referenced (derived) |
| --- | --- | --- | --- | --- | --- | --- |
| CORE-1 | CORE-1: Repo skeleton: go.mod, internal/ package layout, .gitignore | done | P0 | [task.md](CORE-1--eea035e4/task.md) | _not fetched_ | [c0a5bdb6-8b57-4382-adb1-db6657850818](Re-verify-CORE-1-s-gofmt-proof-with-the-corrected-go-env--c0a5bdb6/task.md) [fc8cd234-d275-43a1-9cb0-d10bca4a4086](../PROCESS/Backfill-non-vacuous-proof_cmd-across-the-14-actionable--fc8cd234/task.md) |
| CORE-2 | CORE-2: cmd/agent-bus main entrypoint + config/flags | done | P0 | [task.md](CORE-2--3eb30518/task.md) | _not fetched_ | — |
| CORE-3 | CORE-3: GET /healthz and GET /v1/info endpoints | done | P0 | [task.md](CORE-3--bcdb7443/task.md) | _not fetched_ | — |
| CORE-4 | CORE-4: Structured logging + request middleware | done | P0 | [task.md](CORE-4--d9ffbf08/task.md) | _not fetched_ | — |
| be447589-6583-4d5c-a9d4-ec9d9fef0f1c | Enforce data-directory permissions at startup, and bound the message-seq floor | done | P0 | [task.md](Enforce-data-directory-permissions-at-startup-and-bound--be447589/task.md) | _not fetched_ | — |
| 72d7f10d-5f4a-4ad7-a680-e548c331eb20 | os.MkdirAll(cfg.DataDir, 0o700) at main.go:157 never tightens an ALREADY-LOOSE pre-existi… | superseded | P1 | [task.md](os.MkdirAll-cfg.DataDir-0o700-at-main.go-157-never-tight--72d7f10d/task.md) | _not fetched_ | [DUR-8](../DUR/DUR-8--6f099429/task.md) |
| CORE-12 | CORE-12: defaultListen=":8080" binds all interfaces -- prefer 127.0.0.1:8080 | superseded | P1 | [task.md](CORE-12--ae000d92/task.md) | _not fetched_ | [DEPLOY-1](../DEPLOY/DEPLOY-1--fa0c5a4e/task.md) [DEPLOY-2](../DEPLOY/DEPLOY-2--14f8ec3b/task.md) [CORE-1](CORE-1--eea035e4/task.md) [CORE-4](CORE-4--d9ffbf08/task.md) [AUTH-2](../AUTH/AUTH-2--4b45a6d8/task.md) [AGENTIF-1](../AGENTIF/AGENTIF-1--5bc152d6/task.md) +2 more |
| c0a5bdb6-8b57-4382-adb1-db6657850818 | Re-verify CORE-1's gofmt proof with the corrected ($(go env GOROOT)/bin/gofmt) invocation… | done | P1 | [task.md](Re-verify-CORE-1-s-gofmt-proof-with-the-corrected-go-env--c0a5bdb6/task.md) | _not fetched_ | [CORE-1](CORE-1--eea035e4/task.md) [84b76d5e-fe02-4651-9828-caba3d82606b](../TOOLING/Proof-command-guard-a-run-pattern-that-matches-no-test-m--84b76d5e/task.md) |
| CORE-10 | CORE-10: .gitignore has no secret patterns while the stop hook stages with \`git add -A\` | done | P2 | [task.md](CORE-10--27ad23ef/task.md) | _not fetched_ | [CORE-1](CORE-1--eea035e4/task.md) [CORE-4](CORE-4--d9ffbf08/task.md) |
| CORE-15 | CORE-15: logging.format() calls .String()/.Error() on a typed-nil -&gt; panic inside the log… | done | P2 | [task.md](CORE-15--68ad525c/task.md) | _not fetched_ | [CORE-1](CORE-1--eea035e4/task.md) [CORE-4](CORE-4--d9ffbf08/task.md) |
| CORE-6 | CORE-6: logging maxValueLen=1024 truncates panic stack traces (exempt \`stack\` or raise to… | done | P2 | [task.md](CORE-6--f4ce3f6a/task.md) | _not fetched_ | [CORE-1](CORE-1--eea035e4/task.md) [CORE-4](CORE-4--d9ffbf08/task.md) |
| CORE-7 | CORE-7: HEAD is 405'd by requireGET while writeJSON still guards MethodHead -- dead code,… | done | P2 | [task.md](CORE-7--60f49022/task.md) | _not fetched_ | [CORE-1](CORE-1--eea035e4/task.md) [CORE-4](CORE-4--d9ffbf08/task.md) |
| CORE-8 | CORE-8: Unmatched paths return ServeMux's text/plain 404, breaking the JSON error contract | done | P2 | [task.md](CORE-8--1e9dae04/task.md) | _not fetched_ | [CORE-1](CORE-1--eea035e4/task.md) [CORE-4](CORE-4--d9ffbf08/task.md) [AUTH-6](../AUTH/AUTH-6--1640e0b4/task.md) |
| CORE-9 | CORE-9: Set IdleTimeout + MaxHeaderBytes on http.Server -- and deliberately leave Read/Wr… | done | P2 | [task.md](CORE-9--a1f74fcc/task.md) | _not fetched_ | [CORE-1](CORE-1--eea035e4/task.md) [CORE-4](CORE-4--d9ffbf08/task.md) [AUTH-1](../AUTH/AUTH-1--54fa94c0/task.md) [MSG-2](../MSG/MSG-2--50995c75/task.md) [MSG-3](../MSG/MSG-3--2655c6ae/task.md) |
| CORE-11 | CORE-11: shutdownGrace (10s) &lt; defaultPollTimeout (30s) -- record the ctx.Done() contract… | done | P3 | [task.md](CORE-11--fd330606/task.md) | _not fetched_ | [CORE-1](CORE-1--eea035e4/task.md) [CORE-4](CORE-4--d9ffbf08/task.md) [POLL-1](../POLL/POLL-1--1b0635b9/task.md) |
| CORE-13 | CORE-13: Middleware implements Flusher/Hijacker unconditionally and drops io.ReaderFrom (… | done | P3 | [task.md](CORE-13--42cb8a90/task.md) | _not fetched_ | [CORE-1](CORE-1--eea035e4/task.md) [CORE-4](CORE-4--d9ffbf08/task.md) |
| CORE-14 | CORE-14: A handler that writes then panics logs status=200 -- the audit trail is wrong | done | P3 | [task.md](CORE-14--13488041/task.md) | _not fetched_ | [CORE-1](CORE-1--eea035e4/task.md) [CORE-4](CORE-4--d9ffbf08/task.md) [CORE-6](CORE-6--f4ce3f6a/task.md) |
| CORE-5 | CORE-5: Observability: metrics/inspect endpoint (follow-up) | superseded | P3 | [task.md](CORE-5--06c5b1f5/task.md) | _not fetched_ | [ADMIN-8](../ADMIN/ADMIN-8--7f550309/task.md) |

## Epic description

go.mod, cmd/agent-bus main, config/flags, and the two unauthenticated liveness/info routes. Foundation every other epic builds on.

---

_Generated by `scripts/gen-spec-mirror.sh`; never hand-edit._
