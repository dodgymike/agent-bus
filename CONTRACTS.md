# Contracts — index

Every route, CLI flag, env var, header, and durable record type agent-bus exposes. **Update the
relevant plane file below in the same commit that changes any of the surfaces it documents**
(`CLAUDE.md` step 9). This is the authoritative reference; `README.md` and `AGENT_PROTOCOL.md`
summarise for humans/agents but these files are where the exact shape lives.

**Split 2026-08-02** (`CONTRACTS-SPLIT`, public_id `360a2679-b5dc-4b17-863f-fb4462764e6d`) out of a
single `CONTRACTS.md` into the four plane files below, to remove a single-writer chokepoint on this
file that caused three P0s across two consecutive triage loops (concurrent agents all needing to
land a documentation update in the same file). This split was a **pure content move** — every
section kept its exact prior wording, only its location changed — so a diff against the pre-split
file should show only relocation, not rewording. `CONTRACTS.md` itself stays at this path as an
index so existing references and muscle memory land somewhere useful instead of 404ing.

## Where each surface lives

| Plane file | What it documents |
| --- | --- |
| [`CONTRACTS-CLI.md`](CONTRACTS-CLI.md) | Server / CLI flags (`cmd/agent-bus`) and environment variables. |
| [`CONTRACTS-HTTP.md`](CONTRACTS-HTTP.md) | HTTP routes, headers, enrolment/sessions, and authentication (the `authMiddleware` contract, the allow-list, the credential model). |
| [`CONTRACTS-ONDISK.md`](CONTRACTS-ONDISK.md) | Durable record types / wire protocol versions, on-disk files in the data directory (`bus.lock`, `bus.wal`), and the write-ahead log at startup. **This is the plane most in flux** (DUR / on-disk format version 2 work is active) — it benefits most from being isolated in its own file. |
| [`CONTRACTS-AGENT.md`](CONTRACTS-AGENT.md) | The agent-facing surface (`scripts/bus-*.sh` wrappers, `AGENT_PROTOCOL.md`) and repo tooling scripts (`scripts/spec-cloud.sh`, `scripts/proof-check.sh`) that are NOT agent-facing but are documented alongside it. |

Two known-wrong passages were moved **unchanged, wrong exactly as before** — fixing their content is
explicitly out of scope for the split itself and is tracked by other tasks, not this one:

- `CONTRACTS-CLI.md`'s `-listen` default row still reads `:8080` (should be `127.0.0.1:8080` per the
  DECISIONS.md localhost-default decision) — tracked by `b0a5630b` (LISTENADDR-FU-CONTRACTS) and
  `c27f9439` (AUTH-1-FU-LISTENADDR).
- `CONTRACTS-ONDISK.md`'s WAL-repair prose still documents the reverted refuse-to-start policy
  (`provably torn tail`, `RepairTail`) instead of the shipped always-restart `RepairLog`/quarantine
  behaviour — tracked by `5b178dde` (DUR-11-FU-CONTRACTS).

Do not "fix while you're in there" on either passage from this file; that is scope creep on whichever
task lands the correction, and both are already filed.
