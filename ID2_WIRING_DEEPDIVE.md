# ID2_WIRING_DEEPDIVE — where the sequence resume floor comes from, and why the task as filed cannot be implemented yet

**Task:** ID-2-WIRING — "Derive the sequence resume floor from ALL prepares, never from committed
history" · Spec Server `public_id` `838677e6-d424-45ed-8580-924cb2da28a6` · project `agent-bus` ·
epic `ID` · P0 · status `in_progress`.

**Mode:** DESIGN INVESTIGATION ONLY. No production code was written or modified. Every experiment
below ran against a throwaway copy of the tree under
`/tmp/claude-1000/-mnt-sdb4-mike-mike-source-agent-bus/b828c013-a5a5-4da0-b21c-d56d21066f9e/scratchpad/repo`,
never against the tracked `data/` dir.

**Author:** deep-diver · **Date:** 2026-08-02 · **Toolchain:** `go1.19.4 linux/amd64`

**Bottom line in one sentence:** triage's premise is **CONFIRMED** — the sequence number lives in the
caller-written PREPARE *body*, no message-body schema exists, and nothing in production mints a
sequence at all — so ID-2-WIRING is **not implementable as written today** and is **not exploitable
today**; it becomes P0 the instant the first MSG write path lands, and the half of it that *is*
implementable now (the inert `RaiseFloor`) should be split out and shipped immediately.

---

## 0. Scope and what this investigation did NOT cover

Stated explicitly so the reader knows the bounds:

- I read `internal/ids/sequence.go` (all 254 lines), `internal/wal/log.go` (the `Entry`, payload,
  encode/decode and `Open` regions — not the writer/txn internals in full), `internal/wal/reader.go`
  (all 172 lines), `internal/wal/replay.go` (the scan callback and `Recovered`),
  `internal/wal/recover.go` (the `TailRepair` doc and `RepairTail` preamble — **not** the
  `truncatableTail`/`inspectTail` bodies, which are DUR-10/DUR-11's live territory),
  `internal/wal/format.go` (constants and header parse), `cmd/agent-bus/main.go` (the whole startup
  path), `internal/httpapi/server.go` (routes + `DurableLog`).
- I did **not** audit the WAL writer's fsync ordering. ID-2-WIRING assumes it; DUR-1/DUR-2/DUR-6
  own it.
- I did **not** review DUR-10/DUR-11's in-flight truncation work. `git status --porcelain` showed
  `M internal/wal/doc.go`, `M internal/wal/recover.go`, `M internal/wal/recover_test.go` and
  `?? internal/ids/agentid.go` staged/untracked by **other agents running concurrently**. None of
  those are mine; my scratch copy was taken at investigation start and may lag their edits. None of
  my findings touch `recover.go` behaviour.
- The backlog sweep covered the 137 tasks returned by
  `GET /api/v1/projects/agent-bus/tasks?limit=500`, filtered to epics `ID`, `DUR`, `MSG`. I did not
  read the CRYPTO/IDEM/SIGN epics' task bodies beyond the cross-references quoted in DUR-5 and MSG-5.

---

## 1. Premise verdict — CONFIRMED (with one correction to the wording)

### 1.1 CONFIRMED: the sequence is not a WAL-level field; it lives in the caller-written body

`internal/ids/sequence.go:27-40` states the contract in the allocator's own doc comment:

```
// # The allocator holds NO durable state of its own
//
// Sequence is memory only. It writes nothing, reads nothing, and fsyncs
// nothing. A number becomes durable because the CALLER writes it into a WAL
// PREPARE frame and fsyncs that frame before the send is acknowledged.
```

And the WAL says, at `internal/wal/log.go:73-74`, that it will not look:

```
// Entry is one application-level durable change. wal does not interpret Kind
// or Body; they are the application's business.
type Entry struct {
```

The on-disk PREPARE payload schema is `internal/wal/log.go:532-536`, and it has **no sequence
field**:

```go
type preparePayload struct {
	Kind string          `json:"kind"`
	TS   string          `json:"ts"`
	Body json.RawMessage `json:"body"`
}
```

So the sequence, wherever it ends up, is inside `Body` — an opaque `json.RawMessage`. **Triage is
right.**

### 1.2 CONFIRMED, but triage's grep count is wrong — the substance holds

Triage reported: *"`grep -rn '"message"' --include=*.go internal/ cmd/` matches exactly ONE line, a
comment at `internal/wal/log.go:75`."*

That command actually matches **~90 lines**, almost all of them in `internal/wal/*_test.go`. The
accurate claim, which I ran first-hand:

```
$ grep -rn '"message"' --include='*.go' internal/ cmd/ | grep -v '_test\.go'
internal/wal/log.go:75:	// Kind is the application discriminator: "message", "agent", ... It must
```

**Exactly one NON-test line, and it is a comment.** The test files use `Kind: "message"` with ad-hoc
bodies (`{"n":1}`, `{"to":"bus-1.agent-a","seq":1}`, `{"seq":6}` — see
`internal/wal/replay_test.go:799-803`, `internal/wal/crash_injection_test.go:64-67`) purely as WAL
fixtures. Those are **not a schema**: three different shapes appear across three test files and
nothing reads them back as a typed struct.

**Verdict: triage's conclusion is correct — no durable message-entry body schema exists.** The
miscount does not change anything; if anything the test fixtures' inconsistency (`{"n":N}` vs
`{"seq":N}` vs `{"to":…,"seq":N}`) *reinforces* that no schema has been agreed.

