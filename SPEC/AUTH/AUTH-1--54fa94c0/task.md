# AUTH-1: POST /v1/enroll -- signed credential issuance

| Field | Value |
| --- | --- |
| Public id | `54fa94c0-6ca3-459a-aaa2-a3ea047f97d9` |
| Key | AUTH-1 |
| Epic | [AUTH](../epic.md) |
| Status | done |
| Priority | P0 |
| Component | auth |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T09:05:47.865608+00:00 |
| Updated | 2026-08-02T19:13:41.320891+00:00 |
| Completed | 2026-08-02T19:13:41.320873+00:00 |

## Proof command

```sh
go test -race -run TestEnroll ./internal/auth
```

## Status note

dispatched by triage-20260802-authpath-r1

## Description

CORRECTED 2026-08-02 (spec-keeper) -- STATUS UNTOUCHED, a feature-runner is in flight. THREE PARTS OF
THIS TASK'S PREVIOUS TEXT WERE STALE AND HAVE BEEN REMOVED. They are listed here so nobody restores
them from an older copy.

 REMOVED (1) THE "OPEN QUESTION" ON BEARER-VS-PER-REQUEST SIGNING. It is ANSWERED, do not re-open it
 and do not spend a DECISIONS.md entry deciding it. The settled design (DECISIONS.md 2026-08-02):
 enrolment records the public key; then the CLIENT ASKS FOR A SESSION, THE **SERVER** PROVIDES THE
 TOKEN VALUE, AND THE CLIENT **SIGNS** IT with its enrolment private key; the server verifies against
 the recorded public key and thereafter accepts that session. Signing happens ONCE PER SESSION, NOT
 PER REQUEST, so the hot path (long-poll, send) is a cheap credential check. The token is
 server-provided so the client never chooses the value it signs -- a client-chosen challenge would
 allow pre-computation and prove far less.

 REMOVED (2) "THE SERVER SIGNS THE PUBLIC KEY + MINTED ID INTO THE CREDENTIAL USING A PERSISTED BUS
 SIGNING SECRET." THAT IS THE OLD DESIGN AND IT IS SUPERSEDED. **Tokens are OPAQUE SERVER-SIDE
 HANDLES, not signed claims** (decision, 2026-08-02). That is precisely what makes IMMEDIATE
 revocation possible -- a stateless signed claim cannot be revoked. So: DO NOT generate or persist a
 bus signing secret for credential issuance, and do not put claims in the token. The server keeps the
 session state; the token is a lookup key into it.

 REMOVED (3) THE CONSTRAINT MANDATING `scripts/bus-enrol.sh` + AGENTIF-2 IN THE SAME TASK. Invariant 7
 was AMENDED on 2026-08-02: the compiled Go CLI replaces the shell wrappers. AGENTIF-2 is SUPERSEDED
 and its work is CLI-2. There is no openssl-in-bash keypair requirement any more -- the CLI generates
 and stores the key. AUTH-1 is therefore SERVER-SIDE ONLY. The pairing rule itself survives in
 amended form: this endpoint is not "done" for an agent until CLI-2 ships, so keep them cross-linked.

WHAT AUTH-1 IS, AS IT NOW STANDS.

POST /v1/enroll. The agent submits a desired short name plus the PUBLIC half of a client-generated
Ed25519 AUTH keypair. This is an ASYMMETRIC keypair, not a shared secret -- a symmetric option
(HMAC over agent-id+key with a persisted bus secret) is NOT acceptable and must not be implemented:
the server must never hold material that would let it FORGE an agent's calls, only material that lets
it VERIFY them. Use stdlib `crypto/ed25519` (invariant 9: standard, audited, high-level sign/verify;
never assemble primitives). The agent holds the private key; the server stores only the public key
against the roster entry.

The server MINTS the agent id (invariant 1 -- ids are server-authoritative, never client-supplied;
ID-3 provides the `<bus-id>.<name>-<n>` minting). The roster entry binds the minted id to the
presented public key, so a caller cannot later present a different key under the same id.

THIS AUTH KEYPAIR IS DISTINCT FROM THE MESSAGING IDENTITY KEYPAIR minted in CRYPTO-3 -- two keypairs,
two purposes (authentication vs E2E message encryption), never conflated or reused. CRYPTO-3 depends
on this task for the roster/enrolment shape it extends.

DURABILITY -- AND THE DEPENDENCY THAT MAKES IT SHIPPABLE NOW. A client must never get a credential for
an enrolment that is not durable: the roster entry goes through the two-phase prepare->commit write
path (invariant 4). **THE PERSISTENCE ITSELF IS DELIVERED BY AUTH-3 (roster persistence & recovery),
NOT BY THIS TASK.** AUTH-1 therefore ships against an INJECTED PERSISTENCE INTERFACE -- define the
narrow interface AUTH-1 needs, take it as a dependency, and let AUTH-3 supply the durable
implementation. Do not inline a bespoke roster file here; do not block on AUTH-3 either.

