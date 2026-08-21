# IDEM-18: Wrappers generate the idempotency key ONCE and reuse it across retries, + AGENT_PROTOCOL.md / PROTOCOL.md / CONTRACTS.md

| Field | Value |
| --- | --- |
| Public id | `61f80a28-b177-4224-be5d-dce0418bfd2f` |
| Key | IDEM-18 |
| Epic | [IDEM](../epic.md) |
| Status | todo |
| Priority | P1 |
| Component | agentif |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T13:17:45.425457+00:00 |
| Updated | 2026-08-14T23:02:31.320403+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run TestCLISendReusesIdempotencyKeyOnRetry ./cmd/agent-busctl/... && grep -qi idempotency AGENT_PROTOCOL.md && grep -qi idempotency CONTRACTS-CLI.md && grep -qi idempotency PROTOCOL.md
```

## Status note

NOT DONE (bookkeeping reconciliation pass, 2026-08-07). The old proof_cmd was unrunnable: scripts/bus-send.sh does not exist (ls scripts/ shows only bus-serve.sh) and has not existed since the 2026-08-02 CLI/AGENTIF epic merge decision replaced scripts/bus-*.sh wrappers with the busctl CLI + client package -- this task title (Wrappers generate the idempotency key ONCE...) is stale terminology for that same reason, and an earlier note on this task already said so (2026-08-02 spec-keeper). What IS true and proven on the CLI side: client/messages.go mints the idempotency key once and reuses it across every transport retry (go test -race -run TestCLISendReusesIdempotencyKeyOnRetry ./cmd/busctl/... -- PASS, verified this pass), and, since 9accb65 (2026-08-07), an ambiguous send/broadcast failure now carries that key forward instead of losing it (CLI-4, 2b4ecf0b, done). What remains missing and blocks this task under invariant 7 (a feature is not done without its protocol-doc entry): AGENT_PROTOCOL.md has ZERO mentions of idempotency (grep -c -i idempot AGENT_PROTOCOL.md = 0) and is itself stale -- it still documents scripts/bus-*.sh as (not yet shipped) rather than describing busctl at all. PROTOCOL.md already mentions idempotency (3 hits) and CONTRACTS-CLI.md documents the send/broadcast idempotency contract in full, but AGENT_PROTOCOL.md, the primary agent-facing usage doc, does not. Remaining work: rewrite AGENT_PROTOCOL.md against busctl send/broadcast --idempotency-key (not shell wrappers), documenting that the key is minted once and reused across retries and that it now survives an ambiguous failure. Left in_progress; do not complete on the CLI tests alone.

--- APPENDED 2026-08-14 (spec-keeper, on the feature-runner's audit). Everything above remains ACCURATE and is retained deliberately. Two changes to the RECORD, none to the work:

(1) STALE OWNER CLEARED, status reset in_progress -> todo. The stored owner was the generic slug 'feature-runner', which names NOBODY: the feature-runner on this pass did not claim this task and confirms no agent is on it. Left as-was it was neither being worked nor claimable. It is now claimable.

(2) SCOPE CONFIRMED for whoever picks it up: the remaining half is DOC + CLI SURFACE, not server code -- a rewrite of AGENT_PROTOCOL.md against `busctl send/broadcast --idempotency-key` (not the retired scripts/bus-*.sh wrappers) plus the CLI surface that delivers it. That is entirely outside the internal/idem boundary the feature-runner works within, which is why this pass audited it and left the work untouched rather than half-doing it.

## Description

GATED on IDEM-10 (key contract) and IDEM-12 (idempotent send/broadcast). Filed 2026-08-02 as the one gap left after merging two concurrently-filed IDEM epics (see the IDEM epic note): IDEM-10..17 cover the server side thoroughly and say nothing about the agent-facing side, which invariant 7 makes non-optional -- agents never hand-write HTTP, so the idempotency key is the WRAPPER's responsibility, not the calling agent's. THE SINGLE MOST LIKELY WAY THIS EPIC SHIPS BROKEN: a wrapper that generates a FRESH key on every attempt. Every retry then looks like a brand-new operation, the server dedupes nothing, duplicates flow exactly as before -- and every server-side test in IDEM-16/IDEM-17 keeps passing, because none of them exercise the wrapper. DELIVER: (1) each mutating wrapper (bus-enrol, bus-send, bus-broadcast, bus-leave, bus-peer) generates ONE key per logical operation, holds it for the whole retry loop, and reuses it verbatim on every attempt. (2) Key generation is real randomness -- no PIDs, no timestamps, no counters that reset across restarts, all of which collide in exactly the multi-process, post-crash situations this epic exists for. (3) A test that FORCES a retry (first attempt killed or refused) and asserts exactly ONE message resulted -- run through scripts/bus-*.sh against a running throwaway bus with its own data dir under /tmp, never hand-written curl: if the wrapper doesn't retry idempotently, the feature doesn't work. (4) AGENT_PROTOCOL.md: agents call the wrapper and do NOT craft keys themselves; what a replayed-ack response means; and that after an IDEM-14 disconnect, reconnecting and retrying with the SAME key is CORRECT, while reusing a key for different content is a protocol violation that will disconnect them again. (5) PROTOCOL.md: the key's transport, the per-agent scope tuple, the payload fingerprint, and -- stated honestly -- IDEM-11's retention window as the BOUNDARY of the guarantee: duplicates are suppressed within the window, and a retry arriving after its key is evicted is applied as a new operation. The system does not provide unconditional exactly-once and the docs must not imply it does. (6) CONTRACTS.md: the header/field, every new error code, the record type IDEM-11 reserved, and any flag/env var bounding retention.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [2b4ecf0b-7f01-436b-8135-811ff4963a0e](../../CLI/busctl-send-broadcast-lose-the-minted-idempotency-key-on--2b4ecf0b/task.md) — busctl send/broadcast lose the minted idempotency key on an ambiguous failure (done)
- [CLI-4](../../CLI/CLI-4--137465b9/task.md) — CLI-4: send + broadcast, incl. stdin and interactive (replaces bus-send.sh and bus-broadc… (done)
- [IDEM-10](../IDEM-10--b28e5153/task.md) — IDEM-10: Idempotency key -- format, client-supplied untrusted validation, scoped per-agent (done)
- [IDEM-11](../IDEM-11--8e2c4de3/task.md) — IDEM-11: Durable applied-key store, recovered via WAL replay, with a bounded retention wi… (done)
- [IDEM-12](../IDEM-12--26dd5625/task.md) — IDEM-12: Idempotent send/broadcast -- retries return the original result, no new sequence… (todo)
- [IDEM-14](../IDEM-14--b0facce9/task.md) — IDEM-14: Idempotency violation path -- key reuse with a different payload rejects, logs a… (todo)
- [IDEM-16](../IDEM-16--b6b76aeb/task.md) — IDEM-16: Exactly-once test suite -- retry storm, concurrent race under -race, and key-reu… (todo)
- [IDEM-17](../IDEM-17--8b1e85fd/task.md) — IDEM-17: Crash-injection test -- restart mid-retry-window still yields exactly one effect (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CLI-4](../../CLI/CLI-4--137465b9/task.md) — CLI-4: send + broadcast, incl. stdin and interactive (replaces bus-send.sh and bus-broadc… (done)
- [IDEM-10](../IDEM-10--b28e5153/task.md) — IDEM-10: Idempotency key -- format, client-supplied untrusted validation, scoped per-agent (done)
- [IDEM-11](../IDEM-11--8e2c4de3/task.md) — IDEM-11: Durable applied-key store, recovered via WAL replay, with a bounded retention wi… (done)
- [IDEM-9](../IDEM-9--b0dc4a12/task.md) — IDEM-9: Wrappers generate the key ONCE and reuse it on retry, + AGENT_PROTOCOL.md / PROTO… (superseded)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
