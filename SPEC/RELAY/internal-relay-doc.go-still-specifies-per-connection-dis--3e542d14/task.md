# internal/relay/doc.go still specifies per-connection disconnect on idempotency-key-reuse-with-different-payload, contradicting invariant 10 as narrowed 2026-08-08

| Field | Value |
| --- | --- |
| Public id | `3e542d14-81ea-4b86-8b95-a8ea6cfc4a79` |
| Key | _(null in the export)_ |
| Epic | [RELAY](../epic.md) |
| Status | in_progress |
| Priority | P2 |
| Component | relay |
| Section | backlog |
| Tags | spec-defect, invariant-10, doc-only |
| Created | 2026-08-08T10:22:17.911272+00:00 |
| Updated | 2026-08-08T11:35:07.370351+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run TestPackageDocDoesNotReviveTheWithdrawnDisconnect ./internal/relay
```

## Status note

Code complete; reviewer + security gates PASSED on re-verification; documentation done (CONTRACTS-ONDISK.md). Awaiting the orchestrator's coordinated commit -- no commit_sha yet. NOT live: internal/relay is not registered on any mux (gated behind INVITE-PEERGUARD f5d91dbe and MTLS-RELAYGUARD 8192c3c7), so this is code-only and nothing is observable from a running bus.

## Description

internal/relay/doc.go:246-250 (comment on RelayHandler, section "RELAY-2 and RELAY-3") reads:

  One more handoff MTLS-RELAYGUARD owns: invariant 10 requires that an
  idempotency key reused with a DIFFERENT payload is rejected, logged AND THE
  OFFENDING PEER DISCONNECTED. RelayHandler does the first two (409 plus a log
  line that says so); it cannot close a connection it does not own. The gate
  task must wire the disconnect.

This is now WRONG on the object-level fact, not just stale wording. Invariant 10 was
narrowed 2026-08-08 (code: commit 1c6c540, "Aim invariant 10's disconnect at the
replayer, not at the confused client"; contract: commit 0dbb025, CLAUDE.md). Same
idempotency key + DIFFERENT payload is reject-and-log ONLY -- it no longer disconnects,
on EITHER /v1/send or /v1/enroll, because the key is scoped to the caller's own agent
and reusing it is evidence of a confused client, not an attacker. The ONE case that
still disconnects is third-party replay of an already-accepted signed message
(sender-mismatch on checkSignedMint), which relay ingest has not built a path to yet.

WHY THIS IS WORSE THAN A STALE COMMENT, NOT JUST STALE. Relay ingest (RelayHandler)
is not yet built (the whole surface is gated behind INVITE-PEERGUARD f5d91dbe and
MTLS-RELAYGUARD 8192c3c7 and "NOT REGISTERED ON ANY MUX"). When someone DOES build it,
a peer bus legitimately presents a sender that is not the connection's principal, for MANY
AGENTS AT ONCE on one relay connection -- a peer relays traffic on behalf of its whole
local roster over one link. An implementer who inherits doc.go's literal instruction
("the gate task must wire the disconnect" on key-reuse) would either (a) wire a
same-payload-reuse disconnect that invariant 10 no longer wants at all, or worse,
(b) generalize the ALREADY-CORRECT third-party-replay disconnect to fire at the
connection level on this multi-tenant link, dropping EVERY agent behind that peer bus
simultaneously over one agent's buggy or malicious traffic. That is the exact
"abuse defence aimed at the wrong party" defect this project has hit four times
before (see 1c6c540's own commit message), one scale up: instead of disconnecting one
confused client, it would disconnect an entire federated bus's worth of agents.

THE TEST TO APPLY, per invariant 10 as narrowed (CLAUDE.md, and 0dbb025's own wording):
before wiring ANY disconnect, ask (1) can a merely BUGGY client reach this line, and
(2) does this connection carry only ONE principal's traffic? For relay ingest the
answer to (2) is NO -- one relay connection multiplexes an entire peer bus's roster --
which is precisely why a connection-level disconnect is the wrong mechanism here even
for the one case (third-party replay) that legitimately disconnects a single agent
elsewhere in the codebase. doc.go's own "Loop prevention is AVAILABILITY, never
security" section (lines 252-262) already reasons correctly in this direction for a
DIFFERENT mechanism (loop suppression); the idempotency paragraph two sections above it
does not yet carry the same care.

SCOPE: internal/relay/doc.go is a comment-only file (no registered handlers -- see the
file's own "NOT REGISTERED ON ANY MUX" banner), so this is a documentation/specification
fix, not a behavior change: rewrite lines 246-250 to state what invariant 10 actually
requires post-narrowing (reject+log only for key-reuse-with-different-payload; the
disconnect, if relay ever needs one, belongs to third-party replay of an accepted
signed message, and even that needs a scoping decision for a multi-principal
connection that plain per-socket disconnect does not answer). Do not invent that
scoping decision here -- name it as an open question for whoever builds RelayHandler
for real (MTLS-RELAYGUARD / RELAY-2), since a connection-level primitive is very
plausibly the wrong tool for a multi-tenant relay link and the actual mechanism
(e.g. per-origin-agent rejection without dropping the transport) needs its own design.

Rated P2: this is a specification defect in CODE THAT DOES NOT RUN YET (RelayHandler is
gated off any mux), not a live vulnerability -- do not inflate it. It earns urgency from
being read-and-trusted by whoever builds relay ingest next, not from being exploitable
today.

Cross-reference: 1c6c540's own commit message flags this exact file/lines as
"unreconciled" ("internal/relay/doc.go already specifies OFFENDING PEER DISCONNECTED...
the relay ingest path must reconcile with this narrowing rather than inherit it").
This task is that reconciliation, filed rather than left as a commit-message footnote.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


_None recorded._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [INVITE-PEERGUARD](../../INVITE/INVITE-PEERGUARD--f5d91dbe/task.md) — INVITE-PEERGUARD: no ungated peer/federation enrolment path may ever exist -- enumerate t… (todo)
- [MTLS-RELAYGUARD](../../MTLS/MTLS-RELAYGUARD--8192c3c7/task.md) — MTLS-RELAYGUARD: bus-to-bus relay links are mutually authenticated too -- acceptance crit… (todo)
- [RELAY-2](../RELAY-2--654140d7/task.md) — RELAY-2: Message relay + ongoing roster sync across peers (done)
- [RELAY-3](../RELAY-3--e944edda/task.md) — RELAY-3: Loop prevention via traversed-bus path (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [11d171c2-9f5a-427f-b4fe-cdbfc9f0ad48](../../IDEM/Stale-invariant-10-unconditional-disconnect-prose-WIDENE--11d171c2/task.md) — Stale invariant-10 unconditional-disconnect prose -- WIDENED 2026-08-08: 6 files, 14 site… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
