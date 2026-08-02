# PROTOCOL — the on-disk format

Human/maintainer facing. This document specifies the bytes agent-bus writes to disk and the rules a
reader must follow to interpret them.

**Scope, stated honestly.** This file currently covers the **on-disk format only**. The wire
protocol (HTTP routes, headers, enrolment and sessions) is specified in `CONTRACTS-HTTP.md`; it will
move here when a task is filed to move it, and until then this document does not describe it. Agent-
facing instructions live in `AGENT_PROTOCOL.md`.

Reference implementation: `internal/wal`. Where this document and the code disagree, that is a bug in
one of them — say which in `AGENT_LOG.md` rather than quietly following the code.

---

## 1. On-disk format versions

The format version is a **reserved** number, allocated through the Spec Server
`ondisk-format-version` namespace. Nobody picks one by looking at this table.

| Version | Status | Integrity | File header | Frame header | Notes |
|---|---|---|---|---|---|
| 1 | **legacy, read-only** | CRC32C (unkeyed) | 16 bytes | 20 bytes | Written by every build before 2026-08-02. Still *read*; a writer never *appends* to it — the only write this binary ever makes to a v1 file is the in-place CRC32C repair that precedes the one-time upgrade to version 2. |
| 2 | **current** | HMAC-SHA256 (keyed) | 48 bytes | 48 bytes | DUR-12, 2026-08-02. |

A reader refuses a version it does not implement rather than guessing at the layout. A **writer only
ever emits the current version** — there is no downgrade write, and no file ever contains a mixture
of versions, because the version lives in the file header and a v2 writer never emits a v1 frame.

All integers are big-endian, so a hex dump reads left to right.

## 2. File kinds

The kind is encoded in the 8-byte file magic, so `head -c8 <file>` identifies any file on disk:

| Magic | Kind | File |
|---|---|---|
| `AGNTBUSW` | write-ahead log | `<data-dir>/bus.wal` |
| `AGNTBUSA` | audit log | append-only audit trail (invariant 6: metadata and routing only, never message bodies) |

A magic naming the *other* kind is a fatal read error, never damage to be repaired: an audit file is
not a WAL, and relabelling one would replay records that were never write-ahead records.

## 3. Format version 2 — the current layout

### 3.1 File header (48 bytes, written once at creation)

```
[0:8]    magic            "AGNTBUSW" or "AGNTBUSA"
[8:12]   uint32           format version = 2
[12:16]  uint32           reserved, written as 0
[16:48]  32 bytes         HMAC-SHA256(key, header[0:16])
```

The header tag has **two jobs**, and the second is the one that matters operationally:

1. it detects damage to the header;
2. it is the **key check value**. A wrong key makes this tag fail before a single record is read,
   which is what lets recovery distinguish "the operator pointed us at the wrong key" from "the
   media ate this log" — see §6.

Stated plainly so nobody over-reads it: the header tag authenticates 16 **constant** bytes. It does
not bind the header to a particular file, and copying a header between two files of the same kind
under the same key produces a header that verifies. That is not a weakness worth fixing here,
because anybody who can rewrite the file can also read the key (§7).

### 3.2 Record frame (48-byte header, then the payload)

```
[0:4]    uint32           payload length, at most MaxPayloadSize (1 MiB)
[4:12]   uint64           record index — first record is 1, +1 per append, NEVER reused
[12:14]  uint16           record type
[14:16]  uint16           reserved, written as 0; a non-zero value is corruption
[16:48]  32 bytes         HMAC-SHA256(key, frame[0:16] || payload)
[48:...] payload          opaque to the framing layer
```

**The covered range, exactly.** The tag is computed over the **16 header bytes** — payload length,
index, type and reserved — **followed by every payload byte**, in that order, with nothing between
them.

- The **length field is inside the covered range**. This is not incidental: it is what kills the
  length-inflation class of attack, in which a forged or damaged length makes a torn prefix of a
  record look like a complete one. A reader that trusted the length before verifying the tag would
  have re-opened exactly that hole.
