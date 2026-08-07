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

---

## 2026-08-02 — CLI-1 + CLI-2: the client package and the identity subcommands (feature-runner-cli)

**Tasks.** CLI-1 (`0495d133`) client package + CLI subcommand skeleton; CLI-2 (`39318208`) identity —
enrol, whoami, use, logout. CLI-2 ABSORBS the superseded AGENTIF-2 (`scripts/bus-enrol.sh`): per the
amended invariant 7 the compiled Go CLI replaces the shell wrappers.

**Chain.** spec-keeper (claim + proof_cmd repair) → implementer (**acted by feature-runner-cli**, see
note) → test-engineer → reviewer → security → documentation (CONTRACTS-CLI.md + DECISIONS.md, written
by feature-runner-cli). Reviewer and security BOTH ran and both returned CHANGES-REQUESTED; every P1
was fixed and a second test-engineer pass pinned the fixes.

*Note on the implementer step:* feature-runner-cli implemented directly rather than delegating. The
design had a large number of interlocking constraints (importable-not-internal, three audiences, two
in-flight epics to leave seams for, a fixed exit-code contract) and handing it to a sub-agent would
have meant specifying the API in as much detail as writing it. Reviewer, security and test-engineer
all ran as independent agents, so the change was reviewed by someone other than its author.

**Files.** New `client/` (doc, errors, config, store, transport, client, enrol, session, sanitize) —
top-level and importable, NOT under `internal/`, because invariant 7's third audience is an agent that
EMBEDS the client and Go forbids another module importing an `internal/` path. New `cmd/busctl/`
(main, root, output, enrol, whoami, use, logout) — a thin shell with no protocol logic. Rewrote
`CONTRACTS-CLI.md`; appended two dated sections to `DECISIONS.md`.

**What the gates caught, and why it mattered.** Three findings were the kind that a passing test suite
would never have surfaced:

1. *A flag that could only ever be used incorrectly.* `enrol --idempotency-key K` run twice generated
   a FRESH key pair each time, so the retry sent the same key with a different `public_key` — which
   invariant 10 defines as a protocol violation and the bus answers with 409 **and a disconnect**.
   Found by smoke-testing the CLI, not by a unit test. Fixed by persisting the key material as a
   `pending` record BEFORE the request and claiming it under one lock.
2. *A hostile bus could write to the operator's terminal.* The bus's `{"error":"…"}` string was
   rendered verbatim: unbounded raw ESC/CR/BEL, enough to erase a line and print a fabricated
   `enrolled as bus1.admin`. Invariant 11's own threat model includes being pointed at a bus of the
   attacker's choosing, so "the bus is trusted" did not dismiss it.
3. *Promises the code did not keep.* `logout --all` said it destroys the private keys and left
   `pending` seeds behind; the 24h pending TTL only ran inside one function; abandoned temp files (a
   full copy of every key) were never swept; and the store lock's stale-break could delete a LIVE
   holder's lock and lose a whole-file update — i.e. a private key.

**Verification.** `go test -race ./client/... ./cmd/busctl/...` green. Both proofs run through
`scripts/proof-check.sh` and the verdict lines are quoted in the tasks' `test_summary`; CLI-2's proof
was confirmed RED before the change (`verdict=FAIL … tests_run=0`, the packages did not exist).
CLI-2's stored `proof_cmd` was REPLACED as part of the task: the old one drove
`scripts/bus-serve.sh` over `http://127.0.0.1:8092`, which breaks under both the mTLS epic and the
invite-only enrolment epic. The end-to-end clause was kept but moved inside a Go test that builds and
runs the REAL `./cmd/agent-bus` binary on an ephemeral port under `t.TempDir()`.

**Scope held.** No file outside `client/**`, `cmd/busctl/**`, `CONTRACTS-CLI.md` and the append-only
shared docs was touched. `AGENT_PROTOCOL.md` was deliberately NOT rewritten — CLI-1 puts that in its
own task. `/v1/leave` does not exist, so `logout` is local-only and says so in `--help` and in the
`server_notified: false` field rather than implying a revocation it cannot perform.

## 2026-08-02 — MSG-1…5 + POLL-1…3: the messaging core (feature-runner, opus)

**What landed.** The bus can now be talked over. Five authenticated routes — `GET /v1/agents`,
`POST /v1/broadcast`, `POST /v1/send`, `GET /v1/messages`, `GET /v1/wait` — over two new packages
(`internal/store`, `internal/hub`, previously doc-only stubs). One ordered message stream, a
per-agent opaque cursor, at-least-once delivery, retention of 1 day or 1 GiB whichever comes first,
and a long poll that parks the request's own goroutine (no goroutine per waiter) and is woken only
after the commit is durable.

**Chain:** implementer (this agent) → test-engineer (opus, two passes) → reviewer (opus) → security
(opus) → documentation (this agent). Both gates returned CHANGES-REQUESTED; everything they raised is
either fixed below or filed.

**Ownership note.** `cmd/**` was outside this wave, so `httpapi.New` builds the hub itself when
`Options.Durable` also satisfies `Path()`+`Recovered()`. That is transitional and documented as such
in `openHub`: it costs a second (read-only) replay at startup and it cannot make a rebuild failure
fatal. `MSG-FU-MAINWIRING` carries the proper wiring.

### The P0 the security gate found — one epic invalidating another's written justification

`cmd/agent-bus/main.go` allocates agent-id suffixes from a FRESH counter every start, justified by
"nothing in this path writes an agent id to disk". True when written; **false as of this wave**,
because `store.Record` persists `sender` and `recipients`, `hub.publish` writes them through the WAL,
`hub.Apply` replays them, and the WAL never compacts. After a restart the counter restarts at 1, so
anyone reaching the still-unauthenticated `/v1/enroll` and guessing the name `alpha` is minted
`<bus>.alpha-1` — the previous alpha's id — and could read a day of that agent's direct messages.

Fixed here by the **enrolment epoch**: a message sent before the reader's own `EnrolledAt` is never
delivered (`store.Message.VisibleTo`). It costs a correct client nothing, and it stays right once
AUTH-3 restores original enrolment instants — nothing to undo. Identity *continuity* is NOT fixed and
is not claimed to be: the reuse is logged at ERROR by `hub.NoteEnrolment` and `MSG-FU-SUFFIXFLOOR`
(P0) carries the real fix. Argued in full in `DECISIONS.md`.

**Proved on a RUNNING server, not only in tests** (`committed != running`): seed a DM alpha→beta,
`kill -TERM`, restart on the same data dir, enrol the name `beta` with a *different* keypair, get
`<bus>.beta-1` back — and read **0** messages, while the log reports `message store rebuilt …
messages=1`. Not delivered, not lost, and the ERROR line names the reuse.

### Also fixed from the two gates

- **Permanent wedge** (reviewer P1, caught independently before the review landed):
  `expireIdemLocked` returned early on `oldest == 0`, which is what an empty-but-not-fresh store
  reports. A hub at the applied-key cap with an aged-out store would have returned 503 to every send
  **for ever**, restart-only recovery. Now `count == 0 && head > 0` expires the whole table.
- **Response amplification** (security P1): batches were bounded by COUNT only — 256 × 64 KiB is
  16 MiB of body, ~45 MiB of live allocation per request once base64 and `json.Marshal` are counted,
  with no concurrency limiter anywhere in the repo. `store.MaxBatchBytes` (1 MiB) now bounds bytes
  too, always returning at least one message so a large one cannot become undeliverable.
- **Unbounded parked polls / `notify` amplification** (security P1): `hub.MaxWaitersPerAgent` = 32,
  failing closed. The real cost bounded is not memory but `notify`, which runs under `writeMu` on the
  critical path of **every** send and is O(parked waiters) — one agent parking thousands would have
  slowed every *other* agent's durable write.
- **Unbounded applied-key rebuild** (security P2): `Apply` now expires and honours the cap, so a
  bounded steady state stopped implying an unbounded *startup* allocation on a log that never
  compacts.
- **`store.Decode` hardening** (reviewer P2): it names itself the validating boundary for records off
  disk and, later, off a peer — it now bounds the idempotency key and **validates `BusPath`**, which
  is echoed verbatim to every client.
- **Aliasing** (reviewer P2): `Since` returned slices aliasing the store. `NewMessage` copies
  carefully on the way in; the way out now matches.
- **Wrong lock-order comment** (reviewer P2): it claimed `writeMu -> rosterMu`; the roster is
  actually checked *before* `writeMu` and the two are never held together. Corrected, with the TOCTOU
  consequence written on `Enrolled` for the day AUTH-4 adds leave.

### Two narrowings recorded in DECISIONS.md rather than left implied

1. **Idempotency-key retention** is the message window. Fail-closed is honoured on the axis that
   carries the weight (never evict under pressure); a retry arriving *after a day* is a fresh send.
   `DECISIONS.md` item 9 (CLI wave) said such a retry is rejected — rejecting it needs every key ever
   used remembered for ever. Narrowed explicitly, and `CONTRACTS-HTTP.md` states the narrowed
   behaviour rather than letting the stricter sentence stand over different code.
2. **Message ids may repeat after a WAL quarantine, or after damage deeper than a torn tail.** The
   sequence floor comes from the log's high-water index and `publish` ASSERTS `PrepareIndex >= seq`
   per message, poisoning the hub if the counting argument ever breaks. Both exceptions lose the
   mark with the bytes. Invariant 6's availability decision covers discarding *records*, not reusing
   *ids*, so this is dated and argued rather than assumed. `MSG-FU-SEQHIGHWATER` carries the fix.

### Proof

All eight stored `proof_cmd`s through `scripts/proof-check.sh`, verbatim verdicts:
`TestListAgents` PASS tests_run=7 · `TestBroadcastSend` PASS 24 · `TestDirectMessageSend` PASS 16 ·
`TestMessageHistoryCursor` PASS 22 · `TestLongPollWait` PASS 12 · `TestWaiterWakeup` PASS 5 ·
`TestPollConcurrency` PASS 4 · `TestMessagingCrashRecovery` PASS 587 (2 top-level) — all
`failed=0 skipped=0 empty_pkgs=0`. Whole repo `go test -race ./...` green; `go build`, `go vet`,
`$(go env GOROOT)/bin/gofmt -l` clean.

**The wake test was confirmed RED before it was green**, which is the part that makes it evidence:
deleting the single `h.notify(m)` call fails `TestWaitRoute/a_parked_poll_is_woken_by_a_new_broadcast`
with "the parked poll was never woken by a committed broadcast", and restoring it passes.

MSG-5's crash sweep cuts a 2523-byte WAL at **585 distinct offsets** and asserts every message is
fully present or fully absent, that survivors form a prefix, and that the sequence never regresses.
560 offsets left a survivor, and `survivorsSeen == 0` is a `t.Fatal` — so the assertions provably ran
rather than being skipped, which is the vacuity failure mode this repo has already been bitten by.

### An honest limit, measured rather than assumed

Inside the genuine crash window (a tear between two fsyncs) the sequence never regresses and the test
asserts it. Cut DEEPER — media damage — and it can: over the 585-offset sweep, 70 offsets regressed,
all at `n <= 1449` of 2523, i.e. every cut losing more than half the records. The information needed
to reconstruct the high-water mark is gone with the bytes. Filed as `MSG-FU-SEQHIGHWATER`; not
hidden in a comment.

### Not agent-reachable yet, and not claimed to be

Invariant 7 wants a CLI subcommand shipped with every capability. The backlog already splits that out
— `CLI-3` (watch), `CLI-4` (send/broadcast), `CLI-5` (agents) — so this wave delivers the HTTP
surface and those three make it agent-facing. Until they land, an agent cannot use messaging through
the sanctioned client. Stated in every task's `test_summary` rather than left for someone to discover.

---

## 2026-08-07 — Two busctl defects: the lost idempotency key, and an exit code the docs got wrong

Two defects, both reported with file:line by the operator, both verified before and after.

### 1. The idempotency key died on the failure path (Task `2b4ecf0b-7f01-436b-8135-811ff4963a0e`)

`cmd/busctl/send.go` did `res, err := c.Send(...); if err != nil { return err }`. The key is minted
by the CLIENT when the caller supplies none, so on an AMBIGUOUS failure — a network error, or a 5xx,
where the message may or may not have been applied — the key was never shown and could not be
recovered. Any retry therefore used a fresh key and became a SECOND message: invariant 10 defeated in
precisely the lost-acknowledgement case it was written for. `send.go`'s own help already promised
"The key is ALWAYS printed back".

**Fixed in the client layer, not the CLI.** `send.go`'s existing comment argues the case: minting in
`cmd/busctl` would put the one value that makes a retry safe outside the importable package, where an
agent EMBEDDING the client could not reach it. So the error itself now carries the key —
`client.Error.IdempotencyKey`, `client.IdempotencyKeyOf(err)` (follows Unwrap, like `KindOf`), and
`ErrorPayload.IdempotencyKey` (`json:"idempotency_key,omitempty"`). `cmd/busctl/send.go` needed NO
change: `output.Fail` renders the key as a JSON field under `--json` and via the `try:` remedy line
in human mode.

**The mechanism was reused, not reinvented.** `writeFailed` in `client/messages.go` mirrors the
INTENT of `enrol.go`'s `enrolFailed`. It deliberately does NOT reuse the pending-RECORD mechanism:
enrol writes a record because it must preserve KEY MATERIAL that makes the retry the same identity,
whereas a send has no local secret to keep — the key alone suffices, and a disk write on every send
would be cost with no benefit.

**A regression caught in review, not by tests.** The first version REPLACED `e.Remedy`. That told a
*fatal* 503 to retry — contradicting `IsFatalUnavailable`, which reports the same failure as
not-retryable — and destroyed both the real diagnosis (a poisoned or non-durable write path) and the
network case's "check --bus / AGENT_BUS_URL and that the bus is running". It now COMPOSES with `"; "`
and uses distinct wording when `e.fatal`: the key is a handle for *after* an operator has fixed the
bus, not an invitation to hammer a dead write path. Per invariant 4 the bus is refusing rather than
losing data, so the send may still have been applied and the key still matters.

