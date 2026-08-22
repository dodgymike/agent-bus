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

**This table covers the WAL frame layout only (versions 1 and 2).** The `ondisk-format-version`
namespace is shared by every on-disk file format this project defines, not only the WAL: version 3
is the `agent-suffixes` file (§9) and version 4 is the `wal-index-floor` file (§11). Each is
documented in its own section rather than folded into this table, because neither shares this
table's frame layout at all.

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
backup of the other** — see §8.5, which is where the distinction is normative.

**The filenames are settled, and a running bus now produces them** (`MTLS-BUSCERT`, 2026-08-07).
`cmd/agent-bus` calls `buscert.LoadOrCreate` on startup — after the data-directory lock and before
the listener — so the three secrets of the table above live at these paths:

| file | mode | secret? | contents |
|---|---|---|---|
| `<data-dir>/wal-mac.key` | `0600` | **SECRET** | the WAL/audit-log MAC key (`internal/wal`, §4 above) |
| `<data-dir>/bus-tls.crt` | `0644` | PUBLIC | one PEM `CERTIFICATE` block: the self-signed bus certificate whose fingerprint clients pin |
| `<data-dir>/bus-tls.key` | `0600` | **SECRET** | one PEM `PRIVATE KEY` block, PKCS#8 Ed25519 — the key inside that certificate |
| `<data-dir>/bus-signing.key` | `0600` | **SECRET** | one PEM `PRIVATE KEY` block, PKCS#8 Ed25519 — the SEPARATE attestation key peer buses pin |

The certificate is `0644` deliberately: it is sent to every client on every handshake and its
fingerprint is published in invite blobs, so it is public by construction. Both **bus key** files are
`0600`, and a **bus** key file with any group-or-other permission bit set is a **fatal** startup
error, not a warning. That check is `internal/buscert`'s and applies to `bus-tls.key` and
`bus-signing.key` only: `wal-mac.key` is read with a plain `os.ReadFile` and its mode is **not**
enforced on load (`internal/wal/mackey.go`), so a loosened `wal-mac.key` is a real exposure that
nothing refuses — do not read the row above as a promise that it is checked. Also fatal for the bus
material: a key or certificate that is malformed, truncated, of the wrong PEM block type, not
Ed25519, or not matched to the certificate; so is a certificate outside its validity window; and so
is finding **some but not all** of the three present. Nothing is ever silently regenerated —
regenerating the TLS key breaks every client that pinned the old fingerprint (there is no TOFU), and
regenerating the signing key kills the pin held by every peer bus. Fresh material is minted **only**
when all three are absent, and that start says so once, loudly, at `warn`:

```
level=warn msg="bus certificate and signing key GENERATED: …" data_dir=… cert=…/bus-tls.crt fingerprint=<64 hex> not_after=<RFC3339>
```

Generating the material is separate from serving it: this step only produces the certificate and
keys on disk. The listener that presents this certificate on every handshake is TLS-only (invariant
11) — there is no plaintext listener and no fallback — and the `server started` line reports `tls=true`
plus `tls_min_version` and `client_auth` alongside `bus_cert_fingerprint`, derived from the config the
listener actually serves rather than restated as a separate claim.

**Back up all THREE secrets, plus the log.** `wal-mac.key` + the log alone is not a backup of this
bus: restore it without `bus-tls.key` and no client that pinned the certificate can connect; restore
it without `bus-signing.key` and every peer bus's pin is dead and re-peering is required. A backup
that omits any one of the three restores a bus that cannot do its job, and none of the three is
regenerable. A backup that contains all of them is worth exactly as much to a thief as the data
directory itself; protect it accordingly.

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

#### 8.4.1 What SIGN-2 actually shipped, and where it DIVERGES from the paragraph above (2026-08-07)

The paragraph above is the specification. This subsection is the implementation, written separately
because one clause of the specification was **deliberately not implemented as stated**.

**The durable artefact is a SEQUENCE FLOOR, not a per-key assignment record.** A new WAL entry kind
`"seqfloor"` (`hub.SeqFloorRecordKind`, body `{"v":1,"floor":N}`) records that **every sequence
`<= N` is BURNED and will never be issued by this bus again**. It is fsynced AHEAD of any number
above the proven floor being handed out, in batches of `hub.MintBatchSize = 256` — so the amortised
cost is one extra fsync per 256 mints and **zero on the send path itself**. On-disk shape:
`CONTRACTS-ONDISK.md`.

**THE DIVERGENCE.** The specification says the assignment must reach disk "bound to the idempotency
key that asked for it". It is **not**. What reaches disk is only that the NUMBER is burned; the table
mapping `(agent, op, idempotency_key) → sequence` is **in memory only**, bounded
(`MaxOutstandingMintsPerAgent = 64`, `MaxOutstandingMints = 8192`) and expiring (`MintTTL = 15m`).

That is a deliberate narrowing, and it is safe, because the property the binding was there to protect
is invariant 1 (never issue a number twice) and the floor protects that directly and more cheaply.
What is LOST is only that a reservation does not survive a restart:

- A client that minted and then met a restart gets `409` (`hub.ErrUnknownMint`) on its send. It
  re-mints under the **same** idempotency key, receives a **fresh** sequence, re-signs and re-sends.
- The old number stays burned — a hole, which §8.4 above already declares correct and which
  `internal/ids/sequence.go` pins as the rule for consumers: **strictly increasing, never dense.**
