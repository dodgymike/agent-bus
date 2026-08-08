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
| `ondisk-format-version` | `1` (legacy, read-only), `2` (current), `3` (`agent-suffixes`, `internal/ids/suffixstore.go`), `4` (`wal-index-floor`, `internal/wal/indexfloor.go`), `5` (`message-seq-floor`, `internal/hub/seqfloorfile.go`) | **The namespace covers every ON-DISK FILE FORMAT this project defines, not only the WAL frame layout.** `1`/`2` are the WAL file/frame layout (`internal/wal/format.go`'s `FormatVersion`); version 2 replaced the unkeyed CRC32C of version 1 with a keyed HMAC-SHA256 (DUR-12). `3` is the per-name agent-id suffix floor file's own format (see "The durable applied-key store" and neighbouring sections below). `4` is the durable WAL record-index floor file's format (see the 2026-08-07 subsection below). `5` is the durable MESSAGE-SEQUENCE floor file's format — a DIFFERENT counter from `4`, and the distinction is load-bearing (see the `message-seq-floor` subsection below) |

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

## On-disk files in the data directory: the durable WAL record-index floor (e120153b, db350e39, added 2026-08-07)

**Corrects two Spec Server defects, both P0, with one root cause.** Recovery previously derived the
index the next WAL append would use SOLELY from what survived in the log — one past the highest
SURVIVOR. That reissued ids two ways: a discarded tail record's index was handed straight back out
(`e120153b`), and a whole-log QUARANTINE reset the derivation to 1, reissuing the bus's entire
history of record indices and, through `internal/hub`'s `Recovered.NextIndex - 1` derivation, every
message id it had ever minted (`db350e39`). See `DECISIONS.md`, 2026-08-07, for the full defect
writeup and how invariants 1 and 6 were reconciled without narrowing either.

`<data-dir>/wal-index-floor` (mode `0600`, on-disk format version **4** — RESERVED via the Spec
Server `ondisk-format-version` namespace on 2026-08-07, never hand-picked; `1`/`2` are the WAL frame
format above, `3` is `agent-suffixes`) is a small, atomically-replaced file living OUTSIDE the WAL,
implemented in `internal/wal/indexfloor.go`. Format: a header line carrying an **HMAC-SHA256 tag**,
then a three-line body:

```
agent-bus-wal-index-floor v4 hmac-sha256=<64 hex>
reserved <decimal uint64>
written <decimal uint64>
sealed <0|1>
```

**The tag is a KEYED MAC, not a checksum (invariant 6), keyed with the data directory's own
`wal-mac.key` — the same key every WAL frame is authenticated under.** It covers
`agent-bus-wal-index-floor v4\n` followed by the body, i.e. the whole file except the tag field
itself; covering the version line binds the tag to the format version so a future body cannot be
replayed as a v4 one. Computed and verified with stdlib `crypto/hmac` + `crypto/sha256` exactly as
`internal/wal/format.go`'s frame MAC is, and compared with `hmac.Equal` — never `==` or
`bytes.Equal`, because a tag comparison that leaks timing is a forgery oracle.

*This replaced an UNKEYED SHA-256 on 2026-08-07 after a security gate proved the seal forgeable with
no key at all: flip `sealed 0` to `sealed 1`, recompute the digest by hand, and the reopened bus
reissued indices at 2268 of 2289 truncation offsets — while every frame it then wrote carried a valid
MAC, because the server itself computes it. See `DECISIONS.md`, 2026-08-07.*

**VERSION 4 ACCEPTS THREE SHAPES ON READ AND WRITES ONLY THE FIRST.** The version number is NOT
bumped for the older two, because neither is a layout an older binary would misread into a LOWER
floor, which is the only thing the version field defends:

| shape | written by | read as |
| --- | --- | --- |
| `hmac-sha256=` + 3-line body | current | authenticated; `sealed` TRUSTED |
| `sha256=` + 3-line body | the same-day pre-HMAC revision | digest verified; `sealed` **discarded**, forced `false`; WARN; rewritten keyed at the next start |
| `sha256=` + 2-line body | **`f56c723`, which is in `main`** | same as above (there is no `sealed` line to read) |

The two-line body was briefly declared CORRUPT on the premise that "v4 never shipped". **That premise
was false** — `f56c723` is in `main` and writes exactly that — so a routine upgrade hit
`ErrIndexFloorCorrupt`, refused to start, and pointed the operator at a remedy that reissues ids.
Discarding a legacy file's `sealed` bit costs at most one burned reservation block on the first start
after upgrade; trusting it costs invariant 1.

**Three fields. `reserved` and `written` are STRICTLY NON-DECREASING and enforced as such in code;
there is deliberately no field that can rewind a floor:**
- `reserved` — no WAL record index above this value has EVER been authorised by this data directory.
- `written` — every WAL record index at or below this value is BURNED: it has either been written to
  the log, or permanently SKIPPED by recovery.
- `sealed` — `1` means the run that wrote the file reached a clean `Writer.Close`, so `written` is
  EXACT rather than a lower bound. It is EXEMPT from the monotonicity rule (it goes `1`→`0` at every
  `begin`, fsynced before the writer may append, so a crash can only ever leave `0`), and clearing it
  can only make the next start MORE conservative.
- Always `written <= reserved`; a loaded file where that fails is corrupt.

**Written AHEAD of the index it authorises.** `Writer.Append` reserves the index durably BEFORE
stamping it into a frame, and POISONS the writer if the reservation cannot be made. Reservation is
amortised in blocks of `indexReserveBlock = 64`. A floor write is a temp file + fsync + rename +
directory fsync — roughly THREE syncs — so a block of 64 costs about 3 extra syncs per 64 appends,
call it ~5% on the fsync count of the send path. (**UNMEASURED**: that is arithmetic on sync counts,
not a profile, and `64` is a knob nobody has benchmarked.) The price of the block: a crash burns up to
63 unused indices, which show up as a HOLE in the WAL's index sequence. Holes are legal and permanent
here — invariant 1 beats gap-freeness.

**`wal.Open` resumes at the MAXIMUM of** the replayed high-water mark, one past the highest index the
repair pass OBSERVED (`Repair.NextIndex`), and `written+1` from this floor — **plus `reserved+1`
whenever the previous run did not close cleanly (`sealed 0`)**. The trigger is the seal bit and NOT
`Repair.LostUnidentified`: "did this recovery find damage" is unknowable, because a truncation at a
clean frame boundary is byte-for-byte a legitimately shorter log. `LostUnidentified` remains exported
and is still good diagnostics; it is no longer a correctness trigger.

**Read AFTER the MAC key is settled.** `wal.Open` reads this file after `macKeyFor`/`repairLog` have
ruled on the key, not before. Reading it first made a merely WRONG key surface as a corrupt floor and
pointed the operator at deleting it — a remedy that forfeits invariant 1 when the actual fix was to
restore the key.

**Written atomically:** a temp file in the same directory is written, fsynced, renamed into place,
and then the directory itself is fsynced. A crash can therefore never produce a corrupt floor file —
corruption means media damage or tampering. Temp files a crash left behind are reaped at the next
open by `os.ReadDir` + prefix match, **never `filepath.Glob`**: `-data` is a path, and interpolating
it into a glob PATTERN let `-data /srv/bus[1]` unlink a SIBLING directory's temp file while missing
its own, and `-data /srv/bus[` disable the reaper permanently via `ErrBadPattern`.

**MISSING vs UNVERIFIED vs CORRUPT — three states, handled differently:**
- **MISSING is benign** — a data directory written by a binary that predates this file — and yields a
  zero floor, logged at WARN when the directory is not otherwise fresh. Refusing to start over a
  missing file would brick every already-deployed bus on upgrade, which is exactly the bricking the
  existing `upgradeV1` path exists to avoid.
- **UNVERIFIED is loud but not fatal.** If `wal-mac.key` was ABSENT when the directory was opened and
  recovery minted a new one, nothing the previous identity wrote can verify — floor included. That is
  a re-founded directory, not a damaged floor, so the file is read WITHOUT authentication: its
  numbers are kept (they are only ever consumed as a RAISE, so they can only make the start more
  conservative) while `sealed` is discarded, and it is logged at **ERROR**. An attacker who could
  forge that file could equally DELETE it, which no MAC can prevent; what the MAC buys is that the
  forgery is no longer SILENT.
- **CORRUPT (bad header, unknown version, a tag that fails under the directory's OWN key, malformed
  number, or `written > reserved`) is FATAL**, wraps the exported sentinel
  `wal.ErrIndexFloorCorrupt`, and is **NEVER regenerated** — regenerating it would resume the WAL
  record index below numbers already handed out, silently, with nothing downstream able to detect it.
  (*Corrected 2026-08-07: this used to add "and the message sequence derived from it". It no longer
  is — see the retired counting argument below. The MESSAGE SEQUENCE is now raised independently by
  `message-seq-floor`, so regenerating THIS file does not rewind it. `internal/wal/indexfloor.go`'s
  own `indexFloorCorrupt` error string still carries the old parenthetical; it errs conservative —
  it tells the operator to restore from backup either way — and is filed as a follow-up.*) The error
  tells the operator to restore the file from a
  backup, and states plainly that **deleting it FORFEITS INVARIANT 1 for that data directory unless
  the previous run shut down cleanly**. The old wording ("delete it and restart; correct unless the
  log has ALSO been damaged") was unsound and is gone: a log truncated at a record boundary is
  byte-for-byte identical to a shorter one, so no operator can satisfy that caveat, and following it
  reissued an index at 2268 of 2289 measured truncation offsets.

**This file adds NO refuse-to-start behaviour, and recovery from LOG DAMAGE still always reaches a
running server.** A quarantine still starts a fresh log; it just starts it above the floor instead
of at 1. Every index skip is logged loudly (WARN, or ERROR after a quarantine) naming
from/to/indices skipped and the floor file's path. That "always" is about media damage to the log
and is NOT unconditional for startup as a whole — three named MISCONFIGURATION carve-outs are fatal,
and they are listed together under "Recovery always reaches a running server — with three named
exceptions" below.

**Scope limit, stated plainly and not softened:** the AUDIT log writer (`wal.OpenWriter`,
`wal.KindAudit`) attaches NO floor — only `wal.Open` (the WAL proper) does. An audit log's discarded
tail record index can therefore still be reissued; this file protects the WAL's record indices — it
does not protect the audit trail.

**CORRECTED 2026-08-07 — this file no longer protects MESSAGE IDS, and the claim that it does is
retired.** The sentence above used to end "…this file protects the WAL's record indices and, through
`internal/hub`'s derivation, message ids". That was true when written and is now FALSE. It rested on
a COUNTING argument — each message consumed one sequence and at least two record indices, so the
indices outran the sequences for ever and this floor bounded the message sequence transitively. **The
mint batching introduced by `SIGN-2`/`SIGN-6` broke that ratio**: `/v1/mint` hands a client a
sequence BEFORE any record carries it, and burns `hub.MintBatchSize` (256) of them per floor record,
so five mints consume five sequences against two indices. Message ids are now protected by a SECOND,
independent file — `message-seq-floor`, documented in the next section — and **not** by this one.
Measured: with that file removed, 247 of 248 truncation offsets reissued a sequence despite this
floor being present and intact.

**Migration residual, stated plainly and not softened:** on a data directory that predates this
file, the floor reads as zero until the first `Open` under the new binary writes it. If such a
directory's WAL is quarantined on that very first start under the new binary, ids can still be
reissued. The window closes permanently after one successful start.

New exported surface introduced by this file: `wal.IndexFloorFileName` (= `"wal-index-floor"`),
`(*wal.Log).IndexFloorPath()`, `wal.Repair.LostUnidentified`, `wal.Recovered.FirstIndex`. No new
HTTP route, CLI flag, env var, or header was introduced by this change.

## On-disk files in the data directory: the durable MESSAGE-SEQUENCE floor (`MSG-FU-SEQHIGHWATER`, documented 2026-08-07)

**This is a SECOND, INDEPENDENT floor file, and it is not a duplicate of `wal-index-floor` above.**
The two guard DIFFERENT counters: `wal-index-floor` guards the WAL RECORD INDEX, this one guards the
MESSAGE SEQUENCE that becomes the `<bus-id>-<seq>` message id a client signs. Anyone tempted to
collapse them should read the next paragraph first — the argument that once tied the two together
has been retired, deliberately, and reinstating it would reopen a P0.

**Why `wal-index-floor` does NOT cover this transitively (the retired counting argument).** Before
`SIGN-2`/`SIGN-6`, every sequence issued was `<=` the WAL index of the prepare carrying it — each
message consumed one sequence and at least two indices, so the indices outran the sequences for
ever, and the WAL's own durable index floor bounded this counter for free. **That argument is dead.**
`/v1/mint` now hands a client a sequence BEFORE any record carries it, and it burns a BATCH of
`hub.MintBatchSize` (256) numbers per floor record — five mints consume five sequences against two
WAL indices. The counters are no longer related. `internal/hub/mint.go` and `hub.Open`'s
floor-derivation comment both say so at length; **do not reinstate any reasoning that ties a
sequence to a WAL index.**

`<data-dir>/message-seq-floor` (mode `0600`, on-disk format version **5** — RESERVED via the Spec
Server `ondisk-format-version` namespace, never hand-picked) is a small, atomically-replaced file
living OUTSIDE the WAL, implemented in `internal/hub/seqfloorfile.go`. Format: a header line
carrying an unkeyed SHA-256 digest, then a one-line body:

```
agent-bus-message-seq-floor v5 sha256=<64 hex>
floor <decimal uint64>
```

The digest covers the BODY only. Numbers are canonical decimal (no sign, no leading zeros), so a
floor has exactly one spelling and the file stays readable by eye — an operator diagnosing a bus
should be able to `cat` it. An UNKNOWN version is a HARD ERROR, never a "read what you can": a file
written by a newer binary may encode a HIGHER floor this one cannot see, and reading it partially
would LOWER the floor, which is the one thing this file exists to make impossible.

**The digest is INTEGRITY, not AUTHENTICATION, and that is a deliberate difference from
`wal-index-floor`'s keyed HMAC.** Anyone who can write the file can recompute the digest. The
justification, stated in the form a security gate accepted on 2026-08-07 rather than the weaker form
first written here:

- **The FLOOR VALUE is consumed only as a RAISE** — `hub.Open` folds it into a maximum over four
  sources, `ids.Sequence.RaiseFloor` refuses a lower value, and `seqFloorFile.persistLocked` refuses
  a decrease at the last point before the bytes. Verified across every path; none trusts the number
  directly.
- **Forging it LOW therefore achieves exactly what DELETING it achieves**, and no MAC prevents
  deletion, because a missing file is deliberately non-fatal.
- **Forging it HIGH is a real but bounded outcome, and is recorded here rather than omitted:** a
  valid-digest `floor 18446744073709551615` seals the allocator at `MaxUint64`, every subsequent mint
  fails permanently with `ids.ErrSequenceExhausted`, and the floor is monotonic so it can never be
  lowered back. That is a denial of service, not an id reissue. The error names neither this file nor
  the remedy; **the remedy is to move `message-seq-floor` aside and restart.**
- **Both attacks require write access to the data directory**, which already holds `wal-mac.key`,
  `bus-signing.key` and `bus-tls.key` — an adversary there owns the bus outright, and can reach the
  identical DoS through a properly-MAC'd in-log `"seqfloor"` record. A keyed MAC here would buy
  almost nothing. *Tracked as a consistency follow-up, not as a known hole.*

*Note the argument above deliberately does NOT claim this file has no trust bit at all.
`existedAtOpen()` is consumed DIRECTLY rather than as a raise (`hub.Open`, the quarantine report): a
file that exists but carries a stale low floor flips the loud "message ids may repeat" ERROR into the
reassuring one. That is a false negative in an operator report, not an id reissue — filed as a
follow-up.*

**The single field, and the invariant on it.** `floor` — every message sequence AT OR BELOW this
value is BURNED: handed to a client, written to the log, or permanently skipped. It is strictly
non-decreasing, and a decrease is REFUSED in code rather than silently accepted. There is
deliberately NO second field and NO clean-shutdown flag: `wal-index-floor` needs `reserved`/`written`
because its recovery distinguishes "authorised" from "consumed", whereas here the two collapse — a
burned sequence is burned identically whether a message carried it or a crash wasted it. Gaps are
CORRECT (`internal/ids/sequence.go` says so for the neighbouring counter). **A floor that can go down
is the entire defect**, so no field exists that a future edit could reason downwards from.

**The ordering contract: the floor is fsynced AHEAD of the number it authorises.** `Hub.Mint` raises
and fsyncs the floor past the whole batch BEFORE handing any number out, so a number a client holds
is always already covered by a durable floor. This is invariant 4's ordering one layer down —
nothing is ISSUED before its floor is durable.

**Startup derivation — the floor is the MAXIMUM of four durable facts** (`hub.Open`), of which
**(0) is authoritative and (1)–(3) are defence in depth**:

| # | source | why it is not sufficient alone |
|---|---|---|
| **0** | `message-seq-floor` (this file) | **authoritative** — the only one that survives a quarantine |
| 1 | the log's high-water index, `wal.Recovered.NextIndex - 1` | read OUT OF the log; retired counting argument above |
| 2 | the highest replayed `"seqfloor"` record (`hub.SeqFloorRecordKind`, see that section below) | read OUT OF the log — a quarantine discards it |
| 3 | the highest sequence on a replayed message record | read OUT OF the log; also misses a DANGLING prepare's sequence |

Sources (1)–(3) all drop together, to nearly zero, on exactly the start where the floor matters most.
Each can only ever RAISE, never lower, so all four are kept.

**Measured evidence (2026-08-07), the reason this file is not optional.** A truncation sweep over
every offset of a mint-bearing WAL, resuming the bus at each one and measuring the sequence by
performing a REAL mint rather than trusting an accessor:

| data directory | offsets swept | offsets reissuing a sequence |
|---|---|---|
| with `message-seq-floor` present | 248 | **0** |
| with `message-seq-floor` removed | 248 | **247** |

The removed-file column is the pre-fix behaviour and the migration window, not a hypothetical.

**Failure modes.** A MISSING file is NOT fatal — a data directory written by a binary that predates
it is a legitimate benign cause, and refusing would brick every deployed bus on upgrade. `hub.Open`
falls back to sources (1)–(3), logs at WARN when the directory is not otherwise fresh, and — **when
the derived floor is non-zero** — CLOSES the window immediately by persisting it. When the derived
floor is zero the write is skipped, which is the case the migration note below qualifies. A CORRUPT file (`hub.ErrSeqFloorFileCorrupt`) is
FATAL and is **never regenerated**: regenerating resumes the sequence below numbers already handed
out and already SIGNED, so two validly-signed messages would carry one message id and **both
signatures would verify**. A crash can never produce that state — the write is temp file + fsync +
rename + directory fsync — so corruption means media damage or tampering. The error names a one-step
remedy and states its cost rather than handing over a command that is merely safe on most days.

**Migration residual, stated plainly and not softened:** a data directory that predates this file and
whose WAL is QUARANTINED on its very first start under the new binary still resumes below numbers it
already issued. `hub.Open` reports exactly that, at **ERROR**, rather than pretending otherwise. This
is the same one-start residual `wal-index-floor` carries, for the same reason.

**Be precise about when the window actually closes — a successful start is not always enough.** A
directory with HISTORY normally gets the file written at `Open`, because the derived floor is
non-zero. But when the derived floor is `0` — a fresh directory, or a first start that is ALSO a
quarantine, since a quarantine restarts `NextIndex` at 1 — `raise(0)` is a no-op that writes no file,
and the window does NOT close merely because the start succeeded. In that case it closes as soon as
this bus burns its first sequence, because the mint fsyncs the floor before handing any number out.
Safety holds throughout (nothing was issued in the interim), but an operator told "one successful
start" would have believed the window closed when it had not.

**Upgrade/downgrade.** Existing data directories are unaffected on upgrade: the file is absent, that
is benign, and the first `Open` writes it. **Downgrade is effectively one-way** — an older binary
does not read `message-seq-floor`, so it silently derives the floor from the log alone and forfeits
this protection; it does not, however, fail to start, and the file is left intact for a later
upgrade.

New exported surface introduced by this file: `hub.SeqFloorFileName` (= `"message-seq-floor"`),
`hub.ErrSeqFloorFileCorrupt`. `hub.Options.DataDir` is REQUIRED for any hub with a durable write
path, because this is where the file lives. No new HTTP route, CLI flag, env var, or header is
introduced by it.

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
wording is set by `internal/wal/format.go`'s `corruptf`, not by `cmd/agent-bus`). **Recovery from LOG
DAMAGE always reaches a running server** (decision of 2026-08-02, invariant 6).
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

### Recovery always reaches a running server — with three named exceptions (corrected 2026-08-07)

**This paragraph corrects an unqualified claim that was VERIFIED FALSE at the time of writing.** The
sentence above, and its twin in the `wal-index-floor` section, both used to read "Recovery ALWAYS
reaches a running server" with no carve-out. An operator who met one of the refusals below had no
document to search, which is the failure this correction exists to fix.

The decision of 2026-08-02 is about **MEDIA DAMAGE TO THE LOG**: when acknowledged bytes rot, the bus
chooses availability over retention, discards loudly and starts. It has never applied to
**MISCONFIGURATION or LOST KEY MATERIAL** — a class where starting anyway would silently reuse
identities or make already-durable records unverifiable, and where the damage of booting is
unbounded and invisible while the damage of refusing is one restart. Three carve-outs are fatal
today. All three name the offending path and a remedy, and none of the three is ever "repaired" by
regenerating the missing file:

| refusal | condition | remedy |
|---|---|---|
| `wal-mac.key` missing or wrong | the log positively identifies itself as a format-version-2 log with content (`PROTOCOL.md` §4/§6) | restore the key from backup; it is never regenerated over an existing v2 log, because that makes every record unverifiable |
| `agent-suffixes` missing | the data directory HAS history (it was non-empty at startup, or the log holds records) — `DECISIONS.md` 2026-08-07, `AUTH-3-FU-FAILOPEN`, and the seal-line ruling in `cmd/agent-bus/suffixfloors.go` | restore the floors file from backup; or, only if the directory genuinely never issued an agent id, restart ONCE with `-backfill-suffix-floors` |
| bus key material unusable or partial | any of `bus-tls.crt` / `bus-tls.key` / `bus-signing.key` malformed, expired, group-or-other-readable, or present-but-incomplete (`MTLS-BUSCERT`, see the section on those three files below) | restore the named file from backup; the bus never re-mints, because a new TLS key breaks every pinned client and a new signing key kills every peer bus's pin |

A CORRUPT `agent-suffixes` (`ids.ErrSuffixFileCorrupt`), a CORRUPT `wal-index-floor`
(`wal.ErrIndexFloorCorrupt`) and a CORRUPT `message-seq-floor` (`hub.ErrSeqFloorFileCorrupt`) are
fatal on the same reasoning and with no override at all; they are listed in their own sections rather
than repeated here. Note the shared shape: all three are damaged IDENTITY files, not a damaged LOG,
which is why refusing over them does not narrow invariant 6.

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

**2026-08-07 addendum — the start index is no longer derived from the log alone (e120153b,
db350e39).** `wal.Open` now computes the index its first append will use as the MAXIMUM of three
sources, not the log's own arithmetic: (1) the replayed high-water mark, (2) one past the highest
index the repair pass OBSERVED (`Repair.NextIndex`, now including identified discards — it used to
be one past the highest SURVIVOR, which was defect `e120153b`), and (3) `written+1` from the durable
record-index floor at `<data-dir>/wal-index-floor` — plus `reserved+1` from that same floor
**whenever the previous run did not close cleanly (`sealed 0`)**. The ceiling was briefly conditional
on `Repair.LostUnidentified` instead; that is unsound, because a truncation at a clean frame boundary
is byte-for-byte a legitimately shorter log and so "did recovery find damage" is not a question
anything can answer. See "On-disk files in the data directory" above for the floor file itself, and
`DECISIONS.md` (2026-08-07) for why deriving the mark from the log alone, on its own, was the defect
rather than an accepted limit.

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

## The applied-key store's per-agent fair share (IDEM-11-FU-FAIRSHARE, added 2026-08-07)

**Nothing about the on-disk shape changed.** No field was added to `recordJSON` (the table above is
still complete), no WAL record type or `ondisk-format-version` was reserved, and the PREPARE payload's
`idem` field is unchanged from what IDEM-11 shipped. The fair share is a LIVE ADMISSION policy — a
decision about whether to accept an operation that has not happened yet — never a property of a stored
record, so there is nothing for a record to carry and no migration for an existing log.

**The defect it closes:** `MaxEntries` (65536, above) was a BUS-WIDE bound only, and entries are
evicted solely by age, never under pressure. One authenticated agent could occupy the whole table,
after which every OTHER agent's mutating operations were refused with `ErrCapacity` for up to the full
`RetentionWindow` (50h10m22s) — even agents holding zero keys of their own.

**The rule** (`internal/idem/retention.go`'s derivation; enforced by `internal/idem/store.go`'s
`admitAgentLocked`, called from both `Store.Admit` and `Store.Remember`):

```
under pressure : retained >= maxEntries/2          (idem.PressureLine, for the default 65536 bound)
fair share      : maxEntries / (agents + 1)          agents = distinct agents holding >= 1 record
admission       : not under pressure                     -> admit
                  under pressure and held >= fair share  -> refuse (idem.ErrAgentQuota / hub.ErrAgentQuota)
                  otherwise                                -> admit
```

`idem.PressureLine` is exported for exactly this citation — it names the fill level at which the
table's FREE space stops exceeding its USED space (a derived crossover, not a chosen round number).
**Below the pressure line, nothing changes**: a bus that never approaches its cap sees no behaviour
difference from this rule at all.

The `+1` in the divisor is the agent that has not arrived yet. With a divisor of `agents`, a lone
agent's share would be the whole table, so the exact attack the rule exists to close — one agent,
acting alone, filling the table before any victim holds a single record — would pass straight through:
the victim cannot be counted in a bucket it holds nothing in, precisely because it is the one being
starved. The phantom slot reserves its room before it exists.

**The cost, stated plainly:** a SOLE agent on a bus can now hold at most `maxEntries/2` = 32768 applied
keys instead of 65536, halving its sustained throughput ceiling (~0.36 -> ~0.18 accepted mutating
ops/sec, sustained over the retention window). Task IDEM-11-FU-THROUGHPUT already tracks the
sustained-ceiling concern generally; this is not a new instance of it, just a lower number for the
single-agent case.

**It fails CLOSED and evicts nothing** — the identical posture the bus-wide cap already takes, and for
the identical reason: evicting a live key silently turns that key's next legitimate retry into a
SECOND effect, which invariant 10 forbids. A refused operation is recoverable; a duplicated one is not.

**The replay path does not adjudicate the share.** `hub.Apply` calls `idem.Store.Recover`
(`internal/idem/store.go`), never `Remember`. `Recover` is `Remember` minus exactly the per-agent
check — it still validates the record, still expires first, and still enforces the bus-wide
`MaxEntries` cap the same way `Remember` does; only `admitAgentLocked` is skipped. A record on disk is
proof that admission ALREADY succeeded (acknowledged to the client and fsynced); re-testing that
decision at replay could only ever disagree with a decision already acted on, and the disagreement is
not a stricter bus, it is a LOST key — the client's next legitimate retry of it becomes a SECOND
message, delivered by the very mechanism added to prevent exactly that. Two concrete cases make this a
real hazard rather than a tidiness argument: a clock stepping backwards makes the replayed retained set
a SUPERSET of what was live (the safe direction for expiry's own predicate, the unsafe one here), and a
log written BEFORE this change can already hold one agent above the new share, so the FIRST restart
after upgrading would otherwise drop that agent's already-accepted keys. Both are exercised by
`TestReplayNeverRefusesWhatTheLivePathAccepted` (`internal/hub/idem_quota_test.go`). `byAgent` counters
ARE rebuilt during recovery, so an agent recovered above its own share stays frozen — refused until its
own keys age out — exactly as it would have been pre-restart; only the ADJUDICATION at replay time is
skipped, not the bookkeeping.

**Error surface:** `idem.ErrAgentQuota` and `hub.ErrAgentQuota` (`internal/idem/errors.go`,
`internal/hub/errors.go`). `go.mod` pins go 1.19 (no multi-`%w`), so each package's refusal is a small
type whose `Is` matches BOTH its own quota sentinel and that package's `ErrCapacity` — required, not
tidy: `internal/httpapi` maps `hub.ErrCapacity` to 503 with `Retry-After: 5`, and a refusal that missed
that second match would silently fall through to a generic 500. **The wire behaviour is therefore
unchanged by this task**: a per-agent refusal is still a plain 503 with the same fixed body
(`"server at capacity, retry later"`) — the agent name, its holding, the fair share and the agent count
appear ONLY in the server's operator log (the message `admitAgentLocked` builds), never in the client
response. `CONTRACTS-HTTP.md` does not yet carry an explicit row for this refusal (task
IDEM-11-FU-PAPERTRAIL). Whether 503 is the right status for a per-agent (rather than bus-wide) cap is
a separate, already-tracked question — `AUTH-1-FU-ACTIVECAP-RETRYAFTER` covers it once across all
three per-agent caps — and was deliberately not touched here.

**New Go-level option:** `hub.Options.MaxIdempotencyEntries` (`internal/hub/hub.go`) bounds the applied-
key table for one hub; 0 (the default) means the derived `idem.MaxEntries` / `hub.MaxIdempotencyEntries`
constant (65536). It is not a CLI flag or an environment variable — it exists so the fair share is
PROVABLE in a test: filling the real 65536-entry table to exercise the rule means 65536 durable,
fsynced writes, which is a test nobody would run.

No new WAL record type, `ondisk-format-version`, HTTP route, CLI flag, header, or env var was
introduced by IDEM-11-FU-FAIRSHARE.

## The durable enrolment record (AUTH-3, added 2026-08-07)

Invariants 1 and 5 require that a server-minted agent id survive a restart, not just an in-memory
process: state is held in memory for speed and REBUILT by replaying the durable store. AUTH-3 is
that store for enrolment — `internal/auth`'s `record.go` (the on-disk shape, `Encode`/`Decode`),
`roster.go` (`RosterEntry`, `CertBinding`, `MaxCertBindings`), `walroster.go` (`WALRoster`, the
`wal.Applier` that rebuilds the roster by replay) and `floors.go` (`EnrolmentSuffixesInWAL`, an audit
scan of the suffixes in enrolment records — NOT a suffix floor and never to be sealed into an
allocator; see the correction below). Read `internal/auth/doc.go`'s package doc
for the honest statement of what is and is not durable today; this section documents only the on-disk
shape.

**NO new WAL record type and NO `ondisk-format-version` bump. Nothing was reserved.**
`wal.Entry.Kind` is a free-form APPLICATION STRING (`internal/wal/log.go`: "the application
discriminator: `\"message\"`, `\"agent\"`, ..."), not a reserved on-disk record-type NUMBER — the
numbers (`wal.Type`: `TypePrepare`, `TypeCommit`, ...) are owned by `internal/wal` and reserved
through the Spec Server `ondisk-format-version`/record-type namespaces; `Entry.Kind` is not one of
them and needs no reservation to add a new value. `"agent"` (`auth.RecordKind`) is moreover the exact
name `internal/wal/log.go`'s own `Entry.Kind` doc already reserved this discriminator's name for, so
it is the name the format was written expecting, not a name invented in this task. An enrolment rides
in the existing PREPARE/COMMIT frames exactly as a message does — same two-phase write path, same
fsync-at-prepare-and-again-at-commit, no new frame shape.

**One WAL now carries at least two `Entry.Kind` values.** `auth.RecordKind = "agent"` /
`auth.RecordVersion = 1` sit alongside the existing `store.RecordKind = "message"` /
`store.RecordVersion` (`internal/store/message.go` — **`1` when this section was written; `2` since
SIGN-2/SIGN-6, see "The message record is version 2" at the end of this file**. `auth.RecordVersion`
is a SEPARATE, independently-versioned number and is still `1`: bumping the message record did not
touch the enrolment record, and an enrolment written by a pre-SIGN-6 build is still read by a
post-SIGN-6 one) in the SAME log file. Each applier is handed
every committed entry regardless of kind and SILENTLY SKIPS the ones that are not its own —
`WALRoster.Apply` returns `nil` immediately when `c.Entry.Kind != RecordKind`, the same shape
`Hub.Apply` already uses for message records it does not own. Neither applier treats the other kind's
records as damage.

**The JSON shape** (`internal/auth/record.go`'s `recordJSON`, verified against the struct tags, not
copied from a summary):

```
{"v":1,
 "agent_id":"<bus-id>.<name>-<n>",
 "name":"<name>",
 "auth_pub":"<base64 std, 32 bytes>",
 "msg_pub":"<base64 std, 32 bytes>",        // omitted while unpopulated
 "invite_id":"<string>",                    // omitted while unpopulated
 "epoch":"<RFC3339Nano UTC>",
 "cert_bindings":[{"fp":"<hex, 32 bytes>",
                   "bound_at":"<RFC3339Nano UTC>",
                   "retired_at":"<RFC3339Nano UTC>"}],  // omitted while live
 "enrolled_at":"<RFC3339Nano UTC>"}
```

| field | Go type | on-disk encoding | omitted when |
| --- | --- | --- | --- |
| `v` | `int` | `RecordVersion` (currently `1`) | never |
| `agent_id` | `string` | fully-qualified `<bus-id>.<name>-<n>` (invariant 2) | never |
| `name` | `string` | the short name, byte-identical to the name half of `agent_id` | never |
| `auth_pub` | `ed25519.PublicKey` | **base64 standard encoding** (`encoding/base64`), 32 bytes | never |
| `msg_pub` | `ed25519.PublicKey` | base64 standard encoding, 32 bytes | `RosterEntry.MessagingPublicKey` is empty (reserved, unpopulated) |
| `invite_id` | `string` | verbatim | `RosterEntry.InviteID` is empty (reserved, unpopulated) |
| `epoch` | `time.Time` | `RFC3339Nano`, UTC | never |
| `cert_bindings` | `[]CertBinding` | array of `{"fp","bound_at","retired_at"}`, bounded to `MaxCertBindings` | `RosterEntry.CertBindings` is empty (reserved, unpopulated) |
| `cert_bindings[].fp` | `[32]byte` | **hex** (`encoding/hex`), the fingerprint `sha256.Sum256(cert.Raw)` | never (present in every element) |
| `cert_bindings[].bound_at` | `time.Time` | `RFC3339Nano`, UTC | never (present in every element) |
| `cert_bindings[].retired_at` | `*time.Time` | `RFC3339Nano`, UTC | the binding is LIVE (`RetiredAt == nil`) |
| `enrolled_at` | `time.Time` | `RFC3339Nano`, UTC | never |

The encodings match the precedents already on disk rather than being picked per field: times are
`RFC3339Nano` in UTC exactly as `idem.Record` writes `committed_at`; the certificate fingerprint is
HEX, the same choice `idem` makes for its own `fp`; the public keys are BASE64 STANDARD ENCODING,
which is what the enrolment wire format already uses for the same bytes, so an operator reading the
log sees the same string the client sent. Every reserved field is `omitempty`, so **a record written
today is byte-for-byte the record a pre-INVITE, pre-MTLS build would have written**, and the reserved
keys appear on disk only once something actually populates them.

**The reserved-but-unpopulated fields are on disk from record 1, deliberately.** `msg_pub`
(SIGN/CRYPTO-3), `invite_id` (the INVITE epic) and `cert_bindings` (MTLS-BIND) are declared and
encoded even though nothing writes them yet, per the ordering rule in `DECISIONS.md`'s
2026-08-07 "ENROL-SHAPE" entry: nothing was persisted before AUTH-3, which made this the LAST MOMENT
the durable record could be shaped without a migration — and because an agent id is bound to a
keypair, a migration here is not a schema edit but a FORCED RE-ENROLMENT OF EVERY AGENT.
`cert_bindings` is `MaxCertBindings = 16`, a BOUNDED HISTORY and not a current-value field — the same
class of bound as the applied-key table's `MaxEntries`, for the same reason (an agent rotating a
client certificate in a loop must not grow the record without limit, and every binding is decoded off
disk during recovery, where the input is whatever the file holds rather than whatever a handler
validated). Retirement is EXPLICIT: `retired_at` absent means the binding is LIVE, never
implicit-by-supersession — a binding that silently aged out would be indistinguishable from one that
was revoked, and those need different operator responses.

**`Decode` is STRICT** (`encoding/json`'s `DisallowUnknownFields`, plus an explicit `v ==
RecordVersion` equality check), matching `idem.DecodeRecord` and `wal.decodePayload`. The downgrade
hazard that buys is the one `wal.Entry.Idem`'s CONTRACTS-ONDISK section above already states for the
applied-key record, and it applies here unchanged: a binary built BEFORE a field existed, reading a
log written AFTER it, sees EVERY enrolment record carrying that field as undecodable — and
**an undecodable enrolment record is DISCARDED at replay** (`WALRoster.Apply`), so the agent it named
is silently absent from the roster and must re-enrol, under a new id, because the old suffix is
burned. The version check catches the ORDINARY form of that downgrade with a much better error
message; `DisallowUnknownFields` only catches an additive field that did NOT move `RecordVersion` —
the case this package intends to keep open for the INVITE/MTLS/SIGN epics, which must therefore land
their field and its decoder together, in one build.

**Recovery semantics** (`WALRoster.Apply`, `internal/auth/walroster.go`):

- The roster is rebuilt entirely by REPLAY: `WALRoster` is constructed first (empty), handed to
  `wal.Open` as the `Applier`, and every committed `"agent"`-kind entry is folded in before `Open`
  returns — the same construction order `hub.Apply` uses for messages.
- A DUPLICATE agent id KEEPS THE FIRST record and never overwrites. An overwrite would rebind a live
  identity to a different keypair — the worst outcome available on this path (invariants 1 and 3), since
  every DM addressed to that id would then route to the new key holder. The later record is logged at
  ERROR and dropped; `Apply` does not return an error for it.
- An undecodable record is discarded LOUDLY, at ERROR, with its prepare and commit indices, and the
  bus still starts. This is invariant 6's recovery contract (2026-08-02): recovery always reaches a
  running server, damaged records are discarded, and the absolute requirement is that every discard is
  logged — availability over retention, not silence over damage.

> **CORRECTION (2026-08-07, same day, after the security gate).** The two paragraphs below described
> `SuffixFloors` as the source of the startup suffix floors. **That was wrong and the function has
> been renamed `EnrolmentSuffixesInWAL` to make the mistake unrepeatable.** It reports the highest
> suffix in an **enrolment record only** — a strict SUBSET of the agent ids on disk, because a
> `store` message record names its sender and recipients and those suffixes are burned too. On any
> data directory written by a build where enrolment is still memory-only, the enrolment subset is
> EMPTY and the function returns an empty map with a **nil** error, indistinguishable from a fresh
> bus. Sealing that into an allocator re-mints every live agent id.
>
> **The production floor source is `ids.OpenNameSuffixes`** (`internal/ids/suffixstore.go`, commit
> `61b7c9a`), which PERSISTS AND FSYNCS each name's floor BEFORE issuing the suffix and derives
> nothing from history — so no tail repair and no log quarantine can rewind it, which makes the
> "known residual hole" below structural to derivation and absent from write-ahead. It is wired in
> `cmd/agent-bus/suffixfloors.go`. The roster-wiring task (**`MSG-FU-ROSTERSOURCE`**, Spec Server
> `public_id` `fa26036c` — this line used to name `AUTH-7`, a label that is not in the backlog) must
> NOT derive floors from `EnrolmentSuffixesInWAL` alone.
>
> **This relabel is PARTIAL and is not this file's to finish.** The `AUTH-7` → `MSG-FU-ROSTERSOURCE`
> correction is the acceptance criterion of a task of its own (Spec Server `public_id` `6fd8c8c5`,
> "Correct stale wave label AUTH-7 to its real task identity across code and docs"), which was
> `todo` and unclaimed when this line was written. Only the two `CONTRACTS-ONDISK.md` occurrences
> were corrected here, as a side effect of the durable-enrolment paragraph below being rewritten
> from false to true; roughly a dozen Go comments (`cmd/agent-bus/main.go`, `internal/auth`) still
> say `AUTH-7`. Whoever claims `6fd8c8c5` should expect this file to be already done and the code
> not to be.
>
> `EnrolmentSuffixesInWAL` is kept, not deleted, for one reason: the production derivation in
> `cmd/agent-bus/suffixfloors.go` folds only `store.RecordKind`, so the two cover COMPLEMENTARY
> halves. Unioning them is a filed follow-up; using either alone is not a floor.
>
> Read the rest of this subsection as a description of what the function scans, not as a
> recommendation to seal its output.

**`EnrolmentSuffixesInWAL` (`internal/auth/floors.go`) is a SECOND scan of the WAL, over raw PREPARE
records — not derived from the committed roster.** Per points 3 and 7 of `ids.NameSuffixes`' doc, deriving the
per-name suffix floor from the replayed (committed) roster is wrong twice over: it misses a suffix
burned by a dangling prepare (allocated, prepare fsynced, crash before commit — on disk and in the
audit log, but no committed roster entry mentions it), and it misses every agent that has since left.
`EnrolmentSuffixesInWAL` instead walks every `wal.TypePrepare` record via `wal.ScanAll` and a deliberately
LENIENT, non-`DisallowUnknownFields` decoder that reads only `agent_id` — a record too damaged for the
strict `Decode` to accept still burned a suffix, and validation protects the roster while the floor is
a claim about bytes that reached disk. An ABORTED prepare counts too. This is currently a genuinely
SECOND full pass over the log (`wal.Open` already walks it once to replay); `ID-2-WIRING-OBSERVER`
(`c31f6999-da4e-400d-ab55-178b82e2a42e`) is filed to fold the two into one pass via
`wal.ReplayWithPrepares`. **Known residual hole, not fixed here:** `wal.Open`'s `RepairLog` may
discard or quarantine records BEFORE this scan ever runs, and a discarded prepare's suffix becomes
invisible, lowering the floor — tracked by `db350e39-3dde-4166-b241-b21fa4635359` (whole-log
quarantine reissues every sequence) and `e120153b-9d8a-4b6a-bd4e-89431954496b` (recovery reissuing a
discarded tail index).

**Backward compatibility: an existing log needs no migration.** Nothing was persisted before this
change — no build of `agent-bus` before AUTH-3 ever wrote an `"agent"`-kind entry — which was the
entire value of settling `ENROL-SHAPE` first, and that value is now spent: any log written by an
AUTH-3-or-later build already carries the full field set above.

**WIRED — this section describes SHIPPED runtime behaviour** (corrected 2026-08-07; shipped at
`aad611c`, "Durable roster + signed sends: two agents survive a restart"). This paragraph previously
read "NOT YET WIRED", said `cmd/agent-bus/main.go` "still constructs
`auth.NewService(auth.Options{Minter: minter})` with no roster injected", and told the reader not to
treat enrolment as durable. **Every clause of that is now false**, and it named a task label
(`AUTH-7`) that does not exist in the backlog: the real Part 1 is **`MSG-FU-ROSTERSOURCE`** (Spec
Server `public_id` `fa26036c`, which is a task id and NOT a commit sha — do not try to `git show` it;
the code landed at `aad611c`). The label correction here is deliberately partial — see the note in
the blockquote above, and task `6fd8c8c5`, which owns the rest of it.

What `run()` actually does, in the fixed three-step order `auth.WALRoster`'s type doc spells out:

1. `authRoster := auth.NewWALRoster(lg)` — the roster is the log's APPLIER, so it must exist first;
2. `wal.Open(wal.LogOptions{…, Applier: authRoster})` — replay REBUILDS the roster inside `Open`,
   which is what makes an enrolment survive a restart;
3. `authRoster.Attach(walLog)` — only now may it accept a LIVE enrolment, and every `Put` is
   prepared, committed and fsynced before `Enrol` returns (invariant 4).

It is then passed as `auth.NewService(auth.Options{Minter: minter, Roster: authRoster})`, so
`MemoryRoster` is no longer reachable from `cmd/`. An agent that has enrolled does NOT have to
re-enrol after a restart; its **session** is still in-memory only and must be re-established, which
is a deliberate, permanent exception rather than a gap — see the `SESSIONS are in-memory only`
startup WARN, which states exactly that split.

**The startup suffix floors are still NOT derived from `EnrolmentSuffixesInWAL` alone**, and that
constraint outlived the wiring task — see the correction above. The allocator is
`ids.OpenNameSuffixes` in `cmd/agent-bus/suffixfloors.go`; `EnrolmentSuffixesInWAL` is folded in only
as one half of a per-name MAXIMUM on the one-time backfill path, where it can raise a floor and never
lower one.

No new route, CLI flag, env var, record type or `ondisk-format-version` was introduced by this task.

## The durable invite record (INVITE-STORE, added 2026-08-07)

Invariant 3 requires enrolment to be invite-only, and doc.go's opening line states the property
everything here rests on: single use is decorative unless it survives a restart. INVITE-STORE is
that store — `internal/invite`'s `record.go` (`RecordKind`, `State`, `Record`, the `recordJSON` wire
shape, `Encode`/`DecodeRecord`), `retention.go` (the derived bounds) and `store.go` (`Store`, the
in-memory table `wal.Applier` rebuilds by replay, and the mint/lookup/redeem/revoke lifecycle). Read
`internal/invite/doc.go` for the full model and the honest statement of its one fail-open exception;
this section documents only the on-disk shape. **Nothing in this section is reachable by an agent
today** — this package registers no HTTP route (that is `INVITE-GATE`'s task) and ships no operator
wrapper (`INVITE-MINT` / `INVITE-REVOKE`); it is the state machine and its durability, nothing more.

**NO new WAL record type and NO `ondisk-format-version` bump. Nothing was reserved.**
`wal.Entry.Kind = "invite"` (`invite.RecordKind`) is, exactly as `auth.RecordKind = "agent"` is
documented above, a FREE-FORM APPLICATION DISCRIMINATOR — it sits inside the PREPARE payload, above
the framing layer that `wal.Type` (`TypePrepare`, `TypeCommit`, ...) owns. `internal/wal/format.go`
was not touched, and there was nothing to reserve from either the `record-type` or
`ondisk-format-version` namespace above. This is written down explicitly so a future reader does not
go and reserve a number nothing requires. An invite entry rides in the same two-phase
prepare-fsync-commit-fsync frames as a message or an enrolment — no new frame shape, no new fsync.

**A third `Entry.Kind` value now shares the log.** `invite.RecordKind = "invite"` sits alongside
`store.RecordKind = "message"` and `auth.RecordKind = "agent"` in the same WAL file. `Store.Apply`
skips any entry whose `Kind != RecordKind` silently and returns nil — the same shape `WALRoster.Apply`
and `Hub.Apply` already use for kinds they do not own — so none of the three appliers treats another
kind's records as damage.

**The JSON shape** (`internal/invite/record.go`'s `recordJSON`, verified against the struct tags):

```
{"id":"inv-<20-32 lowercase base32 chars>",
 "bus":"<bus id>",
 "secret_sha256":"<hex, 32 bytes>",
 "label":"<operator text>",                     // omitted while empty
 "created_at":"<RFC3339Nano UTC>",
 "expires_at":"<RFC3339Nano UTC>",
 "state":"open"|"redeemed"|"revoked",
 "redeemed_at":"<RFC3339Nano UTC>",              // present only when state=="redeemed"
 "redeemed_by":"<bus-id>.<agent-id>",            // present only when state=="redeemed"
 "redeem_key":"<client idempotency key>",        // present only when state=="redeemed"
 "redeem_fp":"<hex, idem.FingerprintSize bytes>",// present only when state=="redeemed"
 "result":<opaque JSON>,                         // omitted when empty/absent
 "cert_sha256":"<hex, 32 bytes>",                // present only when state=="redeemed" AND a cert was bound
 "revoked_at":"<RFC3339Nano UTC>",                // present only when state=="revoked"
 "revoked_reason":"<operator text>"}              // present only when state=="revoked" and non-empty
```

| field | Go type | on-disk encoding | omitted when |
| --- | --- | --- | --- |
| `id` | `string` | server-minted, `^inv-[a-z2-7]{16,32}$` (`invite.InviteIDPattern`) | never |
| `bus` | `string` | the bus this invite admits to, `<= MaxBusIDLen` (64) | never |
| `secret_sha256` | `[DigestSize]byte` | **hex** (`encoding/hex`) — `HashSecret(secret)`, never the plaintext | never |
| `label` | `string` | operator note, `<= MaxLabelLen` (128), never echoed to a client | empty |
| `created_at` | `time.Time` | `RFC3339Nano`, UTC | never |
| `expires_at` | `time.Time` | `RFC3339Nano`, UTC | never |
| `state` | `State` | **fixed string** `"open"` / `"redeemed"` / `"revoked"` — never a number, so an operator reading the log with `head -c` and a pretty-printer can interpret it directly and it cannot silently change meaning if the constants are reordered | never |
| `redeemed_at` | `time.Time` | `RFC3339Nano`, UTC | `State != StateRedeemed` |
| `redeemed_by` | `string` | fully-qualified `<bus-id>.<agent-id>` (invariant 2) | `State != StateRedeemed` |
| `redeem_key` | `string` | the client idempotency key that redeemed it, verbatim | `State != StateRedeemed` |
| `redeem_fp` | `idem.Fingerprint` | **hex**, matching `idem`'s own `fp` encoding | `State != StateRedeemed` |
| `result` | `json.RawMessage` | the minted redemption result, verbatim, compacted, capped at `idem.MaxResultBytes` (512) | empty/absent result |
| `cert_sha256` | `[DigestSize]byte` | **hex** — `sha256.Sum256(cert.Raw)` of the client certificate bound at redemption | `CertFingerprint` is the zero value (DEFINED BUT UNUSED, see below) |
| `revoked_at` | `time.Time` | `RFC3339Nano`, UTC | `State != StateRevoked` |
| `revoked_reason` | `string` | operator note, `<= MaxReasonLen` (128), never echoed to a client | `State != StateRevoked` or empty |

Times are `RFC3339Nano` in UTC and digests are hex, matching the precedent `idem.Record` and
`auth.RosterEntry` already set on this log — an operator reading any of the three record kinds off
disk sees the same conventions. `state` is deliberately a fixed string rather than the numeric
`State` enum for the same reason `auth.RecordVersion`-style equality checks exist elsewhere: a
numeric value in a durable record is unreadable without the source, and it silently changes meaning
if the constants are ever reordered.

**EVERY ENTRY CARRIES THE COMPLETE RECORD IN ITS POST-TRANSITION STATE, never a delta.** Two reasons,
both load-bearing (doc.go section 3, restated here because it drives the wire shape): replay then
needs no ordering logic beyond a monotonic upsert (`Store.upsertLocked` is the one place anything
enters or changes in the table), and if an EARLIER record for an invite is discarded by recovery — a
corrupt frame, a capacity discard — a surviving LATER record still reconstructs the invite in its
SPENT state on its own. A delta scheme would leave the invite looking OPEN under the same loss, which
is the one direction that can produce a second redemption. The upsert itself is MONOTONIC:
`open -> redeemed` and `open -> revoked`, nothing else; a record that would move an invite backwards
is refused and logged loudly, never applied.

**The invite SECRET is never stored — only its SHA-256 digest.** `Record.SecretDigest` is the only
form of the secret that reaches `Encode`, and the plaintext (`Minted.Secret`, `GenerateSecret`'s
output — `SecretBytes` = 32 bytes of `crypto/rand`, base64.RawURLEncoding) exists only in `Store.Mint`'s
return value and is never logged, echoed, or written to the WAL. This is concrete evidence, not just a
design claim: `internal/invite/store_test.go`'s `TestInviteMintNeverStoresTheSecret` mints an invite
through a real `*wal.Log` and asserts the plaintext secret appears nowhere in the resulting `bus.wal`
bytes.

**`CertFingerprint` is DEFINED BUT UNUSED, deliberately, from day one** — per `DECISIONS.md`'s
2026-08-07 "ENROL-SHAPE" entry, which fixes `sha256.Sum256(cert.Raw)` as the one fingerprint
computation so nobody invents a second, incompatible one for the same certificate. It is
`Record.CertFingerprint [DigestSize]byte`; the ZERO value means "no certificate was bound", which is
the only value anything writes today (`Store.Redeem`/`Redemption.Consume` accept a `Result.CertFingerprint`
field, but nothing populates it with a real fingerprint yet). It rides on disk now, exactly as
`auth.RosterEntry`'s reserved fields do, so `MTLS-BIND` adds a CHECK against an already-durable field
rather than a schema change to records that already exist.

**The bounds and retention, cited from `retention.go`'s own derivations rather than restated as picked
numbers:**

- `MaxRecordBytes = 2 KiB` — one retained record's worst-case footprint, summed field by field
  (`retention.go`'s comment carries the full arithmetic: id, bus, three digests, redeemed_by,
  redeem_key, result, label, revoked_reason, four timestamps, the state string, JSON punctuation and
  Go map/struct overhead).
- `MaxRetainedBytes = 16 MiB`, a QUARTER of `idem.MaxRetainedBytes` (64 MiB) — the difference is the
  ARRIVAL RATE, not the record size: applied-key records arrive at client speed, invites are
  operator-minted through the filesystem (`INVITE-MINT`), so a budget sized for machine-driven
  traffic would be four times larger than any plausible use.
- `MaxInvites = MaxRetainedBytes / MaxRecordBytes = 8192` — the hard cap on retained records. It
  **fails closed and evicts nothing**, exactly as `idem.ErrCapacity` does, and is easier to justify
  here: an evicted invite is an unknown, unredeemable one, so eviction could never produce a second
  redemption — it would only make a live invite silently stop working, and a refused mint is loud and
  recoverable instead.
- `SpentRetention = idem.RetentionWindow` (50h10m22s) — cited BY REFERENCE from `IDEM-11`'s own
  derivation, not re-derived, because the retry it must outlive (a client that never saw its
  acknowledgement and is still retrying) is the same kind of retry `idem`'s window already covers.
  It is also the diagnosis window: an expired or revoked record is kept past the moment it stopped
  working, not dropped at that instant, so `ErrExpired`/`ErrRevoked` stay reachable by an operator
  chasing a failed enrolment instead of collapsing to `ErrUnknownInvite`.
- `DefaultTTL = 24h`, `MaxTTL = 7 * 24h` — an invite must survive the ordinary gap between an
  operator minting it and an agent using it, and no longer than it has to, because the invite blob is
  a bearer credential travelling over a channel this bus does not control (`DECISIONS.md`, E6). A TTL
  requested over `MaxTTL` is **REJECTED (`ErrInvalidTTL`), never clamped**.
- `ReservationTTL = 30s` — how long `Store.Begin`'s reservation may be held before `Consume` without
  being reaped; roughly two orders of magnitude above the two fsyncs and one id mint the path actually
  costs. It sweeps ONLY before `Consume` — after a successful `Consume` the reservation is no longer
  sweepable, because the caller may already have committed a durable consumption record, and reaping
  it back to open would admit a second redemption of an invite the log already says is spent. After
  `Consume` only `Commit` or `Abort` resolves it, and an abandoned one stays locked until restart,
  which is fail-closed.

**THE FAIL-CLOSED RULE, AND ITS ONE HONEST EXCEPTION** (doc.go section 5, not flattened here). Dropping
a whole invite makes it UNKNOWN, and an unknown invite is rejected — that covers expiry, the capacity
bound, and a mint record discarded at replay. Such a drop can cost availability (a still-valid invite
becomes unusable) and the ability to answer a retry with its original result. **It cannot produce a
second redemption.** The exception: losing a SPEND record while its MINT record survives is
**FAIL-OPEN** — the invite is then present and OPEN, and it can be redeemed again. The two ways to
reach it are a consumption record that will not decode at replay (`Store.Apply`) and one refused by
the monotonic upsert for disagreeing with the existing record's identity; both are logged at ERROR
naming the invite. Neither is reachable by corruption alone — `internal/wal` verifies every frame with
a keyed HMAC-SHA256 and discards what fails, so a record that decodes is a record this bus wrote,
which leaves a bug in this package as the realistic cause. It is bounded, not eliminated, and an
operator seeing that ERROR line should revoke the named invite rather than assume the discard was
safe.

**No new HTTP route, CLI flag, env var, or header was introduced by this task.** `INVITE-GATE` owns
the `POST /v1/enroll` wire shape that will present an invite secret; `INVITE-MINT`/`INVITE-REVOKE` own
the operator-facing surface for minting and revoking one. Nothing in this section is reachable by an
agent yet.

## The message record is version 2 — a DESTRUCTIVE, BIDIRECTIONAL break (SIGN-2/SIGN-6, 2026-08-07)

**`store.RecordVersion` is `2`.** It was `1`, and every passage in this file that still says `1` for
the MESSAGE record is stale; the one such passage above has been corrected in place. The value was
**RESERVED from the Spec Server `store-record-version` namespace on 2026-08-07** — value `1` was
seeded in the same pass to cover the already-shipped v1 record, so the namespace describes the whole
history rather than starting at the bump. It was not picked by eyeballing the constant.

**This is the destructive part, and it is the first thing an operator needs.** `store.Decode` refuses
any record whose `v` is not exactly `RecordVersion`, so:

- **Upgrade:** every `"message"`-kind record already on disk is `v:1`. On the first start under the
  new binary, recovery refuses each one and `Hub.Apply` **discards it, LOUDLY** (ERROR, naming the
  record). **A bus upgraded across this boundary loses its entire message history.** That is not a
  bug to be reported and there is no migration flag: a pre-SIGN-6 message carries no signature and no
  sender timestamp, and a v2 record REQUIRES both, so there is no value to migrate them to. Inventing
  one — a zero signature, a synthesised timestamp — would manufacture a record that looks signed and
  verifies as nothing, which is the exact silent failure invariant 9 warns about.
- **Rollback:** the break runs BOTH ways. An old binary reading a v2 log discards every v2 message
  record for the same reason. Rolling back is therefore not a way to get the history back; it is a
  second discard. Take a copy of the data directory before upgrading if the history matters.

**What is NOT affected** (say it explicitly so nobody widens the blast radius): `auth.RecordKind =
"agent"` (enrolment — still `auth.RecordVersion = 1`), `invite.RecordKind = "invite"`, and the new
`"seqfloor"` kind below. The enrolment roster, the invite store and the sequence floor all survive
the upgrade intact. An agent does **not** have to re-enrol because of this bump.

**The two new fields**, both REQUIRED — `store.NewMessage` refuses to construct without them, and
`store.Decode` refuses to read without them, because `Decode` is the boundary for bytes this process
did not validate:

| field | JSON key | Go type | on-disk encoding | rule |
| --- | --- | --- | --- | --- |
| `TimestampUnixMilli` | `timestamp_ms` | `int64` | plain JSON number | the **SENDER's** clock, Unix milliseconds UTC. Must be `> 0`. It is **covered by the signature**, which is precisely why the bus stores the sender's value verbatim and never substitutes its own |
| `Signature` | `signature` | `[]byte` | standard base64 (what `encoding/json` does with `[]byte`) | exactly `signing.SignatureSize` (64) bytes. Carried as **OPAQUE BYTES**; this bus never verifies it |

`SentAtUnixNs` (`sent_at_unix_ns`) is unchanged and is a **different fact**: it is this bus's clock
and is **NOT** covered by the signature. The two are not interchangeable and conflating them makes
every verification fail. `store.Message.SigningMessage()` is the ONE place the mapping from a stored
message to the bytes its signature covers is written down — read it rather than re-deriving the
field order.

### A fourth `Entry.Kind`: `"seqfloor"` — the durable sequence floor

**Nothing was reserved, and nothing needed to be.** `wal.Entry.Kind` is a free-form APPLICATION
STRING, exactly as this file already records for `"agent"` and `"invite"` above; the reserved
NUMBERS are `wal.Type`'s framing values, which are untouched. `hub.SeqFloorRecordKind = "seqfloor"`
now sits alongside `"message"`, `"agent"` and `"invite"` in the same log, and every applier still
silently skips the kinds it does not own.

| | |
|---|---|
| `Entry.Kind` | `"seqfloor"` (`hub.SeqFloorRecordKind`) |
| `Entry.Body` | `{"v":1,"floor":<uint64>}` (`hub`'s unexported `seqFloorRecord`, `seqFloorRecordVersion = 1`) |
| Meaning | **every sequence `<= floor` is BURNED. This bus will never issue any of them again.** |
| Written | AHEAD — fsynced BEFORE any sequence above the currently-proven floor is handed to a client |
| Batch | `hub.MintBatchSize = 256`. One floor record burns 256 numbers, so the cost is one extra fsync per 256 mints and **zero on the send path itself** |
| Frames | the ordinary PREPARE/COMMIT pair — no new frame shape, no new fsync discipline |
| Undecodable at replay | **skipped LOUDLY at ERROR** (invariant 6: discard is sanctioned, SILENT discard is the defect) |

`hub.Open` derives the resume floor as the **maximum** of FOUR sources — the durable
`message-seq-floor` FILE, `Recovered.NextIndex - 1`, the highest floor from replayed `"seqfloor"`
records, and the highest applied message sequence — then seals. Every term can only raise the floor,
never lower it.

**CORRECTED 2026-08-07: this record is DEFENCE IN DEPTH, not the guarantee.** The three sources
listed above are all read OUT OF THE LOG, and a QUARANTINE discards the log — so all three collapse
together on exactly the start where the floor matters most. The authoritative source is the durable
`<data-dir>/message-seq-floor` FILE (on-disk format version 5), which lives outside the log; see
"the durable MESSAGE-SEQUENCE floor" above for the whole argument, the failure modes and the
measured evidence. The "Written AHEAD" row below describes this RECORD's ordering, which is real but
is not by itself what survives deep damage.

**The operator-visible consequence: SEQUENCE NUMBERS NOW ADVANCE IN JUMPS.** A restart typically
resumes at the next multiple of 256, and a mint that is never spent leaves a permanent hole.
**This is CORRECT, not corruption.** `internal/ids/sequence.go` already states the rule consumers
must hold to: **treat the sequence as strictly increasing, never as dense.** Anything that infers
"messages were lost" from a gap was already wrong; it is now visibly wrong. A gap costs nothing —
invariant 1 (ids are never reused, including across restarts) beats gap-freeness, and this record is
what buys it across the mint.

**The mint TABLE is deliberately NOT durable, only the burned NUMBER is.** Which
`(agent, op, idempotency_key)` holds which sequence lives in memory
(`hub.MaxOutstandingMintsPerAgent = 64`, `hub.MaxOutstandingMints = 8192`,
`hub.MintTTL = 15m`). A restart therefore invalidates every outstanding reservation: the client's
next `POST /v1/send` gets `409` (`hub.ErrUnknownMint`), re-mints under the SAME idempotency key,
gets a FRESH sequence, re-signs and re-sends. The old number stays burned. This cannot double-apply:
if the crash landed after the message became durable, the re-sent request carries the same key and
the same fingerprint, so `internal/idem` answers `OutcomeRetry` and replays the ORIGINAL result.

**No new on-disk FILE, no `ondisk-format-version` bump, no new `record-type` number, and no new HTTP
route, CLI flag, env var or header is introduced by the `"seqfloor"` RECORD itself.** The HTTP
surface this wave DID change (`POST /v1/mint`, `POST /v1/send`, `POST /v1/broadcast`) is in
`CONTRACTS-HTTP.md`. *The same wave DID also add an on-disk file and format version — the
`message-seq-floor` FILE at `ondisk-format-version` 5, documented in its own section above. The
sentence scopes to the record, not to the wave.*

## Three new files in the data directory: the bus certificate and its two keys (MTLS-BUSCERT, `internal/buscert`, added 2026-08-07)

`internal/buscert` mints and loads the bus's TLS identity — the **bus certificate** it presents in
every handshake — plus a second, independent key that attests agent key bundles to peer buses. All
three files live directly in `<data-dir>`, alongside the existing `bus.lock` and `wal-mac.key`.

| file | mode | secret? | encoding | contents |
| --- | --- | --- | --- | --- |
| `bus-tls.crt` (`buscert.CertFileName`) | `0644` | PUBLIC | one PEM `CERTIFICATE` block | the DER of the self-signed **bus certificate** (leaf) whose fingerprint clients pin |
| `bus-tls.key` (`buscert.TLSKeyFileName`) | `0600` | SECRET | one PEM `PRIVATE KEY` block, PKCS#8 Ed25519 | the private key inside the bus certificate; authenticates the CONNECTION |
| `bus-signing.key` (`buscert.SigningKeyFileName`) | `0600` | SECRET | one PEM `PRIVATE KEY` block, PKCS#8 Ed25519 | a SEPARATE key that attests agent key bundles; PINNED BY PEER BUSES at peering |

`bus-tls.crt` is public **by construction** — it is sent to every client on every handshake and its
fingerprint is already published in invite blobs — so `0644` is not a security-relevant relaxation;
forcing `0600` would only collide with an operator-supplied certificate mounted `0644` (E7). Both key
files are `0600` and are refused outright if any group-or-other permission bit is set.

**Why two keys, not one:** `DECISIONS.md` 2026-08-07 ("The bus TLS key and the bus SIGNING key are
SEPARATE") — the two rotations have incompatible blast radii. Rotating the TLS key affects only this
bus's own clients; rotating the signing key invalidates the pins held by **every peer bus**, a
federation-wide event. The failure domains are also kept apart: a compromised TLS key can only
impersonate the bus to its clients, while a compromised signing key can forge attestations for every
agent on the bus — strictly worse. Sharing one key would drag every routine TLS rotation up to
federation-wide cost, and the two are generated from two independent `ed25519.GenerateKey` calls;
neither is ever derived from the other.

**Three long-lived secrets now live in the data dir, not one:** `wal-mac.key` (owned by
`internal/wal`), `bus-tls.key` and `bus-signing.key`. **A backup that copies the logs but omits any
one of the three restores a bus that cannot do its job:**

- without `wal-mac.key`, no record in the WAL or the audit log verifies;
- without `bus-tls.key`, the bus cannot present the certificate its clients already pinned;
- without `bus-signing.key`, every attestation this bus ever made is unverifiable, and every peer's
  pin on it is dead.

None of the three can be regenerated in place — see the generation rule below.

**Generation happens exactly once, on a virgin data directory.** `buscert.LoadOrCreate` mints fresh
material only when all three files are ABSENT. Any other split — some present, some missing — is
FATAL (`buscert.ErrIncomplete`), and the error names exactly which files are present and which are
missing. A missing file is **never** regenerated next to surviving ones: a fresh TLS key silently
breaks every client that pinned the old fingerprint (there is no TOFU — E6), and a fresh signing key
invalidates every peer bus's pin, a federation-wide event. A crash mid-first-generation lands in this
same `ErrIncomplete` state; the only remedy is an OPERATOR removing the named files by hand and
restarting to mint a clean set — the bus never deletes them itself.

**The fingerprint construction is fixed:** `sha256.Sum256(cert.Raw)` — the DER of the leaf, not a
re-encoding — rendered as **lowercase hex**. This is the same construction already durable in
`internal/invite.Record.CertFingerprint` and `internal/auth.CertBinding.Fingerprint`; a digest over
the SPKI, or over a PEM re-encoding, would be a second, incompatible identity for the same
certificate, so this package offers exactly one.

**Certificate validity: 365 days** when self-generated (`DECISIONS.md`, MTLS-DESIGN: "Both the bus
TLS certificate (`MTLS-BUSCERT`) and the client TLS certificate (`MTLS-CLIENTCERT`) default to 365
days when self-generated"). `NotBefore` is backdated 5 minutes to absorb clock skew between bus and
client at first contact. A certificate found outside its validity window at load is FATAL
(`buscert.ErrExpired`) — the bus refuses to start rather than silently regenerate (which would break
every existing pin) or start anyway (which would fail every handshake with nothing in this bus's own
logs to explain why). Rotation — E3's two-certificate rollover — is a separate, not-yet-written task.

**Other fatal load conditions**, briefly: a key file readable by group or other
(`buscert.ErrPermissions`); a key or certificate path that is not a regular file; malformed or
truncated PEM; the wrong PEM block type; more than one PEM block in a file; a non-Ed25519 key; or a
private key that does not match the certificate — all `buscert.ErrMalformed`.

**Status: WIRED — a running bus produces these three files** (`MTLS-BUSCERT`, 2026-08-07). This
paragraph previously read "UNWIRED: nothing imports `internal/buscert` yet", which was true and is
now false. `cmd/agent-bus/buscert.go`'s `openBusCertMaterial` calls `buscert.LoadOrCreate` from
`run()`, positioned AFTER `dirlock.Acquire` (generation WRITES to the data dir, and a start refused
at the lock must still touch nothing but `bus.lock` — `TestRunRefusesALockedDataDir`), AFTER
`ids.LoadOrCreateBusID` (the bus id is the certificate's descriptive CommonName), and BEFORE
`wal.Open` (fail-fast: a start that will refuse over unusable key material should refuse before
recovery has repaired or quarantined a byte of the log). Verified on a throwaway data directory: a
first start leaves `bus-tls.crt` `0644`, `bus-tls.key` `0600` and `bus-signing.key` `0600` beside
`wal-mac.key`.

**The `-listen` host becomes a subject alternative name.** `certHosts` adds the host half of
`-listen` to the loopback set `internal/buscert` always includes, because a client checks the name it
DIALLED against the SANs (Go dropped the CommonName fallback in 1.15). Wildcard binds — `:8080`,
`0.0.0.0:…`, `[::]:…` — contribute nothing and are dropped. **SANs are fixed at generation and
material is never re-minted**, so moving a bus from a loopback bind to a public address after its
first start leaves a certificate that does not name the new address; the remedy is the same
deliberate operator act as any other certificate change, not a restart with a different flag.

**Two operator-visible log lines, and their levels are contract** (asserted by
`cmd/agent-bus/buscert_test.go`):

```
level=warn msg="bus certificate and signing key GENERATED: …"  data_dir=… cert=<dir>/bus-tls.crt fingerprint=<64 lowercase hex> not_after=<RFC3339>
level=info msg="bus certificate and signing key loaded"        data_dir=… cert=<dir>/bus-tls.crt fingerprint=<64 lowercase hex> not_after=<RFC3339>
```

The `warn` line fires **exactly once per data directory** — it is the only notice an operator gets
that the fingerprint every client must pin has come into existence, and seeing it twice on one
directory means the material was lost and re-minted. **Neither line carries private key MATERIAL** —
and it does not claim to hide the key PATHS: the WARN names both key filenames in its prose and logs
`data_dir` as a field, so the paths are trivially reconstructible. That is deliberate and harmless
(the filenames are fixed public constants, documented here), because an operator being told which
files to back up is the whole point of the line. The `server started` line gained two fields:
`bus_cert_fingerprint=<64 lowercase hex>` (public by construction — this is the value handed to
clients to pin) and `tls=false`.

**A crashed first generation can leave an orphan `bus-*.key.tmp-*` file, and nothing sweeps it.**
`buscert`'s writer creates each file as a same-directory temp, `fchmod`s it to its final mode, writes
and fsyncs it, then renames; a crash between the write and the rename leaves a `0600` file holding a
real, never-used Ed25519 private key. It is inert — no start ever reads it, and the directory is
still `ErrIncomplete` until an operator resolves it — but it is a live private key that will be
picked up by a backup. Delete any `bus-tls.key.tmp-*` / `bus-signing.key.tmp-*` / `bus-tls.crt.tmp-*`
you find; treat it as key material until it is gone.

**`tls=false` is not a placeholder to be edited; it is the state.** This task GENERATES AND LOADS
ONLY. `internal/httpapi` is unchanged, `http.Server` still has no `TLSConfig`, and the listener is
still `net.Listen("tcp", …)` + `srv.Serve(ln)` — a plaintext `GET /healthz` behaves exactly as it did
before. Serving TLS is `MTLS-LISTENER` and must NOT land before a client can speak it
(`MTLS-CLIENTCERT`): server-side enforcement ahead of client-side capability is a total outage, and
this repo has shipped one already. `cmd/agent-bus`'s `TestCmdDoesNotServeTLS` is an AST-level guard
on that constraint and is to be DELETED by `MTLS-LISTENER`, never weakened.

**Failure mode:** every unusable state is FATAL. `run()` returns an error, `main()` prints it to
stderr prefixed `agent-bus: ` and exits `1`, and nothing binds a listener:
```
agent-bus: preparing the bus certificate and key material: the bus certificate and key material in "<data-dir>" could not be loaded, and it is NEVER regenerated over a partial or unusable set (…): buscert: <path> …
```
A refused start regenerates nothing and deletes nothing: the surviving files are byte-identical
afterwards and a missing one stays missing.

**No new HTTP route, CLI flag, env var, record type or `ondisk-format-version` is introduced**, and
there is still no agent-facing surface — no `scripts/bus-*.sh` wrapper and no `AGENT_PROTOCOL.md`
entry, because agents never see a bus's TLS or signing key material directly.

## `RetentionWindow`'s `PeerOutageBudget` term is now an ENFORCED constraint, not a stated one (RELAY-4, added 2026-08-08)

The `PeerOutageBudget` (24h) term cited above (line 572 area, `internal/idem/retention.go`) was
documented as a constraint RELAY-4 had not yet been built to satisfy. **That is no longer true.**
RELAY-4 (peer-down retry/backoff, `internal/relay/forward.go`) now enforces the bound structurally:
`NewForwarder` REFUSES TO CONSTRUCT unless `RetryHorizon + Timeout <= idem.PeerOutageBudget`
(`RetryHorizonCeiling`, cited by reference so the two constants cannot drift apart). The default is
`DefaultRetryHorizon` (`23h59m30s`) plus `DefaultForwardTimeout` (`30s`) = exactly `24h`.

The retry deadline is anchored on a job's **enqueue instant** (`enqueuedAt`), never on its first
attempt — this is what keeps a per-peer queue that drains serially from stacking two horizons back
to back behind one dead peer.

**Why an operator should care:** a retry that lands after its horizon has elapsed is applied as a
NEW operation and the message is DELIVERED TWICE, by the same "duplicates are suppressed only
within the retention window" rule stated above — so `PeerOutageBudget` and the forwarder's retry
horizon are one bound, not two, and anyone raising either MUST move both together.

Asserted by `go test -race -run TestPeerRetryBackoffHorizonStaysInsideTheOutageBudget ./internal/relay`.

**Not yet reachable.** `internal/relay` is NOT registered on any mux (`TestHandshakeHandlerIsNotWiredIntoAnyMux`)
— it is gated behind INVITE-PEERGUARD and MTLS-RELAYGUARD, so none of this is live/observable from a
running bus yet. The outbound queue this retry logic drains is still IN-MEMORY with no durable
outbox, so cross-bus delivery remains BEST EFFORT, not reliable, even once wired.
