# MSG-FU-SUFFIXFLOOR: resume per-name agent-id suffix counters from disk (agent ids are now durable)

| Field | Value |
| --- | --- |
| Public id | `94159d93-fe87-4c3e-b938-86fe7068c787` |
| Key | _(null in the export)_ |
| Epic | [ID](../epic.md) |
| Status | in_progress |
| Priority | P0 |
| Component | id |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T22:31:37.073715+00:00 |
| Updated | 2026-08-08T10:29:39.001593+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run TestAgentIDSuffixesResumeAcrossRestart ./internal/ids ./internal/hub
```

## Status note

internal/ids half CODE-COMPLETE and unstaged-for-commit 2026-08-07; NOT live. cmd/agent-bus/main.go:327 still calls ids.NewNameSuffixes() and there are ZERO production callers of ids.OpenNameSuffixes, so a restarting bus STILL re-mints agent ids. Acceptance criteria (a)-(d) all concern main.go and remain OPEN. Do not close this task until a running server, restarted, provably mints a strictly greater suffix for a re-enrolled name.

## Description

Found by the security gate during the MSG/POLL wave (2026-08-02). cmd/agent-bus/main.go builds ids.NewNameSuffixes() -- a FRESH counter every start -- justified by the comment 'nothing in this path writes an agent id to disk'. THAT PREMISE IS NOW FALSE: store.Record persists sender and recipients as fully-qualified agent ids, hub.publish writes them through the WAL, hub.Apply replays them, and the WAL never compacts. So after a restart the suffix counter restarts at 1 and anyone who enrols the name 'alpha' is minted the id the previous alpha held (invariant 1 broken). CONFIDENTIALITY IS ALREADY CLOSED by the enrolment epoch shipped in the same wave (store.Message.VisibleTo refuses any message sent before the reader enrolled -- proved on a live server: a re-enrolled beta-1 reads 0 of the previous holder's DMs while the message is still in the store), and the reuse is logged at ERROR by hub.NoteEnrolment. WHAT REMAINS is identity continuity: a new keypair holding an id with a prior history, whose future messages are attributed to it. FIX: derive a per-name suffix floor from the highest suffix EVER WRITTEN TO DISK -- parse every sender and recipient seen during replay through ids.ParseAgentID and keep the max per name -- and seed ids.ResumeNameSuffixes with it before the listener binds. internal/hub already collects exactly these ids in Apply (see Hub.recovered), so the derivation belongs there and main passes it to the minter. ALSO correct the now-false justification comment at cmd/agent-bus/main.go:312-317: it is what will make the next reader believe this is safe. AUTH-3 (durable roster) is the complete fix; this is the half that does not depend on it.
---

## ACCEPTANCE CRITERIA ADDED 2026-08-03 (spec-keeper, dictated by security)

Security's PASS-WITH-NOTES verdict on `ID-2-WIRING-SEAL-FU-NAMESUFFIXES` (public_id
`1c207a62-e904-4988-84c2-f4b69712ee35`) named these as MUST-CLOSE-BEFORE-ENROLMENT-IS-DURABLE
conditions for THIS task:

(a) `cmd/agent-bus/main.go` constructs the allocator via `ids.ResumeNameSuffixes` (or `RaiseFloor`
    folded over the replay stream) and calls `Seal()` exactly ONCE with the error CHECKED.
(b) A derivation that cannot complete is a FATAL startup error -- explicitly NEVER a fallback to
    `ids.NewNameSuffixes()`, which is the residual hole this task exists to close by name.
(c) Once `main.go` no longer calls `ids.NewNameSuffixes()`, flip `NewNameSuffixes` to born-unsealed
    to restore parity with `Sequence`, or delete it.
(d) Cheap interim guard worth adding: a test asserting no production package outside `cmd/` calls
    `ids.NewNameSuffixes`.

See `ID-2-WIRING-SEAL-FU-NAMESUFFIXES` notes for the full security/reviewer context this closes the
residual gap in.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [AUTH-3](../../AUTH/AUTH-3--d53e3b21/task.md) — AUTH-3: Roster persistence & recovery (done)
- [ID-2-WIRING-SEAL-FU-NAMESUFFIXES](../ID-2-WIRING-SEAL-FU-NAMESUFFIXES--1c207a62/task.md) — ID-2-WIRING-SEAL-FU-NAMESUFFIXES: NameSuffixes has the identical inert-floor-guard defect… (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [1aed37a9-3a8e-4940-8b36-ee2dbe28afb5](../Unify-the-atomic-temp-rename-fsync-file-writer-duplicate--1aed37a9/task.md) — Unify the atomic temp+rename+fsync file writer duplicated between ids.writeBusIDFile and… (todo)
- [2db4a36f-561d-4ecb-8ba6-6329183c36cd](../Wire-the-durable-agent-id-suffix-floors-into-server-star--2db4a36f/task.md) — Wire the durable agent-id suffix floors into server startup (superseded)
- [3e46d43b-ac93-4c69-89cc-e299133791b4](../Amortise-the-agent-suffixes-write-reserve-a-block-of-suf--3e46d43b/task.md) — Amortise the agent-suffixes write: reserve a block of suffixes per name instead of one fi… (todo)
- [ID-2-WIRING](../ID-2-WIRING--838677e6/task.md) — ID-2-WIRING: Derive the sequence resume floor from ALL prepares, never from committed his… (superseded)
- [ID-2-WIRING-OBSERVER](../ID-2-WIRING-OBSERVER--c31f6999/task.md) — ID-2-WIRING-OBSERVER: wal offers EVERY prepare (committed, aborted AND dangling) to an ob… (todo)
- [ID-2-WIRING-SEAL-FU-NAMESUFFIXES](../ID-2-WIRING-SEAL-FU-NAMESUFFIXES--1c207a62/task.md) — ID-2-WIRING-SEAL-FU-NAMESUFFIXES: NameSuffixes has the identical inert-floor-guard defect… (done)
- [MSG-FU-SUFFIXFLOOR-FU-DOCS](../../DOCS/MSG-FU-SUFFIXFLOOR-FU-DOCS--e5fa08ba/task.md) — MSG-FU-SUFFIXFLOOR-FU-DOCS: PROTOCOL.md and internal/ids docs still say the suffix wiring… (todo)
- [MSG-FU-SUFFIXFLOOR-FU-ENROLRECORDS](../MSG-FU-SUFFIXFLOOR-FU-ENROLRECORDS--477b8eeb/task.md) — MSG-FU-SUFFIXFLOOR-FU-ENROLRECORDS: fold ENROLMENT records into the legacy-dir suffix bac… (todo)
- [MSG-FU-SUFFIXFLOOR-FU-ROSTERASSERT](../MSG-FU-SUFFIXFLOOR-FU-ROSTERASSERT--6b0e561e/task.md) — MSG-FU-SUFFIXFLOOR-FU-ROSTERASSERT: assert a freshly-minted agent-suffix is not already i… (todo)
- [MSG-FU-SUFFIXFLOOR-FU-STREAMSCAN](../../DUR/MSG-FU-SUFFIXFLOOR-FU-STREAMSCAN--6f4c17ef/task.md) — MSG-FU-SUFFIXFLOOR-FU-STREAMSCAN: export a streaming raw WAL scan and reinstate the every… (todo)
- [MSG-FU-SUFFIXFLOOR-FU-UNSEAL](../MSG-FU-SUFFIXFLOOR-FU-UNSEAL--d5ed5ccc/task.md) — MSG-FU-SUFFIXFLOOR-FU-UNSEAL: make ids.NewNameSuffixes born-unsealed (or delete it) now t… (todo)
- [a18a9a00-33e3-46e2-bec1-61bae440fc55](../The-hub-id-reuse-detector-is-narrower-than-its-log-line--a18a9a00/task.md) — The hub id-reuse detector is narrower than its log line implies (broadcast-only agents le… (todo)
- [cca64afd-f75d-46e4-91ca-ebc502151253](../../RELAY/RELAY-precondition-roster-check-LOCAL-recipients-before--cca64afd/task.md) — RELAY precondition: roster-check LOCAL recipients before the durable write, or a peer can… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