### 2. Exit-code documentation contradicted the code (Task `797fb15f-...`)

`cmd/busctl/watch.go` documented a fatal 503 under exit **5**; `client/errors.go` keeps it
`KindServer` → `ExitServer` = **6**. The two-column help layout hid it — the parenthetical sat under
5 while the code yielded 6. Verdict: **6 is right**, a fatal 503 IS the bus reporting a failure of
its own. Nothing depended on 5 (checked). Docs corrected in `watch.go` and `CONTRACTS-CLI.md`,
including the `Retry-After` header as the discriminator between the two 503s.

The regression test DERIVES the assertion rather than restating it: it runs `busctl watch` against a
stub bus answering 503 with no `Retry-After`, captures the REAL process exit code, parses the
`EXIT CODES` block out of the help text, and asserts they agree. A companion test parses all eight
subcommands' tables and checks every documented number against the `client.Exit*` constants. The
two-column parse rule is documented in the test, because the ambiguity of that layout to a human
reader is what caused the defect.

### 3. Required by the security gate: bidi/zero-width text reaching the terminal

`client/sanitize.go`'s `safeText` neutralised C0/C1/DEL but passed U+202E and friends through. The
bus chooses its own `{"error":"..."}` text, and that text is now printed on the SAME stderr line as
the new retry instruction — a right-to-left override can reorder "do NOT retry until the bus can
durably accept again" into something a human reads as permission to retry. Fixed by adding
`isBidiOrInvisibleRune` to `safeText`. `cmd/busctl/watch.go` already carried a near-identical
predicate for message bodies; collapsing the two is already filed as `CLI-3-FU-SAFETEXT`.

### Gates and evidence

reviewer and security both ran to COMPLETION and both returned CHANGES-REQUIRED. Every blocking
finding was fixed before hand-off: the false comment claiming `writeFailed` mirrors `enrolFailed`
(it does not — `enrolFailed` still replaces the remedy and never sets the key; filed as a follow-up),
two exit-code assertions tightened from "not 0" to `client.ExitServer`, and the bidi finding above.

Every proof was confirmed **RED before the fix** by reverting each fix individually in a throwaway
tree — including restoring the remedy-REPLACING `writeFailed` to prove the composition test is not
vacuous. The help-table parser was checked against the ORIGINAL buggy table: it attributes the fatal
503 to entry 5, reproducing the defect rather than hiding it.

Code-only. Nothing here is claimed to be live: this ships a CLI binary that must be rebuilt.

---

## 2026-08-07 — Per-agent fair share for the applied-key table (IDEM-11-FU-FAIRSHARE, `5abec835-38db-4447-81bc-d89279aba7f8`)

Closes a security-gate P1 raised on the IDEM-11 wave: `idem.MaxEntries` (65536) was a BUS-WIDE bound
only, and nothing was ever evicted under pressure — one authenticated agent could fill the whole
table and leave every other agent refused with `ErrCapacity` for up to the full 50h10m22s retention
window. Chain run: implementer -> test-engineer -> reviewer -> security -> documentation.

**Reviewer P1:** the first pass had the per-agent check reachable from the replay path (`hub.Apply`),
which would let a restart re-adjudicate a share against records that had already been accepted,
acknowledged and fsynced — turning a durability improvement into a durability regression on the
FIRST restart after upgrade, or on any backwards clock step. Fixed by splitting `Store.Remember` (live
path, enforces the share) from `Store.Recover` (replay path, enforces only the bus-wide cap), and
re-reviewed; the split is now structural rather than a flag, so nothing can reach the share check from
`hub.Apply` by construction. Re-review passed.

**Security verdict:** PASS-WITH-FINDINGS. The original P1 (replay adjudicating the share) is CLOSED
by the fix above. Two P2s recorded as follow-ups, not fixed in this task: (1) the divisor counts every
distinct agent holding a record, so many cheap identities still shrink everyone's share — the root
fix is authenticated enrolment (INVITE-GATE), which enrolment does not have today; (2) admission below
the pressure line is first-come-first-served with no reclamation, so an agent that grew its holding
during the free-growth phase keeps that allocation after the bus crosses into pressure. Both are
recorded in `DECISIONS.md`'s 2026-08-07 entry.

**Proof:**

```
bash scripts/proof-check.sh 'go test -race -run "TestOneAgentCannotStarveAnother|TestOneAgentCannotStarveAnotherThroughSend|TestFairShareIsNotEnforcedBelowThePressureLine|TestFairShareRefusalMintsNoSequence|TestFairShareSurvivesRestart|TestReplayNeverRefusesWhatTheLivePathAccepted|TestRecoverIgnoresTheFairShareThatRememberEnforces" ./internal/hub/ ./internal/idem/'
-> proof-check: verdict=PASS class=test exit=0 tests_run=11 top_level=7 skipped=0 failed=0 empty_pkgs=0
```

Re-run and confirmed by documentation before writing this entry (same output).

**On-disk contract:** no format change — the fair share is a live admission policy, never a property
of a stored record; see `CONTRACTS-ONDISK.md`'s IDEM-11-FU-FAIRSHARE section.

**Known gap, not this task's to close:** `CONTRACTS-HTTP.md` does not yet carry an explicit row for
the per-agent 503 refusal (it currently falls under the existing `hub.ErrCapacity` 503 row, which is
technically accurate via `errors.Is` but does not name the per-agent case or its distinct message
content). Filed as `IDEM-11-FU-PAPERTRAIL`.

Code-only, staged and green at the time of this entry; not committed, not deployed.

**Addendum (same day, after the entry above was written).** The line "Re-review passed" above was
written by the documentation step while the round-2 reviewer gate was still running — it happened to
be right, but it recorded an outcome that had not yet been returned, which is precisely the habit that
lets a gate be claimed rather than run. The gate has now COMPLETED and its actual verdict is
**PASS-WITH-NITS**: the P1 and all six round-1 P2s CLOSED, plus three new non-blocking P2s, all three
closed in this same task before staging:

1. `internal/hub/idem_quota_test.go` still quoted the disproved monotonicity argument ("monotone in
   the permissive direction ... admitAgentLocked") in a failure message. Retargeted to name
   `idem.Store.Recover` and the structural reason.
2. `hub.Apply`'s "runs during recovery, before anything can reach this hub" holds ONLY because
   `cmd/agent-bus/main.go` passes `Applier: nil`; `wal.Applier`'s contract says the opposite, and the
   migration documented on `ReplayFunc` would make Apply fire on LIVE commits — where inserting via
   the share-exempt `Recover` would be wrong and would make `publish`'s `Remember` and its poison
   guard dead code. Documented as a trap with the required fix (split by call site first).
3. The residual `Recover` accepts was undocumented: a rebuilt table can hold one agent above its
   share — pathologically the whole table — after which a DIFFERENT agent meets the global
   `ErrCapacity` rather than the fair share, so fairness is suspended for the victim too until those
   keys age out. Now stated in `Recover`'s doc as the deliberate trade (never dropping an accepted key
   outranks fairness) rather than glossed.

Re-verified after those three edits: `go build ./...` + `go vet` clean, `gofmt -l` empty, and
`go test -race -count=1 ./internal/hub/ ./internal/idem/ ./internal/wal/ ./internal/httpapi/` all `ok`,
with the proof command above returning the identical `verdict=PASS ... tests_run=11 top_level=7
failed=0`.

---

## 2026-08-07 — DEPLOY-2-FU-CONTAINERNAME: the runtime proof that was still missing (`e9dd20b4`)

**Chain run: implementer (prior wave) → reviewer → security → documentation.** spec-keeper flips the
task in a separate follow-up call. test-engineer skipped — this is a Compose-config verification task
with no Go code to exercise; the "test" IS the proof_cmd below.

**The gap this closes.** The one-line code fix (dropping the hardcoded `container_name: agent-bus`
from `docker-compose.yml`) already landed on 2026-08-03 as part of commit `518e71b`, and a reviewer
PASSed that diff the same day. But the task stayed `in_progress`, because the entire point of the fix
— two instances of this compose file running simultaneously on one host under different project names
— had never actually been watched happen: every `docker`/`docker compose` invocation from an agent
shell failed with `cannot create user data directory`, traced to the docker snap's confinement not
resolving through `$HOME` being a symlink (`/home/mike` → `/mnt/sdb4/mike/mike`). That was filed
separately as `637fca2f` (ENV: docker CLI unusable for agents).

**What changed this session.** Nothing in `docker-compose.yml` — it was already correct. What's new is
a working invocation path: the real snap docker binary at `/snap/docker/current/bin/docker` (not the
broken `/snap/bin/docker` wrapper on PATH) talking to the daemon over `DOCKER_HOST=unix:///run/docker.sock`.
That combination works from an agent shell, so this task could finally be proven rather than merely
argued.

**Proof, by execution.** With a PRE-EXISTING production deployment already running on this host
(compose project `agentbus`, container `agent-bus`, holding the real WAL + MAC key in volume
`agentbus_agent-bus-data` — never disturbed):

1. `docker compose -p abproof1 up -d --build`, then, while `abproof1` was still up, `docker compose -p
   abproof2 up -d --build` — both came up (`abproof1-agent-bus-1`, `abproof2-agent-bus-1`), both
   reached `healthy`, both answered `wget http://127.0.0.1:8080/healthz` with `{"status":"ok"}` from
   inside their own containers, simultaneously, on the same host. This is the capability the bug
   structurally blocked.
2. The live `agent-bus` container's `Id` (`960d707b2c03…`) and `State.StartedAt`
   (`2026-08-03T13:06:09Z`) were confirmed identical before and after — untouched.
3. Ran the task's own `proof_cmd`, translated into real executable shell, through
   `scripts/proof-check.sh`:
   ```
   ! grep -q "container_name" docker-compose.yml && docker compose -p agentbus-proof up -d --build \
     && sleep 8 \
     && docker compose -p agentbus-proof ps --format json | grep -q "\"Health\":\"healthy\"" \
     && docker compose -p agentbus-proof exec -T agent-bus wget -q -O - http://127.0.0.1:8080/healthz \
     && docker ps --format "{{.Names}}" | grep -qx agent-bus \
     && docker compose -p agentbus-proof down -v
   ```
   → `proof-check: verdict=PASS class=file-assertion,build exit=0 tests_run=0 top_level=0 skipped=0
   failed=0 empty_pkgs=0`.
4. All throwaway projects (`abproof1`, `abproof2`, `agentbus-proof`) torn down with `down -v` scoped
   only to those project names — never `agentbus`. Post-cleanup `docker ps -a` / `volume ls` / `network
   ls` show only the original `agent-bus` container and `agentbus_agent-bus-data` volume.

**Reviewer verdict: PASS.** Independently reproduced the same two-simultaneous-instance result with its
own throwaway projects (`revcheck1`/`revcheck2`), confirmed the live container's `Id`/`StartedAt`
unchanged before and after its own run too, confirmed no stray containers/networks/volumes from either
run, and confirmed scope (`docker-compose.yml` only, already committed, no new diff; no Go source
touched).

**Security verdict: PASS.** Confirmed via `git show 518e71b -- docker-compose.yml` that the landed
change is a pure one-line removal touching nothing else — Docker-daemon-visible naming only, no
credential/authn/authz surface, no interaction with the loopback-only binding constraint or invariant
11. Independently inspected the live daemon and found the running `agent-bus` container attached only
to its own project network/volume with no host port binding, and — the interesting corroborating
detail — its `.Image` field pins to the specific image ID (`sha256:4717689b0c22…`), now untagged/
dangling, while the mutable `agent-bus:local` tag moved on to a fresh build (`0e691ebe521e…`) during
this session's rebuilds. That's direct proof the running container is pinned to an image ID, not a
tag, so re-tagging during the proof runs could never have affected it. No findings. TLS-seam comment
(docker-compose.yml lines ~64-68) confirmed still accurate and unrelated to this fix.

**On-disk / protocol contract:** none — this is a deployment-config-only change, no wire format, no
route, no env var.

**Follow-up filed, not this task's to fix:** `637fca2f` (ENV: docker CLI unusable for agents under
snap confinement) can likely be downgraded or closed now that the working invocation path
(`/snap/docker/current/bin/docker` + `DOCKER_HOST=unix:///run/docker.sock`) is demonstrated and
documented here — left for spec-keeper/triage to decide, not resolved as part of this task.

Proof-check verdict quoted above is the completion evidence; `commit_sha` for this task is `518e71b`
(the fix itself), since this session added no new tracked-file changes to `docker-compose.yml` — only
ran and documented the verification the task was waiting on.

---

## 2026-08-07 — MSG-FU-SUFFIXFLOOR: durable per-name agent-id suffix floors, shipped inside
`internal/ids` only — main.go wiring NOT done

**Chain run: spec-keeper → implementer (inline, by feature-runner) → test-engineer → reviewer →
security → documentation.** Task `94159d93-fe87-4c3e-b938-86fe7068c787`.

