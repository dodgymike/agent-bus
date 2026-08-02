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

---

## 2026-08-02 — DUR-9: wire the WAL into server startup

**Chain run: spec-keeper (task already `in_progress` from triage) → implementer → test-engineer →
reviewer → security → documentation.** All ran; none were skipped.

**The gap.** The durability plane was a complete LIBRARY and entirely absent from the RUNNING BINARY.
Verified before starting: `wal.Open` had ZERO non-test callers in the whole repo, and the only match
for `internal/wal` under `cmd/` or `internal/httpapi/` was a COMMENT. DUR-1..DUR-4 were all `done`
and none of their behaviour was reachable from the process. This task wires what already existed; it
adds no WAL feature.

**Shipped.** `cmd/agent-bus`'s `run()` now calls
`wal.Open(wal.LogOptions{Dir: cfg.DataDir, Logger: lg})` strictly AFTER `dirlock.Acquire` (opening a
log a second process already holds is the corruption DUR-8 exists to prevent) and strictly BEFORE
`srv.Serve`, so replay always completes before anything is answered (invariant 5). Any open/replay
failure is FATAL: `run()` returns, `main()` prints `agent-bus: opening the write-ahead log in
"<dir>": …` and exits 1, and nothing binds — the only self-repair is the provably torn tail
`RepairTail` already handles. The `*wal.Log` is held for the process lifetime, closed on the existing
SIGINT/SIGTERM path by a `defer` registered after the lock's, so LIFO closes the log while the data
dir is still locked. One INFO line (`msg="write-ahead log opened"` with `records_replayed`, `applied`,
`aborted`, `dangling`, `next_index`, `repaired`, `repaired_bytes`) plus a DEBUG `write-ahead log
closed` line make both halves observable. The log is exposed to the HTTP layer as the one-method
`httpapi.DurableLog` interface (`Options.Durable`, `Server.Durable()`); NO handler uses it yet and no
route changed.

**The Applier is nil, deliberately and honestly.** There is no in-memory serving copy yet
(`internal/store` is a `doc.go` stub), so replay today is a durability fsck that rebuilds no state.
That is stated in the code and in `CONTRACTS.md` rather than papered over; see `DECISIONS.md`.

**Tests.** `cmd/agent-bus/wal_startup_test.go` runs the server in a real SUBPROCESS (a `TestMain`
that sets `os.Args` and calls `main()`, so `parseFlags`, `Config.validate` and main's error→exit
mapping are all under test rather than re-implemented):
- `TestServerOpensWALOnStart/fresh_data_dir` — `bus.wal` created non-empty; the stderr order
  `data directory locked` < `write-ahead log opened` < `server started`; `/healthz` 200 on the
  ephemeral port the child reports; SIGTERM → exit 0; and `write-ahead log closed` before
  `data directory lock released`.
- `/replays_an_existing_log` — the data dir is SEEDED with 3 committed transactions before start and
  the startup line must account for all of them, with expectations DERIVED from what was written
  (2×3 records, `applied=3`, `next_index=last.CommitIndex+1`), not hard-coded.
- `TestServerOpensWALOnStartRefusesACorruptLog` — a garbage file header must exit 1, bind nothing,
  name the data dir, and leave the file BYTE-FOR-BYTE unchanged (a refusal must never "repair").
- `internal/httpapi/durable_test.go` — `Options.Durable` round-trips by identity, a nil Durable is
  safe, and `/healthz` + `/v1/info` are byte-identical with and without a log.

**Proof:** `bash scripts/proof-check.sh --task DUR-9` →
`verdict=PASS class=test,wrapper,file-assertion exit=0 tests_run=8 top_level=4 skipped=0 failed=0`.
The proof's last clauses are the invariant-7 end-to-end check: a real server brought up through
`scripts/bus-serve.sh` (isolated `AGENT_BUS_RUN_DIR=/tmp/agent-bus-dur9`, port 8091, never the
tracked `data/`) leaves a non-empty `bus.wal`. `go build ./...`, `go vet ./...`, `gofmt -l` clean;
`go test -race -count=1 ./...` green including `internal/wal`.

