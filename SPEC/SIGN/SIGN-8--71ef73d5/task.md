# SIGN-8: Agent-side messaging key material -- \`agent-bus keygen\`, key file location/permissions, bus-enrol.sh wiring, AGENT_PROTOCOL.md

| Field | Value |
| --- | --- |
| Public id | `71ef73d5-5625-44bb-959c-17b364200f4b` |
| Key | SIGN-8 |
| Epic | [SIGN](../epic.md) |
| Status | todo |
| Priority | P1 |
| Component | agentif |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T13:11:15.466536+00:00 |
| Updated | 2026-08-02T13:11:15.466536+00:00 |
| Completed | — |

## Proof command

```sh
scripts/bus-enrol.sh against a running throwaway bus creates a 0600 private key, registers the public half, and a SECOND run neither overwrites it nor silently re-keys ; go test -race -run TestKeyfilePerms ./internal/...
```

## Description

The AGENT-SIDE half of CRYPTO-3, which is server-side only (it registers a public key the agent must already have). Nobody owns generating and protecting the private half, and AGENTIF-2 (scripts/bus-enrol.sh) predates the whole signing decision and knows nothing about keys -- so as it stands an agent has no way to obtain a messaging identity. Invariant 7: agents never hand-write HTTP and never hand-roll key handling either; shell cannot do Ed25519, so add an `agent-bus keygen` subcommand to the same Go binary (crypto/ed25519.GenerateKey with crypto/rand -- invariant 9, no custom key derivation, no hand-rolled entropy) and have the wrapper shell out to it. DELIVER: (1) a documented default key location outside the repo, overridable by one env var, with the private key written 0600 inside a 0700 directory, created atomically; refuse to run -- loudly, non-zero exit -- if an existing key file is group- or world-readable (the same refusal CRYPTO-10 makes on the verify side, so the two agree). (2) The private key is NEVER printed to stdout, NEVER logged, and NEVER sent to the bus -- only the 32-byte public half goes over the wire, at enrolment. Add its path pattern to .gitignore (related: CORE-10, which notes the stop hook stages with `git add -A`; a messaging private key landing in a commit is the worst realistic outcome of this epic). (3) bus-enrol.sh generates the keypair if absent and registers the public half, and is IDEMPOTENT: a second run must NOT silently overwrite an existing private key. Silent re-keying is the dangerous failure -- it orphans the already-registered public key and trips every verifier's TOFU pin (CRYPTO-4) as if the bus were MITM-ing, so an accident becomes indistinguishable from an attack. Re-keying must be explicit and human-driven. (4) State plainly how this file differs from the AUTH credential from AUTH-1 (the bearer token that authenticates TO the bus): two files, two lifetimes, two purposes -- the token proves you to the bus, the messaging key proves you to your PEERS, and only the second one a compromised bus cannot forge. (5) AGENT_PROTOCOL.md entry ships IN THIS TASK (invariant 7); CONTRACTS.md gains the subcommand, the env var and the file path. Rotation and revocation are OUT of scope -- CRYPTO-4's key_epoch and AUTH-4 own them -- but say what re-enrolment does. Verify the way an agent would: through scripts/bus-enrol.sh against a running throwaway bus with its own data dir under /tmp, not hand-written curl.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocked by** [CRYPTO-3](../../CRYPTO/CRYPTO-3--dd1066af/task.md)
- **blocked by** [SIGN-1](../SIGN-1--43fd21ae/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [AGENTIF-2](../../AGENTIF/AGENTIF-2--15e4509c/task.md) — AGENTIF-2: scripts/bus-enrol.sh + AGENT_PROTOCOL.md entry (superseded)
- [AUTH-1](../../AUTH/AUTH-1--54fa94c0/task.md) — AUTH-1: POST /v1/enroll -- signed credential issuance (done)
- [AUTH-4](../../AUTH/AUTH-4--a853261d/task.md) — AUTH-4: POST /v1/leave -- leave / revocation (todo)
- [CORE-10](../../CORE/CORE-10--27ad23ef/task.md) — CORE-10: .gitignore has no secret patterns while the stop hook stages with \`git add -A\` (done)
- [CRYPTO-10](../../CRYPTO/CRYPTO-10--68ff679d/task.md) — CRYPTO-10: \`agent-bus verify\` helper + scripts/bus-*.sh validate-before-accept + AGENT_PR… (todo)
- [CRYPTO-3](../../CRYPTO/CRYPTO-3--dd1066af/task.md) — CRYPTO-3: Enrolment mints and registers the second (messaging) keypair, bound to the serv… (todo)
- [CRYPTO-4](../../CRYPTO/CRYPTO-4--13f3947e/task.md) — CRYPTO-4: Key-distribution endpoint -- server-attested messaging key bundles (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [RELAY-13-FU-KEYGEN](../../UNASSIGNED/RELAY-13-FU-KEYGEN--518b18c0/task.md) — RELAY-13-FU-KEYGEN: 3 error-message remedy strings name the nonexistent agent-busctl keyg… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