### 1.3 A third confirmation triage did not state: nothing mints a sequence at all

```
$ grep -rn 'NewSequence(\|ids\.Resume(\|RaiseFloor(' --include='*.go' internal/ cmd/ | grep -v '_test\.go'
internal/ids/sequence.go:152:func NewSequence() *Sequence { return &Sequence{} }
internal/ids/sequence.go:234:// ... a bare `s.RaiseFloor(x)` is
internal/ids/sequence.go:242:func (s *Sequence) RaiseFloor(atLeast uint64) error {
```

Only the definitions and a doc comment. **No production code constructs a `Sequence`.** The only
`internal/ids` symbols wired into `cmd/agent-bus/main.go` are `ids.ValidateBusID` (`main.go:145`),
`ids.LoadOrCreateBusID` (`main.go:193`) and `ids.BusIdentity` (`main.go:295`) — all bus-id, none
sequence.

**Conclusion on the premise: CONFIRMED on both counts.** Implementing ID-2-WIRING as written
requires either (a) inventing the message-entry body schema, or (b) changing the on-disk PREPARE
payload. Both are decisions the backlog does not settle. This was correctly routed to design.

---

## 2. The exploitable window — NOT reachable today; P0 the moment MSG lands

This is the difference between "drop everything" and "get the ordering right", so it is worth being
exact.

### 2.1 Three independent reasons the hazard is unreachable in `main` today

1. **No `Sequence` is ever constructed in production** (§1.3). There is no floor to get wrong.
2. **No handler writes to the WAL.** Only two routes are registered
   (`internal/httpapi/server.go:131-132`):
   ```go
   mux.HandleFunc("/healthz", s.handleHealthz)
   mux.HandleFunc("/v1/info", s.handleInfo)
   ```
   `Options.Durable` is held for the process lifetime and exposed via `Server.Durable()`
   (`server.go:149-152`), but **no handler calls `Write`**. `main.go` even says so at the `wal.Open`
   call site: *"The Applier is deliberately nil … there is no in-memory serving copy yet"*
   (`cmd/agent-bus/main.go:225-231`).
3. **No production code ever writes an `Entry` with `Kind: "message"`** (§1.2).

So a `bus.wal` written by today's `main` contains **zero message records** and therefore zero burned
sequence numbers. There is nothing on disk for a wrong floor to collide with.

### 2.2 But the hazard is REAL and I reproduced it end-to-end

Throwaway repro in the scratch copy, using **only exported API** — `wal.Open`, `wal.Begin` (prepare
without commit), `wal.Replay`, `wal.ScanAll`, `wal.DecodePrepare`, `ids.Resume`, `ids.MessageID` —
and an *invented* `msgBody{Seq uint64 \`json:"seq"\`}` (inventing it is precisely the blocked
decision). 99 committed messages, then seq 100 prepared+fsynced with no commit:

```
$ go test -race -run TestDanglingPrepareReissuesAnID -v ./internal/demo
=== RUN   TestDanglingPrepareReissuesAnID
wal=/tmp/TestDanglingPrepareReissuesAnID3588997911/001/bus.wal size=15503
floor from COMMITTED history = 99
floor from ALL PREPARES      = 100
Resume(99).Next() = 100 -> "bus-1-100"
Resume(100).Next() = 101 -> "bus-1-101"
    demo_test.go:127: CONFIRMED: committed-history floor reissues burned sequence 100 as "bus-1-100"
--- PASS: TestDanglingPrepareReissuesAnID (0.28s)
```

That is the task description's break, reproduced exactly: the obvious wiring reissues `bus-1-100`,
which invariant 1 forbids outright.

**Two useful facts fall out of this repro:**

- `wal.ScanAll` (`internal/wal/reader.go:20`) + `wal.DecodePrepare` (`internal/wal/log.go:599`)
  **already do everything the correct derivation needs.** They enumerate every PREPARE regardless of
  how it resolved, and decode it. No new WAL capability is required for the *scan*.
- The **only** missing ingredient is agreement on where `seq` lives inside `Body`. I had to make one
  up to run the test.

### 2.3 Verdict on urgency

