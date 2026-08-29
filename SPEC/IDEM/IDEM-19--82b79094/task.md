# IDEM-19: expiry-queue compaction is O(retained) -- 48.4s vs 32ms measured, on the every-send path

| Field | Value |
| --- | --- |
| Public id | `82b79094-d6af-4cc4-889d-e3ae18bd62b8` |
| Key | IDEM-19 |
| Epic | [IDEM](../epic.md) |
| Status | done |
| Priority | P1 |
| Component | BE |
| Section | backlog |
| Tags | — |
| Created | 2026-08-16T12:07:34.864807+00:00 |
| Updated | 2026-08-16T14:04:37.883061+00:00 |
| Completed | 2026-08-16T14:04:37.883045+00:00 |

## Proof command

```sh
go test -race -count=1 ./internal/idem/ ./internal/ack/
```

## Status note

BOTH halves code-complete in the worktree, NOT committed. internal/idem: expiry sweep O(retained) -> amortised O(evicted) via a head index + deferred compaction; 62.70s -> 57.11ms at 65536 staggered entries. internal/ack: the same defect in sweepLocked, fixed the same way; 51.00s -> 29.08ms at 65536 staggered rows (1754x), settled 2-entries-per-row shape 11.59s -> 7.62ms. Both measured in clean overlays of HEAD. idem gates PASS; ack reviewer+security in flight. Awaiting integrator.

## Description

FILED 2026-08-16 by main, from the ACK-2 security gate, which MEASURED it.

# The defect

Draining a full expiry table with staggered deadlines takes 48.4 SECONDS where the amortised form
takes 32 MILLISECONDS. That is ~1500x, measured by the security gate on ACK-2, not estimated.

The sweep is O(retained) per call. In internal/idem that has been tolerable. It is NOT tolerable
wherever the sweep runs under a global write lock, because the cost is paid by every writer.

# Two call sites, and the second one is the pre-existing half

  internal/ack     -- new (ACK-2). The implementer deliberately did NOT copy internal/idem here.
                      ACK-CONTRACT.md section 11.2 said "mirror internal/idem exactly"; that would
                      have imported a P1 into a path that runs under the global write lock, so the
                      PATTERN was mirrored (front-popped ordered queue) rather than the map scan.
                      That divergence was the right call and is recorded here so it is not
                      "corrected" back later by someone reading section 11.2 literally.

  internal/idem    -- PRE-EXISTING, byte-identical code, and OUTSIDE ACK-2's file boundary so it was
                      correctly reported rather than fixed. This is the half that is live today.

# Why this is worth a task rather than a note

internal/idem is on the hot path for EVERY send: invariant 10 requires duplicate detection
everywhere, durable across restart. A 48-second sweep on that path is a self-inflicted outage under
exactly the conditions -- a full table with staggered deadlines -- that a busy bus produces
naturally. It does not need an attacker.

Related history: the idempotency plane already produced one P0 this month (RELAY-FU-IDEM-METER-BY-PEER,
where the denominator was 32767 and one peer could lock out every agent). That was fixed at 72d6f5d.
This is a different defect in the same package and should not be assumed covered by it.

# Scope

Amortise the compaction in BOTH packages. internal/ack first (it is new, unreleased, and its shape
is already correct) then internal/idem (live, hot path, needs care).

# Acceptance
  - a benchmark or test that REPRODUCES the 48.4s figure before the fix -- a performance claim whose
    slow case was never observed is not evidence
  - the same test after, showing the amortised figure
  - internal/idem's fix must not change duplicate-detection semantics. Invariant 10's three cases
    must stay uncollapsed: same key + same payload is a retry and returns the original result; same
    key + different payload is refused and logged; only a replayed accepted SIGNED message
    disconnects. A sweep change must not touch any of that.
  - crash-injection: durable-across-restart is part of invariant 10, so prove recovery still yields
    the same idempotency state after the change.

# Do NOT
  - do not "fix" internal/ack back to matching internal/idem's map scan. Section 11.2 of
    ACK-CONTRACT.md is wrong on this point and this task supersedes it.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


_None recorded._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [ACK-2](../../ACK/ACK-2--9564f953/task.md) — ACK-2: Durable local send acceptance and ACK/NACK lifecycle record (done)
- [RELAY-FU-IDEM-METER-BY-PEER](../../RELAY/RELAY-FU-IDEM-METER-BY-PEER--8774f265/task.md) — RELAY-FU-IDEM-METER-BY-PEER: Meter the applied-key table by the AUTHENTICATED PEER, not t… (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [IDEM-20](../IDEM-20--5f914b18/task.md) — IDEM-20: the amortised sweep overshoots the 64 MiB budget by ~4 MiB, and CONTRACTS-HTTP.m… (todo)
- [RELAY-52](../../RELAY/RELAY-52--67c6248d/task.md) — RELAY-52: invariant 6's loud-discard line at hub.go:1104 has no test anywhere in the repo (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
