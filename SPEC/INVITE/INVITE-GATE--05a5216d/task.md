# INVITE-GATE: POST /v1/enroll REQUIRES a valid invite and fails closed; invite consumption and the roster write commit TOGETHER

| Field | Value |
| --- | --- |
| Public id | `05a5216d-097c-4279-8a27-a0fb9479542f` |
| Key | INVITE-GATE |
| Epic | [INVITE](../epic.md) |
| Status | done |
| Priority | P0 |
| Component | auth |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T21:12:48.414925+00:00 |
| Updated | 2026-08-14T16:09:05.604877+00:00 |
| Completed | 2026-08-14T16:09:05.604861+00:00 |

## Proof command

```sh
go test -race -run TestInviteRedemptionIsAtomicWithEnrolment ./internal/httpapi
```

## Status note

RESOLVED 2026-08-14 (spec-keeper, per coordinator ask during RELAY-25 closure computation): does the INVITE-CLIENT/INVITE-GATE separation hold -- i.e. can the smoke test need only INVITE-CLIENT while INVITE-GATE's security-posture deferral stands unharmed? NO, IT DOES NOT HOLD. Verified directly against source, not against task prose: internal/auth/service.go's EnrolRequest struct (line 223) has NO invite field at all -- only Name, PublicKey, MessagingPublicKey (+ IdempotencyKey) -- and Service.Enrol (line 340) contains zero invite validation/consumption logic. Repo-wide search for InviteSecret/ConsumeInvite/RedeemInvite/ValidateInvite touches only internal/invite/{store,record,id,errors}.go (minting/durable storage) and cmd/agent-bus/invite.go (the mint CLI) -- nothing wires an invite into the enrol path. INVITE-GATE's own description says exactly where that wiring lands: "internal/httpapi/auth.go:122 handleEnroll and internal/auth/service.go:276 Service.Enrol gain the gate." So today there is NO server-side code path that reads, validates or consumes a presented invite at enrol time -- that code is INVITE-GATE's to write, not incidental to it. INVITE-CLIENT alone (the CLI flag + secure-secret-handoff plumbing) has nothing on the server to redeem AGAINST until INVITE-GATE lands. INVITE-CLIENT's own DEPENDS ON: INVITE-GATE (already recorded in its description before this finding) is therefore functionally correct, not just a scheduling nicety. CONCLUSION: the deferral of INVITE-GATE ("security hardening, until end-to-end relay runs") is genuinely circular as suspected, and it LIFTS -- RELAY-25's closure needs INVITE-GATE, not only INVITE-CLIENT. Real blocks relations wired: INVITE-GATE -> INVITE-CLIENT -> RELAY-25 (confirmed live via GET .../relations on both tasks).

## Description

EPIC: 0b43393e-556b-409a-938a-846be2fb4a75 | DEPENDS ON: ENROL-SHAPE, INVITE-STORE, INVITE-MINT | BLOCKS: INVITE-HARDEN, INVITE-REVOKE, INVITE-CLIENT, INVITE-PEERGUARD

