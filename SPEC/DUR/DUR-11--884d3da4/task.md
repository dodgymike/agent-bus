# DUR-11: Close security's two OPEN HIGH truncation holes -- anchor the tail veto on record INDEX, and stop truncating a checksum-failing LAST acknowledged record

| Field | Value |
| --- | --- |
| Public id | `884d3da4-bceb-4ac2-93a2-e147c77f9dca` |
| Key | DUR-11 |
| Epic | [DUR](../epic.md) |
| Status | done |
| Priority | P0 |
| Component | durability |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T14:24:29.382823+00:00 |
| Updated | 2026-08-07T12:42:32.059333+00:00 |
| Completed | 2026-08-07T12:42:32.059316+00:00 |

## Proof command

```sh
go test -race -run TestWALResyncSurvivesALargeIndexHole ./internal/wal
```

## Status note

OPEN ITEM ONLY: the security-blocking stage-2 resync fix (index-density-hole data-loss BLOCKER) is committed and regression-tested (salvage.go resyncFrom stage 2, TestWALResyncSurvivesALargeIndexHole, both confirmed present at HEAD) and reviewer P1-1/P1-3/P1-4 are independently confirmed fixed in committed code (main.go:236-250 corrected comment, doc.go cascade caveat, main.go quarantined/discard_count/discarded_bytes startup fields) with P1-2 correctly carved out to its own tracked task (5b178dde, DUR-11-FU-CONTRACTS) -- but no reviewer or security agent has posted a post-fix PASS kind=response confirming the BLOCKER is actually cleared, so the mandated gate has not run; do not complete until one does.

## Description

RE-SCOPED 2026-08-02 BY THE USER DECISION "THE BUS ALWAYS RESTARTS" (DECISIONS.md, 2026-08-02, section 1).
STATUS DELIBERATELY UNCHANGED -- a feature-runner is in flight against this task under exactly the policy
below. Read this whole section before the historical text further down, which was written against the
OLD refuse-to-start policy and is retained only as the record of how the findings were discovered.

THE POLICY THIS TASK NOW IMPLEMENTS.

(a) FINDING (a) STANDS AS A REAL BUG, unchanged and still P0. One damaged record must never cause the
    MASS DELETION of later records that are themselves INTACT. The veto's forward search must be
    ANCHORED ON RECORD INDEX, not on end-of-file. Security's probe: one flipped bit in a mid-file
    length field plus one junk byte at EOF deleted 8 committed records, NextIndex 41 -> 33, silently.
    Anchoring on EOF gives ZERO protection in precisely the case RepairTail exists for -- a genuine
    torn tail -- because the veto only fires when the file ends exactly on a record boundary.

(b) FINDING (b) IS NO LONGER AN INVARIANT-4 VIOLATION. Discarding a checksum-failing LAST record is
    now SANCTIONED behaviour: "always be able to restart, prefer to discard messages and/or
    corruption, with logging". Invariant 4 is narrowed accordingly -- acknowledged data may be
    discarded when it is found corrupt; we do not lose acknowledged data through our OWN write path,
    but we will not hold the bus hostage to damaged media.
    THE REMAINING DEFECT IN (b) IS THE SILENCE AND THE FALSE DOC COMMENTS, NOT THE DISCARD.
    - Every discard must be OBSERVABLE: a specific log record naming what was discarded (offset,
      record index, record type, byte count, and why), not a bare boolean or a silent success.
    - The doc comments that claim the discard is "provably" of a never-fsynced record are FALSE and
      must go. Reviewer flagged them as P0 on DUR-4; the implementer already narrowed the worst of
      them at c362152, but the claim must not survive anywhere: "the frame is torn" does NOT imply
      "its fsync never completed". The code and the comments must agree.
    There is no longer a design call to make here and no DECISIONS.md entry is owed for it -- the user
    has decided. Do NOT re-open the refuse-vs-truncate debate.

