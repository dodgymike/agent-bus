# Contracts — index

Every route, CLI flag, env var, header, and durable record type agent-bus exposes. **Update the
relevant plane file below in the same commit that changes any of the surfaces it documents**
(`CLAUDE.md` step 9). This is the authoritative reference; `README.md` and `AGENT_PROTOCOL.md`
summarise for humans/agents but these files are where the exact shape lives.

**Split 2026-08-02** (`CONTRACTS-SPLIT`, public_id `360a2679-b5dc-4b17-863f-fb4462764e6d`) out of a
single `CONTRACTS.md` into the four plane files below, to remove a single-writer chokepoint on this
file that caused three P0s across two consecutive triage loops (concurrent agents all needing to
land a documentation update in the same file). This split was a **pure content move** — every
section kept its exact prior wording, only its location changed — so a diff against the pre-split
file should show only relocation, not rewording. `CONTRACTS.md` itself stays at this path as an
index so existing references and muscle memory land somewhere useful instead of 404ing.

## Where each surface lives

| Plane file | What it documents |
| --- | --- |
| [`CONTRACTS-CLI.md`](CONTRACTS-CLI.md) | Server / CLI flags (`cmd/agent-bus`) and environment variables. |
| [`CONTRACTS-HTTP.md`](CONTRACTS-HTTP.md) | HTTP routes, headers, enrolment/sessions, and authentication (the `authMiddleware` contract, the allow-list, the credential model). |
| [`CONTRACTS-ONDISK.md`](CONTRACTS-ONDISK.md) | Durable record types / wire protocol versions, on-disk files in the data directory (`bus.lock`, `bus.wal`), and the write-ahead log at startup. **This is the plane most in flux** (DUR / on-disk format version 2 work is active) — it benefits most from being isolated in its own file. |
| [`CONTRACTS-AGENT.md`](CONTRACTS-AGENT.md) | The agent-facing surface (`cmd/agent-busctl` and the one surviving wrapper `scripts/bus-serve.sh`; `AGENT_PROTOCOL.md`) and repo tooling scripts (`scripts/spec-cloud.sh`, `scripts/proof-check.sh`) that are NOT agent-facing but are documented alongside it. |

Two known-wrong passages were moved **unchanged, wrong exactly as before** — fixing their content is
explicitly out of scope for the split itself and is tracked by other tasks, not this one:

- `CONTRACTS-CLI.md`'s `-listen` default row still reads `:8080` (should be `127.0.0.1:8080` per the
  DECISIONS.md localhost-default decision) — tracked by `b0a5630b` (LISTENADDR-FU-CONTRACTS) and
  `c27f9439` (AUTH-1-FU-LISTENADDR).
- `CONTRACTS-ONDISK.md`'s WAL-repair prose still documents the reverted refuse-to-start policy
  (`provably torn tail`, `RepairTail`) instead of the shipped always-restart `RepairLog`/quarantine
  behaviour — tracked by `5b178dde` (DUR-11-FU-CONTRACTS).

Do not "fix while you're in there" on either passage from this file; that is scope creep on whichever
task lands the correction, and both are already filed.

## 2026-08-07 — MSG-FU-SUFFIXFLOOR: new on-disk file + new `internal/ids` Go surface

**No new HTTP route, no new env var, no `scripts/bus-*.sh` change** — those three still hold. **This
task DOES ship one new CLI flag**, documented in the table below; an earlier version of this entry
said "no new CLI flag", which was false the moment the operator opt-in below shipped. The
"no `scripts/bus-*.sh` change" half is a real GAP, not a virtue, stated so it is not mistaken for one:
`scripts/bus-serve.sh` has no way to pass `-backfill-suffix-floors`, so there is no sanctioned wrapper
path for the legacy-directory migration (invariant 7) — tracked separately as
`5a900716-1916-44ac-bd5a-ff695146adc8`, not fixed here.

