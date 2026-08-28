# AUTH-3: Roster persistence & recovery

| Field | Value |
| --- | --- |
| Public id | `d53e3b21-2dae-4be8-974f-87ce7d62c919` |
| Key | AUTH-3 |
| Epic | [AUTH](../epic.md) |
| Status | done |
| Priority | P0 |
| Component | auth |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T09:05:48.742293+00:00 |
| Updated | 2026-08-14T22:31:01.182209+00:00 |
| Completed | 2026-08-14T22:31:01.182193+00:00 |

## Proof command

```sh
go test -race -run TestAUTH3AcceptanceAgentAuthenticatesAndIsListedAfterARestart ./internal/auth
```

## Status note

UNBLOCKED 2026-08-07. ENROL-SHAPE is settled in DECISIONS.md (commit a7a4c3d). AUTH-3 must encode the FULL target RosterEntry field set even for fields nothing populates yet: AgentID, Name, AuthPublicKey (renamed from PublicKey), MessagingPublicKey, InviteID, Epoch, CertBindings []CertBinding{Fingerprint,BoundAt,RetiredAt}. Writing the durable record once with reserved-but-empty fields is why this was blocked rather than deprioritised -- landing a narrower record means migrating it three times and force-re-enrolling every agent. | UPDATE 2026-08-08 (spec-keeper): commit b293ce2 landed "Crash point D" (torn PREPARE), proving floors.go enrolment scan is NOT a suffix floor -- comment-only, gates PASS. That is ONE increment, not completion: this task own stored proof_cmd (TestRosterRecovery) is VACUOUS, no such test exists. Task remains in_progress pending the full durable-roster acceptance criteria.

## Description

The agent roster (id, name, public key/verifier material, enrolled-at) is rebuilt on startup by WAL replay, not held only in memory -- an agent enrolled before a restart is still authenticated and listed after one, with no re-enrolment required.

CORRECTION (spec-keeper, 2026-08-02, from ID-3 security+reviewer gate findings): the resume floor for name suffixes must NOT be derived from the committed roster alone -- internal/ids/agentmint.go point 3 explicitly forbids that derivation. It must be reconstructed from ALL prepares ever written -- committed, aborted, AND dangling -- covering agents still enrolled and agents that have since departed, or a new agent minted with a different keypair can silently inherit a previous agent's id/suffix. This task must land BEFORE any enrolment record reaches the WAL (once an agent id is on disk, id-reuse-on-restart escalates from MEDIUM to CRITICAL). Cross-reference ID-2-WIRING-OBSERVER (c31f6999-da4e-400d-ab55-178b82e2a42e), the task that exposes dangling prepares needed to compute this floor correctly.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [ENROL-SHAPE](../../INVITE/ENROL-SHAPE--8942c8c8/task.md) — ENROL-SHAPE: settle the FINAL /v1/enroll wire shape and auth.RosterEntry field set ONCE,… (done)
- [ID-2-WIRING-OBSERVER](../../ID/ID-2-WIRING-OBSERVER--c31f6999/task.md) — ID-2-WIRING-OBSERVER: wal offers EVERY prepare (committed, aborted AND dangling) to an ob… (todo)
- [ID-3](../../ID/ID-3--745cce13/task.md) — ID-3: Agent id minting \`&lt;bus-id&gt;.&lt;name&gt;-&lt;n&gt;\` (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [1c4d3dea-b4f6-4f68-b823-78bb76a6b5aa](../SEC-unauthenticated-enrol-permanently-bricks-the-roster--1c4d3dea/task.md) — SEC: unauthenticated enrol permanently bricks the roster -- 4096-cap fails closed forever… (done)
- [AUTH-1](../AUTH-1--54fa94c0/task.md) — AUTH-1: POST /v1/enroll -- signed credential issuance (done)
- [CRYPTO-1](../../CRYPTO/CRYPTO-1--30570fb9/task.md) — CRYPTO-1: DESIGN SPIKE -- Signal-style E2E crypto for agent-bus (CRYPTO_DEEPDIVE.md + DEC… (done)
- [CRYPTO-3](../../CRYPTO/CRYPTO-3--dd1066af/task.md) — CRYPTO-3: Enrolment mints and registers the second (messaging) keypair, bound to the serv… (todo)
- [ENROL-SHAPE](../../INVITE/ENROL-SHAPE--8942c8c8/task.md) — ENROL-SHAPE: settle the FINAL /v1/enroll wire shape and auth.RosterEntry field set ONCE,… (done)
- [ID-2-WIRING](../../ID/ID-2-WIRING--838677e6/task.md) — ID-2-WIRING: Derive the sequence resume floor from ALL prepares, never from committed his… (superseded)
- [ID-2-WIRING-OBSERVER](../../ID/ID-2-WIRING-OBSERVER--c31f6999/task.md) — ID-2-WIRING-OBSERVER: wal offers EVERY prepare (committed, aborted AND dangling) to an ob… (todo)
- [ID-2-WIRING-SEAL-FU-NAMESUFFIXES](../../ID/ID-2-WIRING-SEAL-FU-NAMESUFFIXES--1c207a62/task.md) — ID-2-WIRING-SEAL-FU-NAMESUFFIXES: NameSuffixes has the identical inert-floor-guard defect… (done)
- [ID-3](../../ID/ID-3--745cce13/task.md) — ID-3: Agent id minting \`&lt;bus-id&gt;.&lt;name&gt;-&lt;n&gt;\` (done)
- [IDEM-13](../../IDEM/IDEM-13--a869264d/task.md) — IDEM-13: Idempotent enrol / leave / peer-enrol (todo)
- [IDEM-6](../../IDEM/IDEM-6--208c4fb5/task.md) — IDEM-6: Idempotent enrol, leave, and peer-enrol (superseded)
- [LIVE-5](../../LIVE/LIVE-5--7f62eeee/task.md) — LIVE-5: Durable last-observation and restart liveness reconstruction (todo)
- [MSG-FU-ROSTERSOURCE](../MSG-FU-ROSTERSOURCE--fa26036c/task.md) — MSG-FU-ROSTERSOURCE: the hub must read the AUTHORITATIVE roster the moment AUTH-3 makes e… (done)
- [MSG-FU-SUFFIXFLOOR](../../ID/MSG-FU-SUFFIXFLOOR--94159d93/task.md) — MSG-FU-SUFFIXFLOOR: resume per-name agent-id suffix counters from disk (agent ids are now… (in_progress)
- [MSG-FU-SUFFIXFLOOR-FU-ENROLRECORDS](../../ID/MSG-FU-SUFFIXFLOOR-FU-ENROLRECORDS--477b8eeb/task.md) — MSG-FU-SUFFIXFLOOR-FU-ENROLRECORDS: fold ENROLMENT records into the legacy-dir suffix bac… (todo)
- [MTLS-BIND](../../MTLS/MTLS-BIND--b6378bda/task.md) — MTLS-BIND: enrolment binds the presenting client-cert fingerprint to the SERVER-MINTED ag… (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
