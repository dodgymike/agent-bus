# ACK-17: four keying mutations still leave internal/httpapi green -- session-keyed is the dangerous one

| Field | Value |
| --- | --- |
| Public id | `d4a2d828-b697-4616-a183-f165e7119057` |
| Key | ACK-17 |
| Epic | [ACK](../epic.md) |
| Status | done |
| Priority | P2 |
| Component | BE |
| Section | backlog |
| Tags | — |
| Created | 2026-08-21T10:44:33.426078+00:00 |
| Updated | 2026-08-23T11:22:48.585645+00:00 |
| Completed | 2026-08-23T11:22:48.585626+00:00 |

## Proof command

```sh
go test -count=3 -race -run "TestAckStatusParkedWaitCapBindsAcrossSessionsOfOneAgent|TestAckStatusParkedWaitCapKeyIncludesTheNameSuffix|TestAckStatusHumanRenderingPairsLabelsAndValues|TestAckStatusHumanRenderingSuppressesEmptyFields" ./internal/httpapi ./cmd/agent-busctl
```

## Status note

code complete at HEAD af67186, awaiting integrator commit. TEST-ONLY: 553 insertions, 0 deletions across AGENT_LOG.md, internal/httpapi/ackstatus_test.go, cmd/agent-busctl/ackstatus_test.go. No production file changed. Gates: reviewer PASS, security PASS, documentation COMPLETED. Complete this task with the commit_sha once the integrator lands it.

## Description

FILED 2026-08-21 by main, from the reviewer on ACK-16. These are mutations the reviewer named that
STILL leave the whole internal/httpapi package green after ACK-16's fix. Recording them because "the
mutations I chose" is a statement about the chooser, not about the code -- and this task exists so the
gap is written down rather than implied by a count.

1. KEYING ON THE SESSION INSTEAD OF THE AGENT -- highest value.
   acquire(r.Header.Get("Authorization")) passes every existing test. With
   auth.DefaultMaxActiveSessionsPerAgent = 32, that is up to 1024 parked waits for ONE agent,
   directly contradicting what CONTRACTS-HTTP.md:456 publishes. Catchable cheaply: give one agent a
   SECOND session and assert the cap still binds across both.

2. KEYING ON THE UNQUALIFIED NAME -- two same-named agents on DIFFERENT buses share a bucket. That is
   an invariant-2 shape (every agent id is fully qualified; never shorten it). Needs a two-bus
   fixture, so not cheap -- say so rather than half-doing it.

3. CLI label/value pairing is unpinned: shortTimestamp(x) -> x, or swapping the accepted:/settled:
   values between their labels, leaves the assertion counts unchanged.

4. CLI empty-guards (if r.Recipient != "" / if r.AttestedBy != "") are invisible because every
   fixture row is fully populated.

Do 1 first; 3 and 4 are cheap and can ride along. 2 is a judgement call on fixture cost.


PROOF_CMD CORRECTION (2026-08-21, spec-keeper): the previously recorded proof_cmd
`go test -race ./internal/httpapi ./cmd/agent-busctl` is NON-DETERMINISTIC on this box -- it
intermittently fails on TestCLIEnrolEndToEnd (cmd/agent-busctl/enrol_test.go:88, "the priming
server exited badly: signal: terminated"), reproduced by feature-runner at PRISTINE HEAD with none
of this task's files present (3 of 5 runs under -count=5). Pre-existing and unrelated to this task;
tracked separately as ACK-17-FU-ENROL-FLAKE. Replaced with a deterministic command scoped to this
task's own tests:
go test -count=3 -race -run "TestAckStatus|TestAckWaiter" ./internal/httpapi ./cmd/agent-busctl
Verified verdict (overlay's own scripts/proof-check.sh):
proof-check: verdict=PASS class=test exit=0 tests_run=28 top_level=16 skipped=0 failed=0 empty_pkgs=0
Do not restore the old command.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


_None recorded._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [ACK-16](../ACK-16--f60cdd30/task.md) — ACK-16: the per-principal wait cap is untested -- a global bucket passes the whole package (done)
- [ACK-17-FU-ENROL-FLAKE](../ACK-17-FU-ENROL-FLAKE--c20c15c8/task.md) — ACK-17-FU-ENROL-FLAKE: TestCLIEnrolEndToEnd is flaky at HEAD (SIGTERM race before handler… (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [ACK-17-FU-CONTRACT-CITATION](../ACK-17-FU-CONTRACT-CITATION--327f7cf7/task.md) — ACK-17-FU-CONTRACT-CITATION: pin CONTRACTS-HTTP.md per-principal parked-wait cap claim to… (todo)
- [ACK-17-FU-ENROL-FLAKE](../ACK-17-FU-ENROL-FLAKE--c20c15c8/task.md) — ACK-17-FU-ENROL-FLAKE: TestCLIEnrolEndToEnd is flaky at HEAD (SIGTERM race before handler… (todo)
- [ACK-17-FU-FOREIGNPREFIX](../ACK-17-FU-FOREIGNPREFIX--f9c6d8b0/task.md) — ACK-17-FU-FOREIGNPREFIX: tripwire -- auth.WALRoster.Apply does not filter foreign bus pre… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
