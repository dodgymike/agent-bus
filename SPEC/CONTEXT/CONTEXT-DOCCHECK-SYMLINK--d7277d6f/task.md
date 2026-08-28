# CONTEXT-DOCCHECK-SYMLINK: physical (realpath) containment for cmd_section -- a symlink still escapes the repo

| Field | Value |
| --- | --- |
| Public id | `d7277d6f-6a0a-484a-8cd9-cf5f5247446c` |
| Key | CONTEXT-DOCCHECK-SYMLINK |
| Epic | [CONTEXT](../epic.md) |
| Status | todo |
| Priority | P2 |
| Component | TOOLING |
| Section | backlog |
| Tags | — |
| Created | 2026-08-21T19:43:33.050765+00:00 |
| Updated | 2026-08-21T19:43:33.050765+00:00 |
| Completed | — |

## Proof command

```sh
bash -c 'set -e
if [ ! -f scripts/doc-check.sh ]; then echo "doc-check.sh not tracked yet -- proof not runnable until CONTEXT-DOCCHECK lands" >&2; exit 3; fi
T=$(mktemp -d); git archive HEAD | tar -x -C "$T"
if [ ! -f "$T/scripts/doc-check.sh" ]; then echo "doc-check.sh not tracked at HEAD -- proof not runnable until CONTEXT-DOCCHECK lands" >&2; exit 3; fi
O=$(mktemp -d)
printf "# Outside\n\noutside-the-repo-needle\n" > "$O/outside.md"
ln -s "$O/outside.md" "$T/escape-symlink.md"
cd "$T"
if bash scripts/doc-check.sh section escape-symlink.md Outside outside-the-repo-needle; then echo SYMLINK_ESCAPE_STILL_OPEN >&2; exit 1; fi
echo SYMLINK_CONTAINMENT_HOLDS'
```

## Description

Follow-up to CONTEXT-DOCCHECK (b3b28f45-54b3-4d0e-bde7-933c9c3923b2). scripts/doc-check.sh's cmd_section validates its file argument with path_is_contained, a LEXICAL check: it rejects an absolute path and any ".." traversal, and two independent review gates on 2026-08-21 confirmed both of those routes now FAIL. But the check is lexical, not physical: a symlink committed inside the repo, whether its target is written as a relative or an absolute path, passes path_is_contained and section_range then reads straight through it to whatever the symlink points at, outside the repo tree.

Concrete harm, specific to this repo's own verification discipline: CLAUDE.md's mandated verify step extracts HEAD into a clean overlay with `git archive HEAD` specifically so a proof cannot see uncommitted text. A proof_cmd invoking `doc-check.sh section` on a path that is, or passes through, a symlink committed inside that overlay and pointing back out at the live worktree (for example two directories up at CLAUDE.md) would be read and could report PASS on text that was never committed -- defeating the exact protection the clean-overlay rule exists to provide. That is a false PASS on the trust path for whether a task's documented change is actually done.

Why this is not urgent enough to have blocked CONTEXT-DOCCHECK's own landing (both review gates agreed): HEAD tracks zero symlinks today, so the gap is latent in exactly the same sense as the setext-heading, indented-ATX and unbalanced-fence limits the script's own header already discloses. Exploiting it needs a symlink actually COMMITTED to the repo whose target is an absolute path on some developer's machine (or a relative path that happens to still resolve for whoever runs the proof) -- which is conspicuous in review, and the overlay directory doc-check.sh's own tests build is randomly named per run, so a committed symlink cannot be pre-aimed at it.

Scope: add PHYSICAL containment (realpath-based) for cmd_section's file argument, on top of -- not instead of -- the existing lexical check, which already closes every non-symlink route and should stay as the cheap first gate. The new check must resolve the argument's real path and confirm it still sits under the repo root even when a symlink is on the path.

Portability constraint, because it is exactly the trap this file's own header already warns about elsewhere: realpath's availability and exact behaviour (canonicalization of a dangling symlink, -e/-m style semantics, GNU vs BSD flag differences) is NOT uniform, and this project's runtime target is a container (CLAUDE.md, "Runtime target: Docker Compose"), not this workstation. The fix must not silently no-op in an environment where realpath is absent, behaves differently, or errors on an edge case -- a containment check that silently passes is a WORSE regression than the lexical check it replaces, which is the same lesson this file's own comments already draw from the AMBIGUOUS-heading and is_uint bugs. If no portable route exists, fail closed (refuse the file) rather than skip the check.

RED verified 2026-08-21 (spec-keeper filing) against the live, currently-untracked scripts/doc-check.sh: in a throwaway copy, a symlink with a relative target and a second with an absolute target, each pointing at a file with a real heading and needle that lives entirely outside the repo copy, both PASSED with exit 0 and reported the needle found inside the section -- lexical containment and existence are both satisfied by the symlink itself, and section_range then reads straight through it. The same scenario against the existing lexical-only check reproduces on demand; see the proof_cmd notes below for how it is scoped.

proof_cmd is scoped to run once CONTEXT-DOCCHECK lands and scripts/doc-check.sh is tracked -- it is not, yet. Until then the stored proof_cmd exits 3 with an explicit "not tracked yet" message, which is an honest non-runnable signal rather than a vacuous pass; that was confirmed by running it against current HEAD on 2026-08-21. Once tracked but before this fix, the same proof_cmd exits 1 with a diagnostic (escape still open) -- confirmed by running it against a copy of the current lexical-only implementation. Once the physical-containment fix lands it must exit 0 for the identical setup.

Chain: this ships a shell script change -- reviewer + security, not documentation-only, matching CONTEXT-DOCCHECK's own chain note.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CONTEXT-DOCCHECK](../CONTEXT-DOCCHECK--b3b28f45/task.md) — CONTEXT-DOCCHECK: doc-check.sh -- the instrument every other proof in this epic depends on (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [29ac1f66-efb9-4b05-afda-a2775e50f1c6](../gen-spec-mirror-guard-trips-on-column-0-shell-in-two-CON--29ac1f66/task.md) — gen-spec-mirror guard trips on column-0 shell in two CONTEXT-epic task descriptions, bloc… (todo)
- [CONTEXT-DOCCHECK-FU-COUNTPIN](../CONTEXT-DOCCHECK-FU-COUNTPIN--5f3a3cd6/task.md) — CONTEXT-DOCCHECK-FU-COUNTPIN: the outer doc-check.sh --selftest run's assertion count is… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
