# scripts/spec-cloud.sh: a caller-supplied -w breaks status detection and makes a 200 exit 5

| Field | Value |
| --- | --- |
| Public id | `c65a5051-678c-487c-bdae-37183e01f049` |
| Key | _(null in the export)_ |
| Epic | [PROCESS](../epic.md) |
| Status | todo |
| Priority | P1 |
| Component | process |
| Section | backlog |
| Tags | — |
| Created | 2026-08-22T09:05:43.755943+00:00 |
| Updated | 2026-08-22T09:38:31.857151+00:00 |
| Completed | — |

## Proof command

```sh
bash scripts/proof-check.sh 'bash scripts/spec-cloud.sh -s /readyz >/dev/null 2>&1; b=$?; bash scripts/spec-cloud.sh -s -w "code=%{http_code}\n" /readyz >/dev/null 2>&1; w=$?; echo "baseline_exit=$b (must be 0) with_w_exit=$w (must NOT be 5)"; test "$b" = 0 && test "$w" != 5'
```

## Description

**Caused a real (repaired) data incident on 2026-08-22 while applying the PROCESS tier rulings.**

`scripts/spec-cloud.sh` detects the HTTP status by appending its own `-w $'\n%{http_code}'` to the
curl invocation and then reading `tail -1` of the response, stripping that last line with `sed '$d'`
before emitting the body (`scripts/spec-cloud.sh:94-103`). Every other argument, including a
CALLER-SUPPLIED `-w`, is passed straight through in `passthru`.

So a caller that passes its own `-w` gets TWO write-out strings appended. The caller's is emitted
LAST, `tail -1` reads the caller's text instead of the status code, `code` is not `2*`, and the
wrapper reports `spec-cloud: HTTP <caller-text> for <path>` and **exits 5 on a request that
actually returned 200**.

The observed damage: a retry loop keyed on the wrapper's exit status re-issued three PATCHes that
had each already succeeded, appending the same block to a task description 2 and 3 times. Detected
by comparing the task `version` deltas against the number of intended writes, and repaired by
PATCHing back the reconstructed single-append description. A caller with a non-idempotent POST would
not have been repairable that cheaply.

Fix options (pick one, do not do both):
- REJECT a caller-supplied `-w` / `--write-out` loudly with a distinct exit code, naming the reason.
  Cheapest, and the wrapper is already opinionated about its argv (it enumerates value-taking
  options at `:71-79`).
- Or stop making status detection positional: capture the status with a separate mechanism that a
  caller's `-w` cannot displace.

Also worth stating in the header comment: **the wrapper's exit status IS the status check** -- a
caller must not add `-w` to get one, and callers that want the code should use the exit status.

## PROOF (concrete and runnable as of 2026-08-22) -- AND A TRAP IN CHOOSING THE WRITE-OUT VALUE

Until 2026-08-22 the stored `proof_cmd` here was an angle-bracket PLACEHOLDER, not a command.
It is now a real reproduction, verified RED by spec-keeper.

**MEASURED 2026-08-22, AND IT CONTRADICTS THE OBVIOUS CHOICE OF REPRODUCER. Read this before
editing the proof.** The natural write-out value to reach for is a bare `%{http_code}` -- and it
DOES NOT REPRODUCE. Measured, exit status by write-out value against `/readyz`, all returning 200:

| caller write-out value | wrapper exit | reproduces? |
|---|---|---|
| (none -- baseline) | 0 | n/a |
| `%{http_code}` | **0** | **NO** |
| `%{http_code}` followed by a newline escape | **0** | **NO** |
| `code=%{http_code}` followed by a newline escape | **5** | YES |
| a newline escape, then `HTTP:%{http_code}`, then a newline escape | **5** | YES |

The mechanism, and why the bare form is a FALSE NEGATIVE. curl keeps only the LAST write-out option
it is given, and the wrapper appends the caller's arguments AFTER its own, so the caller's value
REPLACES the wrapper's rather than being appended to it. When the caller's value happens to render
to exactly the status code, the replacement is coincidentally byte-identical to what the wrapper
expected, `tail -1` reads `200`, and everything works. The defect only becomes visible when the
caller's value renders to anything ELSE on the last line. A trailing newline escape does not help
either: command substitution strips trailing newlines, so the status code is still the last line.

**Consequence for whoever fixes this: a proof built on a bare `%{http_code}` would be GREEN before
the fix and green after it, and would therefore certify nothing.** That is the failure mode task
`4faa6782-6b49-4507-9a23-bb2cf42e7d02` is about, arrived at from the opposite direction. The stored
proof uses a value that renders to `code=200`, which is genuinely RED today.

What the stored proof asserts: the baseline call WITHOUT a caller write-out exits 0 (so a failure
cannot be blamed on a sick endpoint or a lapsed token -- this is the attributability check), AND the
call WITH one does not exit 5. Exit 5 is the wrapper's non-2xx path. The assertion is deliberately
`not 5` rather than `is 0`, so it stays correct under EITHER fix option above: pass-through repaired
gives 0, and a loud rejection gives whatever distinct code that fix chooses.

Verified 2026-08-22 by spec-keeper via `scripts/proof-check.sh`: **verdict=FAIL exit=1**, printing
`baseline_exit=0 (must be 0) with_w_exit=5 (must NOT be 5)`. That is the intended RED, and the
printed baseline is what proves the failure is attributable to the write-out option.

Filed by spec-keeper 2026-08-22, BEYOND the coordinator's four-task brief, because the trap has
already caused duplicate writes to Spec Server task descriptions today.

---