This is the epic's crux and the root fix for the pre-auth attack family. internal/httpapi/auth.go:122 handleEnroll and internal/auth/service.go:276 Service.Enrol gain the gate. THE CORRECTNESS CRUX: single-use consumption and the enrolment effect must land in the SAME two-phase transaction, or a crash between them either burns an invite with no agent or enrols an agent without spending the invite. SECOND CRUX (invariant 10): a legitimate retry carrying the same idempotency_key and the same payload must return the ORIGINAL result and must NOT consume the invite a second time; same key with a DIFFERENT payload stays a 409 + Connection: close. Must update CONTRACTS-HTTP.md -- in particular the "Known gaps" bullet at CONTRACTS-HTTP.md:172-186 which currently states enrolment is unauthenticated, and the POST /v1/enroll route rows at CONTRACTS-HTTP.md:14-17. BREAKING WIRE CHANGE -- escalated to the user; do not land before ENROL-SHAPE. RESIDUAL RISK TO DOCUMENT IN THE SAME TASK: until MTLS-LISTENER lands, the invite secret crosses the wire in CLEARTEXT; exposure is bounded only by the -listen 127.0.0.1:8080 loopback default, and the bus must not be exposed on a non-loopback interface until mTLS ships.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [0b43393e-556b-409a-938a-846be2fb4a75](../EPIC-invite-only-enrolment-the-root-fix-for-the-pre-auth--0b43393e/task.md) — EPIC: invite-only enrolment -- the root fix for the pre-auth attack family (needs planner… (superseded)
- [ENROL-SHAPE](../ENROL-SHAPE--8942c8c8/task.md) — ENROL-SHAPE: settle the FINAL /v1/enroll wire shape and auth.RosterEntry field set ONCE,… (done)
- [INVITE-CLIENT](../INVITE-CLIENT--4123e25d/task.md) — INVITE-CLIENT: the Go client/CLI redeems an invite at enrol (+ AGENT_PROTOCOL.md entry) -… (done)
- [INVITE-HARDEN](../INVITE-HARDEN--d250d0dd/task.md) — INVITE-HARDEN: constant-time invite-secret comparison and ONE indistinguishable failure r… (in_progress)
- [INVITE-MINT](../INVITE-MINT--1d0d0e60/task.md) — INVITE-MINT: an operator mints a single-use, expiring invite -- the server is authoritati… (done)
- [INVITE-PEERGUARD](../INVITE-PEERGUARD--f5d91dbe/task.md) — INVITE-PEERGUARD: no ungated peer/federation enrolment path may ever exist -- enumerate t… (todo)
- [INVITE-REVOKE](../INVITE-REVOKE--d9def083/task.md) — INVITE-REVOKE: durably revoke an un-redeemed invite, and state what revocation does to an… (todo)
- [INVITE-STORE](../INVITE-STORE--a9ef92de/task.md) — INVITE-STORE: durable single-use invite record (mint/lookup/consume/expire), recovered by… (done)
- [MTLS-LISTENER](../../MTLS/MTLS-LISTENER--17e70a7e/task.md) — MTLS-LISTENER: serve TLS ONLY and REFUSE TO START without a usable cert/key -- there is n… (done)
- [RELAY-25](../../RELAY/RELAY-25--10491a01/task.md) — RELAY-25: fed-smoke.sh: the epic's deliverable -- three-bus loopback federation smoke test (in_progress)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [0b43393e-556b-409a-938a-846be2fb4a75](../EPIC-invite-only-enrolment-the-root-fix-for-the-pre-auth--0b43393e/task.md) — EPIC: invite-only enrolment -- the root fix for the pre-auth attack family (needs planner… (superseded)
- [1c4d3dea-b4f6-4f68-b823-78bb76a6b5aa](../../AUTH/SEC-unauthenticated-enrol-permanently-bricks-the-roster--1c4d3dea/task.md) — SEC: unauthenticated enrol permanently bricks the roster -- 4096-cap fails closed forever… (in_progress)
- [4b51635d-336f-4f25-94c2-64c53578859d](../../AGENTIF/AGENT_PROTOCOL.md-is-missing-the-CLI-11-key-export-publi--4b51635d/task.md) — AGENT_PROTOCOL.md is missing the CLI-11 (key export-public) and CLI-6 (log) sections -- b… (todo)
- [ADMIN-1](../../ADMIN/ADMIN-1--db334b3c/task.md) — ADMIN-1: record the operator-console trust/transport/control rulings D1-D7 in DECISIONS.m… (blocked)
- [ADMIN-10](../../ADMIN/ADMIN-10--958d66e8/task.md) — ADMIN-10: online invite mint from the console (BLOCKED -- ruled out for now by D6; filed… (blocked)
- [ADMIN-11](../../ADMIN/ADMIN-11--07926508/task.md) — ADMIN-11: remove an agent from the console (BLOCKED on AUTH-4) (blocked)
- [ADMIN-2](../../ADMIN/ADMIN-2--786e0de1/task.md) — ADMIN-2: client.Info/Health/Discovery + \`agent-busctl status \[--json\]\`, shipped together… (todo)
- [ADMIN-3](../../ADMIN/ADMIN-3--76bfce36/task.md) — ADMIN-3: \`agent-busadm serve\` -- loopback-only console with a capability token and an emb… (todo)
- [ADMIN-4](../../ADMIN/ADMIN-4--e12b4149/task.md) — ADMIN-4: N buses from a config file, polled concurrently -- one hung bus must not stall t… (todo)
- [ADMIN-5](../../ADMIN/ADMIN-5--fc0b4a88/task.md) — ADMIN-5: roster + live flow view from the console's OWN long-poll (/v1/wait) -- metadata… (todo)
- [ADMIN-6](../../ADMIN/ADMIN-6--f92aa33f/task.md) — ADMIN-6: bounded, tail-tolerant STREAMING audit reader in internal/wal (no dir lock, torn… (todo)
- [ADMIN-7](../../ADMIN/ADMIN-7--2147523d/task.md) — ADMIN-7: audit view in the console, for a CO-LOCATED bus only (D5) (todo)
- [ADMIN-8](../../ADMIN/ADMIN-8--7f550309/task.md) — ADMIN-8: GET /v1/status -- authenticated, in-process counters, exhaustive field-set pin,… (todo)
- [ADMIN-9](../../ADMIN/ADMIN-9--8bb10db2/task.md) — ADMIN-9: the console enrols by redeeming an invite blob (BLOCKED on INVITE-GATE) (blocked)
- [ADMIN-C1](../../ADMIN/ADMIN-C1--9074f7f2/task.md) — ADMIN-C1: versioned control/telemetry schema in a new internal/adminctl -- unknown kinds… (todo)
- [ADMIN-C2](../../ADMIN/ADMIN-C2--d31d77ff/task.md) — ADMIN-C2: \`agent-busctl report\` -- the node reporter: allow-list check, refuse-with-reaso… (todo)
- [ADMIN-C3](../../ADMIN/ADMIN-C3--ca0653e3/task.md) — ADMIN-C3: console issues/renews telemetry leases and renders the stream -- A REFUSAL MUST… (todo)
- [AUTH-3-FU-ROSTERDOS-DOCS](../../AUTH/AUTH-3-FU-ROSTERDOS-DOCS--d5197abb/task.md) — AUTH-3-FU-ROSTERDOS-DOCS: extend session.go availability analysis (untargeted/unamplified… (todo)
- [AUTH-ROSTER-RECLAIM](../../AUTH/AUTH-ROSTER-RECLAIM--b418638c/task.md) — AUTH-ROSTER-RECLAIM: operator-side "agent-bus roster remove &lt;id&gt;" escape hatch -- filesys… (todo)
- [CLI-6](../../CLI/CLI-6--47001cb4/task.md) — CLI-6: log -- read the append-only audit log (metadata only; also absorbs the WAL-dumper… (done)
- [DISCOVERY-DOC](../../CORE/DISCOVERY-DOC--2d7ce37b/task.md) — DISCOVERY-DOC: self-describing unauthenticated discovery document so an agent with only a… (in_progress)
- [ENROL-SHAPE](../ENROL-SHAPE--8942c8c8/task.md) — ENROL-SHAPE: settle the FINAL /v1/enroll wire shape and auth.RosterEntry field set ONCE,… (done)
- [HANDOVER-CHECK](../../HANDOVER/HANDOVER-CHECK--0f909b6c/task.md) — HANDOVER-CHECK: one command that tells you the health of this repo, plus its recorded out… (todo)
- [HANDOVER-REGISTER](../../HANDOVER/HANDOVER-REGISTER--7fddae9d/task.md) — HANDOVER-REGISTER: KNOWN_ISSUES.md, the known-defect register (todo)
- [HANDOVER-RUNBOOK-DOC](../../HANDOVER/HANDOVER-RUNBOOK-DOC--a0e009e1/task.md) — HANDOVER-RUNBOOK-DOC: RUNBOOK.md narrates exactly what the smoke script does (todo)
- [HANDOVER-WIRED](../../HANDOVER/HANDOVER-WIRED--6d85978f/task.md) — HANDOVER-WIRED: assert and document which packages are present but not wired (todo)
- [IDEM-11-FU-FAIRSHARE-IDENTITIES](../../IDEM/IDEM-11-FU-FAIRSHARE-IDENTITIES--287ff78e/task.md) — IDEM-11-FU-FAIRSHARE-IDENTITIES: fair-share divisor is gameable by identity count, not fi… (todo)
- [INVITE-CLIENT](../INVITE-CLIENT--4123e25d/task.md) — INVITE-CLIENT: the Go client/CLI redeems an invite at enrol (+ AGENT_PROTOCOL.md entry) -… (done)
- [INVITE-FU-STORE-TEST-RED-ON-MAIN](../INVITE-FU-STORE-TEST-RED-ON-MAIN--fb7be1d6/task.md) — INVITE-FU-STORE-TEST-RED-ON-MAIN: TestInviteNotDurableIsRefused fails on a pristine HEAD (done)
- [INVITE-GATE-ENFORCE](../INVITE-GATE-ENFORCE--8297d7e2/task.md) — INVITE-GATE-ENFORCE: enforce invite-only enrolment (P0: anonymous roster exhaustion) (in_progress)
- [INVITE-GATE-ENFORCE-FU-DECISIONS](../INVITE-GATE-ENFORCE-FU-DECISIONS--a02d0684/task.md) — INVITE-GATE-ENFORCE-FU-DECISIONS: add dated DECISIONS.md entry superseding the invite_req… (todo)
- [INVITE-GATE-FU-SWEEPCOST](../INVITE-GATE-FU-SWEEPCOST--15880d66/task.md) — INVITE-GATE-FU-SWEEPCOST: invite.Store.Begin's O(n) sweep is now on an anonymous pre-auth… (todo)
- [INVITE-HARDEN](../INVITE-HARDEN--d250d0dd/task.md) — INVITE-HARDEN: constant-time invite-secret comparison and ONE indistinguishable failure r… (in_progress)
- [INVITE-MINT](../INVITE-MINT--1d0d0e60/task.md) — INVITE-MINT: an operator mints a single-use, expiring invite -- the server is authoritati… (done)
- [INVITE-PEERGUARD](../INVITE-PEERGUARD--f5d91dbe/task.md) — INVITE-PEERGUARD: no ungated peer/federation enrolment path may ever exist -- enumerate t… (todo)
- [INVITE-REVOKE](../INVITE-REVOKE--d9def083/task.md) — INVITE-REVOKE: durably revoke an un-redeemed invite, and state what revocation does to an… (todo)
- [INVITE-STORE](../INVITE-STORE--a9ef92de/task.md) — INVITE-STORE: durable single-use invite record (mint/lookup/consume/expire), recovered by… (done)
- [MTLS-BIND](../../MTLS/MTLS-BIND--b6378bda/task.md) — MTLS-BIND: enrolment binds the presenting client-cert fingerprint to the SERVER-MINTED ag… (in_progress)
- [RELAY-20](../../RELAY/RELAY-20--701dc54d/task.md) — RELAY-20: Mount /v1/peer/{enroll,relay,roster} behind a PEER principal (done)
- [RELAY-24](../../RELAY/RELAY-24--e303c624/task.md) — RELAY-24: Composition root: wire federation into cmd/agent-bus/main.go (done)
- [RELAY-24-BLOCKER-HUBINGEST-FU-AUDITHASH-DOC](../../RELAY/RELAY-24-BLOCKER-HUBINGEST-FU-AUDITHASH-DOC--7126f08b/task.md) — RELAY-24-BLOCKER-HUBINGEST-FU-AUDITHASH-DOC: Record the relayed audit content-hash decisi… (done)
- [RELAY-6](../../RELAY/RELAY-6--0f7275b9/task.md) — RELAY-6: Record the FEDERATION deployment assumptions (done)
- [de0fc1df-a948-4b44-95a4-4b9d01cab267](../../TOOLING/DECISIONS.md-HTML-comment-section-fences-are-imbalanced--de0fc1df/task.md) — DECISIONS.md HTML-comment section fences are imbalanced (6 BEGIN / 8 END) -- introduced b… (todo)
- [e109c867-fcd2-4ddc-bc4d-55779dc5f5e1](../../PROCESS/Spec-Server-PATCH-tasks-id-rejects-the-key-field-outrigh--e109c867/task.md) — Spec Server: PATCH /tasks/{id} rejects the key field outright (422 Unknown field) -- a ke… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
