# Contracts: on-disk formats and record types

Split out of `CONTRACTS.md` (2026-08-02) — see that file for the index of all contract planes and
the rest of the surface (CLI/env, HTTP, agent-facing). This is a pure content move: everything below
this header is unchanged from the prior single-file `CONTRACTS.md`, verbatim.

**This is the plane most in flux** (DUR / on-disk format version 2 work is active) — that volatility
is the reason it got its own file rather than sharing one with a more stable plane.

## Record types / wire protocol versions

**Corrected 2026-08-03 — the "None yet" wording below was stale, not another wave's in-flight
prose; it described a durable store and WAL that have since been built and shipped.** As of this
correction, `internal/wal/format.go` reserves four WAL record types (`record-type` namespace) and
the on-disk format has been bumped once (`ondisk-format-version` namespace):

| namespace | reserved values | meaning |
| --- | --- | --- |
| `record-type` | `1`=`TypePrepare`, `2`=`TypeCommit`, `3`=`TypeAbort`, `4`=`TypeAuditMessage` | WAL frame types (`internal/wal/format.go`) |
| `ondisk-format-version` | `1` (legacy, read-only), `2` (current) | WAL file/frame layout (`internal/wal/format.go`'s `FormatVersion`); version 2 replaced the unkeyed CRC32C of version 1 with a keyed HMAC-SHA256 (DUR-12) |

Both tables are confirmed against the Spec Server reservations for this project (`GET
/api/v1/projects/agent-bus/reservations?namespace=record-type` and `...=ondisk-format-version`) —
this is the live reservation ledger, not a number picked by eyeballing the list. **The rule below
still stands and is unchanged**: when a NEW record type or format version is needed, **reserve its
number via `POST /api/v1/projects/agent-bus/reservations`, never hand-pick it** — that is the
standing rule (`CLAUDE.md`, "Parallel-agent coordination") for record-type numbers, wire protocol
versions, and epic task keys alike, so two agents working in parallel can never collide on the
same number.

## On-disk files in the data directory (added 2026-08-02)

`<data-dir>/bus.lock` (mode `0o600`, inside the `0o700` `-data-dir`) — an exclusive advisory lock
(`syscall.Flock(LOCK_EX|LOCK_NB)`, `internal/dirlock`) taken by `cmd/agent-bus`'s `run()` immediately
after `os.MkdirAll(cfg.DataDir, 0o700)` and BEFORE `ids.LoadOrCreateBusID` or anything else reads or
writes the data dir — so a WAL replay always happens inside the lock. Held for the process's
lifetime, released on clean shutdown (`Lock.Release`, deferred) and by the KERNEL on any death
(SIGKILL, panic, OOM kill) — the flock lives on the open file description, not on the path. Named
`bus.lock`, deliberately **not** `*.log`: `wal.log` is the WAL and `.gitignore` ignores `*.log`, so a
`.log`-suffixed lock file would be one typo/glob away from being mistaken for log data. It is NOT
durable state and NOT a record store — replay never reads it. Its only contents are a single
`<pid>\n` line, written (and fsynced) only AFTER the lock is held, so a refusal can name a probable
holder.

**Operator-facing failure mode:** a second `agent-bus` on the same `-data-dir` fails FAST — never
blocks, never proceeds — with exit code `1` and:
```
agent-bus: locking the data directory: dirlock: data directory "<dir>" is locked by another agent-bus process (pid N, best-effort: read from <dir>/bus.lock after the lock failed, so it may be stale); refusing to start — two servers on one data directory destroy the write-ahead log
```
`pid N` is best-effort/advisory only — read from the lock file *after* our own flock failed, so the
named process may already be gone. Treat it as a hint for `ps`, never as proof of a live holder.

**Stale locks: there are none.** A crash leaves the lock FILE but no LOCK (the kernel drops the
flock when the process dies), so the next start acquires it normally and simply overwrites the pid
line. `Release`/the package deliberately NEVER unlinks `bus.lock` — unlinking would let a starter
lock a fresh inode at the same path while another process still holds the old one, i.e. two
holders on one data directory, the exact failure this file exists to prevent. Operators must never
manually delete `bus.lock`, and never need to when no server is running against that directory.

**Limits:** advisory only — it excludes other processes that `flock` the same file (in practice,
other `agent-bus` servers), not `rm`, `cp`, an editor, or a backup job. Unreliable on NFS before
Linux 2.6.12 and on some network filesystems; a data dir on such a mount gets NO protection from
this lock.

`.gitignore` already ignores the default `./data/` dir wholesale, so `bus.lock` there is never
committed; a data dir at a non-default, non-ignored path is the operator's own responsibility.

No new route, CLI flag, env var, or header was introduced by this change — see the sections above,
which remain the complete index.

## The write-ahead log at startup (added 2026-08-02)

`cmd/agent-bus`'s `run()` now opens `internal/wal` with
`wal.Open(wal.LogOptions{Dir: cfg.DataDir, Logger: lg})`, creating `<data-dir>/bus.wal` (mode
`0o600`, a 16-byte file header) on first start. This is wiring only: the on-disk WAL format itself
is unchanged (see `PROTOCOL.md`) — this task connects the already-existing library to the server
binary, it does not add a record type or bump a format version.

**Startup order, which is the contract:** `os.MkdirAll(-data-dir, 0o700)` → `dirlock.Acquire`
(`bus.lock`, see above) → `ids.LoadOrCreateBusID` (`bus-id`) → `wal.Open` (which REPLAYS the file
before returning) → `net.Listen` → serve. `wal.Open` must run after the lock, because replay reads
the file and a torn-tail repair truncates bytes a second server could otherwise be appending to —
opening the log before locking would defeat the lock entirely. It must run before the listener
binds, because `wal.Open` does not return until replay has finished, so no request is ever served
from an unreplayed store (invariant 5: disk is the truth, memory is only the serving copy). Read that
guarantee precisely: what is enforced (and asserted) is that nothing is ever **answered** before
replay — `srv.Serve` starts after `wal.Open` returns. Nothing promises the socket is unbound during
replay; a listener that is bound but not yet served answers nothing.

**Honest limit of what "replay" means right now:** the `Applier` passed to `wal.Open` is `nil`.
There is no in-memory serving copy yet — `internal/store` is still a stub — so there is nothing for
a committed entry to be applied to. Replay today is a durability fsck: it verifies every frame,
resolves each prepare against its commit or discards it, and establishes the next-index high-water
mark, but it rebuilds no application state, because none exists. When the store lands it is passed
here as the `Applier`, and this line changes with it.

The opened `*wal.Log` is held for the process lifetime and passed to the HTTP layer as
`httpapi.Options.Durable` (new field; interface `httpapi.DurableLog`, one method,
`Write(wal.Entry) (wal.Committed, error)`; accessor `func (s *Server) Durable() DurableLog`, which
may return `nil`). **No handler and no route reads it yet** — `/healthz` and `/v1/info` are
unaffected — it is wired through now so the epics that add writing handlers have exactly one write
path to reach for (invariant 4), rather than each minting its own.

On shutdown the log is `Close()`d via a `defer` registered *after* the lock's own deferred release,
so Go's LIFO ordering closes the WAL (flushing and releasing its file handle) while the data
directory is still locked, and only releases the lock afterward — the reverse order would open a
window where a second `agent-bus` could acquire the directory while this process still held the WAL
open. A `Close` error does not change the process exit code but is logged at `ERROR` with the
`data_dir`, `path`, and the error, since it is a durability signal an operator should see. A
SUCCESSFUL close logs, at `DEBUG`, `msg="write-ahead log closed" data_dir=<dir> path=<dir>/bus.wal`.
That line is not decoration and must not be deleted as noise: it is the only observable proof the
close ran at all (the kernel closes the descriptor at process exit, so `bus.wal` is byte-identical
either way), and the tests assert it appears BEFORE `msg="data directory lock released"`, which is
what pins the close-then-unlock order described above.

**Failure mode: any open-or-replay failure is FATAL.** `run()` returns a non-nil error, `main()`
prints it to stderr prefixed `agent-bus: ` and exits `1`, and nothing binds a listener — the same
"fail fast, never degrade to an empty store" shape as the `bus.lock` failure above. The message is:
```
agent-bus: opening the write-ahead log in "<data-dir>": <wal error>
```
where `<wal error>` is whatever `internal/wal` reports — for example a corrupt file header reads
`wal: <data-dir>/bus.wal: corrupt at offset 0: bad magic "XXXXXXXX", want "AGNTBUSW"` (the exact
wording is set by `internal/wal/format.go`'s `corruptf`, not by `cmd/agent-bus`). **Recovery ALWAYS reaches a running server** (decision of 2026-08-02, invariant 6).
Damaged records are repaired in place where possible, and otherwise QUARANTINED — the unusable log
is moved aside with its bytes preserved on disk, and the bus starts. A bad magic, a wrong format
version, a commit naming no open prepare, or a payload that will not decode no longer refuse to
start; they are discarded, loudly.

This supersedes the previous wording, which said those cases "refuse to start". The absolute
requirement that replaced it is that **every discard is logged** — a silent discard is the actual
defect (rated P0), not the discard itself, because a server quietly serving an empty bus after
eating a log is indistinguishable to an operator from one that had nothing to serve. Reaching the
fatal path above now means recovery could not complete at all (an unreadable file, a failed
quarantine), not merely that the log was damaged.

**New INFO log line, asserted on by tests — treat its shape as part of this contract.** After a
successful open, `run()` logs one line naming what recovery found:
```
msg="write-ahead log opened" data_dir=<dir> path=<dir>/bus.wal records_replayed=<n> applied=<n> aborted=<n> dangling=<n> next_index=<n> repaired=<bool> repaired_bytes=<n> quarantined=<bool> discard_count=<n> discarded_bytes=<n>
```
The last three fields are load-bearing, not decoration: without them a whole-log QUARANTINE prints
`repaired=false next_index=1`, which is byte-identical to a brand-new empty bus — an operator could
not tell "your log was eaten" from "you have not sent anything yet". That was a P0. Any change
that drops them reintroduces silent data loss at the outermost layer.

This fires even for a brand-new, empty log (all-zero fields, `next_index=1`), so its presence is
proof a replay ran before the process served anything. `wal.Open` itself additionally emits its own
`msg="wal replayed"` line (only when the file held ≥1 record) plus a `WARN` per discarded dangling
prepare (a prepare that was fsynced but never committed — the signature of a crash between the two
phases) — see `internal/wal/log.go`. Both log lines are internal library output, not routes or
headers, but an operator relying on this file to confirm "the WAL loaded" should look for the
`cmd/agent-bus` line above.

**Test-only env vars, not supported configuration:** `cmd/agent-bus/wal_startup_test.go` reads
`AGENT_BUS_TEST_RUN_SERVER`, `AGENT_BUS_TEST_DATA_DIR`, `AGENT_BUS_TEST_LISTEN`, and
`AGENT_BUS_TEST_LOG_LEVEL` in its own `TestMain`, to re-exec the test binary as a real server for a
startup/crash test. The server binary (`cmd/agent-bus/main.go`) does not read any of them. This
does not add an entry to the "Env vars" section above, which remains empty.

No new HTTP route, CLI flag, production env var, header, or on-disk record type was introduced by
this change.

## The durable applied-key store (IDEM-11, added 2026-08-03)

Invariant 10 requires that duplicate detection survive a restart — the applied-key memory has to be
part of RECOVERED state, not an in-memory cache that a crash empties. IDEM-11 is that store:
`internal/idem` (the retention policy, the record shape, and `Store`, the in-memory table it
recovers into) plus one additive field on the existing PREPARE payload (`internal/wal/log.go`) that
carries the record durably. Read `internal/idem/store.go`'s package doc for the honest statement of
the guarantee this buys; this section documents only the on-disk shape.

**NO new WAL record type and NO `ondisk-format-version` bump.** Nothing was reserved from either
namespace above, because IDEM-11 did not need a new frame — it needed the applied-key record to
commit in the SAME two-phase (prepare → commit → fsync) transaction as the effect it records. A
`wal.Entry` is exactly one transaction, so "same transaction" means "same PREPARE payload": adding
an optional JSON field to the existing payload keeps the record inside the one fsync that already
exists, where a second, separately-committed frame would reopen precisely the crash window
invariant 10 exists to close (a message durable, its applied-key record not, a client retry landing
in that gap).

**The PREPARE payload's shape (`internal/wal/log.go`'s `preparePayload`), updated:**

```
PREPARE  {"kind":"<Entry.Kind>","ts":"<RFC3339Nano>","body":<Entry.Body>,"idem":<opaque JSON, omitted when absent>}
```

`idem` is `Entry.Idem` — IDEM-11's applied-key record, opaque to `internal/wal` exactly as `body` is
(the package does not interpret either). It carries the Go struct tag `json:"idem,omitempty"`, so an
entry with `Entry.Idem == nil` **omits the field entirely** rather than writing `"idem":null` — a
PREPARE record for an operation with no applied-key record is BYTE-IDENTICAL to one written before
this field existed. That is proved, not merely asserted:
`internal/wal/idem_field_test.go`'s `TestPrepareWithoutIdemIsByteIdentical` encodes the same
`(kind, body, ts)` through the pre-IDEM-11 encoder (`encodePrepare`) and the current one
(`encodePrepareWithIdem(..., nil, ...)`) and fails if the bytes, or the presence of the literal
`"idem"` field, differ. Like `body`, the `idem` bytes are canonicalised (JSON-compacted, an explicit
`null` normalised to absent) with the same helper (`canonicalBody`), so a live write and a replayed
read see identical bytes for both fields.

**The applied-key record's own JSON shape** (`internal/idem/record.go`'s `recordJSON`, the value
that rides inside `idem`):

