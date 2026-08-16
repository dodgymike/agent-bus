# RELAY-24-BLOCKER-HUBINGEST: internal/hub exported relay-ingest entry point -- foreign sender, no local mint, bus path preserved, idem.Outcome uncollapsed

| Field | Value |
| --- | --- |
| Public id | `9ee98866-d8c2-472c-8711-383c22997dda` |
| Key | RELAY-24-BLOCKER-HUBINGEST |
| Epic | [RELAY](../epic.md) |
| Status | done |
| Priority | P0 |
| Component | hub |
| Section | backlog |
| Tags | — |
| Created | 2026-08-14T14:30:43.508061+00:00 |
| Updated | 2026-08-14T16:10:08.921595+00:00 |
| Completed | 2026-08-14T16:10:08.921578+00:00 |

## Proof command

```sh
go test -race -run 'TestIngestRelayed|TestRelayIngestCrash|TestForeignSenderIsRefusedByEveryOtherWritePath' ./internal/hub
```

## Status note

CODE-COMPLETE 2026-08-14, NOT COMMITTED. Both gates PASS. Eight staged internal/hub/ files, package green at 222 tests, gofmt clean. HELD on documentation: the reviewer requires PROTOCOL.md section 8.6 and DECISIONS.md text to land BEFORE OR WITH this commit, not after -- same discipline this session has enforced elsewhere (a wire-visible behaviour change must not land ahead of its doc). Do not complete this task until that documentation lands and the commit exists. Also note: this task is itself now blocked-adjacent to SIGN-1-FU-OUTOFORDER-POISON (bbd81523) -- security has ruled hub.IngestRelayed must not be wired into a SERVED acceptor until that is fixed; this task's own commit should land the method without wiring it live, or should explicitly gate the live wiring on bbd81523, per security's ruling recorded there.

## Description

Filed 2026-08-14. RELAY-24's implementer attempted the composition-root wiring and discovered this method does not exist anywhere: relay.LocalIngest cannot be implemented over internal/hub's exported API, so relay.NewAcceptor cannot be constructed, so RelayConfig.AcceptRelay cannot be supplied, so httpapi.PeerSurface.Relay cannot be built (PeerSurface requires EVERY field). RELAY-24 is BLOCKED on this, with no code written -- see RELAY-24's own notes for the full evidence (hub.Mint/hub.Send both refuse a foreign sender not on the local roster; publish consumes a LOCAL mint reservation a peer bus never obtains; publishRequest.busPath is unexported with no exported caller; hub.Result reports a bool Replayed, not the uncollapsed idem.Outcome invariant 10 requires).

SURFACE TO ADD: `func (h *Hub) IngestRelayed(ctx context.Context, req RelayedIngestRequest) (RelayedIngestResult, error)` where `RelayedIngestResult` carries `{MessageID, Seq, Outcome}`.

FIVE REQUIREMENTS:
1. Do NOT require the sender on the local roster -- require the sender's BUS half to NOT be ours (the mirror-image check of the local-send path).
2. Mint the local sequence INTERNALLY -- no client-supplied mint, ever (this is server-authoritative sequence territory, invariant 1).
3. Carry the peer bus path through relay.CheckIncomingPath into publishRequest.busPath -- this is what RELAY-11 (done, d4a1985) built the capability for and RELAY-11-FU-INGEST-LOOPGUARD (a41c273c) exists to guard.
4. Return idem.Outcome UNCOLLAPSED -- invariant 10's three-way split (new/retry/violation) must survive this seam. Collapsing it here is exactly the zero-value amplification trap already recorded on RELAY-24 (idem.Outcome's zero value is OutcomeNew, the FORWARDING answer -- a seam that fills MessageID and forgets Outcome silently re-forwards everything).
5. Stay on the SINGLE two-phase write path -- no second durable write path, no second applied-key answer. A second write path is exactly what this task must NOT introduce; RelayConfig.AcceptRelay's own doc and hub.publish's own doc both forbid it.

PROVENANCE, RECORDED BECAUSE IT IS THE INTERESTING PART -- this hole belongs to no task, by MUTUAL DEFERRAL, not neglect by any one agent. RELAY-11-FU-INGEST-LOOPGUARD (a41c273c-3573-4144-bdab-fb77f063e993) states that RELAY-21 "is the task that wires ingest to hub.publish." RELAY-21 shipped the relay.LocalIngest INTERFACE and deferred the implementation to the wiring site (RELAY-24). internal/relay/doc.go gap 4 agrees: "WHAT IS STILL THE WIRING SITE'S: supplying the Acceptor with a real applied-key store and a real forwarder." So RELAY-21 pointed at RELAY-24 and RELAY-24's own dependency chain pointed back at RELAY-21/internal/hub -- each side deferred to the other, the method exists in neither, and it took RELAY-24 actually attempting the wiring to discover the gap. This is a failure mode of task decomposition across a package boundary, not a defect in any one agent's work -- worth remembering when splitting an interface's DEFINITION from its IMPLEMENTATION across two tasks that don't share a file boundary.

