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

**A backup of `wal-mac.key` alone is NOT a backup of the bus's secrets.** Two cross-bus decisions of
2026-08-07 (`DECISIONS.md`, "Cross-bus key trust: pin the origin bus key at peering, NO TOFU" and
"The bus TLS key and the bus SIGNING key are SEPARATE") add **two more** long-lived secrets to the
data directory, so a complete bus holds **three**. None is regenerable, and none of the three losses
is obvious from a file listing — which is exactly why they are spelled out here:

| Secret | What its loss costs |
|---|---|
| `wal-mac.key` | No record in the WAL or the audit log can be authenticated, so a v2 log becomes unreadable and recovery cannot verify what it replays (§6). |
| The bus **TLS** key | The bus can no longer present the certificate whose fingerprint its clients have **pinned** from the invite blob (`DECISIONS.md` E6). Every pinned client fails to connect; a new certificate means re-issuing invites. |
| The bus **SIGNING** key | Every attestation this bus ever made becomes unverifiable and the pin held by **every peer bus** is dead — a federation-wide event requiring re-peering, not a local outage. |

**The TLS key and the signing key are two different keys with two different jobs, and one is not a
backup of the other** — see §8.5, which is where the distinction is normative. Their on-disk
filenames are settled by the task that creates them (`MTLS-BUSCERT`), **not** by this section: at the
time of writing neither key file is produced by anything on the startup path, so a data directory
today still holds `wal-mac.key` alone. Backup procedure written now should be written for three.

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

## 8. Canonical signing format — the exact bytes a message is signed over

SIGN-1. Reference implementation: `internal/signing`. Test vectors:
`internal/signing/testdata/canonical_vectors.json`.

**Why this section is in a document whose scope note at the top says it covers the on-disk format
only.** These bytes
are neither on-disk nor on-the-wire: they are the *input* to `ed25519.Sign` and to the audit log's
content hash, and they are the one artefact the sender, the recipient, every relay hop and the audit
writer must agree on byte for byte. They therefore belong with the byte layouts, not with the HTTP
routes in `CONTRACTS-HTTP.md`, which describe the envelope that *carries* the signature.

### 8.1 What the format is for, and the failure it prevents

A detached signature is a claim about a byte string. If the sender and the verifier build that byte
string even slightly differently — a different field order, a re-encoded number, a trimmed space, a
field one side includes and the other does not — the result is one of two failures:

- verification fails intermittently, for no reason a user can see; or, far worse,
- verification **succeeds over bytes that omit a field**, so that field is silently forgeable while
  every signature still checks out.

The second failure is invisible to a test suite that only asks "does a good signature verify?". So
the format is pinned here, in one place, with byte-exact vectors that SIGN-2, SIGN-5 and CRYPTO-10
check their own implementations against.

### 8.2 Encoding: length-prefixed binary, NOT canonical JSON

**Chosen:** fixed field order, big-endian integers, every variable-length field preceded by its
`uint32` byte length. **Rejected:** a canonical JSON form. The reasons, in the order they matter:

1. **JSON is re-encodable, and things on the path re-encode it.** A relayed message crosses an
   intermediate bus (SIGN-7). Any hop that unmarshals and remarshals — reordering keys, reformatting
   a number, normalising escapes or whitespace — breaks verification at the far end with nobody
   having done anything wrong. A length-prefixed binary string either arrives identical or does not
   arrive.
2. **"Canonical JSON" is a specification we would have to implement** (key ordering, string escaping,
   number formatting, Unicode normalisation). A subtly wrong implementation of it fails in exactly
   the silent way invariant 9 warns about.
3. **Bodies are opaque bytes.** JSON cannot carry arbitrary bytes without a second encoding layer
   (base64), which is one more place two implementations can disagree.

**Version 1 is bound exclusively to Ed25519.** The algorithm is not a negotiable field: there is no
"algorithm" slot in the layout, so nothing on the wire can talk a verifier into a weaker one, and
changing algorithm means a new context string and a new version.

**The context string is also what keeps signing inputs disjoint.** An agent's key already signs one
other thing: `auth.SessionSigningContext` + token (`"agent-bus:session-token:v1:"`, see
`CONTRACTS-HTTP.md`). The two languages differ in their **first byte** — a canonical message always
starts with the `0x00` of a `uint32` length, a session challenge always starts with `'a'` (0x61) —
so neither can be replayed as the other, and `/v1/session/begin` cannot be used as a signing oracle
for message signatures. Any future artefact signed with an agent's key must preserve that
disjointness.

Length prefixes are **framing, not a cryptographic construction** — the distinction invariant 9
turns on. Nothing here hashes, pads, derives or mixes anything: the bytes below are handed to
`crypto/ed25519` (RFC 8032, Go stdlib, chosen by RATCHET-7) and to `crypto/sha256`, and to nothing
else. Framing is required *because* it makes the encoding **injective**: distinct field tuples always
produce distinct byte strings, so no attacker can shift bytes across a field boundary — move the tail
of a sender id into the head of a recipient id, say — and present a different logical message under a
signature that still verifies.

### 8.3 The byte layout

All integers are **big-endian**, matching §3. `len(X)` is a `uint32` byte count. Fields appear in
exactly this order, with nothing between them:

| # | Field | Bytes | Encoding | Minted by |
|---|---|---|---|---|
| 1 | context | 4 + 19 | `uint32` length, then the ASCII string `agent-bus/msg-sig/1` | constant |
| 2 | message id | 4 + len | `uint32` length, then the id `"<bus-id>-<seq>"` as UTF-8 | **server** (origin bus) |
| 3 | sequence | 8 | `uint64` | **server** (origin bus) |
| 4 | sender | 4 + len | `uint32` length, then the fully-qualified id `"<bus-id>.<agent-id>"` | **server** (at enrolment) |
| 5 | recipient count | 4 | `uint32` | sender chooses the set |
| 6 | recipient *i* | 4 + len | `uint32` length, then the fully-qualified id — **repeated `count` times, sorted bytewise ascending, no duplicates** | server minted each id |
| 7 | timestamp | 8 | `int64`, two's complement: Unix **milliseconds** UTC | sender |
| 8 | body | 4 + len | `uint32` length, then the body bytes verbatim | sender |

Worked example — the `minimal` vector, byte for byte:

```
00000013 6167656e742d6275732f6d73672d7369672f31   uint32(19) "agent-bus/msg-sig/1"
00000007 6275732d612d31                           uint32(7)  "bus-a-1"
0000000000000001                                  uint64(1)  sequence
0000000d 6275732d612e616c6963652d31               uint32(13) "bus-a.alice-1"
00000001                                          uint32(1)  recipient count
0000000b 6275732d612e626f622d32                   uint32(11) "bus-a.bob-2"
0000000000000001                                  int64(1)   timestamp, Unix ms
00000002 6869                                     uint32(2)  "hi"
```

Rules a second implementation must reproduce, all enforced by `signing.Canonicalize` and all of which
make it **fail closed** — it returns an error and *no bytes*, never a best-effort serialisation:

- **The message id and the sequence must agree.** The id's sequence half and field 3 are the two
  halves of one server assignment; a message that spells them differently is refused.
- **The sender must belong to the bus that minted the message id.** `"bus-a-7"` sent by
  `"bus-b.eve-1"` is refused. Without this, a peer could present its own agent as the sender of
  another bus's message id and the signature would faithfully cover the mismatch.
- **Recipients are sorted by the encoder, not by the caller**, so two implementations cannot disagree
  about ordering. Duplicates are **rejected, not collapsed** — collapsing would change the audience
  the sender believed it signed.
- **At least one recipient, and at most 4096** (`MaxRecipients`). The upper bound is derived, not
  picked: an id is at most 150 bytes plus a 4-byte prefix, so 4096 caps the recipient block at about
  616 KiB — the same order as the body bound. Without it the body would be bounded and the recipient
  list would not, which is a size amplifier reachable from untrusted input.
- **Sequence 0 and timestamp ≤ 0 are refused** as unset fields, the same posture `internal/ids` takes.
  The timestamp is nevertheless a *signed* int64 in the layout, so admitting a pre-1970 clock later
  would not need a new field width.
- **Body ≤ 1 MiB** (`MaxBodyLen`, matching `MaxPayloadSize`). A zero-length body is legal and encodes
  as a length prefix of 0 — an empty field, never an absent one.

**Which key verifies it.** The verification key MUST be resolved from this bus's roster using the
fully-qualified **`sender` field inside the signed bytes**, and from nothing else — never a key id,
key, hint or sender name carried beside the signature in the envelope. There is deliberately no key
identifier in the layout: a self-describing signature is one an attacker can point at a key of its
choosing, and the sender field is already covered, so binding to it is free. Cross-bus key trust —
whether bus B may attest a key for bus A's agent — is **settled** by SIGN-7 and specified in §8.5: the
origin bus's own attestation travels intact and is checked against that bus's **signing key, pinned at
peering time**, never re-attested by an intermediate and never trusted on first use.

**Versioning.** The format version is a **reserved** number, allocated through the Spec Server
`signing-format-version` namespace (value **1**, reserved 2026-08-02 for SIGN-1). It is spelled
exactly once, inside the context string, so two version indicators can never disagree. Adding,
removing, reordering or re-widening **any** field is a new version with a new context string — never
an in-place edit of version 1, because the vector file is a published artefact that other tasks pin
themselves to.

### 8.4 The central design call: the server's assignment is INSIDE the signed bytes

The message id and the sequence are minted by the server (invariant 1), yet they must be covered by
the sender's signature — otherwise an intermediate bus can reorder or misattribute messages and no
recipient can detect it. The two candidate resolutions, and the one taken:

| | Option (a) — **CHOSEN** | Option (b) — rejected |
|---|---|---|
| Shape | The sender signs the **origin server's assignment**: it obtains the id/sequence first, then canonicalizes and signs, then submits. | Signing happens before minting; the signature covers only sender-known fields and the id/sequence are bound separately by the durable record. |
| Cost | One extra round trip per send. | None. |
| Ordering claim | **Authenticated end to end.** A recipient can prove the sender agreed to this position in the origin's stream. | **Unauthenticated.** The id/sequence a recipient sees are asserted by whichever bus handed the message over. |
| Stops | Renumbering, reordering, re-attributing a body to a different sequence, and re-presenting one message under a fresh sequence — all detectable at the recipient. | Only misattribution of the *sender*, which is covered either way. |
| Does not stop | An intermediate **dropping** a message (a gap is visible, a tail truncation is not until the next message arrives); delay; the path metadata (see below). | The above, plus everything in the row above it. |

