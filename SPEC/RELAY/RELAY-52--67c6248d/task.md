# RELAY-52: invariant 6's loud-discard line at hub.go:1104 has no test anywhere in the repo

| Field | Value |
| --- | --- |
| Public id | `67c6248d-c611-4a3b-85ad-97cdd7c4cb20` |
| Key | RELAY-52 |
| Epic | [RELAY](../epic.md) |
| Status | done |
| Priority | P2 |
| Component | BE |
| Section | backlog |
| Tags | — |
| Created | 2026-08-16T15:17:13.264046+00:00 |
| Updated | 2026-08-21T10:59:16.980746+00:00 |
| Completed | 2026-08-21T10:59:16.980729+00:00 |

## Proof command

```sh
go test -race -run 'TestUndecodableMessageRecordIsDiscardedLoudlyAndTheBusStarts' ./internal/hub
```

## Status note

Code-complete at working tree over HEAD 14ed009; awaiting integrator commit. Single new file internal/hub/discard_relay52_test.go (402 lines, package hub_test), staged, no production code changed. Test TestUndecodableMessageRecordIsDiscardedLoudlyAndTheBusStarts with two subtests covering both sides of the maxDecodeFailuresLogged cap (1-record below-cap fixture, 12-record past-cap fixture; measured 8 individual lines + 1 cap line, summary reporting the true total 12). Proof verdict, quoted: "proof-check: verdict=PASS class=test exit=0 tests_run=3 top_level=1 skipped=0 failed=0 empty_pkgs=0" -- reproduced independently in three separate clean overlays of HEAD (feature-runner, reviewer, security). GATES: reviewer COMPLETED PASS; security COMPLETED PASS (both posted kind=response notes). Mutation evidence: test-engineer 8/9 RED, reviewer 15/16 RED, security 11 mutations all RED, feature-runner independently confirmed the field-stripping mutation RED. The single GREEN (Hub.Apply returning the decode error at hub.go:1116) is unobservable by construction -- internal/wal/replay.go:293-301 demotes an applier rejection to its own loud discardRecord and returns nil, and hub.Open never reads wal.Recovered's discard list; verified independently by reviewer and security, documented in the test file itself. documentation gate SKIPPED with justification: test-only change, no HTTP route / CLI flag / env var / on-disk record type / wire version / agent-facing surface moved, so no CONTRACTS-*.md or AGENT_PROTOCOL.md plane is owed (reviewer explicitly ruled invariant 7 not engaged -- a test adds no capability).

## Description

FILED 2026-08-16 by main, from the RELAY-48 gate re-verification (security B2, second half). CORRECTED 2026-08-21 after a grep audit at HEAD 14ed009 -- the original premise below was FALSE and is preserved struck through for history; the accurate scope follows.

# CORRECTION (2026-08-21)

The original text below claimed internal/hub/hub.go:1104 "logs a DISCARD when a WAL record carries
an attestation that cannot be used". That is wrong. There is NO attestation-related discard
anywhere in internal/hub/hub.go (the string "attestation" appears only around lines 1428/1437/
1795/1861, none of which is a recovery discard). The proof_cmd previously stored named
TestMalformedOriginAttestationIsDiscardedLoudlyAndTheBusStarts in ./cmd/agent-bus -- that test does
not exist anywhere in the repo; running it reports VACUOUS, not FAIL.

