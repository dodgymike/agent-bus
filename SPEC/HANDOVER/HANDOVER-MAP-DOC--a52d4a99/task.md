# HANDOVER-MAP-DOC: INVARIANTS.md -- each of the 11 invariants, its real status at HEAD, and the evidence

| Field | Value |
| --- | --- |
| Public id | `a52d4a99-9679-4fec-84e2-f615c7762b14` |
| Key | HANDOVER-MAP-DOC |
| Epic | [HANDOVER](../epic.md) |
| Status | todo |
| Priority | P1 |
| Component | docs |
| Section | backlog |
| Tags | — |
| Created | 2026-08-08T14:49:23.675833+00:00 |
| Updated | 2026-08-08T15:26:06.731353+00:00 |
| Completed | — |

## Proof command

```sh
bash scripts/proof-check.sh 'test -s INVARIANTS.md && test 11 -le "$(grep -c "^### Invariant " INVARIANTS.md)" && grep -n "NOT ENFORCED" INVARIANTS.md && grep -n "tls.NoClientCert" INVARIANTS.md && grep -n "InviteRequired: false" INVARIANTS.md'
```

## Description

Audience: maintainer.

Priority P1 justification: CLAUDE.md's design contract reads as a description of the system. It is a description of the INTENT. Invariant 3 (invite-only) is NOT ENFORCED -- internal/httpapi/discovery.go:263 advertises InviteRequired: false. Invariant 11 (mutual TLS) is HALF enforced -- cmd/agent-bus/tlslisten.go:109 sets ClientAuth: tls.NoClientCert and a test pins it there. Invariant 10 is partial (enrol idempotency is an in-memory map; session begin/complete take no key at all). A maintainer who trusts CLAUDE.md will build on guarantees that do not exist. That is the epic's core problem in its purest form.

Definition of done: one row per invariant (and per named sub-clause where they diverge, e.g. 3a enrolment vs 3b session signing), each carrying: status in {ENFORCED, PARTIAL, NOT ENFORCED}, the NAMED TEST that proves the status, a file:line anchor, and for anything not ENFORCED the owning Spec task public_id. Header stamped with the commit sha it was measured at. Must record the two nuances recon surfaced, because losing them re-creates false alarms: the WAL index floor triggers on !sealedClean() (not on damage -- it is NOT blind), and cmd/agent-bus/seqfloorrestart_test.go:198-217 only t.Logf's a reissue and labels itself a KNOWN GAP.

SPLIT POINT (task is at the one-day size limit -- FLAG). If the implementer runs long, split sequentially (same file, so the two passes are NOT parallel):
  1. Invariants 1, 2, 4, 5, 6 (id authority + durability plane).
  2. Invariants 3, 7, 8, 9, 10, 11 (auth, client, crypto, idempotency, transport).

Depends on: HANDOVER-WIRED (HARD -- the NOT-ENFORCED rows cite its enumeration), HANDOVER-CHECK (SOFT -- supplies the sha and the honest suite status).

Model: OPUS. This is a correctness judgment across auth, durability and id authority; a wrong row here poisons everything downstream.

Size: at the one-day limit -- FLAG (see split point above).

UPDATED 2026-08-08 (spec-keeper, filing the CONTEXT epic): the file this task targets is NO LONGER
absent. A separate, ungated change (reviewed 2026-08-08, CHANGES-REQUESTED on that change but not on
this task) split CLAUDE.md's invariants section out into INVARIANTS.md (a single 220-line file, rule
+ reasoning together), then added the 11 `### Invariant N -- <title>` headings this task's proof
requires. INVARIANTS.md is ONE file, not a new one this task creates from scratch: it already carries
the CONTRACT + REASONING; this task's job is to add per-invariant STATUS/EVIDENCE blocks UNDER the
existing headings, not to create a new document.

RE-OBSERVED PROOF STATE (2026-08-08, replaces the "file absent" RED evidence above, which is now
STALE and must not be quoted as current): `test -s INVARIANTS.md` PASSES (18,577 B). `grep -c
"^### Invariant " INVARIANTS.md` now returns 11, so `test 11 -le "$(grep -c ...)"` PASSES -- the
heading half of the proof is satisfied and was previously broken (measured at 0 headings). The
evidence half is STILL RED, genuinely: `grep -n "NOT ENFORCED" INVARIANTS.md`, `grep -n
"tls.NoClientCert" INVARIANTS.md` and `grep -n "InviteRequired: false" INVARIANTS.md` all currently
return NO MATCHES -- the per-invariant status rows this task exists to write have not been added yet.
So the task's real remaining work is exactly its original definition-of-done (the STATUS/EVIDENCE
rows), not file creation.

"Parallel-safe: YES (new file, no contention)" is now FALSE and is REMOVED as a claim. INVARIANTS.md
is a live, shared file with reviewer findings already recorded against its current content (see the
kind=response note above, which itself recommends this same "add headings, not a new file" resolution
this update records as decided). Sequence this task's own edits against
CONTEXT-PLANE-TOC (which indexes INVARIANTS.md's headings once they carry real content) --
do not run those two concurrently against this file. Depends-on set is otherwise unchanged.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocked by** [HANDOVER-WIRED](../HANDOVER-WIRED--6d85978f/task.md)
- **blocks** [HANDOVER-MAP-CHECK](../HANDOVER-MAP-CHECK--dce30493/task.md)
- **blocks** [HANDOVER-REGISTER](../HANDOVER-REGISTER--7fddae9d/task.md)
- **relates to** [HANDOVER-CHECK](../HANDOVER-CHECK--0f909b6c/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CONTEXT-PLANE-TOC](../../CONTEXT/CONTEXT-PLANE-TOC--463afaf6/task.md) — CONTEXT-PLANE-TOC: a generated heading index at the top of every large reference doc (todo)
- [HANDOVER-CHECK](../HANDOVER-CHECK--0f909b6c/task.md) — HANDOVER-CHECK: one command that tells you the health of this repo, plus its recorded out… (todo)
- [HANDOVER-WIRED](../HANDOVER-WIRED--6d85978f/task.md) — HANDOVER-WIRED: assert and document which packages are present but not wired (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [9a02d65a-e96b-4fbe-93cf-846d8b5c2034](../../DOCS/Invariant-3-s-unauthenticated-route-enumeration-is-stale--9a02d65a/task.md) — Invariant 3's unauthenticated-route enumeration is stale in three docs -- six entries in… (todo)
- [HANDOVER-MAP-CHECK](../HANDOVER-MAP-CHECK--dce30493/task.md) — HANDOVER-MAP-CHECK: make the invariant map executable, not prose (todo)
- [HANDOVER-README](../HANDOVER-README--1dc9cf90/task.md) — HANDOVER-README: README stops telling a human things that are false (todo)
- [HANDOVER-REGISTER](../HANDOVER-REGISTER--7fddae9d/task.md) — HANDOVER-REGISTER: KNOWN_ISSUES.md, the known-defect register (todo)
- [HANDOVER-WIRED](../HANDOVER-WIRED--6d85978f/task.md) — HANDOVER-WIRED: assert and document which packages are present but not wired (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