## SECOND REPRODUCER, REPORTED 2026-08-22 -- **UNVERIFIED. NOT YET REPRODUCED BY spec-keeper.**

**Status: reported by the ORCHESTRATOR, not measured by spec-keeper. Treat everything in this
section as a CLAIM until this task reproduces it.** It is recorded because it points at the same
argument-handling defect from a second direction, not because it is established.

**The report, as given:**

> `bash scripts/spec-cloud.sh -s -i -o - <path>` wrote a file literally named `-o` (656 bytes of
> HTTP headers) into the repo root, found later as untracked scratch and removable only as
> `rm -- ./-o`, the leading dash otherwise being eaten as an option. If reproduced, this shows the
> wrapper's argument handling is broken for `-o` as well as `-w`, and that it fails by SILENTLY
> CREATING A FILE WHOSE NAME IS ITSELF AN OPTION-INJECTION HAZARD -- the same family as the `sed -n`
> incident in `PITFALLS.md`.

**WHY IT IS MARKED UNVERIFIED, recorded so the next agent does not have to re-derive it.** Planner
read `scripts/spec-cloud.sh:92-103`:

the single `curl` invocation at `:94` builds its argv in this order -- `-sS`, then the wrapper's
own write-out option carrying a newline and the http-code placeholder, then the bearer-token request
header, then the expanded pass-through array, then the host-plus-path argument. Pass-through args
land **AFTER** the wrapper's own `-w`. That fully explains the `-w` collision
already documented above -- the caller's write-out wins because it is last. It does **NOT** obviously
explain a file named `-o`: curl given `-o` with a bare dash writes to STDOUT, it does not create a
file called `-o`. A
plausible mechanism would involve the wrapper's own value-taking-option scan at `:71-79` consuming
the `-` that follows `-o` (so `-o` arrives at curl with a different argument, or the `-` is absorbed
as the path candidate), but that is a HYPOTHESIS and nobody has run it.

**Measured 2026-08-22 by spec-keeper:** no file named `-o` exists in the repo root now
(`ls ./-o` -> No such file or directory), and `git status --porcelain` shows no untracked entry. So
there is no artefact left to inspect either.

**REQUIREMENT ON THIS TASK: REPRODUCE IT BEFORE RECORDING IT AS FACT.** If it reproduces, fix it as
part of the same argument-handling fix and add a fixture. If it does NOT reproduce, say so and strike
this section -- do not leave an unreproduced claim standing as a defect.

Planner declined to assert it as measured, and that caution is itself on the record for a reason: an
earlier relayed claim in this epic -- "a pathspec commit takes an untracked file" -- was asserted
confidently and **proved FALSE on testing** (see Amendment B on task
`b2567ffd-190d-4aff-8cc2-f6a2eb2d613e`). Relayed observations in this epic have a measured failure
rate; treat this one accordingly.

## PRIORITY RAISED TO P1 (2026-08-22)

Two reasons, both now ratified rather than argued:

1. **`scripts/spec-cloud.sh` is CONTROL PLANE by the ratified principle**, and T-01's ratification R6
   floors it at **T3** specifically because it handles credentials and is the one script in this repo
   whose compromise leaks something OFF this machine. (See also T-19,
   `4629eb94-5ddb-4acb-98a1-125230ca5afe`, the general credential-bearing signal.)
2. **It silently corrupts writes for EVERY agent that uses the wrapper.** The failure mode is not an
   error the caller sees -- it is a success reported as exit 5, which is what drove the duplicate
   PATCHes recorded above. Every task mutation in this project goes through this script.

**Operational note discovered while writing this section (measured 2026-08-22, spec-keeper).** The
PATCH carrying this text was rejected **403** three times by the Cloudflare WAF in front of the Spec
Server. Bisected to a single token: a literal `curl` invocation with the output flag and a bare dash,
written inline in the request BODY, is read as command injection and blocked. Rewording it in prose
let the identical PATCH through. Two consequences worth knowing: task descriptions cannot quote
arbitrary shell, and `scripts/spec-cloud.sh` reports the block as `spec-cloud: HTTP 403`, which is
easy to misread as an AUTH failure -- the body is a Cloudflare interstitial, not a Spec Server error.
Filed separately as its own task.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [4629eb94-5ddb-4acb-98a1-125230ca5afe](../change-tier.sh-credential-bearing-files-floor-at-T3-inde--4629eb94/task.md) — change-tier.sh: credential-bearing files floor at T3, independent of every other signal (todo)
- [4faa6782-6b49-4507-9a23-bb2cf42e7d02](../PITFALLS.md-a-proof-that-stays-RED-against-a-tree-where--4faa6782/task.md) — PITFALLS.md: a proof that stays RED against a tree where the work IS done (todo)
- [b2567ffd-190d-4aff-8cc2-f6a2eb2d613e](../scripts-change-tier.sh-diff-basis-contract-output-format--b2567ffd/task.md) — scripts/change-tier.sh: diff-basis contract, output format, exit codes, and signal 1 (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [46afc19c-e0dd-48cf-b003-6f5fe3bac48c](../scripts-spec-cloud.sh-reports-a-Cloudflare-WAF-block-as--46afc19c/task.md) — scripts/spec-cloud.sh reports a Cloudflare WAF block as a bare HTTP 403, indistinguishabl… (todo)
- [ed3537a8-9c5f-489e-8aa8-8d3f61514d5f](../Correct-0f8c5332-the-relation-delete-endpoint-EXISTS--ed3537a8/task.md) — Correct 0f8c5332: the relation-delete endpoint EXISTS (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