What hub.go:1104 actually is: the UNDECODABLE MESSAGE RECORD discard, inside func (h *Hub) Apply on
the store.Decode error path -- "DISCARDING a message record that could not be decoded during
recovery; it is not in this bus's history and will not be delivered". It is guarded by
`if h.undecodableMessages <= maxDecodeFailuresLogged` (cap = 8, hub.go:64), with a one-shot
cap-reached line at hub.go:1110 ("further undecodable message records will NOT be logged
individually...").

The real, verified gap (grepping every DISCARDING line in internal/hub against every *_test.go in
the repo):
  - hub.go:1104 decode-failure discard -- NO test anywhere, positive or negative.
  - hub.go:1110 cap-reached one-shot line -- NO test anywhere.
  - hub.go:1126 apply-failure discard -- the string is pinned as const discardOnApplyLine at
    internal/hub/outoforder_poison_sign1fu_test.go:56, but its ONLY use (line 208) asserts the line
    is ABSENT. So there is no test that this line is ever EMITTED or names the record actionably --
    a weaker position than "tested".

By contrast the discard lines in internal/auth, internal/invite and internal/relay ARE tested, so
this gap is specific to internal/hub.

# Scope (as corrected)

This task covers the hub.go:1104 decode-discard branch together with its cap (hub.go:1110), and the
invariant-6 requirement that the bus still reaches a running state despite the discard. hub.go:1126
(needs a positive-assertion test, currently only proven absent) and the further untested hub-plane
discard lines (hub.go:1187, hub.go:1207, hub.go:1240, mint.go:559, mint.go:567, roster.go:351) are
OUT of scope here and tracked separately in RELAY-52-FU-HUBDISCARDS.

Write a test that:
  - constructs a WAL containing a message record that store.Decode cannot decode
  - starts the bus against it
  - asserts the bus REACHES A RUNNING STATE (invariant 6: recovery always reaches a running server)
  - asserts the discard is logged, and that the line NAMES the record specifically enough to act on
  - optionally covers the cap-reached one-shot line at hub.go:1110 (>8 undecodable records)

# Standard of evidence

The test must be observed RED before it is trusted -- mutate the discard log line away (or the cap
line) and confirm the test fails. A test asserting a log line, that was never seen failing, proves
nothing: log assertions are unusually prone to matching something incidental elsewhere in the
output.

Note the related pattern found repeatedly on 2026-08-16: three separate guards written specifically
to catch a defect could not fire (ACK-4 fixtures too short to reach their boundary; a SIGCOPY field
map the assertions could not see; IDEM-19's TestSweepIsNotOccupancyLinear asserting sweptEntries==0
so it only ran the case where nothing expired). Mutation found all three; review found none.

# Related
  RELAY-48 (9887b0eb) -- the durability fix that surfaced this.
  RELAY-52-FU-HUBDISCARDS -- follow-up covering the remaining untested hub/mint/roster discard
  lines out of scope here.

---

# ORIGINAL TEXT (2026-08-16), PREMISE CORRECTED ABOVE -- kept for history only, do not act on it

# The gap

internal/hub/hub.go:1104 logs a DISCARD when a WAL record carries an attestation that cannot be
used. NOTHING IN THE REPO TESTS THAT LINE. No test writes a record with a malformed attestation and
asserts that the bus BOOTS and LOGS the discard.

# Why it matters

Invariant 6 is explicit: recovery ALWAYS reaches a running server, damaged records are discarded and
the bus starts, but EVERY DISCARD MUST BE LOGGED LOUDLY AND SPECIFICALLY -- silent discard IS the
defect. The whole point is that the log is the only evidence a record was dropped.

So this is not "a missing test for a rare branch". It is the ONE observable that invariant 6 requires
to exist, unverified. If that line ever stops firing -- or fires without the detail an operator needs
-- nothing catches it, and the failure is by construction invisible.

# Why it did NOT block RELAY-48

The reviewer ranked it below the doc sweep and was right to: the Apply arm is PRE-EXISTING and
UNMODIFIED by RELAY-48. RELAY-48 made attestations mandatory on the ingest path, which is what
brought attention to the branch; it did not create or change it. Blocking a durability fix on a
pre-existing untested branch would have been the wrong trade.

The store-level half of B2 DID land in full and is good: internal/store/originattestation_relay48_test.go
at :165, :283 and :408 is a table test over the refusal cases.

# Standard of evidence (original)

The test must be observed RED before it is trusted -- mutate the discard log line away and confirm
the test fails. A test asserting a log line, that was never seen failing, proves nothing: log
assertions are unusually prone to matching something incidental elsewhere in the output.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [ACK-4](../../ACK/ACK-4--aeb32123/task.md) — ACK-4: ACK/NACK authorization, anti-forgery and privacy review implementation (done)
- [IDEM-19](../../IDEM/IDEM-19--82b79094/task.md) — IDEM-19: expiry-queue compaction is O(retained) -- 48.4s vs 32ms measured, on the every-s… (done)
- [RELAY-48](../RELAY-48--9887b0eb/task.md) — RELAY-48: onward relay is NOT crash-safe -- a pending onward hop is durably ABANDONED at… (done)
- [RELAY-52-FU-HUBDISCARDS](../RELAY-52-FU-HUBDISCARDS--d2cad9e7/task.md) — RELAY-52-FU-HUBDISCARDS: remaining untested hub/mint/roster discard-and-recovery log lines (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [RELAY-52-FU-HUBDISCARDS](../RELAY-52-FU-HUBDISCARDS--d2cad9e7/task.md) — RELAY-52-FU-HUBDISCARDS: remaining untested hub/mint/roster discard-and-recovery log lines (done)
- [dd2cdc20-8920-4e5b-bf0a-668f439cc3a6](../../UNASSIGNED/Reservation-counters-silently-drift-stale-and-hand-out-C--dd2cdc20/task.md) — Reservation counters silently drift stale and hand out COLLIDING task keys (RELAY, DOCS,… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
