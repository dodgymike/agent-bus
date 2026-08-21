# SWEEP-TWO-PASS-DISCIPLINE: a contract change needs a MECHANISM sweep and a PROSE sweep, not one grep -- plus the discipline of recording verified negatives

| Field | Value |
| --- | --- |
| Public id | `268a0c73-d201-44be-bb49-a36cea11aab6` |
| Key | SWEEP-TWO-PASS-DISCIPLINE |
| Epic | [PROCESS](../epic.md) |
| Status | done |
| Priority | P2 |
| Component | docs |
| Section | backlog |
| Tags | — |
| Created | 2026-08-14T20:27:40.251455+00:00 |
| Updated | 2026-08-14T20:29:22.687516+00:00 |
| Completed | 2026-08-14T20:29:22.687500+00:00 |

## Proof command

```sh
grep -n 'SWEEP-TWO-PASS-DISCIPLINE' SPEC/PROCESS/*/task.md 2>/dev/null | head -1 && echo FOUND_IN_MIRROR
```

## Description

PROCESS FINDING, recorded 2026-08-14 during the SIGN-1-FU-REORDER-WATERMARK stale-claim sweep (security C11: freshness enforced server-side at ingest, never by a recipient-side sequence cursor).

THE FINDING: a contract change needs TWO sweeps, not one, and they are not substitutable.

SWEEP 1 -- MECHANISM (code call-sites): every place that IMPLEMENTS or would implement the old behaviour. For this sweep that meant grepping for actual recipient-side sequence-cursor rejection logic in client/, internal/httpapi/, internal/hub/ -- code that would physically discard a message on sequence <= high-water-mark.

SWEEP 2 -- PROSE (how a reader would phrase the promise): every place that DESCRIBES the contract in the words a human or an implementing agent would naturally reach for -- "ascending sequence order", "total and stable", "MUST only advance, never rewind", "sequence+cursor". This sweep is NOT a subset of sweep 1's grep terms; it requires reading task descriptions, epic bodies and doc comments for the SHAPE of the claim, not a literal string match on the old wording, because a task that describes the mechanism specifies it will be built the way it's phrased. In this case: SIGN-4's own description (a specification, not yet built) carried the exact defect as a requirement -- the most urgent site of the whole sweep, since nothing there had been written yet to grep for. IDEM-14 and RATCHET-2 carried the same claim as a CROSS-REFERENCE ("SIGN-4's sequence+cursor") rather than a re-derivation, which a mechanism-only sweep restricted to the defect's own code would never surface. CRYPTO-10 specified a CLIENT-SIDE verify helper's exit-code contract ("replayed message (SIGN-4's cursor)") -- prose describing a not-yet-built CLI surface, doubly invisible to a code sweep.

WHY THE PROSE SWEEP MATTERS SEPARATELY: it protects clients you cannot see. A spec task or doc paragraph that states the old contract will get built correctly-to-spec by whichever agent picks it up next, even if every existing line of committed code is already fixed. The bug is not in the tree yet at that point -- it is in what the tree is about to become. Skipping the prose sweep because "the code sweep is done" ships the defect on a delay instead of preventing it.

THREE VERIFIED NEGATIVES from this same sweep, recorded because a sweep that reports only its edits cannot be checked for over-reach any more than for completeness -- these are sites that LOOKED like they might carry the stale claim (matched a broad candidate regex or were flagged for review) but were read in full and found to already be correct, and were deliberately left unedited:
  1. CONTRACTS-CLI.md:1366 -- a "never skips" pointer; editing it would have made a TRUE statement FALSE. Flagged by the coordinator explicitly as a verified negative in the same message that supplied the SIGN-4 amendment draft.
  2. MSG-4 (task, done) -- "cursor semantics" in its title matched the sweep's regex, but its description defines the cursor as an OPAQUE per-agent-visible sequence POSITION with no sequence-based rejection semantics anywhere in it. Already consistent with the corrected contract; not touched.
  3. SIGN-6 (task, todo) -- matched on "cursor" in its poison-message-wedge clause ("the cursor advances past the unverifiable message... SIGN-4's cursor must tolerate gaps"), but on full read this is the CORRECT direction already (cursor tolerates gaps, does not reject on them) and needed no amendment.

ACTION ITEM FOR FUTURE SWEEPS: when a contract changes, plan BOTH sweeps up front as two explicitly named passes, not one grep followed by "and also check the docs" as an afterthought -- the prose pass needs its own candidate-gathering method (read task/epic descriptions and doc comments for phrasing that WOULD describe the old contract, not a literal string match), and its own verified-negatives list should be recorded alongside its edits so the sweep's completeness (and lack of over-reach) can be checked by someone else.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CRYPTO-10](../../CRYPTO/CRYPTO-10--68ff679d/task.md) — CRYPTO-10: \`agent-bus verify\` helper + scripts/bus-*.sh validate-before-accept + AGENT_PR… (todo)
- [IDEM-14](../../IDEM/IDEM-14--b0facce9/task.md) — IDEM-14: Idempotency violation path -- key reuse with a different payload rejects, logs a… (todo)
- [MSG-4](../../MSG/MSG-4--25ebcbc9/task.md) — MSG-4: Cursor semantics + GET /v1/messages history (done)
- [RATCHET-2](../../RATCHET/RATCHET-2--ade31a62/task.md) — RATCHET-2: Threat model -- what Ed25519 signing defends against, and explicitly what it d… (todo)
- [SIGN-1-FU-REORDER-WATERMARK](../../SIGN/SIGN-1-FU-REORDER-WATERMARK--c829af9a/task.md) — SIGN-1-FU-REORDER-WATERMARK: a late-arriving lower sequence is never delivered to a reade… (superseded)
- [SIGN-1-FU-REORDER-WATERMARK](../../SIGN/SIGN-1-FU-REORDER-WATERMARK--86c7d368/task.md) — SIGN-1-FU-REORDER-WATERMARK: a late-arriving lower sequence is never delivered to a reade… (todo)
- [SIGN-4](../../SIGN/SIGN-4--33fa35d8/task.md) — SIGN-4: Replay/freshness -- enforced SERVER-SIDE at ingest, never by recipient-side seque… (todo)
- [SIGN-6](../../SIGN/SIGN-6--c9e4aea1/task.md) — SIGN-6: A signature is MANDATORY on the wire -- ingest policy and fail-closed handling of… (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [0f4a0736-979b-4a20-b75f-0b2950f2181c](../gen-spec-mirror.sh-REFUSES-TO-WRITE-unexpected-non-blank--0f4a0736/task.md) — gen-spec-mirror.sh REFUSES TO WRITE ("unexpected non-blank column-0 lines") -- markdown e… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
