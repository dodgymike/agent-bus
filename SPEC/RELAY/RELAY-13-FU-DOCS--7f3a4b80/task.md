# RELAY-13-FU-DOCS: three docs/comments assert the opposite of shipped RELAY-13 behaviour -- BLOCKS marking RELAY-13 done

| Field | Value |
| --- | --- |
| Public id | `7f3a4b80-a043-433e-8f65-44400a57b39b` |
| Key | _(null in the export)_ |
| Epic | [RELAY](../epic.md) |
| Status | done |
| Priority | P1 |
| Component | docs |
| Section | backlog |
| Tags | relay-13, doc-defect, blocks-done |
| Created | 2026-08-08T19:53:23.838687+00:00 |
| Updated | 2026-08-14T17:57:49.992449+00:00 |
| Completed | 2026-08-14T17:57:49.992433+00:00 |

## Proof command

```sh
bash -c 'set -e; ! grep -n "not registered at enrolment" AGENT_PROTOCOL.md; ! grep -n "on first use, lazily" CONTRACTS-CLI.md | grep -qi messaging; ! grep -n "no messaging key is registered at enrolment" client/client.go; grep -q messaging_public_key CONTRACTS-CLI.md'
```

## Status note

4/5 LANDED at 0d31d2f. Do NOT complete. Four sites fixed on main. The FIFTH is a doc comment in client/client.go, HELD BACK by the integrator -- not because anyone failed to write the text, but because that file also carries TWO ungated non-comment hunks from the live INVITE-CLIENT agent (a new endpointWith func and a resolvePinsWith func that rewrites resolvePins body): committing the doc-comment path would have shipped unreviewed code under a docs title. Blocked on INVITE-CLIENTs own commit landing, then the doc-comment fix can go in cleanly. This also means RELAY-13 (P0, 97f3f1b4) stays blocked a little longer -- unchanged conclusion, same reason, later resolution. SEPARATELY: the documentation agent found a FIFTH stale site the original brief did not list -- AGENT_PROTOCOL.md:641 ("minted on first send"), now also fixed at 0d31d2f alongside the four originally named. The brief said four; there were five real ones, four fixable now.

## Description

RELAY-13 (97f3f1b4-8575-4f63-9196-96bfbc049510) now registers the messaging public key at enrolment (server half committed at 61a59eb; client half staged, pending integrator commit). Four sites still assert or omit the OLD (pre-RELAY-13) behaviour, all confirmed by direct read at HEAD/working-tree 2026-08-08, all outside the implementing agents five-file boundary per both gates verdicts:

1. AGENT_PROTOCOL.md:549 -- "Nobody can fetch your messaging public key from the bus. It is not registered at enrolment..." FALSE since 61a59eb. This is the AGENT-FACING contract, so it misleads the audience most likely to act on it (an agent deciding whether it can rely on out-of-band key exchange).
2. CONTRACTS-CLI.md:1070 -- the MESSAGING key row says Minted "on first use, lazily, under the store lock (Store.EnsureMessagingKey)". FALSE for every new enrolment (the key is now minted and sent at enrol time); still true only for legacy/pre-RELAY-13 credentials resuming EnsureMessagingKey. Needs both cases stated.
3. client/client.go:434 -- MessagingPublicKey doc comment: "no messaging key is registered at enrolment and CRYPTO-4 ... does not exist". FALSE (first clause); CRYPTO-4 not existing is still true.
4. CONTRACTS-CLI.md -- the identities.json field table (~912/922) documents messaging_key_seed for the ON-DISK record, but the table does not document the WIRE field the client now sends (messaging_public_key) or the pending records new messaging_key_seed bookkeeping field distinctly from the promoted credential field. Cross-check against internal/httpapi/auth.go and client/enrol.go (EnrolRequest.MessagingPublicKey, pendingEnrolment.MessagingKeySeed) and add/confirm coverage.

A task is not complete until its documentation is true (CLAUDE.md). Per the orchestrators explicit instruction, this BLOCKS marking RELAY-13 done -- do not flip RELAY-13 to done until all four are fixed and re-verified.

Related: RELAY-13 (97f3f1b4-8575-4f63-9196-96bfbc049510).

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CRYPTO-4](../../CRYPTO/CRYPTO-4--13f3947e/task.md) — CRYPTO-4: Key-distribution endpoint -- server-attested messaging key bundles (todo)
- [INVITE-CLIENT](../../INVITE/INVITE-CLIENT--4123e25d/task.md) — INVITE-CLIENT: the Go client/CLI redeems an invite at enrol (+ AGENT_PROTOCOL.md entry) -… (done)
- [RELAY-13](../RELAY-13--97f3f1b4/task.md) — RELAY-13: Enrolment registers the agent's messaging public key (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [2ca053dd-1b63-42b5-a485-f57b623722ac](../internal-relay-guards_test.go-912-says-the-RELAY-6-subst--2ca053dd/task.md) — internal/relay/guards_test.go:912 says the RELAY-6 substitution 'IS NOT RECORDED IN DECIS… (done)
- [4b51635d-336f-4f25-94c2-64c53578859d](../../AGENTIF/AGENT_PROTOCOL.md-is-missing-the-CLI-11-key-export-publi--4b51635d/task.md) — AGENT_PROTOCOL.md is missing the CLI-11 (key export-public) and CLI-6 (log) sections -- b… (todo)
- [CLAUDE-PATHSPEC-MM-NOT-GATE](../../PROCESS/CLAUDE-PATHSPEC-MM-NOT-GATE--077bcba5/task.md) — CLAUDE.md: MM is not the gate -- a clean M can still hide another agent's worktree hunks (todo)
- [RELAY-13](../RELAY-13--97f3f1b4/task.md) — RELAY-13: Enrolment registers the agent's messaging public key (done)
- [RELAY-FU-DOCGO-CROSSBUSTRUST-STALE](../RELAY-FU-DOCGO-CROSSBUSTRUST-STALE--4988156c/task.md) — internal/relay/doc.go asserts relay ingest is structurally blocked (no CrossBusTrust impl… (todo)
- [c716f8e7-ad9c-4af9-9fac-1bdb75c8f900](../../DOCS/PROTOCOL.md-1002-says-internal-relay-is-imported-by-noth--c716f8e7/task.md) — PROTOCOL.md:1002 says internal/relay is 'imported by nothing' -- false since ed77bba (int… (todo)
- [fbb16f9b-1b81-4fd0-a60f-5b2a76806bff](../internal-httpapi-peermount.go-pre-auth-prober-does-not-e--fbb16f9b/task.md) — internal/httpapi/peermount.go: 'pre-auth prober does not exist' overstates ruling (h), an… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