**Reviewer:** CHANGES REQUIRED on the first pass, and it was earned — the reviewer MUTATION-TESTED
the suite and found that deleting the deferred `walLog.Close()` left every test GREEN while the test
header claimed to prove close-on-shutdown (the kernel closes the fd at exit, so the file looks
identical). It also disproved a comment claiming that moving `wal.Open` after `net.Listen` "must fail
exactly here" — it does not; binding early answers nothing, and only "served after replay" is
enforced. Both were fixed in a second pass: the DEBUG close line + an assertion that it precedes the
unlock line (mutation-verified to FAIL when the defer is deleted), the corrected ordering comment
plus the full lock < open < serve chain, and the harness now calls `main()` instead of copying its
exit mapping (mutation-verified: `os.Exit(1)`→`os.Exit(2)` now fails the corrupt-log test, and
previously did not). **Security:** PASS, no must-fix; its doc-accuracy nit was taken (a start refused
at the LOCK leaves only `bus.lock`; a start refused later has legitimately written `bus-id`). Its
remaining findings are follow-ups, reported to the orchestrator rather than silently dropped:
`bus.wal` is opened without `O_NOFOLLOW` while `dirlock` deliberately uses it; `os.MkdirAll` never
tightens an already-loose data dir; and startup makes three full passes over the log
(`RepairTail`/`Replay`/`OpenWriter`).

**Not applicable:** no `scripts/bus-*.sh` wrapper or `AGENT_PROTOCOL.md` entry (invariant 7), because
this task adds no agent-facing capability — no route, no flag, no production env var, no header.
`CONTRACTS.md` gained "The write-ahead log at startup" instead.

---

## 2026-08-02 — DUR-11: recovery always restarts, and every discard is observable

**Task:** DUR-11 (`884d3da4-bceb-4ac2-93a2-e147c77f9dca`). Re-scoped against the user's policy
reversal of the same day (`DECISIONS.md`, "Availability over retention"): *"always be able to
restart, prefer to discard messages and/or corruption, with logging"*. The stored description
predated the reversal and was overridden.

**The rule the whole change is written against:** DAMAGE IS NEVER FATAL. NOT BEING ABLE TO READ THE
FILE STILL IS. Damage — a torn frame, a flipped bit, a lost sector, a payload that will not decode, a
record type with no meaning here, a corrupt file header — is discarded, logged with offset/index/
type/length, and recovery continues. Permission denied, a device I/O error, an audit file where a WAL
was expected, a format version this binary does not implement, an unknown `Kind`, and the
replay/open disagreement check all stay fatal, because none of them are damage.

**Verified before changing anything.** The task's finding (a) — that the tail veto only fires when
the file ends exactly on a record boundary — was ALREADY FIXED by DUR-4 (`dad04aa`). A probe against
a /tmp copy reproducing the reported shape (mid-file length bit flip + one junk byte at EOF) showed
the veto firing correctly and REFUSING to start, not deleting eight records. The security probe
predated the hardening. What (a) actually required under the new policy was the opposite work:
discard the damaged record and KEEP the intact ones behind it, which no amount of veto can do.

**New:** `internal/wal/salvage.go` — a tolerant walk (`salvage`) that resynchronises past damage by
searching FORWARD for the next intact record by RECORD INDEX (`resyncFrom`), plus `rewriteLog`
(temp file + atomic rename, survivors keep their ORIGINAL indices) and `quarantine` (rename aside,
never delete). `RepairTail` → `RepairLog`; `TailRepair` kept as a type alias. `scanFrom` relaxed
from a DENSE index sequence to a strictly RISING one, because a repaired log has permanent holes and
renumbering survivors would reuse ids. `Replay`'s semantic failures became discards. A record whose
LENGTH FIELD alone is corrupt is now RECOVERED rather than discarded — the old veto check,
repurposed to lose less.

**Reviewer:** CHANGES-REQUESTED, and it was earned. It passed the implementation ("all three
judgement calls correct, all in scope"; it independently verified chunk-overlap correctness,
two-pass determinism and crash-safety) and failed the DOC deliverable, naming four sentences it
would not sign: "damage does not cascade" stated flatly while `Repair.Exhausted` documents the
cascade; "a genuine proof" and "only the true length reproduces the stored value", both false for a
32-bit CRC and forgeable besides; "EVERY field here is also written to the operator log"; and
"Open logs every entry of Recovered.Discarded" against a 64-entry retention cap. All four rewritten.