NOTE FOR THE SESSION WORK (AUTH-2/AUTH-4, not this task, but the shape is decided): sessions last AT
MOST ONE HOUR; the client refreshes at 75% of lifetime; server-side expiry is authoritative and an
expired token is rejected even if the client believes otherwise; **SESSIONS DO NOT SURVIVE A SERVER
RESTART** (they are expired on restart, the CLI re-authenticates); and **REVOCATION IS IMMEDIATE** --
/v1/leave invalidates outstanding sessions at once, not at the <=1h boundary.

ACCEPTANCE CRITERION (RATCHET-7 fallout, verified first-hand by reading this box's stdlib source at
crypto/ed25519/ed25519.go under GOROOT): **ed25519.Verify PANICS -- it does not return false -- when
len(publicKey) != ed25519.PublicKeySize.** This is a remote DoS trap, and it is ASYMMETRIC with
malformed-signature handling (a bad signature safely returns false), so a call site that only checks
the signature looks correct and is not. The public key presented here is client-supplied, untrusted
input by definition. REQUIRED: length-check the presented public key against ed25519.PublicKeySize
BEFORE any ed25519.Verify call in this path, returning a normal validation error on mismatch, never
panicking. REQUIRED TEST: a negative test feeding a wrong-size public key and a nil/empty public key
through the enrolment path, asserting clean rejection rather than a panic. See the standalone
cross-cutting task (4eb903f8) tracking this trap across all Verify call sites (AUTH-1, CRYPTO-10,
SIGN-2, and any roster-reload-from-disk path).

IDEMPOTENCY (invariant 10): enrol carries a client-supplied idempotency key and is safe to retry --
same key + same payload returns the ORIGINAL result and must NOT mint a second id; same key +
different payload is a protocol violation. IDEM-13 owns the full treatment; do not design enrol in a
way that makes it impossible.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


