# ID-2-WIRING-SEAL-FU-NAMESUFFIXES: NameSuffixes has the identical inert-floor-guard defect, unfixed

| Field | Value |
| --- | --- |
| Public id | `1c207a62-e904-4988-84c2-f4b69712ee35` |
| Key | ID-2-WIRING-SEAL-FU-NAMESUFFIXES |
| Epic | [ID](../epic.md) |
| Status | done |
| Priority | P0 |
| Component | ids |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T20:56:07.669501+00:00 |
| Updated | 2026-08-07T12:07:42.774583+00:00 |
| Completed | 2026-08-07T12:07:42.774565+00:00 |

## Proof command

```sh
git show b7701cb:internal/ids/doc.go | grep -q 'different stages of that wiring'
```

## Status note

GATES COMPLETE (reviewer PASS-WITH-NITS on re-review, security PASS-WITH-NOTES, no P0/P1 open). Code LANDED in commit 518e71b (agentmint.go, agentmint_test.go, sequence.go, sequence_test.go). ONE FILE STILL OWED: internal/ids/doc.go is STAGED BUT UNCOMMITTED -- it carries the correction of two FALSE doc claims that 518e71b introduced (that the fresh counter is 'sound only while no enrolment reaches disk' -- false, agent ids are durable TODAY inside WAL message bodies; and the over-correction that 'both allocators still owe their resume wiring' -- false, Sequence's is committed at 0bbbd27). Do NOT complete until doc.go is committed. Proof verdict=PASS tests_run=18 top_level=1 failed=0 non-vacuous for TestNameSuffixesRefusesToIssueFromAnUnsealedFloor; package PASS tests_run=225. Triage VERIFIED 518e71b independently: go build ./... exit 0, go vet ./... exit 0, go test -race -count=1 ./... all packages ok. DESIGN: seal is GLOBAL not per-name (a per-name seal cannot express 'derivation complete' and would let an unseen name mint from an unproven floor of 0 -- the exact collapse the task exists to prevent). DELIBERATE DEVIATION: NewNameSuffixes() born SEALED, ResumeNameSuffixes() born unsealed, because main.go:327 is on the live enrolment path and was outside the boundary; residual hole tracked on MSG-FU-SUFFIXFLOOR 94159d93.

## Description

`ID-2-WIRING-SEAL` fixed `internal/ids.Sequence` but deliberately left `internal/ids.NameSuffixes` (agentmint.go) alone as out of scope. `NameSuffixes.RaiseFloor` carries the SAME inert guard -- `if last != 0 && atLeast <= last` at agentmint.go:~298 -- which fires only once a suffix has been issued, so during the window in which the per-name floors are actually derived, every value including a far-too-low one is accepted silently. Its own doc comment already admits this in the same words `Sequence`'s used to ("during the window where the floor is actually derived ... RaiseFloor is therefore a check on a caller that keeps computing floors after it has started serving"), and `go vet` cannot flag a dropped `RaiseFloor` error (proven in ID2_WIRING_DEEPDIVE.md sec 3.4).

This is arguably WORSE than the message-sequence case, and agentmint.go's own doc says why: "re-minting an agent id is worse than re-minting a message id because the agent id is the routing and authorization subject." A reissued agent id means two agents sharing one routing/authorization identity.

The fix is the same shape as ID-2-WIRING-SEAL -- a `Seal()` gate, born unsealed on BOTH `NewNameSuffixes` and `ResumeNameSuffixes`, `NextSuffix` refusing with `ErrFloorUnproven` until sealed, `RaiseFloor` refusing with `ErrFloorSealed` after -- but the per-name shape needs a design call this task must make explicitly: is the seal GLOBAL (one seal for the whole map, which is what a single startup derivation pass implies) or PER-NAME (a name's floor is sealed when that name's derivation completes)? Global is almost certainly right, because names are discovered by the same single replay pass and a per-name seal would let an unknown-at-startup name mint from an unproven floor of 0 -- but say so deliberately rather than by default. Reuse the existing `ErrFloorUnproven` / `ErrFloorSealed` sentinels; do not add parallel ones.

Also update `internal/ids/doc.go`'s `agentmint.go` bullet, which ID-2-WIRING-SEAL deliberately left describing the unfixed state, and add the `NameSuffixes` rows wherever ID-2-WIRING-SEAL-FU-CONTRACTS lands the `Sequence` ones.

Note the interaction with AUTH-3 (restoring the per-name suffix floors from replay): that task is the CALLER that must derive the floors and call `Seal()`, so these two want to be sequenced together.

proof_cmd is VACUOUS TODAY BY CONSTRUCTION -- the named test `TestNameSuffixesRefusesToIssueFromAnUnsealedFloor` does not exist; writing it is the point. This task must NOT be completed on a VACUOUS `scripts/proof-check.sh` verdict; it must report PASS with tests_run > 0.
---

## AMENDMENT 2026-08-03 (spec-keeper, following reviewer finding P1-b: CHANGES-REQUESTED)

The reviewer flagged that the shipped code and the original description above now disagree IN
WRITING: the original text (point 5, "born unsealed on BOTH `NewNameSuffixes` and
`ResumeNameSuffixes`") is SUPERSEDED by what follows. The original text stays above, unedited, as a
record of what was originally asked; this section is the authoritative correction.

**1. What actually shipped, and why it is the correct call, not a bug.** `ResumeNameSuffixes` is
born UNSEALED and must have `Seal()` called on it before anything is issued -- that half of the
original ask is unchanged. `NewNameSuffixes` is born SEALED. It is the FRESH-BUS constructor: an
empty-disk bus has no suffixes to derive a floor from, so calling `NewNameSuffixes()` at all IS the
empty-disk claim, carried by the constructor's name rather than by a separate `Seal()` call the
fresh-bus caller would have no meaningful floor to seal against.

**2. Why the deviation from the shipped `Sequence` template was taken.** `Sequence` (fixed by
`ID-2-WIRING-SEAL`) has ZERO production call sites, so making `NewSequence` born-unsealed cost
nothing live. `NameSuffixes` is different: it is wired into the LIVE enrolment path.
`cmd/agent-bus/main.go:327` builds `ids.NewNameSuffixes()` on every server start, and every
enrolment mints an agent id through it; `internal/auth` and `internal/httpapi`'s test suites also
construct through it. Making `NewNameSuffixes` born-unsealed with no compensating change would have
made it refuse EVERY enrolment on a running bus the moment this task landed -- and fixing the actual
caller at `cmd/agent-bus/main.go:327` to construct via `ResumeNameSuffixes` plus a real per-name
floor derivation was explicitly OUTSIDE this task's file boundary (internal/ids only, one task, mid
a parallel wave -- see status_note). The born-sealed `NewNameSuffixes` is the choice that keeps the
live path working without touching `cmd/`, `internal/auth`, or `internal/httpapi`.

**3. The residual hole, recorded honestly.** A caller that attempts a per-name floor derivation,
has that derivation FAIL, and then falls back to calling `NewNameSuffixes()` mints every name from 1,
silently -- exactly as before this gate existed. Security's ruling (kind=response, 2026-08-03): this
is NOT attacker-reachable today (no client input selects which constructor a caller uses), and it is
strictly net-positive versus the status quo ante -- before this task, NEITHER constructor was gated;
after it, the resume path, which is the only one that will ever carry a real derivation, is gated,
and the fresh path can no longer silently absorb a late/partial derivation the way an ungated
`NewNameSuffixes` used to. Reviewer's sharper point, which belongs in the record rather than only in
a review note: `cmd/agent-bus/main.go` performs NO per-name derivation at all today -- it just calls
`ids.NewNameSuffixes()` -- so the residual hole described above is not a hypothetical edge case, it
is the DEFAULT behaviour of the bus today. Closing it is not optional hardening; it is finishing the
job. That is exactly what `MSG-FU-SUFFIXFLOOR` (cross-linked below) is for.

**4. Both gates' verdicts on the shipped 5-file diff** (`internal/ids/{agentmint.go,sequence.go,
doc.go,agentmint_test.go,sequence_test.go}`): reviewer returned **CHANGES-REQUESTED** -- documentation
plus this spec amendment only, explicitly NO behavioural/code change requested, no P0 findings.
Security returned **PASS-WITH-NOTES** -- no P0 and no P1 open.

**5. Cross-reference.** The residual hole named in point 3 is closed by P0 task
`MSG-FU-SUFFIXFLOOR`, public_id `94159d93-fe87-4c3e-b938-86fe7068c787` ("resume per-name agent-id
suffix counters from disk (agent ids are now durable)"). See that task's acceptance criteria, appended
2026-08-03 as a `kind=request` note.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


_None recorded._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [AUTH-3](../../AUTH/AUTH-3--d53e3b21/task.md) — AUTH-3: Roster persistence & recovery (in_progress)
- [ID-2-WIRING-SEAL](../ID-2-WIRING-SEAL--8c9b6489/task.md) — ID-2-WIRING-SEAL: Sequence refuses to issue from an UNSEALED floor (the only half impleme… (done)
- [ID-2-WIRING-SEAL-FU-CONTRACTS](../ID-2-WIRING-SEAL-FU-CONTRACTS--9c183c8e/task.md) — ID-2-WIRING-SEAL-FU-CONTRACTS: land the Sequence seal contract rows that the file-boundar… (todo)
- [MSG-FU-SUFFIXFLOOR](../MSG-FU-SUFFIXFLOOR--94159d93/task.md) — MSG-FU-SUFFIXFLOOR: resume per-name agent-id suffix counters from disk (agent ids are now… (in_progress)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [9e0db530-e0d1-4b7e-8190-a5b9a0e2ff29](../Question-whether-a-peer-belongs-on-the-legitimate-floor--9e0db530/task.md) — Question whether a peer belongs on the legitimate floor-source list at all (ids.RaiseFloo… (todo)
- [MSG-FU-SUFFIXFLOOR](../MSG-FU-SUFFIXFLOOR--94159d93/task.md) — MSG-FU-SUFFIXFLOOR: resume per-name agent-id suffix counters from disk (agent ids are now… (in_progress)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
