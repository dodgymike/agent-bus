# HANDOVER-CHECK: one command that tells you the health of this repo, plus its recorded output at a named sha

| Field | Value |
| --- | --- |
| Public id | `0f909b6c-719d-4682-a293-4fa835e14628` |
| Key | HANDOVER-CHECK |
| Epic | [HANDOVER](../epic.md) |
| Status | todo |
| Priority | P1 |
| Component | tooling |
| Section | backlog |
| Tags | — |
| Created | 2026-08-08T14:49:23.095212+00:00 |
| Updated | 2026-08-08T14:54:55.884235+00:00 |
| Completed | — |

## Proof command

```sh
bash scripts/proof-check.sh 'bash scripts/check.sh'
```

## Description

Audience: maintainer (an operator inherits it later via the runbook).

Priority P1 justification: today there is no single command, no CI, and the only "green" signal we have (`go test -race ./...` = 16 packages ok) was captured while five other agents ran `go test` against the same checkout -- recon saw three packages FAIL in the same window and could not confirm it. Every downstream HANDOVER doc that says "the suite passes" would be repeating an unverified claim. This is a lie-prevention task, hence P1, and it is deliberately below the open P0s (INVITE-GATE, MTLS-VERIFY-FU-DOCSCHEME, DOCS-2, MTLS-MIGRATE 59883178, e120153b).

Definition of done:
- /mnt/sdb4/mike/mike/source/agent-bus/scripts/check.sh exists: go build ./..., go vet ./..., the CORRECT gofmt form (`test -z "$("$(go env GOROOT)/bin/gofmt" -l .)"` -- not bare gofmt, not exit-status-judged), go test -race ./..., and it exits non-zero on any failure. It prints a per-package table and a TOTAL SKIP COUNT (top-level *and* nested), because a suite that skips 42 results and reports ok is the failure mode this repo actually has.
- It runs against a throwaway data dir under /tmp, never the tracked data/.
- It is executed ONCE with the repo quiet -- no other agent running go test against this checkout -- at a named commit, and that transcript (sha, per-package result, skip count, wall time) is recorded in AGENT_LOG.md.

CAVEAT (load-bearing): 69eb6f56 (proof-check recursion) means check.sh must NOT itself invoke scripts/proof-check.sh.

Depends on: the separately-filed proof-check top-level-counting P1 (public_id cea09b96-72db-40f1-84b4-c2e227eae1cf) -- recorded as a real blocks relation (cea09b96 blocks this task) per the epic critical path, even though HANDOVER-CHECK proof_cmd itself does not literally invoke that fix; it is the epic-wide evidentiary prerequisite (see planner disagreement (d): it outranks everything else in this epic). If it lands first, check.sh should call the fixed counter rather than reimplement it.

Parallel-safe: NO. It requires an otherwise-idle checkout. Schedule it alone.

Model: sonnet for the script, but the isolation run and its interpretation want opus judgment if the anomaly reproduces. Suggest sonnet with an explicit instruction to escalate on any FAIL.

Size: half a day.

RED verification observed (2026-08-08): scripts/check.sh does not exist at HEAD -- trivially RED, file absent.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [DOCS-2](../../DOCS/DOCS-2--41c52cfa/task.md) — DOCS-2: PROTOCOL.md -- wire protocol + on-disk format (todo)
- [INVITE-GATE](../../INVITE/INVITE-GATE--05a5216d/task.md) — INVITE-GATE: POST /v1/enroll REQUIRES a valid invite and fails closed; invite consumption… (done)
- [MTLS-MIGRATE](../../MTLS/MTLS-MIGRATE--59883178/task.md) — MTLS-MIGRATE: pin add cannot migrate a pre-TLS (http-enrolled) identity onto TLS; its own… (done)
- [MTLS-VERIFY-FU-DOCSCHEME](../../DOCS/MTLS-VERIFY-FU-DOCSCHEME--cb4fd330/task.md) — MTLS-VERIFY-FU-DOCSCHEME: README + AGENT_PROTOCOL still tell agents to dial http:// a bus… (todo)
- [PROOF-CHECK-FU-RECURSION](../../TOOLING/PROOF-CHECK-FU-RECURSION--69eb6f56/task.md) — PROOF-CHECK-FU-RECURSION: bash scripts/proof-check.sh hangs / spawns runaway processes wh… (todo)
- [cea09b96-72db-40f1-84b4-c2e227eae1cf](../../TOOLING/proof-check.sh-subtest-SKIP-PASS-lines-invisible-to-plai--cea09b96/task.md) — proof-check.sh: subtest SKIP/PASS lines invisible to plain-text counter -- parent-PASS/al… (done)
- [e120153b-9d8a-4b6a-bd4e-89431954496b](../../DUR/Fix-WAL-recovery-reissuing-a-discarded-tail-record-index--e120153b/task.md) — Fix WAL recovery reissuing a discarded tail record index (invariant 1 violation, NOT a na… (in_progress)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CONTEXT-BUDGET-WIRE](../../CONTEXT/CONTEXT-BUDGET-WIRE--be76c7e2/task.md) — CONTEXT-BUDGET-WIRE: the byte ceilings from this whole epic become a standing, wired-in c… (todo)
- [CONTEXT-DOCCHECK](../../CONTEXT/CONTEXT-DOCCHECK--b3b28f45/task.md) — CONTEXT-DOCCHECK: doc-check.sh -- the instrument every other proof in this epic depends on (done)
- [HANDOVER-CONTRIBUTING](../HANDOVER-CONTRIBUTING--39484b80/task.md) — HANDOVER-CONTRIBUTING: CONTRIBUTING.md -- how this repo is actually developed, and how to… (todo)
- [HANDOVER-MAP-CHECK](../HANDOVER-MAP-CHECK--dce30493/task.md) — HANDOVER-MAP-CHECK: make the invariant map executable, not prose (todo)
- [HANDOVER-MAP-DOC](../HANDOVER-MAP-DOC--a52d4a99/task.md) — HANDOVER-MAP-DOC: INVARIANTS.md -- each of the 11 invariants, its real status at HEAD, an… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