```
{"agent","enrol_bus_wide","op","key","fp","result","seq","committed_at"}
```

| field | Go type | on-disk encoding | omitted when |
| --- | --- | --- | --- |
| `agent` | `string` | fully-qualified `<bus-id>.<agent-id>` | empty (bus-wide enrolment record) |
| `enrol_bus_wide` | `bool` | — | `false` |
| `op` | `Operation` | fixed string (e.g. `"send"`, `"broadcast"`) | never |
| `key` | `string` | the client-supplied idempotency key, verbatim | never |
| `fp` | `string` | the payload fingerprint, **hex-encoded** (`encoding/hex`) | never |
| `result` | `json.RawMessage` | the minted result, verbatim, compacted | empty/absent result |
| `seq` | `uint64` | the message sequence, or 0 when the operation mints none | `0` |
| `committed_at` | `string` | `RFC3339Nano`, UTC | never |

`result` is capped at `MaxResultBytes = 512` bytes (`internal/idem/record.go`, `record.go`'s
`validate`/`Encode`); a result that would exceed it fails the operation with `ErrResultTooLarge`
BEFORE anything durable is written, not at replay time. `Record.Encode` validates before it
returns, for the same reason: a record that cannot be stored must fail the whole operation with
nothing written, never be discovered as broken only when replay tries to decode it.

