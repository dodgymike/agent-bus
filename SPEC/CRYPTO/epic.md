# EPIC CRYPTO — End-to-end message cryptography (dual keypairs, Double Ratchet, agent-side validation)

[← all epics](../../SPEC.md)

**10 open / 12 total.** Full records live in `SPEC/CRYPTO/<task>/task.md`.

_Relations (real)_ are authoritative edges from the Spec Server. _Referenced (derived)_ are guesses parsed out of description free text — useful, but not a dependency list.

## Open tasks (10)

| Key | Title | Status | Prio | Task | Relations (real) | Referenced (derived) |
| --- | --- | --- | --- | --- | --- | --- |
| CRYPTO-10 | CRYPTO-10: \`agent-bus verify\` helper + scripts/bus-*.sh validate-before-accept + AGENT_PR… | todo | P1 | [task.md](CRYPTO-10--68ff679d/task.md) | _not fetched_ | [AGENTIF-6](../AGENTIF/AGENTIF-6--31c1257c/task.md) [AGENTIF-5](../AGENTIF/AGENTIF-5--8109ab88/task.md) [SIGN-1](../SIGN/SIGN-1--43fd21ae/task.md) [CRYPTO-4](CRYPTO-4--13f3947e/task.md) [SIGN-4](../SIGN/SIGN-4--33fa35d8/task.md) [RATCHET-7](../RATCHET/RATCHET-7--aaa7cddc/task.md) +5 more |
| CRYPTO-3 | CRYPTO-3: Enrolment mints and registers the second (messaging) keypair, bound to the serv… | todo | P1 | [task.md](CRYPTO-3--dd1066af/task.md) | _not fetched_ | [AUTH-1](../AUTH/AUTH-1--54fa94c0/task.md) [SIGN-1](../SIGN/SIGN-1--43fd21ae/task.md) [SIGN-2](../SIGN/SIGN-2--1c183f10/task.md) [AUTH-3](../AUTH/AUTH-3--d53e3b21/task.md) [CRYPTO-4](CRYPTO-4--13f3947e/task.md) |
| CRYPTO-4 | CRYPTO-4: Key-distribution endpoint -- server-attested messaging key bundles | todo | P1 | [task.md](CRYPTO-4--13f3947e/task.md) | _not fetched_ | [AUTH-4](../AUTH/AUTH-4--a853261d/task.md) |
| CRYPTO-11 | CRYPTO-11: Audit-log content hash for signed (cleartext) messages -- implements the invar… | todo | P2 | [task.md](CRYPTO-11--0047e5b7/task.md) | _not fetched_ | [DUR-5](../DUR/DUR-5--a7123e88/task.md) [CRYPTO-1](CRYPTO-1--30570fb9/task.md) [SIGN-1](../SIGN/SIGN-1--43fd21ae/task.md) [SIGN-2](../SIGN/SIGN-2--1c183f10/task.md) [RATCHET-2](../RATCHET/RATCHET-2--ade31a62/task.md) |
| CRYPTO-12 | CRYPTO-12: PROTOCOL.md wire format + CONTRACTS.md for the crypto surface | todo | P3 | [task.md](CRYPTO-12--eb1827ff/task.md) | _not fetched_ | [SIGN-1](../SIGN/SIGN-1--43fd21ae/task.md) [CRYPTO-3](CRYPTO-3--dd1066af/task.md) [DOCS-2](../DOCS/DOCS-2--41c52cfa/task.md) [CRYPTO-4](CRYPTO-4--13f3947e/task.md) [SIGN-4](../SIGN/SIGN-4--33fa35d8/task.md) [DOCS-3](../DOCS/DOCS-3--a24bb214/task.md) +1 more |
| CRYPTO-5 | CRYPTO-5: X3DH session establishment between two agents | deferred | P3 | [task.md](CRYPTO-5--9f3f8065/task.md) | _not fetched_ | [CRYPTO-1](CRYPTO-1--30570fb9/task.md) [CRYPTO-2](CRYPTO-2--0ad37da2/task.md) [CRYPTO-4](CRYPTO-4--13f3947e/task.md) [CRYPTO-6](CRYPTO-6--260e6003/task.md) [CRYPTO-7](CRYPTO-7--f90d7889/task.md) |
| CRYPTO-6 | CRYPTO-6: Double Ratchet encrypt/decrypt on the direct-message send path | deferred | P3 | [task.md](CRYPTO-6--260e6003/task.md) | _not fetched_ | [CRYPTO-1](CRYPTO-1--30570fb9/task.md) [MSG-3](../MSG/MSG-3--2655c6ae/task.md) [CRYPTO-7](CRYPTO-7--f90d7889/task.md) [SIGN-2](../SIGN/SIGN-2--1c183f10/task.md) |
| CRYPTO-7 | CRYPTO-7: Ratchet-state durability and recovery (CRASH-INJECTION TEST REQUIRED) | deferred | P3 | [task.md](CRYPTO-7--f90d7889/task.md) | _not fetched_ | [CRYPTO-1](CRYPTO-1--30570fb9/task.md) [DUR-2](../DUR/DUR-2--4132b879/task.md) [DUR-3](../DUR/DUR-3--d8a991ea/task.md) [DUR-6](../DUR/DUR-6--d56a997d/task.md) [SIGN-4](../SIGN/SIGN-4--33fa35d8/task.md) |
| CRYPTO-8 | CRYPTO-8: Broadcast to N agents -- authenticated encryption for the fan-out path | deferred | P3 | [task.md](CRYPTO-8--2b1068eb/task.md) | _not fetched_ | [CRYPTO-1](CRYPTO-1--30570fb9/task.md) [MSG-2](../MSG/MSG-2--50995c75/task.md) [AUTH-4](../AUTH/AUTH-4--a853261d/task.md) [SIGN-3](../SIGN/SIGN-3--f2daa6bc/task.md) |
| CRYPTO-9 | CRYPTO-9: Cross-bus relay of encrypted messages -- what an intermediate bus can and canno… | deferred | P3 | [task.md](CRYPTO-9--0a4562fc/task.md) | _not fetched_ | [CRYPTO-1](CRYPTO-1--30570fb9/task.md) [RELAY-2](../RELAY/RELAY-2--654140d7/task.md) [RELAY-3](../RELAY/RELAY-3--e944edda/task.md) [SIGN-7](../SIGN/SIGN-7--aeb90793/task.md) |

## Closed tasks (2) — done, cancelled, superseded

| Key | Title | Status | Prio | Task | Relations (real) | Referenced (derived) |
| --- | --- | --- | --- | --- | --- | --- |
| CRYPTO-1 | CRYPTO-1: DESIGN SPIKE -- Signal-style E2E crypto for agent-bus (CRYPTO_DEEPDIVE.md + DEC… | done | P1 | [task.md](CRYPTO-1--30570fb9/task.md) | _not fetched_ | [CRYPTO-2](CRYPTO-2--0ad37da2/task.md) [CRYPTO-12](CRYPTO-12--eb1827ff/task.md) [DUR-5](../DUR/DUR-5--a7123e88/task.md) [CRYPTO-11](CRYPTO-11--0047e5b7/task.md) [CORE-1](../CORE/CORE-1--eea035e4/task.md) [RELAY-1](../RELAY/RELAY-1--9bc9d6c4/task.md) +4 more |
| CRYPTO-2 | CRYPTO-2: Adopt the crypto primitive layer chosen by the spike (internal/cryptobox + go.m… | superseded | P2 | [task.md](CRYPTO-2--0ad37da2/task.md) | _not fetched_ | [CRYPTO-1](CRYPTO-1--30570fb9/task.md) [CRYPTO-11](CRYPTO-11--0047e5b7/task.md) [DUR-5](../DUR/DUR-5--a7123e88/task.md) [CORE-1](../CORE/CORE-1--eea035e4/task.md) [SIGN-1](../SIGN/SIGN-1--43fd21ae/task.md) [SIGN-2](../SIGN/SIGN-2--1c183f10/task.md) |

## Epic description

User ask (2026-08-02, verbatim): "Add to the backlog to add a mechanism to validate messages in the agent script before accepting them. enrolment generates a pub/prv keypair for auth, and for messaging. Use the messaging ratchet library the signal people made for signal /whatsapp to ensure pfs and message integrity / authenticity between agents". Scope: (a) enrolment mints TWO keypairs per agent -- one for AUTH (extends invariant 3) and one for MESSAGING; (b) messages carry end-to-end authenticated encryption using the Signal design (X3DH key agreement + Double Ratchet) giving forward secrecy and per-message integrity/authenticity; (c) the agent-side scripts/bus-*.sh wrapper VALIDATES a received message before accepting it (invariant 7 -- shell cannot do X25519/AEAD, so a Go helper subcommand is required). This epic collides head-on with standing invariants 1, 3, 4, 5, 6, 7 and 8, so CRYPTO-1 is a DESIGN SPIKE and every implementation task is gated behind it. RESERVATION RULE: any on-disk record type number, wire protocol version, or epic task key this epic needs MUST be reserved via POST /api/v1/projects/agent-bus/reservations (namespace e.g. record-type / wire-version / task-key-CRYPTO) -- never chosen by hand. This epic sits BEHIND CORE, ID, DUR, AUTH and MSG, none of which are built yet; only CRYPTO-1 is near-term.

RESCOPE, user instruction verbatim (2026-08-02): "ok, let's keep it simple and just use standard message auth/integrity using libsodium. encryption can come later." This SUPERSEDES part (b) above -- NO X3DH, NO Double Ratchet, NO forward secrecy, NO encryption for now. Part (a) (two keypairs) and part (c) (agent-side validate-before-accept) STAND, rescoped to signing/verification only. CRYPTO-1/2/5/6/7/8/9 are superseded (encryption/ratchet-specific); CRYPTO-3/4/10/11/12 are kept and rescoped to a sign-only design. The bulk of the new implementation work now lives in the SIGN epic (canonical format, send-path signing, broadcast digest signing, replay/freshness, mandatory negative tests) -- see SIGN epic for the full task map. See also rescoped RATCHET-2/6/7.

---

_Generated by `scripts/gen-spec-mirror.sh`; never hand-edit._