- This cannot double-apply. If the crash landed AFTER the message became durable, the re-sent request
  carries the same key and the same fingerprint, so `internal/idem` answers `OutcomeRetry` and
  returns the ORIGINAL result. One message, not two.

**The counting argument is RETIRED, and replaced by a stronger direct assertion.** `hub.Open`
previously derived its floor as `NextIndex - 1` on the argument that "every sequence issued is <= the
WAL index of the prepare carrying it" — which held only while a sequence could not be issued before a
record existed, and the mint breaks that outright. `Open` now takes the **maximum** of
`NextIndex - 1`, the highest replayed `"seqfloor"` value, and the highest applied sequence
(`NextIndex - 1` is kept purely as defence in depth — it can only raise the floor). The runtime check
is now the direct assertion **every sequence handed out is <= the durably-recorded floor**, asserted
where the sequence is issued and re-asserted at publish; a violation POISONS the hub exactly as
before.

**Bounds fail CLOSED.** Over the per-agent bound is `hub.ErrAgentQuota`, over the global bound is
`hub.ErrCapacity`; **another agent's reservation is never evicted to make room.**

### 8.5 Relay: the origin's numbers are signed, the receiving bus's are not

This is the collision SIGN-7 raised, and this is its resolution. The signed bytes carry the **origin
bus's** message id and sequence, which are already bus-namespaced (`"<bus-id>-<seq>"`, invariants 1
and 2) and so are globally unambiguous and not a peer's to mint. A **receiving** bus assigns its own
**delivery position** for its own recipients' cursors **outside** the signed bytes and binds it in
its durable record. Neither bus cedes id authority to a peer, and no relayed signature breaks.

**Amended 2026-08-14 by `SIGN-1-FU-REORDER-WATERMARK`.** The receiving bus's number is a **delivery
POSITION** (the WAL commit index), not a "local delivery sequence", and a recipient's cursor is a
position rather than a sequence. The distinction is not cosmetic: it is what makes a relayed message
deliverable at all. A relayed message is committed like any other, so it takes the next commit index
and therefore lands ABOVE every reader's cursor — it can never arrive below one and be silently
suppressed. The earlier wording implied the receiving bus mints a *sequence* the recipient then
orders and freshness-checks on, which is the design that task removed; a recipient performs **no**
sequence-based freshness check and deduplicates on `message_id`. See `CONTRACTS-ONDISK.md`, "The
DELIVERY POSITION, and why it is not on disk".

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
It did not always, and the mismatch was a defect worth closing regardless of severity: a peer that
merely re-ordered a legitimately signed recipient array produced the *same* idempotency key — the
origin message id — with a *different* fingerprint, which is `idem.OutcomeViolation`. Invariant 10
as narrowed (2026-08-08) answers `OutcomeViolation` with reject-and-log only, never a disconnect —
so re-ordering can no longer get an honest peer's connection dropped — but reject-and-log is still
the wrong answer here: `idem.Store.Lookup` remembers only the FIRST fingerprint it saw under a key
(`internal/idem/store.go`), so whichever re-ordered copy arrives first is accepted as `OutcomeNew`
and delivered — the harm lands on the *second* arrival, which is not new content from a confused or
malicious sender, it is the SAME signed payload honestly re-sent, and treating it as a violation means
the honest peer can never get THAT delivery acknowledged on this route (until the key's retention
window expires) and is logged as a protocol violator for content it was never told was wrong.
**The rule to carry forward: the fingerprint's notion of "the same payload" must match the
signature's, exactly.** Nothing is lost by sorting —
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

#### 8.6.1 The RELAYED case: the same rule, applied to bytes signed on ANOTHER bus (RELAY-24-BLOCKER-HUBINGEST, 2026-08-14)

**Nothing in §8.6 changes.** The rule is still *hash the bytes the signature covers*, and what follows
is that rule applied to a message whose signature was made on a different bus. It is not a second rule
and it does not supersede the one above. It is written down because the bytes the rule selects are not
the local record's, and a reader who assumes they are will fail to reproduce a relayed record's hash
and conclude the trail is wrong.

**A relayed record has no canonical bytes of its own, and cannot be given any.** The local record of a
relayed message carries a message id THIS bus minted — a bus never adopts a peer's id (invariant 1) —
and a sender belonging to the ORIGIN bus (invariant 2). `Canonicalize` refuses that pair
unconditionally: the origin binding of §8.3 compares the sender's bus half against the bus that minted
the id, EXACTLY and with no fold, because a message is signed by an agent of the bus that minted its
id. So there is no local derivation to prefer over the origin's. It is not merely unused — it is
impossible, and `internal/signing` will not produce it.

**The content hash is therefore computed over the ORIGIN's canonical bytes.** Exactly two fields are
substituted — the message id and the sequence — because they are exactly the two this bus re-minted;
the sender, the recipients, the sender's timestamp and the body are already the origin's on both
sides. The result is byte-identical to what `(relay.RelayedMessage).CanonicalBytes()` builds, which is
the same byte string the origin agent signed and the same one the relay ingress re-derived and checked
the signature against before this bus recorded anything. Hashing anything else would leave a relayed
record's hash covering bytes NOBODY ever signed.

**The substitution is gated on the message being RELAYED, not on the origin fields being present.**
A local send that arrives at the write path carrying an origin assignment is refused as an internal
error rather than honoured. Deriving the behaviour from the field alone would mean any future local
caller that populated it — for any reason — had silently moved a local send's audit hash onto an id of
its own choosing.

