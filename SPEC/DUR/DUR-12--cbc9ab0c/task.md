# DUR-12: Replace CRC32C with an HMAC-SHA256 keyed MAC (ON-DISK FORMAT CHANGE, reserved ondisk-format-version=2) -- UNBLOCKED, key lives in data dir mode 0600

| Field | Value |
| --- | --- |
| Public id | `cbc9ab0c-3b34-48d0-acd8-5eabd4dc4a02` |
| Key | DUR-12 |
| Epic | [DUR](../epic.md) |
| Status | in_progress |
| Priority | P0 |
| Component | durability |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T17:57:06.513981+00:00 |
| Updated | 2026-09-05T11:59:07.486772+00:00 |
| Completed | 2026-08-07T12:08:09.114907+00:00 |

## Proof command

```sh
n/a - bookkeeping correction only, see original proof_cmd
```

## Status note

Executing plan step 6.

## Description

ON-DISK FORMAT CHANGE. THE RESERVED FORMAT VERSION IS **ondisk-format-version = 2**, allocated
2026-08-02 from the Spec Server `ondisk-format-version` namespace by spec-keeper (the same namespace
internal/wal/format.go:14-19 already cites for FormatVersion = 1). DO NOT PICK A DIFFERENT NUMBER AND
DO NOT LET ANOTHER FORMAT CHANGE REUSE IT. Note ID-2-WIRING-SCHEMA may ALSO need a format bump if it
chooses Option B -- it must reserve its OWN value. Format changes are ORDERED.

BLOCKED -- AND THE BLOCKER IS A QUESTION THE USER HAS NOT ANSWERED.

  WHERE DOES THE MAC KEY LIVE?

  A key stored beside the WAL in the data directory defends against the attack that motivated this
  change -- an ordinary REMOTE CLIENT crafting a payload -- but it does NOT defend against an
  attacker who already has DATA-DIRECTORY WRITE ACCESS, because that attacker can read the key and
  recompute any MAC at will. The candidate answers (key file in the data dir at 0600; key outside the
  data dir; key from an env var / operator-supplied at start; OS keyring; derived from a passphrase)
  trade off differently on unattended restart, containerised deployment, key rotation and backup, and
  the choice determines whether a lost key means a bus that cannot read its own log. THIS IS A
  PRODUCT DECISION, NOT AN IMPLEMENTATION DETAIL. Do not start coding until it is answered and
  recorded in DECISIONS.md. Also settle, in the same decision: what happens on a MISSING or WRONG key
  at startup -- under the always-restart policy that is arguably a NON-DAMAGE error (the media is
  fine, the operator misconfigured it) and should stay FATAL rather than discard the entire log.

WHY THIS CHANGE, in one line the implementer must not lose: CRC32C is an error-detecting code, not an
integrity primitive -- it is UNKEYED and GF(2)-LINEAR, and security DEMONSTRATED end-to-end (DUR-10
kind=response, 2026-08-02) that an ordinary remote client, submitting nothing but printable-ASCII
JSON in its own message body, could solve for bytes that make a TORN prefix of its own record satisfy
recovery's completeness "proof". A keyed MAC eliminates that BY CONSTRUCTION: a client cannot compute
a MAC over a key it does not hold. User decision, 2026-08-02, verbatim: "don't use crc! use a
hash/hmac/more modern approach. We're not optimising for efficiency, we're optimising for integrity
and security".

CONSTRUCTION -- INVARIANT 9 IS ABSOLUTE HERE. Use the Go stdlib's high-level API: `crypto/hmac` +
`crypto/sha256`, via hmac.New / hmac.Equal. NEVER hand-roll, "adapt" or assemble a MAC out of
primitives; never compare MACs with bytes.Equal or ==. This outranks invariant 8's stdlib-first bias
and any argument from performance -- broken crypto fails SILENTLY, so "our tests pass" is not
evidence. No third-party dependency is needed or wanted.

SCOPE.
1. Replace the CRC32C field in the frame with an HMAC-SHA256 tag over the header-plus-payload bytes
   the CRC covered today (define the covered range EXACTLY, in PROTOCOL.md, and make it unambiguous:
   the length field MUST be inside the covered range or the length-inflation class of attack survives
   the change).
2. Bump FormatVersion 1 -> 2 in internal/wal/format.go, using the RESERVED value above.
3. A RECOVERY PATH FOR LOGS ALREADY WRITTEN IN THE CRC32C FORMAT IS MANDATORY. Format changes are
   ordered: a version-1 file must be recognised by its header and either read with the old verifier
   or converted, with the behaviour stated explicitly and tested. Today `verifyFileHeader`
   (internal/wal/format.go:328) rejects any version != FormatVersion outright, so a naive bump turns
   every existing bus into one that will not read its own log. Decide and document the upgrade story
   (read-v1-verify-with-CRC then write v2 going forward, or an explicit one-shot conversion) and
   whether a v2 reader may ever DOWNGRADE-write v1 (it should not).
4. DUR-4's TORN-TAIL HEURISTIC SHOULD GET **SIMPLER**, NOT MORE COMPLEX. Much of inspectTail /
   lengthOnlyDamage / truncatableTail exists to compensate for a weak, forgeable checksum. Under a
   strong MAC, "this frame verifies" becomes trustworthy, so the plausible-boundary search and the
   completeness "proof" should shrink or disappear. Actively look for code to DELETE here; a change
   that only adds is a sign the opportunity was missed. Coordinate with DUR-11, which is rewriting
   the same functions for the always-restart policy -- DUR-11 lands FIRST and this task must not
   collide with it in internal/wal/recover.go.
5. Key handling per the decision above, plus rotation: at minimum say what happens when the key
   changes, even if the answer is "not supported yet, and here is the error you get".
