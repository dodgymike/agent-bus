# invite mint bypasses the data-directory permission gate entirely -- the invite blob is the trust anchor, so this is worse than the file substitution the gate closes

| Field | Value |
| --- | --- |
| Public id | `c47379ae-9873-4800-a442-03e34a7f1294` |
| Key | _(null in the export)_ |
| Epic | [CORE](../epic.md) |
| Status | deferred |
| Priority | P0 |
| Component | security |
| Section | backlog |
| Tags | — |
| Created | 2026-08-08T15:32:06.900163+00:00 |
| Updated | 2026-08-08T15:44:56.201319+00:00 |
| Completed | — |

## Proof command

```sh
grep -q 'enforceDataDirPermissions(dataDir, lg)' cmd/agent-bus/invite.go && go test -race -count=1 -run TestInviteMintRefusesAnOtherWritableDataDir ./cmd/agent-bus
```

## Status note

DEFERRED 2026-08-08 (user decision, recorded by spec-keeper -- see full text on RELAY epic notes / pending DECISIONS.md transfer): three-bus laptop<->internet-machine<->this-machine topology, every inter-bus link an SSH tunnel, no bus ever publicly listening, user is sole operator/local user of all three machines. Local-attacker scenario is explicitly OUT OF SCOPE while that holds; this finding requires local write access to the data directory to exploit. Deferral is TIME-BOXED to 'until end-to-end relay is running', not indefinite, and REVERSES immediately if any bus is exposed on a real interface, a second local user is added to any machine, or an uncontrolled peer bus is admitted.

## Description

SECURITY GATE FINDING (HIGH) against the data-dir permission gate shipped under be447589-6583-4d5c-a9d4-ec9d9fef0f1c, committed at 217a3c0. PROVED BY RUNNING CODE, end to end, not by reading.

MECHANISM. `enforceDataDirPermissions` is wired into `run()` ONLY. Re-verified at HEAD 16da89f by spec-keeper: the sole call site in the whole tree is cmd/agent-bus/main.go:299 (`grep -rn enforceDataDirPermissions cmd/agent-bus/` returns the definition at datadirperm.go:88 and that one call). `mintInvite` (cmd/agent-bus/invite.go:448-510) stats the dir (invite.go:455), checks IsDir (:464), then takes the lock, replays and APPENDS to the WAL, and publishes the bus certificate fingerprint -- with no permission check anywhere on that path.

MEASURED. On a real 0777 data dir: the server REFUSES to start, but `agent-bus invite mint` exits 0, mutates bus.wal (md5 changed) and emits zero warning.

THE COMPLETED ATTACK CHAIN (the reason this is P0 and not tidy-up). The bus id is readable from the world-readable bus-tls.crt (0644), so an attacker mints a same-CN certificate, drops it plus keys into the 0777 dir, and the operator's next `invite mint` printed a fingerprint BYTE-IDENTICAL to the attacker's certificate. Under invariant 11 the invite blob is the TRUST ANCHOR -- "whoever can substitute an invite can point an agent at a bus of their choosing" -- so the outcome is strictly worse than the file substitution the gate was built to close. The gate refusing `run()` while `invite mint` sails through on the same directory is the whole defect: one command enforces the trust boundary and the other one, which MINTS THE TRUST ANCHOR, does not.

MINIMAL FIX (given by the gate). Call `enforceDataDirPermissions(dataDir, lg)` in `mintInvite` after the `IsDir` check (invite.go:470) and BEFORE `checkBusIdentityPresent`. That placement preserves the existing property that a refusal writes nothing: it is still ahead of the lock and ahead of every write.

ALSO RECORDED, EXPLICITLY LOWER PRIORITY -- `healthcheck`. cmd/agent-bus/healthcheck.go takes -data-dir (:122) and reads only bus-tls.crt (:152); it takes no lock and mutates nothing, so an ungated healthcheck can only report a FALSE OK against a substituted certificate, not launder one. Wire the gate into it in the same change if that is cheap; it does not block this task and must not be used to widen it.

PROOF STATE OBSERVED (spec-keeper, HEAD 16da89f, not assumed): `bash scripts/proof-check.sh '<proof_cmd>'` -> verdict=FAIL, exit 1. RED, and RED FOR THE RIGHT REASON: the pin `grep -c 'enforceDataDirPermissions(dataDir, lg)' cmd/agent-bus/invite.go` returns 0, so the && short-circuits and the (not-yet-written) test never runs -- i.e. this proof is RED today rather than VACUOUS. The test half must ALSO be observed RED before the fix lands, or it proves nothing.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **follow-up of** [be447589-6583-4d5c-a9d4-ec9d9fef0f1c](../Enforce-data-directory-permissions-at-startup-and-bound--be447589/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [be447589-6583-4d5c-a9d4-ec9d9fef0f1c](../Enforce-data-directory-permissions-at-startup-and-bound--be447589/task.md) — Enforce data-directory permissions at startup, and bound the message-seq floor (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
