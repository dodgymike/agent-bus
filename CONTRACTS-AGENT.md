# Contracts: the agent-facing surface and repo tooling scripts

Split out of `CONTRACTS.md` (2026-08-02) — see that file for the index of all contract planes and
the rest of the surface (CLI/env, HTTP, on-disk). This is a pure content move: everything below this
header is unchanged from the prior single-file `CONTRACTS.md`, verbatim.

## Agent-facing wrappers (`scripts/bus-*.sh`) and `AGENT_PROTOCOL.md`

No wrapper is due this wave. `/healthz` and `/v1/info` are operator/discovery surfaces, not agent
capabilities — invariant 7 ("every capability ships with a `scripts/bus-*.sh` wrapper and an
`AGENT_PROTOCOL.md` entry in the same task") is not yet triggered because no agent-facing capability
(enrol, send, wait, relay) exists yet. `AGENT_PROTOCOL.md` does not exist in the repo and should not
be created until the ENROL epic lands the first real agent-facing route.

## Repo tooling scripts (`scripts/*.sh`, NOT agent-facing)

These are maintainer/process tools, not bus capabilities. They are deliberately **not** `bus-*.sh`
and deliberately **not** in `AGENT_PROTOCOL.md`: invariant 7 governs the agent-facing bus surface
(enrol, send, wait, relay), and naming a process tool `bus-*.sh` would wrongly imply an agent calls
it to talk to a bus.

| Script | Purpose |
| --- | --- |
| `scripts/spec-cloud.sh` | Authed `curl` shim for the Spec Server (task state). See `CLAUDE.md`. |
| `scripts/proof-check.sh` | Runs a task's `proof_cmd` and refuses to call it a pass unless it demonstrated something. |

### `scripts/proof-check.sh` (added 2026-08-02)

**Why it exists.** Three ways a proof command exits 0 while proving nothing:

1. `go test -race -run TestThatDoesNotExist ./internal/wal` prints `ok … [no tests to run]` and
   **exits 0**. ~70% of this backlog's `proof_cmd` values have that shape, so a task could be
   flipped to `done` behind a proof whose named test was never written.
2. A test body that is just `t.Skip()` exits 0 with **no** `[no tests to run]` text at all, so
   grepping for that string does not catch it.
3. `A ; B` exits with `B`'s status and `|| true` swallows failure outright — so a **red** suite can
   sit behind a green exit code.

The script closes all three: it counts what actually ran rather than trusting the exit status.

```
scripts/proof-check.sh [--task <id>] [--classify] [--strict] [--quiet] '<proof command>'
```

| Option | Meaning |
| --- | --- |
| `--task <id>` | Fetch `proof_cmd` from the Spec Server (task key or `public_id`) via `scripts/spec-cloud.sh`, then check it. Requires `jq`. Id is validated against `^[A-Za-z0-9._-]+$` — it is interpolated into a URL that carries a bearer token. Note this *does* make a network call before classifying; it just never executes the proof. |
| `--classify` | Static classification only — **executes no part of the proof command**. |
| `--strict` | Additionally require *every* package listed in a `go test` invocation to contribute ≥1 test. Opt-in; reports `VACUOUS`/exit 4. |
| `--quiet` | Suppress the proof's own output; print only the verdict. |

| Env var | Default | Meaning |
| --- | --- | --- |
| `PROOF_CHECK_PROJECT` | `agent-bus` | Spec Server project slug used by `--task`. |

**Exit codes** (distinct on purpose — "I cannot check this" must never read as "this is broken"):

| Code | Verdict | Meaning |
| --- | --- | --- |
| `0` | `PASS` | Ran, exited 0, and if Go tests ran then ≥1 really ran, none failed, not all skipped |
| `1` | `FAIL` | Ran and exited non-zero, **or** ≥1 test failed behind an exit code that masked it |
| `2` | — | Usage error |
| `3` | `UNVERIFIABLE` | Cannot be checked: `n/a`, unfilled `<placeholder>`, invalid shell, a segment whose command does not exist (prose, or a wrapper not built yet), or a proof naming `go test` whose test output was never captured (absolute-path `go`, scrubbed `PATH`). **Not** a claim the work is broken. |
| `4` | `VACUOUS` | Exited 0 but proved nothing: zero tests ran, or every test that ran skipped |

Stdout carries **only** the machine-readable verdict line. All human output *and the proof's own
output* go to stderr, so a proof cannot print a convincing forgery of the verdict onto stdout. The
exit code, not the text, is authoritative.

```
proof-check: verdict=<PASS|FAIL|VACUOUS|UNVERIFIABLE> class=<tags> exit=<n> tests_run=<n> top_level=<n> skipped=<n> failed=<n> empty_pkgs=<n>
```

**Non-Go proofs are first-class.** `test -s PROTOCOL.md`, `grep -q … FILE`, `scripts/bus-*.sh …`,
`docker compose …` are legitimate proofs and are judged **purely on exit status** — they are never
forced through a test-count check.

**Decided: multi-package `-run` misses are a PASS.** `go test -race -run TestX ./internal/auth
./internal/httpapi`, where the pattern matches in one package and not the other, passes with a
warning naming the empty packages. `./internal/...` expands to a dozen packages of which two ever
match, so requiring all of them would fail almost every legitimate proof here — the false-negative
mode that gets guards switched off. The trap being closed is *zero tests anywhere*. `--strict`
opts into the stricter rule per-proof.

**Trust boundary.** A `proof_cmd` is executable input: the script runs it verbatim, with your
privileges and your full environment, in the repo root. With `--task` the string comes from the Spec
Server, so anyone who can edit that backlog can choose a command that runs on your machine. Use
`--classify` to inspect statically without executing. The echoed `proof:` line has non-printable
bytes replaced with `?`, so ANSI escapes cannot repaint it to hide what will run.

**Known limitations.**

- To count tests, `go test` runs through a `go` shim (installed on `PATH` for every run, so
  indirectly-invoked tests are still counted) that injects `-v` and merges stderr into stdout;
  `go build`/`go vet`/`go list` pass through untouched. A proof that parses non-verbose `go test`
  output, or redirects the two streams separately, will see different text than standalone. Nothing
  in the current backlog does.
- The empty-package warning is per output line, not per listed package, and the multi-package
  allowance generalises from one invocation to the whole proof: `go test -run TestOld ./a &&
  go test -run TestNew ./b` PASSES with a warning even when `TestNew` does not exist. **The
  completing agent must read the warning line, not just the verdict.**

**Policy (recommendation, not yet enforced):** completion should *require running* `proof-check.sh
--task <id>` and quoting its verdict line in `test_summary`, while `proof_cmd` stays stored as the
bare command — a proof that only runs inside our harness is a worse artifact than one anyone can
paste into a shell. Nothing in the tool can enforce this; its value is an auditable verdict line.
Full rationale and tradeoffs are in the comment block at the top of the script.
