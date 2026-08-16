# CRYPTO-1: DESIGN SPIKE -- Signal-style E2E crypto for agent-bus (CRYPTO_DEEPDIVE.md + DECISIONS.md)

| Field | Value |
| --- | --- |
| Public id | `30570fb9-c5dd-40cd-b0f7-858a250d23b7` |
| Key | CRYPTO-1 |
| Epic | [CRYPTO](../epic.md) |
| Status | done |
| Priority | P1 |
| Component | crypto |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T09:41:19.512596+00:00 |
| Updated | 2026-08-02T14:10:52.955327+00:00 |
| Completed | 2026-08-02T13:01:39.211318+00:00 |

## Proof command

```sh
test -s CRYPTO_DEEPDIVE.md && grep -q 'Message auth/integrity only' DECISIONS.md
```

## Status note

Deliverable (CRYPTO_DEEPDIVE.md) completed and stands as historical background, but its recommendation (adopt go.mau.fi/libsignal, bump toolchain, full X3DH+Double Ratchet) is SUPERSEDED by user instruction (2026-08-02): keep it simple, standard sign/verify via libsodium-equivalent (crypto/ed25519). No further action on this task. See SIGN epic.

## Description

INVESTIGATION ONLY -- NO PRODUCTION CODE. Run this as deep-diver (+ planner for the resulting task ordering), model opus: this is judgment/design work where a wrong call is expensive. Throwaway spikes under /tmp to measure or prototype are fine; nothing lands in internal/ or cmd/ from this task.

DELIVERABLES: (1) CRYPTO_DEEPDIVE.md at the repo root, resolving every remaining tension below with a recommendation and its rationale; (2) a dated DECISIONS.md entry per decision taken (invariant 8 requires this for any third-party dependency, and several of these decisions WEAKEN a standing invariant, which CLAUDE.md says needs an explicit recorded decision); (3) a revised, ordered task list for CRYPTO-2..CRYPTO-12 handed to spec-keeper -- correct/split/supersede those tasks if the design says so rather than forcing the design to fit them.

USER ASK (verbatim, 2026-08-02): "Add to the backlog to add a mechanism to validate messages in the agent script before accepting them. enrolment generates a pub/prv keypair for auth, and for messaging. Use the messaging ratchet library the signal people made for signal /whatsapp to ensure pfs and message integrity / authenticity between agents".

TWO OF THE ORIGINAL SIX TENSIONS ARE NOW SETTLED BY DIRECT USER INSTRUCTION (2026-08-02) -- do not reopen them in the spike:

- TENSION 2 (audit log vs forward secrecy) IS CLOSED. User's words: "ok, log only metadata and routing info." The append-only audit log (invariant 6, DUR-5) records ONLY message id, sequence, sender, recipient(s), bus path traversed, timestamp, size, and a content hash -- never ciphertext, never plaintext. This resolves the plaintext-becomes-unwritable-under-PFS / ciphertext-is-dead-weight conflict by not storing content at all; the hash keeps the log probative without retention. See CRYPTO-11 (implements this) and DUR-5 (already amended to this shape). Nothing left for this spike to decide here.
- THE GO-VERSION HALF OF TENSION 1 IS ALSO SETTLED. User's words: "this bus is meant to run in a docker compose, so use the applicable version for the ratchet requirements." agent-bus ships as a container under Docker Compose; the CONTAINER's builder image pins the Go toolchain, NOT this workstation's ambient go1.19.4. CORE-1's go1.19 pin was an artifact of the dev box, not a permanent product constraint -- see CLAUDE.md's "Runtime target: Docker Compose" section. This does NOT close the rest of tension 1 below (library choice, roll-your-own vs import, invariant 8 dependency justification) -- only the "are we stuck on go1.19.4" sub-question is closed: no, choose on the merits and say what the container must pin.

THE TENSIONS THIS SPIKE MUST STILL RESOLVE:

