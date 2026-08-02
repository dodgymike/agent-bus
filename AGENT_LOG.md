# Agent Log

Per-task work log. Append-only, newest entry last — one dated section per wave.

---

## 2026-08-02 — Wave: CORE-1, CORE-2, CORE-3, CORE-4, DOCS-1

**Chain run: spec-keeper → implementer → test-engineer → reviewer → security → documentation.**
All steps ran; none were skipped.

- **CORE-1** — repo skeleton: `go.mod` (`module github.com/dodgymike/agent-bus`, `go 1.19`), the
  `internal/` package layout (`httpapi`, `logging`, and stub packages for `ids`, `store`, `wal`,
  `hub`, `auth`, `relay`), `cmd/agent-bus/`. Proof: `go build ./...` clean, `gofmt -l .` empty.
- **CORE-2** — `cmd/agent-bus` entrypoint: flag parsing/validation (`-listen`, `-data-dir`,
  `-poll-timeout`, `-log-level`, `-bus-id`), signal-driven shutdown with the root-context-cancelled-
  before-`Shutdown` ordering. Proof: `go test -race ./cmd/agent-bus/...` green, including
  `TestShutdownReleasesLongPoll` (the regression guard for that ordering).
- **CORE-3** — `GET /healthz` and `GET /v1/info`, both unauthenticated. Proof:
  `go test -race ./internal/httpapi/...` green.
- **CORE-4** — `internal/logging` (structured logfmt logger) + `LoggingMiddleware` (request id
  handling, panic recovery, per-request log line). Proof:
  `go test -race ./internal/logging/... ./internal/httpapi/...` green.
- **DOCS-1** — this entry, plus `README.md`, `DECISIONS.md`, `CONTRACTS.md`. Proof:
  `test -s README.md && test -s DECISIONS.md && test -s CONTRACTS.md`.

Every task above also passed `go build ./...`, `go vet ./...`, and `gofmt -l .` clean, per the
"Verify" section of `CLAUDE.md`.

**Reviewer:** PASS-WITH-NITS. **Security:** PASS. **Zero P0s from either pass.**

Three P1s were found and fixed **in-wave** (not filed as follow-ups):
1. `-bus-id` given runtime validation (`^[A-Za-z0-9_-]{1,64}$`, `.` rejected) plus a `WARN` log line
   when used, so the test-only affordance is visibly flagged at runtime, not just in a doc comment.
2. Log-value quoting fixed to treat every byte `>= 0x7f` (not just non-ASCII multi-byte sequences)
   as needing quoting, closing a log-injection path via raw high-bit bytes.
3. `cmd/agent-bus` given a test suite, including a regression test
   (`TestShutdownReleasesLongPoll`) that pins the cancel-root-before-`Shutdown` ordering so a future
   edit that reverses those two lines fails a test instead of silently reintroducing a shutdown hang.

Remaining findings were filed as follow-up tasks rather than fixed in-wave: **CORE-6** (log
`maxValueLen` truncates panic stacks), **CORE-7** (dead `HEAD` guard in `writeJSON` vs. `requireGET`'s
actual 405 behaviour), **CORE-8** (unmatched paths return ServeMux's text/plain 404 instead of the
JSON envelope), **CORE-9** (`http.Server` missing `IdleTimeout`/`MaxHeaderBytes`, with `Read`/
`WriteTimeout` deliberately left unset so a future long-poll isn't killed mid-flight), **CORE-10**
(`.gitignore` has no secret patterns), **CORE-11** (shutdownGrace < defaultPollTimeout — document the
`ctx.Done()` contract), **CORE-12** (`defaultListen` binds all interfaces), **CORE-13** (middleware
advertises `Flusher`/`Hijacker` unconditionally), **CORE-14** (a handler that writes then panics logs
a false `status=200`), **CORE-15** (`logging.format()` panics on a typed-nil `Stringer`/`error`),
**AUTH-6** (routes are wired individually on the mux — fail-open once authenticated routes are
added), plus one pre-existing tooling item noticed in passing: a task to stop `scripts/spec-cloud.sh`
from putting `SPEC_CLOUD_PASSWORD` on the `aws` command's argv (visible via `/proc/<pid>/cmdline` to
any local user for the life of that call).

**Git-hygiene deviation, recorded honestly.** `CLAUDE.md` step 10 asks for one logical commit per
task. What actually landed on `main` is a run of catch-all commits from the repo's
`commit-on-stop` hook — `df79ff6`, `d63c730`, `936cb22` (and two more since:
`6aced19`, `72d3281`) — each titled `Session update: N file(s)` and each spanning more than one of
the five tasks in this wave. No agent in this wave ran `git commit` directly; the hook fired on
session stop and staged/committed whatever was in the tree at that point. There is therefore no
single commit SHA that corresponds 1:1 to any one of CORE-1..CORE-4/DOCS-1. The orchestrator
reconstructs a per-task record from the reported file-path list in each task's `complete` call
(`commit_sha` on each task points at the nearest `Session update` commit that contains its files,
not a dedicated commit) rather than from git history alone. This is noted here so a future reader of
`git log` is not misled into thinking one commit == one task for this wave.

