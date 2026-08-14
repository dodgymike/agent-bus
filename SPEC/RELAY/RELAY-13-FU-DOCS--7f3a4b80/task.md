# RELAY-13-FU-DOCS: three docs/comments assert the opposite of shipped RELAY-13 behaviour -- BLOCKS marking RELAY-13 done

| Field | Value |
| --- | --- |
| Public id | `7f3a4b80-a043-433e-8f65-44400a57b39b` |
| Key | _(null in the export)_ |
| Epic | [RELAY](../epic.md) |
| Status | todo |
| Priority | P1 |
| Component | docs |
| Section | backlog |
| Tags | relay-13, doc-defect, blocks-done |
| Created | 2026-08-08T19:53:23.838687+00:00 |
| Updated | 2026-08-14T12:34:26.515460+00:00 |
| Completed | — |

## Proof command

```sh
bash -c 'set -e; ! grep -n "not registered at enrolment" AGENT_PROTOCOL.md; ! grep -n "on first use, lazily" CONTRACTS-CLI.md | grep -qi messaging; ! grep -n "no messaging key is registered at enrolment" client/client.go; grep -q messaging_public_key CONTRACTS-CLI.md' # each false/missing claim must be gone; RED today (all four match/are missing)
```

## Status note

DELIBERATELY HELD, not neglected (coordinator, 2026-08-14): unowned since github-copilot-1 died mid-task, but NOT dispatched to a replacement yet on purpose. Its two edit targets -- AGENT_PROTOCOL.md:549 and CONTRACTS-CLI.md:1070/~912 -- are BOTH currently open in other agents own working trees (the CLI-11 and CLI-6 implementers). Dispatching this task now risks either a merge collision on the same lines or this task landing a correction that CLI-11/CLI-6s own concurrent edits then silently revert or contradict. Will be dispatched once CLI-11 and CLI-6 land. WHY THIS MATTERS ENOUGH TO HOLD RATHER THAN RUN IN PARALLEL: this task exists because a task is not complete until its documentation is TRUE, and right now the agent-facing contract (AGENT_PROTOCOL.md) states the OPPOSITE of RELAY-13s shipped behaviour -- an agent reading it today would wrongly conclude its messaging key is not registered at enrolment and rely on out-of-band key exchange it does not need. That is exactly the kind of doc-contradicts-code defect this session has repeatedly found and fixed; holding briefly for a clean file-boundary is worth it here.

## Description

RELAY-13 (97f3f1b4-8575-4f63-9196-96bfbc049510) now registers the messaging public key at enrolment (server half committed at 61a59eb; client half staged, pending integrator commit). Four sites still assert or omit the OLD (pre-RELAY-13) behaviour, all confirmed by direct read at HEAD/working-tree 2026-08-08, all outside the implementing agents five-file boundary per both gates verdicts:

1. AGENT_PROTOCOL.md:549 -- "Nobody can fetch your messaging public key from the bus. It is not registered at enrolment..." FALSE since 61a59eb. This is the AGENT-FACING contract, so it misleads the audience most likely to act on it (an agent deciding whether it can rely on out-of-band key exchange).
2. CONTRACTS-CLI.md:1070 -- the MESSAGING key row says Minted "on first use, lazily, under the store lock (Store.EnsureMessagingKey)". FALSE for every new enrolment (the key is now minted and sent at enrol time); still true only for legacy/pre-RELAY-13 credentials resuming EnsureMessagingKey. Needs both cases stated.
3. client/client.go:434 -- MessagingPublicKey doc comment: "no messaging key is registered at enrolment and CRYPTO-4 ... does not exist". FALSE (first clause); CRYPTO-4 not existing is still true.
4. CONTRACTS-CLI.md -- the identities.json field table (~912/922) documents messaging_key_seed for the ON-DISK record, but the table does not document the WIRE field the client now sends (messaging_public_key) or the pending records new messaging_key_seed bookkeeping field distinctly from the promoted credential field. Cross-check against internal/httpapi/auth.go and client/enrol.go (EnrolRequest.MessagingPublicKey, pendingEnrolment.MessagingKeySeed) and add/confirm coverage.

A task is not complete until its documentation is true (CLAUDE.md). Per the orchestrators explicit instruction, this BLOCKS marking RELAY-13 done -- do not flip RELAY-13 to done until all four are fixed and re-verified.

Related: RELAY-13 (97f3f1b4-8575-4f63-9196-96bfbc049510).

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocks** [RELAY-13](../RELAY-13--97f3f1b4/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CLI-11](../../UNASSIGNED/CLI-11--bf966c07/task.md) — CLI-11: export the bus signing public key from the operator CLI (todo)
- [CLI-6](../../CLI/CLI-6--47001cb4/task.md) — CLI-6: log -- read the append-only audit log (metadata only; also absorbs the WAL-dumper… (in_progress)
- [CRYPTO-4](../../CRYPTO/CRYPTO-4--13f3947e/task.md) — CRYPTO-4: Key-distribution endpoint -- server-attested messaging key bundles (todo)
- [RELAY-13](../RELAY-13--97f3f1b4/task.md) — RELAY-13: Enrolment registers the agent's messaging public key (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [RELAY-13](../RELAY-13--97f3f1b4/task.md) — RELAY-13: Enrolment registers the agent's messaging public key (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