**Backward compatibility: an existing log needs no migration.** A log written before this change
has no `idem` field in any PREPARE payload. `hub.Apply`'s `recoverIdemRecord`
(`internal/hub/hub.go`) checks `Entry.Idem` first and, when it is nil, falls back to rebuilding the
applied-key record from the message record's own `store.Message.IdempotencyKey` field (a durable
field of the message record since before IDEM-11, kept precisely so this fallback would be
possible) plus a recomputed fingerprint (`publishFingerprint`, the same function the live write
path uses). No applied key is lost across the upgrade; no operator migration step, no log rewrite.

**FORWARD/DOWNGRADE HAZARD — stated plainly, not softened.** `wal.decodePayload` decodes every
record with `encoding/json`'s `DisallowUnknownFields`. A binary built BEFORE this change, reading a
log written AFTER it, therefore treats every PREPARE record that carries an `idem` field as an
undecodable payload — a `CorruptError` — and recovery DISCARDS it (loudly logged, per the
`invariant 6` recovery contract above, but discarded all the same). That is an acknowledged write
LOST on downgrade, not a degraded-but-correct read. Downgrade of the server binary is **not a
supported operation** in this project (one binary, one container, forward-only) — this is not a
defect to be fixed by loosening the decoder, because a lenient decoder here is exactly how a file
that no longer says what history was accepted gets served as if it did. Operators: do not roll the
`agent-bus` binary back over a log written by a newer one.