(c) ADDED SCOPE -- CONVERT EVERY DAMAGE-CLASS REFUSAL INTO DISCARD + SPECIFIC LOG + CONTINUE.
    Recovery must ALWAYS reach a running server. Sweep internal/wal (RepairTail, truncatableTail,
    inspectTail, and every error path that today propagates out of wal.Open as fatal, plus the
    fatal-on-repair-refusal handling in cmd/agent-bus/main.go) and turn each DAMAGE-class error into:
    discard the damaged record(s), log loudly and specifically what was discarded, keep running.
    Truncation is no longer restricted to a verified-corrupt TAIL (invariant 6 narrowed): damaged
    records ANYWHERE may be discarded -- with a log entry EACH.
    THE LINE, AND IT MATTERS: NON-DAMAGE ERRORS STAY FATAL. Permission denied, an I/O failure, the
    data-directory lock already held, a missing/unwritable data dir -- these are not damaged records
    and must still refuse to start with a clear operator message and a non-zero exit. Do not turn an
    unreadable disk into a silently empty bus. Note cmd/agent-bus/wal_startup_test.go currently has
    TestServerOpensWALOnStartRefusesACorruptLog, which asserts the OLD policy for a garbage file
    HEADER -- decide explicitly whether a bad file header is damage (discard/reinitialise + log) or a
    non-damage refusal, say which in the commit message, and make the test assert whichever you chose.
    This also removes the permanent refuse-to-start DoS, and with it the operator escape hatch that
    was previously recommended: always-restart IS the escape hatch (DUR-4-FU-TOOLING is superseded).

OUT OF SCOPE, EXPLICITLY: the CHECKSUM SCHEME and the ON-DISK FORMAT. CRC32C is being replaced by an
HMAC-SHA256 keyed MAC under a separate P0 task holding the reserved ondisk-format-version=2. Do not
touch format.go's checksum construction, do not bump FormatVersion, and do not try to fix the
GF(2)-linearity forgery here. Expect the torn-tail heuristic to get SIMPLER, not more complex, once a
strong MAC can distinguish damage from truth -- so do not build elaborate new heuristics that the MAC
task will have to unwind.

TESTS. Keep TestCrashInjectionSingleBitCorruptionSweep (internal/wal/crash_injection_test.go) green
and EXTEND the net: the torn-tail-PLUS-mid-file-corruption combination that finding (a) exploits has
no coverage today, and every new discard path needs a test asserting the SERVER STILL STARTS and the
specific log line was emitted. A discard with no log line is the bug, so assert the log, not just the
absence of an error. Needs the mandated reviewer AND security gates.

--- HISTORICAL TEXT, retained as the discovery record. Its "DESIGN CALL REQUIRED" paragraph and its
--- refuse-to-start framing are SUPERSEDED by the policy above. ---

FILED BY spec-keeper so two DEMONSTRATED, STILL-OPEN silent-data-loss findings are not lost inside a task that is already marked done. Source: the security agent's kind=response on DUR-4 (PASS-WITH-CONCERNS, 2026-08-02T14:13:06), which was posted BEFORE DUR-4 was flipped done and was never resolved or waived. Both findings were reproduced with probes against a /tmp copy, not argued from the code. CRITICALLY, security re-ran its probes against the WORKING-TREE fix (the laterRecordInTail veto that DUR-10 covers) and both holes SURVIVE it -- DUR-10 is a strict improvement but does NOT close these.

FINDING (a) HIGH -- the veto's anchor is the wrong thing. laterRecordInTail only fires when the file ends EXACTLY on a record boundary, i.e. when there is NO torn tail. Probe: one flipped bit in a mid-file length field PLUS one junk byte appended at EOF (or one byte truncated off) => 8 committed records deleted, NextIndex 41 -> 33, no error, Open+Replay succeed silently. RECOMMENDED FIX (security's): anchor the forward search on the record INDEX, not on end-of-file.

FINDING (b) HIGH -- a checksum-failing LAST record is assumed torn. A single flipped bit in the PAYLOAD of the final record -- a complete, fsynced, ACKNOWLEDGED record -- is byte-indistinguishable from a torn write and is truncated away. Probe: replay applied 2 -> 1, NextIndex 5 -> 4.

