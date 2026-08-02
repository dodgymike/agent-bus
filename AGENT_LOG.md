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

## 2026-08-02 — AUTH-1-FU-PENDINGCAP: the per-agent pending cap is removed (P0 lockout)

**Task:** `687ad8c9-b111-4ff9-8411-d01e9ba82383`, dispatched by triage run `triage-20260802-r3`.
**Chain run:** spec-keeper → test-engineer (RED) → implementer (GREEN) → reviewer → security →
documentation. All six ran; none skipped.

**One sentence:** `MaxPendingPerAgent` was keyed on `agentID`, which on the unauthenticated
`/v1/session/begin` route is an attacker-supplied *victim* identifier, so 9 anonymous requests per
round permanently locked out any named agent — the cap is deleted outright rather than retuned, and
`MaxSessions` plus expiry now bound the table alone. Full rationale, including why option (a)
(re-keying on request source) was rejected in favour of option (b), is in `DECISIONS.md` under the
same date.

**Deliberately RED first.** The adversarial test was written and run against the unfixed code before
any production line changed, and all three of its subtests failed with
`unknown or expired session: no session for the presented token` — the victim being unable to complete
its own challenge. That is the evidence the test exercises the defect rather than merely asserting
that memory stays bounded, which would have proved nothing here.

**Proof:** `bash scripts/proof-check.sh 'go test -race -run TestSessionBeginNoVictimLockout ./internal/auth'`
→ `verdict=PASS class=test exit=0 tests_run=4 top_level=1 skipped=0 failed=0 empty_pkgs=0`.
`go build ./...`, `go vet ./...`, `go test -race ./internal/auth`, `$(go env GOROOT)/bin/gofmt -l internal/auth`
all clean. Not exercised against a running server: this task changes no route, request, response or
wrapper — the HTTP surface is byte-identical, and `cmd/` and `internal/httpapi/` never referenced the
removed option (verified by grep).

**Both gates found real things, and both were folded in rather than deferred.** The reviewer caught
that the new comment claimed a challenge "leaves this table by exactly two routes" while
`CompleteSession`'s single-attempt rule is a third, and that "a flooder can *still* fill this table"
understated the change — global exhaustion got CHEAPER, since pending entries used to be bounded by
cap × roster size. Security (PASS-WITH-FINDINGS) raised a HIGH: the stated bound "`MaxSessions` plus
`ChallengeTTL` and nothing else" is wrong for ACTIVE sessions, which are reclaimed only after
`SessionLifetime`, making a cheaper and hour-long outage reachable by an attacker that enrols its own
agent. All three corrections landed in `internal/auth/session.go` and `CONTRACTS.md` before
completion. This mattered more than usual: this package was bitten once already by a comment that
described a lockout primitive as a defence, and a comment that justifies REMOVING a control while
understating what remains is the same defect wearing the opposite hat.

**Follow-ups filed:** `AUTH-1-FU-ACTIVECAP` (`2d92b699-818a-4fd0-bbb7-76c06449756b`, P1) for the
uncapped active-session gap — the one place an agent-id-keyed cap IS safe, because an active session
requires proving possession of the private key. A three-point rider was posted to
`AUTH-1-FU-SESSIONSCALE` (`067b80cf-…`): its planned evict-globally-oldest-pending policy reintroduces
this same class, it will fail an existing subtest that must be honoured rather than rewritten, and two
of the three O(n) scans it was written to fix (`countPendingLocked`, `oldestPendingLocked`) no longer
exist.

**Commit hygiene note.** The working tree already held staged work from AUTH-2 (docs) and
AUTH-1-FU-LISTENADDR (`cmd/agent-bus/main.go`, `README.md`) when this task started. This task's paths
are disjoint from those and are listed explicitly in its final report so the commit can be split; no
`git add -A` was used anywhere in this chain, and nothing was committed by it.

## 2026-08-02 — ID-2-WIRING-SCHEMA (80b54ee4): where the message sequence high-water mark lives on disk

**Agent:** feature-runner (opus). **Task:** P0 docs-only decision task blocking the floor derivation.
**Files changed: `DECISIONS.md` ONLY** (+158/-0, append-only new dated section). No code, no
reservation taken, no `ondisk-format-version` value consumed. Staged with an explicit pathspec
(`git add DECISIONS.md`); not committed — the orchestrator commits.

**Outcome: Option A′ chosen.** The message sequence is a top-level cleartext `"seq"` field of the WAL
message body; `wal` offers every PREPARE to an observer during the replay pass that already happens
and the *application* decodes it, so `wal` still does not interpret `Body`. No on-disk format change.

**The §4.4 disproof test was RUN and does not fire.** The deep-diver flagged reading the CRYPTO epic
task bodies as its one bounded gap (§0); this task closed it. No CRYPTO task specifies
`wal.Entry.Body` for `Kind=="message"` as a bare opaque blob — CRYPTO-5..9 are all `deferred`, and
CRYPTO-11/CRYPTO-12 say the opposite outright (bodies are cleartext, not encrypted), which is already
the recorded user decision at `DECISIONS.md:197`. It further does not fire even in the world where
encryption returns: the superseded ratchet design itself kept a `{ciphertext, ratchet_header, …}`
envelope; a bare blob is not representable at the WAL layer at all (`Entry.Body` must be valid JSON);
and DUR-5 plus SIGN-4 independently require the server to read `seq` out of the body.

