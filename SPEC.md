# Agent Bus

> Checkbox legend: `[ ]` todo · `[~]` in progress · `[x]` done · `[-]` superseded/cancelled.

## Backlog

### EPIC AGENTIF — Agent-facing surface (shell wrappers + protocol doc)

- [ ] AGENTIF-2 · AGENTIF-2: scripts/bus-enrol.sh + AGENT_PROTOCOL.md entry — agentif, P0
  Wrapper for POST /v1/enroll -- generates/loads local key material, submits it, stores the returned token+agent-id locally for subsequent wrapper calls. Pairs with the enroll-endpoint task; per invariant 7 they ship together.
  _Proof: scripts/bus-enrol.sh --name testagent_
- [ ] AGENTIF-6 · AGENTIF-6: scripts/bus-wait.sh + AGENT_PROTOCOL.md entry — agentif, P1
  Wrapper for GET /v1/wait, looping the cursor forward across calls and printing new messages as they arrive. Pairs with the long-poll endpoint task; per invariant 7 they ship together.
  _Proof: scripts/bus-wait.sh --timeout 5_
- [ ] AGENTIF-8 · AGENTIF-8: scripts/bus-peer.sh + AGENT_PROTOCOL.md entry — agentif, P2
  Wrapper for the peer-enrolment handshake (add/list/remove a peer bus). Pairs with the peer-enrolment task; per invariant 7 they ship together.
  _Proof: scripts/bus-peer.sh add http://peer-host:8081_
- [ ] AGENTIF-1 · AGENTIF-1: scripts/bus-serve.sh + AGENT_PROTOCOL.md entry — agentif, P0
  Wrapper to start/stop/status a local agent-bus server (foreground or backgrounded with a pidfile) plus its AGENT_PROTOCOL.md section. Pairs with the main-entrypoint task -- needed first since every other wrapper assumes a running server to talk to. Per invariant 7 the wrapper and doc entry land in the SAME task/commit as the feature it fronts.
  _Proof: scripts/bus-serve.sh start && scripts/bus-serve.sh status && scripts/bus-serve.sh stop_
- [ ] AGENTIF-7 · AGENTIF-7: scripts/bus-leave.sh + AGENT_PROTOCOL.md entry — agentif, P1
  Wrapper for POST /v1/leave, clearing the locally stored token afterward. Pairs with the leave/revocation task; per invariant 7 they ship together.
  _Proof: scripts/bus-leave.sh_
- [ ] AGENTIF-3 · AGENTIF-3: scripts/bus-agents.sh + AGENT_PROTOCOL.md entry — agentif, P1
  Wrapper for GET /v1/agents. Pairs with the roster-listing task; per invariant 7 they ship together.
  _Proof: scripts/bus-agents.sh_
- [ ] AGENTIF-4 · AGENTIF-4: scripts/bus-broadcast.sh + AGENT_PROTOCOL.md entry — agentif, P1
  Wrapper for POST /v1/broadcast. Pairs with the broadcast task; per invariant 7 they ship together.
  _Proof: scripts/bus-broadcast.sh "hello bus"_
- [ ] AGENTIF-5 · AGENTIF-5: scripts/bus-send.sh + AGENT_PROTOCOL.md entry — agentif, P1
  Wrapper for POST /v1/send (DM). Pairs with the direct-message task; per invariant 7 they ship together.
  _Proof: scripts/bus-send.sh <agent-id> "hello"_

### EPIC AUTH — Enrolment & authentication

- [ ] AUTH-5 · AUTH-5: Auth crash/recovery test — auth, P1
  End-to-end crash-injection test: enrol an agent, simulate a crash before/after the commit fsync at each stage, restart, and assert the token is valid iff the enrolment was durably committed; separately, revoke an agent, crash, restart, and assert the token stays rejected.
  _Proof: go test -race -run TestAuthCrashRecovery ./internal/auth_
- [ ] AUTH-2 · AUTH-2: Token verification middleware — auth, P0
  Middleware that validates the bearer token on every route except /healthz, /v1/info, and /v1/enroll (invariant 3) -- rejects missing/malformed/forged/expired tokens with 401, and attaches the verified fully-qualified agent id to the request context for downstream handlers.
  _Proof: go test -race -run TestAuthMiddleware ./internal/auth_
