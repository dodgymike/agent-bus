# Contracts: the agent-facing surface and repo tooling scripts

Split out of `CONTRACTS.md` (2026-08-02) — see that file for the index of all contract planes and
the rest of the surface (CLI/env, HTTP, on-disk). This is a pure content move: everything below this
header is unchanged from the prior single-file `CONTRACTS.md`, verbatim.

## Agent-facing surface (`cmd/agent-busctl`, `scripts/bus-serve.sh`) and `AGENT_PROTOCOL.md`

**The paragraph that stood here — "No wrapper is due this wave … `AGENT_PROTOCOL.md` does not exist
in the repo" — is FALSE and is corrected below.** It described the CORE wave, when the only routes
were `/healthz` and `/v1/info`.

**The agent-facing surface is `cmd/agent-busctl`, not `scripts/bus-*.sh`.** Invariant 7 was amended on
2026-08-02 (`DECISIONS.md`, "The Go CLI replaces the shell wrappers"): a Go CLI over the importable
`github.com/dodgymike/agent-bus/client` package satisfies the invariant in the wrappers' place, and
serves a third audience the wrappers never could — an agent that EMBEDS the client. `AGENT_PROTOCOL.md`
exists and is the usage doc; `CONTRACTS-CLI.md` is the exact flag/JSON/exit-code reference.

`scripts/` holds exactly six files (count corrected 2026-08-14: it said three, while `git ls-files
scripts/` lists six), and only one of them is agent-facing:

| Script | Agent-facing? | Purpose |
|---|---|---|
| `scripts/bus-serve.sh` | yes | Start/stop/status the SERVER for a session. The only surviving `bus-*.sh`. |
| `scripts/spec-cloud.sh` | no | Authed `curl` shim for the Spec Server (task state) |
| `scripts/proof-check.sh` | no | Runs a task's `proof_cmd` and refuses to call it a pass unless it demonstrated something |
| `scripts/proof-check_test.sh` | no | Guard test for `proof-check.sh` — pins the caller-cwd resolution fix (`535876c`) |
| `scripts/gen-spec-mirror.sh` | no | Regenerates the backlog mirror: `SPEC.md` (epic index) **and** the `SPEC/` tree. The ONLY supported way to write either. |
| `scripts/fed-smoke.sh` | no | Three-bus federation smoke test (RELAY-25). Deliberately describes the surface as it SHOULD be, so it fails loudly at the first unavailable step. |

### `scripts/bus-serve.sh` — the health probe is now https, and verified (`MTLS-LISTENER`/`MTLS-VERIFY`, 2026-08-07)

