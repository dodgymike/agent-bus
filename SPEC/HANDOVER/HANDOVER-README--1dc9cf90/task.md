# HANDOVER-README: README stops telling a human things that are false

| Field | Value |
| --- | --- |
| Public id | `1dc9cf90-70e3-4381-be33-4157ad0f1efa` |
| Key | HANDOVER-README |
| Epic | [HANDOVER](../epic.md) |
| Status | todo |
| Priority | P1 |
| Component | docs |
| Section | backlog |
| Tags | — |
| Created | 2026-08-08T14:52:26.499750+00:00 |
| Updated | 2026-08-08T14:52:26.499750+00:00 |
| Completed | — |

## Proof command

```sh
bash scripts/proof-check.sh '! grep -n "curl -s localhost:8080/healthz" README.md && grep -n "KNOWN_ISSUES.md" README.md && grep -n "INVARIANTS.md" README.md'
```

## Description

Audience: both.

Priority P1 justification: the "What works today" block hands a human two plaintext curl -s localhost:8080/healthz commands against a TLS-only listener; they return a bare 400 Bad Request from net/http. The Requirements section states Go 1.19.4 as THE requirement while CLAUDE.md says the container pins the toolchain and the E2E plan needs 1.20+. And the Quickstart's <64-hex-from-invite> placeholder has no instruction anywhere for obtaining it, while `agent-busctl agents` is shown with no --as.

SCOPE BOUNDARY -- this is the residue that existing tasks do NOT cover. 5f8e0cba owns "bus http://" in README/AGENT_PROTOCOL, "listener is still plaintext" in PROTOCOL.md, and "until mutual TLS lands" in README. cb4fd330 owns the AGENT_PROTOCOL half. DISCOVERY-DOC-FU-README (be3c84f3) owns the stale three-field /v1/info body. NONE OF THEM TOUCH the curl block, the Go-version claim, or the unrunnable Quickstart. This task owns exactly those three and nothing else.

Definition of done: the plaintext curl demonstrations are replaced with something a human can actually run (or removed and pointed at the runbook); the Go-version paragraph states what is true at HEAD and defers the pin to DEPLOY-4; the Quickstart either works end-to-end or is replaced by a pointer to RUNBOOK.md; links to INVARIANTS.md and KNOWN_ISSUES.md added.

BLOCKED (hard, same file, both ahead of this in priority): 5f8e0cba and cb4fd330 must land first.
README.md IS ALSO CONTENDED BY be3c84f3 and f0ef1ed9 (stale CONTRACTS pointer at README.md:88). ALL FOUR must be serialised, with HANDOVER-README LAST.

Depends on: HANDOVER-MAP-DOC and HANDOVER-REGISTER (the links must resolve); 5f8e0cba and cb4fd330 (hard, same-file, must land first).

Parallel-safe: NO -- README.md is contended by 5f8e0cba, be3c84f3, f0ef1ed9. Serialise all four; this one last.

Model: sonnet (writing-heavy, scope is fixed by the task).

Size: two hours.

RED verification observed (2026-08-08): confirmed via a REAL check (not file-absence), not incidental: the proof_cmd shell fragment (negated grep for the plaintext-curl string, plus greps for the two new doc links) currently exits 1 -- README.md:96 contains the exact plaintext-curl string.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [DEPLOY-4](../../DEPLOY/DEPLOY-4--48b5d5b4/task.md) — DEPLOY-4: Go toolchain pin -- go.mod + builder image (no longer crypto-gated) (todo)
- [DISCOVERY-DOC-FU-README](../../DOCS/DISCOVERY-DOC-FU-README--be3c84f3/task.md) — DISCOVERY-DOC-FU-README: README.md still documents the old three-field /v1/info body (todo)
- [HANDOVER-MAP-DOC](../HANDOVER-MAP-DOC--a52d4a99/task.md) — HANDOVER-MAP-DOC: INVARIANTS.md -- each of the 11 invariants, its real status at HEAD, an… (todo)
- [HANDOVER-REGISTER](../HANDOVER-REGISTER--7fddae9d/task.md) — HANDOVER-REGISTER: KNOWN_ISSUES.md, the known-defect register (todo)
- [MTLS-VERIFY-FU](../../AUTH/MTLS-VERIFY-FU--5f8e0cba/task.md) — MTLS-VERIFY-FU-DOCSCHEME (README/PROTOCOL half): main still documents the bus as plaintex… (todo)
- [MTLS-VERIFY-FU-DOCSCHEME](../../DOCS/MTLS-VERIFY-FU-DOCSCHEME--cb4fd330/task.md) — MTLS-VERIFY-FU-DOCSCHEME: README + AGENT_PROTOCOL still tell agents to dial http:// a bus… (todo)
- [f0ef1ed9-cbcb-4ddd-9dec-394e1800ae78](../../DOCS/Stale-CONTRACTS.md-pointers-after-the-CONTRACTS-SPLIT-RE--f0ef1ed9/task.md) — Stale CONTRACTS.md pointers after the CONTRACTS-SPLIT: README.md:88, AGENT_PROTOCOL.md:12… (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [HANDOVER-DOCMAP](../HANDOVER-DOCMAP--e5802f9d/task.md) — HANDOVER-DOCMAP: say which of the tracked documents is authoritative, which is generated,… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