The accept flow option (a) implies — specified here, **implemented by SIGN-2**:

1. the client asks the origin bus to mint a message id and sequence, quoting its **idempotency key**
   (invariant 10), so a retry gets the *same* assignment back instead of burning a second one;
2. the client canonicalizes with those values and signs;
3. the client submits the signed envelope. The bus **rejects it unless the id and sequence are the
   ones it minted for that key**, then performs the ordinary two-phase durable write, and only then
   acknowledges (invariant 4).

An assignment that is never submitted simply leaves a hole in the sequence. That is correct and must
not be "fixed": indices are identities and are never reused, and the sequence advances past a hole
rather than rewinding (invariant 1).

**But the mint must be DURABLE, and this is the sharp edge of option (a).** `internal/ids`
(`sequence.go`) derives its resume floor from *the highest sequence number ever written to disk* —
prepared, committed, aborted or dangling alike. A number handed to a client in step 1 and never
submitted appears in **no disk record at all**, so a restart would hand that same number to a
different message. Two distinct, validly signed messages would then bear the same origin message id,
each with a signature that verifies — which is precisely the equivocation option (a) exists to
prevent, and a straight breach of invariant 1's "never reused, including across restarts".

So step 1 is not a bare `Sequence.Next()`. The assignment must reach disk before it is returned to
the client, bound to the idempotency key that asked for it (invariant 10, so a retry gets the same
assignment rather than burning another), and the recovery floor must count **assigned-but-unsubmitted**
sequences, not merely those carried by an accepted message. SIGN-2 implements this; it is stated here
because a SIGN-2 that skips it is silently wrong in a way no signature test detects.

### 8.5 Relay: the origin's numbers are signed, the receiving bus's are not

This is the collision SIGN-7 raised, and this is its resolution. The signed bytes carry the **origin
bus's** message id and sequence, which are already bus-namespaced (`"<bus-id>-<seq>"`, invariants 1
and 2) and so are globally unambiguous and not a peer's to mint. A **receiving** bus mints its **own
local delivery sequence** for its own recipients' cursors (SIGN-4) **outside** the signed bytes and
binds it in its durable record. Neither bus cedes id authority to a peer, and no relayed signature
breaks.

What is deliberately **not** covered, stated so nobody assumes otherwise:

- **The traversed bus path is outside the signature and cannot ever be inside it** — it grows on
  every hop, so covering it would invalidate the signature at the first relay. A lying peer can
  therefore rewrite the path: loop prevention (RELAY-3) is an **availability** mechanism, never a
  security one, and duplicate suppression rests on idempotency and the origin identity (IDEM-15).
- **A relay must forward the signed bytes verbatim.** Any normalisation on the path breaks
  verification at the far end. The envelope carries the signature and the covered fields exactly as
  the sender produced them; SIGN-6's mandatory-signature ingest rule applies to the relay ingest path
  exactly as it applies to a client send, so a stripped or re-signed message is rejected rather than
  silently downgraded.

**Verify the bytes you are about to ACT on — never a blob that arrived beside them.** A verifier MUST
re-derive the canonical bytes with `Canonicalize` from the exact field values it will route, deliver,
attribute and log, and check the signature over *those*. It must not verify a pre-serialised byte
string supplied by the sender or a relay while taking its routing fields from separately-supplied
envelope JSON: that split re-opens the entire hole §8.1 exists to close — every covered field becomes
forgeable under a signature that still verifies, because the two copies are never compared. If an
implementation does transmit the canonical bytes as an opaque blob, it MUST parse the blob and use
only the parsed contents; the envelope's copy of a covered field is then decoration, and any
disagreement is a rejection.

**SIGN-7 shipped the mechanism, and it is CODE ONLY — nothing below is served.** `internal/relay`
(`signed.go`, `message.go`) now carries the signed relay envelope, the re-derivation and a mandatory
verification at ingest. It registers no handler on any mux and is imported by nothing outside itself
(a guard test, `TestHandshakeHandlerIsNotWiredIntoAnyMux` in `internal/relay/guards_test.go`, walks
the repository and fails if any other package imports it), and it is gated
behind `INVITE-PEERGUARD` (`f5d91dbe`) and `MTLS-RELAYGUARD` (`8192c3c7`), neither of which has
landed. **No relayed signature is verified in production and no cross-bus message flows at all
today.** Everything from here to the end of §8.5 is the contract the gate and wiring tasks must
implement against; §10 carries the concrete envelope, the caps and the status codes.