**Proof:** `bash scripts/proof-check.sh "grep -q 'message sequence high-water mark' DECISIONS.md"` →
`proof-check: verdict=PASS class=file-assertion exit=0`. Verified RED (0 matches) before the change,
so the pass is non-vacuous. A tightened, heading-anchored variant also passes:
`grep -q '^## .* The message sequence high-water mark lives in the WAL message body, read via a replay-time PREPARE observer' DECISIONS.md`.

**Chain justification (CLAUDE.md step 10).** reviewer and security gates SKIPPED: this change is
documentation only — a single append to `DECISIONS.md`, no executable surface, no route, flag, env
var or record type touched, and nothing to audit for vulnerabilities. spec-keeper filed the task and
holds completion (this agent posts notes but never commits, so it cannot supply a `commit_sha`).
`kind=request`, `kind=report` and `kind=model` notes posted to the task by `feature-runner`.

**Left for others:** the contract sentence this decision implies is reported to the orchestrator
verbatim rather than written here — `CONTRACTS.md` is mid-split into per-plane files by a concurrent
agent, so its follow-up task must carry a `proof_cmd` that globs `CONTRACTS*.md`. ID-2-WIRING-OBSERVER
(c31f6999) is unblocked as specced.

### 2026-08-02 — ID-2-WIRING-SCHEMA addendum (same task, second append)

