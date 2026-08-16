# ID-3: Agent id minting \`&lt;bus-id&gt;.&lt;name&gt;-&lt;n&gt;\`

| Field | Value |
| --- | --- |
| Public id | `745cce13-bae9-4ad4-99e5-1aca9635758f` |
| Key | ID-3 |
| Epic | [ID](../epic.md) |
| Status | done |
| Priority | P0 |
| Component | id |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T09:05:44.067910+00:00 |
| Updated | 2026-08-02T19:41:21.239676+00:00 |
| Completed | 2026-08-02T19:41:21.239659+00:00 |

## Proof command

```sh
go test -race ./internal/ids
```

## Status note

Both mandated gates have now run. Security: PASS (no critical/high; one MEDIUM on NameSuffixes durability, tracked on AUTH-3). Reviewer: CHANGES-REQUIRED on ONE blocking item -- F1: internal/ids/agentmint.go has 0.0% coverage on all eight functions (go tool cover -func); the nine TestAgentIDMinting* tests in agentid_test.go test the id FORMAT, not the MINTER. F1 remediation is IN FLIGHT: a test-engineer is writing internal/ids/agentmint_test.go now. Complete this task ONLY once those tests land with a non-vacuous proof-check.sh PASS covering agentmint.go.

## Description

STATUS CORRECTION 2026-08-02 (spec-keeper) -- NOT COMPLETABLE YET, AND THE REASON IS NOT THE CODE.

The CODE IS IN `main` and its proof PASSES. But the MANDATED reviewer and security gates NEVER RAN,
and there is no justification for the skip in AGENT_LOG.md. Completing it now would repeat exactly
the failure DUR-10 exists to record: production code reaching `main` with no gate.

VERIFIED FIRST-HAND THIS PASS (commands quoted, nothing taken on the task's word):
- `git log --oneline -- internal/ids/agentid.go internal/ids/agentmint.go` -> ONE commit, 10dd7f4
  "Agent id minting <bus-id>.<name>-<n> (ID-3)": internal/ids/agentid.go +239, agentid_test.go +391,
  agentmint.go +389, doc.go +14/-2. `git status --porcelain` is EMPTY -- nothing left uncommitted.
- `scripts/proof-check.sh 'go test -race -run TestAgentIDMinting ./internal/ids'` ->
  verdict=PASS class=test exit=0 tests_run=80 top_level=9 skipped=0 failed=0 empty_pkgs=0.
  NOT vacuous; 9 top-level TestAgentIDMinting* tests exist in internal/ids/agentid_test.go.
- Task journal: `main` posted kind=request; spec-keeper posted report+model; implementer posted
  report+model. THERE IS NO kind=response FROM reviewer AND NONE FROM security, and no
  reviewer/test-engineer/security note of any kind. `grep -n 'ID-3' AGENT_LOG.md` -> NO MATCHES, so
  the skip is not justified there either. The likely cause is the session-token kill recorded in
  this task's own first spec-keeper note; the dispatched chain did not survive to its gates.

REMAINING SCOPE OF THIS TASK -- pay the gate debt on ALREADY-COMMITTED code (10dd7f4). No rewrite.
1. REVIEWER GATE on internal/ids/agentid.go, agentmint.go and doc.go as committed at 10dd7f4.
   Focus: is the `<bus-id>.<name>-<n>` grammar unambiguous under every input the parser accepts
   (invariant 2 -- the '.' separator is what makes cross-bus routing parseable); is the per-name
   counter genuinely durable and monotonic across restart (invariant 1 -- ids are never reused,
   including across restarts); is the suffix spelling pinned to the sequence spelling so the two
   cannot drift.
2. SECURITY GATE. The short name is UNTRUSTED CLIENT INPUT that ends up inside a routing identifier.
   Focus: id spoofing / separator injection (can a crafted name make one agent's id parse as
   another's, or as a bus-qualified id it does not own), length bounds and the oversized-id
   non-echo path, and any Unicode/normalisation trick that makes two distinct names collide.
3. AGENT_LOG.md entry for ID-3 (there is none), recording the outcome and the fact that the gates
   ran after the commit rather than before it.
4. If either gate finds a defect, fix it in a SEPARATE follow-up commit -- do not amend 10dd7f4.

COMPLETION BAR: this task may be completed once both gates have posted kind=response (plus
kind=report + kind=model) and AGENT_LOG.md carries an ID-3 entry. commit_sha will be 10dd7f4 plus
any follow-up sha. The proof_cmd below is already validated PASS and does not need to change.

--- ORIGINAL DESCRIPTION (delivered by 10dd7f4) ---
Server mints the fully-qualified agent id at enrolment: client submits a desired short name, server appends a durable per-name counter suffix (-1, -2, ...) so a reused name never collides with a previous holder, and prefixes the bus id. Client never chooses its own id (invariant 1).

SCOPE NOTE carried forward: CODE-ONLY, like ID-2. No enrolment wiring -- AUTH-1 owns that and is
in flight separately. Nothing in production calls the minting code yet.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [AUTH-1](../../AUTH/AUTH-1--54fa94c0/task.md) — AUTH-1: POST /v1/enroll -- signed credential issuance (done)
- [AUTH-3](../../AUTH/AUTH-3--d53e3b21/task.md) — AUTH-3: Roster persistence & recovery (done)
- [DUR-10](../../DUR/DUR-10--bab09b2e/task.md) — DUR-10: Review the RepairTail truncation veto -- half is already in \`main\` UNREVIEWED (la… (done)
- [ID-2](../ID-2--a3a5edc4/task.md) — ID-2: Monotonic sequence allocator (drives message ids) (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [AUTH-1](../../AUTH/AUTH-1--54fa94c0/task.md) — AUTH-1: POST /v1/enroll -- signed credential issuance (done)
- [AUTH-3](../../AUTH/AUTH-3--d53e3b21/task.md) — AUTH-3: Roster persistence & recovery (done)
- [AUTH-4](../../AUTH/AUTH-4--a853261d/task.md) — AUTH-4: POST /v1/leave -- leave / revocation (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