**Byte-exactness comes from RE-DERIVATION, never from carrying the signed bytes.** The relay envelope
transports the covered field *values* and no pre-serialised canonical byte string — so there is
nothing on the wire for a hop to normalise, and nothing for the rule above to be tempted away from.
The verifier rebuilds the byte string with `Canonicalize` from the exact values it will route,
deliver, attribute and log; `(relay.RelayedMessage).CanonicalBytes()` is the reference implementation.
What makes that deterministic across hops is §8.2's encoding and only that: fixed field order,
big-endian fixed-width integers, every variable-length field length-prefixed, recipients sorted
bytewise by the encoder on a copy, and the body carried as opaque length-prefixed bytes. Because JSON
transports values only, a hop may re-order keys, re-format numbers, re-pad base64 or re-indent and
break nothing. Only changing a *value* changes the signed bytes — and that is a detected forgery.

**The origin sequence is not a second wire field.** The envelope makes one claim about the sequence:
`message_id`. The relay ingress parses the sequence out of that id and passes both to `Canonicalize`,
which re-checks that the two halves of the one server assignment agree (§8.3). A separate sequence
field would be a second claim that could disagree with the first.

**Recipient ORDER is outside the signed payload, and every other notion of "the same message" must
agree with that.** `Canonicalize` sorts a copy of the recipient set, so one signature covers every
permutation of one set. The relay's idempotency fingerprint (`relayFingerprint`) therefore sorts too.
It did not always, and the mismatch was a weapon rather than an inconsistency: a peer that merely
re-ordered a legitimately signed recipient array produced the *same* idempotency key — the origin
message id — with a *different* fingerprint, which is `idem.OutcomeViolation`, which invariant 10
requires be answered by **disconnecting the sender**. Re-ordering was thus a way to get an honest peer,
holding a perfectly valid signature, disconnected. **The rule to carry forward: the fingerprint's
notion of "the same payload" must match the signature's, exactly.** Nothing is lost by sorting —
re-*addressing* a message changes the recipient set, hence the sorted list, hence both the fingerprint
and the canonical bytes, so a retry still cannot quietly re-address anything. Only a pure permutation
collapses, and it collapses because the sender signed a set and not a sequence.

**A relayed BROADCAST cannot be signed under version 1, so it is refused — fail-closed.**
`Canonicalize` rejects an empty recipient set (§8.3: an empty set would sign an audience of nobody)
and a relay broadcast carries no recipient list by construction. There are therefore **no canonical
bytes for a relayed broadcast**, and no signature over one can exist to be checked — not "is not
implemented yet": cannot exist. The relay ingress rejects it (`ErrUnsignable`, HTTP 400 `unsigned`).
The tempting alternative, exempting broadcasts from the mandatory-signature rule, is precisely the
unauthenticated downgrade a hostile peer selects by setting `"broadcast": true`, on the surface with
the largest blast radius, since a broadcast is addressed to every agent on the receiving bus; an
exemption reachable from the wire is not a policy. **Relayed broadcasts consequently do not work
today, and that is deliberate.** SIGN-3 ("broadcast signature covers the recipient set") is the task
that must define what a broadcast's signed audience *is*; until it lands, the honest answer to a peer
is a refusal rather than a silently unverified delivery.

