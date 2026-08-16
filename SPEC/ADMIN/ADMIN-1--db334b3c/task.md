# ADMIN-1: record the operator-console trust/transport/control rulings D1-D7 in DECISIONS.md, and name agent-busadm in the CONTRACTS.md index

| Field | Value |
| --- | --- |
| Public id | `db334b3c-50ca-432f-b3af-e0a2e699366d` |
| Key | ADMIN-1 |
| Epic | [ADMIN](../epic.md) |
| Status | blocked |
| Priority | P2 |
| Component | docs |
| Section | backlog |
| Tags | — |
| Created | 2026-08-08T10:55:23.121844+00:00 |
| Updated | 2026-08-08T10:59:33.143601+00:00 |
| Completed | — |

## Proof command

```sh
grep -Fq 'ADMIN: the operator console is LOCAL, CONSENT-BASED and READ-FIRST (D1-D7)' DECISIONS.md && test "$(grep -cE '^- \*\*D[1-7]\*\*' DECISIONS.md)" = 7 && grep -Fq 'cmd/agent-busadm' CONTRACTS.md
```

## Status note

BLOCKED on INVITE-GATE (05a5216d, P0 todo). UNBLOCK WHEN INVITE-GATE CLOSES; the epic's sequencing ruling is in the ADMIN epic description -- read it before clearing this flag. Same blocker and same reason as ADMIN-9. This task is docs-only and harmless in itself; what is being guarded is the SIGNAL. An agent that claims an ADMIN task reasonably concludes the epic is live, and the NEXT claim after this one is not docs-only -- it is a console holding fleet-wide credentials against a bus anyone can currently enrol against. The sequencing ruling was prose in an epic description, and claim-next cannot read prose: `blocked` is the mechanism that enforces it. Also lowered P1 -> P2 so nothing in the epic sits above the band the operator ruled. CLEARING THIS DOES NOT CLEAR THE EPIC: ADMIN-9 (INVITE-GATE), ADMIN-10 (ruled out by D6) and ADMIN-11 (AUTH-4) are blocked INDEPENDENTLY on their own blockers and must each be judged on their own.

## Description

DEPENDENCIES: none. BLOCKS EVERYTHING ELSE IN THE ADMIN EPIC -- no console code lands before the rulings are written down.

Write a dated DECISIONS.md section whose heading is EXACTLY (the proof pins this string, so do not paraphrase it):

    ## <date> -- ADMIN: the operator console is LOCAL, CONSENT-BASED and READ-FIRST (D1-D7)

and inside it, seven bullets that each BEGIN `- **D1**` ... `- **D7**` (the proof counts exactly 7 such lines):

- **D1** UI TRANSPORT: plaintext HTTP on loopback + a per-process capability token, strict `Origin` /
  `Sec-Fetch-Site` checks, plus a 0600 unix socket for non-browser access. Ruled EXPLICITLY, not defaulted:
  this is a console surface, not a bus surface, and TLS here would reintroduce the browser trust-store problem
  the architecture exists to eliminate. Record WHY this does not weaken invariant 11: invariant 11 governs bus
  surfaces (client and relay), and every bus connection this console makes is still pinned mutual TLS via
  `client/`.
- **D2** the reporter is an `agent-busctl report` subcommand, not a third binary.
- **D3** ADVISORY LEASE: bounded, expires, does NOT survive restart. Cost accepted -- if the console is down,
  telemetry stops; that is fail-closed on a control channel, and a stolen console credential buys an attacker
  only until the lease expires. Durable standing configuration was EXPLICITLY REJECTED (it would need a new
  durable record type, let a remote party permanently alter a node's behaviour, and make revocation a
  distributed problem).
- **D4** authorisation is a LOCAL ALLOW-LIST of admin agent ids per node. No new bus authority, no new route,
  no role tier.
- **D5** NO REMOTE AUDIT READING. Co-located filesystem read only: the audit log is the bus's complete social
  graph, and reading it stays an operator/filesystem capability exactly as invite-minting already is.
- **D6** NO ONLINE INVITE MINT. `agent-bus invite mint` takes the exclusive dirlock and needs the bus stopped;
  the console links to the command instead.
- **D7** the control-message schema gets its OWN reserved namespace, `admin-control-schema` (v1 reserved
  2026-08-08), so it can never be confused with `signing-format-version` or `ondisk-format-version`.

Also record the epic-wide constraints: relay must not be imported (`TestHandshakeHandlerIsNotWiredIntoAnyMux`
fails the build if any package outside `internal/relay` imports it), `/v1/broadcast` answers 501 so telemetry
must be DM, the telemetry cost warning (two round trips + a fsynced durable write + a permanent audit record +
a never-reusable sequence number per sample; ten nodes at 1/s is ~864k permanent records a day saying "fine" --
this bus is engineered never to lose a message, which makes it a poor telemetry sink), and the fact that the
console is a NEW concentration of privilege created by the request rather than by the implementation.

Then add `cmd/agent-busadm` to the CONTRACTS.md INDEX (CONTRACTS.md is an index only -- name the binary and say
which plane file will document its flags: CONTRACTS-CLI.md).

PROOF DISCIPLINE: the proof pins the exact heading text and counts the seven `- **Dn**` bullets rather than
grepping the word "admin" (an incidental match elsewhere in DECISIONS.md would green-light nothing). CONFIRM IT
IS RED BEFORE THE FIX and quote scripts/proof-check.sh's verdict; a doc proof never observed failing is not
evidence.

SEQUENCING (epic-wide, operator-decided): must not start before INVITE-GATE (05a5216d) lands -- enrolment is currently open, so this console would hold fleet-wide credentials against a bus anyone can enrol against.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [ADMIN-10](../ADMIN-10--958d66e8/task.md) — ADMIN-10: online invite mint from the console (BLOCKED -- ruled out for now by D6; filed… (blocked)
- [ADMIN-11](../ADMIN-11--07926508/task.md) — ADMIN-11: remove an agent from the console (BLOCKED on AUTH-4) (blocked)
- [ADMIN-9](../ADMIN-9--8bb10db2/task.md) — ADMIN-9: the console enrols by redeeming an invite blob (BLOCKED on INVITE-GATE) (blocked)
- [AUTH-4](../../AUTH/AUTH-4--a853261d/task.md) — AUTH-4: POST /v1/leave -- leave / revocation (todo)
- [INVITE-GATE](../../INVITE/INVITE-GATE--05a5216d/task.md) — INVITE-GATE: POST /v1/enroll REQUIRES a valid invite and fails closed; invite consumption… (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [ADMIN-2](../ADMIN-2--786e0de1/task.md) — ADMIN-2: client.Info/Health/Discovery + \`agent-busctl status \[--json\]\`, shipped together… (todo)
- [ADMIN-3](../ADMIN-3--76bfce36/task.md) — ADMIN-3: \`agent-busadm serve\` -- loopback-only console with a capability token and an emb… (todo)
- [ADMIN-6](../ADMIN-6--f92aa33f/task.md) — ADMIN-6: bounded, tail-tolerant STREAMING audit reader in internal/wal (no dir lock, torn… (todo)
- [ADMIN-C1](../ADMIN-C1--9074f7f2/task.md) — ADMIN-C1: versioned control/telemetry schema in a new internal/adminctl -- unknown kinds… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