| | |
|---|---|
| **P0 now?** | **No.** Zero live exposure, zero data at risk, no code path reaches it. |
| **P0 the moment MSG lands?** | **Yes** — specifically the first task that writes an `Entry{Kind:"message"}` and mints an id, i.e. **MSG-2** (`50995c75-…`, "assigns a message id via the sequence allocator") or **MSG-3** (`2655c6ae-…`), both currently `todo`/P1. |

So ID-2-WIRING is an **ordering constraint, not an incident**. It should not block a deploy today.
It must block MSG-2/MSG-3.

**Latent landmine that makes the ordering fragile:** the wrong wiring is the *easy* wiring. A
`wal.Replay(path, func(c wal.Committed) error {…})` fold is the natural thing an implementer writes,
it compiles, it is `go vet`-clean, and it is silently wrong. Nothing in the current API pushes back —
which is exactly what §4 fixes.

---

## 3. Root cause(s)

### 3.1 CONFIRMED root cause of "cannot implement as written"

**The unit of durable identity (the message sequence) is stored in a layer that has not been
designed yet.** `internal/ids` says the caller owns it; `internal/wal` says the body is the
application's business; there is no application. The task is not wrong — it is **ordered before its
prerequisite**, and the prerequisite (the message-entry body schema) is unowned in the backlog.

*Disproof test:* find any non-test Go file that writes or reads a typed message body.
`grep -rn '"message"' --include='*.go' internal/ cmd/ | grep -v _test` returning more than the single
comment line at `log.go:75` would disprove it. It returns exactly that one line.

### 3.2 CONFIRMED root cause of the id-reissue hazard itself

`wal.Replay`'s callback receives **committed entries only** — `r.Applied++` and the `fn(…)` call sit
in the `case TypeCommit:` arm (`internal/wal/replay.go:161-175`) — while a dangling PREPARE is
durable and its number is burned forever. `wal.Recovered` (`replay.go:264-319`) exposes
`NextIndex`, `Applied`, `Aborted`, `Dangling []uint64` — and `NextIndex` is explicitly the **WAL
record index**, not a message sequence (`replay.go:268-272`, and `sequence.go:124-128` warns the two
"are not interchangeable"). `Dangling` gives you the *indices* of discarded prepares but not their
sequences.

*Disproof test:* the repro in §2.2. Reproduced.

### 3.3 CONFIRMED: `RaiseFloor`'s guard really is inert at startup

`internal/ids/sequence.go:246`:

```go
if s.last != 0 && atLeast <= s.last {
```

`s.last` is 0 until the first `Next` (`sequence.go:142-146`, `183-191`). So during floor assembly —
the only window in which the floor is chosen — **every value is accepted**, including a far-too-low
one. Reproduced:

```
$ go test -race -run TestRaiseFloorIsInertAtStartup -v ./internal/ids-scratch
RaiseFloor(1)=nil RaiseFloor(0)=nil at startup; first Next()=100
after Next(), RaiseFloor(1) err = true
--- PASS
```

`ids.Resume(99).RaiseFloor(1)` and even `.RaiseFloor(0)` both return `nil`. The doc comment at
`sequence.go:227-232` already admits this in full: *"during the window where the floor is actually
derived — startup, `Last() == 0` — every value is accepted, including one far too low … it is NOT a
defence against a wrong initial floor, and nothing in this package is."*

### 3.4 CONFIRMED, and worse than filed: `go vet` **cannot** be made to catch a bare `RaiseFloor`

The task says a bare `s.RaiseFloor(x)` is `go vet`-clean. I checked whether a vet-based gate could be
the cheap fix. **It cannot.** Scratch probe:

```go
package vetprobe
func Wire() {
	s := ids.Resume(99)
	s.RaiseFloor(100) // the bare call the doc comment warns about
}
```

```
$ go vet ./internal/vetprobe                                              -> exit=0  (clean)
$ go vet -unusedresult.funcs='(*…/internal/ids.Sequence).RaiseFloor' ./…  -> exit=0  (still clean)
$ which errcheck staticcheck                                              -> exit=1  (neither installed)
```

The `unusedresult` analyzer does not flag discarded `error` results, and its `funcs` flag does not
change that. Adding `errcheck`/`staticcheck` would be a third-party dependency requiring a
`DECISIONS.md` justification (invariant 8) **and** a CI story this repo does not have.

**Therefore the mitigation cannot be a linter. It must be an API that fails closed.** See §4.1.

### 3.5 Ranked CANDIDATES for "where should the sequence live on disk" (this is the open question)

Not a bug — a design fork. Ranked by my recommendation, each with its disproof test.

---

## 4. The fix

Two clearly separable pieces. The first ships **today**; the second needs one user decision.

### 4.1 Half A — the `RaiseFloor`-is-inert half: SEPARABLE, implementable TODAY, `internal/ids` only

**Yes, it is separable, and yes it is implementable now with zero schema decision.** But not as
"treat a non-nil `RaiseFloor` as fatal at the wiring" — there *is* no wiring, and §3.4 shows nothing
can force a caller to check. The correct smallest change is to make the *allocator* fail closed:

