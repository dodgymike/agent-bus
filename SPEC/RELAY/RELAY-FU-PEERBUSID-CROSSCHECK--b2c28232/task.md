# RELAY-FU-PEERBUSID-CROSSCHECK: invariant 11's PEER cross-check is documented but unimplemented -- httpapi.PeerBusIDFromContext has zero non-test consumers

| Field | Value |
| --- | --- |
| Public id | `b2c28232-4a33-4194-b38a-c0bc0b85a922` |
| Key | RELAY-FU-PEERBUSID-CROSSCHECK |
| Epic | [RELAY](../epic.md) |
| Status | todo |
| Priority | P0 |
| Component | httpapi |
| Section | backlog |
| Tags | — |
| Created | 2026-08-14T18:06:28.005942+00:00 |
| Updated | 2026-08-14T18:06:28.005942+00:00 |
| Completed | — |

## Proof command

```sh
grep -rn 'PeerBusIDFromContext' --include=*.go . | grep -v _test.go | grep -v 'func PeerBusIDFromContext' | wc -l | grep -qv '^0$'
```

## Description

Filed 2026-08-14. P0 #2 of four verified independently by the security gate against the watermark task (86c7d368), previously recorded only inside that task's notes.

THE GAP: httpapi.PeerBusIDFromContext exists and is populated once a peer connection is authenticated (RELAY-20's mount attaches it to the request context), but grep confirms it has ZERO non-test consumers anywhere in the tree -- nothing calls it to CROSS-CHECK the resolved peer identity against anything else the request carries.

NOW GROUNDED AGAINST internal/relay/doc.go, READ DIRECTLY AT HEAD (not from a summary): this general statement is the umbrella over THREE specific, individually-filed manifestations, all sharing one fix mechanism (compare httpapi.PeerBusIDFromContext(ctx) against a claimed field) and one structural constraint (the comparison can only live in the CALLBACK, at the wiring site, because internal/relay must never import internal/httpapi -- doc.go:393-396, confirmed by the coordinator): (a) doc.go gap 3 / RosterUpdate.BusID, filed as RELAY-FU-ROSTER-VERSION-BOUND; (b) doc.go gap 5's inbound twin / PeerEnrollRequest.BusID, filed as RELAY-FU-PEERENROLL-BUSID-BIND; (c) doc.go gap 6 / BusPath's last hop, already filed as RELAY-FU-BUSPATH-BIND-PEER (f6a9fad0, P2). Real relates relations wired to all three.

OPEN QUESTION, RECORDED RATHER THAN RESOLVED BY GUESS: it is not yet settled whether this task (P0 #2) is (i) purely the umbrella/tracking task for those three, closable once all three individually close, or (ii) covers additional cross-check surface none of the three touches (e.g. some OTHER consumer of PeerBusIDFromContext security had in mind beyond RosterUpdate/PeerEnrollRequest/BusPath). Resolve by reading the security gate's original finding text again once available, not by assumption now.

DISTINCT FROM MTLS-CROSSCHECK (2b2af075, verified status=todo at filing) -- SAY SO EXPLICITLY SO NOBODY MERGES THEM. MTLS-CROSSCHECK is the AGENT plane: rejecting a session token presented over a connection whose client certificate belongs to a different AGENT. This task (and the three it umbrellas) is the PEER plane: using the already-authenticated peer BUS identity to cross-check claims a peer request makes about itself. Same shape of defect, different principal type, different files, different tasks.

STRUCTURAL FACT FOR WHOEVER DISPATCHES THIS (settles a question asked twice, per the coordinator): the fix genuinely CANNOT be done inside internal/relay by construction, not by preference -- internal/httpapi already imports internal/relay (peermount.go:115) and guards_test.go forbids the reverse, so PeerBusIDFromContext is unreachable from internal/relay's own callback bodies without inverting the dependency or rewriting three callback signatures (AcceptPeer, AcceptRelay, RosterConfig.Apply) to take an explicit peerBusID parameter instead of reading it from context. doc.go itself argues for the explicit-parameter form over the context form, for the same reason security prefers it elsewhere in this session: a context value can be silently forgotten and still compile; a missing parameter will not build. This strengthens the case that this task's real fix is a REQUIREMENT of RELAY-24's own wiring (or a callback-signature change immediately upstream of it) rather than a separable blocker before it -- see RELAY-24's own record for the full judgement call.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocks** [RELAY-24](../RELAY-24--e303c624/task.md)
- **relates to** [RELAY-FU-BUSPATH-BIND-PEER](../RELAY-FU-BUSPATH-BIND-PEER--f6a9fad0/task.md)
- **relates to** [RELAY-FU-PEERENROLL-BUSID-BIND](../RELAY-FU-PEERENROLL-BUSID-BIND--12f39697/task.md)
- **relates to** [RELAY-FU-ROSTER-VERSION-BOUND](../RELAY-FU-ROSTER-VERSION-BOUND--b361d9e2/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [MTLS-CROSSCHECK](../../MTLS/MTLS-CROSSCHECK--2b2af075/task.md) — MTLS-CROSSCHECK: reject a session token presented over a connection whose client certific… (todo)
- [RELAY-20](../RELAY-20--701dc54d/task.md) — RELAY-20: Mount /v1/peer/{enroll,relay,roster} behind a PEER principal (done)
- [RELAY-24](../RELAY-24--e303c624/task.md) — RELAY-24: Composition root: wire federation into cmd/agent-bus/main.go (todo)
- [RELAY-FU-BUSPATH-BIND-PEER](../RELAY-FU-BUSPATH-BIND-PEER--f6a9fad0/task.md) — RELAY-FU-BUSPATH-BIND-PEER: Bind the arriving BusPath's last hop to the authenticated pee… (todo)
- [RELAY-FU-PEERENROLL-BUSID-BIND](../RELAY-FU-PEERENROLL-BUSID-BIND--12f39697/task.md) — internal/relay/doc.go gap 5 (inbound twin): peer B presenting its own valid certificate a… (todo)
- [RELAY-FU-ROSTER-VERSION-BOUND](../RELAY-FU-ROSTER-VERSION-BOUND--b361d9e2/task.md) — internal/relay/doc.go gap 3: RosterUpdate.BusID is not bound to the authenticated connect… (todo)
- [SIGN-1-FU-REORDER-WATERMARK](../../SIGN/SIGN-1-FU-REORDER-WATERMARK--86c7d368/task.md) — SIGN-1-FU-REORDER-WATERMARK: a late-arriving lower sequence is never delivered to a reade… (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [RELAY-24](../RELAY-24--e303c624/task.md) — RELAY-24: Composition root: wire federation into cmd/agent-bus/main.go (todo)
- [RELAY-FU-INGEST-RATELIMIT](../RELAY-FU-INGEST-RATELIMIT--e7c66d83/task.md) — RELAY-FU-INGEST-RATELIMIT: no rate limit, quota or concurrency cap of any kind on relayed… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
