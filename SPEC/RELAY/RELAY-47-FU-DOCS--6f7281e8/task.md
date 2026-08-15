# RELAY-47-FU-DOCS: three shipped docs still tell agents multi-hop relay does not work, after RELAY-47 made it work -- CONTRACTS-ONDISK.md:1546, AGENT_PROTOCOL.md:244 and :1122

| Field | Value |
| --- | --- |
| Public id | `6f7281e8-91cd-4b50-a5ac-e031041eb5ad` |
| Key | RELAY-47-FU-DOCS |
| Epic | [RELAY](../epic.md) |
| Status | done |
| Priority | P1 |
| Component | docs |
| Section | backlog |
| Tags | docs, relay, doc-debt, relay-47-followup |
| Created | 2026-08-15T12:57:08.852144+00:00 |
| Updated | 2026-08-15T14:42:24.974973+00:00 |
| Completed | 2026-08-15T14:42:24.974931+00:00 |

## Proof command

```sh
bash -c 'fail=0; for p in "multi-hop path yet" "multi-hop relay is not implemented" "nothing yet verifies a live connection" "no federated traffic flows yet"; do if grep -qF "$p" AGENT_PROTOCOL.md; then echo "STALE PRESENT AGENT_PROTOCOL.md: $p"; fail=1; fi; done; if grep -qF "Onward multi-hop relay is deliberately not implemented" CONTRACTS-ONDISK.md; then echo "STALE PRESENT CONTRACTS-ONDISK.md"; fail=1; fi; for m in "RELAY-48" "peer-client-fingerprint" "onward_relay" "proven against"; do if grep -qF "$m" AGENT_PROTOCOL.md; then :; else echo "MARKER MISSING AGENT_PROTOCOL.md: $m"; fail=1; fi; done; if [ "$fail" -ne 0 ]; then echo DOCS_STALE; exit 1; fi; echo DOCS_OK'
```

## Description

Filed 2026-08-15 by spec-keeper on behalf of the RELAY-47 feature-runner. **Documentation debt RELAY-47 COULD NOT DISCHARGE**, for a specific and legitimate reason: at the time, all three shared files carried ANOTHER agent's UNCOMMITTED edits (`CONTRACTS-ONDISK.md`, `AGENT_PROTOCOL.md` and `DECISIONS.md` were all dirty), and CLAUDE.md forbids a pathspec commit over a contaminated worktree. RELAY-47 therefore updated `CONTRACTS-HTTP.md` and `AGENT_LOG.md` only. This task carries the rest, to be taken by a `documentation` agent ONCE THOSE FILES ARE FREE (`git status --porcelain -- CONTRACTS-ONDISK.md AGENT_PROTOCOL.md DECISIONS.md` empty; and read `git diff HEAD -- <path>` before committing, not just the index).

== WHY P1 ==

**Agents reading `AGENT_PROTOCOL.md` today are being told multi-hop relay does not work, when it does.** That is the agent-facing surface -- the file exists precisely so an agent does not have to read the code -- and it is now actively misleading about a capability that shipped.

== THE THREE FALSE STATEMENTS ==

1. **`CONTRACTS-ONDISK.md:1546`** -- "**Onward multi-hop relay is deliberately not implemented.** The egress adapter forwards only ..." -- now FALSE.
2. **`AGENT_PROTOCOL.md:244`** -- every audit record "carries a **single** element, this bus's own id: nothing produces a multi-hop path yet" -- now FALSE: a relayed record carries the full ordered traversal.
3. **`AGENT_PROTOCOL.md:1122`**, under "What is still NOT supported" -- a bus "does not carry it onward to a further hop -- multi-hop relay is not implemented, and each bus is a leaf", and the claim that two buses not directly peered cannot reach each other through a third -- now FALSE.

**The replacement text is already WRITTEN OUT in feature-runner's RELAY-47 handoff report** -- use it rather than re-deriving it, and check it against the shipped behaviour before pasting (including what is still true: the fan-out bound, the hop limit, and that a pending onward hop is not yet crash-safe -- RELAY-48).

Also add the DECISIONS.md entry RELAY-47 could not write (the onward-relay wiring decision and the `maxOnwardBusesPerMessage = 8` choice), and an AGENT_LOG.md line for this doc sweep.

== PROOF ==

    bash -c 'set -e; ! grep -q "Onward multi-hop relay is deliberately not implemented" CONTRACTS-ONDISK.md; ! grep -q "multi-hop path yet" AGENT_PROTOCOL.md; ! grep -q "multi-hop relay is not implemented" AGENT_PROTOCOL.md; grep -q onward CONTRACTS-ONDISK.md; echo DOCS_OK'

Each of the three greps was **CONFIRMED PRESENT (i.e. the proof is RED) on 2026-08-15** -- `grep -c` returned exactly 1 for each string -- so this is a proof that has been observed FAILING before the fix, per CLAUDE.md's rule on grep-based proofs. It pins the three specific stale sentences rather than matching incidentally. Note the proof is NECESSARY BUT NOT SUFFICIENT: deleting the sentences passes it, so the reviewer must also confirm the REPLACEMENT text is present and accurate.

RELATES: RELAY-47, RELAY-48.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **follow-up of** [RELAY-47](../RELAY-47--dd69c4d3/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [RELAY-47](../RELAY-47--dd69c4d3/task.md) — RELAY-47: ONWARD RELAY -- WIRE an intermediate bus to forward a relayed message to a THIR… (done)
- [RELAY-48](../RELAY-48--9887b0eb/task.md) — RELAY-48: onward relay is NOT crash-safe -- a pending onward hop is durably ABANDONED at… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
