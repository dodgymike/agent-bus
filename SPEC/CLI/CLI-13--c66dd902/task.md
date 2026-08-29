# CLI-13: TestCLIEnrolEndToEnd SIGTERMs the priming server before it installs a handler -- fails under load

| Field | Value |
| --- | --- |
| Public id | `c66dd902-9a9f-431c-896a-9724534755af` |
| Key | CLI-13 |
| Epic | [CLI](../epic.md) |
| Status | todo |
| Priority | P2 |
| Component | BE |
| Section | backlog |
| Tags | — |
| Created | 2026-08-16T13:22:56.653402+00:00 |
| Updated | 2026-08-16T13:22:56.653402+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -count=3 -run '^TestCLIEnrolEndToEnd$' ./cmd/agent-busctl
```

## Description

FILED 2026-08-16 by main. Diagnosed to root cause, not merely observed.

# The symptom

  --- FAIL: TestCLIEnrolEndToEnd (2.27s)
      enrol_test.go:88: the priming server exited badly: signal: terminated

Reproduced in a PRISTINE git-archive-HEAD overlay with no local changes, at load average 10.95.
The SAME test PASSES in the same overlay at load average ~3.2. It is load-SENSITIVE, and that
sensitivity is what makes it look environmental. It is not.

# The root cause -- a race the test loses under load

cmd/agent-busctl/enrol_test.go:85-88 does:

  waitForBusCertificate(t, dataDir, primeStderr)     // waits for the CERT FILE on disk
  _ = primeCmd.Process.Signal(syscall.SIGTERM)       // then SIGTERMs immediately
  if err := primeCmd.Wait(); err != nil { t.Fatalf("the priming server exited badly: %v", err) }

But the server installs its handler at cmd/agent-bus/main.go:1559:

  signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

which is LATE in startup -- long after the certificate is written during buscert setup.

So the test's readiness signal (a file exists) does NOT imply the process can handle a signal. If
SIGTERM arrives before line 1559, the DEFAULT disposition terminates the process, Wait() returns
"signal: terminated", and the test treats any non-nil Wait() error as a failure.

Under low load the server reaches signal.Notify before the signal lands. Under load it does not.

# Why this matters beyond one flaky test

1. It burns real time and misdirects diagnosis. It was called "environmental / resource contention"
   twice today -- once by me -- before anyone traced it. An integrator committing DOCS-22 correctly
   refused to attribute it to that change and flagged it for an owner instead.
2. Fan-out makes it WORSE and more frequent. This repo now routinely runs 4-6 agents concurrently,
   so load average above 10 is normal, not exceptional. A load-sensitive test becomes a
   load-CERTAIN test.
3. It teaches the wrong lesson. A test that fails for reasons unrelated to the code under test
   trains readers to ignore reds -- and this repo's entire discipline rests on reds meaning
   something.

# Two candidate fixes -- pick one and record why

  (a) Fix the READINESS SIGNAL. The certificate file is the wrong thing to wait for; wait for
      something that implies the server is fully started, e.g. a successful /healthz, or a startup
      line the server logs AFTER signal.Notify. Preferred: it fixes the actual mismatch.

  (b) Fix the ASSERTION. Accept "signal: terminated" as a legitimate outcome of SIGTERM. Cheaper,
      but weaker -- it would also mask a server that genuinely fails to shut down gracefully, which
      is a real property worth keeping under test.

DO NOT simply add a sleep. A sleep restores the same race at a different load.

# Acceptance
  - the test passes reliably UNDER LOAD -- reproduce at load average > 10 (e.g. run it while a
    parallel `go test -race ./...` saturates the box), not just on an idle machine
  - if (a): the readiness check provably waits for a state that implies signal handling is installed
  - if (b): the DECISIONS.md note records that graceful-shutdown behaviour is no longer asserted here
    and says where it IS asserted

# Note for whoever takes it
cmd/agent-busctl/enrol_test.go is also the only thing under cmd/agent-busctl that imports
internal/buscert (enrol_test.go:24). Unrelated, pre-existing, and not this task's to fix -- but you
will be in that file.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


_None recorded._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [DOCS-22](../../DOCS/DOCS-22--2f8ae959/task.md) — DOCS-22: The four agent ENTRY POINTS the invite gate missed — \`README\` Quickstart, \`agent… (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [ed6853d4-b5de-437a-a3dc-430e1d38243f](../../PROCESS/Establish-a-periodic-repo-wide-security-sweep-additive-t--ed6853d4/task.md) — Establish a periodic repo-wide security sweep, additive to per-task review (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
