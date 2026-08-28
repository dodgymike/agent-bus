# Stale invariant-10 unconditional-disconnect prose -- WIDENED 2026-08-08: 6 files, 14 sites (was 5 files, messages.go:1175 covered separately by IDEM-14-FU-CLIENTTEXT)

| Field | Value |
| --- | --- |
| Public id | `11d171c2-9f5a-427f-b4fe-cdbfc9f0ad48` |
| Key | _(null in the export)_ |
| Epic | [IDEM](../epic.md) |
| Status | todo |
| Priority | P2 |
| Component | docs |
| Section | backlog |
| Tags | spec-defect, invariant-10, doc-only, stale-security-prose |
| Created | 2026-08-08T10:23:15.169441+00:00 |
| Updated | 2026-08-14T19:12:00.910334+00:00 |
| Completed | — |

## Proof command

```sh
bash -c '! grep -qF "and a disconnection" CONTRACTS-CLI.md && ! grep -qF "and disconnects" CONTRACTS-CLI.md && ! grep -qF "earns a 409 AND a disconnection" client/messages.go && ! grep -qF "protocol violation that disconnects the client" client/messages.go && ! grep -qF "answer to it is a disconnection" client/messages.go && ! grep -qF "DISCONNECTS the offending client" internal/auth/errors.go && ! grep -qF "bus punishes with a disconnect" client/store.go && ! grep -qF "and earning a disconnect" client/store.go && ! grep -qF "client DISCONNECTED" client/store.go && ! grep -qF "bus answers 409 and DISCONNECTS" client/store.go && ! grep -qF "refusal comes with a disconnection" client/enrol.go && ! grep -qF "violation that earns a disconnect" client/enrol.go && ! grep -qF "DISCONNECTS the client (invariant 10)" client/enrol.go && ! grep -qF "disconnects the client" cmd/agent-busctl/enrol.go'
```

## Status note

PROGRESS 2026-08-14: commit 54395b6 (INVITE-CLIENT-FU-PENDINGINVITE + -EXIT9) fixed both CONTRACTS-CLI.md sites as a byproduct -- this is the CONTRACTS-CLI.md contradicting invariant 10 in three places the coordinator separately flagged (bus claimed to disconnect on a 409 key conflict; narrowed 2026-08-08 to reject-and-log, internal/httpapi/auth.go:509 logs "rejected, and the connection is KEPT"; AGENT_PROTOCOL.md was already correct). Re-ran the full 14-site proof directly: now 10/14 fixed (up from 8/14), all four remaining stale sites are in client/messages.go (3) and internal/auth/errors.go (1). This task is the correct owner of that finding -- recorded here since the coordinator asked for it on whichever task owns invariant-10 doc conformance.

## Description

WIDENING of the original five-site sweep (spec-keeper, 2026-08-08), triggered by RELAY-13's reviewer finding the real count in RELAY-13's own client-half diff was EIGHT sites, not the original three named for that boundary. Verified all current sites by direct read/grep against HEAD/working-tree, since RELAY-13's ~950-line client diff shifted every line number this task's original proof_cmd was pinned to (the original AGENT_PROTOCOL.md:631-636 pin is now VACUOUS -- that text is already fixed and now lives at ~line 699-713, correctly narrowed. Confirmed by direct read: it now says 'does NOT disconnect you' and correctly scopes the one real disconnect case to signed-replay with a well-formed sender claim. AGENT_PROTOCOL.md is DONE and dropped from the remaining list).

REMAINING STALE (verified present at HEAD/working-tree 2026-08-08, content-matched rather than line-pinned to survive further drift):
- CONTRACTS-CLI.md:1107,1129 (line-shifted from the original :814/:836; same content -- 'and a disconnection' / 'and disconnects').
- client/messages.go:202,595,602 (line-shifted from the original :188/:1141-1148 region; NOTE the :1141-1148 region itself is now split -- part of it, around current :1283/:1317-1326, IS already correctly fixed by IDEM-14-FU-CLIENTTEXT and says 'does NOT disconnect'; do not re-break that half).
- internal/auth/errors.go -- 'DISCONNECTS the offending client' on ErrIdempotencyKeyReused's doc comment, unchanged from the original finding.
- client/store.go -- the two ORIGINALLY tracked sites (content-identical to the old :192/:344, now at :202/:380) PLUS TWO NEW instances introduced by RELAY-13's own diff: :371 ('a protocol violation that gets the client DISCONNECTED', on pendingEnrolment's type doc) and :1349 ('the bus answers 409 and DISCONNECTS', on ClaimEnrolment's doc, both discovered because RELAY-13 added ~950 lines to this file).
- client/enrol.go (WHOLE FILE NEW TO THIS TASK -- did not exist as a tracked site before, since RELAY-13's client half is what added the messaging-key plumbing to it): :192 ('the bus's refusal comes with a disconnection'), :291 ('is exactly the violation that earns a disconnect'), :584 ('a protocol violation that DISCONNECTS the client (invariant 10)'). NOTE :537 in the same file is ALREADY correct ('does NOT disconnect') -- do not touch it.
- cmd/agent-busctl/enrol.go:90 (WHOLE FILE NEW TO THIS TASK) -- 'the bus treats that as a protocol violation and disconnects the client'.

proof_cmd rewritten to check each of the 14 remaining stale phrases by CONTENT rather than line number, specifically to avoid the vacuous-on-drift failure mode this task's own reviewer already caught once on the AGENT_PROTOCOL.md segment. Confirmed RED today: all 14 phrase-checks fail (script exits 1) before any fix.

Cross-reference: 3e542d14 (internal/relay/doc.go) remains the sixth original location, from a different angle, tracked separately -- unaffected by this widening.

IDEM-14-FU-CLIENTTEXT (30a9e4f6) remains scoped to exactly client/messages.go's Remedy string (now the fixed :1283 region) -- still do not duplicate that scope; the messages.go sites THIS task now names (:202, :595, :602) are different lines with different content.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [3e542d14-81ea-4b86-8b95-a8ea6cfc4a79](../../RELAY/internal-relay-doc.go-still-specifies-per-connection-dis--3e542d14/task.md) — internal/relay/doc.go still specifies per-connection disconnect on idempotency-key-reuse-… (done)
- [IDEM-14-FU-CLIENTTEXT](../IDEM-14-FU-CLIENTTEXT--30a9e4f6/task.md) — IDEM-14-FU-CLIENTTEXT: client remedy text (messages.go:1175) asserts a server disconnect… (done)
- [INVITE-CLIENT-FU-PENDINGINVITE](../../INVITE/INVITE-CLIENT-FU-PENDINGINVITE--7bb6edf0/task.md) — INVITE-CLIENT-FU-PENDINGINVITE: pendingEnrolment does not record the invite id, so a mism… (done)
- [RELAY-13](../../RELAY/RELAY-13--97f3f1b4/task.md) — RELAY-13: Enrolment registers the agent's messaging public key (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