- add `ErrFloorUnproven` and `ErrFloorSealed`;
- add an unexported `sealed bool` to `Sequence`;
- add `func (s *Sequence) Seal() error` — ends floor assembly; sealing twice is an error;
- `Next()` returns `(0, ErrFloorUnproven)` while `!sealed`;
- `RaiseFloor` returns `ErrFloorSealed` after `Seal()`;
- **both** `NewSequence()` and `Resume()` are born **unsealed**, because `sequence.go:201-206`
  legitimately allows the floor to be assembled from several sources before the first `Next`.

Why this is the right shape: it converts "silently guessing low" into "refuses to issue", which is
verbatim what `sequence.go:119-122` demands — *"A caller that cannot PROVE its floor is greater than
or equal to every sequence number ever written MUST refuse to start rather than guess."* And it makes
the floor claim a **visible, greppable, reviewable line** (`seq.Seal()`) at exactly the point where a
reviewer must check the derivation. An implementer can no longer ship the naive
`Resume(committedMax)` wiring without writing that line.

**Prototyped and proven in the scratch copy.** `go build ./...` clean, `gofmt` clean, `go vet` clean:

```
$ bash scripts/proof-check.sh 'go test -race -run TestSequenceRefusesToIssueFromAnUnsealedFloor ./internal/ids'
proof-check: PASS — 5 test(s) ran (1 top-level), 1 passed, 0 skipped.
proof-check: verdict=PASS class=test exit=0 tests_run=5 top_level=1 skipped=0 failed=0 empty_pkgs=0
```

Covering: `Resume`-then-`Next` refused · `NewSequence`-then-`Next` refused · `Seal`-then-`Next`
returns 100 · pre-seal `RaiseFloor(150)` holds while post-seal `RaiseFloor(200)` is refused and
`Next()` returns 151.

**Measured cost:** 5 existing top-level tests in `internal/ids/sequence_test.go` go red until each
adds a `Seal()` (`TestSequenceAllocator`, `…ConcurrentNext`, `…Overflow`, `…RaiseFloor`,
`…RaiseFloorRace`; 24 constructor call sites in that file). That is mechanical and is the honest
price of a fail-closed API. **It is not scope creep — a fail-closed allocator whose own tests never
exercise the closed state would be theatre.**

**Landmine this uncovers:** `ids.Resume(0)` currently means "nothing on disk" and is documented as
"exactly equivalent to `NewSequence`" (`sequence.go:159-160`). Under the seal gate that stays true —
both are unsealed — but a caller who derives floor `0` from an *empty* scan and one who derives `0`
because the scan *failed* are indistinguishable at the type level. `Seal()` does not fix that; the
derivation code must return an error rather than 0 on scan failure. Call this out in the wiring task.

### 4.2 Half B — the floor-derivation half: BLOCKED on one decision

The building blocks are proven to work (§2.2). What is missing is *where `seq` lives*. Four options.

---

#### Option A — caller-side `ScanAll` + decode the body

The application scans the WAL itself and folds `body.seq` over every PREPARE.

- **Proven working** (§2.2): `floor from ALL PREPARES = 100`.
- **On-disk format change:** none. **Migration:** none. **Downgrade risk:** none.
- **Cost:** a **third full scan** of the WAL at startup, plus a JSON decode of every prepare body.
  This directly aggravates the already-filed P2 `2a961fcc` — *"Startup scans the WAL twice (soon
  three times) — bound the cost"*. That task's "(soon three times)" appears to have anticipated
  exactly this.
- **Verdict:** works, but it is the most expensive way to get the answer.

---

#### Option A′ — RECOMMENDED — offer every PREPARE to an observer during the replay pass that already happens

The decisive finding: **`Replay` already calls `DecodePrepare` on every single PREPARE record**,
committed, aborted and dangling alike, at `internal/wal/replay.go:129-131`:

```go
case TypePrepare:
	// Decoded eagerly, even though the entry may never commit: a
	// prepare payload that does not decode means the file no longer
	// says what it recorded …
	e, _, err := DecodePrepare(path, rec)
```

The decoded `Entry` is right there and is then thrown away unless the entry later commits. So the
fix is to hand it to the application:

```go
func Replay(path string, fn func(Committed) error) (Recovered, error) {
	return ReplayWithPrepares(path, fn, nil)
}

// ReplayWithPrepares is Replay plus onPrepare, called once for every PREPARE
// record in file order regardless of how it later resolves.
func ReplayWithPrepares(path string, fn func(Committed) error, onPrepare func(Entry) error) (Recovered, error) {
```

- **Prototyped in scratch: 16 added lines in `internal/wal/replay.go`.**
- **`./internal/wal` stays fully green** — `Replay` just delegates:
  `$ go test ./internal/wal` → `ok  github.com/dodgymike/agent-bus/internal/wal  3.639s`