**Cross-bus key trust: pin the ORIGIN bus's signing key at PEERING time. No trust-on-first-use,
anywhere.** Decided 2026-08-07 (`DECISIONS.md`, "Cross-bus key trust: pin the origin bus key at
peering, NO TOFU"); normative here. The reasoning is carried with the rule because the reasoning is
what stops a later "simplification" undoing it.

- **The origin bus's attestation travels intact.** A relayed message keeps the *origin* bus's
  attestation of its own agent's messaging key, signed by the **origin bus's signing key**. An
  intermediate bus may FORWARD an attestation; it may never RE-ATTEST one. If each bus re-attested its
  neighbours' agents' keys locally, a compromised hop would substitute its own key and every cross-bus
  signature would still verify — proving only that the nearest bus asserted something, which is
  exactly what the unsigned envelope already asserted.
- **The origin bus's signing key is pinned at peering time.** It follows that **a bus we have not
  peered with cannot have its agents' signatures verified, by construction. That is intended
  behaviour, not a gap**, and the remedy for a refusal is to complete the peering — never to add a
  fallback. `VerifyRelayed` fetches the pin first and unconditionally, so the property lives in the
  code (`ErrUnpeeredBus`, HTTP 403 `unpeered_bus`) rather than in this paragraph.
- **There is no trust-on-first-use: not a mode, not a fallback, not a hook for one.** TOFU's exposure
  window is the moment of FIRST CONTACT, which is exactly when a hostile intermediate is best placed
  to act; and a fallback is not a narrowing of the hole but the whole of it, because it applies to
  every peer not yet seen — which is every peer, once.
- **The seam is `relay.CrossBusTrust`**, and its shape is the ruling made structural. Its two methods
  — `PinnedBusSigningKeys(busID)` and then `AttestedSignerKey(fqAgentID, pins)` — hand an
  implementation the *only* keys it is permitted to check an attestation against, so "verify against
  the origin bus and nothing else" stops being a comment an implementor can overlook. It replaced a
  one-method key oracle, which could not distinguish an implementation that checked the origin's
  attestation from one that believed the nearest bus's. The package ships no implementation and no
  default, deliberately. **CRYPTO-4 still owns the attested bundle's concrete format, its transport,
  and `key_epoch`** — none of those wire numbers is reserved, so none is picked here.

**The bus TLS key and the bus SIGNING key are SEPARATE, and a peer pins TWO things obtained at
DIFFERENT MOMENTS.** Decided 2026-08-07 (`DECISIONS.md`, "The bus TLS key and the bus SIGNING key are
SEPARATE"); see also §4, which records what losing each one costs.

| | Bus **TLS** key | Bus **SIGNING** key |
|---|---|---|
| Authenticates | the CONNECTION | AGENT KEY BUNDLES (attestations) |
| Pinned by | CLIENTS, from the certificate fingerprint in the invite blob (`DECISIONS.md` E6) | PEER BUSES, at peering time |
| Rotation blast radius | one bus's clients; the two-certificate rollover (E3) makes it non-disruptive | every peer bus's pin — a **federation-wide** event |
| Compromise | impersonate the bus to CLIENTS | forge attestations for **any agent on the bus** |
| Relayed signatures depend on it | no | **yes** |

Fusing them would make every routine TLS rotation inherit the federation-wide cost, and the
predictable outcome is that neither key is ever rotated; it would also make the lesser compromise
automatically become the greater one. **Pinning one does not give you the other, and no field name,
doc phrase or variable may conflate them.** Each key follows the two-key rollover rule
*independently*: a bus rotating its signing key serves both the outgoing and the incoming key for an
overlap window, exactly as E3 does for certificates, or every peering breaks at once and re-peering
becomes indistinguishable from an attack. That, and only that, is why `PinnedBusSigningKeys` returns a
slice — and the multiple pins are consumed by the ATTESTATION check, never by trying each in turn
against the message signature, which would be verifying an agent's message with a bus's key. Both keys
are stdlib per invariant 9: `crypto/ed25519` for signing, `crypto/tls` + `crypto/x509` for the
certificate. Neither is hand-rolled.

**KNOWN GAP — no pin can be established today, so relay ingest cannot be served at all.** The peering
handshake (`PeerEnrollRequest` / `PeerEnrollResponse`, `internal/relay/peer.go`) carries only `bus_id`
and `agents`: **no bus signing key and no certificate fingerprint**. There is therefore no moment at
which a pin is established, `PinnedBusSigningKeys` has no source of truth, and every relayed message
is `ErrUnpeeredBus` by construction. Adding that field belongs to `INVITE-PEERGUARD` (`f5d91dbe`),
which owns the peering handshake: the key is peering material and must arrive over the same
operator-mediated channel the invite uses, because a key field added without that channel is
trust-on-first-use wearing a field name. `internal/relay/doc.go` handoff item 8 is the full text.
Separately, `internal/buscert` (`MTLS-BUSCERT`) *does* mint and load both key files — `bus-tls.key`
and `bus-signing.key` — but as of this writing nothing on the startup path imports it, so a running
bus produces neither.

### 8.6 One byte sequence, two consumers — DUR-5 must not fork it

The canonical bytes are the **single shared input** to both consumers:

| Consumer | What it does with them |
|---|---|
| SIGN-2 / SIGN-5 / CRYPTO-10 | `ed25519.Sign` / `ed25519.Verify` over the bytes **UNHASHED**. |
| **DUR-5** and CRYPTO-11 | `sha256.Sum256` over the **same** bytes — the audit log's content hash (`signing.CanonicalDigest`). |

**Ed25519 signs the message, never a digest.** `crypto/ed25519` exposes no pre-hash mode; handing it
a digest is an API misuse, not a shortcut (RATCHET-7). A test in `internal/signing` fails if anyone
"optimises" the send path by signing the digest.

**DUR-5, read this.** DUR-5 describes its content hash as a hash *of the body*. It is implemented as
SHA-256 over the **canonical bytes**, which include the body length-prefixed along with the sender,
recipients, id and sequence. Hashing the bare body would fingerprint content while proving nothing
about who sent it, to whom, or in what order — and would decouple the audit record from the
signature, which is the silent correctness hole CRYPTO-11 names explicitly. Paired with the
signature over the same bytes, the audit trail proves **delivery, ordering and authorship** without
ever holding the content (invariant 6). If DUR-5's record needs fields this format does not cover —
the traversed bus path, the local delivery sequence, the byte size — they are additional, clearly
out-of-band columns of the audit record; they are never folded into the canonical bytes and never
substituted for them.

### 8.7 Test vectors are a published artefact

`internal/signing/testdata/canonical_vectors.json` holds, for each vector, the input message, the
exact canonical bytes in hex, their SHA-256, and the Ed25519 signature under a fixed key. The key is
**RFC 8032 §7.1 TEST 1**, chosen so that the key derivation itself is checkable against an externally
published pair rather than against our own output; it is a test key and must never be used by a real
agent.

There is deliberately **no `-update` flag**. Regenerating that file to make a red test go green is a
wire-format change, not a fix. Two independent checks keep the vectors honest: one vector's expected
bytes are also written out **by hand** in `canonical_test.go` from the table in §8.3, and a **second,
independently written encoder** in the test file is diffed against the implementation over the whole
sample set.

## 9. The agent-suffixes file — per-name agent-id suffix floors (MSG-FU-SUFFIXFLOOR, 2026-08-07)

Reference implementation: `internal/ids/suffixstore.go`. This is a **third** on-disk file kind,
distinct from the WAL and audit log in §2 — it does not share their magic, framing or MAC, and it is
not part of the write-ahead record stream at all. It exists because the WAL is the wrong place for
this counter: §1–§8 above are unchanged by it, and this section adds to the scope of "the on-disk
format" rather than amending any of them.

**Path and mode.** `<data-dir>/agent-suffixes`, mode `0600` inside the `0700` data directory — the
same posture as `wal-mac.key` (§4).

**Format version.** **3**, in the *same* Spec Server `ondisk-format-version` namespace as the WAL/audit
versions in §1 (values 1 and 2 there are the WAL's; 3 was reserved 2026-08-07 by feature-runner for
this file). Sharing the namespace is deliberate — one registry of "numbers that mean an on-disk
layout" for the whole data directory — even though this file's layout has nothing else in common with
§3's frame format. An unknown version is a hard error, exactly as in §1: a file written by a newer
binary may encode floors this one cannot see, and reading it partially would risk *lowering* a floor,
which is the one thing this file exists to make impossible.

**Layout — plain text, not the binary framing of §3:**

```
agent-bus-agent-suffixes v3 sha256=<64 hex chars over the body>
<agent-name> <highest-suffix-burned>
...                                    (sorted by name, one line per name)
```

- The header line's magic (`agent-bus-agent-suffixes`) is spelled out in full for the same reason as
  the WAL/audit magics in §2: a stray file is identifiable by `head -1` alone.
- The digest is a **plain SHA-256 over the body** (everything after the header's newline), **not**
  the keyed HMAC of §3/§4 — this file does not use `wal-mac.key` at all and has no MAC key of its own.
  It is an **integrity** check against media damage and accidental editing, not an authenticity check:
  it defends the data dir's integrity, not its authenticity, exactly as the WAL header tag does *not*
  claim to in §3.1's own caveat. Verified BEFORE any entry is parsed, for the same reason as §3.2's
  covered-range note: a reader that trusted a line before checking the digest could be reading a floor
  that is lower than what was actually persisted, which is the exact silent rewind this file exists to
  prevent.
- One line per name, `<name> SPACE <suffix>`, sorted by name so the bytes are a pure function of the
  map (what makes the digest meaningful and the file diffable). Names need no quoting because every
  name written has already passed `ValidateAgentName` (`[a-z0-9][a-z0-9_-]{0,63}`, no spaces, no
  newlines — see §8's namesake validator in `agentid.go`), so a space is an unambiguous separator.
- **Floor 0 is spelled by ABSENCE.** A name never written gets no line at all; an explicit `0` is
  rejected on read as a second spelling of the same state (parallel to §1's "a writer only ever emits
  the current version" posture: one state, one spelling).
- The suffix is canonical decimal — no sign, no leading zero, non-zero — the same "one spelling per
  number" rule §8.3 states for the message id's sequence field.

**Write discipline.** Temp file in the same directory, `chmod 0600`, written, fsynced, closed, renamed
over the target, then the **directory** is fsynced — the identical sequence `writeBusIDFile` already
performs for `bus-id` and described nowhere else in this document because it predates it; see that
function's doc for the rationale. A reader therefore sees either the complete previous file or the
complete new one, never a torn one — there is no partial state for a repair pass to reason about, and
therefore (unlike §6) no tail-repair rule that could lower a floor. The **whole map** is rewritten on
every write, which is the interesting cost: O(distinct names ever seen) per issued suffix, where the
WAL is O(record) per append. See `DurableNameSuffixes`'s doc, "Cost" section, for the full accounting
and the amortisation this leaves as a filed follow-up rather than a present concern.

**Failure posture — no repair path, unlike §6.** This is the one place this section's rule differs
sharply from §6's "damage is never fatal" for the WAL/audit files. Any verification failure here —
bad header, wrong magic, unknown version, digest mismatch, a malformed or duplicated entry — is
**fatal** (`ids.ErrSuffixFileCorrupt`) and the file is **never regenerated**. There is no quarantine-
and-continue branch as in §6's table. The reason is the asymmetry between the two files' failure
modes: a WAL that loses its damaged tail still recovers a true prefix of history (§6's whole point),
but a suffix-floors file that "recovers" by regenerating from empty resumes every name at 1 — which
mints straight over agent ids already on disk, breaking invariant 1 outright. A loud, unrecoverable
startup refusal is the only response that does not risk that.

**Relationship to the WAL.** The floors are **not** derived from the WAL and are **not** stored in it.
That is the load-bearing design choice recorded in `DECISIONS.md` (2026-08-07,
"MSG-FU-SUFFIXFLOOR") — see that entry for the full rationale, including why deriving from committed
WAL replay or from the live roster is wrong, and for the residual that still applies to a data
directory that predates this file.

**Production wiring — NOT yet done.** This section documents the file `internal/ids` now knows how to
read and write. `cmd/agent-bus/main.go` does not call `ids.OpenNameSuffixes` anywhere today; it still
constructs a fresh `ids.NewNameSuffixes()` on every start, so no `agent-suffixes` file is written or
read by a running bus yet, and the restart re-minting bug this file exists to close is unchanged in
production. See `CONTRACTS.md` (2026-08-07 entry) and `AGENT_LOG.md` (2026-08-07) for the full
scope statement.

## 10. Loop prevention and the relay envelope (RELAY-2 / RELAY-3 / SIGN-7, 2026-08-07 — NOT SERVED)

Reference implementation: `internal/relay` (`path.go`, `message.go`, `signed.go`). **Nothing in this section is on
the wire today.** `internal/relay` registers no handler on any mux and is imported by nothing outside
itself (a guard test in `internal/relay/guards_test.go` walks the repository and enforces this); it is gated behind two unlanded
tasks, `INVITE-PEERGUARD` (`f5d91dbe`) and `MTLS-RELAYGUARD` (`8192c3c7`). This section documents the
format those gate tasks — and the wiring task that eventually registers the handlers — must implement
against; see `CONTRACTS.md`'s 2026-08-07 entry for the JSON shapes and status codes, and
`internal/relay/doc.go` for the full design rationale. §8.5 above already settles the trust model this
section builds on — the traversed bus path is outside the signature and cannot ever be inside it, the
verifier re-derives the canonical bytes rather than trusting a transported blob, and cross-bus trust
rests on the origin bus's signing key pinned at peering. This section does not restate those
arguments, only applies them to the concrete envelope.

**`bus_path` semantics.** An ordered list of bus ids, oldest first: it starts at the origin bus (the
bus that accepted the message from its own agent) and gains exactly one hop, appended at the end, on
every relay a message passes through — so `bus_path[0]` is always the origin and the last element is
always whichever bus most recently forwarded it. A path carries at most `MaxBusPath` (64) hops, the
same constant as `store.MaxBusPath` (§3's on-disk cap): the relay ingress cap must never exceed the
durable one, because a path accepted here and later refused by the durable record would be an
acknowledged-but-lost message, which invariant 5 forbids.

**Two mechanisms, and both are needed — the ingress rule and the egress split horizon.**

- **Ingress: a bus that finds itself already on an incoming path drops the message.** This is the
  *backstop*: it still works even when a peer lies about the path, because it is evaluated against
  *our own* identity, the one thing on the path we can independently verify.
- **Egress: a bus never forwards to a peer already on the path** (the split horizon). This is the
  *optimisation*: it stops cycle traffic before a byte leaves the process, so a correct mesh never puts
  a message on the wire toward a bus that has demonstrably already seen it.

Neither alone is sufficient. Drop the split horizon and a correct mesh sends every message once around
every cycle before anything stops it. Drop the ingress check and one lying peer — one that strips
itself, or us, out of the path it forwards — restores unbounded circulation, because the split horizon
it defeated was the only thing that had been stopping it.

**What an untrusted `bus_path` is, and is not, guaranteed to say.** Guaranteed by the ingress
validation (`ValidateBusPath` / `CheckIncomingPath`): well-formed (every hop a legal bus id), at most
`MaxBusPath` hops, no duplicate hop (compared case-insensitively, closing the same confusable-id
avenue `ValidatePeerBusID` closes elsewhere), `bus_path[0] == origin_bus`, and this bus is not
currently on it. **Not guaranteed: that any of it is true.** A peer that strips us out of the path
before forwarding defeats the ingress check completely, and there is no detection of that — the path
is metadata outside the signature (§8.5) and a lying peer can rewrite it freely. There is, however, no
*second* evasion route: hop comparison folds ASCII case and every hop's charset is already restricted
to `ids.ValidateBusID`'s `[A-Za-z0-9_-]`, so no Unicode-folding trick reaches the membership check.

**The relay idempotency fingerprint EXCLUDES `bus_path`, and this is deliberate, not an oversight.**
The fingerprint (`relayFingerprint`) covers the message's identity-defining content — origin bus,
origin message id, sender, the broadcast flag, size, content hash, the sender's signed timestamp, and
the recipient list **sorted** (see below, and §8.5: sorted because the *signature* sorts, so the two
agree about what "the same payload" means) — and nothing about how this particular copy was routed.
In a meshed or cyclic
topology, the *same* message legitimately arrives at one bus by more than one route, and each copy
carries a *different* `bus_path` — that is the normal steady state, not an edge case. If the path were
covered by the fingerprint, the second arrival would present the same idempotency key (the origin
message id — see below) with a different fingerprint, which is `idem.OutcomeViolation`. CLAUDE.md
invariant 10 mandates that a violation is rejected, logged, **and the offending peer disconnected** —
so covering the path would make two correct peers disconnect each other as the *ordinary* behaviour of
a correct mesh, a self-inflicted partition produced by the very mechanism meant to make retries safe.
Relatedly, the relay idempotency key is not a per-hop value the forwarding peer mints — it **is** the
origin's `message_id` (`idem.HeaderName` on the wire), enforced by `ValidateRelayRequest`. That
identity is what makes two copies of one message, arriving by two disjoint paths, resolve to *one*
`idem.Scope`; a peer free to mint a fresh key per hop would defeat invariant 10 silently, because every
copy would look new and every copy would be delivered.

**Loop prevention COMPLEMENTS idempotency and never substitutes for it — invariant 10 states this
directly.** RELAY-3's traversed-path check bounds *traffic*: it stops a message being re-relayed back
through a bus that has already seen it, which is a topology-shape defence. It does nothing for a
message that reaches the same bus via two *different*, loop-free paths — a diamond rather than a
cycle — which only the idempotency check (origin message id as key, content fingerprint as the
retry-vs-violation test) catches. A relay implementation with loop prevention but without idempotent
application will silently double-deliver in exactly that diamond topology; one with idempotent
application but without loop prevention will burn unbounded traffic circulating a message around any
cycle in the peer graph forever. Both are required, and neither is optional because the other exists.

**The envelope changed for SIGN-7, and it is NOT a compatibility break.** `internal/relay` is served
by nothing and imported by nothing, so this format is not yet on any wire and there is nothing to stay
compatible with. Against the shape RELAY-2 first described:

| Change | Field | Why |
|---|---|---|
| **removed** | `sent_at_unix_ns` | the ORIGIN BUS's nanosecond clock reading — a different quantity, from a different source, in a different unit from the one the signature covers. An envelope carrying it did not carry the sender's signed clock at all, so a receiving bus **could not reconstruct the canonical bytes**: byte-exactness was impossible, not merely unimplemented. |
| **added** | `timestamp_unix_ms` (`int64`) | the SENDING AGENT's signed wall clock — the exact integer `signing.Message.TimestampUnixMilli` covers. **One timestamp, the signed one, and nothing on the path converts it**; every conversion between the wire form and the signed form is a place the two sides drift. Milliseconds, not nanoseconds, because the value must be exactly representable as a JSON number (§8.3). |
| **added** | `signature` (64 bytes, base64 in JSON) | the origin agent's detached Ed25519 signature over `Canonicalize`'s output, carried **verbatim on every hop**. `ed25519.SignatureSize` exactly; any other length is treated as no signature at all. |

`Forward` re-emits every other field unchanged, including the signature and the signed timestamp: the
**only** field that changes on a hop is `bus_path`, which is the one field that is outside the
signature and can never be inside it, because it grows on every hop (§8.5).

**`timestamp_unix_ms` is PROVENANCE and must NEVER become the local `store.Message.SentAt`.**
`store.Message.VisibleTo` compares `SentAt` against an agent's **enrolment instant** — "you do not
receive mail sent before you existed" — which is an **authorization** boundary. A wiring task that
copied the relayed value into the local record would hand a peer the power to choose a message's
visibility: backdate it out of every local agent's view, or forward-date it into the view of agents
that enrol later. **A valid signature over it changes nothing about that**: the signature proves the
sending agent *chose* that number, not that the number is *true*, and an agent may sign any clock
reading it likes. The local bus stamps its own acceptance time; the relayed one is worth recording in
the audit trail and showing an operator, and is never an input to a visibility or ordering decision.

**Verification is mandatory at ingest, and a relayed broadcast is refused.** `ValidateRelayRequest`
takes a `CrossBusTrust` as a **required** parameter and runs `VerifyRelayed` before it returns, so no
validated-but-unverified `RelayedMessage` can exist; a nil trust is a refusal, not a skipped check,
and `NewRelayHandler` fails at construction without one. A relayed **broadcast** carries no recipient
list, canonical format v1 refuses an empty recipient set, and so no signature over one can exist —
the ingress rejects it rather than exempting it (§8.5; SIGN-3 owns the fix). **Relayed broadcasts do
not work today, deliberately.**

**Three signature-related error codes, and the split between them is deliberate.** All are FINAL:
re-sending identical bytes cannot change any of the three verdicts, so none invites a retry and none
is a 503 or a `dropped_reason` (a `dropped_reason` rides on HTTP 200 and means "settled, and not your
fault", which none of these is).

| Code | HTTP | Meaning | Sentinel |
|---|---|---|---|
| `unsigned` | **400** | the envelope could never be verified **by anyone**: no signature, a wrong-length one, or a shape the canonical format cannot encode — a relayed broadcast being the case that actually occurs | `ErrMissingSignature`, `ErrUnsignable` |
| `bad_signature` | **403** | the envelope is well-formed but is **not attributable** to the agent it names: the signature does not verify, or the origin bus attests no key for that sender | `ErrBadSignature`, `ErrNoSignerKey` |
| `unpeered_bus` | **403** | we hold **no peering-time pin** for the origin bus's signing key, so nothing that bus's agents sign is verifiable here | `ErrUnpeeredBus` |

The 400/403 split is "nobody could verify this" versus "we will not attribute this to that agent" —
the second is an authorization answer. `unpeered_bus` is split out from `bad_signature` because they
are different **operator** problems with different remedies: "we have never peered with your bus" is
fixed by completing a peering, while "your signature is wrong" starts a forgery investigation. One
code for both would send an operator hunting an attack on what is the ordinary day-one state of an
unfinished federation. `unpeered_bus` is *not* a "not yet", and must never be answered as one: nothing
the sending peer can do on a retry establishes a pin, and there is deliberately no trust-on-first-use
path that would make the code unreachable.