**This is NOT the substitution the paragraph above forbids.** That paragraph governs the out-of-band
fields it names — the traversed bus path, the local delivery sequence, the byte size — which are
additional columns of the audit record, are never folded into the canonical bytes, and never stand in
place of them. Neither happens here. The hash is still SHA-256 over a canonical byte string produced
by `Canonicalize`; the only question this subsection answers is which server assignment that byte
string was produced under, and the answer is the one the signature was made under.

**What a READER of the trail must know, stated carefully.** For a relayed record the content hash does
**not** reproduce from that record's own `message_id` and `seq`. It DOES reproduce from the ORIGIN's
pair, and that pair is durably recorded: the origin message id is the message record's
`idempotency_key`, and the origin sequence parses out of it. The MESSAGE log therefore carries
everything needed to re-derive the hash. **The AUDIT log alone does not** — the audit record carries
neither the origin message id nor the sender's claimed timestamp — but that limitation is not new and
is equally true of a local record, whose hash also needs the sender's timestamp from the message log.
Reproducing any content hash from this trail has always required both files.

**The discriminator is the SENDER's bus half, and NEVER the bus path.** A multi-hop path does not imply
a relayed record: `internal/hub/buspath_test.go` publishes a three-hop path
(`[busa, busb, testbus]`) with a LOCAL sender, and that record's hash IS locally reproducible from its
own assignment. The structural test is whether the sender's bus half is this bus's. A future reader,
tool or fsck that keys on the path length will misclassify exactly those records.

**This is not a precedent for inventing a hash where the correct bytes do not exist.** The relayed case
is the OPPOSITE of the broadcast case, and the difference is the whole justification. For a relayed
message the correct bytes EXIST, are already computed by `(relay.RelayedMessage).CanonicalBytes()`,
and were already verified against a signature before the hub was asked to record anything — the
substitution selects bytes that are already there. For a broadcast under signing format v1 there are
no such bytes at all: `Canonicalize` rejects an empty recipient set and a broadcast is stored as a
FLAG rather than an expanded roster, so any value chosen would be one this project invented. The
broadcast path accordingly still **fails closed**, SIGN-3 still owns the question of what a
broadcast's signed audience IS, and nothing in this subsection answers it.

**Hashing the bare body remains FORBIDDEN, for a relayed record exactly as for a local one.**
`store.ContentHash(body)` is also 64 lowercase hex characters, so no framing check, no decoder and no
assertion on shape can tell the two apart. Three value-pinning tests are the only defence and each
rebuilds the expected digest independently rather than by calling the producer:
`TestSendWritesItsAuditRecord` (`internal/hub/audit_roundtrip_test.go`),
`TestIngestRelayedAuditHashIsTakenUnderTheOriginAssignment`
(`internal/hub/relayingest_relay24blocker_test.go`), which additionally asserts that the LOCAL
derivation is refused outright, and `TestLocalSendAuditPayloadIsUnchanged`
(`internal/hub/buspath_test.go`), whose whole-record golden fails if a local send's trail entry moves
by a byte.

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

**A MISSING file is a second, distinct fatal case — added by `AUTH-3-FU-FAILOPEN`, 2026-08-07 — and
is not the same thing as corruption.** `OpenNameSuffixes` on an absent file returns an empty,
`existed=false` allocator rather than an error; that alone is not fatal at the `internal/ids` layer.
What is fatal is `cmd/agent-bus`'s own policy on top of it: if the data directory otherwise **has
history** — it was non-empty at startup, or the WAL
holds records — a missing `agent-suffixes` file means the directory's already-issued suffixes cannot
be proven, and starting anyway would resume every name at 1 and re-mint a live agent's id. `run()`
refuses in that case, naming the file path and two remedies: restore it from backup, or — **only** if
the directory genuinely never issued an agent id — restart exactly once with the `-backfill-suffix-
floors` flag (`CONTRACTS-CLI.md`), which derives the floors from the durable log instead of trusting
an absent file. See `cmd/agent-bus/suffixfloors.go` and `CONTRACTS-ONDISK.md`'s "Recovery always
reaches a running server — with three named exceptions" for the operator-facing table this feeds.

**Relationship to the WAL.** The floors are **not** derived from the WAL and are **not** stored in it.
That is the load-bearing design choice recorded in `DECISIONS.md` (2026-08-07,
"MSG-FU-SUFFIXFLOOR") — see that entry for the full rationale, including why deriving from committed
WAL replay or from the live roster is wrong (except as the explicit, operator-invoked `-backfill-
suffix-floors` fallback above), and for the residual that still applies to a data directory that
predates this file.