---

## 2026-08-02 — DUR-8: exclusive lock on the bus data directory

**Chain run: spec-keeper → implementer → test-engineer → reviewer → security → documentation.**
All six ran; none were skipped.

**The gap.** `grep -rn 'flock\|LOCK_EX\|lockfile' --include=*.go` over the whole tree returned
exactly ONE hit, and it was a COMMENT, not an implementation (`internal/wal/log.go` ~L274), stating
that the replay-vs-open agreement check "IS NOT A LOCK" and that excluding a second process "needs a
real lock on the data directory (an flock held for the Log's lifetime) and is a follow-up". Until
this task the only thing stopping two servers destroying one WAL was a convention line in
`CLAUDE.md`.

**Shipped.** New stdlib-only package `internal/dirlock`: `Acquire(dir) (*Lock, error)` takes
`syscall.Flock(LOCK_EX|LOCK_NB)` on `<data-dir>/bus.lock` (0o600, `O_NOFOLLOW`, no `O_TRUNC` before
the lock is held), records `<pid>\n` only afterwards, and `Release` unlocks + closes without ever
unlinking. `cmd/agent-bus`'s `run()` acquires it after `os.MkdirAll` and before
`ids.LoadOrCreateBusID`, so every read/write of the data dir — including any future `wal.Open` — is
inside the lock. A second server exits 1 naming the directory and the probable holder pid.

**Deviation from the task text**, recorded in `DECISIONS.md` per CLAUDE.md step 8: the task said the
lock "goes in `internal/wal/log.go` Open"; it went into a new package taken at process startup
instead, because `wal.Open` is not the first thing to touch the data dir (`ids.LoadOrCreateBusID`
is), and "one server per data directory" is a property of the process, not of one file handle.

**Proof — this is a durability claim, so "the code looks right" was not accepted as evidence:**

- `TestDirLockCrossProcessExclusion` — **cross-process**, by re-execing the test binary in the same
  idiom as `internal/wal/replay_crash_test.go`. An in-process test can pass while the real bug
  remains, so the parent holds the lock, a genuinely separate process is refused with `ErrLocked` and
  the parent's pid, and then — the half that stops a lock that refuses *everybody* from passing —
  a fresh child SUCCEEDS after Release.
- `TestDirLockReleasedAfterSIGKILL` — kill -9 of a holding child, with the death verified from the
  WAIT STATUS (`Signaled() && Signal() == SIGKILL`), not from `err != nil`; then the lock file is
  asserted still present (nothing unlinks) and the next `Acquire` asserted to succeed with no
  cleanup. That is the "there is no such thing as a stale lock" claim, asserted rather than assumed.
- Children write a REPORT FILE the parent asserts on, because a test binary given a `-run` pattern
  that matches nothing exits 0 — the vacuous shape `scripts/proof-check.sh` exists to catch.
- `TestAcquireRefusesASymlinkedLockFile` — the `O_NOFOLLOW` hardening, both the existing-target and
  the (destructive) dangling-target case.
- `TestRunRefusesALockedDataDir` — the WIRING: a refused `run()` returns `ErrLocked` and leaves
  `bus.lock` as the ONLY entry in the data dir, i.e. it bailed before `ids.LoadOrCreateBusID`. This
  is the ordering guard DUR-9 needs.
- The test-engineer MUTATION-TESTED the suite in a throwaway copy outside the repo: `LOCK_EX`→
  `LOCK_SH` fails both headline tests, a naive pid-file lock fails the SIGKILL test, and
  `SIGKILL`→`SIGTERM` trips the wait-status guard. The tests discriminate.

Every test uses `t.TempDir()`; the tracked `data/` directory was never touched.

**Proof:** `bash scripts/proof-check.sh --quiet '<proof_cmd>'` →
`verdict=PASS class=test exit=0 tests_run=26 top_level=11 skipped=1 failed=0 empty_pkgs=0`
(the 1 skip is the env-guarded re-exec child helper, a no-op in a normal run). `go build ./...`,
`go vet`, `go fmt` clean; `go test -race -count=2 ./internal/dirlock ./cmd/agent-bus` green.

**Reviewer:** PASS on the code, no P0, 3 P1s — all three fixed before completion (vacuous stored
`proof_cmd` repointed; the untested `run()` wiring covered by `TestRunRefusesALockedDataDir`; this
entry and the `DECISIONS.md` deviation record written). **Security:** SAFE-TO-SHIP, no P0/P1; its
one P2 worth taking — `O_NOFOLLOW` — was applied and given its own test. Remaining P2s are recorded
as follow-ups on the Spec Server, not silently dropped.

**Not applicable:** no `scripts/bus-*.sh` wrapper or `AGENT_PROTOCOL.md` entry (invariant 7), because
this task adds no agent-facing capability — no route, no flag, no env var, no header. `CONTRACTS.md`
gained an "On-disk files in the data directory" section instead.