**Security:** CHANGES-REQUESTED with a reproduced P0 in the NEW code, and it is the finding of the
task. `resyncFrom`'s index-DENSITY window bounded a candidate's index by how many records could
still fit before EOF — which after a large hole is smaller than the real next index. The genuine
next record was rejected by a cheap filter, the search reported "nothing follows", and recovery
deleted every committed record to the end of the file WHILE LOGGING THAT IT HAD FOUND A TORN TAIL.
Measured on indices 1, 2, 50001, 50002 with one flipped length bit: an acknowledged write gone, no
error. This is the exact cascade the function was written to prevent, arriving through the fix for
it. The search now runs in TWO STAGES — density window first, then the same scan without it — under
the rule **a bounded search finding nothing is never on its own grounds for "nothing follows"**.
`TestWALResyncSurvivesALargeIndexHole` pins it and is mutation-checked: restoring the density bound
on stage two fails it.

Security also measured `Replay` retaining 8 bytes per PREPARE in the whole FILE (1.76 MB on a
23.7 MB log, linear) through a side list compacted only from the eviction path, which never runs on
a healthy log — the documented O(unresolved prepares) bound broken, and at 10 GiB the boot-time OOM
the eviction exists to avoid. The list is deleted; the victim is now found by scanning the ≤1024
live entries, which also removed a stranding bug the reviewer had flagged separately.

Its HIGH-2 (forged frames admitted by an unkeyed CRC32C) is real and today unreachable only because
a frame header contains NUL bytes and every WAL payload goes through `json.Compact`. That was true
by ACCIDENT and nothing recorded it; it is now written down in `resyncFrom`'s doc and pinned by
`TestWALPayloadsCannotCarryAFrameHeader`, which fails the moment the payload channel widens to
arbitrary bytes — making the keyed MAC a blocking precondition for that change rather than a later
improvement. Not gated on the MAC, on security's own advice.

**Chain:** spec-keeper (orchestrator, task already `in_progress`) → implementer role taken by
feature-runner directly — recorded here as required, and taken because the design (salvage, resync,
rewrite-vs-truncate, the fatal boundary) had to be settled from probes rather than described →
test-engineer → reviewer → security. **`documentation` was NOT run**, deliberately: every remaining
doc surface (`PROTOCOL.md`, `CONTRACTS.md`, `cmd/agent-bus/main.go`) is OUTSIDE this task's
file-ownership boundary and is handed to the orchestrator as required follow-up, listed in the
final report.

**Proof:** `bash scripts/proof-check.sh "go test -race -run 'TestCrashInjection|TestWALRepairTail'
./internal/wal"` → `verdict=PASS class=test exit=0 tests_run=72 top_level=20 skipped=1 failed=0`
(the skip is `TestCrashInjectionChild`, the re-exec harness, which only runs as a subprocess).
`go build ./...`, `go vet ./...`, `gofmt -l` clean, `go test -race -count=1 ./...` green.

**Two process failures worth recording, neither mine to fix:** this task's work was swept into a
commit titled "AUTH-1: enrolment and session establishment" by a parallel agent committing without a
pathspec — the one-logical-commit-per-task rule is broken for DUR-11 and the history no longer says
where this change came from. And a log repaired by this version carries index holes that a
PRE-DUR-11 binary rejects outright (`reader.go` at `6d792b2` requires a dense sequence), so this is a
one-way upgrade for any data directory that has been repaired.

---

## 2026-08-02 — AUTH-2: token verification middleware, default-deny (folds in AUTH-6)

**Task.** AUTH-2 (P0, `4b45a6d8`) — validate the bearer token on every route except an explicit
unauthenticated allow-list, 401 on missing/malformed/forged/expired, attach the verified
fully-qualified agent id to the request context. AUTH-6 (P1, `1640e0b4`) was folded into the same
change on the orchestrator's instruction: routes were registered individually on the mux, so a later
`mux.HandleFunc("/v1/send", …)` without the auth wrapper would have shipped an unauthenticated route
on a message bus **with no failing test**. Retrofitting default-deny afterwards would have meant
reviewing the enforcement change twice.

**Shape.** `authMiddleware` wraps the WHOLE mux — `LoggingMiddleware(s.log, s.authMiddleware(mux))`,
that order being load-bearing so 401s are logged and `RequestIDFromContext` resolves inside the auth
middleware. Exact-match allow-list of five paths; everything else 401. `auth.Service.Authenticate` is
consumed unchanged (`internal/auth` was NOT touched — it belongs to a parallel AUTH-3 pass). Tokens
stay opaque server-side handles: no claim parsing, no caching, so revocation and expiry are immediate.
A `(*Server).route` registry exists solely because Go 1.19's `http.ServeMux` cannot be enumerated and
the enumeration test needs to walk the real surface.