- [ ] AUTH-1 · AUTH-1: POST /v1/enroll -- signed credential issuance — auth, P0
  Agent submits a name + client-generated key material. Server RECORDS A DESIGN DECISION in DECISIONS.md (HMAC-SHA256 over agent-id+key with a persisted bus secret, or Ed25519 -- implementer's call, must be justified there), persists a bus signing secret (generated once, like the bus-id task), mints the agent id, signs a credential, and returns a bearer token. The whole enrolment (roster entry + credential) goes through the two-phase write path -- a client never gets a token for an enrolment that isn't durable.
  _Proof: go test -race -run TestEnroll ./internal/auth_
- [ ] AUTH-4 · AUTH-4: POST /v1/leave -- leave / revocation — auth, P1
  Lets an enrolled agent durably remove itself from the roster; its token is rejected by the auth middleware on every call afterward, including after a restart (the revocation itself goes through the two-phase write path).
  _Proof: go test -race -run TestLeaveRevocation ./internal/auth_
- [ ] AUTH-3 · AUTH-3: Roster persistence & recovery — auth, P0
  The agent roster (id, name, public key/verifier material, enrolled-at) is rebuilt on startup by WAL replay, not held only in memory -- an agent enrolled before a restart is still authenticated and listed after one, with no re-enrolment required.
  _Proof: go test -race -run TestRosterRecovery ./internal/auth_

### EPIC CORE — Repo skeleton & server bootstrap

- [ ] CORE-5 · CORE-5: Observability: metrics/inspect endpoint (follow-up) — observability, P3
  Low-priority follow-up. Add a GET /v1/debug (or /metrics) endpoint exposing in-process counters (messages sent, active waiters, WAL bytes, roster size, relay peer status) as plain JSON -- stdlib-first, no Prometheus dependency needed initially. Depends on MSG/POLL/RELAY existing to have something worth reporting.
  _Proof: go test -race -run TestInspectEndpoint ./internal/http_
- [ ] CORE-2 · CORE-2: cmd/agent-bus main entrypoint + config/flags — core, P0
  cmd/agent-bus/main.go wires flag parsing (listen addr, data-dir, bus-id override for testing, long-poll timeout, log level) into a Config struct; server binds the listener and shuts down cleanly on SIGINT/SIGTERM. No routes yet beyond a bare mux.
  _Proof: go build ./... && ./agent-bus -h_
- [ ] CORE-3 · CORE-3: GET /healthz and GET /v1/info endpoints — core, P0
  GET /healthz returns 200 {"status":"ok"} once the server is accepting connections (liveness only, no auth). GET /v1/info returns bus id, server version/build info, and uptime (also unauthenticated -- needed for pre-enrolment discovery). Both registered on the main mux.
  _Proof: go test -race -run TestHealthzInfo ./internal/http_
- [ ] CORE-4 · CORE-4: Structured logging + request middleware — core, P0
  log/slog-based structured logger (stdlib, no third-party dep per invariant 8), wired as HTTP middleware logging method/path/status/latency/request-id for every route. Configurable level via the -log-level flag.
  _Proof: go test -race -run TestLoggingMiddleware ./internal/http_
- [ ] CORE-1 · CORE-1: Repo skeleton: go.mod, internal/ package layout, .gitignore — core, P0
  Initialize go.mod (module path, go1.19 toolchain pin), create the internal/ package layout (ids, store, wal, hub, http, relay, auth) as empty packages with doc.go stubs, cmd/agent-bus/ dir, and a .gitignore for build artifacts and the data/ dir. No server logic yet -- this is the scaffold every other task builds on.
  _Proof: go build ./... && test -z "$(gofmt -l .)"_

### EPIC DOCS — Documentation

- [ ] DOCS-2 · DOCS-2: PROTOCOL.md -- wire protocol + on-disk format — docs, P1
  Every HTTP route (method, path, auth requirement, request/response shape) and the on-disk format (WAL record framing, audit log format, roster/counter file layouts) -- maintainer-facing, kept current as routes land.
  _Proof: test -s PROTOCOL.md_
- [ ] DOCS-1 · DOCS-1: README.md + DECISIONS.md seed — docs, P0
  README.md -- what agent-bus is, quickstart (build, run one bus, enrol two agents, exchange a message via the wrappers). DECISIONS.md -- seeded with its append-only-dated-entry convention and a placeholder for the enrolment signing-scheme decision. Written early so later tasks have somewhere to record decisions.
  _Proof: test -s README.md && test -s DECISIONS.md_
- [ ] DOCS-3 · DOCS-3: CONTRACTS.md -- route/flag/env-var/record-type table — docs, P1
  A single table of every route, CLI flag, env var, and durable record type, with the convention that every future task updates it in the same commit that changes any of those surfaces (CLAUDE.md step 9).
  _Proof: test -s CONTRACTS.md_

### EPIC DUR — Durability: WAL, two-phase commit, recovery, audit log

- [ ] DUR-2 · DUR-2: Two-phase prepare->commit write path — durability, P0
  Implement prepare(record)->commit(id) as two distinct fsynced WAL appends; in-memory state is applied ONLY after the commit record is durable. A response is never sent to the caller until commit-fsync completes (invariant 4). This is the write path every mutating route (enrol, send, broadcast, leave) goes through.
  _Proof: go test -race -run TestPrepareCommit ./internal/wal_
- [ ] DUR-4 · DUR-4: Corrupt-tail detection & truncation — durability, P0
  During replay, a checksum mismatch or short read at the END of the WAL (the torn record a crash mid-write leaves behind) is detected, logged, and the file is truncated at the last verified-good record boundary -- the ONLY truncation ever permitted (invariant 6). A corrupt record anywhere but the tail is a fatal startup error, not a truncation.
  _Proof: go test -race -run TestCorruptTailTruncation ./internal/wal_
- [ ] DUR-5 · DUR-5: Append-only message audit log — durability, P0
  A second, separate append-only file (distinct from the WAL) that every message (broadcast + DM) is written to as part of the same commit, independent of the WAL's own record-keeping -- the audit trail invariant 6 calls out explicitly. Never edited or truncated except by the verified-corrupt-tail rule.
  _Proof: go test -race -run TestAuditLog ./internal/wal_
- [ ] DUR-7 · DUR-7: Snapshot/compaction follow-up (bounds WAL replay time) — durability, P3
  Low-priority follow-up. As the WAL grows unbounded, startup replay time grows with it; add periodic snapshotting of in-memory state plus safe truncation of the WAL prefix the snapshot covers, so recovery time is bounded by (snapshot load + tail replay) rather than full history. Not required for correctness, only for long-run startup latency.
  _Proof: go test -race -run TestSnapshotCompaction ./internal/wal_
- [ ] DUR-1 · DUR-1: WAL record framing + writer — durability, P0
  Define the on-disk WAL record format (length-prefixed + CRC32 checksum per record, monotonic record index) in internal/wal, and implement the append-only writer: Append(record) writes framed bytes and fsyncs before returning. The single building block every other DUR task builds on.
  _Proof: go test -race -run TestWALFraming ./internal/wal_
- [ ] DUR-6 · DUR-6: Crash-injection test suite for the write path — durability, P0
  A test harness that writes, then simulates a crash (kill / truncate / corrupt the file at a chosen byte offset) at each stage of the two-phase write path -- before prepare fsync, between prepare and commit, mid-commit-write, after commit fsync -- and asserts recovery always yields a valid PREFIX of the accepted history: nothing acknowledged is ever lost, nothing unacknowledged is ever visible. The load-bearing evidence for invariants 4/5.
  _Proof: go test -race -run TestCrashInjection ./internal/wal_
- [ ] DUR-3 · DUR-3: Replay/recovery on start — durability, P0
  On startup, replay the WAL from the beginning, reconstructing in-memory state (roster, sequence counters, message store) by applying only records that reached commit -- any prepare without a matching commit is discarded. Uncommitted prepares must never be visible after a restart.
  _Proof: go test -race -run TestWALReplay ./internal/wal_

### EPIC ID — Server-authoritative id minting

- [ ] ID-1 · ID-1: Bus id minting + persistence — id, P0
  On first start with an empty data-dir, generate a bus id (opaque random/ULID-style string), persist it to a file in data-dir, and load the SAME id on every subsequent restart rather than regenerating. Exposed via GET /v1/info. This is the root of invariant 2's `<bus-id>.<agent-id>` namespacing.
  _Proof: go test -race -run TestBusIDPersistence ./internal/ids_
- [ ] ID-4 · ID-4: Id-counter recovery property test — id, P1
  Cross-cutting test (depends on the WAL replay task): enrol several agents and send several messages, kill the process, restart, and assert every counter (sequence, per-name agent suffix) resumes strictly above its last-issued value -- table-driven across several kill points.
  _Proof: go test -race -run TestIDCounterRecovery ./internal/ids_
- [ ] ID-3 · ID-3: Agent id minting `<bus-id>.<name>-<n>` — id, P0
  Server mints the fully-qualified agent id at enrolment: client submits a desired short name, server appends a durable per-name counter suffix (-1, -2, ...) so a reused name never collides with a previous holder, and prefixes the bus id. Client never chooses its own id (invariant 1).
  _Proof: go test -race -run TestAgentIDMinting ./internal/ids_
- [ ] ID-2 · ID-2: Monotonic sequence allocator (drives message ids) — id, P0
  A durable, gap-free monotonic counter (internal/ids) that the WAL commit path advances -- every allocated sequence number is durable before it is handed out. Message ids are `<bus-id>-<seq>`. Counter state is restored by the WAL replay task so a restart never re-issues a previously-issued sequence number.
  _Proof: go test -race -run TestSequenceAllocator ./internal/ids_

### EPIC MSG — Messaging surface

- [ ] MSG-4 · MSG-4: Cursor semantics + GET /v1/messages history — messaging, P1
  Define an opaque cursor (wraps a per-agent-visible sequence position, not a raw offset a client could forge into another agent's stream) with encode/decode/validate, and implement the paginated history endpoint using it -- this is the SAME cursor type the long-poll wait endpoint consumes.
  _Proof: go test -race -run TestMessageHistoryCursor ./internal/hub_
- [ ] MSG-3 · MSG-3: POST /v1/send -- direct message — messaging, P1
  Targeted single-recipient send to a fully-qualified agent id; 404 on unknown recipient. Same durable write path as broadcast, delivered only to that agent's history/wait stream.
  _Proof: go test -race -run TestDirectMessageSend ./internal/hub_
- [ ] MSG-2 · MSG-2: POST /v1/broadcast — messaging, P1
  Any enrolled agent posts a message visible to the whole roster. Goes through the two-phase write path and the audit log before the 200 is returned; assigns a message id via the sequence allocator.
  _Proof: go test -race -run TestBroadcastSend ./internal/hub_
- [ ] MSG-5 · MSG-5: Messaging durability integration test — messaging, P1
  Crash-injection test for the messaging path specifically: simulate a crash mid-broadcast and mid-DM at each write-path stage, restart, and assert a message is either fully present (in history, in the audit log, and delivered to any waiter that should have seen it) or fully absent -- never partially visible.
  _Proof: go test -race -run TestMessagingCrashRecovery ./internal/hub_
- [ ] MSG-1 · MSG-1: GET /v1/agents -- roster listing — messaging, P1
  Authenticated endpoint returning the current enrolled-agent roster as fully-qualified `<bus-id>.<agent-id>` entries (name, id, enrolled-at). Read path only, no durability concerns beyond the already-recovered roster.
  _Proof: go test -race -run TestListAgents ./internal/hub_

### EPIC POLL — HTTP long-poll wait endpoint

- [ ] POLL-1 · POLL-1: GET /v1/wait -- long-poll endpoint — poll, P1
  Agent calls with its last-seen cursor; if messages exist beyond it, respond immediately; otherwise park the request until a new message arrives OR a configurable timeout elapses, at which point return 200 with an empty batch (not an error) and the same cursor.
  _Proof: go test -race -run TestLongPollWait ./internal/hub_
- [ ] POLL-3 · POLL-3: Poll concurrency test suite (goroutine leak + thundering herd) — poll, P1
  Two properties under -race: (1) a client disconnect mid-wait releases the parked goroutine promptly -- no leak, asserted via goroutine-count before/after; (2) thundering herd -- many agents parked on the same bus, one new broadcast wakes every eligible waiter exactly once, no duplicate or missed delivery.
  _Proof: go test -race -run TestPollConcurrency ./internal/hub_
- [ ] POLL-2 · POLL-2: Wake-on-new-message wiring — poll, P1
  The hub notifies every parked waiter whose cursor is behind a newly committed message -- wiring between the two-phase commit path and the waiter registry, so wake-up happens only after the write is durable, never before.
  _Proof: go test -race -run TestWaiterWakeup ./internal/hub_

### EPIC RELAY — Bus-to-bus federation

- [ ] RELAY-4 · RELAY-4: Peer-down retry/backoff — relay, P2
  If a peer is unreachable, relay to it retries with backoff on a background path rather than blocking the local sender's response -- a slow/dead peer must never make a local broadcast/DM slow or fail.
  _Proof: go test -race -run TestPeerRetryBackoff ./internal/relay_
- [ ] RELAY-2 · RELAY-2: Message relay + ongoing roster sync across peers — relay, P2
  A broadcast/DM whose target is (or might be, for broadcast) on a peer bus is forwarded to that peer using the fully-qualified agent id; roster changes (new enrolment, leave) are pushed to peers incrementally after the initial exchange so routing tables stay current.
  _Proof: go test -race -run TestMessageRelay ./internal/relay_
- [ ] RELAY-1 · RELAY-1: Peer enrolment + initial agent-list exchange — relay, P2
  A bus-to-bus handshake (POST /v1/peer/enroll or similar) where two buses mutually authenticate and exchange bus ids plus their current rosters, so each learns the other's fully-qualified agent ids for routing (invariant 2).
  _Proof: go test -race -run TestPeerEnrollment ./internal/relay_
- [ ] RELAY-3 · RELAY-3: Loop prevention via traversed-bus path — relay, P2
  Every relayed message carries the list of bus ids it has already traversed; a bus that sees itself in that list drops the message instead of re-relaying it -- required the moment peer topology has a cycle.
  _Proof: go test -race -run TestRelayLoopPrevention ./internal/relay_
- [ ] RELAY-5 · RELAY-5: Relay crash/loop integration test — relay, P2
  Multi-bus (3+) topology test with a cycle in the peer graph: send a broadcast, simulate a crash on one bus mid-relay, restart it, and assert every agent across the topology sees the message exactly once -- no loop, no duplicate, no loss.
  _Proof: go test -race -run TestRelayCrashLoopIntegration ./internal/relay_