- The concatenation needs no separator because the length is the first four covered bytes, so the
  split point between header and payload is unambiguous for any well-formed frame. There is no
  second parse of the covered bytes that yields a different (header, payload) pair.
- The index is covered, so a record cannot be moved or renumbered in place without detection.

**Verification is constant-time**: `hmac.Equal`. Never `==`, never `bytes.Equal`.

### 3.3 Record types

Reserved through the Spec Server `record-type` namespace. They are written to disk and read back by
every future version, so they are not free to change.

| Value | Name | Meaning |
|---|---|---|
| 1 | `prepare` | phase one of the two-phase write: the change exists on disk, but is not yet accepted history |
| 2 | `commit` | phase two: the prepared record at the referenced index is now accepted history |
| 3 | `abort` | a prepared record that will never commit |
| 4 | `audit_message` | a message record in the append-only audit log |

An **unknown** record type is deliberately *not* rejected by a reader whose tag verified: the tag
proves some writer wrote those bytes intact under our key, and refusing them would turn a
forward-compatibility question into data loss.

## 4. The MAC key

| | |
|---|---|
| Path | `<data-dir>/wal-mac.key` |
| Mode | `0600`, inside the `0700` data directory |
| Content | 64 lowercase hexadecimal characters (32 bytes), optional trailing newline |
| Generated with | `crypto/rand` |
| Scope | one key per **data directory**, used by every log file in it (WAL and audit alike) |

**Where it lives is a recorded product decision** (`DECISIONS.md`, 2026-08-02, "Five decisions" §1):
the key is a file in the data directory. See §7 for exactly what that does and does not buy.

**Creation.** The key is generated automatically, and logged at INFO, when it is absent — **unless
the log positively identifies itself as a format version 2 log with content**, i.e. it carries our
magic, a version field reading 2, and more bytes than its own file header. In that one case an
absent key is fatal (§6). A malformed key file is *always* fatal; it is never silently replaced,
because replacing it makes every existing record unverifiable.

That predicate is deliberately the *same* one that decides the wrong-key case in §6 — one condition,
two errors depending on whether the key file is absent or merely wrong. A wrong or missing key never
damages the magic or the version field, so it never escapes the predicate. The residual, stated
rather than hidden: a real version 2 log whose **magic is also damaged**, in a directory whose key
file is missing, is **quarantined** — renamed aside, every byte preserved — instead of refusing the
start. Nothing is destroyed there, so a refusal would buy nothing; see §6 for why the destructive
paths are unreachable without a header that verifies.

**Rotation is NOT SUPPORTED.** There is no re-keying path. Changing the key while a version-2 log
exists produces the fatal mismatch of §6 on the next start. To change keys today you must stop the
bus, archive the log and the key together, and start against an empty data directory — which loses
history. A proper rotation (re-MAC every record under a new key, with the same two-pass verify-then-
rename discipline as the v1 upgrade) is a separate, unfiled piece of work.

**Backups.** The key and the log must be backed up **together**; either one alone is useless. A
backup that contains both is worth exactly as much to a thief as the data directory itself.

## 5. Format version 1 — legacy, and how it is upgraded

Version 1 is what every build before 2026-08-02 wrote:

```
file header, 16 bytes:   [0:8] magic | [8:12] uint32 version = 1 | [12:16] uint32 CRC32C over [0:12]
record frame, 20 bytes:  [0:4] len | [4:12] index | [12:14] type | [14:16] reserved
                         [16:20] uint32 CRC32C over frame[0:16] || payload | [20:...] payload
```

**How a v1 record is verified.** With its CRC32C, exactly as before — and with no more confidence
than before. A v1 record carries no MAC and cannot be MAC-verified; the key does not enter into it.
Everything §7 says about CRC32C being forgeable applies in full to a v1 log right up to the moment
it is upgraded. **A v1 log therefore has no key requirement at all**, which is why a pre-existing
data directory with no key file starts normally.

