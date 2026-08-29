# AUTH-10-WIRING: wire the operator principal into cmd/agent-bus/main.go — until this lands, the CLI is unreachable and operator records are silently discarded at replay

| Field | Value |
| --- | --- |
| Public id | `b11ef24c-3791-456f-a45f-1223cce5b50b` |
| Key | AUTH-10-WIRING |
| Epic | [AUTH](../epic.md) |
| Status | done |
| Priority | P0 |
| Component | BE |
| Section | backlog |
| Tags | — |
| Created | 2026-08-16T14:25:29.698768+00:00 |
| Updated | 2026-08-22T22:00:09.603824+00:00 |
| Completed | 2026-08-22T22:00:09.603804+00:00 |

## Proof command

```sh
go test -race -timeout 600s -run 'TestOperatorRecordsSurviveServerRestart|TestOperatorSubcommandIsReachableFromArgv' ./cmd/agent-bus
```

## Status note

Code complete and gated (reviewer + security both re-verified at base 591355f). NOT committed - integrator must land it. Two landing conditions from the gates: (1) cmd/agent-bus/main.go's worktree copy also carries ACK-9's uncommitted `AckStatus: ackStore` hunk and will NOT build against HEAD alone - either land ACK-9's internal/httpapi files first/together, or apply the isolated 6-hunk patch that was verified to apply to HEAD and build; (2) AUTH-10-WIRING-DOCS must land with or before the commit - six published sites assert the exact opposite of what this change makes true.

## Description

AUTH-10 delivered the operator principal CODE-ONLY. `cmd/agent-bus/main.go` was outside the
implementing agent's file-ownership boundary (RELAY-48 held it concurrently), so TWO wiring gaps
remain and BOTH were confirmed by the reviewer and security gates.

## Gap 1 — the CLI is unreachable from argv (invariant 7)
`main.go` dispatches `invite`/`healthcheck`/`peer`/`key`/`log` on `os.Args[1]` and contains zero
occurrences of `operatorCommandName`. `agent-bus operator …` falls through to `parseFlags` and is
refused as an unexpected argument. **`operator revoke` is the only revocation mechanism in the
design, and it cannot currently be invoked.** CONTRACTS-CLI.md publishes the whole surface,
including an exit-code table as CONTRACT, with this caveat attached.

Add beside the identical `peerCommandName` block at main.go:219-221:

    if len(os.Args) > 1 && os.Args[1] == operatorCommandName {
        os.Exit(runOperatorCommand(os.Args[2:], os.Stdout, os.Stderr))
    }

## Gap 2 — operator records are SILENTLY discarded at server replay (invariant 6)
`main.go`'s applier map (main.go:569-573) does not register `auth.OperatorRecordKind`, and
`auth.MultiplexApplier.Apply` (internal/auth/inviteenrol.go:284-290) `return nil`s with NO log at
any level for a kind it does not own. So every operator record in the WAL is passed over at server
start without a word. That is the exact shape invariant 6 rates as the defect ("silent discard is
the actual defect, not discard itself"). It fails CLOSED today — the server ends with zero
operators — but a silently dropped REVOCATION is the fail-open direction.

Three lines, mirroring authRoster exactly:

    operatorRegistry := auth.NewOperatorRegistry(lg)          // beside authRoster, main.go:505
    appliers[auth.OperatorRecordKind] = operatorRegistry      // in the map literal, main.go:569-573
    if err := operatorRegistry.Attach(walLog); err != nil {   // beside authRoster.Attach, main.go:771
        return fmt.Errorf("attaching the operator registry: %w", err)
    }

The repo's own precedent for the gated case is `unreplayedPeerRecords`
(cmd/agent-bus/relaywiring.go:1691-1748), which counts and reports skipped kinds by name and number
rather than passing them over.

## Acceptance — this task is about OBSERVABLE behaviour, not code
- `agent-bus operator add` / `list` / `revoke` / `keygen` run against a REAL data directory through
  the COMPILED binary and return their documented exit codes and --json shapes.
- A server STARTED over a data dir containing operator records reports them recovered in its
  startup line, and a REVOKED operator is still revoked after the restart.
- proof_cmd must exercise the compiled binary, not only a Go test.

NOTE FOR WHOEVER PICKS THIS UP: a `task-key-AUTH` reservation currently returns 8, which collides
with the live AUTH-8 — the namespace was never seeded past the epic's max of 10. Use derived keys
for AUTH follow-ups until that is sorted, and do NOT raise `initial_value` (it is ignored once the
counter row exists and only burns numbers).

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocks** [AUTH-7](../AUTH-7--4ba67a7b/task.md)
- **blocks** [CONV-AUTHZ-ADMIN](../../CONV/CONV-AUTHZ-ADMIN--70dd573a/task.md)
- **blocks** [INVMINT-3](../../INVMINT/INVMINT-3--8555e659/task.md)
- **follow-up** [AUTH-10](../AUTH-10--37993b49/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [ACK-9](../../ACK/ACK-9--08f9987f/task.md) — ACK-9: Sender CLI/API acknowledgement status and observability (done)
- [AUTH-10](../AUTH-10--37993b49/task.md) — AUTH-10: An operator/admin principal -- the missing noun blocking AUTH-7, INVMINT and CON… (done)
- [AUTH-10-WIRING-DOCS](../AUTH-10-WIRING-DOCS--82724d91/task.md) — AUTH-10-WIRING-DOCS: six published sites still say the operator plane is unwired - correc… (done)
- [AUTH-8](../AUTH-8--b65948b7/task.md) — AUTH-8: DEEP DIVE — the balance between usability and security / abuse protection (done)
- [RELAY-48](../../RELAY/RELAY-48--9887b0eb/task.md) — RELAY-48: onward relay is NOT crash-safe -- a pending onward hop is durably ABANDONED at… (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [AUTH-10](../AUTH-10--37993b49/task.md) — AUTH-10: An operator/admin principal -- the missing noun blocking AUTH-7, INVMINT and CON… (done)
- [AUTH-10-WIRING-DOCS](../AUTH-10-WIRING-DOCS--82724d91/task.md) — AUTH-10-WIRING-DOCS: six published sites still say the operator plane is unwired - correc… (done)
- [b5089ddf-5a5a-41e0-8278-036f6a195e2a](../agent-bus-operator-list-mints-wal-mac.key-as-a-side-effe--b5089ddf/task.md) — agent-bus operator list mints wal-mac.key as a side effect of a read-only command (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