- **Proven to yield the right floor in ONE pass, from the dangling prepare:**
  ```
  $ bash scripts/proof-check.sh 'go test -race -run TestFloorFromPrepareObserverInOnePass ./internal/demo'
  one-pass floor = 100 (records=100 applied=99 dangling=1)
  proof-check: PASS — 1 test(s) ran (1 top-level), 1 passed, 0 skipped.
  proof-check: verdict=PASS class=test exit=0 tests_run=1 top_level=1 skipped=0 failed=0 empty_pkgs=0
  ```
- **On-disk format change:** none. **Migration:** none. **Downgrade risk:** none. **Extra scans:**
  zero.
- **Layering:** preserved. `wal` still does not interpret `Body` — the **application** does, in its
  own callback. `log.go:73`'s promise survives intact.
- **Still requires** the schema commitment: `seq` must be a readable top-level field of the message
  body.
- *Disproof test:* if `Replay` did **not** see every prepare, this would not work. It does — quoted
  above at `replay.go:129`, and the repro proves the dangling one is observed.

---

#### Option B — carry `seq` in the WAL PREPARE header (`preparePayload`)

Add `Seq uint64 \`json:"seq,omitempty"\`` to `internal/wal/log.go:532-536`, populate it from a new
`Entry.Seq`, and expose `Recovered.HighestSequence`.

**Measured in scratch — the code cost is tiny, the format cost is not.**

- Code: `go build ./...` clean; **exactly one existing test breaks** —
  `TestWALReplayRejectsMalformedPreparePayload/an_unknown_field`, because
  `internal/wal/replay_test.go:1109` uses literally `"seq":7` as its example unknown field:
  ```go
  {"an unknown field", `{"kind":"message","ts":"` + ts + `","body":null,"seq":7}`, "does not decode"},
  ```
  It needs a different field name. That is a one-line fix.
- **On-disk format break — PROVEN.** `internal/wal/log.go:671` calls `dec.DisallowUnknownFields()`.
  I wrote a record with the seq-bearing encoder, then reverted `log.go` to the shipped version and
  replayed the same file:
  ```
  BYTES: …{"kind":"message","ts":"2026-08-02T16:00:25.751363669Z","body":{"to":"bus-1.a"},"seq":100}…
  REPLAY err=wal: …/bus.wal: corrupt at offset 16: record 1: prepare payload does not decode:
                  json: unknown field "seq"   records=1 applied=0
  ```
  An older binary meeting a newer log **refuses to start**. That is fail-*safe* (good — and
  `RepairTail` will not eat it, because it is a framing-only pass and a complete frame with a
  semantic failure is fatal where it sits, `recover.go:110-117`), but it is a hard downgrade break.
- **The version field would LIE.** The file header still carries `FormatVersion = 1`
  (`internal/wal/format.go:19`, written at `format.go:307`), so the operator gets `corrupt at offset
  16` instead of `format version 2, want 1` (`format.go:328-329`). **Option B therefore requires a
  `FormatVersion` bump**, and `format.go:14-19` says that number is *"reserved through the Spec
  Server `ondisk-format-version` namespace"*. I verified the namespace exists and currently holds
  exactly one value:
  ```
  {"namespace":"ondisk-format-version","reserved_by":"feature-runner","value":1,"created_at":"2026-08-02T10:01:54.997434+00:00"}
  ```
  **An implementer must NOT pick `2`.** Reserve it. (This is the LOC-10/FLEET-9 collision class.)
- And a version bump makes **every existing WAL unreadable** (`format.go:328-329` refuses a mismatch
  outright), so it needs either a migration path or an explicit recorded decision that dev data dirs
  are disposable.
- **Upside, and it is a real one:** the floor stops being derivable-wrongly at all. `wal` computes
  `Recovered.HighestSequence` in its own pass and no caller can get it wrong. It also **survives
  body encryption** — see §4.3.
- **Downside:** `wal` acquires knowledge of a message sequence, which is a deliberate layering
  change needing a `DECISIONS.md` entry.

---

#### Option C — sequencing: make ID-2-WIRING an acceptance criterion of the first MSG task that writes a message

Not a mechanism, a schedule. Zero cost now; the hazard is unreachable until MSG-2/MSG-3 (§2).

- **Risk:** the criterion gets lost, and the MSG-2 implementer writes the natural, wrong,
  `go vet`-clean `Replay`-fold.
- **Mitigation, and this is why §4.1 matters:** land the **seal gate now**. Then MSG-2 *physically
  cannot* mint an id without writing `seq.Seal()`, and the reviewer's eye lands on the derivation.
  Option C without the seal gate is a promise; with it, it is a mechanism.

---

### 4.3 RECOMMENDATION

**Ship A′ + C, with the seal gate (§4.1) landing first and independently.**

Concretely:

1. **Now, no decision needed:** the seal gate in `internal/ids`. Fail-closed `Next()`.
2. **Now, decision needed (see §4.4):** record in `DECISIONS.md` that the message sequence is a
   top-level cleartext field of the WAL message body — `{"seq": <uint64, non-zero>, …}` — and that
   this is the ONLY field ID-2-WIRING commits the MSG epic to. Everything else about the message body
   (sender, recipients, timestamp, content hash, idempotency key, ciphertext envelope) stays open.
3. **Before MSG-2:** add `ReplayWithPrepares` (16 lines, `./internal/wal` stays green) and the
   startup derivation + `Seal()` in `main.go`, refusing to start if the scan errored.
4. **With MSG-2/MSG-3:** they consume the sealed allocator. ID-4 proves it across a restart.

**Why A′ over B**, stated honestly: A′ costs 16 lines, no format change, no version reservation, no
migration, no downgrade break, and no extra scan — while getting *exactly* the same correct floor
(100, from the dangling prepare, proven). B is architecturally cleaner but buys a format break, a
reserved version number, a data-dir migration and a `DECISIONS.md` layering entry, in exchange for
moving a correctness obligation from a reviewed 10-line callback into the WAL. With the seal gate in
place, that obligation is already visible and enforced. **B is the right answer only under the
condition in §4.4.**

**Ranking:** A′ ≫ B > A ≫ (any of them without the seal gate).

### 4.4 THE ONE DECISION THE USER MUST MAKE

> **Will the bus ever be unable to read `seq` out of a WAL message body?**

- **If NO** — the WAL message body is always a *cleartext JSON envelope* with any ciphertext confined
  to one field — then **A′ is correct and B is over-engineering.** This is the shape DUR-5 already
  implies: its audit record is deliberately *"METADATA AND ROUTING INFO ONLY — message id, sequence,
  sender, recipient(s), bus path, timestamp, size, and a content hash of the body — and never the
  message body itself"*, with a forward-compat requirement to *"reserve/permit additional optional
  fields in the JSON payload"*. That is precisely a cleartext-metadata envelope.
- **If YES** — the CRYPTO epic will make the whole WAL body opaque to the server (it is described as
  *"Signal-style end-to-end encryption with forward secrecy"*) — then **A′ breaks the day CRYPTO
  lands**, the floor derivation has to move into the header anyway, and doing A′ first buys a
  **second** on-disk format break. In that world **do B now, while the project has no production data
  dirs and the migration is free.**

*Disproof test for the recommendation:* if a CRYPTO task specifies that `wal.Entry.Body` for
`Kind=="message"` is a bare opaque blob rather than an envelope, A′ is disproven and B wins. I did
**not** read the CRYPTO epic task bodies (see §0) — that is a bounded gap and the fastest way to
settle this decision.

---

## 5. SPEC-ready task breakdown

Atomic, ordered, each with a proof command whose `proof-check.sh` verdict is quoted. **I did not
create these tasks** — a spec-keeper agent files them.

**On keys:** do **not** hand-pick `ID-<N>`. Either reserve from the `task-key-ID` namespace
(`POST /api/v1/projects/agent-bus/reservations {"namespace":"task-key-ID","reserved_by":"<you>"}` —
it currently holds 1..4, so a fresh reservation is safe) or, preferred for these, use a descriptive
title + the server-assigned `public_id`, as ID-2-WIRING itself does. Derived keys below
(`ID-2-WIRING-SEAL`, `-SCHEMA`, `-OBSERVER`, `-STARTUP`) are unique by suffix and need no reservation.

---

### T1 — `ID-2-WIRING-SEAL`: Sequence refuses to issue from an unsealed floor · **P0** · no dependencies

Add `Seal()`, `ErrFloorUnproven`, `ErrFloorSealed` to `internal/ids/sequence.go` per §4.1. `Next()`
returns `(0, ErrFloorUnproven)` until sealed; `RaiseFloor` returns `ErrFloorSealed` after. Both
constructors born unsealed. Update `sequence.go`'s doc comment (§"When it may be called") and the 5
existing tests. Update `CONTRACTS.md`.

- **Proof:** `go test -race -run TestSequenceRefusesToIssueFromAnUnsealedFloor ./internal/ids`
- **Verdict (validated against the scratch prototype):** `proof-check: verdict=PASS class=test
  exit=0 tests_run=5 top_level=1 skipped=0 failed=0 empty_pkgs=0`
- **Also required green:** `go test -race ./internal/ids` (all 5 pre-existing top-level tests, after
  adding `Seal()`).
- **Note:** this is the ONLY task here that can start immediately.

---

### T2 — `ID-2-WIRING-SCHEMA`: Decide and record where the message sequence lives on disk · **P0** · docs only, no code

Answer §4.4 and write it into `DECISIONS.md` (dated section, append — the file is contended). Record
the chosen option, the rejected ones, and the §4.4 disproof test. If **B** is chosen, this task also
reserves the `ondisk-format-version` value and records it.