**The upgrade, at startup, once.**

1. The log is repaired if it is damaged, using the v1 (CRC32C) verifier — the ordinary recovery pass
   described in §6, unchanged.
2. Every surviving record is re-framed as version 2, **preserving its index, its type and its payload
   byte for byte**. Nothing is renumbered: indices are identities and are never reused (invariant 1),
   so a repaired log's index holes survive the upgrade as holes.
3. The converted file is written to `<log>.upgrade`, fsynced, and **verified before anything is
   renamed** — re-scanned under version 2 and checked, record count and a running SHA-256 over every
   `(index, type, payload)`, against what was read. A disagreement is fatal and leaves the original
   untouched.
4. A hard link `<log>.v1-<unix-nanos>` is left behind as a backup where the filesystem supports it.
   If linking fails it is logged at WARN and the upgrade continues — refusing to boot for lack of a
   backup would contradict the always-restart policy.
5. `<log>.upgrade` is renamed over the log and the directory is fsynced.

**Crash safety.** The original file is untouched until the final rename, so a crash at any point
leaves a complete, readable v1 log and the whole upgrade simply runs again on the next start. A
stale `.upgrade` file is removed rather than reused: it is a partial copy of a file that is still
intact.

**Downgrade: there is none.** A v1 binary refuses a v2 log (unknown version), and a v2 writer never
emits a v1 frame. If you must roll back to a pre-DUR-12 binary:

- restore the `<log>.v1-<unix-nanos>` backup (or a filesystem backup) over `bus.wal`, and
- accept that **every record written after the upgrade is lost** — those records exist only in the
  v2 file, and nothing converts them back.

Deleting `wal-mac.key` does not downgrade anything; it only makes the v2 log unreadable (§6).

## 6. What recovery does, and the one thing that is fatal

The standing policy (`DECISIONS.md`, 2026-08-02, "Availability over retention") is:

> **DAMAGE IS NEVER FATAL. NOT BEING ABLE TO READ THE FILE STILL IS.**

Damage — a torn frame, a flipped bit, a lost sector, a payload that no longer decodes, a corrupt
file header — is discarded and **logged, never silently**, and the bus starts. Being unable to read
the file at all — permission denied, a device error, an audit file where a WAL was expected, a
format version this binary does not implement — still refuses the start.

**DUR-12 adds exactly one deliberate exception to "damage is never fatal": the MAC key.**

| Situation | Behaviour |
|---|---|
| Key absent, log absent / empty / v1 / unidentifiable | Key is generated, logged at INFO. Not fatal. |
| Key absent, log positively version 2 with content | **FATAL** (`ErrMACKeyMissing`). The error names the key path and the log path. |
| Key malformed (not 64 hex chars, or exists but cannot be read) | **FATAL** (`ErrMACKeyMalformed`). The error names the key path. Never regenerated. |
| Key present, file-header tag fails, **at least one record verifies** | Not fatal. The verifying record *proves the key is right*, so this is header damage: the header is rebuilt, every record is kept, the bus starts. |
| Key present, file-header tag fails, **no record verifies anywhere**, log positively version 2 with content | **FATAL** (`ErrMACKeyMismatch`). Indistinguishable from a wrong key. |
| Anything else unreadable (garbage magic, file shorter than a header) | Quarantined — renamed aside, never deleted — and the bus starts fresh, exactly as before. |

**Why the two fatal rows are keyed on "positively version 2 with content", and why that is not a
hole.** Under a fresh or wrong key on a log that cannot be identified, the file-header tag fails and
no record verifies, which lands on the **quarantine** branch: the file is renamed aside with every
byte intact and the bus starts. The destructive paths — truncate, and rewrite-discarding-records —
are unreachable in that state, because both require a file header that verifies. So there is nothing
to protect there that a refusal would protect better.