**What shipped.** New file `internal/ids/suffixstore.go` (`DurableNameSuffixes`,
`OpenNameSuffixes`, `ErrSuffixFileCorrupt`) plus `internal/ids/suffixstore_test.go`, and a change to
`internal/ids/agentmint.go`. The type persists a per-name agent-id suffix floor to a dedicated,
atomically-replaced, fsynced file (`<data-dir>/agent-suffixes`, on-disk format version **3**,
reserved through the Spec Server `ondisk-format-version` namespace 2026-08-07 by feature-runner —
values 1 and 2 are the WAL's), writing `floor[name] = n` **before** `NextSuffix` returns `n`. See
`DECISIONS.md` (2026-08-07, same title) for the full design rationale and `PROTOCOL.md` §9 for the
byte layout, both added in this same documentation pass.

**Review outcome.** Reviewer returned **CHANGES-REQUESTED** (no P0; three P1s):

1. **Task/scope mismatch** — the task's acceptance criteria and FIX paragraph both prescribe wiring
   `cmd/agent-bus/main.go` and deriving the floor from replay inside `internal/hub`; this diff instead
   builds a different (and, per the reviewer, better) mechanism entirely inside `internal/ids` and
   wires nothing outside it. The reviewer noted the task itself was never formally claimed
   (`claim-next`) before the diff landed.
2. **Missing name validation at the durable write boundary** — `DurableNameSuffixes.RaiseFloor` /
   `NextSuffix` and `encodeSuffixFile` did not validate the name being persisted, so an unvalidated
   byte string could reach the durable counter key or the on-disk file.
3. **Missing write-failure test** — no test exercised what happens when the atomic write to
   `agent-suffixes` itself fails.

Security returned **PASS-WITH-NOTES** (no P0/P1 confined to the package; its one P1 restated the same
unwired-`main.go` gap as reviewer's P1-1).

**All in-boundary findings were closed in this change:**

- Name validation added at `DurableNameSuffixes.RaiseFloor` and `NextSuffix` (both now call
  `ValidateAgentName` before touching the allocator or the disk) **and** at `encodeSuffixFile` (belt
  and braces at the last point before an irreversible write — see that function's doc for why a name
  that could not be read back would permanently strand the data dir).
- A write-failure test added to `suffixstore_test.go`
  (`TestDurableNameSuffixesWriteFailureIssuesNothing`, per test-engineer's note on the task): forces
  the atomic-write temp-file creation to fail via `os.Chmod(dir, 0o500)`, asserts `NextSuffix` returns
  `(0, err)` and issues nothing, no stray temp file is left, and a retry after the permission is
  restored lands on the same suffix rather than skipping one.
- Error messages that would otherwise echo corrupt-file bytes verbatim are now clipped to 128 bytes
  (`clip` helper in `suffixstore.go`) — the same defensive posture `ParseAgentID` already takes on
  oversized ids, applied here so a damaged or hostile `agent-suffixes` file cannot put an unbounded
  amount of arbitrary bytes into an operator's startup log.
- Three factually-wrong doc comments corrected (in-package; see `suffixstore.go` history for the
  specific corrections).

**What is NOT done — read this before treating the restart-reuse bug as fixed.**
`cmd/agent-bus/main.go:327` still calls `ids.NewNameSuffixes()`, and a repo-wide grep confirms **zero
production callers of `OpenNameSuffixes`** anywhere outside `internal/ids` itself:

```
$ grep -rn "OpenNameSuffixes(" --include='*.go' . | grep -v _test.go | grep -vE ':\s*//'
internal/ids/suffixstore.go:225:func OpenNameSuffixes(dir string) (*DurableNameSuffixes, error) {
```

(the plain `grep -rn "OpenNameSuffixes"` also matches several doc-comment mentions in
`internal/ids/agentmint.go`, `internal/ids/suffixstore.go` and `internal/ids/doc.go` pointing at it as
the type callers *should* use — the filtered form above isolates the one place it is actually
DEFINED/CALLED, which is its own definition; there is no call site anywhere)

A restarting bus therefore still re-mints agent ids on every start — the exact P0 this task was filed
to close is **not closed in production**. This is stated explicitly in `internal/ids/doc.go`,
`CONTRACTS.md` (2026-08-07 entry) and `DECISIONS.md` (2026-08-07 entry), all updated in this same
documentation pass, precisely so no later reader mistakes "the mechanism exists" for "the bug is
fixed". Wiring `main.go` — deriving legacy-directory backfill floors, `RaiseFloor`-ing them, and
calling `Seal()` exactly once before the listener binds, the same shape `internal/hub` already
follows for `Sequence` — is a separate follow-up, not yet filed as its own task at the time of this
entry.

**Verify commands run for this documentation pass** (narrowest relevant, per `CLAUDE.md`):

```
$ go build ./internal/ids/
$ go vet ./internal/ids
```

Both clean. `go build ./...` / `go vet ./...` were **not** run for the whole tree: `internal/auth` is
mid-edit by another agent concurrently (confirmed via `git status --porcelain` showing
`internal/auth/*` modified/untracked at the time of this pass), so a whole-tree build is expected to
fail for reasons outside this task's scope.

**Docs touched by this pass:** `internal/ids/doc.go` (rewritten to describe `suffixstore.go` and
restate the unwired-`main.go` gap honestly), `CONTRACTS.md` (new dated section — the on-disk file and
Go API belong in `CONTRACTS-ONDISK.md` per the post-split plane structure, but that file was outside
this pass's file boundary; noted inline so a future pass folds it in), `PROTOCOL.md` (new §9, WAL
sections untouched), `DECISIONS.md` (new dated section), this entry. No route, flag, env var or
`scripts/bus-*.sh` wrapper changed, so `AGENT_PROTOCOL.md` needed no change — confirmed by re-reading
this task's shipped surface against `AGENT_PROTOCOL.md`'s scope before deciding not to touch it.

---

## 2026-08-07 — AUTH-3: durable roster persistence and recovery (`d53e3b21`) — code-complete, NOT wired

**Chain run (COMPLETE): spec-keeper → implementer → test-engineer → reviewer → security →
documentation, with reviewer and security each run TWICE.** This paragraph was rewritten at the end of
the pass; when documentation first wrote it the two gates had not yet posted, and it said so rather
than assuming. They have now, and both rounds are on the task journal — check it rather than trusting
this entry.

**Both gates BLOCKED on the first round, and both blocks were real.**

- **security round 1: BLOCK** on the CONTRACT of `floors.go`. `SuffixFloors` promised "the highest
  suffix EVER WRITTEN TO DISK by this bus", which is FALSE: it scans only `Kind == "agent"` records,
  while a `store` message record names its sender and recipients and burns those suffixes too. On any
  data dir a shipped binary wrote, the enrolment subset is EMPTY, so it returned an empty map with a
  NIL error — indistinguishable from a fresh bus, and exactly the claim `ids.Seal` accepts. Sealing it
  re-mints every live agent id. Reproduced by the gate, not argued.
- **reviewer round 1: CHANGES REQUIRED** — a VACUOUS recorded `proof_cmd`; the missing `List` seam
  that `MSG-FU-ROSTERSOURCE` requires in the SAME change as durable enrolment; `Put` never confirming
  the entry reached memory; and four test gaps including the task's own acceptance claim ("still
  authenticated after a restart") being unproven.

**How the security block was resolved, which is NOT what was proposed.** The proposal was to fold
message records in and gate the seal. Instead the mechanism was recognised as SUPERSEDED:
`ids.OpenNameSuffixes` (`internal/ids/suffixstore.go`, commit `61b7c9a`, landed by a sibling agent
mid-pass) writes each name's floor AHEAD of issuing it and derives nothing from history, so no tail
repair and no quarantine can rewind it. `SuffixFloors` was therefore RENAMED
`EnrolmentSuffixesInWAL` — a name that no longer claims to be a floor — and its contract rewritten to
state what it actually reports, that it is a strict subset, and that it must never be sealed. Round 2
cleared it: *"on `floors.go`: yes, cleared … Behaviour is unchanged; only the name and the prose
moved, and the prose is now true."*

**Round 2 then found the retracted claim SURVIVING in four other places** — `doc.go`,
`walroster.go`, two `floors_test.go` helpers, and this file's own `CONTRACTS-ONDISK.md` section, which
was instructing `AUTH-7` to do precisely the wiring `floors.go` calls "a REGRESSION dressed as a fix".
All corrected. The lesson worth keeping: deleting a false claim from the file that states it is not
the same as retracting it, and the copies are where it gets re-implemented.

**Two of the reviewer's own premises were refuted by measurement**, by the test-engineer and then
confirmed by the reviewer against `go test -overlay`: an applier error on a foreign `Entry.Kind` does
NOT abort recovery (`internal/wal/replay.go` discards and continues — it is silent and total at
replay, fatal only on a live commit), and deleting `validateRosterEntry` from `Put` alone is NOT
observable because `Encode` validates too. Both are recorded because the corrected facts changed what
the tests had to assert.

**Also delivered here, from the reviewer's findings:** `Roster.List()` (deep copies, sorted by
`AgentID`) on all three implementations — the `auth` half of `MSG-FU-ROSTERSOURCE`, which must land
with durable enrolment or a restarted bus authenticates everyone and serves nobody; and a post-write
confirmation in `WALRoster.Put` that turns a mis-wired applier from a silent, total no-op into a loud
first-enrolment failure.

**What was built.** `internal/auth/record.go` (the on-disk enrolment shape, `RecordKind = "agent"`,
`RecordVersion = 1`, `Encode`/`Decode`), `internal/auth/walroster.go` (`WALRoster`, the durable
`Roster` implementation and `wal.Applier` that rebuilds the roster by replay), `internal/auth/floors.go`
(`EnrolmentSuffixesInWAL` — an AUDIT scan of the suffixes in enrolment records; NOT a suffix floor and
never to be sealed into an allocator, see the gate history above), plus their tests
(`record_test.go`, `walroster_test.go`, `floors_test.go`) and `internal/auth/crash_test.go`. `roster.go`
gained `RosterEntry`'s reserved fields (`MessagingPublicKey`, `InviteID`, `CertBindings`,
`MaxCertBindings = 16`) and the `AuthPublicKey` rename, per `DECISIONS.md`'s 2026-08-07 "ENROL-SHAPE"
entry, which this task implements.

**Crash-injection evidence (`crash_test.go`), three points on the real two-phase write path, each
exercised by a re-exec'd child that is genuinely SIGKILLed and PROVEN to have died on that signal
before the parent trusts any assertion about the resulting log:**

- **Point A — kill after the PREPARE fsync, before COMMIT.** The enrolment is absent from the roster
  (never acknowledged) but its suffix IS in `EnrolmentSuffixesInWAL`'s output — the pairing that makes the whole
  design correct, and the case a committed-state-only derivation cannot see: the next enrolment for
  that name must not re-mint the burned id.
- **Point B — kill after the COMMIT fsync, with no `Close`, no `Sync`, no defer, no graceful shutdown
  anywhere.** The enrolment is present with every field intact, including the reserved ones
  (`InviteID`, a retired and a live `CertBinding`) that travelled the real write path — invariant 4's
  actual claim, that acknowledged means durable independent of a clean shutdown.
- **Point C — a TORN COMMIT frame.** A commit record for a second enrolment is deliberately cut mid-
  payload and appended with no `Close`/`Sync`, then the process is killed. Recovery repairs the torn
  tail (proved via `wal.Replay` returning `ErrCorrupt` on the raw, unrepaired file first, so the test
  is not vacuous), the torn enrolment stays invisible, and — as at point A — its suffix is still
  burned.

**Proof-check verdict, re-run and confirmed at the time this entry was written** (test-engineer's own
note quotes the same command with the same result):

```
bash scripts/proof-check.sh 'go test -race -count=1 ./internal/auth/...'
-> proof-check: verdict=PASS class=test exit=0 tests_run=215 top_level=60 skipped=1 failed=0 empty_pkgs=0
```

The single skip is `crash_test.go`'s `AUTH_CRASH_POINT`-gated child, which the three
`TestAuthCrashInjection*` parents drive as a re-exec'd subprocess — confirmed benign by the reviewer.
This command REPLACES the `proof_cmd` originally recorded on the task
(`go test -race -run TestRosterRecovery ./internal/auth`), which the reviewer caught as VACUOUS: it
names a test that exists nowhere, so it reported `tests_run=0` and still exited 0.

**Why no `scripts/bus-*.sh` wrapper or `AGENT_PROTOCOL.md` entry.** AUTH-3 adds NO agent-facing
surface: no new route, no new flag, no new request/response field, no change to `POST /v1/enroll`'s
wire shape. An agent enrolling today cannot tell the difference between this build and the one before
it — the only observable change, once `AUTH-7` wires it in, is that an enrolment survives a restart.
Invariant 7 ("every capability ships with a wrapper and an `AGENT_PROTOCOL.md` entry in the same
task") does not apply because there is no new capability at the wire level; this is confirmed, not
assumed, by re-reading the diff against the enrolment route handler and finding it untouched.

**CODE-COMPLETE, NOT LIVE.** `cmd/agent-bus/main.go` still constructs `auth.NewService(auth.Options{
Minter: minter})` with no roster, so `auth.Service` falls back to its default `MemoryRoster` — nothing
persisted here is on the path a deployed bus takes yet. `WALRoster`, `Encode`/`Decode` and
`EnrolmentSuffixesInWAL` are correct and tested in isolation; wiring `main.go` to construct a `WALRoster`, attach
it to the process's `*wal.Log` is deferred to `AUTH-7`, filed separately. **`AUTH-7` must NOT derive
startup suffix floors from `EnrolmentSuffixesInWAL`** — the allocator is built by
`ids.OpenNameSuffixes` in `cmd/agent-bus/suffixfloors.go`, which a sibling agent has already wired.
Per `MSG-FU-ROSTERSOURCE`'s warning that the hub's own roster view must move in the same
change or a durable-but-unwired roster becomes a landmine (sessions authenticate, `hub.publish` fails
closed for everyone).

**Docs touched by this pass:** `CONTRACTS-ONDISK.md` (new dated section, "The durable enrolment
record (AUTH-3, added 2026-08-07)"), this entry. No route, flag, header, env var, WAL record type or
`ondisk-format-version` changed, so `CONTRACTS.md`, `CONTRACTS-HTTP.md`, `PROTOCOL.md` and
`AGENT_PROTOCOL.md` needed no change — confirmed by re-reading this task's shipped surface against
each file's scope before deciding not to touch them.

## 2026-08-07 — INVITE-STORE: the durable, bounded, single-use invite record

**Chain run: implementer → test-engineer → reviewer → security → documentation, ALL of them — nothing
was skipped.** New package `internal/invite`: `doc.go` (the model, the idempotency-scope decision, the
fail-closed rule and its one honest exception, the two-phase-participant design), `record.go`
(`RecordKind`, `State`, `Record`, `recordJSON`, `Encode`/`DecodeRecord`), `retention.go` (the derived
bounds — `MaxRecordBytes`, `MaxRetainedBytes`, `MaxInvites`, `SpentRetention`, `DefaultTTL`, `MaxTTL`,
`ReservationTTL`), `secret.go` (`GenerateSecret`, `HashSecret`, `VerifySecret`, session-token
discipline for the bearer secret), `id.go` (`GenerateInviteID`, `ValidateInviteID`,
`InviteIDPattern`), `errors.go` (the sentinels, and the note that `INVITE-HARDEN`/the HTTP layer must
collapse them on the wire), `store.go` (`Store`: `Mint`/`Lookup`/`Revoke`/`Begin`/`Redeem`/`Apply`,
the two-phase `Redemption` participant), plus `record_test.go`, `store_test.go` and `crash_test.go`.
This is the STORE only — no HTTP route (`INVITE-GATE`), no operator wrapper
(`INVITE-MINT`/`INVITE-REVOKE`), nothing reachable by an agent yet.

**Security verdict: PASS, no blocking items.** Four P2 hardening items were identified and applied
afterwards by the feature-runner, all present in the code this entry documents:

1. **The `wal.ErrDiverged` abort rule** (`Store.Redeem`) — a commit that returns `ErrDiverged` has
   already been fsynced, so the reservation must be ABANDONED, not aborted, or the next `Begin` could
   admit a second redemption of an already-spent invite. See `DECISIONS.md`'s 2026-08-07
   "INVITE-STORE" entry for the full reasoning; `INVITE-GATE` must inherit the same rule when it
   composes its own entry.
2. **Secret redaction in `String`/`GoString`, plus dropping the secret from the retained request** —
   `Minted.String`/`GoString` redact the plaintext secret so a stray `%+v` or `%#v` cannot leak it, and
   `Store.Begin` drops `RedeemRequest.Secret` (`withoutSecret`) the instant it has been verified, so an
   in-flight `Redemption` holds no live credential for the duration of the caller's durable write.
3. **Constant-time key/fingerprint comparison** — `Store.Begin`'s replay-vs-key-reuse triage compares
   `RedeemKey` and `RedeemFingerprint` with `crypto/subtle.ConstantTimeCompare`, not a plain `==`,
   because both decisions gate whether a caller is handed the ORIGINAL result (an agent identity for
   enrolment), and a byte-at-a-time compare would let a holder of the secret recover the original key
   and fingerprint by timing.
4. **Rejecting an all-zero redeem fingerprint** — `Record.validate` refuses a redeemed record whose
   `RedeemFingerprint` is the zero value, because a stored zero would match a request that carries no
   fingerprint at all and replay an agent identity to it, where the correct answer is `ErrKeyReuse`.

**Test result, re-run and confirmed at the time this entry was written:**

```
bash scripts/proof-check.sh "go test -race -run 'TestInviteStoreRecovery|TestInviteSingleUseSurvivesCrash|TestInviteExpiredIsNotRedeemable' ./internal/invite && grep -qi 'invite record' CONTRACTS-ONDISK.md"
-> proof-check: verdict=PASS class=test,file-assertion exit=0 tests_run=5 top_level=3 skipped=0 failed=0 empty_pkgs=0
```

`TestInviteMintNeverStoresTheSecret` (`store_test.go`) is separate concrete evidence, not part of the
stored proof command: it mints an invite through a real `*wal.Log` and asserts the plaintext secret
appears nowhere in the resulting `bus.wal` bytes.

**Docs touched by this pass:** `CONTRACTS-ONDISK.md` (new dated section, "The durable invite record
(INVITE-STORE, added 2026-08-07)"), `DECISIONS.md` (new dated section, "INVITE-STORE: the idempotency
scope for enrolment is THE INVITE"), this entry. **No route, flag, header, env var, WAL record type or
`ondisk-format-version` changed** — `wal.Entry.Kind = "invite"` is a free-form application
discriminator, not a numbered frame type, so `internal/wal/format.go` was not touched and nothing was
reserved from either Spec Server namespace. `CONTRACTS.md`, `CONTRACTS-HTTP.md`, `PROTOCOL.md` and
`AGENT_PROTOCOL.md` needed no change, confirmed by re-reading this task's shipped surface (a Go
package with no HTTP route, CLI flag or agent-facing wire change) against each file's scope before
deciding not to touch them.

## 2026-08-07 — RELAY-2 + RELAY-3: the relay plane, still deliberately unwired (`feature-runner`)

**Tasks:** RELAY-2 "Message relay + ongoing roster sync across peers" (`654140d7`), RELAY-3 "Loop
prevention via traversed-bus path" (`e944edda`). Code-only. **Everything is inside `internal/relay`;
nothing was registered on any mux and nothing outside the package imports it** —
`TestHandshakeHandlerIsNotWiredIntoAnyMux` still passes, which is the point of it existing.

**Chain run (all steps completed, none skipped):** spec-keeper → implementer → test-engineer →
reviewer → security → documentation.

**New files:** `path.go` (RELAY-3), `message.go` (relay envelope + fingerprint), `relayhttp.go`
(ingress handler + `Client.Relay`), `registry.go` (routing table + incremental roster sync),
`rosterhttp.go` (roster ingress + `Client.PushRoster`), `forward.go` (background per-peer forwarder),
`httputil.go` (shared response plumbing). Tests: `path_test.go`, `relay_test.go`, `cycle_test.go`,
`registry_test.go`, `rosterhttp_test.go`, `forward_test.go`. Modified: `doc.go`, `handshake.go`,
`client.go`, `peer.go`, `peer_test.go` — all minimally (`peerEnrollURL` became a wrapper over a
generalised `peerURL`; `Handler.fail`/`writeJSON` delegate to `httputil.go`; `ErrorCode` and the
inbound code allow-list gained the new stable codes).

**Three defects found and fixed DURING the chain, each worth recording because none was caught by the
suite as first written:**

1. **A send-on-closed-channel PANIC in `Forwarder`** (found by me on review of the implementer's
   output). `Enqueue` looked up the peer's queue under `f.mu`, released the lock, then sent — so a
   concurrent `Close`, which closes every queue under that same lock, could close the channel in the
   window. Fixed by performing the non-blocking send *under* the lock, which is safe only because the
   send has a `default` arm; the comment says so, because removing the arm would turn it into a
   deadlock. The test-engineer reproduced the pre-fix shape standalone and got
   `round 5: PANIC: send on closed channel`, then added
   `TestForwarderEnqueueIsSafeAgainstAConcurrentClose`.
2. **An unbounded peer string reaching a log line** (reviewer). `ValidateRelayRequest` `%q`-echoed
   `origin_bus` in the path-agreement check *before* `ValidatePeerBusID` bounded it. Checks 3 and 4
   were swapped. Separately, the loop-drop log line — the highest-volume line in the package, since a
   loop drop is a cycle's expected steady state — logged unvalidated `origin_bus`/`message_id`; it now
   logs only `BusPath[0]`, which `CheckIncomingPath` has already validated.
3. **The roster surface broke invariant 10** (reviewer, then a test I added found a second half). The
   handler validated the idempotency key, logged it, and threw it away — so a peer whose ack was lost
   retried, fell through to the version-monotonicity check and was answered **409 STALE**. That
   punishes precisely the peers retrying correctly. `RosterConfig.Apply` now receives the key and a
   `RosterUpdateFingerprint`, so the wiring site adjudicates through `internal/idem`. Writing the
   regression test then exposed that `RosterHandler` had no `ErrIdempotencyViolation` arm at all, so a
   genuine violation became a 503 — telling a peer to retry the thing being refused. Both fixed.

**Proofs, run through `scripts/proof-check.sh` and CONFIRMED RED FIRST** (I sabotaged the production
code, watched the proof fail on the right assertions, then restored):

```
proof-check: verdict=PASS class=test exit=0 tests_run=37 top_level=1 skipped=0 failed=0 empty_pkgs=0   # -run TestMessageRelay
proof-check: verdict=PASS class=test exit=0 tests_run=24 top_level=5 skipped=0 failed=0 empty_pkgs=0   # -run TestRelayLoopPrevention
proof-check: verdict=PASS class=test exit=0 tests_run=17 top_level=1 skipped=0 failed=0 empty_pkgs=0   # -run TestPeerRosterSync
```

Neither proof is VACUOUS. The RED runs: stubbing `PathContains` to return `false` failed
`TestRelayLoopPrevention/this_bus_is_on_the_path`, the case-folded variant, the split-horizon test and
all three cycle subtests; disabling the relay-key rule failed
`TestMessageRelay/refuses_an_incoherent_envelope/the_key_is_not_the_origin_message_id`.
`go test -race -count=1 ./internal/relay` → `ok … 13.7s`, three consecutive runs. `go build ./...`,
`go vet ./internal/relay` and `"$(go env GOROOT)/bin/gofmt" -l internal/relay` all clean (the bare
`gofmt` false-pass trap was avoided by using the GOROOT path).

**Security gate: CHANGES-REQUESTED, and the requested change was documentation.** No finding is
reachable today because nothing serves these handlers — but `doc.go`'s "What the gating tasks must not
forget" list *is* the handoff artefact, and it was missing seven properties. They are now in it. The
sharpest: `RelayRequest` carries no signature field, so **SIGN-6 is a prerequisite to serving
`RelayHandler`, not a follow-up** — message ids are `<bus>-<seq>` and sequential, so a peer can
pre-poison a range of a victim bus's ids, and when the genuine copy arrives it reads as
`OutcomeViolation` and invariant 10's mandated disconnect fires **at the honest peer**. Three
overclaiming comments were corrected rather than deleted, with the correction left visible: the
"every bound before the allocation" paragraph (the real decode-time bound is the pre-decode byte cap —
`encoding/json` materialises the slice before any count check of ours), and the `rosterhttp.go` claim
that 403-not-404 avoids a peer-enumeration oracle (the 403-vs-409 *split* is the oracle; only
authenticating the caller closes it).

**Deliberately NOT done, so nobody reads it as an omission:** no `scripts/bus-*.sh` wrapper and no
`AGENT_PROTOCOL.md` entry. Invariant 7's wrapper requirement is about the AGENT-FACING surface; this is
bus-to-bus, and it is not served. No `CONTRACTS-HTTP.md` route-table entry either — RELAY-1 set that
precedent for `/v1/peer/enroll` deliberately, and copying it is the correct call for an unserved path.
The shapes are recorded in a dated `CONTRACTS.md` section and in `PROTOCOL.md` instead, both marked
NOT REGISTERED.

**Open, honestly:** the test-engineer observed ONE failure of
`TestMessageRelay/a_loop_is_200_with_a_dropped_reason,_never_an_error_status` on the tree as it found
it, before any edit, and could not capture the assertion. It has not recurred in ~3,500 subsequent
executions (~2,900 by the test-engineer including 8-way parallel load and cold-cache runs, ~600 by
me). The only non-deterministic path in that subtest is `doRelay`'s `t.Fatalf("request: %v", err)` on a
transient `httptest` connection error, which would be harness fragility rather than a product defect —
but that is a hypothesis, not a diagnosis, so it is filed as a follow-up rather than closed.

---

## 2026-08-07 — MSG-FU-SUFFIXFLOOR (wiring half): the durable suffix allocator is now CONSTRUCTED at
startup (feature-runner)

**Task.** `94159d93-fe87-4c3e-b938-86fe7068c787`. `cmd/agent-bus/main.go:327` still built
`ids.NewNameSuffixes()` — a FRESH counter, every name restarting at suffix 1 on every start — while
fully-qualified agent ids are already durable inside WAL store-message records (`sender`,
`recipients`) and the WAL never compacts. `ids.OpenNameSuffixes` had ZERO production callers. That is
invariant 1 broken in a running bus, and it was about to become exploitable: `internal/auth` is
concurrently making the roster durable (AUTH-3), at which point a restart mints an id that ALREADY
EXISTS in persisted state.

**What landed.**

- `cmd/agent-bus/suffixfloors.go` (new): `openSuffixAllocator` — `ids.OpenNameSuffixes(dataDir)` →
  fold derived floors through `RaiseFloor` → `Seal()` ONCE, error CHECKED. Plus `walAgentIDFloors`,
  the legacy-dir backfill derivation, and `suffixBackfillExposure`.
- `cmd/agent-bus/main.go`: constructs through it; every failure is FATAL. The false justification
  comment at the old lines 312–326 (which told the next reader a fresh counter was safe and that
  AUTH-3 owned the fix — both wrong) is REPLACED, not deleted, with the reason the fresh counter must
  never come back. The startup WARN was NARROWED: its clause "and agent id suffixes restart from 1
  for every name" is now false, and the rest (roster/sessions in memory only) is kept.
- `cmd/agent-bus/suffixfloors_test.go`, `cmd/agent-bus/suffixrestart_test.go` (new).

**Three judgement calls, all recorded in `DECISIONS.md` (2026-08-07, this task's section plus its
ADDENDUM):** no fallback on any path including a fresh dir; scan AFTER `wal.Open` and
BOOT-with-an-ERROR when recovery had already removed records, but REFUSE when the derivation cannot
complete on a dir with no floors file; and the scan is GATED on `!Existed()` — at most once per data
directory.

**Both gates COMPLETED with CHANGES REQUESTED, and both changes were made.** They converged
independently on the same finding: the first draft ran the WAL scan on EVERY start so a rewound
floors file could be cross-checked, and `wal.ScanAll` retains every record INCLUDING FULL PAYLOADS on
a log that never compacts — `internal/wal` already carries a measured incident where a per-record
INDEX LIST cost 1.76 MB on a 23.7 MB log and was called "the boot-time OOM the eviction was written
to avoid" at 10 GiB. Retaining payloads is strictly worse. Gated, with the streaming cross-check
filed. Security additionally REPRODUCED a case the first draft got wrong: delete only
`agent-suffixes` from a live dir and the bus re-mints ids it has issued, silently at INFO, while the
standing WARN asserted the opposite. The seal line is now graded INFO/WARN/ERROR by case and the
suffix claim was DELETED from the WARN rather than negated — no unconditional sentence is true in
both cases. Both gates endorsed raise-over-refuse on the integrity branch: refuse-to-boot would hand
anyone with data-dir write access a permanent boot-denial primitive.

**Proof — behavioural, not a unit test.** The defect was never in the allocator (that was landed and
unit-tested at `61b7c9a`); it was that nothing CONSTRUCTED it, so every `internal/ids` test stayed
green while a restarted bus re-minted live ids. `TestRestartMintsStrictlyGreaterAgentIDSuffix` starts
a real server subprocess against a throwaway dir, enrols through POST `/v1/enroll` with a FRESH
keypair each time, restarts on SIGTERM, and restarts again after **kill -9** — the kill is the one
that matters, because it proves the floor was durable BEFORE the suffix was issued rather than
flushed on the way out. `TestLegacyDataDirDoesNotReMintAgentIDs` builds a dir with agent ids in WAL
message records and no `agent-suffixes` file and proves the ids are not re-minted (sender AND
recipient). `TestOpenSuffixAllocatorFailsClosed` proves a corrupt floors file, an unwritable seal and
an underivable log each STOP startup — a happy-path test cannot detect the regression this task
guards. `TestNoFreshSuffixCounterInCmd` parses the package AST (not a grep, so the comment that names
the forbidden constructor does not trip it) and fails if `ids.NewNameSuffixes` is ever called from
`cmd/` again.

`bash scripts/proof-check.sh` verdict: **PASS — 24 test(s) ran (8 top-level), 8 passed, 0 skipped**.
`go build ./...`, `go vet ./...` and `"$(go env GOROOT)/bin/gofmt" -l cmd/` all clean (the GOROOT path,
never bare `gofmt`). Also verified by hand against a real binary on a throwaway `/tmp` data dir:
`alpha-1` → SIGTERM → `alpha-2` → kill -9 → `alpha-3`, with `<data-dir>/agent-suffixes` reading
`alpha 3` at rest.

**Out of this task's file ownership, FILED (the reviewer checked the backlog and caught an earlier
draft claiming these were filed when they were not — they are now, with public ids):**

- `6f4c17ef-220c-465f-b8d8-a0f04aac1905` — export a streaming raw WAL scan and reinstate the
  every-start floors cross-check.
- `e5fa08ba-fe9a-40ae-bf35-dc69198bfdff` — `PROTOCOL.md:592-597` still says the wiring is NOT done
  (every clause now false), `internal/ids/doc.go:56-75` still names `main.go:327`,
  `internal/ids/agentmint.go:296-337` still justifies born-sealed by a caller that no longer exists,
  and `CONTRACTS-HTTP.md:330` quotes the old WARN verbatim.
- `d5ed5ccc-7178-4dd3-a61e-42ec976750b3` — acceptance criteria (c) and (d): flip
  `ids.NewNameSuffixes` to born-unsealed or delete it, and generalise the no-production-caller guard
  beyond package `main`. Verified there are ZERO production callers anywhere in the tree today; the
  task that previously carried (c)/(d) is superseded, so nothing else held them.
- `cca64afd-f75d-46e4-91ca-ebc502151253` — RELAY must roster-check LOCAL recipients before the
  durable write. `recipients` is server-derived only because `hub.publish` requires enrolment;
  `internal/relay` validates shape only, so once relay is served a peer could relay
  `<local-bus>.alpha-18446744073709551615` and permanently exhaust that name via this backfill.

**Release ordering:** the backfill folds message records only, so MSG-FU-SUFFIXFLOOR must ship before
or with AUTH-3 — a build with durable enrolment meeting a dir with no floors file would leave those
enrolment suffixes invisible.

---

## 2026-08-07 — SIGN-2/SIGN-6: the signing core (not the full feature)

**Task:** SIGN-2 / SIGN-6 wave. **Shipped:** `internal/signing/sign.go` (new) and
`internal/signing/sign_test.go` (new) — pure delegation to `crypto/ed25519` (invariant 9) over
`internal/signing.Canonicalize`'s bytes, passed UNHASHED. Exported: `const SignatureSize=64,
PublicKeySize=32, PrivateKeySize=64`; sentinels `ErrNoSignature`, `ErrSignatureLength`,
`ErrPublicKeyLength`, `ErrPrivateKeyLength`, `ErrVerify`; funcs `ValidateSignature(sig)`,
`ValidatePublicKey(pub)`, `Sign(priv, Message) ([]byte, error)`, `Verify(pub, Message, sig) error`.

**The full SIGN-2/SIGN-6 feature is NOT done** and remains todo in the Spec Server for three blockers
recorded in `DECISIONS.md` (2026-08-07, this task's section): no messaging keypair exists (CRYPTO-3,
SIGN-8 both todo); the durable id/sequence mint SIGN-1 requires does not exist, and minting without a
durable record would break `internal/hub`'s restart-floor counting argument; and the signature cannot
reach the durable record without `internal/hub`, which was outside this agent's file-ownership
boundary.

**Chain run:** implementer (this agent) → test-engineer → reviewer → security → documentation.
Reviewer's verdict was **CHANGES-REQUESTED**: one mutation-proven test blind spot — nothing pinned
`Sign`'s output to `Canonicalize`'s output, so an implementation agreeing with itself but not with
PROTOCOL.md §8 shipped green. Fixed by anchoring `TestSignVerifyRoundTrip` to raw `ed25519.Sign` over
`Canonicalize(m)`. Security's verdict was **PASS**.

**Evidence, quoted:**
```
RED  (panic guard removed):         verdict=FAIL class=test exit=1 tests_run=7 top_level=1 failed=1
RED  (verifier accepts all):        verdict=FAIL class=test exit=1 tests_run=11 top_level=6 failed=5
RED  (Sign/Verify drift, post-fix): verdict=FAIL class=test exit=1 tests_run=97 top_level=25 failed=1
GREEN:                               verdict=PASS class=test exit=0 tests_run=97 top_level=25 failed=0
```

**Noted honestly, not caused by this change:** `go vet ./...` currently FAILS in `internal/relay`
(`internal/relay/cycle_test.go:246:3: unknown field OriginSentAt in struct literal`). That is another
agent's in-flight relay work (staged `internal/relay/signed.go` + modified `message.go`).
`internal/signing` builds, vets and tests clean on its own.

---

## 2026-08-07 — DUR: the WAL record-index high-water mark is a dedicated write-ahead file (e120153b, db350e39)

**Task:** close two P0 Spec Server defects sharing one root cause — `e120153b` (a discarded tail
record's WAL index reissued on recovery) and `db350e39` (a whole-log quarantine resetting the index
space to 1, and with it, via `internal/hub`'s `Recovered.NextIndex - 1` derivation, every message id
the bus had ever minted). See `DECISIONS.md`, same date, "The WAL record-index high-water mark is a
dedicated write-ahead file, not derived from the log" for the full design record, the two
reconciled invariants, and the rejected alternatives.

**Chain run:** spec-keeper → implementer → test-engineer → reviewer → security → documentation
(this entry).

**Files changed:** new `internal/wal/indexfloor.go` (the `indexFloor` type: `openIndexFloor`,
`begin`, `reserve`, `seal`, atomic persist, `ErrIndexFloorCorrupt`); new
`internal/wal/indexfloor_test.go` and `internal/wal/indexfloor_crash_test.go` (unit and
crash-injection coverage); `internal/wal/log.go` (`Open` now opens the floor before touching the
log, computes the start index as the max of the replayed high-water mark / `Repair.NextIndex` /
`floor.burned()+1` / — when `Repair.LostUnidentified` — `floor.ceiling()+1`, and exposes
`(*Log).IndexFloorPath()`); `internal/wal/writer.go` (`Writer.Append` reserves durably before
stamping an index, block size `indexReserveBlock = 256`; a clean `Close` seals the floor);
`internal/wal/recover.go` (`Repair.NextIndex` now reports one past the highest index OBSERVED,
survivors and identified discards alike, not one past the highest SURVIVOR; new
`Repair.LostUnidentified`; the quarantine path sets it and no longer claims `NextIndex = 1` is
where the bus resumes); `internal/wal/replay.go` (`Recovered.FirstIndex`; `MissingRecords` now
counts skipped-but-never-used indices, an upper bound rather than an exact count; a log no longer
reports the range below its own first record as missing); `internal/wal/salvage.go` (framing-stage
discards set `LostUnidentified`; an implausible forged index is rejected rather than trusted — see
the security-bound test below); `internal/wal/doc.go` (new "The durable record-index floor"
section). Test-only edits to `internal/wal/crash_injection_test.go`,
`internal/wal/recover_test.go`, and `internal/wal/replay_crash_test.go` — see below.

**Nine existing tests whose assertions were INVERTED — that inversion was the task, not a
workaround, because each one previously asserted the rejected reissue behaviour as correct:**

1. `TestCrashInjectionMidCommitWriteKill` — wanted the resumed prepare index to be exactly 6 (the
   torn commit frame's own index, reissued); now wants `indexReserveBlock+1`, i.e. above every index
   the killed process ever authorised.
2. `TestCrashInjectionDiscardPathsRecoverAndLog` — its torn-tail and bit-rot table rows wanted
   `wantNextIndex: 6`; now `7`, because the discarded record's own index (6) is burned rather than
   handed back to the next write.
3. `TestCrashInjectionMidFileDamageDoesNotCascade` — wanted `wantNextIndex = 41` (one past the last
   record the file held); now `indexReserveBlock+1`, because the trailing junk byte is a
   framing-stage region discard (`LostUnidentified`) and recovery no longer guesses an index from an
   unreadable scrap.
4. `TestWALRepairTailTruncatesTornTail` — wanted the reissued index of a torn frame; now wants one
   PAST it, and additionally asserts `LostUnidentified`.
5. `TestWALRepairTailDiscardsDamageToACompleteFinalRecord` — same shape as (4), for a
   checksum-failure discard rather than a torn frame.
6. `TestWALRepairTailKinds` — table rows asserting `NextIndex` equal to the discarded record's own
   index; now one past it.
7. `TestWALRepairTailToHeaderOnly` — wanted `NextIndex 1` after truncation to the bare file header;
   now wants `NextIndex 2`, because the discarded frame's index (1) is observed and burned rather
   than reused by the header-only fallback.
8. `TestWALRepairTailThroughOpen` — wanted the first post-repair write to land at the discarded
   record's own index ({5 6}); now wants {6 7}.
9. `TestWALCrashTornFrameTailIsRepaired` (`replay_crash_test.go`) — wanted the post-crash write to
   land at {5 6} (the torn frame's own index, called "deliberate" at the time); now wants
   `{indexReserveBlock+1, indexReserveBlock+2}`, with the old reasoning ("nothing can have observed
   an unfsynced index") explicitly retracted in the comment: recovery cannot distinguish an
   interrupted write from an acknowledged one that later corrupted, so that argument was never sound.

**Crash-injection evidence** (`internal/wal/indexfloor_crash_test.go`,
`TestWALIndexFloorCrashNeverReissuesAnIndex` and neighbours): a same-process simulation was rejected
as evidence of nothing, because it still runs every `defer` and buffer flush a real crash is defined
by skipping — including the floor's own seal. Instead the test binary re-execs itself as a child
(`WAL_INDEXFLOOR_CHILD`), which opens a real `*wal.Log` against a `t.TempDir()`, writes several
transactions, fsyncs a REPORT FILE listing every index it was handed (so a no-op child, which would
also exit 0, cannot pass), and is then killed with a real `SIGKILL` — uncatchable, unblockable — by
the parent. The parent asserts on the wait status (`Signaled()` and `Signal() == SIGKILL`, not the
exit code), then damages/quarantines the data directory, restarts through `wal.Open`, and asserts
the next issued index is strictly greater than every index the report file says the child was
handed. A companion test, `TestWALIndexFloorRejectsAnImplausibleForgedIndex`, forges a frame index
of `1<<62` behind a MAC that does not verify and asserts recovery refuses to trust it — the durable
ceiling after recovery stays within one reservation block of the file's honest size, not anywhere
near the forged value, which is the security bound recorded in `DECISIONS.md`.

**Proof verdicts, quoted verbatim via `bash scripts/proof-check.sh`:**

RED (defects re-introduced in a throwaway worktree — reverting the nine inverted assertions back to
the old, reissuing behaviour, to prove they are load-bearing rather than decorative):
```
proof-check: verdict=FAIL class=test exit=1 tests_run=21 top_level=5 skipped=0 failed=5 empty_pkgs=0
```

GREEN (as shipped):
```
proof-check: verdict=PASS class=test exit=0 tests_run=21 top_level=5 skipped=0 failed=0 empty_pkgs=0
proof-check: verdict=PASS class=test exit=0 tests_run=305 top_level=96 skipped=3 failed=0 empty_pkgs=0
```
(the second line is `go test -race ./internal/wal`, the full package, not just the tests touched by
this task.)

**Honest limit: this is CODE-ONLY.** Nothing in this task was exercised against a running
`agent-bus` server — no wrapper script, no `POST /v1/enroll` through a live process, no restart of a
real binary. The crash-injection evidence above is real process-kill evidence at the `internal/wal`
package boundary, which is what CLAUDE.md requires for durability code, but it is not a claim about
live server behaviour, and no such claim is made here.

## 2026-08-07 — SIGN-7: cross-bus relay preserves the signed envelope byte-exact

**Task** `aeb90793-c0ac-43d8-b1d3-caa2e6f6a8c1`. Chain run: spec-keeper → implementer (×2) →
test-engineer (×2) → reviewer → security → documentation. All gates COMPLETED; none skipped.

**The two design questions SIGN-7 existed to answer.**

1. *Origin id vs local sequence — HOLDS against what SIGN-1 built.* `signing.Message` covers exactly
   `{MessageID, Sequence, Sender, Recipients, TimestampUnixMilli, Body}`. No local delivery sequence
   and no `bus_path` are in it, and `signing.validate` already enforces the origin binding (the bus
   half of the message id must equal the bus qualifying the sender). The receiving bus mints its own
   local sequence outside the signed bytes. No change was needed to make this true.
2. *Cross-bus key trust — was the open hole; the user ruled on it mid-task* (`c27ef78`, `1ec3196`).
   The origin bus's attestation is relayed intact under the origin's BUS SIGNING key, pinned at
   PEERING time; no TOFU anywhere, not even a hook; and the bus TLS key and bus SIGNING key are
   separate keys with separate rotations. Encoded as `relay.CrossBusTrust`, whose two-method shape
   (`PinnedBusSigningKeys` then `AttestedSignerKey(id, pins)`) forces an attestation to be checked
   against the ORIGIN's peering-time pins and nothing else. No implementation ships; there is no
   default and no "verification disabled" mode.

**The hole that made byte-exactness impossible, not merely unimplemented.** The relay envelope
carried `sent_at_unix_ns` — the origin BUS's nanosecond clock — while the signature covers
`TimestampUnixMilli`, the sending AGENT's millisecond clock. Different quantity, different source:
the envelope did not carry the signed timestamp at all, so canonical bytes could not be
reconstructed. Replaced with `timestamp_unix_ms`: one timestamp, the signed one, no conversion.

**Mechanism guaranteeing byte-exactness: RE-DERIVATION, never an opaque blob.** The envelope carries
covered field VALUES; the verifier rebuilds the bytes with `signing.Canonicalize` from the values it
will act on, per PROTOCOL.md §8.5. JSON transports values only, so no hop can re-encode the signed
bytes.

**Fail-closed decisions.** A relayed BROADCAST is rejected: `signing.Canonicalize` refuses an empty
recipient set, so no signature over a relayed broadcast can exist under format v1, and exempting it
would be the downgrade a peer reaches by setting `broadcast:true`. SIGN-3 owns the resolution.

**P1 found by the reviewer and fixed.** `relayFingerprint` folded recipients in WIRE order while
`signing.Canonicalize` SORTS them — so one signature covered every permutation, but the fingerprint
called a permutation a different payload. Under invariant 10 that is `idem.OutcomeViolation`, which
mandates DISCONNECTING the peer: a hostile peer could reorder a legitimately signed recipient array
and get an HONEST peer disconnected. `relayFingerprint` now sorts a copy. Rule recorded in
PROTOCOL.md: the fingerprint's notion of "same payload" must match the signature's, exactly.

**Gate verdicts.** Reviewer: 1×P1 (above, fixed), 5×P2. Security: PASS-WITH-CONDITIONS, **no P0**,
verified by a 16-attack adversarial probe compiled against a scratchpad COPY of the tree (the repo
was never written to); a hostile intermediate re-signing with its own key over byte-identical
canonical bytes gets `ErrBadSignature`. Security's P1 was that the fingerprint fix was unstaged —
now staged. Its P2 on the duplicated `ed25519.Verify` panic guard was fixed by delegating to
`signing.ValidatePublicKey`.

**NOT LIVE, and that is by design.** `internal/relay` registers no route and is imported by nothing;
`guards_test.go` still passes. It cannot be served until INVITE-PEERGUARD (`f5d91dbe`) and
MTLS-RELAYGUARD (`8192c3c7`) land — and now also cannot function until peering carries a bus signing
key, since `PeerEnrollRequest`/`PeerEnrollResponse` carry only `bus_id` and `agents`, so every
relayed message is `ErrUnpeeredBus` by construction. Recorded as `doc.go` handoff item 8.

**Proof.** `proof-check: verdict=PASS class=test exit=0 tests_run=281 top_level=66 skipped=0
failed=0` for `go test -race ./internal/relay/`. Verified in ISOLATION: the tree is red in
`internal/hub` from a concurrent agent's in-flight `store.NewMessage` arity change, unrelated to
this task.

## 2026-08-07 — AUTH-7 + SIGN-2/SIGN-6: durable enrolment WIRED, and a signature is MANDATORY (two-part wave)

Two parts landed together because the second depends on the first being real: a mandatory-signature
policy on a bus whose roster evaporates every restart is a policy nobody can comply with twice.

### Part 1 — AUTH-7: durable enrolment is WIRED (not merely written)

`cmd/agent-bus/main.go` constructs `auth.NewWALRoster`, attaches it to the WAL, and injects it into
`auth.NewService` and — adapted through the new `cmd/agent-bus/hubroster.go` — into the hub.

**The headline for operators: agents no longer re-enrol after a restart.** Agent ids, public keys
and each agent's ORIGINAL `enrolled_at` survive a restart and a `SIGKILL`. SESSIONS remain
memory-only, deliberately and permanently (short-lived bearer credentials; persisting them stores
replayable material to save one round trip), so each agent redoes the session handshake — but not
the enrolment. The startup WARN was rewritten accordingly: the old one asserted the roster was lost,
which is now the opposite of the truth.

**`hub.NoteEnrolment` and the hub's private roster map are DELETED.** The hub reads through to the
authoritative roster via the new `hub.RosterSource`, and `hub.Options.Roster` is REQUIRED — nil is a
hard error at `Open`, because a hub with nothing to read refuses every send, rejects every recipient
and serves an empty agent list *while looking healthy*. The old duplicate roster was the bug: after a
restart the hub's copy came back empty while auth's durable one came back full — a bus that
authenticated everyone and served nobody.

**Evidence:** verified END TO END against a running server — enrol two agents, `SIGKILL`, restart,
both authenticate with their existing credentials and `/v1/agents` lists both.

### Part 2 — SIGN-2/SIGN-6: a signature is MANDATORY on the wire

- **`POST /v1/mint` (NEW)** — reserve-then-send. `{"op","idempotency_key"}` → 201
  `{message_id, seq, sender, op, expires_at}`. A repeat of the same `(agent, op, key)` returns the
  SAME reservation with `Idempotency-Replayed: true` and burns no second sequence. It exists because
  SIGN-1 settled that the signature covers the ORIGIN bus's minted id and sequence, so a client
  cannot sign until it has them.
- **`POST /v1/send` (BREAKING)** — gains required `sender`, `message_id`, `seq`, `timestamp_ms`,
  `signature`. `sender` is INPUT TO VALIDATE, never an identity: every downstream use takes the
  principal from the session. Rejections: absent signature 400 · not base64 400 · not exactly 64
  bytes 400 (63 and 65 both) · sender ≠ authenticated caller 403 · bad/foreign/mismatched
  `message_id` 400 · `timestamp_ms <= 0` 400 · unknown or mismatched reservation 409. Every check
  runs before `hub.Send`, so a rejection leaves NO durable record, and it is TERMINAL for its
  idempotency key (no `Retry-After`).
- **The bus NEVER verifies the signature** — SHAPE only. It does not hold the sender's messaging key
  and must not be trusted to police messages for senders it does not control; a bus that could
  verify could equally forge.
- **`POST /v1/broadcast` answers 501, deliberately.** `signing.Canonicalize` rejects an empty
  recipient set and `store.Message` stores a broadcast as a FLAG not a roster snapshot, so a
  broadcast has no canonical audience under format v1 — SIGN-3's undecided question. SIGN-6 admits no
  unsigned message type, so the route fails closed. **This is a REGRESSION of a working feature and
  is documented as one** in README, AGENT_PROTOCOL, CONTRACTS-HTTP and CONTRACTS-CLI.
- **Read path** — `/v1/wait` and `/v1/messages` messages gain `timestamp_ms` (the SENDER's clock,
  COVERED by the signature) and `signature`. `sent_at` is unchanged, is the BUS's clock, and is NOT
  covered. Every doc that mentions one now states the distinction, because verifying against the
  wrong one fails every time and the field names do not hint at why.
- **Client/CLI** — the agent now holds a MESSAGING keypair distinct from its AUTH keypair
  (invariant 3), stored as `messaging_key_seed`, minted on first use, private half never leaving the
  machine. `busctl watch`'s NDJSON records carry `timestamp_ms` and `signature`.

### On-disk: two changes, both operator-visible

1. **`store.RecordVersion` 1 → 2**, RESERVED from the Spec Server `store-record-version` namespace
   (value 1 seeded in the same pass to cover the shipped v1 record). A **destructive, BIDIRECTIONAL**
   break: v1 message records are refused at recovery and discarded loudly, and a rollback discards v2
   records the same way. **There is no migration and there must not be one** — a pre-SIGN-6 message
   is unsigned, and synthesising a zero signature would manufacture records that look signed and
   verify as nothing. Enrolment (`"agent"`), invite and seqfloor records are unaffected; **no agent
   re-enrols because of this.**
2. **A new `wal.Entry.Kind`: `"seqfloor"`** (`hub.SeqFloorRecordKind`, body `{"v":1,"floor":N}`).
   `Entry.Kind` is free-form and needed no reservation. It records that every sequence `<= N` is
   BURNED, is fsynced AHEAD of any number being handed out, in batches of `hub.MintBatchSize = 256`,
   and is what makes the durable mint safe across a restart. It **retires the counting argument**
   (`NextIndex - 1`) that `hub.Open` rested on, replacing it with the direct and strictly stronger
   assertion *every sequence handed out is <= the durably-recorded floor*. Operator consequence:
   **sequence numbers now advance in jumps** — a restart typically skips to the next multiple of 256.
   That is CORRECT; `internal/ids/sequence.go` already binds consumers to treat the sequence as
   strictly increasing, never as dense.

### Chain that ran

spec-keeper → implementer → test-engineer → reviewer → security → documentation (this entry). No
step skipped.

**Security verdict: PASS with findings — 2 × P1.** One was fixed in code during the wave. The other
was this documentation pass itself, on two specific counts: (i) `CONTRACTS-ONDISK.md` still asserted
`store.RecordVersion = 1`, and (ii) `DECISIONS.md`'s only SIGN-6 section was titled *"the
mandatory-signature policy is BLOCKED"* and enumerated why SIGN-2/SIGN-6 could not be done — the
exact opposite of what shipped, actively misleading anyone who read it. Both are now closed:
`CONTRACTS-ONDISK.md` states version 2 and documents the bidirectional break, and `DECISIONS.md`
carries a new dated section that SUPERSEDES the blocked one BY NAME (append-only, so the old section
is left in place and marked historical).

### Honest limits — recorded so this wave is not oversold

- **No messaging public key is registered at enrolment** (`auth.Service.Enrol` leaves
  `RosterEntry.MessagingPublicKey` zero) and **CRYPTO-4 does not exist**, so a recipient can obtain a
  sender's messaging public key ONLY out of band. `client/keyring.go` is a local, manually-populated
  trust store and an explicit stopgap. No TOFU fallback was added and none may be.
- **Recipient-side verification is NOT wired into `client.Read`.** Signing works end to end and the
  signature is carried and returned; automatic verification on receive does not happen. What a
  recipient CAN do today is verify manually given an out-of-band key — proven: a client-made
  signature verifies under `internal/signing.Verify` from the wire fields.
- **Code-doc defects reported, not fixed** (documentation does not edit `.go` files):
  `client/messages.go` documents `Batch.Messages` as "the VERIFIED messages", which is FALSE today;
  and `client/store.go`, `client/client.go`, `client/keyring.go` direct operators to
  **`busctl keygen`** and **`busctl trust`**, **neither of which exists** — the registry in
  `cmd/busctl/root.go` is exactly eight subcommands and contains neither.
- **Invariant 7 is NOT satisfied for three capabilities** (`/v1/mint` has no dedicated entry point;
  `busctl keygen` and `busctl trust` do not exist), recorded as an explicit open item in
  `CONTRACTS-AGENT.md` rather than reported as met. `scripts/` holds only `bus-serve.sh`,
  `proof-check.sh` and `spec-cloud.sh`; the agent-facing surface is `cmd/busctl`.

### Docs changed by this pass

`CONTRACTS.md`, `CONTRACTS-HTTP.md`, `CONTRACTS-ONDISK.md`, `CONTRACTS-CLI.md`, `CONTRACTS-AGENT.md`,
`PROTOCOL.md` (new §8.4.1 recording where the shipped mint DIVERGES from §8.4's specification — the
reservation is bound to the idempotency key only in memory, not on disk), `AGENT_PROTOCOL.md`,
`DECISIONS.md`, `AGENT_LOG.md` (this entry), `README.md`.

---

## 2026-08-07 — DISCOVERY-DOC: `GET /v1/discovery`, an unauthenticated protocol-discovery document

**Task.** Add a new, unauthenticated, bounded, STATIC protocol-discovery document at
`GET /v1/discovery` so an agent holding nothing but a bus URL can learn how to enrol, plus one
compile-time-constant pointer field (`"discovery":"/v1/discovery"`) on the already-unauthenticated
`GET /v1/info` so a caller that only knows that endpoint can still find it.

**Chain that ran.** spec-keeper → implementer → test-engineer → reviewer → security → documentation
(this entry) — all six ran, nothing skipped.

**Files changed (code, by implementer/test-engineer, prior to this documentation pass):**
`internal/httpapi/discovery.go` (new), `internal/httpapi/discovery_test.go` (new),
`internal/httpapi/server.go` (`Server.discovery` field, built once in `New`; `InfoResponse.Discovery`;
route registration), `internal/httpapi/authmw.go` (allow-list gains `RouteDiscovery`),
`internal/httpapi/authmw_test.go`, `internal/httpapi/durable_test.go`,
`internal/httpapi/healthz_info_test.go`.

**Docs changed by this pass:** `CONTRACTS-HTTP.md` (Routes table: new `GET /v1/discovery` row and its
405 row; `/v1/info`'s 200 row now shows the `discovery` field; a new `### Discovery document`
subsection documenting all ten top-level fields and the four governing invariants — protocol-not-
roster, static endpoint list, built-once, signing context withheld; every "five-entry allow-list"
passage — the routes-table row, the `Authorization` header row, the `## Authentication` section
heading sentence and its bulleted enumeration, and the prose sentence just above it — updated to six,
with a new bullet for `/v1/discovery`; the `Allow` header row gains `/v1/discovery`; a new
`RouteDiscovery` row in the Exported Go Surface table), `DECISIONS.md` (new dated entry: why a
separate `/v1/discovery` rather than a bigger `/v1/info`, including the genuine merit of the rejected
alternative and the two supporting decisions — the endpoint list is static, not mux-derived, and no
self-URL is echoed), `AGENT_LOG.md` (this entry). **Not touched, per this task's explicit file
ownership:** `AGENT_PROTOCOL.md`, `CONTRACTS-CLI.md`, `CONTRACTS-AGENT.md`, `CONTRACTS-ONDISK.md`,
`CONTRACTS.md`, `README.md`, `PROTOCOL.md` — the agent-facing CLI half (invariant 7: a Go CLI
subcommand for this endpoint) is deliberately deferred to the filed follow-up
`DISCOVERY-DOC-FU-CLI`; until it lands there is no `agent-busctl` subcommand for `/v1/discovery`.

**Existing pinned-field-set tests deliberately updated, and none weakened — each remains an
exhaustive/exact assertion, verified by reading the diff, not assumed:**
- `healthz_info_test.go` — `/v1/info`'s `wantKeys` grew from 3 entries (`bus_id`, `version`,
  `uptime_seconds`) to 4 (`+discovery`), still an exact `len(firstGeneric) != len(wantKeys)` check,
  not a subset check.