**Two design calls, both recorded in `DECISIONS.md`.** BOTH session routes are allow-listed —
`/v1/session/complete`'s caller holds a token, but it names a PENDING session that `Authenticate`
rejects exactly like an unknown one, so a bearer requirement there would be *unsatisfiable, not
strict*; that route is authenticated by the Ed25519 signature instead. And an anonymous request to an
UNKNOWN path returns **401, not 404** — any 404-before-authenticating means checking route existence
outside the wrapper, which is itself an unauthenticated code path, i.e. the exact shape AUTH-6 forbids.
This CONSTRAINS CORE-8: its JSON 404 catch-all must be registered inside the wrapper.

**The reviewer caught a vacuous test, which is the finding worth remembering.**
`TestEveryRouteRequiresAuth`'s headline loop — the actual AUTH-6 deliverable — passed with **zero
children**: all five registered routes are allow-listed, so `continue` fired every iteration and the
body never executed. The existing `len(routes) == 0` guard did not catch it, because the slice is
non-empty; it is the *filtered* set that was empty. A loop that has never run its body is not evidence
that the loop works. Fixed by `TestEveryRouteRequiresAuthOnASyntheticRoute`, which rebuilds the stack
with the real `route`/`authMiddleware`/`LoggingMiddleware` helpers plus one genuinely protected
`/v1/synthetic` route, and asserts `probed > 0`. That test is the only place AUTH-6's actual claim — *a
route added later is authenticated because it was registered, not because someone remembered* — is
exercised until the first real protected route lands. The black-box loop now logs (never fails) when
it probes nothing, pointing at that test.

**Security (PASS, no P0/P1)** probed the allow-list over raw TCP rather than reasoning from Go source:
27 request lines (`%2f`/`%2e%2e`, dot-dot walks, `;`-params, absolute-form request-URI, `CONNECT`,
doubled/trailing slashes) and 10 `Authorization` header shapes. All divergent spellings landed on the
deny side. Two conclusions worth not re-litigating: the proxy header-folding attack is defeated twice
independently (no-embedded-space rule, then the base64url alphabet filter, since `,` is outside it);
and **constant-time token comparison would be cargo cult here** — the table is keyed on
`hex(SHA-256(token))`, so a near-miss guess yields an uncorrelated hash. One accepted coverage
boundary: `OPTIONS * HTTP/1.1` never reaches the middleware (Go's `globalOptionsHandler` answers above
the application handler; `DisableGeneralOptionsHandler` is go1.20+ and we pin go1.19). Written into
`authmw.go`'s doc comment rather than worked around.

**Chain:** spec-keeper (corrected a VACUOUS `proof_cmd` — it named `./internal/auth` for middleware
living in `./internal/httpapi`) → implementer → test-engineer → reviewer → security → documentation.
All six ran; none skipped. The one code edit made by feature-runner directly is the `OPTIONS *`
comment in `authmw.go`, on security's recommendation.

**Proof:** `bash scripts/proof-check.sh "go test -race -run 'TestAuthMiddleware|TestEveryRouteRequiresAuth' ./internal/httpapi"`
→ `verdict=PASS class=test exit=0 tests_run=69 top_level=4 skipped=0 failed=0 empty_pkgs=0`.
`go build ./...`, `go vet ./...`, `gofmt -l .` clean, `go test -race -count=1 ./...` green. ALSO
exercised against the RUNNING binary via `scripts/bus-serve.sh` on a throwaway `/tmp` data dir: a real
enrol → session/begin → Ed25519 sign → session/complete handshake, then 401 with no token, 401 with a
forged token, 401 with a PENDING token, 401 on a non-`Bearer` scheme, and **404 with the valid token**
— 404 being the proof the middleware passed the request to the mux, since an anonymous caller can never
reach it. Server log confirmed to contain no token.

**Process failure, and it is the second time this session.** A parallel agent committed this task's
in-flight files as `ce226a8` while the test-engineer was still editing them — no `git add`/`git commit`
was run by anyone in this task's chain. The committed content happens to be correct (verified: the
post-revert `/v1/synthetic` version, not the test-engineer's temporary mutation), but the commit also
swept in `internal/ids/agentmint_test.go`, which belongs to a different task. That is the
one-logical-commit-per-task rule broken again, by the same mechanism as the DUR-11 incident recorded
above: a sweeping `git add` with no pathspec. The docs for this task (`CONTRACTS.md`,
`AGENT_PROTOCOL.md`, `DECISIONS.md`, this file) were still unwritten at that point, so AUTH-2's
documentation lands in a SEPARATE commit from its code — the task is split across two commits through
no choice of its own.
