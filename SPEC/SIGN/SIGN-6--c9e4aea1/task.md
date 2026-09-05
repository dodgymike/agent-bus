# SIGN-6: A signature is MANDATORY on the wire -- ingest policy and fail-closed handling of missing/malformed/unverifiable signatures

| Field | Value |
| --- | --- |
| Public id | `c9e4aea1-3eb8-4a75-9684-35cca569a0aa` |
| Key | SIGN-6 |
| Epic | [SIGN](../epic.md) |
| Status | todo |
| Priority | P1 |
| Component | crypto |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T13:11:15.006580+00:00 |
| Updated | 2026-08-14T11:25:06.381182+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run TestUnsignedRejected ./internal/httpapi ./internal/store ; scripts/bus-send.sh with the signature stripped is rejected by a running throwaway bus and leaves NO durable record
```

## Description

GATED on SIGN-1 (canonical bytes) and SIGN-2 (signing on send). SIGN-1..5 specify how to sign and how to verify; NOTHING yet specifies what the bus does with a message that is not signed, or what a recipient does with one that fails to verify. That gap is not cosmetic: if either side treats "no signature" as "unsigned but fine", an attacker strips the signature and the entire epic is theatre. THIS TASK CLOSES IT. (1) THE SIGNATURE FIELD IS REQUIRED, NOT OPTIONAL. There is no unsigned message type, no allow_unsigned flag, no --insecure escape hatch, no legacy path; if one is ever argued for it needs its own dated DECISIONS.md entry. (2) INGEST POLICY on POST /v1/send and /v1/broadcast (MSG-2/MSG-3): the bus does NOT verify authenticity -- it must not be trusted to police messages on behalf of senders it does not control (SIGN-2), and the trust decisions live with the recipient (CRYPTO-4 TOFU pins) -- but it DOES enforce, and reject 4xx on failure: signature present; signature exactly 64 bytes (Ed25519); the claimed sender equals the AUTHENTICATED caller (invariants 1 and 2 -- a client-asserted identity is input to validate, never an identity to trust, so no caller may inject a message attributed to another agent no matter how well-formed the signature looks). (3) A REJECTED MESSAGE MUST LEAVE NO TRACE: no WAL record, no audit-log entry beyond a rejection event, no delivery, no ack -- the mirror image of invariant 4. DECIDE AND DOCUMENT whether a rejected send consumes a sequence number: if it does, recipients see gaps and SIGN-4's cursor must tolerate them; if it does not, sequence minting must happen after validation. Pick one, say why, make SIGN-4 consistent. (4) RECEIVE PATH: GET /v1/wait and GET /v1/messages return the signature with every message so the recipient can verify (CRYPTO-10). Verification failure is FAIL-CLOSED -- the body is NEVER handed to the calling agent -- and LOUD: log message id, sender, and which check failed; never swallow it. (5) THE POISON-MESSAGE WEDGE, the subtle one: if a message that fails verification also blocks the recipient's cursor from advancing, one bad message wedges that agent FOREVER and a malicious bus gets a trivial denial of service. Recommended policy to specify and test: the cursor advances past the unverifiable message (it was durably delivered and cannot be un-sent), the body is discarded rather than delivered, and the event is recorded so the failure is visible. Whatever is chosen, prove the poller cannot be wedged. (6) Interacts with invariant 10 (IDEM epic): a rejection must not turn into a client retry loop that produces duplicates -- a rejection is terminal for that idempotency key, not a transient error. TESTS: unsigned send rejected with no durable record; 64-byte-length check (63 and 65 bytes both rejected); sender-mismatch rejected; relay ingest is subject to the SAME check (see SIGN-7 -- a relay path that skips it is the obvious backdoor); a recipient handed one unverifiable message still makes progress on the next good one.

RELAY-25 SECURITY AMENDMENT (2026-08-14): the receive-side requirement is production, not only a future helper. `client.Client.Read` / `agent-busctl watch` currently calls `validateBatch` but does not call `verifySignedMessage`; `client/wedge_test.go:TestReadDoesNotYetVerifyReceivedMessages` intentionally pins this gap. Before RELAY-25 can claim a real three-bus delivery proof, wire the fail-closed verification policy through the actual watch/read path: unsigned, malformed, wrong-key, tampered, unknown-sender, and replayed messages must never reach stdout/agent callbacks; the cursor must advance according to the poison-message policy and a rejection must be visible. Do not satisfy this with the seam-only wedge test. The compiled CLI path is mandatory under invariant 7. This task therefore BLOCKS RELAY-25 (10491a01) for its signed-delivery acceptance, and depends on CRYPTO-10 (68ff679d) for the verifier/key-resolution contract; SIGN-5 supplies the mandatory rejection matrix and remains a gate, not a substitute for production wiring.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocked by** [SIGN-1](../SIGN-1--43fd21ae/task.md)
- **blocks** [RELAY-25](../../RELAY/RELAY-25--10491a01/task.md)
- **relates to** [IDEM-4](../../IDEM/IDEM-4--d9c00d0d/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CRYPTO-10](../../CRYPTO/CRYPTO-10--68ff679d/task.md) — CRYPTO-10: \`agent-bus verify\` helper + scripts/bus-*.sh validate-before-accept + AGENT_PR… (todo)
- [CRYPTO-4](../../CRYPTO/CRYPTO-4--13f3947e/task.md) — CRYPTO-4: Key-distribution endpoint -- server-attested messaging key bundles (todo)
- [MSG-2](../../MSG/MSG-2--50995c75/task.md) — MSG-2: POST /v1/broadcast (done)
- [MSG-3](../../MSG/MSG-3--2655c6ae/task.md) — MSG-3: POST /v1/send -- direct message (done)
- [RELAY-25](../../RELAY/RELAY-25--10491a01/task.md) — RELAY-25: fed-smoke.sh: the epic's deliverable -- three-bus loopback federation smoke test (done)
- [SIGN-1](../SIGN-1--43fd21ae/task.md) — SIGN-1: Canonical signing format for messages (Ed25519 detached signatures) (done)
- [SIGN-2](../SIGN-2--1c183f10/task.md) — SIGN-2: Sign on the send path (Ed25519 detached signature travels with the message) (todo)
- [SIGN-4](../SIGN-4--33fa35d8/task.md) — SIGN-4: Replay/freshness -- enforced SERVER-SIDE at ingest, never by recipient-side seque… (todo)
- [SIGN-5](../SIGN-5--5cedc580/task.md) — SIGN-5: MANDATORY negative-test suite -- prove the verifier rejects everything it must (done)
- [SIGN-7](../SIGN-7--aeb90793/task.md) — SIGN-7: Cross-bus relay preserves the signed envelope byte-exact -- an intermediate bus c… (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CONV-CREATE-CLI](../../CONV/CONV-CREATE-CLI--627d20e0/task.md) — CONV-CREATE-CLI: mint a conversation -- HTTP route + agent-busctl subcommand + AGENT_PROT… (done)
- [CRYPTO-10](../../CRYPTO/CRYPTO-10--68ff679d/task.md) — CRYPTO-10: \`agent-bus verify\` helper + scripts/bus-*.sh validate-before-accept + AGENT_PR… (todo)
- [ID-2-WIRING-OBSERVER](../../ID/ID-2-WIRING-OBSERVER--c31f6999/task.md) — ID-2-WIRING-OBSERVER: wal offers EVERY prepare (committed, aborted AND dangling) to an ob… (todo)
- [IDEM-12](../../IDEM/IDEM-12--26dd5625/task.md) — IDEM-12: Idempotent send/broadcast -- retries return the original result, no new sequence… (in_progress)
- [IDEM-12-FU-BROADCAST](../../IDEM/IDEM-12-FU-BROADCAST--facdd241/task.md) — IDEM-12-FU-BROADCAST: broadcast idempotency is untestable until SIGN-3 defines a canonica… (todo)
- [IDEM-4](../../IDEM/IDEM-4--d9c00d0d/task.md) — IDEM-4: Idempotent send and broadcast -- a legitimate retry returns the ORIGINAL result a… (superseded)
- [RELAY-25](../../RELAY/RELAY-25--10491a01/task.md) — RELAY-25: fed-smoke.sh: the epic's deliverable -- three-bus loopback federation smoke test (done)
- [SIGN-7](../SIGN-7--aeb90793/task.md) — SIGN-7: Cross-bus relay preserves the signed envelope byte-exact -- an intermediate bus c… (done)
- [SWEEP-TWO-PASS-DISCIPLINE](../../PROCESS/SWEEP-TWO-PASS-DISCIPLINE--268a0c73/task.md) — SWEEP-TWO-PASS-DISCIPLINE: a contract change needs a MECHANISM sweep and a PROSE sweep, n… (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
