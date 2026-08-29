# Settle the message-seq-floor KEYING question as an explicit follow-up, replacing the 'worth doing for consistency' framing -- and fix hub.go's operator-facing forging recipe in the SAME task

| Field | Value |
| --- | --- |
| Public id | `7fbe58ec-6b27-43dd-b0c1-986d7c702870` |
| Key | _(null in the export)_ |
| Epic | [DUR](../epic.md) |
| Status | todo |
| Priority | P2 |
| Component | security |
| Section | backlog |
| Tags | — |
| Created | 2026-08-08T15:32:07.957874+00:00 |
| Updated | 2026-08-08T15:32:07.957874+00:00 |
| Completed | — |

## Proof command

```sh
grep -q 'group-writable tighten path' DECISIONS.md && go test -race -count=1 -run TestSeqFloorRemedyTextMatchesTheOnDiskScheme ./internal/hub
```

## Description

BOTH SECURITY GATES on be447589-6583-4d5c-a9d4-ec9d9fef0f1c AGREED that keying `message-seq-floor` with the WAL MAC is the wrong fix, and EACH supplied a stronger reason than the one currently recorded. This task replaces the recorded framing and closes the one code artefact that depends on the answer.

WHAT IS RECORDED TODAY, AND WHY IT IS TOO WEAK. DECISIONS.md (in the 2026-08-07 feature-runner data-directory-permissions section, "What was deliberately NOT done") ends: "Keying remains worth doing for consistency with `wal-index-floor`, as a separate and honestly-labelled change." "For consistency" understates the case in one direction and overstates it in another, and both gates said so.

THE TWO STRONGER REASONS, TO BE RECORDED.
(1) THREAT-MODEL: keying only helps an attacker with directory-WRITE but WITHOUT file-READ -- precisely the attacker `enforceDataDirPermissions` now EXCLUDES. The attacker who remains (the bus's own uid, or root) can read `wal-mac.key` and forge any MAC we add. So against the POST-GATE threat model it buys approximately nothing.
(2) AVAILABILITY: `wal-mac.key`'s documented loss remedy is "move the log aside and restart". Key the floor to that SAME key and that remedy BRICKS THE BUS, because `ErrSeqFloorFileCorrupt` is fatal and the floor file is never regenerated. That couples the ONE FILE THAT EXISTS TO SURVIVE LOG LOSS to the key whose loss already forces abandoning the log -- a direct conflict, not a nicety.
(3) Per invariant 9, sharing one key across two message types needs a DOMAIN-SEPARATED SUBKEY, never plain reuse. Any keying that does land must say which construction and why.

TWO AMENDMENTS THE GATES ASKED FOR EXPLICITLY.
(a) File it as an EXPLICIT FOLLOW-UP with a decision, not as "for consistency" -- because there IS one place keying adds value over the directory gate: the GROUP-WRITABLE TIGHTEN path adopts PRE-PLANTED files and CONTINUES. `enforceDataDirPermissions` chmods a group-writable dir to 0700 and starts; anything already planted in it before that chmod is adopted unchecked. That is the honest scope of what keying would buy, and it should be recorded as the reason rather than "consistency".
(b) internal/hub/hub.go CURRENTLY HANDS OPERATORS A COMPLETE FORGING RECIPE for an unkeyed file. In the `ErrSeqFloorUnprovable` remedy text (hub.go, the error block at ~:733-741 at HEAD 16da89f -- the gate cited :707, the line has drifted, the text is confirmed present) it reads: "write it to %s yourself -- the format is two plain-text lines, %q followed by \"floor <n>\", where the digest is an unkeyed SHA-256 over the second line". That is SAFE TODAY (the file genuinely is unkeyed, and the remedy is genuinely needed) and WRONG THE DAY KEYING LANDS -- an operator following it would produce a file the bus rejects. It MUST change in the same task as the keying decision, whichever way that decision goes.

SUGGESTED DELIVERABLE. A dated DECISIONS.md section recording the decision and all three reasons above plus amendment (a); and a guard test that asserts hub.go's remedy text agrees with the ACTUAL on-disk scheme produced by `encodeSeqFloor`, so the recipe cannot drift out of truth silently -- that test is valid under EITHER decision and goes RED the day keying lands without the text being updated.

PROOF STATE OBSERVED (spec-keeper, HEAD 16da89f, not assumed): `bash scripts/proof-check.sh '<proof_cmd>'` -> verdict=FAIL, exit 1. RED, and RED FOR THE RIGHT REASON (`grep -c 'group-writable tighten path' DECISIONS.md` = 0, so the && short-circuits) -- RED today rather than VACUOUS. The test half must ALSO be observed RED before the fix.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **follow-up of** [be447589-6583-4d5c-a9d4-ec9d9fef0f1c](../../CORE/Enforce-data-directory-permissions-at-startup-and-bound--be447589/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [be447589-6583-4d5c-a9d4-ec9d9fef0f1c](../../CORE/Enforce-data-directory-permissions-at-startup-and-bound--be447589/task.md) — Enforce data-directory permissions at startup, and bound the message-seq floor (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
