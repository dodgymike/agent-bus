# CLI-2: identity -- enrol, whoami, use, logout (ABSORBS AGENTIF-2; there is no bus-enrol.sh)

| Field | Value |
| --- | --- |
| Public id | `39318208-ee2c-4a0b-ab4b-e0b81ab63fa7` |
| Key | CLI-2 |
| Epic | [CLI](../epic.md) |
| Status | done |
| Priority | P0 |
| Component | cli |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T12:44:32.649624+00:00 |
| Updated | 2026-08-07T18:08:45.105105+00:00 |
| Completed | 2026-08-02T23:33:27.468915+00:00 |

## Proof command

```sh
go test -race -run 'TestEnrol|TestCLIEnrol' ./client/... ./cmd/agent-busctl/...
```

## Description

MERGED EPIC 2026-08-02. The CLI and AGENTIF epics are now ONE epic (user decision, DECISIONS.md
2026-08-02: "Merge the CLI and AGENTIF epics" / "the go cli should take the place of the .sh files and
be easy to use for a human + friendly for an agent to use or embed"). Invariant 7 is AMENDED, not
weakened: nobody hand-writes HTTP, but the vehicle is a CLI subcommand, not a scripts/bus-*.sh
wrapper. A feature without its CLI subcommand is still not done.

THREE AUDIENCES, and every subcommand serves all three: a HUMAN (readable output, remedial errors); an
AGENT SHELLING OUT (--json, stable exit codes, NO interactive prompts, NO TTY-dependent credential
input); an AGENT EMBEDDING (the reusable client package, which therefore CANNOT live under internal/).

**THIS TASK ABSORBS AGENTIF-2 ("scripts/bus-enrol.sh"), which is SUPERSEDED.** AGENTIF-2 was a P0
telling someone to write a shell wrapper; that is exactly the instruction the 2026-08-02 amendment
retires, and leaving it would have had two agents build two enrolment clients. There is ONE
enrolment client and it is this subcommand.

SCOPE -- identity, for humans AND agents, against the AUTH surface.
 - `enrol` -- generate the Ed25519 AUTH keypair locally, submit ONLY the public half, receive the
   SERVER-MINTED fully-qualified id `<bus-id>.<agent-id>` (invariant 1 -- the client never chooses
   its id), and store the credential.
 - SESSION HANDLING, per the 2026-08-02 auth decision: the client asks for a session, the SERVER
   provides the token value, the client SIGNS it with its enrolment private key, and the server
   verifies against the recorded public key. The session lasts AT MOST ONE HOUR and the client
   REFRESHES AT 75% OF LIFETIME (server expiry is authoritative; do not refresh at the boundary).
   Tokens are OPAQUE server-side handles, not signed claims. **SESSIONS DO NOT SURVIVE A SERVER
   RESTART** -- the CLI must re-authenticate transparently rather than surfacing a confusing failure.
 - `whoami`, `use` (switch identity/bus), `logout` (calls /v1/leave AND clears the local credential).
   **Revocation is IMMEDIATE** -- /leave invalidates outstanding sessions at once, not at expiry.
 - Credential storage under the user's config dir at 0600, NEVER in the repo, never world-readable.
   No interactive prompt and no TTY-dependent input -- an agent shelling out has no TTY.
 - A human is just another enrolled participant with no special server-side privilege.

DEPENDS ON: AUTH-1 (enrol, in flight), AUTH-2 (token middleware), AUTH-4 (leave/revocation), CLI-1.

PROOF. Unit tests plus a REAL end-to-end enrolment against a server brought up through
scripts/bus-serve.sh on an isolated run dir and port -- because "the subcommand is written" is not the
same as "an agent can enrol". FAILS TODAY by construction (neither the CLI nor /v1/enroll exists).
Do NOT complete this on the unit-test clause alone.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [AGENTIF-2](../../AGENTIF/AGENTIF-2--15e4509c/task.md) — AGENTIF-2: scripts/bus-enrol.sh + AGENT_PROTOCOL.md entry (superseded)
- [AUTH-1](../../AUTH/AUTH-1--54fa94c0/task.md) — AUTH-1: POST /v1/enroll -- signed credential issuance (done)
- [AUTH-2](../../AUTH/AUTH-2--4b45a6d8/task.md) — AUTH-2: Token verification middleware (done)
- [AUTH-4](../../AUTH/AUTH-4--a853261d/task.md) — AUTH-4: POST /v1/leave -- leave / revocation (todo)
- [CLI-1](../CLI-1--0495d133/task.md) — CLI-1: client package (NOT under internal/) + CLI subcommand skeleton -- the single clien… (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [932fe938-0e42-42d8-802d-ff018cb6c955](../../PROCESS/Audit-stored-proof_cmds-for-the-subtest-skip-vacuous-sha--932fe938/task.md) — Audit stored proof_cmds for the subtest-skip vacuous shape (parent-PASS/hidden-child-SKIP… (todo)
- [AGENTIF-2](../../AGENTIF/AGENTIF-2--15e4509c/task.md) — AGENTIF-2: scripts/bus-enrol.sh + AGENT_PROTOCOL.md entry (superseded)
- [AGENTIF-7](../../AGENTIF/AGENTIF-7--5cc8872d/task.md) — AGENTIF-7: scripts/bus-leave.sh + AGENT_PROTOCOL.md entry (superseded)
- [AUTH-1](../../AUTH/AUTH-1--54fa94c0/task.md) — AUTH-1: POST /v1/enroll -- signed credential issuance (done)
- [CLI-1](../CLI-1--0495d133/task.md) — CLI-1: client package (NOT under internal/) + CLI subcommand skeleton -- the single clien… (done)
- [CLI-10](../CLI-10--aba6e399/task.md) — CLI-10: Rewrite AGENT_PROTOCOL.md against CLI subcommands (it currently documents shell w… (todo)
- [CLI-2-FU-GITIGNORE](../CLI-2-FU-GITIGNORE--6fb7f295/task.md) — CLI-2-FU-GITIGNORE: Add the credential store to .gitignore (done)
- [CLI-2-FU-LEAVE](../CLI-2-FU-LEAVE--df79f84f/task.md) — CLI-2-FU-LEAVE: Add /v1/leave and make busctl logout actually revoke (todo)
- [CLI-2-FU-TLSSEAM](../CLI-2-FU-TLSSEAM--e4d60d97/task.md) — CLI-2-FU-TLSSEAM: The client transport is built before the identity is resolved (todo)
- [CLI-3](../CLI-3--6e70abe5/task.md) — CLI-3: watch -- long-poll tail, human-readable for a person and NDJSON for a pipe (replac… (done)
- [CLI-4](../CLI-4--137465b9/task.md) — CLI-4: send + broadcast, incl. stdin and interactive (replaces bus-send.sh and bus-broadc… (done)
- [CLI-5](../CLI-5--86dea094/task.md) — CLI-5: agents -- roster listing (replaces bus-agents.sh) (done)
- [CLI-7](../CLI-7--e600bde6/task.md) — CLI-7: peers -- relay topology and health (replaces bus-peer.sh) (todo)
- [CLI-8](../CLI-8--ae4caacc/task.md) — CLI-8: doctor -- diagnose a broken setup with a specific remedy per failure (todo)
- [CLI-BUSCTL-IMAGE](../CLI-BUSCTL-IMAGE--9be2105d/task.md) — CLI-BUSCTL-IMAGE: Ship the busctl binary in the container image (todo)
- [INVITE-CLIENT](../../INVITE/INVITE-CLIENT--4123e25d/task.md) — INVITE-CLIENT: the Go client/CLI redeems an invite at enrol (+ AGENT_PROTOCOL.md entry) -… (done)
- [MTLS-LISTENER](../../MTLS/MTLS-LISTENER--17e70a7e/task.md) — MTLS-LISTENER: serve TLS ONLY and REFUSE TO START without a usable cert/key -- there is n… (done)
- [fc8cd234-d275-43a1-9cb0-d10bca4a4086](../../PROCESS/Backfill-non-vacuous-proof_cmd-across-the-14-actionable--fc8cd234/task.md) — Backfill non-vacuous proof_cmd across the 14 actionable tasks that have none (CLI-1..9 +… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