SEQUENCING: DUR-10 is now DONE (review debt paid; reviewer CHANGES-REQUIRED comment-only, security PASS-WITH-CONCERNS, comment fixes landed at c362152). Proof command validated by scripts/proof-check.sh: verdict=PASS class=test tests_run=60 top_level=17 skipped=1 failed=0 empty_pkgs=0 (re-run 2026-08-02 against HEAD) -- it is a real net today, and must be EXTENDED by this task rather than merely kept passing.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


_None recorded._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [DUR-10](../DUR-10--bab09b2e/task.md) — DUR-10: Review the RepairTail truncation veto -- half is already in \`main\` UNREVIEWED (la… (done)
- [DUR-11-FU-CONTRACTS](../../DOCS/DUR-11-FU-CONTRACTS--5b178dde/task.md) — DUR-11-FU-CONTRACTS: CONTRACTS.md still documents the reverted refuse-to-start WAL policy… (todo)
- [DUR-4](../DUR-4--59c36769/task.md) — DUR-4: Corrupt-tail detection & truncation (superseded)
- [DUR-4-FU-TOOLING](../DUR-4-FU-TOOLING--26c2ce16/task.md) — DUR-4-FU-TOOLING: Operator tooling for a WAL that refuses to start (superseded)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [804fa84c-e97b-4737-8866-801f87468da4](../Document-what-the-bus-does-with-an-UNKNOWN-WAL-record-ty--804fa84c/task.md) — Document what the bus does with an UNKNOWN WAL record type -- the answer is now discard-a… (todo)
- [9160ba8d-09f8-4510-bd0c-dcf1b22b82a5](../Startup-summary-silently-omits-whole-log-quarantine-quar--9160ba8d/task.md) — Startup summary silently omits whole-log quarantine (quarantined/discard_count/discarded_… (done)
- [DUR-10](../DUR-10--bab09b2e/task.md) — DUR-10: Review the RepairTail truncation veto -- half is already in \`main\` UNREVIEWED (la… (done)
- [DUR-11-FU-CONTRACTS](../../DOCS/DUR-11-FU-CONTRACTS--5b178dde/task.md) — DUR-11-FU-CONTRACTS: CONTRACTS.md still documents the reverted refuse-to-start WAL policy… (todo)
- [DUR-11-FU-STAGE2SHORTCIRCUIT](../DUR-11-FU-STAGE2SHORTCIRCUIT--b792fa34/task.md) — DUR-11-FU-STAGE2SHORTCIRCUIT: resyncFrom never runs the sound stage-2 scan after a stage-… (todo)
- [DUR-12](../DUR-12--cbc9ab0c/task.md) — DUR-12: Replace CRC32C with an HMAC-SHA256 keyed MAC (ON-DISK FORMAT CHANGE, reserved ond… (done)
- [DUR-4](../DUR-4--59c36769/task.md) — DUR-4: Corrupt-tail detection & truncation (superseded)
- [DUR-4-FU-DECISIONS](../DUR-4-FU-DECISIONS--180f11f8/task.md) — DUR-4-FU-DECISIONS: record the SHIPPED damage-class taxonomy in DECISIONS.md -- which cla… (todo)
- [DUR-4-FU-DOCS](../DUR-4-FU-DOCS--0b6d5c11/task.md) — DUR-4-FU-DOCS: state invariants 4/6 as explicit NARROWINGS + document the WAL recovery AP… (todo)
- [DUR-4-FU-TOOLING](../DUR-4-FU-TOOLING--26c2ce16/task.md) — DUR-4-FU-TOOLING: Operator tooling for a WAL that refuses to start (superseded)
- [c3a27591-5b0c-44c0-ac68-94072f3c3fc2](../RESOLVED-2026-08-02-SUPERSEDED-CRC32C-tail-repair-proofs--c3a27591/task.md) — \[RESOLVED 2026-08-02 -- SUPERSEDED\] CRC32C tail-repair proofs are remotely forgeable =&gt; p… (superseded)
- [e120153b-9d8a-4b6a-bd4e-89431954496b](../Fix-WAL-recovery-reissuing-a-discarded-tail-record-index--e120153b/task.md) — Fix WAL recovery reissuing a discarded tail record index (invariant 1 violation, NOT a na… (in_progress)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
