# Agent Bus

> This mirror lists OPEN tasks only (todo, in progress, blocked, deferred).
> 152 closed tasks are omitted; the Spec Server holds them in full.
> Regenerate with `bash scripts/gen-spec-mirror.sh` (`--all` to include closed).

> Checkbox legend: `[ ]` todo · `[~]` in progress · `[x]` done · `[-]` superseded/cancelled.

## Backlog

- [ ] None · RELAY-13-FU-KEYGEN: 3 error-message remedy strings name the nonexistent agent-busctl keygen subcommand — cli, P2, error-message, keygen, sign-8
  Three shipped error-message remedy strings tell the caller to run `agent-busctl keygen`, which does not exist (no keygen.go, no dispatch entry anywhere in cmd/agent-busctl) -- a remedy that cannot be followed. Confirmed by direct grep across the module (2026-08-08), Go source only, excluding test files and doc mentions:
  
  1. client/store.go:271 -- Credential.MessagingPrivateKey, "no messaging key yet" -> "run `agent-busctl keygen` to mint one, then hand the printed public key to your peers". This site is inside RELAY-13s client-half file boundary (client/store.go) but was NOT touched by that diff -- pre-existing, a different code path than the new damagedMessagingSeedRemedy const RELAY-13 added nearby.
  2. client/keyring.go:181 -- outside RELAY-13s boundary.
  3. client/client.go:477 -- outside RELAY-13s boundary (comment at :464 references the same nonexistent command but is prose, not a runtime error string).
  
  AGENT_PROTOCOL.md:552-554 already documents this gap accurately ("Some error messages tell you to run agent-busctl keygen; that command does not exist") and CONTRACTS-AGENT.md:70-73 and CONTRACTS-CLI.md:474-477 do too -- those are NOT defects, they are honest disclosure, and should stay as-is or be updated in step with whichever fix lands here.
  
  The real fix is SIGN-8 (71ef73d5-5625-44bb-959c-17b364200f4b, todo, P1, epic unclear -- title starts with SIGN-8), which is the task that adds the `agent-bus keygen` subcommand itself; landing it makes all three remedies true. This task exists as the narrower, cheaper interim option and as an explicit tracker in case SIGN-8 stays deferred: either (a) land SIGN-8 first and this task closes for free once the remedies are re-verified true, or (b) if SIGN-8 remains out of scope for a while, soften the three remedy strings to name an actionable alternative (e.g. embed the client package and call Store.EnsureMessagingKey / Client.MessagingPublicKey directly -- both already exist and do exactly this) so the error leaves SOME path forward, per the repos own rule that an error with no path forward is a defect in its own right.
  
  Relate to SIGN-8; do not duplicate its scope (SIGN-8 owns the actual keygen implementation).
  _Proof: bash -c 'set -e; ! grep -q "agent-busctl keygen" client/store.go client/keyring.go client/client.go'_
- [ ] None · AST guard: assert a doc comment attaches to the declaration it names (repo-wide godoc-attachment check) — tooling, P2, ast-guard, code-hygiene, godoc
  RELAY-13 client-half review (2026-08-08) found a real defect invisible to gofmt, go vet and every test: hoisting a new `const damagedMessagingSeedRemedy` between `Credential.MessagingPrivateKey`s doc comment and the function itself made godoc attach the WHOLE doc block to the constant, leaving the function undocumented -- and the orphaned doc carried a load-bearing rule (do not quietly mint key material in a read-only accessor; minting is a write and belongs under the store lock). `go doc ./client Credential.MessagingPrivateKey` printed nothing until fixed. The reviewer wrote a working AST scanner for this class (go/parser with ParseComments, walking every FuncDecl/GenDecl/ValueSpec/TypeSpec/struct field, flagging any doc comment whose leading word does not name the declaration it precedes) and mutation-verified it: reintroducing the defect makes it fire. Recommends adopting it repo-wide.
  
  Definition of done: a package-level (or repo-wide, run via go generate/go test) AST scanner, modelled on the reviewers ad hoc tool and on the existing in-tree pattern at client/guard_test.go (an AST walk, not a grep), that walks Go source across the module and asserts every doc comment is attached to the declaration it names. Wire it as a test (e.g. internal/lint or a root-level guard_test.go) so it runs under `go test ./...` and fails loudly on the next accidental doc-comment reattachment. Must be mutation-tested: verify it goes RED when a doc comment is deliberately detached from its declaration (e.g. by hoisting a const/var between a func doc and the func, as happened here).
  
  CONSOLIDATION DECISION (recorded by spec-keeper, 2026-08-08): kept SEPARATE from RELAY-9-FU-CODEGUARD (1e9b54d2-f529-4c91-a02b-116cc11bc829, AST guard for peer-error-code allow-list completeness in internal/relay). The two guards scan unrelated properties (doc-comment attachment vs enum-to-switch-case completeness), touch different packages, and would need different failure messages and different remediation. Consolidating them into one task would violate the atomic-task rule (one outcome each) for no shared implementation benefit -- they do not share scanner logic beyond both using go/ast. If a THIRD AST-guard-shaped follow-up appears, that is the point to consider a small shared `internal/astguard` helper package that each guard test imports, not a single mega-task.
  
  Not epic-scoped to RELAY -- this is general repo hygiene applicable to every package, surfaced by but not specific to RELAY-13.
  _Proof: go test -run TestNoOrphanedDocComments ./..._
### EPIC ADMIN — ADMIN: the local operator console (agent-busadm)

- [ ] ADMIN-11 · ADMIN-11: remove an agent from the console (BLOCKED on AUTH-4) — admin, P3, blocked
  BLOCKED ON `AUTH-4` (`a853261d`, P1, todo -- `POST /v1/leave`, leave / revocation). DEPENDENCIES: AUTH-4,
  ADMIN-4.
  
  Let the operator remove an agent from a bus through the console. This is the epic's first genuinely
  DESTRUCTIVE action, and the console is a read-first surface everywhere else, so it needs:
  - explicit confirmation naming the fully-qualified agent id (`<bus-id>.<agent-id>`, invariant 2) -- never a
    one-click removal, never a removal keyed on an ambiguous short name;
  - the refusal path rendered visibly if the bus declines (same principle as ADMIN-C3: an invisible refusal
    trains the operator to assume success);
  - no new bus authority: it calls the ordinary authenticated `/v1/leave` route as an authenticated agent. If
    the bus does not permit the console to remove another agent, THAT IS THE ANSWER, and the console shows it.
  
  WHY BLOCKED: `/v1/leave` does not exist yet. Its semantics -- who may revoke whom, what happens to in-flight
  sessions and cursors -- are AUTH-4's to decide, and the console must follow them rather than invent them.
  
  SEQUENCING (epic-wide, operator-decided): must not start before INVITE-GATE (05a5216d) lands -- enrolment is currently open, so this console would hold fleet-wide credentials against a bus anyone can enrol against.
  _Proof: go test -race -run 'TestAdmRemoveAgentCallsLeave|TestAdmRemoveAgentRequiresExplicitConfirmation|TestAdmRemoveAgentSurfacesRefusal' ./cmd/agent-busadm_
- [ ] ADMIN-7 · ADMIN-7: audit view in the console, for a CO-LOCATED bus only (D5) — admin, P3
  DEPENDENCIES: ADMIN-6 (the streaming reader), ADMIN-4 (the multi-bus console).
  
  Render the audit log in the console for a bus whose data directory is on THE SAME MACHINE, read through
  ADMIN-6's streaming reader. Per D5 there is NO REMOTE AUDIT READING and no route is added for it: the audit log
  is the bus's complete social graph, and reading it stays an operator/filesystem capability exactly as
  invite-minting already is.
  
  So the view must REFUSE, visibly and with a reason, for any configured bus that has no local data-dir path --
  not silently show an empty panel. A blank panel and "you are not permitted to read this remotely" are the same
  pixels and completely different facts.
  
  Show what the log holds and nothing else: message id, sequence, sender, recipient(s), traversed bus path,
  timestamp, size, content hash. NO BODIES (invariant 6). The TRAVERSED BUS PATH is also the ONLY sanctioned way
  to build a topology view -- importing `internal/relay` breaks the build
  (`TestHandshakeHandlerIsNotWiredIntoAnyMux`).
  
  Surface ADMIN-6's torn-tail signal in the UI. Silent discard is the defect; a UI that hides an incomplete tail
  reintroduces it one layer up.
  
  SEQUENCING (epic-wide, operator-decided): must not start before INVITE-GATE (05a5216d) lands -- enrolment is currently open, so this console would hold fleet-wide credentials against a bus anyone can enrol against.
  _Proof: go test -race -run 'TestAuditViewRequiresColocatedDataDir|TestAuditViewRefusesRemoteBus|TestAuditViewShowsTornTailNotice' ./cmd/agent-busadm_
- [ ] ADMIN-3 · ADMIN-3: `agent-busadm serve` -- loopback-only console with a capability token and an embedded page showing one bus's status — admin, P2
  DEPENDENCIES: ADMIN-1 (rulings recorded), ADMIN-2 (the status capability it renders).
  
  THE FIRST USABLE THING IN THIS EPIC: a new binary `cmd/agent-busadm` with a `serve` subcommand that binds
  loopback only, mints a per-process capability token, and serves ONE embedded HTML page showing a single bus's
  status via `client.Info/Health/Discovery`.
  
  Per D1 the console surface is plaintext HTTP on loopback -- a console surface, not a bus surface. That makes
  these mandatory, not optional:
  - bind 127.0.0.1 only; refuse a non-loopback bind rather than warn;
  - a per-process capability token, required on every request (in-memory, regenerated each start, never written
    to a world-readable path);
  - strict `Origin` / `Sec-Fetch-Site` checks so a page in the operator's browser cannot drive the console;
  - a 0600 unix socket for non-browser access.
  
  FIRST `go:embed` IN THE REPO -- the page ships inside the binary; no runtime asset path, no directory the
  operator must keep in sync with the binary. Note the Go version requirement in the task's report if it forces
  anything (see the Docker Compose toolchain section of CLAUDE.md).
  
  PRIVILEGE CONCENTRATION starts here: the console holds bus credentials that no single component holds today.
  Mitigations that begin in this task and continue through ADMIN-4: a distinct identity per bus, 0600 storage for
  anything credential-bearing, and a blast radius bounded by the lease (ADMIN-C2/C3).
  
  SEQUENCING (epic-wide, operator-decided): must not start before INVITE-GATE (05a5216d) lands -- enrolment is currently open, so this console would hold fleet-wide credentials against a bus anyone can enrol against.
  _Proof: go test -race -run 'TestServeBindsLoopbackOnly|TestCapabilityTokenRequired|TestCrossOriginRequestRejected|TestUnixSocketIs0600|TestEmbeddedPageRendersBusStatus' ./cmd/agent-busadm && grep -Fq 'agent-busadm serve' CONTRACTS-CLI.md_
- [ ] ADMIN-2 · ADMIN-2: client.Info/Health/Discovery + `agent-busctl status [--json]`, shipped together (invariant 7) — client, P2
  DEPENDENCIES: NONE -- this is startable independently of ADMIN-1's docs (though the epic-wide sequencing
  below still applies). `/healthz` and `/v1/info` are on the unauthenticated allow-list, so this works BEFORE
  enrolment, which is exactly why it is the console's first real capability.
  
  Add `Info`, `Health` and `Discovery` methods to the exported `client/` package (it CANNOT live under
  `internal/` -- invariant 7's third audience is an agent EMBEDDING the client), and ship the
  `agent-busctl status [--json]` subcommand in the SAME task. A capability without its subcommand and its
  `AGENT_PROTOCOL.md` / `CONTRACTS-AGENT.md` entry is not done.
  
  Requirements:
  - Human output is readable by default; `--json` is a stable documented shape; exit codes documented; never an
    interactive prompt; credentials from config/env.
  - Every request still goes out over the pinned mutual-TLS transport built by `client/transport.go` /
    `client/pin.go`. Unauthenticated ROUTE does not mean unpinned CONNECTION.
  
  THE NEGATIVE IS PART OF THE PROOF: `TestNoBusRequestBypassesPinnedTLSConfig` must fail if ANY code path in
  `client/` or `cmd/agent-busctl` can reach a bus without going through `pinnedTLSConfig` (e.g. a bare
  `http.DefaultClient`, an `http.Get`, or a hand-rolled `&http.Client{}`). Model it on the existing
  `client/guard_test.go` style. Without that negative, the positive tests pass just as happily against an
  unpinned connection.
  
  SEQUENCING (epic-wide, operator-decided): must not start before INVITE-GATE (05a5216d) lands -- enrolment is currently open, so this console would hold fleet-wide credentials against a bus anyone can enrol against.
  _Proof: go test -race -run 'TestClientInfoHealthDiscovery|TestStatusCommandJSONShape|TestNoBusRequestBypassesPinnedTLSConfig' ./client ./cmd/agent-busctl && grep -Fq 'agent-busctl status' CONTRACTS-AGENT.md_
- [ ] ADMIN-8 · ADMIN-8: GET /v1/status -- authenticated, in-process counters, exhaustive field-set pin, leaks no configuration — httpapi, P3
  DEPENDENCIES: ADMIN-2.
  
  SUPERSEDES CORE-5 (`06c5b1f5-99e4-4823-a679-2c074c2aee80`, "Observability: metrics/inspect endpoint") -- that
  task is being marked superseded by this one. Do NOT file or implement a second inspect endpoint.
  
  Add an AUTHENTICATED `GET /v1/status` returning in-process counters (uptime, message count, connected agents
  count, sequence high-water, and similar aggregates).
  
  DO NOT ADD IT TO `unauthenticatedRoutes` (internal/httpapi/authmw.go). Default-deny already handles it: the
  existing `TestEveryRouteRequiresAuth` golden-list pin will pass without any edit, and that no-edit is the
  point. Adding a route to the allow-list is meant to be a visible, justified diff -- this route needs no such
  diff, and any change to that allow-list in this task is a review failure.
  
  MUST LEAK NO CONFIGURATION -- no data-directory path, no listen address, no peer list, no roster, no invite
  state. That is the same rule `/v1/info`'s pin already enforces, and for the same reason: an authenticated agent
  is not an operator.
  
  `TestStatusFieldSetIsExhaustive` must pin the EXACT field set (fail on an unexpected field as well as on a
  missing one), so that a future field cannot be added without someone consciously re-reading the leak rule.
  
  SEQUENCING (epic-wide, operator-decided): must not start before INVITE-GATE (05a5216d) lands -- enrolment is currently open, so this console would hold fleet-wide credentials against a bus anyone can enrol against.
  _Proof: go test -race -run 'TestStatusRouteRequiresAuth|TestStatusFieldSetIsExhaustive|TestStatusLeaksNoConfiguration|TestEveryRouteRequiresAuth' ./internal/httpapi && grep -Fq 'GET /v1/status' CONTRACTS-HTTP.md_
- [ ] ADMIN-9 · ADMIN-9: the console enrols by redeeming an invite blob (BLOCKED on INVITE-GATE) — admin, P3, blocked
  BLOCKED ON `INVITE-GATE` (`05a5216d`, P0, todo). DEPENDENCIES: INVITE-GATE, ADMIN-3/ADMIN-4.
  
  The console becomes an enrolled agent the ONE way anything becomes an enrolled agent: by redeeming an
  operator-minted, single-use, expiring invite blob (invariant 3, invite-only enrolment). No side door, no
  special console enrolment path, no new authority.
  
  The invite blob is also the TRUST ANCHOR: it carries the bus's certificate fingerprint alongside the bus id,
  address and invite secret, so the console knows what to expect BEFORE its first connection -- there is no
  trust-on-first-use window to widen. Enrolment must pin from the blob and refuse if the presented certificate
  does not match.
  
  Per ADMIN-4, each bus gets its own IdentityDir, so redeeming N invites produces N distinct client certificates,
  not one fleet-wide key.
  
  WHY BLOCKED, not merely ordered: until INVITE-GATE lands, `/v1/enroll` does not require an invite at all, so
  this task cannot assert the behaviour that makes it correct. Building it first would encode the open-enrolment
  world into the console.
  
  SEQUENCING (epic-wide, operator-decided): must not start before INVITE-GATE (05a5216d) lands -- enrolment is currently open, so this console would hold fleet-wide credentials against a bus anyone can enrol against.
  _Proof: go test -race -run 'TestAdmEnrolRedeemsInviteBlob|TestAdmEnrolPinsBusFingerprintFromInvite|TestAdmEnrolRefusesWithoutInvite' ./cmd/agent-busadm ./client_
- [ ] ADMIN-C1 · ADMIN-C1: versioned control/telemetry schema in a new internal/adminctl -- unknown kinds and versions are REFUSED, not ignored — adminctl, P3
  DEPENDENCIES: ADMIN-1.
  
  A new package `internal/adminctl` defining the versioned control/telemetry message schema, version 1 (reserved:
  `admin-control-schema` namespace, value 1 -- D7). Four kinds:
  
  - `telemetry.lease.request`  -- console asks a node to stream telemetry for a bounded period
  - `telemetry.lease.granted`  -- node consents, with the interval and expiry IT chose
  - `telemetry.lease.refused`  -- node declines, WITH A REASON
  - `telemetry.sample`         -- one sample under a live lease
  
  UNKNOWN KINDS AND UNKNOWN VERSIONS ARE REFUSED, NOT IGNORED. Silently ignoring an unknown control message is
  how a control plane ends up in a state neither end believes it is in: the sender thinks it asked, the receiver
  thinks nothing happened, and no one can tell that from a lost message. Refusal is observable; silence is not.
  
  WHAT THIS IS NOT, and the proof/gates must hold this line:
  - NOT a new wire protocol version. Do not reserve `signing-format-version` or bump the protocol.
  - NOT a new on-disk record type. Do not reserve `record-type`, do not touch the WAL format.
  It is an APPLICATION PAYLOAD carried inside an ordinary authenticated `/v1/send` body -- the bus does not know
  or care what it means. That is precisely why it needs its own independent schema version.
  
  Note `/v1/broadcast` answers 501 unconditionally: every one of these is a DM.
  
  SEQUENCING (epic-wide, operator-decided): must not start before INVITE-GATE (05a5216d) lands -- enrolment is currently open, so this console would hold fleet-wide credentials against a bus anyone can enrol against.
  _Proof: go test -race -run 'TestUnknownControlKindIsRefused|TestUnknownSchemaVersionIsRefused|TestLeaseRequestGrantedRefusedRoundTrip|TestTelemetrySampleRoundTrip' ./internal/adminctl_
- [ ] ADMIN-10 · ADMIN-10: online invite mint from the console (BLOCKED -- ruled out for now by D6; filed so the reasoning is not lost) — admin, P3, blocked
  BLOCKED -- RULED OUT FOR NOW BY D6. Filed deliberately so the reasoning survives and nobody re-derives it
  from scratch, or worse, implements it without noticing the constraint.
  
  D6: `agent-bus invite mint` takes the EXCLUSIVE data-directory lock and therefore requires the bus to be
  STOPPED. A console that is by design a read-first, always-on observer cannot stop the node it is observing in
  order to mint an invite, and a mint path that did not take the dirlock would be writing to a directory another
  process owns exclusively.
  
  WHAT SHIPS INSTEAD (and what the proof pins today): the console LINKS TO THE COMMAND -- it shows the exact
  `agent-bus invite mint` invocation for the selected node and states that the bus must be stopped. The proof is
  therefore a NEGATIVE plus a positive: the console has no online mint path, and it does display the command.
  
  UNBLOCKING WOULD REQUIRE a decision recorded in DECISIONS.md about minting without the exclusive dirlock (for
  example an in-server authenticated mint route), which is a change to how invites -- the system's trust anchor --
  are created. That is a security-gate conversation, not an implementation detail.
  
  SEQUENCING (epic-wide, operator-decided): must not start before INVITE-GATE (05a5216d) lands -- enrolment is currently open, so this console would hold fleet-wide credentials against a bus anyone can enrol against.
  _Proof: go test -race -run 'TestConsoleDoesNotMintInvitesOnline|TestConsoleShowsInviteMintCommand' ./cmd/agent-busadm_
- [ ] ADMIN-C3 · ADMIN-C3: console issues/renews telemetry leases and renders the stream -- A REFUSAL MUST BE VISIBLE — admin, P3
  DEPENDENCIES: ADMIN-C2 (the node reporter), ADMIN-4 (the multi-bus console).
  
  The console side of the control plane: issue `telemetry.lease.request` to selected nodes, renew before expiry,
  and render the arriving `telemetry.sample` stream.
  
  A REFUSAL MUST BE VISIBLE. `telemetry.lease.refused` -- and its reason -- is rendered as a refusal, next to the
  node it came from. A control plane whose refusals are invisible trains the operator to assume success: "no data
  from node 7" then means indistinguishably "node 7 is quiet", "node 7 is down", and "node 7 told you no". Those
  are three different operational situations and the console must not merge them.
  
  Same rule for expiry: when a lease lapses, the panel says the lease lapsed. It never keeps rendering the last
  sample as though it were current -- stale data presented as live is the same failure wearing a nicer coat.
  
  The console renders the interval and expiry THE NODE CHOSE (ADMIN-C2), never the ones it requested.
  
  COST DISCIPLINE, verbatim from the epic: every telemetry sample is two round trips (`/v1/mint` then
  `/v1/send`), a two-phase fsynced durable write, a permanent audit record that can never be deleted, and a
  sequence number that can never be reused. Ten nodes at one sample/second is ~864,000 permanent audit records a
  day whose entire content is "fine". This bus is engineered never to lose a message, which makes it a poor
  telemetry sink. PREFER THE POLL PLANE (ADMIN-2 / ADMIN-8) for anything the console can simply ask for -- default
  the lease interval accordingly, and make the UI state the cost when the operator shortens it.
  
  SEQUENCING (epic-wide, operator-decided): must not start before INVITE-GATE (05a5216d) lands -- enrolment is currently open, so this console would hold fleet-wide credentials against a bus anyone can enrol against.
  _Proof: go test -race -run 'TestConsoleRendersRefusalVisibly|TestConsoleRenewsLeaseBeforeExpiry|TestConsoleShowsLeaseExpiredNotStaleData' ./cmd/agent-busadm_
- [ ] ADMIN-C2 · ADMIN-C2: `agent-busctl report` -- the node reporter: allow-list check, refuse-with-reason or grant a bounded lease, hard minimum interval, no survival across restart — cli, P3
  DEPENDENCIES: ADMIN-C1.
  
  The node-side reporter, as an `agent-busctl report` SUBCOMMAND (D2) -- not a third binary. It receives
  `telemetry.lease.request` messages and decides.
  
  1. ALLOW-LIST (D4): check the SENDER's fully-qualified agent id against a LOCAL allow-list of admin agent ids
     configured on this node. Not on the list => `telemetry.lease.refused` with a reason. No new bus authority,
     no new route, no role tier -- this is literally the "configured to allow it" in the request.
     **THE REFUSAL ASSERTION IS THE AUTHORISATION MODEL.** `TestReportRefusesSenderNotOnAllowList` is not a
     politeness test; it is the whole security property. If it is missing or weak, any enrolled agent on the bus
     can make any node stream telemetry to it.
  2. BOUNDED LEASE (D3): a grant is bounded and EXPIRES. It does NOT survive a restart -- on restart the node
     reverts to its configured default and streams nothing until asked again. Cost accepted: if the console is
     down, telemetry stops. That is fail-closed on a control channel, and a stolen console credential buys an
     attacker only until the lease expires.
  3. HARD MINIMUM INTERVAL REGARDLESS OF WHAT WAS ASKED. The node clamps; it never adopts a requested interval
     below its own floor. Remember the cost per sample: two round trips (`/v1/mint` then `/v1/send`), a two-phase
     fsynced durable write, a permanent audit record that can never be deleted, and a sequence number that can
     never be reused. A node that honoured "every 10ms" would be asked to do it.
  4. The grant reports the interval and expiry THE NODE CHOSE, not the ones requested, so the console renders
     truth rather than its own hopes.
  
  Telemetry samples are DMs -- `/v1/broadcast` answers 501 unconditionally.
  
  SEQUENCING (epic-wide, operator-decided): must not start before INVITE-GATE (05a5216d) lands -- enrolment is currently open, so this console would hold fleet-wide credentials against a bus anyone can enrol against.
  _Proof: go test -race -run 'TestReportRefusesSenderNotOnAllowList|TestReportEnforcesHardMinimumIntervalRegardlessOfRequest|TestLeaseDoesNotSurviveRestart|TestReportRefusalCarriesReason' ./cmd/agent-busctl ./internal/adminctl_
- [ ] ADMIN-1 · ADMIN-1: record the operator-console trust/transport/control rulings D1-D7 in DECISIONS.md, and name agent-busadm in the CONTRACTS.md index — docs, P2, blocked
  DEPENDENCIES: none. BLOCKS EVERYTHING ELSE IN THE ADMIN EPIC -- no console code lands before the rulings are written down.
  
  Write a dated DECISIONS.md section whose heading is EXACTLY (the proof pins this string, so do not paraphrase it):
  
      ## <date> -- ADMIN: the operator console is LOCAL, CONSENT-BASED and READ-FIRST (D1-D7)
  
  and inside it, seven bullets that each BEGIN `- **D1**` ... `- **D7**` (the proof counts exactly 7 such lines):
  
  - **D1** UI TRANSPORT: plaintext HTTP on loopback + a per-process capability token, strict `Origin` /
    `Sec-Fetch-Site` checks, plus a 0600 unix socket for non-browser access. Ruled EXPLICITLY, not defaulted:
    this is a console surface, not a bus surface, and TLS here would reintroduce the browser trust-store problem
    the architecture exists to eliminate. Record WHY this does not weaken invariant 11: invariant 11 governs bus
    surfaces (client and relay), and every bus connection this console makes is still pinned mutual TLS via
    `client/`.
  - **D2** the reporter is an `agent-busctl report` subcommand, not a third binary.
  - **D3** ADVISORY LEASE: bounded, expires, does NOT survive restart. Cost accepted -- if the console is down,
    telemetry stops; that is fail-closed on a control channel, and a stolen console credential buys an attacker
    only until the lease expires. Durable standing configuration was EXPLICITLY REJECTED (it would need a new
    durable record type, let a remote party permanently alter a node's behaviour, and make revocation a
    distributed problem).
  - **D4** authorisation is a LOCAL ALLOW-LIST of admin agent ids per node. No new bus authority, no new route,
    no role tier.
  - **D5** NO REMOTE AUDIT READING. Co-located filesystem read only: the audit log is the bus's complete social
    graph, and reading it stays an operator/filesystem capability exactly as invite-minting already is.
  - **D6** NO ONLINE INVITE MINT. `agent-bus invite mint` takes the exclusive dirlock and needs the bus stopped;
    the console links to the command instead.
  - **D7** the control-message schema gets its OWN reserved namespace, `admin-control-schema` (v1 reserved
    2026-08-08), so it can never be confused with `signing-format-version` or `ondisk-format-version`.
  
  Also record the epic-wide constraints: relay must not be imported (`TestHandshakeHandlerIsNotWiredIntoAnyMux`
  fails the build if any package outside `internal/relay` imports it), `/v1/broadcast` answers 501 so telemetry
  must be DM, the telemetry cost warning (two round trips + a fsynced durable write + a permanent audit record +
  a never-reusable sequence number per sample; ten nodes at 1/s is ~864k permanent records a day saying "fine" --
  this bus is engineered never to lose a message, which makes it a poor telemetry sink), and the fact that the
  console is a NEW concentration of privilege created by the request rather than by the implementation.
  
  Then add `cmd/agent-busadm` to the CONTRACTS.md INDEX (CONTRACTS.md is an index only -- name the binary and say
  which plane file will document its flags: CONTRACTS-CLI.md).
  
  PROOF DISCIPLINE: the proof pins the exact heading text and counts the seven `- **Dn**` bullets rather than
  grepping the word "admin" (an incidental match elsewhere in DECISIONS.md would green-light nothing). CONFIRM IT
  IS RED BEFORE THE FIX and quote scripts/proof-check.sh's verdict; a doc proof never observed failing is not
  evidence.
  
  SEQUENCING (epic-wide, operator-decided): must not start before INVITE-GATE (05a5216d) lands -- enrolment is currently open, so this console would hold fleet-wide credentials against a bus anyone can enrol against.
  _Proof: grep -Fq 'ADMIN: the operator console is LOCAL, CONSENT-BASED and READ-FIRST (D1-D7)' DECISIONS.md && test "$(grep -cE '^- \*\*D[1-7]\*\*' DECISIONS.md)" = 7 && grep -Fq 'cmd/agent-busadm' CONTRACTS.md_
- [ ] ADMIN-4 · ADMIN-4: N buses from a config file, polled concurrently -- one hung bus must not stall the others; one IdentityDir per bus — admin, P2
  DEPENDENCIES: ADMIN-3.
  
  Read a config file listing N buses and show them all. THE LOAD-BEARING REQUIREMENT: one hung or unreachable bus
  MUST NOT stall the others -- per-bus timeouts, per-bus goroutines, per-bus error state rendered as an error
  rather than as an absence. A console that goes blank because one node is wedged is worse than no console.
  
  ONE `IdentityDir` PER BUS. `client-tls/{cert,key}.pem` is shared per credential store, and the console wants a
  DISTINCT client certificate per bus: sharing one client cert across the fleet would make the console a single
  key whose theft is fleet-wide, and would let any one bus operator identify the console on every other bus. This
  is also the concrete form of the epic's privilege-concentration mitigation.
  
  PROOF UNDER `-race`. This is the epic's ONLY genuinely concurrent code, and per CLAUDE.md a data race here is a
  P0. The hung-bus test must use a bus stub that accepts the connection and never answers (not one that refuses
  the connection) -- refusal is the easy case and does not exercise the timeout path.
  
  DO NOT import `internal/relay` for any fan-out or topology work: `TestHandshakeHandlerIsNotWiredIntoAnyMux`
  fails the build if any package outside it imports it. The fan-out lives here, in the console.
  
  SEQUENCING (epic-wide, operator-decided): must not start before INVITE-GATE (05a5216d) lands -- enrolment is currently open, so this console would hold fleet-wide credentials against a bus anyone can enrol against.
  _Proof: go test -race -run 'TestOneHungBusDoesNotStallOthers|TestPerBusIdentityDirIsDistinct|TestConfigFileLoadsNBuses' ./cmd/agent-busadm_
- [ ] ADMIN-6 · ADMIN-6: bounded, tail-tolerant STREAMING audit reader in internal/wal (no dir lock, torn tail surfaced) — wal, P2
  DEPENDENCIES: ADMIN-1.
  
  `wal.ScanAll` (internal/wal/reader.go) loads the WHOLE file and errors on ANY malformed frame, which makes it
  unusable against a LIVE, GROWING log: the last frame is routinely a partial write, and a growing file has no
  bounded size. The streaming seam already exists inside the package but is unexported.
  
  Export a bounded streaming reader: read forward from an offset, yield records incrementally, stop at a caller
  supplied bound (record count and/or byte budget), and TOLERATE a torn tail instead of failing the whole read.
  
  TWO HARD CONSTRAINTS:
  1. It MUST NOT take the data-directory lock. `bus.lock` is EXCLUSIVE and held by the running bus; a reader that
     takes it either fails or -- worse -- would have to wait for the bus to stop. This is a read-only,
     lock-free, second-opener path.
  2. THE TORN TAIL MUST BE SURFACED, not swallowed. Invariant 6 is explicit that SILENT DISCARD IS THE DEFECT
     (rated P0), not discard itself. The reader returns a distinguishable "the tail is incomplete at offset N"
     signal that callers (ADMIN-7) render; it never quietly returns a short list that looks complete.
  
  Also required: never write to the log, never truncate, never repair. This is a reader. Any temptation to
  "fix up" the tail belongs to the recovery path, which is a different task with different gates.
  
  SEQUENCING (epic-wide, operator-decided): must not start before INVITE-GATE (05a5216d) lands -- enrolment is currently open, so this console would hold fleet-wide credentials against a bus anyone can enrol against.
  _Proof: go test -race -run 'TestStreamAuditSurfacesTornTail|TestStreamAuditTakesNoDirLock|TestStreamAuditIsBounded' ./internal/wal_
- [ ] ADMIN-5 · ADMIN-5: roster + live flow view from the console's OWN long-poll (/v1/wait) -- metadata only, never a body — admin, P2
  DEPENDENCIES: ADMIN-4.
  
  Show each bus's roster, and a live flow view driven by the console's OWN long-poll against `/v1/wait`. Render
  per message: sender, recipient(s), sequence, timestamp, size, content hash. NEVER THE BODY (invariant 6: the
  audit trail is metadata and routing info only, deliberately so that it stays compatible with end-to-end
  encrypted, forward-secret payloads).
  
  THE NEGATIVE IS THE INVARIANT-6 GUARD: `TestFlowViewRendersContentHashAndNeverBody` must assert BOTH that the
  rendered output CONTAINS the content hash AND that it does NOT contain the body bytes. Use a distinctive body
  sentinel so the negative can actually fail. A positive-only test passes just as happily on a view that leaks
  the payload.
  
  DOCUMENT THE HONEST LIMIT, in the UI and in the docs: this shows the CONSOLE AGENT'S OWN DMs, not the bus's
  whole traffic. The console is an ordinary enrolled agent; it sees what is addressed to it. Anything broader is
  the audit-log path (ADMIN-6/ADMIN-7), which is co-located-only per D5. Do not let the UI imply fleet-wide
  visibility it does not have.
  
  `/v1/broadcast` answers 501 unconditionally -- nothing in this view may wait on broadcast.
  
  SEQUENCING (epic-wide, operator-decided): must not start before INVITE-GATE (05a5216d) lands -- enrolment is currently open, so this console would hold fleet-wide credentials against a bus anyone can enrol against.
  _Proof: go test -race -run 'TestFlowViewRendersContentHashAndNeverBody|TestRosterViewRendersAgents' ./cmd/agent-busadm_

### EPIC AGENTIF — Agent-facing surface (shell wrappers + protocol doc)

- [ ] None · Wire `-backfill-suffix-floors` through scripts/bus-serve.sh and document it (invariant 7 gap) — docs, P1
  Invariant 7 (agents never hand-write HTTP / a feature ships with its scripts/bus-*.sh wrapper AND an AGENT_PROTOCOL.md entry in the same task) is currently violated for the suffix-floor backfill migration: the flag `-backfill-suffix-floors` exists at cmd/agent-bus/main.go:121, but there is NO sanctioned wrapper path to pass it -- scripts/bus-serve.sh does not accept or forward it -- and the string "backfill" appears in ZERO .md files in the repo (verified by grep). So an operator who needs to run the suffix-floor backfill migration has no documented, wrapper-mediated way to do it and must hand-construct the server invocation, which is exactly what invariant 7 forbids. Acceptance: (1) scripts/bus-serve.sh accepts a flag/env var and forwards `-backfill-suffix-floors` to the server binary; (2) AGENT_PROTOCOL.md documents when/why an operator would run the backfill and how to invoke it via the wrapper; (3) CONTRACTS.md and/or CONTRACTS-CLI.md record the flag, its default, and its effect.
- [ ] None · RUN_DIR created with no ownership check -- enables binary swap and pidfile symlink attack — security, P1
  Pre-existing, found by the security gate on parent task 10e93262-8e34-4738-b435-bfe23d880057, outside that task's scope. scripts/bus-serve.sh does mkdir -p "$RUN_DIR" (default /tmp/agent-bus) without verifying ownership or mode. An attacker who owns that directory can (a) swap $RUN_DIR/bin/agent-bus between the go build and the nohup that executes it -- code execution as the operator -- and (b) symlink $RUN_DIR/agent-bus.pid or agent-bus.log for an arbitrary truncate+write, since start does : > "$LOG_FILE". Note this is now the ONLY remaining security value in that log: after the parent task the log is no longer a trust anchor, so this is about the binary swap and the symlink, not the fingerprint. Fix should verify ownership/mode of $RUN_DIR (and refuse, not repair, a dir owned by someone else), and create the log/pidfile with O_NOFOLLOW semantics. File: scripts/bus-serve.sh.
  _Proof: bash scripts/proof-check.sh 'RUN_DIR ownership/symlink hazard test to be authored by implementer -- see task description'_

### EPIC AUTH — Enrolment & authentication

- [ ] None · AUTH-1-FU-ACTIVECAP-RETRYAFTER: a per-agent cap 503 tells the client the wrong thing and the wrong Retry-After — httpapi, P1
  NOTE: this proof_cmd is VACUOUS today by construction -- TestSessionCapRetryAfter does not exist yet; writing it is part of this task. Found by the reviewer gate on AUTH-1-FU-ACTIVECAP. internal/httpapi/auth.go:48-52 documents `capacityRetryAfterSeconds = "5"` on the premise that "every capacity limit in internal/auth is a live, in-memory bound that a departing agent or an expiring session can relieve within seconds". That premise is now FALSE. The new per-agent ACTIVE-session cap reaches the same mapping (auth.go:225 -> writeAuthError -> auth.go:289) but persists until one of the agent's OWN sessions expires -- up to SessionLifetime, one hour. A genuine agent at its own cap receives 503 {"error":"server at capacity, retry later"} with Retry-After: 5: it blames the SERVER for a client-side condition, and the retry advice is wrong by up to three orders of magnitude (5s vs 3600s). A conforming client honouring Retry-After polls ~720 times over the hour while its operator diagnoses a bus outage that is not happening. Fix the SURFACE, not the cap value 32: a distinct sentinel (e.g. ErrAgentSessionCapacity) or at minimum a distinct terse client message and a Retry-After derived from the agent's own soonest-expiring session. Note the disclosure constraint the security gate confirmed: the branch is unreachable without the agent's private key, so distinguishing it to THAT caller is not an oracle. Out of scope for AUTH-1-FU-ACTIVECAP, whose boundary was internal/auth only.
  _Proof: go test -race -run TestSessionCapRetryAfter ./internal/httpapi_
- [ ] None · AUTH-2-FU-POLLEXPIRY: A long-poll can outlive its session, quietly contradicting immediate revocation — auth, P1
  Origin: security audit of AUTH-2, 2026-08-02, found forward-looking. Authentication is evaluated ONCE at request entry (handleWait does not re-authenticate; it reads the principal authMiddleware already attached via messagingPrincipal, which checks nothing). CORRECTED 2026-08-08 (was: '-poll-timeout is validated only as > 0 with no ceiling against the 1h auth.SessionLifetime' -- that was WRONG, see kind=report note): -poll-timeout IS ceilinged, at hub.MaxPollTimeout (5 minutes), on all three paths -- hub.Wait clamps (wait.go:165-166), hub.Open clamps the operator's -poll-timeout flag (hub.go:498-499), and readTimeoutParam refuses rather than clamps an over-ceiling client request (messages.go:947-949, 400). So the pre-fix exposure was always <=5 minutes, never <=1 hour. A poll parked at entry still keeps serving after its session expires or is revoked, up to that 5-minute bound, and up to hub.MaxWaitersPerAgent (32) parked polls per agent, so the lag can cover up to 32 batches, not one. auth.Principal already carries ExpiresAt, so a handler CAN enforce it but nothing requires it. The POLL epic must cap the wait at min(PollTimeout, time.Until(principal.ExpiresAt)) and re-Authenticate before delivering. Re-polling does NOT chain past a revoke: /v1/wait is not on unauthenticatedRoutes, so the next poll is refused. P1 because 'revocation is immediate' (DECISIONS.md 2026-08-02) is otherwise false for any poll already in flight -- STAYS P1 today (no reachable revocation surface exists yet; only expiry is reachable, a bounded <=5min overrun by a principal that legitimately held the credential moments earlier) but becomes P0 the day AUTH-4 (/v1/leave) ships, since that is the day an operator starts relying on immediate revocation. ORDERING CONSTRAINT: this task MUST land before AUTH-4 (a853261d-2829-4101-906d-31a8a81eb59f) -- recorded as a `blocks` relation -- so revocation never ships without covering the parked-poll case. The same argument extends to MTLS-CROSSCHECK (2b2af075-a295-4cf3-9826-b1a3554c8795, planner note on this task, 2026-08-02): it adds a second property, the cert-to-agent binding, that a parked poll can equally outlive, so a re-check must cover both the session AND the cross-check -- however MTLS-CROSSCHECK has NOT shipped either (no reachable exposure yet), so no `blocks` relation is added there, only this note; revisit if MTLS-CROSSCHECK lands first. STATUS SPLIT (2026-08-08): the DOC-ACCURACY half of the underlying audit finding (F8/S8b, overstated 'revocation and expiry immediate' comment in internal/httpapi/authmw.go) has landed as a comment-only change (see task notes for the diff/verification) -- reviewer PASS, security PASS. The BEHAVIOUR half -- the min(pollTimeout, time.Until(ExpiresAt)) cap and re-authenticate-before-delivering in handleWait/hub.Wait -- is UNTOUCHED. This task's definition-of-done is the behaviour fix; it stays `todo` until that lands, not merely documented.
- [ ] None · AUTH-1-FU-SESSIONSCALE: session-table O(n) scans and refuse-not-evict policy cause CPU/lock amplification — auth, P1
  Follow-up to AUTH-1 (public_id 54fa94c0-6ca3-459a-aaa2-a3ea047f97d9). Security measured roughly 1.04 ms of exclusive global-mutex time per ~180-byte request at default caps with a full table (a ~960 req/s ceiling for the WHOLE auth surface), caused by the O(n) sweepLocked / countPendingLocked / oldestPendingLocked scans over a 16384-entry table, all held under sessMu. Separately, MaxSessions currently REFUSES new session-begins rather than evicting the globally-oldest PENDING session, so once the table fills, every legitimate agent is denied until entries time out -- an unauthenticated flooder can hold the table full indefinitely. Fix: replace the O(n) scans with an amortized structure (e.g. a min-heap or a periodically-swept ring keyed on expiry) and change the full-table policy to evict-oldest-pending rather than refuse. NOTE: AUTH-1 already split Service.mu into enrolMu + sessMu, so this task's fix must NOT reunify them -- keep AUTH-3's durable enrolment fsync off the Authenticate hot path.
  _Proof: go test -race -run TestSessionTableEvictsOldestPendingUnderLoad ./internal/auth_
- [ ] None · AUTH-2-FU-SESSMU: auth.Service.Authenticate now takes an exclusive mutex on every request's hot path — auth, P2
  Origin: security audit of AUTH-2, 2026-08-02. internal/auth/service.go guards the session table with a plain sync.Mutex. AUTH-2 puts Authenticate on EVERY request, while the UNAUTHENTICATED BeginSession holds the same mutex for an O(n) sweepLocked. An anonymous /v1/session/begin flood can therefore stall legitimate authenticated traffic in a way it could not before AUTH-2. Fix: give Authenticate an RWMutex read path, or amortise the sweep. Note this lives in internal/auth, which AUTH-2 deliberately did not touch.
- [ ] None · SEC: unauthenticated enrol permanently bricks the roster -- 4096-cap fails closed forever, no eviction, survives restart — auth, P0
  Found independently twice within the same hour (an external security-testing agent reading the route table, and the AUTH-3 security-gate review) -- corroboration is why this is filed as established rather than a claim.
  
  Compose three individually-reasonable facts: (1) POST /v1/enroll is unauthenticated (invite gate not yet landed -- INVITE-GATE). (2) DefaultMaxRosterEntries = 4096 (internal/auth/service.go:30) and enrolment fails CLOSED at the cap. (3) The roster is durable across restart (internal/auth roster WAL replay, AUTH-3) and there is NO route or method that removes a roster entry -- verified: no /v1/leave, no Remove/Delete/Evict/Leave on the roster. AGENT_PROTOCOL.md confirms bus logout is local-only (server_notified: false).
  
  Consequence: 4096 unauthenticated POSTs -- no key material, no invite, no session, no client cert -- and the roster is full FOREVER. Not until a TTL; not until a restart, because restart REPLAYS the durable roster and restores the attacker entries. The only remedy today is an operator deleting the whole data directory, destroying every legitimate agent id and the message history along with it.
  
  The security gate's independent wording: durable enrolment turned a TRANSIENT DoS into a PERMANENT one -- no /v1/leave, no delete on WALRoster, first-write-wins, WAL never compacts.
  
  Why this is filable rather than a restatement of "INVITE-GATE is unfinished": internal/auth/session.go (~line 244-260) carries a careful availability analysis of the session-table flood, concluding it is untargeted, unamplified and SELF-HEALING. That write-up is the canonical analysis of what an unauthenticated caller can cost the bus -- and it stops one resource short. The roster version is untargeted and unamplified too, but it is NOT self-healing, not TTL-bounded, and survives reboot. A permanent cost sitting undocumented beside a careful analysis of a transient one is worse than an undocumented hole, because a reader reasonably concludes the analysis is complete.
  
  SEVERITY: the reporter rated this P0 and explicitly declined to inflate it, noting the listener is loopback-only (127.0.0.1:18080 by default) so the reachable attacker set today is a local process. spec-keeper judgement recorded here: rated P0 anyway, on impact rather than current reachability -- the damage is irreversible (destroys the data directory to recover, which also destroys legitimate history) and invariant 11's own text anticipates a bus deliberately exposed on a real interface as a real, intended deployment target, not a hypothetical; a permanent-DoS primitive should not wait for that exposure to be reprioritised. Track reachability separately: if it becomes reachable from a non-loopback interface before this and INVITE-GATE both ship, that is an immediate re-escalation trigger, not a new finding.
  
  This is a TRACKING/UMBRELLA task for the finding. Three concrete follow-ups are filed separately: extending session.go's availability analysis (doc), an operator-side roster-reclamation escape hatch independent of INVITE-GATE, and a priority note on INVITE-GATE (already P0) plus AUTH-4 (leave/revocation, P1) as the in-protocol remedy once auth exists.
  _Proof: grep -n roster internal/auth/session.go | grep -qi bricked || grep -n roster CONTRACTS-HTTP.md | grep -qi permanent_
- [ ] None · AUTH-1-FU-ACTIVECAP-DOCS: document the per-agent ACTIVE-session cap in CONTRACTS-HTTP.md + AGENT_PROTOCOL.md — docs, P1
  The code shipped in AUTH-1-FU-ACTIVECAP; the docs could not, because CONTRACTS*.md and AGENT_PROTOCOL.md were owned by concurrent agents during that loop. This is tracked debt, deliberately incurred. The proof globs CONTRACTS*.md so it survives the CONTRACTS split. Four edits, all in CONTRACTS-HTTP.md unless stated:
  (a) NEW row in the admission-control caps table (~line 122-124, columns Cap | Default | Behaviour at the cap):
  | `MaxActiveSessionsPerAgent` | 32 | **Fails closed**: `POST /v1/session/complete` returns 503 (`ErrCapacity`, `Retry-After: 5`) when the transition from pending to active would take the agent past its cap of concurrently ACTIVE sessions. Enforced in `CompleteSession`, after the Ed25519 signature verifies and after the already-active early return, so re-completing an already-active session (invariant 10's legitimate retry) is never refused. Never evicts, and a refusal mutates nothing -- the pending challenge survives and can complete once one of the agent's OWN sessions expires (up to `SessionLifetime`, 1 hour, away). This cap is keyed on agent id and is safe to key that way *only here*: unlike `BeginSession`'s `agent_id`, which is an attacker-supplied victim identifier, the key on this route is a PROVEN identity, not an attacker-supplied victim identifier -- an entry only enters an agent's bucket behind a valid Ed25519 signature made with that agent's own enrolment private key, so a flooder can only fill its own bucket. See `MaxSessions` for the residual risk this narrows but does not close. |
  (b) FIX the `MaxSessions` row (line 124). Its tail is now FALSE -- it still says "nothing caps active sessions per agent" and that the gap "is filed as AUTH-1-FU-ACTIVECAP". Replace those sentences with: MaxActiveSessionsPerAgent now bounds how much of the hour-long outage a SINGLE enrolment can hold; at the 16384/32 defaults filling the table with active entries takes ceil(16384/32) = 512 DISTINCT enrolments rather than one agent. Be honest per the security audit: that is only +1.6% attacker cost (33280 vs 32769 requests) and the sustained hold is UNCHANGED at ~9.1 req/s, because Enrol accepts duplicate public keys and names so the 512 enrolments come from ONE keypair. 512 is 12.5% of MaxRosterEntries, so the roster bound is NOT binding. The cap bounds blast radius per identity; it does not make the table unfillable. Root fix is the invite-only enrolment EPIC (0b43393e-556b-409a-938a-846be2fb4a75); partial mitigation AUTH-1-FU-RATELIMIT (42670f8b).
  (c) NEW route-table row (match the format of lines 21-28):
  | `POST` | `/v1/session/complete` | none | 503 | the completing agent already holds `MaxActiveSessionsPerAgent` (default 32) ACTIVE sessions; `Retry-After: 5`. Never evicts -- the refusal leaves the pending challenge and every session the agent already holds untouched |
  (d) AMEND the "There is deliberately no per-agent pending-challenge cap" paragraph (~line 126) so it reads as a deliberate ASYMMETRY, not a blanket ban -- a future reader must not cite it to delete the active cap. Append: "**This is a statement about the PENDING side only.** `MaxActiveSessionsPerAgent` (AUTH-1-FU-ACTIVECAP) IS a per-agent cap, on the ACTIVE side of `CompleteSession`, and it is safe precisely because that key is a proven identity rather than an attacker-supplied one -- do not cite this paragraph to justify removing it."
  (e) AGENT_PROTOCOL.md (~line 110, after the 401 paragraph): a `POST /v1/session/complete` can now return 503 where before it could not. Correct client behaviour: honour `Retry-After`, do NOT re-enrol and do NOT treat it as an auth failure. Retry the SAME pending challenge only while it is still within `ChallengeTTL` (2 minutes); after that the challenge is gone and a fresh `POST /v1/session/begin` is required. A cap of 32 genuinely exhausted usually means the agent is leaking sessions rather than refreshing at `refresh_after_seconds`.
  (f) DECISIONS.md entry (dated 2026-08-02), text supplied by feature-runner: "Per-agent ACTIVE-session cap: refuse-new, never evict, default 32. An agent-id-keyed bucket is a lockout primitive on the unauthenticated BeginSession route (AUTH-1-FU-PENDINGCAP removed one) but is SAFE on CompleteSession, because an entry only enters a bucket behind a valid Ed25519 signature with that agent's enrolment private key -- a proven identity, so a flooder can only fill its own bucket and a refusal is self-inflicted. Refuse over evict, deliberately: evicting an agent's own oldest session would let a thief who compromised its key destroy the legitimate holder's LIVE sessions on demand. 32 = ~16x the compliant steady state of 2 concurrent sessions (a client refreshes at 75% of lifetime, so old and new overlap), bounding one identity to 0.2% of the session table."
  _Proof: grep -q 'a PROVEN identity, not an attacker-supplied victim identifier' CONTRACTS*.md && grep -q 'MaxActiveSessionsPerAgent' CONTRACTS*.md && grep -q '503' AGENT_PROTOCOL.md && echo DOCS_OK_
- [ ] AUTH-5 · AUTH-5: Auth crash/recovery test — auth, P1
  End-to-end crash-injection test: enrol an agent, simulate a crash before/after the commit fsync at each stage, restart, and assert the token is valid iff the enrolment was durably committed; separately, revoke an agent, crash, restart, and assert the token stays rejected.
  _Proof: go test -race -run TestAuthCrashRecovery ./internal/auth_
- [ ] None · AUTH-1-FU-RATELIMIT: per-source rate limiting on the three unauthenticated auth routes — auth, P1
  Follow-up to AUTH-1 (public_id 54fa94c0-6ca3-459a-aaa2-a3ea047f97d9). POST /v1/enroll, POST /v1/session/begin and POST /v1/session/complete are unauthenticated by design, but there is currently NO per-source (per-IP or similar) rate limit on any of them -- every admission-control cap that exists today is GLOBAL. Consequence: an anonymous caller can deny enrolment bus-wide by exhausting MaxRosterEntries (4096) with enrol requests, and can deny session establishment bus-wide by exhausting MaxSessions (16384) with begins. Security measured roughly 137 req/s as enough to sustain the session-table denial from a single source. Add per-source rate limiting (token bucket or similar, stdlib-first per invariant 8) in front of these three routes so a single source cannot exhaust a bus-wide cap alone.
  _Proof: go test -race -run TestSessionBeginRateLimit ./internal/httpapi_
- [ ] None · MTLS-VERIFY-FU-DOCSCHEME (README/PROTOCOL half): main still documents the bus as plaintext HTTP after MTLS-LISTENER shipped TLS-only — docs, P1
  MTLS-LISTENER is COMMITTED at HEAD (invariant 11: TLS is the required transport, no plaintext listener; cmd/agent-bus/tlslisten_test.go is tracked and passing). But at committed HEAD, README.md:113-114 still reads agent-busctl --bus http://127.0.0.1:8080 enrol ... with no --bus-fingerprint, and PROTOCOL.md:195 still states as fact The listener is still plaintext HTTP. Both are false today, not merely about to become false -- confirmed by git show HEAD:README.md / git show HEAD:PROTOCOL.md. Any agent enrolling by following mains own docs today is told to dial a scheme the bus no longer serves.
  
  AGENT_LOG.md already has an entry for this fix (2026-08-07 -- MTLS-VERIFY-FU-DOCSCHEME: README.md:113-114, AGENT_PROTOCOL.md:266/252/174, PROTOCOL.md:195), and the working tree currently HAS that fix applied (confirmed: grepping for the stale strings in README.md, AGENT_PROTOCOL.md and PROTOCOL.md all return nothing in the working tree) -- but it has never been committed (git status shows README.md, PROTOCOL.md and AGENT_LOG.md all modified/uncommitted), so main still ships the stale docs.
  
  ADDITIONALLY, that same AGENT_LOG.md entry explicitly deferred two more instances rather than fixing them, and they are STILL stale in the current working tree too (checked directly, not from the log entrys say-so): README.md:87-91 (until mutual TLS lands (invariant 11), enrolment and session material would cross the wire in cleartext -- mTLS has already landed) and README.md:95-99 (a curl quickstart example with no scheme, in a What works today section). Both were explicitly left for the owning task / CONTRACTS-HTTP.md pass to avoid colliding with the listener agents own doc updates -- that pass never happened.
  
  SCOPE: finish and commit the fix. (1) Get the already-drafted README.md/AGENT_PROTOCOL.md/PROTOCOL.md/AGENT_LOG.md changes committed (they are sitting in the working tree now). (2) Additionally fix the two deferred README.md passages (87-91, 95-99) to reflect that TLS is now mandatory and self-signed/mTLS-pinned (invariant 11), not upcoming. (3) AGENT_LOG.md lands in the SAME commit as the doc changes it describes, per the shared-append-only-file convention.
  
  PRIORITY P1: main currently documents the wrong transport as fact to any agent reading it, which actively misleads enrolment.
  _Proof: bash scripts/proof-check.sh '! grep -n bus.http:// README.md AGENT_PROTOCOL.md && ! grep -n listener.is.still.plaintext PROTOCOL.md && ! grep -n until.mutual.TLS.lands README.md'_
- [ ] None · AUTH-1-FU-POPKEY: enrolment does not prove possession of the enrolling private key — auth, P1
  Follow-up to AUTH-1 (public_id 54fa94c0-6ca3-459a-aaa2-a3ea047f97d9). Enrolment binds a caller-supplied public key to a fresh server-minted agent id, but never checks that the caller holds the matching private key -- so anyone can bind ANY public key, including someone else's already-published one, to a new identity. Security recommends requiring a signature over (name || public_key || idempotency_key) at enrolment time, verified with the submitted public key, before the binding is accepted. IMPORTANT: this CHANGES THE ENROL WIRE SHAPE (adds a signature field to the request), so it must be coordinated with the Go CLI / AGENTIF work that also touches the enrol payload -- do not land it unilaterally. The invariant that already holds and must be preserved: once an id is bound to a key, it can never later present a different key (this task only adds proof-of-possession at the initial binding, it does not change post-enrolment behaviour).
  _Proof: go test -race -run TestEnrollRequiresProofOfPossession ./internal/auth_
- [ ] AUTH-4 · AUTH-4: POST /v1/leave -- leave / revocation — auth, P1
  Lets an enrolled agent durably remove itself from the roster; its token is rejected by the auth middleware on every call afterward, including after a restart (the revocation itself goes through the two-phase write path).
  
  ACCEPTANCE CRITERION ADDED (spec-keeper, 2026-08-02, from ID-3 reviewer F2 + security LOW finding): internal/ids/agentmint.go point 8 delegates bounding distinct-name growth to admission control, but AUTH-1 (now done) carried no such obligation in its description. Today growth is contained only because the roster never shrinks (no leave existed yet) and admission caps roster.Len(). Once this task lets leave shrink the roster while suffix counters must NOT be reclaimed (ids are never reused), an enrol/leave loop over distinct 64-byte names can grow suffix-counter memory without bound. This task must explicitly state, and test, how it bounds suffix-counter growth under a repeated enrol/leave loop (e.g. a cap on distinct names ever seen, eviction policy, or an explicit accepted-and-documented unbounded-but-slow-growth argument) -- do not ship leave without addressing this.
  _Proof: go test -race -run TestLeaveRevocation ./internal/auth_
- [ ] None · Enrol accepts a duplicate enrolment public key -- one keypair can hold unlimited agent ids — auth, P2
  Found by the security gate on AUTH-1-FU-ACTIVECAP, verified empirically (three enrolments with a byte-identical public key were all accepted, minting alpha-1/-2/-3, after which ONE private key held 3x the per-agent active-session cap). Service.Enrol validates the public key's LENGTH but never checks whether that key is already enrolled against another agent id, and the Roster interface offers no by-key lookup to do so. Two consequences. First, it is the direct reason AUTH-1-FU-ACTIVECAP raises the flood cost by only ~1.6%: the "512 distinct enrolments" the cap forces are 512 unauthenticated POSTs from ONE keypair, not 512 identities an attacker must obtain. Second, it makes key->identity one-to-many where several planned features assume one-to-one: the invite-only + self-signed-mTLS design (invariant 11) binds a client-certificate fingerprint to an agent id at enrolment, AUTH-4 revocation is naturally expressed per key, and a roster listing two agents with identical keys is a spoofing surface. The right answer is NOT obviously "reject" -- refusing a duplicate leaks "this key is already enrolled" to an unauthenticated caller, which is its own oracle. Needs a recorded DECISIONS.md decision (reject / allow-and-document / defer to the invite gate, which would moot it) rather than a silent code change.
- [ ] None · AUTH-ROSTER-RECLAIM: operator-side "agent-bus roster remove <id>" escape hatch -- filesystem authority, not an HTTP route, works even when the roster is already full — auth, P0
  Follow-up 2 of 3 from SEC roster-brick finding (1c4d3dea-b4f6-4f68-b823-78bb76a6b5aa). Today the only remedy for a full roster (whether from an attack or legitimate churn) is an operator deleting the WHOLE data directory, destroying every agent id and the message history. This task adds an operator-facing subcommand on the server binary itself (same precedent as the existing `agent-bus invite mint` -- filesystem/process authority, not a network route) that durably removes one or more roster entries via the two-phase write path, so recovery after a restart reflects the removal. This is DELIBERATELY NOT an HTTP route and does NOT depend on INVITE-GATE, MTLS, or any authenticated in-protocol path -- it exists precisely for the case where the roster is already saturated and no in-protocol path can free space. Judge and record whether this needs a new WAL record type (reserve via POST .../reservations, namespace matching this repo's on-disk record-type convention -- see CONTRACTS-ONDISK.md) or can reuse an existing leave/revoke record type once AUTH-4 lands. Cross-reference AUTH-4 (a853261d, POST /v1/leave, P1, in-protocol/authenticated) -- that is a DIFFERENT mechanism (an agent revoking itself, or an authenticated admin action over HTTP) from this one (operator, out-of-band, works when the bus cannot otherwise be reasoned with). Must be documented in CONTRACTS-CLI.md and AGENT_LOG.md notes the id is never reissued (invariant 1) -- removal frees the roster SLOT, not the id/suffix.
  _Proof: go build ./... && ./agent-bus roster --help 2>&1 | grep -qi remove_
- [ ] None · AUTH-3-FU-ROSTERDOS-DOCS: extend session.go availability analysis (untargeted/unamplified/self-healing) to cover the roster, which is NOT self-healing — auth, P1
  Follow-up 1 of 3 from SEC roster-brick finding (1c4d3dea-b4f6-4f68-b823-78bb76a6b5aa). internal/auth/session.go (~line 244-260) carries a careful availability writeup for the session-table flood, ending with: roughly maxSessions/ChallengeTTL sustained requests per second to hold an UNTARGETED, unamplified, SELF-HEALING outage. Add an adjacent paragraph (or a clearly linked comment on DefaultMaxRosterEntries in service.go) stating the roster case explicitly: enrolling to DefaultMaxRosterEntries (4096) is also untargeted and unamplified, but UNLIKE the session table it is NOT self-healing (no expiry drains a roster entry) and NOT bounded by restart (WAL replay restores it). State the current remedy (operator deletes the whole data directory, destroying legitimate history) and cross-reference the operator-reclamation follow-up and INVITE-GATE. Docs-only / comment-only change -- no route or behaviour change.
  _Proof: grep -n -i "not self-healing\|self-healing" internal/auth/session.go internal/auth/service.go | grep -qi roster_
- [~] AUTH-3 · AUTH-3: Roster persistence & recovery — auth, P0, in progress
  The agent roster (id, name, public key/verifier material, enrolled-at) is rebuilt on startup by WAL replay, not held only in memory -- an agent enrolled before a restart is still authenticated and listed after one, with no re-enrolment required.
  
  CORRECTION (spec-keeper, 2026-08-02, from ID-3 security+reviewer gate findings): the resume floor for name suffixes must NOT be derived from the committed roster alone -- internal/ids/agentmint.go point 3 explicitly forbids that derivation. It must be reconstructed from ALL prepares ever written -- committed, aborted, AND dangling -- covering agents still enrolled and agents that have since departed, or a new agent minted with a different keypair can silently inherit a previous agent's id/suffix. This task must land BEFORE any enrolment record reaches the WAL (once an agent id is on disk, id-reuse-on-restart escalates from MEDIUM to CRITICAL). Cross-reference ID-2-WIRING-OBSERVER (c31f6999-da4e-400d-ab55-178b82e2a42e), the task that exposes dangling prepares needed to compute this floor correctly.
  _Proof: go test -race -run TestRosterRecovery ./internal/auth_
- [ ] None · MSG-FU-ROSTERSOURCE: the hub must read the AUTHORITATIVE roster the moment AUTH-3 makes enrolment durable — core, P1
  internal/hub keeps its OWN roster view, fed by internal/httpapi/auth.go calling hub.NoteEnrolment on every accepted enrolment. That is honest TODAY only because the two views have identical lifetimes: auth.MemoryRoster is in memory only and lost on restart, and so is the hub's. auth.Roster exposes Put/Get/Len and NO listing, and internal/auth was outside the MSG/POLL wave's ownership, which is why it was done this way. THE DAY AUTH-3 LANDS THIS BECOMES A LANDMINE: auth's roster survives a restart while the hub's starts empty, so sessions authenticate fine but hub.publish returns 403 (ErrUnknownSender) for every send, 404 for every recipient, and both read paths fail closed with ErrUnknownSender -- a bus that authenticates everyone and serves nobody. FIX: add List (or an iterator) to auth.Roster and auth.MemoryRoster, inject it into hub.Options, and delete NoteEnrolment together with its call site in handleEnroll. CRITICAL DETAIL: the enrolment epoch (store.Message.VisibleTo) reads Agent.EnrolledAt, so the durable roster MUST carry each agent's ORIGINAL enrolment instant. With it, a genuinely continuous agent keeps seeing everything sent since it enrolled, which is exactly the behaviour the epoch was designed to preserve. MUST land in the same change as AUTH-3, never after it.
  _Proof: go test -race -run TestHubReadsTheDurableRoster ./internal/hub_

### EPIC CLI — Human CLI interface to the bus

- [ ] None · post-200 validation failures on send say may or may not have been applied — cli, P3
  client/messages.go: the four validation failures AFTER a 200 was received are routed through writeFailed and so are told this send may or may not have been applied, when in fact a 200 WAS received and the bus did accept it. Errs safe but is inaccurate; consider a distinct wording for the post-acceptance validation path.
- [ ] CLI-6 · CLI-6: log -- read the append-only audit log (metadata only; also absorbs the WAL-dumper idea from the dissolved DUR-4-FU-TOOLING) — cli, P2
  MERGED EPIC 2026-08-02. The CLI and AGENTIF epics are now ONE epic (user decision, DECISIONS.md
  2026-08-02: "Merge the CLI and AGENTIF epics" / "the go cli should take the place of the .sh files and
  be easy to use for a human + friendly for an agent to use or embed"). Invariant 7 is AMENDED, not
  weakened: nobody hand-writes HTTP, but the vehicle is a CLI subcommand, not a scripts/bus-*.sh
  wrapper. A feature without its CLI subcommand is still not done.
  
  THREE AUDIENCES, and every subcommand serves all three: a HUMAN (readable output, remedial errors); an
  AGENT SHELLING OUT (--json, stable exit codes, NO interactive prompts, NO TTY-dependent credential
  input); an AGENT EMBEDDING (the reusable client package, which therefore CANNOT live under internal/).
  
  Read the audit log -- which under invariant 6 is **METADATA AND ROUTING INFO ONLY** (id, sequence,
  sender, recipients, bus path, timestamp, size, content hash), NEVER bodies. That is a deliberate
  2026-08-02 decision so the audit trail stays compatible with end-to-end encrypted, forward-secret
  payloads. Support filtering by sender, recipient, time range and sequence range, and --follow to tail
  it. **The CLI must not imply message bodies are retrievable from the log; make their absence EXPLICIT
  in the output and in --help**, so an operator is never misled into thinking a body was lost. The proof
  greps --help for that statement precisely because it is the kind of thing that quietly goes missing.
  
  ABSORBED FROM THE DISSOLVED DUR-4-FU-TOOLING (superseded 2026-08-02 by the always-restart decision):
  a read-only frame-level view of the WAL -- offset, record index, record type, length, MAC-ok, one line
  per frame -- so an operator can see what is on disk without writing a throwaway Go program. It is now
  an ORDINARY diagnostic rather than an emergency tool, because the bus always restarts. Ship it here or
  under CLI-8 doctor, but ship it somewhere.
  
  DEPENDS ON: DUR-5 (the audit log itself), CLI-1. PROOF fails today by construction.
  _Proof: go test -race -run 'TestCLILog' ./client/... ./cmd/agent-bus-cli/... && go run ./cmd/agent-bus-cli log --help 2>&1 | grep -qi 'metadata only'_
- [~] None · client: 404 on a route the client depends on reports as version skew, not exit-7 message rejection — client, P1, in progress
  Field-observed (2026-08-07) by a real agent connecting a HEAD-built agent-busctl to an older running bus (~8h uptime, predating the mint work). The old bus has no /v1/mint route. The client gets 404 and surfaces it as exit 7 (rejected) on every send, while enrol/whoami/agents/watch all work normally -- so the skew is invisible until the first send.
  
  Why this matters: exit 7 in CONTRACTS-CLI.md means "the bus understood the request and refused it" (400/404/409/413/415/422 grouped together per client/transport.go statusError). A missing route is not that -- the bus never understood the request because it does not know the route exists. Telling the agent its MESSAGE was rejected when its BUS is too old invites exactly the wrong responses: retry, abandon the message, or report a delivery failure to the operator -- all wrong when the real remedy is "upgrade the bus". CLAUDE.md requires errors that name the remedy rather than the stack; "rejected" names neither the cause nor the fix.
  
  Scope: client/**, cmd/agent-busctl/**. For a 404 on a route the client is known to depend on for the operation attempted (starting with /v1/mint, generalize if other route-dependent 404s exist), detect and report it as a version-skew condition: name the missing route and the likely remedy (upgrade the bus, or use a bus at/after the commit that added the route), with a distinct exit code or at minimum a distinct message/Kind from the generic KindRejected/exit-7 path used for genuine 400/409/413/415/422 refusals. Add the corresponding AGENT_PROTOCOL.md entry so an agent hitting this can recognise it, and a CONTRACTS-CLI.md row documenting the new behaviour/exit code.
  
  Proof must show the OLD behaviour is currently reachable (a 404 on a route-dependent call surfaces as exit 7 today) before showing the fix distinguishes it.
  _Proof: go test -race -run TestVersionSkew ./client/... && grep -qi "version skew\|route.*missing\|upgrade the bus" AGENT_PROTOCOL.md_
- [ ] None · cmd/agent-busctl/cli_test.go valid-exit-code map omits client.ExitVersionSkew(9) -- latent trap for the 52930611 doc landing — cli, P1, 52930611-followup, exit-codes, latent-trap
  cmd/agent-busctl/cli_test.go:287-297 (TestHelpExitCodeTablesAgreeWithClientExitCodes) hardcodes a CLOSED `valid` map of exit codes it will accept in any subcommand help-text exit-code table. It lists ExitOK/Error/Usage/Config/Auth/Network/Server/Rejected/Empty but NOT client.ExitVersionSkew (9), added by 52930611 (client/errors.go). It is GREEN TODAY ONLY because no subcommand help text documents exit 9 yet -- confirmed by running it. The moment 52930611 (or any change) adds a `9`/`version skew` row to ANY subcommand's --help text (cmd/agent-busctl/{send,watch,agents,whoami,root}.go all embed their own exit-code tables), this test will fail with `... documents exit code 9 ..., which is not one of the client.Exit* constants` -- in a package (cmd/agent-busctl/) outside client/**, so the doc task's author will appear to have broken an unrelated test in a package they were not touching. Both the reviewer and security gates on 52930611 flagged this independently (security gate F2, 2026-08-08T14:58: "exit 9 appears in ZERO of CONTRACTS-CLI.md/AGENT_PROTOCOL.md/README.md/PROTOCOL.md, and cmd/agent-busctl/cli_test.go:287-297 is a CLOSED valid-code set omitting client.ExitVersionSkew"). Fix: add `client.ExitVersionSkew: "client.ExitVersionSkew"` to the `valid` map (and add a `canonical` phrase entry, e.g. matching "version skew"/"predates" -> client.ExitVersionSkew, mirroring the existing entries) in the SAME change that lands 52930611's CONTRACTS-CLI.md/AGENT_PROTOCOL.md rows -- whoever completes 52930611 must extend this map, not a separate agent working cmd/agent-busctl/ later. Filed as its own task (rather than folded into 52930611) because the file is cmd/agent-busctl/, outside client/**, which is why it was not fixed alongside the client/** code.
  _Proof: grep -n "client.ExitVersionSkew" cmd/agent-busctl/cli_test.go && go test -race -run TestHelpExitCodeTablesAgreeWithClientExitCodes ./cmd/agent-busctl/_
- [ ] CLI-9 · CLI-9: shell completion + man/usage polish — cli, P3
  MERGED EPIC 2026-08-02. The CLI and AGENTIF epics are now ONE epic (user decision, DECISIONS.md
  2026-08-02: "Merge the CLI and AGENTIF epics" / "the go cli should take the place of the .sh files and
  be easy to use for a human + friendly for an agent to use or embed"). Invariant 7 is AMENDED, not
  weakened: nobody hand-writes HTTP, but the vehicle is a CLI subcommand, not a scripts/bus-*.sh
  wrapper. A feature without its CLI subcommand is still not done.
  
  THREE AUDIENCES, and every subcommand serves all three: a HUMAN (readable output, remedial errors); an
  AGENT SHELLING OUT (--json, stable exit codes, NO interactive prompts, NO TTY-dependent credential
  input); an AGENT EMBEDDING (the reusable client package, which therefore CANNOT live under internal/).
  
  Bash/zsh completion for subcommands, flags, and where cheaply possible enrolled agent ids. Usage text
  good enough that --help answers the common questions without opening a doc.
  
  Post-2026-08-02 additions: --help must state the AT-LEAST-ONCE delivery guarantee (duplicates are the
  normal steady state) and the exit-code table, because an agent author reads --help, not PROTOCOL.md.
  Completion must not require a TTY or an interactive prompt to generate.
  
  DEPENDS ON: CLI-1..CLI-8. PROOF fails today by construction.
  _Proof: go test -race -run 'TestCLIHelp' ./cmd/agent-bus-cli/... && go run ./cmd/agent-bus-cli completion bash | grep -q 'complete '_
- [ ] None · CLI-BUSCTL-IMAGE: Ship the busctl binary in the container image — deploy, P2
  CLI-1/CLI-2 delivered cmd/busctl as a go-buildable binary, but go build ./cmd/busctl is the delivery -- it is NOT yet part of the Dockerfile/compose story (DEPLOY-1/DEPLOY-2), so nobody running agent-bus via docker compose has busctl available inside or alongside the server container. Decide and implement the packaging: (a) a separate stage in the existing multi-stage Dockerfile that builds cmd/busctl alongside cmd/agent-bus and copies the resulting binary into the runtime image (or a sibling image/tag) so an operator can docker compose exec into the server container and run busctl directly, or (b) a standalone busctl image/tag built from the same Dockerfile via a build target, for an agent/operator to pull without the full server. Server image must NOT accidentally grow the client attack surface it does not need (keep cmd/agent-bus the default ENTRYPOINT; busctl is opt-in). Update DECISIONS.md with the chosen shape and CONTRACTS-CLI.md / README.md with how to invoke it in the compose setup. Depends on DEPLOY-1 (Dockerfile) and CLI-1-FU-BINARYNAME (final installed name) landing first so the packaging does not have to be redone.
  _Proof: docker compose build && docker compose run --rm busctl --help 2>&1 | grep -q enrol_
- [ ] CLI-10 · CLI-10: Rewrite AGENT_PROTOCOL.md against CLI subcommands (it currently documents shell wrappers that are being retired) — docs, P1
  FILED 2026-08-02. The decision that retired the shell wrappers says in terms: "AGENT_PROTOCOL.md must be
  rewritten against CLI subcommands rather than shell scripts." No task carried that, so it is filed
  here rather than smuggled into a CLI subcommand task.
  
  WHY IT NEEDS ITS OWN TASK: AGENT_PROTOCOL.md is THE agent-facing document -- it is what an agent reads
  to learn how to use the bus. Leaving it describing `scripts/bus-*.sh` while the binary grows
  subcommands means every new agent is onboarded onto a retired interface. And spreading the rewrite
  across CLI-2..CLI-8 guarantees it ends up written eight different ways.
  
  SCOPE.
   - Rewrite every capability entry against a CLI subcommand: enrol/whoami/use/logout, watch, send,
     broadcast, agents, log, peers, doctor. Keep the shape agents rely on -- one section per capability,
     copy-pasteable invocation, exact output shape.
   - Document the AGENT-FACING contract explicitly, because agents are now a first-class consumer:
     `--json`, the EXIT-CODE TABLE, NO interactive prompts, NO TTY-dependent credential input, and the
     fact that the long-poll subcommand streams NDJSON (one object per line, flushed as it arrives).
   - **STATE AT-LEAST-ONCE DELIVERY.** Required by the decision by name ("Must be stated in PROTOCOL.md
     and AGENT_PROTOCOL.md"). Duplicates are the NORMAL steady state; the agent's handler must be
     idempotent; freshness comes from the server-minted monotonic sequence plus the recipient cursor,
     not from a signature.
   - State that the client generates its idempotency key ONCE and reuses it across retries, and what
     happens if a key is reused with a DIFFERENT payload (protocol violation -> disconnect).
   - State the session model an agent will actually hit: sessions last <=1h, refresh is automatic at 75%
     of lifetime, sessions DO NOT survive a bus restart (the CLI re-authenticates), and /leave revokes
     IMMEDIATELY.
   - Mention the embedding path -- the importable client package -- for agents that would rather link
     than shell out.
   - KEEP `scripts/bus-serve.sh`: it is an operator/server-lifecycle tool, not an agent protocol call,
     it is the only surviving wrapper, and it is load-bearing in several proof_cmds. Say so, so nobody
     deletes it during the wrapper cull.
  
  SEQUENCING: written incrementally as CLI subcommands land; the final sweep after CLI-8. Do not write
  entries for subcommands that do not exist -- an AGENT_PROTOCOL.md documenting vapour is worse than one
  documenting a wrapper.
  
  PROOF. `! grep -q 'scripts/bus-{enrol,send,wait,agents,broadcast,leave,peer}.sh' AGENT_PROTOCOL.md && grep -q 'at-least-once' AGENT_PROTOCOL.md`
  -- the negative clause proves the retired wrappers are GONE from the doc (the actual deliverable) and
  the positive clause pins the one statement the decision mandates by name. Deliberately does NOT
  mention bus-serve.sh, which is allowed to remain.
  _Proof: ! grep -q 'scripts/bus-enrol.sh\|scripts/bus-send.sh\|scripts/bus-wait.sh\|scripts/bus-agents.sh\|scripts/bus-broadcast.sh\|scripts/bus-leave.sh\|scripts/bus-peer.sh' AGENT_PROTOCOL.md && grep -q 'at-least-once' AGENT_PROTOCOL.md_
- [ ] CLI-8 · CLI-8: doctor -- diagnose a broken setup with a specific remedy per failure — cli, P2
  MERGED EPIC 2026-08-02. The CLI and AGENTIF epics are now ONE epic (user decision, DECISIONS.md
  2026-08-02: "Merge the CLI and AGENTIF epics" / "the go cli should take the place of the .sh files and
  be easy to use for a human + friendly for an agent to use or embed"). Invariant 7 is AMENDED, not
  weakened: nobody hand-writes HTTP, but the vehicle is a CLI subcommand, not a scripts/bus-*.sh
  wrapper. A feature without its CLI subcommand is still not done.
  
  THREE AUDIENCES, and every subcommand serves all three: a HUMAN (readable output, remedial errors); an
  AGENT SHELLING OUT (--json, stable exit codes, NO interactive prompts, NO TTY-dependent credential
  input); an AGENT EMBEDDING (the reusable client package, which therefore CANNOT live under internal/).
  
  One command that checks the common failure modes end to end: server reachable, /healthz green,
  identity present and non-expired, session token accepted, CLOCK SKEW within tolerance (server expiry
  is authoritative and the client refreshes at 75% of lifetime -- skew is a real, diagnosable failure),
  data-dir writable, audit log readable. **Prints a SPECIFIC REMEDY per failure rather than a generic
  error.** This is the command that stops a human from hand-writing curl to work out what is wrong --
  which is the whole point of invariant 7.
  
  Add, post-2026-08-02: a check for the "sessions do not survive restart" case, so an agent whose token
  stopped working after a bus restart gets told that rather than "unauthorized"; and a role check
  (endpoint vs relay -- never both).
  
  PROOF: the second clause asserts doctor EXITS NON-ZERO against an unreachable bus. A diagnostic that
  exits 0 when everything is broken is worse than no diagnostic, and that is the regression a shape-only
  test would miss. Fails today by construction.
  
  DEPENDS ON: CLI-1, CLI-2.
  _Proof: go test -race -run 'TestCLIDoctor' ./client/... ./cmd/agent-bus-cli/... && go run ./cmd/agent-bus-cli doctor --bus http://127.0.0.1:1 --json; test $? -ne 0_
- [ ] DISCOVERY-DOC-FU-CLI · DISCOVERY-DOC-FU-CLI: `agent-busctl` subcommand that fetches and renders the bus discovery document (+ AGENT_PROTOCOL.md / CONTRACTS-CLI.md entries) — cli, P1
  Invariant 7's delivery half of DISCOVERY-DOC. The server now serves a machine-readable discovery document; an agent must be able to read it through the compiled Go CLI rather than hand-writing HTTP. Add the subcommand over the importable client/ package, plus the AGENT_PROTOCOL.md entry and the CONTRACTS-CLI.md row, in the same task. Blocked on the cmd/busctl -> cmd/agent-busctl rename settling. Depends on DISCOVERY-DOC (server side).
- [ ] AGENTIF-9 · CLI-VALIDATE: envelope/schema validation in the CLIENT before a message is handed to the caller (was AGENTIF-9, was a bash+jq check) — agentif, P1
  RE-SCOPED 2026-08-02 FROM A SHELL-WRAPPER CHECK TO A CLIENT-PACKAGE CHECK. The user's original
  instruction stands verbatim -- "add a mechanism to validate messages in the agent script before
  accepting them" -- but the "agent script" is now the Go CLI and its reusable client package
  (DECISIONS.md 2026-08-02: the Go CLI replaces the .sh files; invariant 7 amended). Moved to the CLI
  epic.
  
  WHY THIS GETS *EASIER* AND MUST NOT BE DROPPED: the original framing worried about `bash` + `jq`
  trusting server JSON blindly and feeding it into `eval`/interpolation. A typed Go client removes the
  shell-injection half of that outright -- but NOT the half that actually matters: a malformed,
  truncated, or unexpectedly-shaped response from a MISBEHAVING OR RELAY-HOPPED BUS must be rejected
  before it reaches the calling agent. Under invariant 2 a message may have crossed a bus you do not
  directly trust, and under the 2026-08-02 relay decision relay auth is bi-directional precisely because
  an intermediate bus is not automatically trusted.
  
  REQUIRED: strict decoding (reject unknown/missing fields rather than zero-valuing them), bounds on
  every length, validation that the fully-qualified `<bus-id>.<agent-id>` parses and that the claimed
  sender is well-formed, and a typed error the caller can branch on. FAIL CLOSED: on a validation
  failure return an error and NOTHING usable, never a partially-populated message. Applies on every
  inbound path -- watch/long-poll, message history, roster listing, peer exchange.
  
  LAYERING, unchanged: CRYPTO-10 covers the CRYPTOGRAPHIC verification layer (signature/MAC/decrypt),
  wired in once the CRYPTO epic lands. THIS task is the layer underneath it and INDEPENDENT of it --
  needed from day one, and still needed after CRYPTO-10 exists, because a signature over a malformed
  envelope is still a malformed envelope.
  
  PROOF. `go test -race -run 'TestClientRejectsMalformedEnvelope' ./client/...` -- table-driven over
  truncated JSON, wrong types, missing required fields, oversized fields, and an unparseable qualified
  id; each case must yield an error AND no partially-populated result. FAILS TODAY by construction (the
  client package does not exist). The OLD proof_cmd was prose, not a command
  ("bash scripts/bus-wait.sh (against a throwaway server) fed a malformed/truncated response -- exits
  non-zero, prints nothing usable to stdout"), so it could not have been run by proof-check.sh at all.
  
  --- ORIGINAL DESCRIPTION ---
  Origin: user instruction 2026-08-02, "add a mechanism to validate messages in the agent script before accepting them" -- split into two layers. CRYPTO-10 covers the CRYPTO-verification layer (MAC/decrypt, wired in once the CRYPTO epic lands). THIS task covers the layer underneath that and independent of it: basic envelope/schema validation of what a shell wrapper accepts from the server BEFORE it hands the payload to the calling agent, needed from day one (AGENTIF-3/4/5/6/7/8 all parse server JSON today with no such check specified).
  
  A shell wrapper (bash + jq/curl) that trusts server JSON blindly is fragile and, on a compromised/misbehaving/relay-hopped bus (invariant 2: multiple buses relay to each other -- a message may have crossed a bus you don't directly trust), a foot-gun: a malformed or unexpected-shaped response fed straight into `msg=$(...)`, `eval`, or interpolated into a follow-up curl call can corrupt state or worse. Scope, for every scripts/bus-*.sh wrapper that consumes a server response (bus-agents.sh, bus-broadcast.sh, bus-send.sh, bus-wait.sh, bus-leave.sh, bus-peer.sh):
  - Validate the response is well-formed JSON before doing anything else with it (a wrapper must not treat a non-2xx or non-JSON body as if it were data).
  - Validate the expected top-level shape/required fields are present and are the expected JSON type (e.g. `id` is a string, `messages` is an array) before extracting and printing/using any field -- reject with a clear non-zero exit and a stderr message on anything else, printing nothing usable to stdout on failure (same "fail loud, fail closed" contract CRYPTO-10 uses for the crypto layer, so the two layers compose instead of conflicting).
  - Cap/guard against absurd sizes (a pathological huge response should not be slurped unbounded into a bash variable).
  - Document the validation contract (accepted shape, exit codes) in AGENT_PROTOCOL.md per invariant 7 -- ships in the same task as the wrapper behaviour it documents.
  
  Does NOT cover cryptographic verification, decryption, or replay/sender-identity checks -- that is CRYPTO-10, layered on top of this once it lands. This task is not gated on the CRYPTO epic and should land first since every wrapper needs it regardless of whether E2E crypto is ever enabled.
  _Proof: go test -race -run 'TestClientRejectsMalformedEnvelope' ./client/..._
- [ ] None · CLI-2-FU-LEAVE: Add /v1/leave and make busctl logout actually revoke — cli, P1
  Today busctl logout only deletes the LOCAL credential: the enrolment stays on the roster and any live session lives out its hour (up to 1h of continued access after a user believes they have logged out). client.LogoutResult.ServerNotified exists and is hardcoded false precisely so a consumer cannot mistake local deletion for revocation; it should flip to true once the bus is genuinely told. CLI-2's originally written scope said logout "calls /v1/leave ... revocation is IMMEDIATE" -- that half of the promise is carried by THIS task: (1) add a /v1/leave (or equivalent) server route that revokes the agent's active session(s) and marks the roster entry left/revoked, following invariant 3 (auth) and invariant 10 (idempotency) exactly as every other mutating route does; (2) wire busctl logout to call it and set ServerNotified=true on success, with a documented fallback (still delete locally, but say so) when the server is unreachable. Depends on AUTH-4 (session/roster machinery). Update CONTRACTS-HTTP.md, CONTRACTS-CLI.md and AGENT_PROTOCOL.md.
- [ ] None · CLI-2-FU-TLSSEAM: The client transport is built before the identity is resolved — cli, P2
  client.New calls newHTTPClient(cfg) before any credential is read, but invariant 11's pinning needs a PER-IDENTITY client certificate and a PER-BUS fingerprint, neither of which is a function of Config alone -- both only become known once an identity/credential has been loaded from the store (or minted at enrol time). The seam is in the right PLACE (newHTTPClient is genuinely the single transport constructor, confirmed by CLI-1/CLI-2 review) but the wrong TIME. Fix: build the *http.Client LAZILY on first authenticated use (or key a small cache by (agentID, busURL) so multiple identities against different buses do not share a transport/connection pool inappropriately). Also, when this lands: delete the loopback-only-plaintext exception in client.parseBusURL (added in CLI-1/CLI-2 specifically because /v1/session/begin returns a bearer token in the response body over what would otherwise be unencrypted HTTP) once the mTLS listener ships and plaintext has no remaining justification. Blocks the mTLS epic. Recorded in DECISIONS.md 2026-08-02 addendum.
  _Proof: go test -race -run TestTransportSeam ./client/..._
- [ ] CLI-7 · CLI-7: peers -- relay topology and health (replaces bus-peer.sh) — cli, P2
  MERGED EPIC 2026-08-02. The CLI and AGENTIF epics are now ONE epic (user decision, DECISIONS.md
  2026-08-02: "Merge the CLI and AGENTIF epics" / "the go cli should take the place of the .sh files and
  be easy to use for a human + friendly for an agent to use or embed"). Invariant 7 is AMENDED, not
  weakened: nobody hand-writes HTTP, but the vehicle is a CLI subcommand, not a scripts/bus-*.sh
  wrapper. A feature without its CLI subcommand is still not done.
  
  THREE AUDIENCES, and every subcommand serves all three: a HUMAN (readable output, remedial errors); an
  AGENT SHELLING OUT (--json, stable exit codes, NO interactive prompts, NO TTY-dependent credential
  input); an AGENT EMBEDDING (the reusable client package, which therefore CANNOT live under internal/).
  
  REPLACES AGENTIF-8 (`scripts/bus-peer.sh`), superseded. Covers add/list/remove a peer bus as well as
  health.
  
  Show configured peer buses, their reachability, last successful exchange, and pending relay backlog --
  the operator's answer to "is federation actually working?".
  
  TWO DECIDED CONSTRAINTS THIS COMMAND MUST REFLECT (2026-08-02): **relay auth is BI-DIRECTIONAL and
  uses the SAME scheme as clients**; and **a node is EITHER a client endpoint OR a relay, NEVER both** --
  that exclusivity is a routing and trust simplification and is to be ENFORCED, not merely documented.
  So this command must surface a node's role, and must make a misconfigured both-roles node visibly
  wrong rather than silently working.
  
  DEPENDS ON: RELAY epic, CLI-1, CLI-2. PROOF fails today by construction.
  _Proof: go test -race -run 'TestCLIPeers' ./client/... ./cmd/agent-bus-cli/..._

### EPIC COMMS — COMMS: measuring inter-agent communication quality on the bus

- [ ] COMMS-CORPUS · Extract a real inter-agent message corpus (mechanical, not asserted) — comms, P1
  Mechanically extract the actual corpus of inter-agent messages exchanged over agent-bus into a
  committed, versioned NDJSON file (one JSON object per message: message_id, sequence, sender,
  recipient(s), timestamp, text, bus_path). Extraction must be MECHANICAL -- generated by a script
  against the real audit log / message store, never hand-transcribed -- so it can be re-run and
  re-verified.
  
  This corrects two of the epic's own founding claims, which the planner re-measured and found
  UNSUPPORTED. Record both in the corpus README, because they are why the epic starts here rather
  than with hand-authored examples:
    - "Some messages near the 64 KiB ceiling" is FALSE. Max observed inbound is 11,928 B = 18.2% of
      MaxBodyBytes. Do not plan chunking or compaction on this epic's account.
    - "Both sides converged on a headline verdict in the first line" is FALSE -- true for only
      1 of 53 messages. Section headers appear in 25/53 and are uneven by sender (sec-tester-1
      14/20, mic-array-1 11/21, speckeeper-1 0/9). "Convergence" describes two of three
      counterparties some of the time, not a general property.
    - Denominator, stated exactly and not rounded away: 60 NDJSON lines in the raw pull -> 54
      distinct message_id values -> 53 with non-empty text. That is 6 duplicate deliveries, i.e.
      at-least-once delivery is visibly live at roughly 10% in this sample. Sequence order in the
      file is NOT monotonic -- do not assume file order is delivery/send order; sort explicitly.
  
  Definition of done:
    1. A script (e.g. scripts/comms-corpus-extract.sh) that pulls every real message the extracting
       agent can read from the bus/audit trail and writes deduplicated NDJSON to a tracked path
       (e.g. docs/comms/corpus.ndjson), plus a short docs/comms/CORPUS.md recording the counts above
       (raw lines, distinct message_id, non-empty-text count, duplicate count, note on non-monotonic
       order) as MEASURED by the script, not typed by hand.
    2. Provenance: every corpus row is tagged with which agent it came from and is NOT authored or
       edited by the extracting/orchestrating agent -- this corpus is raw material for COMMS-METRICS,
       COMMS-TYPES, COMMS-STRUCT and COMMS-THREAD-TRIAL, all of which depend on it.
    3. No hand-labelling happens in this task -- that is deliberately deferred to the tasks that
       consume the corpus (see COMMS-CONSENT and the labelling-agent constraint recorded on the
       epic).
  
  Parallel-safety: safe to run standalone; touches only new files under docs/comms/ plus the new
  script. Needs read access to the live bus audit trail / message log -- run against a real or
  snapshotted bus data dir, never synthesized.
  
  Depends on: nothing (first task in the epic; several later tasks depend on it).
  _Proof: bash scripts/comms-corpus-extract.sh --verify_
- [ ] COMMS-READER · Build a corpus reader tool for message-exchange review — comms, P2
  A small CLI/script that reads docs/comms/corpus.ndjson (from COMMS-CORPUS) and renders individual
  message exchanges (sender, recipient(s), timestamp, text, and any thread/reply linkage inferrable
  from convention) in a human-scannable form, for whoever does the hand-labelling work in
  COMMS-METRICS / COMMS-STRUCT / COMMS-TYPES. Read-only against the corpus; makes no bus calls itself.
  
  Definition of done:
    1. scripts/comms-reader.sh (or a small tool under docs/comms/tools/) that takes the corpus NDJSON
       and prints one exchange at a time (or filters by sender/date/message_id), legible enough for a
       human labeller to work from without re-deriving JSON by eye.
    2. Documented usage in docs/comms/CORPUS.md.
  
  Parallel-safety: standalone once COMMS-CORPUS's output file exists; no live bus needed.
  
  Depends on: COMMS-CORPUS (needs the corpus.ndjson file to read).
  _Proof: bash scripts/comms-reader.sh --self-test_
- [ ] COMMS-CONSENT · Ask consent from the non-orchestrator agents whose messages are in the corpus — comms, P1
  Before any hand-labelling of corpus messages or any live trial that studies a named autonomous
  agent (COMMS-METRICS scoring, COMMS-STRUCT trial, COMMS-TYPES classification, COMMS-THREAD-TRIAL,
  COMMS-RETRACT), ask each non-orchestrator agent whose messages appear in the COMMS-CORPUS extract
  (observed senders include at least sec-tester-1, mic-array-1, speckeeper-1) for consent to have
  their messages used and labelled in this study. This is a "you cannot study autonomous third
  parties before asking them" requirement, not a formality -- the planner argued for FILING IT AT P1
  even though it does not strictly meet this epic's own stated P1 rule (perishability / contract-
  breakage), purely on the grounds that asking first outranks the scheduling rule. That inconsistency
  is recorded here deliberately rather than silently resolved.
  
  Consent must be requested over the bus itself (the honest channel, not a side-channel), with a
  stated deadline. THIS TASK IS DELIBERATELY COMPLETABLE WITHOUT A RESPONSE: an unanswered request by
  the stated deadline is a valid, non-vacuous outcome -- "we asked, nobody in the corpus answered by
  <date>" is real information (it bounds what COMMS-METRICS/STRUCT/TYPES/THREAD-TRIAL/RETRACT may
  ethically do next: proceed only with agents that consented, or fall back to fully anonymised /
  aggregate-only measurement for the rest).
  
  Definition of done:
    1. A consent-request message sent over the live bus to each candidate agent, naming the study,
       what data of theirs would be used, and a response deadline.
    2. A durable record (docs/comms/CONSENT.md or equivalent) of who was asked, when, and the outcome
       per agent: GRANTED, DECLINED, or `NO-RESPONSE-BY <date>` once the deadline passes with no
       reply.
    3. Downstream tasks (COMMS-METRICS, COMMS-STRUCT, COMMS-TYPES, COMMS-THREAD-TRIAL, COMMS-RETRACT)
       must read this record before touching any message from a given sender, and must respect a
       DECLINED or unresolved NO-RESPONSE the same way (exclude, or anonymise-and-aggregate only).
  
  Parallel-safety: requires a LIVE bus and real remote agents able to receive and (optionally) answer
  a DM -- this is NOT a standalone/offline task. Coordinate before running concurrently with anything
  else that messages the same agents, to avoid confusing an unrelated DM with the consent ask.
  
  Depends on: nothing structurally, but MUST run (or reach its NO-RESPONSE-BY terminal state) before
  COMMS-METRICS, COMMS-STRUCT, COMMS-TYPES, COMMS-THREAD-TRIAL and COMMS-RETRACT proceed with any
  per-agent labelling or trial.
  _Proof: bash scripts/comms-consent-check.sh docs/comms/CONSENT.md_
- [ ] COMMS-THREAD-FIELD · Add a wire-level thread/reply field -- ONLY if COMMS-THREAD-TRIAL shows convention is insufficient — comms, P3, blocked
  Contingent task: add an explicit `thread_id` / `in_reply_to` field to the wire protocol (a new
  on-disk/wire field needs a reserved wire-protocol-version bump per the epic's numbered-resource
  rule -- reserve via POST .../reservations, namespace e.g. `relay-wire-version` or
  `signing-format-version` as appropriate, never chosen by hand). This task is FILED BLOCKED because
  it must not start until COMMS-THREAD-TRIAL's trial produces a `WIRE_FIELD_NEEDED` recommendation --
  adding a wire field pre-emptively is exactly the kind of un-measured assertion this epic exists to
  avoid.
  
  Definition of done (once unblocked):
    1. Confirm docs/comms/THREAD-TRIAL.md recommends WIRE_FIELD_NEEDED.
    2. Reserve the wire-protocol-version bump, design the field (signing-format impact must be
       assessed -- this MAY require resolving SIGN-3, unlike COMMS-MULTI, since it changes the
       canonical layout rather than reusing existing plurality).
    3. Implement, sign, verify, document (CONTRACTS-ONDISK.md, CONTRACTS-HTTP.md, AGENT_PROTOCOL.md),
       and ship the CLI surface in the same task per invariant 7.
  
  Parallel-safety: not applicable while blocked. Once unblocked, touches signing/wire format -- high
  coordination needs with SIGN/RELAY work in flight.
  
  Depends on: COMMS-THREAD-TRIAL (hard blocker -- filed with status=blocked).
  _Proof: grep -q 'WIRE_FIELD_NEEDED' docs/comms/THREAD-TRIAL.md && go test -race -run TestThreadWireField ./internal/signing_
- [ ] COMMS-THREAD-TRIAL · Trial threading via convention (no wire field) and measure whether it's enough — comms, P2
  Run a real trial exchange between two or more consenting agents (per COMMS-CONSENT) using an
  EXISTING convention -- e.g. a subject-line/prefix or in-body reference to a prior message_id --
  to represent a threaded discussion, with NO new wire-level field. Measure whether participants (and
  a later reader) can correctly reconstruct the thread structure from the convention alone. This is
  one of the three genuinely uncertain questions the planner flagged as worth spending real
  measurement budget on (the other two: does heavy structure pay [COMMS-STRUCT], does retraction need
  marking [COMMS-RETRACT]) -- deliberately NOT decided by assertion.
  
  Definition of done:
    1. A pre-registered convention (committed before the trial starts) for representing a reply/thread
       without a wire field.
    2. A real trial exchange over the live bus with consenting agents, recorded into the corpus.
    3. docs/comms/THREAD-TRIAL.md: the reconstruction-accuracy result (e.g. N of M replies correctly
       linked by an independent reader), and an explicit recommendation -- convention is sufficient,
       or a wire field is needed -- with the observation that would retire the recommendation.
  
  Parallel-safety: requires a LIVE bus and real, consenting remote agents -- not standalone/offline.
  
  Depends on: COMMS-CONSENT. Blocks COMMS-THREAD-FIELD and COMMS-DOC.
  _Proof: test -f docs/comms/THREAD-TRIAL.md && grep -qE 'RECOMMENDATION: (CONVENTION_SUFFICIENT|WIRE_FIELD_NEEDED)' docs/comms/THREAD-TRIAL.md && echo TRIAL_OK_
- [ ] COMMS-STRUCT · Measure whether heavy message structure pays off -- pre-registered, mechanically ordered — comms, P2
  The third of the three genuinely uncertain questions. Measure whether heavily-structured messages
  (headers, bullet lists, explicit verdict lines) produce measurably better outcomes (faster
  convergence, fewer follow-up clarifications, higher labelled quality per COMMS-METRICS) than
  lightly-structured ones, via a pre-registered trial with consenting agents.
  
  THIS TASK CARRIES THE EPIC'S CENTRAL HONESTY GUARD and it must survive transcription intact: the
  hypothesis and scoring rubric MUST be committed to git BEFORE the first trial message is sent, and
  the proof_cmd asserts this MECHANICALLY -- by comparing the git commit timestamp of the
  pre-registration file against the earliest timestamp recorded in the trial corpus -- not by a human
  eyeballing dates. A pre-registration written or back-dated after the fact defeats the entire point
  of pre-registration, so this check cannot be satisfied by prose assertion.
  
  Definition of done:
    1. docs/comms/STRUCT-PREREG.md committed FIRST, containing the hypothesis, the structure/no-
       structure trial design, and the scoring rubric (reusing COMMS-METRICS where possible).
    2. A real trial over the live bus with consenting agents, appended to docs/comms/struct-
       trial.ndjson, each row timestamped.
    3. Hand-scoring done by an agent that is NOT claude-code-agent-bus-1 (same constraint as
       COMMS-METRICS -- the orchestrator is a subject in every measured exchange).
    4. docs/comms/STRUCT.md with the result and an explicit recommendation plus its retiring
       observation.
  
  Parallel-safety: needs a live bus and consenting agents for the trial phase.
  
  Depends on: COMMS-CORPUS, COMMS-CONSENT. Blocks COMMS-DOC.
  _Proof: PREREG_TS=$(git log --format=%aI -1 -- docs/comms/STRUCT-PREREG.md) && FIRST_MSG_TS=$(jq -r '.timestamp' docs/comms/struct-trial.ndjson | sort | head -1) && [ -n "$PREREG_TS" ] && [ -n "$FIRST_MSG_TS" ] && [ "$(date -d "$PREREG_TS" +%s)" -lt "$(date -d "$FIRST_MSG_TS" +%s)" ] && echo PREREG_BEFORE_TRIAL_OK_
- [ ] COMMS-MULTI-DESIGN · Design: widen /v1/send to true multi-recipient (Finding A) without touching SIGN-3 — comms, P2
  Finding A (planner, 2026-08-08): the `/v1/broadcast` 501 does NOT block multi-party discussion.
  Everything below the HTTP handler is already plural: internal/signing/canonical.go:65
  (MaxRecipients = 4096), the canonical layout already encodes a sorted, deduplicated recipient list
  (canonical.go:236-292), internal/store/message.go:227-243 (VisibleTo iterates a recipient set),
  :245-255 (the audit record carries `recipients` plural per invariant 6), internal/hub/hub.go:1665-
  1676 (the fingerprint is already length-prefixed and plural). The narrowing to a single recipient
  happens at exactly TWO struct fields: internal/httpapi/messages.go:196 and internal/hub/hub.go:1152.
  
  So multi-recipient send is SIGNABLE TODAY: it needs no new signing-format version and does NOT
  require resolving SIGN-3 first. It is also a better fit than broadcast for genuine multi-party
  discussion -- the audience is explicit and signature-covered, rather than implicit and unbounded.
  
  This task is DESIGN ONLY -- output a short design note (docs/comms/MULTI-SEND-DESIGN.md) describing
  exactly how the two struct fields widen to a recipient list, how the existing plural machinery is
  reused end to end (HTTP request/response shape, CLI flag(s), audit record, hub fan-out), and what
  changes (if any) are needed in CONTRACTS-HTTP.md / AGENT_PROTOCOL.md / the CLI. Record the decision
  in DECISIONS.md, referencing invariant 7 (the CLI must ship a subcommand in the same task as any new
  capability) so COMMS-MULTI's implementation task inherits that requirement explicitly.
  
  Definition of done:
    1. docs/comms/MULTI-SEND-DESIGN.md covering the two narrow fields, the request/response shape
       change, CLI surface, and explicit confirmation that no signing-format version bump or SIGN-3
       resolution is required.
    2. A DECISIONS.md entry recording the choice and citing the exact file:line locations above.
  
  Parallel-safety: standalone, read-only investigation of the existing codebase; writes no production
  code. No live bus needed.
  
  Depends on: nothing. Blocks COMMS-MULTI.
  _Proof: test -f docs/comms/MULTI-SEND-DESIGN.md && grep -q 'SIGN-3' docs/comms/MULTI-SEND-DESIGN.md && echo DESIGN_OK_
- [ ] COMMS-TOKENS · Fix the kind=model cost channel -- make token counts real (independently justified, survives the epic) — comms, P1
  Finding B (full audit of ~2700 Spec Server notes, 2026-08-08): of 1020 kind=model notes, ZERO carry
  a usable token count -- 474 are bare (no numbers at all), 323 explicitly say unavailable, 223 are
  literal zeros, and 11 are nonzero but do not conform to the `model=<id>; tokens_in=<N>;
  tokens_out=<N>; tokens_total=<N>` format CLAUDE.md specifies. Additionally 225 author/task groups
  carry duplicate kind=model notes (387 excess notes) against the documented one-per-agent-per-task
  convention. CLAUDE.md designates kind=model "the auditable cost signal" for every task in this
  project; right now that signal is unmet on every single sample.
  
  THIS TASK IS JUSTIFIED INDEPENDENTLY OF THE COMMS EPIC and should survive even if COMMS as a whole
  is dropped or descoped -- it is filed here because the audit that surfaced it was done as part of
  this epic's corpus work, not because its value depends on the rest of COMMS.
  
  Definition of done:
    1. A checker script (e.g. scripts/comms-tokens-audit.sh) that scans a project's kind=model notes
       via the Spec Server notes API and reports, per the four categories above (bare / unavailable /
       zero / non-conforming-nonzero), plus the duplicate-group count -- reproducing Finding B's
       numbers against the live project as a baseline.
    2. A documented, adopted convention (recorded via a dated DECISIONS.md-equivalent entry, or a
       CLAUDE.md amendment if the user approves) for how an agent that CAN read its own token meter
       posts a real, non-zero count, and how one that CANNOT is required to post the explicit
       "unavailable" form rather than a bare or zero note -- zero is not a safe default; it reads as
       a real zero-cost run, which is worse than an honest "unavailable".
    3. This task's OWN kind=model note must not be zero or bare -- if the completing agent cannot
       read its own meter, it must use the explicit-unavailable form, not a placeholder, since posting
       zeros here would be the exact defect this task exists to fix.
  
  Parallel-safety: safe standalone; touches only a new script plus documentation. No live-bus /
  remote-agent dependency -- it reads the Spec Server, not the message bus.
  
  Depends on: nothing.
  _Proof: bash scripts/comms-tokens-audit.sh --project agent-bus_
- [ ] COMMS-DOC · Write up the COMMS epic findings and recommendations — comms, P2
  Synthesize the epic's measurement outputs (COMMS-METRICS, COMMS-STRUCT, COMMS-TYPES,
  COMMS-THREAD-TRIAL, COMMS-RETRACT, COMMS-TOKENS, and COMMS-MULTI if landed) into a single
  docs/comms/COMMS_FINDINGS.md: what a well-formed inter-agent message looks like on this bus, which
  recommendations are backed by a measurement and which are decided-not-measured (per the epic's own
  adopted convention), and -- for every recommendation -- the observation that would retire it, per
  the epic description's own standing requirement. Update AGENT_PROTOCOL.md / CONTRACTS-HTTP.md if
  any wire-level change landed (COMMS-MULTI, and COMMS-THREAD-FIELD if it unblocked and shipped).
  
  Definition of done:
    1. docs/comms/COMMS_FINDINGS.md covering: corpus corrections, Finding A/B outcomes, each measured
       question's result (structure, threading, retraction), the token-cost-channel fix status, and
       an explicit "retiring observation" per recommendation.
    2. Doc updates to any CONTRACTS-*.md / AGENT_PROTOCOL.md entries affected by COMMS-MULTI or
       COMMS-THREAD-FIELD, if either shipped.
    3. The rejected A/B-real-backlog-tasks proposal recorded here as a still-open question flagged to
       the user (not resolved by this doc), per the epic notes.
  
  Parallel-safety: standalone synthesis/writing task; depends on the substance of the tasks below
  existing, not on any live-bus access itself.
  
  Depends on: COMMS-METRICS, COMMS-STRUCT, COMMS-TYPES, COMMS-THREAD-TRIAL, COMMS-RETRACT,
  COMMS-TOKENS, COMMS-MULTI.
  _Proof: test -f docs/comms/COMMS_FINDINGS.md && grep -qi 'retiring observation' docs/comms/COMMS_FINDINGS.md && echo FINDINGS_OK_
- [ ] COMMS-RETRACT · Determine whether message retraction needs explicit protocol marking — comms, P2
  One of the three genuinely uncertain questions worth real measurement (see COMMS-THREAD-TRIAL for
  the other framing). Determine, via corpus measurement (does any observed exchange show an implicit
  retraction/correction pattern already, e.g. a natural-language "ignore my last / correction:") plus
  a small live trial with consenting agents, whether the bus needs an explicit retraction/correction
  marker or whether natural-language follow-up already carries the signal reliably enough.
  
  Definition of done:
    1. Corpus scan for implicit retraction/correction patterns (from COMMS-CORPUS's extract),
       reported with counts and examples (redacted per consent status).
    2. A small live trial (consenting agents only) exercising an explicit correction scenario.
    3. docs/comms/RETRACT.md with an explicit recommendation -- NO_MARKER_NEEDED or MARKER_NEEDED --
       and the observation that would retire it.
  
  Parallel-safety: the live-trial portion needs a live bus and consenting agents; the corpus-scan
  portion is offline once COMMS-CORPUS exists.
  
  Depends on: COMMS-CORPUS, COMMS-CONSENT. Blocks COMMS-DOC.
  _Proof: test -f docs/comms/RETRACT.md && grep -qE 'RECOMMENDATION: (NO_MARKER_NEEDED|MARKER_NEEDED)' docs/comms/RETRACT.md && echo RETRACT_OK_
- [ ] COMMS-MULTI · Implement true multi-recipient /v1/send per COMMS-MULTI-DESIGN — comms, P2
  Implement the design from COMMS-MULTI-DESIGN: widen internal/httpapi/messages.go:196 and
  internal/hub/hub.go:1152 from a single recipient to a recipient list, reusing the already-plural
  canonical signing layout, store visibility, audit record and hub fingerprint machinery (Finding A).
  Per invariant 7, ship the CLI subcommand support and the AGENT_PROTOCOL.md / CONTRACTS-HTTP.md
  updates in THIS SAME TASK -- a capability without its CLI surface is not done.
  
  Definition of done:
    1. `/v1/send` (NOT `/v1/broadcast`, which stays 501 and out of scope) accepts multiple recipients,
       signed and verified against the existing canonical format with no version bump.
    2. CLI support (whatever the CLI subcommand for send is) accepts multiple `--to` values or
       equivalent.
    3. CONTRACTS-HTTP.md and AGENT_PROTOCOL.md updated to document the new multi-recipient shape.
    4. A race-safe test proving two+ recipients each receive the message exactly once and the audit
       record lists all recipients (invariant 6).
  
  Parallel-safety: touches internal/httpapi, internal/hub, the CLI, and docs -- coordinate if another
  task is mid-edit on the same files (check RELAY/SIGN work in flight first, since hub.go is shared
  with federation).
  
  Depends on: COMMS-MULTI-DESIGN.
  _Proof: go test -race -run TestMultiRecipientSend ./internal/httpapi ./internal/hub_
- [ ] COMMS-METRICS · Define measurable message-quality metrics against the corpus, honestly denominated — comms, P2
  Produce docs/comms/METRICS.md defining the metrics this epic will use to judge message quality
  (e.g. verdict-class clarity, section-header usage rate, time-to-verdict, convergence-on-first-line
  rate, structure-use rate) against the COMMS-CORPUS extract. Each metric must state its exact
  denominator (per the corpus corrections recorded on this epic -- 53 texted messages, not 60 or 54,
  unless a metric is explicitly scoped otherwise) and its formula.
  
  CRITICAL REQUIREMENT, non-negotiable: at least one metric MUST be marked NOT COMPUTABLE against the
  current corpus, with the specific reason (missing field, insufficient sample, requires data this
  bus does not record e.g. per invariant 6's metadata-only audit log). A metrics document in which
  everything is conveniently computable has not been honest about what the corpus actually contains
  -- this mirrors the epic's own founding-claim corrections (COMMS-CORPUS) and must not repeat the
  mistake it exists to fix.
  
  Practices to ADOPT BY DECISION rather than measure (per the planner's recommendation -- record each
  via a dated DECISIONS.md entry with a stated falsifier, not by running an experiment to "prove" it):
  reporting negatives (a metric that finds nothing is reported, not omitted), honest denominators
  (state N explicitly, every time), verdict-class precision (define PASS/FAIL/CHANGES/etc. exactly,
  do not eyeball), provenance marking (every corpus row keeps its source), and naming the confound
  whenever a metric's result could be explained by something other than the thing being measured.
  
  Hand-labelling for this task must be done by an agent that is NOT claude-code-agent-bus-1 (the
  orchestrator is a subject in every measured exchange, so it cannot also be the measurer), and the
  labelling key/rubric must be committed to git BEFORE the scorer script is written -- this is one of
  the epic's named measuring-the-instrument threats.
  
  Definition of done:
    1. docs/comms/METRICS.md: each metric with formula, denominator, and COMPUTABLE / NOT COMPUTABLE
       status (>=1 NOT COMPUTABLE, with reason).
    2. A committed labelling key/rubric predating any scorer code (git log timestamps must show the
       rubric commit before the scorer commit).
    3. The five decide-vs-measure practices above adopted as dated DECISIONS.md entries with
       falsifiers.
  
  Parallel-safety: needs COMMS-CORPUS's output and COMMS-CONSENT's resolved per-agent record before
  labelling any specific agent's messages. Otherwise standalone (no live bus calls).
  
  Depends on: COMMS-CORPUS, COMMS-CONSENT.
  _Proof: test -f docs/comms/METRICS.md && [ "$(grep -c 'NOT COMPUTABLE' docs/comms/METRICS.md)" -ge 1 ] && echo METRICS_OK_
- [ ] COMMS-TYPES · Define a message verdict-class / type taxonomy from measured corpus usage — comms, P2
  Define a precise message-type / verdict-class taxonomy (e.g. PASS / FAIL / CHANGES / QUESTION /
  INFO / STATUS) derived from how the corpus actually uses these terms today, not from an idealized
  list. Per the decide-vs-measure recommendation on verdict-class precision (see COMMS-METRICS),
  precision here is a DECIDED practice: define each class exactly, with an explicit rule for
  ambiguous cases, then measure how many corpus messages classify unambiguously versus how many are
  genuinely ambiguous (report the ambiguous count -- do not silently force-fit them).
  
  Definition of done:
    1. docs/comms/TYPES.md: the taxonomy with exact per-class definitions and the disambiguation rule.
    2. A classification pass over the COMMS-CORPUS extract (consent-respecting), reporting counts per
       class plus an explicit ambiguous-count that is NOT folded into any other class.
  
  Parallel-safety: standalone once COMMS-CORPUS and COMMS-CONSENT resolve; no live bus needed for the
  classification pass itself.
  
  Depends on: COMMS-CORPUS, COMMS-CONSENT. Blocks COMMS-DOC.
  _Proof: test -f docs/comms/TYPES.md && grep -qi 'ambiguous' docs/comms/TYPES.md && echo TYPES_OK_

### EPIC CONTEXT — CONTEXT: cut the token cost of this repo's documentation without losing load-bearing rationale

- [ ] CONTEXT-DRIFT-PHANTOM · CONTEXT-DRIFT-PHANTOM: two agent defs instruct writing to SESSION_REPORT.md, which has never existed — docs, P2
  Priority P2 justification: feature-runner.md and security.md instruct agents to write to a file
  that has never existed in this repo. Cheap to hit and cheap to fix: an agent that obeys creates an
  untracked root-level file and then fails CLAUDE.md step 10's clean-tree requirement.
  
  Definition of done: both references removed, or retargeted to the Spec Server task-note journal
  (kind=report / kind=model notes), whichever the implementer judges reads more naturally in context.
  
  Depends on: none besides CONTEXT-DOCCHECK.
  
  Parallel-safe: yes. Size: 15 minutes. Saving: approximately 0 tokens; correctness only.
  _Proof: bash scripts/proof-check.sh '! grep -rq "SESSION_REPORT" .claude/ && ! test -e SESSION_REPORT.md'_
- [ ] CONTEXT-LOG-RETIRE · CONTEXT-LOG-RETIRE: AGENT_LOG.md freezes its narrative and moves to one line per task — docs, P2
  Priority P2 justification: AGENT_LOG.md has grown to 43 entries averaging 5,963 B each, all written
  in 6 days (roughly 43 KB/day, the fastest-growing file in the repo), with no committed tooling that
  reads it -- the only automated readers were two `todo` proof_cmds. It is also the file that
  repeatedly shows `MM` in git status and blocks pathspec commits. Every sampled entry has a
  corresponding Spec Server task whose notes are equal or richer.
  
  USER DECISION -- ALREADY RULED, not a blocker (2026-08-08): APPROVED as "freeze + one-line entries".
  The earlier "needs a user decision before implementation" gate is REMOVED. The ruling: a dated
  "narrative entries end here" marker; the existing ~3,451 lines of narrative stay UNTOUCHED
  (append-only is respected, nothing is deleted or rewritten); the new convention going forward is one
  line per task, <= 240 B, carrying task id, sha, gate verdicts and proof verdict; REVIEWER/SECURITY
  SKIP JUSTIFICATIONS STAY IN AGENT_LOG.md -- that is the one category CLAUDE.md step 10 uniquely
  mandates recording there, and it fits on one line. All other narrative moves to `kind=report` task
  notes on the Spec Server.
  
  RECORDED FALSIFIER (keep this, it is load-bearing): if HANDOVER-CONTRIBUTING finds that Spec Server
  credentials cannot transfer to a new maintainer, REVERSE THIS TASK -- a credential-less maintainer
  would lose all in-repo narrative from the freeze date forward with no way to read the note journal
  that replaced it. In that event, build a notes-to-WORKLOG.md exporter instead of retiring the
  narrative.
  
  Definition of done:
    1. A dated "## 2026-0X-XX -- narrative entries end here" marker appended to AGENT_LOG.md. Existing
       lines untouched.
    2. New convention documented and enforced going forward: one line per task, <= 240 B --
       `date . <task-key/public_id> . <sha> . gates: reviewer=... security=... (or SKIPPED: <one-line
       reason>) . proof: <proof-check verdict>`.
    3. Skip justifications stay in AGENT_LOG.md per CLAUDE.md step 10 -- confirm this explicitly in the
       new convention text so nobody "simplifies" it away later.
    4. CLAUDE.md steps 8 and 10 updated to describe the new convention. A DECISIONS.md entry recording
       the change, the loss (credential-less narrative access from the freeze date), and the falsifier
       above -- do NOT write DECISIONS.md as part of THIS task's own execution if a concurrent appender
       risk exists; coordinate per CLAUDE.md's shared-file rules.
  
  Who loses what: a reader WITHOUT Spec Server credentials loses in-repo narrative detail from the
  freeze date on. Genuine loss, mitigated by the one-liner carrying the task's public_id so the full
  narrative is one API call away for a credentialed reader.
  
  CONFLICT recorded as a real relation, MUST land first or be rescoped: open task 0ba2372a
  ("Journal catch-up: DECISIONS.md + AGENT_LOG.md entries owed by INVITE-MINT and MTLS-ROTATE") greps
  AGENT_LOG.md for narrative content as part of its own proof. If it has not landed before this task
  starts, either land it first or rescope it to write to the note journal instead of AGENT_LOG.md
  narrative.
  
  Depends on: CONTEXT-DOCCHECK; CONTEXT-DONEGATE-CANON (sixth and LAST of the six CLAUDE.md-serialised
  tasks). Also depends on 0ba2372a per the conflict above.
  
  Parallel-safe: no. Size: half a day.
  
  Saving basis -- PER-TASK OUTPUT (distinct from per-spawn and per-read: this is narrative text an
  agent no longer WRITES, once per completed task, not once per spawn or once per file read): roughly
  5,700 B of narrative not written per task => approximately -1,425 output tokens/task, approximately
  -14.3k output tokens/session at 10 tasks/session. Plus the file stops growing ~43 KB/day, plus a
  large drop in `MM`-blocked commits.
  _Proof: bash scripts/proof-check.sh 'grep -q "narrative entries end here" AGENT_LOG.md && bash scripts/doc-check.sh section CLAUDE.md "## Work in atomic increments" "one line per task" && bash scripts/doc-check.sh section AGENT_LOG.md "## Convention" "reviewer=" "SKIPPED:"'_
- [ ] CONTEXT-DRIFT-WRAPPERS · CONTEXT-DRIFT-WRAPPERS: two per-spawn files still call the retired shell wrappers 'the ONLY interface' — docs, P1
  Priority P1 justification: CLAUDE.md's own repository-layout row and documentation.md:19 instruct
  agents to maintain a surface that invariant 7 has already retired (the compiled Go CLI is THE
  client; scripts/bus-*.sh wrappers are retired as their CLI subcommands land). The documentation
  agent acting on this instruction does WRONG work, not merely wasted work -- this is a correctness
  fix, not a size fix.
  
  Definition of done: CLAUDE.md's repository-layout row and .claude/agents/documentation.md:19
  restated to match invariant 7: the CLI is the client; wrappers are retired as their subcommands
  land; do not add a new one. Do NOT touch AGENT_PROTOCOL.md in this task -- an existing CLI-epic task
  (CLI-10) owns that rewrite; duplicating it here would create the exact two-copies-drift problem this
  epic exists to fix.
  
  Depends on: CONTEXT-DOCCHECK; CONTEXT-NOTESBLOCK (fourth of the six CLAUDE.md-serialised tasks --
  must run after NOTESBLOCK, before CONTEXT-LOG-RETIRE). CONFLICT recorded as a real relation: the
  existing open task "Stale CONTRACTS.md pointers after the CONTRACTS-SPLIT: README.md:88,
  AGENT_PROTOCOL.md:122, CLAUDE.md:332" (public_id f0ef1ed9) ALSO edits CLAUDE.md near this same area
  -- that existing task must land FIRST; this task is sequenced after it.
  
  Parallel-safe: no. Size: 30 minutes. Saving: approximately 0 tokens directly; this is a correctness
  fix, priced at P1 for that reason alone, not for its (negligible) token saving.
  _Proof: bash scripts/proof-check.sh '! grep -q "the ONLY interface agents use" CLAUDE.md && ! grep -q "the \`scripts/bus-\*.sh\` wrappers are the" .claude/agents/documentation.md && bash scripts/doc-check.sh section CLAUDE.md "## Repository layout" "retired"'_
- [ ] CONTEXT-PROTOCOL-WALFLOOR-DEDUP · CONTEXT-PROTOCOL-WALFLOOR-DEDUP: one file owns the WAL-index-floor bytes, not two that can silently diverge — docs, P2
  Priority P2 justification: PROTOCOL.md section 11 (roughly 7,388 B) and CONTRACTS-ONDISK.md's WAL
  record-index-floor section (roughly 11,541 B) describe THE SAME on-disk file with the same ASCII
  header diagram, the same three-shapes compatibility table, the same field semantics word-for-word,
  the same forensic statistic ("2268 of 2289 measured truncation offsets"), and the same
  MISSING/UNVERIFIED/CORRUPT taxonomy. PROTOCOL.md's own text even claims the split is "bytes here,
  rationale there" when in fact both files carry both. This is a live DIVERGENCE risk -- a correction
  landing in one copy is invisible in the other -- not just a byte-count problem.
  
  Definition of done: CONTRACTS-ONDISK.md (its charter per CLAUDE.md's plane-file split) keeps the
  FULL text, unabridged. PROTOCOL.md section 11 reduces to the byte diagram, the field-name list, and
  an explicit pointer to CONTRACTS-ONDISK.md. The forensic statistic and the failure-taxonomy prose
  are kept IN FULL in the owning file -- nothing is deleted from the repository, only de-duplicated.
  
  Who loses what: a PROTOCOL.md-only reader loses the failure taxonomy at that specific spot and gets
  a named pointer instead.
  
  CONFLICTS recorded as real relations, both MUST land first (both are higher priority and both own
  PROTOCOL.md): the open P0 task DUR-4-FU-DOCS (public_id 0b6d5c11) and the open P1 task
  MSG-FU-SUFFIXFLOOR-FU-DOCS (public_id e5fa08ba) both edit PROTOCOL.md. Sequence this task strictly
  after both.
  
  Depends on: CONTEXT-DOCCHECK; DUR-4-FU-DOCS; MSG-FU-SUFFIXFLOOR-FU-DOCS.
  
  Parallel-safe: no. Size: 3 hours.
  
  Saving basis -- PER-READ: approximately -1.5k tokens per PROTOCOL.md read, plus removal of a
  divergence hazard that has no token value but a real correctness value (a correction applied once
  instead of needing to be applied twice).
  _Proof: bash scripts/proof-check.sh 'bash scripts/doc-check.sh section CONTRACTS-ONDISK.md "durable WAL record-index floor" "2268 of 2289" "UNVERIFIED" && ! grep -q "2268 of 2289" PROTOCOL.md && grep -q "CONTRACTS-ONDISK.md" PROTOCOL.md && test "$(wc -c < PROTOCOL.md)" -le 74000'_
- [ ] CONTEXT-READRULE · CONTEXT-READRULE: tell agents to grep and range-read the big docs, in the one file every agent gets — docs, P1
  Priority P1 justification: highest-expected-value item in the epic, and it is ADDITIVE, not a
  deletion -- it changes HOW a document is fetched, not what information exists.
  
  Definition of done: a ~14-line "## Reading the documents in this repo" section in CLAUDE.md: current
  line/byte sizes of the eight large docs; SPEC.md is NEVER whole-read (`claim-next` and the task API
  give you the task directly -- the mirror exists for humans without credentials); DECISIONS.md ->
  read DECISIONS-INDEX.md first, then `Read` with offset/limit, never whole; CONTRACTS-* -> `grep -n
  '^## '` to locate a section, then range-read it; before whole-reading ANY file over 600 lines, read
  its first 20 lines first (that is where frozen/superseded banners live).
  
  Who loses what: nobody loses information -- this only constrains HOW it is fetched. The bet is that
  a grepped/range-read answer is as good as a whole-read one. Falsifier: an agent asserting something
  false about a doc it grep-sampled instead of reading in full. Two occurrences => narrow the rule to
  "grep to locate, then range-read +/-60 lines" rather than deleting it.
  
  Depends on: CONTEXT-DOCCHECK; CONTEXT-CLAUDE-TRIM (second of the six CLAUDE.md-serialised tasks --
  same file, must run after CLAUDE-TRIM, before CONTEXT-NOTESBLOCK). Soft dependency on
  HANDOVER-DECISIONS-INDEX: the pointer text here names DECISIONS-INDEX.md, so land this after that
  task or the pointer names a file that does not yet exist.
  
  Parallel-safe: no (owns CLAUDE.md, position 2 of 6 in the serialised chain). Size: 1 hour.
  
  Saving basis -- mixed and must NOT be conflated: a PER-SPAWN COST of approximately +900 B
  (~+225 tokens/spawn, ~+6.8k tokens/session) against a PER-READ saving of roughly -105k tokens each
  time it prevents one whole-read of SPEC.md (or roughly -76k tokens for a whole-read of DECISIONS.md)
  -- these are different denominators and differ by orders of magnitude; do not add them as if
  comparable. Breaks even if it prevents one whole-read per approximately 15 sessions, which is
  plausible but is an estimate (EV), not a guarantee -- record it as such.
  _Proof: bash scripts/proof-check.sh 'bash scripts/doc-check.sh section CLAUDE.md "## Reading the documents in this repo" "never whole-read" "SPEC.md" "DECISIONS-INDEX.md" "offset" "first 20 lines"'_
- [ ] CONTEXT-RESERVE-CANON · CONTEXT-RESERVE-CANON: the reservation guidance stops disagreeing with itself across four agent defs — docs, P1
  Priority P1 justification: pure correctness. Three of four copies of the reservation-namespace
  block instruct an action -- "seed the namespace past the epic's existing max" -- that a 2026-08-08
  Spec Server change turned into a 409-loop that BURNS reservation numbers. Any agent that files a
  follow-up task key can hit this. feature-runner.md already carries the corrected copy; the other
  three defs still ship the disproven instruction.
  
  Definition of done: feature-runner.md's corrected copy becomes canonical and moves to
  .claude/ORCHESTRATION.md (created by CONTEXT-CLAUDE-TRIM). All four defs (planner, spec-keeper,
  deep-diver, feature-runner) drop their own copy of the block for a 2-line pointer plus the standing
  one-liner already in CLAUDE.md ("Numbers are reserved, not chosen"). The MOBILE-21/23/24 collision
  evidence AND the 409-loop correction both survive VERBATIM in the canonical copy -- that narrative
  is exactly the kind this repo has proven it needs kept, not summarised.
  
  Depends on: CONTEXT-CLAUDE-TRIM (creates .claude/ORCHESTRATION.md -- this task cannot start until
  that file exists).
  
  Parallel-safe: yes vs. the CLAUDE.md-chain tasks (this task does not touch CLAUDE.md itself, only
  ORCHESTRATION.md and the four agent defs); NOT parallel-safe vs. CONTEXT-DISPATCH-RULE -- both own
  .claude/ORCHESTRATION.md and must be sequenced against each other (order between the two is not
  mandated; spec-keeper or the implementer picks one at claim time and the other follows).
  
  Size: 1 hour.
  
  Saving basis -- PER-SPAWN, and the saving is ONLY real because the canonical text lands in an
  on-demand file rather than CLAUDE.md: roughly 1,900 B removed across 4 defs => a weighted
  approximately -950 B/spawn, approximately -238 tokens/spawn, approximately -7.1k tokens/session.
  Putting the canonical text in CLAUDE.md instead would be token-NEUTRAL (paid on all ~30 spawns to
  save it on ~15) -- do not "simplify" this task by relocating there.
  _Proof: bash scripts/proof-check.sh 'bash scripts/doc-check.sh section .claude/ORCHESTRATION.md "## Reserving task keys" "MOBILE-21" "409" "repeat the request unchanged" && test "$(grep -rl "seed it past" .claude/agents/ | wc -l)" -eq 0'_
- [ ] CONTEXT-CLI-SECTIONS · CONTEXT-CLI-SECTIONS: CONTRACTS-CLI.md's 857-line mega-section becomes real, range-readable sections — docs, P2
  Priority P2 justification: one `##` section in CONTRACTS-CLI.md spans roughly 75% of the file
  (lines 243-1,099), containing pinning, signed sends, exit codes, JSON shapes, watch, credential
  storage, cursor migration, the messaging keypair, idempotency and the client package, all flattened
  as `###` subsections under ONE heading. It defeats range-reading completely -- a reader looking for
  exit codes has no way to jump there without reading the whole block.
  
  Definition of done: pure heading promotion -- `###` -> `##` at natural topic boundaries. NO PROSE
  CHANGED, NO BYTES REMOVED -- this is a restructure, not a reduction, and its proof enforces that.
  Enforce going forward: no `##` section in this file may exceed 250 lines.
  
  Depends on: CONTEXT-DOCCHECK. CONFLICT recorded as a real relation, MUST land first: the existing
  open task "CONTRACTS-CLI.md client export table is missing the three symbols MTLS-EXPIRY added"
  (public_id 083c468e) also owns this file -- it must land before this task starts.
  
  Parallel-safe: no vs. any other CONTRACTS-CLI.md task. Blocks: CONTEXT-PLANE-TOC (regenerate the TOC
  after this restructure, not before).
  
  Size: approximately 1 day -- FLAG, at the epic's stated size limit. Split point if it runs long:
  (1) pinning + sends + exit codes; (2) watch + credential storage + idempotency + client package.
  
  Saving basis -- PER-READ: approximately -14k tokens per targeted lookup that no longer needs the
  full 857-line block.
  _Proof: bash scripts/proof-check.sh 'python3 -c "import sys; L=open(\"CONTRACTS-CLI.md\").read().splitlines(); h=[i for i,l in enumerate(L) if l.startswith(\"## \")]+[len(L)]; m=max(b-a for a,b in zip(h,h[1:])); print(\"sections\",len(h)-1,\"max\",m); sys.exit(0 if len(h)-1>=8 and m<=250 else 1)" && test "$(wc -c < CONTRACTS-CLI.md)" -ge 87000'_
- [ ] CONTEXT-PLANE-TOC · CONTEXT-PLANE-TOC: a generated heading index at the top of every large reference doc — tooling, P2
  Priority P2 justification: no CONTRACTS-* plane file, PROTOCOL.md or INVARIANTS.md has a table of
  contents; CONTRACTS-ONDISK.md alone has 12 sections across 1,295 lines and today a reader has to
  `grep -n '^## '` it themselves to find one. This is the mechanism CONTEXT-READRULE's grep-first
  guidance depends on actually existing.
  
  Definition of done: scripts/gen-doc-toc.sh inserts/refreshes a `<!-- TOC -->...<!-- /TOC -->` block
  (heading text + line number) in CONTRACTS-CLI.md, CONTRACTS-HTTP.md, CONTRACTS-ONDISK.md,
  CONTRACTS-AGENT.md, PROTOCOL.md, INVARIANTS.md. Must be IDEMPOTENT (running it twice produces no
  diff), so it cannot rot; wired into `doc-check.sh budget` as a "TOC is current" assertion.
  
  Cost, stated honestly: roughly +800 B per file, paid once per file per read -- this is a per-read
  cost that pays back on the very first targeted read of any of these files.
  
  Depends on: CONTEXT-DOCCHECK; soft-runs-after CONTEXT-CLI-SECTIONS (regenerate the TOC after that
  task's heading restructure, not before) and after the PROTOCOL.md/CONTRACTS-ONDISK.md dedup task
  CONTEXT-PROTOCOL-WALFLOOR-DEDUP. NOTE: INVARIANTS.md already has its 11 `### Invariant N` headings
  (landed 2026-08-08, outside this epic) so this task indexes an already-headed file for that one; no
  extra dependency needed there.
  
  Parallel-safe: yes vs. everything in the epic except CONTEXT-CLI-SECTIONS and
  CONTEXT-PROTOCOL-WALFLOOR-DEDUP, which it must follow. Size: half a day.
  
  Saving basis -- PER-READ: a targeted CONTRACTS-ONDISK.md lookup drops from 1,295 lines to roughly
  160 lines needed => approximately -24k tokens per targeted lookup.
  _Proof: bash scripts/proof-check.sh 'bash scripts/gen-doc-toc.sh && git diff --quiet -- CONTRACTS-*.md PROTOCOL.md INVARIANTS.md && for f in CONTRACTS-CLI.md CONTRACTS-HTTP.md CONTRACTS-ONDISK.md CONTRACTS-AGENT.md PROTOCOL.md INVARIANTS.md; do grep -q "<!-- TOC -->" "$f" || exit 1; done'_
- [ ] CONTEXT-FANOUT-COMPRESS · CONTEXT-FANOUT-COMPRESS: shrink the 2,040 B fan-out doctrine duplicated across five reviewer-family defs — docs, P2
  Priority P2 justification: real per-spawn saving, but this is the task the planning pass flagged as
  the FIRST TO CUT if the epic needs to shrink -- it is the one item where no concrete harm from the
  deleted text could be named (see epic notes for the planner's full disagreement). Keep it filed, but
  spec-keeper/implementer should treat it as expendable relative to everything else in this epic.
  
  Definition of done: the fan-out doctrine block compressed to roughly 500 B in reviewer.md,
  security.md, architecture-reviewer.md, reliability-reviewer.md, performance-reviewer.md. Rules kept
  VERBATIM: sub-agents are READ-ONLY; ask a NARROW question; VERIFY any fact the conclusion depends
  on; prefer one good explorer over five shallow ones. Dropped: the worked good/bad-brief illustrative
  examples.
  
  Who loses what: a reviewer loses the illustrations of a good brief; it keeps every rule verbatim.
  Falsifier: a reviewer's explorer returning a file dump that the reviewer then has to read anyway --
  two occurrences => restore the examples.
  
  Depends on: CONTEXT-DOCCHECK. Otherwise none -- does not touch CLAUDE.md.
  
  Parallel-safe: yes. Size: 1 hour.
  
  Saving basis -- PER-SPAWN: roughly -1,540 B on 5 of 14 agent types => a weighted approximately
  -460 B/spawn, approximately -115 tokens/spawn, approximately -3.5k tokens/session.
  _Proof: bash scripts/proof-check.sh 'python3 -c "import glob,sys; bad=[f for f in glob.glob(\".claude/agents/*.md\") if \"Fanning out\" in open(f).read() and len(open(f).read().split(\"## Fanning out\")[1].split(chr(10)+chr(35)+chr(35)+chr(32))[0].encode())>700]; print(bad); sys.exit(1 if bad else 0)" && test "$(grep -rc "READ-ONLY" .claude/agents/reviewer.md)" -ge 1'_
- [ ] CONTEXT-SPEC-BRIEF · CONTEXT-SPEC-BRIEF: the SPEC.md mirror carries the lede of each task, not the full 382 KB of descriptions — tooling, P2
  Priority P2 justification: the largest single PER-READ number in this epic. Partially overlapping
  with CONTEXT-READRULE (see the epic-level disagreement note recorded on this epic -- if agents
  actually stop whole-reading SPEC.md because of the READRULE behavioural change, this task's saving
  partly evaporates; the two are closer to substitutes than additive, and READRULE should land first
  since it is cheaper and broader).
  
  Definition of done: scripts/gen-spec-mirror.sh gains description truncation: first paragraph, hard
  capped at 600 B, followed by a pointer -- "full text: bash scripts/spec-cloud.sh -s
  /api/v1/projects/agent-bus/tasks/<public_id>". A `--full` flag reproduces today's untruncated output
  exactly. `proof_cmd` and `status_note` lines are KEPT IN FULL -- they are already short (22.8 KB /
  22.2 KB total across all open tasks) and are exactly what a reader checks first.
  
  Who loses what: a CREDENTIAL-LESS reader loses scope/non-scope/cross-reference detail in the mirror.
  An agent loses nothing -- `claim-next` and `GET tasks/<id>` return the full description from the API
  regardless of what the mirror shows. This is the same trade the closed-task mirror filter already
  made, one notch further, and is the item the planning pass was LEAST certain about.
  
  Measured: open task descriptions 382,052 B -> first-paragraph-only 150,144 B -> capped@600B
  approximately 110-130 KB. Saving approximately 250-270 KB, approximately 62-68k tokens PER WHOLE-READ
  of the mirror.
  
  Depends on: HARD dependency on the in-flight SPEC.md-generation-filter task (same script,
  scripts/gen-spec-mirror.sh) being committed first. Also depends on CONTEXT-DOCCHECK.
  
  Parallel-safe: no (owns scripts/gen-spec-mirror.sh). Size: half a day.
  
  Saving basis -- PER-READ: approximately -62k tokens each time the mirror would otherwise have been
  read whole. Also a small per-write saving in the repo diff each time the mirror regenerates.
  _Proof: bash scripts/proof-check.sh 'bash scripts/gen-spec-mirror.sh --stdout > /tmp/sm.md && test "$(wc -c < /tmp/sm.md)" -le 200000 && grep -q "full text: bash scripts/spec-cloud.sh" /tmp/sm.md && bash scripts/gen-spec-mirror.sh --all --stdout | wc -c | xargs -I{} test {} -ge 600000'_
- [ ] CONTEXT-CLAUDE-TRIM · CONTEXT-CLAUDE-TRIM: the agent roster descriptions and model-selection rationale leave CLAUDE.md's per-spawn path — docs, P1
  Priority P1 justification: CLAUDE.md is injected into EVERY agent spawn (per-spawn cost). Two
  sections in it benefit only the ~4 agent types that spawn others, and today all ~30 spawns in a
  session pay for them regardless.
  
  Definition of done: new `.claude/ORCHESTRATION.md` (read ON DEMAND, not per-spawn) takes: the 14
  roster descriptions (one paragraph each), the model-selection RATIONALE, and the feature-runner
  override note. CLAUDE.md keeps: (a) the bare 14 agent names, one line; (b) the one-line rule "ALWAYS
  pass model explicitly: sonnet = mechanical, opus = judgment/correctness-critical"; (c) an imperative
  pointer -- "Before spawning ANY sub-agent, read .claude/ORCHESTRATION.md."  This is the same
  rule-inline / rationale-relocated pattern already applied to CLAUDE.md -> INVARIANTS.md.
  
  Who loses what: a non-spawning agent loses the one-paragraph description of every OTHER agent --
  it never used them. A spawning agent pays one extra Read per session for ORCHESTRATION.md.
  
  Depends on: CONTEXT-DOCCHECK (proof instrument). HARD PRE-REQUISITE: the in-flight CLAUDE.md split
  (rule inline, rationale to INVARIANTS.md) must be COMMITTED before this task starts -- otherwise
  this edits a file with uncommitted deletions in it, which is exactly the `MM` pathspec-commit trap
  CLAUDE.md itself warns about (a pathspec commit takes the WORKTREE, not the index).
  
  Parallel-safe: NO -- this is the first of six tasks that serialise on CLAUDE.md, in this order:
  CONTEXT-CLAUDE-TRIM -> CONTEXT-READRULE -> CONTEXT-NOTESBLOCK -> CONTEXT-DONEGATE-CANON ->
  CONTEXT-DRIFT-WRAPPERS -> CONTEXT-LOG-RETIRE. That serialisation is this epic's schedule risk --
  each of the six must land, in order, before the next starts; do not parallelise any pair of them.
  
  Size: 2 hours.
  
  Saving basis -- PER-SPAWN (paid on every one of ~30 spawns/session, NOT the same order of magnitude
  as a per-read saving): roughly 2,279 B (roster descriptions) + 938 B (model-selection rationale)
  collapsing to ~630 B of pointer text => approximately -2,587 B/spawn, approximately -647 tokens/spawn,
  approximately -19,400 tokens/session at 30 spawns (4 bytes/token, markdown-with-code).
  _Proof: bash scripts/proof-check.sh 'bash scripts/doc-check.sh section CLAUDE.md "## Agent roster" "ORCHESTRATION.md" && bash scripts/doc-check.sh section .claude/ORCHESTRATION.md "## Model selection" "claude-opus-5" "feature-runner" && test "$(wc -c < CLAUDE.md)" -le 21500'_
- [ ] CONTEXT-DISPATCH-RULE · CONTEXT-DISPATCH-RULE: dispatch briefs stop restating standing rules already in every sub-agent's context — docs, P1
  Priority P1 justification: the only saving in this epic denominated in OUTPUT tokens, which are the
  expensive direction, and it targets the orchestrator's specific documented habit of repeating
  standing warnings in nearly every dispatch brief.
  
  Definition of done: a "## Writing a dispatch brief" section in .claude/ORCHESTRATION.md: a brief
  carries ONLY task-specific content -- the task id, the goal, the file-ownership boundary, the proof
  command, and any correction. It NEVER restates the gofmt-exits-0-while-listing-files trap, the
  proof-check.sh subtest blind spot, the pathspec/worktree trap, "do not commit", or "do not mutate
  Spec Server state" -- every sub-agent already receives CLAUDE.md in full plus its own agent
  definition. Includes one escape hatch: restate a standing rule only when explicitly OVERRIDING it
  for this task, and say so.
  
  Evidence this redundancy is real, not assumed: the planning agent that produced this task breakdown
  is itself a sub-agent whose context already contained CLAUDE.md in full plus its own agent
  definition plus all 14 frontmatter descriptions -- restating those rules in a dispatch brief is
  provably redundant, not merely suspected to be.
  
  Who loses what: the orchestrator loses the belt-and-braces feeling of repeating a warning inline.
  Falsifier: an agent violating one of those five traps AFTER this lands. If that happens, the
  correct response is to strengthen the rule in CLAUDE.md (paid once per spawn) -- NOT to resume
  restating it per dispatch (paid per dispatch, forever, in output tokens).
  
  Depends on: CONTEXT-CLAUDE-TRIM (creates ORCHESTRATION.md).
  
  Parallel-safe: no vs. CONTEXT-RESERVE-CANON (same new file, .claude/ORCHESTRATION.md) -- sequence
  the two against each other, order not mandated.
  
  Size: 45 minutes.
  
  Saving basis -- PER-DISPATCH (distinct from per-spawn and per-read; a dispatch happens once per
  sub-agent invocation, not once per file read): roughly 500 tokens of boilerplate x roughly 30
  dispatches/session => approximately -15k OUTPUT tokens/session, plus that boilerplate stops
  accreting permanently in the orchestrator's resident context. Basis is ESTIMATED, not measured --
  sampled `kind=request` notes are summaries of dispatch briefs, not the briefs themselves, so the
  actual per-dispatch boilerplate is not recorded anywhere verifiable. This is the epic's
  least-evidenced number; do not treat it as more certain than the per-spawn figures above.
  _Proof: bash scripts/proof-check.sh 'bash scripts/doc-check.sh section .claude/ORCHESTRATION.md "## Writing a dispatch brief" "only task-specific" "never restate" "gofmt" "pathspec" "already receives CLAUDE.md"'_
- [ ] CONTEXT-CONTRACTS-PARKING · CONTEXT-CONTRACTS-PARKING: CONTRACTS.md admits, in its own text, that it is 90% parking lot — docs, P2
  Priority P2 justification: only lines 1-36 of CONTRACTS.md are actually an index. Lines 38-352
  (roughly 26 KB, roughly 90% of the file) are substantive contract tables whose own text says they
  were "recorded here instead" of their eventual plane file. A reader told "CONTRACTS.md is an index"
  has to read 351 lines to discover that most of it is not.
  
  Definition of done: do NOT fold the parked content into plane files (see rationale below). Insert at
  line 37 a "## PARKED -- contract text awaiting its plane file" heading, and add a one-line reader
  instruction in the index block: "lines 1-36 are the index; everything below is parked content, each
  block naming its owning Spec task -- stop reading at line 36 unless you are working one of those
  tasks." Each parked block gains its owning task's public_id. Cap CONTRACTS.md's size in
  docs/doc-budgets.tsv so it cannot grow further without triggering a fold.
  
  Why NOT fold: the RELAY-2/3 block (lines 108-237) and the SIGN-7 block (lines 239-306) describe
  ROUTES THAT DO NOT EXIST YET. Moving them into CONTRACTS-HTTP.md would document unshipped surface as
  if it were live -- that is precisely why they were parked in the first place, and it remains a good
  reason. The MSG-FU block (lines 38-106) IS foldable and is already owned by existing FU follow-up
  tasks -- do not duplicate that work here.
  
  Depends on: CONTEXT-DOCCHECK.
  
  Parallel-safe: yes. Size: 1 hour.
  
  Saving basis -- PER-READ: approximately -6.5k tokens each time an agent consults CONTRACTS.md as an
  index and can now stop at line 36 instead of reading all 351.
  _Proof: bash scripts/proof-check.sh 'bash scripts/doc-check.sh section CONTRACTS.md "## PARKED" "owning Spec task" && head -40 CONTRACTS.md | grep -q "stop reading at line 36"'_
- [ ] CONTEXT-NOTESBLOCK · CONTEXT-NOTESBLOCK: one canonical note-journal instruction, not twelve copies (two of them wrong) — docs, P1
  Priority P1 justification: CORRECTNESS first, tokens second. This removes 12 copies of a STALE
  model id (`claude-opus-4-8` / `claude-sonnet-4-6`) that are today corrupting the one auditable cost
  signal this project has -- the `kind=model` notes CLAUDE.md itself designates for that purpose.
  CLAUDE.md's own roster uses the current ids `claude-opus-5` / `claude-sonnet-5`.
  
  Definition of done: delete the `### Record your work as Spec Server task notes` block (roughly
  920-1,121 B each) from all 12 `.claude/agents/*.md` files that carry it; replace each with a 2-line
  pointer to CLAUDE.md. CLAUDE.md's existing "## Spec Server task notes are the work JOURNAL" section
  gains the three facts the deleted blocks carried: `author` = your agent slug; every agent posts
  `kind=report` AND `kind=model` on completion; the EXACT current ids `claude-opus-5` /
  `claude-sonnet-5`; one `kind=model` note per agent per task.
  
  Who loses what: nothing -- the canonical text lives in a file every one of those 12 agents already
  receives on every spawn.
  
  Depends on: CONTEXT-DOCCHECK; CONTEXT-READRULE (third of the six CLAUDE.md-serialised tasks, same
  ordering constraint as CONTEXT-CLAUDE-TRIM).
  
  Parallel-safe: no vs. the other CLAUDE.md tasks in the chain; yes vs. everything else in the epic.
  Size: 2 hours.
  
  Saving basis -- PER-SPAWN: roughly 1,000 B removed from 12 of 14 defs => a weighted approximately
  -860 B/spawn, approximately -215 tokens/spawn, approximately -6.5k tokens/session.
  _Proof: bash scripts/proof-check.sh '! grep -rlq "claude-opus-4-8\|claude-sonnet-4-6" .claude/agents/ && test "$(grep -rl "kind=report" .claude/agents/ | wc -l)" -le 2 && bash scripts/doc-check.sh section CLAUDE.md "## Spec Server task notes are the work JOURNAL" "claude-opus-5" "claude-sonnet-5" "one kind=model note per agent per task"'_
- [ ] CONTEXT-DOCCHECK · CONTEXT-DOCCHECK: doc-check.sh -- the instrument every other proof in this epic depends on — tooling, P1
  Priority P1 justification: not for size, but because this repo's known failure mode is a doc proof
  that passes on an incidental match elsewhere in a file -- that has already green-lit a wrong task
  closure here. Every other CONTEXT task claims a doc changed; without a section-scoped assert, each
  of those proofs repeats that exact bug. This is proof-check.sh's sibling and is a hard prerequisite
  to trusting the rest of the epic.
  
  Definition of done: scripts/doc-check.sh with three modes:
    - `section <file> '<heading>' '<needle>'...` -- locates the heading, computes its line range to
      the next same-level heading, asserts each needle occurs INSIDE that range. Exits non-zero if
      the heading is absent (cannot pass vacuously).
    - `budget` -- reads docs/doc-budgets.tsv (path, max_bytes), fails on any overrun; and reads
      docs/doc-preserve.tsv (path, literal_phrase), fails if a phrase is MISSING. Ceilings apply only
      to per-spawn and generated files; DECISIONS.md/AGENT_LOG.md are exempt BY DESIGN, with the
      reason recorded in the tsv file itself.
    - `--selftest` -- asserts the assert: heading-absent => FAIL, needle-only-outside-section => FAIL,
      needle-inside => PASS.
    - Must NOT invoke scripts/proof-check.sh (recursion -- see 69eb6f56).
  
  Files: scripts/doc-check.sh, docs/doc-budgets.tsv, docs/doc-preserve.tsv, CONTRACTS-AGENT.md
  (repo-tooling section, document the new script there).
  
  RED verification observed (2026-08-08, spec-keeper filing): scripts/doc-check.sh does not exist --
  trivially RED, file absent.
  
  Depends on: nothing. Soft-relates to HANDOVER-CHECK (0f909b6c) -- wire `doc-check.sh budget` into
  scripts/check.sh THERE, not here; do not duplicate the wiring in this task.
  
  Parallel-safe: yes. Size: half a day. Saving: 0 tokens directly -- this is the enabler every other
  task's proof_cmd depends on.
  
  Chain: this ships a shell script, so it needs reviewer + security, not documentation-only sign-off.
  
  Blocks every other CONTEXT task (recorded as real `blocks` relations) -- none of their doc-scoped
  proof_cmds are trustworthy until this lands.
  _Proof: bash scripts/proof-check.sh 'bash scripts/doc-check.sh --selftest'_
- [ ] CONTEXT-DONEGATE-CANON · CONTEXT-DONEGATE-CANON: 'do not mark done when the behaviour is not yet live' said once, not three times — docs, P2
  Priority P2 justification: a byte-identical roughly-1,430 B block ("committed does not mean
  running") is currently duplicated verbatim in feature-runner.md, implementer.md and spec-keeper.md.
  
  Definition of done: the canonical text (condensed to 4-6 lines, all rules kept: keep in_progress
  with a status_note until deploy is verified, OR complete as code-only with an honest test_summary and
  a paired <KEY>-DEPLOY/<KEY>-VERIFY follow-up) folded into CLAUDE.md's "## Work in atomic increments"
  step 7. The three duplicate copies are replaced by a pointer to that step.
  
  Depends on: CONTEXT-DOCCHECK; CONTEXT-DRIFT-WRAPPERS (fifth of the six CLAUDE.md-serialised tasks --
  must run after DRIFT-WRAPPERS, before CONTEXT-LOG-RETIRE). NOTE: the top-level sequencing instruction
  for this epic states the six-task CLAUDE.md chain runs CLAUDE-TRIM -> READRULE -> NOTESBLOCK ->
  DONEGATE-CANON -> DRIFT-WRAPPERS -> LOG-RETIRE -- i.e. DONEGATE-CANON precedes DRIFT-WRAPPERS in that
  ordering. The real `blocks` relations filed for this epic follow that authoritative order
  (NOTESBLOCK blocks DONEGATE-CANON; DONEGATE-CANON blocks DRIFT-WRAPPERS); treat the ordering
  statement here as the one that governs, and this task's own dependency line above as describing the
  same chain position, not a conflicting order.
  
  Parallel-safe: no (CLAUDE.md chain). Size: 45 minutes.
  
  Saving basis -- PER-SPAWN, small: weighted approximately -415 B/spawn, approximately -104
  tokens/spawn, approximately -3.1k tokens/session.
  _Proof: bash scripts/proof-check.sh 'test "$(grep -rl "not yet live" .claude/agents/ | wc -l)" -eq 0 && bash scripts/doc-check.sh section CLAUDE.md "## Work in atomic increments" "not yet live"'_
- [ ] CONTEXT-BUDGET-WIRE · CONTEXT-BUDGET-WIRE: the byte ceilings from this whole epic become a standing, wired-in check — tooling, P2
  Priority P2 justification: this is what makes every reduction elsewhere in the epic SAFE rather than
  a one-time snapshot that silently rots -- without it, nothing stops a later edit from re-growing a
  file past the ceiling this epic established, and nothing stops a "helpful" future edit from deleting
  one of the load-bearing traps this epic deliberately preserved.
  
  Definition of done: docs/doc-budgets.tsv populated with the ceilings established by every sizing task
  in this epic. docs/doc-preserve.tsv populated with the load-bearing phrases that must NEVER be
  deleted -- at minimum: "gofmt -l . && echo CLEAN", "takes the WORKTREE, not the index", "no tests to
  run", "deleting it, or deleting the callback beside it", "InsecureSkipVerify", "MOBILE-21",
  "2268 of 2289". `doc-check.sh budget` is invoked from scripts/check.sh.
  
  This task is what makes every reduction above safe: the preservation list means "shrink a file by
  deleting a trap it contains" now fails MECHANICALLY, not just by convention.
  
  Depends on (HARD): HANDOVER-CHECK (public_id 0f909b6c) -- scripts/check.sh must exist before this
  task can wire anything into it. This is also why this task is LAST in the epic: it depends on every
  sizing task above for the ceiling values it records, in addition to the hard HANDOVER-CHECK
  dependency.
  
  Parallel-safe: no (last task in the epic by design). Size: 2 hours.
  
  Saving basis: 0 direct tokens -- this task's value is preventing reversion of every saving recorded
  elsewhere in this epic, not producing a new one.
  _Proof: bash scripts/proof-check.sh 'bash scripts/doc-check.sh budget && grep -q "doc-check.sh budget" scripts/check.sh'_
- [ ] CONTEXT-DEEPDIVE-CONVENTION · CONTEXT-DEEPDIVE-CONVENTION: stop the next 75 KB deep-dive from landing at the repo root — docs, P2
  Priority P2 justification: .claude/agents/deep-diver.md currently mandates writing
  `<TOPIC>_DEEPDIVE.md` at the repo root. Two such files already exist (113 KB combined, both written
  within 6 days) and this instruction is a growth generator that will keep producing more of them at
  root with no staleness signal.
  
  Definition of done: deep-diver.md changed to write `docs/deepdives/<TOPIC>.md` with a mandatory
  4-line header: date, sha measured at, "point-in-time snapshot, not maintained", and "findings must
  be filed as Spec Server tasks -- this document is evidence, not a plan". A stated soft size ceiling
  is recorded in the same section. The two EXISTING deep-dive files are NOT moved by this task -- see
  the epic-level disagreement note recorded on this epic for why (DECISIONS.md cites them by bare
  filename at multiple sites and is append-only; internal/ids/suffixstore.go:96 cites
  ID2_WIRING_DEEPDIVE.md in production code and a live task's proof_cmd greps it, so it is cited
  reference material, not an unmaintained artefact, and moving it would create dangling references in
  a file this repo is forbidden to repair).
  
  Depends on: CONTEXT-DOCCHECK.
  
  Parallel-safe: yes. Size: 30 minutes.
  
  Saving basis -- AVOIDED FUTURE GROWTH, not a measured reduction of anything existing today:
  approximately 55 KB per future investigation that no longer lands at root, and each future doc
  self-declares as stale in its first 4 lines -- which is exactly what CONTEXT-READRULE's
  head-first-20-lines rule keys off.
  _Proof: bash scripts/proof-check.sh 'bash scripts/doc-check.sh section .claude/agents/deep-diver.md "## Output" "docs/deepdives/" "not maintained" "evidence, not a plan" && ! grep -q "_DEEPDIVE.md\` at the repo root" .claude/agents/deep-diver.md'_
- [ ] CONTEXT-AGENTDESC-TRIM · CONTEXT-AGENTDESC-TRIM: budget the 14 frontmatter description: fields, the real per-spawn agent-def lever — docs, P2
  Priority P2 justification: a real per-spawn cost with zero correctness risk, but smaller than the
  P1 items above. NOTE for anyone re-scoping this task: the 14 `.claude/agents/*.md` FILES total
  93,644 B, but that total is a REPOSITORY size, not a per-spawn cost -- only ONE agent definition is
  injected per spawn (mean 5,853 B). Do not restate the 93 KB / 16-file figures as a per-spawn cost;
  that overstates this task's saving by roughly 16x. The genuine per-spawn lever this task addresses
  is narrower: the frontmatter `description:` field of EVERY agent def IS injected on every spawn that
  carries the Agent tool (currently 3,545 B total across 14 files, five fields over 350 B each).
  
  Definition of done: each of the 14 `description:` fields reduced to <= 120 B: one sentence of what
  the agent does, plus "Use when...". The routing signal (what it does, when to pick it) is preserved;
  any worked examples currently living in the description move into the BODY of the def, where they
  are paid only when that specific agent is spawned.
  
  Who loses what: a spawning agent loses per-agent elaboration at selection time, keeping only the
  short routing sentence. Falsifier: a measurable rise in wrong-agent dispatches after this lands.
  
  Depends on: CONTEXT-DOCCHECK. Otherwise none.
  
  Parallel-safe: yes -- touches only frontmatter; conflicts with any other agent-def-body task only at
  the file level (sequence loosely with CONTEXT-NOTESBLOCK / CONTEXT-RESERVE-CANON if they touch the
  same file, or accept a trivial merge).
  
  Size: 45 minutes.
  
  Saving basis -- PER-SPAWN: roughly 1,645 B removed, paid by every spawn carrying the Agent tool
  (~70% of spawns) => a weighted approximately -1,150 B/spawn, approximately -288 tokens/spawn,
  approximately -8.6k tokens/session.
  _Proof: bash scripts/proof-check.sh 'python3 -c "import glob,re,sys; bad=[(f,len(re.search(r\"^description:\\s*(.*)\$\",open(f).read(),re.M).group(1).encode())) for f in glob.glob(\".claude/agents/*.md\")]; over=[b for b in bad if b[1]>120]; print(over); t=sum(b[1] for b in bad); print(\"total\",t); sys.exit(0 if not over and t<=1900 else 1)"'_
- [ ] CONTEXT-LOG-GUARD · CONTEXT-LOG-GUARD: the AGENT_LOG.md freeze is enforced mechanically, not hoped for — tooling, P2
  Priority P2 justification: a freeze with no enforcement reverts the first time someone is in a hurry
  -- this closes that gap with a mechanical check rather than a convention nobody re-reads.
  
  Definition of done: scripts/doc-check.sh gains an `agentlog` mode that fails if any entry AFTER the
  freeze marker exceeds 240 B, or if a post-freeze entry lacks a `gates:` field. Registered in
  docs/doc-budgets.tsv.
  
  Depends on: CONTEXT-LOG-RETIRE (needs the freeze marker to exist); CONTEXT-DOCCHECK.
  
  Parallel-safe: no. Size: 2 hours. Saving: 0 direct tokens; this task's value is preventing
  reversion of CONTEXT-LOG-RETIRE's saving, not producing a new one.
  _Proof: bash scripts/proof-check.sh 'bash scripts/doc-check.sh agentlog --selftest && bash scripts/doc-check.sh agentlog'_

### EPIC CORE — Repo skeleton & server bootstrap

- [ ] None · MSG-FU-MAINWIRING: main should construct the hub and pass it as BOTH httpapi.Options.Hub and wal.LogOptions.Applier — core, P1
  The MSG/POLL wave could not touch cmd/** (file ownership), so httpapi.New builds the hub itself whenever Options.Durable also satisfies Path() + Recovered() -- see openHub in internal/httpapi/server.go, which documents this as transitional. Two costs to remove: (1) the durable log is REPLAYED TWICE at startup, once as an fsck by wal.Open with a nil Applier and once read-only by the hub to rebuild the store; (2) a rebuild FAILURE cannot be fatal because httpapi.New returns no error -- it is logged at ERROR and the messaging routes are left unregistered, so an operator sees 404s rather than a refusal to start, which is indistinguishable from running an old build. FIX: main constructs the hub, passes it as wal.LogOptions.Applier so replay happens exactly once inside wal.Open, seals the sequence floor from wal.Recovered, and hands it to httpapi.Options.Hub; a failure is then a startup error. openHub and the recoverableLog assertion are deleted in the same change. ALSO IN SCOPE: cmd/agent-bus/main_test.go TestShutdownReleasesLongPoll parks a SYNTHETIC handler -- point it at the real GET /v1/wait now that the route exists, so the ordering guard covers the real park.
  _Proof: go test -race -run TestRunWiresTheHub ./cmd/agent-bus_
- [~] None · DISCOVERY-DOC: self-describing unauthenticated discovery document so an agent with only a bus URL can bootstrap — httpapi, P1, in progress
  GET /v1/info returns only {bus_id, version, uptime_seconds}, which tells an agent nothing about HOW TO JOIN. An agent handed only a bus URL cannot enrol. Serves invariant 7 (nobody hand-writes HTTP; the compiled Go CLI is THE client) by making the bus self-describing.
  
  Precedent: the Spec Server's own GET /api/v1/agent-enrollments — an unauthenticated, machine-readable document with a `service` name, an ordered `steps` array, exact URLs, an explicit token_source explanation, and what the caller must save.
  
  Scope (server side ONLY, this task): internal/httpapi/** and CONTRACTS-HTTP.md. Add a bounded, static, unauthenticated discovery document describing: what the service is + bus id; the ORDERED enrolment steps with exact paths; whether enrolment is invite-only (describe what is TRUE today and flag what is imminent — INVITE-GATE is still `todo`); that the agent supplies an Ed25519 public key and receives a SERVER-MINTED fully-qualified <bus-id>.<agent-id> it does not choose (invariant 1); the session model (client signs a SERVER-PROVIDED token, max one hour, refresh at 75%); where to get the client and that an importable Go package exists at client/; and the HONEST LIMITS (no TLS yet so loopback only; messages are signed but NOT verified on receipt because key distribution (CRYPTO-4) does not exist).
  
  SECURITY LINE — describe the PROTOCOL, never the ROSTER. No agent list, no agent count, no data-dir path, no peer list, no key material, no on-disk file paths. An unauthenticated caller learns HOW TO JOIN and nothing about WHO HAS JOINED. The response must be bounded and static — its size must NOT grow with bus state (that is both an information leak and a DoS surface). internal/httpapi has a test pinning /v1/info's EXACT field set; it must be updated deliberately, never weakened or deleted.
  
  Design question to settle and record in DECISIONS.md: extend /v1/info vs add a separate endpoint.
  
  Explicitly OUT OF SCOPE (follow-up): the CLI subcommand half, AGENT_PROTOCOL.md and CONTRACTS-CLI.md — cmd/** and client/** are owned by other agents right now.
  _Proof: go test -race ./internal/httpapi/... && D=$(mktemp -d) && P=18173 && (go run ./cmd/agent-bus -listen 127.0.0.1:$P -data-dir "$D" &>"$D/log" & echo $! >"$D/pid") && for i in $(seq 1 30); do curl -sf http://127.0.0.1:$P/healthz >/dev/null 2>&1 && break; sleep 0.5; done && curl -sf http://127.0.0.1:$P/v1/discovery | jq . && kill "$(cat "$D/pid")" 2>/dev/null; rm -rf "$D"_
- [ ] None · The data-dir permission gate checks MODE but not OWNERSHIP, and follows symlinks -- and it is now the SOLE defence for invariant 1 against a downward seq-floor forge — security, P1, deferred
  FOUND INDEPENDENTLY BY BOTH SECURITY GATES on be447589-6583-4d5c-a9d4-ec9d9fef0f1c (committed 217a3c0). Two gates working separately corroborated this, which is why it is filed at P1 rather than as a nit.
  
  MECHANISM, three parts, all at cmd/agent-bus/datadirperm.go:75-96.
  (1) NO OWNERSHIP CHECK. `enforceDataDirPermissions` reads `info.Mode().Perm()` (datadirperm.go:88 onward) and nothing else. Re-verified at HEAD 16da89f by spec-keeper: `grep -rn 'Uid|Gid|Stat_t|Lstat' cmd/agent-bus/ internal/dirlock/` returns ZERO hits in the whole of both. A 0755 directory OWNED BY ANOTHER UID passes cleanly, and that owner can substitute every identity file.
  (2) SYMLINKS ARE FOLLOWED. `os.Stat` and `os.Chmod` both follow symlinks, so an attacker with write access to the PARENT can replace the data dir with a symlink to a 0700 directory they own. PROVED: it starts silently and writes all ten identity files into the attacker's target.
  (3) THE PARENT IS NEVER CONSIDERED. A 0777 non-sticky PARENT also starts silently. Renaming a directory is a permission on its PARENT, so the comment at datadirperm.go:75-96 claiming "every step below trusts that the files in this directory cannot be substituted by another local user" is FALSE in that case -- the directory itself can be swapped wholesale.
  
  WHY THIS IS NOW URGENT RATHER THAN TIDY, and this is the part that changes its priority. One gate traced that `maxPlausibleSeqFloor` is ONE-DIRECTIONAL: it bounds only UPWARD forgery. A DOWNWARD forge is guarded by nothing but the unkeyed digest, and is masked by the log EXCEPT for the minted-but-unspent tail -- i.e. up to `MintBatchSize` = 256 already-signed sequences reissued. That is a genuine INVARIANT 1 violation whose SOLE remaining defence is this directory permission gate. The gate is therefore LOAD-BEARING FOR INVARIANT 1, not defence in depth, and every hole in it is a hole in invariant 1.
  
  SCOPE / FIX. cmd/agent-bus/datadirperm.go. Check the owning uid (refuse a foreign-owned dir rather than repair it -- a non-owner cannot be fixed by chmod). Use `os.Lstat` to detect a symlinked data dir, and consider the parent's mode. NOTE THE KNOWN REGRESSION RISK, raised by the security gate that reviewed the original fix and the reason it was deferred there: an ownership check carries a Docker bind-mount regression risk, and two bricking refusals were already produced during that task. Whatever is chosen must be tested against a bind-mounted volume before it lands, and the refuse-vs-warn choice recorded in DECISIONS.md.
  
  RELATED, NOT DUPLICATES: 6c482cc0-ce83-49e9-a7ff-f8575795cb39 (wal.OpenWriter/RepairTail open bus.wal without O_NOFOLLOW -- same class, different file, internal/wal); ae594fa8-03bb-4d51-aa31-641f5ddcae66 (RUN_DIR ownership/symlink in scripts/bus-serve.sh -- same class, different directory).
  
  PROOF STATE OBSERVED (spec-keeper, HEAD 16da89f, not assumed): `bash scripts/proof-check.sh '<proof_cmd>'` -> verdict=FAIL, exit 1. RED, and RED FOR THE RIGHT REASON (`grep -c Lstat cmd/agent-bus/datadirperm.go` = 0, so the && short-circuits) -- RED today rather than VACUOUS. Both test halves must ALSO be observed RED before the fix.
  _Proof: grep -q 'Lstat' cmd/agent-bus/datadirperm.go && go test -race -count=1 -run 'TestRunRefusesASymlinkedDataDir|TestRunRefusesAForeignOwnedDataDir' ./cmd/agent-bus_
- [ ] None · invite mint bypasses the data-directory permission gate entirely -- the invite blob is the trust anchor, so this is worse than the file substitution the gate closes — security, P0, deferred
  SECURITY GATE FINDING (HIGH) against the data-dir permission gate shipped under be447589-6583-4d5c-a9d4-ec9d9fef0f1c, committed at 217a3c0. PROVED BY RUNNING CODE, end to end, not by reading.
  
  MECHANISM. `enforceDataDirPermissions` is wired into `run()` ONLY. Re-verified at HEAD 16da89f by spec-keeper: the sole call site in the whole tree is cmd/agent-bus/main.go:299 (`grep -rn enforceDataDirPermissions cmd/agent-bus/` returns the definition at datadirperm.go:88 and that one call). `mintInvite` (cmd/agent-bus/invite.go:448-510) stats the dir (invite.go:455), checks IsDir (:464), then takes the lock, replays and APPENDS to the WAL, and publishes the bus certificate fingerprint -- with no permission check anywhere on that path.
  
  MEASURED. On a real 0777 data dir: the server REFUSES to start, but `agent-bus invite mint` exits 0, mutates bus.wal (md5 changed) and emits zero warning.
  
  THE COMPLETED ATTACK CHAIN (the reason this is P0 and not tidy-up). The bus id is readable from the world-readable bus-tls.crt (0644), so an attacker mints a same-CN certificate, drops it plus keys into the 0777 dir, and the operator's next `invite mint` printed a fingerprint BYTE-IDENTICAL to the attacker's certificate. Under invariant 11 the invite blob is the TRUST ANCHOR -- "whoever can substitute an invite can point an agent at a bus of their choosing" -- so the outcome is strictly worse than the file substitution the gate was built to close. The gate refusing `run()` while `invite mint` sails through on the same directory is the whole defect: one command enforces the trust boundary and the other one, which MINTS THE TRUST ANCHOR, does not.
  
  MINIMAL FIX (given by the gate). Call `enforceDataDirPermissions(dataDir, lg)` in `mintInvite` after the `IsDir` check (invite.go:470) and BEFORE `checkBusIdentityPresent`. That placement preserves the existing property that a refusal writes nothing: it is still ahead of the lock and ahead of every write.
  
  ALSO RECORDED, EXPLICITLY LOWER PRIORITY -- `healthcheck`. cmd/agent-bus/healthcheck.go takes -data-dir (:122) and reads only bus-tls.crt (:152); it takes no lock and mutates nothing, so an ungated healthcheck can only report a FALSE OK against a substituted certificate, not launder one. Wire the gate into it in the same change if that is cheap; it does not block this task and must not be used to widen it.
  
  PROOF STATE OBSERVED (spec-keeper, HEAD 16da89f, not assumed): `bash scripts/proof-check.sh '<proof_cmd>'` -> verdict=FAIL, exit 1. RED, and RED FOR THE RIGHT REASON: the pin `grep -c 'enforceDataDirPermissions(dataDir, lg)' cmd/agent-bus/invite.go` returns 0, so the && short-circuits and the (not-yet-written) test never runs -- i.e. this proof is RED today rather than VACUOUS. The test half must ALSO be observed RED before the fix lands, or it proves nothing.
  _Proof: grep -q 'enforceDataDirPermissions(dataDir, lg)' cmd/agent-bus/invite.go && go test -race -count=1 -run TestInviteMintRefusesAnOtherWritableDataDir ./cmd/agent-bus_
### EPIC CRYPTO — End-to-end message cryptography (dual keypairs, Double Ratchet, agent-side validation)

- [ ] CRYPTO-11 · CRYPTO-11: Audit-log content hash for signed (cleartext) messages -- implements the invariant-6 trade-off — durability, P2
  RESCOPED 2026-08-02 (sign-only). SETTLED by the user, verbatim: "ok, log only metadata and routing info." Invariant 6 is now written that way in CLAUDE.md, so the audit log records METADATA AND ROUTING INFO ONLY: message id, sequence, fully-qualified sender and recipient(s), bus path traversed, timestamp, size, and a content hash of the body. It never records bodies. DUR-5 has ALREADY been amended to this exact shape and its description is authoritative for the on-disk record; do not diverge from it. UNGATED: the CRYPTO-1 design spike this task used to wait on is DONE (CRYPTO_DEEPDIVE.md), and its ratchet recommendation was overridden by direct user instruction -- do not action it. WHAT THIS TASK IMPLEMENTS: the content-hash computation over the message body (crypto/sha256, stdlib -- invariant 9, no bespoke construction), wired in at the send path and handed to DUR-5's audit writer alongside the envelope metadata. THE PLAINTEXT-vs-CIPHERTEXT QUESTION THIS TASK USED TO POSE IS NOW MOOT AND MUST NOT BE RE-OPENED: there is no ciphertext -- bodies travel in cleartext with a detached Ed25519 signature -- so the hash is over the body bytes, and it MUST be the same bytes SIGN-1 canonicalises and signs, so that the logged hash and the signature are provably about the same content. State that binding explicitly; a hash over a different serialisation than the signature covers is a silent correctness hole. NON-REPUDIATION (the reason this is worth doing): the hash alone is only a fingerprint anyone could have produced -- paired with SIGN-2's signature over the same canonical bytes it PROVES a specific sender produced specific content at a specific sequence, without the log ever holding the content. Also deliver the operability answer: how a human debugs a flow they cannot read -- ordering, delivery and provenance reconstructable from metadata + hash alone (e.g. correlating hashes across sender / relay / recipient logs to prove the same content transited unmodified). Update PROTOCOL.md's on-disk section to match DUR-5, and make DECISIONS.md say plainly that the audit trail proves DELIVERY, ORDERING and AUTHORSHIP -- not CONTENT. NOTE the asymmetry to state honestly: the audit log withholds the body from a later reader of the log, but the body is NOT confidential in transit -- the bus and every relay peer read it (see RATCHET-2's threat model).
  _Proof: go test -race -run TestAuditLogContentHash ./internal/store_
- [ ] CRYPTO-9 · CRYPTO-9: Cross-bus relay of encrypted messages -- what an intermediate bus can and cannot see — relay, P3, deferred
  GATED: do not start until CRYPTO-1 (design spike) is done and its DECISIONS.md entry is recorded -- the spike chooses the crypto library/primitives, the audit-log trade-off, the ratchet-state durability model, the broadcast/relay scheme and the key-trust model. Implementing before that decision exists is guessing. Make E2E messages survive bus-to-bus relay (RELAY-2) end to end: a message from bus A's agent to bus B's agent must be decryptable ONLY by the destination agent, never by either bus. Requires cross-bus key-bundle fetch (how does an agent on bus A obtain and trust the messaging key of <bus-B>.<agent>? bus B attests it, but bus A is now trusting bus B -- implement the chain CRYPTO-1 defined, and state the residual trust plainly). Specify and test what a relaying/intermediate bus can see: envelope metadata, the traversed-bus path (RELAY-3), fully-qualified sender/recipient ids, sizes and timing -- and what it must never see: content, and any key material that would let it join a session. Cover the partial-failure cases the RELAY epic already worries about (peer down, retry/backoff, loop prevention) so a retried relay cannot cause a ratchet double-advance or a duplicate that decrypts twice.
  _Proof: go test -race -run TestRelayEncrypted ./internal/relay_
- [ ] CRYPTO-4 · CRYPTO-4: Key-distribution endpoint -- server-attested messaging key bundles — auth, P1
  RESCOPED 2026-08-02 per user instruction ("keep it simple, standard sign/verify; encryption later"): this is now a bundle of SIGNING material, not X3DH session-establishment material. GOVERNED BY INVARIANT 9 -- the bus attests bundles by signing them with its own Ed25519 signing key (crypto/ed25519, stdlib, audited); no custom attestation construction. Add the authenticated route that lets an enrolled agent fetch another agent's messaging (signing) key bundle: {fully-qualified <bus-id>.<agent-id>, messaging public key, key_epoch, issued_at}, signed by a bus signing key so the caller can verify the bus is vouching for this binding. Route is keyed by the fully-qualified id (invariant 2). Requires auth (invariant 3): an unenrolled caller gets 401; consider whether roster enumeration via this route needs rate-limiting or scoping. PLUS mandatory TOFU pinning: a recipient pins a peer's messaging public key on first use, in a local pin file; if the bus later serves a DIFFERENT key for a peer whose key is already pinned, that is a hard failure (never an auto-accept, never a silent re-pin) -- this is the actual defence against a malicious bus MITM-ing an established relationship, since attestation alone only protects first contact. Re-pinning requires an explicit human-driven trust command with an out-of-band comparison. key_epoch is bumped by the server on AUTH-4 leave/revocation and invalidates outstanding bundles. Any on-disk record type number or wire protocol version this needs MUST be reserved via POST /api/v1/projects/agent-bus/reservations -- never hand-picked. NOT NEEDED under this rescope (drop if present in any earlier draft): signed prekeys, one-time prekeys, prekey replenishment/exhaustion policy -- those were X3DH-specific and there is no X3DH; this bundle carries exactly one long-lived signing public key per agent.
  _Proof: go test -race -run TestKeyBundle ./internal/httpapi_
- [ ] CRYPTO-6 · CRYPTO-6: Double Ratchet encrypt/decrypt on the direct-message send path — crypto, P3, deferred
  GATED: do not start until CRYPTO-1 (design spike) is done and its DECISIONS.md entry is recorded -- the spike chooses the crypto library/primitives, the audit-log trade-off, the ratchet-state durability model, the broadcast/relay scheme and the key-trust model. Implementing before that decision exists is guessing. Wire the Double Ratchet into the DM path (MSG-3): sender seals, recipient opens, with a DH ratchet step on each direction change and a symmetric-key ratchet per message, giving forward secrecy and per-message integrity/authenticity. Must handle out-of-order and skipped messages by retaining skipped message keys, WITH AN EXPLICIT BOUND on how many are retained (an unbounded skipped-key store is a memory-exhaustion DoS an attacker triggers by claiming a huge counter jump). The message header must carry what the ratchet needs (ratchet public key, previous chain length, message number) and NOTHING the bus should not see. In-memory ratchet state only in this task -- persistence, fsync and recovery are CRYPTO-7, and that ordering is deliberate so the protocol is proven correct before the durability problem is layered on. Tests: long conversation both directions, delayed message delivered after a ratchet step, duplicate/replayed ciphertext rejected, tampered ciphertext/header rejected, decrypt with the wrong session rejected.
  _Proof: go test -race -run TestRatchet ./internal/crypto && go test -race -run TestSendEncrypted ./internal/httpapi_
- [ ] CRYPTO-8 · CRYPTO-8: Broadcast to N agents -- authenticated encryption for the fan-out path — crypto, P3, deferred
  GATED: do not start until CRYPTO-1 (design spike) is done and its DECISIONS.md entry is recorded -- the spike chooses the crypto library/primitives, the audit-log trade-off, the ratchet-state durability model, the broadcast/relay scheme and the key-trust model. Implementing before that decision exists is guessing. The Double Ratchet is strictly PAIRWISE; agent-bus broadcasts to N agents (MSG-2). Implement whichever scheme CRYPTO-1 chose: pairwise fan-out (N ciphertexts, one per recipient session -- keeps full ratchet PFS, costs N seals and N envelope copies) or a Signal-style SENDER KEY group session (one ciphertext, a distribution message per member -- cheaper, but WEAKER forward secrecy, which is precisely why the choice is the spike's and not the implementer's). Must specify and implement membership change: an agent joining or leaving (AUTH-4) forces a rekey, and a departed agent must not be able to read subsequent broadcasts. Document in the task outcome what the bus sees for a broadcast (recipient set, sizes, timing) versus what it cannot. Tests: every recipient decrypts the same plaintext, a non-member cannot, a removed member cannot read post-removal traffic, and a tampered broadcast is rejected by every recipient.
  _Proof: go test -race -run TestBroadcastEncrypted ./internal/httpapi ./internal/crypto_
- [ ] CRYPTO-10 · CRYPTO-10: `agent-bus verify` helper + scripts/bus-*.sh validate-before-accept + AGENT_PROTOCOL.md — agentif, P1
  RESCOPED 2026-08-02 per user instruction ("keep it simple, standard sign/verify; encryption later"): this is now VERIFY-ONLY, no decryption. GOVERNED BY INVARIANT 9 -- calls crypto/ed25519.Verify (stdlib, audited, high-level, misuse-resistant) and nothing else; no custom verification logic. THIS IS THE TASK THE USER ACTUALLY ASKED FOR: "a mechanism to validate messages in the agent script before accepting them". Shell cannot do Ed25519, so add a subcommand to the same Go binary (e.g. `agent-bus verify`) that the wrapper shells out to, and wire it into the receive path of the agent-facing wrappers (bus-wait.sh, and bus-agents/bus-send as applicable -- AGENTIF-6/AGENTIF-5) so a message is VERIFIED (per SIGN-1's canonical format, against the sender's messaging public key from CRYPTO-4's bundle/TOFU pin) BEFORE it is handed to the calling agent. Contract: defined stdin/stdout shape; on ANY verification failure exit non-zero and print NOTHING to stdout, so a naive `msg=$(...)` cannot accidentally pass unverified content through. Distinct exit codes per failure mode, at minimum: bad signature (tampered or wrong key), unknown sender (no key binding), replayed message (SIGN-4's cursor), sender identity key CHANGED since pinned (CRYPTO-4's TOFU alarm -- must be loud, never silent), and bundle attestation invalid (bus signature failed). Define where the agent's private key lives and with what file permissions, and refuse to run on world-readable key files. Per invariant 7 the wrapper AND its AGENT_PROTOCOL.md entry ship IN THIS SAME TASK -- a feature without its wrapper is not done. Verify the way an agent would: through scripts/bus-*.sh against a running throwaway bus (own data dir under /tmp), not hand-written curl. NOT NEEDED under this rescope (drop if present in any earlier draft): decrypt/AEAD-open, X3DH session state (no session/handshake required -- verification is stateless given the sender's pinned public key), out-of-order/skipped-key ratchet handling.
  
  ACCEPTANCE CRITERION ADDED 2026-08-02 (RATCHET-7 fallout, verified first-hand by backlog-triage by reading this box's own stdlib source at crypto/ed25519/ed25519.go under GOROOT): ed25519.Verify PANICS (does not return false) when len(publicKey) != ed25519.PublicKeySize -- a remote DoS trap, asymmetric with malformed-signature handling (a bad signature safely returns false, a malformed key does not). This is directly relevant here because CRYPTO-10 verifies attacker-influenceable contact-list/sender public keys, including keys loaded from the roster ON DISK after a restart -- that reload path is also untrusted input and needs the same guard. REQUIRED: the `agent-bus verify` subcommand and its wrapper must length-check every public key against ed25519.PublicKeySize BEFORE calling ed25519.Verify, failing closed (non-zero exit, empty stdout, per this task's existing fail-closed contract) rather than panicking/crashing the process on a malformed key. REQUIRED TEST: a negative test feeding a wrong-size public key and a nil/empty public key (both a freshly-received one and one reloaded from the on-disk roster) through the verify path, asserting a clean non-zero-exit rejection, never a panic. See also the standalone cross-cutting task filed to track this trap across all Verify call sites (AUTH-1, CRYPTO-10, SIGN-2).
  _Proof: scripts/bus-wait.sh against a running throwaway bus rejects a tampered message with non-zero exit and empty stdout_
- [ ] CRYPTO-5 · CRYPTO-5: X3DH session establishment between two agents — crypto, P3, deferred
  GATED: do not start until CRYPTO-1 (design spike) is done and its DECISIONS.md entry is recorded -- the spike chooses the crypto library/primitives, the audit-log trade-off, the ratchet-state durability model, the broadcast/relay scheme and the key-trust model. Implementing before that decision exists is guessing. Implement the X3DH (extended triple Diffie-Hellman) key agreement over CRYPTO-2's primitives, using the bundles from CRYPTO-4, producing the shared secret + associated data that seeds a Double Ratchet session. Pure protocol logic against an in-memory key store -- no HTTP wiring, no message send path, no persistence (CRYPTO-6 and CRYPTO-7 carry those). Must include: the initiator and responder halves, verification of the signed prekey against the peer's identity key, the associated-data binding that ties the session to both parties' fully-qualified ids (so a session can never be transplanted onto a different identity), and a test proving two independently-run halves derive the SAME root key. Table-driven negative cases: tampered prekey signature, mismatched ids in the associated data, replayed one-time prekey, missing one-time prekey (the degraded path CRYPTO-4 defines).
  _Proof: go test -race -run TestX3DH ./internal/crypto_
- [ ] CRYPTO-3 · CRYPTO-3: Enrolment mints and registers the second (messaging) keypair, bound to the server-minted id — auth, P1
  RESCOPED 2026-08-02 per user instruction ("keep it simple, standard sign/verify via libsodium-equivalent; encryption later"): this messaging keypair is now a SIGNING identity, not an E2E-encryption identity. Algorithm: Ed25519 (crypto/ed25519, Go stdlib since 1.13 -- no toolchain bump needed; invariant 9 -- audited high-level Sign/Verify API, we implement no primitive). Extend enrolment (AUTH-1) so an agent ends up with TWO keypairs: the AUTH keypair (existing -- presented at enrolment, server-signed, used for the bearer credential on every route) and a MESSAGING (signing) identity keypair used only to sign/verify message bodies (SIGN-1/SIGN-2). The agent generates the messaging keypair LOCALLY and registers the public half at enrolment -- the private key never leaves the agent, never reaches the bus. This separation is the whole security value of the SIGN epic, stated explicitly: the bus verifies AUTH keys, but a message signature is verified by the RECIPIENT, so a compromised or malicious bus cannot forge a message from agent A. The server MUST bind the messaging public key to the fully-qualified server-minted <bus-id>.<agent-id> (invariants 1 and 2) -- a client-asserted identity is input to validate, never an identity to trust. Persist the binding in the roster (AUTH-3) so it survives recovery. Cover: re-enrolment with a different messaging key, enrolment with a malformed/wrong-length key (Ed25519 public keys are a fixed 32 bytes -- reject anything else), and an attempt to register a key against someone else's id. Do NOT ship the key-distribution endpoint here -- that is CRYPTO-4. NOT NEEDED under this rescope (drop if present in any earlier draft): X25519 DH keys, signed prekeys, one-time prekeys -- those were X3DH-specific and there is no X3DH.
  _Proof: go test -race -run TestEnrolMessagingKey ./internal/auth ./internal/httpapi_
- [ ] CRYPTO-12 · CRYPTO-12: PROTOCOL.md wire format + CONTRACTS.md for the crypto surface — docs, P3
  RESCOPED 2026-08-02 per user instruction ("keep it simple, standard sign/verify; encryption later"): documents the SIGN surface, not a ratchet/encryption surface. Documentation close-out for the epic, run after the SIGN-1..5 and rescoped CRYPTO-3/4/10/11 tasks land. PROTOCOL.md (DOCS-2): the signed-envelope wire format -- SIGN-1's exact canonical byte layout, where the detached Ed25519 signature travels in the envelope, the key-bundle format (CRYPTO-4), the replay-cursor mechanism (SIGN-4), and the wire protocol version this lands under (RESERVED via POST /reservations, never hand-picked). State PLAINLY in PROTOCOL.md that message bodies are NOT encrypted -- any relaying/intermediate bus and any party with WAL/disk access can read every message body; this is a deliberate, user-approved property, not an oversight, and readers must not assume confidentiality. CONTRACTS.md (DOCS-3): every new route (key bundle fetch), every new flag/env var (key file paths), every new record type, and the `agent-bus verify`/`agent-bus sign` subcommands' exit codes. AGENT_PROTOCOL.md ships with CRYPTO-10, not here -- do not duplicate it. Fold the relevant CRYPTO_DEEPDIVE.md background (why Signal's own library isn't usable, why sign-only was the pragmatic choice) into the standing docs, clearly marked as historical context for a decision that was ultimately overridden by direct user instruction, not as the shipped design. NOT NEEDED under this rescope (drop if present in any earlier draft): ratchet public key / previous-chain-length / message-number header fields, AEAD layout, on-disk ratchet-state format -- none of that exists.
  _Proof: grep -qi 'Ed25519' PROTOCOL.md && grep -qi 'not encrypted' PROTOCOL.md && grep -q 'agent-bus verify' CONTRACTS-CLI.md_
- [ ] CRYPTO-7 · CRYPTO-7: Ratchet-state durability and recovery (CRASH-INJECTION TEST REQUIRED) — durability, P3, deferred
  GATED: do not start until CRYPTO-1 (design spike) is done and its DECISIONS.md entry is recorded -- the spike chooses the crypto library/primitives, the audit-log trade-off, the ratchet-state durability model, the broadcast/relay scheme and the key-trust model. Implementing before that decision exists is guessing. THE HARD ONE. Double Ratchet state is MUTABLE per-session state that advances with every message; the store is append-only and recovery REPLAYS it (invariants 5 and 6). If ratchet state is lost the session breaks; if it is REPLAYED or ROLLED BACK on recovery you get key and NONCE REUSE, which is a catastrophic AEAD failure -- two ciphertexts under one key/nonce leaks plaintext. Implement exactly the persistence model CRYPTO-1 specified: how state is encoded, how the state advance is committed and fsynced RELATIVE to the two-phase message commit (DUR-2), and how replay reconstructs the ratchet without re-advancing or rewinding it. Per CLAUDE.md, a durability claim needs a CRASH-INJECTION TEST, not code review: write, kill at each chosen point in the write path, and assert what recovery yields. At minimum prove: (a) no key/nonce is EVER reused across a crash, at any injection point; (b) an acknowledged message is decryptable after recovery; (c) a message killed before commit leaves neither a half-advanced ratchet nor an acked-but-lost message; (d) the recovered state is a PREFIX of the accepted history. Also bound and persist the skipped-message-key store. Any on-disk record type number or wire protocol version this needs MUST be reserved via POST /api/v1/projects/agent-bus/reservations -- never hand-picked. Coordinate with DUR-2/DUR-3/DUR-6 rather than forking a second write path.
  _Proof: go test -race -run TestRatchetStateCrash ./internal/store ./internal/crypto_

### EPIC DEPLOY — Containerised runtime (Docker Compose)

- [ ] DEPLOY-5 · DEPLOY-5: container build/test check (CI or make/script target) — deploy, P2
  A checkable target (a `make docker-build`/`make docker-test` pair, or an equivalent scripts/ci-*.sh, or a CI workflow if this repo gains CI) that builds the Dockerfile (DEPLOY-1) and runs the test suite (or at minimum `go build`/`go vet`/`gofmt -l`/`go test -race`) INSIDE the container, so "it builds in the container" is something anyone can run and verify rather than an assumption carried in a PR description. Should also smoke-test docker-compose.yml (DEPLOY-2): bring the stack up, hit /healthz, bring it down, assert clean exit and volume persistence across a restart. Depends on DEPLOY-1 and DEPLOY-2; benefits from running after DEPLOY-4 (toolchain bump) so the checked build matches the shipped toolchain, but can be written against whatever go.mod pins at the time and re-verified after DEPLOY-4 lands.
  
  CONSTRAINT ADDED 2026-08-02 (RATCHET-7 fallout): this task must name `govulncheck` as an explicit build/test step (run against the module, gating the build/test target this task defines) plus a base-image vulnerability scan (trivy or grype) of the built container image -- both as concrete, runnable steps in the make/script/CI target this task delivers, not aspirational text. `govulncheck` currently appears NOWHERE in the backlog outside RATCHET-7's own decision text, so the Ed25519 supply-chain decision (stdlib crypto/ed25519 over the unmaintained cgo libsodium bindings) has no implementation home for its ongoing advisory-monitoring mechanism until this task provides one. Also record and carry forward the residual risk RATCHET-7 logged: this box's go1.19 toolchain is out of upstream support, so a stdlib CVE (including a future crypto/ed25519 one) would NOT be backported to it -- the mitigation is DEPLOY-4 (toolchain pin/bump), and this task (DEPLOY-5) is what surfaces that gap via govulncheck rather than leaving it unknown.
  _Proof: make docker-build docker-test  # (or equivalent script) exits 0_
- [ ] DEPLOY-4 · DEPLOY-4: Go toolchain pin -- go.mod + builder image (no longer crypto-gated) — deploy, P2
  RESCOPED 2026-08-02 per user instruction ("keep it simple, standard sign/verify; encryption later"): the crypto urgency that used to gate this task is GONE. crypto/ed25519 (the SIGN epic's chosen primitive) has been in Go's stdlib since 1.13 and works fine on this box's ambient go1.19.4 -- NO toolchain bump is required for the SIGN epic's crypto/ecdh, x/crypto, or any other reason. This task is NOT deleted, because the container's builder image still needs to pin an explicit Go version deliberately rather than floating on whatever base image tag is latest at build time (that is a real, independent DEPLOY concern -- reproducible builds -- unrelated to crypto). SEQUENCING (unchanged): do NOT start until the in-flight ID/DUR wave (building against go1.19 right now) is done, or coordinate an explicit go/no-go with spec-keeper. SCOPE NOW: pick and pin a specific go1.19.x (or later, if there is an independent reason to move, e.g. a security fix in the toolchain itself) patch version for both go.mod's go directive and DEPLOY-1's Dockerfile builder-image tag, record the version and reason in DECISIONS.md (a visible, deliberate change, not a side effect of another task), and re-verify `go build ./...`, `go vet ./...`, `gofmt -l .`, `go test -race ./...` green on the pinned version, locally and via a container build (DEPLOY-5). Downgraded from P1 to P2: this is no longer blocking any crypto work.
  _Proof: go build ./... && go vet ./... && test -z "$(gofmt -l .)" && go test -race ./... (on the new pinned toolchain)_
- [ ] DEPLOY-3 · DEPLOY-3: multi-bus Compose profile (2+ peered buses) for RELAY end-to-end testing — deploy, P2
  A second Compose profile/override (e.g. docker-compose.multi.yml or a `relay` profile in the same file) that runs 2+ agent-bus services peered with each other over the RELAY epic's peer-enrolment mechanism (RELAY-1..5), each with its own named volume and healthcheck. This is what makes the RELAY epic testable END-TO-END in a realistic topology (message relay, agent-list exchange, loop prevention via traversed-bus path, peer-down retry/backoff) instead of only via unit tests against in-process buses. Depends on DEPLOY-2 (single-bus compose) and on enough of the RELAY epic existing to peer two buses (coordinate timing with spec-keeper -- do not block indefinitely on RELAY if the compose scaffolding itself can be written first with a status_note saying peering is not yet exercisable). Priority reflects that it pairs with and unblocks RELAY verification, so it should be picked up once RELAY has a peer-enrolment route to point it at.
  _Proof: docker compose -f docker-compose.multi.yml up -d && <peer-enrol two buses via scripts/bus-peer.sh, exchange a message, verify relay> && docker compose -f docker-compose.multi.yml down_
- [ ] None · DEPLOY-REDEPLOY: recreate the Compose bus fresh (volume included) and prove two agents exchange a message on it — deploy, P1
  The existing `agentbus` Compose project predates messaging/signing/the durable roster and cannot carry a message. The on-disk store.RecordVersion 1->2 break makes its volume unusable and its contents are throwaway, so `down -v` is USER-AUTHORISED (ruling recorded in DECISIONS.md at commit fe02ebb). docker is NOT usable via bare `docker` on PATH -- that resolves to a broken snap wrapper. Use /snap/docker/current/bin/docker for the docker CLI and the compose plugin at /snap/docker/3505/usr/libexec/docker/cli-plugins/docker-compose, with DOCKER_HOST set appropriately. Steps: `docker compose down -v` (via the paths above) to fully clear the old project and volume, then `docker compose up -d --build` to recreate fresh. Constraint: docker-compose.yml declares no `ports:` -- the server binds 127.0.0.1:8080 INSIDE the container only. Do NOT widen the listener / do NOT add a ports: mapping to satisfy this task. busctl is not present in the built image (see CLI-BUSCTL-IMAGE, public_id 9be2105d, status todo), so proof requires `docker cp`-ing a statically built client binary into the running container and invoking it there (or via `docker exec`) against the container-local listener. Acceptance criterion is VERIFICATION BY EXECUTION, not container health: the container being healthy/Up is NOT sufficient. The task is only done once two distinct agents actually enrol against the freshly recreated bus and exchange at least one message, with the transcript/output captured as proof.
### EPIC DOCS — Documentation

- [ ] None · Journal catch-up: DECISIONS.md + AGENT_LOG.md entries owed by INVITE-MINT and MTLS-ROTATE — docs, P2
  Both DECISIONS.md and AGENT_LOG.md were HOT during the 2026-08-07 triage pass -- three uncommitted appended sections plus a prior integrator/append race were already sitting in the working tree -- so the feature agents dispatched against INVITE-MINT (public_id 1d0d0e60-06d3-4610-aa09-1439159a114d) and MTLS-ROTATE (public_id c2e8df5b-cafa-4a38-8384-a99e7f66f968) were EXPLICITLY forbidden from editing either file this pass, and told instead to post their journal text as kind=report notes on their own tasks.
  
  This task exists so that deviation from CLAUDE.md step 8 ("Record decisions in DECISIONS.md; append to AGENT_LOG.md") is tracked and paid down deliberately, rather than quietly lost the moment both tasks are marked done.
  
  WHEN DECISIONS.md and AGENT_LOG.md are next safe to edit (i.e. not concurrently held by another in-flight agent), copy the design-decision content from INVITE-MINT's task notes into a new dated DECISIONS.md section, and the work-log content from both INVITE-MINT's and MTLS-ROTATE's task notes into new dated AGENT_LOG.md entries -- append-only, do not edit existing dated history in either file.
  
  SOURCE TASK IDS TO COPY FROM:
    - INVITE-MINT: public_id 1d0d0e60-06d3-4610-aa09-1439159a114d (GET .../tasks/1d0d0e60.../notes)
    - MTLS-ROTATE: public_id c2e8df5b-cafa-4a38-8384-a99e7f66f968 (GET .../tasks/c2e8df5b.../notes)
  _Proof: grep -q 'INVITE-MINT' DECISIONS.md && grep -q 'MTLS-ROTATE' AGENT_LOG.md && grep -q 'INVITE-MINT' AGENT_LOG.md_
- [ ] DOCS-2 · DOCS-2: PROTOCOL.md -- wire protocol + on-disk format — docs, P0
  RAISED P1 -> P0 2026-08-02. **PROTOCOL.md DOES NOT EXIST.** Verified first-hand this pass: CLAUDE.md's
  repository-layout section lists it as a tracked contract document ("PROTOCOL.md -- the wire protocol +
  on-disk format") and there is no such file in the repo. THREE OTHER TASKS ARE WRITTEN AS THOUGH IT
  DOES and grep it in their proof commands -- DUR-4-FU-DOCS (0b6d5c11, now P0), the unknown-record-type
  docs task (804fa84c), and CLI/CONTRACTS work -- so its absence is now BLOCKING, not merely a gap.
  This task OWNS CREATING THE FILE; those tasks own sections within it.
  
  MANDATED CONTENT ADDED BY THE 2026-08-02 DECISIONS -- the user's decision text says these MUST be
  stated in PROTOCOL.md, so they are not discretionary:
   - **AT-LEAST-ONCE DELIVERY.** "Duplicates are the normal steady state, which is what invariant 10's
     idempotency exists to absorb. Must be stated in PROTOCOL.md and AGENT_PROTOCOL.md."
   - **THE NARROWED INVARIANT 4.** Acknowledged data may be discarded when found corrupt: "The
     narrowing is deliberate and must be stated in PROTOCOL.md, not left implicit." Likewise the
     narrowed invariant 6 (truncation no longer restricted to a verified-corrupt tail).
   - **THE AUDIT LOG IS METADATA AND ROUTING INFO ONLY** -- id, sequence, sender, recipients, bus path,
     timestamp, size, content hash. NEVER bodies. A deliberate 2026-08-02 decision so the trail stays
     compatible with E2E-encrypted, forward-secret payloads.
   - Retention: 1 day or 1 GB, whichever comes first. Default listen address: localhost.
   - Sessions: server-provided token, client-signed, <=1h, opaque handle, DO NOT survive restart;
     revocation via /leave is IMMEDIATE.
   - On-disk format: FormatVersion 1 today; ondisk-format-version=2 is RESERVED for DUR-12's
     CRC32C -> HMAC-SHA256 change. Say what is current and what is reserved.
  
  PROOF. `test -s PROTOCOL.md && grep -q 'at-least-once' PROTOCOL.md && grep -q 'metadata' PROTOCOL.md`
  -- FAILS TODAY at clause 1 (the file does not exist), correctly and non-vacuously. The previous
  proof (`test -s PROTOCOL.md`) was fine but did not pin the two mandated statements.
  
  --- ORIGINAL DESCRIPTION ---
  Every HTTP route (method, path, auth requirement, request/response shape) and the on-disk format (WAL record framing, audit log format, roster/counter file layouts) -- maintainer-facing, kept current as routes land.
  _Proof: test -s PROTOCOL.md && grep -q 'at-least-once' PROTOCOL.md && grep -q 'metadata' PROTOCOL.md_
- [ ] None · CONTRACTS-AGENT.md/AGENT_PROTOCOL.md document the removed log-scrape as bus-serve.sh start's contract — documentation, P1
  CONTRACTS-AGENT.md:43-47 currently says bus-serve.sh start prints the certificate fingerprint scraped from the log (bus_cert_fingerprint=...). As of the fix in parent task 10e93262-8e34-4738-b435-bfe23d880057, that is false and it documents the VULNERABLE behaviour as current: cert_fingerprint() now computes sha256(DER) from $CERT_FILE and never reads $LOG_FILE. Replace with this exact wording, supplied by the documentation gate:
  
  "- On success, start additionally prints: the https://host:port URL, the certificate path, the certificate fingerprint computed directly from $CERT_FILE (sha256(DER) of the leaf, via cert_fingerprint(): openssl x509 -noout -fingerprint -sha256, falling back to awk+base64 -d+sha256sum when openssl is absent), and a ready-to-paste agent-busctl enrol --bus ... --bus-fingerprint ... --name <name> line. It is never scraped from $LOG_FILE (fixed 2026-08-07: the log is a mutable artefact under AGENT_BUS_RUN_DIR, default /tmp/agent-bus, so a log-scrape let a local attacker plant a fake bus_cert_fingerprint=... line and win the wrapper's own tail -1, handing the operator a confident, paste-ready fingerprint naming the attacker's certificate). If neither openssl nor the coreutils fallback is available, start still exits 0 (the bus IS up) but prints no fingerprint line -- instead a WARNING to stderr naming the remedy."
  
  Also in the same task: CONTRACTS-AGENT.md:48-50 and AGENT_PROTOCOL.md:65-67 should note the new pidfile refusal (exit codes are UNCHANGED: start 0/1/2, status 0/1/3, stop 0/2 -- an unusable pidfile is treated as not running). Files: CONTRACTS-AGENT.md, AGENT_PROTOCOL.md. Note both had uncommitted work from another agent at the time, which is why parent task 10e93262-8e34-4738-b435-bfe23d880057 could not touch them.
  _Proof: grep -n 'never scraped from' CONTRACTS-AGENT.md && ! grep -n 'scraped from the log' CONTRACTS-AGENT.md_
- [ ] None · DUR-11-FU-CONTRACTS: CONTRACTS.md still documents the reverted refuse-to-start WAL policy verbatim — documentation, P1
  Discovered during the DUR-11 orphaned-task reconciliation pass (2026-08-02). HALF OF THIS TASK IS ALREADY DONE: c7e017d removed the stale "provably torn tail" / "refuses to start and leaves the file byte-for-byte" / "RepairTail" phrasing (verified absent from CONTRACTS-ONDISK.md, the plane the WAL-repair section moved to in the CONTRACTS split, 360a2679). THE REMAINING, UNMET HALF: CONTRACTS-ONDISK.md has ZERO mention of RepairLog, the bus.wal.corrupt-<ts> quarantine-rename-aside artefact name, the .repair temp-file-during-rewrite artefact name, or the Repair/Recovered struct fields actually surfaced to callers (Rewritten, Quarantined, DiscardCount, MissingRecords, Exhausted) -- confirmed via grep, zero matches for every one of the eight terms (2026-08-02). Fix: document the SHIPPED RepairLog / quarantine / always-restart behaviour in CONTRACTS-ONDISK.md, naming the on-disk artefacts and enumerating the struct fields.
  
  *** BLOCKING: DO NOT DISPATCH until DUR-12 (cbc9ab0c) lands. *** DUR-12 is rewriting the on-disk WAL format (CRC32C -> HMAC-SHA256 MAC, format version 2) right now and will change this exact plane -- documenting the WAL surface concurrently would be stale on arrival, same ordering constraint applied to e120153b and db350e39.
  _Proof: grep -qF "RepairLog" CONTRACTS-ONDISK.md && grep -qF "bus.wal.corrupt-" CONTRACTS-ONDISK.md && grep -qF ".repair" CONTRACTS-ONDISK.md && grep -qF "Rewritten" CONTRACTS-ONDISK.md && grep -qF "Quarantined" CONTRACTS-ONDISK.md && grep -qF "DiscardCount" CONTRACTS-ONDISK.md && grep -qF "MissingRecords" CONTRACTS-ONDISK.md && grep -qF "Exhausted" CONTRACTS-ONDISK.md && ! grep -qE "provably torn tail|refuses to start and leaves the file byte-for-byte|RepairTail" CONTRACTS-ONDISK.md_
- [ ] None · AGENT_PROTOCOL.md error-block label says remedy: but the CLI prints try: — docs, P3
  AGENT_PROTOCOL.md's error-block examples label the second line "remedy:" (e.g. lines ~203 and ~208, the mTLS certificate-mismatch examples), but cmd/agent-busctl/output.go:148 actually prints `  try: %s` via fmt.Fprintf(o.stderr, "  try: %s\n", payload.Remedy). The `agent-busctl: %s` prefix on the first line (output.go:146) IS correct and matches the doc. This is PRE-EXISTING -- present at 29cdafc and earlier, not introduced by MTLS-ROTATE's doc work, though that work carried the mislabeled examples forward so it now appears more than once (two blocks). Consequence: an agent grepping AGENT_PROTOCOL.md for "remedy:" to programmatically extract a remedy line will never find one, because the CLI never emits that word -- only "try:". Fix: either update the doc examples to say "  try:" (matching code, the simpler and safer fix since output.go is otherwise correct and already has one legitimate "try:" example elsewhere in the doc at line ~648), or rename payload.Remedy's label in code to match the doc -- doc-only fix is preferred absent a reason to touch the CLI. Verified independently: git show HEAD:cmd/agent-busctl/output.go lines 146/148, and grep -n remedy: AGENT_PROTOCOL.md returns lines 203 and 208.
  _Proof: grep -qF '"  try: %s\n"' cmd/agent-busctl/output.go && grep -A2 -F 'REFUSING to talk' AGENT_PROTOCOL.md | grep -qF '  try:'_
- [ ] None · DECISIONS.md carries the pre-correction (wrong) accepted-limit sentence for the MAC key; PROTOCOL.md has the fix, DECISIONS.md does not — documentation, P2
  The DUR-12 security gate established that the original accepted-limit rationale for the HMAC key
  (section 1 of the 2026-08-02 "Five decisions" entry) is factually wrong as written, and corrected it
  IN PROTOCOL.md -- but DECISIONS.md, which is outside DUR-12's task boundary, still carries the
  original wrong sentence uncorrected.
  
  WRONG (DECISIONS.md line ~1089-1090, section 1 "The HMAC key lives in the DATA DIR..."):
    "It buys nothing against an attacker who already has data-directory write access; such an attacker
    can read the key and forge freely."
  
  This is wrong because replacing a file on POSIX needs only w+x on the DIRECTORY, not any permission
  on the 0600 key file inside it -- so "can read the key" overstates what the attacker needs and
  understates what they can actually do.
  
  CORRECTED (PROTOCOL.md lines ~248-255, section 7):
    "But the reason usually given for that is wrong, and the difference matters. The stock
    justification -- 'such an attacker can read wal-mac.key, same directory, same trust boundary' -- is
    a statement about READ access, and reading is not what the attacker needs. Replacing a file on
    POSIX needs only w+x on the directory, not any permission on the 0600 key inside it. The accurate
    statement is that an attacker with directory-write access can replace the key and the log together
    -- plant a v2 log of their own making alongside a key of their own choosing -- and the bus will
    replay it as history. That is why the limit is real; it does not depend on the key being readable."
  
  FIX SCOPE: DECISIONS.md is append-only by convention (see its own section 4, "Commit history: LEAVE
  IT" -- the pattern established for this file is record corrections, don't rewrite history in place).
  So the fix here is almost certainly a DATED CORRECTION APPENDED at the end of the file (new section,
  today's date), quoting the wrong original sentence, stating it is superseded, and pointing at
  PROTOCOL.md section 7 for the accurate wording -- NOT an in-place edit of the 2026-08-02 "Five
  decisions" entry's original text. Whoever takes this should confirm that convention still holds
  before editing, since the append-only rule is a repo convention rather than something this task
  verified is universal.
  
  Not urgent -- this is a documentation-only inconsistency; the operationally-authoritative statement
  (PROTOCOL.md, which operators actually read for the accepted-limit boundary) is already correct.
  DECISIONS.md is history/rationale, not the operator-facing doc, which is why this is P2 not higher.
  _Proof: grep -qi 'replace the key and the log' DECISIONS.md_
- [ ] None · DECISIONS.md:1302 cites a superseded bus-serve.sh line for the plaintext-probe follow-on — documentation, P3
  DECISIONS.md:1302 names scripts/bus-serve.sh:54 and HEALTH_URL="http://..." as follow-on work; the plaintext probe was removed by MTLS-LISTENER/MTLS-VERIFY and the line number has moved. DECISIONS.md is append-only so the historical entry must NOT be rewritten -- the right fix is a new dated entry noting the supersession. Low priority, flagging only. Found during review of parent task 10e93262-8e34-4738-b435-bfe23d880057.
  _Proof: grep -n 'supersed' DECISIONS.md | tail -5_
- [ ] DOCS-3 · DOCS-3: CONTRACTS.md -- route/flag/env-var/record-type table — docs, P1
  A single table of every route, CLI flag, env var, and durable record type, with the convention that every future task updates it in the same commit that changes any of those surfaces (CLAUDE.md step 9).
  _Proof: test -s CONTRACTS-CLI.md && test -s CONTRACTS-HTTP.md && test -s CONTRACTS-ONDISK.md && test -s CONTRACTS-AGENT.md && grep -qF "CONTRACTS-CLI.md" CONTRACTS.md && grep -qF "CONTRACTS-HTTP.md" CONTRACTS.md && grep -qF "CONTRACTS-ONDISK.md" CONTRACTS.md && grep -qF "CONTRACTS-AGENT.md" CONTRACTS.md_
- [ ] None · PROTOCOL.md §8 cites Spec Server task id INVITE-PEERGUARD (f5d91dbe) as if it were a commit sha -- same defect DOCS-2 just fixed for e120153b/db350e39 — docs, P2
  PROTOCOL.md §8 cites `f5d91dbe` three times (lines ~590, 694, 845) in git-sha backtick styling, alongside MTLS-RELAYGUARD (`8192c3c7`). It does not resolve: `git show f5d91dbe` reports "unknown revision or path not in the working tree". f5d91dbe is the Spec Server public_id prefix for the task INVITE-PEERGUARD, formatted identically to the real landing commit shas used elsewhere in the same document.
  
  This is the exact ambiguity `7ddf757` (DOCS-2) just fixed for e120153b/db350e39 elsewhere in this repo -- there the fix was to cite the real landing commit and label task ids explicitly as "Spec Server task" rather than backtick-styling them like a sha. It was pre-existing at HEAD and was correctly ruled out of scope for that fix. Apply the same remedy here: either cite the real commit(s) that will land INVITE-PEERGUARD/MTLS-RELAYGUARD (once they land -- both are currently todo per the backlog) or, until then, relabel f5d91dbe and 8192c3c7 explicitly as Spec Server task ids rather than styling them as commit shas.
  
  Scope: PROTOCOL.md only. No source, no other docs.
  _Proof: ! grep -n "f5d91dbe\|8192c3c7" PROTOCOL.md | grep -v "Spec Server task"_
- [ ] DISCOVERY-DOC-FU-README · DISCOVERY-DOC-FU-README: README.md still documents the old three-field /v1/info body — docs, P2
  Found by the reviewer gate during DISCOVERY-DOC. README.md (around line 100) still shows GET /v1/info returning only {bus_id, version, uptime_seconds}. It now also returns discovery: /v1/discovery, a constant path pointing at the new unauthenticated protocol-discovery document. README.md was outside DISCOVERY-DOC's file-ownership boundary so it was flagged rather than edited. Fix the README body and, while there, consider whether README should mention /v1/discovery as the bootstrap entry point for an agent handed only a URL.
- [ ] None · IDEM-11-FU-PAPERTRAIL: DECISIONS.md and CONTRACTS-HTTP.md state the OPPOSITE of what IDEM-11 shipped — docs, P1
  Reviewer gate finding on IDEM-11 (staged, uncommitted), 2026-08-03. Raised as a P1 in the PAPER TRAIL, not in the code -- the code is right and the docs contradict it. Deliberately out of the implementing agent's file boundary (DECISIONS.md / CONTRACTS-HTTP.md were single-writer-locked during a 4-agent parallel wave), hence this task.
  
  1. DECISIONS.md:706-708 says idempotency keys "fail closed" and retention is "1 day or 1 GB". Neither is what shipped: retention is a DERIVED 50h10m22s window with a fail-closed COUNT cap of 65536, and an expired key is NOT rejected -- it is applied as a NEW operation.
  
  2. CONTRACTS-HTTP.md:164-176 still documents the message-retention coupling that IDEM-11 deleted.
  
  SUPERSEDING TEXT PROPOSED BY THE IMPLEMENTING AGENT (review before landing, do not paste blind):
  "IDEM-11 supersedes items 8-11 of the 2026-08-02 sixteen-questions decision. Idempotency keys are retained for a DERIVED bounded window (50h10m22s = (24h peer-outage budget + 1h max session + 5m max parked poll + 11s client retry horizon) x 2) with a fail-closed count cap of 65536. A retry arriving after its key expires is NOT rejected -- it is applied as a NEW operation. Fail-closed is unimplementable over opaque client-supplied keys (IDEM-10): an evicted key is byte-indistinguishable from a never-seen key, and every legitimate first attempt is a never-seen key. The honest guarantee is 'duplicates are suppressed within the retention window', never unconditional exactly-once."
  
  That last sentence is the load-bearing one and should survive editing: the system does NOT provide unconditional exactly-once, and any doc implying it is wrong.
  
  Also fold in the operator-facing note: no migration needed (existing logs replay unchanged), rebuild the binary, and see IDEM-11-FU-DOWNGRADE for the downgrade hazard.
- [ ] MTLS-VERIFY-FU-DOCSCHEME · MTLS-VERIFY-FU-DOCSCHEME: README + AGENT_PROTOCOL still tell agents to dial http:// a bus that is now https-only — docs, P0
  MTLS-LISTENER made the bus TLS-only, so a plaintext request gets a bare 400 Bad Request ("Client sent an HTTP request to an HTTPS server.") from net/http and never reaches a route. These files were OUTSIDE the feature-runner's file-ownership boundary and are still wrong: README.md:113-114 (agent-busctl --bus http://127.0.0.1:8080 enrol --name planner and --name builder --keep-current) and AGENT_PROTOCOL.md:266 (agent-busctl enrol --bus http://127.0.0.1:8080 --name planner). AGENT_PROTOCOL.md:252 is worse than an example: it states as fact "today every real bus is http://127.0.0.1:... and no fingerprint is involved", which is now false. PROTOCOL.md:195 says "The listener is still plaintext HTTP". FIX: change each to https:// plus --bus-fingerprint <hex>, and rewrite the two prose claims. RATED P0 BY THE SECURITY GATE, with this reasoning: "A documented command that fails with a transport error is the single most reliable generator of 'just add an insecure flag' in the field, and invariant 11 forbids exactly that flag." Must land before this change is announced to agents.
  _Proof: ! grep -rn "bus http://" README.md AGENT_PROTOCOL.md && ! grep -n "listener is still plaintext" PROTOCOL.md_
- [ ] None · MSG-FU-SUFFIXFLOOR-FU-DOCS: PROTOCOL.md and internal/ids docs still say the suffix wiring is NOT done — docs, P1
  Found by the reviewer gate on MSG-FU-SUFFIXFLOOR (94159d93-fe87-4c3e-b938-86fe7068c787). The startup wiring LANDED: cmd/agent-bus/main.go now constructs ids.OpenNameSuffixes via openSuffixAllocator, seals once, and has no fallback. Several docs still assert the opposite and were OUTSIDE that task's file-ownership boundary, so they ship contradicting the code.
  
  FIX, all of them:
  1. PROTOCOL.md:592-597 -- 'Production wiring - NOT yet done ... cmd/agent-bus/main.go does not call ids.OpenNameSuffixes anywhere today; it still constructs a fresh ids.NewNameSuffixes() on every start, so no agent-suffixes file is written or read by a running bus yet, and the restart re-minting bug this file exists to close is unchanged in production.' EVERY CLAUSE IS NOW FALSE.
  2. internal/ids/doc.go:56-75 -- still says cmd/agent-bus/main.go:327 builds a fresh ids.NewNameSuffixes() on every start.
  3. internal/ids/agentmint.go:296-337 -- NewNameSuffixes' doc justifies being born SEALED by 'a LIVE PRODUCTION CALLER: cmd/agent-bus/main.go builds ids.NewNameSuffixes() on every start'. That caller no longer exists; see the paired -FU-UNSEAL task.
  4. CONTRACTS-HTTP.md:330 quotes the startup WARN verbatim, including the clause 'and agent id suffixes restart from 1 for every name' and the wording that followed it. The WARN was rewritten: the suffix claim was REMOVED from it entirely (it cannot be stated unconditionally -- a data dir whose floors file is lost DOES resume from 1), and the truth now lives in the per-start 'agent-id suffix floors' line, which is INFO / WARN / ERROR depending on the case. Re-quote the current line.
  
  PROOF. grep the four files for the stale claims; go build ./... green.
- [ ] None · Stale CONTRACTS.md pointers after the CONTRACTS-SPLIT: README.md:88, AGENT_PROTOCOL.md:122, CLAUDE.md:332 — documentation, P2
  Discovered by the CONTRACTS-SPLIT agent (360a2679, 2026-08-02) while splitting CONTRACTS.md into per-plane files (CONTRACTS-CLI/HTTP/ONDISK/AGENT.md, with CONTRACTS.md left as an index). That agent flagged but could not fix these -- outside its file-ownership boundary for that pass:
  
  1. README.md:88 -- `- [`CONTRACTS.md`](./CONTRACTS.md) — every route, flag, env var, and record type` still claims CONTRACTS.md directly HOLDS that table. It does not any more; it is now a short index pointing at the four plane files. Fix: reword to describe it as the index, and/or link the plane files directly.
  
  2. AGENT_PROTOCOL.md:122 -- `... see `CONTRACTS.md`, `## Authentication`) ...` cites a specific heading, `## Authentication`, inside CONTRACTS.md. That heading no longer exists there -- it moved verbatim to CONTRACTS-HTTP.md:192 (`## Authentication (added 2026-08-02)`) in the split. Fix: repoint the citation to CONTRACTS-HTTP.md.
  
  3. CLAUDE.md:332 (Parallel-agent coordination section) -- `- For the remaining shared files (`DECISIONS.md`, `AGENT_LOG.md`, `CONTRACTS.md`), only ONE agent at a time; prefer adding a new dated section over editing existing lines.` This is actively MISLEADING post-split: naming CONTRACTS.md alongside DECISIONS.md/AGENT_LOG.md as a single-writer-contended file is exactly the chokepoint the split (360a2679) existed to remove -- three P0s across two triage loops were caused by concurrent agents needing to land a doc update in that one file. Leaving this warning in place would keep agents needlessly serialising on a file that no longer holds the contended content (CONTRACTS.md is now a stable ~36-line index; the actual content lives in CONTRACTS-CLI/HTTP/ONDISK/AGENT.md, each independently editable). Fix: remove CONTRACTS.md from this single-writer list (the plane files still need their own single-writer discipline if a task touches more than one at once, but that is a materially different, narrower risk than the old whole-file chokepoint).
  
  NOTE: CLAUDE.md line ~158 (repository-layout section) and step 9 were ALREADY updated by the split agent to name CONTRACTS.md as INDEX only -- this task is only the three residual pointers above, do not re-touch line 158.
  
  PROOF STRENGTHENED 2026-08-02 (spec-keeper): the original proof_cmd was three negative assertions only, which is satisfiable by DELETING the three stale lines rather than fixing them (the same structural flaw fixed on 5b178dde) -- it now also requires positive evidence that each file points at the correct replacement (README.md cites CONTRACTS-HTTP.md/CONTRACTS-CLI.md/CONTRACTS-ONDISK.md, AGENT_PROTOCOL.md cites CONTRACTS-HTTP.md, and CLAUDE.md's "remaining shared files" bullet now names a CONTRACTS-*.md plane file instead of just dropping CONTRACTS.md from the list).
  _Proof: grep -qF "CONTRACTS-HTTP.md" README.md && grep -qF "CONTRACTS-CLI.md" README.md && grep -qF "CONTRACTS-ONDISK.md" README.md && ! grep -qF ") — every route, flag, env var, and record type" README.md && grep -qF "CONTRACTS-HTTP.md" AGENT_PROTOCOL.md && ! grep -qF "see `CONTRACTS.md`, `## Authentication`" AGENT_PROTOCOL.md && grep -A2 "remaining shared files" CLAUDE.md | grep -qF "CONTRACTS-" && ! grep -qF "For the remaining shared files (`DECISIONS.md`, `AGENT_LOG.md`, `CONTRACTS.md`), only ONE agent at" CLAUDE.md_

### EPIC DUR — Durability: WAL, two-phase commit, recovery, audit log

- [ ] None · DUR-4-FU-DOCS: state invariants 4/6 as explicit NARROWINGS + document the WAL recovery API surface (RepairTail/TailRepair) in PROTOCOL.md/CONTRACTS-ONDISK.md -- at-least-once clause already satisfied by DOCS-2 — docs, P0
  NARROWED 2026-08-07 (spec-keeper triage). Verified against HEAD: `7ddf757` (DOCS-2) added the literal "at-least-once" phrasing to PROTOCOL.md (line ~891, "Delivery is at-least-once, never exactly-once"), which satisfies clause (2) of the original description below. Do NOT re-add that obligation.
  
  STILL OUTSTANDING -- verified RED at HEAD 2026-08-07, both by direct grep:
  
  (1) THE NARROWED INVARIANTS, EXPLICITLY STATED AS NARROWINGS. PROTOCOL.md §6 substantively documents the always-restart/damage-never-fatal policy in detail (the "DAMAGE IS NEVER FATAL" callout and its table), but nowhere ties that back to invariant 4 or invariant 6 by name as a NARROWING -- `grep -in "invariant 4" PROTOCOL.md` finds only the unrelated normal-write-path mention at line ~492, and `grep -in "narrow" PROTOCOL.md` finds three hits, none about invariants 4/6. The 2026-08-02 decision text says this "must be stated in PROTOCOL.md, not left implicit" -- content-adjacent is not the same as stated. Add an explicit passage: invariant 4 is narrowed (acknowledged data is not lost through our OWN write path, but is not guaranteed to survive damaged media -- see invariant 6 discard); invariant 6 is narrowed (truncation is no longer restricted to a verified-corrupt TAIL; any damaged record may be discarded, each one logged).
  
  (3) THE WAL RECOVERY API SURFACE. `grep -n "RepairTail\|TailRepair" CONTRACTS-ONDISK.md` returns NOTHING. CONTRACTS-ONDISK.md still lacks entries for `RepairTail(path, kind, logger)`, the `TailRepair{Path,Truncated,At,Removed,NextIndex,Reason}` struct, and `Recovered.Repaired`. This is unchanged from the original filing.
  
  RELATED, DO NOT DUPLICATE: 804fa84c (P1) covers unknown-record-type startup behaviour; bd3cc650 (P2) covers the stale CONTRACTS.md:55 record-type list; DOCS-2 (7ddf757, landed) created PROTOCOL.md and added the at-least-once phrasing satisfying clause (2) above.
  
  ORIGINAL FILING (2026-08-02), preserved for context -- clause (2) is now satisfied, clauses (1) and (3) are the current scope:
  
  GROWN 2026-08-02 BY THE USER DECISIONS. This task now carries THREE documentation obligations the
  2026-08-02 decisions create, not just the original RepairTail API surface. Two of them the decision
  text says explicitly must be documented -- so they are not optional.
  
  NOTE FIRST: **PROTOCOL.md DOES NOT EXIST.** Verified 2026-08-02 -- CLAUDE.md's repository layout lists
  it as a tracked contract document ("PROTOCOL.md — the wire protocol + on-disk format") and there is no
  such file in the repo. Three separate tasks (this one, 804fa84c, and DOCS-2) are written as though it
  does. DOCS-2 owns CREATING it; this task owns the RECOVERY section within it. If DOCS-2 has not landed
  when this is picked up, create the file with only the sections this task owns and let DOCS-2 fill the
  rest -- do not block, and do not write the wire protocol here.
  
  (1) THE NARROWED INVARIANTS -- REQUIRED BY THE DECISION, NOT OPTIONAL.
      Invariant 4 ("nothing is acknowledged before it is durable") is NARROWED: acknowledged data may
      now be DISCARDED when it is found corrupt. The decision says in terms: "The narrowing is
      deliberate and must be stated in PROTOCOL.md, not left implicit." Say it honestly -- we do not
      lose acknowledged data through our OWN write path, but we will not hold the bus hostage to
      damaged media.
      Invariant 6 is NARROWED: truncation is no longer restricted to a verified-corrupt TAIL; damaged
      records anywhere may be discarded, with a log entry each.
      Document the operator-facing consequence: the bus ALWAYS restarts on damage, and every discard is
      logged loudly and specifically. Non-damage errors (permission denied, I/O failure, dirlock held)
      still refuse to start.
  
  (2) AT-LEAST-ONCE DELIVERY -- ALSO REQUIRED BY THE DECISION.
      "Delivery is AT-LEAST-ONCE. Duplicates are the normal steady state, which is what invariant 10's
      idempotency exists to absorb. Must be stated in PROTOCOL.md and AGENT_PROTOCOL.md." So state it in
      BOTH, and state the consequence for an agent author: your handler must be idempotent, and the
      server-minted monotonic sequence plus your cursor -- not the signature -- is what gives freshness.
  
  (3) THE ORIGINAL SCOPE -- the WAL recovery API surface.
      CONTRACTS.md entries for RepairTail(path, kind, logger), the TailRepair{Path,Truncated,At,Removed,
      NextIndex,Reason} struct, and Recovered.Repaired. A PROTOCOL.md section describing WHEN records
      are discarded and what the operator sees.
      WARNING: the ORIGINAL wording of this task said the policy is "a single, provably-incomplete frame
      at EOF -- never more than one cut per start" and that anything else "is a REFUSAL TO START". THAT
      IS THE OLD, REVERSED POLICY. Do not write it. Confirm the FINAL shape against the code after
      DUR-11 lands -- the API has already been rewritten twice (laterRecordInTail -> inspectTail) and
      DUR-11 is rewriting the failure modes right now.
  
  RELATED, DO NOT DUPLICATE: 804fa84c (P1) covers the unknown-record-type startup behaviour, itself
  re-scoped to always-restart; bd3cc650 (P2) covers the stale CONTRACTS.md:55 record-type list; DOCS-2
  owns creating PROTOCOL.md. Read all three first.
  
  SEQUENCING: after DUR-11 lands. Raised to P0 because two of the three obligations are explicit
  "must be stated in PROTOCOL.md" instructions from the user's own decision, and because the currently
  shipped documentation describes a policy the code no longer follows.
  
  PROOF. `test -f PROTOCOL.md && grep -q 'at-least-once' PROTOCOL.md && grep -q 'always restart' PROTOCOL.md && grep -q 'RepairTail' CONTRACTS.md`
  -- FAILS TODAY at the first clause (PROTOCOL.md does not exist), which is correct and non-vacuous.
  _Proof: test -f PROTOCOL.md && grep -qi "invariant 4.*narrow\|narrow.*invariant 4" PROTOCOL.md && grep -qi "invariant 6.*narrow\|narrow.*invariant 6" PROTOCOL.md && grep -q "RepairTail" CONTRACTS-ONDISK.md_
- [ ] None · existedAtOpen() is not a snapshot -- it returns a field persistLocked mutates, so reordering raise() above the guard silently disables the P0 seq-floor refusal — core, P1
  SECURITY/REVIEW GATE FINDING (MEDIUM) against the code shipped under be447589-6583-4d5c-a9d4-ec9d9fef0f1c (217a3c0). A LATENT FAIL-OPEN: correct today, and correct only by accident of statement order.
  
  MECHANISM. `existedAtOpen()` (internal/hub/seqfloorfile.go:285) is a one-liner returning the MUTABLE field `f.existed`, and `persistLocked` SETS `f.existed = true` (seqfloorfile.go:373). The name promises a value captured at open; the code returns a value that changes under it.
  
  WHY IT IS CORRECT TODAY, AND ONLY JUST. Re-verified at HEAD 16da89f by spec-keeper: all three readers precede the first mutation. internal/hub/hub.go:599, :732 and :744 read `existedAtOpen()`; the first `raise()` is at hub.go:745, and `ensureExists()` is later still at hub.go:849. So the guard sees the true open-time value purely because nothing has written yet.
  
  THE CONSEQUENCE, NAMED PRECISELY. Reorder `raise` above the guard at hub.go:732 -- an ordinary-looking refactor -- and the P0 refusal (`ErrSeqFloorUnprovable`, the one that stops a legacy directory with a damaged log from reissuing already-signed sequences) SILENTLY NEVER FIRES. No test that asserts a positive outcome would notice; the guard simply stops being reachable. That is the exact fail-open shape the guard exists to close, and the method name actively misleads the person doing the reordering into thinking it is safe.
  
  MINIMAL FIX (given by the gate). Capture the open-time value into a SEPARATE IMMUTABLE field at construction (e.g. `existedAtConstruction`, set once in `openSeqFloorFile` and never assigned again) and have `existedAtOpen()` return that. Then the guard is order-independent and the name is true. Add a test that calls `raise()`/`ensureExists()` and asserts `existedAtOpen()` is UNCHANGED -- that test is the durable protection, since it goes RED the day someone collapses the two fields back together.
  
  PROOF STATE OBSERVED (spec-keeper, HEAD 16da89f, not assumed): `bash scripts/proof-check.sh '<proof_cmd>'` -> verdict=FAIL, exit 1. RED, and RED FOR THE RIGHT REASON: the negated pin matches the CURRENT one-liner exactly (`grep -c` = 1, so `!` makes it false and the && short-circuits) -- RED today rather than VACUOUS. The test half must ALSO be observed RED before the fix.
  _Proof: ! grep -q 'func (f \*seqFloorFile) existedAtOpen() bool { return f.existed }' internal/hub/seqfloorfile.go && go test -race -count=1 -run TestSeqFloorExistedAtOpenIsASnapshot ./internal/hub_
- [ ] None · DUR-4-FU-DECISIONS: record the SHIPPED damage-class taxonomy in DECISIONS.md -- which classes discard, what each logs, and the exact list of non-damage errors that stay FATAL — durability, P1
  RE-SCOPED 2026-08-02 AFTER VERIFYING WHAT THE NEW DECISIONS.md ENTRY ALREADY COVERS. All THREE of
  this task's original items are now SATISFIED by the 2026-08-02 user decision (DECISIONS.md,
  "Sixteen open questions settled"), checked line by line:
  
   (1) "truncation is permitted ONLY for a provably-incomplete frame at EOF -- a full-length frame that
       fails its own checksum is a FATAL STARTUP ERROR" -- REVERSED, not merely recorded. Section 1
       ("the bus ALWAYS restarts") narrows invariant 6 explicitly: "truncation is no longer restricted
       to a verified-corrupt *tail*. Damaged records anywhere may be discarded -- with a log entry
       each." There is nothing left to record; recording the old policy would be actively harmful.
   (2) "a NUL tail longer than one frame length is refused, not truncated" -- same reversal, subsumed by
       the general discard-and-log rule.
   (3) "the tail-safety proofs are CRC-based, so they do NOT hold against an attacker with write access
       to the data directory" -- RECORDED. Section 3 replaces CRC32C with an HMAC-SHA256 keyed MAC and
       states the residual verbatim: "storing it beside the WAL defends against a remote client but not
       against an attacker who already has data-directory write access."
  
  WHAT IS GENUINELY STILL MISSING, and is now this task's only scope: the decision states the POLICY,
  but not the TAXONOMY the code will actually implement. That has to be written down once, or every
  future maintainer re-derives it from recover.go:
  
   A. Enumerate the DAMAGE CLASSES that trigger discard-and-continue -- torn tail, checksum/MAC failure
      on a complete frame, a length field that overshoots EOF, a NUL run, an unknown record type, a
      corrupt file header, a mid-file damaged frame -- and for EACH say what is discarded (one frame?
      to EOF? the whole file?) and what the log line must contain (offset, record index, record type,
      bytes discarded, reason). "Log loudly and specifically" is the requirement; this is where
      "specifically" gets defined.
   B. Enumerate the NON-DAMAGE errors that STAY FATAL: permission denied, I/O failure, data-directory
      lock already held, missing/unwritable data dir, and (per DUR-12) a missing or wrong MAC key. This
      list is the thing that stops always-restart from degrading into "silently start empty on an
      unreadable disk".
   C. State the honest narrowing of invariants 4 and 6 in the same section, so PROTOCOL.md and
      AGENT_PROTOCOL.md can quote it rather than paraphrase it.
  
  WRITE THIS AFTER DUR-11 LANDS, so it describes the code that actually shipped rather than an interim
  version. DUR-11 is the task doing the discard/log/continue conversion. Append a NEW dated section --
  DECISIONS.md is contended; never edit existing lines.
  
  PROOF. `grep -q 'damage class' DECISIONS.md && grep -q 'stays fatal' DECISIONS.md` -- verdict FAIL
  (class=file-assertion, exit 1) TODAY, which is correct and non-vacuous: it fails precisely because
  the taxonomy is unwritten and flips to PASS when it exists. The written entry must therefore contain
  both phrases literally.
  _Proof: grep -q 'damage class' DECISIONS.md && grep -q 'stays fatal' DECISIONS.md_
- [ ] None · seq-floor guard predicate keys on discard, not on accounting -- boundary-exact truncation escapes it at every record boundary — durability, P1
  MECHANISM: internal/hub/hub.go:702 guards with `!h.seqFloorFile.existedAtOpen() && o.LogRepaired != ""`. `LogRepaired` answers "did recovery PHYSICALLY REMOVE records" -- a proxy for "the log is complete". A truncation landing exactly on a record boundary removes nothing during recovery (replay reads cleanly to EOF, nothing is torn), so LogRepaired == "" and the guard does not fire even though records are missing -- they were removed by the CUT, not by recovery. This is a DIFFERENT bug from the ones already fixed under e120153b/db350e39 (which were about the floor's own bookkeeping); this one is that the REFUSAL PREDICATE reads the wrong signal.
  
  MEASURED (see 9fd58deb's notes for the full test-oracle writeup): on a purpose-built specimen (7 delivered messages, seqs 1,2,3 and 257-260, floor written=22, 8900-byte WAL), two exhaustive byte-by-byte sweeps covering the WHOLE file (1-4439 and 4440-8900, 8900/8900 offsets) found the escape set is EXACTLY the record-boundary set: 22 of 22 record boundaries escape the guard (offsets 360, 427, 738, 805, 937, 1004, 2016, 2083, 3095, 3162, 4172, 4240, 4372, 4440, 5487, 5555, 6602, 6670, 7717, 7785, 8832, 8900). 13 of those 22 reissue a sequence already delivered; the other 9 are harmless only because the derived floor had already stepped past the delivered high-water by that point in THIS specimen's history -- not a property of the bug, so a differently-shaped directory would have more harmful boundaries. The refusal path itself is measured GOOD (8878/8900 = 99.75% loud refusals, ZERO silent) -- this task is about the remaining escape set, not about weakening or removing the existing refusal.
  
  WHY A DISCARD-KEYED PREDICATE CANNOT SEE THIS: a paired measurement showed a boundary-exact cut and a mid-record cut at the SAME offset derive IDENTICAL floors (checked at 1004, 2016, 4240, 4440, 5487, 6602, 7785) -- floor derivation is one monotonic step function, the same on both recovery paths, and its steps land exactly on record boundaries. The two paths cannot disagree about the FLOOR; they only disagree about whether LogRepaired gets set. LogRepaired answers "was something torn", not the question that matters: does the surviving log account for every index durably authorised.
  
  FIX REQUIRED: replace (or supplement) the o.LogRepaired-keyed predicate with one keyed on wal.Recovered's highest-CONSUMED-index field, once 9fd58deb exposes it -- i.e. refuse when the floor file is absent AND the log's highest consumed index is provably below what this run's replay can account for, rather than only when recovery physically discarded something. BLOCKED ON 9fd58deb: the field this predicate needs does not exist yet on wal.Recovered.
  
  SCOPE: internal/hub/hub.go (the predicate at :702) plus the seq-floor guard tests (cmd/agent-bus/seqfloorrestart_test.go, seqfloormissing_test.go, internal/hub/mint_test.go). Do NOT weaken or remove the existing o.LogRepaired refusal path -- it is independently correct and measured good; this task ADDS coverage for the boundary-exact case, it does not replace working coverage.
  
  ORACLE FOR THE FIX: must refuse at all 22 record-boundary offsets on the reference specimen (see 9fd58deb notes for the full list and the reasoning for why 22, not 13, is the right target), not just the 13 that happen to be harmful on that one specimen's history.
  _Proof: go test -race -run TestSeqFloorGuardCatchesBoundaryExactTruncation ./internal/hub_
- [ ] DUR-12-FU-AUDITUPGRADE · DUR-12-FU-AUDITUPGRADE: version 1 audit logs have no upgrade path -- must land before the audit log ships (blocks DUR-5) — durability, P2
  P2, durability. MUST BE DONE BEFORE THE AUDIT LOG SHIPS (blocks DUR-5). Reviewer P2-3: upgradeV1 is reachable only from wal.Open, which is WAL-only, so a version 1 AUDIT log has no upgrade path and OpenWriter (writer.go:67) now refuses it outright. Harmless today (no KindAudit file exists outside tests) and a live landmine the moment the audit log lands.
- [ ] None · Bound the wal-index-floor reserved value the same way as the message-seq floor — security, P1
  SIBLING FINDING to the message-seq-floor brick (see parent task be447589-6583-4d5c-a9d4-ec9d9fef0f1c): An unauthenticated wal-index-floor claiming reserved = 2^64-2 is ACCEPTED and then RE-SIGNED as MaxUint64 under a VALID HMAC -- the same implausible-value shape as the message-seq-floor finding, in internal/wal. The keyed MAC does not help here, because the server re-signs the attackers value itself. The bound must land consistently across both floor files (message-seq-floor in internal/hub and wal-index-floor in internal/wal).
  
  SCOPE: internal/wal only. A DUR-5 agent is live in internal/wal -- coordinate / sequence with that work rather than colliding on the same files. NOT in the parent tasks boundary (parent is cmd/agent-bus + internal/hub only).
  
  Depends on / references the parent task be447589-6583-4d5c-a9d4-ec9d9fef0f1c (data-directory permissions + message-seq floor bound).
  _Proof: bash scripts/proof-check.sh 'go test -race -run TestWALIndexFloorBound ./internal/wal/...'_
- [ ] None · CONTRACTS-ONDISK.md and four sibling Go comments overstate the seq-floor migration guard: it says the window CLOSES, but boundary-exact truncation escapes it — docs, P2
  CONTRACTS-ONDISK.md:326 (message-seq-floor section) currently reads: "hub.Open falls back to sources (1)-(3), logs at WARN when the directory is not otherwise fresh, and -- when the derived floor is non-zero -- CLOSES the window immediately by persisting it." That claim is the same false-all-clear shape already found and fixed in the internal/hub/hub.go WARN log line's own wording (see 9fd58deb's notes and be447589): the guard this sentence describes is keyed on o.LogRepaired (did recovery physically discard something), which is a proxy for log completeness with one hole -- a truncation landing exactly on a record boundary tears nothing, so the guard does not fire even though records are missing. Measured: the escape set is exactly the record-boundary set (22 of 22 record boundaries escape on the reference specimen; see 9fd58deb's notes for the full figures). CONTRACTS-ONDISK.md does not mention this at all; an operator reading it is told the window closes on a successful start with a non-zero derived floor, which is not true at every record-boundary-exact truncation.
  
  FIX: qualify the "CLOSES the window" sentence (and the neighbouring "Migration residual" / "Be precise about when the window actually closes" paragraphs a few lines below, which have the same gap) to say plainly: the guard covers DISCARD-DETECTABLE damage (recovery found something torn) and NOT boundary-exact truncation (a cut that lands exactly on a record boundary, which recovery cannot distinguish from a log that legitimately ended there). Cross-reference the tracking task (9fd58deb, and its blocked follow-up 18eac796-d1fd-4619-94cb-1164bf989634) so a reader knows this is tracked, not merely disclosed once and forgotten.
  
  SCOPE: CONTRACTS-ONDISK.md only (the message-seq-floor section, roughly lines 299-350). Do not touch internal/hub/hub.go -- its comment/WARN text is being handled under a separate, already-in-flight dispatch; this task is the operator-facing PLANE FILE, which is a different audience and currently has NO version of this caveat at all (checked: grep -n boundary-exact CONTRACTS-ONDISK.md currently finds nothing).
  WIDENED 2026-08-08 (spec-keeper). The reviewer that PASSed the hub.go/mint_test.go rewrite (owned
  separately, see Spec Server task 9fd58deb and the new task filed for that specific rewrite) flagged
  that the SAME false-all-clear claim ("closes/closed the window", "the one uncovered case") survives
  in FOUR sibling Go comments this task did not originally cover -- and one of them is the very source
  the new honest hub.go block cites, so fixing only the docs plane leaves the origin uncorrected:
  
    - internal/hub/seqfloorfile.go:231 -- "Open logs it at WARN when the data directory is not
      otherwise fresh, and CLOSES the window immediately by persisting the derived floor."
    - internal/hub/seqfloorfile.go:241 -- "Missing-file plus quarantine on the SAME start is the one
      uncovered case" -- stated as the ONLY gap, when boundary-exact truncation on a NON-quarantine
      start is also uncovered and is the one the new hub.go WARN and 9fd58deb now document.
    - internal/hub/hub.go:716 -- repeats "the one uncovered case" (quoting seqfloorfile.go's framing)
      about 40 lines above the new honest block added under 9fd58deb's rewrite, which directly refutes
      it in the same file.
    - internal/hub/mint_test.go:455 -- "The window is closed by the very start that finds it open: Open
      writes the derived floor before it serves." -- same shape, in a test's doc comment this time.
  
  FIX for all four: same correction as the hub.go WARN -- state plainly that the guard covers
  DISCARD-DETECTABLE damage only (o.LogRepaired / recovery physically removed something) and NOT a
  truncation landing exactly on a record boundary, which recovery cannot distinguish from a log that
  legitimately ended there. Do not claim the window is closed or that the uncovered case is limited to
  missing-file-plus-quarantine; boundary-exact truncation on ANY start (quarantine or not) is a second,
  now-documented uncovered case. Cross-reference 9fd58deb.
  
  ALSO WIDENED to two more CONTRACTS-ONDISK.md locations beyond the original 325-327 sentence, both in
  the message-seq-floor section:
  
    - CONTRACTS-ONDISK.md:334-337 ("Migration residual, stated plainly...") and :339-346 ("Be precise
      about when the window actually closes...") -- both currently frame the ONLY residual as the
      missing-file-plus-quarantine-on-first-start case. Neither mentions boundary-exact truncation at
      all, which is a second, independent way the "window" stays open past a successful start with a
      non-zero derived floor. Add it alongside the existing residual, do not let the new caveat read as
      though it replaces or narrows the one already documented there.
  
    - CONTRACTS-ONDISK.md:269-273 is SEPARATELY STALE (security finding, distinct from the false-
      all-clear shape above): it documents a valid-digest `floor 18446744073709551615` as *adopted*
      ("seals the allocator at MaxUint64, every subsequent mint fails permanently"). At HEAD this is no
      longer true: internal/hub/seqfloorfile.go:539 has a plausibility bound that REFUSES an
      implausibly-high floor outright (ErrSeqFloorFileCorrupt, naming both the file and the remedy:
      "move message-seq-floor aside and restart") rather than adopting it and bricking the allocator.
      Update the bullet to describe the refusal, not the adoption -- the DoS-shaped conclusion
      ("denial of service, not an id reissue") may no longer be the right framing once the value is
      refused rather than adopted; the implementer should re-derive whatever the current behaviour's
      actual failure mode is (refusal is FATAL and not regenerated, per the CORRUPT-file paragraph a
      few lines below) and write that, not assume the old conclusion still holds.
  
  SCOPE stays CONTRACTS-ONDISK.md plus the four Go comment locations listed above. Still do not touch
  the hub.go WARN log line itself or its guard predicate -- that is 9fd58deb's tracking task and the
  separately-filed task for the WARN-wording rewrite; this task's job is every OTHER place the same
  claim was written down.
  _Proof: bash -c 'grep -q "boundary-exact truncation" CONTRACTS-ONDISK.md && ! grep -rn "CLOSES the window immediately\|the one uncovered case\|window is closed by the very start" internal/hub/seqfloorfile.go internal/hub/hub.go internal/hub/mint_test.go'_
- [ ] None · Startup scans the WAL twice (soon three times) -- bound the cost — durability, P2
  Startup replay currently scans the WAL twice: the log.go replay pass and the writer.go open pass. DUR-4 (corrupt-tail detection) adds a third scan. This is fine at small WAL sizes but does not bound startup cost as the log grows. Relates to DUR-7 (snapshot/compaction follow-up), which is the real long-term fix for unbounded replay time -- this task is narrower: either (a) collapse the passes where safe, or (b) explicitly document/measure the cost and defer the real fix to DUR-7, whichever the implementer judges correct after reading the current pass structure post-DUR-4.
  _Proof: go test -bench=BenchmarkWALOpen ./internal/wal_
- [ ] DUR-12-FU-DOUBLEBACKUP · DUR-12-FU-DOUBLEBACKUP: crash between os.Link and os.Rename in upgradeV1 can leave a second .v1-<ns> backup on redo — durability, P3
  P3, durability. Reviewer P2-4: a crash between os.Link and os.Rename in upgradeV1 (recover.go:528) yields a second <log>.v1-<ns> backup on redo. Harmless (hard links to one inode) but it contradicts the "exactly 1 backup" invariant a test asserts; wants a comment or a guard.
- [ ] None · maxPlausibleSeqFloor is enforced on the READ path only -- hub can persist a floor its own reader then refuses, and the documented remedy loops — security, P0, deferred
  SECURITY GATE FINDING (HIGH) against the seq-floor bound shipped under be447589-6583-4d5c-a9d4-ec9d9fef0f1c, committed at 217a3c0. PROVED BY RUNNING CODE (a throwaway test), not by reading.
  
  MECHANISM. The bound is checked in ONE place, on the READ path: internal/hub/seqfloorfile.go:538 (`if n > maxPlausibleSeqFloor`), inside the parse. The WRITE path bounds nothing: `persistLocked` (seqfloorfile.go:365) refuses only a DECREASE and then writes whatever it was handed. Re-verified at HEAD 16da89f by spec-keeper: the whole persistLocked body is 11 lines and `sed -n '/^func (f \*seqFloorFile) persistLocked/,/^}/p' ... | grep -c maxPlausibleSeqFloor` returns 0.
  
  WHY THAT IS REACHABLE. `hub.Open` derives the floor from three LOG-derived sources and persists it through `raise()` (seqfloorfile.go:311) -> `persistLocked`. So the value the bound exists to reject can arrive from the log rather than from the floor file, and never passes `parseSeqFloorLine` at all.
  
  MEASURED. `raise(math.MaxUint64)` is ACCEPTED and fsynced, and the next `openSeqFloorFile` REFUSES the file this package itself just wrote. The documented remedy -- move the floor file aside -- re-derives from the poisoned log and LOOPS. The same test proved a 256-value window in which a floor at the bound plus one `MintBatchSize` bricks the next start.
  
  COMPOUNDING (do not treat as separate). WAL v1 is accepted with an UNKEYED CRC32 and laundered into a MAC'd v2 log (tracked as DUR-12-FU-V1LAUNDER, daf18983-fb58-47cd-8e1b-b9dc50a36f08), so a directory-write attacker reaches the floor VIA THE LOG and never touches parseSeqFloorLine -- i.e. the read-path bound is not merely incomplete, it is on the wrong side of the actual attack path.
  
  MINIMAL FIX (given by the gate). Move the bound into `persistLocked`, the last point before bytes are written. That is the single choke point all four inputs (the three log-derived sources plus the file) pass through, so it fires at the source and covers them at once. Note persistLocked already carries the monotonicity refusal for exactly this reason -- its own comment says a bad value here "is caught at the last point before the bytes are written" -- so this is completing an argument the code already makes, not adding a new one.
  
  SIBLING, NOT A DUPLICATE: 259b7033-2191-423f-bb7b-cff8c6b59dc1 bounds the wal-index-floor reserved value in internal/wal. This task is internal/hub only.
  
  PROOF STATE OBSERVED (spec-keeper, HEAD 16da89f, not assumed): `bash scripts/proof-check.sh '<proof_cmd>'` -> verdict=FAIL, exit 1. RED, and RED FOR THE RIGHT REASON (the sed/grep pin returns 0 matches inside persistLocked, so the && short-circuits) -- RED today rather than VACUOUS. The test half must ALSO be observed RED before the fix.
  _Proof: sed -n '/^func (f \*seqFloorFile) persistLocked/,/^}/p' internal/hub/seqfloorfile.go | grep -q maxPlausibleSeqFloor && go test -race -count=1 -run TestSeqFloorPersistRefusesAnImplausibleFloor ./internal/hub_
- [ ] DUR-12-FU-VERSIONFLIP · DUR-12-FU-VERSIONFLIP: single-bit version-field flip on a v2 log misidentifies it as v1 and quarantines it — durability, P2
  P2, durability. Reviewer P2-1: a version 2 log whose version FIELD alone flips 2->1 is misidentified as v1, nothing verifies under the v1 codec, and the ErrMACKeyMismatch guard at recover.go:306 is skipped because of !c.isV1() -- so an intact log is QUARANTINED and the bus starts empty. Bytes are preserved and it is logged at ERROR, and it is strictly MORE available than the pre-DUR-12 behaviour (which was fatal), so it is not a regression. Fix: in repairLog, when c.isV1() && HeaderDamaged && !Salvageable, try the v2 header tag first to disambiguate.
- [ ] DUR-12-FU-READONLYKEY · DUR-12-FU-READONLYKEY: read-only fsck paths (ScanAll, Replay) create wal-mac.key as a side effect — durability, P3
  P3, durability. Reviewer P2-2: reader.go:34 (ScanAll) and replay.go:94 (Replay) will CREATE wal-mac.key for a log that exists but is unidentifiable, although both are documented as read-only paths. A read-only fsck should not have a file-creating side effect.
- [ ] None · Invert stale internal/hub/hub.go quarantine reissue-permitted assertions once MSG-FU-SEQHIGHWATER lands — durability, P1
  internal/hub/hub.go contains two locations describing post-quarantine message-id reuse as EXPECTED/accepted framing, which is now SUPERSEDED (DECISIONS.md 2026-08-07, "SUPERSEDES two earlier passages"; invariant 1 stands unnarrowed -- reuse is a DEFECT, not a documented limit):
  
  - hub.go:137-140 (doc comment on Options.Quarantined): "...so the sequence high-water mark from before it is unrecoverable and message ids may repeat values the quarantined file already used."
  - hub.go:383-394 (Open, the Quarantined branch): a comment block plus the h.log.Error(...) call whose MESSAGE TEXT literally states "message ids may repeat values the quarantined log already used (invariant 1)" -- this is a PRODUCTION log line an operator reads as ground truth.
  
  These were correct when written (2026-08-02, before the WAL index-floor fix) but are STALE once 6ebe51be (MSG-FU-SEQHIGHWATER, raised to P0 2026-08-07) lands and is verified: internal/wal/indexfloor.go (uncommitted as of 2026-08-07 -- see db350e39s implementer note, 2026-08-07T12:25) already updates every equivalent comment INSIDE internal/wal to the new invariant-1-preserving language ("an index this data directory has authorised is never authorised again"), but internal/hub is OUT OF that agents file ownership (internal/wal only) and was explicitly flagged "NOT DONE" ("hub-side assertions") in their report.
  
  SCOPE: once 6ebe51be is verified fixed -- hubs derived sequence floor (o.NextIndex = wal.Recovered.NextIndex) is provably protected across quarantine, not just asserted to be -- invert these two hub.go passages to state the CORRECT, current guarantee instead of the superseded one. Reconsider the h.log.Error(...) call level once quarantine no longer implies id-reuse exposure (may drop to WARN, or the branch may become unnecessary -- implementing agents call, backed by a test).
  
  ALSO check internal/wal/recover_test.go:443 ("the next append reissues the index the discarded frame carried") -- as of 2026-08-07 this is a stale assertion sitting in a file already mid-edit (git-modified) by the live agent fixing db350e39/e120153bs 9 newly-failing wal tests; verify it lands there rather than duplicating the fix here.
  
  NOT IN SCOPE (already correctly handled by this projects append-only-log convention -- no action needed): DECISIONS.md:1541 and AGENT_LOG.md:1048, both historical journal entries already superseded via the NEW 2026-08-07 DECISIONS.md section ("SUPERSEDES two earlier passages") rather than edited in place, per the append-only rule.
  
  Do NOT invert this language before 6ebe51be actually lands and is verified end-to-end -- inverting the wording while the underlying defect might still be present would be worse than todays honest-but-stale warning.
  _Proof: grep -n "may repeat" internal/hub/hub.go; test $? -ne 0_
- [ ] None · wal.OpenWriter/RepairTail open bus.wal without O_NOFOLLOW -- a planted symlink is followed (writer.go:68, recover.go:593/618) — durability, P1
  internal/wal opens the WAL data file WITHOUT O_NOFOLLOW on both paths that can open a pre-existing name at the log's path, unlike internal/dirlock which already gets this right for bus.lock (dirlock.go:185-193, "O_NOFOLLOW because we TRUNCATE this path once the lock is ours: a symlink here now fails with ELOOP").
  
  Exact sites (verified by reading the file this pass, current line numbers):
    - internal/wal/writer.go:56  -- os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, fileMode) (the create-fresh branch; O_EXCL means a pre-existing symlink at path already fails here, so this specific call is not exploitable -- included for completeness / the reviewer's benefit only).
    - internal/wal/writer.go:68  -- os.OpenFile(path, os.O_RDWR, fileMode) THE VULNERABLE ONE: reached when O_EXCL above failed with os.ErrExist (path already exists, e.g. is a symlink). No O_NOFOLLOW, so a symlink at <data-dir>/bus.wal is followed and every subsequent Append/fsync lands on whatever the symlink points at instead of the file the operator/server believes it opened.
    - internal/wal/recover.go:593 -- os.Open(path) inside scanFraming (called from RepairTail on every startup).
    - internal/wal/recover.go:618 -- os.OpenFile(path, os.O_RDWR, fileMode) inside truncateAt (the one place that SHORTENS the file -- following a symlink here means an attacker-planted link could redirect a truncation onto an unrelated file).
  
  Confirmed experimentally this pass (standalone stdlib-only probe, no repo file touched): a symlink planted at the WAL's path pointing at an unrelated file, then opened with exactly writer.go:68's flags (os.O_RDWR, no O_NOFOLLOW), IS FOLLOWED -- the open succeeds and writes land on the symlink's target, not on a new file at the WAL's own path. dirlock's own comment (dirlock.go:185-193) already documents this exact class of risk for the lock file and fixes it with syscall.O_NOFOLLOW; the WAL's writer/recover code never got the same treatment.
  
  DONE means: every os.Open/os.OpenFile call in internal/wal/writer.go and internal/wal/recover.go that opens a path which may already exist (i.e. everywhere except the deliberate O_EXCL create branch, which is already safe) adds syscall.O_NOFOLLOW to its flags, mirroring dirlock.go:185-193, plus a regression test analogous to dirlock_test.go:TestAcquireRefusesASymlinkedLockFile that plants a symlink at a WAL path and asserts Open/OpenWriter/RepairTail refuse it rather than following it.
  
  proof_cmd validated via scripts/proof-check.sh: verdict=FAIL (exit 1) today -- grep finds no O_NOFOLLOW in either file yet. Will PASS once the fix lands. (A behavioral regression test is also required per the "done" criteria above; the grep is the cheap CI-checkable floor, not a substitute for the test.)
  _Proof: grep -q "O_NOFOLLOW" internal/wal/writer.go && grep -q "O_NOFOLLOW" internal/wal/recover.go_
- [ ] None · MSG-FU-SUFFIXFLOOR-FU-STREAMSCAN: export a streaming raw WAL scan and reinstate the every-start suffix-floor cross-check — wal, P1
  Found by the reviewer AND security gates on MSG-FU-SUFFIXFLOOR (94159d93-fe87-4c3e-b938-86fe7068c787), which shipped the startup wiring.
  
  PROBLEM. cmd/agent-bus/suffixfloors.go derives legacy-dir suffix floors with wal.ScanAll, which accumulates EVERY record INCLUDING FULL PAYLOADS in memory (internal/wal/reader.go: recs = append(recs, rec)). The WAL never rotates or compacts and enrolment is unauthenticated, so peak RSS at startup is proportional to the whole log. internal/wal already has a measured incident on record where a per-record INDEX LIST -- far smaller than payloads -- cost 1.76 MB on a 23.7 MB log and was called 'the boot-time OOM the eviction was written to avoid' at 10 GiB. wal.Replay is streaming by contrast (scanFrom callback); the raw scan is not, because scanFrom is UNEXPORTED.
  
  MITIGATION ALREADY SHIPPED: the scan is gated on !alloc.Existed(), so it runs at most once per data directory (the legacy backfill). That bounds the exposure to the migration start, and it is why this is P1 and not P0.
  
  WHAT IT COST. Gating it also removed the every-start CROSS-CHECK: a WAL suffix exceeding the persisted floor is a detectable integrity failure (a rewound or restored-from-backup agent-suffixes file), and nothing detects it now. A DELETED floors file is still caught (Existed()==false triggers the backfill, and the missing-file case is logged at ERROR when the dir has history); a CORRUPT one is caught by ids.ErrSuffixFileCorrupt; a REWOUND-BUT-VALID one is not.
  
  DO. (1) Export the streaming seam internal/wal already has -- e.g. wal.ScanFunc(path, kind, fn error) wrapping scanFrom, whose own doc calls it 'the seam recovery uses for a streaming replay'. (2) Rewrite cmd/agent-bus/suffixfloors.go's walAgentIDFloors to fold floors as records arrive: O(record) peak, not O(log). (3) Reinstate the cross-check on EVERY start, comparing the derived floors against ids.DurableNameSuffixes.Floors(); on a WAL suffix ABOVE the persisted floor, log at ERROR and RaiseFloor to it (raising can never lower a floor, and detection without correction leaves the bus knowingly re-minting a visible id -- both gates endorsed raise-over-refuse; refuse-to-boot would hand anyone with data-dir write access a permanent boot-denial primitive).
  
  PROOF. A test that a dir whose agent-suffixes file has been rewound to a stale-but-checksum-valid version does NOT re-mint, and logs at ERROR. Plus a memory assertion, or at minimum a test that the scan never materialises the whole log.
  
  ACCEPTANCE. wal exports a streaming raw scan; cmd/agent-bus/suffixfloors.go uses it; the every-start cross-check is restored and proven by a rewound-floors-file test; go test -race green.
- [ ] DUR-12-FU-CONTRACTS · DUR-12-FU-CONTRACTS: land the six CONTRACTS-ONDISK.md rows deferred from DUR-12 — docs, P2
  P2, docs. Land the six CONTRACTS-ONDISK.md rows quoted in the DUR-12 kind=report note (author feature-runner) on task cbc9ab0c-3b34-48d0-acd8-5eabd4dc4a02: on-disk format version 1 vs 2, wal-mac.key file (mode 0600, 64 lowercase hex chars, wal.MACKeyFileName), exported constant value changes (wal.FormatVersion 1->2, wal.FileHeaderSize 16->48, wal.FrameHeaderSize 20->48, new wal.MACSize=32), new errors.Is-checkable sentinels (wal.ErrMACKeyMissing, wal.ErrMACKeyMalformed, wal.ErrMACKeyMismatch), startup failure modes that REFUSE TO START, and upgrade artefacts left in the data dir (<log>.upgrade, <log>.v1-<unix-nanos>). State in the description that reviewer raised this as P1-1 and that CONTRACTS-ONDISK.md:12 still reads "None yet -- no durable store, no WAL record types...", which is now false. proof_cmd must GLOB so it survives any further CONTRACTS split, and it is CONFIRMED RED right now (exit 1, verified before filing): grep -q 'wal-mac.key' CONTRACTS*.md && grep -q 'ondisk-format-version = 2' CONTRACTS*.md && echo CONTRACTS_ONDISK_OK
- [ ] None · Settle the message-seq-floor KEYING question as an explicit follow-up, replacing the 'worth doing for consistency' framing -- and fix hub.go's operator-facing forging recipe in the SAME task — security, P2
  BOTH SECURITY GATES on be447589-6583-4d5c-a9d4-ec9d9fef0f1c AGREED that keying `message-seq-floor` with the WAL MAC is the wrong fix, and EACH supplied a stronger reason than the one currently recorded. This task replaces the recorded framing and closes the one code artefact that depends on the answer.
  
  WHAT IS RECORDED TODAY, AND WHY IT IS TOO WEAK. DECISIONS.md (in the 2026-08-07 feature-runner data-directory-permissions section, "What was deliberately NOT done") ends: "Keying remains worth doing for consistency with `wal-index-floor`, as a separate and honestly-labelled change." "For consistency" understates the case in one direction and overstates it in another, and both gates said so.
  
  THE TWO STRONGER REASONS, TO BE RECORDED.
  (1) THREAT-MODEL: keying only helps an attacker with directory-WRITE but WITHOUT file-READ -- precisely the attacker `enforceDataDirPermissions` now EXCLUDES. The attacker who remains (the bus's own uid, or root) can read `wal-mac.key` and forge any MAC we add. So against the POST-GATE threat model it buys approximately nothing.
  (2) AVAILABILITY: `wal-mac.key`'s documented loss remedy is "move the log aside and restart". Key the floor to that SAME key and that remedy BRICKS THE BUS, because `ErrSeqFloorFileCorrupt` is fatal and the floor file is never regenerated. That couples the ONE FILE THAT EXISTS TO SURVIVE LOG LOSS to the key whose loss already forces abandoning the log -- a direct conflict, not a nicety.
  (3) Per invariant 9, sharing one key across two message types needs a DOMAIN-SEPARATED SUBKEY, never plain reuse. Any keying that does land must say which construction and why.
  
  TWO AMENDMENTS THE GATES ASKED FOR EXPLICITLY.
  (a) File it as an EXPLICIT FOLLOW-UP with a decision, not as "for consistency" -- because there IS one place keying adds value over the directory gate: the GROUP-WRITABLE TIGHTEN path adopts PRE-PLANTED files and CONTINUES. `enforceDataDirPermissions` chmods a group-writable dir to 0700 and starts; anything already planted in it before that chmod is adopted unchecked. That is the honest scope of what keying would buy, and it should be recorded as the reason rather than "consistency".
  (b) internal/hub/hub.go CURRENTLY HANDS OPERATORS A COMPLETE FORGING RECIPE for an unkeyed file. In the `ErrSeqFloorUnprovable` remedy text (hub.go, the error block at ~:733-741 at HEAD 16da89f -- the gate cited :707, the line has drifted, the text is confirmed present) it reads: "write it to %s yourself -- the format is two plain-text lines, %q followed by \"floor <n>\", where the digest is an unkeyed SHA-256 over the second line". That is SAFE TODAY (the file genuinely is unkeyed, and the remedy is genuinely needed) and WRONG THE DAY KEYING LANDS -- an operator following it would produce a file the bus rejects. It MUST change in the same task as the keying decision, whichever way that decision goes.
  
  SUGGESTED DELIVERABLE. A dated DECISIONS.md section recording the decision and all three reasons above plus amendment (a); and a guard test that asserts hub.go's remedy text agrees with the ACTUAL on-disk scheme produced by `encodeSeqFloor`, so the recipe cannot drift out of truth silently -- that test is valid under EITHER decision and goes RED the day keying lands without the text being updated.
  
  PROOF STATE OBSERVED (spec-keeper, HEAD 16da89f, not assumed): `bash scripts/proof-check.sh '<proof_cmd>'` -> verdict=FAIL, exit 1. RED, and RED FOR THE RIGHT REASON (`grep -c 'group-writable tighten path' DECISIONS.md` = 0, so the && short-circuits) -- RED today rather than VACUOUS. The test half must ALSO be observed RED before the fix.
  _Proof: grep -q 'group-writable tighten path' DECISIONS.md && go test -race -count=1 -run TestSeqFloorRemedyTextMatchesTheOnDiskScheme ./internal/hub_
- [ ] None · Document what the bus does with an UNKNOWN WAL record type -- the answer is now discard-and-continue, not refuse-to-start (REVERSED 2026-08-02) — docs, P1
  REVERSED 2026-08-02 BY USER DECISION. THE BEHAVIOUR THIS TASK WAS FILED TO DOCUMENT IS BEING REMOVED,
  SO DO NOT DOCUMENT IT.
  
  The task said: "DUR-3 introduced a new way for a bus to refuse to boot: a WAL containing a record this
  code cannot interpret now FAILS STARTUP ... That direction is correct (silent data loss is worse than
  a loud refusal to start)." The user decided the opposite (DECISIONS.md, 2026-08-02): *"always be able
  to restart, prefer to discard messages and/or corruption, with logging"*. An unknown record type is a
  DAMAGE class: discard it, log loudly and specifically, keep running. The decision reconciles the two
  positions -- "The defect was never that data was discarded; it is that the discard was SILENT."
  
  WHAT TO DOCUMENT INSTEAD (in PROTOCOL.md, plus the operator-facing notes):
   - What an unknown record type IS (a record written by a NEWER binary, or a damaged type field --
     the reader cannot tell them apart, which is exactly why refusing to start was a downgrade trap).
   - What the bus DOES: discards that record, logs a specific line naming offset, record index, the
     unrecognised type value and the byte count discarded, and CONTINUES to a serving state.
   - What the operator SEES and what it means -- in particular that a burst of unknown-type discards
     after a rollback is the signature of a DOWNGRADE, not of media failure, and the remedy is to run
     the newer binary again rather than to repair the log.
   - What is NOT affected: NON-DAMAGE errors still refuse to start (permission denied, I/O failure,
     data-directory lock held, missing/unwritable data dir).
  
  RELATED, DO NOT DUPLICATE: DUR-4-FU-DOCS (now P0) owns the RepairTail/TailRepair API surface, the
  narrowed invariants 4 and 6, and at-least-once delivery; bd3cc650 owns the stale CONTRACTS.md:55
  record-type list; DOCS-2 owns CREATING PROTOCOL.md, WHICH DOES NOT EXIST YET (verified 2026-08-02 --
  this task's original proof_cmd grepped a file that is not in the repo, so it could never have passed).
  e875182a is the sibling forward-compat problem: internal/wal/log.go's decodePayload uses
  DisallowUnknownFields, so an unknown FIELD is currently fatal for the same downgrade reason an unknown
  TYPE was -- reconcile the two answers rather than documenting them differently.
  
  SEQUENCING: after DUR-11, which is the task actually converting the refusals into discards.
  
  PROOF. `test -f PROTOCOL.md && grep -q 'unknown record type' PROTOCOL.md` -- FAILS TODAY at clause 1,
  correctly and non-vacuously, because PROTOCOL.md does not exist. The previous proof_cmd
  (`grep -n "unknown record\|refuses to start\|startup failure" PROTOCOL.md`) named a nonexistent file
  AND grepped for the retired policy's wording.
  _Proof: test -f PROTOCOL.md && grep -q 'unknown record type' PROTOCOL.md_
- [ ] None · IDEM-11-FU-DOWNGRADE: an old binary SILENTLY DISCARDS acknowledged writes after IDEM-11 -- bump FormatVersion so it refuses to start instead — durability, P1
  NEEDS A USER DECISION (on-disk format). Raised by triage 2026-08-03 reviewing IDEM-11's staged (UNCOMMITTED) work.
  
  IDEM-11 adds an additive `idem` field to the WAL prepare payload (internal/wal/log.go, Entry.Idem json.RawMessage). Forward direction is correct and mandated: old logs replay unchanged under the new binary. The REVERSE direction is the problem.
  
  internal/wal/log.go:791 decodePayload uses dec.DisallowUnknownFields(). A binary built BEFORE this field, reading a log written AFTER it, treats EVERY prepare carrying an idem record as an undecodable payload -- and recovery DISCARDS it. That is an ACKNOWLEDGED WRITE LOST on downgrade, silently, not a degraded-but-correct read. The implementing agent documented this honestly in a FORWARD-COMPATIBILITY HAZARD block at internal/wal/log.go:104-115 and mitigated it by POLICY ("downgrade is not a supported operation here -- one binary, one container, forward-only") rather than by enforcement.
  
  TRIAGE'S POSITION: policy is not a mechanism. FormatVersion is still 2 (internal/wal/format.go:30) even though the payload shape changed, so nothing on disk tells an older binary that it is out of its depth. The house failure mode everywhere else in this codebase is REFUSE TO START, loudly; here it is DISCARD, silently -- the worst possible pairing with invariant 4 (nothing acked before durable). Bumping FormatVersion (RESERVED through the Spec Server, never picked by hand) makes an old binary refuse to start on a new log, converting silent data loss into a loud, correct failure. The new binary still reads v2 logs, so the mandated ordering (a format change ships WITH the recovery path for previous-format logs) is preserved.
  
  NOTE ON PROVENANCE: triage's own dispatch brief explicitly FORBADE the IDEM-11 agent from touching FormatVersion, to keep it out of the contested recovery code. That guard was right about recover.go and wrong about the version field. This task exists to correct triage's guard, not the agent's work -- the agent complied with its brief and documented what it could not fix.
  
  DECISION NEEDED FROM THE USER: (a) bump FormatVersion to 3 so downgrade fails closed, accepting that a v3 log can never be read by any released binary; or (b) accept the forward-only policy as-is and record it in DECISIONS.md as an explicit, dated acceptance of silent-data-loss-on-downgrade. Do NOT choose (c) loosen the decoder -- a lenient decoder is how a file that no longer says what history was accepted gets served as if it did.
  
  Related: DUR-12-FU-VERSIONFLIP already tracks a single-bit version-field flip misidentifying a v2 log, so the version field's failure behaviour is under review anyway.
- [ ] None · Stale references to deleted test name TestServerOpensWALOnStartRefusesACorruptLog in DECISIONS.md and AGENT_LOG.md — durability, P2
  DECISIONS.md:508 and AGENT_LOG.md:173 still name the test TestServerOpensWALOnStartRefusesACorruptLog, which tested the OLD refuse-to-start-on-corruption policy. That test was deleted/replaced by TestServerQuarantinesACorruptLogAndStartsAnyway (cmd/agent-bus/wal_startup_test.go:315), which asserts the current (2026-08-02, availability-over-retention) policy. Fix: update both references to name the current test, keeping the surrounding historical narrative intact (do not rewrite history, just correct the pointer to a test that still exists). NOTE: SPEC.md:1190 has the same stale reference but SPEC.md is a generated mirror of the Spec Server -- do NOT hand-edit it as part of this task; it will self-correct once the underlying task text is fixed and the mirror is regenerated, or via its own text update if it is carried directly in a task description.
  _Proof: ! grep -rn "TestServerOpensWALOnStartRefusesACorruptLog" DECISIONS.md AGENT_LOG.md_
- [ ] None · maxPlausibleSeqFloor is 2^56, which exceeds the JSON safe-integer range 2^53 -- seq ships as a bare uint64 JSON number a float64 consumer cannot represent exactly — httpapi, P2
  SECURITY GATE FINDING (LOW) against the bound shipped under be447589-6583-4d5c-a9d4-ec9d9fef0f1c (217a3c0).
  
  MECHANISM. `const maxPlausibleSeqFloor uint64 = 1 << 56` (internal/hub/seqfloorfile.go:97). `seq` ships on the wire as a BARE uint64 JSON number, so a forged-but-passing floor near the bound yields sequences a float64-parsing consumer (JavaScript, jq's default numeric handling, any language whose JSON number is a double) cannot represent EXACTLY -- it silently rounds, and two distinct sequences can land on one value in the consumer.
  
  CALL SITES re-verified at HEAD 16da89f by spec-keeper (the gate's line numbers had drifted; the content is confirmed): internal/httpapi/messages.go:176, :225, :252, :263 and internal/store/message.go:269 all declare `Seq uint64` with a plain `json:"seq"` tag. No string encoding, no bounds note.
  
  FIX AND ITS COST. Lower the bound to `1 << 53`. It costs nothing: at 1,000,000 sequences/second 2^53 is still roughly 285 YEARS, so no real bus is constrained by it, and the bound's stated purpose (reject an implausible value) is unaffected. This is strictly a tightening of an arbitrary constant to the boundary that the SERIALISATION already imposes.
  
  SEQUENCING NOTE: if the persist-path bound task is done first, this becomes a one-constant change covering both paths at once; if this is done first, the read path is safe and the write path is still unbounded. They are independent but the persist-path task is the higher priority of the two.
  
  PROOF STATE OBSERVED (spec-keeper, HEAD 16da89f, not assumed): `bash scripts/proof-check.sh '<proof_cmd>'` -> verdict=FAIL, exit 1. RED, and RED FOR THE RIGHT REASON (the pin matches 0 times; `grep -n 'maxPlausibleSeqFloor uint64' internal/hub/seqfloorfile.go` shows the current `1 << 56` at line 97) -- RED today rather than VACUOUS. The test half must ALSO be observed RED before the fix.
  _Proof: grep -q 'maxPlausibleSeqFloor uint64 = 1 << 53' internal/hub/seqfloorfile.go && go test -race -count=1 -run TestSeqFloorBoundIsJSONSafe ./internal/hub_
- [ ] None · Expose on wal.Recovered the highest index a record actually CONSUMED — durability, P1
  SHARED BLOCKER, reached from three directions independently: (1) be447589 (data-directory permissions + message-seq-floor guard) -- the shipped fix removed both durable predicate arms that tried to approximate this (NextIndex accounting, then MissingRecords) after each one PERMANENTLY BRICKED a healthy directory on an ordinary unclean shutdown; feature-runners closing note on that task says explicitly: closing the gap needs internal/wal to expose the highest CONSUMED index -- BLOCKER, outside boundary. (2) e120153b (WAL recovery reissuing a discarded tail record index) -- reviewer found the P0 symptom remains reachable by a non-quarantine route: floor.written is only raised at begin() and at a CLEAN seal(); a crashed run leaves ONLY reserved as evidence, and reserved is consulted only when the log ITSELF proves damage -- so a truncation that looks clean (no torn frame, LostUnidentified=false) can still reissue. (3) An independent measurement (reported by an agent in another repo, see notes on e120153b and be447589) swept 553 byte-exact truncation offsets against a purpose-built specimen (7 delivered messages, seqs 1,2,3 and 257-260, floor written=22 after a restart, 8900-byte WAL) and found a reissuing band (offset 1004-4439, derived floor 256) that is INDISTINGUISHABLE from a healthy directory one restart younger -- valid digest, nothing wrong with the file -- so no plausibility bound, human inspection, or MAC/integrity check can ever see it. Only knowing what the log itself proves was CONSUMED (survived + discarded + quarantined, as opposed to merely reserved-but-never-written) can close this.
  
  REQUIRED: add a field to wal.Recovered (or an equivalent accessor) reporting the highest record index the replay/repair pass actually CONSUMED this run -- i.e. observed in the file, whether it survived, was discarded as damaged, or was quarantined -- distinct from NextIndex (which is floor-raised) and distinct from the durable floors reserved/written (which persist across runs and can go stale). wal already computes and LOGS this internally (log.go: indices_skipped / fileNext at the WARN line noted by reviewer), it is just not on the public Recovered struct.
  
  This unblocks: a narrower be447589 guard predicate (refuse only when the floor is absent AND this-run-consumed exceeds what the log alone can prove, not on every unclean shutdown); a real fix for e120153bs non-quarantine reissue route; and any future low-floor plausibility check (the class of check the independent measurement showed a high-value bound cannot substitute for).
  
  SCOPE: internal/wal only. Coordate with whichever agent has DUR-5 (append-only audit log) live in the package to avoid two agents rewriting replay.go/recover.go concurrently.
  _Proof: bash scripts/proof-check.sh 'go test -race -run TestWALRecoveredExposesHighestConsumedIndex ./internal/wal'_
- [ ] None · DUR-11-FU-STAGE2SHORTCIRCUIT: resyncFrom never runs the sound stage-2 scan after a stage-1 budget exhaustion — durability, P1
  internal/wal/salvage.go:537-540 (resyncFrom): `o, exhausted, err := scanForFrame(f, c, size, from, lastIndex, true); if err != nil || o >= 0 || exhausted { return o, exhausted, err }`. The `|| exhausted` short-circuit means the sound STAGE 2 scan (the one that drops the index-density window and is what closed security post-fix BLOCKER on DUR-11) never runs once stage 1 reports budget exhaustion. This directly contradicts the doc rule three lines below it: "A BOUNDED SEARCH FINDING NOTHING IS NEVER ON ITS OWN GROUNDS FOR NOTHING FOLLOWS." Stage 2 exists precisely because a bounded stage-1 search wrongly concluded nothing followed -- this short-circuit reintroduces that exact failure mode by a different route.
  
  REPRODUCED by security during the DUR-11 post-fix audit (2026-08-07): with the genuine next record sitting BEFORE a field of decoys, a control call `scanForFrame(dense=false)` finds it at offset 266 with a fresh budget, yet full recovery truncated 199898 bytes and deleted the record because resyncFrom returned early on stage-1 exhaustion without ever trying stage 2. Confirmed: it is the short-circuit, not the budget, that kills the record. Security ran this as a probe (intended to demonstrate the bug, not to pass): `proof-check: verdict=FAIL class=test exit=1 tests_run=1 top_level=1 skipped=0 failed=1 empty_pkgs=0` -- FAIL is the correct/expected outcome of that specific probe.
  
  Suggested minimal fix (record only, do not implement here): drop `|| exhausted` from the stage-1 short-circuit and OR the two stages exhaustion flags together instead, so budget exhaustion at stage 1 does not suppress stage 2.
  
  Secondary LOW/cosmetic item, same file, fold into this task or split at implementer discretion: internal/wal/salvage.go:370-371 renders a 1-byte tail discard as "it and the 0 bytes after it" while the same log line reports bytes=1 -- the English phrasing implies zero total bytes discarded when one byte was in fact discarded.
  
  SCHEDULING CONSTRAINT: internal/wal is currently OWNED by another live (feature-runner) agent with substantial uncommitted edits (indexfloor.go added; doc.go/log.go/recover.go/recover_test.go/replay.go/salvage.go/writer.go modified, +616/-71 as of 2026-08-07). Do NOT dispatch an implementer against this task until that in-flight work lands and internal/wal is clear -- editing salvage.go concurrently with an unrelated in-flight rewrite of the same file risks clobbering both.
  
  WHY P1 NOT P0 (security's own reasoning, recorded so the call is not silent): (1) it is LOUD, not silent -- the exhausted path already logs two ERROR lines, sets Repair.Exhausted, and the discard reason text states the region was removed WITHOUT proof it was unreadable, so an operator has a true signal even though the outcome is wrong; (2) it is already the one documented remaining cascade path (DUR-11's own doc.go/recover.go narrative names Exhausted as the sole surviving cascade mechanism), not a newly-introduced silent hole; (3) it is NOT client-reachable today -- planting the >=4096 density-passing decoy headers needed to exhaust the stage-1 budget requires raw NUL bytes in a payload, and canonicalBody/json.Compact (the only WAL payload write path) rejects raw control bytes, so this is media-damage-triggerable but not attacker-triggerable via the one payload channel that exists. If a future task widens that payload channel (raw/binary bodies, base64-decoded content, ciphertext), this finding's blast radius should be re-assessed.
  _Proof: grep -n "|| exhausted" internal/wal/salvage.go_
- [ ] None · internal/hub/hub.go:590's no-floor-file quarantine ERROR promises the file will be written this start, but the LogRepaired guard now refuses Open before that write happens — durability, P2
  internal/hub/hub.go's quarantine branch (currently around line 591-611, the `switch` inside `if o.Quarantined != ""`) has a `default:` case whose h.log.Error(...) call ends: "This is the one-start migration window for a data directory written before " + SeqFloorFileName + " existed; the file is written on this start, so the next one is covered".
  
  That promise is now FALSE, and not merely optimistic -- it is directly CONTRADICTED by the guard a few dozen lines below it in the same function. cmd/agent-bus/logrepair.go's describeLogRepair sets a non-empty LogRepaired string whenever rec.Repaired.Quarantined != "" -- i.e. on every quarantine, unconditionally. hub.go's guard (currently ~line 732): `if h.seqFloorFile != nil && !h.seqFloorFile.existedAtOpen() && o.LogRepaired != "" { return nil, fmt.Errorf(...ErrSeqFloorUnprovable...) }` -- fires on EXACTLY the population the quarantine ERROR at line ~606 is printed for (no seq-floor file on disk, log just repaired/quarantined) and REFUSES to open the hub at all. Open returns an error before it ever reaches the `h.seqFloorFile.raise(floor)` call (~line 745) that would write the file.
  
  So the sequence on that start, in order, is: (1) the quarantine block logs the ERROR at line ~606 promising the file is written this start; (2) a few lines later in the SAME Open call, the guard at ~732 refuses and Open returns an error; (3) main.go's caller treats that as fatal (`opening the messaging hub: %w`) and the server does not start. The file is never written. The very next line an operator reads after the reassurance is the refusal that falsifies it.
  
  This is the same reassurance SHAPE already corrected in the migration WARN under Spec Server task 9fd58deb (and its still-open follow-up task for the sibling comments/docs) -- a sentence claiming a check or a write closes something that the code, read a few lines further, does not actually let happen on this path.
  
  FIX: rewrite the tail of the no-floor-file quarantine ERROR (hub.go, currently ~line 606) to stop promising the file is written this start. State plainly that whether the file gets written on this start depends on the guard below (o.LogRepaired / existedAtOpen()): when DataDir is configured (h.seqFloorFile != nil) this exact condition (no floor file + a repaired/quarantined log) makes Open REFUSE outright rather than write the file and continue -- so for a DataDir-backed bus this ERROR is typically followed immediately by a fatal refusal on the SAME start, not by a covered next one. If there is a real path where the file DOES get written after this ERROR (e.g. no DataDir configured, or seqFloorFile is nil for some other reason), name that path explicitly instead of a blanket claim. Add or extend a hub_test.go/mint_test.go case that starts a hub with a quarantine, no floor file, and a non-empty LogRepaired, and asserts BOTH that this ERROR is logged AND that Open returns ErrSeqFloorUnprovable on the same call -- pinning the contradiction so the wording cannot regress silently.
  
  Origin: reviewer flagged this while reviewing the seq-floor migration WARN rewrite (Spec Server task 9fd58deb / its sibling follow-up). Checked the current backlog (search by q= for the exact phrase and for line 590/606, plus a grep of SPEC.md) before filing -- no existing task covers this call site or this specific contradiction; the nearby task 'Invert stale internal/hub/hub.go quarantine reissue-permitted assertions once MSG-FU-SEQHIGHWATER lands' (SPEC.md, no numbered key) covers the OTHER quarantine case (the seqFloorFile-existed-and-survived branch's 'message ids may repeat' language, hub.go ~137-140/383-394) and is explicitly blocked pending 6ebe51be -- this is a different branch, a different defect (promise vs contradiction, not staleness), and is NOT blocked.
  _Proof: grep -n "so the next one is covered" internal/hub/hub.go; test $? -ne 0_
- [ ] DUR-7 · DUR-7: Snapshot/compaction follow-up (bounds WAL replay time) — durability, P3
  Low-priority follow-up. As the WAL grows unbounded, startup replay time grows with it; add periodic snapshotting of in-memory state plus safe truncation of the WAL prefix the snapshot covers, so recovery time is bounded by (snapshot load + tail replay) rather than full history. Not required for correctness, only for long-run startup latency.
  _Proof: go test -race -run TestSnapshotCompaction ./internal/wal_
- [ ] None · CONTRACTS.md:55 is stale -- says no WAL record types/wire version exist yet, false as of DUR-1/2/3 — docs, P2
  Verified: CONTRACTS.md:55 still reads "None yet -- no durable store, no WAL record types, no wire protocol version exists in this wave," which is false as of DUR-1/DUR-2/DUR-3 landing: record types 1=PREPARE, 2=COMMIT, 3=ABORT, 4=AUDIT, and ondisk-format-version=1 are all reserved (via the reservations API) and in use in the codebase. Update that section to list them accurately. FLAG: CONTRACTS.md is being edited by another agent right now as part of the parallel DUR wave -- re-read the file fresh before editing to avoid clobbering a concurrent change; this is a targeted one-section fix, not a full rewrite.
  _Proof: ! grep -qF "None yet" CONTRACTS-ONDISK.md && grep -qE "PREPARE[^A-Za-z]*=[^A-Za-z]*1|1[^A-Za-z]*=[^A-Za-z]*PREPARE" CONTRACTS-ONDISK.md && grep -qE "COMMIT[^A-Za-z]*=[^A-Za-z]*2|2[^A-Za-z]*=[^A-Za-z]*COMMIT" CONTRACTS-ONDISK.md && grep -qE "ABORT[^A-Za-z]*=[^A-Za-z]*3|3[^A-Za-z]*=[^A-Za-z]*ABORT" CONTRACTS-ONDISK.md && grep -qE "AUDIT[^A-Za-z]*=[^A-Za-z]*4|4[^A-Za-z]*=[^A-Za-z]*AUDIT" CONTRACTS-ONDISK.md && grep -qi "ondisk-format-version" CONTRACTS-ONDISK.md_
- [ ] None · CONTRACTS-ONDISK.md: document the bus.audit on-disk file (DUR-5 landed, wave 217a3c0, doc plane never updated) — docs, P2
  DUR-5 (append-only message audit log, internal/wal/audit.go + audit wiring in internal/hub) landed in commit 217a3c0 and is now a LIVE on-disk file: bus.audit, written on every directed send (message id, sequence, sender, recipient(s), bus path, timestamp, size, content hash -- never the body, per invariant 6). CONTRACTS-ONDISK.md was not updated by that wave: the record-type table at line 19 already reserves and names record-type 4/TypeAuditMessage (so this is a DOC GAP, not an unreserved format change -- nothing to reserve), but there is no dedicated "On-disk files in the data directory" section for bus.audit anywhere in the file, unlike the sibling sections that exist for bus.wal, wal-index-floor, message-seq-floor and the bus certificate/keys. Confirmed: grep -n bus.audit CONTRACTS-ONDISK.md currently finds nothing.
  
  FIX: add a section following the existing pattern (see "On-disk files in the data directory: the durable WAL record-index floor" and "...the durable MESSAGE-SEQUENCE floor" for the shape to match) covering: path (<data-dir>/bus.audit), mode, on-disk format version, the record shape (fields, and explicitly that the body is NEVER recorded -- this is the forward-compatibility-for-E2E-encryption rationale and is load-bearing, per invariant 6), how it relates to the WAL's own prepare/commit cycle (audit-fsync happens between prepare-fsync and commit-fsync per the DUR-5 task notes, making the audit trail a superset of committed history), crash-injection behaviour (what a crash between the audit fsync and the commit fsync leaves on disk), and what CanonicalDigest is signing.CanonicalDigest binds per PROTOCOL.md section 8.6 (not store.ContentHash -- the two are both 64 lowercase hex chars and validate() cannot tell them apart, which is exactly why this needs to be written down rather than left to source-reading). Cross-reference the broadcast-audit-fails-closed scope note from the DUR-5 commit (broadcast currently SKIPs pending SIGN-3's canonical-audience answer) so this isn't read as an oversight.
  
  SCOPE: CONTRACTS-ONDISK.md only.
  _Proof: grep -n "bus.audit" CONTRACTS-ONDISK.md_
- [ ] None · WAL-APPLIER-DOC-STALE: internal/wal/log.go Applier doc contradicts replay.go on Apply errors — wal, P2
  internal/wal/log.go's Applier interface doc says returning an error "from recovery it makes Open fail". internal/wal/replay.go does not do that -- it DISCARDS the entry and counts it as acknowledged loss, which is what invariant 6 requires (recovery always reaches a running server). Two appliers were written against the stale wording. RELAY-15 could not fix it (outside its file-ownership boundary) and states the correct behaviour in its own rationale. Correct the interface doc, and check other appliers (internal/invite, internal/auth, internal/hub) for the same inherited claim.
- [ ] None · Shutdown-timeout path can release the data-dir lock while handlers are still running — durability, P2
  In cmd/agent-bus/main.go waitAndShutdown, when srv.Shutdown exceeds shutdownGrace the code calls srv.Close(), which does NOT wait for in-flight handlers to return. run()'s deferred lock.Release() then drops the data-directory flock while a handler may still be running. Harmless TODAY because no handler writes to the data dir -- but it becomes a real hole the moment DUR-9 puts WAL writes behind those handlers: a second server could acquire the lock while the first is still mid-write. Fix direction: hold the lock until every handler has genuinely returned, or make the data-dir writers refuse to run once shutdown has passed the grace period. Also fold in the DUR-8 security pass's remaining P2: internal/dirlock.Acquire could fstat the opened lock file and require S_ISREG, closing the FIFO/directory-at-the-lock-path cases (both already fail closed today -- FIFO via EINVAL on Truncate, directory via EISDIR -- but the flock is taken on the FIFO first, and O_RDWR-on-a-FIFO-not-blocking is Linux-specific).
  
  Filed by the DUR-8 feature-runner. Related to DUR-9, which will edit the same sequence in run().
  _Proof: TBD by whoever picks this up -- a crash/shutdown-injection test asserting the lock is held until all in-flight handlers return, plus a dirlock test asserting Acquire refuses a non-regular file at the lock path_
- [ ] None · Close the two coverage gaps the security gates declared UNVERIFIED on the seq-floor/data-dir work: the exhaustive describeLogRepair fail-open analysis and the high-floor blast-radius sweep — test, P2
  HONEST COVERAGE RECORD, filed so these are not mistaken for verified-clean. Both security gates on be447589-6583-4d5c-a9d4-ec9d9fef0f1c dispatched sub-agents that DID NOT RETURN before the gates were asked to wrap up, and both said so explicitly rather than implying full coverage. This task carries what they could not finish.
  
  UNVERIFIED ITEM 1 -- exhaustive `describeLogRepair` fail-open analysis. cmd/agent-bus/logrepair.go now carries five arms (Quarantined, Truncated, Rewritten, LostUnidentified, and the narrow `rec.Records == 0 && rec.NextIndex > 1` emptied-log arm). Two arms have ALREADY been added and removed for permanently bricking a healthy directory (the NextIndex accounting arm and the MissingRecords arm -- both removals are documented in place at logrepair.go:106-135 and :160-175). No exhaustive analysis exists of which of the SURVIVING arms are one-shot versus durable across restart, so the per-shape claim ("TRUNCATED->ONE-SHOT, INTERIOR->ONE-SHOT, QUARANTINE->every start") rests on targeted tests rather than a sweep.
  
  UNVERIFIED ITEM 2 -- full high-floor blast-radius sweep. What a floor near `maxPlausibleSeqFloor` does to every downstream consumer was not swept. Overlaps the JSON safe-integer task; do that one first if both are picked up.
  
  ITEM 3 -- RECORDED, AND IT IS NOW STALE. One gate flagged, in its own words, as "UNVERIFIED BY ME, NOT SUSPECTED BROKEN" (preserve that distinction -- it is not a suspicion of a defect): whether `MissingRecords` in `highestIndexSeen` (cited as cmd/agent-bus/logrepair.go:81-86) can ever extend PAST the last surviving record, since if it can, that addition inflates `highestIndexSeen` and masks a real loss -- the one fail-open that would matter.
  
      spec-keeper CHECKED THIS AT HEAD 16da89f AND IT NO LONGER APPLIES AS WRITTEN: `highestIndexSeen` HAS BEEN REMOVED. `grep -n 'highestIndexSeen|MissingRecords' cmd/agent-bus/logrepair.go` returns only COMMENTS -- :106 ("MissingRecords IS NOT COUNTED, and this is the second arm removed for the same reason on the same day") and :162 (documenting that the highestIndexSeen arm was removed 2026-08-08 because it bricked healthy directories). Neither symbol is referenced in any live code path under cmd/agent-bus. So the specific fail-open the gate could not rule out is not reachable, because the code that would have contained it is gone.
  
      THE UNDERLYING QUESTION SURVIVES THE REMOVAL AND IS ALREADY TRACKED: distinguishing "hole because records were lost" from "hole because a reservation was burned" needs the highest index a record actually CONSUMED, which is exactly task 9fd58deb-6fb8-4d4e-8bf1-6df01329c3b2 ("Expose on wal.Recovered the highest index a record actually CONSUMED"). logrepair.go:126-135 says so in the code. Do NOT re-file it here.
  
  PROOF STATE OBSERVED (spec-keeper, HEAD 16da89f, not assumed): `bash scripts/proof-check.sh '<proof_cmd>'` -> verdict=VACUOUS, exit 4, tests_run=0, empty_pkgs=2 ("the -run pattern matched nothing, so this command proves nothing"). This proof is VACUOUS TODAY, NOT RED, because neither named test exists -- it names the evidence that must be created. Per CLAUDE.md this task MUST NOT be completed while the proof is still VACUOUS.
  _Proof: go test -race -count=1 -run 'TestDescribeLogRepairArmsSurviveARestartAsDocumented|TestHighSeqFloorBlastRadius' ./cmd/agent-bus ./internal/hub_
- [ ] DUR-12-FU-V1LAUNDER · DUR-12-FU-V1LAUNDER: v1-format WAL laundering re-signs forged CRC32C records with the real MAC key — security, P1
  P1, security, HIGH (from DUR-12 security gate, VERIFIED BY RUNNING IT). internal/wal/log.go:256-273 branches to the version 1 path on detectFormat alone, and internal/wal/mackey.go:372-374 returns a keyless v1 codec without consulting the key file. So an attacker who can drop a CRC32C-forged version 1 file at bus.wal gets its records re-framed and SIGNED WITH THE REAL MAC KEY -- forging without ever touching wal-mac.key. Capability required is directory w+x (replace a file), which does NOT require reading the 0600 key. It grants no new class of attacker (directory write already allows planting a key+log pair wholesale) but it destroys FORENSICS: forged records become indistinguishable from genuine ones even to someone holding the original key. THE OBVIOUS FIX IS UNSAFE AS STATED AND THE TASK MUST SAY SO: "refuse the v1 path when a key file already exists" strands a legitimate crash-mid-upgrade redo, which leaves exactly that state (key created, log still v1, rename not yet done). A correct fix must distinguish those two, e.g. by staging the key file and only moving it into place after the upgrade rename. Directly relevant to the in-flight Dockerfile/docker-compose work: a bind-mounted volume with loose permissions is the enabler, and MkdirAll does not tighten an existing directory. Reference: PROTOCOL.md section 7 "Known residual".
- [~] None · Fix WAL recovery reissuing a discarded tail record index (invariant 1 violation, NOT a narrowing) -- BLOCKED on DUR-12 — durability, P0, in progress
  internal/wal/recover.go (RepairLog, Repair.NextIndex comment ~lines 56-91) documents and internal/wal/recover_test.go:440 and crash_injection_test.go (e.g. ~line 960) currently ASSERT AS CORRECT that a damaged TAIL record has its index REISSUED on the next successful write: "That record is discarded and its index is reissued. If it had been acknowledged, an id a client saw is handed out again." CLAUDE.md invariant 1 ("ids are never reused, including across restarts") was reaffirmed by the user on 2026-08-02 WITHOUT NARROWING (DECISIONS.md, "Five decisions" #3, and the addendum to ID-2-WIRING-SCHEMA): "Recovery may not reissue an index it has already handed out, even for a record it discards... when recovery discards a record the sequence advances past the hole, it never rewinds." THIS IS THEREFORE A DEFECT TO FIX, NOT A NARROWING TO DOCUMENT. The question this task was originally filed to raise -- "is the quarantined-tail index reissue observable, should we narrow invariant 1?" -- is CLOSED; invariant 1 stands unmodified.
  
  Contrast deliberately drawn by the user: invariant 4 (durability) WAS narrowed on 2026-08-02 -- that narrowing was a choice made up front and recorded as such. This reissue behaviour was discovered AFTER THE FACT (by reviewer and security, on DUR-11) and is REJECTED, not accepted.
  
  FIX REQUIRED in internal/wal: when RepairLog discards a damaged tail record (whether length-only damage, a torn frame, or bit rot indistinguishable from an interrupted write), the sequence/index counter must advance PAST the hole and never be handed to the next Append -- i.e. NextIndex must be one past the highest index EVER OBSERVED IN THE FILE, including a discarded record, not one past the highest SURVIVING record. This likely touches: Repair.NextIndex computation, the length-field-repair and torn-frame-truncation paths in recover.go, and every existing test that currently asserts reissue as wanted behaviour (recover_test.go:440, crash_injection_test.go ~671/838/960/982, replay_crash_test.go ~345/512) -- those tests encode the REJECTED behaviour and must be flipped to assert no-reissue, not left in place.
  
  PRIORITY RAISED TO P0 by triage on CONSEQUENCE, not by explicit user P0 label -- flagging this so the user can overrule: a shipped violation of a load-bearing identity invariant is exactly what invariant 1 exists to prevent (ids repeating), and the code is live today, not merely planned.
  
  BLOCKED ON DUR-12: this task is in internal/wal, the same package/recovery loop DUR-12 (HMAC-SHA256 MAC replacing CRC32C, ondisk-format-version=2) is actively rewriting per the 2026-08-02 decision, and DUR-12s own description says it should SIMPLIFY the torn-tail heuristic under a strong MAC. Do NOT dispatch this task until DUR-12 lands, to avoid two agents rewriting internal/wal/recover.go concurrently.
  
  RELATED: DUR-11 (884d3da4, commits 0c122fa/6bb9f6c) implemented exactly the reissue behaviour this task now rejects; see DUR-11s notes for the reconciliation.
  _Proof: go test -race -run TestWALRepairDoesNotReissueDiscardedIndex ./internal/wal_
- [ ] None · Codec forward-compat comment contradicts the code (pre-existing) -- reconcile or fix — durability, P2
  Pre-existing inconsistency in internal/wal/log.go (~line 476): a comment claims a payload field can be added without a format-version bump, but decodePayload uses DisallowUnknownFields, so an OLD binary actually refuses to start on a NEWER log written by a binary with an extra field -- the opposite of what the comment promises. Decide which is wrong -- the comment (fix the wording to match reality: adding a field DOES require a version bump or an explicit unknown-field-tolerance change) or the code (relax DisallowUnknownFields if forward-compat is actually wanted) -- and make them agree. Standing rule to record alongside whichever fix: a format change must ship WITH a recovery/compat path for logs written by the previous format, never added after the fact.
  _Proof: grep -n "DisallowUnknownFields\|format-version\|without a" internal/wal/log.go_
- [ ] DUR-12-VERIFY · DUR-12-VERIFY: verify the WAL MAC upgrade against a real running bus (paired not-yet-live task) — durability, P2
  P2, durability. THE PAIRED NOT-YET-LIVE TASK. DUR-12 is complete as CODE-ONLY. Nothing is deployed and no real bus has been upgraded. This task carries the observable-behaviour proof: rebuild the binary, start a bus against a data directory holding a genuine pre-DUR-12 version 1 bus.wal, and verify from OUTSIDE the test suite that (a) the upgrade runs and is logged, (b) the .v1-<ns> backup exists and holds the original bytes, (c) the bus serves its recovered history, (d) deleting wal-mac.key afterwards makes the next start refuse with an error naming the key path, and (e) restoring it makes the bus start again. ON-DISK FORMAT CHANGE -- say plainly in the description that EXISTING LOGS ARE AFFECTED: any bus started on a new binary silently upgrades its log to format version 2 and CANNOT BE DOWNGRADED without restoring the .v1-<ns> backup and losing everything written since.
- [ ] DUR-12-FU-KEYMODE · DUR-12-FU-KEYMODE: loadMACKey never checks the key file's permission mode — security, P3
  P3, security. Security MEDIUM: loadMACKey (mackey.go:100-131) never checks the key file's MODE. A key that has been chmod'd 0644 loads silently. Warn or refuse.
- [ ] None · Stale comment at main.go:236-245 still argues the reverted refuse-to-start policy for wal.Open errors — durability, P1
  cmd/agent-bus/main.go:236-245 (the comment on the wal.Open error branch) still says the FATAL path is "deliberately so", "Never degrade to that", and describes "the one damage case that is survivable -- a provably torn tail" as if RepairTail were the only sanctioned recovery. That branch is now UNREACHABLE for the whole quarantine class: DECISIONS.md 2026-08-02 ("Availability over retention") sanctions discarding a checksum-failing LAST record via quarantine so the server always restarts, rather than refusing to start. The comment was written against, and still argues for, the OLD refuse-to-start policy that decision reverted. This is the exact stale-doc pattern that let TestServerOpensWALOnStartRefusesACorruptLog outlive the decision that killed it (see the companion follow-up on that test name). Fix: rewrite the comment to describe what actually remains fatal here (e.g. a torn tail RepairTail itself cannot resolve, or an I/O error unrelated to corruption) now that quarantine handles the checksum-failing-last-record case, and cross-reference the 2026-08-02 decision instead of contradicting it.
  _Proof: grep -q "2026-08-02" cmd/agent-bus/main.go && ! grep -n "Never degrade to that" cmd/agent-bus/main.go_

### EPIC HANDOVER — HANDOVER: make agent-bus ready to hand to a human

- [ ] HANDOVER-CHECK · HANDOVER-CHECK: one command that tells you the health of this repo, plus its recorded output at a named sha — tooling, P1
  Audience: maintainer (an operator inherits it later via the runbook).
  
  Priority P1 justification: today there is no single command, no CI, and the only "green" signal we have (`go test -race ./...` = 16 packages ok) was captured while five other agents ran `go test` against the same checkout -- recon saw three packages FAIL in the same window and could not confirm it. Every downstream HANDOVER doc that says "the suite passes" would be repeating an unverified claim. This is a lie-prevention task, hence P1, and it is deliberately below the open P0s (INVITE-GATE, MTLS-VERIFY-FU-DOCSCHEME, DOCS-2, MTLS-MIGRATE 59883178, e120153b).
  
  Definition of done:
  - /mnt/sdb4/mike/mike/source/agent-bus/scripts/check.sh exists: go build ./..., go vet ./..., the CORRECT gofmt form (`test -z "$("$(go env GOROOT)/bin/gofmt" -l .)"` -- not bare gofmt, not exit-status-judged), go test -race ./..., and it exits non-zero on any failure. It prints a per-package table and a TOTAL SKIP COUNT (top-level *and* nested), because a suite that skips 42 results and reports ok is the failure mode this repo actually has.
  - It runs against a throwaway data dir under /tmp, never the tracked data/.
  - It is executed ONCE with the repo quiet -- no other agent running go test against this checkout -- at a named commit, and that transcript (sha, per-package result, skip count, wall time) is recorded in AGENT_LOG.md.
  
  CAVEAT (load-bearing): 69eb6f56 (proof-check recursion) means check.sh must NOT itself invoke scripts/proof-check.sh.
  
  Depends on: the separately-filed proof-check top-level-counting P1 (public_id cea09b96-72db-40f1-84b4-c2e227eae1cf) -- recorded as a real blocks relation (cea09b96 blocks this task) per the epic critical path, even though HANDOVER-CHECK proof_cmd itself does not literally invoke that fix; it is the epic-wide evidentiary prerequisite (see planner disagreement (d): it outranks everything else in this epic). If it lands first, check.sh should call the fixed counter rather than reimplement it.
  
  Parallel-safe: NO. It requires an otherwise-idle checkout. Schedule it alone.
  
  Model: sonnet for the script, but the isolation run and its interpretation want opus judgment if the anomaly reproduces. Suggest sonnet with an explicit instruction to escalate on any FAIL.
  
  Size: half a day.
  
  RED verification observed (2026-08-08): scripts/check.sh does not exist at HEAD -- trivially RED, file absent.
  _Proof: bash scripts/proof-check.sh 'bash scripts/check.sh'_
- [ ] HANDOVER-README · HANDOVER-README: README stops telling a human things that are false — docs, P1
  Audience: both.
  
  Priority P1 justification: the "What works today" block hands a human two plaintext curl -s localhost:8080/healthz commands against a TLS-only listener; they return a bare 400 Bad Request from net/http. The Requirements section states Go 1.19.4 as THE requirement while CLAUDE.md says the container pins the toolchain and the E2E plan needs 1.20+. And the Quickstart's <64-hex-from-invite> placeholder has no instruction anywhere for obtaining it, while `agent-busctl agents` is shown with no --as.
  
  SCOPE BOUNDARY -- this is the residue that existing tasks do NOT cover. 5f8e0cba owns "bus http://" in README/AGENT_PROTOCOL, "listener is still plaintext" in PROTOCOL.md, and "until mutual TLS lands" in README. cb4fd330 owns the AGENT_PROTOCOL half. DISCOVERY-DOC-FU-README (be3c84f3) owns the stale three-field /v1/info body. NONE OF THEM TOUCH the curl block, the Go-version claim, or the unrunnable Quickstart. This task owns exactly those three and nothing else.
  
  Definition of done: the plaintext curl demonstrations are replaced with something a human can actually run (or removed and pointed at the runbook); the Go-version paragraph states what is true at HEAD and defers the pin to DEPLOY-4; the Quickstart either works end-to-end or is replaced by a pointer to RUNBOOK.md; links to INVARIANTS.md and KNOWN_ISSUES.md added.
  
  BLOCKED (hard, same file, both ahead of this in priority): 5f8e0cba and cb4fd330 must land first.
  README.md IS ALSO CONTENDED BY be3c84f3 and f0ef1ed9 (stale CONTRACTS pointer at README.md:88). ALL FOUR must be serialised, with HANDOVER-README LAST.
  
  Depends on: HANDOVER-MAP-DOC and HANDOVER-REGISTER (the links must resolve); 5f8e0cba and cb4fd330 (hard, same-file, must land first).
  
  Parallel-safe: NO -- README.md is contended by 5f8e0cba, be3c84f3, f0ef1ed9. Serialise all four; this one last.
  
  Model: sonnet (writing-heavy, scope is fixed by the task).
  
  Size: two hours.
  
  RED verification observed (2026-08-08): confirmed via a REAL check (not file-absence), not incidental: the proof_cmd shell fragment (negated grep for the plaintext-curl string, plus greps for the two new doc links) currently exits 1 -- README.md:96 contains the exact plaintext-curl string.
  _Proof: bash scripts/proof-check.sh '! grep -n "curl -s localhost:8080/healthz" README.md && grep -n "KNOWN_ISSUES.md" README.md && grep -n "INVARIANTS.md" README.md'_
- [ ] HANDOVER-CONTRIBUTING · HANDOVER-CONTRIBUTING: CONTRIBUTING.md -- how this repo is actually developed, and how to work on it as a human — docs, P2
  Audience: maintainer. Priority P2 (structural).
  
  Justification: the development model here is unusual enough that a competent Go maintainer would violate it on day one. They need to know: task state is a remote Spec Server, not a file; SPEC.md is generated and hand-editing it is destructive; proof-check.sh exists because `go test -run <nonexistent>` exits 0; and the pathspec-commit trap that has produced four mis-titled commits and one un-compilable main.
  
  Definition of done: covers (a) the agent chain and which parts are ceremony vs load-bearing; (b) Spec Server access for a human: scripts/spec-cloud.sh, that credentials live outside the repo at a path that must be handed over separately, how to rotate them, the local docker compose fallback, and what to do if they are lost entirely; (c) the commit rules -- explicit pathspec, and that a pathspec commit takes the WORKTREE not the index; (d) "I just want to edit some Go code" -- the minimum honest path, which is scripts/check.sh plus a real proof command.
  
  RISK / OPEN QUESTION (record, needs a decision before this task starts): it is NOT KNOWN whether scripts/spec-cloud.sh credentials (which live outside the repo at /mnt/sdc/mike/claude-scratch/spec-cloud-creds.env) can transfer to a new maintainer at all. If they cannot, this task grows from "document the existing access path" into "stand up your own Spec Server and import the export", which is a materially bigger task. This must be resolved with the user before implementation starts -- do not assume either answer.
  
  Depends on: HANDOVER-CHECK (must reference a script that exists). Parallel-safe: YES.
  
  Model: sonnet. Size: half a day.
  
  RED verification observed (2026-08-08): CONTRIBUTING.md does not exist -- trivially RED, file absent.
  _Proof: bash scripts/proof-check.sh 'grep -n "scripts/spec-cloud.sh" CONTRIBUTING.md && grep -n "SPEC.md is generated" CONTRIBUTING.md && grep -n "takes the WORKTREE" CONTRIBUTING.md && grep -n "scripts/check.sh" CONTRIBUTING.md'_
- [ ] HANDOVER-BACKLOG-RECONCILE · HANDOVER-BACKLOG-RECONCILE: the inherited backlog stops lying about what is in flight — process, P2
  Audience: maintainer. Priority P2 -- FILED BUT DELIBERATELY OFF THE HANDOVER CRITICAL PATH (planner's explicit recommendation; see also disagreement (f) in the planner's notes).
  
  Justification: 15 tasks sit in_progress, several already shipped. A recipient cannot tell what is being worked on. But the fix is large and the instrument (proof-check.sh) is itself broken in ways that would produce confidently wrong reconciliation.
  
  WHY THIS IS OFF THE CRITICAL PATH (record explicitly, per planner + user instruction):
  - It is blocked on two tooling fixes (521d68b5, a9a433dd) -- reconciling against a broken evidence instrument produces confidently wrong results.
  - It MUTATES SHARED TASK STATE that P0 work depends on.
  - It COMPETES FOR SPEC-KEEPER, the single agent permitted to mutate task state.
  - A SEPARATE AUDIT of the 15 in_progress tasks is running right now (as of filing, 2026-08-08) and covers the cheap half of this work -- do not duplicate it.
  
  SPLIT POINT if attempted (task is over a day -- FLAG): split by epic (DUR/MTLS/IDEM/other).
  
  Definition of done: each of the 15 in_progress tasks is either completed with a real commit_sha and a quoted proof-check.sh verdict, or reset to todo with a status_note stating precisely what remains; SPEC.md mirror refreshed. spec-keeper owns this -- it is the only agent permitted to mutate task state.
  
  Depends on: 521d68b5 (proof-check cannot distinguish executed from asserted) and a9a433dd (conjunction-masking vacuous proofs). Related but distinct: fc8cd234 backfills MISSING proof_cmds; this reconciles STATUS. A third concern -- that 3 of 4 sampled stored proof_cmds were WRONG -- is a separate sweep and should be its own task filed after 521d68b5 lands, not smuggled in here.
  
  Parallel-safe: NO (mutates shared task state).
  
  Model: sonnet, driven by spec-keeper. Size: over a day -- FLAG (see split point above).
  
  RED verification observed (2026-08-08): confirmed via a REAL check (not file-absence) -- ran the exact proof_cmd's python check against the live Spec Server export: currently 15 tasks have status=in_progress, which is > 3, so the proof correctly exits 1 (RED) today.
  _Proof: bash scripts/proof-check.sh 'bash scripts/spec-cloud.sh -s "/api/v1/projects/agent-bus/export?format=json" | python3 -c "import json,sys; t=json.load(sys.stdin)[\"tasks\"]; n=[x for x in t if x[\"status\"]==\"in_progress\"]; print(len(n)); sys.exit(0 if len(n)<=3 else 1)"'_
- [ ] HANDOVER-RUNBOOK-SMOKE · HANDOVER-RUNBOOK-SMOKE: an executable cold-start -- certs, invite, two agents, one message — tooling, P1
  Audience: operator (maintainer benefits).
  
  Priority P1 justification: the README Quickstart presents a recipe that cannot be followed -- that is a lie, not merely a gap. Nobody can start a bus without tribal knowledge about data directories, self-signed certificate generation, fingerprints and identity directories.
  
  Definition of done: scripts/handover-smoke.sh runs from a clean state against a throwaway /tmp data dir: starts a bus, extracts the certificate fingerprint from the WARN line the server emits, mints an invite via cmd/agent-bus/invite.go, enrols two agents over pinned TLS, sends a directed message, receives it on the watcher, and shuts down cleanly -- exiting non-zero at any step. It must not touch the tracked data/.
  
  SPLIT POINT (likely over a day -- FLAG). If attempted, split as:
    1. Cert generation + fingerprint extraction + single-agent enrol.
    2. Second agent + send/watch/teardown.
  
  Depends on: DEPLOY-REDEPLOY (f801d128, currently in_progress) -- it already proves "two agents exchange a message on a fresh Compose bus". Do NOT re-derive that; consume it. If it has landed, this task encodes its transcript; if not, this task is blocked on it.
  
  Parallel-safe: NO -- needs its own bus and data dir, and must not race other bus-running agents.
  
  Model: opus (the certificate/fingerprint/invite sequencing is where this will go wrong).
  
  Size: likely over a day -- FLAG (see split point above).
  
  RED verification observed (2026-08-08): scripts/handover-smoke.sh does not exist -- trivially RED, file absent.
  _Proof: bash scripts/proof-check.sh 'bash scripts/handover-smoke.sh'_
- [ ] HANDOVER-WIRED · HANDOVER-WIRED: assert and document which packages are present but not wired — test, P1
  Audience: maintainer.
  
  Priority P1 justification: this is the repo's most expensive lie and it is told by the FILE TREE, not by prose. internal/relay is a complete federation plane with zero production importers; internal/invite is a 1183-line crash-tested store whose Redeem has zero non-test callers; verifySignedMessage has zero production callers. A maintainer reading `go list ./...` reasonably concludes all sixteen packages are live. None of the existing tasks state the unwired set as a set.
  
  Definition of done:
  - A guard test -- following the existing repo convention of tests that assert a GAP (TestReadDoesNotYetVerifyReceivedMessages, cmd/agent-bus/tlslisten_test.go:823 pinning NoClientCert) -- that enumerates the currently-unwired surfaces and FAILS when one of them gains a production caller. It therefore goes RED the day INVITE-GATE or the relay wiring lands, forcing the doc to be updated instead of rotting.
  - A short "Present but not wired" section listing each entry with its owning Spec task id, consumed by HANDOVER-MAP-DOC and HANDOVER-REGISTER.
  
  Depends on: none in this epic. Parallel-safe: YES (new test files only).
  
  Model: opus -- deciding what counts as "wired" and where the guard lives is a judgment call, and a sloppy guard is worse than none.
  
  Size: half a day.
  
  RED verification observed (2026-08-08): the named test TestUnwiredSurfacesHaveNoProductionCaller does not exist yet. Running the proof now through proof-check.sh gives verdict=VACUOUS (not a bare RED) -- go test -run on a non-existent test name prints "ok ... [no tests to run]" and exits 0, but proof-check.sh's vacuous-proof guard (84b76d5e) correctly catches it: `proof-check: verdict=VACUOUS class=test exit=0 tests_run=0 top_level=0 skipped=0 failed=0 empty_pkgs=3`. This is the expected/correct pre-task state -- note it precisely as VACUOUS, not RED, when reporting.
  _Proof: bash scripts/proof-check.sh 'go test -race -run TestUnwiredSurfacesHaveNoProductionCaller ./internal/relay ./internal/invite ./client'_
- [ ] HANDOVER-REGISTER · HANDOVER-REGISTER: KNOWN_ISSUES.md, the known-defect register — docs, P1
  Audience: both -- maintainer for the causes, operator for the blast radius.
  
  Priority P1 justification: there is no known-defect register at all, and the defects are only visible as Spec Server tasks behind cloud credentials that live outside the repo. A human who clones this cannot discover that a boundary-exact WAL truncation reissues sequence numbers, or that the roster-brick DoS is unmitigated because INVITE-GATE never landed. Handing over undisclosed known data-loss and availability defects is the most serious form of the repo lying.
  
  Definition of done: CURATED, HARD-CAPPED AT 20 ENTRIES, symptom-first. Each entry: what a user or operator would OBSERVE, blast radius, class (data-loss / security / availability / functionality), current mitigation or workaround, owning Spec task public_id. Must include at minimum:
  - Seq-floor migration guard blind at record-boundary-exact truncation (22/22 boundaries measured, 13 reissued end-to-end); real fix blocked on 9fd58deb. Cross-reference 2a38cdec, which owns the doc correction -- do not duplicate its text.
  - Roster-brick DoS, gated on INVITE-GATE.
  - Enrolment is not invite-gated (InviteRequired: false); /v1/enroll is on the unauthenticated allow-list.
  - Server presents no client-cert requirement (NoClientCert); CertBindings declared but never written.
  - Enrol idempotency is in-memory only -- a retry straddling a restart mints a second agent id. /v1/session/begin and /v1/session/complete take no idempotency key.
  - Recipient signature verification is absent (three independent causes).
  - POST /v1/broadcast and agent-busctl broadcast return 501.
  - The relay/federation plane is unwired.
  - Upgrade discards message history (record v1 -> v2, no migration, breaks both ways).
  - Idempotency-Key header vs JSON body-field divergence (internal/idem/key.go:8-12 vs every live route; idem.FromRequest is dead code).
  - The backlog's own reliability -- see HANDOVER-BACKLOG-RECONCILE (filed, off critical path).
  
  Depends on: HANDOVER-WIRED (HARD), HANDOVER-MAP-DOC (SOFT -- the map's NOT-ENFORCED rows are the register's security entries).
  
  Parallel-safe: YES (new file).
  
  Model: OPUS -- ranking blast radius and deciding what a symptom looks like from outside is judgment.
  
  Size: three-quarters of a day.
  
  RED verification observed (2026-08-08): KNOWN_ISSUES.md does not exist -- trivially RED, file absent. The -le 20 clause makes the curation constraint mechanical rather than aspirational; confirmed the pinned strings ("record-boundary-exact truncation", "9fd58deb") do not occur in any tracked non-SPEC.md file, so scoping to KNOWN_ISSUES.md needs no tightening.
  _Proof: bash scripts/proof-check.sh 'test -s KNOWN_ISSUES.md && test "$(grep -c "^### " KNOWN_ISSUES.md)" -le 20 && grep -n "record-boundary-exact truncation" KNOWN_ISSUES.md && grep -n "9fd58deb" KNOWN_ISSUES.md && grep -n "INVITE-GATE" KNOWN_ISSUES.md'_
- [ ] HANDOVER-DECISIONS-INDEX · HANDOVER-DECISIONS-INDEX: generated table of contents for DECISIONS.md — tooling, P2
  Audience: maintainer. Priority P2 (structural).
  
  Justification: 4,338 lines of append-only sections from a dozen agents; the single most valuable maintainer artefact and currently unnavigable. NO HISTORY REWRITE -- several entries are dated corrections of earlier entries, and that sequence is itself information.
  
  Definition of done: scripts/gen-decisions-index.sh emits DECISIONS-INDEX.md from DECISIONS.md's `^## ` headings (date, topic, line number). Nothing in DECISIONS.md changes.
  
  Depends on: none. Parallel-safe: YES (reads DECISIONS.md, writes only new files -- safe even while other agents append to it, though the index must be regenerated after they do).
  
  Model: sonnet. Size: two hours.
  
  RED verification observed (2026-08-08): scripts/gen-decisions-index.sh and DECISIONS-INDEX.md do not exist -- trivially RED, files absent.
  _Proof: bash scripts/proof-check.sh 'bash scripts/gen-decisions-index.sh > /tmp/di.md && diff -q /tmp/di.md DECISIONS-INDEX.md'_
- [ ] HANDOVER-FRONTDOOR · HANDOVER-FRONTDOOR: CLAUDE.md tells a human where to start instead of dropping them into an agent protocol — docs, P2
  Audience: maintainer.
  
  Priority P2 justification: README says of CLAUDE.md "read this first", and CLAUDE.md is 461 lines of agent operating procedure. That is a navigational defect, not a false statement -- hence P2, not P1.
  
  Definition of done: a short block at the top of CLAUDE.md: "If you are a human, start here" -> README.md -> RUNBOOK.md -> INVARIANTS.md -> KNOWN_ISSUES.md -> CONTRIBUTING.md, with one line each on what it answers. No other change to CLAUDE.md.
  
  Depends on: all five linked docs existing. Parallel-safe: YES (nothing else in this epic touches CLAUDE.md) -- but it is the epic's natural last task.
  
  Model: sonnet. Size: 30 minutes.
  
  RED verification observed (2026-08-08): confirmed via a REAL check (not file-absence): `head -20 CLAUDE.md | grep -n "If you are a human"` and the KNOWN_ISSUES.md companion grep both currently produce no output (exit 1) -- the head -20 bound is what stops it matching incidentally anywhere in 461 lines.
  _Proof: bash scripts/proof-check.sh 'head -20 CLAUDE.md | grep -n "If you are a human" && head -20 CLAUDE.md | grep -n "KNOWN_ISSUES.md"'_
- [ ] HANDOVER-RUNBOOK-DOC · HANDOVER-RUNBOOK-DOC: RUNBOOK.md narrates exactly what the smoke script does — docs, P1
  Audience: operator.
  
  Priority P1 (same justification as HANDOVER-RUNBOOK-SMOKE; this is the half a human reads).
  
  Definition of done: every command in RUNBOOK.md is copied from the smoke script's actual invocations, not written from memory. Includes a PROMINENT SCOPE STATEMENT: loopback evaluation only -- this bus must not be placed on a real interface until INVITE-GATE and MTLS-CLIENTAUTH land (with the KNOWN_ISSUES.md entries linked). Includes teardown and the `docker compose down -v` data-destruction warning.
  
  OPERATOR SCOPE LIMITATION (user decision, must appear here and in RUNBOOK.md itself): the operator half of the HANDOVER epic is scoped to LOOPBACK EVALUATION ONLY. A genuine operator handover is a separate future epic, gated on INVITE-GATE + MTLS-CLIENTAUTH.
  
  Depends on: HANDOVER-RUNBOOK-SMOKE, HANDOVER-REGISTER. Parallel-safe: YES (new file) once the smoke script exists.
  
  Model: sonnet. Size: three hours.
  
  RED verification observed (2026-08-08): RUNBOOK.md does not exist and scripts/handover-smoke.sh does not exist -- trivially RED, both files absent (compounding).
  _Proof: bash scripts/proof-check.sh 'bash scripts/handover-smoke.sh && grep -n "loopback evaluation only" RUNBOOK.md && grep -n -- "--bus-fingerprint" RUNBOOK.md && grep -n "INVITE-GATE" RUNBOOK.md'_
- [ ] HANDOVER-MAP-DOC · HANDOVER-MAP-DOC: INVARIANTS.md -- each of the 11 invariants, its real status at HEAD, and the evidence — docs, P1
  Audience: maintainer.
  
  Priority P1 justification: CLAUDE.md's design contract reads as a description of the system. It is a description of the INTENT. Invariant 3 (invite-only) is NOT ENFORCED -- internal/httpapi/discovery.go:263 advertises InviteRequired: false. Invariant 11 (mutual TLS) is HALF enforced -- cmd/agent-bus/tlslisten.go:109 sets ClientAuth: tls.NoClientCert and a test pins it there. Invariant 10 is partial (enrol idempotency is an in-memory map; session begin/complete take no key at all). A maintainer who trusts CLAUDE.md will build on guarantees that do not exist. That is the epic's core problem in its purest form.
  
  Definition of done: one row per invariant (and per named sub-clause where they diverge, e.g. 3a enrolment vs 3b session signing), each carrying: status in {ENFORCED, PARTIAL, NOT ENFORCED}, the NAMED TEST that proves the status, a file:line anchor, and for anything not ENFORCED the owning Spec task public_id. Header stamped with the commit sha it was measured at. Must record the two nuances recon surfaced, because losing them re-creates false alarms: the WAL index floor triggers on !sealedClean() (not on damage -- it is NOT blind), and cmd/agent-bus/seqfloorrestart_test.go:198-217 only t.Logf's a reissue and labels itself a KNOWN GAP.
  
  SPLIT POINT (task is at the one-day size limit -- FLAG). If the implementer runs long, split sequentially (same file, so the two passes are NOT parallel):
    1. Invariants 1, 2, 4, 5, 6 (id authority + durability plane).
    2. Invariants 3, 7, 8, 9, 10, 11 (auth, client, crypto, idempotency, transport).
  
  Depends on: HANDOVER-WIRED (HARD -- the NOT-ENFORCED rows cite its enumeration), HANDOVER-CHECK (SOFT -- supplies the sha and the honest suite status).
  
  Model: OPUS. This is a correctness judgment across auth, durability and id authority; a wrong row here poisons everything downstream.
  
  Size: at the one-day limit -- FLAG (see split point above).
  
  UPDATED 2026-08-08 (spec-keeper, filing the CONTEXT epic): the file this task targets is NO LONGER
  absent. A separate, ungated change (reviewed 2026-08-08, CHANGES-REQUESTED on that change but not on
  this task) split CLAUDE.md's invariants section out into INVARIANTS.md (a single 220-line file, rule
  + reasoning together), then added the 11 `### Invariant N -- <title>` headings this task's proof
  requires. INVARIANTS.md is ONE file, not a new one this task creates from scratch: it already carries
  the CONTRACT + REASONING; this task's job is to add per-invariant STATUS/EVIDENCE blocks UNDER the
  existing headings, not to create a new document.
  
  RE-OBSERVED PROOF STATE (2026-08-08, replaces the "file absent" RED evidence above, which is now
  STALE and must not be quoted as current): `test -s INVARIANTS.md` PASSES (18,577 B). `grep -c
  "^### Invariant " INVARIANTS.md` now returns 11, so `test 11 -le "$(grep -c ...)"` PASSES -- the
  heading half of the proof is satisfied and was previously broken (measured at 0 headings). The
  evidence half is STILL RED, genuinely: `grep -n "NOT ENFORCED" INVARIANTS.md`, `grep -n
  "tls.NoClientCert" INVARIANTS.md` and `grep -n "InviteRequired: false" INVARIANTS.md` all currently
  return NO MATCHES -- the per-invariant status rows this task exists to write have not been added yet.
  So the task's real remaining work is exactly its original definition-of-done (the STATUS/EVIDENCE
  rows), not file creation.
  
  "Parallel-safe: YES (new file, no contention)" is now FALSE and is REMOVED as a claim. INVARIANTS.md
  is a live, shared file with reviewer findings already recorded against its current content (see the
  kind=response note above, which itself recommends this same "add headings, not a new file" resolution
  this update records as decided). Sequence this task's own edits against
  CONTEXT-PLANE-TOC (which indexes INVARIANTS.md's headings once they carry real content) --
  do not run those two concurrently against this file. Depends-on set is otherwise unchanged.
  _Proof: bash scripts/proof-check.sh 'test -s INVARIANTS.md && test 11 -le "$(grep -c "^### Invariant " INVARIANTS.md)" && grep -n "NOT ENFORCED" INVARIANTS.md && grep -n "tls.NoClientCert" INVARIANTS.md && grep -n "InviteRequired: false" INVARIANTS.md'_
- [ ] HANDOVER-DECISIONS-READINGLIST · HANDOVER-DECISIONS-READINGLIST: "the decisions that explain this design, in order" — docs, P2
  Audience: maintainer. Priority P2.
  
  Justification: an index of 100+ headings is navigable but not COMPREHENSIBLE. A curated ~12-entry reading path is what actually transfers the design.
  
  Definition of done: a `## Start here` section at the top of DECISIONS-INDEX.md, ~12 entries in reading order with one line each on what question it answers. Explicitly notes where a later dated entry corrects an earlier one, AT INDEX LEVEL ONLY -- the entries themselves are untouched. The generator (gen-decisions-index.sh) is extended to validate that every anchor named in the reading list resolves to a real `## ` heading in DECISIONS.md, so the list cannot silently rot.
  
  Depends on: HANDOVER-DECISIONS-INDEX. Parallel-safe: NO against it (same file/script).
  
  Model: OPUS -- choosing which twelve of a hundred decisions explain the system is exactly the judgment being handed over.
  
  Size: half a day.
  
  RED verification observed (2026-08-08): DECISIONS-INDEX.md and the --validate-readinglist flag do not exist -- trivially RED, file/flag absent.
  _Proof: bash scripts/proof-check.sh 'grep -n "^## Start here" DECISIONS-INDEX.md && bash scripts/gen-decisions-index.sh --validate-readinglist'_
- [ ] HANDOVER-MAP-CHECK · HANDOVER-MAP-CHECK: make the invariant map executable, not prose — tooling, P2
  Audience: maintainer.
  
  Priority P2 (structural -- the map is already useful without it; this is what stops it rotting).
  
  Definition of done: scripts/check-invariant-map.sh parses INVARIANTS.md, extracts each row's named test, and runs it -- failing if a named test DOES NOT EXIST (the vacuous--run case 84b76d5e already taught this repo to detect) or fails. Wired into scripts/check.sh.
  
  Depends on: HANDOVER-MAP-DOC, HANDOVER-CHECK. Parallel-safe: YES against everything except those two.
  
  Model: sonnet (mechanical). Size: half a day.
  
  RED verification observed (2026-08-08): scripts/check-invariant-map.sh does not exist -- trivially RED, file absent.
  _Proof: bash scripts/proof-check.sh 'bash scripts/check-invariant-map.sh'_
- [ ] HANDOVER-DOCMAP · HANDOVER-DOCMAP: say which of the tracked documents is authoritative, which is generated, and which is a frozen snapshot — docs, P2
  Audience: both.
  
  Priority P2 (structural/navigational).
  
  Justification: 13+ tracked top-level .md files, ~19,400 lines. A human cannot tell that SPEC.md (3,671 lines) is generated and unhand-editable, that AGENT_LOG.md (3,451 lines) is a journal, or that CRYPTO_DEEPDIVE.md and ID2_WIRING_DEEPDIVE.md are point-in-time investigations that were never maintained and may now assert false things.
  
  Definition of done: README's "More docs" section becomes a table with AUDIENCE and STATUS in {authoritative, generated -- do not edit, frozen snapshot -- measured at <sha>, journal -- append-only}; every tracked top-level .md appears. Each frozen-snapshot document gains a one-line banner at its top naming the sha it was measured at and stating it is not maintained.
  
  Depends on: HANDOVER-README (same file). Parallel-safe: NO against README work; YES against everything else.
  
  Model: sonnet. Size: two hours.
  
  RED verification observed (2026-08-08): confirmed via a REAL check (not file-absence): the proof loop currently exits 1 at "MISSING: CRYPTO_DEEPDIVE.md" -- README.md's "More docs" list does not name every tracked .md file, and CRYPTO_DEEPDIVE.md's head has no "not maintained" banner.
  _Proof: bash scripts/proof-check.sh 'for f in $(git ls-files "*.md" | grep -v "^\.claude/"); do grep -qF "$f" README.md || { echo "MISSING: $f"; exit 1; }; done; head -5 CRYPTO_DEEPDIVE.md | grep -qi "not maintained"'_

### EPIC ID — Server-authoritative id minting

- [ ] None · Unify the atomic temp+rename+fsync file writer duplicated between ids.writeBusIDFile and ids.atomicWriteFile — id, P2, msg-fu-suffixfloor-followup
  Deliberately not done inside MSG-FU-SUFFIXFLOOR (public_id 94159d93-fe87-4c3e-b938-86fe7068c787) under CLAUDE.md's no-unrequested-refactor rule; reviewer explicitly endorsed leaving them separate for that task's scope but flagged the duplication as worth its own follow-up. Two byte-identical copies of a durability-critical sequence (temp file create, write, fsync file, rename, fsync dir) now exist in internal/ids -- internal/ids/busid.go's writeBusIDFile and internal/ids/suffixstore.go's atomicWriteFile. If either changes, both must. Unify into one shared helper.
  _Proof: go build ./internal/ids/... && go test -race ./internal/ids/..._
- [ ] None · Amortise the agent-suffixes write: reserve a block of suffixes per name instead of one file rewrite per enrolment — id, P1, msg-fu-suffixfloor-followup
  Flagged by the security gate on MSG-FU-SUFFIXFLOOR (public_id 94159d93-fe87-4c3e-b938-86fe7068c787). Today DurableNameSuffixes.NextSuffix rewrites the WHOLE floor map (O(distinct names ever seen)) and fsyncs twice per issued suffix. The map never shrinks by design (a departed name's counter must never be reset). While the roster cap bounds distinct names this is tolerable, but the moment a leave/revocation path frees roster slots, enrol-leave churn makes cumulative I/O quadratic from unauthenticated ~100-byte requests. Fix: persist a RESERVED high-water block (e.g. floor+N) and issue from memory within it; the gaps that leaves are already declared correct by point 4 of the ids.NameSuffixes doc.
  _Proof: go test -race -run TestDurableNameSuffixes ./internal/ids -v_
- [ ] None · MSG-FU-SUFFIXFLOOR-FU-ENROLRECORDS: fold ENROLMENT records into the legacy-dir suffix backfill now that AUTH-3 has landed — id, P2
  Flagged by the security gate as a HAND-OFF HAZARD on MSG-FU-SUFFIXFLOOR (94159d93-fe87-4c3e-b938-86fe7068c787), and now live: AUTH-3 (durable roster, commit ece714f) landed immediately before the suffix wiring (commit 6985d2c).
  
  WHAT IS PINNED TODAY. cmd/agent-bus/suffixfloors.go's walAgentIDFloors folds the SENDER and RECIPIENTS of store.RecordKind records ONLY. Enrolment records (kind=agent) are deliberately EXCLUDED, and cmd/agent-bus/suffixfloors_test.go pins that with the subtest 'records of another kind are skipped'. That exclusion was RIGHT when it was written: enrolment was memory-only, so on every dir the shipped binary had produced the enrolment-record set was EMPTY while message records were the entire population -- which is exactly why internal/auth/floors.go's doc forbids auth.EnrolmentSuffixesInWAL as a floor SOURCE.
  
  WHY IT IS NOW WORTH REVISITING. With durable enrolment, kind=agent records DO carry agent ids, and the pin means a legacy backfill cannot see them. The exposure is currently NARROW and is not a live bug: any binary built from this tree has BOTH changes, and the first start against any dir writes the agent-suffixes file, after which the backfill never runs again. The only dir that could be affected is one written by a build taken BETWEEN ece714f and 6985d2c, which is not a released artifact.
  
  DO. Fold enrolment records into the backfill as defence in depth -- either by extending walAgentIDFloors to recognise auth.RecordKind (its record.go is now committed, so the dependency is stable) or by cross-checking with auth.EnrolmentSuffixesInWAL, which that function's own doc explicitly sanctions as a CROSS-CHECK even though it forbids it as a sole source. Folding it can only RAISE a floor, and raising is always safe. Update the pinning subtest and say in the comment why the exclusion existed, so the history is not lost.
  
  SEE ALSO 6f4c17ef-220c-465f-b8d8-a0f04aac1905 (streaming scan): if both land, do the enrolment fold in the same single streaming pass rather than adding a second full read.
  
  ACCEPTANCE. A test that a legacy dir whose ONLY agent-id evidence is an enrolment record does not re-mint that name; go test -race ./cmd/agent-bus green.
- [ ] None · MSG-FU-SUFFIXFLOOR-FU-ROSTERASSERT: assert a freshly-minted agent-suffix is not already in the roster -- the roster backstop against forge-low is accidental, not designed — auth, P1, follow-up, id-authority, security
  FINDING (independent security agent, reproduced with a working exploit, confirmed against the tree). agent-suffixes (cmd/agent-bus/suffixfloors.go / internal/ids) is UNKEYED -- no legacy path needed, the live format is already keyless. Rewinding it to an older value with a valid recomputed digest is accepted SILENTLY, no rewind warning: openSuffixAllocator cross-checks the persisted floors against the WAL only when the floors file is ABSENT (suffixfloors.go:143-169; the files own comment names the omission at line 166: a floors file that EXISTS but has been rewound to an older version ... is not detected today).
  
  THE REPORTERS OWN THESIS WAS THEN REFUTED, AND THAT IS THE FINDING. Rewinding and re-enrolling a fully-rostered name (e.g. worker-2) does NOT reuse the id: the bus tries to reissue it, and auth.ErrDuplicateAgentID (internal/auth/errors.go:88, roster.go:252, walroster.go:231) rejects it. The floor then self-heals upward. So forge-low against a rostered name costs only a couple of burned suffixes, not id reuse.
  
  THE PRECISELY-BOUNDED RESIDUAL: reuse is reachable only for a suffix that was BURNED but is NOT in the roster -- exactly the set the floor exists to protect and the roster structurally cannot cover:
  (a) a dangling/aborted enrol prepare -- number fsynced, crash before commit. internal/auth/crash_test.go:TestAuthCrashInjectionTornPrepare demonstrates worker-7 issued and reported as worker:1 after a SIGKILL, i.e. a suffix can be durably burned with no committed roster entry. Note this case is NOT fixed by 6f4c17ef (MSG-FU-SUFFIXFLOOR-FU-STREAMSCAN)s planned every-start WAL cross-check as currently scoped: that derivation folds committed store.RecordKind (and, per 477b8eeb, committed auth enrolment) records only. ID-2-WIRING-OBSERVER (c31f6999-da4e-400d-ab55-178b82e2a42e, still todo) is the task that would expose dangling/aborted prepares to any observer during replay at all -- until it lands, no WAL-derived floor, however often it is recomputed, can see this class of burned suffix.
  (b) once AUTH-4 (a853261d-2829-4101-906d-31a8a81eb59f, POST /v1/leave) lands, a departed agent -- the roster removes the entry on leave by design, so a rewound floor plus re-enrol binds a fresh keypair to an id a previous holder used, with no roster entry left to object.
  
  For either case: a rewound floor reissues the suffix, the roster has nothing to compare against, and enrol SUCCEEDS -- a new keypair silently inherits an id with prior history.
  
  THE LOAD-BEARING ASK, in the reporters own wording: the roster backstop is protecting you by accident of ordering, not by design intent stated at the floor. A defence nobody knows is load-bearing is one refactor from removal, and the plausible refactor is named: an idempotent re-enrol that overwrites the pubkey, which somebody will reasonably reach for (it is the natural fix for the ErrDuplicateAgentID case above under retry-safety expectations) and would silently convert a contained forge-low into full id reuse.
  
  DO. Add a NAMED assertion at the enrol path, independent of and in addition to any WAL-derived floor cross-check: a freshly minted suffix must not already be present in the roster BEFORE it is treated as fresh -- if it is, the floor was rewound (or narrower than reality): refuse the enrol and log LOUDLY at ERROR naming the name/suffix/expected-vs-found. This converts todays accidental 500 (ErrDuplicateAgentID, which only fires because Put happens to check) into a NAMED, tested detection that survives a future idempotent-re-enrol refactor.
  
  ALSO NOTE (lower priority, same root cause): forge-HIGH on agent-suffixes bricks enrolment for that ONE name only (the floor jumps ahead, so every mint for that name is starved), not the whole bus -- smaller blast radius than the wal-index-floor / message-seq-floor findings, but the same unkeyed root and the same remedy family (bound the accepted value; see be447589-6583-4d5c-a9d4-ec9d9fef0f1c and 259b7033-2191-423f-bb7b-cff8c6b59dc1, the sibling floor-bound tasks).
  
  SCOPE: internal/ids, internal/auth, cmd/agent-bus/suffixfloors.go. Coordinate with the live MSG-FU-SUFFIXFLOOR-* family (6f4c17ef streaming scan, 477b8eeb enrolment-record fold, e5fa08ba docs, d5ed5ccc unseal) -- this is a NEW, narrower assertion that is correct even before any of those land, and remains valuable defense-in-depth after they do.
  
  ACCEPTANCE. A test: mint an agent, durably burn its suffix via a torn/aborted prepare (or a committed-then-left entry once AUTH-4 exists) so the roster has no entry for it, rewind/replace the agent-suffixes floors file to a value at-or-below that suffix with a valid checksum, attempt to enrol the same name -- the enrol MUST be refused with a named, ERROR-logged reason, not silently reissue the suffix. go test -race ./internal/ids ./internal/auth ./cmd/agent-bus green.
- [ ] ID-4 · ID-4: Id-counter recovery property test — id, P1
  Cross-cutting test (depends on the WAL replay task): enrol several agents and send several messages, kill the process, restart, and assert every counter (sequence, per-name agent suffix) resumes strictly above its last-issued value -- table-driven across several kill points.
  _Proof: go test -race -run TestIDCounterRecovery ./internal/ids_
- [~] None · MSG-FU-SUFFIXFLOOR: resume per-name agent-id suffix counters from disk (agent ids are now durable) — id, P0, in progress
  Found by the security gate during the MSG/POLL wave (2026-08-02). cmd/agent-bus/main.go builds ids.NewNameSuffixes() -- a FRESH counter every start -- justified by the comment 'nothing in this path writes an agent id to disk'. THAT PREMISE IS NOW FALSE: store.Record persists sender and recipients as fully-qualified agent ids, hub.publish writes them through the WAL, hub.Apply replays them, and the WAL never compacts. So after a restart the suffix counter restarts at 1 and anyone who enrols the name 'alpha' is minted the id the previous alpha held (invariant 1 broken). CONFIDENTIALITY IS ALREADY CLOSED by the enrolment epoch shipped in the same wave (store.Message.VisibleTo refuses any message sent before the reader enrolled -- proved on a live server: a re-enrolled beta-1 reads 0 of the previous holder's DMs while the message is still in the store), and the reuse is logged at ERROR by hub.NoteEnrolment. WHAT REMAINS is identity continuity: a new keypair holding an id with a prior history, whose future messages are attributed to it. FIX: derive a per-name suffix floor from the highest suffix EVER WRITTEN TO DISK -- parse every sender and recipient seen during replay through ids.ParseAgentID and keep the max per name -- and seed ids.ResumeNameSuffixes with it before the listener binds. internal/hub already collects exactly these ids in Apply (see Hub.recovered), so the derivation belongs there and main passes it to the minter. ALSO correct the now-false justification comment at cmd/agent-bus/main.go:312-317: it is what will make the next reader believe this is safe. AUTH-3 (durable roster) is the complete fix; this is the half that does not depend on it.
  ---
  
  ## ACCEPTANCE CRITERIA ADDED 2026-08-03 (spec-keeper, dictated by security)
  
  Security's PASS-WITH-NOTES verdict on `ID-2-WIRING-SEAL-FU-NAMESUFFIXES` (public_id
  `1c207a62-e904-4988-84c2-f4b69712ee35`) named these as MUST-CLOSE-BEFORE-ENROLMENT-IS-DURABLE
  conditions for THIS task:
  
  (a) `cmd/agent-bus/main.go` constructs the allocator via `ids.ResumeNameSuffixes` (or `RaiseFloor`
      folded over the replay stream) and calls `Seal()` exactly ONCE with the error CHECKED.
  (b) A derivation that cannot complete is a FATAL startup error -- explicitly NEVER a fallback to
      `ids.NewNameSuffixes()`, which is the residual hole this task exists to close by name.
  (c) Once `main.go` no longer calls `ids.NewNameSuffixes()`, flip `NewNameSuffixes` to born-unsealed
      to restore parity with `Sequence`, or delete it.
  (d) Cheap interim guard worth adding: a test asserting no production package outside `cmd/` calls
      `ids.NewNameSuffixes`.
  
  See `ID-2-WIRING-SEAL-FU-NAMESUFFIXES` notes for the full security/reviewer context this closes the
  residual gap in.
  _Proof: go test -race -run TestAgentIDSuffixesResumeAcrossRestart ./internal/ids ./internal/hub_
- [ ] ID-2-WIRING-SEAL-FU-CONTRACTS · ID-2-WIRING-SEAL-FU-CONTRACTS: land the Sequence seal contract rows that the file-boundary deferred — docs, P1
  Deliberately incurred, tracked debt. `ID-2-WIRING-SEAL` (public_id 8c9b6489-abb1-444e-9eeb-3ff87646f632) shipped `Seal()`, `ErrFloorUnproven` and `ErrFloorSealed` on `internal/ids.Sequence`, and its own description said "Update CONTRACTS.md" -- but CONTRACTS.md was being split into per-plane files by a concurrent agent in the same loop (`CONTRACTS-SPLIT`, 360a2679-b5dc-4b17-863f-fb4462764e6d) and admits ONE writer per loop, so the feature-runner was explicitly barred from touching it. No contract row was written. This task lands them.
  
  Note there is currently NO section anywhere in CONTRACTS*.md documenting `internal/ids.Sequence` at all (grep confirms zero matches for `Sequence`, `RaiseFloor`, `NewSequence`). Suggested home: `CONTRACTS-ONDISK.md`, since the sequence number is the durable half of a message id and its floor is derived from the WAL -- but the owner of this task decides, and may reasonably argue for a new internal-package section instead.
  
  Rows to add:
  
  ### Message-sequence allocator (`internal/ids.Sequence`) -- added 2026-08-02 (ID-2-WIRING-SEAL)
  
  The allocator has two states and moves between them once, in one direction: UNSEALED -> SEALED.
  
  | Symbol | Contract |
  | --- | --- |
  | `ids.NewSequence() *Sequence` | Allocator for a FRESH bus: floor 0, first `Next` returns 1. Born **UNSEALED**. |
  | `ids.Resume(highestOnDisk uint64) *Sequence` | Allocator resuming strictly above `highestOnDisk` -- the highest sequence number EVER WRITTEN TO DISK: every prepare, committed, aborted and dangling alike; NOT a record count, NOT the highest COMMITTED sequence. Born **UNSEALED**. |
  | `(*Sequence).RaiseFloor(atLeast uint64) error` | Legal **only while UNSEALED**. Raises the floor to `atLeast`; never lowers it; may be called repeatedly from several sources in any order (it takes a maximum). Returns an error wrapping `ErrFloorSealed` after `Seal()` and changes nothing. |
  | `(*Sequence).Seal() error` | Ends floor assembly. One-way, exactly once: `nil` the first time; wraps `ErrFloorSealed` on every subsequent call and changes nothing. Concurrency-safe. `Seal()` is the caller ASSERTING the floor is >= every sequence ever written; the allocator holds no durable state and cannot verify that assertion. |
  | `(*Sequence).Next() (uint64, error)` | Returns `(0, ErrFloorUnproven)` while UNSEALED and allocates NOTHING (floor and last untouched). After `Seal()` issues floor+1, strictly monotonic, concurrency-safe; `(0, ErrSequenceExhausted)` at `math.MaxUint64`, never a wrap. The unsealed check runs BEFORE the exhaustion check. |
  | `(*Sequence).Last() uint64` | Highest number ISSUED by this allocator, 0 if none. Unchanged by ID-2-WIRING-SEAL. |
  | `ids.ErrFloorUnproven` | Sentinel returned by `Next` on an unsealed allocator. The fail-closed half of invariant 1: an allocator with no proven floor refuses to mint rather than minting from a guess. |
  | `ids.ErrFloorSealed` | Sentinel returned by `RaiseFloor` after `Seal`, and by a second `Seal`. |
  | `ids.ErrFloorBelowIssued` | Sentinel. **Unreachable on `Sequence`** under the seal gate (`last != 0` requires a successful `Next`, which requires `sealed`, and the sealed check returns first); the branch is kept as defence-in-depth. Still LIVE per-name on `NameSuffixes.RaiseFloor`, which has no seal. |
  
  **Startup contract.** A bus MUST derive the floor, `RaiseFloor` it from every source it has, and then call `Seal()` exactly ONCE before serving. Until it does, every `Next` is refused. Both constructors are born unsealed on purpose: "floor 0 because the log was empty" and "floor 0 because the recovery scan failed" are the same value, so neither is allowed to issue until somebody says out loud that the floor is proven.
  
  **What this does NOT defend.** `Seal()` proves a CLAIM was made, not that the claim is TRUE. `Sequence` holds no durable state, so a floor computed off a record count or off committed history seals just as cleanly as a correct one. Proving the floor remains the caller's obligation; deriving it is ID-2-WIRING (838677e6), blocked on ID-2-WIRING-SCHEMA.
  
  proof_cmd is RED today: verified zero matches for `ErrFloorUnproven` anywhere in the repo's CONTRACTS files. The glob `CONTRACTS*.md` is deliberate so the proof survives the CONTRACTS split into per-plane files.
  _Proof: grep -q 'ErrFloorUnproven' CONTRACTS*.md_
- [ ] None · Question whether a peer belongs on the legitimate floor-source list at all (ids.RaiseFloor) — ids, P2
  Filed per security recommendation on ID-2-WIRING-SEAL-FU-NAMESUFFIXES (public_id
  1c207a62-e904-4988-84c2-f4b69712ee35), explicitly NOT part of that task.
  
  Both `internal/ids/agentmint.go` (~:474-477) and `internal/ids/sequence.go` (:194, :343-344) list
  "a peer" as a legitimate source for assembling a floor claim passed to RaiseFloor. Security's
  judgement: a peer has NO basis for knowledge about THIS bus's own per-name suffix or sequence
  high-water mark -- that is derivable only from this bus's own disk -- and under invariants 1 and 2
  (server-authoritative ids; ids are never client-supplied identities to be trusted) a remote claim
  about our own namespace should not be a floor source at all.
  
  SEVERITY NOTE, recorded so it is not read as a lesser hazard than the whole-bus Sequence case: a
  per-NAME exhaustion is repeatable per name, so a peer able to reach RaiseFloor exhausts the WHOLE
  enrollable name space one call at a time, converging on the same outcome as the whole-bus Sequence
  exhaustion case at O(names) cost. It is not a smaller version of that hazard, just a slower one.
  
  WHAT NOT TO DO: security recommends AGAINST an in-code bound inside RaiseFloor itself -- RaiseFloor
  must stay able to accept a genuinely high LOCALLY-derived floor (that is its entire legitimate job),
  so a bound inside it is the wrong layer and would plant a policy number in the wrong place.
  
  THE FIX is one of:
    (1) remove "a peer" from both source lists (agentmint.go and sequence.go), or
    (2) if a real relay/peer-enrol requirement emerges later, bound the peer-supplied claim at the
        PEER-INPUT layer, validated against the locally-derived maximum plus configured headroom,
        before it ever reaches RaiseFloor.
  
  NOT URGENT: there is no peer/relay code yet and no production caller of either RaiseFloor today.
  File against the relay/peer-enrol work when that lands; do not block on it now.
- [ ] None · The hub id-reuse detector is narrower than its log line implies (broadcast-only agents leave no trace) — hub, P2, msg-fu-suffixfloor-followup
  Reported by the MSG-FU-SUFFIXFLOOR runner (public_id 94159d93-fe87-4c3e-b938-86fe7068c787), who did not own this file so filed rather than fixed. internal/hub/roster.go:65-88 fires only when the reused id is in h.recovered, and internal/hub/hub.go:497-499 populates that set ONLY from store.Message.Sender and .Recipients. An agent that enrolled, never sent a message, and was only ever a BROADCAST recipient (broadcasts are stored as a flag, not an expanded recipient list) leaves no trace, so its id can be reused with NO error logged at all. The detector is a partial backstop and must not be relied on as a safety net for invariant 1 (server-authoritative, never-reused ids). Fix should ensure the recovered/seen-id set also captures enrolment events themselves, not just message sender/recipient references.
  _Proof: go test -race -run TestHubIDReuse ./internal/hub_
- [ ] ID-2-WIRING-OBSERVER · ID-2-WIRING-OBSERVER: wal offers EVERY prepare (committed, aborted AND dangling) to an observer during the existing replay pass — durability, P0
  SPLIT OUT OF ID-2-WIRING (838677e6). See ID2_WIRING_DEEPDIVE.md sec 5/T3 (committed 2f89fc1).
  
  BLOCKED ON ID-2-WIRING-SCHEMA choosing Option A'. If SCHEMA chooses Option B instead, this task is SUPERSEDED and replaced by ID-2-WIRING-HEADER (add Entry.Seq + preparePayload.Seq, expose Recovered.HighestSequence, RESERVE a fresh ondisk-format-version value -- never pick it -- bump FormatVersion, fix replay_test.go:1109's unknown-field fixture, ship a downgrade note; proof `go test -race -run 'TestWALRecoveredHighestSequence|TestWALFormatVersionRefusal' ./internal/wal`).
  
  REQUIRED (Option A' shape). Add wal.ReplayWithPrepares(path, fn, onPrepare); Replay delegates with a nil observer so no existing caller changes. onPrepare fires for EVERY prepare in file order -- committed, aborted and dangling -- BEFORE resolution. The wal package still does not interpret Body; it hands the bytes up. Update CONTRACTS.md and PROTOCOL.md.
  
  THE ASSERTION THAT MATTERS: the observer must see the DANGLING prepare's entry. That is the whole point -- assert a floor of 100 from a log whose only seq-100 record never committed. A test that only observes committed prepares proves nothing.
  
  PROOF. `go test -race -run TestWALReplayObservesEveryPrepare ./internal/wal && go test -race ./internal/wal`. VACUOUS TODAY BY CONSTRUCTION (the test does not exist). The deep-diver's scratch equivalent (TestFloorFromPrepareObserverInOnePass) is proven PASS, so the command is executable once written. DO NOT COMPLETE ON A VACUOUS VERDICT.
  
  ---
  
  ## RE-SCOPED 2026-08-07 (spec-keeper), following the correction of ID-2-WIRING's (838677e6) supersession reason
  
  STILL OPEN, STILL P0 -- but the JUSTIFICATION above is now partly obsolete and must not be read as-is. The steady-state need this task originally served has been closed a DIFFERENT way; a real, narrower need remains.
  
  WHAT CHANGED. ID-2-WIRING-SCHEMA (80b54ee4, done) did choose Option A' in DECISIONS.md, so the "BLOCKED ON SCHEMA" gate above is cleared. But the consumer this task was built for -- ID-2-WIRING's T4, deriving `ids.Resume(floor)` in main.go by folding an observer over every WAL prepare body -- never landed, because ID-2-WIRING itself is now SUPERSEDED for a corrected reason: `internal/hub/seqfloorfile.go` (the `message-seq-floor` file, on-disk format version 5, landed under commit aad611c) persists the message-sequence floor OUTSIDE the log, written AHEAD of any sequence `/v1/mint` hands out. Since SIGN-2/SIGN-6, a sequence can be durably claimed in a batch of `hub.MintBatchSize=256` BEFORE any message record -- committed, aborted or dangling -- exists at all. There is therefore nothing in a message's WAL prepare body left for `ReplayWithPrepares`/`onPrepare` to derive the AUTHORITATIVE message-sequence floor FROM any more; scanning prepare bodies cannot see a number that was never written into one.
  
  SO: is a prepare-body observer still needed? YES, but NOT for the reason this task was filed. Two OTHER open gaps still need exactly this capability, both already on record as migration-window residuals that explicitly name this task as their closure:
  
  1. **MSG-FU-SEQHIGHWATER's residual** (6ebe51be, todo) -- a data directory that predates `message-seq-floor` has no floor file (`existedAtOpen() == false`) and must back-fill one on first start. Today that back-fill can only see COMMITTED history (hub.go's own source (3), `wal.Replay`'s ordinary committed-only callback), so a legacy directory's back-filled floor can still miss a sequence burned by a dangling, uncommitted mint/message record from before the upgrade. An observer closes that specific window.
  2. **MSG-FU-SUFFIXFLOOR's residual, via AUTH-3** (d53e3b21, in_progress) -- `ids.DurableNameSuffixes` (internal/ids/suffixstore.go) has the identical shape and the identical gap for agent-id suffixes: DECISIONS.md's 2026-08-07 "MSG-FU-SUFFIXFLOOR" entry says explicitly, in so many words, that a legacy directory's back-fill "still cannot see a suffix burned by a dangling prepare, because the prepare-observer work named above [[this task]] is not implemented," and that the gap "closes for good ... once ID-2-WIRING-OBSERVER lands." AUTH-3 cross-references this task for exactly that reason.
  
  CORRECTED SCOPE. Same code shape as originally specced (`wal.ReplayWithPrepares(path, fn, onPrepare)`, `Replay` delegates with `nil`, fires for every prepare -- committed, aborted, dangling -- in file order, before resolution; `wal` still does not interpret `Body`). The DIFFERENCE is what it is FOR: this is no longer the authoritative message-sequence derivation (that is `seqfloorfile.go`, unconditionally, on every start where the file exists) -- it is the ONE-TIME LEGACY-DIRECTORY BACK-FILL helper for BOTH `internal/hub`'s message-seq-floor back-fill and `internal/ids.DurableNameSuffixes`'s suffix-floor back-fill, invoked only on the `!existedAtOpen()` migration path each already documents. Keep the proof and assertion as specified below (a dangling prepare's entry must be observed) -- that requirement is unchanged; only the caller and the stakes are narrower than "P0, blocks every message ever sent" and are now "closes a bounded, already-logged, already-acknowledged migration-window gap for restarts of directories written before 2026-08-07."
  
  NOT reopening ID-2-WIRING (838677e6) for this -- that task's own scope (steady-state `ids.Resume` wiring in `main.go`) is genuinely superseded and stays closed; this task's remaining justification lives entirely in the two residuals above.
  
  Priority kept at P0 because AUTH-3 is P0/in_progress and blocked on an honest suffix-floor back-fill; whoever picks this up should coordinate with AUTH-3's owner rather than duplicate the WAL-side change.
  _Proof: go test -race -run TestWALReplayObservesEveryPrepare ./internal/wal && go test -race ./internal/wal_
- [ ] None · MSG-FU-SUFFIXFLOOR-FU-UNSEAL: make ids.NewNameSuffixes born-unsealed (or delete it) now that cmd/ no longer calls it — id, P1
  Acceptance criteria (c) and (d) of MSG-FU-SUFFIXFLOOR (94159d93-fe87-4c3e-b938-86fe7068c787), dictated by the security gate as MUST-CLOSE-BEFORE-ENROLMENT-IS-DURABLE. They live in internal/ids, which was outside the wiring task's file-ownership boundary, and the task that previously carried them (2db4a36f) is SUPERSEDED, so nothing else holds them.
  
  (c) ids.NewNameSuffixes (internal/ids/agentmint.go:339) is born SEALED, and its doc justifies that solely by 'a LIVE PRODUCTION CALLER: cmd/agent-bus/main.go builds ids.NewNameSuffixes() on every start'. THAT CALLER IS GONE -- verified: there are currently ZERO production callers of ids.NewNameSuffixes anywhere in the tree. So flip it to born-UNSEALED for parity with NewSequence (so even the empty case has to say out loud that it is empty), or delete it outright. Born-sealed with no caller is a loaded footgun: the next startup path that reaches for the obvious-looking constructor gets a silently sealed, all-zero floor map.
  
  (d) Add a guard that no PRODUCTION package calls ids.NewNameSuffixes. cmd/agent-bus/suffixfloors_test.go:TestNoFreshSuffixCounterInCmd already does this for package main, by parsing the AST (not grepping, so doc comments naming the constructor do not trip it) and resolving the ids import name so an alias or dot-import cannot evade it. Generalise that to the whole module, or place the equivalent in internal/ids.
  
  PROOF. go test -race ./internal/ids ./cmd/... green; the new guard fails when a call is reintroduced (prove the RED).

### EPIC IDEM — Duplicate detection and idempotency (invariant 10)

- [ ] None · RELAY-2-FU-IDEM-ROSTEROP: internal/idem has no OpRosterSync, so roster pushes borrow OpPeerEnrol — idem, P2
  internal/idem/scope.go's Operation set is CLOSED and validated (OpEnrol, OpSend, OpBroadcast, OpLeave, OpPeerEnrol, OpRelay). A roster push is its own mutating operation but has no constant, so relay.RosterUpdateFingerprint uses OpPeerEnrol as the nearest correct neighbour. Consequence, bounded but real: a peer that reuses ONE key across a peer-enrol AND a roster push lands both in the same scope, so the second is adjudicated a violation -- a peer bug either way, but diagnosed as the wrong bug. Fix: add OpRosterSync, add it to MutatingOperations and valid(), then switch RosterUpdateFingerprint. Adding it was outside RELAY-2's file boundary.
- [ ] IDEM-17-FU-CROSSAGENT · Crash-injection coverage for cross-agent applied-key isolation across recovery — test, P2
  No crash test proves the applied-key scope's CROSS-AGENT isolation survives recovery. The scope is the (agent, op, key) tuple, and IDEM-17 now pins the cross-OP half across a restart (TestIdemCrashInjectionRestartBroadcastRetryIsAnsweredOnce), but the cross-AGENT half is covered only IN MEMORY (internal/idem/idem_test.go, store_test.go). Needed: after a crash and restart, agent B replaying agent A's key must be applied as NEW, must not be answered with A's result, and must not be reported as a key-reuse violation -- i.e. no cross-agent oracle. Raised by the security gate as P2-4, which it explicitly confirmed still stands open after re-verification.
  _Proof: TBD by implementer -- e.g. go test -race -count=1 -run TestIdemCrashInjectionRestartCrossAgentKeyIsolation ./internal/idem/_
- [ ] None · Give the hub a sentinel for a reservation spent by a DIFFERENT agent — hub, P2
  Discovered while narrowing invariant 10's disconnect (see task 372b5072-2396-4e2a-8a80-398d5d006894, "Narrow invariant 10's disconnect to the third-party replay path"). `POST /v1/send` answers 409 "no matching sequence reservation" for TWO different actors and only one is hostile:
    - a third party presenting ANOTHER agent's (message_id, seq) under a key it never minted -- hostile;
    - the same agent re-presenting its OWN already-spent reservation under a fresh key -- a confused-but-honest client.
  Both surface as `hub.ErrUnknownMint` from the `h.mints[{agent,op,key}]` miss in internal/hub/hub.go. internal/httpapi cannot tell them apart: the miss carries no ownership information, the hub keeps no message-id -> minting-agent index, and internal/store has no lookup by message id. So the hostile case is currently rejected but NOT disconnected, which is the deliberate fail-safe choice (never disconnect an ambiguous case).
  
  Fix: add a secondary index in internal/hub from message id to the holding agent, and raise a DISTINCT sentinel (e.g. `hub.ErrForeignMint`) when the presented (message_id, seq) matches an OUTSTANDING reservation held by an agent other than the authenticated caller. internal/httpapi then disconnects on that sentinel only.
  
  Known limitation to design for: this catches theft of an OUTSTANDING reservation only. A reservation is deleted from the mint table once spent, so a stolen SPENT id would still be `ErrUnknownMint` and still indistinguishable. Decide explicitly whether that residual gap is acceptable or needs a separate mechanism.
  
  Test to replace when this lands: `TestCrossMintIsIndistinguishableFromAnHonestSpentReservation` in internal/httpapi/disconnect_socket_test.go currently ASSERTS the ambiguity (that the theft and the honest client get byte-identical responses). It must become a disconnect assertion.
- [ ] None · Stale invariant-10 unconditional-disconnect prose -- WIDENED 2026-08-08: 6 files, 14 sites (was 5 files, messages.go:1175 covered separately by IDEM-14-FU-CLIENTTEXT) — docs, P2, doc-only, invariant-10, spec-defect, stale-security-prose
  WIDENING of the original five-site sweep (spec-keeper, 2026-08-08), triggered by RELAY-13's reviewer finding the real count in RELAY-13's own client-half diff was EIGHT sites, not the original three named for that boundary. Verified all current sites by direct read/grep against HEAD/working-tree, since RELAY-13's ~950-line client diff shifted every line number this task's original proof_cmd was pinned to (the original AGENT_PROTOCOL.md:631-636 pin is now VACUOUS -- that text is already fixed and now lives at ~line 699-713, correctly narrowed. Confirmed by direct read: it now says 'does NOT disconnect you' and correctly scopes the one real disconnect case to signed-replay with a well-formed sender claim. AGENT_PROTOCOL.md is DONE and dropped from the remaining list).
  
  REMAINING STALE (verified present at HEAD/working-tree 2026-08-08, content-matched rather than line-pinned to survive further drift):
  - CONTRACTS-CLI.md:1107,1129 (line-shifted from the original :814/:836; same content -- 'and a disconnection' / 'and disconnects').
  - client/messages.go:202,595,602 (line-shifted from the original :188/:1141-1148 region; NOTE the :1141-1148 region itself is now split -- part of it, around current :1283/:1317-1326, IS already correctly fixed by IDEM-14-FU-CLIENTTEXT and says 'does NOT disconnect'; do not re-break that half).
  - internal/auth/errors.go -- 'DISCONNECTS the offending client' on ErrIdempotencyKeyReused's doc comment, unchanged from the original finding.
  - client/store.go -- the two ORIGINALLY tracked sites (content-identical to the old :192/:344, now at :202/:380) PLUS TWO NEW instances introduced by RELAY-13's own diff: :371 ('a protocol violation that gets the client DISCONNECTED', on pendingEnrolment's type doc) and :1349 ('the bus answers 409 and DISCONNECTS', on ClaimEnrolment's doc, both discovered because RELAY-13 added ~950 lines to this file).
  - client/enrol.go (WHOLE FILE NEW TO THIS TASK -- did not exist as a tracked site before, since RELAY-13's client half is what added the messaging-key plumbing to it): :192 ('the bus's refusal comes with a disconnection'), :291 ('is exactly the violation that earns a disconnect'), :584 ('a protocol violation that DISCONNECTS the client (invariant 10)'). NOTE :537 in the same file is ALREADY correct ('does NOT disconnect') -- do not touch it.
  - cmd/agent-busctl/enrol.go:90 (WHOLE FILE NEW TO THIS TASK) -- 'the bus treats that as a protocol violation and disconnects the client'.
  
  proof_cmd rewritten to check each of the 14 remaining stale phrases by CONTENT rather than line number, specifically to avoid the vacuous-on-drift failure mode this task's own reviewer already caught once on the AGENT_PROTOCOL.md segment. Confirmed RED today: all 14 phrase-checks fail (script exits 1) before any fix.
  
  Cross-reference: 3e542d14 (internal/relay/doc.go) remains the sixth original location, from a different angle, tracked separately -- unaffected by this widening.
  
  IDEM-14-FU-CLIENTTEXT (30a9e4f6) remains scoped to exactly client/messages.go's Remedy string (now the fixed :1283 region) -- still do not duplicate that scope; the messages.go sites THIS task now names (:202, :595, :602) are different lines with different content.
  _Proof: bash -c '! grep -qF "and a disconnection" CONTRACTS-CLI.md && ! grep -qF "and disconnects" CONTRACTS-CLI.md && ! grep -qF "earns a 409 AND a disconnection" client/messages.go && ! grep -qF "protocol violation that disconnects the client" client/messages.go && ! grep -qF "answer to it is a disconnection" client/messages.go && ! grep -qF "DISCONNECTS the offending client" internal/auth/errors.go && ! grep -qF "bus punishes with a disconnect" client/store.go && ! grep -qF "and earning a disconnect" client/store.go && ! grep -qF "client DISCONNECTED" client/store.go && ! grep -qF "bus answers 409 and DISCONNECTS" client/store.go && ! grep -qF "refusal comes with a disconnection" client/enrol.go && ! grep -qF "violation that earns a disconnect" client/enrol.go && ! grep -qF "DISCONNECTS the client (invariant 10)" client/enrol.go && ! grep -qF "disconnects the client" cmd/agent-busctl/enrol.go'_
- [ ] IDEM-12 · IDEM-12: Idempotent send/broadcast -- retries return the original result, no new sequence, no second audit record — core, P1
  GATED on IDEM-10, IDEM-11, MSG-2 (POST /v1/broadcast) and MSG-3 (POST /v1/send). Wire the idempotency key into both routes: on a request whose (agent, key) already has an applied-key record, look it up (IDEM-11) BEFORE doing any normal send work. THE CARVE-OUT THAT MUST NOT BE COLLAPSED (state this in the code comments and the task's own tests, not just in this description): same key + SAME payload is a LEGITIMATE RETRY -- the ack was probably lost in flight. Return the ORIGINAL message id and sequence number verbatim, allocate NO new sequence (invariant 1: sequences are server-minted and never duplicated for one logical operation), write NO second record to the append-only audit log (invariant 6 -- a retry must not create a phantom second entry for what is, from the audit trail's point of view, one logical send), do NOT return an error, and do NOT disconnect the client. This is the entire point of idempotency: punishing a well-behaved retrying client breaks exactly the client doing the right thing. ONLY same key + DIFFERENT payload is a violation, and that path is IDEM-14's job, not this task's -- this task implements the happy path only. 'Same payload' comparison MUST be exact/content-addressed (e.g. compare a hash of the canonical request body), not fuzzily approximated. This task's own narrow test must show: a same-key-same-payload retry of both /v1/send and /v1/broadcast returns identical id+sequence on the second call, and the audit log gains no second entry for it. Broader exactly-once coverage (retry storms, concurrency) lives in IDEM-16/IDEM-17, not here.
  
  --- FOLDED IN FROM THE WITHDRAWN DUPLICATE EPIC (IDEM-4, superseded 2026-08-02), reconciliation pass 2. This content was unique to the withdrawn set and is now in THIS task's scope, not merely offered as a note: (a) THE CONCURRENT IN-FLIGHT CASE, which is where implementations usually break: two requests with the same key arrive concurrently because the client retried before the first ack landed, so the first operation is committed-in-progress and there is NO stored result yet. A naive check-then-act double-applies. Handle it with a single-flight reservation on the key taken inside the SAME critical section that mints the sequence: the second caller either blocks and then returns the first's result, or receives an explicitly retriable 'in progress' response -- pick one and document it. (b) MARK A REPLAYED ACK: give the caller a way to tell a replayed ack from a fresh one (a response field or header) for debugging and for the wrapper's logging -- but the rest of the body must be byte-identical to the original. (c) BROADCAST DEDUPES ON THE OPERATION, NOT ON PER-RECIPIENT DELIVERY: a retried broadcast must not fan out a second time to ANYONE, including recipients whose delivery failed on the first attempt. (d) SIGN-6 INTERACTION: a message rejected for a missing or invalid signature was NEVER applied, so its key must not be recorded as applied -- a corrected resend under the same key is a new operation, not a retry. State this explicitly, or an implementer will record keys before validation and permanently burn them.
  _Proof: go test -race -run TestIdempotentSend ./internal/hub/... ./internal/httpapi/... ; then, against a throwaway bus with its own data dir under /tmp, the same scripts/bus-send.sh call issued TWICE with one idempotency key returns the SAME message id and sequence both times_
- [ ] IDEM-11-FU-FAIRSHARE-IDENTITIES · IDEM-11-FU-FAIRSHARE-IDENTITIES: fair-share divisor is gameable by identity count, not fixed by INVITE-GATE — security, P2
  The fair-share divisor counts distinct agents holding >= 1 applied key, so an identity costs ONE durable send. Measured by the security gate against the real predicate: 654 identities holding 51 keys each drives the share to 100; 2048 identities to 31; 32768 identities to a share of 1, at which point every honest agent holding a single key is refused until it ages out (up to the 50h10m22s window). Total cost ~32768 fsynced sends versus the 65536 the pre-fix attack needed -- i.e. roughly half the write cost for an equivalent denial. The change still strictly improves the single-identity case, which is why this is a follow-up and not a blocker.
  
  ROOT FIX is authenticated enrolment (INVITE-GATE): enrolment is unauthenticated today (/v1/enroll is on unauthenticatedRoutes, internal/httpapi/authmw.go). A floor under the share is a possible mitigation but does not remove the root cause. Re-assess once INVITE-GATE lands rather than fixing independently.
- [ ] None · IDEM-11-FU-THROUGHPUT: sustained ceiling roughly halves to ~0.36 ops/s and nothing surfaces IdempotencyStats to an operator — performance, P2
  Performance note from the IDEM-11 gates, 2026-08-03. Not a blocker; the durability cost is deliberate (invariant 4 -- nothing acked before durable, never traded for latency).
  
  Two follow-ups: (1) the sustained throughput ceiling roughly halves to ~0.36 ops/s with the applied-key record sharing the prepare's fsync -- this deserves an explicit operator-facing line in the docs rather than being discovered under load; (2) IDEM-11 added hub.IdempotencyStats() idem.Stats but nothing exposes it, so an operator cannot see how close the 65536 cap is to filling until it 503s. CORE-5 (metrics/observability) should surface it. That matters more given IDEM-11-FU-FAIRSHARE: without visibility, table exhaustion is indistinguishable from a general outage.
  
  Also latent, P2 within this task: idem.MaxResultBytes=512 vs store.MaxRecipients=64 -- only 0-1 recipients can reach publish today, so the bound is unreachable, but it becomes reachable the moment multi-recipient sends land.
- [~] IDEM-18 · IDEM-18: Wrappers generate the idempotency key ONCE and reuse it across retries, + AGENT_PROTOCOL.md / PROTOCOL.md / CONTRACTS.md — agentif, P1, in progress
  GATED on IDEM-10 (key contract) and IDEM-12 (idempotent send/broadcast). Filed 2026-08-02 as the one gap left after merging two concurrently-filed IDEM epics (see the IDEM epic note): IDEM-10..17 cover the server side thoroughly and say nothing about the agent-facing side, which invariant 7 makes non-optional -- agents never hand-write HTTP, so the idempotency key is the WRAPPER's responsibility, not the calling agent's. THE SINGLE MOST LIKELY WAY THIS EPIC SHIPS BROKEN: a wrapper that generates a FRESH key on every attempt. Every retry then looks like a brand-new operation, the server dedupes nothing, duplicates flow exactly as before -- and every server-side test in IDEM-16/IDEM-17 keeps passing, because none of them exercise the wrapper. DELIVER: (1) each mutating wrapper (bus-enrol, bus-send, bus-broadcast, bus-leave, bus-peer) generates ONE key per logical operation, holds it for the whole retry loop, and reuses it verbatim on every attempt. (2) Key generation is real randomness -- no PIDs, no timestamps, no counters that reset across restarts, all of which collide in exactly the multi-process, post-crash situations this epic exists for. (3) A test that FORCES a retry (first attempt killed or refused) and asserts exactly ONE message resulted -- run through scripts/bus-*.sh against a running throwaway bus with its own data dir under /tmp, never hand-written curl: if the wrapper doesn't retry idempotently, the feature doesn't work. (4) AGENT_PROTOCOL.md: agents call the wrapper and do NOT craft keys themselves; what a replayed-ack response means; and that after an IDEM-14 disconnect, reconnecting and retrying with the SAME key is CORRECT, while reusing a key for different content is a protocol violation that will disconnect them again. (5) PROTOCOL.md: the key's transport, the per-agent scope tuple, the payload fingerprint, and -- stated honestly -- IDEM-11's retention window as the BOUNDARY of the guarantee: duplicates are suppressed within the window, and a retry arriving after its key is evicted is applied as a new operation. The system does not provide unconditional exactly-once and the docs must not imply it does. (6) CONTRACTS.md: the header/field, every new error code, the record type IDEM-11 reserved, and any flag/env var bounding retention.
  _Proof: go test -race -run TestCLISendReusesIdempotencyKeyOnRetry ./cmd/agent-busctl/... && grep -qi idempotency AGENT_PROTOCOL.md && grep -qi idempotency CONTRACTS-CLI.md && grep -qi idempotency PROTOCOL.md_
- [ ] IDEM-11-FU-FAIRSHARE-FREEGROWTH · IDEM-11-FU-FAIRSHARE-FREEGROWTH: below-pressure admission is first-come-first-served and never reclaimed, so an early mover keeps an outsized share once the bus crosses into pressure — core, P2
  Admission below the pressure line (retained < maxEntries/2) is first-come-first-served and is NEVER reclaimed, so a first mover keeps an outsized allocation after the bus crosses into pressure. The security gate showed 3 identities arriving in a staggered sequence reaching the GLOBAL cap (32768 + 21845 + 10923 = 65536) and starving a 4th with ErrCapacity, where the same 3 arriving concurrently cap at 49152 and leave room.
  
  Any fix must not evict live keys -- evicting turns a key's next legitimate retry into a second effect (invariant 10) -- so the design space is about admission, not reclamation.
- [~] IDEM-17 · IDEM-17: Crash-injection test -- restart mid-retry-window still yields exactly one effect — test, P0, in progress
  PRIORITY P0, matching DUR-6's own P0 for the identical reason: per CLAUDE.md's durability discipline, 'the code looks right' is not evidence for a durability claim, and this is IDEM-11's crash-injection test -- kept as its own task, separate from IDEM-16's functional suite, the same way DUR-6 is kept separate from the rest of the DUR epic. GATED on IDEM-11 (durable applied-key store) and reuses the DUR-3/DUR-6 crash-injection harness pattern rather than inventing a second one. Test shape: issue a mutating request (send/broadcast at minimum) carrying an idempotency key, kill the process at a chosen point in the write path -- at minimum BEFORE the applied-key record is committed, and separately AFTER it is committed but before the ack reaches the client (both are the interesting crash points, matching DUR-2's two-phase prepare/commit boundary) -- restart, replay the WAL, then retry the SAME request with the SAME key and payload. Assert exactly ONE effect survives regardless of which crash point was hit: if the crash was pre-commit, the post-restart retry is correctly treated as a FRESH operation (nothing was durably applied) and produces exactly one effect; if the crash was post-commit, the post-restart retry is recognized via the recovered applied-key store and returns the ORIGINAL result with no second effect. THE FAILURE MODE THIS TEST EXISTS TO CATCH: a crash landing between 'operation applied' and 'applied-key record durably written' that, on restart, forgets the key was ever used and lets a retry silently re-apply -- that is a torn record by invariant 10's own definition even though invariant 5's general prefix-of-history property might otherwise look satisfied by the rest of the state. This is exactly the kind of claim CLAUDE.md says an ordinary test suite cannot detect by inspection alone.
  _Proof: go test -race -count=1 -run TestIdemCrashInjectionRestart ./internal/idem/_
- [ ] IDEM-17-FU-CHILDNONCE · Per-run nonce to gate crash-injection self-SIGKILL children (repo-wide) — test, P2
  Every crash-injection harness in this repo (internal/wal x3, internal/hub, internal/auth, internal/idem) gates its self-SIGKILL child on an environment variable alone. If those variables were ever exported in a shell or CI environment, an ordinary `go test ./...` would run the child for real: SIGKILL the test process and write a WAL into an operator-chosen directory. Fix is a per-run nonce the parent passes and the child requires. Deferred from IDEM-17 because fixing it in one package alone would make that package the outlier. Raised by the security gate as P2-1 during IDEM-17 review. REPO-WIDE scope, not idem-only.
  _Proof: TBD by implementer -- e.g. exporting the old bare env var alone (without the nonce) across internal/wal, internal/hub, internal/auth, internal/idem crash-injection tests must NOT trigger the self-SIGKILL child; go test -race ./internal/wal/... ./internal/hub/... ./internal/auth/... ./internal/idem/... still PASS_
- [~] IDEM-11 · IDEM-11: Durable applied-key store, recovered via WAL replay, with a bounded retention window — core, P0, in progress
  PRIORITY P0 (escalated from the epic default of P1): every other IDEM task's correctness depends on this store actually being durable, and per invariant 5/10 a store that LOOKS idempotent under normal operation but silently reverts to double-applying after a restart is the exact failure mode invariant 10 exists to prevent -- the same reasoning that makes DUR-1/DUR-2 P0 rather than P1. The store answering 'have I already applied this (agent, key) pair, and if so what was the result' MUST be durable, NOT an in-memory-only cache: a restart must not turn a duplicate into a second effect (invariant 10 explicitly, plus invariant 5 -- memory is the serving copy, disk is the truth). GATED on DUR-1 (WAL record framing), DUR-2 (two-phase prepare->commit write path) and DUR-3 (replay/recovery on start, currently in_progress -- do NOT touch DUR-3 itself, this task only depends on its contract). Applied-key records are written through the SAME prepare->commit path as every other durable write (invariant 4) and rebuilt by replaying the WAL on start, exactly like message history and the roster; the write-path half of this task can be developed against DUR-3's documented contract in parallel, but the recovery half cannot land until DUR-3 does. RETENTION is the sharp edge and MUST be decided, not left vague: keys cannot be kept forever (unbounded growth on an append-only durable store is DUR-7's snapshot/compaction problem, multiplied by one record per mutating call ever made). Choose ONE concrete bounded window -- by wall-clock time (e.g. a fixed 24h TTL), by count (e.g. the last N keys per agent), or by sequence range (e.g. keys older than current-sequence-minus-W) -- and record the choice plus its rationale in DECISIONS.md; a configurable-with-no-default is not an acceptable substitute for picking one. Explicitly specify and implement the behaviour for a retry that arrives AFTER its key's window has expired: it MUST FAIL CLOSED -- rejected as unrecognized/expired with a distinct, documented error -- never silently re-applied as if it were a fresh operation and never silently treated as already-seen when it in fact was not. Depends on IDEM-10 for the key shape being stored. BLOCKS IDEM-12 through IDEM-15.
  
  --- FOLDED IN FROM THE WITHDRAWN DUPLICATE EPIC (IDEM-2 and IDEM-3, superseded 2026-08-02), reconciliation pass 2. This content was unique to the withdrawn set and is now in THIS task's scope, not merely offered as a note: (a) SAME-TRANSACTION IS THE LOAD-BEARING REQUIREMENT: the applied-key record MUST commit in the SAME two-phase (prepare -> commit -> fsync) transaction as the effect it records. Not a second write, not ordered 'after' the effect. If the message commits and the key record does not, a crash in that window plus a client retry produces exactly the duplicate this epic exists to prevent -- and it stays invisible in ordinary testing because the window is small. (b) STORE THE RESULT, NOT JUST THE KEY: the record holds the scope tuple, IDEM-10's payload fingerprint, the MINTED RESULT (message id, sequence, timestamp) and the commit time. A key with no stored result cannot satisfy IDEM-12's 'return the original result verbatim'. (c) RESERVE THE ON-DISK RECORD-TYPE NUMBER via POST /api/v1/projects/agent-bus/reservations {"namespace":"record-type"} -- never hand-pick it; that is the classic parallel-agent collision, and DUR-1's framing already has neighbours. Bump the on-disk format version the same way if the framing changes. (d) RECOVERY MUST BE PREFIX-CONSISTENT: a key whose effect was NOT committed must not appear as applied after replay (invariant 5). (e) DERIVE THE RETENTION WINDOW, DO NOT PICK A ROUND NUMBER: it must EXCEED the maximum client retry horizon or the guarantee is a lie in exactly the case that matters. The realistic worst cases to derive it from are a peer reconnecting after an outage (RELAY-4's backoff ceiling) and a long-poll client resuming after a network partition. (f) EVICTION MUST BE CONSISTENT ACROSS MEMORY AND DISK: evicting in memory while the record survives on disk (or the reverse) makes behaviour depend on whether a restart happened since -- the worst kind of intermittent bug. State how eviction interacts with DUR-7 snapshot/compaction: a snapshot must neither silently reinstate evicted keys nor drop live ones. (g) MAKE THE BOUND OBSERVABLE: expose the applied-key count and the oldest-key age wherever CORE-5's inspect/metrics endpoint lands, so the bound is verified in production rather than assumed.
  
  --- CONTRADICTION RAISED BY THE MERGE (2026-08-02), MUST BE RESOLVED BY WHOEVER IMPLEMENTS THIS TASK: the paragraph above says a retry arriving after its key's window expired MUST FAIL CLOSED (rejected as unrecognized/expired), while withdrawn IDEM-3 and the surviving IDEM-18 doc task both state the honest guarantee as 'duplicates are suppressed within the retention window' -- i.e. a retry arriving after eviction IS applied as a NEW operation and produces a second effect. Both cannot ship. THE MECHANISM PROBLEM THAT DECIDES IT: keys are opaque client-supplied strings (IDEM-10), so a server that has evicted a key CANNOT distinguish it from a key it has never seen -- and every legitimate first attempt is a key it has never seen. Fail-closed is therefore only implementable if this task ALSO specifies a mechanism that makes expiry detectable (e.g. a retained eviction watermark plus a verifiable mint-time carried with the key); designing that mechanism is in scope here, assuming it is not. So: either (i) specify that mechanism and keep fail-closed, or (ii) adopt the bounded-window statement and document the boundary honestly. Record the choice and its rationale in DECISIONS.md, and make IDEM-18's PROTOCOL.md wording and IDEM-16's past-the-window test match it -- both of those currently assume (ii).
  _Proof: go test -race -run TestIdemCrash ./internal/hub/_
- [ ] IDEM-17-FU-PLACEMENT · Decide crash-suite package placement: internal/idem vs internal/hub — test, P2
  IDEM-17's crash-injection suite drives internal/hub but lives in internal/idem as an external test package (package idem_test), purely because the authoring agent's file-ownership boundary was internal/idem/** and internal/hub/** belonged to another live agent. Consequence: it has zero references to package idem, so `go test ./internal/hub/` does not run it and coverage is attributed to the wrong package. Decide deliberately whether to relocate it next to internal/hub/idem_crash_test.go or to keep it and record the placement as intentional. Note the two files are complementary, not duplicates: hub's indexes crash points by DURABLE WAL STATE, idem's by position in the CLIENT'S RETRY WINDOW.
  _Proof: TBD by implementer -- either a relocation diff that still passes go test -race ./internal/hub/... ./internal/idem/..., or a doc.go note recording the placement decision as intentional_
- [ ] IDEM-13 · IDEM-13: Idempotent enrol / leave / peer-enrol — core, P1
  GATED on IDEM-10, IDEM-11, AUTH-1 (POST /v1/enroll), AUTH-4 (POST /v1/leave) and RELAY-1 (peer enrolment). Extends the IDEM-12 discipline to the non-messaging mutating operations invariant 10 names explicitly: enrol, leave, peer-enrol. Same-key-same-request-shape returns the original result rather than erroring or re-minting -- e.g. re-presenting the same enrolment public key with the same idempotency key after a lost ack returns the SAME signed credential/token, not a second one and not an 'already enrolled' error that would force the agent down a spurious re-enrolment path. Same-key-different-content is a violation and is IDEM-14's job, not this task's. Each of the three routes has its own notion of 'same request' worth being explicit about in CONTRACTS.md: enrol's identity is the presented public key; leave's is the agent being revoked; peer-enrol's is the peer bus id plus its offered credential. Because enrol issues a signed credential (invariant 3), pay particular attention to NOT minting a second valid token for a retried enrol -- a client holding two live tokens for one identity is a small security smell worth avoiding even when neither token is individually wrong. Document each route's comparison basis in CONTRACTS.md alongside its existing route entry.
  
  --- FOLDED IN FROM THE WITHDRAWN DUPLICATE EPIC (IDEM-6, superseded 2026-08-02), reconciliation pass 2. This content was unique to the withdrawn set and is now in THIS task's scope, not merely offered as a note: (a) WHY A DOUBLE-APPLIED ENROL IS WORSE THAN A DUPLICATE MESSAGE: ids are never reused (invariant 1), so minting a second agent burns an id permanently and leaves a PHANTOM agent in the roster that nothing will ever collect, while the client ends up holding a credential for an identity its peers were never told about. (b) THE UNAUTHENTICATED SCOPE: enrol has no authenticated caller yet, so it uses the alternative key scope IDEM-10 settles (the presented enrolment public key, or bus-wide) -- implement exactly that, and ensure it cannot be used by an unauthenticated caller to squat or probe another party's keys. (c) RE-ENROLMENT WITH A DIFFERENT PUBLIC KEY under the same idempotency key is a different-payload VIOLATION (IDEM-14), not a retry -- important, because that is precisely how an attacker would attempt an identity takeover. (d) LEAVE MUST NOT DOUBLE-APPLY ITS SIDE EFFECTS: return success (not an error) on a second call, and do not repeat revocation side effects -- notably CRYPTO-4's key_epoch bump, where a second bump needlessly invalidates freshly-issued bundles. (e) PEER-ENROL MUST CONVERGE: two buses enrolling each other concurrently, and a peer retrying after a timeout, must end up with ONE peering, not two half-configured ones. (f) All three operations persist their applied-key records through IDEM-11's store so they survive restart, and all three must still behave after roster recovery (AUTH-3). PRIORITY NOTE: kept at P1 (the withdrawn counterpart was P2); the merge preserves the STRONGER priority of the two batches, never the weaker.
  _Proof: go test -race -run TestIdempotentEnrol ./internal/auth/... ./internal/relay/..._
- [ ] None · IDEM-11-FU-HUBAPPLY: hub.Apply returns early for non-message Entry.Kind, so IDEM-13/14/15 cannot fold their own Entry.Idem — durability, P2
  Noted by the IDEM-11 agent 2026-08-03 as a structural blocker for the rest of the epic, not a defect in shipped behaviour.
  
  hub.Apply returns early for any Entry.Kind that is not a message, so a prepare carrying an Entry.Idem record for a NON-message operation (enrol, leave, peer-enrol) is never folded into the applied-key store during replay. IDEM-13 (idempotent enrol/leave/peer-enrol) and IDEM-14 (the violation path) therefore cannot work until Apply is extended to route by Kind and fold Idem for every kind that carries one.
  
  Whoever takes IDEM-13 should expect to do this first, or it should be split out ahead of them. Flagging it now so it is not rediscovered as a surprise mid-task.
- [ ] IDEM-15 · IDEM-15: Relay duplicate suppression via idempotency keys — relay, P2
  GATED on IDEM-10, IDEM-11, RELAY-2 (message relay across peers) and RELAY-3 (loop prevention via traversed-bus path). Relay is where idempotency earns its keep: a cyclic peer topology combined with at-least-once delivery (invariant 4's guarantee, extended across the relay plane) means a relayed message can legitimately arrive at a bus by two different paths, or be resent by a peer retrying after a lost ack -- duplicates are the NORMAL steady state here, not an edge case. Apply the same applied-key check IDEM-12 uses to inbound relayed messages: a relayed message carries (or is assigned, at the originating bus) an idempotency key, and a receiving bus that has already applied that key suppresses the duplicate exactly as a duplicate direct send is suppressed -- no second delivery to local agents, no second audit record. STATE THIS EXPLICITLY, because RELAY-3 alone reads as sufficient and it is NOT: RELAY-3's traversed-bus-path loop prevention COMPLEMENTS this and is NEVER a substitute for it. RELAY-3 stops a message from being re-relayed back through a bus it has already visited (a topology-shape defence); it does nothing about a message that legitimately reaches the same bus via two DIFFERENT paths that never revisit any bus, which only idempotency catches. A relay implementation with RELAY-3 but without this task will silently double-deliver in exactly that topology. Priority is P2, matching the RELAY epic's own priority band, since it cannot land before RELAY-2/RELAY-3 exist. Test alongside RELAY-5's crash/loop integration test in IDEM-17, not as a replacement for it.
  
  --- FOLDED IN FROM THE WITHDRAWN DUPLICATE EPIC (IDEM-7, superseded 2026-08-02), reconciliation pass 2. This content was unique to the withdrawn set and is now in THIS task's scope, not merely offered as a note: (a) DEDUPE ON THE ORIGIN'S IDENTITY, NOT THE FORWARDING PEER'S. Two different peers legitimately forward the SAME origin message; keying suppression on the sending peer's own idempotency key treats those as two distinct messages and delivers both. The dedupe identity must be the ORIGIN bus's message identity -- already globally unambiguous per invariant 2 because it is <bus-id>-namespaced -- carried UNCHANGED across every hop. This is the single most important sentence in this task and it was absent before the merge. (b) IT MUST NOT BE FORGEABLE BY AN INTERMEDIATE (see SIGN-7): if a lying peer can rewrite the dedupe identity, it can split one message into two deliveries (duplicate injection) or collide two distinct messages into one (silent suppression). Prefer an identity that is inside, or verifiably derived from, SIGN-1's signed bytes -- and state explicitly what an intermediate CAN still do: the traversed-bus path is metadata OUTSIDE the signature, so RELAY-3's loop prevention is an availability mechanism, not a security one. (c) 'APPLIED ONCE' MEANS ONCE LOCALLY: the receiving bus mints its OWN local delivery sequence for its own recipients (SIGN-7), so the assertion is one local delivery and one local sequence consumed -- not that the origin's numbers are reused. (d) RELAY-4's RETRY/BACKOFF IS THE DUPLICATE SOURCE this defends against, so test them together: a peer that acks late and retries must not produce a second delivery, INCLUDING across a restart of the receiving bus -- which is where the durability of the relay-side applied-key record (IDEM-11) is actually exercised. (e) Put the complement-never-substitute argument in the CODE COMMENT and in PROTOCOL.md, not only in this task, so a later implementer does not delete one defence because the other exists. CROSS-REFERENCE: SIGN-7 point (5) now points at THIS task (it referenced the withdrawn IDEM-7 until the merge).
  _Proof: go test -race -run TestRelayIdempotentSuppression ./internal/relay/..._
- [ ] IDEM-14 · IDEM-14: Idempotency violation path -- key reuse with a different payload rejects, logs as security, and disconnects — core, P1
  GATED on IDEM-10, IDEM-11, and at least one of IDEM-12/IDEM-13 landing first (the happy path must exist before the violation path can be distinguished from it). Implements invariant 10's violation clause: when a client reuses an (agent, key) pair the applied-key store (IDEM-11) already has a record for, but the NEW request's payload does NOT match the original, the server must (1) REJECT the request, (2) log it as a SECURITY event -- not a routine 4xx; same severity class as an auth failure, discoverable the way the security agent expects to find things -- and (3) DISCONNECT the offending client. THE CARVE-OUT THAT MUST NOT BE COLLAPSED (restate it here explicitly, do not assume the reader has IDEM-12's copy in front of them): this path fires ONLY for same-key-DIFFERENT-payload. Same-key-SAME-payload is IDEM-12/IDEM-13's legitimate-retry path and must NEVER reach this code -- an implementation that disconnects on every duplicate key regardless of payload is WRONG and will disconnect well-behaved retrying clients, precisely the bug invariant 10's text calls out by name. TWO DECISIONS THIS TASK MUST PIN DOWN and record in DECISIONS.md, because CLAUDE.md's invariant 10 text leaves them open: (a) the EXACT HTTP status code returned for the rejected request (409 Conflict is the natural fit for 'conflicts with a prior request under this key' -- pick and justify one, don't reuse a generic 400); (b) whether 'disconnect' means merely dropping the current connection/long-poll (the agent can reconnect and retry with a fresh key) or FULL CREDENTIAL REVOCATION requiring re-enrolment (the agent's AUTH-1 token is invalidated, same blast radius as AUTH-4's leave path) -- these have very different consequences and the choice must be deliberate, not whichever was easiest to wire up. Also applies conceptually to 'replay of an already-accepted signed message' per invariant 10's third bullet -- SIGN-4/SIGN-5 own the signature-replay detection mechanics; this task's reject/log/disconnect plumbing is the natural place that behaviour hooks into, so cross-reference SIGN-4 rather than building a second, divergent disconnect path.
  
  --- FOLDED IN FROM THE WITHDRAWN DUPLICATE EPIC (IDEM-5, superseded 2026-08-02), reconciliation pass 2. This content was unique to the withdrawn set and is now in THIS task's scope, not merely offered as a note: (a) DEFINE 'DISCONNECT' CONCRETELY -- on an HTTP server it is not self-evident: at minimum, close the connection without keep-alive reuse. That is the MECHANICS, a separate axis from the blast-radius decision this task already carries (drop the connection vs revoke the credential); both must be written down. (b) THE ERROR MUST BE GREPPABLE: a distinct code, not the generic validation error, plus a log line an operator actually sees carrying the caller identity, the operation, the key, and BOTH payload fingerprints (the stored one and the offending one). (c) DO NOT CREATE A SELF-INFLICTED RECONNECT STORM: a disconnected long-poll client (POLL-1) reconnects immediately, so the rejection must be either sticky enough to stop the loop or cheap enough not to matter -- say which. (d) KEEP THE SIGNED-REPLAY PATH DISTINCT: replay of an already-accepted SIGNED message also rejects and disconnects, but its freshness check is SIGN-4's sequence+cursor, NOT the payload fingerprint. Reuse this task's reject/log/disconnect plumbing, but do not merge the two detectors into one path -- cross-reference them instead. (e) BOTH DIRECTIONS GET THEIR OWN NAMED TEST: it fires on same-key-different-payload, and it provably does NOT fire on same-key-same-payload. Getting that backwards turns a correctness feature into an outage for exactly the well-behaved clients that retry correctly.
  _Proof: go test -race -run TestIdempotencyViolation ./internal/..._
- [ ] IDEM-16 · IDEM-16: Exactly-once test suite -- retry storm, concurrent race under -race, and key-reuse-different-payload disconnect — test, P1
  GATED on IDEM-12, IDEM-13, IDEM-14. Functional/concurrency coverage proving invariant 10's guarantees under `-race` (CLAUDE.md: concurrency here is the product, a data race is a P0). Required, each as its OWN named test so a future regression names exactly which property broke: (1) RETRY STORM -- fire N (e.g. 50) requests sharing one (agent, key, payload) and assert exactly ONE effect resulted: one sequence allocated, one audit record written, all N responses are byte-identical to the original result, and none of the N connections was disconnected. (2) CONCURRENT RACE -- run under `go test -race`, launching the identical-payload retries genuinely concurrently (goroutines released via a barrier, not serialized one after another) so the applied-key check-then-write path's OWN race safety is exercised, not just its logic in isolation; a naive check-then-insert without a lock/CAS looks correct read serially but double-applies under real concurrency, and this test must be able to catch that. (3) KEY-REUSE-DIFFERENT-PAYLOAD -- reuse an (agent, key) with a different payload and assert IDEM-14's full behaviour: rejection with the pinned status code, the security-event log entry, and the disconnect (whichever form IDEM-14 decided). STATE THE CARVE-OUT EXPLICITLY in the test names/comments so a future reader cannot miscopy the storm test's assertions into the disconnect test or vice versa. Exercise via the actual HTTP routes (send/broadcast at minimum; enrol/leave/peer-enrol if IDEM-13 landed first), not by calling internal functions directly, so this proves the wire behaviour the AGENTIF wrappers actually depend on. Kept separate from IDEM-17's crash-injection test the same way DUR's functional tests are kept separate from DUR-6.
  
  --- FOLDED IN FROM THE WITHDRAWN DUPLICATE EPIC (IDEM-8, superseded 2026-08-02), reconciliation pass 2. This content was unique to the withdrawn set and is now in THIS task's scope, not merely offered as a note: (a) ASSERT EXACTLY ONE OF EVERYTHING, NOT MERELY 'NO ERROR': one WAL record, one append-only audit entry (invariant 6), one delivery to the recipient, one sequence consumed. A test that only inspects the response body passes against an implementation that quietly writes two durable records. (b) ADD A RETRIED-BROADCAST CASE: each recipient receives it exactly once, including a recipient whose first-attempt delivery failed. (c) ADD A POST-VIOLATION INTEGRITY CASE: after IDEM-14 rejects and disconnects a key-reuse-with-different-payload attempt, the ORIGINAL message is still intact, still in history, and still deliverable -- a violation must not damage the operation it collided with. (d) ADD A PAST-THE-RETENTION-WINDOW CASE asserting IDEM-11's DOCUMENTED behaviour explicitly, so the honest boundary of the guarantee is pinned by a test rather than left to the reader. NOTE that IDEM-11 currently carries an unresolved contradiction about what that behaviour is (fail-closed vs applied-as-a-new-operation); write this test against whatever DECISIONS.md records, and do NOT write it against whichever one the implementation happens to do.
  _Proof: go test -race -run 'TestIdemRetryStorm|TestIdemConcurrentRace|TestIdemViolationDisconnect' ./internal/..._
### EPIC INVITE — Invite-only enrolment

- [ ] INVITE-GATE · INVITE-GATE: POST /v1/enroll REQUIRES a valid invite and fails closed; invite consumption and the roster write commit TOGETHER — auth, P0
  EPIC: 0b43393e-556b-409a-938a-846be2fb4a75 | DEPENDS ON: ENROL-SHAPE, INVITE-STORE, INVITE-MINT | BLOCKS: INVITE-HARDEN, INVITE-REVOKE, INVITE-CLIENT, INVITE-PEERGUARD
  
  This is the epic's crux and the root fix for the pre-auth attack family. internal/httpapi/auth.go:122 handleEnroll and internal/auth/service.go:276 Service.Enrol gain the gate. THE CORRECTNESS CRUX: single-use consumption and the enrolment effect must land in the SAME two-phase transaction, or a crash between them either burns an invite with no agent or enrols an agent without spending the invite. SECOND CRUX (invariant 10): a legitimate retry carrying the same idempotency_key and the same payload must return the ORIGINAL result and must NOT consume the invite a second time; same key with a DIFFERENT payload stays a 409 + Connection: close. Must update CONTRACTS-HTTP.md -- in particular the "Known gaps" bullet at CONTRACTS-HTTP.md:172-186 which currently states enrolment is unauthenticated, and the POST /v1/enroll route rows at CONTRACTS-HTTP.md:14-17. BREAKING WIRE CHANGE -- escalated to the user; do not land before ENROL-SHAPE. RESIDUAL RISK TO DOCUMENT IN THE SAME TASK: until MTLS-LISTENER lands, the invite secret crosses the wire in CLEARTEXT; exposure is bounded only by the -listen 127.0.0.1:8080 loopback default, and the bus must not be exposed on a non-loopback interface until mTLS ships.
  _Proof: go test -race -run 'TestEnrolRequiresInvite|TestEnrolConsumesInviteAtomically|TestEnrolRetryDoesNotReconsumeInvite' ./internal/auth ./internal/httpapi && grep -q 'invite' CONTRACTS-HTTP.md_
- [ ] INVITE-CLIENT · INVITE-CLIENT: the Go client/CLI redeems an invite at enrol (+ AGENT_PROTOCOL.md entry) -- invariant 7's delivery vehicle is the CLI subcommand, NOT a bus-enrol.sh — agentif, P1
  EPIC: 0b43393e-556b-409a-938a-846be2fb4a75 | DEPENDS ON: INVITE-GATE, CLI-1 (0495d133), CLI-2 (39318208) | BLOCKS: none
  
  DECIDED AND RECORDED HERE so it is not re-litigated -- invite redemption reaches an agent as a flag on the existing CLI identity subcommand (agent-bus-cli enrol --invite <blob>), NOT as a new scripts/bus-*.sh wrapper. This is consistent with the 2026-08-02 amendment to invariant 7 (DECISIONS.md:605-637, "The Go CLI replaces the shell wrappers"), with CLI-2 (39318208) which absorbed enrolment, and with AGENTIF-2 (15e4509c) which is already superseded. DEPENDS ON CLI-1 (0495d133) and CLI-2 (39318208) -- neither exists yet; there is no client package and no second cmd binary today. CONTRADICTION TO RESOLVE BEFORE STARTING (flagged by the planner, who was boundary-blocked from editing CLI-*): CLI-2's recorded proof_cmd enrols with no invite and over http://, so it is invalidated by BOTH this task and MTLS-LISTENER.
  _Proof: go test -race -run TestClientEnrolWithInvite ./client/... && grep -qi 'invite' AGENT_PROTOCOL.md && grep -qi 'invite' CONTRACTS-AGENT.md_
- [ ] INVITE-HARDEN · INVITE-HARDEN: constant-time invite-secret comparison and ONE indistinguishable failure response for unknown/expired/revoked/already-consumed — security, P1
  EPIC: 0b43393e-556b-409a-938a-846be2fb4a75 | DEPENDS ON: INVITE-GATE | BLOCKS: none
  
  Mirrors the existing deliberate indistinguishability of the 401 and 404 surfaces (CONTRACTS-HTTP.md:19, :235-239) -- distinguishing the four invite failure modes is an enumeration oracle. Comparison uses stdlib crypto/subtle.ConstantTimeCompare. INVARIANT 9: do not hand-roll a comparison, a hash, or a token format; if any part of this looks like inventing a scheme, stop and escalate.
  _Proof: go test -race -run 'TestInviteRedeemFailuresIndistinguishable|TestInviteSecretComparedInConstantTime' ./internal/httpapi ./internal/invite_
- [ ] INVITE-REVOKE · INVITE-REVOKE: durably revoke an un-redeemed invite, and state what revocation does to an agent that already redeemed one — auth, P1
  EPIC: 0b43393e-556b-409a-938a-846be2fb4a75 | DEPENDS ON: INVITE-STORE, INVITE-GATE | BLOCKS: none
  
  Revocation must survive restart (same durable store as INVITE-STORE). BLOCKED ON THE ESCALATED DECISION: does revoking an invite cascade to the agent that already redeemed it and kill its live sessions (requires AUTH-4 leave/revocation, a853261d), or is an invite simply spent at redemption so revocation only affects un-redeemed invites? Whichever the user picks, this task must state it explicitly in CONTRACTS-HTTP.md -- silence here is the failure mode.
  _Proof: go test -race -run 'TestInviteRevokedCannotBeRedeemed|TestInviteRevocationSurvivesRestart' ./internal/invite && grep -qi 'revocation' CONTRACTS-HTTP.md_
- [ ] INVITE-PEERGUARD · INVITE-PEERGUARD: no ungated peer/federation enrolment path may ever exist -- enumerate the routes and assert it — security, P1
  EPIC: 0b43393e-556b-409a-938a-846be2fb4a75 | DEPENDS ON: INVITE-GATE | BLOCKS: RELAY-1 (9bc9d6c4), MTLS-RELAYGUARD
  
  The user's decision says redemption is the only route onto the bus INCLUDING for peer buses. internal/relay/ is a 9-line doc.go stub today (internal/relay/doc.go:8) and no peer route exists, so the landable increment now is the GUARD, not the feature: a test that walks (*Server).Routes() (internal/httpapi/server.go, the same enumeration TestEveryRouteRequiresAuth uses) and the five-entry allow-list (internal/httpapi/authmw.go:57-63) and fails if any peer/federation/relay-enrolment path is reachable without invite redemption. RELAY-1 (9bc9d6c4) must satisfy this guard rather than route around it; record that as an acceptance criterion in this task's own description (the planner was not permitted to edit RELAY-1).
  _Proof: go test -race -run 'TestNoUnauthenticatedPeerEnrolRoute|TestAllowListIsExactlyTheFiveKnownPaths' ./internal/httpapi && grep -qi 'peer' CONTRACTS-HTTP.md_

### EPIC MSG — Messaging surface

- [ ] None · Acceptance criterion for the first durable-write HTTP handler (MSG-2/MSG-3): wal.ErrClosed/wal.ErrPoisoned must 5xx and MUST NOT acknowledge — httpapi, P1
  internal/wal.ErrClosed (format.go:156, "reported by Append and Sync after Close") and wal.ErrPoisoned both propagate all the way up through Log.Write/Begin/Commit (log.go:343-351, :388-392, :446) as ordinary Go errors -- correctly, at the wal layer: nothing there is ever swallowed. But VERIFIED THIS PASS: no HTTP handler exists yet that calls DurableLog.Write at all. The DurableLog interface was wired onto httpapi.Server by DUR-9 (internal/httpapi/server.go:34-38, "Write is the whole of invariant 4 as a handler needs it") and durable_test.go proves the wiring end to end with a fakeDurable, but grep confirms `.Durable().Write(` / `s.durable.Write(` has zero call sites anywhere in internal/httpapi -- the only two live routes are /healthz and /v1/info, neither of which writes anything durable. The first real write handler is POST /v1/send (MSG-3) and POST /v1/broadcast (MSG-2), both still `todo`.
  
  Filing this NOW, ahead of MSG-2/MSG-3, so the constraint is not lost or improvised differently by whichever agent picks up the first write handler: invariant 4 ("nothing is acknowledged before it is durable") means that when Durable().Write returns wal.ErrClosed (server is shutting down / already closed the log) or wal.ErrPoisoned (a torn write, see writer.go:145-150 -- "the writer refuses to keep going instead of trading a recoverable file for an unrecoverable one"), the handler MUST map that to a 5xx response (503 for ErrClosed -- the server is draining, a retry against another bus/instance may succeed; 500 for ErrPoisoned -- the log itself is suspect) and MUST NOT write any 2xx body, partial or otherwise, that a caller could read as "the message was sent". A response body written before the error is known, or any code path that treats a non-nil error from Write as anything but a hard failure, breaks invariant 4 the first time it happens.
  
  DONE means: when MSG-2/MSG-3 land, their handler tests include a negative case using the SAME fakeDurable pattern durable_test.go already established (fakeDurable{err: wal.ErrClosed} and fakeDurable{err: wal.ErrPoisoned}) asserting the response status is >=500 and the response body is the standard ErrorResponse shape, never the success shape.
  
  proof_cmd validated via scripts/proof-check.sh: verdict=FAIL (exit 1) today -- no reference to wal.ErrClosed or wal.ErrPoisoned exists anywhere in internal/httpapi yet, because no handler calls Write. This grep is a necessary-but-not-sufficient floor (a real handler MUST reference these sentinels to branch on them); the actual proof once MSG-2/MSG-3 land is the negative-path handler test described above, not this grep alone.
  _Proof: grep -rq "wal.ErrClosed\|wal.ErrPoisoned" internal/httpapi/*.go_
### EPIC MTLS — Mutual TLS with self-signed certs, no CA

- [ ] None · CONTRACTS-CLI.md client export table is missing the three symbols MTLS-EXPIRY added — docs, P2
  CONTRACTS-CLI.md (~line 748/847, the client-package export table) already enumerates ErrBusFingerprintMismatch, ErrBusPresentedNoCertificate and BusFingerprintError, but not ErrBusCertificateExpired, ErrBusCertificateUnusable or BusCertificateExpiredError. Deferred deliberately: that file was granted to a CONCURRENT agent during MTLS-EXPIRY, so editing it would have collided. Both the reviewer and security gates raised it.
- [~] MTLS-CLIENTCERT · MTLS-CLIENTCERT: the client generates and stores its own TLS keypair + self-signed certificate (0600) and presents it on every connection — agentif, P1, in progress
  EPIC: a1b628fb-8cbf-47e8-9682-034fda8636c7 | DEPENDS ON: MTLS-DESIGN, CLI-1 (0495d133) | BLOCKS: MTLS-PIN
  
  Client-side half of the mutual handshake, in the importable client/ package (NOT under internal/ -- CLI-1's non-negotiable). Key stored 0600 in the user's config dir, never in the repo, no interactive prompt, no TTY-dependent input. Stdlib crypto/tls + crypto/x509 only. DEPENDS ON CLI-1 (0495d133) -- no client package exists today.
  
  === AUDIT 2026-08-08 (spec-keeper): PARTIALLY SHIPPED -- the split, explicitly ===
  MET (in main at 9418a48, an ancestor of HEAD efde70c): client/clientcert.go (802 lines),
  cmd/agent-busctl/clientcert.go registering the `client-cert` subcommand, and BOTH tests the
  proof_cmd names -- TestClientGeneratesClientCert and TestClientTLSKeyIs0600 -- exist at HEAD in
  client/clientcert_test.go. The proof_cmd is USABLE.
  NOT MET -- the documentation half that invariant 7 requires IN THE SAME TASK, and it is worse than
  merely absent:
    (a) AGENT_PROTOCOL.md at HEAD contains ZERO occurrences of "client-cert", "clientcert" or "client
        certificate". The `agent-busctl client-cert` subcommand is undocumented for agents.
    (b) CONTRACTS-CLI.md at HEAD also names the subcommand ZERO times, and its seven MTLS-CLIENTCERT
        mentions are FORWARD REFERENCES asserting the OPPOSITE of HEAD -- line 988: "the **client
        certificate** half of mutual TLS is still to come (`MTLS-CLIENTCERT`)"; line 999:
        "certificate** still has no home, and `MTLS-CLIENTCERT` gives it one"; line 37: "before
        `MTLS-CLIENTCERT` teaches the client to present one". That is doc drift asserting UNSHIPPED
        what shipped at 9418a48.
  The earlier status_note ("neither is written yet") was half right and is now stale in the other
  direction: CONTRACTS-CLI.md HAS text about this task, and that text is WRONG. Fixing (b) is a
  correction, not an addition.
  _Proof: go test -race -run 'TestClientGeneratesClientCert|TestClientTLSKeyIs0600' ./client/..._
- [~] None · Derive the bus fingerprint from the certificate, not the log; correct the CONTRACTS-CLI expiry claim — security, P1, in progress
  Two P1 security-gate findings already in main at commit 9f2878a (they reached main via a pathspec-less `git commit --amend` while the code was gated CHANGES-REQUESTED).
  
  P1-1: scripts/bus-serve.sh derived the paste-ready --bus-fingerprint value by grepping bus_cert_fingerprint= out of the mutable log file and taking tail -1. A local attacker who can write that log (e.g. pre-creating /tmp/agent-bus/) makes the operator pin the ATTACKER's certificate -- the exact MITM that "no trust-on-first-use" exists to prevent. Fix: derive the fingerprint from $CERT_FILE (the authoritative self-signed leaf) and delete the log-scrape path entirely.
  
  P1-1b (same file, pre-existing): read_pid did not validate the pidfile contents, so -1 could reach `kill -TERM -1` (signals every process the user owns). Fix: accept only a plain positive decimal.
  
  P1-2: CONTRACTS-CLI.md asserted client-side certificate expiry is NOT checked and that MTLS-EXPIRY is "in flight, not in main", citing a proof that `git show HEAD:client/pin.go` matches no NotAfter/ErrBusCertificateExpired/ParseCertificate. It matches all three at HEAD -- MTLS-EXPIRY landed in 9f2878a. Fix: rewrite the paragraph to state what is true at HEAD.
  
  Files: scripts/bus-serve.sh, CONTRACTS-CLI.md (+ AGENT_LOG.md append).
  _Proof: bash scripts/proof-check.sh 'bash /tmp/claude-1000/-mnt-sdb4-mike-mike-source-agent-bus/b828c013-a5a5-4da0-b21c-d56d21066f9e/scratchpad/fp-proof.sh' -- a live run of scripts/bus-serve.sh start against a real bus with an attacker-planted bus_cert_fingerprint= line in the log, asserting the printed fingerprint equals the sha256 of the DER in $DATA_DIR/bus-tls.crt and NOT the planted value._
- [ ] MTLS-PIN-FU-MIRRORSYNC · MTLS-PIN-FU-MIRRORSYNC: an agreement test that client.BusFingerprint and internal/buscert.Fingerprint cannot silently diverge — security, P2
  Raised by the reviewer gate on MTLS-PIN (2026-08-07). client/pin.go duplicates internal/buscert/fingerprint.go's construction -- sha256 over the leaf's DER, lowercase hex, one spelling -- because client/ may not import internal/ (invariant 7, client/doc.go). The two agree TODAY (the reviewer verified line by line), but NOTHING MECHANICALLY KEEPS THEM AGREEING. This is the same duplication class as client/canonical.go vs internal/signing.
  
  Divergence fails CLOSED (no pin would ever match, so every https connection would be refused) which is the safe direction, but it would present as a total, confusing outage rather than as a test failure.
  
  The fix is a one-way agreement test, and the DIRECTION matters: internal/ MAY import client/, but client/ may NOT import internal/. So the test belongs in internal/buscert (or a package under internal/), importing github.com/dodgymike/agent-bus/client, and asserting that for a set of real certificates buscert.FingerprintOf(cert).String() == the value client's ParseBusFingerprint round-trips and that client's verifier accepts exactly the certificate buscert fingerprinted. Do NOT add an import of internal/ to client/ to make this easier -- that would break the embeddability requirement invariant 7 exists to protect.
  
  Out of scope for MTLS-PIN because internal/buscert was outside that task's file-ownership boundary.
  _Proof: go test -race -run 'TestFingerprintMirrorAgreesWithClient' ./internal/buscert_
- [~] MTLS-LISTENER · MTLS-LISTENER: serve TLS ONLY and REFUSE TO START without a usable cert/key -- there is no plaintext listener — security, P0, in progress
  EPIC: a1b628fb-8cbf-47e8-9682-034fda8636c7 | DEPENDS ON: MTLS-DESIGN, MTLS-BUSCERT | BLOCKS: MTLS-CLIENTAUTH, MTLS-VERIFY
  
  invariant 11. Today cmd/agent-bus/main.go:375 does net.Listen("tcp", cfg.Listen) and main.go:386 does srv.Serve(ln); http.Server at main.go:368-372 sets no TLSConfig and there is no TLS/x509 code anywhere in the tree. Attach via tls.NewListener or srv.ServeTLS. The server must exit non-zero with a remedial message naming the cert path rather than degrading. Config.validate() (main.go:128-152) is purely syntactic and has no data-dir knowledge, so the refusal belongs in run(), not flag parsing. New flags land in CONTRACTS-CLI.md. BREAKING -- escalated: this strands every plaintext client, including scripts/bus-serve.sh's health probe (fixed by MTLS-VERIFY) and CLI-2's recorded proof_cmd.
  _Proof: go test -race -run 'TestServerServesTLSOnly|TestPlaintextClientIsRejected|TestRunRefusesToStartWithoutUsableCert|TestCmdHasNoPlaintextListener' ./cmd/agent-bus && grep -qi 'tls' CONTRACTS-CLI.md_
- [ ] MTLS-CROSSCHECK · MTLS-CROSSCHECK: reject a session token presented over a connection whose client certificate belongs to a DIFFERENT agent — security, P0
  EPIC: a1b628fb-8cbf-47e8-9682-034fda8636c7 | DEPENDS ON: MTLS-BIND | BLOCKS: MTLS-VERIFY, AUTH-2-FU-POLLEXPIRY (03d7ca66)
  
  **THE PART MOST LIKELY TO BE QUIETLY OMITTED -- the user called this out by name. DO NOT fold it into MTLS-BIND and do not complete either task on the other's tests.** CLAUDE.md invariant 11 and DECISIONS.md:1139-1144: mTLS does NOT replace the session token; BOTH are required and they must be CROSS-CHECKED. mTLS proves which key holder is on the connection; the session token is the revocable, time-bounded application credential. Three call sites, all of which must be covered: (1) (*Server).authMiddleware (internal/httpapi/authmw.go:241, which calls s.auth.Authenticate at :277 and attaches the auth.Principal at :299) must compare the connection's peer-cert fingerprint against the fingerprint bound to principal.AgentID; (2) POST /v1/session/begin (internal/httpapi/auth.go:172) takes an agent_id from an unauthenticated body -- a begin naming agent X over agent Y's certificate must be refused; (3) POST /v1/session/complete (auth.go:211) re-reads the roster entry at internal/auth/session.go:344. NOTE httpapi.Options.Auth is the CONCRETE *auth.Service (internal/httpapi/server.go:108), not an interface, so this needs either a new method (e.g. AuthenticateBound(token, fingerprint)) or a new interface seam. A mismatch is a protocol violation, not a routine 401 -- log it as security. Also record in this task that AUTH-2-FU-POLLEXPIRY (03d7ca66) must re-evaluate the cross-check mid-poll, not only at request entry.
  _Proof: go test -race -run 'TestSessionTokenRejectedOnForeignClientCert|TestSessionBeginRejectedOnForeignClientCert|TestSessionCompleteRejectedOnForeignClientCert' ./internal/httpapi ./internal/auth && grep -qi 'cross-check' CONTRACTS-HTTP.md_
- [ ] None · No regression guard exists for the bus-fingerprint trust-anchor fix — test, P1
  cmd/agent-bus/busservewrapper_test.go:133 asserts only strings.Contains(out, "fingerprint "), so a return to the log-scrape would NOT be caught by anything in the repo. Add an assertion to that existing live-wrapper test: plant an attacker bus_cert_fingerprint=<64 hex> line in the log while bus-serve.sh start runs, and assert the printed fingerprint AND the paste-ready enrol line both equal sha256(DER) of $DATA_DIR/bus-tls.crt (i.e. buscert.FingerprintOf), not the planted value. A working standalone version of exactly this proof exists at /tmp/claude-1000/-mnt-sdb4-mike-mike-source-agent-bus/b828c013-a5a5-4da0-b21c-d56d21066f9e/scratchpad/fp-proof.sh (RED at 9f2878a, GREEN after the fix) and should be ported into the Go test. File: cmd/agent-bus/busservewrapper_test.go. It was outside parent task 10e93262-8e34-4738-b435-bfe23d880057's file-ownership boundary.
  _Proof: go test -count=1 -race -run TestLiveBusServeWrapperOverTLS ./cmd/agent-bus_
- [ ] MTLS-PIN-FU-SCHEMEGUARD · MTLS-PIN-FU-SCHEMEGUARD: a direct test for client.transportSecurity, whose unknown-scheme default arm is currently unguarded — security, P3
  Raised by the reviewer gate on MTLS-PIN (2026-08-07), non-blocking. client/transport.go's transportSecurity() decides whether a transport may be built at all: it refuses https with no pinned fingerprint (no trust-on-first-use) and refuses http WITH a pin (no certificate to check). A `default:` arm was added during the security gate so an unrecognised URL scheme fails CLOSED rather than open.
  
  The reviewer mutation-tested it and found the gap: changing that `default:` arm to `return nil` passes the ENTIRE test suite. The function has no direct test -- it is only ever exercised through Client, and parseBusURL admits only http and https, so the arm is unreachable from outside and nothing pins its behaviour.
  
  That is acceptable as defence-in-depth today and would stop being acceptable the moment parseBusURL's scheme list widens, because the guard would already be silently dead. Fix: a small table-driven test calling transportSecurity directly with (scheme, pin) combinations including an unsupported scheme such as ftp://, asserting the refusal and its Kind. Roughly two table entries.
  
  Note this is the same class as the wider MTLS-PIN lesson: a check nothing exercises is indistinguishable from a check that was deleted.
  _Proof: go test -race -run 'TestTransportSecurity' ./client/..._
- [~] MTLS-DESIGN · MTLS-DESIGN: record the decided certificate lifecycle in DECISIONS.md -- key locations, how a client learns the bus fingerprint, rotation, expiry, and the no-plaintext-in-tests answer — security, P0, in progress
  EPIC: a1b628fb-8cbf-47e8-9682-034fda8636c7 | DEPENDS ON: none | BLOCKS: MTLS-BUSCERT, MTLS-LISTENER, MTLS-CLIENTAUTH, MTLS-BIND, MTLS-CLIENTCERT, MTLS-PIN
  
  BLOCKED ON USER DECISION. DECISIONS.md:1131-1147 settles "self-signed, mutual, no CA, bound at enrolment" but leaves these open, and every one of them is load-bearing: (1) how a client learns the bus's cert fingerprint BEFORE its first connection -- the planner recommends the invite blob carry bus-id + address + bus-cert fingerprint + invite secret, which removes the TOFU window entirely and is what makes the two epics genuinely compose, versus plain TOFU-on-first-connect; (2) certificate validity period and what happens when an agent's client cert EXPIRES (re-enrol with a fresh invite, or a re-bind route); (3) bus-key rotation, which invalidates every client's pin -- accepted "operator must re-pin" event, or must the bus serve two certs during a rollover; (4) whether a plaintext escape hatch exists for tests or local dev (invariant 11 says no); (5) whether the cert/key are always self-generated or may be operator-supplied via flags. INVARIANT 9 IS ABSOLUTE: stdlib crypto/tls + crypto/x509, standard fingerprint = SHA-256 over the certificate DER. No hand-rolled fingerprint scheme, cert format, nonce or key exchange -- if a sub-task looks like it needs one, it is mis-specced; stop and escalate.
  _Proof: grep -q 'MTLS-DESIGN' DECISIONS.md && grep -q 'InsecureSkipVerify' DECISIONS.md && grep -qi 'rotation' DECISIONS.md && grep -qi 'fingerprint' DECISIONS.md_
- [ ] None · No behavioural test asserts that a resumed TLS handshake still re-verifies the pinned bus certificate -- only a shape guard holds it — tests, P2
  MTLS-EXPIRY established that crypto/tls does NOT call VerifyPeerCertificate on a RESUMED handshake (handshake_client.go:423 / handshake_client_tls13.go:421, under "Resumptions currently do not reverify certificates"). The security gate reproduced the consequence over live TLS 1.2: with a ClientSessionCache attached, the second connection resumed and was ACCEPTED while the server served a completely unpinned certificate. That is now prevented by TestPinnedSkipIsAlwaysPairedWithAPinCheck, which rejects ClientSessionCache in the pinned tls.Config literal and by assignment -- but that guard is SHAPE-ONLY (an AST walk). There is no behavioural test that a resumed connection actually re-verifies. Exposure today is zero (no cache anywhere in-tree, and every silent spelling now fails the guard), which is why both gates rated this informational and non-blocking. Worth adding: a live-TLS test with session tickets enabled that asserts a RESUMED connection still refuses an unpinned certificate. Raised by the reviewer gate.
- [ ] None · MTLS-MIGRATE: pin add cannot migrate a pre-TLS (http-enrolled) identity onto TLS; its own remedy mints a new id, contradicting DECISIONS.md E3 — client, P0, field-evidence, migration, tls
  Field evidence (2026-08-07), from a real external agent in another repo that migrated across
  tonight's plaintext->TLS switch live:
  
  ```
  $ agent-busctl pin add 68e8...7b14
  agent-busctl: pin: identity ...mic-array-1 enrolled against http://127.0.0.1:18080, which is a
    plaintext URL and presents no certificate
    try: ... enrol against its https URL with `agent-busctl enrol --bus <https-url> ...`
  ```
  
  Every identity enrolled BEFORE the TLS switch is now in a dead end. The agent id still works, but
  only if `--bus https://...` AND `--bus-fingerprint` are passed on EVERY invocation, forever -- there
  is no way to persist either onto an http-enrolled identity (identities.json has no bus_fingerprints
  recorded for it, because none existed at enrolment time). `pin add` refuses outright, and its own
  printed remedy is to re-enrol -- which mints a NEW agent id (invariant 1: ids are never reused) and
  abandons the old one, with no continuity path.
  
  That is the exact outcome DECISIONS.md's E3 entry (line ~1224, "Rotation serves TWO certificates
  during rollover") says must never be the only route: "Rotation must never require every client to
  re-enrol -- that would make routine key hygiene indistinguishable from a security incident." The
  2026-08-07 re-bind decision (DECISIONS.md ~line 2503) restates the same principle for the CLIENT's
  own cert and explicitly enumerates two situations -- (1) cert approaching NotAfter with a still-valid
  session token -> re-bind route (not yet built, see MTLS re-bind route task, public_id 7a197025), and
  (2) cert already lapsed with no prior renewal -> re-enrol is accepted as the correct, deliberate
  answer, new id and all.
  
  THIS FINDING IS A THIRD, DISTINCT SITUATION NOT COVERED BY EITHER: an identity that was enrolled
  entirely over plaintext HTTP, before mTLS/TLS existed at all, and therefore never had ANY client
  certificate or bus-fingerprint pin recorded -- there is nothing here to "renew" (situation 1) and the
  identity's AUTH keypair has NOT lapsed (situation 2 does not apply either; the agent id and its
  Ed25519 AUTH key are still valid and working). `pin add`'s job (per CONTRACTS-ONDISK/CONTRACTS-CLI and
  client/pinset.go) is narrower still: it only lets an identity that ALREADY has a recorded bus
  fingerprint recover from a dropped accept-set (MTLS-ROTATE's MaxBusPins=2 evict path) -- it has no
  notion of bootstrapping a FIRST bus-fingerprint pin onto an identity from a pre-TLS era, nor of
  provisioning that identity's first client certificate for mTLS (MTLS-CLIENTCERT/MTLS-BIND, which as
  scoped only cover the FIRST binding for a brand-new enrolment, not a retrofit onto an existing id).
  
  Cross-reference and disambiguation (do this explicitly in the implementation task):
  - MTLS re-bind route (7a197025, todo, NEEDS USER RATIFICATION) = renews an EXISTING client cert for an
    identity that already has one, authenticated by its still-valid session token. Does NOT help here --
    there is no existing client cert to renew, and the whole problem is bootstrapping the first one plus
    the first bus-fingerprint pin.
  - MTLS-BIND (b6378bda, todo) = binds a client-cert fingerprint to a server-minted agent id at FIRST
    enrolment (new agent, new invite). Does NOT help here either -- it is scoped to brand-new agents, not
    retrofitting a pre-TLS agent id that must be preserved.
  - This finding needs its OWN task: a migration/bootstrap path that lets a pre-TLS (http-enrolled)
    identity, still holding a working AUTH key and a live session, acquire (a) a first client
    certificate and (b) a first pinned bus fingerprint, and bind both to its EXISTING, unchanged agent
    id -- without spending a fresh invite and without minting a new id. If no such path can be made safe
    (e.g. because binding a first client cert to an existing id needs the same anti-spoofing care as
    MTLS-BIND's invite-authorised first binding), the alternative is to say so explicitly and fix `pin
    add`'s error text so it stops recommending an action (re-enrol) that the project's own decisions say
    must not be the only route -- right now the tool's own advice contradicts DECISIONS.md.
  
  Rated P0: it is a migration dead-end affecting 100% of pre-TLS identities the moment TLS is required,
  it contradicts a recorded decision (E3) at the exact moment that decision is supposed to be protecting
  users, and the CLI's own printed remedy makes the outcome worse, not better.
  _Proof: go test -race -run TestPinAdd_PreTLSMigration ./client/... (test to be written: enrol an identity against a plaintext bus, flip that data dir to TLS-only, assert `pin add`/an equivalent migration path lets the SAME agent id acquire a first client cert and bus-fingerprint pin without re-enrolling). Until written, the CURRENT dead-end is reproducible manually exactly as quoted in this task's description via agent-busctl against a bus flipped from http to https mid-lifetime._
- [ ] None · CONTRACTS-ONDISK.md: document the client-side identities.json format and the bus_fingerprints pin set — documentation, P2
  Residual noted by the MTLS-ROTATE feature-runner and confirmed by spec-keeper (2026-08-07): CONTRACTS-ONDISK.md does not describe the client-side `identities.json` store at all (grepped for `identities.json` and `bus_fingerprints`, zero hits). It is the wrong absence to leave silent now: identities.json carries a real, growing on-disk contract --
    - the Credential/Identity record shape (agent id, bus id, name, BusURL, private/messaging key seeds, idempotency key, and now BusFingerprints []string);
    - the BusPinSet bound (client.MaxBusPins=2), refuse-not-evict on a third pin, and refusal to remove the last pin (MTLS-ROTATE, 2026-08-07);
    - the one-way legacy migration from the single `bus_fingerprint` string field to the `bus_fingerprints` array (store format version deliberately NOT bumped -- additive, same reasoning already applied to messaging_key_seed);
    - the downgrade behaviour: an older binary that only READS fails closed (no pin recognised, https refused); one that WRITES the store permanently drops the accept-set, recoverable via `agent-busctl pin add <hex>` rather than re-enrolment;
    - file permissions (0600, O_EXCL temp + fsync + atomic rename).
  
  Most of this is already written correctly in CONTRACTS-CLI.md (JSON shapes, --json field names, exit codes) and in code comments (client/store.go, client/pinset.go) -- this task is specifically about giving CONTRACTS-ONDISK.md its own section for the on-disk FILE FORMAT, matching how it already documents the server-side WAL/record-type on-disk files, so a reader of that document is not left assuming the client has no durable on-disk state at all.
  
  Boundary: CONTRACTS-ONDISK.md only. Verify every field/claim against the current client/store.go and client/pinset.go before writing (do not transcribe CONTRACTS-CLI.md uncritically -- confirm it still matches code, since CONTRACTS-CLI.md itself has needed correction mid-task before).
  _Proof: grep -qi 'identities.json' CONTRACTS-ONDISK.md && grep -qi 'bus_fingerprints' CONTRACTS-ONDISK.md_
- [ ] None · MTLS re-bind route: an agent renews its client certificate against its EXISTING agent id, without spending an invite — security, P1, needs-user-ratification
  EPIC: a1b628fb-8cbf-47e8-9682-034fda8636c7 | DEPENDS ON: MTLS-DESIGN (39dcdcff), MTLS-BIND | NEEDS USER RATIFICATION
  
  MTLS-DESIGN decided (DECISIONS.md, 2026-08-07 entry) that an agent whose client cert is approaching NotAfter, while still holding a valid session token, calls a NEW re-bind route authenticated by that session token to bind the NEW cert fingerprint to its EXISTING, unchanged agent id -- no invite spent, no new identity minted. The rationale is E3's rule that routine key hygiene must not be indistinguishable from a security incident.
  
  THIS ADDS A NEW ROUTE TO THE AUTHENTICATED SURFACE AND WAS DECIDED BY A SUB-AGENT, NOT BY THE USER -- it is RECORDED but NOT RATIFIED, and must not be implemented until the user confirms it.
  
  Deliberately NOT folded into MTLS-BIND, which covers only the FIRST binding at enrolment.
  
  Interaction with AUTH-1-FU-POPKEY (6e3083b0): the re-bind must prove possession of the existing AUTH key, never of the TLS key alone, since the TLS key is exactly what is being replaced.
- [ ] MTLS-RELAYGUARD · MTLS-RELAYGUARD: bus-to-bus relay links are mutually authenticated too -- acceptance criterion plus a guard test — security, P2
  EPIC: a1b628fb-8cbf-47e8-9682-034fda8636c7 | DEPENDS ON: MTLS-CLIENTAUTH, INVITE-PEERGUARD | BLOCKS: RELAY-1 (9bc9d6c4), RELAY-2 (654140d7)
  
  Every relay hop is both a certificate-verifying TLS client and a TLS server, and invariant 2's <bus-id>.<agent-id> addressing plus traversed-bus-path loop prevention must keep working over it. internal/relay/ is a stub (internal/relay/doc.go:8), so the landable increment now is the guard and the acceptance criterion; RELAY-1 (9bc9d6c4) and RELAY-2 (654140d7) must satisfy it (the planner was not permitted to edit those tasks). Pairs with INVITE-PEERGUARD: a peer bus needs BOTH an invite and mutual TLS.
  _Proof: go test -race -run 'TestRelayDialerRequiresMutualTLS|TestRelayListenerRequiresClientCert' ./internal/relay_
- [ ] MTLS-LISTENER-FU-CLIENTHTTP · MTLS-LISTENER-FU-CLIENTHTTP: client/config.go still allows unpinned http:// to loopback, and its own comment says to delete that case when the TLS listener ships — security, P2
  From the MTLS-LISTENER security gate (L3), flagged as out of the runner's boundary and provisional. client/config.go:326-344's case "http": permits unpinned plaintext to any loopback host, carrying the comment // DELETE THIS CASE ENTIRELY when the TLS listener ships. The TLS listener has now shipped. Harmless against this bus (the 400 stops it), but it leaves the CLI with a code path requiring no pin, reachable through any loopback forward (ssh -L, docker publish 127.0.0.1:...). Decide deliberately: delete the case, or keep it and rewrite the comment to say why it survived. Coordinate with whoever owns client/ -- MTLS-EXPIRY was in flight there on 2026-08-07.
  _Proof: go test -race -run 'TestParseBusURL|TestTransportSecurity' ./client_
- [~] MTLS-VERIFY · MTLS-VERIFY: fix scripts/bus-serve.sh's plaintext health probe AND prove a RUNNING bus is TLS-only and mutually authenticated (committed is not running) — security, P1, in progress
  EPIC: a1b628fb-8cbf-47e8-9682-034fda8636c7 | DEPENDS ON: MTLS-LISTENER, MTLS-CLIENTAUTH, MTLS-PIN | BLOCKS: none
  
  Paired committed-vs-running verification per CLAUDE.md. scripts/bus-serve.sh:54 sets HEALTH_URL="http://${LISTEN}/healthz" and curls it at :80 and :161; that is the only surviving bus-*.sh wrapper (AGENTIF-1, done) and it BREAKS the moment MTLS-LISTENER lands, taking every other task's server-startup proof with it. Live assertions required: a plaintext client is refused; a TLS client with NO client certificate is refused; a TLS client with a client certificate and the correct pin reaches /healthz. ALSO FLAG (planner was boundary-blocked from editing them): DEPLOY-1 (fa0c5a4e) and DEPLOY-2 (14f8ec3b) both assume a plaintext listener, and a Compose healthcheck cannot curl plaintext against a TLS-only bus.
  
  === AUDIT 2026-08-08 (spec-keeper): PARTIALLY SHIPPED -- the split, explicitly ===
  MET (in main at 9f2878a, an ancestor of HEAD efde70c):
    - The plaintext health-probe defect is FIXED. scripts/bus-serve.sh:107 at HEAD reads
      HEALTH_URL="https://${PROBE_ADDR}/healthz" and line 113 curls it with --cacert "$CERT_FILE".
      The proof_cmd's second clause (! grep -q 'HEALTH_URL="http://' scripts/bus-serve.sh) PASSES at
      HEAD.
    - TestLiveBusServeWrapperOverTLS exists at HEAD in cmd/agent-bus/busservewrapper_test.go, so the
      proof_cmd's first clause is NON-VACUOUS.
  NOT MET -- the "mutually authenticated" half named in this task's own title, and it cannot be met
  today. The required live assertion "a TLS client with NO client certificate is refused" is FALSE by
  construction: cmd/agent-bus/tlslisten.go:109 at HEAD pins `ClientAuth: tls.NoClientCert`
  DELIBERATELY (its comment at lines 26-31 says moving it is MTLS-CLIENTAUTH's job, and
  MTLS-CLIENTAUTH may not precede MTLS-CLIENTCERT). MTLS-CLIENTAUTH has NOT shipped, so a TLS client
  presenting no client certificate is currently ACCEPTED.
  KEEP OPEN until MTLS-CLIENTAUTH lands, then run the three live assertions together. Do NOT close on
  the two clauses that currently pass -- the title's "mutually authenticated" claim would be false,
  which is exactly the committed-vs-running trap this task was filed to prevent.
  _Proof: go test -race -run 'TestLiveBusServeWrapperOverTLS' ./cmd/agent-bus && ! grep -q 'HEALTH_URL="http://' scripts/bus-serve.sh_
- [ ] MTLS-ROTATE-FU-SERVERSIDE · MTLS-ROTATE-FU-SERVERSIDE: the bus serves ONE certificate, so DECISIONS.md E3's two-certificate rollover is only half built — security, P1
  Raised by the documentation pass on MTLS-LISTENER. MTLS-ROTATE (29cdafc) built the CLIENT half -- a client pins an accept-SET of up to two fingerprints. The SERVER half does not exist: cmd/agent-bus/tlslisten.go puts exactly one tls.Certificate in tls.Config.Certificates, and internal/buscert states it has "no rotation machinery yet". Invariant 11 requires certificate rotation to serve TWO certificates during rollover so clients can re-pin without downtime, and requires that rotation never force every client to re-enrol. Until this lands, a rotation is still an outage.
  _Proof: go test -race -run 'TestBusTLSConfig' ./cmd/agent-bus_
- [ ] MTLS-BIND · MTLS-BIND: enrolment binds the presenting client-cert fingerprint to the SERVER-MINTED agent id -- the invite is what authorises the binding — security, P0
  EPIC: a1b628fb-8cbf-47e8-9682-034fda8636c7 | DEPENDS ON: MTLS-CLIENTAUTH, ENROL-SHAPE, INVITE-GATE | BLOCKS: MTLS-CROSSCHECK, AUTH-3 (d53e3b21)
  
  DECISIONS.md:1146 -- the invite authorises binding a new client certificate to a new agent id; the two happen together, not as two independent gates either of which alone would suffice. Populates the fingerprint field that INVITE-STORE and ENROL-SHAPE reserved, on auth.RosterEntry (internal/auth/roster.go:16-37). INVARIANT 1: the certificate supplies a fingerprint and NOTHING else -- it must not influence the agent id, the name, or the suffix, which are minted by ids.AgentIDMinter.Mint (internal/ids/agentmint.go:360). auth.Roster.Put already refuses a duplicate AgentID rather than overwriting (internal/auth/roster.go:105-107); the same refuse-never-overwrite rule must apply to a fingerprint already bound to a different agent. ORDERING: land before AUTH-3 (d53e3b21, durable roster) or AUTH-3 encodes a durable record that immediately needs migrating.
  
  FORBIDDEN IMPLEMENTATION (security-testing finding, 2026-08-07): the binding here must stay an EXACT-MATCH comparison of the presented certificate's fingerprint (SHA-256 over the DER) against the fingerprint stored on auth.RosterEntry -- never chain verification against an x509.CertPool built from enrolled agents' certificates. client/clientcert.go (~line 550-620) explains why the client-cert template deliberately has IsCA:false and no KeyUsageCertSign: with those set, a CertPool entry would be a TRUSTED ROOT and any agent could mint a certificate for any name that chains to itself and validates, becoming a CA for the whole bus. This binding step is exactly the mechanism that makes chain verification unnecessary -- do not reach for a CertPool 'for consistency' with anything else in the codebase. See also MTLS-RELAYGUARD-FU-BUSCERTPOOL (c873482f) for the same trap on the bus's own dual-usage (ServerAuth + ClientAuth) certificate.
  _Proof: go test -race -run 'TestEnrolBindsClientCertFingerprint|TestEnrolRejectsAlreadyBoundFingerprint|TestClientCertCannotInfluenceAgentID' ./internal/auth ./internal/httpapi && grep -qi 'fingerprint' CONTRACTS-HTTP.md_
- [ ] None · parseBusURL does not canonicalise redundant path slashes/segments, so a differently-spelled retry misses its own idempotency scope key (invariant 10) — security, P1
  Raised by the MTLS-ROTATE security gate (2026-08-07, final kind=response item 6), on the INVARIANT 10 angle rather than the pin angle -- verified independently before filing.
  
  client/config.go:369-380 (parseBusURL) lower-cases the scheme and host and drops a default port, but only trims a SINGLE trailing slash off the path (`u.Path = strings.TrimSuffix(u.Path, "/")`). It does not collapse a doubled slash or normalise `/.`. Confirmed by direct construction:
    - "https://h:8443"   -> Path ""
    - "https://h:8443/"  -> Path ""
    - "https://h:8443//" -> Path "/"   (TrimSuffix removes only the LAST slash)
    - "https://h:8443/." -> Path "/."  (untouched)
  Three distinct u.String() values for what a caller would consider the same bus origin.
  
  This string is not cosmetic -- it IS the idempotency scope key. client/enrol.go:218 and :335 store `BusURL: busURL.String()` on every pending/promoted credential record; client/store.go:1120 and :1215 compare `c.BusURL == busURL`/`want.BusURL` byte-for-byte to find a client's own pending record and to detect idempotency-key reuse. The canonicalisation comment directly above the code (config.go:369-376) already names this exact failure mode for host casing ("https://BUS:443" vs "https://bus") but the fix it describes is incomplete: it never reached the path.
  
  Consequence, per CLAUDE.md invariant 10: a client that retries an enrol (or any BusURL-scoped idempotent call) against a URL spelled with one extra trailing slash builds a DIFFERENT scope key, so `DropPending`/`FindPending`/the conflict check at store.go:1215 never find the original record. The client then re-sends the SAME idempotency key under what the server and the local store both see as a different payload -- invariant 10's protocol-violation case, which is a 409 and a disconnect. The bug is entirely in how the client spelled its own URL; the client did nothing wrong content-wise.
  
  Secondary, minor effect (also true, lower severity): client/client.go's flag-vs-store pin conflict check (`cred.BusURL != u.String()`, MTLS-ROTATE security P2-2/B) is keyed on the same non-canonical string, so the same respelling silently skips that conflict check too. Fix this one file and both problems close together.
  
  Minimal fix (security gate's suggestion, not yet implemented): in parseBusURL, collapse repeated slashes and resolve `.`/`..` path segments (e.g. via path.Clean on u.Path, taking care to restore a leading `/` and to still map an all-slash/empty result to "") before the existing TrimSuffix, so all four spellings above collapse to one canonical value.
  _Proof: go test -race -run 'TestParseBusURLCanonicalisesRedundantPathSegments' ./client/..._
- [ ] MTLS-LISTENER-FU-TLS13 · MTLS-LISTENER-FU-TLS13: raise both ends of the TLS floor to 1.3 and drop the reachable CBC-SHA1 suites — security, P2
  From the MTLS-LISTENER security gate (L2). The server floors at TLS 1.2 to match client/pin.go's pinnedTLSConfig, which is correct today. The gate traced Go 1.19.4's cipherSuitesPreferenceOrder against the Ed25519 leaf and bounded the reachable 1.2 suite set to AES-GCM-{128,256}, ChaCha20-Poly1305 and ECDHE_ECDSA_AES_{128,256}_CBC_SHA. Every reachable suite is ECDHE so forward secrecy holds, Go's server ignores client preference, and the negotiation is signed so there is no downgrade attack -- the gate explicitly did NOT ask for this to change now. Raising BOTH ends to 1.3 removes CBC entirely. Blocked on confirming no non-Go consumer needs 1.2 (an operator's curl --cacert against /healthz is one such consumer).
  _Proof: go test -race -run 'TestBusTLSConfig' ./cmd/agent-bus && grep -n 'VersionTLS13' cmd/agent-bus/tlslisten.go client/pin.go_
- [ ] None · MTLS-RELAYGUARD-FU-BUSCERTPOOL: relay client-cert verification must not build a CertPool of bus/agent certificates -- IsCA/CertSign trap has a larger blast radius for the dual-usage bus certificate — security, P1
  Follow-up from an independent security-testing agent finding on client/clientcert.go (uncommitted at time of report, since landed with IsCA:false and an extensive comment at ~line 550-620 explaining why). That fix and its comment already cover the single-agent-certificate case for MTLS-CLIENTAUTH/MTLS-BIND: do NOT build an x509.CertPool of agent certificates for verification, because IsCA:true + KeyUsageCertSign would make every agent a trusted root able to mint a certificate for any name that chains to itself and validates. Binding must stay fingerprint-based (SHA-256 over DER, exact match), never a pool+Verify.
  
  What is NOT yet covered anywhere: the BUS's own certificate (internal/buscert/buscert.go:636) carries BOTH x509.ExtKeyUsageServerAuth and x509.ExtKeyUsageClientAuth, because a bus both listens for clients and dials peer buses during relay using the SAME certificate. Any future pool-based verification scheme (which this task and MTLS-CLIENTAUTH/MTLS-BIND both already say to avoid, but a relay implementer may reach for independently) would additionally have to reason about a BUS certificate arriving on a client-auth connection during peer relay -- same trap, larger blast radius, since a compromised or malicious peer bus cert would then be a trusted root for the whole mesh rather than one agent's identity. Nobody owns this today; MTLS-RELAYGUARD (8192c3c7) is the landable increment for relay mutual auth and should carry an explicit acceptance note: relay client-cert verification is fingerprint-based like MTLS-BIND, not CertPool-based, and the guard test should assert no code path builds x509.CertPool from enrolled/peer certificates. Add this note to MTLS-RELAYGUARD's description via spec-keeper edit, or reference this task from it, before an implementer picks it up.
  _Proof: grep -rn "x509.CertPool" internal/relay internal/httpapi 2>/dev/null | grep -qi "agent\|peer\|bus" && echo FOUND_POOL_USAGE_REVIEW_NEEDED || echo NO_POOL_USAGE_
- [ ] None · Client-certificate expiry is not enforced anywhere: RequireAnyClientCert does no chain verification, so NotAfter is never checked — security, P1
  EPIC: a1b628fb-8cbf-47e8-9682-034fda8636c7 | DEPENDS ON: MTLS-DESIGN (39dcdcff) | RELATED: MTLS-CROSSCHECK (2b2af075)
  
  tls.RequireAnyClientCert requires a client certificate but performs NO verification, so Go's stdlib TLS handshake does not check the presented client cert's NotBefore/NotAfter. MTLS-DESIGN has now set a 365-day validity policy, but nothing enforces it -- the policy is real and the enforcement path is absent.
  
  Whoever implements MTLS-CROSSCHECK (2b2af075) must EITHER (a) read the presented cert's NotAfter at the application layer after the handshake and reject a connection past it, mirroring the session-token expiry check, OR (b) explicitly decide expiry is advisory and the session-token/revocation layer is the sole enforcement -- and record which in DECISIONS.md.
- [ ] MTLS-CLIENTAUTH · MTLS-CLIENTAUTH: require a client certificate on every connection WITHOUT a CA -- RequireAnyClientCert plus application-layer policy, never InsecureSkipVerify — security, P0
  EPIC: a1b628fb-8cbf-47e8-9682-034fda8636c7 | DEPENDS ON: MTLS-DESIGN, MTLS-LISTENER | BLOCKS: MTLS-BIND, MTLS-CROSSCHECK, MTLS-VERIFY, MTLS-RELAYGUARD
  
  THE LOAD-BEARING SUBTLETY, stated here so it is not discovered by accident. With no CA, tls.RequireAndVerifyClientCert is unusable -- it would need ClientCAs and would reject every client. So the handshake must use tls.RequireAnyClientCert and authorise NOTHING at handshake time; the policy decision moves to the application layer via VerifyConnection/VerifyPeerCertificate plus a middleware. That produces a deliberate asymmetry: the enrolment route MUST accept a cert it has never seen (accepting it is how binding happens), while every other route requires a cert already bound to an agent. internal/httpapi has zero transport knowledge today, so the peer cert must be plumbed from r.TLS through a middleware using the existing ctxKey pattern (internal/httpapi/middleware.go:31, authmw.go:86; next free value is 2). Also ship a permanent guard test that no InsecureSkipVerify exists on any reachable path.
  
  FORBIDDEN IMPLEMENTATION (security-testing finding, 2026-08-07): do NOT verify client certificates by collecting enrolled agents' certificates into one x509.CertPool and calling Verify against it. client/clientcert.go (~line 550-620) documents why in detail: the certificate template deliberately omits IsCA and KeyUsageCertSign for exactly this reason -- with those fields set, a CertPool entry is a TRUSTED ROOT, so any agent could mint a certificate for any name that chains to itself and validates, becoming a CA for the whole bus. Verification here must be fingerprint-based (SHA-256 over the DER, exact match against the fingerprint MTLS-BIND binds at enrolment), never chain/pool verification. See also MTLS-RELAYGUARD-FU-BUSCERTPOOL (c873482f) for the larger-blast-radius version: the bus's own certificate carries both ServerAuth and ClientAuth, so a pool-based scheme would also have to reason about a bus cert arriving on a client-auth connection during relay.
  _Proof: go test -race -run 'TestHandshakeRequiresClientCert|TestUnknownClientCertReachesEnrolOnly|TestNoInsecureSkipVerifyAnywhere' ./internal/httpapi ./cmd/agent-bus_
- [ ] None · Config.HTTPClient lets an embedder bypass certificate pinning entirely — client, P2
  client/config.go (~line 148) -- Client.doer returns an embedder-supplied cfg.HTTPClient before newHTTPClient is reached, so an embedder that sets it gets NO pinning, NO expiry check and no TLS policy from this package. Pre-existing and currently documented rather than prevented; flagged twice by the security gate during MTLS-EXPIRY and explicitly left out of scope. Decide whether to keep it (documented, with the risk stated at the field) or to constrain it -- e.g. requiring the caller to opt in explicitly, or validating the supplied transport's TLS config.
- [ ] None · guard_test.go callback arms accept a nil-valued func variable, so VerifyConnection/VerifyPeerCertificate can be silently nil — tests, P2
  client/guard_test.go's TestPinnedSkipIsAlwaysPairedWithAPinCheck resolves only the LITERAL identifier nil when checking VerifyPeerCertificate and VerifyConnection. Both gates verified independently that a package-level var nilConnVerifier func(tls.ConnectionState) error wired in as VerifyConnection alongside a cache PASSES the guard, and crypto/tls skips a nil func value exactly as it skips a literal nil. Same for a constructor returning a nil callback.
  
  BOTH GATES DECLINED TO BLOCK, and the disagreement between them is the substance of this task, so record it rather than resolving it here:
  - SECURITY proposed accepting only *ast.FuncLit or *ast.CallExpr and erroring on a bare *ast.Ident, mirroring the stricter InsecureSkipVerify arm (which errors on any unevaluable expression).
  - REVIEWER argued AGAINST tightening, because the only stricter rule it could see would reject VerifyConnection: c.verifyConn (the likeliest spelling of a legitimate remedy), and "a guard that rejects its own prescribed fix" is a defect this branch already had once and had to be fixed for.
  
  Note these two proposals are not obviously compatible: a method value like c.verifyConn is an *ast.SelectorExpr, which is neither a FuncLit/CallExpr nor a bare Ident, so security's rule needs a decision about SelectorExpr before it can be implemented. Reaching the hole requires a deliberately-declared nil func var rather than the slip that a literal nil is. The general principle both gates agreed on: a guard is only as good as its false-positive behaviour, since one that fails correct work is one the next agent deletes.
- [ ] None · client/doc.go package documentation does not mention that the pinned certificate's validity window is now enforced — docs, P2
  client/doc.go's "Transport, and the pinned bus certificate" section describes the pin but predates MTLS-EXPIRY, so it does not say the validity window is checked. Not updated because client/doc.go was outside MTLS-EXPIRY's file-ownership boundary.

### EPIC PROCESS — How agents coordinate + backlog integrity (does not ship in the binary)

- [ ] None · Spec Server /export (both format=markdown and format=json) silently drops the commits[] array that /complete correctly persists -- SPEC.md and format=json readers see no commit_sha/test_summary even though the server holds it — tooling, P2
  CORRECTION TO THE ORIGINAL BRIEF (verified 2026-08-07 by spec-keeper before filing, per instructions): the claim "the Spec Server does not persist commit_sha or test_summary at all" is FALSE. It IS persisted. What is actually broken is narrower: the `/export` endpoint (both `format=markdown`, which is what generates SPEC.md, and `format=json`) silently DROPS the commit record, even though the server holds it and two other surfaces expose it correctly.
  
  REPRODUCTION (ran against the live cloud server, project agent-bus, task MTLS-PIN / public_id 8c46dc93-16d0-4eea-8ad3-ac51136551e2, completed with commit_sha=61e6067):
  
  1. Direct single-task GET DOES carry the commit:
     `bash scripts/spec-cloud.sh -s /api/v1/projects/agent-bus/tasks/8c46dc93-16d0-4eea-8ad3-ac51136551e2`
     -> top-level field `"commits": [{"created_at":"2026-08-07T18:56:39.469539+00:00","repo":null,"sha":"61e6067","test_summary":"proof-check.sh verdict=PASS ..."}]` is present and correct.
  
  2. The task LIST endpoint also carries it:
     `bash scripts/spec-cloud.sh -s "/api/v1/projects/agent-bus/tasks?status=done&limit=500"`
     -> every returned task object includes the same `commits` array (verified: of 64 tasks with status=done, 64/64 -- ALL of them -- have a non-empty `commits` array; 0 are missing it at the source-of-truth level). So the blast radius the original brief worried about ("every task ever completed ... has an unverifiable completion claim") does not exist: nothing has been lost.
  
  3. The `completed` event also carries it independently:
     `bash scripts/spec-cloud.sh -s "/api/v1/projects/agent-bus/events?task=8c46dc93-16d0-4eea-8ad3-ac51136551e2&event_type=completed&limit=5"`
     -> `payload` = `{"commit_sha":"61e6067","proof_cmd":"...","test_summary":"proof-check.sh verdict=PASS ..."}`. Same values, third independent surface.
  
  4. THE ACTUAL BUG -- the export endpoint, both formats, drops the field entirely:
     `bash scripts/spec-cloud.sh -s "/api/v1/projects/agent-bus/export?format=json" | python3 -c "import json,sys; d=json.load(sys.stdin); print(sorted(d['tasks'][0].keys()))"`
     -> `['completed_at', 'component', 'created_at', 'description', 'epic_key', 'key', 'position', 'priority', 'proof_cmd', 'public_id', 'section', 'status', 'status_note', 'tags', 'title', 'updated_at']` -- no `commits`, no `commit_sha`, no `test_summary`. Confirmed the same for the markdown export consumed into SPEC.md: `grep -n '61e6067' SPEC.md` returns nothing; the only occurrence of the literal string "commit_sha" anywhere in SPEC.md (`grep -c commit_sha SPEC.md` = 1) is free prose inside an unrelated task's description ("commit_sha will be 10dd7f4 plus ..."), not a rendered field.
  
  DOES /complete ERROR OR SILENTLY ACCEPT? Neither in the sense the brief feared -- it accepts and CORRECTLY PERSISTS commit_sha/test_summary (see reproduction 1-3 above; 64/64 done tasks have it). There is no silent-drop at the /complete or GET layer. The silent drop is specifically in /export's task-serialisation, which uses a narrower field projection than the GET/list endpoints.
  
  WHY THIS STILL MATTERS: SPEC.md is the human/mirror-reading surface (CLAUDE.md: "SPEC.md is a GENERATED MIRROR ... treat it as read-only history that other agents/tools (and humans) can skim"). Anyone who trusts the mirror for "what commit closed this task" sees nothing, even though the server has the answer -- the same *class* of defect as e109c867 (PATCH rejecting `key`): documented workflow and actual server contract disagreeing, just at the export layer rather than at /complete itself. It is P2, not P0/P1, precisely because reproduction 1-3 show no data has actually been lost -- it is a visibility gap in the mirror, not a durability gap in the store.
  
  INTERIM MITIGATION (already standard practice, now written down rather than left to habit): spec-keeper continues to record commit_sha/test_summary redundantly in a `kind=report` note on the task at completion time, in addition to the `commit` API field -- e.g. "Completed with commit_sha=<sha>." This is now belt-and-braces (the primary record survives fine in `commits[]`), but keep doing it because it is the only copy that reaches SPEC.md today, and because free-text notes are more visible to a human skimming the mirror's linked task detail than a field the export layer currently discards.
  
  FIX (out of scope for this bookkeeping task, left for an implementer): have the export serialiser (both format=json and the markdown renderer that produces SPEC.md) include each task's `commits` array, or at minimum the latest entry's `sha`/`test_summary`, alongside the existing fields.
  
  CROSS-REFERENCE: e109c867 (PATCH rejecting `key`) -- same class, workflow/contract mismatch discovered by direct empirical testing rather than by trusting the docs.
  _Proof: bash scripts/spec-cloud.sh -s "/api/v1/projects/agent-bus/tasks/8c46dc93-16d0-4eea-8ad3-ac51136551e2" | python3 -c "import json,sys; t=json.load(sys.stdin); assert t.get('commits'), 'expected commits on direct GET'" && bash scripts/spec-cloud.sh -s "/api/v1/projects/agent-bus/export?format=json" | python3 -c "import json,sys; d=json.load(sys.stdin); mt=[t for t in d['tasks'] if t.get('public_id')=='8c46dc93-16d0-4eea-8ad3-ac51136551e2'][0]; assert 'commits' not in mt, 'export now includes commits -- bug fixed, flip this task'" && echo EXPORT_DROPS_COMMITS_BUG_CONFIRMED_
- [ ] None · Correct stale wave label AUTH-7 to its real task identity across code and docs — process, P2
  The label "AUTH-7" is referenced in roughly 12 Go comments and in CONTRACTS-ONDISK.md, but NO task with that key exists in the backlog -- the work those comments describe is actually tracked as MSG-FU-ROSTERSOURCE (public_id fa26036c). This is a stale/incorrect cross-reference: anyone grepping the backlog for AUTH-7 finds nothing, and anyone reading the code comments is pointed at a task identity that was never real (or superseded and never corrected). Acceptance: every Go code comment and every doc (including CONTRACTS-ONDISK.md) naming AUTH-7 is corrected to reference MSG-FU-ROSTERSOURCE (fa26036c) instead, so the code/doc trail and the backlog agree.
- [ ] None · Audit stored proof_cmds for the subtest-skip vacuous shape (parent-PASS/hidden-child-SKIP), catalogue any affected DONE tasks — process, P2
  Follow-up to cea09b96-72db-40f1-84b4-c2e227eae1cf (the tool fix: proof-check.sh's plain-text counter is column-0-anchored, so indented subtest '--- SKIP:' lines are invisible, letting a parent-PASS/all-children-SKIP test certify PASS instead of VACUOUS). That task fixes the TOOL. This task is about the DAMAGE: some already-`done` tasks' recorded proof_cmd may rest on exactly this shape, meaning the stored evidence for 'done' is weaker than the record implies.
  
  Not hypothetical: in a randomly-selected batch of four tasks closed on 2026-08-08, three had wrong or non-existent proof commands (see PROCESS epic history, e.g. fc8cd234, a9a433dd).
  
  DELIVERABLE: a list of tasks whose stored proof_cmd is vacuous under the corrected (post-cea09b96) rule -- NOT a re-opening of those tasks, and NOT a requirement to fix them. Record the list in a new dated section of AGENT_LOG.md headed exactly 'PROOF_CMD SUBTEST-SKIP AUDIT', naming every task_id examined and its verdict.
  
  PRELIMINARY PASS already run (2026-08-08, spec-keeper, scoped and reported here so the next agent does not re-derive it from scratch):
    - Of 92 currently-`done` tasks with a non-null proof_cmd, 54 contain a `go test ... -run` invocation.
    - Of those 54, 39 have the SPECIFIC risk shape (tests_run > top_level, i.e. subtests exist, AND top-level skipped==0, i.e. any subtest skip would currently be invisible per the cea09b96 bug).
    - Re-ran all 39 with `go test -v` DIRECTLY (not nested through proof-check.sh -- nesting hits the known PROOF-CHECK-FU-RECURSION defect, task 69eb6f56, and corrupts results; confirmed this the hard way: an initial pass that nested proof-check.sh inside itself falsely reported ID-2-WIRING-SEAL's proof (8c9b6489) as FAILING with 5 failures, which evaporated to a clean PASS the moment the same proof_cmd was run WITHOUT nesting -- do not repeat that mistake) and grepped the raw verbose output for indented ('    --- SKIP:') lines.
    - RESULT: zero indented SKIP lines found across all 39 -- none of the currently-done tasks in this sample are resting on a hidden-skip false pass today.
    - One indented FAIL was observed once, transiently, under -race in 39318208 (CLI-2)'s TestEnrolFailedComposesRemedyAndStampsKey subtest; on immediate re-run it passed cleanly. This is NOT an instance of the cea09b96 defect -- unlike a subtest SKIP, a subtest FAIL DOES propagate to the parent's own --- result line and to the process exit code in Go's testing package, so proof-check.sh's existing 'RC != 0 => FAIL' check already catches it regardless of the indentation-counting bug. Flagging as pre-existing test flakiness for whoever owns internal/client's CLI enrol tests, not as a proof-tooling defect.
  
  REMAINING SCOPE for whoever takes this: the other ~38 done tasks with a non-null proof_cmd that do NOT contain `go test -run` (doc/grep-shaped proofs, wrapper-shaped proofs, etc.) are OUT of this specific bug's blast radius by construction (no go test subtests) and do not need re-auditing under THIS rule -- but confirm that assumption rather than assuming it. Also worth re-running the 39-task sweep again AFTER cea09b96 lands, since the fixed tool may report the -json code path differently or reveal something the manual grep missed. Post the full per-task verdict list to AGENT_LOG.md under the heading below, and to this task's own notes.
  
  proof_cmd confirmed RED on 2026-08-08 (heading does not exist yet, phrase absent from AGENT_LOG.md): grep -q 'PROOF_CMD SUBTEST-SKIP AUDIT' AGENT_LOG.md -> exit 1.
  _Proof: grep -q 'PROOF_CMD SUBTEST-SKIP AUDIT' AGENT_LOG.md_
- [ ] None · Triage dispatched two concurrent agents with overlapping ownership of CONTRACTS-CLI.md — process, P2
  Self-reported defect, 2026-08-07: triage (main) dispatched INVITE-MINT and MTLS-ROTATE concurrently, and both agents' tasks required editing CONTRACTS-CLI.md. This is a triage error, not an agent error -- caught only because the INVITE-MINT agent inspected its own diff before staging.
  
  What happened: the shared worktree means a single `git add CONTRACTS-CLI.md` stages whatever ANY agent has written to that file, not just the calling task's hunks. INVITE-MINT's `git add` swept in MTLS-ROTATE's concurrent edits -- the file's working-tree diff was +274/-32 across 11 hunks, and only ONE +134 hunk (the invite section) belonged to INVITE-MINT; the other nine belonged to MTLS-ROTATE (--bus-fingerprint, `busctl pin`, the accept-set, identities.json). The INVITE-MINT agent caught this by diffing its own change before commit and correctly unstaged the file rather than committing another task's work under its own commit message and task id -- but nothing in the process forces that check; it depended on the individual agent noticing.
  
  CLAUDE.md already documents the mechanical half of this failure mode: `git add <paths>` does not scope a later commit (a bare `git commit` after a broad add takes the whole index), and the newer rule (commit d7ebc2b) records that a pathspec-scoped commit takes the WORKTREE at commit time, not the index at add time -- so even a careful `git commit -- CONTRACTS-CLI.md` would have picked up MTLS-ROTATE's uncommitted worktree changes to that same file, not just what INVITE-MINT staged. A shared doc file being edited by two concurrent agents is therefore doubly dangerous: both the index-sweep failure mode AND the worktree-at-commit-time failure mode apply to it simultaneously, and neither is guarded by any mechanism -- only by an agent choosing to diff-inspect before staging.
  
  Recommendation: triage should treat every CONTRACTS-*.md plane file (CONTRACTS-CLI.md, CONTRACTS-HTTP.md, CONTRACTS-ONDISK.md, CONTRACTS-AGENT.md) as a single-owner resource per triage pass, exactly like DECISIONS.md and AGENT_LOG.md already are per CLAUDE.md's 'Parallel-agent coordination' section -- i.e. do not dispatch two concurrently-running agents whose tasks both touch the same CONTRACTS-*.md file; sequence them instead, or route both doc edits through a single agent/pass.
  _Proof: grep -n 'CONTRACTS-\*\.md plane file as a single-owner resource' CLAUDE.md_
- [ ] None · Spec Server: PATCH /tasks/{id} rejects the key field outright (422 Unknown field) -- a keyless task can never acquire one in place — tooling, P2
  Observed 2026-08-07 while bookkeeping the agent-bus backlog. CLI-1-FU-BINARYNAME (public_id 6a1eb5fa-5cfe-4808-a47d-224092f69c14) was created with key: null, and CLAUDE.md / task descriptions across this project cite it by the title-embedded name "CLI-1-FU-BINARYNAME" as if it were a real key -- it is not; it has no key at all.
  
  CORRECTION TO THE ORIGINAL DISPATCH BRIEF, recorded here rather than silently fixed: the brief that raised this described the bug as "PATCH silently ignores key". Empirically that is NOT what happens -- confirmed live against the running server, 2026-08-07. PATCH /tasks/{id} with a body containing "key":"..." returns HTTP 422 {"errors":{"json":{"key":["Unknown field."]}}}. The request is REJECTED, not silently accepted-and-dropped. The observable consequence is the same either way -- a keyless task can never acquire a key post-creation through the documented PATCH surface -- but the mechanism is a loud validation error, not a silent no-op, and the earlier characterisation should not be repeated.
  
  CONSEQUENCE: key is accepted only at creation time (POST .../tasks {"key":"...", ...}) per AGENTS_API.md's 'Create a task' example. There is no documented way to add or change a key on an existing task via the single-task PATCH endpoint. Our own docs and task descriptions routinely cite tasks by key (e.g. "BLOCKS: INVITE-GATE", "DEPENDS ON: MTLS-BUSCERT"); a keyless task silently breaks that convention for anyone or anything resolving by key.
  
  WORKAROUND on record: an export/import round-trip. GET /projects/{slug}/export?format=json returns every task including keyless ones with stable public_id; import is documented as idempotent on public_id (POST /projects/{slug}/import), so editing the key field in the exported JSON before re-importing should update it in place -- not verified end-to-end in this pass, flagged for whoever picks this up to confirm import actually treats key as updatable where PATCH does not.
  
  REPRODUCTION (run 2026-08-07, task subsequently cancelled -- public_id e36661b0-687e-465e-b72f-e33245088e38):
    1. POST /projects/agent-bus/tasks {"title":"probe"}  (no key field) -> 201, public_id=P, key=null
    2. PATCH /projects/agent-bus/tasks/{P} {"key":"PROBE-1"} -> 422 {"errors":{"json":{"key":["Unknown field."]}}}
    3. GET /projects/agent-bus/tasks/{P} -> key is still null, confirming (2) was rejected outright, not applied
  
  Fix: either add key to PATCH's accepted schema (uniqueness-checked, same as at creation), or -- if key is deliberately immutable-after-creation by design -- say so explicitly in AGENTS_API.md's PATCH section so the export/import workaround is the documented path rather than something an agent has to discover by trial and error.
  _Proof: PID=$(bash scripts/spec-cloud.sh -s -X POST "$B/projects/agent-bus/tasks" -H "Content-Type: application/json" -d '{"title":"keypatch-probe"}' | jq -r .public_id) && bash scripts/spec-cloud.sh -s -X PATCH "$B/projects/agent-bus/tasks/$PID" -H "Content-Type: application/json" -d '{"key":"KEYPATCH-PROBE-1"}' >/dev/null 2>&1; bash scripts/spec-cloud.sh -s "$B/projects/agent-bus/tasks/$PID" | jq -r .key | grep -q KEYPATCH-PROBE-1_
- [ ] None · Backfill non-vacuous proof_cmd across the 14 actionable tasks that have none (CLI-1..9 + DUR-4-FU-* + ID-2-WIRING + PROOF-CHECK-FU-RECURSION), and require proof_cmd at completion time — process, P2
  Verified via the Spec Server export this session: 20 of the 137 tasks in the agent-bus project have `proof_cmd == null` (the count given in the brief was correct). Of those 20, 6 are in a terminal state that will never be completed (5 RATCHET-* tasks: d86aaa65, be658b02, 58fd8bc3, e376433d, 9a404c64 -- all `superseded`; and ZZ-LOCKTEST e091e451 -- `cancelled`), so they arguably do not need a backfilled proof_cmd at all, only a decision that they are exempt. The remaining 14 are live/actionable and genuinely need one:
  
    CLI-1 (0495d133), CLI-2 (39318208), CLI-3 (6e70abe5), CLI-4 (137465b9), CLI-5 (86dea094),
    CLI-6 (47001cb4), CLI-7 (e600bde6), CLI-8 (ae4caacc), CLI-9 (93973755),
    ID-2-WIRING (838677e6, currently in_progress -- an owning agent should backfill this one directly rather than have it done for them),
    DUR-4-FU-DOCS (0b6d5c11), DUR-4-FU-DECISIONS (180f11f8), DUR-4-FU-TOOLING (26c2ce16),
    PROOF-CHECK-FU-RECURSION (69eb6f56).
  
  DONE means: every one of the 14 actionable tasks above gets a real, non-vacuous proof_cmd (validated with `bash scripts/proof-check.sh '<cmd>'` before it is saved, exactly as this pass did for its own new tasks) -- for the CLI-* tasks that is naturally deferred until each CLI-N's shape is decided (a `scripts/bus-*.sh`-style invocation or a `go build ./cmd/agent-bus-cli && ...` smoke test, per whichever the implementer lands), and for the terminal 6 either a proof_cmd of `true` with a status_note explaining why, or spec-keeper leaves them proof-less on record as an accepted exemption for non-actionable tasks -- either is fine as long as it is a DECISION, not an omission.
  
  POLICY RECOMMENDATION (the actual point of filing this): a missing proof_cmd should block flipping a task to `done` at LEAST as hard as a VACUOUS one does. Today scripts/proof-check.sh classifies and grades whatever proof_cmd IS supplied, but nothing stops `complete` from succeeding when proof_cmd was never set in the first place -- which is a strictly WORSE version of the vacuous-pass problem this project already fixed once (task 84b76d5e, "a `-run` pattern that matches no test must FAIL, not pass vacuously"): at least a vacuous `-run` pattern names something checkable in principle; a null proof_cmd names nothing at all. Recommend: (1) completing a task should require running `bash scripts/proof-check.sh '<cmd>'` and quoting its verdict in test_summary, not just asserting things worked; (2) the Spec Server's `complete` endpoint (or a spec-keeper-side check ahead of calling it) should refuse a task with proof_cmd unset UNLESS an explicit skip reason is recorded (mirroring how AGENT_LOG.md already carries explicit skip justifications for the reviewer/security chain).
  
  proof_cmd validated via scripts/proof-check.sh: verdict=FAIL (exit 1) today -- 14 actionable tasks currently have proof_cmd unset; the count will read 0 once every one of them is backfilled or explicitly exempted. (Scoped to non-terminal tasks: cancelled/superseded tasks are excluded from the count on purpose, per the exemption discussion above.)
  _Proof: test "$(bash scripts/spec-cloud.sh -s '/api/v1/projects/agent-bus/tasks?limit=500' | jq '[.[] | select(.proof_cmd == null and (.status != "cancelled" and .status != "superseded"))] | length')" = "0"_

### EPIC RATCHET — Ratchet crypto: adopt, do not invent

- [ ] RATCHET-2 · RATCHET-2: Threat model -- what Ed25519 signing defends against, and explicitly what it does not — crypto, P1
  RESCOPED 2026-08-02 per user instruction ("keep it simple, standard sign/verify; encryption later"): this is no longer a ratchet/PFS threat model, it is the threat model for a SIGN-ONLY design. Write down the adversary before further work lands. Who is the attacker -- a compromised bus, a compromised relay peer bus, a network observer, another enrolled agent, someone who later obtains the disk? WHAT SIGNING BUYS: message AUTHENTICITY (this body really was produced by the holder of this messaging private key) and INTEGRITY (this body was not modified in transit), verified by the RECIPIENT -- so a compromised or malicious bus cannot forge a message purporting to be from an agent it does not control, even though the bus relays every message. This is the whole security value of keeping the AUTH keypair (CRYPTO-1/AUTH-1, authenticates to the bus) and the MESSAGING keypair (CRYPTO-3, authenticates to peers) separate -- state that explicitly. WHAT SIGNING DOES NOT BUY, STATE THIS PLAINLY AND WITHOUT HEDGING: NO CONFIDENTIALITY. Without encryption, the bus and any relay peer on a multi-bus path (RELAY-2/3) CAN and WILL read every message body, in cleartext, always. This is now an ACCEPTED property of the system per direct user instruction, not an oversight to be apologized for -- but it must be legible to every future reader of PROTOCOL.md, not discovered by surprise. NO forward secrecy (a compromised messaging private key lets an attacker forge NEW messages as that agent going forward, and there is no ratchet to bound the blast radius -- key rotation via key_epoch, CRYPTO-4, is the only mitigation). NO replay defence from the signature alone (covered separately by SIGN-4's sequence+cursor -- reference it, do not re-derive it here). State plainly which threats are OUT of scope for this rescoped epic (traffic analysis / metadata exposure, a fully compromised endpoint agent, a malicious bus dropping/reordering/duplicating messages -- signing does not stop any of these, only forging content undetected). Without this document the sign/verify choice is unfalsifiable and 'we signed it' becomes a slogan rather than a security property.
  _Proof: grep -rqi 'no confidentiality' THREAT_MODEL.md PROTOCOL.md_
- [ ] RATCHET-6 · RATCHET-6: RFC 8032 Ed25519 known-answer tests wired into the sign/verify implementation — crypto, P1
  RESCOPED 2026-08-02 per user instruction ("keep it simple, standard sign/verify; encryption later"): the construction under test is now Ed25519 (crypto/ed25519, Go stdlib), not a Double Ratchet. MANDATORY, not nice-to-have, per invariant 9 (never write your own crypto -- confirming correct USE of an audited primitive is exactly the discipline invariant 9 demands, since a verifier that accepts everything passes every positive test ever written and 'it round-trips with itself' is not evidence). RFC 8032 publishes canonical Ed25519 test vectors (seed/public key/message/expected signature tuples, including the well-known empty-message and edge-case vectors used across every conformant implementation). Wire a representative set of these into the test suite for whatever function/subcommand SIGN-1/SIGN-2/CRYPTO-10 end up calling crypto/ed25519 through, asserting BYTE-EXACT expected signatures (not just 'it verifies its own output' -- a self-consistent but non-conformant implementation would pass that trivially and still be wrong). This proves our INTEGRATION calls the stdlib correctly (right key format, right message bytes, right signature encoding), not merely that it compiles. Note Go's crypto/ed25519 is itself the reference implementation lineage (adiantum team / Adam Langley's Go ed25519, upstreamed) so a mismatch here would indicate a bug in OUR canonicalisation/wiring (SIGN-1), not in the library.
  _Proof: go test -race -run TestEd25519RFC8032Vectors ./internal/..._

### EPIC RELAY — Bus-to-bus federation

- [ ] RELAY-34 · RELAY-34: Revocation fails OPEN on a WAL discard -- a revoked pinned bus signing key can come back — relay, P1, blocks-relay-17, durability, security
  Found by the security gate during RELAY-10 review (round-3 addendum, finding F2). PeerStore.BusTrustRecord carries the COMPLETE post-transition state on every entry (not a delta), so a discard of the withdrawal (tombstone) record silently REINSTATES the previous generation -- and for the trust table, the previous generation is a pinned bus signing key the operator deliberately REVOKED.
  
  Reproduced end to end against a real wal.Log: PutTrust(bus,key) then RemoveTrust(bus), both acknowledged, PinnedKeys correctly nil. Truncate 8 bytes off the tail of bus.wal -- a torn tail -- reopen: PinnedKeys returns the REVOKED key, active, 1 key pinned. This is reachable through a SUPPORTED path, not an exotic one: invariant 6 (CLAUDE.md) requires recovery to survive exactly this kind of tail damage by discarding and starting anyway, never refusing to boot. Realistic triggers: bit-rot, a torn write, a VM/filesystem snapshot rollback (which un-revokes every pin revoked since the snapshot).
  
  Not reachable today -- PeerStore is constructed nowhere outside internal/relay (RELAY-10 shipped code-complete, unwired). It becomes live the moment RELAY-17 (CrossBusTrust) or RELAY-24 (composition root) wires PeerStore in, since RELAY-17 builds its cross-bus trust anchor directly on this record.
  
  Closing it needs a mechanism the current record design does not have -- this is not a small fix. Candidates raised by the security gate (none applied, read-only gate): (a) refuse to boot -- or at minimum ERROR loudly -- at startup when wal Recovered.DiscardCount > 0 AND the trust table holds any active pins, so an operator is told to re-verify revocations by hand; (b) a durable per-bus REVOCATION FLOOR, independent of the tombstone record itself, that a lost tombstone cannot roll back (structurally the same fix class as RELAY-10s sequence-rewind and swept-tombstone-resurrection defects, but for the specific case where the record that must survive loss is a revocation).
  
  Also: the shipped text (peerstore.go and the matching CONTRACTS-ONDISK.md bullet) claims a discard is fail-closed in the direction that matters and can never install a key this bus did not already hold -- true of Apply() in isolation, false of the system: a discard cannot INSTALL a key, but it CAN fail to REMOVE one, which for a revocation mechanism is exactly the direction that matters. That sentence needs correcting alongside the fix (or immediately, as a documentation-only change, if the mechanism fix lands later).
  
  RELATE TO RELAY-17: the keystone builds its cross-bus trust anchor on this record: its implementer must know which half is sound (routing) and which is not yet (revocation) before consuming PinnedKeys(). A security re-verification of RELAY-10 is running as of 2026-08-08T17:2x and will say precisely what that half is -- post its conclusion as a note on RELAY-17 once it lands, and do not treat RELAY-10 as safe to build on for revocation until this task closes.
  _Proof: go test -race -run TestPeerStoreTrustSurvivesATornWALTail ./internal/relay_
- [ ] RELAY-12 · RELAY-12: agent-bus peer add|list|remove — cli, P0, vacuous-today
  FEDERATION phase, wave 2. Deps: RELAY-10 (durable peer records).
  
  Offline under the dirlock, mirroring invite.go. `--route-for <busID>` installs the static
  next-hop route that makes A->B->C possible. CONTRACTS-CLI.md + AGENT_PROTOCOL.md updated in the
  SAME task (invariant 7 -- a feature without its subcommand+doc is not done).
  
  Owns cmd/agent-bus/peer.go + its test, main.go (wave-2 exclusive), CONTRACTS-CLI.md,
  AGENT_PROTOCOL.md.
  _Proof: go test -race -run TestPeerAddListRemove ./cmd/agent-bus_
- [ ] RELAY-9 · RELAY-9: Peer error-code allow-list admits the three SIGN-7 codes — relay, P2, vacuous-today
  FEDERATION phase, wave 1 (F4). model=sonnet.
  Owns internal/relay/client.go, internal/relay/peercodes_test.go (new).
  
  client.go:296-308's allow-list omits CodeUnsigned, CodeBadSignature, CodeUnpeeredBus
  (handshake.go:66-68) -- which our own RelayHandler emits (relayhttp.go:311-321). Result today:
  "unrecognised error code" instead of a legible failure. Makes every later wave debuggable.
  _Proof: go test -race -run TestPeerErrorCodeAdmitsSignatureCodes ./internal/relay_
- [ ] RELAY-11 · RELAY-11: store/hub can record a MULTI-HOP bus path — hub, P1, vacuous-today
  FEDERATION phase, wave 1 (F6).
  Owns internal/store/message.go, internal/hub/hub.go, internal/hub/audit.go,
  internal/hub/buspath_test.go (new).
  
  Invariant 6's audit trail is the whole reason a relay hop is auditable, and today it is
  unwritable: AuditRecord.BusPath exists and is validated (internal/wal/audit.go:177-181), but
  store.NewMessage hard-codes BusPath: []string{busID} (store/message.go:355) and no hub API accepts
  a path. Thread a path parameter to store.NewMessage; hub.publish accepts a caller-supplied path for
  ingested relayed messages and defaults to []string{busID} for local sends; the audit record carries
  the full path.
  
  NOTE FOR WAVE 2: RELAY-16 (egress admission) also owns hub.go -- it is wave 2, this is wave 1, so
  there is no overlap. Do not start RELAY-16's work here.
  _Proof: go test -race -run TestAuditRecordsMultiHopBusPath ./internal/hub_
- [~] RELAY-6 · RELAY-6: Record the FEDERATION deployment assumptions — docs, P0, in progress
  FEDERATION phase, wave 1 (F1). Owns DECISIONS.md EXCLUSIVELY this wave.
  
  Target topology: laptop(A) <-> internet(B) <-> this machine(C), B is a RELAY HOP. All links
  are SSH tunnels; no bus ever listens publicly; the user is sole operator.
  
  New dated "## 2026-08-08 -- FEDERATION" section in DECISIONS.md. Each ruling needs *what is
  given up* and *what would reverse it*:
  (a) Every bus-to-bus link is an SSH tunnel; no bus listens publicly; operator runs all machines.
  (b) INVITE-GATE (05a5216d) does not block this epic -- with no reachable /v1/enroll the pre-auth
      attacker it exists to stop does not exist. Peer enrolment is operator-driven now; invite
      redemption is later hardening. Reversal trigger, stated mechanically: any bus bound to a
      non-loopback interface, or a tunnel endpoint shared with a non-operator. Given up: single-use
      /expiring/revocable peer admission, redemption audit.
  (c) Peer routes still authenticate a PEER principal -- that is FUNCTIONALITY: roster updates must
      be bound to the connection (internal/relay/doc.go:154-158), and the last bus-path hop must be
      checkable against the sender (doc.go:172-175).
  (d) Local-attacker scenarios are out of scope by operator ruling.
  (e) Peer configuration is an offline `agent-bus peer` subcommand under the dirlock, following the
      `invite mint` / D6 precedent. No new online admin route, no new privilege tier. Given up:
      online re-peering -- a topology change needs a restart.
  (f) Static next-hop routing, not a routing protocol. Given up: topology discovery; a fourth bus
      needs an operator route entry. Right trade for a fixed three-node line.
  
  Standing rules for the whole epic (apply to every RELAY-6..26 task): ownership inside
  internal/relay is per-FILE (new tests in a NEW test file named for the task, never appended to
  relay_test.go/registry_test.go); do not edit DECISIONS.md/AGENT_LOG.md unless the task says so
  this wave; a proof naming a not-yet-written test is VACUOUS not FAIL; judge gofmt by EMPTY OUTPUT
  only; run the mandated reviewer AND security gates; invariant 9 is absolute (crypto/ed25519
  Sign/Verify only -- stop and escalate on anything more).
  
  Verified RED: `grep -c 'SSH tunnel\|ssh-tunnel' DECISIONS.md` -> 0.
  _Proof: grep -n 'every bus-to-bus link is an SSH tunnel' DECISIONS.md && grep -n 'INVITE-GATE does not block the FEDERATION epic' DECISIONS.md_
- [ ] RELAY-25 · RELAY-25: fed-smoke.sh: the epic's deliverable -- three-bus loopback federation smoke test — relay, P0, epic-deliverable, vacuous-today
  FEDERATION phase, wave 5. Deps: RELAY-24 (composition root).
  
  scripts/fed-smoke.sh: three buses on 127.0.0.1:9101/9102/9103, data dirs
  /tmp/fed-smoke-{a,b,c} (NEVER the tracked data/ dir). Peers A<->B and B<->C via `agent-bus peer
  add`, with A carrying `--route-for busC`. An agent on A sends to an agent on C; C receives it
  EXACTLY ONCE; the audit log on each hop shows the bus path, and C's audit entry shows all three
  hops (A, B, C).
  
  The script header MUST state what loopback does NOT prove: tunnel bring-up/flap, NAT/keepalive,
  latency vs RetryHorizonCeiling, pinning through a tunnel. A follow-up task covers a real
  three-host run over actual SSH tunnels; the loopback smoke does not substitute for it.
  _Proof: bash scripts/fed-smoke.sh_
- [ ] RELAY-11-FU-BUSID-ECHO · ids.ValidateBusID echoes an oversized bus id with %q and no length guard — internal/ids, P2
  Raised by the security gate on RELAY-11 (2026-08-08). internal/store/message.go:426-429 calls ids.ValidateBusID on an UNTRUSTED hop with no prior length guard, and ids/busid.go:26 echoes the full id back with %q in its error -- unlike ids.ParseAgentID (agentid.go:155) and internal/relay/path.go:135, which both refuse to echo an oversized value. Unreachable today (no client-reachable caller supplies a bus path yet; bounded to 256KiB by relay.MaxRelayBytes once relay ingest lands, and relay.CheckIncomingPath guards it first). Minimal fix: add a len(b) > 64 check before ValidateBusID in the hop-validation loop and do not echo the id back when it is oversized.
  _Proof: go test -race -run TestValidateBusIDRefusesOversizedEcho ./internal/ids_
- [ ] RELAY-36 · RELAY-36: internal/relay/client.go peerURL accepts a path -- tighten to bare-origin, touches every caller — relay, P2, hardening
  Found during RELAY-10 review. PeerStore.validateBareHTTPSOrigin (peerstore.go) enforces bare-origin (scheme + host [+ port], no path/query/fragment/userinfo/opaque) LOCALLY on durably-stored peer base_url values -- but internal/relay/client.go peerURL, the function that actually DIALS a peer, does not enforce the same constraint: it accepts an arbitrary path component (e.g. https://h.example/some/path or https://h.example:9443/../../x both encode and dial). PeerStore's own validation is strictly MORE restrictive than what the dial path will later accept, so no durable value violates the dial contract today -- but the dial-side function itself is the wrong place to be permissive, since any future caller of peerURL that is NOT gated by PeerStore's validation inherits the gap.
  
  Fix (not applied here): in peerURL, add a check equivalent to u.Path == "" || u.Path == "/" (and reject ForceQuery, matching the fix already applied to validateBareHTTPSOrigin during RELAY-10). NOT done as part of RELAY-10 because peerURL is called from every relay dial site (client.go, forward.go, and their tests) and tightening it is therefore a cross-cutting change outside RELAY-10's file boundary (peerstore.go / peerstore_test.go / CONTRACTS-ONDISK.md only).
  _Proof: go test -race -run TestPeerURLRejectsAPath ./internal/relay_
- [ ] None · RELAY-9-FU-CODEGUARD: AST guard asserting every peer error code constant has a handler case — relay, P2
  internal/relay/client.go's peerErrorCode carries an allow-list of recognised peer error codes, synced BY HAND against the Code* constants declared in internal/relay/handshake.go. RELAY-9 added three that were missing (CodeUnsigned, CodeBadSignature, CodeUnpeeredBus) -- codes our OWN handlers emit -- which until then surfaced to operators as "unrecognised error code".
  
  Both the reviewer and the security gate on RELAY-9 independently recommended a guard against this drift. It was correctly NOT folded into RELAY-9 itself -- RELAY-9's definition of done was the three codes plus tests (delivered exactly, see internal/relay/peercodes_test.go).
  
  Definition of done: a test that walks handshake.go's declared Code* constants via go/ast and asserts each appears as a case in client.go's peerErrorCode, failing loudly when a new constant is added without a handler. Model it on the existing in-tree pattern at client/guard_test.go (an AST walk, not a grep -- this repo has already been bitten by grep-based guards passing on incidental matches).
  
  Priority note: this is drift prevention, not a live defect -- the allow-list is correct today. It becomes materially more valuable as the FEDERATION epic proceeds: RELAY-17 and RELAY-20 will add ingest paths that emit new error codes, and each is a fresh opportunity for the hand-sync to fall behind.
  
  Related to RELAY-9 (public_id 06f5e347-fc8f-45d4-a65d-2af08340dd63), the task that surfaced this gap.
  _Proof: go test -run TestPeerErrorCodeHandlesAllDeclaredCodes ./internal/relay/..._
- [ ] RELAY-16 · RELAY-16: Egress admission: /v1/send accepts a routable remote recipient — hub, P1, vacuous-today
  FEDERATION phase, wave 2. Deps: RELAY-11 (multi-hop bus path).
  
  /v1/send accepts a routable remote recipient via a RemoteRouter seam on the hub (nil => today's
  behaviour exactly, so this is additive not a behavior change for the non-federated case).
  
  The roster check for LOCAL recipients stays BEFORE the durable write -- that is cca64afd's
  precondition; relate it (do not duplicate): "RELAY precondition: roster-check LOCAL recipients
  before the durable write, or a peer can permanently exhaust an agent name."​
  _Proof: go test -race -run TestSendAdmitsRemoteRecipientViaRemoteRouter ./internal/hub_
- [~] RELAY-8 · RELAY-8: Registry.PeerBaseURL accessor + concurrency contract — relay, P1, in progress, vacuous-today
  FEDERATION phase, wave 1 (F3). model=sonnet.
  Owns internal/relay/registry.go, registry_test.go, and the doc comment at
  internal/relay/forward.go:199-202 (COMMENT ONLY -- no other task owns forward.go this wave).
  
  The Registry's own accesses ARE lock-protected (registry.go:298-304, :561-565). The defect is that
  no accessor exists, so every wiring site hand-writes a closure that Enqueue (forward.go:602) and
  each worker (forward.go:856) call concurrently -- and ForwarderOptions.PeerBaseURL's doc never says
  "safe for concurrent use" the way its two siblings do. Add Registry.PeerBaseURL(busID) (string, bool)
  under RLock; state the contract; test that a RemovePeer is observed by an in-flight retry.
  
  NEAR-DUPLICATE FLAGGED BY SPEC-KEEPER 2026-08-08: task ef6c4645 ("Relay forwarder's PeerBaseURL
  callback: give Registry a concurrency-safe accessor and state the contract", filed by the security
  gate) describes the SAME defect with the SAME proof_cmd. Not merged (outside this pass's explicit
  merge instruction) -- related instead; whoever picks this up should resolve the two into one before
  implementing, most likely by superseding ef6c4645 into this numbered task.
  _Proof: go test -race -run TestRegistryPeerBaseURLObservesRemovePeer ./internal/relay_
- [ ] RELAY-23 · RELAY-23: Relay wire protocol version — relay, P1, vacuous-today
  FEDERATION phase, wave 4. Deps: RELAY-17 (CrossBusTrust/envelope), RELAY-20 (peer routes).
  
  Uses the relay-wire-version reservation namespace (freshly reserved 2026-08-08, unseeded -- first
  call returns 1). MUST NOT reuse the "version" JSON key: RosterUpdate.Version is the roster epoch,
  and two meanings on one key is how a peer applies an epoch as a format number.
  _Proof: go test -race -run TestRelayEnvelopeCarriesDistinctWireVersionKey ./internal/relay_
- [ ] None · RELAY-2-FU-DURABLE-OUTBOX: Durable relay outbox: Forwarder's queue is in-memory and lossy — relay, P1
  internal/relay/forward.go's per-peer queues are in-memory. A message accepted by Enqueue is lost if the process crashes with a non-empty queue, and dropped (counted in Stats().Dropped.Full, logged at Warn) when a peer stays down long enough to fill its queue. There is no retry-across-crash path. RELAY-4 owns retry/backoff; this task owns the DURABLE outbox they need to be meaningful. Until both land, cross-bus delivery is BEST EFFORT and no doc may claim otherwise (already stated in internal/relay/doc.go and forward.go).
  
  Merged 2026-08-08 (spec-keeper) from duplicate fec942b4 (superseded into this, the earlier-filed task): Dropped.Full, Dropped.Expired and Dropped.Yielded are ALL silent data loss paths this task must close. Constraint any design inherits: the total retry horizon must stay inside idem.PeerOutageBudget (24h), enforced in NewForwarder -- do not exceed it when sizing outbox replay/backoff.
  _Proof: go test -race -run TestRelayOutboxSurvivesCrash ./internal/relay_
- [ ] RELAY-19 · RELAY-19: Forwarder writes and settles outbox records (part 2 of 2) — relay, P1, vacuous-today
  FEDERATION phase, wave 3. Deps: RELAY-8 (Registry.PeerBaseURL accessor), RELAY-15 (outbox
  record + replay, part 1).
  
  Part 2 of 2: the Forwarder itself now writes and settles durable outbox records (RELAY-15 built
  the record/replay machinery; this task wires the Forwarder to use it on the write and settle
  paths).
  _Proof: go test -race -run TestForwarderSettlesOutboxRecords ./internal/relay_
- [ ] RELAY-35 · RELAY-35: PeerStore composition-root precondition -- replay MUST run before the first write, and the package cannot enforce it — relay, P2, durability
  Found during RELAY-10 review. PeerStore.config_seq (the bus-wide monotonic high-water mark that the whole record design relies on -- see RELAY-10) rebuilds ONLY from records the store's Apply() sees during wal replay. NewPeerStore/PeerStoreOptions.Durable takes a *wal.Log directly rather than owning the replay itself, so nothing stops a caller from constructing a PeerStore, skipping replay, and calling Put/PutTrust immediately: config_seq starts at 0 and mints seq=1 against a log that may already hold 1..N -- reintroducing RELAY-10's P0 sequence-rewind defect via the wiring path rather than the record-store internals.
  
  The peerstore package CANNOT enforce this itself -- it is documented today only as a caller precondition on PeerStoreOptions.Durable (a doc comment). RELAY-24 (composition root: wire federation into cmd/agent-bus/main.go) is exactly the place this precondition becomes load-bearing and exactly the place most likely to get it wrong under time pressure, since it is one call among many in server startup.
  
  Scope for RELAY-24's implementer: design out the footgun rather than trusting the doc comment -- options include (a) the constructor owns the replay itself (PeerStore.Open(path) rather than NewPeerStore(existingLog)), or (b) an internal replayed latch that Put/PutTrust/Remove/RemoveTrust check and refuse before, with a clear error naming the precondition. Either way, main.go's startup sequence must be ordered: open the WAL for the peer store, replay it fully into the store, THEN accept the first mutating call -- and a test in the RELAY-24 wiring package should assert that ordering, not just eyeball it.
  
  Flagged now, filed against RELAY-24 rather than left only in a doc comment, per CLAUDE.md's rule that an unenforceable precondition on the composition root needs to be visible on the wiring task.
  _Proof: go test -race -run TestFederationStartupReplaysPeerStoreBeforeFirstWrite ./cmd/agent-bus_
- [~] None · internal/relay/doc.go still specifies per-connection disconnect on idempotency-key-reuse-with-different-payload, contradicting invariant 10 as narrowed 2026-08-08 — relay, P2, in progress, doc-only, invariant-10, spec-defect
  internal/relay/doc.go:246-250 (comment on RelayHandler, section "RELAY-2 and RELAY-3") reads:
  
    One more handoff MTLS-RELAYGUARD owns: invariant 10 requires that an
    idempotency key reused with a DIFFERENT payload is rejected, logged AND THE
    OFFENDING PEER DISCONNECTED. RelayHandler does the first two (409 plus a log
    line that says so); it cannot close a connection it does not own. The gate
    task must wire the disconnect.
  
  This is now WRONG on the object-level fact, not just stale wording. Invariant 10 was
  narrowed 2026-08-08 (code: commit 1c6c540, "Aim invariant 10's disconnect at the
  replayer, not at the confused client"; contract: commit 0dbb025, CLAUDE.md). Same
  idempotency key + DIFFERENT payload is reject-and-log ONLY -- it no longer disconnects,
  on EITHER /v1/send or /v1/enroll, because the key is scoped to the caller's own agent
  and reusing it is evidence of a confused client, not an attacker. The ONE case that
  still disconnects is third-party replay of an already-accepted signed message
  (sender-mismatch on checkSignedMint), which relay ingest has not built a path to yet.
  
  WHY THIS IS WORSE THAN A STALE COMMENT, NOT JUST STALE. Relay ingest (RelayHandler)
  is not yet built (the whole surface is gated behind INVITE-PEERGUARD f5d91dbe and
  MTLS-RELAYGUARD 8192c3c7 and "NOT REGISTERED ON ANY MUX"). When someone DOES build it,
  a peer bus legitimately presents a sender that is not the connection's principal, for MANY
  AGENTS AT ONCE on one relay connection -- a peer relays traffic on behalf of its whole
  local roster over one link. An implementer who inherits doc.go's literal instruction
  ("the gate task must wire the disconnect" on key-reuse) would either (a) wire a
  same-payload-reuse disconnect that invariant 10 no longer wants at all, or worse,
  (b) generalize the ALREADY-CORRECT third-party-replay disconnect to fire at the
  connection level on this multi-tenant link, dropping EVERY agent behind that peer bus
  simultaneously over one agent's buggy or malicious traffic. That is the exact
  "abuse defence aimed at the wrong party" defect this project has hit four times
  before (see 1c6c540's own commit message), one scale up: instead of disconnecting one
  confused client, it would disconnect an entire federated bus's worth of agents.
  
  THE TEST TO APPLY, per invariant 10 as narrowed (CLAUDE.md, and 0dbb025's own wording):
  before wiring ANY disconnect, ask (1) can a merely BUGGY client reach this line, and
  (2) does this connection carry only ONE principal's traffic? For relay ingest the
  answer to (2) is NO -- one relay connection multiplexes an entire peer bus's roster --
  which is precisely why a connection-level disconnect is the wrong mechanism here even
  for the one case (third-party replay) that legitimately disconnects a single agent
  elsewhere in the codebase. doc.go's own "Loop prevention is AVAILABILITY, never
  security" section (lines 252-262) already reasons correctly in this direction for a
  DIFFERENT mechanism (loop suppression); the idempotency paragraph two sections above it
  does not yet carry the same care.
  
  SCOPE: internal/relay/doc.go is a comment-only file (no registered handlers -- see the
  file's own "NOT REGISTERED ON ANY MUX" banner), so this is a documentation/specification
  fix, not a behavior change: rewrite lines 246-250 to state what invariant 10 actually
  requires post-narrowing (reject+log only for key-reuse-with-different-payload; the
  disconnect, if relay ever needs one, belongs to third-party replay of an accepted
  signed message, and even that needs a scoping decision for a multi-principal
  connection that plain per-socket disconnect does not answer). Do not invent that
  scoping decision here -- name it as an open question for whoever builds RelayHandler
  for real (MTLS-RELAYGUARD / RELAY-2), since a connection-level primitive is very
  plausibly the wrong tool for a multi-tenant relay link and the actual mechanism
  (e.g. per-origin-agent rejection without dropping the transport) needs its own design.
  
  Rated P2: this is a specification defect in CODE THAT DOES NOT RUN YET (RelayHandler is
  gated off any mux), not a live vulnerability -- do not inflate it. It earns urgency from
  being read-and-trusted by whoever builds relay ingest next, not from being exploitable
  today.
  
  Cross-reference: 1c6c540's own commit message flags this exact file/lines as
  "unreconciled" ("internal/relay/doc.go already specifies OFFENDING PEER DISCONNECTED...
  the relay ingest path must reconcile with this narrowing rather than inherit it").
  This task is that reconciliation, filed rather than left as a commit-message footnote.
  _Proof: go test -race -run TestPackageDocDoesNotReviveTheWithdrawnDisconnect ./internal/relay_
- [ ] None · Choose the abuse-control primitive for a MULTI-PRINCIPAL relay link — relay, P1
  Lift the OPEN QUESTION out of internal/relay/doc.go (section "Key reuse is REJECT-AND-LOG") and into the backlog, so it is not inherited by accident from a package comment. Invariant 10 as narrowed 2026-08-08 keeps exactly one disconnect -- third-party replay of an accepted signed message -- but a relay connection MULTIPLEXES AN ENTIRE PEER BUS'S ROSTER, so sender != the connection's principal is the NORMAL correct shape and a per-socket disconnect would drop every agent behind that peer over one agent's traffic. Per-origin-agent rejection without dropping the transport, per-peer rate limiting, and peer-level de-peering are all plausible and have different blast radii. Deliver the DECISION with its rationale in DECISIONS.md before any disconnect is wired onto a relay surface. Gated on MTLS-RELAYGUARD (8192c3c7).
  
  Raised P2->P1 2026-08-08 (spec-keeper, FEDERATION phase): the RELAY-20/21 ingest handler cannot be written without this answer, and the default it would otherwise silently inherit (messages.go:656 disconnect) drops every agent behind a peer bus. Consumed by RELAY-22, which owns DECISIONS.md in its wave and depends on RELAY-17 -- do not duplicate this task, relate.
  _Proof: grep -n "multi-principal relay link" DECISIONS.md_
- [ ] RELAY-38 · RELAY-38: signed-relay-ingest comments and docs are silent on the CodeInvalidRelay path RELAY-27 added — relay, P2
  internal/relay/handshake.go:47-66 and internal/relay/message.go:44-53 still describe signed relay ingest as emitting THREE codes (CodeUnsigned/CodeBadSignature/CodeUnpeeredBus). Since RELAY-27 (commit 06e3cc5), the same ingest path can also answer CodeInvalidRelay (400) via attributionError mapping attest.ErrInvalid -> ErrInvalidRelay. The in-code comments were not updated and now undercount the codes the path can emit.
  
  PROTOCOL.md:960-969 and CONTRACTS.md:280 remain ACCURATE as written but are SILENT on the new attest->relay mapping: they document the original three-code table without a row or note for the attest.ErrInvalid->invalid_relay case now reachable from signed relay ingest specifically (as distinct from CodeInvalidRelay's original roster/relay-validation meaning at message.go).
  
  FLAG AS WANTING AN OWNER BEFORE RELAY-17 LANDS: RELAY-17 (CrossBusTrust implementation + attestation travels in the relay envelope) is P0 and its implementer will read exactly these comments as the current contract for the signed-ingest error surface; stale comments at the exact seam RELAY-17 extends is how the next drift gets introduced.
  
  Fix: update the comment blocks at handshake.go and message.go to mention the CodeInvalidRelay path (and its source, attest.ErrInvalid), and add a row/note to PROTOCOL.md's signature-error table (960-969) and CONTRACTS.md's status-mapping table (273-282) documenting the attest.ErrInvalid -> CodeInvalidRelay(400) mapping for the signed-ingest path.
  
  Filed from RELAY-27 follow-ups (RELAY-27 commit 06e3cc5).
  _Proof: grep -A20 'Signed relay ingest' internal/relay/handshake.go | grep -q CodeInvalidRelay && grep -n attest.ErrInvalid PROTOCOL.md CONTRACTS.md | grep -qi invalid_relay && echo DOCS_UPDATED_
- [ ] None · RELAY-15-FU-CAPACITY-FAIRNESS: Outbox capacity is a 24h throughput ceiling and is not per-peer fair — relay, P1
  MaxOutboxJobs (= MaxPeers 64 x DefaultQueueDepth 256 = 16,384) reads as an in-flight bound but is not one. internal/relay/outbox.go's admission check counts every RETAINED record, and a settled record is kept as a tombstone for OutboxSettledRetention (24h). So the real bound is at most 16,384 enqueues PER 24-HOUR WINDOW -- about 0.19/s across ALL peers combined -- after which Enqueue returns ErrOutboxCapacity with nothing in flight. Measured: at maxJobs=8, eight enqueue-then-deliver cycles exhaust it. RELAY-19 hits this on its first busy day.
  
  The security gate ruled the deferral acceptable but the SCOPE wrong: the cap is GLOBAL, the origin-message-id half of the job id is PEER-SUPPLIED on a multi-hop path (message.go sets it from the relay envelope), and relay ingress has no rate limit -- so ONE peer can consume the whole budget and halt relay for every other peer. This task must therefore deliver PER-PEER fairness, not merely a bigger constant. Options noted by both gates: split the cap so pending jobs and tombstones are bounded separately, and/or a per-peer sub-quota.
  
  RELAY-15 documented the true bound in MaxOutboxJobs's comment rather than re-deriving it, because the right value depends on a target rate that task did not know.
  
  BLOCKS RELAY-19: this task must land before RELAY-19 ships since RELAY-19 exercises this ceiling on its first busy day.
  _Proof: go test -race -run TestOutboxCapacity ./internal/relay_
- [ ] None · RELAY-13-FU-MSGKEYPOP: no proof-of-possession of the messaging private key at enrolment, and the field is write-once with no update route — auth, P2, accepted-gap, key-rotation, messaging-key, proof-of-possession
  RELAY-13 (97f3f1b4-8575-4f63-9196-96bfbc049510) registers an agents messaging PUBLIC key at enrolment but never asks for proof that the enroller holds the matching PRIVATE key -- security raised this as a P2 twice (initial audit and re-verification) and it was deliberately accepted, not fixed, in that task. Two distinct gaps, both real, neither a P0/P1 (impact bounded -- see below), both DOCUMENTED IN CODE without overclaiming per the RELAY-13 gates:
  
  1. NO PROOF OF POSSESSION. Any enroller may register ANY 32-byte value as `messaging_public_key`, including a key it does not hold the private half of. Impact is bounded: trust is keyed agent-id -> key (client/keyring.go:174) and attest.Verify check 1 binds AgentID, so this does not enable forging ANOTHER agents signature -- the exposure is misattribution/false-attestation-binding for the enrolling agent itself, not third-party forgery. Verified NOT covered by AUTH-1-FU-POPKEY (6e3083b0-c113-4b26-9dd6-025825671ceb, todo) -- that task is explicitly scoped to the ENROLLING/AUTH key (signature over name||public_key||idempotency_key), confirmed by direct read of its description; it does not mention the messaging key at all. The RELAY-13 reviewer independently queried the Spec Server (limit=1000, 414 tasks) and found no task covers this.
  
  2. NO UPDATE ROUTE. auth.RosterEntry.MessagingPublicKey is write-once: MemoryRoster.Put and WALRoster.Put both return ErrDuplicateAgentID on an existing agent id, and Mint always allocates a fresh id, so an agent that enrolled before it had a messaging key (or whose seed becomes damaged, see client/store.go:256 damagedMessagingSeedRemedy) can only fix this by re-enrolling under a NEW agent id, spending a fresh invite and losing continuity of identity with its peers roster/trust entries.
  
  Definition of done for a first slice: decide and record in DECISIONS.md whether (a) enrolment should require a signature over the messaging public key using the messaging private key itself (a self-signed binding -- cheap, closes gap 1 without a second round trip) and/or a rotation route bounded by session auth (closes gap 2), or (b) these are explicitly deferred to CRYPTO-4 (key-distribution endpoint -- server-attested messaging key bundles, 13f3947e, todo) as the natural place key lifecycle lands. Either way this task is DONE when the decision is recorded and, if implementation is chosen, the code + tests land; if deferred, this task closes as a design decision with CRYPTO-4 cited as the tracking task and this ones id cross-referenced there.
  
  Relate to: RELAY-13 (97f3f1b4), AUTH-1-FU-POPKEY (6e3083b0, different key), CRYPTO-4 (13f3947e, key-distribution/lifecycle).
  _Proof: grep -n "RELAY-13-FU-MSGKEYPOP" DECISIONS.md_
- [ ] RELAY-39 · RELAY-39: AST guard pinning TestErrorCodeIsStable's premise -- every sentinel relayhttp.go tests before ErrorCode is also an ErrorCode() arm — relay, P2
  internal/relay/peer_test.go:164 TestErrorCodeIsStable is a hand-written table asserting ErrorCode(err) for a fixed list of wrapped sentinels. RELAY-27's severance guard (internal/relay/signed.go, ErrorCode(err) != CodeInternal) depends on an UNVERIFIED premise: that every sentinel internal/relay/relayhttp.go tests via errors.Is BEFORE calling ErrorCode is also covered by an arm inside ErrorCode() itself (peer.go:350). If a future relayhttp.go change adds an errors.Is check for a new sentinel without giving ErrorCode() a matching arm, that sentinel silently falls to the CodeInternal default -- which is exactly the class of drift RELAY-27's severance guard is built to survive, so the guard's own correctness rests on this staying true, untested.
  
  Definition of done: an AST-based (not grep) guard test that walks relayhttp.go's errors.Is(...) call sites (or a package-scoped list they consult) and asserts each named sentinel has a corresponding case in peer.go's ErrorCode() switch, modelled on the existing in-tree AST-guard pattern at client/guard_test.go -- an actual go/ast walk, not string matching, per this repo's standing lesson that grep-based guards pass on incidental matches.
  
  RELATION TO RELAY-9-FU-CODEGUARD (public_id 1e9b54d2-f529-4c91-a02b-116cc11bc829): same drift CLASS (hand-synced tables in the relay error-code plane going stale) but a DIFFERENT DIRECTION and different files. RELAY-9-FU-CODEGUARD guards handshake.go's declared Code* constants against client.go's peerErrorCode allow-list (the CLIENT side recognising a peer-emitted code). This task guards relayhttp.go's tested sentinels against peer.go's ErrorCode() switch (the SERVER side mapping a sentinel to a code). DECISION: kept as a SEPARATE task rather than widening RELAY-9-FU-CODEGUARD, because the two guards check different function pairs in different files with independently satisfiable definitions of done -- folding them together would blur an otherwise atomic, independently completable increment for each. A cross-reference note was posted on both tasks.
  
  Filed from RELAY-27 follow-ups (RELAY-27 commit 06e3cc5).
  _Proof: go test -race -run TestErrorCodeGuardCoversRelayHTTPSentinels ./internal/relay_
- [ ] RELAY-15 · RELAY-15: Durable outbox record + replay (part 1 of 2) — relay, P1, vacuous-today
  FEDERATION phase, wave 2. Consumes the merged outbox task 2309e7ed
  (RELAY-2-FU-DURABLE-OUTBOX, canonical after 2026-08-08 merge with the duplicate fec942b4) --
  relate, do not duplicate.
  
  Part 1 of 2: record and replay only. The Forwarder itself is UNTOUCHED this task (RELAY-19 is
  part 2: forwarder writes and settles outbox records). Constraint inherited from 2309e7ed: total
  retry horizon must stay inside idem.PeerOutageBudget (24h).
  _Proof: go test -race -run TestOutboxRecordAndReplay ./internal/relay_
- [ ] RELAY-20 · RELAY-20: Mount /v1/peer/{enroll,relay,roster} behind a PEER principal — httpapi, P0, critical-path, vacuous-today
  FEDERATION phase, wave 3. Deps: RELAY-17 (CrossBusTrust), RELAY-18 (import guard replaced).
  
  Do NOT add peer paths to unauthenticatedRoutes -- that would create the ungated federation path
  the guard forbids. Routes register only when registry AND trust are both non-nil (nil => 404,
  NEVER a registered-503).
  _Proof: go test -race -run TestPeerRoutesRegisterOnlyWithRegistryAndTrust ./internal/httpapi_
- [ ] RELAY-37 · RELAY-37: peerstore.go:690 unparseable-URL error breaks the file's own elidePeerText(64) discipline — relay, P3, low, security-finding
  Found by the security gate's RELAY-10 round-4 re-verification (2026-08-08). LOW severity, bounded, not a blocker on RELAY-10.
  
  internal/relay/peerstore.go:690 -- validateBareHTTPSOrigin's unparseable-URL branch:
  
      return fmt.Errorf("%w: the peer base URL is unparseable: %v", ErrInvalidPeerRecord, err)
  
  `err` is url.Parse's full *url.Error, which embeds the WHOLE base_url string -- and this fires on
  the DECODE path, where those bytes come off a (possibly damaged) log record, not from a live
  operator input. Measured: a 400-byte control-char base_url produces a 1738-byte error string.
  
  It is bounded (base_url is length-checked to 512 bytes before this point), so this cannot grow
  without bound -- hence LOW, not MEDIUM. But it is the one call site in this file that breaks its
  own elidePeerText(64) discipline: every other decode-path error in peerstore.go truncates
  untrusted text before including it (see :240, :431, :609, :693, :730), and this one does not.
  
  Minimal fix, either is acceptable:
    - report len(base) instead of the parsed value, or
    - wrap the offending text through elidePeerText before inclusion (matching every sibling call
      site in this file).
  
  P3. Not urgent, but cheap, and worth closing opportunistically alongside RELAY-17 or RELAY-24 when
  someone is next in this file, so the file's own stated discipline stops having an exception.
  _Proof: go test -race -run TestValidateBareHTTPSOriginUnparseableErrorIsBounded ./internal/relay_
- [ ] None · RELAY-13-FU-DOCS: three docs/comments assert the opposite of shipped RELAY-13 behaviour -- BLOCKS marking RELAY-13 done — docs, P1, blocks-done, doc-defect, relay-13
  RELAY-13 (97f3f1b4-8575-4f63-9196-96bfbc049510) now registers the messaging public key at enrolment (server half committed at 61a59eb; client half staged, pending integrator commit). Four sites still assert or omit the OLD (pre-RELAY-13) behaviour, all confirmed by direct read at HEAD/working-tree 2026-08-08, all outside the implementing agents five-file boundary per both gates verdicts:
  
  1. AGENT_PROTOCOL.md:549 -- "Nobody can fetch your messaging public key from the bus. It is not registered at enrolment..." FALSE since 61a59eb. This is the AGENT-FACING contract, so it misleads the audience most likely to act on it (an agent deciding whether it can rely on out-of-band key exchange).
  2. CONTRACTS-CLI.md:1070 -- the MESSAGING key row says Minted "on first use, lazily, under the store lock (Store.EnsureMessagingKey)". FALSE for every new enrolment (the key is now minted and sent at enrol time); still true only for legacy/pre-RELAY-13 credentials resuming EnsureMessagingKey. Needs both cases stated.
  3. client/client.go:434 -- MessagingPublicKey doc comment: "no messaging key is registered at enrolment and CRYPTO-4 ... does not exist". FALSE (first clause); CRYPTO-4 not existing is still true.
  4. CONTRACTS-CLI.md -- the identities.json field table (~912/922) documents messaging_key_seed for the ON-DISK record, but the table does not document the WIRE field the client now sends (messaging_public_key) or the pending records new messaging_key_seed bookkeeping field distinctly from the promoted credential field. Cross-check against internal/httpapi/auth.go and client/enrol.go (EnrolRequest.MessagingPublicKey, pendingEnrolment.MessagingKeySeed) and add/confirm coverage.
  
  A task is not complete until its documentation is true (CLAUDE.md). Per the orchestrators explicit instruction, this BLOCKS marking RELAY-13 done -- do not flip RELAY-13 to done until all four are fixed and re-verified.
  
  Related: RELAY-13 (97f3f1b4-8575-4f63-9196-96bfbc049510).
  _Proof: bash -c 'set -e; ! grep -n "not registered at enrolment" AGENT_PROTOCOL.md; ! grep -n "on first use, lazily" CONTRACTS-CLI.md | grep -qi messaging; ! grep -n "no messaging key is registered at enrolment" client/client.go; grep -q messaging_public_key CONTRACTS-CLI.md' # each false/missing claim must be gone; RED today (all four match/are missing)_
- [ ] None · RELAY-16-FU-RETRY404: retry of an already-committed send can 404 if the recipient stopped being addressable — hub, P2, idempotency, vacuous-today
  Pre-existing (reproducible before RELAY-16 for a departed LOCAL recipient at 518e71b), WIDENED by RELAY-16 because peer advertisement can churn faster than local enrolment. The admissibility loop (roster check, then routeRemote) runs BEFORE idem.Lookup (internal/hub/hub.go: admissibility loop ~1274-1308, idem.Lookup at 1335). So a legitimate retry of an already-applied send -- same idempotency key, same payload, ack probably lost in flight -- is refused with ErrUnknownRecipient instead of replaying the original Result, if the recipient (local departure, or a remote id whose peer stopped advertising it) is no longer addressable at retry time. That breaks invariant 10s core promise: same key + same payload must return the original result, not a fresh error.
  
  THE FIX DIRECTION MUST BE RECORDED, because the obvious one is wrong: do NOT move the roster check below idem.Lookup, and do NOT move it after the durable write. cca64afd fences the roster check FIRST and BEFORE the write deliberately -- a local id admitted by anything other than the roster is a permanent id-space injury (relay ingest naming `<local-bus>.alpha-18446744073709551615` would push that names suffix floor to the top and exhaust "alpha" across every future restart; see cmd/agent-bus/suffixfloors.go). The correct shape is: answer a KNOWN idempotency key (retry or violation) BEFORE consulting admissibility at all, so a retry short-circuits ahead of the roster/router check rather than requiring the check to pass again. Relates to IDEM-12 (idempotent send/broadcast, general ordering: lookup BEFORE normal send work) -- this task is the concrete manifestation surfaced by RELAY-16s admission seam, not a duplicate of IDEM-12s general implementation task.
  
  State verified before filing: no test exists for this behavior in internal/hub today (grep for Retry+Departed/Recipient in *_test.go: no match) -- VACUOUS (test absent), not RED.
  _Proof: go test -race -run TestRetryOfCommittedSendSucceedsEvenIfRecipientNoLongerAddressable ./internal/hub_
- [ ] RELAY-17 · RELAY-17: CrossBusTrust implementation + attestation travels in the relay envelope — relay, P0, critical-path, vacuous-today
  FEDERATION phase, wave 2. THE EPIC'S KEYSTONE.
  
  Deps: RELAY-7 (trust deep-dive), RELAY-10 (durable peer records), RELAY-14 (attest package), AND
  SIGN-7 (aeb90793) must RELEASE internal/relay/message.go and signed.go first -- filed as a real
  blocking relation (SIGN-7 blocks RELAY-17), not just text, since SIGN-7 is in_progress and owns
  those files right now.
  
  Signature verification is a HARD UNAVOIDABLE DEPENDENCY, not optional hardening:
  relay.ValidateRelayRequest takes CrossBusTrust as a REQUIRED parameter and nil is a refusal, so
  every relayed message is ErrUnpeeredBus/403 by construction until a trust chain exists. RELAY-7/
  13/14/17 are therefore ~40% of the epic and on the critical path.
  
  ~1 day of work; natural split point: (a) interface + envelope field + relay-side verification,
  (b) peer-store-backed implementation.
  _Proof: go test -race -run TestCrossBusTrustVerifiesAttestedEnvelope ./internal/relay_
- [ ] RELAY-30 · RELAY-30: pin the attest.Canonicalize / internal/signing encoder-deviation condition with an owner — relay, P2
  attest.Canonicalize currently implements its own length-prefixed encoder rather than delegating to internal/signing. Both the reviewer and security gates on RELAY-14 called that acceptable ONLY on a condition: doc.go states the deviation MUST become a delegation if signing.CanonicalizeAttestation ever lands. That condition currently has no owner/task, so it is prose with no enforcement. This task: if/when someone adds a general canonicalizer to internal/signing, it MUST either (a) reuse katCanonicalHex (the byte string transcribed from FEDERATION_TRUST_DEEPDIVE.md 4.3, already pinned in internal/attest/canonical_test.go) as its own known-answer test, or (b) attest.Canonicalize must be changed to delegate to it -- either way pinned by a BYTE-EQUALITY test between the two encoders (or between the new encoder and the existing KAT) so the two paths cannot silently drift apart. No code change needed until internal/signing grows that generalized encoder; this task exists so the obligation has a tracked owner.
  _Proof: go test -race -run TestSigningCanonicalizeAttestationMatchesAttestKAT ./internal/signing_
- [ ] None · RELAY-16-FU-SEQUENCING: RemoteRouter must not be wired before the durable outbox exists — relay, P1, sequencing, vacuous-today
  RELAY-16 added hub.Options.RemoteRouter, an admission-time seam (internal/hub/roster.go, RemoteRouter interface, doc comment "# THE SEQUENCING CONSTRAINT — DO NOT INJECT A ROUTER EARLY", roster.go:97-104). Injecting a live RemoteRouter into a running hub BEFORE the durable outbox exists converts an honest 404 into accepted-and-never-delivered: the message is admitted, durably written, and then has nowhere to go. This is a real risk, not a theoretical one -- the seam deliberately DISCARDS the peer id the router returns (internal/hub/hub.go:1295, `if _, ok := h.routeRemote(r); ok { continue }`), so admission ("can I route this?") and egress ("can I deliver this?") consult router/outbox state INDEPENDENTLY. There is no single place today where both questions are answered together.
  
  Recorded so review, not just prose, catches a violation. Constraint: no code path may construct a non-nil RemoteRouter and pass it into hub.Options until RELAY-19 (Forwarder writes and settles durable outbox records, part 2 of the durable-outbox pair started by RELAY-15) is done and the forwarder is wired to actually carry an admitted message onward. The composition root (RELAY-24, cmd/agent-bus/main.go + relaywiring.go) is the most likely place a RemoteRouter gets constructed and injected -- a direct blocking relation RELAY-19 -> RELAY-24 has been added for that reason -- but this task is the review-visible checklist item for WHICHEVER task ends up doing the wiring, in case it is not RELAY-24.
  
  Acceptance: whoever wires a RemoteRouter must, in the same task, either (a) demonstrate the forwarder/outbox path a routed-true answer can reach is durable end-to-end (RELAY-19 done, wired, and tested), or (b) not wire the router yet and instead file/keep this constraint open. A reviewer checking this task off is confirming (a).
  _Proof: go test -race -run TestRelayWiringNeverConstructsRemoteRouterBeforeDurableOutbox ./cmd/agent-bus_
- [ ] None · RELAY-25-FU-REALHOST: Real three-host SSH-tunnel federation run -- loopback smoke does not prove it — relay, P2
  RELAY-25s scripts/fed-smoke.sh runs three buses on 127.0.0.1 and proves the protocol/wiring is correct, but its own header must say what it does NOT prove: SSH tunnel bring-up/flap, NAT/keepalive behaviour, latency versus RetryHorizonCeiling, and certificate pinning actually traversing a tunnel rather than localhost. This follow-up is the real run: three actual hosts (or three VMs/containers standing in for hosts), real SSH tunnels per the FEDERATION deployment assumptions (RELAY-6, DECISIONS.md), A -> B -> C with B as a relay hop, verifying delivery exactly once and the full bus path in the audit log end to end over the tunnels rather than loopback. NOT on the epics critical path -- the loopback smoke (RELAY-25) is the epic deliverable; this is the harder proof that follows it.
  _Proof: bash scripts/fed-smoke-realhost.sh_
- [ ] None · RELAY-2-FU-BROADCAST-FANOUT: Forwarder.targets fans broadcasts out to peers that always 400 them — relay, P3
  Forwarder.targets (internal/relay/forward.go:641) fans every LOCAL broadcast out to every peer, but a correct peer implementation always answers 400 to a relayed broadcast it cannot legally accept today (no CrossBusTrust chain, no RemoteRouter seam). Not urgent -- wasted round-trips against a 400, no data-loss or correctness risk -- but worth trimming once RELAY-16/17/20/21 land, since a peer bus will then be a real destination and this fan-out logic needs re-examining anyway. Discovered during the FEDERATION phase filing pass 2026-08-08, filed rather than left as a comment.
  _Proof: go test -race -run TestForwarderDoesNotFanBroadcastsToPeersThatWillReject ./internal/relay_
- [ ] None · RELAY-15-FU-JOBID-CASE: Normalise bus-id case at the outbox enqueue boundary — relay, P2
  DeriveJobID (internal/relay/outbox.go) is CASE-SENSITIVE while the rest of the system treats bus ids case-insensitively -- registry.go keys peers on strings.ToLower, path.go folds every hop for loop prevention, ValidatePeerBusID compares with EqualFold, and minted bus ids are lowercase by construction. Two case-variant spellings are ONE bus everywhere else but would mint TWO job ids here and deliver the message twice.
  
  The fix is NOT to fold inside DeriveJobID: validate re-derives the job id from the record's STORED PeerBusID, so folding there makes every mixed-case record fail its own integrity check and be discarded as "names one job and describes another" -- a self-inflicted relay-hop loss. Normalise at the ENQUEUE BOUNDARY, before the record is built. That is a decision about the canonical spelling of a durable field, which is why RELAY-15 did not make it unilaterally. Cheapest to do BEFORE any outbox record exists on disk.
- [ ] RELAY-13 · RELAY-13: Enrolment registers the agent's messaging public key — auth, P0, vacuous-today
  FEDERATION phase, wave 2. Consumes existing CRYPTO-3 (dd1066af) -- do not duplicate, relate.
  
  auth.RosterEntry.MessagingPublicKey is defined and validated but never populated
  (auth/service.go:360).
  
  Owns internal/httpapi/auth.go, internal/auth/service.go, client/enrol.go,
  cmd/agent-busctl/enrol.go, CONTRACTS-HTTP.md (wave-2 exclusive), new msgkey_test.go.
  _Proof: go test -race -run TestEnrolRegistersMessagingPublicKey ./internal/httpapi_
- [ ] RELAY-11-FU-INGEST-LOOPGUARD · Relay ingest MUST route through relay.CheckIncomingPath before hub.publish, or a 64-hop loop becomes a permanent audit record — relay, P2
  Raised by the security gate on RELAY-11 (2026-08-08). Duplicate-hop / loop detection today lives ONLY in relay.ValidateBusPath (case-insensitive); neither store.NewMessageWithBusPath nor store.Decode rejects a repeated hop -- this is a documented, deliberate deferral in store/message.go (see its "What is deliberately NOT checked here" comment). The requirement this creates lands on the relay INGEST task: ingest must route the caller-supplied bus path through relay.CheckIncomingPath and must not call hub.publish with a raw peer path, or a malicious/looping peer can write a 64-hop cycle into the append-only audit trail permanently. Worth pinning with a test on RELAY-21 (AcceptRelay callback: roster-check before durable write, re-forward on OutcomeNew), which is the task that wires ingest to hub.publish.
  _Proof: go test -race -run TestAcceptRelayRefusesPathNotCheckedForLoop ./internal/relay_
- [ ] RELAY-31 · RELAY-31: CONTRACTS-ONDISK.md / DECISIONS.md / AGENT_LOG.md entries for internal/attest — docs, P2
  Documentation for RELAY-14 (internal/attest: bus-signed agent-key attestations) was never written -- outside RELAY-14s own file boundary. Needed: CONTRACTS-ONDISK.md record of the reserved signing-format-version = 2 value and the agent-bus/bus-attest/2 context string (reserved_by feature-runner-RELAY-14, 2026-08-08, from the signing-format-version reservation namespace, NOT chosen); DECISIONS.md dated entry recording the package, the encoder-deviation-with-owner decision (see RELAY-30), and the four binding checks from FEDERATION_TRUST_DEEPDIVE.md 4.2; AGENT_LOG.md entry for the work. Note explicitly: invariant 7s CLI-subcommand-in-the-same-task obligation does NOT bite yet for this task -- no agent-facing surface moved, nothing imports internal/attest yet (code-only, per RELAY-14s own report).
  _Proof: grep -n "signing-format-version = 2" CONTRACTS-ONDISK.md_
- [ ] RELAY-22 · RELAY-22: Choose and wire the multi-principal relay abuse-control primitive — relay, P1, vacuous-today
  FEDERATION phase, wave 3. Deps: RELAY-17 (CrossBusTrust).
  
  Owns DECISIONS.md in its wave (the only other task besides RELAY-6 permitted to touch it this
  epic). Consumes existing task 48223968 ("Choose the abuse-control primitive for a MULTI-PRINCIPAL
  relay link", raised P2->P1 2026-08-08 as part of this filing pass) -- do not duplicate, relate.
  The ingest handler (RELAY-20/21) cannot be written correctly without this answer: the default it
  would otherwise silently inherit (messages.go:656 disconnect) drops every agent behind a peer bus
  over one agent's traffic. Deliver the actual mechanism (per-origin-agent rejection without
  dropping the transport / per-peer rate limiting / peer-level de-peering) and wire it, in addition
  to 48223968's DECISIONS.md entry.
  _Proof: go test -race -run TestMultiPrincipalAbusePrimitiveEnforced ./internal/relay_
- [ ] RELAY-29 · RELAY-29: revocation across a non-adjacent link is unsolved — relay, P2
  From FEDERATION_TRUST_DEEPDIVE.md 4.4: bus C has no channel to bus A, so a compromised A-agent key stays live alongside its replacement for the whole NotAfter window -- there is no way for C to learn of an early revocation short of the attestation expiring naturally. Related to RELAY-28 (MaxAttestationLifetime) -- they are the same exposure viewed from two directions: RELAY-28 bounds how long the window can be, this task is about whether the window can be closed early at all. Needs design work (a revocation list distributed hop-by-hop? shorter-lived attestations with mandatory re-attestation? out-of-band peer notification?) before implementation -- do not start coding without a design decision recorded in DECISIONS.md.
  _Proof: none -- design task, blocked on a DECISIONS.md entry before any code proof applies_
- [ ] RELAY-32 · RELAY-32: add json: tags to internal/attest.Attestation before it goes on the wire — relay, P3
  internal/attest.Attestation currently has no json: tags. FEDERATION_TRUST_DEEPDIVE.md 4.2 specifies wire field names for the attestation. Whichever task first puts an Attestation on the wire (most likely RELAY-17, which threads the attestation through the relay envelope) should add json: tags matching the documented field names and reuse this struct directly rather than forking a second wire-shaped struct that can drift from it. Flagged by the RELAY-14 reviewer gate (F5, P3).
  _Proof: grep -n "json:" internal/attest/attest.go_
- [ ] None · RELAY-16-FU-RECOVEREDPRUNE: Hub.recovered is never pruned of foreign ids its only consumer discards — hub, P3, informational, vacuous-today
  Informational / low priority, per securitys note on RELAY-16s re-audit. h.recovered is harvested unfiltered from every sender and recipient in the replayed log (internal/hub/hub.go:993-995), including foreign (remote-bus) ids now made durable by RELAY-16s admission seam. Its ONLY consumer, noteRecoveredIdentities (internal/hub/roster.go:304-361), already filters foreign ids out at read time (added alongside RELAY-16 to stop the invariant-1 id-reuse detector false-firing on remote recipients). So the retention in h.recovered itself is pure waste once a router is ever wired and used: one map entry (~150B) per distinct foreign id ever addressed, forever, for a value nothing reads.
  
  Harvest-side filtering (skip non-local ids at hub.go:993-995 the same way noteRecoveredIdentities filters them at roster.go) would avoid the retention entirely. NOT urgent: WAL bytes dominate memory/disk cost by a wide margin, so this only matters if a WAL-growth budget is ever set for h.recovered specifically, which nothing does today. File low and revisit then.
  _Proof: go test -race -run TestHubRecoveredIsPrunedOfForeignIdsOnHarvest ./internal/hub_
- [ ] None · RELAY precondition: roster-check LOCAL recipients before the durable write, or a peer can permanently exhaust an agent name — relay, P1
  Found by the security gate on MSG-FU-SUFFIXFLOOR (94159d93-fe87-4c3e-b938-86fe7068c787). LATENT ONLY BECAUSE RELAY IS UNWIRED -- nothing outside internal/relay imports it today.
  
  CHAIN. cmd/agent-bus/suffixfloors.go derives per-name agent-id suffix floors from the SENDER and RECIPIENTS of durable store message records, and it is safe to do so because those fields are SERVER-DERIVED: internal/hub/hub.go:678 requires every recipient to be Enrolled on this bus before anything is written. internal/relay/message.go:519-530 validates recipient SHAPE ONLY.
  
  EXPLOIT once relay is served. A hostile (or merely buggy) peer relays a message naming the local recipient '<local-bus>.alpha-18446744073709551615'. It reaches the durable log. On the next start that dir's backfill folds it into alpha's floor, ids.RaiseFloor applies NO upper bound, and the name 'alpha' is EXHAUSTED (ids.ErrSuffixExhausted) for that bus PERMANENTLY, across every future restart. That is denial of one agent NAME, forever, from a remote party. ids.NameSuffixes.RaiseFloor's own doc names this shape in as many words: 'Validate and BOUND a peer's claim BEFORE it reaches RaiseFloor.'
  
  DO. Roster-check local-bus recipients in the relay ingress path before the durable write, exactly as hub.publish does. Note the sender vector is already closed (ValidatePeerBusID plus the sender-bus check), so this is specifically about RECIPIENTS.
  
  PROOF. A test that a relayed message naming an unenrolled local recipient is REFUSED before anything is written, and that the suffix floor for that name is unchanged after a restart.
- [ ] RELAY-26 · RELAY-26: Startup refusal: non-loopback -listen with peer records and invite-gating off — core, P1, vacuous-today
  FEDERATION phase, wave 5. NOT on the critical path (does not block RELAY-25).
  
  Refuse to start if -listen is bound to a non-loopback address while peer records exist and
  invite-gating is off -- turns the SSH-tunnel assumption (RELAY-6) into an ENFORCED precondition
  rather than a comment. Proposed by the planner and accepted by the orchestrator 2026-08-08.
  _Proof: go test -race -run TestStartupRefusesNonLoopbackListenWithPeersAndInviteGateOff ./cmd/agent-bus_
- [ ] None · RELAY-15-FU-SWEEP-TOMBSTONE: Horizon-swept outbox jobs leave no durable abandonment record — relay, P2
  Outbox.sweepLocked drops a pending job past OutboxRetryHorizon from memory and logs it at WARN, but writes NO durable 'abandoned' record -- it cannot, because it runs with mu held and this package never writes durably under the lock. Consequences: (a) the WAL shows an enqueue with no settlement beside it, so the durable trail carries an unresolved job; (b) after the drop the same job id is Enqueue-able again with a FRESH horizon anchor, which is the horizon extension the pending-vs-pending rule refuses elsewhere. The same applies to a job dropped by the FutureDated guard.
  
  Fix: write the abandonment from a caller OUTSIDE the lock (Settle already does exactly that). RELAY-19 must additionally not re-enqueue a job it has seen dropped past the horizon.
  _Proof: go test -race -run TestOutboxAgeHorizonIsEnforcedOnTheReplayPath ./internal/relay_
- [ ] RELAY-28 · RELAY-28: derive a verifier-side MaxAttestationLifetime ceiling for attest.Verify — relay, P1
  internal/attest.Verify currently trusts NotAfter entirely at the minters discretion -- an attestation minted valid until year 292278994 is accepted (demonstrated by the RELAY-14 security gate, finding P2-3). With revocation across a non-adjacent link unsolved (see RELAY-29), expiry is currently the ONLY bound on a compromised agent key. DoD explicitly requires DERIVING the ceiling, not choosing an arbitrary number -- FEDERATION_TRUST_DEEPDIVE.md treats an unjustified constant as its own defect class (same reasoning as reserved-not-chosen resource numbers). State in the task/commit what the derived value is grounded in (e.g. the clock-skew allowance already in internal/buscert, an existing session/cert rotation cadence, or another already-decided lifetime elsewhere in the system -- do not invent a fresh one). Implement as an exported MaxAttestationLifetime enforced inside Verify (NotAfter - IssuedAt > MaxAttestationLifetime is refused), with a test pinning both the derived value and the boundary.
  _Proof: go test -race -run TestVerifyRejectsAttestationExceedingMaxLifetime ./internal/attest_
- [ ] None · RELAY-2-FU-LOOPTEST-FLAKE: Unreproduced single failure of TestMessageRelay's loop subtest — relay, P2
  During RELAY-2/3 the test-engineer observed ONE failure of TestMessageRelay/a_loop_is_200_with_a_dropped_reason,_never_an_error_status on the tree BEFORE any of its edits, and could not capture the failing assertion. Not reproduced in ~3,500 subsequent executions (~2,900 by the test-engineer including 8-way parallel load and cold-testcache runs, ~600 by feature-runner at -count=200). The only non-deterministic path in that subtest is doRelay's t.Fatalf("request: %v", err) on a transient httptest connection error, which would be HARNESS FRAGILITY rather than a product defect -- but that is a hypothesis, not a diagnosis. Task: either reproduce it, or make doRelay distinguish a transport error from an assertion failure so the next occurrence is self-diagnosing.
- [ ] RELAY-24 · RELAY-24: Composition root: wire federation into cmd/agent-bus/main.go — cli, P0, critical-path, vacuous-today
  FEDERATION phase, wave 4. Deps: RELAY-12 (peer subcommand), RELAY-20 (peer routes mounted),
  RELAY-21 (AcceptRelay callback).
  
  The composition root in cmd/agent-bus/main.go + new relaywiring.go: loads peer records, builds
  CrossBusTrust, constructs the Forwarder/Registry, and registers the /v1/peer/* routes only when
  both are non-nil, per RELAY-20's contract.
  _Proof: go build ./... && go test -race -run TestRelayWiringComposesRoutesWhenPeersConfigured ./cmd/agent-bus_
- [ ] RELAY-27-FU-EXPIRED · RELAY-27-FU-EXPIRED: attest.ErrExpired and attest.ErrNoClock still answer a peer bad_signature — relay, P2
  Known gap left explicitly by RELAY-27 (commit 06e3cc5): attributionError.relaySentinelForTrustError maps attest.ErrUnpinned->ErrUnpeeredBus(403) and attest.ErrInvalid->ErrInvalidRelay(400), but attest.ErrExpired and attest.ErrNoClock fall through the default arm to ErrNoSignerKey/bad_signature(403), same as genuine forgery. Both are wrong in the SAME direction: expiry is usually an HONEST queued message that got stale in flight, not an attack, so answering bad_signature tells the peer to stop retrying when the right answer is retriable; and ErrNoClock is OUR OWN wiring fault (no clock configured for verification) reported to the peer as if it were their non-retriable problem.
  
  Fix requires: (1) a NEW wire code, RESERVED via POST .../reservations (a namespace for relay wire codes -- do not choose a string by eyeballing existing Code* constants in internal/relay/handshake.go), distinguishing at minimum expiry (retriable) from a local clock-wiring fault (5xx-shaped, not a peer fault at all); (2) a handler arm in internal/relay/handshake.go declaring the new Code* constant(s) with the existing block's documentation style (HTTP status + retriability); (3) an arm in internal/relay/signed.go relaySentinelForTrustError mapping attest.ErrExpired and attest.ErrNoClock to the new sentinel(s) (NOT both to the same one -- ErrNoClock is local wiring, ErrExpired is peer-observable staleness); (4) an allow-list arm in internal/relay/client.go peerErrorCode (RELAY-9's pattern) so the new code is recognised rather than falling through as unrecognised; (5) regression tests asserting errors.Is for both sentinels through the real VerifyRelayed path, the new ErrorCode()/HTTP status, and that the response is classified RETRIABLE for expiry (PeerRefusedError.Retriable per client.go:78) and appropriately for the clock-fault case.
  
  Outside RELAY-27's boundary, which is why it was not reached there -- filed as its own atomic follow-up per the RELAY-27 implementer's own note (see RELAY-27 commit 06e3cc5 message).
  _Proof: go test -race -run TestExpiredAndNoClockAreDistinctFromForgery ./internal/relay_
- [ ] None · RELAY-2-FU-FORWARDER-REAP: Forwarder never reclaims a departed peer's queue or goroutine — relay, P2
  internal/relay/forward.go creates one bounded channel plus one goroutine per peer on first enqueue and never removes either; there is no counterpart to Registry.RemovePeer. Peer churn or a flapping topology leaks a DefaultQueueDepth-slot channel and a goroutine per bus id ever routed to. Bounded in practice by the peer set, unbounded in principle.
- [ ] RELAY-21 · RELAY-21: AcceptRelay callback: roster-check before durable write, re-forward on OutcomeNew — relay, P0, critical-path, vacuous-today
  FEDERATION phase, wave 3. Deps: RELAY-20 (peer routes mounted).
  
  The AcceptRelay callback: roster-check local recipients BEFORE the durable write, then re-forward
  ONLY on OutcomeNew. Consumes cca64afd (do not duplicate, relate): "RELAY precondition: roster-check
  LOCAL recipients before the durable write, or a peer can permanently exhaust an agent name."​
  _Proof: go test -race -run TestAcceptRelayChecksRosterBeforeDurableWrite ./internal/relay_
- [ ] None · RELAY-33: attest.go:371 quotes want.OriginBus unbounded (%q, 64 KiB -> 262,329-byte refusal string) -- and the hand-copied-slice snapshot pattern has no owner-doc guard — relay, P2
  internal/attest/attest.go:371 quotes want.OriginBus with %q WITHOUT the length bound that F2 (RELAY-14 security gate) added to want.FQAgentID at the sibling comparison. Measured: a 64 KiB want.OriginBus produces a 262,329-byte refusal string (NUL expands roughly four-fold under %q). Not live today -- nothing calls attest.Verify yet (RELAY-17 is the wiring task) and relay.ValidateRelayRequest check 3 already bounds m.OriginBus before it would reach here -- but it contradicts the packages own stated rationale for bounding the OTHER two-sided comparison (want.FQAgentID vs a.AgentID at attest.go:310, fixed under F2), since the OriginBus compare at attest.go:371 is the packages other two-sided comparison and was left unbounded.
  
  Minimal fix: apply ids.ValidateBusID(want.OriginBus) beside the existing bound near attest.go:323 (mirroring the F2 fix pattern), or stop echoing want.OriginBus verbatim in the refusal message.
  
  LATENT HAZARD to close in the same task: Verify snapshot is `checked := a` plus two HAND-WRITTEN slice copies (for the two slice-typed fields today). A third slice field added to Attestation later would silently alias the live backing array again -- the exact TOCTOU class already found and fixed once (P2-1 in the RELAY-14 gate). Add a one-line comment on the Attestation struct (or beside the snapshot code) stating that every slice-typed field must be explicitly copied here, so the next author who adds one is warned rather than relying on rediscovery.
  
  Filed from the RELAY-14 security RE-VERIFICATION (2026-08-08), a new finding not in the original gate report.
  _Proof: go test -race -run TestVerifyBoundsOriginBusBeforeQuote ./internal/attest_
- [ ] RELAY-18 · RELAY-18: Retire the relay import guard deliberately, replaced by a narrower one — relay, P0, vacuous-today
  FEDERATION phase, wave 2. Deps: RELAY-6 (FEDERATION deployment assumptions in DECISIONS.md).
  
  TestHandshakeHandlerIsNotWiredIntoAnyMux (guards_test.go:44) fails if any file outside
  internal/relay imports it. The blanket ban is REPLACED, not deleted, by a narrower guard:
  importable only by internal/httpapi and cmd/agent-bus, and the ingress handler constructible only
  with a non-nil CrossBusTrust. TestPackageDocDoesNotReviveTheWithdrawnDisconnect stays UNTOUCHED --
  this task does not touch invariant-10 disconnect semantics.
  _Proof: go test -race -run TestRelayImportGuardAllowsHttpapiAndMain ./internal/relay_
### EPIC SIGN — SIGN: message authenticity & integrity (Ed25519 sign/verify, no encryption yet)

- [ ] SIGN-2 · SIGN-2: Sign on the send path (Ed25519 detached signature travels with the message) — crypto, P1
  GATED on SIGN-1 (canonical format) and CRYPTO-3 (messaging keypair minted at enrolment). Supersedes the encryption-specific CRYPTO-6 (Double Ratchet encrypt on the DM path), which is superseded outright -- there is no ratchet and no ciphertext. What this task implements instead: the sender signs SIGN-1's canonical serialisation of the outgoing message with its messaging Ed25519 PRIVATE key (crypto/ed25519.Sign -- stdlib, audited, high-level API; invariant 9 -- no custom signing code) and the resulting detached signature travels alongside the plaintext body in the envelope. The private key never leaves the agent's machine and is never sent to the bus. Because SIGN-1 may require the signature to cover the server-minted message id/sequence (see SIGN-1's open question), specify and implement the exact ordering this requires: either (a) the client obtains the id/sequence first (e.g. a reserve-then-send two-step) and then signs, or (b) the client signs everything it controls and the durable record binds the server-minted id/sequence to that signature non-repudiably without them being literally inside the signed bytes -- pick the option SIGN-1 settled on. Wire this into the SAME Go binary used by scripts/bus-send.sh (invariant 7 -- shell cannot do Ed25519, so add a subcommand, e.g. `agent-bus sign`, that the wrapper shells out to) -- ship the wrapper change and any AGENT_PROTOCOL.md update IN THIS SAME TASK. The bus stores and forwards the signature as opaque bytes; it MAY optionally check the signature is well-formed (right length) but verification is the RECIPIENT's job (SIGN-1's epic note on why -- a malicious bus must not be able to forge on behalf of a sender it does not control, and equally must not be trusted to police messages against senders it does not control either). No new key material beyond CRYPTO-3's existing messaging keypair. Test via scripts/bus-send.sh against a running throwaway bus, not hand-written curl.
  
  ACCEPTANCE CRITERION ADDED 2026-08-02 (RATCHET-7 fallout, verified first-hand by backlog-triage by reading this box's own stdlib source at crypto/ed25519/ed25519.go under GOROOT): ed25519.Verify PANICS (does not return false) when len(publicKey) != ed25519.PublicKeySize -- a remote DoS trap, asymmetric with malformed-signature handling (a bad signature safely returns false, a malformed key does not). Any verification this task's send path performs or triggers downstream (including recipient-side verification against a sender's messaging public key, and any self-check before accepting a signature as well-formed) must length-check the public key against ed25519.PublicKeySize BEFORE calling ed25519.Verify, and must fail closed on mismatch rather than panic. REQUIRED TEST: a negative test feeding a wrong-size and a nil/empty public key through this path, proving no panic. See also the standalone cross-cutting task filed to track this trap across all Verify call sites (AUTH-1, CRYPTO-10, SIGN-2).
  _Proof: go test -race -run TestSendSigns ./internal/... ; scripts/bus-send.sh against a running throwaway bus produces a message whose signature verifies against the sender's registered messaging pubkey_
- [ ] SIGN-4 · SIGN-4: Replay/freshness -- server-minted monotonic sequence + recipient-side cursor — crypto, P1
  GATED on SIGN-1. A signature alone does NOT provide a freshness/replay defence: a validly-signed message can be replayed VERBATIM by anyone who saw it once (including a malicious bus), and Ed25519 verification of a replayed message succeeds every time because nothing about the signature changes. Do not let an implementer assume signing solves this -- it does not, and the SIGN epic description says so explicitly. This task specifies and implements the defence: the bus mints a monotonic sequence number per recipient (or per conversation -- decide and document which, consistent with invariant 1: ids/sequences are server-minted, never client-supplied) INSIDE SIGN-1's signed bytes, and the recipient maintains a durable delivery cursor (highest sequence accepted so far, per sender or per conversation) that MUST only advance, never rewind (same shape as the durable-store invariants 4/5: the cursor is part of the recipient's serving state, rebuilt by replay on restart). A message whose sequence is <= the cursor is rejected as a replay BEFORE the body is handed to the calling agent, even if its signature verifies. State plainly what this does and does not cover: it defeats verbatim replay of a message already delivered; it does NOT provide encryption or hide metadata (accepted per RATCHET-2's rescope). Tests: replaying the exact same signed envelope after successful delivery is rejected; out-of-order delivery within a reasonable window is handled sanely (define the policy -- reject strictly increasing-only, or allow a bounded reorder window, and say why); a cursor is durable across a recipient-side restart (crash-injection style test per CLAUDE.md's durability discipline, since this is exactly invariant-4/5 territory even though it lives on the recipient side, not the bus's WAL).
  _Proof: go test -race -run TestReplayRejectedByCursor ./internal/..._
- [ ] None · SEC: ed25519.Verify panics on wrong-size public key -- remote DoS across AUTH-1/CRYPTO-10/SIGN-2 call sites — security, P1
  Cross-cutting security gap surfaced by RATCHET-7 and VERIFIED FIRST-HAND by backlog-triage by reading this box's own stdlib source at crypto/ed25519/ed25519.go (Go 1.19 GOROOT): ed25519.Verify PANICS -- it does not return false -- when len(publicKey) != ed25519.PublicKeySize. This is a remote DoS trap because it is ASYMMETRIC with malformed-signature handling: a bad/tampered signature safely returns false, but a wrong-size (or nil) public key crashes the process. A call site that validates the signature but not the key length therefore looks correct in review and is remotely crashable in production.
  
  This matters immediately because at least three call sites accept or load attacker-influenceable public keys and will call ed25519.Verify on them:
  - AUTH-1 (POST /v1/enroll): the public key is client-supplied at enrolment -- untrusted input by definition (invariant 1: a client-supplied value is input to be validated, never an identity to be trusted).
  - CRYPTO-10 (`agent-bus verify` + wrapper validate-before-accept): verifies contact-list/sender public keys, including keys reloaded from the on-disk roster after a restart.
  - SIGN-2 (sign on the send path) and any downstream recipient-side verification against a sender's messaging public key.
  
  SCOPE OF THIS TASK: own the fix and its verification ACROSS all of the above call sites (do not let each task independently reinvent the guard -- provide or point to one shared, tested helper, e.g. a `safeVerify(pub, msg, sig []byte) bool` that length-checks before delegating to ed25519.Verify) plus any other Verify call site discovered during implementation, including ed25519.PublicKey values loaded from the roster on disk after a restart (DUR/recovery path).
  
  ACCEPTANCE CRITERIA:
  1. Every ed25519.Verify call site in the codebase length-checks the public key against ed25519.PublicKeySize before calling Verify, and returns/propagates a normal validation error on mismatch -- never a panic.
  2. A shared helper exists (not copy-pasted per-call-site logic) so future call sites get the guard by construction.
  3. Each affected call site (AUTH-1's enrolment path, CRYPTO-10's verify subcommand, SIGN-2's send/verify path, and the roster-reload-from-disk path) carries a negative test that feeds a wrong-size public key AND a nil/empty public key, asserting a clean rejection with no panic/crash (run with -race per project convention for anything touching concurrent paths).
  4. Documented in CONTRACTS.md or DECISIONS.md as a standing invariant so it is not silently reintroduced by a later call site.
  
  This task should land alongside (or ahead of) AUTH-1/CRYPTO-10/SIGN-2's implementation since it is a prerequisite acceptance criterion on each of them, but is filed separately because it is a security trap spanning multiple call sites, not a single-task scope.
  _Proof: go test -race -run TestSafeVerify ./... ; go test -race -run TestEnroll_MalformedPublicKey ./internal/auth ; go test -race -run TestVerify_MalformedPublicKey ./internal/... -- all pass with no panic on wrong-size/nil public keys_
- [ ] SIGN-5 · SIGN-5: MANDATORY negative-test suite -- prove the verifier rejects everything it must — crypto, P1
  GATED on SIGN-1/SIGN-2 and CRYPTO-10's verify implementation existing (may run in parallel with CRYPTO-10 against a stub). MANDATORY, not nice-to-have, per invariant 9: broken or misused crypto fails SILENTLY -- it still 'verifies', and provides none of the protection it appears to. 'Our tests pass' is never evidence for a crypto change; a verifier that accepts everything passes every positive test ever written. This task exists specifically to make that failure mode impossible to ship undetected. Required cases, EACH proven REJECTED with a distinct assertion (not just 'an error occurred' -- assert the specific failure path fired): (1) TAMPERED BODY -- flip one byte of the signed body, signature must fail; (2) SWAPPED SENDER -- a validly-signed message re-labelled as if from a different sender must fail (proves the sender id is inside the signed bytes per SIGN-1, not just alongside them); (3) REPLAYED MESSAGE -- re-deliver an already-accepted signed envelope verbatim, must be rejected by SIGN-4's cursor even though the signature itself verifies; (4) WRONG KEY -- verify against a public key that is NOT the signer's (e.g. a different enrolled agent's real key), must fail; (5) TRUNCATED SIGNATURE -- a short/malformed signature byte string must be rejected cleanly (no panic, no out-of-bounds read -- crypto/ed25519.Verify is documented to handle this safely, confirm it and pin the confirmation in a test) . Add any other rejection case the implementation surfaces (e.g. corrupted/garbage public key bytes). Every case must have its own named test, not be folded into one assertion, so a future regression names exactly which property broke.
  _Proof: go test -race -run TestVerifyRejects ./internal/... -- one subtest per rejection case, each asserting non-zero exit / verify-failure, none asserting success_
- [ ] SIGN-8 · SIGN-8: Agent-side messaging key material -- `agent-bus keygen`, key file location/permissions, bus-enrol.sh wiring, AGENT_PROTOCOL.md — agentif, P1
  The AGENT-SIDE half of CRYPTO-3, which is server-side only (it registers a public key the agent must already have). Nobody owns generating and protecting the private half, and AGENTIF-2 (scripts/bus-enrol.sh) predates the whole signing decision and knows nothing about keys -- so as it stands an agent has no way to obtain a messaging identity. Invariant 7: agents never hand-write HTTP and never hand-roll key handling either; shell cannot do Ed25519, so add an `agent-bus keygen` subcommand to the same Go binary (crypto/ed25519.GenerateKey with crypto/rand -- invariant 9, no custom key derivation, no hand-rolled entropy) and have the wrapper shell out to it. DELIVER: (1) a documented default key location outside the repo, overridable by one env var, with the private key written 0600 inside a 0700 directory, created atomically; refuse to run -- loudly, non-zero exit -- if an existing key file is group- or world-readable (the same refusal CRYPTO-10 makes on the verify side, so the two agree). (2) The private key is NEVER printed to stdout, NEVER logged, and NEVER sent to the bus -- only the 32-byte public half goes over the wire, at enrolment. Add its path pattern to .gitignore (related: CORE-10, which notes the stop hook stages with `git add -A`; a messaging private key landing in a commit is the worst realistic outcome of this epic). (3) bus-enrol.sh generates the keypair if absent and registers the public half, and is IDEMPOTENT: a second run must NOT silently overwrite an existing private key. Silent re-keying is the dangerous failure -- it orphans the already-registered public key and trips every verifier's TOFU pin (CRYPTO-4) as if the bus were MITM-ing, so an accident becomes indistinguishable from an attack. Re-keying must be explicit and human-driven. (4) State plainly how this file differs from the AUTH credential from AUTH-1 (the bearer token that authenticates TO the bus): two files, two lifetimes, two purposes -- the token proves you to the bus, the messaging key proves you to your PEERS, and only the second one a compromised bus cannot forge. (5) AGENT_PROTOCOL.md entry ships IN THIS TASK (invariant 7); CONTRACTS.md gains the subcommand, the env var and the file path. Rotation and revocation are OUT of scope -- CRYPTO-4's key_epoch and AUTH-4 own them -- but say what re-enrolment does. Verify the way an agent would: through scripts/bus-enrol.sh against a running throwaway bus with its own data dir under /tmp, not hand-written curl.
  _Proof: scripts/bus-enrol.sh against a running throwaway bus creates a 0600 private key, registers the public half, and a SECOND run neither overwrites it nor silently re-keys ; go test -race -run TestKeyfilePerms ./internal/..._
- [~] SIGN-7 · SIGN-7: Cross-bus relay preserves the signed envelope byte-exact -- an intermediate bus can neither forge nor strip a signature — crypto, P1, in progress
  GATED on SIGN-1; implementation lands with RELAY-2/RELAY-3. RAISED TO P1 DESPITE THE RELAY EPIC BEING P2 BECAUSE IT CHANGES A SIGN-1 DECISION: SIGN-1 must not be completed until the question below is answered, or the canonical format will have to be redesigned after code depends on it. THE COLLISION: SIGN-1 wants the server-minted message id and sequence INSIDE the signed bytes (so a malicious bus cannot reorder or misattribute messages undetected). But those are minted by the ORIGIN bus, while the receiving bus needs its own local sequence for its own recipients' cursors (SIGN-4) and, per invariant 1, does not accept ids minted by a client -- and a peer bus IS a client from its perspective. If the far bus re-mints and substitutes, EVERY relayed signature fails at the far end; if it adopts the origin's numbers wholesale, it has ceded id authority to a peer. RESOLVE IT EXPLICITLY. The likely answer -- state it or a better one, and make SIGN-1 match: the signed bytes carry the ORIGIN's fully-qualified sender id and the ORIGIN's message id, which per invariant 2 are already bus-namespaced and therefore globally unambiguous and not the far bus's to mint, while the receiving bus mints its own LOCAL delivery sequence OUTSIDE the signed bytes and binds it in its durable record. (2) NO FORGERY: an intermediate bus cannot forge a message because it does not hold the sender's messaging private key -- but ONLY if the recipient verifies against a key it trusts. CROSS-BUS KEY TRUST IS AN OPEN HOLE: CRYPTO-4's bundle is attested by the LOCAL bus, so bus B attesting a key for bus A's agent means bus B can simply lie and substitute its own key. Decide and document: relay A's attestation intact (bundle signed by A's bus key) and pin A's BUS key at peering time, or TOFU the agent's messaging key at first contact and alarm on change, or both. Without this, cross-bus signatures verify against whatever the nearest bus says, which is worth nothing. (3) NO STRIPPING: SIGN-6's mandatory-signature ingest rule applies to the relay ingest path EXACTLY as it applies to /v1/send. A relayed message arriving with no signature, or with a re-signed one, is rejected -- an unauthenticated downgrade must not be reachable through a peer. (4) NO MUTATION: the relay forwards the signed bytes verbatim. Any normalisation on the path (re-encoding JSON, reordering keys, trimming whitespace, re-framing the body) breaks verification at the far end -- which is a strong argument for SIGN-1 choosing a length-prefixed binary canonical form, or for the relay carrying the exact signed byte string as an opaque blob. Say which. (5) Complements RELAY-3 (traversed-bus-path loop prevention) and IDEM-15 (relay duplicate suppression -- exactly-once APPLICATION on the relay path; this gloss pointed at IDEM-7 until the 2026-08-02 duplicate-epic merge superseded IDEM-1..9 and folded IDEM-7's origin-identity dedupe and non-forgeability content into IDEM-15): the bus path is metadata OUTSIDE the signature and grows on every hop, so it can never be inside the signed bytes -- state that explicitly, since it means the path is unauthenticated and a lying peer can rewrite it (loop prevention is availability, not security). TESTS: signed on A, verifies for a recipient on B; strip the signature in transit -> rejected at B's ingest; mutate one byte of a signed field in transit -> the recipient's verification fails and the body is never delivered; the far bus's local sequence differs from the origin's without breaking verification.
  
  === AUDIT 2026-08-08 (spec-keeper): PARTIALLY SHIPPED -- the split, explicitly ===
  MET (in main): the signed-envelope preservation code and its tests landed at commit 7b383cf
  ("SIGN-7: relay preserves the signed envelope, by RE-DERIVATION not a blob"), verified an ancestor
  of HEAD efde70c. internal/relay/signed_test.go carries nine TestSign7* tests, including
  SignedOnAVerifiesForARecipientOnB, StrippedSignatureIsRejectedAtIngest,
  MutatedFieldNeverReachesDelivery, LocalDeliverySequenceIsOutsideTheSignedBytes and
  ForwardIsVerbatimAcrossTwoHops. The old status_note "CODE-COMPLETE, awaiting the orchestrator's
  commit" is STALE: it IS committed.
  NOT MET: the proof_cmd's second clause -- "a message signed on bus A verifies unmodified for a
  recipient on bus B using the DEPLOY-3 two-bus Compose profile". At HEAD nothing outside
  internal/relay imports internal/relay (the only cross-package mentions are comments at
  cmd/agent-bus/suffixfloors.go:84 and internal/httpapi/messages.go:97), so the surface is registered
  on no mux and no running bus can exhibit this. Gated behind INVITE-PEERGUARD (f5d91dbe, todo) and
  MTLS-RELAYGUARD (8192c3c7, todo).
  PROOF_CMD IS VACUOUS ON ITS FIRST CLAUSE TOO: TestRelayPreservesSignature does not exist anywhere at
  HEAD. Retarget to `go test -race -run TestSign7 ./internal/relay` before anyone attempts to close
  this, and keep the live cross-bus clause as the thing that holds it open.
  _Proof: go test -race -run TestRelayPreservesSignature ./internal/relay ; a message signed on bus A verifies unmodified for a recipient on bus B using the DEPLOY-3 two-bus Compose profile_
- [ ] SIGN-6 · SIGN-6: A signature is MANDATORY on the wire -- ingest policy and fail-closed handling of missing/malformed/unverifiable signatures — crypto, P1
  GATED on SIGN-1 (canonical bytes) and SIGN-2 (signing on send). SIGN-1..5 specify how to sign and how to verify; NOTHING yet specifies what the bus does with a message that is not signed, or what a recipient does with one that fails to verify. That gap is not cosmetic: if either side treats "no signature" as "unsigned but fine", an attacker strips the signature and the entire epic is theatre. THIS TASK CLOSES IT. (1) THE SIGNATURE FIELD IS REQUIRED, NOT OPTIONAL. There is no unsigned message type, no allow_unsigned flag, no --insecure escape hatch, no legacy path; if one is ever argued for it needs its own dated DECISIONS.md entry. (2) INGEST POLICY on POST /v1/send and /v1/broadcast (MSG-2/MSG-3): the bus does NOT verify authenticity -- it must not be trusted to police messages on behalf of senders it does not control (SIGN-2), and the trust decisions live with the recipient (CRYPTO-4 TOFU pins) -- but it DOES enforce, and reject 4xx on failure: signature present; signature exactly 64 bytes (Ed25519); the claimed sender equals the AUTHENTICATED caller (invariants 1 and 2 -- a client-asserted identity is input to validate, never an identity to trust, so no caller may inject a message attributed to another agent no matter how well-formed the signature looks). (3) A REJECTED MESSAGE MUST LEAVE NO TRACE: no WAL record, no audit-log entry beyond a rejection event, no delivery, no ack -- the mirror image of invariant 4. DECIDE AND DOCUMENT whether a rejected send consumes a sequence number: if it does, recipients see gaps and SIGN-4's cursor must tolerate them; if it does not, sequence minting must happen after validation. Pick one, say why, make SIGN-4 consistent. (4) RECEIVE PATH: GET /v1/wait and GET /v1/messages return the signature with every message so the recipient can verify (CRYPTO-10). Verification failure is FAIL-CLOSED -- the body is NEVER handed to the calling agent -- and LOUD: log message id, sender, and which check failed; never swallow it. (5) THE POISON-MESSAGE WEDGE, the subtle one: if a message that fails verification also blocks the recipient's cursor from advancing, one bad message wedges that agent FOREVER and a malicious bus gets a trivial denial of service. Recommended policy to specify and test: the cursor advances past the unverifiable message (it was durably delivered and cannot be un-sent), the body is discarded rather than delivered, and the event is recorded so the failure is visible. Whatever is chosen, prove the poller cannot be wedged. (6) Interacts with invariant 10 (IDEM epic): a rejection must not turn into a client retry loop that produces duplicates -- a rejection is terminal for that idempotency key, not a transient error. TESTS: unsigned send rejected with no durable record; 64-byte-length check (63 and 65 bytes both rejected); sender-mismatch rejected; relay ingest is subject to the SAME check (see SIGN-7 -- a relay path that skips it is the obvious backdoor); a recipient handed one unverifiable message still makes progress on the next good one.
  _Proof: go test -race -run TestUnsignedRejected ./internal/httpapi ./internal/store ; scripts/bus-send.sh with the signature stripped is rejected by a running throwaway bus and leaves NO durable record_
- [ ] SIGN-3 · SIGN-3: Broadcast signature covers the recipient set (prevents split-content broadcasts) — crypto, P2
  GATED on SIGN-1/SIGN-2. Replaces the encryption-specific scope of superseded CRYPTO-8 (broadcast fan-out under authenticated encryption / Sender Keys) and superseded RATCHET-4 (broadcast fan-out under pairwise ratchets) -- neither ratchets nor per-recipient encryption apply anymore, but the underlying risk they both flagged is REAL and still applies to a signature-only design: MSG-2 broadcasts to N agents as N separate deliveries, and without an extra check a malicious SENDER could put DIFFERENT content in each recipient's copy under the same broadcast id, and no individual recipient could tell (each copy's own per-message signature verifies fine in isolation). Fix: the sender additionally signs (invariant 9 -- crypto/ed25519.Sign, no custom construction) a digest over (broadcast_id, hash-of-body, the SORTED set of recipient fully-qualified ids), included in every recipient's envelope alongside the per-message signature from SIGN-2. A recipient who wants the 'everyone got the same broadcast' guarantee can compare this digest against other recipients' copies (e.g. via bus-trace tooling or by agents comparing out of band); document that comparison, don't just produce the digest and leave it unused. Use a standard, audited hash for hash-of-body (crypto/sha256, stdlib) -- not a bespoke construction. Tests: every recipient's digest for one broadcast is identical; a tampered per-recipient body still fails SIGN-2's per-message signature; a forged/mismatched digest is rejected. ADDED 2026-08-02 (invariant 7, epic-completion pass): the broadcast wrapper ships IN THIS TASK -- scripts/bus-broadcast.sh (AGENTIF-4) must produce both signatures via the `agent-bus sign` subcommand SIGN-2 adds, and AGENT_PROTOCOL.md must document the recipient-set digest and how two recipients compare it. A digest that no wrapper emits and no agent can check is not a defence. Verify through scripts/bus-broadcast.sh against a running throwaway bus, not hand-written curl.
  _Proof: go test -race -run TestBroadcastDigestSignature ./internal/..._

### EPIC TOOLING — The repo's own build & verification tooling (scripts/, .gitignore, dev env)

- [ ] None · proof-check.sh cannot tell "executed" from "asserted" -- adopt a zero-probe guard convention — tooling, P1
  scripts/proof-check.sh classifies a proof PASS / FAIL / VACUOUS / UNVERIFIABLE, and it correctly catches the classic vacuity of `go test -run TestThatDoesNotExist ./pkg` (prints `ok ... [no tests to run]` and EXITS 0). But it only verifies tests EXECUTED -- not that they ASSERTED anything. Observed in this project (AGENT_LOG.md, 2026-08-02 AUTH-2 entry): TestEveryRouteRequiresAuth's headline loop passed with ZERO children -- every registered route was on the allow-list, so `continue` fired every iteration and the body never ran. The existing `len(routes)==0` guard did not catch it because the slice was non-empty; it was the FILTERED set that was empty. The test ran, exited 0, and proved nothing.
  
  Proposed fix is a CONVENTION, not full mutation testing (out of proportion here): a test that loops over cases must count the probes it actually asserted (a `probed` counter) and `t.Fatalf` when that count is zero -- with the guard placed AFTER any filtering, since filtering is exactly what silently empties the set. Where zero is a legitimate expected outcome on the current build (as with TestEveryRouteRequiresAuth today, where every route IS on the allow-list), the convention must allow a documented exception (t.Logf with a named companion test that keeps the real assertion alive, per the existing pattern in internal/httpapi/authmw_internal_test.go's TestEveryRouteRequiresAuthOnASyntheticRoute) -- but that exception must be an explicit, reviewed choice, not silent.
  
  Scope:
  (a) Document the convention in CLAUDE.md's "Verify" section under a heading/phrase containing "zero-probe convention", including the AFTER-filtering placement rule and the documented-exception carve-out.
  (b) Survey the repo for enumeration-shaped tests (loops with a filter/continue) and apply the convention -- audit internal/httpapi/authmw_test.go and internal/httpapi/authmw_internal_test.go (both already have partial `probed` counters -- confirm/align them with the finished convention) plus any other loop-shaped test found elsewhere in the tree.
  (c) Decide, and record in DECISIONS.md under a section containing the phrase "zero-probe convention", whether proof-check.sh itself can detect the zero-probe case mechanically (e.g. via -v output inspection for `probed`/counter patterns) or whether the hand-written convention is the whole answer for now. Either way, write down the reasoning.
  
  Cross-reference: CLAUDE.md's "Verify" section already warns that grep-based doc proofs are the MORE dangerous vacuous family, because a loose pattern can match an unrelated line -- not hypothetical: task c27f9439's proof passed over a still-broken CONTRACTS.md:51 by matching an unrelated line in README.md. A doc proof must pin the SPECIFIC line/phrase it claims to prove and must be CONFIRMED RED before the fix -- a proof never observed failing is not evidence it CAN fail.
  
  proof_cmd was confirmed RED on 2026-08-02 (before this task's work) via:
    bash scripts/proof-check.sh "grep -A2 'zero-probe convention' CLAUDE.md | grep -q 'AFTER any filtering' && grep -q 'zero-probe convention' DECISIONS.md"
  Verdict: FAIL (class=file-assertion, exit=1) -- neither CLAUDE.md nor DECISIONS.md yet contains the phrase, confirming the proof is not vacuous-by-accident (it can and does fail today).
  
  Coordination note: CLAUDE.md and DECISIONS.md are shared files -- confirm no other agent is mid-edit on them before touching (per CLAUDE.md's parallel-agent-coordination rule: only one agent at a time, prefer adding a new dated section over editing existing lines). At time of filing (2026-08-02) a parallel loop had an agent editing CLAUDE.md/CONTRACTS.md/SPEC.md; do not clobber that work -- re-read the file immediately before editing.
  _Proof: bash scripts/proof-check.sh "grep -A2 'zero-probe convention' CLAUDE.md | grep -q 'AFTER any filtering' && grep -q 'zero-probe convention' DECISIONS.md"_
- [ ] None · ENV: docker CLI needs an explicit socket+binary shim for agent shells (workaround known, wrapper script not yet written) — process, P3
  Found 2026-08-02 while proving DEPLOY-2-FU-CONTAINERNAME. EVERY docker invocation from an agent shell fails, not just compose:
  
    $ docker ps
    cannot create user data directory: /home/mike/snap/docker/3505: Not a directory
  
  ROOT CAUSE (verified independently by triage, not just claimed by the sub-agent): /home/mike is a SYMLINK to /mnt/sdb4/mike/mike (ls -ld /home/mike -> lrwxrwxrwx root root 19 Jul 25 20:22). The docker snap's confinement does not resolve through that symlink, so it cannot create its per-user data dir. Reproduced with the sandbox disabled and with HOME overridden -- it is a snap-confinement/environment defect, entirely unrelated to agent-bus.
  
  CONSEQUENCE: any task whose proof_cmd shells out to docker or docker compose can only ever return UNVERIFIABLE from an agent. That includes DEPLOY-2's patched project-isolated proof (docker compose -p agentbus-proof up -d --build), DEPLOY-2-FU-CONTAINERNAME's own proof, and DEPLOY-3 -- which is the ONLY planned path to end-to-end RELAY verification. So this environment defect, not any code defect, is what currently blocks proving the container story.
  
  NEEDS THE USER (environment change, outside an agent's remit): e.g. install docker from the apt/official repo instead of snap, or give the docker snap a real non-symlinked HOME, or run compose proofs manually and paste the transcript. Agents must NOT attempt to reconfigure snap or docker themselves.
  
  Until this is fixed, treat every docker-based proof_cmd as UNVERIFIABLE and say so rather than recording a pass.
- [ ] None · PROOF-CHECK-FU-RECURSION: bash scripts/proof-check.sh hangs / spawns runaway processes when a proof_cmd nests another proof-check.sh invocation of a go-test command — tooling, P2
  Discovered 2026-08-02 during bookkeeping verification of the Proof-command guard task (84b76d5e). Composing `bash scripts/proof-check.sh --quiet "<a command that itself calls bash scripts/proof-check.sh --quiet 'go test ...'">` causes runaway recursive shim processes: the outer invocation's PATH-prepended go-shim directory persists into the nested bash -c subshell, so the inner proof-check.sh installs its OWN shim ahead of the outer one, the inner go test call resolves to a shim that itself re-invokes go test, and this recurses/forks until killed. Observed live: dozens of `/tmp/proof-check.*/bin/go test ...` and `tee -a .../gotest.log` processes accumulating; had to pkill -9 -f proof-check to recover. No repo file was touched, no lasting damage, but on a shared box this is a resource-exhaustion foot-gun for any agent that tries to write a self-checking or meta proof_cmd. Suggested direction (not investigated in depth): proof-check.sh should strip its own shim dir(s) from PATH before invoking a nested shell, or refuse/detect recursive invocation via a marker env var (e.g. if PROOF_CHECK_ACTIVE is already set, run the proof verbatim without installing a second shim). Reproduce: bash scripts/proof-check.sh --quiet 'bash scripts/proof-check.sh --quiet "go test -run TestNoSuchTest ./internal/wal"' (kill it within a few seconds, do not let it run to completion).
  _Proof: timeout 60 bash scripts/proof-check.sh 'bash scripts/proof-check.sh "true"'; test $? -ne 124_
- [ ] None · proof-check.sh runs the proof against its OWN script directory repo root, not the callers cwd -- silently defeats git-archive-overlay isolation testing — tooling, P1
  scripts/proof-check.sh:156-157 computes REPO_ROOT from SCRIPT_DIR (dirname of the script itself) and then, at the actual execution site (lines 508/510), does `( cd "$REPO_ROOT" && bash -c "$CMD" )` unconditionally -- it NEVER runs in the caller's own working directory. It also prints `running (cwd ${REPO_ROOT})...` which looks like a statement of fact but is really an announcement that the caller's cwd was silently discarded.
  
  CONSEQUENCE: the standard isolation technique this project uses to prove a change consumes nothing from other agents' uncommitted work is `git archive HEAD | tar -x -C <tmpdir>` into a clean overlay, then invoking proof-check.sh BY ABSOLUTE PATH against that overlay. Because the script always resolves back to its own repo (the live working tree), that invocation silently computes the verdict against the LIVE tree instead -- including every other agent's uncommitted changes -- and there is NO signal that this happened. An integrator committing RELAY-27 caught this by accident (had to copy the script into the overlay and re-run scoped correctly) -- same result that time, but only because they noticed. Most invocations would not notice.
  
  CONFIRMED RED (not VACUOUS), reproduced live 2026-08-08: created an overlay via `git archive HEAD | tar -x -C $OVERLAY`, wrote a marker file that exists ONLY in the overlay ($OVERLAY/MARKER.txt), cd'd into the overlay, then ran `bash /abs/path/to/scripts/proof-check.sh 'test -f ./MARKER.txt'`. Expected (if isolation held): PASS, since the file genuinely exists relative to the caller's cwd. Actual: `proof-check: running (cwd /mnt/sdb4/mike/mike/source/agent-bus)...` followed by `verdict=FAIL class=file-assertion exit=1` -- it silently substituted the live repo root for the overlay and the assertion failed there instead. The verdict is wrong AND there is no warning that the substitution occurred.
  
  RELATED, not duplicate: this is the SECOND defect found in this tool, after cea09b96 (subtest SKIP/PASS lines invisible to the plain-text counter, so a parent-PASS/all-children-SKIP certifies PASS instead of VACUOUS). Both defects share the same failure shape: the tool CLAUDE.md mandates specifically to make evidence trustworthy has produced a confidently wrong verdict, silently. Filed as a sibling task under the same TOOLING epic rather than a new umbrella parent -- this backlog already tracks proof-check.sh defects as discrete atomic tasks (PROOF-CHECK-FU-RECURSION, the zero-probe-guard-convention task, and cea09b96), so a new parent would mean retrofitting three live tasks rather than following the established pattern. A `relates` link to cea09b96 records the kinship without merging scope.
  
  DEFINITION OF DONE: (1) proof-check.sh either runs the proof in the CALLER'S cwd (the natural fix -- drop the `cd "$REPO_ROOT"` at the execution site, or make it conditional on being invoked with a relative path / no override), OR refuses loudly (distinct exit code / UNVERIFIABLE-class message) when it detects it is being invoked against a different repo root than the caller's cwd -- either is acceptable, but silent substitution is not. (2) A guard test demonstrates the isolation case concretely: same proof command, two trees (a real overlay via git archive, not a mock), two DIFFERENT verdicts -- i.e. a command that is genuinely true relative to the overlay and genuinely false (or absent) relative to the live repo, or vice versa, and the script's reported verdict tracks the overlay once fixed. (3) The `running (cwd ...)` line, once fixed, must report the directory the command ACTUALLY ran in, not a resolved repo root that may differ from where the caller invoked it.
  _Proof: REPO=$(pwd); T=$(mktemp -d); git archive HEAD | tar -x -C "$T"; echo only-in-overlay > "$T/ISO_MARKER.txt"; V=$(cd "$T" && bash "$REPO/scripts/proof-check.sh" "test -f ./ISO_MARKER.txt" 2>&1 | grep -o "verdict=[A-Z]*"); rm -rf "$T"; test "$V" = "verdict=PASS"_
- [ ] DISCOVERY-DOC-FU-GITIGNORE · DISCOVERY-DOC-FU-GITIGNORE: stale untracked busctl binary at repo root is not gitignored — repo-hygiene, P2
  Found independently by BOTH the reviewer and security gates during DISCOVERY-DOC. A 7.6 MB ELF executable named busctl sits untracked at the repo root, left behind by the cmd/busctl -> cmd/agent-busctl rename. .gitignore lists /agent-bus and /agent-busctl but NOT /busctl, so git check-ignore busctl reports it is NOT ignored and any git add -A would commit a binary into the repo. Given this project's documented history of index-sweeping commits mixing several agents' work, this is a live hazard. Fix: delete the artefact and add /busctl to .gitignore (or drop the entry deliberately if the old name is considered gone for good). Outside DISCOVERY-DOC's ownership boundary, so flagged not fixed.
- [ ] None · Conjunction-masking vacuous-proof family: filtered-clause proof_cmds hidden by an unfiltered && clause report PASS on a zero-match filter — tooling, P1
  THIRD vacuous-proof family, distinct from (1) a proof naming a test that does not exist, and (2) a negative-only grep satisfiable by deletion. This one is the most deceptive: proof_cmd of the shape go test -run '<filter>' ./pkg && go test ./pkg reports PASS with a LARGE tests_run even when the -run filter matches ZERO tests, because the second, UNFILTERED clause runs the whole package and its exit code carries the overall verdict. Unlike the first two families this fails LOUD-LOOKING: hundreds of genuinely passing tests mask a filter that matched nothing.
  
  AUDIT (2026-08-02, main/orchestrator): scanned the full backlog for proof_cmd containing both -run and && where the SECOND clause is an unfiltered go test on the same/related package (i.e. genuinely of this shape, as opposed to the many proof_cmds that chain two DIFFERENT -run-filtered clauses, or a -run clause with a grep/CLI check -- those are fine). Exactly THREE tasks have this shape, all P0:
    - 8c9b6489-abb1-444e-9eeb-3ff87646f632 (ID-2-WIRING-SEAL) -- status done. proof_cmd: go test -race -run TestSequenceRefusesToIssueFromAnUnsealedFloor ./internal/ids && go test -race ./internal/ids
    - cbc9ab0c-3b34-48d0-acd8-5eabd4dc4a02 (DUR-12) -- status done. proof_cmd: go test -race -run 'TestWALFrameMACRejectsAlteredPayload|...' ./internal/wal && go test -race ./internal/wal
    - c31f6999-da4e-400d-ab55-178b82e2a42e (ID-2-WIRING-OBSERVER) -- status todo. proof_cmd: go test -race -run TestWALReplayObservesEveryPrepare ./internal/wal && go test -race ./internal/wal
  
  The two DONE ones are NOT false passes: each was verified separately by the completing agent running and reporting the FILTERED clause alone (recorded tests_run=15 and tests_run=19 respectively in their completion test_summary/notes), so no already-completed work is in doubt. The risk is entirely FORWARD-LOOKING.
  
  THE LIVE EXPOSURE is c31f6999 (ID-2-WIRING-OBSERVER), still todo. Ran its proof_cmd verbatim through proof-check.sh on 2026-08-02: the filtered clause (TestWALReplayObservesEveryPrepare, a test that does not exist yet) reports 'no tests to run', but the unfiltered second clause runs the whole internal/wal package and the tool reports verdict=PASS tests_run=245 top_level=74 -- i.e. today, BEFORE any fix, this task could be closed on a proof that never ran the test it claims to add. (There IS a warning line about an empty package in proof-check.sh output, but the overall verdict is still PASS, which is the masking defect.)
  
  SCOPE for whoever takes this:
    (a) Fix c31f6999's proof_cmd so the filtered clause is verified ALONE (e.g. the DUR-3-style pattern: test $(go test -run X -v ./pkg 2>&1 | grep -c RUN) -gt 0 && go test -run X ./pkg -- both clauses filtered, so a zero-match filter fails the count check before the second clause can mask it).
    (b) Add this family to CLAUDE.md's 'Verify' section, alongside the existing two vacuous-proof warnings (test-that-does-not-exist; negative-only grep). NOTE: CLAUDE.md is a contended shared file -- coordinate the edit, prefer adding a new bullet rather than rewriting the section.
    (c) RECOMMENDED FIX (2026-08-02, main/orchestrator, reproduced directly -- see kind=report note for the full repro transcript): proof-check.sh ALREADY COMPUTES the right signal. Running c31f6999's live proof_cmd through it shows the filtered clause alone prints 'ok ... [no tests to run]', and proof-check.sh's own output includes both a human-readable warning ("READ THIS LINE before completing: if the test THIS task claims to add is in one of those packages, the proof did not exercise it") and a machine field `empty_pkgs=1` -- yet the final line still reads `verdict=PASS`. The defect is NOT detection (empty_pkgs is computed correctly); it is that this signal does not affect the verdict. CLAUDE.md tells every agent to quote the verdict specifically, so the one field the protocol trusts is exactly the field that lies, while the accurate signal sits one line away in a field nobody is told to read.
        THE FIX: make `empty_pkgs > 0` DOWNGRADE the verdict (to VACUOUS, or a new PARTIAL/UNVERIFIABLE value) instead of merely printing a warning -- a one-line conditional in a script that already computes the input, not a redesign. This automatically also closes vacuous-proof family (1) (a -run naming a nonexistent test), since that too yields empty_pkgs>0 -- one conditional covers two of the three families. Family (3) here (negative-only greps satisfiable by deletion) is a separate proof-authoring-convention problem and is NOT fixed by this.
        Refusing a proof_cmd containing && outright is now the FALLBACK option, not the leading one: conjunctions are a reasonable way to express "the named test passes AND the package stays green," and banning them outright would push authors toward worse proofs instead of fixing the real defect (a verdict that contradicts evidence the tool already gathered). Only consider the narrower "refuse a -run-filtered clause followed by an unfiltered same-package go test without an explicit non-empty-match check" rule if the empty_pkgs>0 downgrade proves insufficient in practice.
  
  PROOF_CMD for use of this scoping is confirmed RED (verdict=FAIL) via: bash scripts/proof-check.sh "grep -qi 'conjunction' CLAUDE.md && grep -q 'refuse' scripts/proof-check.sh" -> verdict=FAIL class=wrapper,file-assertion exit=1 (neither CLAUDE.md nor proof-check.sh yet mention this family -- confirmed BEFORE the fix, as required).
  
  
  ---
  FOURTH VACUOUS-PROOF FAMILY (2026-08-02, main/orchestrator): a PRE-SATISFIED CLAUSE. This task's OWN original proof_cmd -- `grep -qi 'conjunction' CLAUDE.md && grep -q 'refuse' scripts/proof-check.sh` -- was defective in exactly this new way: `grep -c 'refuse' scripts/proof-check.sh` returns 2 TODAY, before any fix, so that clause contributed zero verification (proof-check.sh already says 'refuse' twice, in unrelated prose about the decision NOT to refuse outright). The whole conjunction was held RED only by the other clause -- and it was pinned to the now-demoted 'refuse &&' fallback wording rather than the current leading fix (empty_pkgs>0 downgrade), so a correct implementation may never even add the word 'refuse'. A clause that is already true before the work starts makes a proof LOOK more rigorous (two checks!) while one of the checks verifies nothing. This is distinct from family (1) (a -run naming a nonexistent test), family (2) (a negative-only grep satisfiable by deletion), and family (3) (conjunction masking, this task's main subject): always verify EACH clause of a compound proof independently -- confirm it is actually RED on today's tree -- before combining them, and watch for clauses that can mask each other.
  
  REPLACEMENT proof_cmd (2026-08-02, main/orchestrator), verified clause-by-clause before combining:
    new proof_cmd: ! bash scripts/proof-check.sh 'go test -run TestDefinitelyDoesNotExistAnywhere ./internal/wal && go test ./internal/wal' | grep -q 'verdict=PASS' && grep -q 'empty_pkgs' CLAUDE.md
    - clause 1 (behavioural, not lexical): runs family (3)'s exact defect shape (`go test -run <nonexistent> ./internal/wal && go test ./internal/wal`) through scripts/proof-check.sh and asserts its verdict is NOT PASS. Verified RED today in isolation: the inner command currently reports `verdict=PASS class=test exit=0 tests_run=245 top_level=74 skipped=2 failed=0 empty_pkgs=1` (the -run filter matches zero tests, empty_pkgs=1, yet the unfiltered second clause still carries the verdict to PASS), so the negated grep for 'verdict=PASS' currently fails (exit 1) -- correctly RED, because the tool has not yet been fixed to downgrade on empty_pkgs>0.
    - clause 2 (documentation): pinned to the specific string 'empty_pkgs' -- the field name the recommended fix must act on -- rather than the loose word 'conjunction' used before. Verified absent from CLAUDE.md today: `grep -c 'empty_pkgs' CLAUDE.md` returns 0.
    - both clauses verified independently RED before combining (per the trap this task itself documents: clauses that can mask each other). Combined command verified RED directly in bash (`bash -c "$CMD"`, exit 1) and, separately, run itself through `scripts/proof-check.sh "$CMD"`, which reports: `proof-check: verdict=FAIL class=wrapper,file-assertion exit=1 tests_run=0 top_level=0 skipped=0 failed=0 empty_pkgs=0` -- confirmed RED, reproduced twice for stability.
  _Proof: ! bash scripts/proof-check.sh 'go test -run TestDefinitelyDoesNotExistAnywhere ./internal/wal && go test ./internal/wal' | grep -q 'verdict=PASS' && grep -q 'empty_pkgs' CLAUDE.md_
- [ ] None · proof-check.sh: subtest SKIP/PASS lines invisible to plain-text counter -- parent-PASS/all-children-SKIP certifies PASS instead of VACUOUS — tooling, P1
  proof-check.sh is the tool CLAUDE.md mandates specifically to stop a proof that runs nothing from being certified as passing. It has a blind spot of exactly that shape.
  
  In the plain-text (non -json) code path (scripts/proof-check.sh, around the PASSED/FAILED/SKIPPED counters), the counters are computed with column-0-anchored patterns:
    PASSED=$(grep -cE '^--- PASS:' "$GOTEST_LOG")
    FAILED=$(grep -cE '^--- FAIL:' "$GOTEST_LOG")
    SKIPPED=$(grep -cE '^--- SKIP:' "$GOTEST_LOG")
  Go indents subtest result lines with leading whitespace (e.g. '    --- SKIP: Test/Subtest'), so these patterns only ever match TOP-LEVEL result lines. A test whose parent PASSes while every one of its table-driven/t.Run children SKIPs is therefore counted as PASSED=1, SKIPPED=0, and sails through the existing 'PASSED==0 && SKIPPED>0 => VACUOUS' guard untouched, because that guard also only sees the parent.
  
  Note there IS a second, JSON code path (go test -json Action:pass/skip with a Test field) that counts subtests correctly regardless of nesting, because JSON events aren't indentation-sensitive. The bug is specific to the plain-text (-v, not -json) branch, which is what every proof_cmd in this repo actually uses.
  
  MEASURED LIVE (2026-08-08), RED before any fix:
    $ bash scripts/proof-check.sh "go test -run TestEnrolmentEpoch ./internal/hub"
    proof-check: verdict=PASS class=test exit=0 tests_run=4 top_level=1 skipped=0 failed=0
  while the verbose output underneath shows:
    --- PASS: TestEnrolmentEpoch (0.18s)
        --- SKIP: TestEnrolmentEpoch/HistoryRefusesTrafficThatPredatesTheReader
        --- SKIP: TestEnrolmentEpoch/AParkedPollIsNotWokenByTrafficThatPredatesTheReader
        --- SKIP: TestEnrolmentEpoch/AReusedAgentIDAfterARestartInheritsNoTraffic
  TestEnrolmentEpoch guards the P0 enrolment-epoch security fix from the 2026-08-02 audit and currently asserts nothing; a task closed on that exact proof_cmd would be certified PASS.
  
  Distinct from the other tracked vacuous-proof families: (1) a -run pattern matching zero tests (task 84b76d5e, fixed), (2) negative-only greps satisfiable by deletion, (3) conjunction-masking where an unfiltered && clause carries the verdict (task a9a433dd, open), (4) a pre-satisfied clause in a compound proof. This is a FIFTH family: correctly-shaped single-clause proofs where the counting itself under-reports skips because of indentation, not shell composition.
  
  SCOPE (definition of done -- the TOOL's report, not making TestEnrolmentEpoch itself pass, which is SIGN-3/gated work):
    (a) Count subtest PASS/FAIL/SKIP lines regardless of indentation in the plain-text path -- e.g. match on the trailing '--- (PASS|FAIL|SKIP):' token rather than anchoring '^' to a literal '-', or switch the shim to always request -json output (which does not have this defect) and drop the plain-text counting path entirely if that is simpler and does not regress the human-readable proof output CLAUDE.md relies on.
    (b) Specifically classify the parent-PASS/all-children-SKIP shape as VACUOUS: a test where every child subtest skipped but the parent itself reports PASS (because t.Run failures/skips don't fail the parent unless the parent itself calls Fail/Skip) must not read as an unqualified pass.
    (c) Add a regression case to whatever test suite covers proof-check.sh itself (or a scripted fixture under scripts/ if none exists) asserting this exact TestEnrolmentEpoch-shaped log is classified VACUOUS.
    (d) Note the finding in CLAUDE.md's 'Verify' section alongside the other vacuous-proof families, and in DECISIONS.md if the fix changes counting semantics materially.
  
  RELATED / DO NOT DUPLICATE: SIGN-3 (f2daa6bc-53ee-4788-935c-ab73693c5e75) is the reason TestEnrolmentEpoch and a large cascade of tests are currently skipped -- TestBroadcastSend, TestMessageHistoryCurser, TestWaiterWakeup, TestPollConcurrency, TestLongPollWait, TestAppliedKeyStoreSurvivesRestart, TestMessagingCrashRecovery and more (42 SKIP results were observed in a full verbose run), all gated behind SIGN-3 landing. This task is NOT about closing SIGN-3 or making those tests pass -- it is about proof-check.sh correctly reporting the vacuity while they remain skipped.
  
  proof_cmd confirmed RED on 2026-08-08 (before any fix):
    bash scripts/proof-check.sh "go test -run TestEnrolmentEpoch ./internal/hub" | grep -q 'verdict=VACUOUS'
    -> exit 1 (grep found no match; live verdict was 'verdict=PASS ... skipped=0', confirming the tool does NOT today classify this as vacuous).
  _Proof: bash scripts/proof-check.sh "go test -run TestEnrolmentEpoch ./internal/hub" | grep -q 'verdict=VACUOUS'_
