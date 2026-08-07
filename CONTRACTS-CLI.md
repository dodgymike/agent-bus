# Contracts: server / CLI flags + env vars

Split out of `CONTRACTS.md` (2026-08-02) — see that file for the index of all contract planes and
the rest of the surface (HTTP, on-disk, agent-facing).

Two binaries live here:

- **`cmd/agent-bus`** — the SERVER.
- **`cmd/busctl`** — the CLIENT, added 2026-08-02 by CLI-1/CLI-2. Per the amended invariant 7 it
  **replaces the `scripts/bus-*.sh` wrappers** as their subcommands land. Its flags, exit codes and
  JSON shapes are a contract with two consumers — a human and an agent shelling out — plus a third,
  an agent **embedding** the Go package it is a thin shell over.

---

## CLI flags (`cmd/agent-bus`)

| Flag | Default | Meaning |
| --- | --- | --- |
| `-listen` | `127.0.0.1:8080` | TCP address to bind (loopback-only by default; use `:8080` for all interfaces) |
| `-data-dir` | `./data` | Directory for the durable store + append-only log; created `0700` if missing |
| `-poll-timeout` | `30s` | Ceiling on a single long-poll wait (not yet consumed by any handler) |
| `-log-level` | `info` | `debug`, `warn`, `info`, or `error` |
| `-bus-id` | *(empty → placeholder `bus-local`)* | **TEST-ONLY.** Validated against `^[A-Za-z0-9_-]{1,64}$`; `.` rejected (qualification separator, invariant 2). Using it logs a runtime `WARN`. See `DECISIONS.md`. |

Exit codes: `2` on invalid flags/config (`parseFlags`/`validate` failure), `1` on a startup failure
(e.g. bind failure), `0` on a clean signal-driven shutdown.

---

## `cmd/busctl` — the client

Binary directory `cmd/busctl`; the importable package it shells over is
`github.com/dodgymike/agent-bus/client` (top-level, deliberately **not** under `internal/` — see
"The client package" below).

```
busctl [flags] <command> [flags]
```

Global flags are accepted **before or after** the subcommand, so both `busctl --json enrol …` and
`busctl enrol --json …` work.

### Global flags

