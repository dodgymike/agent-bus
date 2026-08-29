# ACK-12-FU-DESTINATION-ROW: a relayed message gets no ack row on the DESTINATION bus, so its recipient can never ACK it

| Field | Value |
| --- | --- |
| Public id | `7d564118-e513-43b4-ba20-76e04dee48e6` |
| Key | _(null in the export)_ |
| Epic | [ACK](../epic.md) |
| Status | done |
| Priority | P0 |
| Component | ack |
| Section | backlog |
| Tags | — |
| Created | 2026-08-21T14:33:06.675594+00:00 |
| Updated | 2026-08-28T22:27:48.279133+00:00 |
| Completed | 2026-08-28T22:27:48.279116+00:00 |

## Proof command

```sh
go test -race -run "TestTransitAckResolvesAfterMessageBodyPruned|TestDestinationRowSurvivesRestart|TestDuplicateRelayedIngestOpensNoSecondRow" ./internal/hub
```

## Status note

REWORKED 2026-08-22 per coordinator direction: kept P0, kept open (not superseded). ACK-5 settled ackability; the remaining gap is that relayed/broadcast messages get no delivery-lifecycle row (hub.go:2197, condition includes relayed OR broadcast) so ack authorisation depends on message retention outliving ack.Retention (24h).

## Description

REWORKED 2026-08-22 (spec-keeper, on coordinator direction). ACK-5 landed and made a relayed message ackable on the receiving bus, so the original framing of this task ("a relayed message gets no ack row on the destination bus, so its recipient can never ACK it") is SETTLED and removed below. What remains open is narrower and still P0: the authorisation basis ACK-5 uses can expire before the ack lifecycle it is meant to support.

# What ACK-5 actually did (not a destination-side row)

`internal/hub/hub.go:2197` `recordAcceptance` still early-returns and writes no delivery-lifecycle row for either case its condition names: `if h.acks == nil || relayed || broadcast { return }` -- so a RELAYED message and a same-bus BROADCAST both get no row. (Correction: the original filing cited `internal/hub/relayingest.go:253` as setting `relayed: true` for the relayed path; verified 2026-08-22, that file and line do exist and are unchanged at HEAD 91939dd -- `relayed: true` is set there in the mint passed to `h.applyLocked`. That citation was accurate; it is retained, not removed.) Instead of writing a row, ACK-5 has `hub.transitAck` authorise the ack off the RETAINED MESSAGE itself and forward the outcome synchronously -- no new durable ack-lifecycle row is created for either the relayed or the broadcast case.

# The remaining question (this task's whole scope now)

Because authorisation is keyed off message retention rather than a dedicated ack-lifecycle row, a relayed (or broadcast) message's ack window is bounded by MESSAGE retention, not by `ack.Retention` (24h). If message retention is shorter, or expires first for any reason, the recipient loses the ability to ack a message whose ack lifecycle would otherwise still be live for up to 24h. Decide and implement ONE of:
(a) write a destination-side (or broadcast-side) delivery-lifecycle row after all, closing the gap `recordAcceptance` leaves open for both the `relayed` and `broadcast` arms; or
(b) bind the two retention windows so the authorisation basis (the retained message) cannot expire before `ack.Retention` does, for both arms.

Whichever is chosen needs a test that advances time PAST message retention while the ack row/window is still supposed to be live, and asserts the ack still resolves correctly (not merely that it currently fails to, which would just re-confirm the gap).

# Scope correction from the original filing

- The `broadcast` arm of `recordAcceptance`'s condition was not named in the original filing at all -- it is an equally uncovered case and is now explicitly in scope.
- `f423959c` (surfacing the origin message id to `watch` so a recipient can even name the row to ack) is a RELATED but separate prerequisite for ergonomics, not part of this task's scope. See the `relates` link.

DO NOT reopen the "can a relayed message be acked at all" question -- that is closed, proven by `tests/e2e/threebus_ack_test.go`'s `relayed_message_is_acked_on_the_receiving_bus_under_the_ORIGIN_id` and `recipient_ack_propagates_back_to_the_origin_bus`.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **follow-up of** [cf8feb8e-b845-43d7-bd5f-7b5e9074e4d2](../cmd-agent-busctl-ackstatus.go-correct-the-stale-P0-7d564--cf8feb8e/task.md)
- **relates to** [ACK-12-FU-WATCH-CORRELATION-KEY](../ACK-12-FU-WATCH-CORRELATION-KEY--f423959c/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [ACK-12-FU-WATCH-CORRELATION-KEY](../ACK-12-FU-WATCH-CORRELATION-KEY--f423959c/task.md) — ACK-12-FU-WATCH-CORRELATION-KEY: \`watch\` never exposes the origin message id, so a recipi… (done)
- [ACK-5](../ACK-5--5991ee1a/task.md) — ACK-5: Multi-hop relay ACK/NACK propagation and correlation (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [ACK-12-FU-WATCH-CORRELATION-KEY](../ACK-12-FU-WATCH-CORRELATION-KEY--f423959c/task.md) — ACK-12-FU-WATCH-CORRELATION-KEY: \`watch\` never exposes the origin message id, so a recipi… (done)
- [cf8feb8e-b845-43d7-bd5f-7b5e9074e4d2](../cmd-agent-busctl-ackstatus.go-correct-the-stale-P0-7d564--cf8feb8e/task.md) — cmd/agent-busctl/ackstatus.go: correct the stale "P0 7d564118 is CLOSED" comment (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