**New CLI flag, belongs in `CONTRACTS-CLI.md` (out of this task's file boundary, so recorded here
instead — fold it into that file's flag table the next time it is touched):**

| | |
|---|---|
| Flag | `-backfill-suffix-floors` |
| Type / default | `bool`, default `false` |
| Registered | `cmd/agent-bus/main.go` `parseFlags`, stored on `Config.BackfillSuffixFloors` |
| Consumed by | `openSuffixAllocator` (`cmd/agent-bus/suffixfloors.go`), as its `allowBackfill` parameter |
| Contract | A **one-time operator opt-in, never needed in normal operation**. It is required, and required ONLY, to start a data directory that HAS history (non-empty at process start, or the WAL holds records) but is missing `<data-dir>/agent-suffixes` — a state that otherwise REFUSES to boot, because a missing floors file on a dir with history cannot be told apart from a lost witness of already-issued agent ids (invariant 1). Setting it derives floors from the durable log (`walAgentIDFloors`: the per-name maximum of message-record sender/recipients and enrolment records) and raises them into the newly-opened, empty allocator before it seals. It is a no-op — logged as unnecessary — on a directory that already has a floors file, or that was genuinely empty at startup. Leaving it set permanently defeats the refusal that protects invariant 1 on this path; the startup log tells the operator to remove it once the one migration restart it exists for has run. |

Full semantics — the refusal it bypasses, the counter-arguments considered and rejected, and the log
levels the seal line picks — live in the comment above `alloc.Seal()` inside
`cmd/agent-bus/suffixfloors.go`'s `openSuffixAllocator`, and in `DECISIONS.md` (2026-08-07, "Four
rulings: refuse-to-boot exception, format break, binary rename, redeploy", §1).

**New on-disk file, belongs in `CONTRACTS-ONDISK.md` (out of this task's file boundary, so recorded
here instead — fold it into that file's tables the next time it is touched):**

| | |
|---|---|
| Path | `<data-dir>/agent-suffixes` |
| Mode | `0600`, inside the `0700` data directory |
| Format version | **3**, reserved 2026-08-07 by feature-runner in the Spec Server `ondisk-format-version` namespace (values 1 and 2 are the WAL's — see `CONTRACTS-ONDISK.md` / `PROTOCOL.md` §1) |
| Header | `agent-bus-agent-suffixes v3 sha256=<64 hex chars over the body>` |
| Body | one `<agent-name> <highest-suffix-burned>` line per name, sorted by name; floor 0 is spelled by ABSENCE, never by an explicit `0` |
| Write discipline | temp file in the same directory, fsynced, renamed over the target, directory fsynced — never torn |
| Failure posture | ANY verification failure (bad header, unknown version, digest mismatch, malformed or duplicate entry) is FATAL and the file is NEVER regenerated — regenerating would resume every name from 1 and re-mint agent ids already on disk (invariant 1) |

Full byte-layout prose lives in `PROTOCOL.md` §9 (new section, this task) rather than duplicated here.

**New Go API, package `ids` (`internal/ids/suffixstore.go`):**

| Symbol | Kind | Contract |
|---|---|---|
| `OpenNameSuffixes(dir string) (*DurableNameSuffixes, error)` | func | Loads the persisted floors. Missing file → empty floors, `existed=false`. Corrupt file → fatal, wraps `ErrSuffixFileCorrupt`, never regenerated. Returned allocator is born **unsealed**. |
| `(*DurableNameSuffixes) RaiseFloor(name string, atLeast uint64) error` | method | Legal only before `Seal`; merges an externally-derived floor. Validates `name` via `ValidateAgentName` (new — see below). |
| `(*DurableNameSuffixes) Seal() error` | method | Persists the merged floors to disk FIRST, then seals in memory. A failed write leaves the allocator unsealed and refusing — never partially sealed. |
| `(*DurableNameSuffixes) NextSuffix(name string) (uint64, error)` | method | Implements `ids.SuffixAllocator`. Persists + fsyncs `floor[name] = n` BEFORE returning `n`. Validates `name`. |
| `(*DurableNameSuffixes) LastSuffix(name string) uint64` | method | Highest suffix issued by THIS process for `name`, not what disk holds — see `Floors` for that. |
| `(*DurableNameSuffixes) Floors() map[string]uint64` | method | Snapshot copy of the floors currently on disk; mutating the returned map does not affect the allocator. |
| `(*DurableNameSuffixes) Existed() bool` | method | Whether `agent-suffixes` was present at open — tracks FILE presence, not floor count (an empty-but-present file also reports `true`). |
| `(*DurableNameSuffixes) Path() string` | method | The file path, for operator messages and tests. |
| `ErrSuffixFileCorrupt` | var (error) | Sentinel for any on-disk verification failure. Fatal, non-recoverable by regeneration. |

**Wiring status — corrected.** This entry originally said `cmd/agent-bus/main.go:327` still called
`ids.NewNameSuffixes()` and that `OpenNameSuffixes` had zero production callers, and that a restarting
bus therefore still re-minted agent ids. All three clauses are now FALSE and are corrected here rather
than left standing. `ids.NewNameSuffixes()` has been removed from `cmd/` entirely — the call site in
`main.go`'s `run()` carries a comment stating it "is gone from cmd/ and MUST NOT come back, on any
path, including as a fallback for a failed open or a failed seal." `cmd/agent-bus/suffixfloors.go`'s
`openSuffixAllocator` calls `ids.OpenNameSuffixes(dataDir)` on every startup and is itself invoked from
`main.go`'s `run()`, ahead of the agent-id minter and the auth service, so every enrolment goes through
the durable allocator. A restarting bus therefore does **not** re-mint agent ids: enrolled agent ids
and the floors that protect them survive both a graceful restart and a `SIGKILL`. The one remaining
operator-facing gap this wiring introduced — the `-backfill-suffix-floors` flag, for a legacy directory
that predates the floors file — is documented in the new-CLI-flag table above. See `DECISIONS.md`
(2026-08-07, "Four rulings: refuse-to-boot exception, format break, binary rename, redeploy") and
`internal/ids/doc.go` for the allocator-side detail.

## 2026-08-07 — RELAY-2 / RELAY-3: bus-to-bus relay + roster sync (NOT REGISTERED)

**No new HTTP route is served, no new env var, no new CLI flag.** `internal/relay` registers NO
handler on any mux, authenticates NO peer, and is imported by NOTHING outside itself — enforced by
`internal/relay/guards_test.go`, which fails the build if any other package in the repository imports
it. This mirrors RELAY-1's precedent exactly: `/v1/peer/enroll` was never added to
`CONTRACTS-HTTP.md`'s route tables for the same reason, and that omission was deliberate, not an
oversight. **Do not add `/v1/peer/relay` or `/v1/peer/roster` to `CONTRACTS-HTTP.md` either** — there
is nothing there yet to add. **No `scripts/bus-*.sh` wrapper is owed by this task.** CLAUDE.md
invariant 7 requires a wrapper for every agent-facing capability shipped "in the same task"; this is a
bus-to-bus (server-to-server) surface, not agent-facing, so the wrapper requirement does not apply
here — stated explicitly so this is not mistaken for a skipped step.

**Nothing below is reachable today.** It documents the PLANNED shapes `internal/relay` now carries in
Go, so that the eventual gate tasks — `INVITE-PEERGUARD` (`f5d91dbe`) and `MTLS-RELAYGUARD`
(`8192c3c7`), neither of which has landed — have one place to read the contract from. Until BOTH land
and a wiring task registers these handlers on a listener, nothing here is served, nothing here is
reachable from the network, and nothing here should be wired into `internal/httpapi` or any other mux.

**Path constants (reserved spellings, not registrations):**

| Constant | Value | Surface |
|---|---|---|
| `PeerEnrollPath` | `/v1/peer/enroll` | RELAY-1's handshake (already unregistered; listed here for completeness) |
| `PeerRelayPath` | `/v1/peer/relay` | RELAY-2's message relay ingress |
| `PeerRosterPath` | `/v1/peer/roster` | RELAY-2's ongoing roster sync ingress |

**`RelayRequest`** — the body one bus POSTs to another at `PeerRelayPath`. Every field is untrusted
peer input; nothing here proves the sending bus is entitled to speak for `origin_bus`.

| Field | JSON key | Notes |
|---|---|---|
| OriginBus | `origin_bus` | the bus that accepted the message from its own agent; authority for `message_id` and `sender`'s namespace |
| MessageID | `message_id` | the ORIGIN's `"<bus-id>-<seq>"` id — never this bus's own id for the message |
| Sender | `sender` | fully-qualified `<bus-id>.<agent-id>`, inside `origin_bus`'s namespace |
| Broadcast | `broadcast` | exactly one of `broadcast` and a non-empty `recipients` is set |
| Recipients | `recipients` | fully-qualified ids; empty for a broadcast |
| BusPath | `bus_path` | ordered traversed-bus list; loop-prevention/provenance metadata, NOT covered by any signature |
| SentAtUnixNs | `sent_at_unix_ns` | the ORIGIN bus's clock — provenance only, never authorization input |
| Size | `size` | declared body length, cross-checked against the actual body |
| ContentSHA256 | `content_sha256` | hex SHA-256 of `body` |
| Body | `body` | opaque payload, carried verbatim |

**`RelayResponse`** — the answer to a relay POST, always HTTP 200 once past transport-level checks
(see the status mapping below):

| Field | JSON key | Notes |
|---|---|---|
| Accepted | `accepted` | this bus took responsibility for the message, now or previously |
| Duplicate | `duplicate` | `idem.OutcomeRetry` — same key, same payload; the ORIGINAL result is being replayed, nothing re-applied, nobody disconnected |
| DroppedReason | `dropped_reason` | omitted unless `accepted` is false and the outcome is final; today the only value is `"loop"` (`DropLoop`) |
| MessageID | `message_id` | the id THIS bus minted for its own copy — never the origin's; empty when nothing was accepted |

**`RosterUpdate`** — one incremental change to a peer's roster, pushed as that peer's own agents come
and go, at `PeerRosterPath`:

| Field | JSON key | Notes |
|---|---|---|
| BusID | `bus_id` | the peer whose roster this describes — a peer describes only its own roster |
| Version | `version` | the peer's own monotonic ROSTER EPOCH — see the naming hazard below, this is NOT a wire-protocol version |
| Added | `added` | fully-qualified ids inside `bus_id`'s namespace |
| Removed | `removed` | fully-qualified ids inside `bus_id`'s namespace; disjoint from `added` |

**`RosterUpdateResponse`**:

| Field | JSON key | Notes |
|---|---|---|
| Applied | `applied` | whether the update was applied (including as a replayed duplicate) |
| Version | `version` | the version now in force for that peer, so the pusher can detect divergence without a second round trip |

**Caps:**

| Constant | Value | Notes |
|---|---|---|
| `MaxRelayBytes` | 256 KiB | encoded relay payload, checked before decode; derived, not guessed — see `message.go` |
| `MaxBusPath` | 64 | hard-linked to `store.MaxBusPath` (`MaxBusPath = store.MaxBusPath`) — the relay ingress cap must never exceed the on-disk cap, or an accepted-but-unpersistable path becomes an acknowledged-but-lost message (invariant 5) |
| `MaxRosterUpdateEntries` | 256 | `added` plus `removed`, together — not each |
| `MaxRosterUpdateBytes` | 64 KiB | encoded roster update, checked before decode |
| `MaxPeers` | 64 | peer buses one `Registry` holds |
| `MaxRosterAgents` | 1024 | agents in one peer's roster; the product `MaxPeers * MaxRosterAgents` (65,536 ids) is the real routing-table ceiling |

**Status mapping** (both surfaces use one vocabulary; codes are `CodeXXX` string constants, never the
raw Go error):

| Condition | HTTP | Body | Notes |
|---|---|---|---|
| Non-POST | 405 | `CodeMethodNotAllowed` | |
| Non-JSON content type | 415 | `CodeUnsupportedMediaType` | |
| Over the byte cap | 413 | `CodePayloadTooLarge` | |
| Malformed envelope | 400 | `CodeInvalidRequest` / other `ErrorCode(err)` | |
| **Loop drop (relay only)** | **200** | `{"accepted":false,"dropped_reason":"loop"}` | **NOT an error status.** A loop is the expected steady state of a cyclic topology and is nobody's fault; a 5xx would make RELAY-4's future retry/backoff re-deliver forever the one thing that can never be accepted, and a 4xx would blame a sender that cannot know our federation graph. See `relayhttp.go`'s `ServeHTTP` doc for the full three-part argument. |
| **Duplicate (relay only)** | 200 | `{"accepted":true,"duplicate":true,"message_id":"<original>"}` | invariant 10: return the original result, re-apply nothing, disconnect nobody |
| **Idempotency VIOLATION (relay and roster)** | **409** | `CodeIdempotencyViolation` | same key, different payload. Rejected and logged, and **NOBODY IS DISCONNECTED** — invariant 10 as narrowed 2026-08-08. **The 409 plus the log line is the COMPLETE remedy and no gate task owes more:** this row previously said the peer SHOULD be disconnected and instructed `MTLS-RELAYGUARD` (`8192c3c7`) to wire it up. **That instruction is WITHDRAWN and must not be reinstated** (`internal/relay/relayhttp.go`'s `ErrIdempotencyViolation` doc) — a relay connection multiplexes every agent behind that peer, so dropping it over one caller's key bookkeeping punishes all of them. |
| Stale roster update (roster only) | 409 | `CodeStaleRoster` | version not strictly greater than the one already applied — the update is well-formed, it just lost a race or the peer regressed its counter; recovery is a re-handshake |
| Unknown peer (roster only) | 403 | `CodeUnknownPeer` | a roster update may never CREATE a peer — accepted as a residual, this 403-vs-409 split is itself a peer-enumeration oracle until the gate authenticates the caller (see `doc.go`) |
| Callback failure, otherwise | 503 | `CodeUnavailable` | "not now", so a peer knows retrying is correct |

**The `Idempotency-Key` header rule — unusual and load-bearing.** Every mutating surface carries the
key in `idem.HeaderName` (invariant 10), but **on the relay surface the key MUST equal the origin's
`message_id`** — `ValidateRelayRequest` refuses the envelope otherwise (`ErrRelayKeyMismatch`). A
per-hop key would make every copy of one message, however it was routed, look new to
`internal/idem` and would defeat duplicate suppression silently: the same message arriving via two
disjoint peers must resolve to ONE `idem.Scope`, and only the origin's own message id is common to
every copy. On the roster surface there is no such natural id to reuse — the pusher mints its own key
and reuses it across retries of one update; the per-peer `version` field is the independent mechanism
that makes a late or reordered update harmless, and neither field replaces the other (both are needed
because updates cross an unordered, at-least-once channel).

**Two naming hazards a future task will otherwise trip over:**

1. **`RosterUpdate.Version` is a peer ROSTER EPOCH, not a wire-protocol version**, and it already
   occupies the JSON key `"version"`. It is the peer's own monotonic counter for its own namespace
   (minted by the peer, which does not breach invariant 1 — that invariant governs ids WE mint, and a
   peer's roster epoch is a fact only the peer can know about its own state). The task that eventually
   adds a reserved wire-protocol version field to these envelopes **must not reuse `"version"`** for
   it — use `"protocol_version"` or rename the epoch, but do it deliberately. Conflating the two is how
   a peer ends up applying a roster epoch as a format number.
2. **No relay wire-protocol version has been RESERVED.** Neither `RelayRequest` nor `RosterUpdate`
   carries a version field today, for the same reason `PeerEnroll`'s payload does not (see `doc.go`,
   "No wire protocol version field, on purpose"): versions are allocated through the Spec Server
   reservations API (`POST /api/v1/projects/agent-bus/reservations`), never hand-picked by the agent
   writing the code, and because nothing serves these handlers the format is not yet on any wire, so
   there is nothing to stay compatible with yet. **The task that first REGISTERS these handlers must
   reserve a version and add the field to both surfaces at once** — that reservation is not done by
   this documentation pass.

Full design rationale — the seven properties raised for the gate tasks, the residuals, why loop
prevention is availability-only, why the fingerprint excludes `bus_path` — lives in
`internal/relay/doc.go`'s package comment and in `PROTOCOL.md`'s new loop-prevention section rather
than duplicated here; this entry is the contract shape, not the argument for it.

## 2026-08-07 — SIGN-7: the relay envelope is SIGNED (still NOT REGISTERED, still NOT SERVED)

**No new HTTP route, no new env var, no new CLI flag, and no `scripts/bus-*.sh` wrapper or
`AGENT_PROTOCOL.md` entry is owed by this task.** `internal/relay` still registers NO handler on any
mux and is still imported by NOTHING outside itself (a guard test in `internal/relay/guards_test.go`
walks the repository and fails otherwise), and it remains gated behind `INVITE-PEERGUARD` (`f5d91dbe`) and `MTLS-RELAYGUARD`
(`8192c3c7`), neither of which has landed. This is a bus-to-bus surface, not an agent-facing one, so
CLAUDE.md invariant 7's wrapper requirement does not apply — stated explicitly, as the RELAY-2/RELAY-3
entry above does, so it is not mistaken for a skipped step. **Nothing below is reachable from the
network; no relayed signature is verified in production and no cross-bus message flows.**

**`RelayRequest` field changes — these SUPERSEDE the `RelayRequest` table in the RELAY-2 / RELAY-3
entry above.** Not a compatibility break: the envelope is not yet on any wire, so there is nothing to
stay compatible with.

| Field | JSON key | Change | Notes |
|---|---|---|---|
| ~~SentAtUnixNs~~ | ~~`sent_at_unix_ns`~~ | **REMOVED** | it was the ORIGIN BUS's nanosecond clock, not the sender's signed clock, so the canonical bytes could not be reconstructed from the envelope at all |
| TimestampUnixMilli | `timestamp_unix_ms` | **ADDED** (`int64`) | the SENDING AGENT's signed wall clock in Unix ms — the exact integer `signing.Message.TimestampUnixMilli` covers, carried verbatim with no conversion anywhere. Must be > 0 (0 is an unset field, not the epoch). **PROVENANCE ONLY — never the local `store.Message.SentAt`**, which is an authorization input (`VisibleTo` compares it against the enrolment instant) |
| OriginAttestation | `origin_attestation` | **ADDED** (object) | the origin bus's signed binding of `sender` to its messaging key. Its stable snake-case fields are `agent_id`, `messaging_public_key`, `key_epoch`, `issued_at_unix_ms`, `not_after_unix_ms`, and `signature`; it is carried unchanged across hops and verified only against pins configured for `origin_bus`, never re-attested by an intermediate |
| Signature | `signature` | **ADDED** (64 bytes, base64 in JSON) | the origin agent's detached Ed25519 signature over `signing.Canonicalize`'s output, unhashed, carried verbatim on every hop. Exactly `ed25519.SignatureSize`; any other length is treated as no signature |

Every other `RelayRequest` field is unchanged. The relay **fingerprint** (`relayFingerprint`) now
folds the recipient list **sorted** rather than in wire order, because `signing.Canonicalize` sorts —
so the fingerprint's notion of "the same payload" matches the signature's; an order-sensitive
fingerprint made a mere re-ordering of a validly signed recipient array an `idem.OutcomeViolation`,
which invariant 10 answers by rejecting and logging it (**not** by disconnecting the sender —
narrowed 2026-08-08). It still excludes `bus_path` and still
excludes the signature itself. See `PROTOCOL.md` §8.5 and §10.

**New Go-level construction requirement (not a wire surface):** `relay.RelayConfig.Trust` — a
`relay.CrossBusTrust` — is **required**, and `NewRelayHandler` returns an error without one.
`ValidateRelayRequest` takes it as a required parameter and runs `VerifyRelayed` before returning, so
no validated-but-unverified `RelayedMessage` can exist. A nil trust is a refusal, never a skipped
check; there is no "verification disabled" mode or default construction. `relay.PeerStore` is the
production implementation: it verifies the envelope-carried origin attestation against the durable,
operator-configured pins for that origin bus, and refuses a store without RELAY-34's durable
withdrawal floor. It remains an internal unwired seam — no mux or composition-root wiring is added
here.

**New error codes, appended to the status mapping in the entry above.** All three are FINAL — a retry
cannot change any of the verdicts — so none is a 503 and none is a `dropped_reason` (which rides on
HTTP 200 and means "settled, and not your fault"):

| Condition | HTTP | Body | Notes |
|---|---|---|---|
| Missing, wrong-length or uncanonicalizable signature — **including a relayed broadcast** | **400** | `CodeUnsigned` = `"unsigned"` | "nobody could verify this envelope". A relayed broadcast has no recipient list, canonical format v1 refuses an empty recipient set, so no signature over one can exist; exempting broadcasts would be an unauthenticated downgrade selectable from the wire. **Relayed broadcasts do not work today, deliberately — SIGN-3 owns the fix.** |
| Missing or malformed `origin_attestation` | **400** | `CodeInvalidRelay` = `"invalid_relay"` | malformed origin-attribution evidence is refused before trust lookup or delivery |
| Signature does not verify, or no attested signer key for the sender | **403** | `CodeBadSignature` = `"bad_signature"` | an authorization answer: "we will not attribute this to that agent" |
| No peering-time pin held for the origin bus's signing key | **403** | `CodeUnpeeredBus` = `"unpeered_bus"` | a distinct **operator** problem from `bad_signature`: the remedy is to complete a peering, not to hunt a forgery. NOT a "not yet" — nothing a retry does establishes a pin, and there is deliberately no trust-on-first-use fallback |

**Cross-bus key trust is now DECIDED** (`DECISIONS.md` 2026-08-07, "Cross-bus key trust: pin the origin
bus key at peering, NO TOFU" and "The bus TLS key and the bus SIGNING key are SEPARATE"). The origin
bus's attestation of its own agent's messaging key travels **intact**, signed by the **origin bus's
signing key**, never re-attested by an intermediate; that signing key is pinned **at peering time**;
and there is no trust-on-first-use anywhere. A bus that has not been peered with cannot have its
agents' signatures verified — **intended behaviour, not a gap**. The bus **TLS** key (pinned by
clients from the invite blob's certificate fingerprint) and the bus **SIGNING** key (pinned by peers
at peering time) are different keys with different rotation blast radii and different compromises, and
pinning one does not give you the other. Normative text and the reasoning: `PROTOCOL.md` §8.5.

**KNOWN GAP — the peering handshake still does not bootstrap a signing-key pin, and RELAY-17 does
not serve relay ingress.** `PeerEnrollRequest` / `PeerEnrollResponse` still carry only `bus_id` and
`agents`; an operator instead records the origin pin durably with `agent-bus peer add -signing-key`
(the independently documented offline configuration path). The server composition root still does
not construct the `PeerStore` trust implementation or register a peer relay route, so no running bus
accepts relay traffic yet. Adding a handshake key field remains `INVITE-PEERGUARD` (`f5d91dbe`): it
must arrive over the operator-mediated peering channel, never as trust-on-first-use.

**Still no reserved relay wire-protocol version.** SIGN-7 added fields to `RelayRequest` without one,
for the same reason the entry above gives: nothing serves these handlers, so the format is not yet on
any wire. The task that first REGISTERS these handlers must reserve a version and add the field to
both surfaces at once.

## 2026-08-07 — AUTH-7 + SIGN-2/SIGN-6: durable enrolment WIRED, and a signature is MANDATORY

Unlike the RELAY/SIGN-7 entries above, **this wave is REACHABLE**: it changes routes that are served,
records that are written, and behaviour an operator will notice on the next restart. Each plane file
carries the detail; this entry is the index and the list of breaks.

**Where the detail lives**

| Change | Plane file |
|---|---|
| `POST /v1/mint` (NEW), `POST /v1/send` (BREAKING request), `POST /v1/broadcast` (now 501), `timestamp_ms` + `signature` on the read path, the roster/session durability table | `CONTRACTS-HTTP.md` |
| `store.RecordVersion` 1 → **2** and its bidirectional break; the new `wal.Entry.Kind` value `"seqfloor"`; sequence numbers advancing in jumps | `CONTRACTS-ONDISK.md` |
| `agent-busctl send`'s two-step; `agent-busctl broadcast` exiting 6; the MESSAGING keypair and `messaging_key_seed`; `<identity-dir>/trusted-keys/`; the new `client` exported surface | `CONTRACTS-CLI.md` |
| Invariant 7's status — `agent-busctl keygen` and `agent-busctl trust` DO NOT EXIST | `CONTRACTS-AGENT.md` |
| The reserve-then-send flow as an agent performs it | `AGENT_PROTOCOL.md` |

**Reservations used, none hand-picked:** `store.RecordVersion = 2` from the Spec Server
`store-record-version` namespace on 2026-08-07 (value `1` seeded in the same pass to cover the
already-shipped v1 record). `wal.Entry.Kind` needed none — it is a free-form application string, as
`CONTRACTS-ONDISK.md` already recorded. `signing.FormatVersion` is UNCHANGED at `1`; nothing in this
wave touches the canonical format.

**The three breaks, stated together so none is missed**

1. **`POST /v1/send` is a BREAKING wire change.** Five new required fields. A pre-SIGN-6 client is
   rejected with a 400 and cannot send at all.
2. **`POST /v1/broadcast` is a REGRESSION of a working feature**, to 501, deliberately and failing
   closed. SIGN-6 admits no unsigned message type and a broadcast has no canonical audience under
   signing format v1. SIGN-3 owns re-opening it.
3. **`store.RecordVersion` 1 → 2 is a DESTRUCTIVE, BIDIRECTIONAL on-disk break.** Upgrading discards
   the message history; rolling back discards it again. There is no migration and there cannot be
   one. Enrolment, invite and seqfloor records are unaffected — **an agent does not have to
   re-enrol.**

**Also true, and the good news of the wave:** AUTH-7 means agents **no longer re-enrol after a
restart**. Agent ids, public keys and each agent's ORIGINAL `enrolled_at` survive a restart and a
`SIGKILL`. Sessions remain memory-only, deliberately and permanently, so every agent must redo the
session handshake — but not the enrolment.

**Honest limits, so nothing here is oversold.** No messaging public key is registered at enrolment
and CRYPTO-4 does not exist, so a recipient can obtain a sender's messaging public key ONLY out of
band; recipient-side verification is not yet wired into `client.Read`; and `agent-busctl` has no `keygen`
or `trust` subcommand. Signing works end to end and the signature is carried and returned. Automatic
verification on receive does not happen yet.
