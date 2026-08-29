# ACK-12-FU-WATCH-CORRELATION-KEY: \`watch\` never exposes the origin message id, so a recipient cannot name the correlation key of a relayed message

| Field | Value |
| --- | --- |
| Public id | `f423959c-f86f-45a1-98e5-95ab3011db7a` |
| Key | _(null in the export)_ |
| Epic | [ACK](../epic.md) |
| Status | done |
| Priority | P0 |
| Component | ack |
| Section | backlog |
| Tags | — |
| Created | 2026-08-21T14:33:49.235639+00:00 |
| Updated | 2026-08-22T08:08:37.467578+00:00 |
| Completed | 2026-08-22T08:08:37.467562+00:00 |

## Proof command

```sh
bash scripts/proof-check.sh 'go test -race -run "^TestThreeBusEndToEndAckNack$" ./tests/e2e'
```

## Status note

code complete and fully gated at 2026-08-22, on top of HEAD 493450f, UNCOMMITTED in the main worktree (15 files staged). reviewer PASS (after one CHANGES-REQUIRED, fixed and re-verified PASS), security PASS (1 LOW deferred to follow-up). proof: bash scripts/proof-check.sh 'go test -race -run "^TestThreeBusEndToEndAckNack$" ./tests/e2e' -> verdict=PASS class=test exit=0 tests_run=9 top_level=1 skipped=0 failed=0, run in a clean overlay of HEAD. Awaiting integrator commit; supersedes the abandoned pre-ACK-5 attempt in worktree agent-ab12ab6599510a3d4, which must NOT also be landed.

## Description

`ACK-CONTRACT.md` §3 rules that the correlation key is the ORIGIN bus's server-minted message id, `<origin-bus>-<seq>`, reached through `store.Message.OriginID()`. But `cmd/agent-busctl/watch.go`'s `watchRecord` (~line 316) carries `message_id`, `seq`, `from`, `broadcast`, `to`, `bus_path`, `sent_at`, `size`, `content_sha256`, `timestamp_ms`, `signature`, `body`, `text` -- and NO origin/correlation id field.

For a relayed message the id on the recipient's stream is the DESTINATION bus's own minted id, not the origin's. MEASURED on the ACK-12 three-bus harness: origin id on A was `bus-zdqih2rygav3uzip-11`; the recipient-visible id on C was `bus-2jnyxyibpicviugs-9`. Different bus halves, different seq.

CONSEQUENCE: a recipient has NO WAY, through any compiled subcommand, to learn the correlation key of a message it received over a relay. Even once ACK-12-FU-DESTINATION-ROW writes rows on the destination, the recipient still cannot name the right one. This is invariant 7's "missing half of a feature": the capability has no CLI surface.

Fix shape (to be decided by the implementer): expose the origin/correlation id on the watch stream -- e.g. a `correlation_key` field on `watchRecord`, sourced from `store.Message.OriginID()` and NEVER re-spelled at the call site (`internal/hub/hub.go:2149` forbids re-spelling that branch). Ships with an `AGENT_PROTOCOL.md` entry in the same task per invariant 7.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **follow-up of** [ACK-12-FU-WATCH-CORRELATION-KEY-FU-OMITEMPTYDOC](../ACK-12-FU-WATCH-CORRELATION-KEY-FU-OMITEMPTYDOC--80056e00/task.md)
- **follow-up of** [ACK-5-FU-STALEWATCHCOMMENT](../ACK-5-FU-STALEWATCHCOMMENT--b5ffc730/task.md)
- **follow-up of** [ACK-12-FU-WATCH-CORRELATION-KEY-FU-ACKKEYINDENT](../ACK-12-FU-WATCH-CORRELATION-KEY-FU-ACKKEYINDENT--dcf87771/task.md)
- **relates to** [ACK-12-FU-DESTINATION-ROW](../ACK-12-FU-DESTINATION-ROW--7d564118/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [ACK-12](../ACK-12--17406b3a/task.md) — ACK-12: Three-bus end-to-end ACK/NACK smoke acceptance (done)
- [ACK-12-FU-DESTINATION-ROW](../ACK-12-FU-DESTINATION-ROW--7d564118/task.md) — ACK-12-FU-DESTINATION-ROW: a relayed message gets no ack row on the DESTINATION bus, so i… (done)
- [ACK-5](../ACK-5--5991ee1a/task.md) — ACK-5: Multi-hop relay ACK/NACK propagation and correlation (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [ACK-12-FU-DESTINATION-ROW](../ACK-12-FU-DESTINATION-ROW--7d564118/task.md) — ACK-12-FU-DESTINATION-ROW: a relayed message gets no ack row on the DESTINATION bus, so i… (done)
- [ACK-12-FU-WATCH-CORRELATION-KEY-FU-ACKKEYINDENT](../ACK-12-FU-WATCH-CORRELATION-KEY-FU-ACKKEYINDENT--dcf87771/task.md) — ACK-12-FU-WATCH-CORRELATION-KEY-FU-ACKKEYINDENT: render the human watch "ack key" line at… (todo)
- [ACK-12-FU-WATCH-CORRELATION-KEY-FU-EGRESSCOMMENT](../ACK-12-FU-WATCH-CORRELATION-KEY-FU-EGRESSCOMMENT--cc26db9c/task.md) — ACK-12-FU-WATCH-CORRELATION-KEY-FU-EGRESSCOMMENT: cmd/agent-bus/relayegress.go asserts no… (todo)
- [ACK-12-FU-WATCH-CORRELATION-KEY-FU-EXITEIGHTCOUNT](../ACK-12-FU-WATCH-CORRELATION-KEY-FU-EXITEIGHTCOUNT--a74dd477/task.md) — ACK-12-FU-WATCH-CORRELATION-KEY-FU-EXITEIGHTCOUNT: exit 8 unknown is four answers at once… (todo)
- [ACK-12-FU-WATCH-CORRELATION-KEY-FU-OMITEMPTYDOC](../ACK-12-FU-WATCH-CORRELATION-KEY-FU-OMITEMPTYDOC--80056e00/task.md) — ACK-12-FU-WATCH-CORRELATION-KEY-FU-OMITEMPTYDOC: correct the never-omitempty rationale to… (todo)
- [ACK-12-FU-WATCH-CORRELATION-KEY-FU-RELAYVERIFY](../ACK-12-FU-WATCH-CORRELATION-KEY-FU-RELAYVERIFY--7e23e90f/task.md) — ACK-12-FU-WATCH-CORRELATION-KEY-FU-RELAYVERIFY: client.Message.signingMessage feeds the L… (todo)
- [ACK-5-FU-STALEWATCHCOMMENT](../ACK-5-FU-STALEWATCHCOMMENT--b5ffc730/task.md) — ACK-5-FU-STALEWATCHCOMMENT: internal/hub/ack.go still says agent-busctl watch does not ex… (todo)
- [CLIENT-DOC-PHANTOM-VERIFYRECEIVED](../../UNASSIGNED/CLIENT-DOC-PHANTOM-VERIFYRECEIVED--4a975a81/task.md) — CLIENT-DOC-PHANTOM-VERIFYRECEIVED: two comments name verifyReceivedMessage as the only ca… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
