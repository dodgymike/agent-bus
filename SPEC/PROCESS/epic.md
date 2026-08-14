# EPIC PROCESS — How agents coordinate + backlog integrity (does not ship in the binary)

[← all epics](../../SPEC.md)

**8 open / 11 total.** Full records live in `SPEC/PROCESS/<task>/task.md`.

_Relations (real)_ are authoritative edges from the Spec Server. _Referenced (derived)_ are guesses parsed out of description free text — useful, but not a dependency list.

## Open tasks (8)

| Key | Title | Status | Prio | Task | Relations (real) | Referenced (derived) |
| --- | --- | --- | --- | --- | --- | --- |
| 2582f548-6493-439c-ba71-7f5cf73650fc | Spec Server /export (both format=markdown and format=json) silently drops the commits\[\] a… | todo | P2 | [task.md](Spec-Server-export-both-format-markdown-and-format-json--2582f548/task.md) | — | [MTLS-PIN](../MTLS/MTLS-PIN--8c46dc93/task.md) [e109c867-fcd2-4ddc-bc4d-55779dc5f5e1](Spec-Server-PATCH-tasks-id-rejects-the-key-field-outrigh--e109c867/task.md) |
| 6fd8c8c5-b653-4d35-af83-8c9d1b82dedd | Correct stale wave label AUTH-7 to its real task identity across code and docs | todo | P2 | [task.md](Correct-stale-wave-label-AUTH-7-to-its-real-task-identit--6fd8c8c5/task.md) | — | [MSG-FU-ROSTERSOURCE](../AUTH/MSG-FU-ROSTERSOURCE--fa26036c/task.md) |
| 932fe938-0e42-42d8-802d-ff018cb6c955 | Audit stored proof_cmds for the subtest-skip vacuous shape (parent-PASS/hidden-child-SKIP… | todo | P2 | [task.md](Audit-stored-proof_cmds-for-the-subtest-skip-vacuous-sha--932fe938/task.md) | — | [PROOF-CHECK-FU-RECURSION](../TOOLING/PROOF-CHECK-FU-RECURSION--69eb6f56/task.md) [ID-2-WIRING-SEAL](../ID/ID-2-WIRING-SEAL--8c9b6489/task.md) [CLI-2](../CLI/CLI-2--39318208/task.md) [cea09b96-72db-40f1-84b4-c2e227eae1cf](../TOOLING/proof-check.sh-subtest-SKIP-PASS-lines-invisible-to-plai--cea09b96/task.md) [fc8cd234-d275-43a1-9cb0-d10bca4a4086](Backfill-non-vacuous-proof_cmd-across-the-14-actionable--fc8cd234/task.md) [a9a433dd-4ae0-4a0d-a6f3-6c804105fbcd](../TOOLING/Conjunction-masking-vacuous-proof-family-filtered-clause--a9a433dd/task.md) |
| CLAUDE-PATHSPEC-MM-NOT-GATE | CLAUDE.md: MM is not the gate -- a clean M can still hide another agent's worktree hunks | todo | P2 | [task.md](CLAUDE-PATHSPEC-MM-NOT-GATE--077bcba5/task.md) | — | [RELAY-13-FU-DOCS](../RELAY/RELAY-13-FU-DOCS--7f3a4b80/task.md) [INVITE-CLIENT](../INVITE/INVITE-CLIENT--4123e25d/task.md) |
| SWEEP-TWO-PASS-DISCIPLINE | SWEEP-TWO-PASS-DISCIPLINE: a contract change needs a MECHANISM sweep and a PROSE sweep, n… | todo | P2 | [task.md](SWEEP-TWO-PASS-DISCIPLINE--268a0c73/task.md) | — | [SIGN-1-FU-REORDER-WATERMARK](../SIGN/SIGN-1-FU-REORDER-WATERMARK--c829af9a/task.md) [SIGN-1-FU-REORDER-WATERMARK](../SIGN/SIGN-1-FU-REORDER-WATERMARK--86c7d368/task.md) [SIGN-4](../SIGN/SIGN-4--33fa35d8/task.md) [IDEM-14](../IDEM/IDEM-14--b0facce9/task.md) [RATCHET-2](../RATCHET/RATCHET-2--ade31a62/task.md) [CRYPTO-10](../CRYPTO/CRYPTO-10--68ff679d/task.md) +2 more |
| cbfb7d88-1bb0-4ade-b1d1-f287b4c0c179 | Triage dispatched two concurrent agents with overlapping ownership of CONTRACTS-CLI.md | todo | P2 | [task.md](Triage-dispatched-two-concurrent-agents-with-overlapping--cbfb7d88/task.md) | — | [INVITE-MINT](../INVITE/INVITE-MINT--1d0d0e60/task.md) [MTLS-ROTATE](../MTLS/MTLS-ROTATE--c2e8df5b/task.md) |
| e109c867-fcd2-4ddc-bc4d-55779dc5f5e1 | Spec Server: PATCH /tasks/{id} rejects the key field outright (422 Unknown field) -- a ke… | todo | P2 | [task.md](Spec-Server-PATCH-tasks-id-rejects-the-key-field-outrigh--e109c867/task.md) | — | [CLI-1-FU-BINARYNAME](../CLI/CLI-1-FU-BINARYNAME--6a1eb5fa/task.md) [INVITE-GATE](../INVITE/INVITE-GATE--05a5216d/task.md) [MTLS-BUSCERT](../MTLS/MTLS-BUSCERT--93f0dc19/task.md) [e36661b0-687e-465e-b72f-e33245088e38](../UNASSIGNED/keypatch-probe-spec-keeper-bug-repro-safe-to-cancel--e36661b0/task.md) |
| fc8cd234-d275-43a1-9cb0-d10bca4a4086 | Backfill non-vacuous proof_cmd across the 14 actionable tasks that have none (CLI-1..9 +… | todo | P2 | [task.md](Backfill-non-vacuous-proof_cmd-across-the-14-actionable--fc8cd234/task.md) | — | [ZZ-LOCKTEST](../UNASSIGNED/ZZ-LOCKTEST--e091e451/task.md) [CLI-1](../CLI/CLI-1--0495d133/task.md) [CLI-2](../CLI/CLI-2--39318208/task.md) [CLI-3](../CLI/CLI-3--6e70abe5/task.md) [CLI-4](../CLI/CLI-4--137465b9/task.md) [CLI-5](../CLI/CLI-5--86dea094/task.md) +15 more |

## Closed tasks (3) — done, cancelled, superseded

| Key | Title | Status | Prio | Task | Relations (real) | Referenced (derived) |
| --- | --- | --- | --- | --- | --- | --- |
| TRIAGE-LOCK | TRIAGE-LOCK: backlog-triage mutex | done | P0 | [task.md](TRIAGE-LOCK--25f0eac6/task.md) | — | [INVITE-MINT](../INVITE/INVITE-MINT--1d0d0e60/task.md) [MTLS-ROTATE](../MTLS/MTLS-ROTATE--c2e8df5b/task.md) [MTLS-EXPIRY](../MTLS/MTLS-EXPIRY--3604af80/task.md) [MTLS-VERIFY](../MTLS/MTLS-VERIFY--9dab7303/task.md) [MTLS-LISTENER](../MTLS/MTLS-LISTENER--17e70a7e/task.md) [bd662bae-4c6c-426d-a736-7830d2d21037](../MTLS/parseBusURL-does-not-canonicalise-redundant-path-slashes--bd662bae/task.md) +6 more |
| BACKLOG-FILE-96 | BACKLOG-FILE-96: file every epic-less task into an epic, or say honestly why not | done | P2 | [task.md](BACKLOG-FILE-96--b971e4e6/task.md) | — | [ZZ-LOCKTEST](../UNASSIGNED/ZZ-LOCKTEST--e091e451/task.md) |
| COMMIT-HYGIENE-MIXED-22E8EB6 | COMMIT-HYGIENE-PRACTICE-NOTE: standing practice -- git commit should carry an explicit pa… | cancelled | P2 | [task.md](COMMIT-HYGIENE-MIXED-22E8EB6--dc4f8869/task.md) | — | [DUR-12](../DUR/DUR-12--cbc9ab0c/task.md) |

## Epic description

Epic key reserved 2026-08-08 (reservation namespace epic-key, value 1). Work about HOW THIS PROJECT IS BUILT by parallel agents, not about the product: the backlog-triage mutex, commit-hygiene practice (explicit pathspec), file-ownership overlap between concurrently dispatched agents, stale task/wave labels cited from code and docs, proof_cmd coverage across the backlog, and defects in the Spec Server itself that damage the record we keep (these last are UPSTREAM -- no agent in this repo can fix them, they are tracked here because they corrupt our own backlog and SPEC.md mirror). Distinct from TOOLING, which is the repo's own scripts and is directly dispatchable to one agent. Nothing here ships in cmd/agent-bus. It exists as an epic because this work has PREVENTED real defects (five index-sweeping commits, one triage double-dispatch, a whole family of vacuous proofs) and was previously invisible to anyone scanning epics.

---

_Generated by `scripts/gen-spec-mirror.sh`; never hand-edit._