6. CONTRACTS.md + PROTOCOL.md updated with the new frame layout, the covered range, the version-2
   header and the v1 compatibility statement.

TESTS REQUIRED. A negative test that a frame whose payload was altered fails verification; a test
that a v1 (CRC32C) log is still readable per the chosen compatibility story; a test that a WRONG key
does not silently pass; and the crash-injection sweep kept green against the new format. Constant-time
comparison must be asserted by CODE REVIEW (hmac.Equal), not by a timing test.

PROOF. `go test -race -run 'TestWALFrameMACRejectsAlteredPayload|TestWALReadsFormatVersion1Log' ./internal/wal && go test -race ./internal/wal`
VACUOUS TODAY BY CONSTRUCTION -- neither test exists; they are this task's to write, and they are
named for the two things that must not be got wrong (forgery rejection, and not bricking existing
logs). MUST NOT BE COMPLETED ON A VACUOUS VERDICT: scripts/proof-check.sh must report PASS with
tests_run > 0, and its verdict must be quoted in test_summary.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


_None recorded._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [DUR-10](../DUR-10--bab09b2e/task.md) — DUR-10: Review the RepairTail truncation veto -- half is already in \`main\` UNREVIEWED (la… (done)
- [DUR-11](../DUR-11--884d3da4/task.md) — DUR-11: Close security's two OPEN HIGH truncation holes -- anchor the tail veto on record… (done)
- [DUR-4](../DUR-4--59c36769/task.md) — DUR-4: Corrupt-tail detection & truncation (superseded)
- [ID-2-WIRING-SCHEMA](../../ID/ID-2-WIRING-SCHEMA--80b54ee4/task.md) — ID-2-WIRING-SCHEMA: DECIDE and record where the message sequence high-water mark lives on… (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [83850937-a3c9-4b90-8ac6-19655233cb13](../../DOCS/DECISIONS.md-carries-the-pre-correction-wrong-accepted-l--83850937/task.md) — DECISIONS.md carries the pre-correction (wrong) accepted-limit sentence for the MAC key;… (todo)
- [COMMIT-HYGIENE-MIXED-22E8EB6](../../PROCESS/COMMIT-HYGIENE-MIXED-22E8EB6--dc4f8869/task.md) — COMMIT-HYGIENE-PRACTICE-NOTE: standing practice -- git commit should carry an explicit pa… (cancelled)
- [DEPLOY-1](../../DEPLOY/DEPLOY-1--fa0c5a4e/task.md) — DEPLOY-1: Dockerfile -- multi-stage build, pinned Go builder, minimal runtime image (done)
- [DOCS-2](../../DOCS/DOCS-2--41c52cfa/task.md) — DOCS-2: PROTOCOL.md -- wire protocol + on-disk format (todo)
- [DUR-11-FU-CONTRACTS](../../DOCS/DUR-11-FU-CONTRACTS--5b178dde/task.md) — DUR-11-FU-CONTRACTS: CONTRACTS.md still documents the reverted refuse-to-start WAL policy… (todo)
- [DUR-12-FU-CONTRACTS](../DUR-12-FU-CONTRACTS--7f0c8dd3/task.md) — DUR-12-FU-CONTRACTS: land the six CONTRACTS-ONDISK.md rows deferred from DUR-12 (todo)
- [DUR-12-FU-V1LAUNDER](../DUR-12-FU-V1LAUNDER--daf18983/task.md) — DUR-12-FU-V1LAUNDER: v1-format WAL laundering re-signs forged CRC32C records with the rea… (todo)
- [DUR-12-FU-VERSIONFLIP](../DUR-12-FU-VERSIONFLIP--5f78f749/task.md) — DUR-12-FU-VERSIONFLIP: single-bit version-field flip on a v2 log misidentifies it as v1 a… (todo)
- [DUR-12-VERIFY](../DUR-12-VERIFY--f602c92e/task.md) — DUR-12-VERIFY: verify the WAL MAC upgrade against a real running bus (paired not-yet-live… (todo)
- [DUR-4](../DUR-4--59c36769/task.md) — DUR-4: Corrupt-tail detection & truncation (superseded)
- [DUR-4-FU-DECISIONS](../DUR-4-FU-DECISIONS--180f11f8/task.md) — DUR-4-FU-DECISIONS: record the SHIPPED damage-class taxonomy in DECISIONS.md -- which cla… (todo)
- [INVITE-STORE](../../INVITE/INVITE-STORE--a9ef92de/task.md) — INVITE-STORE: durable single-use invite record (mint/lookup/consume/expire), recovered by… (done)
- [a9a433dd-4ae0-4a0d-a6f3-6c804105fbcd](../../TOOLING/Conjunction-masking-vacuous-proof-family-filtered-clause--a9a433dd/task.md) — Conjunction-masking vacuous-proof family: filtered-clause proof_cmds hidden by an unfilte… (todo)
- [c3a27591-5b0c-44c0-ac68-94072f3c3fc2](../RESOLVED-2026-08-02-SUPERSEDED-CRC32C-tail-repair-proofs--c3a27591/task.md) — \[RESOLVED 2026-08-02 -- SUPERSEDED\] CRC32C tail-repair proofs are remotely forgeable =&gt; p… (superseded)
- [db350e39-3dde-4166-b241-b21fa4635359](../Whole-log-quarantine-reissued-EVERY-sequence-number-ever--db350e39/task.md) — Whole-log quarantine reissued EVERY sequence number ever minted -- fixed by a durable ind… (done)
- [e120153b-9d8a-4b6a-bd4e-89431954496b](../Fix-WAL-recovery-reissuing-a-discarded-tail-record-index--e120153b/task.md) — Fix WAL recovery reissuing a discarded tail record index (invariant 1 violation, NOT a na… (in_progress)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
