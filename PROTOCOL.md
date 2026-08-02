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
- **At least one recipient.** An empty set signs an audience of nobody.
- **Sequence 0 and timestamp ≤ 0 are refused** as unset fields, the same posture `internal/ids` takes.
- **Body ≤ 1 MiB** (`MaxBodyLen`, matching `MaxPayloadSize`). A zero-length body is legal and encodes
  as a length prefix of 0 — an empty field, never an absent one.

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