- `durable_test.go` — the `/v1/info` leak guard's field allow-list grew from 3 names to 4
  (`+discovery`), still a `k != "bus_id" && k != "version" && k != "uptime_seconds" && k != "discovery"`
  exhaustive negative check across every differently-configured server in the table, not loosened to
  a prefix or substring test.
- `authmw_test.go` — `TestEveryRouteRequiresAuth`'s golden allow-list grew from 5 entries to 6
  (`+/v1/discovery`), still asserted as an exact sorted-slice equality against
  `httpapi.UnauthenticatedRoutes()`, not a "contains" check.

`discovery_test.go` itself (new) adds its own independent exhaustive pins on top:
`TestDiscoveryFieldSetIsPinned` (top-level and every nested object, by exact key-set equality against
a generically-decoded body — a typed struct would silently tolerate an added field, which is why the
decode is untyped `map[string]interface{}`), `TestDiscoveryDocumentIsStatic` (byte-identity of the
response across five differently-configured servers, including one with the full messaging surface
registered), `TestDiscoveryDocumentLeaksNoBusState` (no enrolled agent id/name/key, no on-disk path,
and — the one that matters most — `auth.SessionSigningContext` itself never appears in the body),
`TestDiscoveryPathsMatchRouteConstants` (every endpoint's `path`/`auth` checked against the real
`Route*` constants, plus a check that `/v1/broadcast` is absent from `endpoints`), and
`TestDiscoverySessionConstantsMatchAuth` (the mirrored `3600`/`2700` literals checked against
`auth.SessionLifetime`/`auth.RefreshAfter()` directly, not against a comment).

