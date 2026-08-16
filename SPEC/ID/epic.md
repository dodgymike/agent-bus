# EPIC ID — Server-authoritative id minting

[← all epics](../../SPEC.md)

**11 open / 19 total.** Full records live in `SPEC/ID/<task>/task.md`.

_Relations (real)_ are authoritative edges from the Spec Server. _Referenced (derived)_ are guesses parsed out of description free text — useful, but not a dependency list.

## Open tasks (11)

| Key | Title | Status | Prio | Task | Relations (real) | Referenced (derived) |
| --- | --- | --- | --- | --- | --- | --- |
| ID-2-WIRING-OBSERVER | ID-2-WIRING-OBSERVER: wal offers EVERY prepare (committed, aborted AND dangling) to an ob… | todo | P0 | [task.md](ID-2-WIRING-OBSERVER--c31f6999/task.md) | _not fetched_ | [ID-2-WIRING](ID-2-WIRING--838677e6/task.md) [ID-2-WIRING-SCHEMA](ID-2-WIRING-SCHEMA--80b54ee4/task.md) [SIGN-2](../SIGN/SIGN-2--1c183f10/task.md) [SIGN-6](../SIGN/SIGN-6--c9e4aea1/task.md) [MSG-FU-SEQHIGHWATER](../DUR/MSG-FU-SEQHIGHWATER--6ebe51be/task.md) [MSG-FU-SUFFIXFLOOR](MSG-FU-SUFFIXFLOOR--94159d93/task.md) +1 more |
| MSG-FU-SUFFIXFLOOR | MSG-FU-SUFFIXFLOOR: resume per-name agent-id suffix counters from disk (agent ids are now… | in_progress | P0 | [task.md](MSG-FU-SUFFIXFLOOR--94159d93/task.md) | _not fetched_ | [AUTH-3](../AUTH/AUTH-3--d53e3b21/task.md) [ID-2-WIRING-SEAL-FU-NAMESUFFIXES](ID-2-WIRING-SEAL-FU-NAMESUFFIXES--1c207a62/task.md) |
| 3e46d43b-ac93-4c69-89cc-e299133791b4 | Amortise the agent-suffixes write: reserve a block of suffixes per name instead of one fi… | todo | P1 | [task.md](Amortise-the-agent-suffixes-write-reserve-a-block-of-suf--3e46d43b/task.md) | _not fetched_ | [MSG-FU-SUFFIXFLOOR](MSG-FU-SUFFIXFLOOR--94159d93/task.md) |
| ID-2-WIRING-SEAL-FU-CONTRACTS | ID-2-WIRING-SEAL-FU-CONTRACTS: land the Sequence seal contract rows that the file-boundar… | todo | P1 | [task.md](ID-2-WIRING-SEAL-FU-CONTRACTS--9c183c8e/task.md) | _not fetched_ | [ID-2-WIRING-SEAL](ID-2-WIRING-SEAL--8c9b6489/task.md) [CONTRACTS-SPLIT](../DOCS/CONTRACTS-SPLIT--360a2679/task.md) [ID-2-WIRING](ID-2-WIRING--838677e6/task.md) [ID-2-WIRING-SCHEMA](ID-2-WIRING-SCHEMA--80b54ee4/task.md) |
| ID-4 | ID-4: Id-counter recovery property test | todo | P1 | [task.md](ID-4--72c97d23/task.md) | _not fetched_ | — |
| MSG-FU-SUFFIXFLOOR-FU-ROSTERASSERT | MSG-FU-SUFFIXFLOOR-FU-ROSTERASSERT: assert a freshly-minted agent-suffix is not already i… | todo | P1 | [task.md](MSG-FU-SUFFIXFLOOR-FU-ROSTERASSERT--6b0e561e/task.md) | _not fetched_ | [MSG-FU-SUFFIXFLOOR-FU-STREAMSCAN](../DUR/MSG-FU-SUFFIXFLOOR-FU-STREAMSCAN--6f4c17ef/task.md) [ID-2-WIRING-OBSERVER](ID-2-WIRING-OBSERVER--c31f6999/task.md) [AUTH-4](../AUTH/AUTH-4--a853261d/task.md) [MSG-FU-SUFFIXFLOOR](MSG-FU-SUFFIXFLOOR--94159d93/task.md) [MSG-FU-SUFFIXFLOOR-FU-ENROLRECORDS](MSG-FU-SUFFIXFLOOR-FU-ENROLRECORDS--477b8eeb/task.md) [be447589-6583-4d5c-a9d4-ec9d9fef0f1c](../CORE/Enforce-data-directory-permissions-at-startup-and-bound--be447589/task.md) +3 more |
| MSG-FU-SUFFIXFLOOR-FU-UNSEAL | MSG-FU-SUFFIXFLOOR-FU-UNSEAL: make ids.NewNameSuffixes born-unsealed (or delete it) now t… | todo | P1 | [task.md](MSG-FU-SUFFIXFLOOR-FU-UNSEAL--d5ed5ccc/task.md) | _not fetched_ | [MSG-FU-SUFFIXFLOOR](MSG-FU-SUFFIXFLOOR--94159d93/task.md) [2db4a36f-561d-4ecb-8ba6-6329183c36cd](Wire-the-durable-agent-id-suffix-floors-into-server-star--2db4a36f/task.md) |
| 1aed37a9-3a8e-4940-8b36-ee2dbe28afb5 | Unify the atomic temp+rename+fsync file writer duplicated between ids.writeBusIDFile and… | todo | P2 | [task.md](Unify-the-atomic-temp-rename-fsync-file-writer-duplicate--1aed37a9/task.md) | _not fetched_ | [MSG-FU-SUFFIXFLOOR](MSG-FU-SUFFIXFLOOR--94159d93/task.md) |
| 9e0db530-e0d1-4b7e-8190-a5b9a0e2ff29 | Question whether a peer belongs on the legitimate floor-source list at all (ids.RaiseFloo… | todo | P2 | [task.md](Question-whether-a-peer-belongs-on-the-legitimate-floor--9e0db530/task.md) | _not fetched_ | [ID-2-WIRING-SEAL-FU-NAMESUFFIXES](ID-2-WIRING-SEAL-FU-NAMESUFFIXES--1c207a62/task.md) |
| MSG-FU-SUFFIXFLOOR-FU-ENROLRECORDS | MSG-FU-SUFFIXFLOOR-FU-ENROLRECORDS: fold ENROLMENT records into the legacy-dir suffix bac… | todo | P2 | [task.md](MSG-FU-SUFFIXFLOOR-FU-ENROLRECORDS--477b8eeb/task.md) | _not fetched_ | [MSG-FU-SUFFIXFLOOR](MSG-FU-SUFFIXFLOOR--94159d93/task.md) [AUTH-3](../AUTH/AUTH-3--d53e3b21/task.md) [MSG-FU-SUFFIXFLOOR-FU-STREAMSCAN](../DUR/MSG-FU-SUFFIXFLOOR-FU-STREAMSCAN--6f4c17ef/task.md) |
| a18a9a00-33e3-46e2-bec1-61bae440fc55 | The hub id-reuse detector is narrower than its log line implies (broadcast-only agents le… | todo | P2 | [task.md](The-hub-id-reuse-detector-is-narrower-than-its-log-line--a18a9a00/task.md) | _not fetched_ | [MSG-FU-SUFFIXFLOOR](MSG-FU-SUFFIXFLOOR--94159d93/task.md) |

## Closed tasks (8) — done, cancelled, superseded

| Key | Title | Status | Prio | Task | Relations (real) | Referenced (derived) |
| --- | --- | --- | --- | --- | --- | --- |
| 2db4a36f-561d-4ecb-8ba6-6329183c36cd | Wire the durable agent-id suffix floors into server startup | superseded | P0 | [task.md](Wire-the-durable-agent-id-suffix-floors-into-server-star--2db4a36f/task.md) | _not fetched_ | [MSG-FU-SUFFIXFLOOR](MSG-FU-SUFFIXFLOOR--94159d93/task.md) |
| ID-1 | ID-1: Bus id minting + persistence | done | P0 | [task.md](ID-1--5ab7514b/task.md) | _not fetched_ | — |
| ID-2 | ID-2: Monotonic sequence allocator (drives message ids) | done | P0 | [task.md](ID-2--a3a5edc4/task.md) | _not fetched_ | — |
| ID-2-WIRING | ID-2-WIRING: Derive the sequence resume floor from ALL prepares, never from committed his… | superseded | P0 | [task.md](ID-2-WIRING--838677e6/task.md) | _not fetched_ | [ID-2-WIRING-SEAL](ID-2-WIRING-SEAL--8c9b6489/task.md) [ID-2-WIRING-SCHEMA](ID-2-WIRING-SCHEMA--80b54ee4/task.md) [ID-2-WIRING-OBSERVER](ID-2-WIRING-OBSERVER--c31f6999/task.md) [ID-2](ID-2--a3a5edc4/task.md) [SIGN-2](../SIGN/SIGN-2--1c183f10/task.md) [SIGN-6](../SIGN/SIGN-6--c9e4aea1/task.md) +3 more |
| ID-2-WIRING-SCHEMA | ID-2-WIRING-SCHEMA: DECIDE and record where the message sequence high-water mark lives on… | done | P0 | [task.md](ID-2-WIRING-SCHEMA--80b54ee4/task.md) | _not fetched_ | [ID-2-WIRING](ID-2-WIRING--838677e6/task.md) [ID-2-WIRING-OBSERVER](ID-2-WIRING-OBSERVER--c31f6999/task.md) [2a961fcc-426d-4c98-bc63-eb236367fd85](../DUR/Startup-scans-the-WAL-twice-soon-three-times-bound-the-c--2a961fcc/task.md) |
| ID-2-WIRING-SEAL | ID-2-WIRING-SEAL: Sequence refuses to issue from an UNSEALED floor (the only half impleme… | done | P0 | [task.md](ID-2-WIRING-SEAL--8c9b6489/task.md) | _not fetched_ | [ID-2-WIRING](ID-2-WIRING--838677e6/task.md) [ID-2-WIRING-SCHEMA](ID-2-WIRING-SCHEMA--80b54ee4/task.md) |
| ID-2-WIRING-SEAL-FU-NAMESUFFIXES | ID-2-WIRING-SEAL-FU-NAMESUFFIXES: NameSuffixes has the identical inert-floor-guard defect… | done | P0 | [task.md](ID-2-WIRING-SEAL-FU-NAMESUFFIXES--1c207a62/task.md) | _not fetched_ | [ID-2-WIRING-SEAL](ID-2-WIRING-SEAL--8c9b6489/task.md) [ID-2-WIRING-SEAL-FU-CONTRACTS](ID-2-WIRING-SEAL-FU-CONTRACTS--9c183c8e/task.md) [AUTH-3](../AUTH/AUTH-3--d53e3b21/task.md) [MSG-FU-SUFFIXFLOOR](MSG-FU-SUFFIXFLOOR--94159d93/task.md) |
| ID-3 | ID-3: Agent id minting \`&lt;bus-id&gt;.&lt;name&gt;-&lt;n&gt;\` | done | P0 | [task.md](ID-3--745cce13/task.md) | _not fetched_ | [DUR-10](../DUR/DUR-10--bab09b2e/task.md) [ID-2](ID-2--a3a5edc4/task.md) [AUTH-1](../AUTH/AUTH-1--54fa94c0/task.md) [AUTH-3](../AUTH/AUTH-3--d53e3b21/task.md) |

## Epic description

Bus id (persisted, stable across restarts), monotonic sequence allocator, agent id `<bus-id>.<name>-<n>`, message ids. Counters restored on recovery so no id is ever re-issued (invariant 1).

---

_Generated by `scripts/gen-spec-mirror.sh`; never hand-edit._
