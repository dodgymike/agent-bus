# The data-dir permission gate checks MODE but not OWNERSHIP, and follows symlinks -- and it is now the SOLE defence for invariant 1 against a downward seq-floor forge

| Field | Value |
| --- | --- |
| Public id | `3e43c52c-ae62-4b8c-aabb-1b9f7f62d82f` |
| Key | _(null in the export)_ |
| Epic | [CORE](../epic.md) |
| Status | deferred |
| Priority | P1 |
| Component | security |
| Section | backlog |
| Tags | — |
| Created | 2026-08-08T15:32:07.336110+00:00 |
| Updated | 2026-08-08T15:44:57.134056+00:00 |
| Completed | — |

## Proof command

```sh
grep -q 'Lstat' cmd/agent-bus/datadirperm.go && go test -race -count=1 -run 'TestRunRefusesASymlinkedDataDir|TestRunRefusesAForeignOwnedDataDir' ./cmd/agent-bus
```

## Status note

DEFERRED 2026-08-08 (user decision, recorded by spec-keeper -- see full text on RELAY epic notes / pending DECISIONS.md transfer): three-bus laptop<->internet-machine<->this-machine topology, every inter-bus link an SSH tunnel, no bus ever publicly listening, user is sole operator/local user of all three machines. Local-attacker scenario is explicitly OUT OF SCOPE while that holds; this finding requires local write access to the data directory to exploit. Deferral is TIME-BOXED to 'until end-to-end relay is running', not indefinite, and REVERSES immediately if any bus is exposed on a real interface, a second local user is added to any machine, or an uncontrolled peer bus is admitted.

## Description

FOUND INDEPENDENTLY BY BOTH SECURITY GATES on be447589-6583-4d5c-a9d4-ec9d9fef0f1c (committed 217a3c0). Two gates working separately corroborated this, which is why it is filed at P1 rather than as a nit.

MECHANISM, three parts, all at cmd/agent-bus/datadirperm.go:75-96.
(1) NO OWNERSHIP CHECK. `enforceDataDirPermissions` reads `info.Mode().Perm()` (datadirperm.go:88 onward) and nothing else. Re-verified at HEAD 16da89f by spec-keeper: `grep -rn 'Uid|Gid|Stat_t|Lstat' cmd/agent-bus/ internal/dirlock/` returns ZERO hits in the whole of both. A 0755 directory OWNED BY ANOTHER UID passes cleanly, and that owner can substitute every identity file.
(2) SYMLINKS ARE FOLLOWED. `os.Stat` and `os.Chmod` both follow symlinks, so an attacker with write access to the PARENT can replace the data dir with a symlink to a 0700 directory they own. PROVED: it starts silently and writes all ten identity files into the attacker's target.
(3) THE PARENT IS NEVER CONSIDERED. A 0777 non-sticky PARENT also starts silently. Renaming a directory is a permission on its PARENT, so the comment at datadirperm.go:75-96 claiming "every step below trusts that the files in this directory cannot be substituted by another local user" is FALSE in that case -- the directory itself can be swapped wholesale.

WHY THIS IS NOW URGENT RATHER THAN TIDY, and this is the part that changes its priority. One gate traced that `maxPlausibleSeqFloor` is ONE-DIRECTIONAL: it bounds only UPWARD forgery. A DOWNWARD forge is guarded by nothing but the unkeyed digest, and is masked by the log EXCEPT for the minted-but-unspent tail -- i.e. up to `MintBatchSize` = 256 already-signed sequences reissued. That is a genuine INVARIANT 1 violation whose SOLE remaining defence is this directory permission gate. The gate is therefore LOAD-BEARING FOR INVARIANT 1, not defence in depth, and every hole in it is a hole in invariant 1.

SCOPE / FIX. cmd/agent-bus/datadirperm.go. Check the owning uid (refuse a foreign-owned dir rather than repair it -- a non-owner cannot be fixed by chmod). Use `os.Lstat` to detect a symlinked data dir, and consider the parent's mode. NOTE THE KNOWN REGRESSION RISK, raised by the security gate that reviewed the original fix and the reason it was deferred there: an ownership check carries a Docker bind-mount regression risk, and two bricking refusals were already produced during that task. Whatever is chosen must be tested against a bind-mounted volume before it lands, and the refuse-vs-warn choice recorded in DECISIONS.md.

RELATED, NOT DUPLICATES: 6c482cc0-ce83-49e9-a7ff-f8575795cb39 (wal.OpenWriter/RepairTail open bus.wal without O_NOFOLLOW -- same class, different file, internal/wal); ae594fa8-03bb-4d51-aa31-641f5ddcae66 (RUN_DIR ownership/symlink in scripts/bus-serve.sh -- same class, different directory).

PROOF STATE OBSERVED (spec-keeper, HEAD 16da89f, not assumed): `bash scripts/proof-check.sh '<proof_cmd>'` -> verdict=FAIL, exit 1. RED, and RED FOR THE RIGHT REASON (`grep -c Lstat cmd/agent-bus/datadirperm.go` = 0, so the && short-circuits) -- RED today rather than VACUOUS. Both test halves must ALSO be observed RED before the fix.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [6c482cc0-ce83-49e9-a7ff-f8575795cb39](../../DUR/wal.OpenWriter-RepairTail-open-bus.wal-without-O_NOFOLLO--6c482cc0/task.md) — wal.OpenWriter/RepairTail open bus.wal without O_NOFOLLOW -- a planted symlink is followe… (todo)
- [ae594fa8-03bb-4d51-aa31-641f5ddcae66](../../AGENTIF/RUN_DIR-created-with-no-ownership-check-enables-binary-s--ae594fa8/task.md) — RUN_DIR created with no ownership check -- enables binary swap and pidfile symlink attack (todo)
- [be447589-6583-4d5c-a9d4-ec9d9fef0f1c](../Enforce-data-directory-permissions-at-startup-and-bound--be447589/task.md) — Enforce data-directory permissions at startup, and bound the message-seq floor (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
