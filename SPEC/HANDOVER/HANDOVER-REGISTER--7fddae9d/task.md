# HANDOVER-REGISTER: KNOWN_ISSUES.md, the known-defect register

| Field | Value |
| --- | --- |
| Public id | `7fddae9d-9ef8-4d9d-82df-f18e0119653d` |
| Key | HANDOVER-REGISTER |
| Epic | [HANDOVER](../epic.md) |
| Status | todo |
| Priority | P1 |
| Component | docs |
| Section | backlog |
| Tags | — |
| Created | 2026-08-08T14:49:24.193633+00:00 |
| Updated | 2026-08-08T14:49:24.193633+00:00 |
| Completed | — |

## Proof command

```sh
bash scripts/proof-check.sh 'test -s KNOWN_ISSUES.md && test "$(grep -c "^### " KNOWN_ISSUES.md)" -le 20 && grep -n "record-boundary-exact truncation" KNOWN_ISSUES.md && grep -n "9fd58deb" KNOWN_ISSUES.md && grep -n "INVITE-GATE" KNOWN_ISSUES.md'
```

## Description

Audience: both -- maintainer for the causes, operator for the blast radius.

Priority P1 justification: there is no known-defect register at all, and the defects are only visible as Spec Server tasks behind cloud credentials that live outside the repo. A human who clones this cannot discover that a boundary-exact WAL truncation reissues sequence numbers, or that the roster-brick DoS is unmitigated because INVITE-GATE never landed. Handing over undisclosed known data-loss and availability defects is the most serious form of the repo lying.

Definition of done: CURATED, HARD-CAPPED AT 20 ENTRIES, symptom-first. Each entry: what a user or operator would OBSERVE, blast radius, class (data-loss / security / availability / functionality), current mitigation or workaround, owning Spec task public_id. Must include at minimum:
- Seq-floor migration guard blind at record-boundary-exact truncation (22/22 boundaries measured, 13 reissued end-to-end); real fix blocked on 9fd58deb. Cross-reference 2a38cdec, which owns the doc correction -- do not duplicate its text.
- Roster-brick DoS, gated on INVITE-GATE.
- Enrolment is not invite-gated (InviteRequired: false); /v1/enroll is on the unauthenticated allow-list.
- Server presents no client-cert requirement (NoClientCert); CertBindings declared but never written.
- Enrol idempotency is in-memory only -- a retry straddling a restart mints a second agent id. /v1/session/begin and /v1/session/complete take no idempotency key.
- Recipient signature verification is absent (three independent causes).
- POST /v1/broadcast and agent-busctl broadcast return 501.
- The relay/federation plane is unwired.
- Upgrade discards message history (record v1 -> v2, no migration, breaks both ways).
- Idempotency-Key header vs JSON body-field divergence (internal/idem/key.go:8-12 vs every live route; idem.FromRequest is dead code).
- The backlog's own reliability -- see HANDOVER-BACKLOG-RECONCILE (filed, off critical path).

Depends on: HANDOVER-WIRED (HARD), HANDOVER-MAP-DOC (SOFT -- the map's NOT-ENFORCED rows are the register's security entries).

Parallel-safe: YES (new file).

Model: OPUS -- ranking blast radius and deciding what a symptom looks like from outside is judgment.

Size: three-quarters of a day.

RED verification observed (2026-08-08): KNOWN_ISSUES.md does not exist -- trivially RED, file absent. The -le 20 clause makes the curation constraint mechanical rather than aspirational; confirmed the pinned strings ("record-boundary-exact truncation", "9fd58deb") do not occur in any tracked non-SPEC.md file, so scoping to KNOWN_ISSUES.md needs no tightening.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [2a38cdec-528f-47ef-8f38-7f83465b0213](../../DUR/CONTRACTS-ONDISK.md-and-four-sibling-Go-comments-oversta--2a38cdec/task.md) — CONTRACTS-ONDISK.md and four sibling Go comments overstate the seq-floor migration guard:… (todo)
- [9fd58deb-6fb8-4d4e-8bf1-6df01329c3b2](../../DUR/Expose-on-wal.Recovered-the-highest-index-a-record-actua--9fd58deb/task.md) — Expose on wal.Recovered the highest index a record actually CONSUMED (todo)
- [HANDOVER-BACKLOG-RECONCILE](../HANDOVER-BACKLOG-RECONCILE--43d14776/task.md) — HANDOVER-BACKLOG-RECONCILE: the inherited backlog stops lying about what is in flight (todo)
- [HANDOVER-MAP-DOC](../HANDOVER-MAP-DOC--a52d4a99/task.md) — HANDOVER-MAP-DOC: INVARIANTS.md -- each of the 11 invariants, its real status at HEAD, an… (todo)
- [HANDOVER-WIRED](../HANDOVER-WIRED--6d85978f/task.md) — HANDOVER-WIRED: assert and document which packages are present but not wired (todo)
- [INVITE-GATE](../../INVITE/INVITE-GATE--05a5216d/task.md) — INVITE-GATE: POST /v1/enroll REQUIRES a valid invite and fails closed; invite consumption… (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [HANDOVER-README](../HANDOVER-README--1dc9cf90/task.md) — HANDOVER-README: README stops telling a human things that are false (todo)
- [HANDOVER-RUNBOOK-DOC](../HANDOVER-RUNBOOK-DOC--a0e009e1/task.md) — HANDOVER-RUNBOOK-DOC: RUNBOOK.md narrates exactly what the smoke script does (todo)
- [HANDOVER-WIRED](../HANDOVER-WIRED--6d85978f/task.md) — HANDOVER-WIRED: assert and document which packages are present but not wired (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