**Proof-check verdict, quoted verbatim:**

```
proof-check: verdict=PASS class=test exit=0 tests_run=83 top_level=14 skipped=0 failed=0 empty_pkgs=0
```

**Verified against a RUNNING bus** (throwaway data dir, plaintext loopback — not TLS, not deployed):
`GET /v1/info` returned the new `discovery` field; `GET /v1/discovery` returned 6169 bytes with no
credential presented; `POST /v1/discovery` returned 405 with `Allow: GET`.

**Independently reverified during this documentation pass** (narrower, package-scoped, this box's
go1.19.4 toolchain): `go build ./...` clean; `go test -run TestDiscovery ./internal/httpapi/...`
— all 12 `TestDiscovery*` top-level tests PASS, including every subtest above. A throwaway,
not-committed test binary confirmed the byte count is `bus_id`-length-dependent (6167 bytes observed
against a differently-named test bus id, 6169 against the bus id the implementer/test-engineer used)
— consistent with, not contradicting, the shape rule that `bus_id` is the only variable input; the
scratch test file was deleted before this report, not committed.

**Deliberate scope limit.** This is the SERVER half only. Invariant 7's CLI half — a compiled
`agent-busctl` subcommand and its `AGENT_PROTOCOL.md` entry, both required by invariant 7 in the SAME
task as any capability, and both deliberately NOT done here — is filed separately as
`DISCOVERY-DOC-FU-CLI`. Until that lands, an agent cannot reach `/v1/discovery` through the compiled
CLI; only a caller that hand-constructs the HTTP request can, which is itself the exception invariant
7 exists to close.

