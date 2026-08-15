# internal/relay/doc.go asserts relay ingest is structurally blocked (no CrossBusTrust implementation) -- false since RELAY-17 landed PeerStore

| Field | Value |
| --- | --- |
| Public id | `4988156c-26ba-4fe8-b871-e5922a0eb8cf` |
| Key | RELAY-FU-DOCGO-CROSSBUSTRUST-STALE |
| Epic | [RELAY](../epic.md) |
| Status | todo |
| Priority | P1 |
| Component | relay |
| Section | backlog |
| Tags | — |
| Created | 2026-08-14T18:14:29.504069+00:00 |
| Updated | 2026-08-14T18:14:29.504069+00:00 |
| Completed | — |

## Proof command

```sh
grep -qF 'no implementation of CrossBusTrust exists (RELAY-17 owns' internal/relay/doc.go && echo STILL_STALE || echo FIXED
```

## Description

Filed 2026-08-14, read directly at HEAD (not from a summary) after the coordinator's own re-read of gap 8's context turned up a bigger, older claim one paragraph above it. RELAY-17 is done; this task gives its now-unowned corrections an owner (see doc.go:156-157, which already says 'correcting the numbered list is RELAY-17's to do').

THREE STALE CLAIMS, ALL TRACED TO THE SAME ROOT CAUSE (RELAY-17 landing PeerStore as the real CrossBusTrust implementation), EACH VERIFIED INDIVIDUALLY AT HEAD, not assumed from one finding:

1. doc.go:148-150, THE MAIN BLOCKER PARAGRAPH -- FALSE. "A SEPARATE, NARROWER BLOCKER SITS ON RELAY INGEST SPECIFICALLY... no implementation of CrossBusTrust exists (RELAY-17 owns it), so every relayed message is ErrUnpeeredBus by construction." Verified false directly: internal/relay/trust.go:13 `var _ CrossBusTrust = (*PeerStore)(nil)` (compile-time assertion) and :23 `func (s *PeerStore) PinnedBusSigningKeys(...)` (a real method) both live in THIS SAME PACKAGE (trust.go:1 is `package relay`). This is the GOOD direction to be wrong in -- it means relay ingest has no hidden structural blocker beneath the six critical-path tasks already filed today (RELAY-FU-IDEM-METER-BY-PEER and siblings).

2. doc.go:295, checked on the coordinator's own explicit flag rather than guessed at -- PARTIALLY FALSE, split the sentence: "This package ships NO implementation of CrossBusTrust and no default, deliberately." The FIRST clause ("ships NO implementation of CrossBusTrust") is false for the identical reason as (1) -- PeerStore is the implementation, in this package. The SECOND clause ("and no default") is STILL PLAUSIBLY TRUE and is a DIFFERENT claim despite sharing words with the first, matching the coordinator's own hypothesis: trust.go:12 says "There is no permissive/default implementation" -- meaning no automatic/no-op fallback CrossBusTrust is wired anywhere (httpapi.Options carries no default), which is a deliberate, still-accurate design choice, not a gap. Do not delete the whole sentence; correct only the first clause.

3. doc.go:403-412, GAP 8's OPENING CLAIM AND CONCLUSION -- ALSO FALSE, and this is NEW: the annotation at doc.go:151-157 already corrects gap 8's stated CAUSE ("no bus signing key on the peering handshake"), but the paragraph's own CONCLUSION -- "CrossBusTrust.PinnedBusSigningKeys has NO SOURCE OF TRUTH... relay ingest cannot be served at all" -- was NOT re-checked and is ALSO false. Verified directly: PinnedBusSigningKeys (trust.go:23) returns "the active, operator-configured pins... from the durable peer trust table" -- RELAY-10 (f1a787c) landed exactly that source of truth as BusTrustRecord.SigningKeys, deliberately off the wire, which is the SAME correction doc.go already makes for gap 8's cause one paragraph earlier. The annotation fixed the cause but not the conclusion that cause was used to justify.

THE PATTERN WORTH RECORDING ONCE, because this is the FOURTH instance today of doc-drift costing a full extra round (after RELAY-13/RELAY-13-FU-DOCS, MTLS-CLIENTCERT, RELAY-21's 404 framing/PROTOCOL.md:1002/guards_test.go:912/peermount.go): doc.go is SELF-CORRECTING IN PLACE -- it carefully annotates gap 8's stale cause at :151-157, which makes the file READ as maintained -- while the paragraph immediately above that annotation (the main blocker claim, :148-150) carries a STALER and MORE CONSEQUENTIAL claim, left unannotated. A file that corrects itself locally can still be globally wrong, and the careful local annotation is exactly what makes that easy to miss on a skim.

SCOPE: correct all three claims. Remove or reframe the "separate, narrower blocker" paragraph at :148-150 to state RELAY-17 is done and PeerStore is the CrossBusTrust implementation. Correct :295's first clause only. Correct gap 8's conclusion at :410-412 to match its already-corrected cause, and update its opening summary line (:403-404, "RELAY INGEST THEREFORE CANNOT BE SERVED") to match. Do not soften the genuinely-still-open gaps this paragraph correctly lists elsewhere (roster BusID binding, bus-path last-hop check, etc, all separately filed today) -- this task is scoped to the CrossBusTrust-existence claims specifically.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


_None recorded._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [MTLS-CLIENTCERT](../../MTLS/MTLS-CLIENTCERT--0bc7a2eb/task.md) — MTLS-CLIENTCERT: the client generates and stores its own TLS keypair + self-signed certif… (in_progress)
- [RELAY-10](../RELAY-10--7e9a5b63/task.md) — RELAY-10: Durable peer records that survive restart (done)
- [RELAY-13](../RELAY-13--97f3f1b4/task.md) — RELAY-13: Enrolment registers the agent's messaging public key (done)
- [RELAY-13-FU-DOCS](../RELAY-13-FU-DOCS--7f3a4b80/task.md) — RELAY-13-FU-DOCS: three docs/comments assert the opposite of shipped RELAY-13 behaviour -… (done)
- [RELAY-17](../RELAY-17--817649ce/task.md) — RELAY-17: CrossBusTrust implementation + attestation travels in the relay envelope (done)
- [RELAY-21](../RELAY-21--f5ce883e/task.md) — RELAY-21: AcceptRelay callback: roster-check before durable write, re-forward on OutcomeN… (done)
- [RELAY-FU-IDEM-METER-BY-PEER](../RELAY-FU-IDEM-METER-BY-PEER--8774f265/task.md) — RELAY-FU-IDEM-METER-BY-PEER: Meter the applied-key table by the AUTHENTICATED PEER, not t… (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [RELAY-FU-DOCGO-GAP7-BACKOFF](../RELAY-FU-DOCGO-GAP7-BACKOFF--8aacfd4c/task.md) — internal/relay/doc.go gap 7: a fair-share or capacity refusal from AcceptRelay becomes a… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
