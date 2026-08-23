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
| `ondisk-format-version` | `1` (legacy, read-only), `2` (current WAL frames), `3` (`agent-suffixes`, `internal/ids/suffixstore.go`), `4` (`wal-index-floor`, `internal/wal/indexfloor.go`), `5` (`message-seq-floor`, `internal/hub/seqfloorfile.go`), `6` (`peer-withdrawal-floor`, `internal/relay/peerstore.go`), `7` (authenticated WAL checkpoint generations, `internal/wal/checkpoint.go`) | **The namespace covers every ON-DISK FILE FORMAT this project defines, not only the WAL frame layout.** `1`/`2` are the WAL file/frame layout (`internal/wal/format.go`'s `FormatVersion`); version 2 replaced the unkeyed CRC32C of version 1 with a keyed HMAC-SHA256 (DUR-12). `3` is the per-name agent-id suffix floor file's own format (see "The durable applied-key store" and neighbouring sections below). `4` is the durable WAL record-index floor file's format (see the 2026-08-07 subsection below). `5` is the durable MESSAGE-SEQUENCE floor file's format — a DIFFERENT counter from `4`, and the distinction is load-bearing (see the `message-seq-floor` subsection below). `6` is the durable PEER WITHDRAWAL floor file's format (RELAY-34, reserved 2026-08-08 — see the `peer-withdrawal-floor` subsection below). `7` is the authenticated checkpoint-generation directory/manifest/snapshot format described below; its tail still uses WAL frame version 2. |
| `cursor-format-version` | `1` (superseded), `2` (current, `internal/hub/cursor.go`) | The version prefix inside the OPAQUE read cursor. **Not an on-disk server format** — the cursor is a client-held token — but it is reserved from the same ledger because two agents bumping it independently would produce two incompatible `v2`s. `1` was the sequence-based cursor; `2` is the delivery-position cursor introduced by `SIGN-1-FU-REORDER-WATERMARK` (2026-08-14). A cursor carrying an unrecognised version is **accepted and remapped to position 0**, never rejected — see `CONTRACTS-HTTP.md`. Value `1` was allocated retrospectively to cover the already-shipped v1, exactly as `store-record-version` was seeded. |

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

  **Since 2026-08-14 (SIGN-1-FU-REORDER-WATERMARK) a reissued WAL record index costs more than
  reissued ids.** The WAL commit index is now also the message DELIVERY POSITION — what every client
  cursor points at. So resuming the index below numbers already handed out does not merely risk a
  duplicate message id: it makes every message committed in the reissued range **silently and
  permanently undeliverable** to any reader whose cursor already sits above that range. Those messages
  are durable, they are in the audit trail, and they are never handed to that reader and never wake
  its long poll. Nothing downstream can detect it, and unlike a duplicate id it is invisible in the
  log. Treat deletion of `wal-index-floor` as forfeiting delivery for the affected range as well as
  forfeiting invariant 1. The matching operator-facing remedy in `internal/wal/indexfloor.go`'s
  `indexFloorCorrupt` says the same thing, so the file and the error message agree.

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

## The DELIVERY POSITION, and why it is not on disk (2026-08-14, SIGN-1-FU-REORDER-WATERMARK)

Since SIGN-1 a message carries **two** server-minted numbers, and conflating them is the defect
`SIGN-1-FU-REORDER-WATERMARK` fixed:

| | `Seq` — the sequence | `Pos` — the delivery position |
|---|---|---|
| minted when | the client RESERVES (`/v1/mint`) | the record COMMITS |
| minted by | `internal/ids.Sequence`, floored by `message-seq-floor` | `internal/wal` — it IS the commit record's WAL index |
| signed by the client | **yes** | no |
| monotone in ARRIVAL order | **no** — reservations may be spent out of order | **yes**, by construction |
| what it is for | IDENTITY (`<bus-id>-<seq>`, invariant 1) | DELIVERY ORDER — what a cursor points at |
| **on disk** | yes, in the message record | **NO — see below** |

**`Pos` is NOT stored in the message record, and `store.RecordVersion` did NOT move for it.** It is
*derived* from where the record sits in the log: the hub stamps it from `wal.Committed.CommitIndex`
on the live path and from the same field on the recovery path, where replay runs in commit order. A
number that is already implied by a record's own position in an append-only log does not need to be
written into that record, and writing it would be a format change plus a second copy that could
disagree with the first.

**Consequence for recovery, and it is the reason this is the WAL index rather than a counter of our
own:** a store-local counter incremented per successfully-applied record would NOT be stable across
a restart. Recovery skips records it cannot decode, and the set it skips differs between runs (a
schema bump skips all of them). Every skip would shift every later position down by one, so a
persisted client cursor would silently point one message further on and SKIP a message — the very
defect being fixed, reintroduced through the recovery path. The WAL index never moves and is never
reused, so a position means the same thing before and after a restart.

Positions are **sparse and that is correct**: seq-floor records, enrolment records and invite
records all consume WAL indices without being messages, so a reader's cursor jumps. Cursors need
order, not density — the same rule `internal/ids/sequence.go` states for the sequence.

**Nothing may hand two messages the same position.** The one place `CommitIndex` is not unique at
the applier boundary today is a composite entry expanded to several appliers
(`internal/auth/inviteenrol.go`'s multiplex path); none of those records carries a message, and none
ever may. `store.Append` enforces the rule directly — a position at or below the highest already
appended is logged at ERROR as an invariant-1 incident.

## `OriginMessageID` — the relay correlation key (`RELAY-24-FU-STOREMSGLOOKUP`, added 2026-08-15)

`store.Record` gains one new field: **`origin_message_id`**, Go tag `json:"origin_message_id,omitempty"`
(`internal/store/message.go`). It is the CORRELATION KEY linking a message this bus retains back to
the message id it carried on the bus that originated it — set only on a message ingested over a
relay hop; empty (and therefore absent from the JSON, via `omitempty`) on a message this bus
originated itself, which is every record any build wrote before this change.

**This is a third, deliberately distinct notion from `Seq` and `Pos`** (see "The DELIVERY POSITION,
and why it is not on disk" above, which lays out the same distinction between those two).
`OriginMessageID` takes part in **no ordering, no cursor and no retention decision** — it is not
compared, sorted or binary-searched on anywhere; it exists purely so a relayed message can be looked
up again after a restart, by `Store.ByID` (local id → message) and `Store.ByOriginMessageID` (origin
id → local message, falling back to `ByID` for the locally-originated case where `ID` already **is**
the origin id). `Store.ByID`'s value is a `Pos` used strictly as a LOCATOR into the retained-window
index, never as a second delivery position.

**`store.RecordVersion` DELIBERATELY STAYS AT 2. No number was reserved from the Spec Server
`ondisk-format-version` namespace for this change, and that is not an oversight:**

> `RecordVersion`'s own doc says an added OPTIONAL field does not move it, and `Record` decoding is
> non-strict about unknown fields. Bumping to 3 would have been **actively harmful**: `Record.Decode`
> does an EXACT version match, so it would discard all existing message history on upgrade.

A future maintainer who sees a new field with no version bump and "fixes" it by reserving one and
bumping `RecordVersion` would make every existing data directory's history undecodable at the next
start — `Decode` refuses on `rec.V != RecordVersion` with no back-compat tolerance for a lower
number. See `DECISIONS.md`, 2026-08-15, for the full ruling (the reviewer considered and rejected
the bump).

**Compatibility, both directions, and it is the reason the field is safe to ship this way:**
- An OLD build reading a NEW record simply never sees the field (non-strict unknown-field decoding)
  and serves the message normally — it only loses the relay correlation, which it has no use for
  anyway.
- A NEW build reading an OLD record gets `OriginMessageID == ""`, which is the CORRECT answer, not a
  loss: a pre-relay bus originated everything it holds, so "no recorded origin" and "this bus is the
  origin" coincide exactly.

**Operator impact: rebuild the binary. No re-enrolment, no migration, no new file.** The field rides
inside the existing message record — same PREPARE frame, same fsync, same `bus.wal` — so there is
nothing to migrate and no new on-disk file is introduced by this change.

**Duplicate origin ids are resolved last-writer-wins, not refused, and the resolution is
peer-triggerable — see `DECISIONS.md`, 2026-08-15, for the full reasoning and the known throttle
limitation** (`RELAY-24-FU-STOREMSGLOOKUP-THROTTLE`). In outline: `store.Append` retains BOTH
messages (refusing after the record is already fsynced would orphan committed data, invariant 4),
points the `byOrigin` index at the newer one, and counts every occurrence in the exported
`Store.DuplicateOriginMessageIDs() uint64`, while the operator-facing log line itself is emitted at
most once per process.

## `OriginAttestation` — the relayed message's origin binding (`RELAY-48`, added 2026-08-16)

`store.Record` gains one more optional field: **`origin_attestation`**, Go tag
`json:"origin_attestation,omitempty"`, a `*attest.Attestation` (`internal/store/message.go`). It is
the ORIGIN bus's signed binding of the message's `sender` to that agent's MESSAGING public key,
carried VERBATIM from the hop that delivered the message. Like `origin_message_id` it is present
ONLY on a message this bus INGESTED over a relay hop, and absent (`omitempty` keeps it off disk
entirely) on every message this bus originated itself.

**Shape and size.** It serialises `attest.Attestation`'s own JSON: `agent_id`,
`messaging_public_key` (32 raw bytes, base64), `key_epoch`, `issued_at_unix_ms`, `not_after_unix_ms`,
`signature` (64 raw bytes, base64). `agent_id` is bounded by `ids.ParseAgentID` and the two byte
fields are fixed-width, so the field is **under 300 bytes** and scales with nothing. It does NOT
count towards `Message.Size()`, which remains `len(Body)` — the number the audit trail records and
retention accounts against.

**Why it is on the message record and NOT on `relay.OutboxRecord`** (`DECISIONS.md`, 2026-08-16):
the attestation attests THE MESSAGE, while an outbox record is per-HOP, so putting it there would
store one copy per pending hop of a single fact — the same "second copy free to disagree with the
first" argument that keeps `Pos` off this record. It is also the cheaper half of the trade, since the
outbox route is on-disk surface that would need a reserved number, but that is the tiebreak and not
the reason.

**Why it must be durable at all.** It is the ONE field of an ONWARD relay envelope this bus cannot
regenerate: `attest.Sign` refuses a subject in another bus's namespace (invariant 2), so a
relayed-in envelope was previously **unbuildable from durable state by construction**. Its only
reader, `relay.Forwarder.Resume`, runs ONLY after a restart — so a value held in memory alone is
empty at the one moment it is read, and a pending onward hop was settled `abandoned` after this bus
had already answered the upstream peer **200**.

**It is METADATA, and it does not reach the audit trail.** It names an agent, a public key, an epoch
and two timestamps; it carries no part of the message content, so invariant 6 sanctions it. It is
absent from `wal.AuditRecord`, which `internal/hub/audit.go` assembles field by field. It also does
not move either hash: `store.Message.SigningMessage()` omits it and `auditContentHash` derives from
`SigningMessage`.

**`store.RecordVersion` DELIBERATELY STAYS AT 2, and no number was reserved** — the same rule, and
the same warning, as `origin_message_id` above: `RecordVersion`'s own doc says an added OPTIONAL
field does not move it, `Decode` is non-strict about unknown fields, and `Decode`'s EXACT version
match means a bump to 3 would discard all existing message history on upgrade.

**Validation, applied identically on both paths.** `store.validateOriginAttestation` is the one
definition, called by `Message.WithRelayOrigin` on the write path and by `store.Decode` on the
recovery path, so a restart can never load state the write path would refuse to create. It requires
`attest.Canonicalize` to accept the value, the signature to be exactly `signing.SignatureSize` bytes,
and the subject to BE the message's sender (which is what the next hop checks). This package NEVER
verifies the attestation's signature: that needs the origin bus's peering-time pinned signing key,
which lives in the relay peer store, and `relay.ValidateRelayRequest` has already done it before the
hub is reached. `Decode` additionally REFUSES a record carrying an attestation with NO
`origin_message_id`; the converse — an origin id with no attestation — decodes and serves normally,
and costs only the onward hop of that one message.

**Compatibility, both directions:**
- An OLD build reading a NEW record ignores the field (non-strict unknown-field decoding) and serves
  the message normally.
- A NEW build reading an OLD record gets no attestation, which is the correct answer for every
  message a pre-relay bus originated. For a message a pre-`RELAY-48` build relay-INGESTED it is a
  genuine loss, bounded to that message's ONWARD hop: `recoverRelayEnvelope` refuses to rebuild an
  envelope for it by name, and the forwarder settles that one job `abandoned` with the reason logged
  (invariant 6 — loud, not silent). Local delivery of such a message is unaffected.

**Operator impact: rebuild the binary and restart the bus. No re-enrolment, no migration, no new
file.** The field rides inside the existing message record — same PREPARE frame, same fsync, same
`bus.wal`. Existing logs and enrolments are unaffected. The behaviour only becomes live for messages
ingested by the NEW binary: a relayed message already on disk carries no attestation and its onward
hop remains unrecoverable.

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
fair share      : maxEntries / (buckets + 1)        buckets = local agents plus foreign bus halves
admission       : not under pressure                     -> admit
                  under pressure and held >= fair share  -> refuse (idem.ErrAgentQuota / hub.ErrAgentQuota)
                  otherwise                                -> admit
```

`idem.PressureLine` is exported for exactly this citation — it names the fill level at which the
table's FREE space stops exceeding its USED space (a derived crossover, not a chosen round number).
**Below the pressure line, nothing changes**: a bus that never approaches its cap sees no behaviour
difference from this rule at all.

The `+1` in the divisor is the identity bucket that has not arrived yet. With no phantom bucket, a
lone identity's share would be the whole table, so the exact attack the rule exists to close — one agent,
acting alone, filling the table before any victim holds a single record — would pass straight through:
the victim cannot be counted in a bucket it holds nothing in, precisely because it is the one being
starved. The phantom slot reserves its room before it exists.

**Foreign senders are counted by bus, not by asserted agent label**
(`RELAY-FU-IDEM-METER-BY-PEER`, 2026-08-15). A local sender keeps its complete fully-qualified agent
id as its bucket; every foreign sender is assigned to the case-folded bus half of that id. The
record's lookup scope remains the complete agent id, so duplicate detection and result replay do not
become bus-wide. This is accounting only: an honest hundred-agent foreign bus receives the same
share as a one-agent bus, and one busy or hostile agent can consume its bus-mates' shared allowance
until keys age out. The trade prevents a peer from manufacturing an unbounded fair-share divisor by
asserting fresh agent labels.

This is a bound only because relay ingress separately binds `Sender`'s bus half to `OriginBus`, pins
that origin to the authenticated peer, and validates the traversed path. Parsing a fully-qualified id
proves syntax, not authority; a future ingress path that bypasses those checks invalidates the bound.
The production hub therefore constructs the store with a required, validated local BusID and fails
startup on an empty or malformed value. No relay-specific principal was added to the stored record:
recovery re-derives the same local-agent or foreign-bus bucket from the already-persisted full agent
id, preserving the live denominator without an on-disk format change.

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
`TestReplayNeverRefusesWhatTheLivePathAccepted` (`internal/hub/idem_quota_test.go`). Fair-share
counters ARE rebuilt during recovery, using the same complete local-agent or case-folded foreign-bus
bucket as the live path. An identity bucket recovered above its share therefore stays frozen —
refused until its keys age out — exactly as it would have been pre-restart; only the ADJUDICATION at
replay time is skipped, not the bookkeeping.

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
`roster.go` (`RosterEntry`, `CertBinding`, `MaxCertBindings`), `certbind.go` (the certificate-binding
write check and lookup, added by `MTLS-BIND` 2026-08-14), `walroster.go` (`WALRoster`, the
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
 "enrolled_at":"<RFC3339Nano UTC>",
 "left_at":"<RFC3339Nano UTC>"}             // omitted unless this is a LEAVE (tombstone) record
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
| `cert_bindings` | `[]CertBinding` | array of `{"fp","bound_at","retired_at"}`, bounded to `MaxCertBindings` | `RosterEntry.CertBindings` is empty — **the enrolling connection presented no client certificate, or the agent enrolled before `MTLS-BIND` (2026-08-14)**. Both are ordinary; see below |
| `cert_bootstrap_idem` | `string` | same bounded idempotency-key syntax as the HTTP request | absent unless `MTLS-MIGRATE` appended the first client-certificate binding for a legacy identity |
| `cert_bindings[].fp` | `[32]byte` | **hex** (`encoding/hex`), the fingerprint `sha256.Sum256(cert.Raw)` | never (present in every element) |
| `cert_bindings[].bound_at` | `time.Time` | `RFC3339Nano`, UTC | never (present in every element) |
| `cert_bindings[].retired_at` | `*time.Time` | `RFC3339Nano`, UTC | the binding is LIVE (`RetiredAt == nil`) |
| `enrolled_at` | `time.Time` | `RFC3339Nano`, UTC | never |
| `left_at` | `*time.Time` | `RFC3339Nano`, UTC | **absent on a live enrolment (the ordinary case); present ONLY on a leave/tombstone record** — see below |

The encodings match the precedents already on disk rather than being picked per field: times are
`RFC3339Nano` in UTC exactly as `idem.Record` writes `committed_at`; the certificate fingerprint is
HEX, the same choice `idem` makes for its own `fp`; the public keys are BASE64 STANDARD ENCODING,
which is what the enrolment wire format already uses for the same bytes, so an operator reading the
log sees the same string the client sent. Each of the three late-arriving keys is `omitempty`, so **a
record for an enrolment that carries none of them is byte-for-byte the record a pre-INVITE, pre-MTLS
build would have written**, and each key appears on disk only once something actually populates it.

**The three fields were on disk from record 1 before anything wrote them, deliberately.** `msg_pub`,
`invite_id` and `cert_bindings` were declared and encoded while no code path populated them, per the
ordering rule in `DECISIONS.md`'s
2026-08-07 "ENROL-SHAPE" entry: nothing was persisted before AUTH-3, which made this the LAST MOMENT
the durable record could be shaped without a migration — and because an agent id is bound to a
keypair, a migration here is not a schema edit but a FORCED RE-ENROLMENT OF EVERY AGENT.

**`cert_bindings` IS WRITTEN NOW — `MTLS-BIND`, `818207d`, 2026-08-14 — and it cost NO on-disk format
change and NO migration.** This paragraph said "nothing writes them yet" until that commit made it
false for this field. What enrolment writes is exactly ONE binding, live (`retired_at` absent), whose
`bound_at` is the SAME instant as the record's `epoch` and `enrolled_at` — one event, three fields,
deliberately not three clock reads (`auth.newCertBinding`). `MTLS-MIGRATE` (2026-08-23) adds the second writer: `POST /v1/client-cert/bootstrap` appends a full `auth.RecordKind = "agent"` roster record for the same agent id with the identity fields unchanged, exactly one additional live certificate binding, and `cert_bootstrap_idem` set to the idempotency key that produced that first binding. It keeps `auth.RecordVersion = 1` and allocates no new WAL kind because `cert_bindings` was already in the reserved record shape and optional additive fields are permitted under this record version. Nothing retires a binding yet.

**Where the fingerprint comes from is the load-bearing part: the TLS CONNECTION, never the request
body.** `internal/httpapi`'s `WithClientCertificate` middleware reads `r.TLS.PeerCertificates[0]`,
`enrolCertFingerprint` hands it to `auth.EnrolRequest.ClientCertFingerprint`, and `auth.Service.Enrol`
binds that. **There is no wire field a client can set** — see `CONTRACTS-HTTP.md`'s `POST /v1/enroll`
request body, which lists every accepted key and does not include one. A client-supplied fingerprint
would be a claim anyone could make about anyone's certificate, and binding it would durably record a
fact that was never established (invariants 1 and 11).

**A record with NO `cert_bindings` is ORDINARY, not damaged** — the operator question this raises. Three populations have none and all three are healthy before migration: every agent enrolled before 2026-08-14; every agent that enrols over a connection presenting no client certificate (the listener only **requests** one, `tls.RequestClientCert`); and any enrolment whose certificate was outside its own validity window, which the middleware ignores rather than binds. `Decode` accepts the absent key, replay stores the entry, and `auth.RecordVersion` is **unchanged at `1`** — the key was reserved by ENROL-SHAPE, so no build reads a record differently than it did before. Read empty as "this agent has no certificate to cross-check against", never as "this agent is unauthenticated". `MTLS-MIGRATE` can later append that agent's first binding without changing its agent id.

**`left_at` IS WRITTEN NOW — `AUTH-4`, 2026-08-22 — and it too cost NO on-disk format change, NO new
`Entry.Kind` and NO `RecordVersion` bump.** It is the ONE field whose PRESENCE changes what the whole
record MEANS, not merely what it carries. Absent — every record before this task, and every ordinary
enrolment since — the record is an ENROLMENT and `WALRoster.Apply` inserts the agent. Present, the
record is a TOMBSTONE: it names an agent that has LEFT the bus (`POST /v1/leave`,
`CONTRACTS-HTTP.md`), and `Apply` REMOVES it from the serving roster instead. A tombstone carries the
full enrolment field set — it is built from the agent's own existing entry, `left_at` added — so it is
self-describing and reuses `Decode`'s whole validation; only its EFFECT on replay differs. It rides the
SAME `wal.Entry.Kind = "agent"` as an ordinary enrolment on purpose: the roster applier already owns
that kind, so a leave needs no new checkpoint participant, no new entry in the multiplex applier map,
and `EnrolmentSuffixesInWAL` folds it exactly like an enrol record when rebuilding suffix floors —
which is what keeps the departed agent's burned name-suffix visible so it is never re-issued
(invariant 1; see `PROTOCOL.md` §9). Like every other record in this log, a leave is an APPEND: it is
never written in place over the agent's original enrolment record and never truncates history
(invariant 6). A `left_at` present but reading the ZERO time is refused as malformed on decode
(`ErrInvalidRecord`) — a leave record carries the instant it happened, or it carries no `left_at` at
all; there is no third spelling.

**`left_at`'s downgrade consequence is the OPPOSITE of the generic case the paragraph below states,
because it is not on every record.** `msg_pub`/`invite_id`/`cert_bindings` are optional fields on
EVERY enrolment record, so an older binary meeting one it cannot decode discards THAT AGENT's
enrolment and the id must re-enrol. `left_at` appears ONLY on a tombstone — the agent's ORIGINAL
enrolment record, written before this task, decodes exactly as it always did. A binary built before
`AUTH-4`, reading a log a newer binary appended a leave tombstone to, cannot decode that ONE
tombstone record (`DisallowUnknownFields`), discards it at replay, and never applies the removal —
**the departed agent stays enrolled and reachable on the downgraded binary**, the opposite of "must
re-enrol". Downgrade past `AUTH-4` is unsupported, the same posture INVITE and MTLS took; this is
recorded here because "stays enrolled" is a materially different failure mode from "must re-enrol"
and an operator debugging a downgrade needs to know which one they are looking at.

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
- A DUPLICATE agent id KEEPS THE FIRST record and never overwrites, except for the single `MTLS-MIGRATE` update shape: the same `agent_id`, `name`, auth key, messaging key, invite id, epoch and enrolled-at, `left_at` absent, prior `cert_bootstrap_idem` empty, new `cert_bootstrap_idem` present and valid, and `cert_bindings` equal to the stored prefix plus exactly one additional live non-zero fingerprint. That narrow shape updates the serving roster and is logged at INFO. Every other duplicate is still logged at ERROR and dropped. An unrestricted overwrite would rebind a live identity to a different keypair — the worst outcome available on this path (invariants 1 and 3), since every DM addressed to that id would then route to the new key holder.
- An undecodable record is discarded LOUDLY, at ERROR, with its prepare and commit indices, and the
  bus still starts. This is invariant 6's recovery contract (2026-08-02): recovery always reaches a
  running server, damaged records are discarded, and the absolute requirement is that every discard is
  logged — availability over retention, not silence over damage.
- **Two live bindings for ONE fingerprint can come off disk, and the READ is what declines to pick
  (`MTLS-BIND`, 2026-08-14).** `Put` refuses a fingerprint already live on another agent
  (`ErrCertFingerprintBound`), but `Apply` deliberately does not run that check: refusing a record
  that is already durable would not un-write it, it would turn a damaged log into an outage
  (invariant 6, the same reasoning as the duplicate-id bullet above). So a log carrying two live
  holders of one fingerprint recovers into exactly that state, and `AgentIDForCertFingerprint` then
  answers `ErrCertBindingAmbiguous` — **naming the holders, sorted, and resolving to nobody** —
  rather than serving one key holder as a definite agent it may not be. The ZERO fingerprint is
  refused the same way (`ErrCertBindingUnknown`): `validateRosterEntry` checks a binding's `bound_at`
  and `retired_at` but **not** its `fp`, so a hand-edited or damaged record carrying an all-zero one
  decodes cleanly and is stored live, and the zero value is exactly what a caller holds when no
  certificate was presented.

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
field, but nothing populates it with a real fingerprint yet — `internal/httpapi`'s `handleEnroll`
calls `Consume(invite.Result{AgentID, Response})` and leaves this field zero). **Still true after
`MTLS-BIND` landed (2026-08-14):** that task bound the certificate on the AGENT record
(`auth.RosterEntry.CertBindings`, above), not on this one, so the INVITE record still carries no
fingerprint and still needs no schema change to start doing so.

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

## A composite `Entry.Kind`, `"agent+invite"`, and the WAL applier is now a multiplexer (INVITE-GATE, 2026-08-14)

The route described in `CONTRACTS-HTTP.md`'s `## Enrolment and sessions` section — `POST /v1/enroll`
now accepting optional `invite_id`/`invite_secret` — is composed on disk here. **Invite redemption is
now genuinely LIVE, and enrolment is still NOT gated**: an enrolment carrying NO invite is unaffected
and still accepted; see that file's "Known gaps" note, which this section does not repeat.

**One `wal.Entry`, one transaction, two fsyncs — never two entries.** The enrolment record (roster
half) and the invite consumption record (rider half) that authorises it ride in the SAME entry, so a
crash can never leave an agent enrolled against an invite that is still open, nor an invite spent on an
enrolment that never happened. `internal/auth/inviteenrol.go` owns the composition:
`EncodeEnrolWithInvite`/`DecodeEnrolWithInvite`, kind `EnrolInviteRecordKind = "agent+invite"`, version
`EnrolInviteRecordVersion = 1` (versions ONLY the envelope — each half still carries its own version
inside its own bytes). The composer that USES it is `auth.Service.Enrol` + `auth.WALRoster.PutWithInvite`
(`internal/httpapi/auth.go`'s `handleEnroll` is the caller); `internal/invite/store.go`'s standalone
`Store.Redeem` — which writes its own, separate transaction — is explicitly NOT used on this path (see
that function's doc for why splitting the two writes would reopen the exact crash window the composite
entry exists to close).

**NO RESERVATION WAS TAKEN, AND NONE IS NEEDED — same rule as `"agent"` and `"invite"` above.**
`wal.Entry.Kind` is a free-form APPLICATION STRING, not a reserved `record-type` NUMBER; `"agent+invite"`
sits inside the PREPARE payload exactly as `"agent"`, `"invite"` and `"seqfloor"` already do, and
`internal/wal/format.go` was not touched. Written down explicitly, again, so a future reader does not go
and reserve a number nothing requires.

**The envelope** (`internal/auth/inviteenrol.go`'s `compositeJSON`; these field names are FOREVER —
written into an append-only log a later build must still read):

```
{"v":1,
 "enrolment":{...the exact bytes auth.Encode produces, verbatim...},
 "rider_kind":"invite",
 "rider":{...the exact bytes invite.Record.Encode produces, verbatim...}}
```

Both halves are embedded as raw JSON, byte-for-byte what each half's OWN package would have produced
writing it alone — that is what makes replay exact: `rider_kind` names the applier the rider is handed
to (`invite.RecordKind`, i.e. `"invite"` — see the section above), and it may be neither `auth.RecordKind`
(the enrolment kind itself) nor `EnrolInviteRecordKind` (the envelope itself); both are refused by
`validateRiderKind` before anything is written.

**The log's `Applier` is no longer the roster alone — it is `auth.NewMultiplexApplier`, dispatching by
`Entry.Kind`.** This corrects, without rewriting, the "durable enrolment record" section above, whose
`What run() actually does` list (step 2) says `wal.Open(wal.LogOptions{…, Applier: authRoster})`: as of
this task the value passed is the multiplexer, not the roster directly. `cmd/agent-bus/main.go` now
builds it as `auth.NewMultiplexApplier(lg, map[string]wal.Applier{auth.RecordKind: authRoster,
invite.RecordKind: inviteStore})` and passes THAT as `wal.LogOptions.Applier`. On a committed
`"agent+invite"` entry it EXPANDS the composite into its two halves and dispatches each to its own
applier — enrolment first, then the rider, so a rider applier reading the roster (none does today) would
see the agent the invite was spent on. On any OTHER kind (`"message"`, `"seqfloor"`, and any future
neighbour) it is silent, exactly as `WALRoster.Apply` and `invite.Store.Apply` already are about kinds
they do not own — a neighbour's record is not damage. **The invite store is rebuilt by replay exactly as
the roster is**, inside `wal.Open`, and attached afterward
(`inviteStore.Attach(walLog)`) — the SAME three-step ordering `auth.WALRoster`'s doc already establishes
(construct empty → `wal.Open` replays → `Attach` permits live writes), now run for both participants
side by side.

**An undecodable composite discards BOTH halves, loudly, and the two directions are NOT symmetric.**
`MultiplexApplier.Apply` returns `nil`, never an error — a non-nil error would poison the log on a live
write (`wal.ErrDiverged`) and fail `Open` on recovery, which invariant 6 forbids (recovery must always
reach a running server). So the discard is an `ERROR` log line naming both indices, and:

- the AGENT is not in the roster: it was acknowledged as enrolled, holds an id this bus minted and told
  it, and must re-enrol under a NEW id — the old suffix stays burned (invariant 1);
- the INVITE is NOT marked spent, so it stays redeemable — this direction is FAIL-OPEN, the same
  exception `doc.go` section 5 already documents for a lost spend record, and an operator seeing the log
  line should revoke the invite if they can identify it, rather than assume the discard was safe.

**THE FORWARD HAZARD — read this before checkpoints are wired into the server run path.**
`internal/wal` has a SECOND, unrelated type also named for dispatch-by-kind: `wal.MultiApplier`
(`internal/wal/checkpoint.go`), the CHECKPOINT dispatcher, and it must not be confused with
`auth.MultiplexApplier` above — the two share a shape and nothing else. `wal.MultiApplier` treats an
UNOWNED kind as a HARD ERROR (`"no registered checkpoint participant"`), which poisons the log via
`wal.ErrDiverged` on a live write and fails `Open` on recovery. **Checkpoints are NOT wired into the
server run path today** — `cmd/agent-bus/main.go` passes `wal.LogOptions{Applier: applier}` (the
auth-side multiplexer above), never `Checkpoints:` — so `"agent+invite"` reaches only
`auth.MultiplexApplier`, which is deliberately silent about kinds it does not own. **On the day
checkpoints ARE wired in, `"agent+invite"` MUST be registered with a `wal.CheckpointParticipant` or the
bus stops starting on its next restart.** This is not a future-proofing note; it is the exact bug shape
`internal/auth/inviteenrol.go`'s own `EnrolInviteRecordKind` doc warns the next reader about.

No new `record-type` reservation, no `ondisk-format-version` bump, no new on-disk FILE. This is a new
`Entry.Kind` value inside the existing WAL frame shape, nothing more.

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

**And the forwarder is now wired, not merely built.** An earlier draft of this paragraph said
"composition with the forwarder remains RELAY-19" and cited `TestHandshakeHandlerIsNotWiredIntoAnyMux`
as evidence that cross-bus delivery was best effort — **that test no longer exists**, and both halves
of the claim are false by `RELAY-24-BLOCKER-EGRESS`: `relay.Forwarder` is constructed at the
composition root, `hub.Options.Egress` hands it every locally-originated message, and a durable outbox
is attached to the WAL (see the section above and the one below). So cross-bus delivery from a running
server is no longer best effort in the sense this paragraph used to mean.

The remaining limits, stated precisely rather than replaced with a new overclaim:

- **Onward multi-hop relay is now implemented (`RELAY-47`, 2026-08-15).** An intermediate bus DOES
  now carry a message it ingested from a peer to a FURTHER hop when a recipient is on a bus that is
  neither this one nor the sender's — `A→B→C` delivers, proven against running buses. Full contract
  (fan-out bound, loop control, what a 200 still means) is in `CONTRACTS-HTTP.md`'s "Onward relay"
  section. `cmd/agent-bus/relayegress.go`'s `BusPath[0]`-originated-here check is **UNCHANGED and
  still correct**: it still declines to re-forward a relay-ingested message through `hub.Egress`,
  because that seam builds a NEW envelope claiming this bus as origin and `hub.publish` already calls
  it for relay ingest too — relaxing it would forward every ingested message TWICE. Onward relay is
  wired through a separate seam, `relay.AcceptOptions.Onward`, not through that check. `nil` there is
  still a legitimate LEAF configuration (no peer store, nothing to forward with), not a limitation of
  the build — startup reports `onward_relay=true`/`false` accordingly. **CRASH-SAFE as of `RELAY-48`** — this paragraph
  said the opposite until then, and the correction is stated rather than quietly swapped because the
  claim it replaces is exactly the kind that reads as freshly checked. A pending onward hop is now
  RESUMED after a restart: the intermediate retains the origin bus's attestation on `store.Record`
  (`OriginAttestation`, written by `store.Message.WithRelayOrigin` on the ingest path), so the
  envelope CAN be rebuilt from durable state. Before `RELAY-48` the hop was durably ABANDONED after
  this bus had already answered the upstream peer 200; the loss was logged loudly at
  WARN (invariant 6), and the locally-originated case is unaffected. Filed as `RELAY-48`.
- **A crash between a message's own commit and its outbox enqueue loses the FORWARD, never the
  message.** The outbox record is written in a SECOND `wal` transaction, after the message's own
  commit has already been acknowledged (invariant 4) — so the message itself always survives such a
  crash, and the bounded loss is confined to the cross-bus hop: on the next start nothing durable
  records that a peer was owed a copy, and the forward is not retried.
- **The outbound peer handshake is still unwired.** Nothing in this build calls `relay.Client.Enroll`,
  so roster discovery and federated listing do not work: a peer's agent roster is never fetched. This
  does not stop directed sends or broadcast fan-out, because `Registry.Route` and
  `Registry.BroadcastTargets` resolve a recipient by the BUS half of its id and never consult a
  roster.

## The durable relay outbox record and capacity contract (RELAY-15, corrected by RELAY-15-FU-CAPACITY-FAIRNESS, 2026-08-08)

`internal/relay/outbox.go` stores every relay-delivery lifecycle transition as a complete
`wal.Entry{Kind: "outbox"}` record. `"outbox"` is the free-form application discriminator inside a
normal prepare/commit transaction, not a new numbered WAL frame type, so it required neither a
`record-type` reservation nor an `ondisk-format-version` bump. The JSON record is strict and
self-contained routing metadata: `job_id`, `peer_bus`, `origin_message_id`, `size`,
`content_sha256`, `enqueued_at`, and `state`; terminal records also carry `settled_at`, and an
`abandoned` record carries `reason`. It never carries the message body. `job_id` must equal
`peer_bus + "|" + origin_message_id`; decode re-derives that value and refuses disagreement.

The lifecycle is monotonic: `pending` may become `delivered` or `abandoned`, and no terminal record
may become pending again. Settlement writes the whole post-transition record through the same
prepare→fsync→commit→fsync path as enqueue. A terminal record is a retained TOMBSTONE: it is the
evidence that prevents an older pending sibling from resurrecting a message that was already
delivered or deliberately abandoned. Tombstones remain through `OutboxSettledRetention`; they are
never evicted early merely to admit new work.

**Pending and retained capacity are separate.** `MaxJobs` (default `MaxOutboxJobs`, 16,384) is the
global pending-work limit. Settling a job immediately releases that pending slot. The complete
lifecycle table — pending records plus terminal tombstones — is independently bounded by
`MaxRetainedJobs` (default `MaxOutboxRetainedJobs`, 16,384) and `MaxRetainedBytes` (default
`MaxOutboxRetainedBytes`, 32 MiB). Tombstones consume retained count and byte capacity even though
they consume no pending capacity; they are never evicted merely to admit new work.

Live admission also enforces `MaxPendingPerPeer`. Its default is an equal share of `MaxJobs` across
`MaxPeers` (at least one; `DefaultQueueDepth` under the defaults), and it may not exceed `MaxJobs`.
Both limits include reservations for enqueues currently inside the durable write: the global
`pendingWrites` counter and per-peer `pendingWritesByPeer` counter are incremented before fsync and
released only after the write returns. A slow fsync therefore cannot let concurrent calls race past
either bound, and one dead or hostile peer cannot consume the capacity left available to another.

Live admission also reserves retained capacity before fsync: one retained-record slot, one
`MaxRetainedPerPeer` slot for the destination, and the exact worst canonical encoding among the
pending, delivered, and maximum-reason abandoned forms. Those reservations include concurrent
enqueues still being written. This worst-lifecycle byte charge is deliberately kept after
settlement, so `Settle` never performs a capacity check and can never be refused merely because the
terminal encoding is larger than the pending encoding. Settlement remains conditional on the
normal identity/state validation and durable write succeeding, but not on spare capacity.

**Replay and snapshot restore preserve acknowledged legacy debt.** Neither path discards an
otherwise-valid record merely because an older build or older configuration admitted more pending,
retained-count, retained-byte, or per-peer state than today's limits. The complete acknowledged
state is restored, the overage is logged loudly as retained-capacity debt, and live `Enqueue`
refuses further applicable growth until the debt drains. A newer capacity setting is admission
policy, never retroactive permission to lose durable history.

Pending jobs older than `OutboxRetryHorizon` and tombstones older than
`OutboxSettledRetention` stop being visible through `Pending` and `Lookup` immediately. With a
checkpoint-capable durable log they remain present and fully charged until a checkpoint that omits
them publishes successfully; a failed or ambiguous checkpoint reclaims nothing. Every invalid or
expired pending record is logged loudly and specifically, and recovery continues as invariant 6
requires.

### `relay-outbox` checkpoint participant snapshot version 1

`Outbox` is the `wal.CheckpointParticipant` named `relay-outbox` and owns exactly the `outbox` kind.
Its participant payload is strict JSON with `version: 1`, the WAL-supplied `high_water`, and a
`records` array. Records are sorted by `job_id`, unique, and embedded as their exact canonical
`OutboxRecord` JSON. Restore rejects an unknown snapshot field, trailing JSON, a version or
high-water mismatch, duplicate or out-of-order job ids, an invalid record, or bytes that decode but
are not the record's canonical encoding. After Restore, `Apply` ignores commits at or below the
restored high-water and folds only that generation's tail.

Snapshot marks an immutable, generation-scoped cleanup candidate. It records the canonical bytes
of every expired record omitted from that snapshot and every included terminal record. Only after
the underlying WAL checkpoint returns success may the outbox reclaim an omitted record, and then
only if the record is still expired and its current canonical bytes are IDENTICAL to the candidate.
If it settled or otherwise changed while publication was in flight, cleanup retains it for parity
with tail replay. Records that expire only after the snapshot are likewise outside that
generation's cleanup set.

Successful publication may also rebase an included terminal record's conservative
worst-lifecycle byte reservation to its exact canonical terminal length, again only when its bytes
still match the published candidate. Failure performs neither deletion nor rebasing. This
success-only, canonical-identity guard is what makes live retained accounting converge with
snapshot-plus-tail recovery without letting an older checkpoint reclaim a newer lifecycle fact.

## Two durable peer-configuration records: ROUTES and TRUST PINS (RELAY-10, added 2026-08-08)

`relay.Registry` (`internal/relay/registry.go`) is the SERVING copy of the routing table and it
persists **nothing**: every peer it holds vanishes on restart. That is invariant 5 with one half
missing — memory is the serving copy, but there was no disk for it to be the truth of — and for the
three-machine SSH-tunnelled federation this epic targets it meant re-peering every machine, by hand,
after every restart. RELAY-10 is the missing disk: `internal/relay/peerstore.go`.

### ROUTING AND TRUST ARE TWO RECORDS, NOT ONE — read this before extending either

The target topology is `laptop(A) <-> internet(B) <-> this machine(C)`, and **C never peers with A**.
C has no address for A and no reason to acquire one; B is the hop. But C must still **pin A's bus
signing key**, because a relayed message ORIGINATING at A is verified by C against that pin and B is
explicitly not allowed to vouch for it (`internal/relay/signed.go`'s `CrossBusTrust`: "presentation
is not attestation"). So **C needs TRUST for A with NO ROUTE for A**, and the mirror case is just as
real: a bus we relay THROUGH but accept no origin traffic from has a route and no pins. A single
record coupling an address to a key cannot express either, and the mistake would be undiscoverable
until RELAY-17 tried to use it — by which point changing the shape is a migration, not an edit. The
split comes from RELAY-7's cross-bus trust deep-dive.

| `wal.Entry.Kind` | Go type | carries | never carries |
| --- | --- | --- | --- |
| `"peer"` | `relay.PeerRecord` | `bus_id`, `config_seq`, `state`, `base_url`, `next_hop_tls_cert_sha256` | any key material — a certificate fingerprint is a public digest, not a key |
| `"bustrust"` | `relay.BusTrustRecord` | `bus_id`, `config_seq`, `state`, `bus_signing_keys[]`, `peer_client_tls_cert_sha256` (RELAY-45, 2026-08-14) | a route address or an OUTBOUND next-hop pin — see the INBOUND vs OUTBOUND section below |

`bus_signing_keys` is a **LIST, not a scalar**, and that is load-bearing rather than generous:
`signed.go:178-182` fixes the meaning — "MORE THAN ONE KEY IS RETURNED ONLY DURING A SIGNING-KEY
ROLLOVER WINDOW ... It is NOT a general-purpose key list". A scalar would force a federation-wide
outage on every signing-key rotation. `MaxPinnedBusSigningKeys = 2` is derived from that sentence: a
rollover has exactly two participants, the outgoing key and the incoming one. It is also a
per-message CPU bound — every extra pin is one more `ed25519.Verify` attempt on the inbound relay
path.

**NO new WAL record type, NO `ondisk-format-version` bump. NOTHING WAS RESERVED, and nothing needed
to be.** Both `Entry.Kind` values are — exactly as `auth.RecordKind = "agent"` and
`invite.RecordKind = "invite"` are documented above — FREE-FORM APPLICATION DISCRIMINATORS. They sit
inside the PREPARE payload, above the framing layer that `wal.Type` (`TypePrepare`, `TypeCommit`, …)
owns; the numbered record types and format versions belong to `internal/wal` and are reserved through
the Spec Server `record-type` / `ondisk-format-version` namespaces, and `Entry.Kind` is not one of
them. `internal/wal/format.go` was not touched. This is stated explicitly because **"numbers are
reserved, not chosen" is a standing rule in this repo** and the next reader should not go and reserve
a number nothing requires. Both entries ride in the same two-phase prepare-fsync-commit-fsync frames
as a message, an enrolment or an invite — no new frame shape, no new fsync, no `Entry.Audit` (peer
configuration is not part of the message trail).

**Two more `Entry.Kind` values now share the log**, alongside `store.RecordKind = "message"`,
`auth.RecordKind = "agent"`, `invite.RecordKind = "invite"`, `"seqfloor"` and any others documented
above (the set grows; this section deliberately does not state a count, because a count in one
section goes stale the moment another task adds a kind). `PeerStore.Apply` returns `nil` immediately
for any kind it does not own, the same shape every other applier uses, so none of them treats
another kind's records as damage.

### The JSON shapes

Read off the struct tags in `internal/relay/peerstore.go` (`peerRecordJSON`, `busTrustRecordJSON`):

```
{"v":1,"rec":"peer","bus_id":"<bus id>","config_seq":<uint64 >=1>,"state":"active"|"removed",
 "base_url":"https://host[:port]",              // ONLY when state=="active"
 "next_hop_tls_cert_sha256":"<64 lowercase hex>",  // OPTIONAL; ONLY when state=="active"
 "updated_at":"<RFC3339Nano UTC>"}

{"v":1,"rec":"bustrust","bus_id":"<bus id>","config_seq":<uint64 >=1>,"state":"active"|"removed",
 "bus_signing_keys":["<base64 std, 32 bytes>", …],  // ONLY when state=="active", 1..2 entries
 "peer_client_tls_cert_sha256":"<64 lowercase hex>",  // OPTIONAL; ONLY when state=="active" (RELAY-45)
 "updated_at":"<RFC3339Nano UTC>"}
```

**`rec` repeats the `Entry.Kind` INSIDE the body, deliberately.** Without it the two kinds'
TOMBSTONES are byte-identical — both are `{v, bus_id, config_seq, state:"removed", updated_at}` — so a
`Kind` mix-up in future wiring would land a route withdrawal in the trust table with no decode error
at all, silently un-pinning a bus. Each decoder refuses a body whose `rec` disagrees with the record
it is being read as.

| field | on-disk encoding | omitted when |
| --- | --- | --- |
| `v` | `relay.PeerRecordVersion` = **1**; any other value is REFUSED by the decoder, so a future shape change is diagnosable as "version 2, this binary reads 1" rather than as an unrecognised field | never |
| `rec` | the record kind repeated inside the body: `"peer"` or `"bustrust"`, and it must match the `Entry.Kind` it is read as | never |
| `bus_id` | canonical spelling, `ids.ValidateBusID` + `<= MaxPeerBusIDLen` (64). There is no separate server-minted record id: a bus id already names exactly one bus (invariant 2) | never |
| `config_seq` | decimal, `>= 1`, `<= 2^53-1` (`jq` reads JSON numbers as float64, so above that the value an operator reads would stop being the value on disk) | never |
| `state` | **fixed string** `"active"` / `"removed"` — never the numeric enum, for the reason the invite record states | never |
| `base_url` | a **BARE https origin** — scheme, host, optional port and **nothing else**, `<= MaxPeerBaseURLLen` (512, derived: `https://` + a 253-byte DNS name + `:65535` = 267, with headroom for a bracketed IPv6 literal) | `state != "active"`, and always on a trust record |
| `next_hop_tls_cert_sha256` (RELAY-41, 2026-08-14) | **64 LOWERCASE hex** — `buscert.Fingerprint.String()`, i.e. `sha256` over the leaf certificate's DER **exactly as it arrived** (`x509.Certificate.Raw`, never re-marshalled). No prefix, no colons, no whitespace; uppercase is REFUSED by `buscert.ParseFingerprint` rather than normalised, so one fingerprint has exactly one spelling. Parsed by that function and by nothing else — see the byte-for-byte note below | `state != "active"`; **also when the hop is unpinned** (the field is OPTIONAL) and always on a trust record |
| `bus_signing_keys` | **base64 std**, matching `auth.RosterEntry`'s `auth_pub`; each exactly 32 bytes, pairwise distinct, all-zero REFUSED (uninitialised or corrupt, and a small-order point) | `state != "active"`, and always on a route record |
| `peer_client_tls_cert_sha256` (RELAY-45, 2026-08-14) | **64 LOWERCASE hex** — `buscert.Fingerprint.String()`, i.e. `sha256` over the leaf certificate's DER **exactly as it arrived**, but of the peer's TLS **CLIENT** certificate presented on an INBOUND connection (`r.TLS.PeerCertificates[0]`), never the outbound server certificate `next_hop_tls_cert_sha256` describes. Keyed by the record's own `bus_id` — the ADJACENT bus principal that certificate names — not by an address. Parsed and validated by `relay.ParsePeerClientTLSFingerprint` **only** (the same one textual spelling, all-zero additionally REFUSED rather than read as "absent"); the durable decoder and any future operator-facing writer must both call it, so a value one of them would reject can never reach disk through the other | `state != "active"`, and always on a route record; also OPTIONAL when active — most trust records will carry no binding until an operator configures one |
| `updated_at` | `RFC3339Nano`, UTC. On a tombstone it is also the input to `PeerTombstoneRetention` | never |

**`base_url` is validated more strictly HERE than the package's live-dial helper.** `peerURL`
(`internal/relay/client.go`) rejects a query, a fragment and userinfo but ACCEPTS a path, so it would
let `https://h.example/../../x` become durable and then be joined with `PeerRelayPath` at every dial
for the rest of that peer's life. A rejected request is a moment; a persisted bad address is forever.
`validateBareHTTPSOrigin` therefore also refuses a path. Tightening `peerURL` itself is a follow-up
(it changes every caller).

**Both records are OPERATOR CONFIGURATION, never a peer's assertion** — the same point
`Registry.SetPeerBaseURL` makes, and it applies twice as hard to a pin: a key learned from the
network would be a trust anchor chosen by whoever we are trying to authenticate. The keys are copied
out of band exactly as the invite blob carries a bus's TLS certificate fingerprint (`DECISIONS.md`,
E6). A `"removed"` record is a TOMBSTONE and carries NO live configuration — no `base_url`, no keys,
no certificate pin, enforced field by field in both directions.

### `next_hop_tls_cert_sha256` is keyed to the ADDRESS, never to the bus id (RELAY-41, 2026-08-14)

**This is the whole point of the field and the one thing a future edit must not collapse.** A route
record's `bus_id` is the **DESTINATION**; its `base_url` is the address of the **NEXT HOP**. For a
directly-peered bus those are the same machine. For a bus reached *through* another they are
different buses:

```
agent-bus peer add -bus-id busB -url https://b.example:8443 -tls-fingerprint <fpB> -route-for busC
```

writes **two** route records, and both carry **`fpB` — busB's, the next hop's**. The `busC` record
therefore has `bus_id: busC` and `next_hop_tls_cert_sha256: <fpB>`. **That mismatch is correct, not a
mix-up**: the TLS handshake that record describes terminates at busB and presents busB's certificate.
A pin keyed to `bus_id` would compare busB's certificate against busC's fingerprint, fail, and
**refuse every non-adjacent hop** — the entire `A -> B -> C` topology the FEDERATION epic exists to
build. The Go field is named `NextHopTLSCertFingerprint` and the wire key names the next hop for the
same reason: so that the constraint survives in the durable format rather than only in a doc comment.
`TestPeerRecordTLSFingerprintIsKeyedToTheNextHop` (`internal/relay`) and
`TestPeerAddTLSFingerprintRoundTripsOnDisk` (`cmd/agent-bus`) are the anti-regression tests, and both
use two **different real certificates** — with one certificate every assertion would pass under
either keying, which is how this bug class survives a suite.

**It is NOT `idem.Fingerprint`.** `peerFingerprint` / `idem.Fingerprint` in `internal/relay/peer.go`
are the **idempotency** fingerprint of a roster payload — a replay-protection digest over request
bytes. This one authenticates a **hop on the wire** and nothing inside a message. The names are kept
deliberately far apart.

> **WHICH CERTIFICATE, IN WHICH DIRECTION — and DO NOT INVERT IT.**
>
> This field pins the certificate presented **by the hop at `base_url` when THIS bus DIALS it**: an
> **OUTBOUND, SERVER-side** certificate, keyed to an **address**. It is **NOT a source of inbound
> peer identity.** `RELAY-20` holds the mirror-image problem — the peer's **CLIENT** certificate on a
> connection *to* us (`r.TLS.PeerCertificates[0]`) — and **nothing in this task or in
> `MTLS-CLIENTAUTH` establishes that those two certificates are the same.** Binding an inbound client
> certificate to a peer principal needs its own record; this one does not provide it, and an earlier
> draft of this section wrongly implied it did.
>
> **`fingerprint -> bus id` is forbidden.** Next-hop keying puts **one fingerprint on N records with N
> different `bus_id`s** — in the worked example, `fpB` is on busB's route *and* on busC's — so a
> fingerprint-first lookup is **ambiguous by construction** and would resolve an inbound busB
> connection to **busC**: a peer-principal spoof produced entirely by correct data read backwards.
>
> **`base_url -> bus id` is the same trap in the other field**, for the same reason: N records
> legitimately share one address, so an address-keyed map resolves to an arbitrary *destination*
> rather than to the hop.
>
> The only sound direction is **address-first, outbound**: *I am dialling this address — does the
> certificate I was served match this record's pin?* And because the pin is **duplicated across every
> record sharing that address, and can diverge** (a hand-edited log, or a partial failure part-way
> through an `add`), a consumer must read **each record's own pin** rather than caching one per
> address.

**ONE CONSTRUCTION, and the failure mode if that ever stops being true.** The value is produced and
consumed only through `buscert.FingerprintOf` / `buscert.FingerprintOfDER` / `buscert.ParseFingerprint`
— whichever end eventually compares it must compute it the same way. **A second construction — a
digest over PEM, over the SPKI, or an uppercase spelling — would not fail loudly: every peer
connection would simply be refused as an unknown peer, with nothing reporting a mismatch.** That is
why the decoder parses with `buscert.ParseFingerprint` and refuses every other spelling, and why
nothing in `internal/relay` or `cmd/agent-bus` hashes a certificate itself.

**OPTIONAL and ADDITIVE, and `v` is deliberately NOT bumped.** Every route record already on disk was
written without this field, and its absence decodes to the zero fingerprint, meaning *no pin*. A
version bump would do the **opposite** of what it looks like it does: `DecodePeerRecord` requires `v`
to **equal** `PeerRecordVersion`, so raising it to `2` would make this binary refuse every `v1`
record on disk — a migration, not a compatibility marker. Nothing was reserved from
`ondisk-format-version`, and nothing needed to be.

**The cost, stated precisely so an operator can act on it — and it is worse than "it refuses".** The
decoder rejects unknown fields, so an OLDER `agent-bus` binary cannot decode a record carrying this
field. `PeerStore.Apply` **logs at ERROR ("DISCARDING a peer-configuration record that could not be
decoded") and returns nil** — it does not refuse to boot (invariant 6: recovery always reaches a
running server, and every discard is logged loudly). So the actual downgrade behaviour is: **the
pinned generation is discarded at replay and the route reverts to its last generation without the
field — a stale, UNPINNED route — or disappears entirely if there is no earlier generation.** The
discard is loud, and it fails toward "no route" rather than "unverified route", but
**downgrading a binary after pinning a next-hop certificate is not supported**: withdraw the pinned
routes first, or keep the newer binary.

**Set only where there is a hop to pin, checked in both directions** (encode, before the durable
write; decode, because a record off disk is untrusted input even though this server wrote it). A
tombstone carrying a pin is refused: it has given up its address, so the pin names a hop that is not
there — and an address plus the credential to trust it is exactly the shape a resurrection wants.
`PeerStore.Remove` therefore drops the pin with the route. The converse is **not** enforced: an
active route may legitimately have no pin (that is every record written before this field existed,
and every peer whose certificate this bus has not been told to expect).

**Re-pinning is a real write.** `PeerStore.Put`'s no-op predicate compares the **pin as well as the
address**, because a peer rotating its certificate does not move: comparing only `base_url` would
swallow the new pin, leave the old certificate trusted, and report success.

**A record is written WHOLE, never as a delta — so an omitted pin is an ERASED pin.** That is the
record design (every durable entry carries the complete post-transition state) and it is what makes
two fail-silent-unpinned mistakes easy at the CLI. `agent-bus peer add` refuses both **before any
write** rather than repairing them, and the refusals are documented in `CONTRACTS-CLI.md`: re-adding
a pinned route without `-tls-fingerprint`, and re-pinning one destination through a hop while leaving
its siblings on the old certificate. **Nothing serves this pin yet** — no connection is verified
against it until `RELAY-20`.

**One pin per address is enforced at the CLI, not by the record — and only as far as the CLI can
see.** Nothing in `PeerRecord` stops two route records at one `base_url` carrying different
fingerprints; a replayed log holding that state decodes fine. It is `agent-bus peer add` that refuses
to create it, which is the right layer (the record must stay decodable). Two consequences, stated
rather than implied: a hand-edited or externally-generated log can still hold a divergence, and the
CLI's comparison is on the stored `base_url` STRING, compared case-insensitively and resolving
nothing — so `https://h` versus `https://h:443`, a trailing-dot FQDN, and two DNS names for one
machine are all different addresses to it. **A consumer must therefore read each record's own pin and
never cache one pin per address.**

### `peer_client_tls_cert_sha256` is keyed to the BUS PRINCIPAL, never to an address (RELAY-45, 2026-08-14)

**This field is the mirror image of `next_hop_tls_cert_sha256` above, and it lives on a different
record for that exact reason.** Read the "WHICH CERTIFICATE, IN WHICH DIRECTION" block first — this
section states the field that block predicted would need its "own record" and never provided:

| | `next_hop_tls_cert_sha256` (`PeerRecord`, RELAY-41) | `peer_client_tls_cert_sha256` (`BusTrustRecord`, RELAY-45) |
| --- | --- | --- |
| direction | **OUTBOUND** — the certificate the hop at `base_url` presents when THIS bus DIALS it | **INBOUND** — the certificate the adjacent bus presents when IT dials us (`r.TLS.PeerCertificates[0]`) |
| keyed by | an **address** (`base_url`) | a **bus principal** (the record's own `bus_id`) |
| cardinality | one fingerprint legitimately sits on **N records with N different `bus_id`s** (`-route-for`: `fpB` on both busB's route and busC's) | one fingerprint may bind **at most one `bus_id`** — enforced, not merely intended |
| `fingerprint -> bus id` lookup | **FORBIDDEN** — ambiguous by construction, and would resolve an inbound busB connection to busC | **SOUND** — and only for the reason in the next paragraph |

**`fingerprint -> bus id` is sound on THIS field, and ONLY because uniqueness is enforced at write and
ambiguity fails closed at read — do not carry the conclusion to `next_hop_tls_cert_sha256` without
carrying the reason.** `relay.PeerStore.PutTrust` refuses (`ErrPeerClientCertAlreadyBound`) to bind one
fingerprint to a second `bus_id` before anything is written, and `InboundPeerPrincipal` — the one
reader of this field — fails closed with `ErrAmbiguousInboundPeerCert` if it ever finds two active
bindings anyway (a hand-edited data directory, a log written by another binary). It is the enforced
uniqueness that makes the lookup sound, not the direction of the arrow: reading `next_hop_tls_cert_sha256`
the same way is forbidden precisely because no such uniqueness is enforced or enforceable there — one
fingerprint on N `bus_id`s is the deliberate, correct shape of that field.

**Optionality, tombstones and the version, exactly as `next_hop_tls_cert_sha256`'s own entry states —
restated here because this is the field an operator or a downgraded binary will actually meet.** The
zero value means absent; an explicit all-zero digest is REFUSED rather than read as absent
(`relay.ParsePeerClientTLSFingerprint`); a `"removed"` (tombstoned) trust record carries no binding, in
both the encode and the decode direction — a withdrawn principal that still bound a live client
certificate is exactly the admission-after-revocation shape `RELAY-34`'s durable withdrawal floor
exists to prevent. **`v` is NOT bumped**: the field is additive and optional, and `DecodeBusTrustRecord`
requires `v` to equal `PeerRecordVersion`, so bumping it would refuse every `v1` record already on disk
rather than mark a compatibility boundary. The cost is the same shape as `next_hop_tls_cert_sha256`'s:
an OLDER binary refuses (`DisallowUnknownFields`) a record carrying this field, so **downgrading after
binding an inbound peer certificate is not supported** — withdraw the binding first, or keep the newer
binary. `PeerStore.Apply` logs the discard loudly at ERROR rather than refusing to boot (invariant 6).

**An active trust record still requires at least one pinned bus signing key — this field does not
relax that.** A bus adjacent enough to open a TLS connection to us is a bus whose relay signatures we
must be able to verify; a transport binding with no signing pin would describe a peer we admit and then
cannot believe. See `DECISIONS.md` "2026-08-14 — RELAY-45" for the fuller reasoning on this and the
other three decisions this field's shape rests on.

**Nothing reachable yet.** `relay.PeerStore.InboundPeerPrincipal` and `httpapi.RequirePeerPrincipal`
exist and are tested, but no route is mounted (`RELAY-20`) and no running server constructs a
`PeerStore` for the HTTP layer to consult (`RELAY-24`) — see `### Peer-bus transport identity` in
`CONTRACTS-HTTP.md`. This field is durable configuration with no operator surface: there is no CLI flag
yet, and `CONTRACTS-CLI.md` is deliberately not touched by this section.

### `config_seq` is a BUS-WIDE counter, and that is a fix, not a style choice

**Every entry carries the COMPLETE record in its post-transition state, never a delta** (invite's
rule), and `busTable.upsert` is MONOTONIC on `config_seq`: strictly greater applies, equal is
idempotent only if the record is the same generation, **lower is REFUSED and logged at ERROR**.

- *Why not keyed on `state`.* An invite's states are terminal and refusing to go back IS single use.
  A peer's are not — an operator legitimately re-peers a bus they removed, rotates a key, or moves a
  peer — so `removed -> active` is ordinary. State-keyed monotonicity would either forbid re-peering
  (making removal unrecoverable short of wiping the log) or permit anything.
- *Why not keyed on the timestamp.* Clocks step backwards; a trust anchor must not be decided by NTP.
- *Why BUS-WIDE rather than per-peer.* **A per-peer counter is derived from that peer's own entry,
  and that entry can legitimately LEAVE the table** — swept once its tombstone expires, or discarded
  on replay by the capacity cap. The next write for that bus would then restart at 1 while the log
  still held records at 1..N, and on the following replay the OLD generation, arriving first at an
  equal sequence, WINS: the operator's current address or pin set silently replaced by a superseded
  one. That is invariant 1's rule ("recovery may not reissue an index it has already handed out,
  **even for a record it discards**") being broken. One bus-wide counter cannot regress that way
  because it is raised by EVERY record replay decodes, **before any decision to discard it**, and is
  never lowered by a sweep, a cap discard or a removal. Asserted by
  `go test -race -run TestPeerStoreConfigSeqNeverRewinds ./internal/relay`, which covers both routes.
- *What it costs.* A bus's numbers are not contiguous (bus A gets 1, bus B gets 2, A's next is 3).
  Same trade invariant 1 already makes: **uniqueness and monotonicity hold, contiguity does not and
  never did.** Do not write a check that asserts they are contiguous.
- *The residual, stated rather than glossed.* The mark is raised from a record only once that record
  DECODES, so a number carried by a body this binary cannot read is unknown to it and could be issued
  again. Two ways there: a CORRUPT frame (harmless — corruption does not heal, recovery discards the
  same frame on every start, so no two SURVIVING records can ever claim one number), and an INTACT
  body this binary refuses, most concretely a `"v":2` record written by a newer binary and read by an
  older one. The latter is the same downgrade hazard `internal/wal` states for `Entry.Idem`, with the
  same answer: **downgrade is not a supported operation here.** A durable floor file (the
  `wal-index-floor` pattern above) would close both and is deliberately not built — that is the
  durability layer's mechanism, and this counter is not an id.

### Bounds, retention and recovery behaviour

- **`MaxPeers` (64) is enforced on the REPLAY path too**, per table, counting active records and
  tombstones together. It is a MEMORY bound, and a bound one path could exceed is not a bound. A live
  write checks the same bound before writing and holds `writeMu` through the fold, so no other WRITE
  can take the slot in between. **An already-durable record can still reach this refusal**, and the
  earlier wording here denied it: the bound is only as stable as the SWEEP that frees slots, and the
  sweep reads the clock, so a write admitted at live time because a tombstone had expired can be
  refused at replay time on a corrected clock where that tombstone is inside its retention again and
  the table is full again. Reproduced with a four-entry table and a two-hour skew. Fail-closed, logged
  specifically, and self-healing once the clock is right.
- **`PeerTombstoneRetention` = `30 * idem.PeerOutageBudget` = 30 days.** Derived from the only other
  constant that says how long a peer stays relevant after it stops answering: a tombstone must
  outlive the retry traffic of the peer it buried, while withdrawn records must not permanently
  occupy the table. **An ACTIVE record is never swept.**
- **A tombstone stamped in the FUTURE is swept too, not only an expired one.** A record stamped ahead
  of the clock has a negative age, so an "older than retention" rule alone leaves it in the table
  forever. That needs no WAL access to produce — a write stamps the LOCAL clock, so an operator
  machine that is far ahead when a peer is withdrawn writes one — and enough of them fill the bounded
  table until every new peering is refused. The rule is symmetric, and safe because ACTIVE records are
  never swept in either direction: the only thing a wrong clock can drop is a WITHDRAWAL. The
  comparison is made on **times, not on a `time.Duration`**: a duration saturates at ±292 years and
  negating it at the saturation point returns the same value, so a duration-based symmetric test stops
  firing exactly where the stamp is most absurd (asserted with a year-9999 stamp).
- **A SWEPT tombstone leaves an ADMISSION FLOOR behind (`busTable.sweptMax`).** This is the correction
  to what an earlier draft of this section claimed. A tombstone does two jobs and the sweep only ends
  one: while it is present an older duplicate is refused by monotonicity, but once it is gone the bus
  is UNKNOWN and the insert path would take whatever arrives — resurrecting a withdrawn route at its
  old address, or a REVOKED PINNED SIGNING KEY. `config_seq`'s high-water mark could NOT serve as that
  floor (it is a MINTING floor, raised by every record including the newest, so testing an arriving
  record against it would refuse the very record that had just raised it — self-refusal, not a
  concurrent-write race). So each sweep hands the tombstone's sequence to a per-table floor that only
  rises, and the insert path refuses anything at or below it. Asserted by
  `go test -race -run TestPeerStoreASweptTombstoneStillRefusesAnOlderRecord ./internal/relay`.
- **The floor is only safe because WRITES ARE SERIALISED, and that is a correctness requirement, not a
  performance choice.** `PeerStore.writeMu` is held across mint → durable write → fold, so **the order
  of records in the log IS the order their sequences were minted in**. Without it two concurrent
  writers minting 2 and 3 could land in the log as 3 then 2; both are acknowledged, and on a later
  replay past the tombstone retention the seq-2 record arrives behind a swept seq-3 tombstone and the
  floor refuses it — an acknowledged operator configuration lost permanently, on every subsequent
  boot. Both review gates reproduced that against the unserialised version. Serialising costs nothing
  because writes come from an OFFLINE operator subcommand under the dirlock (`DECISIONS.md`,
  FEDERATION (e)). Asserted by
  `go test -race -run TestPeerStoreConcurrentWritesRespectTheCapAndTheSequence ./internal/relay`,
  which requires the sequences in the real WAL to be strictly increasing.
- **Recovery is therefore clock-independent.** Whatever a skewed clock (NTP step-back, a restored VM
  snapshot) does to the tombstones, an older record is refused: by the tombstone if it is there, by the
  floor if the sweep has just removed it. Asserted by
  `go test -race -run TestPeerStoreReplayIsClockIndependent ./internal/relay`, which replays one
  history — INCLUDING duplicated older records behind a withdrawal — under two clocks a decade apart
  and requires the same recovered state.
- **`PeerStore.Apply` NEVER returns a non-nil error**, per invariant 6: from a live write that would
  poison the log (`wal.ErrDiverged`), and from recovery it would refuse the start. Every failure — an
  undecodable record, an invalid one, the capacity bound, a non-monotonic sequence, a record naming
  our OWN bus, an ASCII-case confusable of a known bus — is a DISCARD logged loudly and specifically
  at ERROR, naming the table, the prepare/commit index and the reason — plus the bus, except for a
  record that could not be DECODED, which cannot name the bus because that is exactly what could not be
  read (it names the entry kind instead). Silent discard is the defect, not discard itself. **A discard here is fail-closed in the direction that matters:** the bus
  either stays unknown or keeps the generation already in memory. It can cost availability (the
  operator must re-apply).

  **CORRECTED 2026-08-08 by RELAY-34 — the paragraph that used to follow said revocation fails
  OPEN, and that was TRUE when it was written.** `Apply` never INSTALLS an address or a pinned key
  this bus did not already hold; that half was always sound. The half that was not: a discard **can
  fail to REMOVE one**. Every entry carries the complete post-transition state, so discarding a
  WITHDRAWAL — a torn tail, a bit-rotted frame, a filesystem or VM snapshot rolled back past it, all
  of which invariant 6 REQUIRES us to survive rather than refuse to boot — left the previous
  generation as the surviving truth and reinstated it. For routes an un-peered bus became routable
  again; **for the trust table a REVOKED PINNED SIGNING KEY CAME BACK.** Reproduced by truncating
  eight bytes from a `bus.wal` tail.

  **That is now CLOSED by the durable withdrawal floor** (`peer-withdrawal-floor`, its own section
  below): a withdrawal is fsynced OUTSIDE the log before the tombstone is handed to the log at all,
  so losing the tombstone loses only the tombstone. Both halves are now fail-closed, and **absence of
  a pin now means "not currently trusted"** rather than "no surviving record says otherwise" — which
  is the property RELAY-17 builds its cross-bus trust anchor on.

**A note for whoever wires this up:** `PeerStore` must have the log REPLAYED INTO IT before its first
write. `config_seq` starts at zero and is rebuilt only from the records `Apply` is handed, so a store
wired to a log it has not replayed would mint sequence 1 over a log already holding 1..N and
reintroduce the defect above. Both supported wirings satisfy this — pass the store as
`wal.LogOptions.Applier` (Open replays before it returns), or call `wal.Replay(path, store.Apply)`
first — and the package cannot check it, so it is stated rather than assumed.

### Acceptance evidence is a REAL `kill -9`

`go test -race -run TestPeerStoreSurvivesReplay ./internal/relay` re-execs a child that ROTATES a
bus's pinned signing key and is SIGKILLed the instant the commit is fsynced — before `PutTrust`
returns and before anything is acknowledged (the parent asserts `syscall.WaitStatus.Signaled()`, so a
child that merely failed its own assertions cannot pass for a crash). It proves (a) the rotation is
on stable storage, (b) a fresh store rebuilt only from the crashed log serves the ROTATED pin and
still serves the route written before the crash, and (c) the pre-crash record replayed a SECOND time
— with the real bytes off the crashed log — does NOT put the old pin back.

### NOT YET WIRED, and nothing about a running bus changed

`PeerStore` now implements `CrossBusTrust`: it reads only the durable, operator-configured pins for
the **origin** bus and uses `internal/attest` to verify the envelope-carried origin attestation.
It refuses a store without RELAY-34's durable withdrawal floor, and neither performs TOFU nor lets
an intermediate re-attest the binding. That is an internal implementation seam only. `cmd/agent-bus`
still does not construct it for relay serving, register it as a `wal.Applier`, populate
`relay.Registry`, or mount a peer relay route. RELAY-17 itself adds no records and changes no
existing log decoding; the offline `agent-bus peer` command remains the operator path that creates
`"peer"` and `"bustrust"` records. **No new HTTP route, CLI flag, env var, `AGENT_PROTOCOL.md`
entry or `scripts/bus-*.sh` wrapper is added by RELAY-17** — a running bus cannot yet observe this
trust implementation.

## On-disk files in the data directory: the durable PEER WITHDRAWAL floor (`RELAY-34`, added 2026-08-08)

**This is the FOURTH floor file, and like the other three it exists because a number that must
outlive the log cannot be stored inside it.** `wal-index-floor` guards the WAL record index,
`message-seq-floor` guards the message sequence, `agent-suffixes` guards per-name agent-id suffixes.
This one guards something that is not a counter at all: **the FACT that an operator withdrew a peer
configuration.**

### The defect it closes

`internal/relay/peerstore.go`'s two records (`"peer"`, `"bustrust"`) each carry the **complete
post-transition state**, never a delta. That is what makes replay a plain monotonic upsert — and it
is also what made a withdrawal **losable**. Recovery is REQUIRED by invariant 6 to discard damage and
boot anyway, so when it discarded the tombstone the previous generation became the surviving truth
and was reinstated. For routes an un-peered bus became routable again. **For the trust table a
REVOKED PINNED BUS SIGNING KEY CAME BACK: revocation failed OPEN.**

Reproduced end to end by the security gate: `PutTrust` then `RemoveTrust`, both acknowledged,
`PinnedKeys` correctly nil; truncate **eight bytes** off `bus.wal`'s tail; reopen; `PinnedKeys`
returns the revoked key, active, one key pinned.

**Do not discount this as adversarial.** The triggers are bit-rot, a torn write, and a VM or
filesystem snapshot rolled back past the revocation — **none of them adversarial**, and a
single-operator SSH-tunnelled deployment suffers a snapshot rollback exactly as much as a hostile
one. "Sole operator" reads like the strongest mitigating argument here and is the weakest.

Nothing INSIDE the log could close it. Quoting `internal/wal/indexfloor.go`, which states the
principle: *"A floor derived from the log drops whenever the log does."* A withdrawal stored only as
a log entry inherits every repair the log undergoes.

### The file

`<data-dir>/peer-withdrawal-floor` (mode `0600`, on-disk format version **6** — RESERVED via the Spec
Server `ondisk-format-version` namespace on 2026-08-08, never hand-picked), implemented in
`internal/relay/peerstore.go`, constant `relay.PeerWithdrawalFloorFileName`. Format: a header line
carrying an unkeyed SHA-256 digest of the body, then one line per withdrawal:

```
agent-bus-peer-withdrawal-floor v6 sha256=<64 hex>
route <folded bus id> <config_seq>
trust <folded bus id> <config_seq>
```

| field | rule |
| --- | --- |
| table token | exactly `route` or `trust`. **Deliberately NOT `busTable.what`** ("peer route", "bus trust") — that is prose for log lines and may be reworded, and a durable key an edit to a log message can rename is one that silently forgets every revocation recorded under the old spelling |
| bus id | valid per `ids.ValidateBusID`, at most `MaxPeerBusIDLen` (64), and **already FOLDED to lower case** — so one bus has exactly one floor and a reader cannot take the lower of two spellings |
| `config_seq` | canonical decimal, no sign, no leading zeros, and **strictly below the plausibility bound 2^32** (`maxPlausiblePeerWithdrawalSeq`) |
| ordering | sorted by (table, bus id), so the bytes are a function of the withdrawal set alone |

Entries are capped at **4096** across both tables (`maxPeerWithdrawalFloorEntries`, derived as 32
complete turnovers of the `MaxPeers`=64 live cap) and the read is bounded at **1 MiB**. The cap
matters because the map is MONOTONIC — an entry may never be dropped, since the record it defends
against is still in an append-only log that is never compacted.

### The ordering is the mechanism — WRITE THE FLOOR AHEAD OF THE LOG ENTRY

`PeerStore.write` **fsyncs `floor[table][bus] = seq` BEFORE it hands the tombstone to the durable
log.** This is invariant 4's rule one layer down: nothing is acknowledged as withdrawn before the
fact of the withdrawal is durable somewhere no log repair can reach. **It must not be reversed for
any reason, including latency**, because the interesting failure is the crash BETWEEN the two:

| crash point | outcome | direction |
| --- | --- | --- |
| floor write fails | log entry never written, withdrawal REFUSED, operator told, old configuration stands | nothing claimed |
| floor written, log write fails | floor stands alone; the pins are already un-served and stay un-served across every restart | **fail-CLOSED** |
| floor + log written, tail then discarded | floor survives, tombstone does not; the superseded record is refused at admission and hidden at read | **fail-CLOSED** |

Log-then-floor would leave a tombstone the very next torn tail can discard with nothing outside the
log remembering — precisely the state this closes.

### Where the floor is enforced

- **`busTable.upsert`** refuses any ACTIVE record at or below the floor (`ErrPeerWithdrawn`). A
  TOMBSTONE is never blocked: the record that SET the floor is itself a tombstone at exactly that
  sequence, and an older tombstone reinstates nothing.
- **`busTable.lookup`** reports such a record ABSENT, so `Lookup`, `LookupTrust` and therefore
  `PinnedKeys` all see nothing. This covers the floor-written/log-failed window that `upsert` cannot.
- **`ActivePeers` / `TrustedBuses`** skip it — they read the map directly rather than through
  `lookup`.
- **`PeerStore.write`** treats it as absent too. Not symmetry for its own sake: `Put`/`PutTrust`
  return a NO-OP when the incoming configuration equals the current active one, so an operator
  re-pinning the SAME key after a lost tombstone would otherwise be told "nothing to do" and left
  with an un-pinned bus.

**It cannot refuse a legitimate write.** `NewPeerStore` seeds `config_seq` from the floors, and
`applyLocked` raises it from every applied record, so a live write is always minted strictly above
every floor. The seeding is not tidiness: a directory whose log was quarantined but whose floor
survived would otherwise resume minting at 1, below its own floor, and refuse its own re-pin for
ever.

### `PeerStoreOptions.Dir` — and the hand-off this creates

`Dir` is the data directory and is where the floor lives. **An empty `Dir` does not silently degrade:
`Remove` and `RemoveTrust` fail with `relay.ErrPeerNoWithdrawalFloor`**, and a durable store built
without one WARNS at construction. Reads, `Put`, `PutTrust` and `Apply` are unaffected.

**Any caller that constructs a `PeerStore` MUST set `Dir`** — most immediately the offline
`agent-bus peer` subcommand (RELAY-12), which is where an operator's revocation is actually typed,
and the composition root (RELAY-24). A store built without `Dir` cannot CONSULT a floor another
process wrote, so it may serve a pin the operator revoked; that shape is an unwired audit shape only
and must never back a routing or verification decision.

### A corrupt floor file is FATAL and is NEVER regenerated

`relay.ErrPeerWithdrawalFloorCorrupt` — bad header, unknown version, checksum mismatch, a malformed,
unfolded, duplicated or out-of-range entry. Same posture as `ids.ErrSuffixFileCorrupt`,
`wal.ErrIndexFloorCorrupt` and `hub.ErrSeqFloorFileCorrupt`, and the same reconciliation with
invariant 6: **the LOG still always starts** — a damaged `bus.wal` is still repaired and the bus
still comes up. What refuses is a damaged IDENTITY file, the narrow exception already granted to
`bus-id`, `wal-mac.key`, `agent-suffixes`, `wal-index-floor` and `message-seq-floor`. A crash can
never produce this state: the write is temp file + fsync + rename + directory fsync, so a reader sees
the whole old file or the whole new one.

The error names a one-step remedy, and deliberately NOT a bare "delete it": deleting it forgets every
revocation it recorded. The remedy says to move it aside and restart — **the bus then REBUILDS the
floor from every withdrawal its log still holds**, logging each repair — and to re-apply by hand only
those withdrawals whose log records are gone.

#### THE FLOOR IS REPAIRED FROM THE LOG WHEN IT FALLS BEHIND (security-gate P1, 2026-08-08)

The first version of this fix had a hole the security gate reproduced: the state *"tombstone in the
log, no floor beside it"* is the pre-RELAY-34 behaviour, silently restored, and there are three
non-adversarial ways in — bit-rot on the floor file, an inconsistent snapshot or backup restore that
brings `bus.wal` forward without it, and **an operator following this very error's instruction to
move it aside**. The documented remedy then *failed silently*: the tombstone is still in the log, so
`RemoveTrust` took its already-removed no-op branch, returned success and wrote nothing. Once the
tombstone had been swept it was worse — `RemoveTrust` reported `ErrUnknownPeer` and the floor could
not be re-established at all.

Two changes close it, both in code rather than prose:

- **`PeerStore.Apply` reconciles** (`reconcileWithdrawalFloor`): a withdrawal in the log whose floor
  is missing or lower re-floors at that record's `config_seq`. This is not "deriving the floor from
  the log" in the sense the file exists to avoid — the floor is still WRITTEN AHEAD of the log entry
  and is still the ONLY source when the tombstone is gone. It is the reverse direction: while the
  tombstone IS present it proves a withdrawal happened, so a floor behind it is a floor that lost
  data. It only ever RAISES. On forgery, at the strength the threat model supports rather than the
  flattering one: the WAL's keyed HMAC means the NON-ADVERSARIAL triggers this mechanism exists for —
  bit-rot, torn writes, snapshot rollback — can only DROP records, never invent a tombstone. It is not
  a claim against a deliberate attacker, since `wal-mac.key` sits in the same data directory; what
  bounds that actor is direction (an injected tombstone only ever UN-pins a bus, which is fail-closed)
  plus the sequence bound above.
- **Re-applying a withdrawal re-asserts its floor**, so the manual remedy does what it says.

Cost on a healthy bus: **zero**. On a live commit the floor already covers the sequence, so
`recordWithdrawal` returns without touching the disk. Only a start whose floor really lost data pays
an fsync, and each repair is logged at WARN.

#### The floor is never written with a sequence this binary would refuse to READ (security-gate P2-a, 2026-08-08)

The write side once validated against `maxConfigSeq` while the read side refused at the plausibility
bound, so an **acknowledged** withdrawal could persist a file the next start refuses — and following
that refusal's own remedy (move it aside, restart) had the reconciliation re-derive the same
out-of-range value from the tombstone still in the log and write the **identical unreadable file**. A
bus that never boots again, with a remedy that **loops**. Both sides now refuse at the same bound:
`encodePeerWithdrawalFloors` rejects it (the withdrawal fails loudly, fail-closed, and the bus stays
bootable) and `reconcileWithdrawalFloor` skips and diagnoses a log record carrying one rather than
copying it into the file. Reconcile is the only path by which a sequence from the LOG reaches the
file, so it is the path that has to bound it.

#### The floor update is serialised by its own mutex (security-gate P2-b, 2026-08-08)

`recordWithdrawal` releases `PeerStore.mu` across the file write and rebuilds the whole file from the
in-memory mirror. Its safety was argued from `writeMu` — which covered only one of its two callers.
`reconcileWithdrawalFloor` holds no `writeMu` and **cannot** (it runs from `Apply`, which `write()`
reaches while already holding it), so two callers could interleave snapshot-then-write and the second
could write a snapshot taken before the first's entry existed, **dropping a floor its caller had
already been told was recorded**. A dedicated `floorMu` is now held across snapshot → encode → rename
→ adopt. Lock order is `writeMu → floorMu → mu`, acyclic. Latent rather than reachable today, but it
becomes reachable the moment a second source of applied peer records exists — relay ingest.

#### An out-of-range MINTED sequence has its own error, not the corrupt-file one (security-gate round-3 P2 and P3, 2026-08-08)

When the bound refuses a sequence being **minted** (rather than one being read), the floor file is
perfectly intact — what is out of range is the counter, because some record in the log carried an
implausible `config_seq` and raised it (`applyLocked` raises the high-water mark from every record
including discarded ones, as invariant 1 requires). Wrapping `ErrPeerWithdrawalFloorCorrupt` there
printed *"the persisted peer withdrawal floor is corrupt"* and sent the operator to **move a healthy
floor aside**, permanently losing every revocation whose tombstone had been swept — and still not
letting them withdraw. That case now returns `relay.ErrPeerWithdrawalSeqTooHigh`, which names the log
as the cause and says in as many words that the floor file is NOT corrupt and must not be moved.
Reachable only by forging a WAL frame; the keyed HMAC means the non-adversarial triggers can only
drop records. It is fail-closed for the file and fail-OPEN for the revocation (the withdrawal is
refused, so the pin is still served), which is exactly why the diagnosis has to be right.

`reconcileWithdrawalFloor` also now **skips any record the table refused on IDENTITY grounds** — one
naming this bus's own id (a self-peer, which nothing may legitimately produce, and which wrote a
permanent row for the local bus that no operator action could explain or remove), and one whose bus
id differs only by ASCII case from a bus the table already holds (flooring the confusable would
durably un-pin the LEGITIMATE bus — a revocation nobody performed). Reconcile deliberately runs on
records `applyLocked` refused, because a refused record can still be a withdrawal whose floor must be
kept; an identity refusal is the exception, and is different in kind.

#### A plausibility bound on the stored sequence (security-gate P2, 2026-08-08)

`config_seq` is seeded into `PeerStore.configSeq` from this file, so a single planted entry near
`maxConfigSeq` seeded the counter to its ceiling: the bus started perfectly healthy, read fine, and
then failed **every** `Put`/`PutTrust`/`Remove`/`RemoveTrust`, for every bus in both tables, across
every restart, with `ErrPeerConfigSeqExhausted` naming a ceiling nobody had reached. Total loss of
function with the diagnosis pointing elsewhere. A floor at or above **2^32** is now refused at parse
time as tampered-or-damaged — 2^32 is ~136 years at one configuration change every second, and leaves
over 9.0e15 numbers between it and the ceiling, so a value that passes cannot bring exhaustion within
reach either. Same shape as `hub.maxPlausibleSeqFloor`.

A **MISSING** file is legal and means "nothing has ever been withdrawn". There is no migration window
to be fail-open in: when this landed, nothing outside `internal/relay` had ever constructed a
`PeerStore`, so no data directory held a `"peer"` or `"bustrust"` record at all.

### The digest is INTEGRITY, not AUTHENTICATION — a KNOWN, UNRESOLVED asymmetry

Unkeyed SHA-256, like `agent-suffixes` and `message-seq-floor`, unlike `wal-index-floor`'s keyed
HMAC. **RELAY-34 deliberately does not resolve that asymmetry** — it has its own open task, and
picking a side here would be a crypto decision made as a side effect of a durability fix, which
invariant 9 forbids. What is claimed is what is true: this defends the data directory's INTEGRITY
against media damage and accidental editing. Authenticity is defended one layer up, at the directory,
by `enforceDataDirPermissions` and the dirlock.

Note which direction tampering runs: forging a floor HIGH un-pins a bus (fail-closed, visible,
repairable by re-pinning); forging it LOW or deleting the file restores exactly the pre-RELAY-34
behaviour and no worse. Neither is a new capability for anyone who can already rewrite the directory.

### SINGLE WRITER PER DATA DIRECTORY

The whole map is rewritten on every withdrawal, so **two processes sharing a data directory would
each rewrite it from its own view and the last rename would win — silently LOWERING a floor**, which
is the one operation this file must never perform. `internal/relay` cannot enforce that; it is
enforced one layer up by the data-directory lock (`internal/dirlock`) the server takes at startup and
the offline `agent-bus peer` subcommand takes too (`DECISIONS.md`, FEDERATION (e)). It is the same
assumption `ids.DurableNameSuffixes` and `hub.seqFloorFile` already rest on — written down here
because the consequence is worse: theirs skip numbers, this one forgets a revocation.

### Acceptance evidence is a REAL `kill -9` PLUS the eight-byte truncation

`go test -race -run TestPeerStoreTrustSurvivesATornWALTail ./internal/relay` re-execs a child that
REVOKES a pinned bus signing key and is SIGKILLed the instant the tombstone's commit is fsynced (the
parent asserts `syscall.WaitStatus.Signaled()`). **The dying child itself asserts the floor is
already on disk BEFORE the log write** — that is the ordering proof, and it is unobservable from the
parent afterwards, where both files simply exist. The parent then truncates eight bytes off
`bus.wal`, confirms recovery reports the damage and that the revocation is GONE from the log, and
requires that `PinnedKeys`, `LookupTrust` and `TrustedBuses` all still report the bus as un-pinned —
through both recovery paths (`wal.Open`'s applier and `wal.Replay`) — that the discard was LOGGED,
and that the unrelated route written before the crash is untouched.

Confirmed RED before the fix, and RED again with the mechanism disabled in an otherwise-identical
tree. Supporting tests: `TestPeerStoreRePinsAfterADiscardedRevocation` (a revocation that sticks must
still be reversible), `TestPeerStoreRefusesAWithdrawalItCannotFloor` (the fail-closed refusal, and
that nothing reaches the log), `TestPeerWithdrawalFloorFileIsStrictlyVerified` (twelve tampering
cases), `TestPeerStoreSeedsConfigSeqFromTheWithdrawalFloor`, plus the two security-gate regressions
`TestPeerStoreRepairsALostWithdrawalFloorFromTheLog` and
`TestPeerWithdrawalFloorRefusesAnImplausibleSequence` — both also confirmed RED with their
respective fixes disabled.

### Still NOT WIRED

`PeerStore` is still not constructed by a running `cmd/agent-bus` server, and **no new HTTP route,
CLI flag, env var, `AGENT_PROTOCOL.md` entry or wrapper** comes with this. No existing data directory
gains a `peer-withdrawal-floor` file until something writes a withdrawal through a `PeerStore` built
with a `Dir`.

## WAL checkpoint generations (format version 7)

`internal/wal` may compact the shared application WAL into immutable directories under
`wal-generations/gen-<20-digit-generation>/`. Each generation contains an
HMAC-SHA256-authenticated `manifest.json`, one separately authenticated and length-bounded
`<participant>.snapshot` per exactly registered participant, and a version-2 `tail.wal`. All
authentication uses the data directory's existing `wal-mac.key`; application participants never
receive that key. Checkpoint format version 7 was reserved through the Spec Server and is distinct
from the WAL frame version.

The manifest authenticates its domain, format version, generation number, one shared committed
`high_water`, `next_index`, the fixed tail filename, a fresh random 32-byte `tail_id`, a tail-header
MAC, and the sorted exact participant names/kinds/files/lengths/digests. Every snapshot envelope
independently authenticates the same generation and high-water plus its participant name, owned
kinds, and payload. A snapshot payload is capped at 64 MiB; its envelope and manifest are read with
fixed bounds, and checkpoint paths must be regular files/directories rather than symlinks, FIFOs or
devices.

`tail_id` is also the domain-separation context in every frame MAC written to that generation's
tail. The separately authenticated tail-header MAC binds the tail header to the manifest's
generation and high-water. Consequently, copying a valid tail from another generation fails
authentication even when all copied commit indexes happen to be above the receiving generation's
high-water. Recovery additionally rejects a candidate whose tail contains any commit at or below
its authenticated snapshot boundary.

Every participant snapshot is taken while the shared `Log.mu` is held, at the same `lastCommit`
high-water. Participant names and kind ownership are sorted and must exactly match the registry at
recovery; missing, extra, duplicate, or differently owned kinds are rejected. A live write whose
kind has no registered participant is refused. Recovery verifies the complete manifest, participant
set, every snapshot, and the generation-bound tail before calling any `Restore`; it never combines
a snapshot from one generation with another generation's tail. It then restores all participants at
that common high-water and replays only commits above it from the selected tail, preserving the
shared WAL's global commit order across participants. `next_index` and the independent durable WAL
index floor ensure fallback never reuses an index burned in a later rejected generation.

Publication writes and fsyncs every snapshot, the empty successor tail and the manifest; fsyncs the
temporary generation directory; renames it to its immutable generation name; fsyncs
`wal-generations`; then atomically replaces `CURRENT` and fsyncs the parent again. Any ambiguous
failure after the generation rename poisons the live log until restart rather than allowing writes
through an uncertain hand-off. Older complete generations remain as whole-generation fallback.

**`CURRENT` is only a publication hint, never authority.** Recovery scans published generations
newest-first and selects the newest wholly authenticated generation even when `CURRENT` is missing,
corrupt, or valid-but-stale. A rejected generation is never partially restored: recovery logs the
rejection and tries an older complete generation. Persistent malformed generation material and
interrupted `.tmp` publications are renamed to `.orphan`, parent-fsynced and logged loudly where
safe. Once any published generation exists, failure to authenticate any generation is fatal; the
stale pre-checkpoint `bus.wal` is never silently resurrected.

Generation verification is read-only. Damage inside the selected tail is handled only by the normal
WAL salvage/repair pass after selection, exactly once; the resulting truncation or rewrite remains
visible through `Log.Recovered().Repaired` and the existing loud, specific repair log. The tail is
bounded operationally by the last checkpoint: a new checkpoint snapshots all state through the
shared high-water and starts an empty successor tail, so later recovery replays only post-checkpoint
records rather than the whole historical WAL.

With no published checkpoint generation, `bus.wal` is explicit legacy generation zero. Existing v1
CRC32C WALs still take the established authenticated migration to WAL frame version 2 before their
first checkpoint; v2 `bus.wal` is replayed normally. The first explicit `Checkpoint` publishes
generation one, after which recovery uses authenticated generations and their bounded tails.

## The `"outbox"` record is now REPLAYED AT STARTUP — it was silently skipped (`RELAY-24-BLOCKER-EGRESS`, 2026-08-15)

`relay.OutboxRecordKind = "outbox"` (documented above, RELAY-15) has existed on disk since RELAY-15
but was **in no applier map**. `auth.MultiplexApplier` is deliberately silent about kinds it does not
own — which is what keeps `"message"` and `"seqfloor"` records from being read as damage — so an
outbox record in the WAL was passed over by startup **without a word**. That is the silent discard
invariant 6 rates as the actual defect: the record IS the durable proof that this bus owes a peer a
delivery, and a replay that skips it cannot tell "nothing is owed" from "I did not look".

`cmd/agent-bus/main.go` now registers it, on the SAME conditional as the two other federation kinds:

| peer store | `appliers["outbox"]` | effect at startup |
| --- | --- | --- |
| built | `*relay.Outbox` (constructed with **no** `Durable`, before `wal.Open`) | replay rebuilds the delivery table; `Outbox.Attach(walLog)` afterwards makes it writable |
| **not** built (`relay.NewPeerStore` failed) | `*unreplayedPeerRecords` | records are COUNTED and reported at `ERROR` after replay, never silently dropped — and on **their own line**, separate from the peer-route/bus-trust count, because the remedies differ: configuration returns intact on the next start, a cross-bus delivery this bus owed does not return at all |

The three-step order is `invite.Store`'s, and is not optional: **construct the applier → `wal.Open`
(replay fills it) → `Attach(log)`**. Between steps 1 and 3 the table can be read and rebuilt but not
written; every mutating call refuses with `relay.ErrOutboxNotDurable`. `relay.Outbox.Attach` is new
and is modelled on `invite.Store.Attach` — nil log refused, second attach refused, and the durable
log read through an accessor so `Attach` cannot race `Enqueue`/`Settle`/`Checkpoint`.

**What this does NOT change.** No record format moved: no `record-type` reservation, no
`ondisk-format-version` bump, and the JSON record documented in the RELAY-15 section above is
byte-identical.

**And `cmd/agent-bus` DOES write these records on this build.** An earlier draft of this paragraph
said "the forwarder that would is still not constructed" — written while the outbound-TLS blocker was
open, and false by the time it landed in the same change. `relay.Forwarder` is constructed at the
composition root, `hub.Options.Egress` hands it every locally-originated message, and
`Forwarder.Enqueue` writes one `pending` record per peer target before it returns, settling it
`delivered` or `abandoned` afterwards. A bus running this build both replays the table **and** adds
to it.

**Retention actually reclaims capacity.** A settled record is retired by `Outbox.sweepLocked` once it
is past `SettledRetention`, and the per-peer retained charge is released with it. That is only true
because the sweep asks whether a checkpoint **can run** (`wal.Log.CheckpointSupported`, i.e. whether
the log was opened with `Checkpoints`) rather than whether the log merely *has* a `Checkpoint`
method: `wal.Open` here is called with **no** `Checkpoints`, so deferring the reclaim to a checkpoint
would defer it for ever. See `DECISIONS.md` 2026-08-15.

## A fifth `Entry.Kind`: `"ack"` — the durable sender-visible delivery lifecycle row (`ACK-2`, 2026-08-16)

**Nothing was reserved, and nothing needed to be.** `wal.Entry.Kind` is a free-form APPLICATION
STRING that sits inside the prepare payload, above the framing layer (see "A composite `Entry.Kind`"
above). The reserved NUMBERS are `wal.Type`'s framing values, which are untouched.
`ack.RecordKind = "ack"` (`internal/ack/record.go`) is the fifth application discriminator to share
the WAL, alongside `store.RecordKind = "message"`, `auth.RecordKind = "agent"`,
`invite.RecordKind = "invite"`, `auth.EnrolInviteRecordKind = "agent+invite"`,
`hub.SeqFloorRecordKind = "seqfloor"` and `relay.OutboxRecordKind = "outbox"`. **No `record-type`
reservation, no `ondisk-format-version` bump, no new on-disk FILE, no HTTP route.**

### What it records, and what it is deliberately NOT

One row is one **(correlation key, recipient)** pair's SENDER-VISIBLE delivery state. Three facts are
routinely collapsed into the word "ack" and this row is only the first and third of them:

| plane | fact | where it lives |
| --- | --- | --- |
| A local acceptance | this bus committed and fsynced the message | **this record** (`accepted`) |
| B peer-hop receipt | the next bus took responsibility for a copy | `relay.OutboxRecord` — a DIFFERENT table |
| C recipient delivery | the addressed agent's application accepted it | **this record** (`delivered`/`refused`), not yet written by anything |

**A hop ACK does not advance this row.** There is no method on `ack.Store` that a hop ACK could call;
the absence is the enforcement.

### The record

`Entry.Body` is compact JSON, no HTML escaping, `record_version` **1**, enums as fixed STRINGS and
times RFC3339Nano in UTC — so a row can be read straight out of the WAL:

| field | type | rule |
| --- | --- | --- |
| `record_version` | int | `1`. A different value is REFUSED, never read with today's field meanings. |
| `correlation_key` | string | the ORIGIN bus's server-minted message id, `<origin-bus>-<seq>`, `<= ids.MaxMessageIDLen` (85). Reached through `store.Message.OriginID()`; **not a fourth identifier**. |
| `recipient` | string | fully qualified `<bus-id>.<agent-id>` (invariant 2), `<= ids.MaxAgentIDLen`. |
| `sender` | string | the authenticated principal that sent the message, fully qualified. It is what authorises the future status read. |
| `state` | enum | `accepted` \| `in_flight` \| `delivered` \| `refused` \| `undeliverable`. **There is no `unknown` and there must never be one** — it is a REPORTING value, and writing "I don't know" durably overwrites a real outcome with ignorance. |
| `class` | enum, omitempty | set **iff** `state` is a NEGATIVE terminal, and the half must match: `refused` takes one of the 3 recipient-emitted classes, `undeliverable` one of the 9 bus-emitted ones. Validated in both directions. |
| `attested_by` | enum, omitempty | set **iff** `state` is terminal. `peer_bus` \| `recipient_signature_unverified`. **There is deliberately no value meaning "verified"** — nothing in this system can produce one. |
| `accepted_at` | RFC3339Nano | required. The retention anchor for a non-terminal row, and PRESERVED across every transition. |
| `settled_at` | RFC3339Nano, omitempty | set **iff** terminal. The retention anchor for a terminal row. |

**There is no variable-length free-text field, by construction** (invariant 6 — the trail is metadata
and routing ONLY; a reason string sourced from a recipient is a body by another name). The record
also carries **no** body, **no** content hash, **no** `Seq` and **no** `Pos`: it has no ordering axis
at all, which is exactly why the correlation key is safe to key on.

The decoder is strict in the way `invite.DecodeRecord` and `relay.DecodeOutboxRecord` are: unknown
fields refused, trailing data refused, unrecognised enum spellings **rejected and never defaulted**,
every field re-validated, and untrusted text elided before it is quoted into an error.

### Monotonicity, and what replay may therefore do

`accepted` → `in_flight` → terminal, and **terminal is ABSORBING**: never revisited, never reopened,
never downgraded. The rule is keyed on the state RANK and on nothing else — not a sequence, not a
timestamp, not the record's position — so a stale `accepted` record replayed after a terminal one is
REFUSED rather than applied. The first terminal wins; a second, different one is rejected and logged
and **disconnects nothing**. A duplicate is a no-op.

### Startup wiring — the same three steps every other applier uses

**construct before `wal.Open` → `wal.Open` (replay fills it) → `Attach(log)`.**
`cmd/agent-bus/main.go` builds `ack.NewStore(...)` before the log, registers
`appliers["ack"] = ackStore`, and calls `ackStore.Attach(walLog)` after `wal.Open` returns. Between
steps 1 and 3 the table can be rebuilt but not written; every mutating call refuses with
`ack.ErrNotDurable`, and a failure to attach is FATAL.

**It is NOT gated on the peer store**, unlike the three federation kinds: the rows are LOCAL
acceptance, which a bus with no peers produces on every send.

### Retention and capacity

| bound | value | note |
| --- | --- | --- |
| `ack.Retention` | **24h** | `= idem.PeerOutageBudget`, the ROOT of the chain `relay.OutboxSettledRetention` = `relay.RetryHorizonCeiling` sits on. Adopted BY REFERENCE, never as a second literal; the equivalence is asserted by `TestAckRetentionMatchesOutboxSettledRetention` in `cmd/agent-bus/ack_retention_drift_test.go` — at the composition root, because `internal/ack` must not import `internal/relay` (it would invert the direction `ACK-4`/`ACK-5` need, and `TestRelayImportedOnlyByWiringSites` permits that import only from `internal/httpapi` and `cmd/agent-bus`; that guard was NOT widened). |
| `ack.MaxRecordBytes` | 1 KiB | one row's worst-case footprint, derived field by field and asserted by `TestMaxRecordBytesBoundsWorstCase`. |
| `ack.MaxRetainedBytes` | 64 MiB | the table's memory budget. |
| `ack.MaxEntries` | 65536 | the quotient. ~0.76 new rows/second sustained over the window. |
| `ack.PressureLine` | 32768 | `MaxEntries/2` — the crossover where free space stops exceeding used space; the per-sender fair share engages above it. |
| per-sender fair share | `maxEntries / (senders + 1)` | the `+1` is the sender that has not arrived yet, without which a lone sender's share is the whole table. |

**Both bounds fail closed and evict NOTHING.** Evicting a live row turns a real terminal outcome into
a false `unknown` — an inversion of the truth rather than a gap in it.

### The one place this design degrades instead of failing closed

> **At capacity the SEND STILL SUCCEEDS (201) and the row is NOT created.** A future
> `GET /v1/ack/...` then reports `unknown`. The refusal is counted (`ack.Stats().CapacityRefusals`)
> and logged at ERROR, loudly and specifically — the first one unconditionally, repetitions throttled
> to one a minute with a running total, so a full table cannot flood the log and push the informative
> line out of retention.

Stated because it is the one asymmetry in this repository: refusing here would mean an
**observability** table causing a **messaging** outage, and it would break everything while violating
nothing — the message is already durable and the sender was already told 201 (invariant 4).

### What writes it on this build, and what does not

`hub.Options.Acks` (a new OPTIONAL `hub.AckRecorder` seam; nil is byte-for-byte the old behaviour) is
called from `Hub.publish` **after the message's own two-phase commit and before local waiters are
woken**, writing one `accepted` row per recipient. It **never fails a send**: an error degrades the
observation and is logged.

| message | row? | why |
| --- | --- | --- |
| local directed send | **yes**, one per recipient | this is `ACK-2` |
| broadcast | no | a broadcast has no canonical audience under signing format v1, so there is no (message, recipient) pair to key on; `/v1/broadcast` answers 501 today |
| relayed ingest | no | an intermediate's rows are `ACK-5`'s back-propagation shape |

**The row costs a SECOND two-phase transaction PER RECIPIENT** — one per send today, since
`hub.SendRequest` carries a single `To`; measured at **+32%** on local send latency (9.59 ms → 12.66 ms
over 50 sends against a real `wal.Log`). It does **not** ride in the message's own entry, and that is a
TRADE rather than a limitation: a composite `Entry.Kind` could carry both in one transaction exactly
as `"agent+invite"` already does, but it would change the discriminator on every message record,
split the message applier and oblige every existing log to be read under both spellings — a migration
across the whole message plane, to close a window that costs an observation and never a message.

A crash in the window between the message's commit and the row's therefore leaves the message durable
with **no** row, so the sender is later told `unknown` rather than `accepted` — a bounded loss of
OBSERVATION and never of the message. That window is asserted by
`TestAckCrashBeforeRowLeavesMessageDurableAndStatusUnknown`; closing it is `ACK-8`'s.

**A multi-recipient LOCAL send would make this up to 64 serial two-phase transactions under the
global write lock** (`store.MaxRecipients`), repeatable by any enrolled agent — the live twin of the
latent broadcast hazard `Hub.forwardOnward` already names. It is unreachable today and is the thing
to re-check before any task gives a local send several recipients; batching the rows into one
`wal.Entry` is the fix.

**Nothing reads these rows yet.** `GET /v1/ack/{correlation_key}` is `ACK-9`, `POST /v1/ack` is
`ACK-6` and `POST /v1/peer/ack` is `ACK-3`, so there is no CLI subcommand and no `AGENT_PROTOCOL.md`
entry in this change — invariant 7 binds the task that adds the ROUTE, and this task adds none.
`ack.Store.Settle` and `ack.Store.MarkInFlight` exist and are tested but have **no production
caller** in this build.

## `relay-wire-version = 1` IS NOW SPENT — the peer ACK frame carries it (`ACK-3`, added 2026-08-16)

The `relay-wire-version` namespace is a **wire** protocol version, not an on-disk format, and it is
recorded here because this file is where this project's reserved numbers live and because two agents
bumping it independently would produce two incompatible `v1`s.

| namespace | reserved values | meaning |
| --- | --- | --- |
| `relay-wire-version` | `1` — reserved 2026-08-08 (note: *"FEDERATION phase, RELAY-23 will spend this"*), **SPENT by `ACK-3` on 2026-08-16** | The wire-protocol version of the **bus-to-bus peer frames**: the ACK frame at `POST /v1/peer/ack` (`relay.AckWireVersion`, JSON key `protocol_version`) and — when `RELAY-23` lands — the relay envelope at `POST /v1/peer/relay` (`relay.WireVersion`, same key). |

**ONE reserved value covers BOTH frames, and `ACK-3` did NOT reserve a second.** `ACK-CONTRACT.md`
§10 rules that the ACK frame and the relay envelope are two frames of one peer protocol and are
versioned together. `ACK-3` spends value 1 on the ACK frame; `RELAY-23` spends the same value on the
relay envelope. Neither allocates a new number, and **nobody picks the next one by reading a
constant** — a bump needs a fresh reservation through
`POST /api/v1/projects/agent-bus/reservations`.

### The reading rules, identical on both frames

- A **missing** version reads as **1**. That is the only backward-compatible read and it is exact:
  version 1 *is* the format currently on the wire, so a frame written before the field existed is a
  version-1 frame by definition. Both frames use `omitempty`, so an unset value is **absent** rather
  than an explicit `0`; `0` is not a version anyone may transmit.
- An **unrecognised** version is **REJECTED, never defaulted** — `parseOutboxState`'s posture
  (`internal/relay/outbox.go:316-330`) and for the same reason: guessing turns a corrupt or
  future-format frame into a plausible-looking valid one. On the ACK frame the stakes are higher than
  for an outbox row, because the frame carries a **TERMINAL** outcome and terminal is **absorbing** —
  a v2 frame read under v1's rules could durably settle a message in a way that can never afterwards
  be corrected. The wire answer is `400 {"error":"unsupported_ack_version"}`.
- **The literal `1` in the "absent reads as 1" rule must NOT be respelled as the version constant.**
  When the version is bumped to 2 against a fresh reservation, a versionless frame is *still* a v1
  frame — it was encoded by a binary that had never heard of v2 — and spelling it as the constant
  would silently reinterpret every legacy frame as the new format on the day of the bump. That is the
  same defect as defaulting an unrecognised version, arriving by a different door.

### A KNOWN, TEMPORARY DUPLICATION, recorded rather than papered over

`ACK-3` declares `relay.AckWireVersion` and `resolveAckWireVersion` in `internal/relay/ackframe.go`.
`RELAY-23` declares `relay.WireVersion` and `resolveWireVersion` in `internal/relay/message.go`, and
was **unmerged** when `ACK-3` landed. Declaring one name in two files of one package produces a build
break that **git cannot flag as a conflict** — no overlapping text, so the merge succeeds and the
package stops compiling. The ACK-scoped spelling merges cleanly and compiles. **A follow-up must
collapse the two onto one constant once `RELAY-23` lands.** The RULES above are what matters and they
are identical on both sides; only the spellings differ.

Likewise `ACK-3` adds `relay.CodeUnsupportedAckVersion = "unsupported_ack_version"` beside
`RELAY-23`'s `CodeUnsupportedRelayVersion = "unsupported_relay_version"`. Two codes is arguably the
better answer — a peer operator reads *which frame* the far end could not parse — but if an operator
would rather read one string, collapsing them is a one-line change.

---

## 2026-08-16 — `wal.Entry.Kind = "operator"`: the OPERATOR PRINCIPAL record (AUTH-10)

`auth.OperatorRecordKind = "operator"` (`internal/auth/operatorrecord.go`) is a NEW application
discriminator sharing this log with `store.RecordKind = "message"`, `auth.RecordKind = "agent"`,
`invite.RecordKind = "invite"`, `auth.EnrolInviteRecordKind = "agent+invite"`,
`hub.SeqFloorRecordKind = "seqfloor"`, `relay.OutboxRecordKind = "outbox"`,
`relay.PeerRecordKind = "peer"`, `relay.BusTrustRecordKind = "bustrust"` and
`ack.RecordKind = "ack"`.

**Deliberately NOT numbered "the Nth discriminator".** Earlier sections in this file each claimed an
ordinal ("a fourth `Entry.Kind`", "a fifth"), and those ordinals are now mutually inconsistent — they
were counted at different times against different subsets, and a draft of THIS section called
`"operator"` the fifth while listing five others beside it. An ordinal is a fact about the whole set
that every new record kind silently invalidates, so it is a stale-note generator. Enumerate the set
instead; a reader can count.

It records an **operator/admin principal**: a bus-scoped,
**NON-AGENT** identity that authenticates to the running bus and that admin capabilities authorise
against. An operator is never a routing subject — no message is addressed to one and no relay path
contains one.

### NO RESERVATION WAS TAKEN, AND NONE IS NEEDED

The same statement `"agent"`, `"invite"` and `"agent+invite"` make above, for the same reason, written
down again so nobody goes and reserves a number nothing requires. `wal.Entry.Kind` is a **free-form
application STRING** that sits inside the PREPARE payload, above the framing layer. The **RESERVED**
numbers are `wal.Type` (`TypePrepare`, `TypeCommit`, …) and the on-disk format version, both owned by
`internal/wal` and allocated through `POST /api/v1/projects/agent-bus/reservations`.
**`internal/wal/format.go` was not touched by AUTH-10.** An operator entry rides in the same two-phase
prepare-fsync → commit-fsync frames as every other kind: no new frame shape, no new fsync.

`auth.OperatorRecordVersion = 1` versions **only** the JSON field set below. It is not the WAL format
version and not the HTTP API version. A record carrying any other value is refused by `DecodeOperator`
(rejected, never defaulted), and unknown JSON fields are refused too.

### The JSON shape — THESE FIELD NAMES ARE FOREVER

`internal/auth/operatorrecord.go`'s `operatorJSON`, verified against the struct tags:

```
{"v":1,
 "operator_id":"op:<bus-id>.<name>-<16 lowercase base32>",
 "name":"<name>",
 "auth_pub":"<base64 std, 32 bytes>",
 "cert_fp":"<hex, 32 bytes>",
 "label":"<operator text>",                 // omitted when empty
 "created_at":"<RFC3339Nano UTC>",
 "revoked_at":"<RFC3339Nano UTC>",          // OMITTED ENTIRELY while LIVE
 "revoked_reason":"<operator text>"}        // omitted while LIVE
```

| field | Go type | on-disk encoding | omitted when |
| --- | --- | --- | --- |
| `v` | `int` | `OperatorRecordVersion`, currently `1`; any other value is REFUSED | never |
| `operator_id` | `string` | server-minted `op:<bus-id>.<name>-<suffix>`, suffix 16 chars of lowercase RFC4648 base32 over 10 bytes of `crypto/rand` | never |
| `name` | `string` | byte-identical to the name half of `operator_id`; re-checked on encode AND decode | never |
| `auth_pub` | `ed25519.PublicKey` | **base64 STANDARD encoding**, exactly 32 bytes — the encoding the enrolment wire format and `recordJSON` already use for the same bytes | never (an operator with no key is refused) |
| `cert_fp` | `[32]byte` | **lowercase hex** — `sha256` over the client certificate DER, the one spelling `buscert.FingerprintOf` and `client.ClientCertificate.Fingerprint` produce | never (the ZERO fingerprint is refused: it is the ABSENCE of a certificate) |
| `label` | `string` | operator note, `<= MaxOperatorLabelLen` (128) — the same bound `invite.MaxLabelLen` uses | empty |
| `created_at` | `time.Time` | `RFC3339Nano`, **UTC** | never |
| `revoked_at` | `*time.Time` | `RFC3339Nano`, UTC | **LIVE** — omitted entirely, so "live" and "revoked at the zero time" cannot be confused (`certBindingJSON`'s `retired_at` rule) |
| `revoked_reason` | `string` | operator note; REQUIRED whenever `revoked_at` is set (invariant 6: an operator action must be loudly attributable) | LIVE, or empty |

Times are `RFC3339Nano` UTC and digests are hex, matching every other kind on this log.

### REVOCATION IS AN APPEND OF A NEW WHOLE RECORD

Never an in-place edit, never a deletion (invariant 6: the log is append-only in the strict sense).
A revocation entry carries the **complete** operator with `revoked_at`/`revoked_reason` set, so — as
for `invite` — a surviving later record reconstructs the principal in its dead state on its own. The
id stays spent forever (invariant 1): revoking does not free it, and adding an operator under a
previously-used *name* mints a NEW suffix and is a DIFFERENT principal.

### The `Apply` fold rules (`OperatorRegistry.Apply`)

It runs during recovery **and** on every live commit and cannot tell them apart, and it deliberately
does **not** re-run the write-side admission checks — a record reaching it is already durable, so
refusing one would turn a damaged log into an outage (invariant 6). The rules, by case:

| stored | incoming | result |
| --- | --- | --- |
| *(absent)* | live | inserted |
| *(absent)* | **revoked** | inserted as revoked, **and logged loudly**: the record that CREATED the principal is missing, so the log was truncated or the add was damaged, and the credential fields being stored are uncorroborated |
| live | live | **DISCARDED, logged.** An overwrite would rebind a live ADMIN identity to a different keypair (invariants 1 and 3) |
| revoked | live | **DISCARDED, logged.** NOTHING SUPERSEDES A REVOCATION — an un-revoke would make the log's most security-critical operation reversible by a duplicated or replayed record |
| revoked | revoked, **agreeing** | discarded **SILENTLY** — a legitimate retry (invariant 10); noise here would train an operator to ignore the line below |
| revoked | revoked, **disagreeing** | **DISCARDED, logged loudly.** The first revocation is kept; a second that differs in instant, reason, key, fingerprint or creation instant can only be corruption or tampering |
| live | revoked | **the revocation is applied — and ONLY the revocation fields are taken.** The key, the fingerprint and the creation instant are kept from the record that CREATED the principal, so a record that claims to revoke while also swapping the credential CANNOT rebind a live identity through the revocation path. A disagreement is applied-and-reported, never merged |

**"Agreeing" is NOT "byte-identical", and the difference is deliberate.** The predicate is
`operatorRecordsAgree` (`internal/auth/operator.go`), which compares exactly the fields that BIND the
identity or the revocation: `auth_pub`, `cert_fp`, `created_at`, `revoked_at` and `revoked_reason`.
It ignores two fields, for two different reasons:

- `label` is an operator's own note. It authorises nothing and binds nothing, so two revocations
  differing only in their label ARE the same event and must not be reported as tampering.
- `name` cannot differ for one `operator_id`: it is re-derived from the id and re-checked on encode
  AND decode (`validateOperator` runs in BOTH `EncodeOperator` and `DecodeOperator`), so a record
  whose name disagreed would already have been refused as undecodable.

An earlier draft of the "agreeing" row and of the code comment beside it both said **BYTE-IDENTICAL**,
which overclaims — a second revocation differing only in `label` is silently discarded, and
"byte-identical" says it would not be. Recorded because this file rates a load-bearing comment that is
false as a defect in its own right, not as a wording nit.

A record that cannot be decoded is DISCARDED and logged with its prepare/commit indices, naming which
direction the loss falls in: a discarded ADD means an operator cannot authenticate; a discarded
REVOCATION means a principal the bus was told to kill is **still LIVE**, which is the fail-OPEN
direction and is why that log line says so in terms.

### `operator-auth-key.pem` — a file that is NOT in the bus data directory

`<identity-dir>/operator-auth-key.pem`: the operator's **PRIVATE** Ed25519 session-signing key,
PKCS#8 PEM, mode **0600**, inside a **0700** directory, written with `O_EXCL` and fsynced along with
its directory entry. It lives on the **OPERATOR's own machine**, created by
`agent-bus operator keygen`, and it is listed here only so nobody looks for it under `-data-dir`:
**the bus never holds it and never generates it.** The durable record above carries the PUBLIC half
and a digest, which is why the whole record is safe to print in `operator list`. Existing material is
LOADED, never overwritten — regenerating would silently invalidate the operator record the bus holds.

### WIRING STATUS — a server replay now REPLAYS these records (`AUTH-10-WIRING`, 2026-08-21)

**This heading read "a server replay currently passes these records over IN SILENCE", and the
paragraph under it said `cmd/agent-bus/main.go` "does **not** register `auth.OperatorRecordKind` in
its applier map" so an `"operator"` record "is skipped at **server** startup without a word — the
shape invariant 6 rates as the defect". That is no longer true.** `main.go` registers
`auth.OperatorRecordKind` in the applier map it hands `wal.Open` and calls `Attach` once `Open` has
returned, so every `"operator"` record on disk is applied at server startup. The outcome is reported
at INFO on every start — `msg="operator registry recovered from the append-only log…"
operators_recovered=<n> live_operators=<n>` — and a log holding two adds and one revoke reads
`operators_recovered=2 live_operators=1`. The second count is the one that proves the **revocation**
survived the restart rather than just the two adds; both reading `0` over a directory that holds
operator records is how this defect would be seen to return.

**The `wal replayed` line does not show it, and never did** — verified 2026-08-21 by running a
pre-wiring and a wired build over one data directory holding two adds and one revoke: **both** print
`records=6 applied=3 … discarded=0`. `Applied` counts entries DELIVERED to the applier (`records=6`
is three prepare/commit pairs), so a multiplexer returning `nil` for a kind it does not own leaves a
replay line that reads perfectly healthy over records nothing rebuilt. Only the operator line above
distinguishes the two.

The `agent-bus operator` subcommands were unaffected in either state: they open the log with their
own applier map (operator registry + enrolment roster + invite store).

**What did NOT change:** the server is a **reader** of this plane and writes no `"operator"` record
itself. `agent-bus operator add|revoke` takes the data directory's exclusive lock, so admitting or
revoking an operator means stopping the bus — a revocation is in effect from the next start, not
immediately — and nothing on the wire consumes an operator principal yet (`AUTH-7`, `INVMINT`,
`CONV-AUTHZ-ADMIN`).