**Why a wrong key is fatal when damage is not.** A wrong key makes *every* record fail verification,
so "discard whatever does not verify" would discard the entire log. Always-restart exists to stop
**media damage** holding the bus hostage; a wrong key is **misconfiguration** — it is fixable in
seconds, and destroying the log over it would be the worst available response. So recovery fails
loudly and names the key path.

**The accepted cost, stated rather than hidden.** A genuinely destroyed version-2 log — right key,
header gone, not one readable record — now needs one manual `mv` before the bus will start, where
previously it quarantined itself automatically. That is the price of not being able to tell it apart
from a wrong key, and it is a price paid only in the case where the log is already gone. Everything
else — a garbage magic, a file too short to hold a header, a v1 log — still quarantines itself
(renamed aside, never deleted) and starts fresh, exactly as before.

## 7. Threat model — what the keyed MAC buys, and what it does not

**What it completely defeats.** An ordinary enrolled client crafting a payload whose checksum makes
damage look like a complete record. That is not hypothetical: it was demonstrated end-to-end against
the CRC32C format. CRC32C is an error-detecting code, not an integrity primitive — it is **unkeyed**
and **GF(2)-linear**, so a client submitting nothing but printable-ASCII JSON in its own message body
could solve for bytes that make a torn prefix of its own record satisfy recovery's completeness
check, and get forged frames admitted as accepted history. **A client cannot compute an HMAC over a
key it does not hold.** That attack is closed by construction, not by a heuristic.

**What it buys: nothing at all** against an attacker who already has **write access to the data
directory**. This is an accepted limit, recorded as such in `DECISIONS.md`, not an oversight. Anyone
proposing to defend that case is proposing a different key-storage decision (an operator-supplied
key, an OS keyring, an HSM), and that is a product decision, not an implementation detail.

**But the reason usually given for that is wrong, and the difference matters.** The stock
justification — *"such an attacker can read `wal-mac.key`, same directory, same trust boundary"* — is
a statement about READ access, and reading is not what the attacker needs. Replacing a file on POSIX
needs only `w+x` on the **directory**, not any permission on the `0600` key inside it. The accurate
statement is that an attacker with directory-write access can **replace the key and the log
together** — plant a v2 log of their own making alongside a key of their own choosing — and the bus
will replay it as history. That is why the limit is real; it does not depend on the key being
readable.

**Known residual: the version 1 upgrade launders a forgery** (found by the security gate at DUR-12
review, follow-up filed). Recovery decides a file is version 1 from its header alone and then reads
it with the **old, unkeyed CRC32C** verifier before converting it. So an attacker who can drop a
CRC32C-forged version 1 file at `bus.wal` gets its records re-framed and **signed with the real MAC
key** — forging without ever touching `wal-mac.key`, and leaving records that verify forever
afterwards. The capability required is the same directory-write access already conceded above, so
this grants no new class of attacker; what it costs is **forensics** — the forged records are
afterwards indistinguishable from genuine ones even to someone holding the original key. The obvious
fix (refuse the version 1 path when a key file already exists) is **not** safe as stated, because a
crash part-way through an upgrade leaves exactly that state — key present, log still version 1 — and
refusing it would strand a legitimate redo. Whoever takes the follow-up must handle that.

Until it is fixed, treat **directory permissions on the data directory as load-bearing security**:
`0700`, owned by the user the bus runs as. A bind-mounted or world-writable volume voids the
guarantee — and note that `MkdirAll` does not tighten a directory that already exists.

**What it does not do at all:** the log is metadata and routing information only (invariant 6) and is
**not encrypted**. A MAC provides integrity and authenticity, never confidentiality. Anyone who can
read the file can read every record.

**Cryptography rules (invariant 9).** The construction is `crypto/hmac` + `crypto/sha256` from the Go
standard library, called through `hmac.New` and `hmac.Equal`. Nothing here is hand-rolled, adapted or
assembled out of primitives, and nothing may become so: broken crypto fails silently — it still
verifies, it simply protects nothing — so a green test suite is not evidence about it.