1. DEPENDENCY + LIBRARY CHOICE (invariant 8: stdlib first; a third-party dep needs a DECISIONS.md justification). libsignal is Rust/Java/Swift; there is NO official Go binding. Realistic options: (a) an unofficial Go Double Ratchet port -- assess maintenance, audit status, correctness risk; (b) CGO against libsignal -- assess build/cross-compile/static-linking cost and that it drags a Rust toolchain into the build; (c) implement X3DH + Double Ratchet ourselves over stdlib-ish primitives. The go1.19.4-forces-a-third-party-module problem is GONE (see above) -- the container builder image can pin whatever Go version the chosen option needs (crypto/ecdh is go1.20+; a current libsignal-compatible stack may want newer still). State PLAINLY, as the spike's headline conclusion: (i) which option is chosen and why, (ii) the exact Go version the container's builder image must pin as a result, and (iii) that this is a version bump, not a dependency-growth license -- invariant 8 (stdlib first, third-party deps need a DECISIONS.md justification) is UNCHANGED and still governs whether golang.org/x/crypto or a Double Ratchet port gets pulled in. Also state the position on rolling our own crypto vs importing it. NOTE ON SEQUENCING: the actual go.mod/toolchain bump and container builder image are owned by the new DEPLOY epic's toolchain-bump task, which is explicitly sequenced to land AFTER the in-flight ID/DUR wave completes (that wave is building against go1.19 right now) -- this spike recommends the version, it does not perform the bump.

2. [CLOSED -- see above.]

3. RATCHET STATE vs DURABILITY (invariants 4 and 5: nothing acked before durable; disk is the truth, recovery replays the store). Double Ratchet state is MUTABLE PER-SESSION state that advances with every message -- it is emphatically NOT append-only. If ratchet state is LOST on crash the session breaks; if it is REPLAYED/rolled back on recovery you get KEY AND NONCE REUSE, which is a catastrophic AEAD failure, not a hiccup. Specify: where ratchet state lives, how it is written and fsynced relative to the two-phase message commit (does the state advance commit atomically with the message?), what recovery does with a message whose ratchet step was committed but whose send was not (and vice versa), how skipped/out-of-order message keys are stored and bounded, and how replay of the WAL is prevented from re-advancing or rewinding a ratchet. Name the crash-injection tests that would prove it. This is a first-class durability problem, not an afterthought.

4. BROADCAST AND RELAY. The Double Ratchet is strictly PAIRWISE. agent-bus does BROADCAST to N agents and CROSS-BUS RELAY. Signal solves groups with Sender Keys, which have DIFFERENT and WEAKER PFS properties than the pairwise ratchet. Specify how a broadcast is authenticated and encrypted (pairwise fan-out with N ciphertexts vs sender-key group session -- state the cost/PFS trade-off and how membership change forces a rekey), and specify for relay exactly what an INTERMEDIATE relaying bus can and cannot see: envelope metadata, routing path, sender and recipient fully-qualified ids, sizes and timing are presumably visible; content must not be. Cross-reference the RELAY epic (RELAY-1..5) and MSG-2 (broadcast).

5. IDS, ENROLMENT AND KEY TRUST (invariant 1: server authoritative on every id; invariant 2: ids are fully qualified <bus-id>.<agent-id>; invariant 3: enrolment issues a signed credential). Enrolment ALREADY issues a signed credential (AUTH-1); this adds a SECOND, messaging keypair. Define: which key signs what (auth key authenticates to the bus; messaging identity key signs prekeys and authenticates peers -- do not conflate them), that the server BINDS the messaging public key to the server-minted fully-qualified <bus-id>.<agent-id> so a client can never assert its own identity, and -- the crux -- HOW AN AGENT FETCHES AND TRUSTS ANOTHER AGENT'S MESSAGING PUBLIC KEY: server-attested (the bus signs the key bundle, so the bus is a trusted introducer and a malicious bus can MITM) vs trust-on-first-use with a safety number/fingerprint an agent can compare (and what changing keys must do to an established session). State the residual threat model plainly: what does a compromised bus get, what does a compromised peer get, what does an offline attacker with the WAL get. Note the AUTH epic OVERLAPS -- cross-reference AUTH-1/AUTH-2/AUTH-3 rather than duplicating them, and say which AUTH tasks need their descriptions amended.

