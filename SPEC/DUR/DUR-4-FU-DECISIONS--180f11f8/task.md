# DUR-4-FU-DECISIONS: record the SHIPPED damage-class taxonomy in DECISIONS.md -- which classes discard, what each logs, and the exact list of non-damage errors that stay FATAL

| Field | Value |
| --- | --- |
| Public id | `180f11f8-d526-4023-b06b-9b991851bcd3` |
| Key | _(null in the export)_ |
| Epic | [DUR](../epic.md) |
| Status | todo |
| Priority | P1 |
| Component | durability |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T14:59:56.226536+00:00 |
| Updated | 2026-08-02T17:59:50.319913+00:00 |
| Completed | — |

## Proof command

```sh
grep -q 'damage class' DECISIONS.md && grep -q 'stays fatal' DECISIONS.md
```

## Description

RE-SCOPED 2026-08-02 AFTER VERIFYING WHAT THE NEW DECISIONS.md ENTRY ALREADY COVERS. All THREE of
this task's original items are now SATISFIED by the 2026-08-02 user decision (DECISIONS.md,
"Sixteen open questions settled"), checked line by line:

 (1) "truncation is permitted ONLY for a provably-incomplete frame at EOF -- a full-length frame that
     fails its own checksum is a FATAL STARTUP ERROR" -- REVERSED, not merely recorded. Section 1
     ("the bus ALWAYS restarts") narrows invariant 6 explicitly: "truncation is no longer restricted
     to a verified-corrupt *tail*. Damaged records anywhere may be discarded -- with a log entry
     each." There is nothing left to record; recording the old policy would be actively harmful.
 (2) "a NUL tail longer than one frame length is refused, not truncated" -- same reversal, subsumed by
     the general discard-and-log rule.
 (3) "the tail-safety proofs are CRC-based, so they do NOT hold against an attacker with write access
     to the data directory" -- RECORDED. Section 3 replaces CRC32C with an HMAC-SHA256 keyed MAC and
     states the residual verbatim: "storing it beside the WAL defends against a remote client but not
     against an attacker who already has data-directory write access."

WHAT IS GENUINELY STILL MISSING, and is now this task's only scope: the decision states the POLICY,
but not the TAXONOMY the code will actually implement. That has to be written down once, or every
future maintainer re-derives it from recover.go:

 A. Enumerate the DAMAGE CLASSES that trigger discard-and-continue -- torn tail, checksum/MAC failure
    on a complete frame, a length field that overshoots EOF, a NUL run, an unknown record type, a
    corrupt file header, a mid-file damaged frame -- and for EACH say what is discarded (one frame?
    to EOF? the whole file?) and what the log line must contain (offset, record index, record type,
    bytes discarded, reason). "Log loudly and specifically" is the requirement; this is where
    "specifically" gets defined.
 B. Enumerate the NON-DAMAGE errors that STAY FATAL: permission denied, I/O failure, data-directory
    lock already held, missing/unwritable data dir, and (per DUR-12) a missing or wrong MAC key. This
    list is the thing that stops always-restart from degrading into "silently start empty on an
    unreadable disk".
 C. State the honest narrowing of invariants 4 and 6 in the same section, so PROTOCOL.md and
    AGENT_PROTOCOL.md can quote it rather than paraphrase it.

WRITE THIS AFTER DUR-11 LANDS, so it describes the code that actually shipped rather than an interim
version. DUR-11 is the task doing the discard/log/continue conversion. Append a NEW dated section --
DECISIONS.md is contended; never edit existing lines.

PROOF. `grep -q 'damage class' DECISIONS.md && grep -q 'stays fatal' DECISIONS.md` -- verdict FAIL
(class=file-assertion, exit 1) TODAY, which is correct and non-vacuous: it fails precisely because
the taxonomy is unwritten and flips to PASS when it exists. The written entry must therefore contain
both phrases literally.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


_None recorded._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [DUR-11](../DUR-11--884d3da4/task.md) — DUR-11: Close security's two OPEN HIGH truncation holes -- anchor the tail veto on record… (done)
- [DUR-12](../DUR-12--cbc9ab0c/task.md) — DUR-12: Replace CRC32C with an HMAC-SHA256 keyed MAC (ON-DISK FORMAT CHANGE, reserved ond… (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [DUR-4](../DUR-4--59c36769/task.md) — DUR-4: Corrupt-tail detection & truncation (superseded)
- [fc8cd234-d275-43a1-9cb0-d10bca4a4086](../../PROCESS/Backfill-non-vacuous-proof_cmd-across-the-14-actionable--fc8cd234/task.md) — Backfill non-vacuous proof_cmd across the 14 actionable tasks that have none (CLI-1..9 +… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