The bus serves TLS and ONLY TLS (invariant 11 — see `CONTRACTS-HTTP.md`'s "## Transport" and
`CONTRACTS-CLI.md`'s TLS paragraph). `bus-serve.sh`'s own probe had to move in the **same commit**, or
`start`/`status` would report a healthy bus as failed and every other task's server-startup proof would
break with it.

- `HEALTH_URL` is now `https://<probe-addr>/healthz`, probed with
  `curl -fsS --cacert "$DATA_DIR/bus-tls.crt"` — **`--cacert`, never `-k`**. The self-signed
  certificate the bus writes into its own data directory IS the trust anchor (there is no CA and no
  trust-on-first-use), so pointing `curl` at that exact file is a full verification against exactly one
  certificate, including the hostname (SANs) and the validity period.
- A new `probe_addr` helper rewrites a wildcard `AGENT_BUS_LISTEN` (`:8080`, `0.0.0.0:8080`,
  `[::]:8080`) to loopback before building `HEALTH_URL` — the old `http://` probe built the unusable
  URL `http://:8080/healthz` in that case, because the certificate deliberately does not name a
  wildcard bind.
- On success, `start` additionally prints: the `https://host:port` URL, the certificate path, the
  **certificate fingerprint** scraped from the log (`bus_cert_fingerprint=…`), and a ready-to-paste
  `agent-busctl enrol --bus … --bus-fingerprint … --name <name>` line — because there is no
  trust-on-first-use, an agent needs the fingerprint out of band before its first connection, and this
  is where a human or a driving script gets it without reading the log by hand.
- Exit codes are **unchanged**: `start` 0/1/2, `status` 0/1/3, `stop` 0/2. A bus that refuses to start
  over unusable certificate material (it never falls back to plaintext) still surfaces as `start`
  exiting `1` with the log named in the failure message.

**The container-internal probe is a different command, not this script.** `Dockerfile`'s `HEALTHCHECK`
and `docker-compose.yml`'s `healthcheck.test` both invoke `agent-bus healthcheck` — a subcommand on the
server binary, documented in `CONTRACTS-CLI.md` — rather than `curl`, because the Alpine runtime image
ships no HTTP client that can be told to trust one self-signed certificate (busybox `wget`'s only
relevant knob is `--no-check-certificate`, which verifies nothing, and invariant 11 forbids disabling
verification to make something work). Both healthcheck definitions changed from
`wget -q ... http://127.0.0.1:8080/healthz` to
`["/usr/local/bin/agent-bus","healthcheck","-data-dir=/data","-addr=127.0.0.1:8080","-timeout=2s"]`.

### OPEN ITEM — invariant 7 is NOT satisfied for three capabilities (2026-08-07)

Recorded as an open question rather than quietly treated as met. Invariant 7 requires every
capability to ship with its agent-facing entry point **in the same task**:

1. **`POST /v1/mint`** has no dedicated entry point. It is not a hole in practice — `agent-busctl send`
   performs the reserve-then-send two-step internally and an agent never issues the mint itself — but
   the route exists on the wire with no way to drive it alone, so it is named here rather than left
   for someone to discover.
2. **`agent-busctl keygen`** — printing this agent's own MESSAGING public key so a peer can trust it —
   **does not exist**. The capability exists only as `client.Client.MessagingPublicKey()`. Several
   error remedies in `client/store.go` and `client/keyring.go` tell the operator to run
   `agent-busctl keygen`; following that advice fails with "unknown subcommand".
3. **`agent-busctl trust`** — adding a peer's messaging public key to `<identity-dir>/trusted-keys/` —
   **does not exist** either. The capability exists only as `client.Client.TrustPeer()`.
   `client/client.go`'s own doc comment refers to `agent-busctl trust` as though it were shipped.

Consequence, stated plainly: **an agent that shells out to `agent-busctl` cannot today publish its
messaging key or trust a peer's**, so it cannot participate in end-to-end verification at all. Only
an agent embedding the `client` package can. Since verification is not wired into `client.Read`
either (see `CONTRACTS-CLI.md`), nothing is currently *blocked* by this — but the two gaps must close
together, and neither should be reported as done.

`POST /v1/broadcast` needs no entry point: `agent-busctl broadcast` already exists and the route now
refuses (501). That is a regression to be re-opened by SIGN-3, not a missing wrapper.

## Repo tooling scripts (`scripts/*.sh`, NOT agent-facing)

These are maintainer/process tools, not bus capabilities. They are deliberately **not** `bus-*.sh`
and deliberately **not** in `AGENT_PROTOCOL.md`: invariant 7 governs the agent-facing bus surface
(enrol, send, wait, relay), and naming a process tool `bus-*.sh` would wrongly imply an agent calls
it to talk to a bus.

| Script | Purpose |
| --- | --- |
| `scripts/spec-cloud.sh` | Authed `curl` shim for the Spec Server (task state). See `CLAUDE.md`. |
| `scripts/proof-check.sh` | Runs a task's `proof_cmd` and refuses to call it a pass unless it demonstrated something. |
| `scripts/proof-check_test.sh` | Guard test for the above: proves proofs resolve against the CALLER's cwd, not the script's tree. |
| `scripts/gen-spec-mirror.sh` | Regenerates `SPEC.md` + the `SPEC/` tree from the Spec Server. |
| `scripts/fed-smoke.sh` | Three-bus federation smoke test (RELAY-25); currently expected to fail at the first unavailable step. |

### `scripts/gen-spec-mirror.sh` — the backlog mirror (restructured 2026-08-14)

The Spec Server is the source of truth for task state; this script writes the mirror, and it is the
only supported way to write it. It emits **two** artefacts and rewrites both on every run:

| Path | Contents |
| --- | --- |
| `SPEC.md` | Epic INDEX only — one row per epic with open/total counts and a pointer. No task descriptions. |
| `SPEC/<EPIC>/epic.md` | Every task in that epic: key, title, status, priority, task pointer, relations, derived refs. Open tasks first, closed in a separate section. |
| `SPEC/<EPIC>/<task>/task.md` | The full record for one task, description UNTRUNCATED. |

Directory names are `<key-or-title-slug>--<public-id-prefix>`; the public-id fragment supplies
uniqueness because `key` is null for about a third of the backlog. Use the full `public_id`
(recorded in each `task.md`) for Spec Server lookups — prefix resolution does not exist server-side.

`SPEC.md` and `SPEC/` are **tracked in git deliberately**: the mirror is the server-down fallback,
and an untracked fallback is not one. The staging directory the script builds into (`/.spec-tree.*`,
a sibling of `SPEC/` so the swap can be a rename) is gitignored, and removed on exit including on
failure.

| Flag | Behaviour |
| --- | --- |
| *(none)* | Write `SPEC.md` + `SPEC/`, fetching real relations. ~70 s per the script header. |
| `--no-relations` | Skip the relation fetch — fast regen. Every file then reads "NOT FETCHED — unknown, not absent" instead of implying a task has no edges. |
| `--stdout` | Print the `SPEC.md` index to stdout and write nothing. |
| `--all` | Accepted, **no-op**. Kept so old invocations do not fail; the tree always contains every task. |
| `-h`, `--help` | Print the script header (the authoritative description of this surface). |

**Closed tasks are INCLUDED.** The previous single-file mirror dropped them to save bytes; in a tree
they cost nothing until a file is opened, and their absence was the main reason the server-down
fallback was worse than the server itself.

**Two kinds of task-to-task link, never blurred.** *Relations* are authoritative edges fetched from
the server (`blocks`, `supersedes`, `relates`, `follow_up`) — there is no bulk endpoint, so this
costs one rate-limited request per task, hence two workers with exponential backoff. *Referenced
(derived)* links are key-shaped strings matched out of description free text: best-effort, labelled
as such everywhere, and not a dependency list. A task whose relations cannot be fetched is a
REFUSAL, not an empty list.

**It refuses to write rather than publish a damaged mirror.** Three guards: a structural check on
the raw markdown export, a count cross-check between the markdown and JSON exports (re-fetched once,
since concurrent agents file tasks constantly), and a no-task-lost check asserting one directory per
task. On any breach `SPEC.md` and `SPEC/` are left untouched. After the swap it also asks git
whether any file it just wrote is IGNORED — a task directory swallowed by an unanchored
`.gitignore` credential pattern would vanish from the fallback while `git status` stayed clean — and
exits 3 with the offending paths if so.

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
privileges and your full environment, in the invoking process's captured working directory. With
`--task` the string comes from the Spec Server, so anyone who can edit that backlog can choose a
command that runs on your machine. Use
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