6. AGENT-SIDE VALIDATION IN THE WRAPPER (invariant 7: agents never hand-write HTTP; every capability ships its scripts/bus-*.sh wrapper AND its AGENT_PROTOCOL.md entry in the SAME task). The user explicitly asked that the AGENT SCRIPT validate a message BEFORE ACCEPTING it. Shell cannot do X25519/AEAD, so the wrapper must shell out to a helper -- specify it (e.g. an `agent-bus verify` / `agent-bus open` subcommand of the same Go binary), its stdin/stdout contract, its exit codes, where the agent's private keys live on disk and with what permissions, and what 'reject' looks like to the calling agent (non-zero exit + nothing printed to stdout, so a naive wrapper cannot accidentally pass unverified content through). Cover the failure modes: bad MAC, unknown sender, no session, out-of-order/skipped, replayed message, and key-changed-since-last-seen.

RESERVATIONS: Any on-disk record type number or wire protocol version this needs MUST be reserved via POST /api/v1/projects/agent-bus/reservations -- never hand-picked. The spike should ENUMERATE which record types and which wire protocol version bumps this design will need, so they can be reserved before implementation starts.

OUT OF SCOPE: writing any of the implementation. CRYPTO-2..CRYPTO-12 carry that.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [AUTH-1](../../AUTH/AUTH-1--54fa94c0/task.md) — AUTH-1: POST /v1/enroll -- signed credential issuance (done)
- [AUTH-2](../../AUTH/AUTH-2--4b45a6d8/task.md) — AUTH-2: Token verification middleware (done)
- [AUTH-3](../../AUTH/AUTH-3--d53e3b21/task.md) — AUTH-3: Roster persistence & recovery (done)
- [CORE-1](../../CORE/CORE-1--eea035e4/task.md) — CORE-1: Repo skeleton: go.mod, internal/ package layout, .gitignore (done)
- [CRYPTO-11](../CRYPTO-11--0047e5b7/task.md) — CRYPTO-11: Audit-log content hash for signed (cleartext) messages -- implements the invar… (todo)
- [CRYPTO-12](../CRYPTO-12--eb1827ff/task.md) — CRYPTO-12: PROTOCOL.md wire format + CONTRACTS.md for the crypto surface (todo)
- [CRYPTO-2](../CRYPTO-2--0ad37da2/task.md) — CRYPTO-2: Adopt the crypto primitive layer chosen by the spike (internal/cryptobox + go.m… (superseded)
- [DUR-5](../../DUR/DUR-5--a7123e88/task.md) — DUR-5: Append-only message audit log (done)
- [MSG-2](../../MSG/MSG-2--50995c75/task.md) — MSG-2: POST /v1/broadcast (done)
- [RELAY-1](../../RELAY/RELAY-1--9bc9d6c4/task.md) — RELAY-1: Peer enrolment + initial agent-list exchange (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CRYPTO-11](../CRYPTO-11--0047e5b7/task.md) — CRYPTO-11: Audit-log content hash for signed (cleartext) messages -- implements the invar… (todo)
- [CRYPTO-2](../CRYPTO-2--0ad37da2/task.md) — CRYPTO-2: Adopt the crypto primitive layer chosen by the spike (internal/cryptobox + go.m… (superseded)
- [CRYPTO-5](../CRYPTO-5--9f3f8065/task.md) — CRYPTO-5: X3DH session establishment between two agents (deferred)
- [CRYPTO-6](../CRYPTO-6--260e6003/task.md) — CRYPTO-6: Double Ratchet encrypt/decrypt on the direct-message send path (deferred)
- [CRYPTO-7](../CRYPTO-7--f90d7889/task.md) — CRYPTO-7: Ratchet-state durability and recovery (CRASH-INJECTION TEST REQUIRED) (deferred)
- [CRYPTO-8](../CRYPTO-8--2b1068eb/task.md) — CRYPTO-8: Broadcast to N agents -- authenticated encryption for the fan-out path (deferred)
- [CRYPTO-9](../CRYPTO-9--0a4562fc/task.md) — CRYPTO-9: Cross-bus relay of encrypted messages -- what an intermediate bus can and canno… (deferred)
- [RATCHET-2](../../RATCHET/RATCHET-2--ade31a62/task.md) — RATCHET-2: Threat model -- what Ed25519 signing defends against, and explicitly what it d… (todo)
- [RATCHET-8](../../RATCHET/RATCHET-8--9a404c64/task.md) — RATCHET-8: Record the decision, then gate the CRYPTO epic on it (superseded)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