DEPENDENCIES: real blocks relations wired -- this task BLOCKS RELAY-24 (e303c624), which BLOCKS RELAY-25 (10491a01, pre-existing edge). This task is now the head of the critical path fed-smoke.sh needs.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [RELAY-11](../RELAY-11--07824e55/task.md) — RELAY-11: store/hub can record a MULTI-HOP bus path (done)
- [RELAY-11-FU-INGEST-LOOPGUARD](../RELAY-11-FU-INGEST-LOOPGUARD--a41c273c/task.md) — Relay ingest MUST route through relay.CheckIncomingPath before hub.publish, or a 64-hop l… (todo)
- [RELAY-21](../RELAY-21--f5ce883e/task.md) — RELAY-21: AcceptRelay callback: roster-check before durable write, re-forward on OutcomeN… (done)
- [RELAY-24](../RELAY-24--e303c624/task.md) — RELAY-24: Composition root: wire federation into cmd/agent-bus/main.go (done)
- [RELAY-25](../RELAY-25--10491a01/task.md) — RELAY-25: fed-smoke.sh: the epic's deliverable -- three-bus loopback federation smoke test (in_progress)
- [SIGN-1-FU-OUTOFORDER-POISON](../../SIGN/SIGN-1-FU-OUTOFORDER-POISON--bbd81523/task.md) — SIGN-1-FU-OUTOFORDER-POISON: Reserve-then-send lets mints be spent out of order, which pe… (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [HUB-FU-INGEST-SIGNATURE-GUARD](../HUB-FU-INGEST-SIGNATURE-GUARD--4b3c79c5/task.md) — HUB-FU-INGEST-SIGNATURE-GUARD: AST guard that IngestRelayed's caller verified the signatu… (todo)
- [HUB-FU-RECOVER-IDEM-RELAY-ARM](../HUB-FU-RECOVER-IDEM-RELAY-ARM--5e74485a/task.md) — HUB-FU-RECOVER-IDEM-RELAY-ARM: recoverIdemRecord has no relay arm, and a lost relay appli… (todo)
- [IDEM-FU-RESULTBYTES-VS-MAXRECIPIENTS](../../IDEM/IDEM-FU-RESULTBYTES-VS-MAXRECIPIENTS--6a09349b/task.md) — IDEM-FU-RESULTBYTES-VS-MAXRECIPIENTS: idem.MaxResultBytes (512) is too small for a multi-… (todo)
- [INVITE-FU-STORE-TEST-RED-ON-MAIN](../../INVITE/INVITE-FU-STORE-TEST-RED-ON-MAIN--fb7be1d6/task.md) — INVITE-FU-STORE-TEST-RED-ON-MAIN: TestInviteNotDurableIsRefused fails on a pristine HEAD (done)
- [RELAY-24-BLOCKER-HUBINGEST-FU-AUDITHASH-DOC](../RELAY-24-BLOCKER-HUBINGEST-FU-AUDITHASH-DOC--7126f08b/task.md) — RELAY-24-BLOCKER-HUBINGEST-FU-AUDITHASH-DOC: Record the relayed audit content-hash decisi… (done)
- [RELAY-FU-BUSPATH-BIND-PEER](../RELAY-FU-BUSPATH-BIND-PEER--f6a9fad0/task.md) — RELAY-FU-BUSPATH-BIND-PEER: Bind the arriving BusPath's last hop to the authenticated pee… (todo)
- [RELAY-FU-BUSPATH-OFFBYONE](../RELAY-FU-BUSPATH-OFFBYONE--97fc6038/task.md) — RELAY-FU-BUSPATH-OFFBYONE: bus-path off-by-one between internal/relay/path.go:128 and int… (done)
- [RELAY-FU-IDEM-METER-BY-PEER](../RELAY-FU-IDEM-METER-BY-PEER--8774f265/task.md) — RELAY-FU-IDEM-METER-BY-PEER: Meter the applied-key table by the AUTHENTICATED PEER, not t… (done)
- [SIGN-1-FU-OUTOFORDER-POISON](../../SIGN/SIGN-1-FU-OUTOFORDER-POISON--bbd81523/task.md) — SIGN-1-FU-OUTOFORDER-POISON: Reserve-then-send lets mints be spent out of order, which pe… (done)
- [SPEC-API-LIST-SILENT-TRUNCATION](../../UNASSIGNED/SPEC-API-LIST-SILENT-TRUNCATION--82f35b73/task.md) — Task-list API silently truncates at 200 with no total, no next and no working pagination… (todo)
- [de0fc1df-a948-4b44-95a4-4b9d01cab267](../../TOOLING/DECISIONS.md-HTML-comment-section-fences-are-imbalanced--de0fc1df/task.md) — DECISIONS.md HTML-comment section fences are imbalanced (6 BEGIN / 8 END) -- introduced b… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