_None recorded._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [4eb903f8-04cd-497c-ba4a-7eadceb65725](../../SIGN/SEC-ed25519.Verify-panics-on-wrong-size-public-key-remot--4eb903f8/task.md) — SEC: ed25519.Verify panics on wrong-size public key -- remote DoS across AUTH-1/CRYPTO-10… (todo)
- [AGENTIF-2](../../AGENTIF/AGENTIF-2--15e4509c/task.md) — AGENTIF-2: scripts/bus-enrol.sh + AGENT_PROTOCOL.md entry (superseded)
- [AUTH-2](../AUTH-2--4b45a6d8/task.md) — AUTH-2: Token verification middleware (done)
- [AUTH-3](../AUTH-3--d53e3b21/task.md) — AUTH-3: Roster persistence & recovery (done)
- [AUTH-4](../AUTH-4--a853261d/task.md) — AUTH-4: POST /v1/leave -- leave / revocation (done)
- [CLI-2](../../CLI/CLI-2--39318208/task.md) — CLI-2: identity -- enrol, whoami, use, logout (ABSORBS AGENTIF-2; there is no bus-enrol.s… (done)
- [CRYPTO-10](../../CRYPTO/CRYPTO-10--68ff679d/task.md) — CRYPTO-10: \`agent-bus verify\` helper + scripts/bus-*.sh validate-before-accept + AGENT_PR… (todo)
- [CRYPTO-3](../../CRYPTO/CRYPTO-3--dd1066af/task.md) — CRYPTO-3: Enrolment mints and registers the second (messaging) keypair, bound to the serv… (todo)
- [ID-3](../../ID/ID-3--745cce13/task.md) — ID-3: Agent id minting \`&lt;bus-id&gt;.&lt;name&gt;-&lt;n&gt;\` (done)
- [IDEM-13](../../IDEM/IDEM-13--a869264d/task.md) — IDEM-13: Idempotent enrol / leave / peer-enrol (todo)
- [RATCHET-7](../../RATCHET/RATCHET-7--aaa7cddc/task.md) — RATCHET-7: Choose and supply-chain-review the Ed25519 implementation (stdlib crypto/ed255… (done)
- [SIGN-2](../../SIGN/SIGN-2--1c183f10/task.md) — SIGN-2: Sign on the send path (Ed25519 detached signature travels with the message) (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [4eb903f8-04cd-497c-ba4a-7eadceb65725](../../SIGN/SEC-ed25519.Verify-panics-on-wrong-size-public-key-remot--4eb903f8/task.md) — SEC: ed25519.Verify panics on wrong-size public key -- remote DoS across AUTH-1/CRYPTO-10… (todo)
- [AUTH-1-FU-LISTENADDR](../AUTH-1-FU-LISTENADDR--c27f9439/task.md) — AUTH-1-FU-LISTENADDR: default listen address is :8080 (all interfaces) but DECISIONS.md s… (done)
- [AUTH-1-FU-PENDINGCAP](../AUTH-1-FU-PENDINGCAP--687ad8c9/task.md) — AUTH-1-FU-PENDINGCAP: MaxPendingPerAgent is a lockout primitive, not a defence -- rekey o… (done)
- [AUTH-1-FU-POPKEY](../AUTH-1-FU-POPKEY--6e3083b0/task.md) — AUTH-1-FU-POPKEY: enrolment does not prove possession of the enrolling private key (todo)
- [AUTH-1-FU-RATELIMIT](../AUTH-1-FU-RATELIMIT--42670f8b/task.md) — AUTH-1-FU-RATELIMIT: per-source rate limiting on the three unauthenticated auth routes (done)
- [AUTH-1-FU-SESSIONSCALE](../AUTH-1-FU-SESSIONSCALE--067b80cf/task.md) — AUTH-1-FU-SESSIONSCALE: session-table O(n) scans and refuse-not-evict policy cause CPU/lo… (todo)
- [AUTH-4](../AUTH-4--a853261d/task.md) — AUTH-4: POST /v1/leave -- leave / revocation (done)
- [CLI-2](../../CLI/CLI-2--39318208/task.md) — CLI-2: identity -- enrol, whoami, use, logout (ABSORBS AGENTIF-2; there is no bus-enrol.s… (done)
- [CORE-9](../../CORE/CORE-9--a1f74fcc/task.md) — CORE-9: Set IdleTimeout + MaxHeaderBytes on http.Server -- and deliberately leave Read/Wr… (done)
- [CRYPTO-1](../../CRYPTO/CRYPTO-1--30570fb9/task.md) — CRYPTO-1: DESIGN SPIKE -- Signal-style E2E crypto for agent-bus (CRYPTO_DEEPDIVE.md + DEC… (done)
- [CRYPTO-10](../../CRYPTO/CRYPTO-10--68ff679d/task.md) — CRYPTO-10: \`agent-bus verify\` helper + scripts/bus-*.sh validate-before-accept + AGENT_PR… (todo)
- [CRYPTO-3](../../CRYPTO/CRYPTO-3--dd1066af/task.md) — CRYPTO-3: Enrolment mints and registers the second (messaging) keypair, bound to the serv… (todo)
- [ID-3](../../ID/ID-3--745cce13/task.md) — ID-3: Agent id minting \`&lt;bus-id&gt;.&lt;name&gt;-&lt;n&gt;\` (done)
- [IDEM-1](../../IDEM/IDEM-1--3cac3349/task.md) — IDEM-1: The idempotency-key contract -- format, transport, scope, and which operations re… (superseded)
- [IDEM-13](../../IDEM/IDEM-13--a869264d/task.md) — IDEM-13: Idempotent enrol / leave / peer-enrol (todo)
- [IDEM-14](../../IDEM/IDEM-14--b0facce9/task.md) — IDEM-14: Idempotency violation path -- key reuse with a different payload rejects, logs a… (todo)
- [IDEM-2](../../IDEM/IDEM-2--1c6a5ef1/task.md) — IDEM-2: Durable applied-key store -- committed in the SAME two-phase transaction as the e… (superseded)
- [IDEM-3](../../IDEM/IDEM-3--e34f9c31/task.md) — IDEM-3: Bounded dedupe window -- retention policy, eviction, and the honest statement of… (superseded)
- [IDEM-4](../../IDEM/IDEM-4--d9c00d0d/task.md) — IDEM-4: Idempotent send and broadcast -- a legitimate retry returns the ORIGINAL result a… (superseded)
- [IDEM-5](../../IDEM/IDEM-5--9631dfcb/task.md) — IDEM-5: Same key + DIFFERENT payload is a protocol violation -- reject, log, and disconne… (superseded)
- [IDEM-6](../../IDEM/IDEM-6--208c4fb5/task.md) — IDEM-6: Idempotent enrol, leave, and peer-enrol (superseded)
- [IDEM-7](../../IDEM/IDEM-7--1c490a08/task.md) — IDEM-7: Exactly-once application on the relay path -- dedupe on the ORIGIN's identity, co… (superseded)
- [IDEM-8](../../IDEM/IDEM-8--d1ecfc75/task.md) — IDEM-8: Proof suite -- a retried send produces exactly one message, including across a cr… (superseded)
- [IDEM-9](../../IDEM/IDEM-9--b0dc4a12/task.md) — IDEM-9: Wrappers generate the key ONCE and reuse it on retry, + AGENT_PROTOCOL.md / PROTO… (superseded)
- [RATCHET-2](../../RATCHET/RATCHET-2--ade31a62/task.md) — RATCHET-2: Threat model -- what Ed25519 signing defends against, and explicitly what it d… (todo)
- [RATCHET-7](../../RATCHET/RATCHET-7--aaa7cddc/task.md) — RATCHET-7: Choose and supply-chain-review the Ed25519 implementation (stdlib crypto/ed255… (done)
- [SIGN-2](../../SIGN/SIGN-2--1c183f10/task.md) — SIGN-2: Sign on the send path (Ed25519 detached signature travels with the message) (todo)
- [SIGN-8](../../SIGN/SIGN-8--71ef73d5/task.md) — SIGN-8: Agent-side messaging key material -- \`agent-bus keygen\`, key file location/permis… (todo)
- [f505fb57-25ab-46e1-a7a1-2ca5787529ab](../Any-roster-reclamation-path-must-ship-a-bound-on-distinc--f505fb57/task.md) — Any roster-reclamation path must ship a bound on distinct agent names in the SAME change… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