While finishing, the user's `4110946` ("Five decisions") landed concurrently and swept my staged
`DECISIONS.md` append into it — there is no standalone commit for this task; the decision text ships
inside `4110946`. Its decision 3 ("NO id reuse — invariant 1 stands, and the salvage reissue is a
DEFECT") contradicts the FRAMING of one residual paragraph I had just written, which called the
quarantine case an accepted invariant-1 narrowing on `e120153b`'s docket. `DECISIONS.md` is
append-only and the paragraph was already committed, so I appended a reconciliation section rather
than editing it. The CHOICE is unaffected (the residual was recorded as shared by A′ and B and
explicitly not a differentiator); what changes is that it is a defect to fix, not a limitation to
accept — and it is sharper for the sequence than for the record index, because quarantine restarts a
fresh log at index 1, so a post-quarantine floor of `0` would reissue every sequence ever used.
Recorded as a required fail-closed behaviour of ID-2-WIRING-STARTUP. Staged: `DECISIONS.md`,
`AGENT_LOG.md`.

## 2026-08-02 — CONTRACTS-SPLIT: split CONTRACTS.md into per-plane files (feature-runner)

Task: `360a2679-b5dc-4b17-863f-fb4462764e6d` (title "CONTRACTS-SPLIT: split CONTRACTS.md into
per-plane files (pure move) + retarget every proof_cmd", no `<EPIC>-<N>` key invented — created fresh
this loop, since it did not previously exist in the backlog). Status left `in_progress` with a
`status_note` of "CODE-COMPLETE, UNCOMMITTED" — feature-runner never commits, so it is not flipped to
`done` until the orchestrator commits and records the real `commit_sha`.

**What moved.** `CONTRACTS.md` (532 lines) split into 4 new per-plane files as a PURE content move —
no wording changed, only location:
- `CONTRACTS-CLI.md` — CLI flags (`cmd/agent-bus`) + env vars.
- `CONTRACTS-HTTP.md` — routes, headers, enrolment/sessions, authentication.
- `CONTRACTS-ONDISK.md` — record types/wire protocol versions, on-disk files, WAL at startup. Most
  in-flux plane (DUR / on-disk format version 2), isolated deliberately.
- `CONTRACTS-AGENT.md` — agent-facing wrappers (`scripts/bus-*.sh`) + repo tooling scripts.
`CONTRACTS.md` stays at the old path, rewritten as a short index pointing at the four files above.

**Equivalence proof (mechanical, not eyeballed).** Reassembled the bodies of the 4 new files (each
stripped only of its new intro header) in the original file's section order and diffed against
`git show HEAD:CONTRACTS.md | sed -n '8,$p'` (everything after the old intro paragraph) — zero diff,
byte-for-byte. Command + output are in this task's `kind=report` note.

**Two known-wrong passages moved unchanged, per instruction (not this task's job to fix):**
- `CONTRACTS-CLI.md`'s `-listen` default row still reads `:8080` (code already reads
  `127.0.0.1:8080` in `cmd/agent-bus/main.go` — the doc is now the stale side). Owned by `b0a5630b`
  / `c27f9439`.
- `CONTRACTS-ONDISK.md`'s WAL-repair section — checked, and found it does NOT actually contain the
  stale "provably torn tail"/`RepairTail` wording `5b178dde` (DUR-11-FU-CONTRACTS) assumed was still
  there; it already reads "Recovery ALWAYS reaches a running server ... This supersedes the previous
  wording, which said those cases 'refuse to start'." That content was apparently already corrected
  by an earlier commit before this split ran. Flagged on `5b178dde`'s own task notes as possibly
  stale/already-resolved — did not close or edit that task's status, that is not this task's call.

**Retargeted 7 dependent tasks' `proof_cmd`s/notes** (via spec-keeper) whose descriptions or
`proof_cmd`s named `CONTRACTS.md` with specific content: `c27f9439`, `b0a5630b` (had no `proof_cmd`
at all — one added), `5b178dde`, `8c9b6489`, `c31f6999`, `2d92b699`, `a24bb214`. Ran
`scripts/proof-check.sh` on every retargeted grep-based proof and quoted the verdict rather than
assuming: `c27f9439`=FAIL (RED, expected — doc still wrong), `b0a5630b`=FAIL (RED, expected),
`5b178dde`=PASS (GREEN, unexpectedly — see above), `a24bb214`=PASS (unchanged, trivially still valid
since the index file remains non-empty).

**CLAUDE.md** — two surgical edits only, per instruction to keep this file minimal: the
"Repository layout" block now lists `CONTRACTS.md` as index + the 4 plane files; workflow step 9 now
names which `CONTRACTS-*.md` file to update for which surface (flags/env, HTTP, on-disk, agent). Did
NOT touch CLAUDE.md's "Parallel-agent coordination" single-writer note (still says `CONTRACTS.md`)
or the illustrative `grep` proof example in the "Verify" section — both are now slightly stale in
spirit but out of this task's explicit, minimal-and-surgical scope; flagged as follow-ups.

**Left alone / follow-ups for a later task (not fixed, per file-ownership boundary):**
`README.md:88` and `AGENT_PROTOCOL.md:122` reference `CONTRACTS.md` for content (the record/flag
table, and the `## Authentication` heading respectively) that no longer lives there post-split —
both files are outside this task's ownership boundary this loop.

Chain: spec-keeper (task creation + proof_cmd retargeting + status_note) → implementer (the split
itself, done directly by feature-runner as a mechanical/documentation change) → reviewer/security
SKIPPED for this task — one-line justification: this is a documentation-only, pure-content-move
change (zero lines of Go touched, zero routes/flags/behaviour changed, mechanically proven
byte-identical to the pre-split content) with no security surface; the mandated chain's intent
(catch behavioural/security regressions) does not apply to a verified no-op content relocation.
documentation step folded into this same task (feature-runner IS the documentation change here).

## 2026-08-02 — LISTENADDR-FU-CONTRACTS (b0a5630b), feature-runner (sonnet)

Task: fix the stale `-listen` default row in `CONTRACTS-CLI.md` (still showed `:8080`, verbatim-moved
by the CONTRACTS split, this task's job to fix). Replaced with the agreed wording matching
`README.md:48`: `| \`-listen\` | \`127.0.0.1:8080\` | TCP address to bind (loopback-only by default;
use \`:8080\` for all interfaces) |`.

Proof (`bash scripts/proof-check.sh`) before fix: FAIL (exit 1, row still `:8080`). After fix: PASS
(`ALL_OK`). Re-ran c27f9439 (AUTH-1-FU-LISTENADDR)'s combined proof
(`defaultListen` in main.go AND the CONTRACTS-CLI.md row) — also PASS (`ALL_OK`), unblocking that
task's completion.

Checked new invariant 11 (TLS required transport, loopback default bounds exposure not a substitute)
for consistency: the `-listen` row is orthogonal to TLS (bind address only) and CONTRACTS-CLI.md has
no TLS flag row yet, so no expansion needed — left alone.

Docs-only, single-row change: reviewer/security skipped per CLAUDE.md step 10 justification (no code
touched, one grep-anchored table row, verified by proof-check before/after).

File-ownership boundary: CONTRACTS-CLI.md only. Did not touch CONTRACTS-ONDISK.md, CONTRACTS-HTTP.md,
CONTRACTS-AGENT.md, CONTRACTS.md, CLAUDE.md, README.md, or any internal/** path (per boundary — DUR-12,
internal/ids, internal/auth, and the backlog agents are concurrently live).

Left task `in_progress` (status_note CODE-COMPLETE/UNCOMMITTED) — feature-runner does not commit;
orchestrator commits after verification.

## 2026-08-02 — ID-2-WIRING-SEAL: Sequence refuses to issue from an UNSEALED floor

Task `ID-2-WIRING-SEAL` (public_id `8c9b6489-abb1-444e-9eeb-3ff87646f632`, P0), run by feature-runner
(opus). Split out of `ID-2-WIRING` on the deep-diver's recommendation (`ID2_WIRING_DEEPDIVE.md` §4.1,
§5/T1) as the only half implementable with no schema decision.

**The defect.** `internal/ids.Sequence.RaiseFloor`'s guard `if s.last != 0 && atLeast <= s.last` was
INERT AT STARTUP: `last` is 0 until the first `Next`, so in exactly the window where the floor is
derived, every value — including a far-too-low one — was accepted silently. §3.4 of the deep-dive
proved `go vet` cannot be made to flag a bare `s.RaiseFloor(x)` that drops the error, so the
mitigation could not be a linter; it had to be an API that fails closed.

**The change.** Two states, one-way: UNSEALED → SEALED, one unexported bool under the existing mutex.
Both `NewSequence()` and `Resume(n)` are born UNSEALED. `Next()` returns `(0, ErrFloorUnproven)` and
allocates nothing while unsealed; `Seal()` ends assembly (exactly once — a second call wraps
`ErrFloorSealed` and changes nothing); `RaiseFloor` returns `ErrFloorSealed` after the seal. Guard
ordering is deliberate and asserted by sentinel name: unsealed is checked BEFORE exhaustion (a floor
of `MaxUint64` means a broken derivation, which is recoverable, not "this bus is finished", which is
not), and sealed is checked BEFORE `ErrFloorBelowIssued` (after the seal every `atLeast` gets the same
sentinel). Consequence, recorded openly: `ErrFloorBelowIssued` is now structurally UNREACHABLE on
`Sequence`; the branch was KEPT as defence-in-depth on the reviewer's explicit verdict, and the
sentinel is still live per-name on `NameSuffixes.RaiseFloor`.

**Files:** `internal/ids/sequence.go`, `internal/ids/sequence_test.go`, `internal/ids/messageid_test.go`,
`internal/ids/doc.go`. Nothing outside `internal/ids/`.

**Proof (non-vacuous — the named test was written by this task):**
`proof-check: verdict=PASS class=test exit=0 tests_run=15 top_level=1 skipped=0 failed=0 empty_pkgs=0`
and for the whole package `verdict=PASS tests_run=203 top_level=41 failed=0`. `go build ./...` exit 0,
`go vet` clean, `"$(go env GOROOT)/bin/gofmt" -l internal/ids` empty, `-race -count=2`/`-count=4` no
flakes.

**Chain:** spec-keeper → implementer → test-engineer → reviewer → security → documentation → spec-keeper.
All ran; none skipped. Reviewer PASS-WITH-NITS, security PASS-WITH-NOTES. Both nit sets were fixed
in-task before completion: two doc sentences that were still factually FALSE (`RaiseFloor`'s no-op
bullet still keyed on `Last() == 0` rather than on the seal; "raising to `math.MaxUint64` succeeds
while nothing has been issued" → "while UNSEALED"), the caller-contract sketch's bare `seq.Seal()` now
checks its error, and — security's MEDIUM — the doc now states that a PEER-supplied floor claim is
untrusted input which must be validated and BOUNDED before it reaches `RaiseFloor`, since `RaiseFloor`
applies no upper bound and an unbounded peer claim exhausts the id space at once and permanently.

**Deliberate, tracked debt.** The task said "Update CONTRACTS.md" and this task did NOT: `CONTRACTS.md`
was being split into per-plane files by a concurrent agent in the same loop and admits one writer.
The rows are carried verbatim in `ID-2-WIRING-SEAL-FU-CONTRACTS` (`9c183c8e-ca4f-4b5a-9d74-30c9c2d6f812`,
P1), proof `grep -q 'ErrFloorUnproven' CONTRACTS*.md`, RED today.

**Follow-up filed:** `ID-2-WIRING-SEAL-FU-NAMESUFFIXES` (`1c207a62-e904-4988-84c2-f4b69712ee35`, P1) —
`NameSuffixes` in `agentmint.go` carries the identical inert guard with no seal, and security rates it
HIGH-latent and worse in kind, because the agent id is the routing AND authorization subject. It must
land BEFORE AUTH-3 makes enrolment durable.

**Code-only.** `ids.Sequence` has zero production call sites, so nothing about a running bus changes
and there is nothing to deploy. The derivation that will actually call `Seal()` is `ID-2-WIRING`,
still blocked on `ID-2-WIRING-SCHEMA`.

## 2026-08-02 — AUTH-1-FU-ACTIVECAP (2d92b699), feature-runner (opus)

Task: cap ACTIVE sessions per agent. Enrolment is unauthenticated, so an attacker enrolled its own
agent, completed handshakes, and filled the 16384-entry session table with ACTIVE entries — reclaimed
only after `SessionLifetime` (1h), not `ChallengeTTL` (2m) — holding a global, pre-auth denial of NEW
session establishment for an hour past the flood at ~9 req/s. Filed P1, dispatched as P0-equivalent,
flipped to P0 by spec-keeper mid-run.

**The fix is a placement, not just a counter.** The cap is enforced in `CompleteSession`, NOT
`BeginSession`: after `ed25519.Verify` succeeds and after the already-active early return, immediately
before the pending→active transition. That is what makes an agent-id key SAFE here when
AUTH-1-FU-PENDINGCAP had to remove one from `BeginSession` — on the unauthenticated begin route
`agent_id` is an attacker-supplied VICTIM identifier, so any bucket there is a lockout primitive,
whereas here an entry only enters a bucket behind a valid Ed25519 signature made with that agent's own
enrolment private key. Proven identity, so a flooder can only fill its OWN bucket and the refusal is
self-inflicted. That argument is written into the code so the next reader does not delete the cap as a
repeat of the mistake PENDINGCAP just fixed.

`DefaultMaxActiveSessionsPerAgent = 32` (~16x the compliant steady state of 2 concurrent sessions,
since a client refreshes at 75% of lifetime and old/new overlap; bounds one identity to 0.2% of the
table). Refuse-new, NEVER evict — evicting an agent's own oldest would let a thief who compromised its
key destroy the legitimate holder's LIVE sessions on demand. A refusal mutates nothing: the pending
challenge survives (the single-attempt rule that burns a challenge is for a FAILED VERIFICATION, and
this signature verified). Re-completing an already-active session is never refused (invariant 10).

Chain ran in full: spec-keeper → implementer → test-engineer → reviewer → security → documentation.
reviewer PASS (verified `sess.State = SessionActive` is written in exactly one place tree-wide and is
unreachable without the check; off-by-one exact at caps 1/2/5; refusal is a bare return with no
delete). security PASS, no P0/P1, with five throwaway probes in a /tmp repo copy: 200 concurrent
completions at cap 5 gave exactly 5 + 195 ErrCapacity, race-clean (no TOCTOU); 50 completions naming
the victim signed with the attacker's key all returned ErrBadSignature leaving the table at 0 — a
third party CANNOT consume a victim's bucket. Both gates' P2 comment findings were folded back into
`session.go` (the roster key is re-read at completion time, so the claim also rests on invariant 1 +
`Roster.Put` never overwriting; the compromised-key case; and that 512 of 4096 roster entries is not a
binding constraint).

Honest residual risk, recorded rather than hidden: a global pre-auth active-entry fill is STILL
reachable for +1.6% attacker cost (33280 vs 32769 requests, sustained hold unchanged at ~9.1 req/s),
because `Enrol` accepts duplicate public keys so the 512 enrolments the cap forces come from ONE
keypair. Filed as ac4f9c2b-5460-4e83-997d-0e433194752f; the root fix is the invite-only enrolment EPIC
(0b43393e). This cap is defence in depth behind that gate, not a substitute for it.

Proof (`bash scripts/proof-check.sh`): verdict=PASS class=test exit=0 tests_run=13 top_level=1 for
`go test -race -run TestSessionActiveCap ./internal/auth`; tests_run=99 for the whole package;
tests_run=722 for `go test -race ./...` (run before internal/wal went red — see below). Mutation-tested
by the test-engineer: neutering the check fails 6 subtests, hoisting it above the early return fails
the idempotency subtest, deleting the pending session on refusal fails 5, and counting PENDING entries
into the bucket fails all three pre-existing PENDINGCAP subtests.

Docs DEFERRED, deliberately and tracked: `CONTRACTS*.md` and `AGENT_PROTOCOL.md` were owned by
concurrent agents this loop, so the documentation agent wrote only `internal/auth/doc.go` and drafted
the rest. Filed as AUTH-1-FU-ACTIVECAP-DOCS (27a811c9-5942-4341-b5fd-67c12a2547d0) with a proof that
globs `CONTRACTS*.md` (survives the split) and pins the literal string `a PROVEN identity, not an
attacker-supplied victim identifier` — CONFIRMED RED before filing (verdict=FAIL class=file-assertion
exit=1). The reviewer's P1 on the wire surface — `capacityRetryAfterSeconds = "5"` and "server at
capacity, retry later" are both wrong for a refusal that can persist an hour and is the client's own
fault — is `internal/httpapi`, outside the boundary: filed as AUTH-1-FU-ACTIVECAP-RETRYAFTER
(03a8512b-450c-4bce-a7b9-b024b98efbf0).

NOTE for whoever builds next: `go build ./...` is currently RED in `internal/wal` (undefined
`encodeFrame`/`parseFileHeader`, `scanFrom` arity). That is a CONCURRENT agent's in-flight work, not
this change — the reported line numbers shifted under me between two invocations. `internal/auth` does
not import `internal/wal` and builds, vets and gofmts clean on its own.

File-ownership boundary: `internal/auth/**` only. Staged exactly `internal/auth/{service.go,session.go,
session_test.go,doc.go}`. Did not touch CONTRACTS*.md, DECISIONS.md, SPEC.md, CLAUDE.md,
AGENT_PROTOCOL.md, internal/ids, internal/wal or internal/httpapi.

Left task `in_progress` (status_note CODE-COMPLETE/UNCOMMITTED) — feature-runner does not commit;
orchestrator commits and completes with `commit_sha`.

## 2026-08-02 — DEPLOY-1/DEPLOY-2: Dockerfile + docker-compose.yml (feature-runner, sonnet)

Task: containerise agent-bus (DEPLOY-1, multi-stage Dockerfile) and give it a single-bus Compose
deployment (DEPLOY-2, named volume + healthcheck), staying strictly loopback-bound until mTLS lands
(CLAUDE.md invariant 11). File-ownership boundary: `Dockerfile`, `.dockerignore`,
`docker-compose.yml` only — no Go source touched, per the wave brief.

**Dockerfile.** Two stages. Builder: `golang:1.19.4-alpine` pinned by digest
(`sha256:86d32cc0...` — matches this box's ambient `go version` 1.19.4, tracking `go.mod`'s `go 1.19`
until DEPLOY-4 bumps it), `CGO_ENABLED=0` static build with `-trimpath -ldflags "-s -w -X
main.version=${VERSION}"`. Dropped the originally-drafted BuildKit `--mount=type=cache` line: this
box's `docker` build has no `buildx` component (confirmed: `docker buildx` → "unknown command"), and
`--mount` fails outright on the legacy builder — plain layer caching (go.mod copied ahead of source)
is enough since the module has zero third-party deps to warm a cache for. Runtime: `alpine:3.19.1`
pinned by digest, fixed non-root UID/GID `10001:10001` (not `adduser`'s next-available default, for
stable volume ownership across rebuilds), `/data` created+chowned before `VOLUME ["/data"]` so a
fresh named volume inherits the right permissions, `EXPOSE 8080` (documentation only), a Dockerfile
`HEALTHCHECK` against `GET /healthz` via `wget` (busybox, no extra package), `ENTRYPOINT
["/usr/local/bin/agent-bus"]` with `CMD` wiring only EXISTING flags (`-listen`, `-data-dir`,
`-log-level`) — no container-specific config invented. Base-image choice (Alpine over
distroless/static — distroless has no shell/HTTP client to heathcheck with) recorded in DECISIONS.md.

**docker-compose.yml.** Single `agent-bus` service, `build:` from the Dockerfile, a named volume
`agent-bus-data:/data` (survives `compose down` without `-v`), a `healthcheck:` mirroring the
Dockerfile's own (belt-and-braces so `docker compose ps`/`depends_on: condition: service_healthy`
work without relying on image defaults alone). **Deliberately no `ports:` and `command:` repeats
`-listen=127.0.0.1:8080` explicitly** rather than omitting it — the security constraint from the
brief ("MUST NOT be exposed on a non-loopback interface until mutual TLS ships") is documented
in-file with a large comment block: since each container has its own network namespace, a
loopback-only bind means a published port would map to nothing anyway (nothing listens on the
container's external interface), so the honest, stated consequence is that no other container and no
host process can reach this bus as shipped. A commented, clearly-labelled opt-in override is given
(`-listen=0.0.0.0:8080` + a loopback-bound host-side `ports:` entry) for anyone who wants to accept
that risk locally; it is not the default. A "TLS SEAM" comment marks where the healthcheck and cert
material will need to move once mTLS lands, per the brief — not implemented here, only marked.
Rationale recorded in DECISIONS.md ("DEPLOY-1/DEPLOY-2" entry, 2026-08-02).

**Environment quirk, PATH-relevant for anyone re-running these proofs.** `docker` on PATH here is
the broken snap wrapper CLAUDE.md warns about ("cannot create user data directory"); the working CLI
is `/snap/docker/current/bin/docker`. Less obviously, `docker compose` is *also* unreachable through
that broken wrapper (no `cli-plugins` dir under `/snap/docker/current`), but the plugin binary itself
works fine invoked directly: `/snap/docker/3505/usr/libexec/docker/cli-plugins/docker-compose`, given
`DOCKER_HOST=unix:///var/run/docker.sock`. That let this task verify `docker compose up -d` /
`docker compose ps` / `docker compose down` **by real execution**, not just static YAML linting,
which the wave brief had flagged as probably unavailable — good news, recorded here so nobody
re-derives it.

**Verified by EXECUTION** (all test images/containers/volumes removed after, `docker images` /
`docker ps -a` / `docker volume ls` confirmed empty of `agent-bus*` afterward):
- `docker build` succeeds; `docker run --rm agent-bus:test -h` exits 0 and prints the real flag help
  (matches DEPLOY-1's stored `proof_cmd` verbatim, modulo the PATH workaround above).
- `docker run -d`: container starts as uid=10001(agentbus), `/data/*` files are `agentbus:agentbus`
  0600, `GET /healthz` from inside the container returns `{"status":"ok"}`, and the WAL/MAC-key/bus-id
  machinery (DUR-9/DUR-12) all wire up correctly inside the container with no changes needed.
- Docker's own `HEALTHCHECK` reaches `"Status":"healthy"` within two probe cycles.
- Named-volume durability, the load-bearing DEPLOY-1 requirement: created a named volume, ran the
  image, recorded the minted `bus-id`, `docker rm -f`'d the CONTAINER (never the volume), ran a fresh
  container against the same volume, confirmed the SAME `bus-id` — proves the data directory survives
  a container replace, not merely a `docker stop`/`start`.
- `docker compose up -d --build` → `docker compose ps` reports `Up ... (healthy)` → `wget` from inside
  the container answers `/healthz` → `docker compose down` (no `-v`) removes the container but leaves
  the named volume (`docker volume ls` still shows it) → `docker compose up -d` again reproduces the
  SAME `bus-id` → `docker compose down -v` for final cleanup.

**Verified only STATICALLY**, not by execution: nothing — everything above ran for real. The one thing
NOT exercised is a genuinely multi-bus/relay topology (DEPLOY-3, explicitly out of scope) and TLS
(not implemented anywhere yet).

**Proof-check verdicts, verbatim** (`bash scripts/proof-check.sh`, run with `PATH` prefixed by
`/snap/docker/current/bin` so the bare `docker` in each stored `proof_cmd` resolves):
- DEPLOY-1, stored `proof_cmd` (`docker build -t agent-bus:test . && docker run --rm agent-bus:test
  -h`) run verbatim: `proof-check: verdict=PASS class=build exit=0 tests_run=0 top_level=0 skipped=0
  failed=0 empty_pkgs=0`.
- DEPLOY-2: the ORIGINAL stored `proof_cmd` (`docker compose up -d && curl -sf localhost:8080/healthz
  && docker compose down`) is not runnable as literally written and, more importantly, is not
  *supposed* to succeed under the loopback-only design above — `curl` from the HOST can never reach a
  container that only binds its own loopback, by construction, not by bug. Replaced it (PATCHed via
  the Spec Server API, version bump recorded on the task) with a command that proves the same
  observable behaviour (compose brings the service up, it reports healthy, `/healthz` answers) without
  contradicting the security constraint the brief mandated. **The stored `proof_cmd` uses `docker
  compose exec -T`**, and that exact string — not the `docker exec` shorthand used earlier in this log
  entry for a quick manual check — is what was proof-checked and is quoted here verbatim: `docker
  compose up -d && sleep 8 && docker compose ps --format json | grep -q '"Health":"healthy"' && docker
  compose exec -T agent-bus wget -q -O - http://127.0.0.1:8080/healthz && docker compose down -v`.
  Verbatim verdict: `proof-check:
  verdict=PASS class=file-assertion,build exit=0 tests_run=0 top_level=0 skipped=0 failed=0
  empty_pkgs=0`.

**Chain**: implementer (this pass) → reviewer, security, documentation dispatched next (per the wave
brief, test-engineer skipped — justification: nothing here is Go code with unit-testable logic; the
"test" for a Dockerfile/compose file IS the execution proof above, which already ran).

File-ownership boundary respected: staged `Dockerfile`, `.dockerignore`, `docker-compose.yml` only.
Did not touch README.md (DEPLOY-2's description mentions a README section, but the wave brief scoped
ownership to the three files above and explicitly routed CONTRACTS-CLI.md to another agent this wave
— treating README the same way and handing the compose-usage write-up to the orchestrator to route,
per that same instruction). Did not touch any Go source, CONTRACTS*.md, or AGENT_PROTOCOL.md.

### 2026-08-02 — DEPLOY-1/DEPLOY-2 post-review fixes (feature-runner, opus review panel)

Dispatched reviewer + security in parallel (opus, read-only) against the three files above.
**Reviewer: DEPLOY-1 PASS, DEPLOY-2 CHANGES-REQUESTED. Security: CHANGES-REQUESTED (no
critical/high, no leaked secrets).** Fixed everything in scope; one item is out of my file-ownership
boundary and is reported as an open blocker rather than silently worked around.

**Fixed, all within `Dockerfile`/`docker-compose.yml`:**
1. **Security LOW, `Dockerfile` `/data` creation.** `mkdir -p /data && chown` alone left the directory
   at the ambient umask (0755, world-readable), diverging from the 0700 contract
   `cmd/agent-bus/main.go`'s `os.MkdirAll(cfg.DataDir, 0o700)` and `internal/wal` both assert for this
   exact directory. Added `&& chmod 0700 /data`. Re-verified by execution: `docker exec ... stat -c
   "%a %U:%G %n" /data` now reports `700 agentbus:agentbus /data`.
2. **Security MEDIUM, `docker-compose.yml` opt-in comment.** The documented `-listen=0.0.0.0:8080`
   escape hatch said binding the host-side `ports:` mapping to loopback was enough to contain the
   risk; it is not — bridge-network traffic between compose services never passes through the
   published-port mapping at all, so widening `-listen` makes the bus reachable by every OTHER service
   on the same compose network regardless of what `ports:` says. Rewrote the comment to say this
   plainly and to state there is no flag combination that fully restores the loopback-only guarantee
   once `-listen` is widened.
3. **Reviewer non-blocking, `Dockerfile` hardcoded `GOARCH=amd64`.** Silently produces a wrong-arch
   binary in an arm64 runtime stage on an arm64 host — wrong architecture, "exec format error" at
   container start, not a build-time failure. Dropped `GOOS=linux GOARCH=amd64` entirely; `go build`
   defaults to the platform it is running on, which already matches the runtime stage since both
   stages pull the platform Docker resolved.
4. **Reviewer non-blocking, `Dockerfile` stale Alpine base.** `alpine:3.19.1` was a ~2-year-old patch
   release (Jan 2024) carrying unpatched OS CVEs the security review flagged as a backlog item.
   Bumped to `alpine:3.22.1` (digest-pinned: `sha256:4bcff63911fcb4448bd4fdacec207030997caf25e9bea4045fa6c8c44de311d1`),
   the current stable series as of this pass; confirmed `wget`/`adduser`/`addgroup` are still present
   (busybox-provided in every Alpine release used here) before switching. go1.19.4 staying EOL is
   UNCHANGED and correctly out of scope — it is DEPLOY-4's job, and DEPLOY-1's own description says
   this task builds against whatever `go.mod` currently pins.
5. **Reviewer non-blocking, proof_cmd/AGENT_LOG.md mismatch.** The DEPLOY-2 write-up above quoted
   `docker exec agent-bus wget ...` in the narrative but the PATCHed `proof_cmd` actually stored on the
   task uses `docker compose exec -T agent-bus wget ...`. Corrected the narrative to quote the exact
   stored string (see the fix inline in the entry above) rather than changing what was already proven.

**Re-verified by execution after all fixes** (fresh `docker build`, fresh `docker run -d`, fresh
`docker compose up -d`/`ps`/`exec`/`down -v`; all test images/containers/volumes removed afterward,
confirmed empty via `docker images`/`docker ps -a`/`docker volume ls`): both proof_cmds re-run
VERBATIM as stored and both still PASS —
`proof-check: verdict=PASS class=build exit=0 tests_run=0 top_level=0 skipped=0 failed=0 empty_pkgs=0`
(DEPLOY-1) and
`proof-check: verdict=PASS class=file-assertion,build exit=0 tests_run=0 top_level=0 skipped=0 failed=0 empty_pkgs=0`
(DEPLOY-2, using the corrected `docker compose exec -T` form).

**NOT fixed — reported as an open blocker, not silently worked around.** The reviewer's remaining
finding is real and unaddressed: DEPLOY-2's own task description asks for "the compose invocation
… in a short README section," and README.md has zero mentions of Docker/Compose. This feature-runner's
file-ownership boundary for this wave is explicitly `Dockerfile`, `.dockerignore`, `docker-compose.yml`
and any `deploy/` directory only — README.md is not on that list, Go source and CONTRACTS-CLI.md were
explicitly named as owned by concurrent agents, and the standing instruction ("NEVER edit a file
outside it — other agents own the rest of the tree concurrently") is absolute. Reported to the
orchestrator for routing rather than silently expanding scope. Both tasks are left `in_progress`, not
`done`, until this is resolved one way or another (either the orchestrator grants a README edit, routes
it to whichever agent owns README this wave, or a follow-up task is filed).

Backlogged, not blockers, from security's review (unchanged by this pass, tracked for later waves):
go1.19.4 EOL (DEPLOY-4, already tracked in DECISIONS.md), no `security_opt`/`cap_drop`/`read_only`/
`pids_limit` hardening in `docker-compose.yml`, and DEPLOY-5's planned image-scanning step.

File-ownership boundary respected for this follow-up pass too: only `Dockerfile` and
`docker-compose.yml` edited; this `AGENT_LOG.md` entry and the accompanying git-add are the only other
touches.

## 2026-08-02 — DUR-12: CRC32C → HMAC-SHA256 keyed MAC (on-disk format version 2)

Ran by `feature-runner`. Chain ran IN FULL: spec-keeper → implementer → test-engineer → reviewer →
security → documentation. Nothing skipped, so no skip justification is owed.

**What shipped (code-only — nothing deployed).** The WAL frame's 4-byte CRC32C is now a 32-byte
HMAC-SHA256 tag over `frame[0:16] || payload` — the length field is INSIDE the covered range, which
is what closes the length-inflation class. On-disk format version 2 (RESERVED, not chosen). The key
is `<data-dir>/wal-mac.key`, 0600, 64 hex chars from `crypto/rand`, one per data directory. A v1 log
is still read with CRC32C, repaired in v1 if damaged, then converted ONCE to v2 preserving every
index, type and payload byte for byte — temp file, fsync, verify-by-rescan-and-SHA-256 BEFORE the
rename, best-effort hard-link backup `<log>.v1-<ns>`, rename, syncDir. No downgrade write.
`PROTOCOL.md` did not exist and was created for this; it now specifies the on-disk format.

**The sharp edge, and how it was resolved.** "Missing/wrong key is FATAL" (DECISIONS.md, "Five
decisions" §1) collides with "a pre-existing v1-only dir has no key file". Resolved by keying BOTH
fatal rules on one predicate: the log must POSITIVELY identify as version 2 (our magic, version field
2) and be longer than a v2 file header. A wrong key never damages the magic or the version field, so
the predicate never misses the case the decision is about; everything else generates a key. The
first attempt at this predicate was too broad — it refused any non-empty log that was not positively
v1, which broke two `cmd/agent-bus` tests that seed garbage into a keyless dir. The correction was
not to edit those tests: under a fresh key an unidentifiable log lands on the QUARANTINE branch,
which renames it aside without destroying a byte, so a refusal there protects nothing. Wrong key vs
damaged header is disambiguated by "does any record verify?" — a verifying record PROVES the key is
right, so header damage still rebuilds and starts.

**Accepted cost, recorded rather than buried:** a genuinely destroyed v2 log (right key, header
gone, no readable record) no longer self-quarantines and needs one manual `mv`. That is the price of
it being byte-indistinguishable from a wrong key.

**Deliberate non-deletion.** DUR-12 predicted the torn-tail heuristics would get SIMPLER under a
strong MAC and warned that a change which only adds has missed the opportunity. They did not get
simpler, and nothing was deleted to satisfy the prediction. Reviewer confirmed why: `inspectTail`,
`lengthOnlyDamage` and `truncatableTail` were ALREADY deleted by DUR-11; what remains answers "where
is the next record", which a MAC cannot answer in a format with no sync marker. The MAC makes those
checks stronger, not unnecessary — `rebuildFrame`'s length-repair check is now a proof against an
adversary, not only against accident.

**Gate verdicts.** Reviewer PASS-WITH-CONCERNS, no P0, golden bytes independently recomputed outside
the repo. Security PASS-WITH-CONCERNS, 0 CRITICAL, 1 HIGH filed as `DUR-12-FU-V1LAUNDER`: recovery
decides "this is v1" from the header alone, so a CRC32C-forged v1 file dropped at `bus.wal` is
re-framed and SIGNED WITH THE REAL KEY — forging without touching the key file. It grants no new
class of attacker (directory write already allows planting a key+log pair) but it destroys
forensics. The obvious fix — refuse v1 when a key exists — is UNSAFE as stated: it strands a
legitimate crash-mid-upgrade redo, which leaves exactly that state. Recorded in `PROTOCOL.md` §7 as
a known residual. Security also corrected the standing justification for the accepted limit: it is
NOT "an attacker with data-dir write access can READ the key" (replacing a file needs only `w+x` on
the directory) — it is that such an attacker can REPLACE the key and the log together. `PROTOCOL.md`
now says so; the same wrong sentence survives in `DECISIONS.md` (owned by another agent this pass)
and wants correcting there.

**Proof.** `scripts/proof-check.sh` on the four named tests: `verdict=PASS tests_run=19 top_level=4
failed=0` — it was `verdict=VACUOUS tests_run=0` before the change, since none of them existed.
Whole repo `go test -race ./...`: `verdict=PASS tests_run=757 failed=0`. Crash injection includes a
REAL SIGKILL landed inside `upgradeV1` (triggered on the appearance of `<log>.upgrade`, death
confirmed from the wait status, `Fatalf` if the kill never lands) plus four constructed post-crash
states, each labelled as constructed.

**A trap worth naming: the stored `proof_cmd` masks its own vacuity.** Its shape is `A && B`, and B
is the full suite, so the aggregate reads PASS even when A's `-run` pattern matches nothing. The
first clause must be run ALONE to mean anything. Same family as the `gofmt`-exits-127 false pass
already recorded in CLAUDE.md.

**COMMIT HYGIENE INCIDENT (filed as `COMMIT-HYGIENE-MIXED-22E8EB6`).** While this task was in
flight, a concurrent agent ran `git commit`, which commits the WHOLE INDEX. Commit `22e8eb6`
("Settle E2-E6 and E8...") therefore contains three DUR-12 files unrelated to its message —
`internal/wal/mackey.go` (new, 388 lines), `internal/wal/doc.go`, `internal/wal/recover_test.go` —
mixed with auth, ids and doc changes from at least three agents. This is precisely the failure the
2026-08-02 removal of the auto-commit hook was meant to end, reproduced by hand. Per DECISIONS.md
("Commit history: LEAVE IT") the history is NOT rewritten; this record is the remedy. Lesson: an
agent that stages its work is exposed to any other agent's bare `git commit`, so `git commit` should
always carry an explicit pathspec.

**Deferred doc debt:** the six `CONTRACTS-ONDISK.md` rows could not be written (that file was being
created by the CONTRACTS-split agent this pass). They are quoted verbatim in DUR-12's `kind=report`
note and carried by `DUR-12-FU-CONTRACTS`, whose proof was confirmed RED before filing. Seven other
follow-ups filed; `DUR-12-VERIFY` carries the not-yet-live deploy proof.