- **Proof:** `grep -q 'message sequence high-water mark' DECISIONS.md`
- **Verdict today:** `proof-check: verdict=FAIL class=file-assertion exit=1` — correct and
  non-vacuous: it fails now precisely because the decision is unrecorded, and flips to PASS when it
  is. (`class=file-assertion` is judged purely on exit status, per `proof-check.sh:29-31`.)
- **Blocks:** T3, T4.

---

### T3 — `ID-2-WIRING-OBSERVER`: `wal` offers every PREPARE to an observer during the existing replay pass · **P0** · depends on T2 choosing A′

Add `ReplayWithPrepares(path, fn, onPrepare)`; `Replay` delegates with `nil`. `onPrepare` fires for
every PREPARE in file order — committed, aborted and dangling — before resolution. `wal` still does
not interpret `Body`. Update `CONTRACTS.md` and `PROTOCOL.md`.

- **Proof:** `go test -race -run TestWALReplayObservesEveryPrepare ./internal/wal`
- **Verdict today:** `proof-check: verdict=VACUOUS class=test exit=0 tests_run=0 empty_pkgs=1` —
  vacuous **by construction**, the test does not exist yet. The equivalent scratch test
  (`TestFloorFromPrepareObserverInOnePass`) is proven **PASS**, so the command is executable and
  non-vacuous once written.
- **Required assertion:** the observer must see the **dangling** prepare's entry (that is the whole
  point) — assert a floor of 100 from a log whose only seq-100 record never committed.
- **Also required green:** `go test -race ./internal/wal` (proven `ok` with the prototype).
- **If T2 chooses B instead:** replace this task with `ID-2-WIRING-HEADER` — add `Entry.Seq` +
  `preparePayload.Seq`, expose `Recovered.HighestSequence`, **reserve** the `ondisk-format-version`
  value (never pick it), bump `FormatVersion`, fix `replay_test.go:1109`'s unknown-field fixture, and
  ship a downgrade note. Proof:
  `go test -race -run 'TestWALRecoveredHighestSequence|TestWALFormatVersionRefusal' ./internal/wal`.

---

### T4 — `ID-2-WIRING-STARTUP`: derive, prove and seal the sequence floor in `main` · **P0** · depends on T1 + T3

In `cmd/agent-bus/main.go`, after `wal.Open` (`main.go:234`), fold the observer over every prepare,
construct `ids.Resume(floor)`, `RaiseFloor` from any other source, then `Seal()` — and **return a
non-nil error from `run()` on any failure**, including: the scan errored, a message prepare's body
had no `seq` / a zero `seq`, `RaiseFloor` returned non-nil, or `Seal()` returned non-nil. Log the
derived floor at INFO alongside the existing `"write-ahead log opened"` line (`main.go:272-284`).

- **Proof:** `go test -race -run TestRunRefusesAnUnprovableSequenceFloor ./cmd/agent-bus`
- **Verdict today:** VACUOUS by construction (test not written). Model it on the existing
  `cmd/agent-bus/wal_startup_test.go`.
- **Must cover the §4.1 landmine:** a scan that *fails* must not be indistinguishable from an *empty*
  log — floor `0` from a failed derivation must refuse to start, not resume as a fresh bus.

---

### T5 — `ID-4-FU-DANGLING`: extend ID-4's id-counter recovery property test with a dangling-prepare kill point · **P1** · depends on T4

ID-4 (`72c97d23-…`, todo, P1) is *"kill the process, restart, assert every counter resumes strictly
above its last-issued value — table-driven across several kill points"*. Add the kill point that
matters: **after the PREPARE fsync, before the COMMIT fsync**, and assert the restarted bus never
mints the burned sequence.

- **Proof:** `go test -race -run TestIDCounterRecoveryAcrossDanglingPrepare ./cmd/agent-bus`
- **Verdict today:** VACUOUS by construction. The scratch `TestDanglingPrepareReissuesAnID` proves
  the scenario is constructible and the assertion meaningful (**PASS**).

---

### T6 — `MSG-2-AC-SEQFLOOR` / `MSG-3-AC-SEQFLOOR`: add the floor as an explicit acceptance criterion · **P1** · notes-only

Post a `kind=request` note on MSG-2 (`50995c75-a565-4c1a-b0a0-6d49e66d30c4`) and MSG-3
(`2655c6ae-bb07-4d9e-97e9-5cf55793c1c4`) stating: the sequence allocator MUST be obtained from the
sealed startup allocator (T4); an implementer MUST NOT construct `ids.Resume(...)` from a
`wal.Replay` committed fold; MSG-2/MSG-3 cannot be completed while `internal/ids` `Next()` is reachable
from an unsealed allocator.