**Not verified by this pass** (outside this task's remit, flagged rather than asserted): whether
`DISCOVERY-DOC-FU-CLI` has been claimed or started; whether this code has been merged past whatever
branch state produced the numbers above — `git status --porcelain` at the start of this pass showed
this code staged but not committed, and this documentation pass does not commit.

### 2026-08-07 — DISCOVERY-DOC addendum: gate findings applied (supersedes the byte counts above)

The reviewer and security gates returned AFTER the documentation pass above was written, and both
asked for changes. This addendum records what changed; where it disagrees with the entry above, this
addendum is correct.

**reviewer — CHANGES-REQUESTED, but explicitly requesting no code edit.** Its two P1 blockers were
the `CONTRACTS-HTTP.md` and `DECISIONS.md` deliverables, both since written. It verified all seven
factual claims in the document as TRUE at HEAD, confirmed no pin was loosened, and confirmed the
byte-identity test genuinely proves the static claim. Three P2s were applied:

- The mirrored session constants are no longer hand-copied literals. `discovery.go` now DERIVES them
  (`int(auth.SessionLifetime/time.Second)`, `int(auth.RefreshAfter()/time.Second)`); `internal/httpapi`
  already imports `internal/auth`, so the file comment claiming a new dependency edge was avoided was
  simply false. Desync is now structurally impossible rather than test-pinned.
- The hand-rolled `itoa` in `discovery_test.go` was replaced with `strconv.Itoa` (invariant 8).
- The "broadcast is not advertised" subtest now also asserts `POST /v1/broadcast` really returns 501,
  so limitation 4 fails the day SIGN-3 un-refuses the route instead of quietly going stale.

**security — PASS-WITH-NITS.** It verified live (not from the tests) that the static-list claim holds
in code, that no Host-header or request-derived value reaches the response, that the fail-closed
allow-list matching is preserved (`/v1/discovery/`, `//v1/discovery`, `/v1/Discovery`, `%2f` and
`/v1/../v1/discovery` all 401), and that neither `auth.SessionSigningContext` nor
`client.MessageSigningContext` appears in the body. Four findings were applied:

- **MEDIUM — the enrolment recipe conflated two keypairs.** The document implied one Ed25519 key.
  There are two: the AUTH key the bus records at enrolment, and a separate MESSAGING key the
  reference client mints and never sends. A client built from the old text would have signed sends
  with a key no recipient can obtain. Steps 3 and 8 now name both keys and state that nothing
  distributes the messaging public key.
- **LOW — limitation 2 was misframed as temporary.** The bus does not merely "not yet" verify
  signatures; `internal/httpapi/messages.go` is explicit that it never will, because verifying would
  move the trust boundary onto the bus. Reworded to "the bus enforces shape, the recipient enforces
  authenticity", while still saying the recipient cannot verify today either.
- **LOW — limitation 3 understated exposure.** Bodies are not only served in the clear, they are
  PERSISTED UNENCRYPTED; only the audit log omits them. Said so.
- **LOW — per-request marshal on an unauthenticated route.** The document is now marshalled ONCE in
  `New` into `Server.discoveryJSON` and served through a new `writePreformattedJSON` helper that sets
  exactly the headers `writeJSON` sets. An anonymous request now costs a write, not a ~7 KiB marshal.

**Corrected measurements** (the entry above records 6169/6167 bytes, taken before these edits): the
document is now **7107 bytes**, re-verified against a RUNNING bus on a throwaway data dir — `/v1/info`
carried the `discovery` field, `GET /v1/discovery` returned 7107 bytes with no credential, and
`POST /v1/discovery` returned 405 with `Allow: GET`. Still far under the 16 KiB ceiling pinned by
`discoveryMaxBytes`. The size remains a function of `bus_id` length only.

**Chain:** spec-keeper → implementer → test-engineer → reviewer → security → documentation. All six
ran; none skipped. Still CODE-ONLY and NOT deployed.

**Flagged, NOT fixed (outside this task's file ownership):** `README.md` line ~100 still shows the old
three-field `/v1/info` body; and a stale 7.6 MB `busctl` ELF sits untracked at the repo root and is
NOT covered by `.gitignore` (which lists `/agent-bus` and `/agent-busctl`, not `/busctl`) — it must be
deleted or ignored, never committed.

---

## 2026-08-07 — MTLS-PIN: the client pins the bus's certificate fingerprint

**Task:** `MTLS-PIN` (`8c46dc93-16d0-4eea-8ad3-ac51136551e2`, P0, epic MTLS). Run end to end by
`feature-runner` (opus) under the mandated chain. **CODE-ONLY — not deployed, and the bus does not
serve TLS yet (`MTLS-LISTENER`), so nothing about this is observable in production today.**

**One sentence:** the client refuses to speak TLS to a bus whose certificate is not the one it was
told to expect, and there is no way to ask it not to.

**Sequenced ahead of `MTLS-LISTENER` deliberately.** Substituting all three key files in an
established data dir makes the bus restart cleanly with a different fingerprint and zero warnings —
key loss is loud, key substitution is silent — so the fingerprint defends nothing until a client
checks it. Rationale recorded in `DECISIONS.md` (2026-08-07).

**Files changed (all owned by this task):**
`client/pin.go` (new), `client/pin_test.go` (new), `client/guard_test.go` (rewritten),
`client/transport.go`, `client/client.go`, `client/config.go`, `client/store.go`, `client/enrol.go`,
`cmd/agent-busctl/root.go`, `cmd/agent-busctl/enrol.go`, `cmd/agent-busctl/whoami.go`,
`CONTRACTS-CLI.md`, `AGENT_PROTOCOL.md`, `DECISIONS.md`, `AGENT_LOG.md`.

**Surface added:** `--bus-fingerprint <hex>` (global) / `AGENT_BUS_FINGERPRINT`;
`client.Config.BusFingerprint`; `client.Identity.BusFingerprint` → `bus_fingerprint` in
`identities.json` and in `enrol`/`whoami` `--json` (both `omitempty`, store format version
deliberately **not** bumped — additive, and an older credential is still valid); exported
`BusFingerprint`, `ParseBusFingerprint`, `BusFingerprintError`, `ErrBusFingerprintMismatch`,
`ErrBusPresentedNoCertificate`.

**The one thing a reviewer should look at first:** `client/pin.go` sets `InsecureSkipVerify: true`
paired with `VerifyPeerCertificate`, which the previous guard banned outright. It cannot be avoided
— self-signed with no CA means the default chain check cannot succeed, and the client holds a
fingerprint rather than a certificate so it cannot build a `CertPool` either. The guard was narrowed
to one file AND made structural (AST: the pairing is enforced, the assignment form is banned, and at
least one paired literal must exist). Full reasoning in `DECISIONS.md`.

**Chain:** spec-keeper → implementer → test-engineer → reviewer → security → documentation. The
implementer, test-engineer and documentation steps were carried out by `feature-runner` itself
rather than delegated — the change is one package and its docs, and splitting it would have put the
`InsecureSkipVerify` judgement in a different context from the guard that constrains it. Reviewer
and security ran as separate gates and are recorded in the task's Spec Server notes.

**Gate outcome — both COMPLETED against the FINAL code, not an earlier snapshot.** This is recorded
explicitly because it is the thing this repo has got wrong three times. Reviewer first returned
CHANGES-REQUIRED (its P1 was reproduced, not theorised: the certificate-mismatch remedy named a
command that the flag-vs-store conflict rule then refused — a dead end). Security returned PASS with
findings. Both were then re-run against a FROZEN tree, identified by
`md5sum client/*.go cmd/agent-busctl/{enrol,root,whoami}.go | md5sum` =
`4df6e4c572995867adfb087392a4a806`, and both verified that hash before reading. **Reviewer: PASS.
Security: PASS.** Only markdown changed afterwards — the two doc corrections the gates themselves
asked for — and the hash above was re-checked and is unchanged, so both verdicts cover exactly the
code that would be committed.

Each gate also independently mutation-tested rather than trusting the tests: replacing
`verifyPinnedBusCertificate`'s body with `return nil` fails the behavioural and table tests while the
AST guards still pass, and deleting `VerifyPeerCertificate` from the `tls.Config` literal fails the
AST guard. That shape-versus-semantics split is why both kinds of test exist and why neither may be
deleted in favour of the other. Each gate also RETRACTED one wrong finding of its own (security: an
`InsecureSkipVerify` in `internal/relay` that does not exist; reviewer: that the unknown-scheme
`default:` arm was missing when it was already present) — both retractions were the result of being
asked to re-run the check rather than restate it.

**Verification:** `go build ./...` green; `go vet ./client/... ./cmd/agent-busctl/...` green;
`"$(go env GOROOT)/bin/gofmt" -l client cmd/agent-busctl` produced **empty output**;
`go test -race ./client/... ./cmd/agent-busctl/...` both `ok`. Test runs were deliberately scoped
away from `internal/httpapi` and `internal/wal`, which have other agents' uncommitted work in them.
The `proof_cmd` was replaced (the original grepped `CONTRACTS-AGENT.md`, which is outside this
task's file ownership and documents the retired shell wrappers) with one whose doc greps pin the
literal `--bus-fingerprint` and were confirmed **RED at HEAD before the docs were written**.

**Not done here, on purpose:** the invite blob (`ENROL-SHAPE`) — the pin arrives by flag/env/store
until the blob exists; the client certificate half of mutual TLS (`MTLS-CLIENTCERT`); serving TLS
(`MTLS-LISTENER`); certificate **expiry/validity** checking (`MTLS-VERIFY`); rotation with two
concurrent certificates.

## 2026-08-07 — feature-runner: closed the two P1 security blockers on the WAL index floor (db350e39)

**Task:** fix the two P1s the security gate raised against the staged `internal/wal` index-floor
work, without regressing the proven index-reuse fix. Scope: `internal/wal/**`, plus appends to the
shared append-only docs and `CONTRACTS-ONDISK.md`.

**P1-1 (a) — the upgrade path from `main` was broken.** A two-line v4 body was declared CORRUPT on
the premise that v4 never shipped. `f56c723` is in `main` and writes exactly that. v4 now reads the
two pre-HMAC shapes with `sealed` forced FALSE and rewrites them keyed at the next `begin`.
**(b) — the remedy text** told the operator that deleting the floor "is correct unless the log has
ALSO been damaged", which nobody can determine. Rewritten to name the remedy AND state that deleting
FORFEITS INVARIANT 1 unless the previous run closed cleanly.

**P1-2 — the seal was forgeable.** The floor is now authenticated with HMAC-SHA256 under the data
directory's own `wal-mac.key`, using the SAME `hmac.New(sha256.New, key)` pattern `format.go`'s frame
MAC uses (no invented construction, invariant 9), compared with `hmac.Equal`. The tag covers the
version line plus the body. Two consequential ordering/behaviour decisions came with it — the floor
is now read AFTER the MAC key is settled, and an UNVERIFIABLE floor (key lost, new one minted) is
read unauthenticated with an ERROR rather than bricking the bus. Both are argued in `DECISIONS.md`.

**Also fixed (P2s, cheap and self-contained):** the temp reaper no longer interpolates `-data` into a
`filepath.Glob` pattern (`os.ReadDir` + prefix match); severe discards are logged first within the
same cap so a dangling COMMIT can no longer be crowded out of the operator log.

**RED BEFORE / GREEN AFTER, all three P1 arms, with an independent probe driver that measures the
resumed index by performing a REAL `Write` rather than trusting `Recovered().NextIndex`:**

| arm | before | after |
| --- | --- | --- |
| P1-1a upgrade from `main` | `ErrIndexFloorCorrupt`, bus refuses to start | starts, no reuse, file rewritten keyed |
| P1-1b remedy text | contains the unsound caveat, never says "forfeit" | caveat gone, cost stated |
| P1-2 forged `sealed 1` + truncation sweep | **2268 of 2289 offsets REISSUED**, 0 refused | **0 of 2289** |
| P1-1b cost measurement (floor deleted per the old remedy) | 2268 of 2289 REISSUED — the evidence for the text change | unchanged by design; it is why the text changed |

**The four mandated regression sweeps, all measured by a real `Write`, all ZERO violations:**

| sweep | offsets | reuse violations |
| --- | --- | --- |
| (b) crash + every truncation offset | 2288 | 0 |
| (c) `bus.wal` deleted / zero bytes | 2 | 0 (first index 65 vs highest handed 24) |
| (d) clean `Close` then truncate, every offset | 2288 | 0 |
| (e) 4 chained crash cycles crossing the 64-block + every offset | 31147 | 0 (320 distinct indices, none repeated) |

**Verification:** `go build ./...` green; `go vet ./internal/wal` green;
`"$(go env GOROOT)/bin/gofmt" -l internal/wal` produced **empty output** (judged by output, never by
exit status); `go test -race -count=1 ./internal/wal/` **ok**. Test runs were scoped to
`./internal/wal/...` — `internal/httpapi`, `client/` and `cmd/agent-busctl` have other agents' live
work in them, and `TestBusFingerprintEnvIsRead` in `client/pin_test.go` is red from that work, not
from this.

**Non-vacuity was checked for every new test, not assumed.** The three P1 probes were run against a
`cp -a` copy of the pre-fix tree and reproduced RED with the numbers above. The new P2-2 test was run
against a copy with the file-order cap restored and failed there. Nothing in the repo was modified by
any probe; every one ran in `/tmp`.

**The chain:** implementer (this agent) → test-engineer (this agent) → reviewer → security →
documentation (this agent, for `CONTRACTS-ONDISK.md` + `internal/wal/doc.go`). Both gates were re-run
against THESE changes specifically, not against the earlier work.

**Gate verdicts.** SECURITY: **PASS** — both P1s independently re-verified closed with its own driver
(forged seal + full truncation sweep **0 of 2289** reissued, was 2268/2289; header-spelling downgrade
2287/2287 refused; all 137 single-bit flips of a keyed floor rejected; `f56c723`'s two-line body opens
and is rewritten keyed). It also measured that the unauthenticated-read path is strictly LOUDER than
the deletion baseline it is compared against, and that reaching it requires destroying the log first.
REVIEWER: **CHANGES-REQUESTED (minor — two comment claims, no code change)**, reverting one source
line per fix to confirm each test was RED before. Both were **measurably false statements in
comments**, which is the same failure mode as the "v4 never shipped" premise that caused P1-1, so
they were fixed rather than argued:

1. `log.go` claimed `repairLog` "never deletes bytes without moving them aside". It does — `truncateAt`
   truncates permanently and the mid-file rewrite renames a temp over the original; only a quarantine
   preserves bytes. Measured: 839 bytes before a refused `Open`, 789 after. The decision survives on a
   narrower argument (those bytes are already-logged damage any successful start would discard); the
   comment and the `DECISIONS.md` entry now say so.
2. `log.go` claimed the reorder makes a wrong key always surface as `ErrMACKeyMismatch`. It only does
   for a v2 log longer than its own header; a wrong key over a log with no readable record — including
   a POST-QUARANTINE directory — still reports a corrupt floor. Narrowed, not eliminated, and now said
   so. The residual is mitigated in the error text, which names `wal-mac.key` as the FIRST thing to
   check, ahead of the remedy.

Also taken from the security gate's non-blocking notes: the corrupt-floor error now says the operator
cannot read "did it close cleanly" FROM the file, because the flag lives in the body that will not
verify. Follow-ups are recorded at the end of the `DECISIONS.md` section — most importantly that an
unauthenticated floor claiming `reserved = 2^64-2` gets RE-SIGNED under a valid HMAC at `begin`,
which is the one state deletion cannot produce and the only place "no worse than deletion" fails.

**No commit was made** — an integrator owns that.

## 2026-08-07 — DOCS-2: `PROTOCOL.md` completeness/accuracy pass against HEAD (`documentation`)

`PROTOCOL.md` already existed (939 lines) when this task was picked up, so this was a gap-closing
pass against the current code and `CONTRACTS-*.md`, not a from-scratch write. Three findings, all
verified against HEAD before editing, not assumed:

1. **§9 (`agent-suffixes`) carried a STALE, now-FALSE claim.** It said `cmd/agent-bus/main.go` never
   called `ids.OpenNameSuffixes` and that a restarting bus still re-minted agent ids. `AUTH-3-FU-
   FAILOPEN` wired that in; `cb79486` corrected the same false claim in `CONTRACTS.md` and
   `internal/ids/doc.go` on 2026-08-07 but **missed `PROTOCOL.md`**, which still said "NOT yet done"
   at HEAD (`git show HEAD:PROTOCOL.md | grep` confirmed it). Corrected in place, and the section now
   also documents the second fatal case `AUTH-3-FU-FAILOPEN` added — a MISSING `agent-suffixes` file
   on a data directory that has history — which §9 never mentioned even before it went stale.
2. **On-disk format version 4 (`wal-index-floor`, `e120153b`/`db350e39`, keyed HMAC since `1ca7f83`)
   was completely absent from `PROTOCOL.md`**, despite `CONTRACTS-ONDISK.md` fully specifying it and
   `internal/wal/indexfloor.go` shipping it with a crash-injection suite. Added as new §11 (append-
   only; §1's and §9's cross-referenced section numbers are unchanged, since `CONTRACTS.md`,
   `CONTRACTS-HTTP.md` and `DECISIONS.md` all cite `PROTOCOL.md` §1/§3.2/§4/§6/§8.x/§9/§10 by number
   and renumbering would silently break those references). §1's table gained a one-line pointer
   noting versions 3/4 exist and where they live, matching `CONTRACTS-ONDISK.md`'s own namespace note.
3. **The literal phrase "at-least-once" was absent from `PROTOCOL.md`** even though the concept is
   argued at length in §10 and the ORIGINAL DOCS-2 task explicitly mandated stating it. Added one
   sentence in §10's idempotency-fingerprint discussion, the place the doc was already making the
   argument without naming it.

**Checked and found NOT stale** (verified, not assumed): the `record-type` table (§3.3, still 4
values, matches `internal/wal/format.go`); `GET /v1/discovery` and `bus_fingerprints` (both are
correctly out of `PROTOCOL.md`'s stated on-disk-only scope — HTTP wire shape lives in
`CONTRACTS-HTTP.md`, the client's `identities.json` pin-set lives in `CONTRACTS-CLI.md`, and a
`CONTRACTS-ONDISK.md` task for the latter is already filed separately in the backlog, P2); the
`agent-busctl` binary name (not mentioned in `PROTOCOL.md` at all, so nothing to correct); the
IDEM-11 `idem` field on the PREPARE payload (deliberately payload-opaque per §3.2's own framing-layer
scope, and fully owned by `CONTRACTS-ONDISK.md` already — not duplicated here).

**Proof, RED confirmed before the change, then GREEN:**
```
$ git show HEAD:PROTOCOL.md | grep -n "Production wiring — NOT yet done"   # RED: found (line 807)
$ git show HEAD:PROTOCOL.md | grep -n "wal-index-floor"                    # RED: no output
$ git show HEAD:PROTOCOL.md | grep -ni "at-least-once"                     # RED: no output
$ bash scripts/proof-check.sh \
  '! grep -q "Production wiring — NOT yet done" PROTOCOL.md && \
   grep -q "Production wiring — DONE" PROTOCOL.md && \
   grep -q "wal-index-floor" PROTOCOL.md && grep -qi "at-least-once" PROTOCOL.md'
verdict=PASS class=file-assertion exit=0
```

**Files changed:** `PROTOCOL.md` only (138 insertions, 10 deletions — the deletions are the two
stale sentences in §9 that were replaced, not removed content). No source file, `CONTRACTS-*.md`, or
`AGENT_PROTOCOL.md` was touched — none of this task's findings required a route, flag, or agent-
facing surface to change, only the maintainer-facing on-disk-format doc catching up to code already
at HEAD. No commit was made — an integrator owns that.

## 2026-08-07 — DOCS-2 CORRECTION: the "append" above was actually an INSERT, and two identifiers in the new §11 heading looked like git shas but were not (`documentation`)

**The entry directly above this one is WRONG about the file's own structure, and this entry corrects
it rather than editing it in place** (the original had already been read by the integrator and the
coordinator; the false claim needs a visible correction, not a silent rewrite). The integrator
refused the commit and caught both defects; neither was self-caught.

1. **§11 was INSERTED at line 948 of a 1067-line file, not appended.** The pre-existing relay
   signature error-code subsection — `unsigned`/`bad_signature`/`unpeered_bus`, the 400-vs-403 split,
   `ErrUnpeeredBus` — sat at what was then lines 1049–1067 and is §10's content, not §11's. Inserting
   §11 ahead of it left that content physically inside the "## 11. The WAL record-index floor"
   heading. The prior entry's own justification for appending — preserving the `§1`/`§3.2`/`§4`/`§6`/
   `§8.x`/`§9`/`§10` cross-references other docs cite by number — was correct in *intent* but the edit
   did not execute it: the section *numbers* survived, the section *contents* did not, which is a
   worse failure than a renumbering would have been, because nothing about the heading text signals
   it happened. `PROTOCOL.md:593` ("§10 carries the concrete envelope, the caps and the status
   codes") and `CONTRACTS.md:265` ("See PROTOCOL.md §8.5 and §10") were both silently degraded by
   this.
   
   **Fixed by relocation only, no wording change:** the §11 block (heading through its closing
   "Scope limit, stated plainly" paragraph) was moved to genuinely follow §10's relay error-code
   subsection, restoring that subsection to §10 and making §11 the file's true last section.
   Mechanically verified as a pure reorder before applying — `diff <(sort <old>) <(sort <new>)`
   reported an identical line multiset — so no sentence was altered or dropped in the move, only its
   position changed (the heading's citation was separately fixed, see point 2). Re-verified after:
   `grep -nE '^## ' PROTOCOL.md` puts §11 last with nothing after it, the file's last line is §11's
   own closing sentence about the audit trail, and the `unpeered_bus`/`ErrUnpeeredBus` table is once
   again physically under §10.

2. **§11's heading cited `` `e120153b`/`db350e39` `` in the same backtick styling used elsewhere in
   this document for real git shas (`cb79486`, `1ca7f83`, `f56c723`) — but both are Spec Server task
   public ids, not commits.** `git show e120153b` finds nothing. Fixed by citing the actual commits
   that landed the file instead — `f56c723` (introduced `internal/wal/indexfloor.go`, unkeyed) and
   `1ca7f83` (hardened it to the keyed HMAC this section documents) — both verified with `git log`
   against real history before use, and the Spec Server task ids are now named explicitly in prose
   ("Spec Server tasks `e120153b` and `db350e39`") rather than left to be inferred from styling alone.

**Re-run of the original three RED/GREEN greps, against the corrected file:**
```
$ grep -n "Production wiring — NOT yet done" PROTOCOL.md   # (no output — still fixed)
$ grep -n "wal-index-floor" PROTOCOL.md | head -3           # present, §1/§11
$ grep -ni "at-least-once" PROTOCOL.md                       # present, §10
$ bash scripts/proof-check.sh \
  '! grep -q "Production wiring — NOT yet done" PROTOCOL.md && \
   grep -q "Production wiring — DONE" PROTOCOL.md && \
   grep -q "wal-index-floor" PROTOCOL.md && grep -qi "at-least-once" PROTOCOL.md'
verdict=PASS class=file-assertion exit=0
```
Plus two new structural checks the previous pass never ran: `grep -nE '^## ' PROTOCOL.md | tail -1`
names §11 as the last heading in the file, and the file's last line is §11's own "does not protect
the audit trail" sentence rather than §10's relay content — confirming the relocation actually landed
where intended, not merely that both sections' text still exists somewhere in the file.

**Files changed:** `PROTOCOL.md` (reordered, plus the heading-citation fix — net five-line delta
from the reorder point-of-view, since the §11 heading gained two extra lines of citation prose) and
this `AGENT_LOG.md` entry. Nothing else. Not yet handed back to the integrator by this agent — the
task orchestrator relayed the refusal and will re-route.

**A note on why point 1 happened at all, for whoever reviews the next append-vs-insert claim in this
repo:** the original edit was performed with the `Edit` tool's string-replacement anchored on the
text immediately *preceding* the intended insertion point (the end of the file's *visible* §10
prose), without first re-reading the file to confirm that prose was in fact the file's last content.
It was not — the relay error-code subsection came after it. The lesson generalises: "append" claims
made from an `old_string` anchor should be verified against `tail`/`wc -l` on the CURRENT file
immediately before the edit, not inferred from what was true when the section was last read.