| Flag | Env | Default | Meaning |
| --- | --- | --- | --- |
| `--bus <url>` | `AGENT_BUS_URL` | *(the selected identity's recorded URL; `enrol` requires it explicitly)* | Base URL of the bus. `https` anywhere; **`http` ONLY to a loopback host** (`127.0.0.1`, `::1`, `localhost`) — see "In flight" below. A path prefix is allowed; **userinfo, query and fragment are rejected**. Canonicalised: host lower-cased, default port dropped, trailing `/` trimmed. |
| `--identity <dir>` | `AGENT_BUS_IDENTITY` | `$XDG_CONFIG_HOME/agent-bus` (`os.UserConfigDir()` + `/agent-bus`) | The credential store **DIRECTORY** — not an agent id. |
| `--as <agent-id>` | `AGENT_BUS_AGENT_ID` | *(the stored selection)* | Act as one stored identity for this command only, changing nothing on disk. **Parallel agents sharing a store should use this, not `use`.** |
| `--json` | — | off | Machine-readable JSON on stdout. |
| `--timeout <dur>` | `AGENT_BUS_TIMEOUT` | `30s` | Bounds ONE operation end to end, retries included. Any `time.ParseDuration` value; must be positive. |

**Resolution order, deterministic:** explicit flag → environment variable → the selected identity's
recorded value (`--bus` only) → built-in default.

`--help` / `-h` / `busctl help <command>` print help and exit `0`.

### Subcommands (as of 2026-08-02)

| Command | Purpose | Network? |
| --- | --- | --- |
| `enrol --name <name>` | Generate an Ed25519 key pair, send only the public half, receive the server-minted `<bus-id>.<agent-id>`, store the credential | yes — `POST /v1/enroll` |
| `whoami [--all] [--verify]` | Show the identity commands act as; `--all` lists them; `--verify` performs a real session handshake | only with `--verify` |
| `use <agent-id\|name>` | Change the stored selection | no |
| `logout [<agent-id>] [--all]` | Delete a credential **locally** | no |
| `agents` | List every agent enrolled on the bus, fully-qualified id first | yes — `GET /v1/agents` |
| `send <to-agent-id> [body]` | Send one direct message, durable before it returns (invariant 4) | yes — `POST /v1/send` |
| `broadcast [body]` | Send one message to every other enrolled agent, durable before it returns | yes — `POST /v1/broadcast` |
| `watch` | Long-poll and stream messages addressed to you until stopped | yes — `GET /v1/messages`, `GET /v1/wait` |

`enrol` flags: `--name` (required), `--invite` (**reserved, currently rejected** — see below),
`--idempotency-key` (resume an earlier attempt), `--keep-current` (do not switch the selection).

`agents` flags: none beyond the globals.

`send`/`broadcast` flags: `--file <path>` (`-` means stdin), `--stdin`, `--idempotency-key <key>`
(retry a specific earlier send/broadcast — see "Send/broadcast idempotency" below). The body itself
is a **positional argument** — `send`'s second (after `<to-agent-id>`), `broadcast`'s first — not a
flag. Both commands **permute flags and positionals** (`parseWithPositionals` in
`cmd/busctl/send.go`) so `busctl send <to> --json` parses as intended: Go's plain
`flag.FlagSet.Parse` stops at the first non-flag argument and hands everything after it back as
positionals, so before that helper existed `busctl send <to> --json` read `"--json"` itself as the
message body and **delivered it as the message**, silently. Any future command that adds a
positional needs the same helper, or it will reproduce that exact bug.

`watch` flags: `--replay`, `--cursor <c>`, `--limit N`, `--poll-timeout <dur>`, `--count N`,
`--for <dur>`, `--no-cursor` — see "`watch`: output modes and the cursor contract" below.

**`logout` is LOCAL ONLY.** `/v1/leave` does not exist yet, so nothing is revoked: the enrolment
stays on the roster and any live session lives out its hour. The JSON field `server_notified` reports
this honestly and is `false` today.

### Exit codes — CONTRACT

An agent branches on these, so a value never changes meaning and a retired value is never reused.
They are produced by `client.ExitCode(err)` in the importable package, so an embedder gets the same
codes without copying a switch.

| Code | Kind | Meaning |
| --- | --- | --- |
| `0` | — | Success |
| `1` | `internal` | Unclassified/internal failure |
| `2` | `usage` | Malformed invocation: bad flag, missing `--name`, unknown subcommand, reserved `--invite` |
| `3` | `config` | Local identity/config not ready: nothing enrolled, no selection, unreadable or damaged store |
| `4` | `auth` | The bus rejected the credential (401/403), or the signature did not verify |
| `5` | `network` | The bus could not be reached: refused, DNS, timeout, or a certificate that does not verify |
| `6` | `server` | The bus reported a failure of its own (5xx), or a capacity refusal that survived retries |
| `7` | `rejected` | The bus understood the request and refused it (400/404/409/413/415/422) |
| `8` | `empty` | Succeeded with **nothing to report** (e.g. `whoami --all` on an empty store) |

`2` is usage rather than `1` to match Go's `flag` package and `cmd/agent-bus`.

No code changes meaning; some commands give one a more specific sense:

- `8` — `agents` on an empty roster, and a **bounded** `watch` (`--count`/`--for`) that delivered
  nothing before it finished. An unbounded `watch` stopped by a signal is always `0`, however many
  messages it saw.
- `7` — a 409 idempotency-key conflict on `send`/`broadcast` (same key, different payload — the bus
  disconnects), and an unknown recipient on `send`.
- `6` — a fatal 503 (the bus's write path cannot durably accept messages, signalled by **no**
  `Retry-After` header) is `KindServer`, so it is exit **6**, not `5`. `5` stays reserved for the bus
  being unreachable at all — refused, DNS, timeout, TLS. See "The 503 split" below.

### JSON shapes — CONTRACT

**Success** — exactly ONE JSON object on stdout, keys sorted, plus `"ok": true`:

```json
{"agent_id":"bus-abc.planner-1","bus_id":"bus-abc","bus_url":"https://127.0.0.1:8080",
 "enrolled_at":"2026-08-02T22:10:24.217971827Z","name":"planner","ok":true,
 "public_key":"KouoAWExNgv14Dh4sg/h/AnDXw/tn583vbvCCyO01Rs=","replayed":false,
 "store_path":"/home/u/.config/agent-bus/identities.json","stored":true}
```

| Command | Fields |
| --- | --- |
| `enrol` | `agent_id`, `bus_id`, `name`, `bus_url`, `public_key`, `enrolled_at`, `replayed`, `idempotency_key`, `stored`, `store_path` |
| `whoami` | the identity fields above, plus `is_current` (bool), and `session` (`agent_id`, `expires_at`, `refresh_at`, `lifetime_seconds`) with `--verify` |
| `whoami --all` | `identities` (array), `current_agent_id` (string), and `pending` (array of `idempotency_key`/`name`/`bus_url`/`created_at`) when any enrolment is unfinished |
| `use` | the identity fields, plus `is_current` (bool) |
| `logout` | `removed` (array of agent ids), `current_agent_id` (string), `server_notified` |
| `agents` | `agents` (array of `agent_id`/`bus_id`/`name`/`enrolled_at`), `count`, `ok` |
| `send`, `broadcast` | `message_id`, `seq`, `from`, `broadcast`, `to`, `sent_at`, `content_sha256`, `replayed`, `idempotency_key`, `ok` |

**`is_current` is a bool; `current_agent_id` is a string.** They are deliberately different keys: one
name that is a bool in one subcommand and a string in another makes `jq .current` unpredictable.

**Failure** — one JSON object on **stdout** in `--json` mode (so a consumer parses one stream), or
two human lines on **stderr** otherwise:

```json
{"ok":false,"error":"enrol: cannot reach the bus at http://127.0.0.1:8080","kind":"network",
 "remedy":"check --bus / AGENT_BUS_URL and that the bus is running","exit_code":5}
```

`status` is added when the failure carried an HTTP status. `idempotency_key` (`omitempty`) is added
when the failed operation was a mutating one that had already minted a key — `send`/`broadcast` — and
is **omitted** when the failure never had one (a local usage error caught before a key existed, e.g.
a missing recipient). It matters because a network error or a 5xx on a send is genuinely ambiguous —
the message may or may not have been applied — and the key is the only handle that makes a later
retry the SAME logical send rather than a second message (invariant 10). In human mode the same key
is named on stderr alongside the `--idempotency-key` flag that resumes it.

**NDJSON — the streaming convention, now landed with `watch`.** A streaming subcommand writes **one
compact JSON object per line, flushed as it arrives**, with **no envelope, no `ok` field and no array
brackets**, so a consumer can act on each record incrementally instead of buffering to completion.
Diagnostics never go to stdout.

### `watch`: output modes and the NDJSON record shape

`watch` picks its output form for you, from stdout alone — there is no flag that forces the human
feed:

| Condition | Output |
| --- | --- |
| `--json` | NDJSON |
| no `--json`, stdout is **not** a terminal (a pipe or redirect) | NDJSON — a pipe is a machine |
| no `--json`, stdout **is** a terminal | a readable live feed, one line (or indented block) per message |

One NDJSON record per message, field by field:

| Field | Meaning |
| --- | --- |
| `message_id` | the server-minted id, the key to deduplicate on |
| `seq` | the server-minted monotonic sequence |
| `from` | the fully-qualified sender id |
| `broadcast` | whether this went to every agent except the sender |
| `to` | the recipient list — one entry for a direct message, empty for a broadcast |
| `bus_path` | bus ids traversed, oldest first |
| `sent_at` | the bus's timestamp, verbatim |
| `size` | body length in bytes, as the bus recorded it |
| `content_sha256` | hex SHA-256 of the decoded body |
| `body` | the decoded body |
| `text` | the body as a string, present only under the conditions below |

`body` is **always present**, standard base64 — the authoritative, lossless form, true for any bytes
at all. `text` is present **only** when the body is valid UTF-8, free of control characters other
than tab/newline/CR, and free of the Unicode bidi and zero-width characters that can reorder or hide
what a terminal renders (`isBidiOrInvisible` in `cmd/busctl/watch.go` — the same forgery class as an
ANSI escape, spelled in Unicode). It is **omitted, never rewritten**, otherwise: a lossily-rewritten
body would be worse than no field at all, since a consumer would have no way to tell what it read is
not what was sent.

So: `jq -r .text` for text traffic, `jq -r .body | base64 -d` for anything (binary or otherwise
disqualified). Running diagnostics — retry notices, cursor-store warnings, the closing summary — go
to stderr and never appear inside the stream. The one exception: under `--json`, the FINAL failure
object (including the exit-8 "nothing arrived" outcome of a bounded watch) is emitted as the last
line of the stream on stdout, in the same shape every other subcommand's failure uses — branch on the
presence of an `"ok"` field, which a failure object always has and a message record never does.

### `watch`: the cursor contract

This is the load-bearing part of `watch`, and it applies whether the output is human or NDJSON:

- the read position (the "cursor") is **persisted by default**, per (identity, bus), in the
  credential store directory;
- the cursor **advances only after a whole batch has been handed to the caller** — poll, hand every
  message in the batch to the caller, only then adopt and (if persisting) write the new cursor. A
  process killed mid-batch **re-delivers that whole batch** on the next run; it never advances past
  messages the caller was never given, and it never skips;
- delivery is **at-least-once**: duplicates are the normal steady state (a cyclic relay topology with
  at-least-once forwarding guarantees them, not just a crash), and a handler must be **idempotent on
  `message_id`**;
- a poll that times out with nothing is a `200` and a **normal** outcome, not an error — on a quiet
  bus it is the steady state;
- `--no-cursor` does **not persist** anything (a throwaway tail), but it still **starts from the
  stored position** — the run's own `--help` says so plainly, and this doc does not soften it: the
  next (persisting) run resumes wherever the stored cursor already was, unaffected by the throwaway
  run;
- `--replay` and `--cursor <c>` are **both start positions**; giving both is a usage error (exit `2`)
  rather than one silently winning over the other — the same "refuse an ambiguous instruction rather
  than guess" rule `send`/`broadcast` apply to a body given twice.

### Credential storage

| Path | Mode | Contents |
| --- | --- | --- |
| `<identity-dir>/` | `0700` | The store. Tightened on open if it already exists looser. |
| `<identity-dir>/identities.json` | `0600` | Format version `1`. Enrolled identities **including Ed25519 private-key seeds**, the current selection, and in-flight (`pending`) enrolments. |
| `<identity-dir>/identities.lock` | `0600` | Exclusive lock for read-modify-write; treated as abandoned after 30s. |
| `<identity-dir>/cursors.json` | `0600` | Format version `1`. One `watch` read position per (`agent_id`, `bus_url`) pair — no key material. Capped at 256 records, and 512 bytes per stored cursor, so a bus cannot grow the file without bound. |
| `<identity-dir>/cursors.lock` | `0600` | A **separate** exclusive lock from `identities.lock` — a cursor advances far more often than a credential changes, and sharing one lock would put `watch` in needless contention with `enrol`/`use`/`logout`. |

**`cursors.json` fails OPEN, unlike `identities.json`.** A damaged file, one that fails to parse, or
one written by an unknown format version is **not fatal**: it is ignored, a warning is printed to
stderr, and `watch` replays from the start of the retained window — the same outcome as an agent that
had simply never watched before. This is the deliberate opposite of `identities.json`, which refuses
outright on an unknown version (see `(*Store).load`): a credential misread is unrecoverable and
dangerous (a private key misparsed as public fails silently), so refusing to guess is the only safe
move there, while a cursor is a **position hint, not a credential** — losing it re-delivers messages,
which at-least-once delivery already permits and which a correct handler already tolerates by
deduplicating on `message_id`. Refusing to run because a position hint was damaged would trade a
harmless replay for an outage.

Written by atomic replace: an `O_EXCL` `0600` temp file in the same directory, fsynced, renamed, then
the directory fsynced. Abandoned temp files are swept on the next write — each is a complete copy of
every private key. **Never inside the repository.**

The lock carries an ownership token (pid + 16 random bytes); a stale break removes only the exact
file it observed as stale, and a release removes only a lock that is still its own. Without that,
two processes breaking one abandoned lock could both believe they held it and one whole-file
update — one private key — would be lost.

A store directory or credential file found at looser permissions is TIGHTENED **and a warning is
printed to stderr**: a key file that was ever readable by another local user must be assumed
compromised, and silently fixing the mode destroys the only evidence.

**Session tokens are never written to disk.** They are bearer credentials with at most an hour of
life that do not survive a bus restart, so persisting them would trade a stealable token at rest for
two saved round trips. Each `busctl` process performs its own handshake.

### Enrolment idempotency (invariant 10)

The key pair is written to the store as a `pending` record **before** `/v1/enroll` is called, so a
process killed after the bus minted an id does not lose the private key. Records are scoped to
**(idempotency key, bus URL)** — the same scoping the server uses — so:

- re-running an enrolment with the same `--idempotency-key` and name is answered **from the store**,
  with `"replayed": true` and **no HTTP request**;
- the same key with a different name on the same bus is refused **locally**, exit `2`, because the
  bus's answer to that is a 409 **and a disconnection**;
- the same key against a different bus is a fresh enrolment;
- a network failure keeps the record and the error names the exact `--idempotency-key <key>` that
  resumes it; `whoami --all` lists every unfinished enrolment with the command that resumes it, so a
  process killed before it printed anything still leaves a recoverable identity;
- pending records are pruned 24h after creation, on the next store write, and are destroyed
  outright by `logout --all`.

The claim decision — already applied / resume / start new — is made in ONE locked read-modify-write.
Two concurrent enrolments under one key would otherwise both generate a key pair, and one private key
would be lost while both sent conflicting payloads under the same key.

### Send/broadcast idempotency (invariant 10)

The idempotency key for `send`/`broadcast` is minted **once per invocation** — before the payload is
marshalled — and reused across every internal transport retry, so a send retried inside `busctl` can
never become two messages. Omit `--idempotency-key` and one is minted for you; it is always printed
back (human output and `--json`'s `idempotency_key` field), because it is the only handle that makes
a *later* retry the same logical send rather than a second message.

- **Same key + byte-identical body** is a legitimate retry. The bus answers from its applied-key
  table, re-applies nothing, and returns the ORIGINAL result — `"replayed": true`, exit `0`.
- **Same key + different content** is a protocol violation. The bus answers `409` **and disconnects**
  — surfaced as its own loud `KindRejected` error, exit `7`. Retrying will not help; use a fresh key
  for new content.
- A key is remembered only as long as the message it produced is **retained** (1 day, or until 1 GiB
  of messages push it out). A "retry" that arrives after that produces a **second message** rather
  than being rejected — a key is a retry handle for minutes and hours, not for days.

### In flight — what will change

- **`--invite` is RESERVED and currently rejected** (exit `2`) rather than guessed. Enrolment is
  becoming invite-only (invariant 3) and the blob will carry bus id, address, **bus-certificate
  fingerprint** and invite secret — but the wire shape is settled by task `ENROL-SHAPE`, and
  `/v1/enroll` is explicitly UNSTABLE until it, certificate binding and POPKEY all land. Inventing a
  field name here would be the same mistake as hand-picking a record-type number.
- **TLS** (invariant 11): `http://` is accepted **only to a loopback host**, and only because the bus
  does not serve TLS yet. Plaintext to anything else is refused, because `/v1/session/begin` returns
  the session token — a bearer credential — in a response body. That restriction is the client-side
  half of E8's sequencing constraint ("the bus must NOT be exposed on a non-loopback interface before
  mTLS lands") and is DELETED once the TLS listener ships. When it does, certificates are
  self-signed, mutual, and pinned from the invite with **no TOFU**. The whole transport is one seam
  (`client.newHTTPClient`) so pinning drops in there and nowhere else. `InsecureSkipVerify` is not
  set, is not reachable through `Config`, and appears in no `.go` file in the tree — a test asserts
  it. Redirects are never followed, because Go's default policy would forward the `Authorization`
  header across an `https`→`http` downgrade on the same port.
- **The transport is built before the identity is resolved**, which will need revisiting: pinning
  needs a per-identity client certificate and a per-bus fingerprint, neither of which is a function
  of `Config` alone. Filed as a follow-up; the seam is in the right place, at the wrong time.

### The client package (`github.com/dodgymike/agent-bus/client`)

Top-level, **not** under `internal/`: invariant 7's third audience is an agent that EMBEDS the
client, and Go forbids another module from importing an `internal/` path, so the requirement would be
silently foreclosed by a directory name. Its exported surface is a public API subject to
compatibility care, and it must **not** import anything under `internal/` — mechanically enforced by
CLI-1's proof clause `! go list -deps ./cmd/busctl | grep -q 'agent-bus/internal/'`.

Constants shared with the server are **pinned literals** with a comment naming the server-side
definition they mirror (`client.SessionSigningContext`, `client.AgentNamePattern`, the route paths).
Divergence fails closed — a signature simply does not verify.

Exported surface as of 2026-08-02:

| Symbol | Purpose |
| --- | --- |
| `Config`, `DefaultConfig`, `Config.ApplyEnv`, `DefaultIdentityDir`, `RetryPolicy`, `HTTPDoer` | Configuration and the transport escape hatch |
| `EnvBusURL`, `EnvIdentityDir`, `EnvAgentID`, `EnvTimeout` | The env var names above |
| `DefaultTimeout`, `DefaultRetryAttempts`, `DefaultRetryBaseDelay`, `DefaultRetryMaxDelay` | Defaults |
| `New`, `Client` | The client; `Config()`, `Store()`, `Identity()`, `Identities()`, `Use()`, `Logout()`, `LogoutAll()`, `Enrol()`, `EnsureSession()`, `Send()`, `Broadcast()`, `Agents()`, `Read()`, `Watch()` |
| `Identity`, `Credential`, `PendingEnrolment`, `Store` (`OpenStore`, `Dir`, `Path`, `Warnings`, `List`, `ListPending`, `Resolve`, `SetCurrent`, `Remove`, `RemoveAll`, `FindApplied`, `PromotePending`, `Cursor`, `SetCursor`, `ClearCursor`, `CursorPath`) | Credential storage, plus `watch`'s persisted read position (`cursors.json` — see above). The in-flight-enrolment methods that take the unexported record type (`ClaimEnrolment`, `FindPending`, `DropPending`) are effectively package-internal and are NOT part of the embeddable surface. |
| `EnrolOptions`, `EnrolResult`, `SessionInfo`, `LogoutResult` | Operation inputs and results |
| `SendOptions`, `BroadcastOptions`, `SendResult`, `AgentSummary`, `AgentList`, `ReadOptions`, `Batch`, `Message`, `WatchOptions`, `WatchStats` | Messaging inputs, results and the wire-faithful `Message`/`Batch` types |
| `Error`, `Kind` (+ the `Kind*` constants), `KindOf`, `ExitCode`, `ErrorPayload`, `NewErrorPayload`, the `Exit*` constants, `IsFatalUnavailable`, `IdempotencyKeyOf` | Errors and the exit-code contract |
| `SessionSigningContext`, `AgentNamePattern` | Pinned protocol constants |
| `MaxBodyBytes`, `MaxBatchLimit`, `DefaultBatchLimit`, `MaxPollTimeout`, `DefaultPollTimeout` | Protocol limits, pinned literals mirroring the server's own (see `client/messages.go`) |

`Identity` is the redacted public half and `Credential` is `Identity` plus the secret seed; the split
is structural, so no rendering path can marshal a private key by forgetting a redaction step.
`Credential.String()` redacts. `SessionInfo` has **no token field at all**, not even a `json:"-"` one.

**The 503 split.** A `503` with a `Retry-After` header is a transient capacity refusal and is retried
with jittered backoff (`Watch`, and the transport's own retry loop, both honour it). A `503` with
**no** `Retry-After` means the bus's write path cannot durably accept messages at all — not
transient — and is **not** retried: it stops a `Watch` outright and is reported through
`client.IsFatalUnavailable`, which every long-running caller (a supervisor, a `watch`) must check and
stop on rather than back off forever, or an operator-visible fault becomes a silent one.

---

## Env vars

| Var | Consumed by | Meaning |
| --- | --- | --- |
| `AGENT_BUS_URL` | `busctl` | Bus base URL (`--bus`) |
| `AGENT_BUS_IDENTITY` | `busctl` | Credential store directory (`--identity`) |
| `AGENT_BUS_AGENT_ID` | `busctl` | Act as this stored identity (`--as`) |
| `AGENT_BUS_TIMEOUT` | `busctl` | Per-operation timeout (`--timeout`) |

`cmd/agent-bus` still reads no environment variables; every server knob is a flag.
(`scripts/bus-serve.sh` has its own `AGENT_BUS_RUN_DIR` / `AGENT_BUS_DATA_DIR` / `AGENT_BUS_LISTEN`
/ `AGENT_BUS_LOG_LEVEL` / `AGENT_BUS_POLL_TIMEOUT` — those are the wrapper's, not the binary's, and
are documented in `CONTRACTS-AGENT.md`.)