**The retention window: `RetentionWindow = 50h10m22s`** (`internal/idem/retention.go`), derived term
by term rather than picked — read that file for the exact terms and the reasoning behind each; in
outline it sums a peer-outage budget (24h), the maximum session lifetime (1h, invariant 3), the
maximum parked long-poll ceiling (5m, `hub.MaxPollTimeout`), and the client transport's own retry
horizon (11s), then doubles the total for margin (`RetentionSafetyFactor = 2`). **`MaxEntries = 65536`**,
the quotient of a 64 MiB memory budget (`MaxRetainedBytes`) and a derived ~1 KiB
worst-case per-record footprint (`MaxRecordBytes`) — also worked out field-by-field in
`retention.go`, not picked. The table **fails closed**: at `MaxEntries`, `Store.Remember` returns
`ErrCapacity` and refuses the operation rather than evicting anything, because evicting a live key
turns its next legitimate retry into a second effect (`internal/idem/store.go`).

State the guarantee in exactly these terms, because the exact wording is the whole point:
**"duplicates are suppressed within the retention window"** — this is NOT unconditional
exactly-once. A retry whose key has aged out of the window is indistinguishable, on disk, from a
key that was never seen (idempotency keys are opaque client-supplied strings), so it is applied as
a NEW operation and produces a second effect. The throughput consequence, stated without softening:
`MaxEntries` records sustained continuously over `RetentionWindow` bounds accepted-mutating-op
throughput at roughly `65536 / 180622s ≈ 0.36 operations/second` — a bus sustaining more than that
reaches the cap and begins refusing operations with `ErrCapacity` until the oldest records age out
of the window.

**The DUR-7 (snapshot/compaction) constraint**, specified now even though DUR-7 is not yet
implemented (`internal/idem/store.go`'s package doc): a future snapshot/compaction pass MUST
capture each retained record's `committed_at` and MUST re-apply the SAME `now.Sub(committed_at) >
window` expiry predicate on load, because eviction here is a pure predicate over `committed_at` —
that is what keeps the in-memory table and the durable log from ever disagreeing about which keys
are live. A snapshot that stores only the keys (dropping `committed_at`) can never expire them at
all; a snapshot that resets `committed_at` to the snapshot time silently EXTENDS every key's life by
the age of the snapshot. Both break the retention window, in opposite directions, and neither is
detectable from the table's own contents afterwards — this has to be got right when DUR-7 is built,
not discovered by an operator watching a key that should have expired keep answering retries.

No new HTTP route, CLI flag, header, or env var was introduced by IDEM-11 — see the sections above,
which remain the complete index for those planes.