**Production wiring — DONE (`AUTH-3-FU-FAILOPEN`, 2026-08-07).** This section previously said
`cmd/agent-bus/main.go` never called `ids.OpenNameSuffixes` and that a restarting bus therefore still
re-minted agent ids. That is now **false**, and stale text of that severity is worse than none, so it
is corrected here rather than left standing (the same correction already made in `CONTRACTS.md` and
`internal/ids/doc.go`, `cb79486`, which this file missed at the time). `ids.NewNameSuffixes()` — the
in-memory-only constructor — has been removed from `cmd/` entirely; the call site that used to build
it now carries a comment stating it "is gone from cmd/ and MUST NOT come back, on any path, including
as a fallback for a failed open or a failed seal." `cmd/agent-bus/suffixfloors.go`'s
`openSuffixAllocator` calls `ids.OpenNameSuffixes(dataDir)` on every startup — ahead of the agent-id
minter and the auth service — so every enrolment goes through the durable allocator. An `agent-
suffixes` file **is** written and read by a running bus today, and a restart does **not** re-mint a
live agent id.

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
always whichever bus most recently forwarded it. A received path carries at most
`MaxReceivedBusPath` (63) hops. The receiving bus appends itself before persistence, producing at
most `store.MaxBusPath` (64) stored hops (§3's on-disk cap). Reserving that local hop at relay
ingress prevents a path accepted there from being refused by durable ingest as an
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
`MaxReceivedBusPath` (63) received hops, no duplicate hop (compared case-insensitively, closing the
same confusable-id avenue `ValidatePeerBusID` closes elsewhere), `bus_path[0] == origin_bus`, and this
bus is not currently on it. **Not guaranteed: that any of it is true.** A peer that strips us out of
the path
before forwarding defeats the ingress check completely, and there is no detection of that — the path
is metadata outside the signature (§8.5) and a lying peer can rewrite it freely. There is, however, no
*second* evasion route: hop comparison folds ASCII case and every hop's charset is already restricted
to `ids.ValidateBusID`'s `[A-Za-z0-9_-]`, so no Unicode-folding trick reaches the membership check.

**The relay idempotency fingerprint EXCLUDES `bus_path`, and this is deliberate, not an oversight.**
The fingerprint (`relayFingerprint`) covers the message's identity-defining content — origin bus,
origin message id, sender, the broadcast flag, size, content hash, the sender's signed timestamp, and
the recipient list **sorted** (see below, and §8.5: sorted because the *signature* sorts, so the two
agree about what "the same payload" means) — and nothing about how this particular copy was routed.
**Delivery is at-least-once, never exactly-once — that is the foundational fact invariant 10 exists
to absorb, not an edge case this format works around.** In a meshed or cyclic
topology, the *same* message legitimately arrives at one bus by more than one route, and each copy
carries a *different* `bus_path` — that is the normal steady state, not an edge case. If the path were
covered by the fingerprint, the second arrival would present the same idempotency key (the origin
message id — see below) with a different fingerprint, which is `idem.OutcomeViolation`. CLAUDE.md
invariant 10 (as narrowed 2026-08-08) mandates that a violation is rejected and logged — it does
**not** disconnect the sender, so covering the path would no longer partition a correct mesh — but it
would still make every second-and-later arrival of one message over a diamond topology get rejected
and logged as a protocol violation against a peer that did nothing wrong, which is exactly the false
positive the fingerprint exists to avoid: the *ordinary* behaviour of a correct mesh would be
misclassified as an attack on every legitimately duplicate delivery.
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

**The envelope changed for SIGN-7, and it is NOT a compatibility break.** `internal/relay` is now
imported — `internal/httpapi/peermount.go` carries the only permitted mount site for the peer routes
(`ed77bba`, and `internal/relay/guards_test.go` bounds it to that one file), and the offline
operator CLI `cmd/agent-bus/peer.go` uses its `PeerStore` — but it is still **served by nothing**: no
binary in `cmd/` constructs an `httpapi.PeerSurface`, so `mountPeerSurface` registers nothing and the
peer paths answer 404 until RELAY-24 wires one. This format is therefore not yet on any wire and there
is nothing to stay compatible with. Against the shape RELAY-2 first described:

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

**A fourth ingest refusal, and it is NOT signature-related (RELAY-21, `14eafd9`).** It is stated apart
from the three above because it blames the *roster*, not the envelope: the message is perfectly well
formed and its signature verified.

| Code | HTTP | Meaning | Sentinel |
|---|---|---|---|
| `unknown_recipient` | **404** | the message names an agent in **this bus's** namespace that this bus's roster does not hold — **nothing is written**, and the code is **FINAL** | `ErrUnknownLocalRecipient` |

**`NOTHING IS WRITTEN` is a durability claim, not a courtesy.** `Acceptor.Accept` asks the roster in
step 1, *before* the durable write in step 2, precisely so a name nobody holds costs this bus nothing
permanent: an id admitted by anything other than the roster would burn that name for ever, because ids
are never reused, including across restarts (invariant 1).

**It is `404` and not `503`, and the difference is an amplification bound.** A `503` would tell the
sending peer's retry machinery to re-send for its whole retry horizon, so a peer could aim a stream of
messages at names that do not exist here and have our own control retry each one. Every 4xx but `408`
and `429` is FINAL (`(*PeerRefusedError).Retriable` decides from the **status**, not the code string),
so the sending bus stops and its operator gets a code whose remedy is its own roster. It is also not
`invalid_relay`: a peer told "invalid" would go hunting a malformed field it does not have.

It discloses no roster membership to anyone who could not already ask — only a peered bus reaches this
handler, and peers exchange full rosters over the roster-sync surface by design.

## 11. The WAL record-index floor — `<data-dir>/wal-index-floor` (`f56c723`, `1ca7f83`, 2026-08-07)

Reference implementation: `internal/wal/indexfloor.go`, introduced by commit `f56c723` and
hardened to a keyed MAC by commit `1ca7f83` (both git commits — distinct from the Spec Server
task ids, not git shas, named next). Full operational rationale — the two P0 defects this file
closes (Spec Server tasks `e120153b` and `db350e39`) and how invariants 1 and 6 were reconciled
without narrowing either — is in `CONTRACTS-ONDISK.md`'s "durable WAL record-index floor" section
and `DECISIONS.md`, 2026-08-07; this section pins the bytes.

**A fourth on-disk file kind**, alongside §2's WAL and audit magics and §9's `agent-suffixes` file:
outside the WAL, atomically replaced, not part of the record stream. Its format version is **4**, in
the same `ondisk-format-version` namespace as §1 (values 1/2, the WAL frame layout) and §9 (value 3,
`agent-suffixes`) — one registry of "numbers that mean an on-disk layout" for the whole data
directory, exactly as §9 already states for its own version. **§1's table above covers only the WAL
frame layout (versions 1/2); versions 3 (§9) and 4 (this section) are documented in their own
sections because their layouts have nothing else in common with §3's frame format.**

**Path and mode.** `<data-dir>/wal-index-floor`, mode `0600` — the same posture as `wal-mac.key`
(§4) and `agent-suffixes` (§9).

**Layout — a header line, then a three-line body:**

```
agent-bus-wal-index-floor v4 hmac-sha256=<64 hex>
reserved <decimal uint64>
written <decimal uint64>
sealed <0|1>
```

**The tag is a keyed HMAC-SHA256, under the SAME `wal-mac.key` every WAL frame is authenticated
under (§3.2, §4) — not a separate key.** It covers `agent-bus-wal-index-floor v4\n` followed by the
body, i.e. every byte of the file except the tag field itself; covering the version line binds the
tag to the format version so a future body can never be replayed as a v4 one. Computed with
`crypto/hmac` + `crypto/sha256` exactly as the frame MAC is, and compared with `hmac.Equal` — never
`==` or `bytes.Equal` (§3.2, §7).

**This replaced an UNKEYED `sha256=` digest on 2026-08-07, and the replacement was not cosmetic.** A
security gate proved the unkeyed digest forgeable with no key at all — flip `sealed 0` to `sealed 1`,
recompute the plain SHA-256 by hand — and a reopened bus reissued indices at 2268 of 2289 measured
truncation offsets, while every frame it went on to write still carried a valid MAC, because the
server itself computes that one. **The on-disk format version was NOT bumped for this change**: see
below for why the older shapes remain readable rather than being retired by a version bump.

**Three fields, and there is deliberately no field that can rewind a floor:**
- `reserved` — no WAL record index above this value has ever been authorised by this data directory.
- `written` — every WAL record index at or below this value is burned: written to the log, or
  permanently skipped by recovery.
- `sealed` — `1` means the run that wrote the file reached a clean `Writer.Close`, so `written` is
  exact rather than a lower bound. It is exempt from the monotonicity rule below: it is cleared to
  `0` at every `begin`, fsynced before the writer may append, so a crash can only ever leave it `0` —
  clearing it can only make the next start *more* conservative, never less.
- `reserved` and `written` are strictly non-decreasing, enforced in code. Always `written <=
  reserved`; a loaded file where that fails is corrupt.

**Read in THREE shapes, written in only one — a deliberate compatibility carve-out, not residual
mess:**

| shape | written by | read as |
|---|---|---|
| `hmac-sha256=` + 3-line body | the current writer | authenticated; `sealed` trusted |
| `sha256=` + 3-line body | the same-day pre-HMAC revision | digest checked, `sealed` discarded and forced `false`, logged at WARN, rewritten keyed at the next start |
| `sha256=` + 2-line body (no `sealed` line) | `f56c723`, which shipped to `main` | same as the row above — there is no `sealed` line to read |

Neither older shape gets the version bumped, because neither is a layout an *older* binary would
misread into a *lower* floor — the one thing the version field defends (§1). The two-line shape was
briefly treated as corrupt on the premise that "v4 never shipped without `sealed`"; that premise was
false (`f56c723` is in `main` and writes exactly that), so a routine upgrade hit
`ErrIndexFloorCorrupt` and refused to start over a shape this binary itself once wrote. Discarding a
legacy file's `sealed` bit costs at most one burned reservation block on the first start after
upgrade; trusting an unauthenticated `sealed` bit would have cost invariant 1.

**Write ordering matches §9's discipline exactly:** a temp file in the same directory is written,
fsynced, renamed over the target, and then the **directory** is fsynced. A crash can therefore never
leave a torn floor file — a read failure means either the file is genuinely absent (benign) or the
bytes are damaged or tampered with, never a partial write.

**Consumption at `Open`.** `wal.Open` resumes indexing at the **maximum** of the replayed high-water
mark, one past the highest index the repair pass observed, and `written+1` from this file — **plus
`reserved+1`** whenever the previous run's seal reads `0`. Reservation during a run is amortised in
blocks of 64 (`indexReserveBlock`), fsynced *ahead* of the index it authorises; `Writer.Append`
poisons the writer rather than ever issuing an index that outruns its own reservation. The file is
read **after** `wal-mac.key` has been settled (§4, §6), not before: reading it first would make a
merely wrong key surface as a corrupt floor and point an operator at deleting it — the one remedy
that forfeits invariant 1.

**Failure posture — three states, parallel to §9's but its own, equally strict, ruling:**
- **Missing** is benign — a data directory written before this file existed — and yields a floor of
  zero, logged at WARN when the directory is not otherwise fresh.
- **Unverified**: if `wal-mac.key` was absent and freshly minted for *this* open, nothing the
  previous identity wrote can verify, floor included. The numbers are still kept — they are only ever
  consumed as a *raise*, so they can only make the next start more conservative — while `sealed` is
  discarded, and the file is logged at **ERROR**.
- **Corrupt** — bad header, unknown version, a tag that fails under the directory's own key, a
  malformed number, or `written > reserved` — is **fatal** (`wal.ErrIndexFloorCorrupt`) and the file
  is **never regenerated**, for the identical reason §9 gives for `agent-suffixes`: regenerating
  either file risks resuming an index (and, through `internal/hub`'s derivation, a message sequence)
  below numbers already handed out, silently, with nothing downstream able to detect it.

**Scope limit, stated plainly.** Only `wal.Open` — the WAL proper — attaches a floor. The **audit**
log writer (`wal.OpenWriter`, `wal.KindAudit`) attaches none, so an audit log's discarded tail
record index can still be reissued; this file protects the WAL's record indices and, through
`internal/hub`'s derivation, message ids — it does not protect the audit trail.

## 12. Backward propagation of a terminal delivery outcome (`ACK-5`, 2026-08-21)

Reference implementation: `internal/relay/ackback.go` (the decision and the emission),
`internal/store/provenance.go` (the body-free provenance accessor), `cmd/agent-bus/ackback.go` (the
one place the two meet). The wire frame itself is `POST /v1/peer/ack`, unchanged, and is specified in
`CONTRACTS-HTTP.md`; `ACK-CONTRACT.md` §9.4 is the ruling. This section is the maintainer-facing
account of the **traversal**, which is the part with a failure mode worth writing down.

**NO NEW WIRE VERSION IS SPENT, and no new record type, route, on-disk file, flag or environment
variable.** Do not go looking for one. The frame is `relay.PeerAckRequest` at
`relay.AckWireVersion` = the already-reserved `relay-wire-version = 1` (`CONTRACTS-ONDISK.md`,
"Record types / wire protocol versions"); this task adds an emitter for a frame that already existed
and a **second reader** of the path §10 already stores. The one thing it adds to the *system* is a
direction of travel.

**The traversal rule.** A terminal outcome travels **backwards, one hop at a time, along the traversed
`bus_path`, and stops at the ORIGIN bus.** In `A→B→C`: `C` hands the outcome to `B`, `B` hands its
copy to `A`, and `A` — which minted the correlation key — keeps it. Nothing fans out, and nothing
skips a hop to reach the origin directly: **a bus contacts only the bus that handed it the message**,
and only if that bus is in its own peer registry. The correlation is `ACK-CONTRACT.md` §3's key — the
ORIGIN bus's server-minted message id, `<origin-bus-id>-<seq>`, `store.Message.OriginID()` — and
nothing else. It is the same value that is already the relay wire idempotency key (§10) and already
`OutboxRecord.OriginMessageID`; **no fourth identifier is minted** (invariant 1).

**Two decisions, in this order, and the order is the design** (`relay.DisposeAck`):

1. **Our own bus id is validated first.** An empty or malformed local id compares unequal to the
   origin half of *every* correlation key, so a bus with a broken id would classify its OWN
   settlements as "forward upstream" and emit the origin's private rows onto the network.
2. **The correlation key is PARSED, never trusted** (`ids.ParseMessageID`, invariant 1). Its bus half
   *is* the origin bus, by construction (invariants 1 and 2), which is what makes the stop condition
   computable with no registry and no lookup.
3. **If we are the origin, the path is not consulted at all.** This is §8.4's rule at the correlation
   layer, and it is what makes a terminal outcome incapable of orbiting the federation: the one bus
   that could turn an inbound ACK back into an outbound one is the bus that minted the key, and it
   never does. Consulting the path first — "forward unless the upstream is missing" — would leave the
   stop condition dependent on a stored field, and a wrong or malicious path would restore the loop.
   Idempotency would absorb the duplicate *settlements*, but the **traffic** would be unbounded: the
   same complement-not-substitute distinction §10 draws for message loop prevention (invariant 10).

Bus ids are compared with `strings.EqualFold` throughout, because `ids.BusIDPattern` admits both
cases and §10's `PathContains` and `hub.relayedBusPath` already fold for the same reason. Folding
widens what counts as "us", which is the safe direction for a **stop** condition.

**The loop rule: the stored path must END at us, and the hop is an INDEX, never a search**
(`relay.UpstreamHop`). The stored path is origin-first and ends with this bus, because
`hub.relayedBusPath` validates the path as it arrived and appends this bus — it is **not** the path as
it came off the wire. So the upstream hop is always index `len-2`.

> **Read this before "making it robust".** The obvious generalisation — find our own id *anywhere* in
> the path and take the hop before it — would let **any** position in a peer-supplied path decide who
> we POST a terminal outcome to; the prefix of a stored path is bytes a peer sent (§10: "Not
> guaranteed: that any of it is true"). Requiring the path to END at us means the only hop we ever
> contact is the one adjacent to a position **we** wrote, and we contact it only if it is in **this
> bus's own peer registry with a base URL** (`relay.BackPropagator.Propagate` re-resolves the address
> on every emission and dials nothing when the lookup misses). A fabricated prefix therefore cannot
> name an address, a host or a scheme, and an unpeered id is the end of the road for that hop.
>
> **CORRECTED 2026-08-21 (`ACK-5`).** This passage previously said the rule stops a peer steering this
> bus's onward contact, full stop; the superseded claim is left visible here because the narrowing
> matters. **It does not prove the last hop of the ARRIVING path is the peer that authenticated.**
> `hub.relayedBusPath` checks that the path is non-empty, within `store.MaxReceivedBusPath`, that every
> hop passes `ids.ValidateBusID`, and that this bus is not already on it — then appends our own hop.
> `hub.RelayedIngestRequest` carries no peer-principal field at all, so **nothing binds
> `received[len-1]` to the authenticated peer**: an authenticated peer can still place a *different
> bus it knows we peer with* immediately before us and have the settlement delivered there. That gap
> is tracked as **`ACK-5-FU-BUSPATH-SENDER`** and is not closed by this rule. The residual is bounded
> at the FAR end instead: `ACK-CONTRACT.md` §6.2's obligation binding means the receiving bus
> independently refuses an ACK it was never owed, and its §12 idempotency absorbs a repeat — loop
> prevention over the path is a complement to idempotency, never a substitute (invariant 10).
>
> When the shape is not what we wrote, the honest answer is to refuse: refusing costs one settlement an
> operator can see in the log, guessing costs a contact the graph never authorised.

Four further refusals guard the same seam, each naming an **index and a length, never the offending
value** (the convention `hub.relayedBusPath` and `validateHops` already follow, because a hop that
failed validation is unbounded input): a path shorter than two hops; any hop that is not a valid bus
id, checked before any of them is compared; this bus appearing **twice** (a fabricated second visit —
the rule §10 enforces on ingest and egress, applied here to the record that survived them); and the
resolved hop being **us**, which is unreachable after the previous check and is kept only because it
states the post-condition the function exists to guarantee.

**Nothing in a frame ever names a destination, and that is the security property.** The upstream bus
**id** comes from this bus's own stored path; its **address** comes from this bus's own peer registry
(`relay.Registry.PeerBaseURL`), re-resolved on every emission so a de-peering takes effect on the next
one. `relay.PeerAckRequest` has no field an address, host, scheme or bus id could arrive in — the same
structural argument the relay envelope already makes. If a future task wants an ACK to reach somewhere
the registry does not name, the answer is a peering change by an operator, **never a field on the
frame**. "Not peered" is the end of the road for that hop and is never a reason to look for another
route to the origin.

**Class and attestation are forwarded VERBATIM** (`relay.AckFrameFrom`). The frame is rebuilt from the
**validated** value — so only fields that passed the closed-set validation can be forwarded, and this
bus cannot launder an unvalidated byte string onward under its own TLS identity — and the recipient's
outcome, class, `emitted_at` and 64-byte signature are reproduced exactly. **An intermediate re-signs
nothing, re-classifies nothing and re-times nothing** (invariant 2), exactly as it re-attests nothing
when forwarding a message. The attestation is opaque bytes over the ORIGINAL canonical ACK bytes, so
touching any covered field would silently invalidate a signature **nobody in this federation can
verify yet** (`ACK-CONTRACT.md` §6.3, §16 Q1) — the corruption would be undetectable end to end.
Restamping `emitted_at` would additionally make every hop look like the origin of the outcome in an
operator's log. The one field deliberately **not** preserved is `protocol_version`: `Client.PeerAck`
overwrites it with the version **this** bus speaks, which is what makes an absent version
unambiguously mean "written before the field existed".

**Durability comes from the chain, not from a local write.** No bus on the path writes anything durable
for an outcome it did not originate — `hub.recordAcceptance` returns early for relayed ingest, so
there is no row to settle and none is created. Invariant 4 is kept because **each hop is
synchronous**: no hop answers "accepted" until the next hop has, and the last one does not answer
until the origin has fsynced. **NARROWED IN PLACE 2026-08-21 (`ACK-5`) — that sentence stood here
unqualified and is true on every arm but one.** An INTERMEDIATE bus absorbs a **409** from the hop
above it and answers ITS downstream `200` (`cmd/agent-bus/relaywiring.go`, `disposeUnrecordedAck`),
so across **two or more backward hops** a recipient can be told `accepted` for an outcome the origin
**refused** — and in the "no obligation binds that recipient" case with nothing durable anywhere. No
bus does this to its own immediate caller: on the agent surface every transit failure, 409 included,
is the 503 below. The rationale — retry amplification, and not leaking the origin's verdict back down
the chain — is in `DECISIONS.md`, 2026-08-21 (`ACK-5`), and the status mapping is `ACK-CONTRACT.md`
§9.4.3. **There is therefore no retry queue on this path and there must not be
one**: a failure at any hop is answered "not now", the party that raised the outcome re-offers the
identical frame, and nothing is lost because nothing was acknowledged. Retry, backoff and bounce are
`ACK-7`/`ACK-14`, once, beside the durable outbox that already survives a restart. Every drop is
logged loudly and specifically (invariant 6) with the local bus, the peer bus, the elided correlation
key and recipient, and the outcome — **the bus path is deliberately never logged**, because §13.3
forbids disclosing the traversed path and an operator log is where such a detail leaks into an export
without anybody deciding to disclose it.

## 13. A LOCAL send's idempotency — the scope tuple, the fingerprint, and the window (IDEM-11 / IDEM-18, 2026-08-21)

Reference implementation: `internal/idem` (the scope type, the fingerprint, the retention derivation
and the table) and `internal/hub/hub.go` (`publishFingerprint`, and the applied-key path around
`idem.NewAgentScope`). §10 already states the relay half of invariant 10 — the origin `message_id`
as the key, and why `relayFingerprint` excludes `bus_path`. This section is the LOCAL half: what a
`send` or `broadcast` originating on this bus durably remembers, so that a retry of it is answered
from the record rather than applied twice.

**Scope of this section, per this document's own scope note above.** How a key REACHES the bus is
wire protocol and lives in `CONTRACTS-HTTP.md`, not here. What follows is only what is remembered,
what "the same payload" means in bytes, and how long the memory lasts.

**The applied-key lookup is the 3-tuple `(agent, operation, key)` — never the key alone.**
`idem.NewAgentScope(sender, op, key)` builds it (`internal/hub/hub.go`, in the send path), with `op`
one of `idem.OpSend`, `idem.OpBroadcast` or `idem.OpRelay` — chosen by what the request IS, not by
what it says it is. Both extra components are load-bearing:

- **The AGENT component** is what stops one agent colliding with, or probing, another's keys. Two
  agents that both mint `"k"` — sequential counters, a shared library default — would, under a
  key-only table, each corrupt the other's retry bookkeeping; and a key-only table also answers "does
  key X exist?" for a key the asker never generated, which is an oracle over another agent's traffic.
  Per-agent scoping closes both with one fix, and it is the same `<bus-id>.<agent-id>` namespacing
  invariant 2 requires everywhere else.
- **The OPERATION component is DOMAIN SEPARATION, not a label.** Without it one agent collides with
  ITSELF across routes: a `send` retried under `"k"`, then a `broadcast` that reuses `"k"`, would be
  read as a retry of the first. The op is part of the scope AND of the fingerprint, so a relayed
  message can never share a scope with — or be mistaken for a retry of — a local send under the same
  key.

`idem.Scope`'s fields are unexported and the only constructors are `NewAgentScope` and
`NewEnrolScope`, both of which demand a non-key component. **A key-only lookup is therefore not
expressible in Go**, not merely discouraged.

**"The same payload" is a content-addressed digest, not an approximation.** `publishFingerprint`
(`internal/hub/hub.go`) hashes, in this fixed order, via `idem.ComputeFingerprint` (SHA-256,
`crypto/sha256`):

```
[ op ("send" | "broadcast" | "relay"),
  8-byte big-endian recipient COUNT,
  each recipient, in order,
  body ]
```

Every field is length-prefixed by `ComputeFingerprint`, so `("ab","c")` and `("a","bc")` cannot digest
alike; the recipient count is hashed in addition, so a directed send to N recipients cannot collide
with one to N-1 split differently. This digest is the ENTIRE test that separates invariant 10's two
non-collapsible cases — same key + same fingerprint is a legitimate retry, answered from the stored
result with nothing re-applied and **nobody disconnected**; same key + a different fingerprint is a
protocol violation, rejected and logged, and **still nobody disconnected** (narrowed 2026-08-08). An
approximate comparison here would get both wrong at once: a real payload change would slip through as
a "retry", and an honest resend would be logged as an attack.

The digest is **stored on disk** (`fp`, hex, in the applied-key record) rather than recomputed at
replay, so changing that field list changes the MEANING of records already written — a retry of an
unchanged request would stop matching its own record and be reported as a violation. Any change to
the list needs a migration, not just a code change.

**The applied-key record commits in the SAME transaction as the message it records.** It rides in
`Entry.Idem`, the optional `idem` field of the PREPARE payload (§3), and a `wal.Entry` is exactly one
transaction — so the record is made durable by the same prepare → commit → fsync pair that makes the
message durable, never by a second write. There is consequently no window in which a message is
durable and its key is not, which is exactly the gap a crash-plus-retry would turn into a duplicate.
No record type and no `ondisk-format-version` value were reserved for it. **The record's
field-by-field JSON shape is in `CONTRACTS-ONDISK.md`'s "The durable applied-key store (IDEM-11)"
section and is deliberately not repeated here** — two copies of a field table drift, and that one is
the copy of record. Recovery rebuilds the table from the log (`hub.Apply`), which is what makes it
recovered state rather than a cache a crash empties.

**The retention window is the HONEST BOUNDARY of the guarantee, and it is stated here as a limit
rather than implied away.** An applied-key record is remembered for `idem.RetentionWindow` —
**50h10m22s** — and then evicted. **Duplicates are suppressed WITHIN that window. This is not
unconditional exactly-once, and nothing in this document should be read as promising it: a retry
that arrives after its key has been evicted is applied as a NEW operation** — a second message, with
its own id and sequence — and the bus cannot tell it apart from genuinely new content, because an
opaque client-supplied key carries no verifiable mint time to check it against.

Three things make that boundary honest rather than merely accepted:

- **The window is DERIVED, term by term, not picked** (`internal/idem/retention.go`): a 24 h peer
  outage budget, plus a full 1 h session lifetime (invariant 3's cap — a client returning from an
  outage must re-establish a session before it can retry), plus a 5 min parked long poll, plus an
  11 s client-side transport retry horizon = 25h5m11s, doubled for margin. **It is deliberately not a
  round number, because a round number is evidence that it was chosen rather than derived.**
- **Eviction is a pure predicate** — `now - committed_at > window`, and nothing else. Nothing is
  written to disk to record an eviction, so the live path and the replay path derive the same live
  set from the same bytes and cannot disagree about which keys are still remembered.
- **Capacity pressure never shortens the window.** The bus-wide cap (`idem.MaxEntries`, 65536) and
  the per-agent fair share fail CLOSED — they refuse a NEW operation with a 503 — and evict nothing.
  Time is the only thing that ends a key's life, so the window above is the whole statement.

`idem.Stats.Expired` (cumulative evictions) and `Stats.OldestAge` make the margin observable rather
than assumed.