- **Proof:** `bash scripts/spec-cloud.sh -sf /api/v1/projects/agent-bus/tasks/50995c75-a565-4c1a-b0a0-6d49e66d30c4 | grep -q AC-SEQFLOOR`
- **Verdict today:** FAIL (`class=file-assertion`, exit 1) — correct and non-vacuous; flips to PASS
  once the note is posted.

---

### T7 — `2a961fcc` cross-reference (existing task, P2)

*"Startup scans the WAL twice (soon three times) — bound the cost."* **Option A′ removes the third
scan before it is ever added.** Post a note linking this deep-dive so whoever picks up `2a961fcc`
knows the third scan is avoidable by design, not merely optimisable.

- **Proof:** `bash scripts/spec-cloud.sh -sf /api/v1/projects/agent-bus/tasks/2a961fcc… | grep -q ID2_WIRING_DEEPDIVE`

---

## 6. Cost / risk / rollback

| | Cost | Risk | Rollback |
|---|---|---|---|
| **T1 seal gate** | ~40 LOC in `sequence.go` + `Seal()` added at 24 call sites in `sequence_test.go` | **Low.** No caller exists in production (§1.3), so nothing can break at runtime. The blast radius is entirely inside `internal/ids` and its tests. | Trivial — revert one commit; nothing depends on it yet. |
| **T3 observer (A′)** | 16 LOC in `replay.go` + a test | **Low.** `Replay` delegates, so `./internal/wal` stays green (verified `ok`). No on-disk change, so no data is at stake. | Trivial — revert; existing logs unaffected. |
| **T3-alt header (B)** | ~30 LOC + `FormatVersion` bump + reservation + `DECISIONS.md` + `CONTRACTS.md` | **Medium-high.** Every existing WAL becomes unreadable (`format.go:328-329`). Proven downgrade break: `json: unknown field "seq"`. Needs a data-dir migration or an accepted wipe. | **Not free.** Rolling back a format bump strands any WAL written under v2. Rollback = wipe the data dir, which is only acceptable pre-production. |
| **T4 startup wiring** | ~30 LOC in `main.go` + a crash-injection test | **Medium.** Adds a new way for the server to refuse to start. That is the *intended* behaviour (`sequence.go:119-122`), but a derivation bug becomes a boot outage. | Revert the wiring; the bus returns to today's state (no ids minted). |
| **Doing nothing** | 0 | **The one that actually costs.** MSG-2 lands with the natural `Replay`-fold and silently reissues ids at the first crash in the prepare→commit window, corrupting the append-only audit trail permanently. Invariant 1 + invariant 10 both break, and an attacker who can induce crashes in that window chooses what lands on the reissued id. | **None — id reuse is not rollbackable.** The audit trail is append-only; two messages under one id stay there forever. |

**Risk note specific to the seal gate:** it makes an allocator that is never sealed a hard failure at
first `Next()` rather than at startup. Prefer sealing during startup (T4) so the failure is a boot
error, not a failed first send.

**Non-risk, worth stating so nobody re-litigates it:** `wal.RepairTail` is **not** part of this
hazard. `internal/wal/recover.go:42-83` argues it at length — it cuts only a frame whose `Append`
never returned, so nothing inside it was ever acknowledged and no number it carried was ever
observed. The dangerous record is the **dangling PREPARE**, which *did* fsync, survives every repair,
and stays burned forever (`recover.go:79-83`). The task description says the same. I confirm it.

---

## 7. Artifacts and reproducibility

Scratch tree (throwaway, safe to delete):
`/tmp/claude-1000/-mnt-sdb4-mike-mike-source-agent-bus/b828c013-a5a5-4da0-b21c-d56d21066f9e/scratchpad/repo`

Commands run against it, all green:

```
go build ./...                                                                  # BUILD_OK
go test -race -run TestDanglingPrepareReissuesAnID       ./internal/demo        # PASS
go test -race -run TestRaiseFloorIsInertAtStartup        ./internal/demo        # PASS
go test -race -run TestFloorFromPrepareObserverInOnePass ./internal/demo        # PASS
go test -race -run TestSequenceRefusesToIssueFromAnUnsealedFloor ./internal/ids # PASS
go test ./internal/wal                                                          # ok (with the observer hook)
"$(go env GOROOT)/bin/gofmt" -l internal/ids/                                   # empty
go vet ./internal/ids                                                           # VET_OK
```

Note on tooling: bare `gofmt` is **not on PATH on this box** — `test -z "$(gofmt -l .)"` false-passes
by exiting 127. Use `go fmt ./...` or `"$(go env GOROOT)/bin/gofmt" -l .`.

Note on `proof-check.sh`: it runs the proof from `$(dirname $0)/..` (`proof-check.sh:156-157`,
`506-510`), so it must sit at `<repo>/scripts/proof-check.sh` — placing it elsewhere makes every Go
proof report `FAIL — go.mod file not found`. I hit this and mention it only so the next reader does
not misread such a verdict as a real failure.
