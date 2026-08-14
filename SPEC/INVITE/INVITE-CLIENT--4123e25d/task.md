# INVITE-CLIENT: the Go client/CLI redeems an invite at enrol (+ AGENT_PROTOCOL.md entry) -- invariant 7's delivery vehicle is the CLI subcommand, NOT a bus-enrol.sh

| Field | Value |
| --- | --- |
| Public id | `4123e25d-4644-48e0-9ee1-e0a9a273fd8c` |
| Key | INVITE-CLIENT |
| Epic | [INVITE](../epic.md) |
| Status | todo |
| Priority | P1 |
| Component | agentif |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T21:12:49.134940+00:00 |
| Updated | 2026-08-14T11:41:59.529070+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run TestClientEnrolWithInvite ./client/... && grep -qi 'invite' AGENT_PROTOCOL.md && grep -qi 'invite' CONTRACTS-AGENT.md
```

## Status note

RESOLVED 2026-08-14 (spec-keeper, per coordinator ask during RELAY-25 closure computation): does the INVITE-CLIENT/INVITE-GATE separation hold -- i.e. can the smoke test need only INVITE-CLIENT while INVITE-GATE's security-posture deferral stands unharmed? NO, IT DOES NOT HOLD. Verified directly against source, not against task prose: internal/auth/service.go's EnrolRequest struct (line 223) has NO invite field at all -- only Name, PublicKey, MessagingPublicKey (+ IdempotencyKey) -- and Service.Enrol (line 340) contains zero invite validation/consumption logic. Repo-wide search for InviteSecret/ConsumeInvite/RedeemInvite/ValidateInvite touches only internal/invite/{store,record,id,errors}.go (minting/durable storage) and cmd/agent-bus/invite.go (the mint CLI) -- nothing wires an invite into the enrol path. INVITE-GATE's own description says exactly where that wiring lands: "internal/httpapi/auth.go:122 handleEnroll and internal/auth/service.go:276 Service.Enrol gain the gate." So today there is NO server-side code path that reads, validates or consumes a presented invite at enrol time -- that code is INVITE-GATE's to write, not incidental to it. INVITE-CLIENT alone (the CLI flag + secure-secret-handoff plumbing) has nothing on the server to redeem AGAINST until INVITE-GATE lands. INVITE-CLIENT's own DEPENDS ON: INVITE-GATE (already recorded in its description before this finding) is therefore functionally correct, not just a scheduling nicety. CONCLUSION: the deferral of INVITE-GATE ("security hardening, until end-to-end relay runs") is genuinely circular as suspected, and it LIFTS -- RELAY-25's closure needs INVITE-GATE, not only INVITE-CLIENT. Real blocks relations wired: INVITE-GATE -> INVITE-CLIENT -> RELAY-25 (confirmed live via GET .../relations on both tasks).

## Description

EPIC: 0b43393e-556b-409a-938a-846be2fb4a75 | DEPENDS ON: INVITE-GATE, CLI-1 (0495d133), CLI-2 (39318208) | BLOCKS: none

DECIDED AND RECORDED HERE so it is not re-litigated -- invite redemption reaches an agent as a flag on the existing CLI identity subcommand (agent-bus-cli enrol --invite <blob>), NOT as a new scripts/bus-*.sh wrapper. This is consistent with the 2026-08-02 amendment to invariant 7 (DECISIONS.md:605-637, "The Go CLI replaces the shell wrappers"), with CLI-2 (39318208) which absorbed enrolment, and with AGENTIF-2 (15e4509c) which is already superseded. DEPENDS ON CLI-1 (0495d133) and CLI-2 (39318208) -- neither exists yet; there is no client package and no second cmd binary today. CONTRADICTION TO RESOLVE BEFORE STARTING (flagged by the planner, who was boundary-blocked from editing CLI-*): CLI-2's recorded proof_cmd enrols with no invite and over http://, so it is invalidated by BOTH this task and MTLS-LISTENER.

SECURITY AMENDMENT (2026-08-14, from RELAY-25 preflight): the invite blob contains a bearer `invite_secret` and must not be forced through process arguments. Implement enrol input so the secret is accepted from a protected file/stdin/JSON blob path with an explicit non-argv contract; do not add `--invite-secret` or any flag whose value places the secret in argv, shell history, process listings, or `/proc`. If the existing `--invite <blob>` remains supported for compatibility, document the exposure and provide the secure stdin/file path as the required agent-facing path; prefer a file descriptor/stdin or a 0600 file, reject group/world-readable secret files, never echo/log the secret, and redact it from all errors. Add a test that inspects the spawned command's argv and confirms the secret is absent, plus a 0600-permission/refusal test and a compiled-CLI proof through the secure path. This is part of INVITE-CLIENT's credential-handling boundary, not a new HTTP route.

CLARIFICATION: the intended CLI surface is `agent-busctl enrol --invite-file <path>` (or the documented stdin sentinel), where argv contains only the pathname and never the bearer value. A 0600 file is the default secure handoff; stdin is the non-file alternative. `--invite <blob>` must not be the secure automation path if it places the blob/secret in argv.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocked by** [INVITE-GATE](../INVITE-GATE--05a5216d/task.md)
- **blocks** [RELAY-25](../../RELAY/RELAY-25--10491a01/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [0b43393e-556b-409a-938a-846be2fb4a75](../EPIC-invite-only-enrolment-the-root-fix-for-the-pre-auth--0b43393e/task.md) — EPIC: invite-only enrolment -- the root fix for the pre-auth attack family (needs planner… (superseded)
- [AGENTIF-2](../../AGENTIF/AGENTIF-2--15e4509c/task.md) — AGENTIF-2: scripts/bus-enrol.sh + AGENT_PROTOCOL.md entry (superseded)
- [CLI-1](../../CLI/CLI-1--0495d133/task.md) — CLI-1: client package (NOT under internal/) + CLI subcommand skeleton -- the single clien… (done)
- [CLI-2](../../CLI/CLI-2--39318208/task.md) — CLI-2: identity -- enrol, whoami, use, logout (ABSORBS AGENTIF-2; there is no bus-enrol.s… (done)
- [INVITE-GATE](../INVITE-GATE--05a5216d/task.md) — INVITE-GATE: POST /v1/enroll REQUIRES a valid invite and fails closed; invite consumption… (in_progress)
- [MTLS-LISTENER](../../MTLS/MTLS-LISTENER--17e70a7e/task.md) — MTLS-LISTENER: serve TLS ONLY and REFUSE TO START without a usable cert/key -- there is n… (in_progress)
- [RELAY-25](../../RELAY/RELAY-25--10491a01/task.md) — RELAY-25: fed-smoke.sh: the epic's deliverable -- three-bus loopback federation smoke test (in_progress)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [0b43393e-556b-409a-938a-846be2fb4a75](../EPIC-invite-only-enrolment-the-root-fix-for-the-pre-auth--0b43393e/task.md) — EPIC: invite-only enrolment -- the root fix for the pre-auth attack family (needs planner… (superseded)
- [INVITE-GATE](../INVITE-GATE--05a5216d/task.md) — INVITE-GATE: POST /v1/enroll REQUIRES a valid invite and fails closed; invite consumption… (in_progress)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
