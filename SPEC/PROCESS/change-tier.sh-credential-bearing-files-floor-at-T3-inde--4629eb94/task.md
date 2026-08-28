# change-tier.sh: credential-bearing files floor at T3, independent of every other signal

| Field | Value |
| --- | --- |
| Public id | `4629eb94-5ddb-4acb-98a1-125230ca5afe` |
| Key | _(null in the export)_ |
| Epic | [PROCESS](../epic.md) |
| Status | todo |
| Priority | P1 |
| Component | process |
| Section | backlog |
| Tags | — |
| Created | 2026-08-22T09:26:54.063846+00:00 |
| Updated | 2026-08-22T09:26:54.063846+00:00 |
| Completed | — |

## Proof command

```sh
bash scripts/proof-check.sh 'bash scripts/change-tier_test.sh'
```

## Description

**T-19 in the PROCESS tiered-chain epic.** Filed 2026-08-22 (coordinator, via spec-keeper).

Add a signal to `scripts/change-tier.sh`:

> **A file that READS, WRITES or TRANSPORTS credentials floors at T3** -- independent of every other
> signal, and in the NEVER-AUTO-LOWER override set. No other signal can bring it below T3, and the
> implementer may not lower it; lowering needs recorded reviewer sign-off like any other floor.

**Rationale, and it must be recorded in `docs/CHANGE-TIERS.md` next to the signal, because it is why
this is its own signal rather than a row in the plane map:** this is the class where a miss is
**UNRECOVERABLE**. A leaked credential is not fixed by a later commit. Almost everything else the
classifier sorts -- a wrong tier on a handler, a missed test deletion, a mis-sorted doc -- is
repairable by the next commit. A credential that reached a log, a mirror, a task description or a
remote is spent, and revoking it is a separate act of work that the tier scheme cannot force. That
asymmetry, not the frequency of the change, is what earns a dedicated signal.

**Known members, to SEED the rule -- NOT an exhaustive list.** State the PRINCIPLE first and label
the list non-exhaustive; `PITFALLS.md` section 8.2 warns explicitly that path tables go stale and
invite "my new file is not on it":

- `scripts/spec-cloud.sh` (already floored at T3 by T-01 ratification R6; this signal generalises it)
- anything referencing `SPEC_CLOUD_CREDS`
- the Cognito token cache path that `scripts/spec-cloud.sh` uses
- `internal/auth`
- `internal/invite`
- `internal/buscert`
- anything handling session tokens, bearer tokens, or private key material

**Detection, and it is cheap:** path membership **UNION** a content match on the TOUCHED DIFF for
credential tokens -- `SPEC_CLOUD_CREDS`, `Authorization`, `Bearer`, `password`, `secret`,
`PRIVATE KEY`, `token`. The union is the point: the path half catches the known files, the content
half catches the file nobody has added to the list yet.

**BIAS TO OVER-TRIGGER. This is the one signal where noise is clearly the right trade.** T-01's
standing caveat -- that a signal firing on nearly everything is noise, and noise trains agents to
lower -- is acknowledged and deliberately overridden HERE and only here, on the unrecoverability
argument above. Say so explicitly in the doc so a later agent does not "tune" it down as an
inconsistency.

**Inherits F1/F2 from T-03 (b2567ffd-190d-4aff-8cc2-f6a2eb2d613e), like every path-classifying
signal:**
- consume the `git status --porcelain --no-renames` file set, never `git diff --name-only`
  (F1: a rename otherwise arrives as one `R old -> new` line and only one half is classified);
- **never treat an empty match set as low-risk** (F2: `git status --porcelain -- <pathspec matching
  nothing>` prints nothing and exits 0). "MEASURED T0" and "COULD NOT MEASURE" are different
  outcomes; the second is an error exit, not a tier.

**Proof:** `bash scripts/proof-check.sh 'bash scripts/change-tier_test.sh'` with three new fixtures:
1. an edit to `scripts/spec-cloud.sh` -- expect **T3**;
2. a `SPEC_CLOUD_CREDS` reference ADDED to an otherwise unrelated file -- expect **T3** (this is the
   content half; the path half cannot see it);
3. a RENAME of a credential-bearing file -- both halves classified, expect **T3**.

Each fixture must be shown **RED before the signal is implemented**, and the RED output quoted in
the task's `kind=report`. A proof never observed failing is not evidence (`PITFALLS.md` section 2).

**BLOCKED BY T-03 (b2567ffd-190d-4aff-8cc2-f6a2eb2d613e)** -- it lands the harness, the diff basis
and the exit-code contract this signal plugs into.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [b2567ffd-190d-4aff-8cc2-f6a2eb2d613e](../scripts-change-tier.sh-diff-basis-contract-output-format--b2567ffd/task.md) — scripts/change-tier.sh: diff-basis contract, output format, exit codes, and signal 1 (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [b2567ffd-190d-4aff-8cc2-f6a2eb2d613e](../scripts-change-tier.sh-diff-basis-contract-output-format--b2567ffd/task.md) — scripts/change-tier.sh: diff-basis contract, output format, exit codes, and signal 1 (todo)
- [c65a5051-678c-487c-bdae-37183e01f049](../scripts-spec-cloud.sh-a-caller-supplied-w-breaks-status--c65a5051/task.md) — scripts/spec-cloud.sh: a caller-supplied -w breaks status detection and makes a 200 exit 5 (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
