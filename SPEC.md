# Agent Bus

> Checkbox legend: `[ ]` todo · `[~]` in progress · `[x]` done · `[-]` superseded/cancelled.

## Backlog

- [ ] None · AUTH-2-FU-POLLEXPIRY: A long-poll can outlive its session, quietly contradicting immediate revocation — auth, P1
  Origin: security audit of AUTH-2, 2026-08-02, found forward-looking. Authentication is evaluated ONCE at request entry, and -poll-timeout is validated only as > 0 (cmd/agent-bus/main.go:137) with no ceiling against the 1h auth.SessionLifetime. A poll parked at entry keeps serving after its session expires or is revoked. auth.Principal already carries ExpiresAt, so a handler CAN enforce it but nothing requires it. The POLL epic must cap the wait at min(PollTimeout, time.Until(principal.ExpiresAt)) and re-Authenticate before delivering. P1 because "revocation is immediate" (DECISIONS.md 2026-08-02) is otherwise false for any poll already in flight.
- [ ] INVITE-GATE · INVITE-GATE: POST /v1/enroll REQUIRES a valid invite and fails closed; invite consumption and the roster write commit TOGETHER — auth, P0
  EPIC: 0b43393e-556b-409a-938a-846be2fb4a75 | DEPENDS ON: ENROL-SHAPE, INVITE-STORE, INVITE-MINT | BLOCKS: INVITE-HARDEN, INVITE-REVOKE, INVITE-CLIENT, INVITE-PEERGUARD
  
  This is the epic's crux and the root fix for the pre-auth attack family. internal/httpapi/auth.go:122 handleEnroll and internal/auth/service.go:276 Service.Enrol gain the gate. THE CORRECTNESS CRUX: single-use consumption and the enrolment effect must land in the SAME two-phase transaction, or a crash between them either burns an invite with no agent or enrols an agent without spending the invite. SECOND CRUX (invariant 10): a legitimate retry carrying the same idempotency_key and the same payload must return the ORIGINAL result and must NOT consume the invite a second time; same key with a DIFFERENT payload stays a 409 + Connection: close. Must update CONTRACTS-HTTP.md -- in particular the "Known gaps" bullet at CONTRACTS-HTTP.md:172-186 which currently states enrolment is unauthenticated, and the POST /v1/enroll route rows at CONTRACTS-HTTP.md:14-17. BREAKING WIRE CHANGE -- escalated to the user; do not land before ENROL-SHAPE. RESIDUAL RISK TO DOCUMENT IN THE SAME TASK: until MTLS-LISTENER lands, the invite secret crosses the wire in CLEARTEXT; exposure is bounded only by the -listen 127.0.0.1:8080 loopback default, and the bus must not be exposed on a non-loopback interface until mTLS ships.
  _Proof: go test -race -run 'TestEnrolRequiresInvite|TestEnrolConsumesInviteAtomically|TestEnrolRetryDoesNotReconsumeInvite' ./internal/auth ./internal/httpapi && grep -q 'invite' CONTRACTS-HTTP.md_
- [~] None · EPIC: invite-only enrolment -- the root fix for the pre-auth attack family (needs planner pass) — auth, P0, in progress
  USER DECISION, 2026-08-02 (DECISIONS.md "Five decisions" #2; CLAUDE.md invariant 3, amended). Root fix for the entire pre-auth attack family. Enrolment is currently UNAUTHENTICATED, so an attacker can mint its own agents and, from there, exhaust the session table (AUTH-1-FU-ACTIVECAP, raised to P0 as defence-in-depth behind this gate), lock out a named agent (AUTH-1-FU-PENDINGCAP, already fixed), or enumerate the roster. Capping table sizes patches the symptoms one at a time; the invite removes the capability that makes all of them possible.
  
  REQUIREMENTS (from the user, verbatim in substance):
  - Invites are SINGLE-USE, EXPIRING and REVOCABLE, minted by an operator.
  - Redeeming an invite is the ONLY route onto the bus -- including for PEER buses (bus-to-bus enrolment/federation must also go through invite redemption, not a separate unauthenticated path).
  - Composes with mTLS (see the paired mTLS epic): the invite is what AUTHORISES binding a new client certificate to a new agent id -- invite redemption and certificate binding happen together, not as two independent gates either of which alone would suffice.
  - CLAUDE.md invariant 3 now covers this directly: "Enrolment is INVITE-ONLY... No agent may enrol without redeeming an operator-minted invite... Invites must be single-use, expiring, and revocable, and redeeming one is the ONLY way onto the bus -- including for peer buses." Read that invariant in full before design.
  
  CONSENT-SENSITIVE: this changes AUTHN DEFAULTS (enrolment moves from open to gated) -- per this project operating rules that is a consent-gated action even though the user has already decided the shape; the atomic tasks under this epic should still each be explicit about what changes for an operator standing up a fresh bus (an invite must now be minted before the FIRST agent can enrol, including whatever bootstraps the operators own tooling).
  
  NEEDS A PLANNER PASS before implementation: this is an epic, not an atomic task. A planner should break it into atomic tasks covering at minimum: invite data model + storage (durable, survives restart -- consider how this interacts with the WAL/store), invite minting (operator-facing, likely a CLI/admin route), invite redemption at enrolment (single-use enforcement, expiry check, replaces or gates todays open enrol route), revocation, peer-bus enrolment redemption (federation path), CONTRACTS-HTTP.md + AGENT_PROTOCOL.md updates, and the mTLS cert-binding integration point once the mTLS epic lands enough to bind to.
  
  Does not yet have atomic sub-tasks; do not claim-next this epic directly -- claim-next the atomic tasks a planner files under it once that pass runs.
- [ ] MTLS-CLIENTCERT · MTLS-CLIENTCERT: the client generates and stores its own TLS keypair + self-signed certificate (0600) and presents it on every connection — agentif, P1
  EPIC: a1b628fb-8cbf-47e8-9682-034fda8636c7 | DEPENDS ON: MTLS-DESIGN, CLI-1 (0495d133) | BLOCKS: MTLS-PIN
  
  Client-side half of the mutual handshake, in the importable client/ package (NOT under internal/ -- CLI-1's non-negotiable). Key stored 0600 in the user's config dir, never in the repo, no interactive prompt, no TTY-dependent input. Stdlib crypto/tls + crypto/x509 only. DEPENDS ON CLI-1 (0495d133) -- no client package exists today.
  _Proof: go test -race -run 'TestClientGeneratesClientCert|TestClientTLSKeyIs0600' ./client/..._
- [-] None · [SUPERSEDED by epic a1b628fb-8cbf-47e8-9682-034fda8636c7] No transport security (TLS) anywhere in the server -- decision made, options list is stale — security, P1
  BLOCKED ON USER DECISION -- do not start implementation. This task exists to record the gap and lay out options; a design/consent decision from the user is required before any code changes.
  
  The server has no transport security of any kind. This is now load-bearing rather than theoretical: with no per-agent binding left on the session token, token unguessability is the ONLY thing protecting a session, and an on-path network observer can also kill a pending challenge outright (no confidentiality or integrity on that handshake). The default listen address was just moved to loopback (task c27f9439), which contains the exposure for a single-host deployment but does nothing for the Docker Compose / multi-bus relay target, where buses talk to each other over a real network -- that is precisely invariant 2's cross-bus routing, so the relay path is plaintext today.
  
  Options to lay out for the user (none pre-selected):
  1. Terminate TLS in the server itself (Go stdlib crypto/tls + net/http's ListenAndServeTLS -- this is stdlib, not 'writing your own crypto': TLS termination via crypto/tls is exactly the audited, high-level API the project's crypto rule calls for, as opposed to hand-rolling a handshake). Needs a cert/key provisioning story (self-signed for dev, ACME/reverse-proxy-issued for prod).
  2. Require a reverse proxy / sidecar (e.g. Caddy, nginx, an Envoy sidecar) in front of the server and document that as the SUPPORTED deployment; the Go server stays plaintext-over-localhost/private-network only. Simplest for the single-bus case, weakest for bus-to-bus relay unless every hop also proxies.
  3. Mutual TLS specifically between relaying buses (bus-to-bus federation traffic authenticates both ends via client + server certs), leaving agent<->bus traffic on option 1 or 2. Most targeted at the actual multi-bus relay risk, but adds cert lifecycle for every bus pair.
  
  Constraints that bear on the decision:
  - NEVER WRITE OUR OWN CRYPTO (absolute project rule, CLAUDE.md rule 9) -- any of the above must use crypto/tls or an equivalently audited, high-level library. No hand-rolled handshake, padding, or nonce scheme under any option.
  - Invariant 3: enrolment issues a signed credential and every route except enrolment authenticates -- TLS is a complement to that credential, not a replacement; the credential-forging and replay protections stay required regardless of which TLS option is chosen.
  - Invariant 2: cross-bus routing depends on unambiguous `<bus-id>.<agent-id>` addressing carried over relay hops -- whichever TLS option is chosen must not break that addressing or the relay's loop-prevention (traversed bus path).
  - Exposing the bus on a non-loopback interface, and any change to authn/authz defaults, are CONSENT-GATED actions per this project's operating rules -- flagging explicitly here since options 1 and 3 both imply the server (or a sidecar in front of it) eventually listens on a non-loopback interface for the Compose/multi-bus target.
  
  No proof_cmd yet -- there is nothing to prove until a decision is made and an implementation task is filed against it. When unblocked, split into: (a) the chosen TLS mechanism, (b) cert/key provisioning + rotation story, (c) the paired <KEY>-DEPLOY/<KEY>-VERIFY task per CLAUDE.md's committed-vs-running rule, since 'TLS code compiles' and 'a bus-to-bus relay call in Docker Compose is actually encrypted on the wire' are different claims.
- [ ] None · AUTH-2-FU-SESSMU: auth.Service.Authenticate now takes an exclusive mutex on every request's hot path — auth, P2
  Origin: security audit of AUTH-2, 2026-08-02. internal/auth/service.go guards the session table with a plain sync.Mutex. AUTH-2 puts Authenticate on EVERY request, while the UNAUTHENTICATED BeginSession holds the same mutex for an O(n) sweepLocked. An anonymous /v1/session/begin flood can therefore stall legitimate authenticated traffic in a way it could not before AUTH-2. Fix: give Authenticate an RWMutex read path, or amortise the sweep. Note this lives in internal/auth, which AUTH-2 deliberately did not touch.
- [ ] MTLS-LISTENER · MTLS-LISTENER: serve TLS ONLY and REFUSE TO START without a usable cert/key -- there is no plaintext listener — security, P0
  EPIC: a1b628fb-8cbf-47e8-9682-034fda8636c7 | DEPENDS ON: MTLS-DESIGN, MTLS-BUSCERT | BLOCKS: MTLS-CLIENTAUTH, MTLS-VERIFY
  
  invariant 11. Today cmd/agent-bus/main.go:375 does net.Listen("tcp", cfg.Listen) and main.go:386 does srv.Serve(ln); http.Server at main.go:368-372 sets no TLSConfig and there is no TLS/x509 code anywhere in the tree. Attach via tls.NewListener or srv.ServeTLS. The server must exit non-zero with a remedial message naming the cert path rather than degrading. Config.validate() (main.go:128-152) is purely syntactic and has no data-dir knowledge, so the refusal belongs in run(), not flag parsing. New flags land in CONTRACTS-CLI.md. BREAKING -- escalated: this strands every plaintext client, including scripts/bus-serve.sh's health probe (fixed by MTLS-VERIFY) and CLI-2's recorded proof_cmd.
  _Proof: go test -race -run 'TestServerServesTLSOnly|TestPlaintextClientIsRejected|TestRunRefusesToStartWithoutUsableCert' ./cmd/agent-bus && grep -qi 'tls' CONTRACTS-CLI.md_
- [ ] INVITE-MINT · INVITE-MINT: an operator mints a single-use, expiring invite -- the server is authoritative on the invite id and secret — auth, P0
  EPIC: 0b43393e-556b-409a-938a-846be2fb4a75 | DEPENDS ON: INVITE-STORE | BLOCKS: INVITE-GATE
  
  invariant 1 -- the invite secret and invite id are minted by the server from crypto/rand; a client-supplied value is never accepted as either. The invite must NOT let a redeemer choose or influence its future agent id. The MINTING SURFACE depends on the escalated bootstrap decision (server subcommand writing to the data dir, versus an authenticated admin HTTP route) -- do not implement until that is answered; if the answer is an HTTP route, retarget the doc clause of the proof from CONTRACTS-CLI.md to CONTRACTS-HTTP.md via spec-keeper before starting.
  _Proof: go test -race -run 'TestInviteMintIsServerAuthoritative|TestInviteMintRejectsClientSuppliedSecret' ./internal/invite && grep -qi 'invite' CONTRACTS-CLI.md_
- [ ] None · MSG-FU-MAINWIRING: main should construct the hub and pass it as BOTH httpapi.Options.Hub and wal.LogOptions.Applier — core, P1
  The MSG/POLL wave could not touch cmd/** (file ownership), so httpapi.New builds the hub itself whenever Options.Durable also satisfies Path() + Recovered() -- see openHub in internal/httpapi/server.go, which documents this as transitional. Two costs to remove: (1) the durable log is REPLAYED TWICE at startup, once as an fsck by wal.Open with a nil Applier and once read-only by the hub to rebuild the store; (2) a rebuild FAILURE cannot be fatal because httpapi.New returns no error -- it is logged at ERROR and the messaging routes are left unregistered, so an operator sees 404s rather than a refusal to start, which is indistinguishable from running an old build. FIX: main constructs the hub, passes it as wal.LogOptions.Applier so replay happens exactly once inside wal.Open, seals the sequence floor from wal.Recovered, and hands it to httpapi.Options.Hub; a failure is then a startup error. openHub and the recoverableLog assertion are deleted in the same change. ALSO IN SCOPE: cmd/agent-bus/main_test.go TestShutdownReleasesLongPoll parks a SYNTHETIC handler -- point it at the real GET /v1/wait now that the route exists, so the ordering guard covers the real park.
  _Proof: go test -race -run TestRunWiresTheHub ./cmd/agent-bus_
- [x] TRIAGE-LOCK · TRIAGE-LOCK: backlog-triage mutex — process, P0
  Reusable mutex. in_progress = held, done = free. Whoever holds it is the only agent allowed to dispatch from the backlog. Acquire = compare-and-set on status via If-Match: "v<version>"; on 412 you lost the race, STOP, do not retry. Release = PATCH {status:done}. Do NOT use /complete on it. NEVER delete this task. Holder identity lives in status_note, never here.
  _Proof: n/a - process lock_
- [ ] MTLS-CROSSCHECK · MTLS-CROSSCHECK: reject a session token presented over a connection whose client certificate belongs to a DIFFERENT agent — security, P0
  EPIC: a1b628fb-8cbf-47e8-9682-034fda8636c7 | DEPENDS ON: MTLS-BIND | BLOCKS: MTLS-VERIFY, AUTH-2-FU-POLLEXPIRY (03d7ca66)
  
  **THE PART MOST LIKELY TO BE QUIETLY OMITTED -- the user called this out by name. DO NOT fold it into MTLS-BIND and do not complete either task on the other's tests.** CLAUDE.md invariant 11 and DECISIONS.md:1139-1144: mTLS does NOT replace the session token; BOTH are required and they must be CROSS-CHECKED. mTLS proves which key holder is on the connection; the session token is the revocable, time-bounded application credential. Three call sites, all of which must be covered: (1) (*Server).authMiddleware (internal/httpapi/authmw.go:241, which calls s.auth.Authenticate at :277 and attaches the auth.Principal at :299) must compare the connection's peer-cert fingerprint against the fingerprint bound to principal.AgentID; (2) POST /v1/session/begin (internal/httpapi/auth.go:172) takes an agent_id from an unauthenticated body -- a begin naming agent X over agent Y's certificate must be refused; (3) POST /v1/session/complete (auth.go:211) re-reads the roster entry at internal/auth/session.go:344. NOTE httpapi.Options.Auth is the CONCRETE *auth.Service (internal/httpapi/server.go:108), not an interface, so this needs either a new method (e.g. AuthenticateBound(token, fingerprint)) or a new interface seam. A mismatch is a protocol violation, not a routine 401 -- log it as security. Also record in this task that AUTH-2-FU-POLLEXPIRY (03d7ca66) must re-evaluate the cross-check mid-poll, not only at request entry.
  _Proof: go test -race -run 'TestSessionTokenRejectedOnForeignClientCert|TestSessionBeginRejectedOnForeignClientCert|TestSessionCompleteRejectedOnForeignClientCert' ./internal/httpapi ./internal/auth && grep -qi 'cross-check' CONTRACTS-HTTP.md_
- [x] None · CONTRACTS-SPLIT: split CONTRACTS.md into per-plane files (pure move) + retarget every proof_cmd — documentation, P1
  Split the single CONTRACTS.md file into per-plane files (a pure content move, verbatim, no rewording) to remove a single-writer chokepoint that has caused three P0s in two consecutive triage loops. Keep CONTRACTS.md at the old path as an index. Retarget every queued task whose proof_cmd or description names CONTRACTS.md and specific line numbers, since after the split those line numbers move to different files. Update CLAUDE.md's repository-layout section and step-9 workflow instruction to say which file to update for which surface. Known affected tasks to repoint: c27f9439 (AUTH-1-FU-LISTENADDR), b0a5630b (LISTENADDR-FU-CONTRACTS, needs a proof_cmd added), 5b178dde (DUR-11-FU-CONTRACTS), 8c9b6489 (ID-2-WIRING-SEAL), c31f6999 (ID-2-WIRING-OBSERVER), 2d92b699 (AUTH-1-FU-ACTIVECAP), a24bb214 (DOCS-3).
  _Proof: test -f CONTRACTS-HTTP.md && test -f CONTRACTS-ONDISK.md && test -f CONTRACTS-AGENT.md && test -f CONTRACTS-CLI.md && grep -q "CONTRACTS-HTTP.md" CONTRACTS.md && grep -q "CONTRACTS-ONDISK.md" CONTRACTS.md && grep -q "CONTRACTS-AGENT.md" CONTRACTS.md && grep -q "CONTRACTS-CLI.md" CONTRACTS.md && ! grep -q "^## Routes" CONTRACTS.md_
- [ ] MTLS-DESIGN · MTLS-DESIGN: record the decided certificate lifecycle in DECISIONS.md -- key locations, how a client learns the bus fingerprint, rotation, expiry, and the no-plaintext-in-tests answer — security, P0
  EPIC: a1b628fb-8cbf-47e8-9682-034fda8636c7 | DEPENDS ON: none | BLOCKS: MTLS-BUSCERT, MTLS-LISTENER, MTLS-CLIENTAUTH, MTLS-BIND, MTLS-CLIENTCERT, MTLS-PIN
  
  BLOCKED ON USER DECISION. DECISIONS.md:1131-1147 settles "self-signed, mutual, no CA, bound at enrolment" but leaves these open, and every one of them is load-bearing: (1) how a client learns the bus's cert fingerprint BEFORE its first connection -- the planner recommends the invite blob carry bus-id + address + bus-cert fingerprint + invite secret, which removes the TOFU window entirely and is what makes the two epics genuinely compose, versus plain TOFU-on-first-connect; (2) certificate validity period and what happens when an agent's client cert EXPIRES (re-enrol with a fresh invite, or a re-bind route); (3) bus-key rotation, which invalidates every client's pin -- accepted "operator must re-pin" event, or must the bus serve two certs during a rollover; (4) whether a plaintext escape hatch exists for tests or local dev (invariant 11 says no); (5) whether the cert/key are always self-generated or may be operator-supplied via flags. INVARIANT 9 IS ABSOLUTE: stdlib crypto/tls + crypto/x509, standard fingerprint = SHA-256 over the certificate DER. No hand-rolled fingerprint scheme, cert format, nonce or key exchange -- if a sub-task looks like it needs one, it is mis-specced; stop and escalate.
  _Proof: grep -q 'MTLS-DESIGN' DECISIONS.md && grep -q 'InsecureSkipVerify' DECISIONS.md && grep -qi 'rotation' DECISIONS.md && grep -qi 'fingerprint' DECISIONS.md_
- [ ] INVITE-CLIENT · INVITE-CLIENT: the Go client/CLI redeems an invite at enrol (+ AGENT_PROTOCOL.md entry) -- invariant 7's delivery vehicle is the CLI subcommand, NOT a bus-enrol.sh — agentif, P1
  EPIC: 0b43393e-556b-409a-938a-846be2fb4a75 | DEPENDS ON: INVITE-GATE, CLI-1 (0495d133), CLI-2 (39318208) | BLOCKS: none
  
  DECIDED AND RECORDED HERE so it is not re-litigated -- invite redemption reaches an agent as a flag on the existing CLI identity subcommand (agent-bus-cli enrol --invite <blob>), NOT as a new scripts/bus-*.sh wrapper. This is consistent with the 2026-08-02 amendment to invariant 7 (DECISIONS.md:605-637, "The Go CLI replaces the shell wrappers"), with CLI-2 (39318208) which absorbed enrolment, and with AGENTIF-2 (15e4509c) which is already superseded. DEPENDS ON CLI-1 (0495d133) and CLI-2 (39318208) -- neither exists yet; there is no client package and no second cmd binary today. CONTRADICTION TO RESOLVE BEFORE STARTING (flagged by the planner, who was boundary-blocked from editing CLI-*): CLI-2's recorded proof_cmd enrols with no invite and over http://, so it is invalidated by BOTH this task and MTLS-LISTENER.
  _Proof: go test -race -run TestClientEnrolWithInvite ./client/... && grep -qi 'invite' AGENT_PROTOCOL.md && grep -qi 'invite' CONTRACTS-AGENT.md_
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
- [-] None · AUTH-2-FU-RATELIMIT: Rate-limit the unauthenticated routes and the 401 path — auth, P2
  Origin: security audit of AUTH-2, 2026-08-02. Measured amplification is ~2.5x over the per-request access log LoggingMiddleware already emits (forged token = 400 bytes/2 lines vs a 158-byte/1-line baseline), so this is a constant factor on a pre-existing vector, NOT a new unbounded one -- which is why it did not block AUTH-2. Security explicitly recommends NOT deleting the Info-level "bearer token did not authenticate" line: it is the only signal an operator gets that someone is guessing credentials. The honest fix is per-source rate limiting on /v1/enroll, /v1/session/begin and the 401 path, plus log rotation.
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
- [ ] None · DUR-11-FU-CONTRACTS: CONTRACTS.md still documents the reverted refuse-to-start WAL policy verbatim — documentation, P1
  Discovered during the DUR-11 orphaned-task reconciliation pass (2026-08-02). HALF OF THIS TASK IS ALREADY DONE: c7e017d removed the stale "provably torn tail" / "refuses to start and leaves the file byte-for-byte" / "RepairTail" phrasing (verified absent from CONTRACTS-ONDISK.md, the plane the WAL-repair section moved to in the CONTRACTS split, 360a2679). THE REMAINING, UNMET HALF: CONTRACTS-ONDISK.md has ZERO mention of RepairLog, the bus.wal.corrupt-<ts> quarantine-rename-aside artefact name, the .repair temp-file-during-rewrite artefact name, or the Repair/Recovered struct fields actually surfaced to callers (Rewritten, Quarantined, DiscardCount, MissingRecords, Exhausted) -- confirmed via grep, zero matches for every one of the eight terms (2026-08-02). Fix: document the SHIPPED RepairLog / quarantine / always-restart behaviour in CONTRACTS-ONDISK.md, naming the on-disk artefacts and enumerating the struct fields.
  
  *** BLOCKING: DO NOT DISPATCH until DUR-12 (cbc9ab0c) lands. *** DUR-12 is rewriting the on-disk WAL format (CRC32C -> HMAC-SHA256 MAC, format version 2) right now and will change this exact plane -- documenting the WAL surface concurrently would be stale on arrival, same ordering constraint applied to e120153b and db350e39.
  _Proof: grep -qF "RepairLog" CONTRACTS-ONDISK.md && grep -qF "bus.wal.corrupt-" CONTRACTS-ONDISK.md && grep -qF ".repair" CONTRACTS-ONDISK.md && grep -qF "Rewritten" CONTRACTS-ONDISK.md && grep -qF "Quarantined" CONTRACTS-ONDISK.md && grep -qF "DiscardCount" CONTRACTS-ONDISK.md && grep -qF "MissingRecords" CONTRACTS-ONDISK.md && grep -qF "Exhausted" CONTRACTS-ONDISK.md && ! grep -qE "provably torn tail|refuses to start and leaves the file byte-for-byte|RepairTail" CONTRACTS-ONDISK.md_
- [ ] None · ENV: docker CLI unusable for agents (snap confinement vs $HOME symlink) -- every compose proof_cmd is UNVERIFIABLE — process, P2
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
- [ ] None · MSG-FU-SEQHIGHWATER: persist the message-sequence high-water mark so deep log damage cannot regress it — durability, P1
  Found by the test-engineer during MSG-5 crash injection (2026-08-02), MEASURED not assumed. The hub derives its sequence floor from wal.Recovered.NextIndex-1, raised to the highest replayed sequence. Each message burns 1 sequence and 2 WAL indices, so a truncation removing MORE records than the bus issued sequences leaves the floor at or below a sequence already issued, and the bus reissues message ids (invariant 1). Measured over a 585-offset truncation sweep of a 2523-byte WAL: 70 offsets regressed, all at n <= 1449 of 2523 -- every cut losing more than half the records. Inside the genuine crash window (a tear between two fsyncs, n >= 2038) the strong property HOLDS and is asserted by TestMessagingCrashRecovery; deep cuts are media damage and the information needed to reconstruct the mark is gone with the bytes. The SAME hazard exists for a QUARANTINED log, where recovery starts a fresh file whose index restarts near 1 -- hub.Open logs that at ERROR and DECISIONS.md 2026-08-02 records it as an accepted, bounded risk, but nothing can recover the mark. FIX: a separately-persisted, fsynced sequence high-water mark, written AHEAD of the sequence it authorises, that recovery reads even when the WAL is damaged or quarantined. AGENT_LOG.md 2026-08-02 (ID-2-WIRING-SCHEMA) already asked where this value lives on disk. NOTE: needs a RESERVED on-disk record-type number via the reservations API -- do not pick one.
  _Proof: go test -race -run TestSequenceHighWaterSurvivesDeepDamage ./internal/hub_
- [ ] None · os.MkdirAll(cfg.DataDir, 0o700) at main.go:157 never tightens an ALREADY-LOOSE pre-existing data dir — durability, P1
  cmd/agent-bus/main.go:157 -- `if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil { ... }`. The comment above it (main.go:155-156) says "0o700: the store holds agent credentials", but os.MkdirAll's mode argument is ONLY applied when it actually creates the directory; per the Go stdlib doc and POSIX mkdir(2) semantics, MkdirAll on a directory that already exists is a no-op with respect to permissions -- it does not chmod it. So a data dir that pre-exists with a looser mode (world-readable/writable -- left over from a bad deploy script, an operator `cp -r` that didn't preserve modes, a container image built with a permissive umask, or a restore from backup) silently KEEPS that loose mode forever. Every subsequent invariant this project cares about (agent credentials in AUTH's roster store, the WAL itself) then lives inside a directory that is not actually 0700, contradicting the comment's own stated intent.
  
  Confirmed experimentally this pass (standalone stdlib-only probe, no repo file touched): os.Chmod(dir, 0o777) followed by the EXACT call at main.go:157 (os.MkdirAll(dir, 0o700) on that already-existing dir) leaves the directory at 0777 -- MkdirAll returns nil and never touches the mode of a directory that was already there.
  
  DONE means: run() explicitly Stat()s the data dir after MkdirAll and, if the existing mode is looser than 0700, either os.Chmod(dir, 0o700)s it (loudly logged, since silently tightening permissions under something already running could theoretically break another process assuming looser access -- unlikely here but worth a WARN) or refuses to start with a clear error naming the actual mode found, whichever the implementer judges correct after reading DUR-8's dirlock precedent (internal/dirlock already assumes an 0700-equivalent trust boundary on this same directory).
  
  proof_cmd validated via scripts/proof-check.sh: verdict=FAIL (exit 1) today -- no os.Chmod call exists anywhere in main.go yet. Will PASS once the fix adds one.
  _Proof: grep -q "os.Chmod" cmd/agent-bus/main.go_
- [ ] MTLS-RELAYGUARD · MTLS-RELAYGUARD: bus-to-bus relay links are mutually authenticated too -- acceptance criterion plus a guard test — security, P2
  EPIC: a1b628fb-8cbf-47e8-9682-034fda8636c7 | DEPENDS ON: MTLS-CLIENTAUTH, INVITE-PEERGUARD | BLOCKS: RELAY-1 (9bc9d6c4), RELAY-2 (654140d7)
  
  Every relay hop is both a certificate-verifying TLS client and a TLS server, and invariant 2's <bus-id>.<agent-id> addressing plus traversed-bus-path loop prevention must keep working over it. internal/relay/ is a stub (internal/relay/doc.go:8), so the landable increment now is the guard and the acceptance criterion; RELAY-1 (9bc9d6c4) and RELAY-2 (654140d7) must satisfy it (the planner was not permitted to edit those tasks). Pairs with INVITE-PEERGUARD: a peer bus needs BOTH an invite and mutual TLS.
  _Proof: go test -race -run 'TestRelayDialerRequiresMutualTLS|TestRelayListenerRequiresClientCert' ./internal/relay_
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
- [x] None · Proof-command guard: a `-run` pattern that matches no test must FAIL, not pass vacuously — process, P0
  TRAP FOUND 2026-08-02 by backlog-triage. `go test -race -run TestCorruptTailTruncation ./internal/wal` prints "ok ... [no tests to run]" and EXITS 0. Every proof_cmd in this backlog is of that shape, so a task can be flipped to done with a proof command that proves literally nothing. Verified: DUR-4 and DUR-6 proofs both exit 0 today with zero tests run. Verified also that NO completed task is currently affected -- all 13 done tasks' proofs were re-run and each executes >0 tests -- so this is a PREVENTIVE guard, not a cleanup of existing corruption. Deliverable: (1) scripts/proof-check.sh -- runs a proof command and FAILS unless it can show at least one test actually ran (parse `-v` RUN/PASS output or `[no tests to run]`), while still supporting the non-Go proofs already in the backlog (test -s FILE, grep -q, scripts/bus-*.sh invocations) -- those must remain valid and must not be forced into a test-count check. (2) A sweep report of all ~70 proof_cmd values classifying each as test-based / file-assertion / wrapper-based / unverifiable, posted as a task note. (3) CONTRACTS.md entry for the script. Policy question to ANSWER in the deliverable: should completion require proof-check.sh rather than a bare command?
  _Proof: test -x scripts/proof-check.sh && bash -n scripts/proof-check.sh && grep -q "scripts/proof-check.sh" CONTRACTS.md_
- [ ] None · CLI-FU-SEEDREDACT: pendingEnrolment holds a raw private-key seed with no redacting String() — security, P2
  Found by the security gate during the MSG/POLL wave (2026-08-02), reported as provisional because client/ was untracked and under concurrent edit at the time. client/store.go: Credential has a redacting String() (around store.go:122-125) but pendingEnrolment carries the same PrivateKeySeed and does NOT. No code path formats it today, so this is parity hardening rather than a live leak -- but the whole point of a redacting String() is that it protects the field before someone adds the %v that would print it. Add the same redaction, and a test that a formatted pendingEnrolment contains no byte of the seed. Owner: the CLI epic (client/** was outside the MSG/POLL wave's ownership).
  _Proof: go test -race -run TestPendingEnrolmentRedactsItsSeed ./client_
- [ ] ENROL-SHAPE · ENROL-SHAPE: settle the FINAL /v1/enroll wire shape and auth.RosterEntry field set ONCE, before invite, mTLS or proof-of-possession break it three times — auth, P0
  EPIC: 0b43393e-556b-409a-938a-846be2fb4a75 | DEPENDS ON: none | BLOCKS: INVITE-STORE, INVITE-GATE, MTLS-BIND, AUTH-3 (d53e3b21), AUTH-1-FU-POPKEY (6e3083b0-c113-4b26-9dd6-025825671ceb)
  
  BLOCKED ON USER DECISION -- do not implement until the escalated questions are answered (bootstrap/who mints the first invite; how a client learns the bus cert fingerprint; migration for already-enrolled agents; rotation/expiry). Three separately-filed changes each break POST /v1/enroll's request body: the invite field (INVITE-GATE), the client-cert fingerprint binding (MTLS-BIND), and the proof-of-possession signature already filed as AUTH-1-FU-POPKEY (6e3083b0-c113-4b26-9dd6-025825671ceb, which explicitly says "this CHANGES THE ENROL WIRE SHAPE ... do not land it unilaterally"). Landing them independently revises the same contract three times. This task records ONE target shape in DECISIONS.md covering: the enrol request/response fields, the final auth.RosterEntry field set (internal/auth/roster.go:16-37 -- today AgentID/Name/PublicKey/EnrolledAt; it needs a client-cert fingerprint field), and the ordering rule that AUTH-3 (d53e3b21, durable roster) must encode that final field set so the durable record is written once, not migrated. Deliverable is a DECISIONS.md entry ONLY -- do NOT update CONTRACTS-HTTP.md, which documents SHIPPED behaviour, and none of this has shipped. Escalation context: today the roster, sessions and idempotency table are ALL in-memory (internal/auth/roster.go MemoryRoster, internal/auth/service.go:161), so there is currently NOTHING persisted to migrate -- that window closes the moment AUTH-3 lands.
  _Proof: grep -q 'ENROL-SHAPE' DECISIONS.md && grep -q 'invite_token' DECISIONS.md && grep -q 'client_cert_fingerprint' DECISIONS.md && grep -q 'RosterEntry' DECISIONS.md_
- [ ] MTLS-PIN · MTLS-PIN: the client PINS the bus's certificate fingerprint and hard-fails on a change -- never InsecureSkipVerify, and no flag that silently disables verification — security, P0
  EPIC: a1b628fb-8cbf-47e8-9682-034fda8636c7 | DEPENDS ON: MTLS-BUSCERT, MTLS-CLIENTCERT, CLI-1 (0495d133) | BLOCKS: MTLS-VERIFY
  
  CLAUDE.md invariant 11: never disable certificate verification to make something work, and never ship a flag that does it silently -- a bus that looks secure and is not is worse than no TLS. Verification via tls.Config.VerifyPeerCertificate against the pinned SHA-256-of-DER; a changed fingerprint is a hard failure whose error names the remedy. Where the pin comes from is settled by MTLS-DESIGN (planner recommends: carried in the invite blob, which removes the TOFU window). "The trusted path must be the easy path" -- enrol against a fresh bus must work without hand-editing a trust store. DEPENDS ON MTLS-BUSCERT, MTLS-CLIENTCERT, CLI-1.
  _Proof: go test -race -run 'TestClientPinsBusFingerprintAtEnrol|TestClientRefusesChangedBusFingerprint|TestClientHasNoInsecureVerificationFlag' ./client/... && grep -qi 'fingerprint' CONTRACTS-AGENT.md_
- [ ] MTLS-BUSCERT · MTLS-BUSCERT: generate/load the bus's self-signed certificate + private key in the data dir (0600), fatal if unusable — security, P0
  EPIC: a1b628fb-8cbf-47e8-9682-034fda8636c7 | DEPENDS ON: MTLS-DESIGN | BLOCKS: MTLS-LISTENER, MTLS-PIN
  
  New internal/buscert package. Copy the precedent that already exists for a fatal-on-missing-key startup secret: the WAL MAC key (internal/wal/mackey.go:34, mode 0600, decided fatal in DECISIONS.md:1093-1098). Loads AFTER the data-dir lock (cmd/agent-bus/main.go:176) and before the listener (main.go:375); note TestRunRefusesALockedDataDir pins that a lock-refused start touches nothing but bus.lock (main.go:210-217), so the cert step must not run before the lock. Fingerprint is sha256.Sum256(cert.Raw) rendered hex -- the standard construction, not an invention. ESCALATED: this introduces a second long-lived secret in the data dir.
  _Proof: go test -race -run 'TestBusCertGeneratedOnFirstStart|TestBusCertKeyIs0600|TestBusCertFingerprintIsSHA256OfDER' ./internal/buscert && grep -qi 'bus certificate' CONTRACTS-ONDISK.md_
- [ ] None · MSG-FU-SUFFIXFLOOR: resume per-name agent-id suffix counters from disk (agent ids are now durable) — id, P0
  Found by the security gate during the MSG/POLL wave (2026-08-02). cmd/agent-bus/main.go builds ids.NewNameSuffixes() -- a FRESH counter every start -- justified by the comment 'nothing in this path writes an agent id to disk'. THAT PREMISE IS NOW FALSE: store.Record persists sender and recipients as fully-qualified agent ids, hub.publish writes them through the WAL, hub.Apply replays them, and the WAL never compacts. So after a restart the suffix counter restarts at 1 and anyone who enrols the name 'alpha' is minted the id the previous alpha held (invariant 1 broken). CONFIDENTIALITY IS ALREADY CLOSED by the enrolment epoch shipped in the same wave (store.Message.VisibleTo refuses any message sent before the reader enrolled -- proved on a live server: a re-enrolled beta-1 reads 0 of the previous holder's DMs while the message is still in the store), and the reuse is logged at ERROR by hub.NoteEnrolment. WHAT REMAINS is identity continuity: a new keypair holding an id with a prior history, whose future messages are attributed to it. FIX: derive a per-name suffix floor from the highest suffix EVER WRITTEN TO DISK -- parse every sender and recipient seen during replay through ids.ParseAgentID and keep the max per name -- and seed ids.ResumeNameSuffixes with it before the listener binds. internal/hub already collects exactly these ids in Apply (see Hub.recovered), so the derivation belongs there and main passes it to the minter. ALSO correct the now-false justification comment at cmd/agent-bus/main.go:312-317: it is what will make the next reader believe this is safe. AUTH-3 (durable roster) is the complete fix; this is the half that does not depend on it.
  _Proof: go test -race -run TestAgentIDSuffixesResumeAcrossRestart ./internal/ids ./internal/hub_
- [ ] MTLS-VERIFY · MTLS-VERIFY: fix scripts/bus-serve.sh's plaintext health probe AND prove a RUNNING bus is TLS-only and mutually authenticated (committed is not running) — security, P1
  EPIC: a1b628fb-8cbf-47e8-9682-034fda8636c7 | DEPENDS ON: MTLS-LISTENER, MTLS-CLIENTAUTH, MTLS-PIN | BLOCKS: none
  
  Paired committed-vs-running verification per CLAUDE.md. scripts/bus-serve.sh:54 sets HEALTH_URL="http://${LISTEN}/healthz" and curls it at :80 and :161; that is the only surviving bus-*.sh wrapper (AGENTIF-1, done) and it BREAKS the moment MTLS-LISTENER lands, taking every other task's server-startup proof with it. Live assertions required: a plaintext client is refused; a TLS client with NO client certificate is refused; a TLS client with a client certificate and the correct pin reaches /healthz. ALSO FLAG (planner was boundary-blocked from editing them): DEPLOY-1 (fa0c5a4e) and DEPLOY-2 (14f8ec3b) both assume a plaintext listener, and a Compose healthcheck cannot curl plaintext against a TLS-only bus.
  _Proof: go test -race -run 'TestLiveMutualTLSHandshake|TestLivePlaintextRefused' ./cmd/agent-bus && ! grep -q 'HEALTH_URL="http://' scripts/bus-serve.sh_
- [~] None · EPIC: mutual TLS with self-signed certs, no CA -- required transport, no plaintext listener (needs planner pass) — security, P0, in progress
  USER DECISION, 2026-08-02 (DECISIONS.md "Five decisions" #5; CLAUDE.md invariant 11, amended). Supersedes the three-option "BLOCKED ON USER DECISION" framing in 0c8dc0aa-2cc2-4431-bdbf-ec5e44f3c308 -- the user has now DECIDED, that task is being corrected in the same pass to point here rather than sit with a stale open-question framing.
  
  THE DECISION: SELF-SIGNED certificates, MUTUAL TLS, NO certificate authority anywhere. Both ends present and verify a certificate.
  - Trust is established at ENROLMENT (the trust-establishing moment the design already needed): the agents client-certificate fingerprint is bound to its server-minted agent id, and the client pins the buss certificate fingerprint. This reuses the TOFU machinery the design already needs rather than inventing a second trust model -- a bus runs on a laptop with no CA in the picture.
  - mTLS does NOT replace the session token -- BOTH are required, and they do DIFFERENT jobs. mTLS proves which key holder is on the connection; the session token is the revocable, time-bounded application credential -- revocability is exactly what a bare certificate lacks without a CRL.
  - CROSS-CHECK REQUIRED: a session token presented over a connection whose client certificate belongs to a DIFFERENT agent must be REJECTED. This is a stronger property than either mechanism alone and is free once both exist -- do not let one silently substitute for the other.
  - NEW INVARIANT 11 (CLAUDE.md, read in full before design): TLS is the required transport, there is no plaintext listener, and the server REFUSES TO START rather than fall back to plaintext. The loopback default (-listen 127.0.0.1:8080) stays but BOUNDS exposure, it does not replace TLS -- a bus deliberately exposed on a real interface needs both.
  
  NEVER WRITE OUR OWN CRYPTO (CLAUDE.md invariant 9, absolute, outranks stdlib-first). The implementation MUST use Go stdlib crypto/tls for the handshake/transport and an audited library for anything cert-generation-adjacent that crypto/tls itself does not cover -- no hand-rolled handshake, padding, nonce or certificate-parsing logic under any circumstance.
  
  INTERACTIONS TO DESIGN AROUND, NOT ASSUME:
  - Composes with the invite-only-enrolment epic (filed separately, 2026-08-02): the invite is what AUTHORISES binding a NEW client certificate to a NEW agent id in the first place -- invite redemption and cert binding happen together.
  - DEPLOY-1 (fa0c5a4e, Dockerfile) and DEPLOY-2 (14f8ec3b, docker-compose.yml): both currently assume a plaintext listener; cert/key provisioning and the compose healthcheck need to account for TLS (e.g. a healthcheck cannot curl plaintext against a TLS-only listener).
  - The relay plane: invariant 2s cross-bus <bus-id>.<agent-id> addressing and loop-prevention (traversed bus path) must keep working over mTLS bus-to-bus links; every relay hop is now also a certificate-verifying TLS client and server.
  
  NEEDS A PLANNER PASS before implementation: this is an epic, not an atomic task. A planner should break it into atomic tasks covering at minimum: self-signed cert generation + storage for the bus itself, the client-cert generation/storage story per agent, the enrolment-time fingerprint-binding + TOFU pinning flow, the crypto/tls server config (mutual auth required, no plaintext fallback -- refuse to start without valid certs per invariant 11), the session-token/client-cert cross-check, CONTRACTS-HTTP.md + PROTOCOL.md + AGENT_PROTOCOL.md updates, and paired <KEY>-DEPLOY/<KEY>-VERIFY tasks per the committed-vs-running rule since Compose/relay behaviour must be verified live, not just compiled.
  
  Does not yet have atomic sub-tasks; do not claim-next this epic directly -- claim-next the atomic tasks a planner files under it once that pass runs.
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
- [ ] INVITE-STORE · INVITE-STORE: durable single-use invite record (mint/lookup/consume/expire), recovered by WAL replay, with a crash-injection test — auth, P0
  EPIC: 0b43393e-556b-409a-938a-846be2fb4a75 | DEPENDS ON: ENROL-SHAPE | BLOCKS: INVITE-MINT, INVITE-GATE, INVITE-REVOKE
  
  New internal/invite package behind an injected interface, following the existing auth.Roster pattern (internal/auth/roster.go:39-67). Durability is REQUIRED, not optional: if single-use state is in memory only, a restart makes every spent invite redeemable again. Uses the existing two-phase path -- wal.Log.Begin/Txn.Commit (internal/wal/log.go:367, :436) with Entry.Kind = "invite". Entry.Kind is a free-form application discriminator (internal/wal/log.go:78-79), NOT a numbered frame type, so NO record-type reservation is needed and internal/wal/format.go's Type enum is not touched. Record must carry the client-cert fingerprint field DEFINED BUT UNUSED from day one, per ENROL-SHAPE, so MTLS-BIND adds a check rather than a schema change. Per CLAUDE.md, durability code requires a crash-injection test. Note DUR-12 (cbc9ab0c, in flight) changes WAL record framing to an HMAC MAC -- that is below Entry.Kind and does not conflict; do not touch DUR-12.
  _Proof: go test -race -run 'TestInviteStoreRecovery|TestInviteSingleUseSurvivesCrash|TestInviteExpiredIsNotRedeemable' ./internal/invite && grep -qi 'invite record' CONTRACTS-ONDISK.md_
- [ ] None · Enrol accepts a duplicate enrolment public key -- one keypair can hold unlimited agent ids — auth, P2
  Found by the security gate on AUTH-1-FU-ACTIVECAP, verified empirically (three enrolments with a byte-identical public key were all accepted, minting alpha-1/-2/-3, after which ONE private key held 3x the per-agent active-session cap). Service.Enrol validates the public key's LENGTH but never checks whether that key is already enrolled against another agent id, and the Roster interface offers no by-key lookup to do so. Two consequences. First, it is the direct reason AUTH-1-FU-ACTIVECAP raises the flood cost by only ~1.6%: the "512 distinct enrolments" the cap forces are 512 unauthenticated POSTs from ONE keypair, not 512 identities an attacker must obtain. Second, it makes key->identity one-to-many where several planned features assume one-to-one: the invite-only + self-signed-mTLS design (invariant 11) binds a client-certificate fingerprint to an agent id at enrolment, AUTH-4 revocation is naturally expressed per key, and a roster listing two agents with identical keys is a spoofing surface. The right answer is NOT obviously "reject" -- refusing a duplicate leaks "this key is already enrolled" to an unauthenticated caller, which is its own oracle. Needs a recorded DECISIONS.md decision (reject / allow-and-document / defer to the invite gate, which would moot it) rather than a silent code change.
- [x] None · LISTENADDR-FU-CONTRACTS: CONTRACTS.md CLI-flag table still shows -listen default :8080 — docs, P1
  The AUTH-1-FU-LISTENADDR change (task c27f9439-c821-4d86-9e92-bac352ec1fd3) changes defaultListen in cmd/agent-bus/main.go from ":8080" to a loopback address (DECISIONS.md settled on localhost as the correct default), but its agent was DELIBERATELY denied ownership of CONTRACTS.md this loop because another agent (AUTH-1-FU-PENDINGCAP) holds that file -- CONTRACTS.md is single-writer by project rule this loop. CONTRACTS.md CLI flags (cmd/agent-bus) table (currently around line 52: `| -listen | :8080 | TCP address to bind, e.g. :8080 or 127.0.0.1:8080 |`) therefore still documents the old :8080 default and must be corrected once the code change lands. The LISTENADDR agent was instructed to report the exact replacement text in its own task journal (c27f9439) -- check there first; as of this filing (2026-08-02 ~19:43 UTC) that task had posted no notes yet (still in flight), so the exact replacement wording is not yet available and must be pulled from that tasks journal once it reports. This is tracked doc debt, deliberately incurred by the file-ownership boundary, not an oversight.
  _Proof: grep -qE "^\| \`-listen\` \| \`127\.0\.0\.1:8080\` \|" CONTRACTS-CLI.md && echo ALL_OK_
- [ ] MTLS-BIND · MTLS-BIND: enrolment binds the presenting client-cert fingerprint to the SERVER-MINTED agent id -- the invite is what authorises the binding — security, P0
  EPIC: a1b628fb-8cbf-47e8-9682-034fda8636c7 | DEPENDS ON: MTLS-CLIENTAUTH, ENROL-SHAPE, INVITE-GATE | BLOCKS: MTLS-CROSSCHECK, AUTH-3 (d53e3b21)
  
  DECISIONS.md:1146 -- the invite authorises binding a new client certificate to a new agent id; the two happen together, not as two independent gates either of which alone would suffice. Populates the fingerprint field that INVITE-STORE and ENROL-SHAPE reserved, on auth.RosterEntry (internal/auth/roster.go:16-37). INVARIANT 1: the certificate supplies a fingerprint and NOTHING else -- it must not influence the agent id, the name, or the suffix, which are minted by ids.AgentIDMinter.Mint (internal/ids/agentmint.go:360). auth.Roster.Put already refuses a duplicate AgentID rather than overwriting (internal/auth/roster.go:105-107); the same refuse-never-overwrite rule must apply to a fingerprint already bound to a different agent. ORDERING: land before AUTH-3 (d53e3b21, durable roster) or AUTH-3 encodes a durable record that immediately needs migrating.
  _Proof: go test -race -run 'TestEnrolBindsClientCertFingerprint|TestEnrolRejectsAlreadyBoundFingerprint|TestClientCertCannotInfluenceAgentID' ./internal/auth ./internal/httpapi && grep -qi 'fingerprint' CONTRACTS-HTTP.md_
- [-] None · [RESOLVED 2026-08-02 -- SUPERSEDED] CRC32C tail-repair proofs are remotely forgeable => permanent refuse-to-start, no operator override — durability, P1
  RESOLVED. THE USER ANSWERED BOTH HALVES OF THIS ESCALATION ON 2026-08-02, AND EACH ANSWER KILLS THE
  FINDING BY A DIFFERENT ROUTE. Superseded rather than done, because nothing was implemented here.
  
  This task asked three questions. All three are answered:
  
   (1) "Does an operator override belong here at all?" -- MOOT. The bus ALWAYS restarts (DECISIONS.md,
       2026-08-02, section 1): damaged records are discarded, logged loudly and specifically, and the
       server keeps running. There is no permanent refuse-to-start state left to override. The decision
       says so in terms: "This also removes the permanent-refuse-to-start DoS, and with it the need for
       the operator escape hatch that was previously recommended: always-restart *is* the escape hatch."
       => carried by DUR-11 (884d3da4), in flight.
  
   (2) "Is the right fix instead upstream -- authenticate WAL frames?" -- YES, DECIDED, and it is the
       chosen fix. Section 3: CRC32C is replaced by an HMAC-SHA256 keyed MAC, precisely because
       "CRC32C is an error-detecting code, not an integrity primitive: it is unkeyed and GF(2)-linear,
       which is precisely why security demonstrated that an ordinary remote client could craft a payload
       making a torn tail look like a complete record. A keyed MAC eliminates that attack by
       construction -- a client cannot compute a MAC over a key it does not hold."
       => carried by DUR-12 (reserved ondisk-format-version=2), BLOCKED on where the MAC key lives.
  
   (3) "Is a self-inflicted DoS via one's own future crash an acceptable trust boundary?" -- MOOT for
       the same reason as (1): under always-restart there is no denial of service to inflict.
  
  THE ONE THING THAT SURVIVES, and it is DUR-12's, not this task's: a key stored beside the WAL defends
  against the REMOTE CLIENT in this finding but NOT against an attacker who already has
  data-directory write access. That residual is stated in the decision and is DUR-12's open blocker.
  
  DO NOT PICK THIS UP. Work DUR-11 and DUR-12.
  
  --- ORIGINAL ESCALATION retained below for the mechanism, which is a good record of how the finding
  --- was constructed (Gaussian elimination over the 32 CRC columns, printable-ASCII JSON payload). See
  --- the DUR-10 security kind=response of 2026-08-02T15:23 for the end-to-end reproduction.
  _Proof: go run ./cmd/agent-bus -h 2>&1 | grep -qiE "repair|force-start|allow-corrupt-tail"_
- [ ] MTLS-CLIENTAUTH · MTLS-CLIENTAUTH: require a client certificate on every connection WITHOUT a CA -- RequireAnyClientCert plus application-layer policy, never InsecureSkipVerify — security, P0
  EPIC: a1b628fb-8cbf-47e8-9682-034fda8636c7 | DEPENDS ON: MTLS-DESIGN, MTLS-LISTENER | BLOCKS: MTLS-BIND, MTLS-CROSSCHECK, MTLS-VERIFY, MTLS-RELAYGUARD
  
  THE LOAD-BEARING SUBTLETY, stated here so it is not discovered by accident. With no CA, tls.RequireAndVerifyClientCert is unusable -- it would need ClientCAs and would reject every client. So the handshake must use tls.RequireAnyClientCert and authorise NOTHING at handshake time; the policy decision moves to the application layer via VerifyConnection/VerifyPeerCertificate plus a middleware. That produces a deliberate asymmetry: the enrolment route MUST accept a cert it has never seen (accepting it is how binding happens), while every other route requires a cert already bound to an agent. internal/httpapi has zero transport knowledge today, so the peer cert must be plumbed from r.TLS through a middleware using the existing ctxKey pattern (internal/httpapi/middleware.go:31, authmw.go:86; next free value is 2). Also ship a permanent guard test that no InsecureSkipVerify exists on any reachable path.
  _Proof: go test -race -run 'TestHandshakeRequiresClientCert|TestUnknownClientCertReachesEnrolOnly|TestNoInsecureSkipVerifyAnywhere' ./internal/httpapi ./cmd/agent-bus_
- [x] None · scripts/spec-cloud.sh leaks SPEC_CLOUD_PASSWORD on the `aws` argv (readable via /proc/*/cmdline) — tooling, P2
  PRE-EXISTING, and outside the CORE-1..4 change wave -- filed here because the reviewer/security pass over that wave noticed it, not because that wave introduced it. scripts/spec-cloud.sh authenticates to Cognito by passing `PASSWORD=$SPEC_CLOUD_PASSWORD` as an element of the `aws` command's ARGV. Process arguments are world-readable on Linux via /proc/<pid>/cmdline, so for the lifetime of that aws invocation ANY local user on the box can read the plaintext Spec Server password -- a plain `ps auxww` or a tight loop over /proc is enough. It may also land in shell history, audit logs, or a process accounting record.
  
  Fix: keep the secret off argv. Options, best first: (a) feed the auth parameters via `--cli-input-json file:///dev/stdin` (or a 0600 temp file in a private dir, removed with a trap) so the password travels on stdin/in a file rather than the command line; (b) use an aws-cli mechanism that reads the value from the environment. Note that the ENVIRONMENT is better than argv but not perfect -- /proc/<pid>/environ is readable by the same UID -- so prefer stdin. Keep the credentials file itself outside the repo where it already correctly lives (/mnt/sdc/mike/claude-scratch/spec-cloud-creds.env), and check its permissions are 0600 while you are there.
  
  Verify by running the wrapper and confirming a concurrent `ps auxww | grep -c <password>` finds nothing (use a throwaway value to test, never the real one). This touches the tooling every agent uses to reach the Spec Server, so change it carefully and confirm `bash scripts/spec-cloud.sh -sf /readyz` still succeeds and a cached token still refreshes on 401.
  _Proof: grep -q -- "--cli-input-json" scripts/spec-cloud.sh && ! grep -q "PASSWORD=\$SPEC_CLOUD_PASSWORD" scripts/spec-cloud.sh_
- [ ] INVITE-HARDEN · INVITE-HARDEN: constant-time invite-secret comparison and ONE indistinguishable failure response for unknown/expired/revoked/already-consumed — security, P1
  EPIC: 0b43393e-556b-409a-938a-846be2fb4a75 | DEPENDS ON: INVITE-GATE | BLOCKS: none
  
  Mirrors the existing deliberate indistinguishability of the 401 and 404 surfaces (CONTRACTS-HTTP.md:19, :235-239) -- distinguishing the four invite failure modes is an enumeration oracle. Comparison uses stdlib crypto/subtle.ConstantTimeCompare. INVARIANT 9: do not hand-roll a comparison, a hash, or a token format; if any part of this looks like inventing a scheme, stop and escalate.
  _Proof: go test -race -run 'TestInviteRedeemFailuresIndistinguishable|TestInviteSecretComparedInConstantTime' ./internal/httpapi ./internal/invite_
- [ ] INVITE-REVOKE · INVITE-REVOKE: durably revoke an un-redeemed invite, and state what revocation does to an agent that already redeemed one — auth, P1
  EPIC: 0b43393e-556b-409a-938a-846be2fb4a75 | DEPENDS ON: INVITE-STORE, INVITE-GATE | BLOCKS: none
  
  Revocation must survive restart (same durable store as INVITE-STORE). BLOCKED ON THE ESCALATED DECISION: does revoking an invite cascade to the agent that already redeemed it and kill its live sessions (requires AUTH-4 leave/revocation, a853261d), or is an invite simply spent at redemption so revocation only affects un-redeemed invites? Whichever the user picks, this task must state it explicitly in CONTRACTS-HTTP.md -- silence here is the failure mode.
  _Proof: go test -race -run 'TestInviteRevokedCannotBeRedeemed|TestInviteRevocationSurvivesRestart' ./internal/invite && grep -qi 'revocation' CONTRACTS-HTTP.md_
- [ ] None · Whole-log quarantine reissues EVERY sequence number ever minted -- startup must refuse, not resume from 1 (invariant 1, second instance) — durability, P0
  THE DEFECT: internal/wal/recover.go:252-262 -- when the entire WAL log is quarantined as corrupt, recovery starts a FRESH log at index 1. No PREPARE record survives anywhere in the log. The message-sequence high-water mark derived at startup is therefore 0, and a bus that then mints sequences from 1 REISSUES EVERY SEQUENCE NUMBER IT HAS EVER USED -- not one index at a damaged tail, but the bus's entire history. Nothing downstream can detect this; it is silent.
  
  This is INVARIANT 1 (CLAUDE.md: 'ids are never reused ... including across restarts') and the user's ruling stands WITHOUT narrowing (commit 4110946; DECISIONS.md 'Five decisions...' section 3; amended invariant 1 in CLAUDE.md). That ruling was made about the tail-salvage reissue tracked on e120153b-9d8a-4b6a-bd4e-89431954496b. THIS TASK IS A SEPARATE, STRICTLY WORSE INSTANCE OF THE SAME VIOLATION and is explicitly NOT covered by e120153b and NOT a duplicate of it: e120153b is about reissuing one index at a damaged tail; this is about the WHOLE log vanishing and the floor silently becoming 0.
  
  REQUIRED BEHAVIOUR (fail-closed, proposed): startup must distinguish 'legitimately empty -- a brand-new bus that has never minted anything' from 'quarantined -- the high-water mark is UNKNOWN because the log that would prove it was discarded as corrupt'. In the second case startup must REFUSE TO START rather than silently resuming from 1. This is a deliberate, narrow exception to the always-restart rule -- in the same family as the user's decision that a missing/wrong MAC key is FATAL. Always-restart exists to stop media damage holding the bus hostage; it does not exist to license silently reissuing every id the bus ever minted.
  
  *** CONSENT-SENSITIVE -- CONFIRM WITH THE USER BEFORE IMPLEMENTATION ***. The user reverted a broader refuse-to-start policy once already. The fail-closed DIRECTION follows from invariant 1 as ruled, but the specific mechanism (what marks 'legitimately empty' vs 'quarantined-unknown', where that marker is durably recorded, and the exact refuse-to-start condition) needs explicit sign-off before code is written, not just inferred from the earlier ruling on the tail-salvage case.
  
  CROSS-REFERENCES:
  - e120153b-9d8a-4b6a-bd4e-89431954496b -- the tail-salvage reissue defect at the same invariant, different site (damaged tail, not whole-log quarantine). This task is NOT a duplicate; do not merge them.
  - 8c9b6489-abb1-444e-9eeb-3ff87646f632 (ID-2-WIRING-SEAL, landing now) -- provides the machinery this needs: Seal()/ErrFloorUnproven, where a sequence allocator is born UNSEALED and Next() refuses to issue until the floor is proven. Its in-tree doc comment already states the requirement verbatim: the floor must be derived 'from the highest sequence number EVER WRITTEN TO DISK -- every prepare, committed, aborted and dangling alike'. The whole-log-quarantine case is PRECISELY the case where that derivation is impossible -- so the seal must never be taken, and Next() must keep refusing (ErrFloorUnproven), which is the mechanism this task should most likely use to implement the refuse-to-start behaviour rather than inventing a new one.
  - c31f6999-da4e-400d-ab55-178b82e2a42e (ID-2-WIRING-OBSERVER) and 838677e6-d424-45ed-8580-924cb2da28a6 (ID-2-WIRING) -- the floor-derivation machinery this interacts with. The ID-2-WIRING-SCHEMA agent (80b54ee4-55d5-44b8-a479-c0a13343d15a) recorded this whole-log-quarantine case as a required fail-closed behaviour of what it called 'ID-2-WIRING-STARTUP' while choosing Option A'.
  
  ORDERING: lives in internal/wal, which DUR-12 (cbc9ab0c-3b34-48d0-acd8-5eabd4dc4a02, in_progress -- CRC32C to HMAC-SHA256 MAC swap, on-disk format v2) owns this loop right now. Do NOT dispatch/implement until DUR-12 lands -- same ordering constraint that applies to e120153b.
  
  PROOF_CMD VERDICT (recorded now, pre-implementation): `bash scripts/proof-check.sh 'go test -race -run TestRecover_WholeLogQuarantine_RefusesStartOnUnprovenSequenceFloor ./internal/wal'` returns verdict=VACUOUS (exit 4, 'no tests to run', empty_pkgs=1) because the test does not exist yet -- this is expected and is recorded here explicitly so nobody mistakes an unwritten-test 0-exit for a pass. DO NOT complete this task on a VACUOUS proof; the test must be written (quarantine a log holding sequences up to N, restart, assert the bus REFUSES TO START rather than minting from 1) and proof-check must report PASS before this is marked done.
  _Proof: go test -race -run TestRecover_WholeLogQuarantine_RefusesStartOnUnprovenSequenceFloor ./internal/wal_
- [-] COMMIT-HYGIENE-MIXED-22E8EB6 · COMMIT-HYGIENE-PRACTICE-NOTE: standing practice -- git commit should carry an explicit pathspec (accusation against 22e8eb6 was FALSE, see corrected note) — process, P2, cancelled
  CORRECTED 2026-08-02 (main/orchestrator). This task originally accused a "concurrent agent" of running a bare git commit that swept unrelated DUR-12 files into commit 22e8eb6. THAT ACCUSATION IS FALSE and has been verified directly: git log -1 --format="%H %an <%ae> %s" 22e8eb6 shows the sole author is "mike <dodgymike@gmail.com>" -- the user, and the ONLY committer on this project. No agent committed anything; there was no concurrent-agent bare git commit incident. This is the same misreading-user-commits-as-rogue-agent failure that has burned this project before (one prior agent even withheld work over it) -- see DECISIONS.md "Commit history: LEAVE IT" (2026-08-02), which the user already ruled on: the history stands as-is, nothing to fix, no rewrite.
  
  The ONE genuinely useful residue, KEPT here as a standing practice note (not a bug report): when several agents concurrent stage work in the same working tree, a plain "git commit" with no pathspec takes the WHOLE index, not just the files the committing party intended. So the standing rule -- already reflected in this repos own git-commit guidance -- is: git commit should be given an explicit pathspec / reviewed staged diff before committing, never a bare git commit -A or blind git add -A, when multiple agents may have staged files concurrently. No corrective action needed beyond this note; there is no incident to remediate.
- [-] ZZ-LOCKTEST · ZZ-LOCKTEST: verify If-Match CAS — process, P3, cancelled
  throwaway; verifies the triage mutex protocol
- [ ] None · Stale CONTRACTS.md pointers after the CONTRACTS-SPLIT: README.md:88, AGENT_PROTOCOL.md:122, CLAUDE.md:332 — documentation, P2
  Discovered by the CONTRACTS-SPLIT agent (360a2679, 2026-08-02) while splitting CONTRACTS.md into per-plane files (CONTRACTS-CLI/HTTP/ONDISK/AGENT.md, with CONTRACTS.md left as an index). That agent flagged but could not fix these -- outside its file-ownership boundary for that pass:
  
  1. README.md:88 -- `- [`CONTRACTS.md`](./CONTRACTS.md) — every route, flag, env var, and record type` still claims CONTRACTS.md directly HOLDS that table. It does not any more; it is now a short index pointing at the four plane files. Fix: reword to describe it as the index, and/or link the plane files directly.
  
  2. AGENT_PROTOCOL.md:122 -- `... see `CONTRACTS.md`, `## Authentication`) ...` cites a specific heading, `## Authentication`, inside CONTRACTS.md. That heading no longer exists there -- it moved verbatim to CONTRACTS-HTTP.md:192 (`## Authentication (added 2026-08-02)`) in the split. Fix: repoint the citation to CONTRACTS-HTTP.md.
  
  3. CLAUDE.md:332 (Parallel-agent coordination section) -- `- For the remaining shared files (`DECISIONS.md`, `AGENT_LOG.md`, `CONTRACTS.md`), only ONE agent at a time; prefer adding a new dated section over editing existing lines.` This is actively MISLEADING post-split: naming CONTRACTS.md alongside DECISIONS.md/AGENT_LOG.md as a single-writer-contended file is exactly the chokepoint the split (360a2679) existed to remove -- three P0s across two triage loops were caused by concurrent agents needing to land a doc update in that one file. Leaving this warning in place would keep agents needlessly serialising on a file that no longer holds the contended content (CONTRACTS.md is now a stable ~36-line index; the actual content lives in CONTRACTS-CLI/HTTP/ONDISK/AGENT.md, each independently editable). Fix: remove CONTRACTS.md from this single-writer list (the plane files still need their own single-writer discipline if a task touches more than one at once, but that is a materially different, narrower risk than the old whole-file chokepoint).
  
  NOTE: CLAUDE.md line ~158 (repository-layout section) and step 9 were ALREADY updated by the split agent to name CONTRACTS.md as INDEX only -- this task is only the three residual pointers above, do not re-touch line 158.
  
  PROOF STRENGTHENED 2026-08-02 (spec-keeper): the original proof_cmd was three negative assertions only, which is satisfiable by DELETING the three stale lines rather than fixing them (the same structural flaw fixed on 5b178dde) -- it now also requires positive evidence that each file points at the correct replacement (README.md cites CONTRACTS-HTTP.md/CONTRACTS-CLI.md/CONTRACTS-ONDISK.md, AGENT_PROTOCOL.md cites CONTRACTS-HTTP.md, and CLAUDE.md's "remaining shared files" bullet now names a CONTRACTS-*.md plane file instead of just dropping CONTRACTS.md from the list).
  _Proof: grep -qF "CONTRACTS-HTTP.md" README.md && grep -qF "CONTRACTS-CLI.md" README.md && grep -qF "CONTRACTS-ONDISK.md" README.md && ! grep -qF ") — every route, flag, env var, and record type" README.md && grep -qF "CONTRACTS-HTTP.md" AGENT_PROTOCOL.md && ! grep -qF "see `CONTRACTS.md`, `## Authentication`" AGENT_PROTOCOL.md && grep -A2 "remaining shared files" CLAUDE.md | grep -qF "CONTRACTS-" && ! grep -qF "For the remaining shared files (`DECISIONS.md`, `AGENT_LOG.md`, `CONTRACTS.md`), only ONE agent at" CLAUDE.md_
- [ ] INVITE-PEERGUARD · INVITE-PEERGUARD: no ungated peer/federation enrolment path may ever exist -- enumerate the routes and assert it — security, P1
  EPIC: 0b43393e-556b-409a-938a-846be2fb4a75 | DEPENDS ON: INVITE-GATE | BLOCKS: RELAY-1 (9bc9d6c4), MTLS-RELAYGUARD
  
  The user's decision says redemption is the only route onto the bus INCLUDING for peer buses. internal/relay/ is a 9-line doc.go stub today (internal/relay/doc.go:8) and no peer route exists, so the landable increment now is the GUARD, not the feature: a test that walks (*Server).Routes() (internal/httpapi/server.go, the same enumeration TestEveryRouteRequiresAuth uses) and the five-entry allow-list (internal/httpapi/authmw.go:57-63) and fails if any peer/federation/relay-enrolment path is reachable without invite redemption. RELAY-1 (9bc9d6c4) must satisfy this guard rather than route around it; record that as an acceptance criterion in this task's own description (the planner was not permitted to edit RELAY-1).
  _Proof: go test -race -run 'TestNoUnauthenticatedPeerEnrolRoute|TestAllowListIsExactlyTheFiveKnownPaths' ./internal/httpapi && grep -qi 'peer' CONTRACTS-HTTP.md_
- [ ] None · MSG-FU-ROSTERSOURCE: the hub must read the AUTHORITATIVE roster the moment AUTH-3 makes enrolment durable — core, P1
  internal/hub keeps its OWN roster view, fed by internal/httpapi/auth.go calling hub.NoteEnrolment on every accepted enrolment. That is honest TODAY only because the two views have identical lifetimes: auth.MemoryRoster is in memory only and lost on restart, and so is the hub's. auth.Roster exposes Put/Get/Len and NO listing, and internal/auth was outside the MSG/POLL wave's ownership, which is why it was done this way. THE DAY AUTH-3 LANDS THIS BECOMES A LANDMINE: auth's roster survives a restart while the hub's starts empty, so sessions authenticate fine but hub.publish returns 403 (ErrUnknownSender) for every send, 404 for every recipient, and both read paths fail closed with ErrUnknownSender -- a bus that authenticates everyone and serves nobody. FIX: add List (or an iterator) to auth.Roster and auth.MemoryRoster, inject it into hub.Options, and delete NoteEnrolment together with its call site in handleEnroll. CRITICAL DETAIL: the enrolment epoch (store.Message.VisibleTo) reads Agent.EnrolledAt, so the durable roster MUST carry each agent's ORIGINAL enrolment instant. With it, a genuinely continuous agent keeps seeing everything sent since it enrolled, which is exactly the behaviour the epoch was designed to preserve. MUST land in the same change as AUTH-3, never after it.
  _Proof: go test -race -run TestHubReadsTheDurableRoster ./internal/hub_
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
### EPIC AGENTIF — Agent-facing surface (shell wrappers + protocol doc)

- [-] AGENTIF-2 · AGENTIF-2: scripts/bus-enrol.sh + AGENT_PROTOCOL.md entry — agentif, P0
  SUPERSEDED 2026-08-02 -- THE GO CLI REPLACES THE SHELL WRAPPERS.
  
  User decision (DECISIONS.md, 2026-08-02): *"the go cli should take the place of the .sh files and be
  easy to use for a human + friendly for an agent to use or embed"*, and "Merge the CLI and AGENTIF
  epics". Invariant 7 is AMENDED, not weakened -- nobody hand-writes HTTP, but the vehicle is a CLI
  subcommand, not `scripts/bus-*.sh`.
  
  This task's enrolment work is carried by **CLI-2**. Do not write the shell wrapper.
  
  --- ORIGINAL DESCRIPTION ---
  Wrapper for POST /v1/enroll -- generates/loads local key material, submits it, stores the returned token+agent-id locally for subsequent wrapper calls. Pairs with the enroll-endpoint task; per invariant 7 they ship together.
  _Proof: scripts/bus-enrol.sh --name testagent_
- [-] AGENTIF-6 · AGENTIF-6: scripts/bus-wait.sh + AGENT_PROTOCOL.md entry — agentif, P1
  SUPERSEDED 2026-08-02 -- THE GO CLI REPLACES THE SHELL WRAPPERS.
  
  User decision (DECISIONS.md, 2026-08-02): *"the go cli should take the place of the .sh files and be
  easy to use for a human + friendly for an agent to use or embed"*, and "Merge the CLI and AGENTIF
  epics". Invariant 7 is AMENDED, not weakened -- nobody hand-writes HTTP, but the vehicle is a CLI
  subcommand, not `scripts/bus-*.sh`.
  
  This task's long-poll wait work is carried by **CLI-3**. Do not write the shell wrapper.
  
  --- ORIGINAL DESCRIPTION ---
  Wrapper for GET /v1/wait, looping the cursor forward across calls and printing new messages as they arrive. Pairs with the long-poll endpoint task; per invariant 7 they ship together.
  _Proof: scripts/bus-wait.sh --timeout 5_
- [-] AGENTIF-8 · AGENTIF-8: scripts/bus-peer.sh + AGENT_PROTOCOL.md entry — agentif, P2
  SUPERSEDED 2026-08-02 -- THE GO CLI REPLACES THE SHELL WRAPPERS.
  
  User decision (DECISIONS.md, 2026-08-02): *"the go cli should take the place of the .sh files and be
  easy to use for a human + friendly for an agent to use or embed"*, and "Merge the CLI and AGENTIF
  epics". Invariant 7 is AMENDED, not weakened -- nobody hand-writes HTTP, but the vehicle is a CLI
  subcommand, not `scripts/bus-*.sh`.
  
  This task's peer add/list/remove work is carried by **CLI-7**. Do not write the shell wrapper.
  
  --- ORIGINAL DESCRIPTION ---
  Wrapper for the peer-enrolment handshake (add/list/remove a peer bus). Pairs with the peer-enrolment task; per invariant 7 they ship together.
  _Proof: scripts/bus-peer.sh add http://peer-host:8081_
- [-] None · AGENTIF-9: Envelope/schema validation in scripts/bus-*.sh before accepting a server response — agentif, P1, cancelled
  Origin: user instruction 2026-08-02, "add a mechanism to validate messages in the agent script before accepting them" -- split into two layers. CRYPTO-10 covers the CRYPTO-verification layer (MAC/decrypt, wired in once the CRYPTO epic lands). THIS task covers the layer underneath that and independent of it: basic envelope/schema validation of what a shell wrapper accepts from the server BEFORE it hands the payload to the calling agent, needed from day one (AGENTIF-3/4/5/6/7/8 all parse server JSON today with no such check specified).
  
  A shell wrapper (bash + jq/curl) that trusts server JSON blindly is fragile and, on a compromised/misbehaving/relay-hopped bus (invariant 2: multiple buses relay to each other -- a message may have crossed a bus you don't directly trust), a foot-gun: a malformed or unexpected-shaped response fed straight into `msg=$(...)`, `eval`, or interpolated into a follow-up curl call can corrupt state or worse. Scope, for every scripts/bus-*.sh wrapper that consumes a server response (bus-agents.sh, bus-broadcast.sh, bus-send.sh, bus-wait.sh, bus-leave.sh, bus-peer.sh):
  - Validate the response is well-formed JSON before doing anything else with it (a wrapper must not treat a non-2xx or non-JSON body as if it were data).
  - Validate the expected top-level shape/required fields are present and are the expected JSON type (e.g. `id` is a string, `messages` is an array) before extracting and printing/using any field -- reject with a clear non-zero exit and a stderr message on anything else, printing nothing usable to stdout on failure (same "fail loud, fail closed" contract CRYPTO-10 uses for the crypto layer, so the two layers compose instead of conflicting).
  - Cap/guard against absurd sizes (a pathological huge response should not be slurped unbounded into a bash variable).
  - Document the validation contract (accepted shape, exit codes) in AGENT_PROTOCOL.md per invariant 7 -- ships in the same task as the wrapper behaviour it documents.
  
  Does NOT cover cryptographic verification, decryption, or replay/sender-identity checks -- that is CRYPTO-10, layered on top of this once it lands. This task is not gated on the CRYPTO epic and should land first since every wrapper needs it regardless of whether E2E crypto is ever enabled.
  _Proof: bash scripts/bus-wait.sh (against a throwaway server) fed a malformed/truncated response -- exits non-zero, prints nothing usable to stdout_
- [x] AGENTIF-1 · AGENTIF-1: scripts/bus-serve.sh + AGENT_PROTOCOL.md entry — agentif, P0
  Wrapper to start/stop/status a local agent-bus server (foreground or backgrounded with a pidfile) plus its AGENT_PROTOCOL.md section. Pairs with the main-entrypoint task -- needed first since every other wrapper assumes a running server to talk to. Per invariant 7 the wrapper and doc entry land in the SAME task/commit as the feature it fronts.
  _Proof: scripts/bus-serve.sh start && scripts/bus-serve.sh status && scripts/bus-serve.sh stop_
- [-] AGENTIF-7 · AGENTIF-7: scripts/bus-leave.sh + AGENT_PROTOCOL.md entry — agentif, P1
  SUPERSEDED 2026-08-02 -- THE GO CLI REPLACES THE SHELL WRAPPERS.
  
  User decision (DECISIONS.md, 2026-08-02): *"the go cli should take the place of the .sh files and be
  easy to use for a human + friendly for an agent to use or embed"*, and "Merge the CLI and AGENTIF
  epics". Invariant 7 is AMENDED, not weakened -- nobody hand-writes HTTP, but the vehicle is a CLI
  subcommand, not `scripts/bus-*.sh`.
  
  This task's leave / logout work is carried by **CLI-2**. Do not write the shell wrapper.
  
  --- ORIGINAL DESCRIPTION ---
  Wrapper for POST /v1/leave, clearing the locally stored token afterward. Pairs with the leave/revocation task; per invariant 7 they ship together.
  _Proof: scripts/bus-leave.sh_
- [-] AGENTIF-3 · AGENTIF-3: scripts/bus-agents.sh + AGENT_PROTOCOL.md entry — agentif, P1
  SUPERSEDED 2026-08-02 -- THE GO CLI REPLACES THE SHELL WRAPPERS.
  
  User decision (DECISIONS.md, 2026-08-02): *"the go cli should take the place of the .sh files and be
  easy to use for a human + friendly for an agent to use or embed"*, and "Merge the CLI and AGENTIF
  epics". Invariant 7 is AMENDED, not weakened -- nobody hand-writes HTTP, but the vehicle is a CLI
  subcommand, not `scripts/bus-*.sh`.
  
  This task's roster listing work is carried by **CLI-5**. Do not write the shell wrapper.
  
  --- ORIGINAL DESCRIPTION ---
  Wrapper for GET /v1/agents. Pairs with the roster-listing task; per invariant 7 they ship together.
  _Proof: scripts/bus-agents.sh_
- [-] AGENTIF-4 · AGENTIF-4: scripts/bus-broadcast.sh + AGENT_PROTOCOL.md entry — agentif, P1
  SUPERSEDED 2026-08-02 -- THE GO CLI REPLACES THE SHELL WRAPPERS.
  
  User decision (DECISIONS.md, 2026-08-02): *"the go cli should take the place of the .sh files and be
  easy to use for a human + friendly for an agent to use or embed"*, and "Merge the CLI and AGENTIF
  epics". Invariant 7 is AMENDED, not weakened -- nobody hand-writes HTTP, but the vehicle is a CLI
  subcommand, not `scripts/bus-*.sh`.
  
  This task's broadcast work is carried by **CLI-4**. Do not write the shell wrapper.
  
  --- ORIGINAL DESCRIPTION ---
  Wrapper for POST /v1/broadcast. Pairs with the broadcast task; per invariant 7 they ship together.
  _Proof: scripts/bus-broadcast.sh "hello bus"_
- [-] AGENTIF-5 · AGENTIF-5: scripts/bus-send.sh + AGENT_PROTOCOL.md entry — agentif, P1
  SUPERSEDED 2026-08-02 -- THE GO CLI REPLACES THE SHELL WRAPPERS.
  
  User decision (DECISIONS.md, 2026-08-02): *"the go cli should take the place of the .sh files and be
  easy to use for a human + friendly for an agent to use or embed"*, and "Merge the CLI and AGENTIF
  epics". Invariant 7 is AMENDED, not weakened -- nobody hand-writes HTTP, but the vehicle is a CLI
  subcommand, not `scripts/bus-*.sh`.
  
  This task's direct message work is carried by **CLI-4**. Do not write the shell wrapper.
  
  --- ORIGINAL DESCRIPTION ---
  Wrapper for POST /v1/send (DM). Pairs with the direct-message task; per invariant 7 they ship together.
  _Proof: scripts/bus-send.sh <agent-id> "hello"_

### EPIC AUTH — Enrolment & authentication

- [ ] None · AUTH-1-FU-ACTIVECAP-RETRYAFTER: a per-agent cap 503 tells the client the wrong thing and the wrong Retry-After — httpapi, P1
  NOTE: this proof_cmd is VACUOUS today by construction -- TestSessionCapRetryAfter does not exist yet; writing it is part of this task. Found by the reviewer gate on AUTH-1-FU-ACTIVECAP. internal/httpapi/auth.go:48-52 documents `capacityRetryAfterSeconds = "5"` on the premise that "every capacity limit in internal/auth is a live, in-memory bound that a departing agent or an expiring session can relieve within seconds". That premise is now FALSE. The new per-agent ACTIVE-session cap reaches the same mapping (auth.go:225 -> writeAuthError -> auth.go:289) but persists until one of the agent's OWN sessions expires -- up to SessionLifetime, one hour. A genuine agent at its own cap receives 503 {"error":"server at capacity, retry later"} with Retry-After: 5: it blames the SERVER for a client-side condition, and the retry advice is wrong by up to three orders of magnitude (5s vs 3600s). A conforming client honouring Retry-After polls ~720 times over the hour while its operator diagnoses a bus outage that is not happening. Fix the SURFACE, not the cap value 32: a distinct sentinel (e.g. ErrAgentSessionCapacity) or at minimum a distinct terse client message and a Retry-After derived from the agent's own soonest-expiring session. Note the disclosure constraint the security gate confirmed: the branch is unreachable without the agent's private key, so distinguishing it to THAT caller is not an oracle. Out of scope for AUTH-1-FU-ACTIVECAP, whose boundary was internal/auth only.
  _Proof: go test -race -run TestSessionCapRetryAfter ./internal/httpapi_
- [ ] None · AUTH-1-FU-SESSIONSCALE: session-table O(n) scans and refuse-not-evict policy cause CPU/lock amplification — auth, P1
  Follow-up to AUTH-1 (public_id 54fa94c0-6ca3-459a-aaa2-a3ea047f97d9). Security measured roughly 1.04 ms of exclusive global-mutex time per ~180-byte request at default caps with a full table (a ~960 req/s ceiling for the WHOLE auth surface), caused by the O(n) sweepLocked / countPendingLocked / oldestPendingLocked scans over a 16384-entry table, all held under sessMu. Separately, MaxSessions currently REFUSES new session-begins rather than evicting the globally-oldest PENDING session, so once the table fills, every legitimate agent is denied until entries time out -- an unauthenticated flooder can hold the table full indefinitely. Fix: replace the O(n) scans with an amortized structure (e.g. a min-heap or a periodically-swept ring keyed on expiry) and change the full-table policy to evict-oldest-pending rather than refuse. NOTE: AUTH-1 already split Service.mu into enrolMu + sessMu, so this task's fix must NOT reunify them -- keep AUTH-3's durable enrolment fsync off the Authenticate hot path.
  _Proof: go test -race -run TestSessionTableEvictsOldestPendingUnderLoad ./internal/auth_
- [-] AUTH-6 · AUTH-6: Auth FAIL-OPEN risk -- wrap the mux with auth + an explicit unauthenticated allow-list — auth, P1
  Origin: reviewer + security pass over CORE-1..CORE-4 (2026-08-02). Zero P0s; the three P1s are being fixed in-wave. SECURITY FINDING P1-3 and the most important of the follow-ups. Routes are registered INDIVIDUALLY on the mux, which means authentication will be applied per-route once the AUTH epic lands. That is a fail-OPEN design: when an implementer later adds `mux.HandleFunc("/v1/send", ...)` and forgets to wrap it in the auth middleware, the result is a fully unauthenticated route on a message bus, and NO TEST FAILS. The mistake is silent, it is the easy mistake to make, and nothing in the current structure catches it.
  
  Fix -- invert the default so forgetting means 401, not open: wrap the WHOLE mux in the auth middleware and carry an EXPLICIT allow-list of unauthenticated paths (`/healthz`, `/v1/info`, `/v1/enrol`). Any path not on that list requires a credential; a newly-added route is therefore authenticated by default and an implementer must make a deliberate, visible, reviewable edit to the allow-list to open one up.
  
  PIN IT WITH A TEST, or the property decays: enumerate the registered routes (keep a single registry/slice the mux is built from so the test can iterate it) and assert that each route is EITHER on the allow-list OR returns 401 when called without a credential. That test is the actual deliverable -- it fails the moment someone adds an unprotected route, which is precisely the event nothing currently detects.
  
  Also assert the allow-list itself is minimal and intentional (a test that the allow-list contains exactly the expected three paths, so ADDING to it is also a visible diff). Coordinate with AUTH-2 (token verification middleware) -- this is the shape AUTH-2 should be built into rather than a later retrofit -- and with CORE-8 (JSON 404 catch-all): decide deliberately whether an unauthenticated request to an UNKNOWN path returns 401 or 404, since a catch-all registered outside the wrapper would itself be an unauthenticated route.
  _Proof: go test -race -run TestEveryRouteRequiresAuth ./internal/httpapi_
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
- [x] None · AUTH-1-FU-ACTIVECAP: cap ACTIVE sessions per agent -- the one place an agent-id-keyed cap is SAFE — auth, P0
  Discovered by the security gate on AUTH-1-FU-PENDINGCAP. Nothing caps ACTIVE sessions per agent, and enrolment is itself unauthenticated, so an attacker that enrols its own agent can complete MaxSessions handshakes and fill the session table with ACTIVE entries. Those are reclaimed only after SessionLifetime (1 hour), not after ChallengeTTL (2 minutes), so this costs roughly 9 req/s to hold rather than ~137, and the resulting denial of NEW session establishment outlives the flood by an hour. Verified empirically by the security agent: advancing the clock past ChallengeTTL reclaims nothing. This is PRE-EXISTING and NOT a regression from AUTH-1-FU-PENDINGCAP -- the cap removed there counted only SessionPending entries and never protected against this. THE LOAD-BEARING INSIGHT, and the reason this is not a repeat of the mistake AUTH-1-FU-PENDINGCAP just fixed: unlike a PENDING-challenge cap, an ACTIVE-session cap keyed on agent id is SAFE, because an active session can only be created by proving possession of that agent private key. The key is a PROVEN identity, not an attacker-supplied victim identifier, so a flooder cannot make its sessions land in a victim bucket. Must be argued explicitly in the implementation comment so the distinction is not lost, and must ship with an adversarial test in the shape of TestSessionBeginNoVictimLockout. Note this is referenced by name from internal/auth/session.go BeginSession and from CONTRACTS.md, so the key AUTH-1-FU-ACTIVECAP must not change.
  _Proof: go test -race -run TestSessionActiveCap ./internal/auth_
- [ ] None · AUTH-1-FU-RATELIMIT: per-source rate limiting on the three unauthenticated auth routes — auth, P1
  Follow-up to AUTH-1 (public_id 54fa94c0-6ca3-459a-aaa2-a3ea047f97d9). POST /v1/enroll, POST /v1/session/begin and POST /v1/session/complete are unauthenticated by design, but there is currently NO per-source (per-IP or similar) rate limit on any of them -- every admission-control cap that exists today is GLOBAL. Consequence: an anonymous caller can deny enrolment bus-wide by exhausting MaxRosterEntries (4096) with enrol requests, and can deny session establishment bus-wide by exhausting MaxSessions (16384) with begins. Security measured roughly 137 req/s as enough to sustain the session-table denial from a single source. Add per-source rate limiting (token bucket or similar, stdlib-first per invariant 8) in front of these three routes so a single source cannot exhaust a bus-wide cap alone.
  _Proof: go test -race -run TestSessionBeginRateLimit ./internal/httpapi_
- [x] AUTH-2 · AUTH-2: Token verification middleware — auth, P0
  Middleware that validates the bearer token on every route except /healthz, /v1/info, and /v1/enroll (invariant 3) -- rejects missing/malformed/forged/expired tokens with 401, and attaches the verified fully-qualified agent id to the request context for downstream handlers.
  _Proof: bash scripts/proof-check.sh 'go test -race -run "TestAuthMiddleware|TestEveryRouteRequiresAuth" ./internal/httpapi'_
- [x] AUTH-1 · AUTH-1: POST /v1/enroll -- signed credential issuance — auth, P0
  CORRECTED 2026-08-02 (spec-keeper) -- STATUS UNTOUCHED, a feature-runner is in flight. THREE PARTS OF
  THIS TASK'S PREVIOUS TEXT WERE STALE AND HAVE BEEN REMOVED. They are listed here so nobody restores
  them from an older copy.
  
   REMOVED (1) THE "OPEN QUESTION" ON BEARER-VS-PER-REQUEST SIGNING. It is ANSWERED, do not re-open it
   and do not spend a DECISIONS.md entry deciding it. The settled design (DECISIONS.md 2026-08-02):
   enrolment records the public key; then the CLIENT ASKS FOR A SESSION, THE **SERVER** PROVIDES THE
   TOKEN VALUE, AND THE CLIENT **SIGNS** IT with its enrolment private key; the server verifies against
   the recorded public key and thereafter accepts that session. Signing happens ONCE PER SESSION, NOT
   PER REQUEST, so the hot path (long-poll, send) is a cheap credential check. The token is
   server-provided so the client never chooses the value it signs -- a client-chosen challenge would
   allow pre-computation and prove far less.
  
   REMOVED (2) "THE SERVER SIGNS THE PUBLIC KEY + MINTED ID INTO THE CREDENTIAL USING A PERSISTED BUS
   SIGNING SECRET." THAT IS THE OLD DESIGN AND IT IS SUPERSEDED. **Tokens are OPAQUE SERVER-SIDE
   HANDLES, not signed claims** (decision, 2026-08-02). That is precisely what makes IMMEDIATE
   revocation possible -- a stateless signed claim cannot be revoked. So: DO NOT generate or persist a
   bus signing secret for credential issuance, and do not put claims in the token. The server keeps the
   session state; the token is a lookup key into it.
  
   REMOVED (3) THE CONSTRAINT MANDATING `scripts/bus-enrol.sh` + AGENTIF-2 IN THE SAME TASK. Invariant 7
   was AMENDED on 2026-08-02: the compiled Go CLI replaces the shell wrappers. AGENTIF-2 is SUPERSEDED
   and its work is CLI-2. There is no openssl-in-bash keypair requirement any more -- the CLI generates
   and stores the key. AUTH-1 is therefore SERVER-SIDE ONLY. The pairing rule itself survives in
   amended form: this endpoint is not "done" for an agent until CLI-2 ships, so keep them cross-linked.
  
  WHAT AUTH-1 IS, AS IT NOW STANDS.
  
  POST /v1/enroll. The agent submits a desired short name plus the PUBLIC half of a client-generated
  Ed25519 AUTH keypair. This is an ASYMMETRIC keypair, not a shared secret -- a symmetric option
  (HMAC over agent-id+key with a persisted bus secret) is NOT acceptable and must not be implemented:
  the server must never hold material that would let it FORGE an agent's calls, only material that lets
  it VERIFY them. Use stdlib `crypto/ed25519` (invariant 9: standard, audited, high-level sign/verify;
  never assemble primitives). The agent holds the private key; the server stores only the public key
  against the roster entry.
  
  The server MINTS the agent id (invariant 1 -- ids are server-authoritative, never client-supplied;
  ID-3 provides the `<bus-id>.<name>-<n>` minting). The roster entry binds the minted id to the
  presented public key, so a caller cannot later present a different key under the same id.
  
  THIS AUTH KEYPAIR IS DISTINCT FROM THE MESSAGING IDENTITY KEYPAIR minted in CRYPTO-3 -- two keypairs,
  two purposes (authentication vs E2E message encryption), never conflated or reused. CRYPTO-3 depends
  on this task for the roster/enrolment shape it extends.
  
  DURABILITY -- AND THE DEPENDENCY THAT MAKES IT SHIPPABLE NOW. A client must never get a credential for
  an enrolment that is not durable: the roster entry goes through the two-phase prepare->commit write
  path (invariant 4). **THE PERSISTENCE ITSELF IS DELIVERED BY AUTH-3 (roster persistence & recovery),
  NOT BY THIS TASK.** AUTH-1 therefore ships against an INJECTED PERSISTENCE INTERFACE -- define the
  narrow interface AUTH-1 needs, take it as a dependency, and let AUTH-3 supply the durable
  implementation. Do not inline a bespoke roster file here; do not block on AUTH-3 either.
  
  NOTE FOR THE SESSION WORK (AUTH-2/AUTH-4, not this task, but the shape is decided): sessions last AT
  MOST ONE HOUR; the client refreshes at 75% of lifetime; server-side expiry is authoritative and an
  expired token is rejected even if the client believes otherwise; **SESSIONS DO NOT SURVIVE A SERVER
  RESTART** (they are expired on restart, the CLI re-authenticates); and **REVOCATION IS IMMEDIATE** --
  /v1/leave invalidates outstanding sessions at once, not at the <=1h boundary.
  
  ACCEPTANCE CRITERION (RATCHET-7 fallout, verified first-hand by reading this box's stdlib source at
  crypto/ed25519/ed25519.go under GOROOT): **ed25519.Verify PANICS -- it does not return false -- when
  len(publicKey) != ed25519.PublicKeySize.** This is a remote DoS trap, and it is ASYMMETRIC with
  malformed-signature handling (a bad signature safely returns false), so a call site that only checks
  the signature looks correct and is not. The public key presented here is client-supplied, untrusted
  input by definition. REQUIRED: length-check the presented public key against ed25519.PublicKeySize
  BEFORE any ed25519.Verify call in this path, returning a normal validation error on mismatch, never
  panicking. REQUIRED TEST: a negative test feeding a wrong-size public key and a nil/empty public key
  through the enrolment path, asserting clean rejection rather than a panic. See the standalone
  cross-cutting task (4eb903f8) tracking this trap across all Verify call sites (AUTH-1, CRYPTO-10,
  SIGN-2, and any roster-reload-from-disk path).
  
  IDEMPOTENCY (invariant 10): enrol carries a client-supplied idempotency key and is safe to retry --
  same key + same payload returns the ORIGINAL result and must NOT mint a second id; same key +
  different payload is a protocol violation. IDEM-13 owns the full treatment; do not design enrol in a
  way that makes it impossible.
  _Proof: go test -race -run TestEnroll ./internal/auth_
- [x] None · AUTH-1-FU-PENDINGCAP: MaxPendingPerAgent is a lockout primitive, not a defence -- rekey or drop it — auth, P0
  Follow-up to AUTH-1 (public_id 54fa94c0-6ca3-459a-aaa2-a3ea047f97d9). MaxPendingPerAgent is keyed on agent_id, but agent_id on the unauthenticated session-begin route is an ATTACKER-SUPPLIED victim identifier. A flooder's challenges land in the victim's bucket, and eviction under the cap then drops the victim's own correctly-issued challenge. The reviewer demonstrated a permanent unauthenticated lockout of a named agent at 9 requests per round. Redesign options: (a) key the cap on the SOURCE of the request instead of the target agent_id, or (b) drop the per-agent cap entirely and rely on the global MaxSessions cap plus ChallengeTTL to bound memory (a pending session is a handful of words, so the memory argument for keeping a per-agent cap is weak). This needs a DECISIONS.md entry recording which option was taken and why. NOTE: the misleading code comments and CONTRACTS.md wording describing this as a defence have ALREADY been corrected as part of AUTH-1's review pass -- this task is the DESIGN fix (the actual keying/eviction behaviour), not a comment fix.
  _Proof: go test -race -run TestSessionBeginNoVictimLockout ./internal/auth_
- [ ] None · AUTH-1-FU-POPKEY: enrolment does not prove possession of the enrolling private key — auth, P1
  Follow-up to AUTH-1 (public_id 54fa94c0-6ca3-459a-aaa2-a3ea047f97d9). Enrolment binds a caller-supplied public key to a fresh server-minted agent id, but never checks that the caller holds the matching private key -- so anyone can bind ANY public key, including someone else's already-published one, to a new identity. Security recommends requiring a signature over (name || public_key || idempotency_key) at enrolment time, verified with the submitted public key, before the binding is accepted. IMPORTANT: this CHANGES THE ENROL WIRE SHAPE (adds a signature field to the request), so it must be coordinated with the Go CLI / AGENTIF work that also touches the enrol payload -- do not land it unilaterally. The invariant that already holds and must be preserved: once an id is bound to a key, it can never later present a different key (this task only adds proof-of-possession at the initial binding, it does not change post-enrolment behaviour).
  _Proof: go test -race -run TestEnrollRequiresProofOfPossession ./internal/auth_
- [ ] AUTH-4 · AUTH-4: POST /v1/leave -- leave / revocation — auth, P1
  Lets an enrolled agent durably remove itself from the roster; its token is rejected by the auth middleware on every call afterward, including after a restart (the revocation itself goes through the two-phase write path).
  
  ACCEPTANCE CRITERION ADDED (spec-keeper, 2026-08-02, from ID-3 reviewer F2 + security LOW finding): internal/ids/agentmint.go point 8 delegates bounding distinct-name growth to admission control, but AUTH-1 (now done) carried no such obligation in its description. Today growth is contained only because the roster never shrinks (no leave existed yet) and admission caps roster.Len(). Once this task lets leave shrink the roster while suffix counters must NOT be reclaimed (ids are never reused), an enrol/leave loop over distinct 64-byte names can grow suffix-counter memory without bound. This task must explicitly state, and test, how it bounds suffix-counter growth under a repeated enrol/leave loop (e.g. a cap on distinct names ever seen, eviction policy, or an explicit accepted-and-documented unbounded-but-slow-growth argument) -- do not ship leave without addressing this.
  _Proof: go test -race -run TestLeaveRevocation ./internal/auth_
- [x] None · AUTH-1-FU-LISTENADDR: default listen address is :8080 (all interfaces) but DECISIONS.md settled on localhost — auth, P0
  Follow-up to AUTH-1 (POST /v1/enroll, public_id 54fa94c0-6ca3-459a-aaa2-a3ea047f97d9). cmd/agent-bus/main.go:37 has `defaultListen = ":8080"`, binding all interfaces; DECISIONS.md line 709 settled that the default listen address is localhost. This mismatch pre-dates AUTH-1, but AUTH-1 just added THREE UNAUTHENTICATED routes (POST /v1/enroll, POST /v1/session/begin, POST /v1/session/complete) onto that surface, so the exposure is now material -- an anonymous caller on the network, not just localhost, can hit them. Fix is a one-line change in main.go plus doc updates in README.md:41,48 and CONTRACTS.md:46 (out of scope for AUTH-1 itself, since README ownership sits outside that task's file boundary). This is an operator-visible behaviour change (existing deployments binding :8080 today would start binding 127.0.0.1 only) -- call that out in the commit message and/or a DECISIONS.md addendum if a migration note is warranted.
  _Proof: grep -n "defaultListen" cmd/agent-bus/main.go | grep -q "127.0.0.1" && grep -qE "^\| \`-listen\` \| \`127\.0\.0\.1:8080\` \|" CONTRACTS-CLI.md && echo ALL_OK_
- [ ] AUTH-3 · AUTH-3: Roster persistence & recovery — auth, P0, blocked
  The agent roster (id, name, public key/verifier material, enrolled-at) is rebuilt on startup by WAL replay, not held only in memory -- an agent enrolled before a restart is still authenticated and listed after one, with no re-enrolment required.
  
  CORRECTION (spec-keeper, 2026-08-02, from ID-3 security+reviewer gate findings): the resume floor for name suffixes must NOT be derived from the committed roster alone -- internal/ids/agentmint.go point 3 explicitly forbids that derivation. It must be reconstructed from ALL prepares ever written -- committed, aborted, AND dangling -- covering agents still enrolled and agents that have since departed, or a new agent minted with a different keypair can silently inherit a previous agent's id/suffix. This task must land BEFORE any enrolment record reaches the WAL (once an agent id is on disk, id-reuse-on-restart escalates from MEDIUM to CRITICAL). Cross-reference ID-2-WIRING-OBSERVER (c31f6999-da4e-400d-ab55-178b82e2a42e), the task that exposes dangling prepares needed to compute this floor correctly.
  _Proof: go test -race -run TestRosterRecovery ./internal/auth_

### EPIC CLI — Human CLI interface to the bus

- [x] CLI-1 · CLI-1: client package (NOT under internal/) + CLI subcommand skeleton -- the single client that replaces the shell wrappers — cli, P0
  MERGED EPIC 2026-08-02. The CLI and AGENTIF epics are now ONE epic (user decision, DECISIONS.md
  2026-08-02: "Merge the CLI and AGENTIF epics" / "the go cli should take the place of the .sh files and
  be easy to use for a human + friendly for an agent to use or embed"). Invariant 7 is AMENDED, not
  weakened: nobody hand-writes HTTP, but the vehicle is a CLI subcommand, not a scripts/bus-*.sh
  wrapper. A feature without its CLI subcommand is still not done.
  
  THREE AUDIENCES, and every subcommand serves all three: a HUMAN (readable output, remedial errors); an
  AGENT SHELLING OUT (--json, stable exit codes, NO interactive prompts, NO TTY-dependent credential
  input); an AGENT EMBEDDING (the reusable client package, which therefore CANNOT live under internal/).
  
  THE DECISION THAT IS ALREADY MADE AND MUST NOT BE RE-LITIGATED: **"embed" is the load-bearing word.**
  The CLI is a THIN SHELL over a REUSABLE GO CLIENT PACKAGE, and that package **CANNOT LIVE UNDER
  `internal/`** -- Go would forbid any other module importing it, which defeats the entire requirement.
  Decided 2026-08-02 precisely because deciding it late would be expensive. Put it at a top-level
  importable path (e.g. `client/`), and treat its exported surface as a PUBLIC API subject to
  compatibility care. The binary is a separate `cmd/` (e.g. `cmd/agent-bus-cli`) so the server image
  never accidentally ships the client, and the client package must NOT import anything under
  `internal/`.
  
  STILL TO DECIDE AND RECORD IN DECISIONS.md (the original CLI-1 question, narrowed): the exact package
  path and binary name. NOT still open: whether the package is importable (it is), and whether one
  binary serves both humans and agents (it does).
  
  SCOPE.
   - The client package: transport, base URL, timeouts, retry/backoff, credential handling, cursor
     management, and typed errors. NO business logic beyond what later CLI tasks need.
   - Subcommand skeleton and global flags: --bus URL, --identity path, --json, --timeout.
     Config/env resolution order, documented, deterministic.
   - EXIT-CODE CONVENTIONS, fixed now and treated as contract: distinct codes for usage error,
     auth/credential failure, network/unreachable, server-side error, and "nothing to report" so an
     agent can branch without parsing text. Put them in CONTRACTS.md.
   - **THE LONG-POLL SUBCOMMAND STREAMS NEWLINE-DELIMITED JSON (NDJSON)** -- one JSON object per line,
     flushed as it arrives, so a consumer can process incrementally rather than buffering to
     completion. Establish that convention here even though CLI-3 implements the command.
   - NO interactive prompts anywhere, and no TTY-dependent credential input. An agent shelling out has
     no TTY.
   - CONTRACTS.md gains the CLI's flags, exit codes and JSON shapes -- the binary now has a second
     consumer with a compatibility expectation.
  
  NOT IN SCOPE: any actual endpoint call (CLI-2..CLI-8 own those), and rewriting AGENT_PROTOCOL.md
  against subcommands (its own task).
  
  PROOF. `go build ./... && go test -race ./client/... && go vet ./... && go run ./cmd/agent-bus-cli --help 2>&1 | grep -q 'enrol' && ! go list -deps ./cmd/agent-bus-cli | grep -q 'agent-bus/internal/'`
  The last clause is the load-bearing one: it MECHANICALLY ENFORCES that the client binary (and hence
  the client package) does not depend on `internal/`, which is the requirement most likely to be broken
  by accident. FAILS TODAY by construction -- neither the package nor the binary exists. Adjust the
  paths to whatever DECISIONS.md settles, but KEEP the internal/-dependency clause.
  _Proof: go build ./... && go test -race ./client/... && go vet ./... && go run ./cmd/busctl --help 2>&1 | grep -q 'enrol' && ! go list -deps ./cmd/busctl | grep -q 'agent-bus/internal/'_
- [~] CLI-4 · CLI-4: send + broadcast, incl. stdin and interactive (replaces bus-send.sh and bus-broadcast.sh) — cli, P1, in progress
  MERGED EPIC 2026-08-02. The CLI and AGENTIF epics are now ONE epic (user decision, DECISIONS.md
  2026-08-02: "Merge the CLI and AGENTIF epics" / "the go cli should take the place of the .sh files and
  be easy to use for a human + friendly for an agent to use or embed"). Invariant 7 is AMENDED, not
  weakened: nobody hand-writes HTTP, but the vehicle is a CLI subcommand, not a scripts/bus-*.sh
  wrapper. A feature without its CLI subcommand is still not done.
  
  THREE AUDIENCES, and every subcommand serves all three: a HUMAN (readable output, remedial errors); an
  AGENT SHELLING OUT (--json, stable exit codes, NO interactive prompts, NO TTY-dependent credential
  input); an AGENT EMBEDDING (the reusable client package, which therefore CANNOT live under internal/).
  
  REPLACES AGENTIF-4 (`scripts/bus-broadcast.sh`) and AGENTIF-5 (`scripts/bus-send.sh`), both superseded.
  
  Send a DM to a fully-qualified agent id, or broadcast to the roster. Body from an argument, from a
  file, or piped on stdin (so it composes with other tools). Refuse ambiguous or empty sends with a
  clear error rather than sending nothing.
  
  **IDEMPOTENCY IS THIS COMMAND'S HARD REQUIREMENT (invariant 10).** The client generates the
  idempotency key ONCE and REUSES IT ON EVERY RETRY of the same logical send. Generating a fresh key per
  attempt turns the retry that idempotency exists to make safe into a duplicate message. The named test
  in the proof exists specifically to pin that. Note the two cases must not be collapsed: same key +
  same payload is a LEGITIMATE RETRY (server returns the original result); same key + DIFFERENT payload
  is a PROTOCOL VIOLATION (server rejects and disconnects) -- the CLI must never produce the second by
  mutating a body between attempts. Idempotency keys are retained for a BOUNDED window and FAIL CLOSED,
  so a retry arriving after the window is rejected rather than silently re-applied: surface that as a
  specific, actionable error, not a generic failure.
  
  DEPENDS ON: MSG epic, IDEM epic, CLI-1, CLI-2.
  
  PROOF. FAILS TODAY by construction. See IDEM-18 (wrappers generate the key once) -- that task is
  re-scoped to this client.
  _Proof: go test -race -run 'TestCLISend|TestCLIBroadcast' ./client/... ./cmd/busctl/... && go test -race -run TestCLISendReusesIdempotencyKeyOnRetry ./cmd/busctl/..._
- [x] CLI-2 · CLI-2: identity -- enrol, whoami, use, logout (ABSORBS AGENTIF-2; there is no bus-enrol.sh) — cli, P0
  MERGED EPIC 2026-08-02. The CLI and AGENTIF epics are now ONE epic (user decision, DECISIONS.md
  2026-08-02: "Merge the CLI and AGENTIF epics" / "the go cli should take the place of the .sh files and
  be easy to use for a human + friendly for an agent to use or embed"). Invariant 7 is AMENDED, not
  weakened: nobody hand-writes HTTP, but the vehicle is a CLI subcommand, not a scripts/bus-*.sh
  wrapper. A feature without its CLI subcommand is still not done.
  
  THREE AUDIENCES, and every subcommand serves all three: a HUMAN (readable output, remedial errors); an
  AGENT SHELLING OUT (--json, stable exit codes, NO interactive prompts, NO TTY-dependent credential
  input); an AGENT EMBEDDING (the reusable client package, which therefore CANNOT live under internal/).
  
  **THIS TASK ABSORBS AGENTIF-2 ("scripts/bus-enrol.sh"), which is SUPERSEDED.** AGENTIF-2 was a P0
  telling someone to write a shell wrapper; that is exactly the instruction the 2026-08-02 amendment
  retires, and leaving it would have had two agents build two enrolment clients. There is ONE
  enrolment client and it is this subcommand.
  
  SCOPE -- identity, for humans AND agents, against the AUTH surface.
   - `enrol` -- generate the Ed25519 AUTH keypair locally, submit ONLY the public half, receive the
     SERVER-MINTED fully-qualified id `<bus-id>.<agent-id>` (invariant 1 -- the client never chooses
     its id), and store the credential.
   - SESSION HANDLING, per the 2026-08-02 auth decision: the client asks for a session, the SERVER
     provides the token value, the client SIGNS it with its enrolment private key, and the server
     verifies against the recorded public key. The session lasts AT MOST ONE HOUR and the client
     REFRESHES AT 75% OF LIFETIME (server expiry is authoritative; do not refresh at the boundary).
     Tokens are OPAQUE server-side handles, not signed claims. **SESSIONS DO NOT SURVIVE A SERVER
     RESTART** -- the CLI must re-authenticate transparently rather than surfacing a confusing failure.
   - `whoami`, `use` (switch identity/bus), `logout` (calls /v1/leave AND clears the local credential).
     **Revocation is IMMEDIATE** -- /leave invalidates outstanding sessions at once, not at expiry.
   - Credential storage under the user's config dir at 0600, NEVER in the repo, never world-readable.
     No interactive prompt and no TTY-dependent input -- an agent shelling out has no TTY.
   - A human is just another enrolled participant with no special server-side privilege.
  
  DEPENDS ON: AUTH-1 (enrol, in flight), AUTH-2 (token middleware), AUTH-4 (leave/revocation), CLI-1.
  
  PROOF. Unit tests plus a REAL end-to-end enrolment against a server brought up through
  scripts/bus-serve.sh on an isolated run dir and port -- because "the subcommand is written" is not the
  same as "an agent can enrol". FAILS TODAY by construction (neither the CLI nor /v1/enroll exists).
  Do NOT complete this on the unit-test clause alone.
  _Proof: go test -race -run 'TestEnrol|TestCLIEnrol' ./client/... ./cmd/busctl/..._
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
- [ ] None · CLI-1-FU-BINARYNAME: Decide the INSTALLED name of the client binary — cli, P2
  The directory is cmd/busctl, but "busctl" is also systemd's D-Bus tool, present on most Linux hosts (part of systemd, ships in the base install on Debian/Ubuntu/Fedora/Arch), so "go install ./cmd/busctl" or dropping the built binary on PATH SHADOWS the system tool. Nothing in the code depends on the installed name -- module path, package name and all internal references are independent of the final binary filename, so this is a pure decision task, no design work. Needs a user decision: keep "busctl" (accept the collision, document "run via full path or an alias"), rename to "agent-busctl", "abus", or something else. Recorded as an open question in DECISIONS.md 2026-08-02 SS1. Once decided: rename the cmd/ directory if changed, update all docs (CONTRACTS-CLI.md, AGENT_PROTOCOL.md, README.md, Dockerfile/CLI-BUSCTL-IMAGE) and go.mod-relative install instructions to match, and record the final decision + rationale in DECISIONS.md.
- [~] CLI-3 · CLI-3: watch -- long-poll tail, human-readable for a person and NDJSON for a pipe (replaces bus-wait.sh) — cli, P1, in progress
  MERGED EPIC 2026-08-02. The CLI and AGENTIF epics are now ONE epic (user decision, DECISIONS.md
  2026-08-02: "Merge the CLI and AGENTIF epics" / "the go cli should take the place of the .sh files and
  be easy to use for a human + friendly for an agent to use or embed"). Invariant 7 is AMENDED, not
  weakened: nobody hand-writes HTTP, but the vehicle is a CLI subcommand, not a scripts/bus-*.sh
  wrapper. A feature without its CLI subcommand is still not done.
  
  THREE AUDIENCES, and every subcommand serves all three: a HUMAN (readable output, remedial errors); an
  AGENT SHELLING OUT (--json, stable exit codes, NO interactive prompts, NO TTY-dependent credential
  input); an AGENT EMBEDDING (the reusable client package, which therefore CANNOT live under internal/).
  
  REPLACES AGENTIF-6 (`scripts/bus-wait.sh`), which is superseded.
  
  The headline command. Drives the long-poll wait endpoint in a loop and renders messages as a readable
  live feed (timestamp, sender, recipient/scope, body), advancing its cursor across reconnects. Handles
  Ctrl-C cleanly, reconnects with backoff on transient failure, and never busy-loops.
  
  **--json STREAMS NDJSON: one JSON object per line, FLUSHED AS IT ARRIVES.** This is the requirement
  that makes the command usable by an embedding or shelling-out agent at all -- a long-poll that buffers
  to completion is useless, because it never completes. The test named in the proof must assert
  INCREMENTAL delivery (a reader sees line 1 before the stream ends), not merely that the output parses.
  
  **DELIVERY IS AT-LEAST-ONCE** (decision, 2026-08-02). Duplicates are the NORMAL steady state, not an
  edge case. The watch loop must not present a duplicate as an error, and the help text must say so, so
  an agent author writes an idempotent handler instead of assuming exactly-once. Freshness comes from
  the server-minted monotonic sequence plus the recipient-side cursor.
  
  Session refresh (75% of lifetime) and transparent re-authentication after a server restart must be
  invisible here -- a watch that dies when the bus restarts is a watch nobody can rely on.
  
  DEPENDS ON: POLL epic, CLI-1, CLI-2.
  
  PROOF. FAILS TODAY by construction. The second clause is deliberately a SEPARATE named test for the
  incremental-streaming property, because a --json flag that buffers would pass a naive shape test.
  _Proof: go test -race -run 'TestCLIWatch' ./client/... ./cmd/busctl/... && go test -race -run TestCLIWatchStreamsNDJSONIncrementally ./cmd/busctl/..._
- [ ] None · CLI-2-FU-GITIGNORE: Add the credential store to .gitignore — cli, P3
  busctl --identity ./creds (or the default in-repo-relative path if a user runs it from inside the repo) would put Ed25519 private-key seeds in the working tree where a careless "git add -A" commits them permanently into history. Add identities.json and identities.json.tmp-* (the store's O_EXCL temp-write pattern) to .gitignore. Trivial, but it is the file that must never be committed -- a leaked seed is a full identity compromise, not just a credential rotation. (.gitignore was outside the CLI-1/CLI-2 wave's file-ownership boundary, hence this follow-up rather than folding it in.)
  _Proof: git check-ignore -q identities.json && git check-ignore -q identities.json.tmp-abc123_
- [~] CLI-5 · CLI-5: agents -- roster listing (replaces bus-agents.sh) — cli, P1, in progress
  MERGED EPIC 2026-08-02. The CLI and AGENTIF epics are now ONE epic (user decision, DECISIONS.md
  2026-08-02: "Merge the CLI and AGENTIF epics" / "the go cli should take the place of the .sh files and
  be easy to use for a human + friendly for an agent to use or embed"). Invariant 7 is AMENDED, not
  weakened: nobody hand-writes HTTP, but the vehicle is a CLI subcommand, not a scripts/bus-*.sh
  wrapper. A feature without its CLI subcommand is still not done.
  
  THREE AUDIENCES, and every subcommand serves all three: a HUMAN (readable output, remedial errors); an
  AGENT SHELLING OUT (--json, stable exit codes, NO interactive prompts, NO TTY-dependent credential
  input); an AGENT EMBEDDING (the reusable client package, which therefore CANNOT live under internal/).
  
  REPLACES AGENTIF-3 (`scripts/bus-agents.sh`), superseded. Raised P2 -> P1 to match the AGENTIF-3 it
  absorbs.
  
  List the enrolled roster as an aligned human-readable table (id, name, bus, enrolled-at, last-seen),
  with --json for scripting. **Make the fully-qualified `<bus-id>.<agent-id>` readable WITHOUT
  TRUNCATING the part that matters for routing** (invariant 2) -- eliding the bus prefix to fit a
  terminal is exactly the wrong end to cut, because that prefix is what disambiguates a cross-bus id.
  If the terminal is narrow, wrap or drop a less important column; never the qualified id.
  
  DEPENDS ON: MSG-1 (GET /v1/agents), CLI-1, CLI-2. PROOF fails today by construction.
  _Proof: go test -race -run 'TestCLIAgents' ./client/... ./cmd/busctl/..._
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

### EPIC CORE — Repo skeleton & server bootstrap

- [ ] CORE-5 · CORE-5: Observability: metrics/inspect endpoint (follow-up) — observability, P3
  Low-priority follow-up. Add a GET /v1/debug (or /metrics) endpoint exposing in-process counters (messages sent, active waiters, WAL bytes, roster size, relay peer status) as plain JSON -- stdlib-first, no Prometheus dependency needed initially. Depends on MSG/POLL/RELAY existing to have something worth reporting.
  _Proof: go test -race -run TestInspectEndpoint ./internal/httpapi_
- [ ] CORE-14 · CORE-14: A handler that writes then panics logs status=200 -- the audit trail is wrong — core, P3
  Origin: reviewer + security pass over CORE-1..CORE-4 (2026-08-02). Zero P0s were found; the three P1s are being fixed in-wave. This is one of the remaining lower-priority items, filed separately so it is actionable on its own. If a handler writes a response (or just the header) and THEN panics, the recovery middleware cannot change the already-sent status: the client receives a truncated 200 body, and the log line records `status=200` for a request that failed. Anyone reading the logs or building an error-rate metric sees a success. The response itself is unfixable once bytes are on the wire -- that is HTTP -- but the LOG must not lie. Add a `panic_after_write` boolean (or an equivalent explicit marker) set when recovery fires after wroteHeader is true, so the log line is unambiguous, and make sure the panic is still logged with its stack (see CORE-6). Test: a handler that writes 200, flushes, then panics -> assert the log line carries the marker and the recorded status.
  _Proof: go test -race -run TestPanicAfterWrite ./internal/httpapi_
- [ ] CORE-8 · CORE-8: Unmatched paths return ServeMux's text/plain 404, breaking the JSON error contract — core, P2
  Origin: reviewer + security pass over CORE-1..CORE-4 (2026-08-02). Zero P0s were found; the three P1s are being fixed in-wave. This is one of the remaining lower-priority items, filed separately so it is actionable on its own. Every handled response honours a JSON error envelope, but a request to an unregistered path falls through to net/http.ServeMux's built-in handler and returns `404 page not found` as text/plain. A client (or a bus-*.sh wrapper piping through a JSON parser) that trusts the documented contract gets a parse error instead of a structured error -- the failure mode is worst exactly when something is already wrong. Fix: register a catch-all (`/`) handler, or wrap the mux, so unmatched paths emit the same JSON error envelope with 404 and the correct Content-Type. Note the ordering interaction with AUTH-6 (the auth wrapper + unauthenticated allow-list): a catch-all must NOT become an unauthenticated route that leaks which paths exist -- coordinate the two, and decide deliberately whether an unauthenticated request to an unknown path gets 401 or 404. Test both an unknown path and an unknown method on a known path.
  _Proof: go test -race -run TestNotFoundJSON ./internal/httpapi_
- [ ] CORE-10 · CORE-10: .gitignore has no secret patterns while the stop hook stages with `git add -A` — core, P2
  Origin: reviewer + security pass over CORE-1..CORE-4 (2026-08-02). Zero P0s were found; the three P1s are being fixed in-wave. This is one of the remaining lower-priority items, filed separately so it is actionable on its own. LATENT, NOT A LIVE LEAK -- nothing sensitive is currently tracked, and the spec-cloud credentials correctly live outside the repo. The risk is mechanical: .claude/hooks/commit-on-stop.sh stages with `git add -A`, and its guards cover file size and conflict markers but NOTHING for credentials, so any key material an agent or a human drops into the working tree is auto-staged and committed without anyone deciding to. Add secret patterns to .gitignore: `*.pem`, `*.key`, `*.p12`, `*.pfx`, `.env`, `.env.*`, `*credentials*`, `*-creds*`, `id_rsa*`, `*.token`. Cheap, permanent, and it becomes materially more important once the CRYPTO epic starts putting agent private keys on disk. Verify with `git check-ignore` against a sample of each pattern; do not commit the sample files.
  _Proof: git check-ignore -q test.pem test.key .env id_rsa my-creds.json && echo ok_
- [x] CORE-2 · CORE-2: cmd/agent-bus main entrypoint + config/flags — core, P0
  cmd/agent-bus/main.go wires flag parsing (listen addr, data-dir, bus-id override for testing, long-poll timeout, log level) into a Config struct; server binds the listener and shuts down cleanly on SIGINT/SIGTERM. No routes yet beyond a bare mux. NOTE: -bus-id is a TEST-ONLY affordance -- invariant 1 says the server is authoritative on ids, so this override exists purely to make tests deterministic and must never be relied on by production callers.
  _Proof: go build ./... && go run ./cmd/agent-bus -h 2>&1 | grep -q -- -data-dir_
- [ ] CORE-13 · CORE-13: Middleware implements Flusher/Hijacker unconditionally and drops io.ReaderFrom (untested) — core, P3
  Origin: reviewer + security pass over CORE-1..CORE-4 (2026-08-02). Zero P0s were found; the three P1s are being fixed in-wave. This is one of the remaining lower-priority items, filed separately so it is actionable on its own. The logging/recovery middleware's responseWriter wrapper declares Flush() and Hijack() unconditionally, so `w.(http.Flusher)` and `w.(http.Hijacker)` ALWAYS succeed at the handler -- even when the underlying ResponseWriter implements neither. A handler that feature-detects (the normal, correct pattern) is misled and will panic or error at call time instead of taking its fallback path. It also drops io.ReaderFrom, which costs net/http's sendfile fast-path for file/large responses. None of this is covered by a test. Fix: either forward the type assertion to the wrapped writer and only advertise what it actually supports, or keep the unconditional methods but return http.ErrNotSupported from Hijack when the inner writer is not a Hijacker -- and add ReaderFrom pass-through. Add a table-driven test that wraps writers with different capability sets and asserts the wrapper advertises exactly the same set. Relevant soon: the POLL epic's long-poll may want Flush.
  _Proof: go test -race -run TestResponseWriterInterfaces ./internal/httpapi_
- [ ] CORE-7 · CORE-7: HEAD is 405'd by requireGET while writeJSON still guards MethodHead -- dead code, and probes use HEAD — core, P2
  Origin: reviewer + security pass over CORE-1..CORE-4 (2026-08-02). Zero P0s were found; the three P1s are being fixed in-wave. This is one of the remaining lower-priority items, filed separately so it is actionable on its own. internal/httpapi's requireGET rejects HEAD with 405, but writeJSON still contains a `if r.Method != http.MethodHead` guard that can therefore never be false-branched -- dead code that directly contradicts the handler's actual behaviour, so a reader cannot tell which one expresses the intent. It also matters operationally: load balancers, container health checks and uptime probes commonly issue HEAD, and today HEAD /healthz returns 405. Decide one way and make the code consistent: either accept HEAD on GET routes (net/http will suppress the body automatically, and the writeJSON guard becomes live and correct) -- recommended for at least /healthz -- or reject it and DELETE the writeJSON guard. Pin the decision with a test asserting the status code for HEAD on every GET route, and record the chosen behaviour in CONTRACTS.md.
  _Proof: go test -race -run TestHeadRequest ./internal/httpapi_
- [x] CORE-15 · CORE-15: logging.format() calls .String()/.Error() on a typed-nil -> panic inside the logger — core, P2
  Origin: reviewer + security pass over CORE-1..CORE-4 (2026-08-02). Zero P0s were found; the three P1s are being fixed in-wave. This is one of the remaining lower-priority items, filed separately so it is actionable on its own. internal/logging's format() type-switches on fmt.Stringer / error and calls .String() / .Error() on the value. A TYPED NIL (e.g. a (*bytes.Buffer)(nil) or a nil *MyError stored in an interface) satisfies the interface but panics on the method call with a nil-pointer dereference. That panic happens INSIDE THE LOGGER, and the logger is called from the recovery defer -- so a typed nil in a log field during panic handling is a panic DURING panic handling, which takes the process down instead of returning a 500. That escalation is why this is P2 rather than a nit. Fix: recover() around the value formatting (or reflect-check for a nil pointer before invoking) and emit a safe placeholder such as `<nil>` or `<unformattable>`. Table-driven test over: untyped nil, typed-nil Stringer, typed-nil error, a Stringer whose String() itself panics, and a normal value -- asserting the logger always produces a line and never panics.
  _Proof: go test -race -run TestFormatTypedNil ./internal/logging_
- [ ] CORE-9 · CORE-9: Set IdleTimeout + MaxHeaderBytes on http.Server -- and deliberately leave Read/WriteTimeout UNSET — core, P2
  Origin: reviewer + security pass over CORE-1..CORE-4 (2026-08-02). Zero P0s were found; the three P1s are being fixed in-wave. This is one of the remaining lower-priority items, filed separately so it is actionable on its own. The http.Server is constructed without resource bounds. Set explicit IdleTimeout (bounds idle keep-alive connections) and MaxHeaderBytes (bounds header memory per connection). DELIBERATELY LEAVE BOTH WriteTimeout AND ReadTimeout UNSET, and write a comment at the construction site saying WHY: either one is an absolute deadline on the whole request/response, so once the POLL epic lands, a 30s long-poll (defaultPollTimeout) is killed mid-flight by any timeout shorter than it -- and 'add a sensible timeout' is exactly the well-intentioned change a later contributor makes without realising it breaks the product's core mechanic. The comment is the guardrail. Request BODY size is bounded separately and per-handler with http.MaxBytesReader inside the JSON-decode helper that the ENROL/SEND epics (AUTH-1, MSG-2, MSG-3) introduce -- that is security finding P1-2, currently UNREACHABLE because both routes 405 before the body is ever touched, which is why it is filed here as a constraint on those tasks rather than as a fix to today's code. Add a note to AUTH-1/MSG-3 so the helper ships with the limit from day one.
  _Proof: go test -race -run TestServerTimeouts ./internal/httpapi_
- [-] CORE-12 · CORE-12: defaultListen=":8080" binds all interfaces -- prefer 127.0.0.1:8080 — core, P1
  SETTLED 2026-08-02 BY USER DECISION -- no longer a proposal to weigh. "**The default listen address is
  localhost.**" (DECISIONS.md, 2026-08-02, answers 8-11.) Raised P2 -> P1 because it is now a decided
  default that the shipped binary contradicts, not a suggestion. Change defaultListen to 127.0.0.1:8080
  and record the flag/env override in CONTRACTS.md; a deployment that needs a wider bind says so
  explicitly. Note DEPLOY-1/DEPLOY-2 (container + Compose) must set the bind explicitly, because a
  container that listens only on 127.0.0.1 inside its own namespace is unreachable from outside it --
  that is the one place this default needs an override, and it should be an explicit, commented one.
  
  --- ORIGINAL DESCRIPTION ---
  Origin: reviewer + security pass over CORE-1..CORE-4 (2026-08-02). Zero P0s were found; the three P1s are being fixed in-wave. This is one of the remaining lower-priority items, filed separately so it is actionable on its own. defaultListen is ":8080", which binds every interface, over plain HTTP, with no authentication implemented yet. Defaults are sticky: this one persists straight through the AUTH epic, and in the window before AUTH-2's middleware lands, anyone on the network can reach the bus. Change the default to "127.0.0.1:8080" and keep the flag/env var so binding wider is an explicit, deliberate operator choice rather than the path of least resistance. Update CONTRACTS.md and any README/AGENT_PROTOCOL.md example that assumes the old default, and check scripts/bus-serve.sh (AGENTIF-1) agrees.
  _Proof: go test -race -run TestDefaultListen ./internal/httpapi_
- [x] CORE-3 · CORE-3: GET /healthz and GET /v1/info endpoints — core, P0
  GET /healthz returns 200 {"status":"ok"} once the server is accepting connections (liveness only, no auth). GET /v1/info returns bus id, server version/build info, and uptime (also unauthenticated -- needed for pre-enrolment discovery). Both registered on the main mux. The bus id is served through a small interface with a placeholder implementation until the ID epic lands (see invariant 1: the server is authoritative on ids).
  _Proof: go test -race -run TestHealthzInfo ./internal/httpapi_
- [x] None · Re-verify CORE-1's gofmt proof with the corrected ($(go env GOROOT)/bin/gofmt) invocation -- its recorded proof_cmd never actually ran gofmt on this box — process, P1
  CORE-1's recorded proof_cmd is `go build ./... && test -z "$(gofmt -l .)"` (see the task's own record, public_id eea035e4-92de-4ca3-95ed-fa8073cd6a81). VERIFIED THIS SESSION: `gofmt` is NOT on PATH on this box -- only `$(go env GOROOT)/bin/gofmt` (currently /usr/local/go/bin/gofmt) is. A bare `gofmt` invocation fails to launch (exit 127), and critically `test -z "$(gofmt -l .)"` still PASSES in that case, because a command substitution whose command fails to even exec produces EMPTY stdout, and `test -z ""` is true. So CORE-1's proof_cmd, run literally as recorded on THIS box, never actually ran gofmt at all -- it recorded a PASS that proves nothing, i.e. it is VACUOUS in the exact sense scripts/proof-check.sh exists to catch (see task 84b76d5e, "a `-run` pattern that matches no test must FAIL, not pass vacuously" -- this is the same failure class one level up the stack: a whole tool silently absent rather than a test silently unmatched).
  
  This is not hypothetical for CORE-1 specifically: reviewer and security's notes on CORE-1 (see its journal) both separately report running gofmt via `$GOROOT/bin/gofmt` or equivalent and finding the repo clean -- so the SUBSTANCE was very likely fine -- but the recorded proof_cmd itself, taken literally, is not evidence of that; it is evidence of nothing.
  
  RE-VERIFICATION ALREADY DONE THIS PASS (spec-keeper, read-only, no repo file touched): ran the CORRECTED command below on the current working tree and got a genuine PASS -- go build succeeds and the real gofmt binary reports zero files needing formatting. So on the evidence gathered so far, CORE-1 does NOT need to be reopened -- but per instructions this is the orchestrator's/user's call, not spec-keeper's, so CORE-1 itself is left untouched (status, version unchanged) and this task exists to carry that re-verification result plus the corrected command for anyone who wants to double-check.
  
  DONE means: this task's proof_cmd (the corrected, non-vacuous gofmt invocation) is run and its verdict is quoted in this task's completion test_summary. If it ever comes back FAIL, file a follow-up (or ask the orchestrator/user) to reopen CORE-1 -- do NOT reopen CORE-1 directly from this task.
  
  BROADER IMPLICATION (see the companion proof_cmd-backfill task filed alongside this one): every OTHER task in this backlog whose proof_cmd contains a bare `gofmt` call (grep the export for the literal string "test -z \"$(gofmt") is equally suspect on any box where gofmt is not on PATH, and should be corrected to `$(go env GOROOT)/bin/gofmt` or an equivalent PATH-independent invocation as those tasks are touched.
  
  proof_cmd validated via scripts/proof-check.sh this session: verdict=PASS (exit 0), class=file-assertion,toolchain -- go build ./... succeeded and the real gofmt binary found zero files to reformat.
  _Proof: go build ./... && test -z "$("$(go env GOROOT)/bin/gofmt" -l .)"_
- [x] CORE-4 · CORE-4: Structured logging + request middleware — core, P0
  A small INTERNAL structured logger built over stdlib log (no third-party dependency, per invariant 8), wired as HTTP middleware logging method/path/status/latency/request-id for every route. Level configurable via the -log-level flag. Note: log/slog landed in go1.21 and is NOT available on this box's go1.19.4 toolchain (verified: log/slog absent from GOROOT/src/log and go list std), so it cannot be used here; the decision is recorded in DECISIONS.md.
  _Proof: go test -race -run TestLoggingMiddleware ./internal/httpapi_
- [x] CORE-1 · CORE-1: Repo skeleton: go.mod, internal/ package layout, .gitignore — core, P0
  PROOF_CMD CORRECTED 2026-08-02 (spec-keeper). The recorded proof was
  `go build ./... && test -z "$(gofmt -l .)"`. **On this box that never ran gofmt at all**: `gofmt` is
  NOT on PATH (only `$(go env GOROOT)/bin/gofmt` is), a command substitution whose command fails to exec
  produces EMPTY stdout, and `test -z ""` is TRUE -- so the clause PASSED by failing to launch. That is
  the same vacuity class scripts/proof-check.sh exists to catch, one level up the stack: a whole TOOL
  silently absent rather than a test silently unmatched.
  
  NOT REOPENED, and here is the evidence for that call rather than an assertion. The CORRECTED command
  was RE-RUN against the current tree on 2026-08-02 through scripts/proof-check.sh:
  `verdict=PASS class=file-assertion,toolchain exit=0` -- `go build ./...` succeeds and the REAL gofmt
  binary reports zero files needing formatting. CORE-1's substance was fine; only its evidence was
  worthless. Reviewer and security notes on this task independently recorded running gofmt via
  $GOROOT/bin and finding the repo clean, which agrees.
  
  STANDING RULE, now in CLAUDE.md: never use a bare `gofmt`; use `go fmt ./...` or
  `"$(go env GOROOT)/bin/gofmt" -l .`. See task c0a5bdb6 for the full write-up and fc8cd234 for the
  sweep of every other proof_cmd containing a bare gofmt call.
  
  --- ORIGINAL DESCRIPTION ---
  Initialize go.mod (module github.com/dodgymike/agent-bus, go1.19 toolchain pin), create the internal/ package layout (ids, store, wal, hub, auth, httpapi, relay) as packages with doc.go stubs, and the cmd/agent-bus/ dir. The HTTP package is named `httpapi`, NOT `http`: naming it `http` would shadow stdlib net/http in every file that imports both, which is a needless papercut. .gitignore already covers build artifacts and /data/ -- verify, do not duplicate. No server logic yet -- this is the scaffold every other task builds on.
  _Proof: go build ./... && test -z "$("$(go env GOROOT)/bin/gofmt" -l .)"_
- [ ] CORE-6 · CORE-6: logging maxValueLen=1024 truncates panic stack traces (exempt `stack` or raise to 8192) — core, P2
  Origin: reviewer + security pass over CORE-1..CORE-4 (2026-08-02). Zero P0s were found; the three P1s are being fixed in-wave. This is one of the remaining lower-priority items, filed separately so it is actionable on its own. internal/logging caps every field value at maxValueLen=1024, which silently truncates the `stack` field of a panic log line -- exactly the field whose tail (the deepest frames, i.e. where the panic actually happened) matters most. Measured: a real net/http request path produces a 1238-byte stack, so production loses the tail. The existing test does NOT catch this because it drives the handler through httptest, whose shorter call stack measures 962 bytes -- under the cap -- so the test passes while production truncates. Fix: either exempt the `stack` key from the cap or raise the cap to 8192 (state which and why; a cap still exists to stop an attacker-controlled header blowing up the log). CRUCIALLY, also fix the TEST so it exercises a stack long enough to trip the old cap -- otherwise the same blind spot reappears the next time the limit is tuned. Keep the cap on all other fields.
  _Proof: go test -race -run TestPanicStackNotTruncated ./internal/logging ./internal/httpapi_
- [ ] CORE-11 · CORE-11: shutdownGrace (10s) < defaultPollTimeout (30s) -- record the ctx.Done() contract in doc.go — core, P3
  Origin: reviewer + security pass over CORE-1..CORE-4 (2026-08-02). Zero P0s were found; the three P1s are being fixed in-wave. This is one of the remaining lower-priority items, filed separately so it is actionable on its own. shutdownGrace is 10s while defaultPollTimeout is 30s, so a parked long-poll outlives the graceful-shutdown window. Today this is SAFE only incidentally: cancelRoot fires first and cancels the request contexts. That safety is an accident of the current wiring, not a stated rule, and the POLL epic is written by someone else later. Record it as an explicit contract in internal/httpapi/doc.go: a POLL handler MUST select on ctx.Done() alongside its timeout, and MUST NOT block on a bare time.After(pollTimeout) -- a handler that ignores ctx.Done() will hang past shutdownGrace and be killed mid-response. Cross-reference from POLL-1's description so the constraint is in front of whoever writes the handler. Doc/comment change only; no behaviour change. Optionally note the alternative (raise shutdownGrace above defaultPollTimeout) and why it was not chosen.
  _Proof: grep -q 'ctx.Done' internal/httpapi/doc.go_

### EPIC CRYPTO — End-to-end message cryptography (dual keypairs, Double Ratchet, agent-side validation)

- [ ] CRYPTO-11 · CRYPTO-11: Audit-log content hash for signed (cleartext) messages -- implements the invariant-6 trade-off — durability, P2
  RESCOPED 2026-08-02 (sign-only). SETTLED by the user, verbatim: "ok, log only metadata and routing info." Invariant 6 is now written that way in CLAUDE.md, so the audit log records METADATA AND ROUTING INFO ONLY: message id, sequence, fully-qualified sender and recipient(s), bus path traversed, timestamp, size, and a content hash of the body. It never records bodies. DUR-5 has ALREADY been amended to this exact shape and its description is authoritative for the on-disk record; do not diverge from it. UNGATED: the CRYPTO-1 design spike this task used to wait on is DONE (CRYPTO_DEEPDIVE.md), and its ratchet recommendation was overridden by direct user instruction -- do not action it. WHAT THIS TASK IMPLEMENTS: the content-hash computation over the message body (crypto/sha256, stdlib -- invariant 9, no bespoke construction), wired in at the send path and handed to DUR-5's audit writer alongside the envelope metadata. THE PLAINTEXT-vs-CIPHERTEXT QUESTION THIS TASK USED TO POSE IS NOW MOOT AND MUST NOT BE RE-OPENED: there is no ciphertext -- bodies travel in cleartext with a detached Ed25519 signature -- so the hash is over the body bytes, and it MUST be the same bytes SIGN-1 canonicalises and signs, so that the logged hash and the signature are provably about the same content. State that binding explicitly; a hash over a different serialisation than the signature covers is a silent correctness hole. NON-REPUDIATION (the reason this is worth doing): the hash alone is only a fingerprint anyone could have produced -- paired with SIGN-2's signature over the same canonical bytes it PROVES a specific sender produced specific content at a specific sequence, without the log ever holding the content. Also deliver the operability answer: how a human debugs a flow they cannot read -- ordering, delivery and provenance reconstructable from metadata + hash alone (e.g. correlating hashes across sender / relay / recipient logs to prove the same content transited unmodified). Update PROTOCOL.md's on-disk section to match DUR-5, and make DECISIONS.md say plainly that the audit trail proves DELIVERY, ORDERING and AUTHORSHIP -- not CONTENT. NOTE the asymmetry to state honestly: the audit log withholds the body from a later reader of the log, but the body is NOT confidential in transit -- the bus and every relay peer read it (see RATCHET-2's threat model).
  _Proof: go test -race -run TestAuditLogContentHash ./internal/store_
- [ ] CRYPTO-9 · CRYPTO-9: Cross-bus relay of encrypted messages -- what an intermediate bus can and cannot see — relay, P3, deferred
  GATED: do not start until CRYPTO-1 (design spike) is done and its DECISIONS.md entry is recorded -- the spike chooses the crypto library/primitives, the audit-log trade-off, the ratchet-state durability model, the broadcast/relay scheme and the key-trust model. Implementing before that decision exists is guessing. Make E2E messages survive bus-to-bus relay (RELAY-2) end to end: a message from bus A's agent to bus B's agent must be decryptable ONLY by the destination agent, never by either bus. Requires cross-bus key-bundle fetch (how does an agent on bus A obtain and trust the messaging key of <bus-B>.<agent>? bus B attests it, but bus A is now trusting bus B -- implement the chain CRYPTO-1 defined, and state the residual trust plainly). Specify and test what a relaying/intermediate bus can see: envelope metadata, the traversed-bus path (RELAY-3), fully-qualified sender/recipient ids, sizes and timing -- and what it must never see: content, and any key material that would let it join a session. Cover the partial-failure cases the RELAY epic already worries about (peer down, retry/backoff, loop prevention) so a retried relay cannot cause a ratchet double-advance or a duplicate that decrypts twice.
  _Proof: go test -race -run TestRelayEncrypted ./internal/relay_
- [-] CRYPTO-2 · CRYPTO-2: Adopt the crypto primitive layer chosen by the spike (internal/cryptobox + go.mod/toolchain) — crypto, P2
  GATED: do not start until CRYPTO-1 (design spike) is done and its DECISIONS.md entry is recorded -- the spike chooses the crypto library/primitives, the ratchet-state durability model, the broadcast/relay scheme and the key-trust model. (The audit-log-vs-PFS trade-off is CLOSED per user instruction 2026-08-02 -- see CRYPTO-11/DUR-5 -- and is no longer part of what CRYPTO-1 decides.) Implementing before the remaining decisions exist is guessing.
  
  Land the dependency decision CRYPTO-1 recorded: add the chosen module(s) to go.mod, and bump the go directive/toolchain to whatever version the spike says is required.
  
  RUNTIME TARGET (user instruction, 2026-08-02): agent-bus ships as a container under Docker Compose. The CONTAINER's builder image pins the Go toolchain, NOT this workstation's ambient go1.19.4 -- CORE-1's go1.19 pin was a dev-box artifact, not a permanent constraint (see CLAUDE.md's "Runtime target: Docker Compose" section). So a bump past go1.19 is no longer something to work around: choose the version the ratchet library actually needs (crypto/ecdh is go1.20+; a current libsignal-compatible stack may want newer) and state it plainly in DECISIONS.md. This relaxes the Go VERSION only -- invariant 8 (stdlib first, third-party deps need a DECISIONS.md justification) is UNCHANGED.
  
  SEQUENCING: the actual go.mod/toolchain bump and the container builder image pin are owned by the DEPLOY epic's toolchain-bump task, which is explicitly sequenced to land AFTER the in-flight ID/DUR wave completes (that wave is building against go1.19 right now). Coordinate with spec-keeper on ordering rather than bumping go.mod unilaterally from this task -- if this task's dependency needs the newer toolchain to even compile its test vectors, block on the DEPLOY toolchain-bump task rather than bumping go.mod early.
  
  Introduce internal/cryptobox as a NARROW interface over the primitives (keypair generation, X25519 agreement, HKDF, AEAD seal/open, constant-time compare). The point of the narrow interface is that the ratchet code above it does not care which of the spike's options (a)/(b)/(c) won, and swapping the implementation later is a one-package change. Include known-answer/test-vector tests for every primitive -- crypto without test vectors is unverified. NO protocol logic in this task: no X3DH, no ratchet, no wire format. Update DECISIONS.md if the adopted dependency differs in any way from what CRYPTO-1 recorded.
  _Proof: go build ./... && go vet ./... && go test -race ./internal/cryptobox_
- [ ] CRYPTO-4 · CRYPTO-4: Key-distribution endpoint -- server-attested messaging key bundles — auth, P1
  RESCOPED 2026-08-02 per user instruction ("keep it simple, standard sign/verify; encryption later"): this is now a bundle of SIGNING material, not X3DH session-establishment material. GOVERNED BY INVARIANT 9 -- the bus attests bundles by signing them with its own Ed25519 signing key (crypto/ed25519, stdlib, audited); no custom attestation construction. Add the authenticated route that lets an enrolled agent fetch another agent's messaging (signing) key bundle: {fully-qualified <bus-id>.<agent-id>, messaging public key, key_epoch, issued_at}, signed by a bus signing key so the caller can verify the bus is vouching for this binding. Route is keyed by the fully-qualified id (invariant 2). Requires auth (invariant 3): an unenrolled caller gets 401; consider whether roster enumeration via this route needs rate-limiting or scoping. PLUS mandatory TOFU pinning: a recipient pins a peer's messaging public key on first use, in a local pin file; if the bus later serves a DIFFERENT key for a peer whose key is already pinned, that is a hard failure (never an auto-accept, never a silent re-pin) -- this is the actual defence against a malicious bus MITM-ing an established relationship, since attestation alone only protects first contact. Re-pinning requires an explicit human-driven trust command with an out-of-band comparison. key_epoch is bumped by the server on AUTH-4 leave/revocation and invalidates outstanding bundles. Any on-disk record type number or wire protocol version this needs MUST be reserved via POST /api/v1/projects/agent-bus/reservations -- never hand-picked. NOT NEEDED under this rescope (drop if present in any earlier draft): signed prekeys, one-time prekeys, prekey replenishment/exhaustion policy -- those were X3DH-specific and there is no X3DH; this bundle carries exactly one long-lived signing public key per agent.
  _Proof: go test -race -run TestKeyBundle ./internal/httpapi_
- [ ] CRYPTO-6 · CRYPTO-6: Double Ratchet encrypt/decrypt on the direct-message send path — crypto, P3, deferred
  GATED: do not start until CRYPTO-1 (design spike) is done and its DECISIONS.md entry is recorded -- the spike chooses the crypto library/primitives, the audit-log trade-off, the ratchet-state durability model, the broadcast/relay scheme and the key-trust model. Implementing before that decision exists is guessing. Wire the Double Ratchet into the DM path (MSG-3): sender seals, recipient opens, with a DH ratchet step on each direction change and a symmetric-key ratchet per message, giving forward secrecy and per-message integrity/authenticity. Must handle out-of-order and skipped messages by retaining skipped message keys, WITH AN EXPLICIT BOUND on how many are retained (an unbounded skipped-key store is a memory-exhaustion DoS an attacker triggers by claiming a huge counter jump). The message header must carry what the ratchet needs (ratchet public key, previous chain length, message number) and NOTHING the bus should not see. In-memory ratchet state only in this task -- persistence, fsync and recovery are CRYPTO-7, and that ordering is deliberate so the protocol is proven correct before the durability problem is layered on. Tests: long conversation both directions, delayed message delivered after a ratchet step, duplicate/replayed ciphertext rejected, tampered ciphertext/header rejected, decrypt with the wrong session rejected.
  _Proof: go test -race -run TestRatchet ./internal/crypto && go test -race -run TestSendEncrypted ./internal/httpapi_
- [ ] CRYPTO-8 · CRYPTO-8: Broadcast to N agents -- authenticated encryption for the fan-out path — crypto, P3, deferred
  GATED: do not start until CRYPTO-1 (design spike) is done and its DECISIONS.md entry is recorded -- the spike chooses the crypto library/primitives, the audit-log trade-off, the ratchet-state durability model, the broadcast/relay scheme and the key-trust model. Implementing before that decision exists is guessing. The Double Ratchet is strictly PAIRWISE; agent-bus broadcasts to N agents (MSG-2). Implement whichever scheme CRYPTO-1 chose: pairwise fan-out (N ciphertexts, one per recipient session -- keeps full ratchet PFS, costs N seals and N envelope copies) or a Signal-style SENDER KEY group session (one ciphertext, a distribution message per member -- cheaper, but WEAKER forward secrecy, which is precisely why the choice is the spike's and not the implementer's). Must specify and implement membership change: an agent joining or leaving (AUTH-4) forces a rekey, and a departed agent must not be able to read subsequent broadcasts. Document in the task outcome what the bus sees for a broadcast (recipient set, sizes, timing) versus what it cannot. Tests: every recipient decrypts the same plaintext, a non-member cannot, a removed member cannot read post-removal traffic, and a tampered broadcast is rejected by every recipient.
  _Proof: go test -race -run TestBroadcastEncrypted ./internal/httpapi ./internal/crypto_
- [x] CRYPTO-1 · CRYPTO-1: DESIGN SPIKE -- Signal-style E2E crypto for agent-bus (CRYPTO_DEEPDIVE.md + DECISIONS.md) — crypto, P1
  INVESTIGATION ONLY -- NO PRODUCTION CODE. Run this as deep-diver (+ planner for the resulting task ordering), model opus: this is judgment/design work where a wrong call is expensive. Throwaway spikes under /tmp to measure or prototype are fine; nothing lands in internal/ or cmd/ from this task.
  
  DELIVERABLES: (1) CRYPTO_DEEPDIVE.md at the repo root, resolving every remaining tension below with a recommendation and its rationale; (2) a dated DECISIONS.md entry per decision taken (invariant 8 requires this for any third-party dependency, and several of these decisions WEAKEN a standing invariant, which CLAUDE.md says needs an explicit recorded decision); (3) a revised, ordered task list for CRYPTO-2..CRYPTO-12 handed to spec-keeper -- correct/split/supersede those tasks if the design says so rather than forcing the design to fit them.
  
  USER ASK (verbatim, 2026-08-02): "Add to the backlog to add a mechanism to validate messages in the agent script before accepting them. enrolment generates a pub/prv keypair for auth, and for messaging. Use the messaging ratchet library the signal people made for signal /whatsapp to ensure pfs and message integrity / authenticity between agents".
  
  TWO OF THE ORIGINAL SIX TENSIONS ARE NOW SETTLED BY DIRECT USER INSTRUCTION (2026-08-02) -- do not reopen them in the spike:
  
  - TENSION 2 (audit log vs forward secrecy) IS CLOSED. User's words: "ok, log only metadata and routing info." The append-only audit log (invariant 6, DUR-5) records ONLY message id, sequence, sender, recipient(s), bus path traversed, timestamp, size, and a content hash -- never ciphertext, never plaintext. This resolves the plaintext-becomes-unwritable-under-PFS / ciphertext-is-dead-weight conflict by not storing content at all; the hash keeps the log probative without retention. See CRYPTO-11 (implements this) and DUR-5 (already amended to this shape). Nothing left for this spike to decide here.
  - THE GO-VERSION HALF OF TENSION 1 IS ALSO SETTLED. User's words: "this bus is meant to run in a docker compose, so use the applicable version for the ratchet requirements." agent-bus ships as a container under Docker Compose; the CONTAINER's builder image pins the Go toolchain, NOT this workstation's ambient go1.19.4. CORE-1's go1.19 pin was an artifact of the dev box, not a permanent product constraint -- see CLAUDE.md's "Runtime target: Docker Compose" section. This does NOT close the rest of tension 1 below (library choice, roll-your-own vs import, invariant 8 dependency justification) -- only the "are we stuck on go1.19.4" sub-question is closed: no, choose on the merits and say what the container must pin.
  
  THE TENSIONS THIS SPIKE MUST STILL RESOLVE:
  
  1. DEPENDENCY + LIBRARY CHOICE (invariant 8: stdlib first; a third-party dep needs a DECISIONS.md justification). libsignal is Rust/Java/Swift; there is NO official Go binding. Realistic options: (a) an unofficial Go Double Ratchet port -- assess maintenance, audit status, correctness risk; (b) CGO against libsignal -- assess build/cross-compile/static-linking cost and that it drags a Rust toolchain into the build; (c) implement X3DH + Double Ratchet ourselves over stdlib-ish primitives. The go1.19.4-forces-a-third-party-module problem is GONE (see above) -- the container builder image can pin whatever Go version the chosen option needs (crypto/ecdh is go1.20+; a current libsignal-compatible stack may want newer still). State PLAINLY, as the spike's headline conclusion: (i) which option is chosen and why, (ii) the exact Go version the container's builder image must pin as a result, and (iii) that this is a version bump, not a dependency-growth license -- invariant 8 (stdlib first, third-party deps need a DECISIONS.md justification) is UNCHANGED and still governs whether golang.org/x/crypto or a Double Ratchet port gets pulled in. Also state the position on rolling our own crypto vs importing it. NOTE ON SEQUENCING: the actual go.mod/toolchain bump and container builder image are owned by the new DEPLOY epic's toolchain-bump task, which is explicitly sequenced to land AFTER the in-flight ID/DUR wave completes (that wave is building against go1.19 right now) -- this spike recommends the version, it does not perform the bump.
  
  2. [CLOSED -- see above.]
  
  3. RATCHET STATE vs DURABILITY (invariants 4 and 5: nothing acked before durable; disk is the truth, recovery replays the store). Double Ratchet state is MUTABLE PER-SESSION state that advances with every message -- it is emphatically NOT append-only. If ratchet state is LOST on crash the session breaks; if it is REPLAYED/rolled back on recovery you get KEY AND NONCE REUSE, which is a catastrophic AEAD failure, not a hiccup. Specify: where ratchet state lives, how it is written and fsynced relative to the two-phase message commit (does the state advance commit atomically with the message?), what recovery does with a message whose ratchet step was committed but whose send was not (and vice versa), how skipped/out-of-order message keys are stored and bounded, and how replay of the WAL is prevented from re-advancing or rewinding a ratchet. Name the crash-injection tests that would prove it. This is a first-class durability problem, not an afterthought.
  
  4. BROADCAST AND RELAY. The Double Ratchet is strictly PAIRWISE. agent-bus does BROADCAST to N agents and CROSS-BUS RELAY. Signal solves groups with Sender Keys, which have DIFFERENT and WEAKER PFS properties than the pairwise ratchet. Specify how a broadcast is authenticated and encrypted (pairwise fan-out with N ciphertexts vs sender-key group session -- state the cost/PFS trade-off and how membership change forces a rekey), and specify for relay exactly what an INTERMEDIATE relaying bus can and cannot see: envelope metadata, routing path, sender and recipient fully-qualified ids, sizes and timing are presumably visible; content must not be. Cross-reference the RELAY epic (RELAY-1..5) and MSG-2 (broadcast).
  
  5. IDS, ENROLMENT AND KEY TRUST (invariant 1: server authoritative on every id; invariant 2: ids are fully qualified <bus-id>.<agent-id>; invariant 3: enrolment issues a signed credential). Enrolment ALREADY issues a signed credential (AUTH-1); this adds a SECOND, messaging keypair. Define: which key signs what (auth key authenticates to the bus; messaging identity key signs prekeys and authenticates peers -- do not conflate them), that the server BINDS the messaging public key to the server-minted fully-qualified <bus-id>.<agent-id> so a client can never assert its own identity, and -- the crux -- HOW AN AGENT FETCHES AND TRUSTS ANOTHER AGENT'S MESSAGING PUBLIC KEY: server-attested (the bus signs the key bundle, so the bus is a trusted introducer and a malicious bus can MITM) vs trust-on-first-use with a safety number/fingerprint an agent can compare (and what changing keys must do to an established session). State the residual threat model plainly: what does a compromised bus get, what does a compromised peer get, what does an offline attacker with the WAL get. Note the AUTH epic OVERLAPS -- cross-reference AUTH-1/AUTH-2/AUTH-3 rather than duplicating them, and say which AUTH tasks need their descriptions amended.
  
  6. AGENT-SIDE VALIDATION IN THE WRAPPER (invariant 7: agents never hand-write HTTP; every capability ships its scripts/bus-*.sh wrapper AND its AGENT_PROTOCOL.md entry in the SAME task). The user explicitly asked that the AGENT SCRIPT validate a message BEFORE ACCEPTING it. Shell cannot do X25519/AEAD, so the wrapper must shell out to a helper -- specify it (e.g. an `agent-bus verify` / `agent-bus open` subcommand of the same Go binary), its stdin/stdout contract, its exit codes, where the agent's private keys live on disk and with what permissions, and what 'reject' looks like to the calling agent (non-zero exit + nothing printed to stdout, so a naive wrapper cannot accidentally pass unverified content through). Cover the failure modes: bad MAC, unknown sender, no session, out-of-order/skipped, replayed message, and key-changed-since-last-seen.
  
  RESERVATIONS: Any on-disk record type number or wire protocol version this needs MUST be reserved via POST /api/v1/projects/agent-bus/reservations -- never hand-picked. The spike should ENUMERATE which record types and which wire protocol version bumps this design will need, so they can be reserved before implementation starts.
  
  OUT OF SCOPE: writing any of the implementation. CRYPTO-2..CRYPTO-12 carry that.
  _Proof: test -s CRYPTO_DEEPDIVE.md && grep -q 'Message auth/integrity only' DECISIONS.md_
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

- [~] DEPLOY-2 · DEPLOY-2: docker-compose.yml -- single bus, named volume, healthcheck — deploy, P1, in progress
  docker-compose.yml running a single agent-bus service built from the Dockerfile (DEPLOY-1): a named volume mounted at the container's data-dir (so `compose down` without `-v` preserves durable state per invariants 4/5), a healthcheck wired to the existing `/healthz` route (interval/timeout/retries tuned so `docker compose ps` and `depends_on: condition: service_healthy` are meaningful), and configuration passed through the EXISTING flags/env the binary already accepts -- no new config surface invented here. Document the compose invocation (`docker compose up -d`, `docker compose logs -f`, `docker compose down`) in a short README section. Depends on DEPLOY-1 (Dockerfile).
  _Proof: docker compose -p agentbus-proof up -d --build && sleep 8 && docker compose -p agentbus-proof ps --format json | grep -q "\"Health\":\"healthy\"" && docker compose -p agentbus-proof exec -T agent-bus wget -q -O - http://127.0.0.1:8080/healthz && docker compose -p agentbus-proof down -v_
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
- [~] None · DEPLOY-2-FU-CONTAINERNAME: docker-compose.yml hardcodes container_name, collides with any other compose project using the same name — deploy, P1, in progress
  docker-compose.yml:80 sets `container_name: agent-bus` unconditionally. Docker container names are global (not project-namespaced), so `docker compose -p <any-other-project> up` for this same file collides with any OTHER running container also named agent-bus -- including, on this box, the live production `agentbus` compose project (its service container is also named agent-bus). Reproduced 2026-08-02: `docker compose -p agentbus-proof up -d --build` from a clean commit failed cleanly with `Conflict. The container name "/agent-bus" is already in use` while the live agentbus project was running -- Docker refused rather than clobbering anything, so no data was harmed, but it means a project-name-isolated proof_cmd (see DEPLOY-2s patched proof_cmd) still cannot succeed while the live deployment is up. Fix: drop the `container_name:` line (or template it off `${COMPOSE_PROJECT_NAME}`) so compose derives a project-scoped name the normal way.
  _Proof: grep -n "container_name" docker-compose.yml — expect no match (or a project-templated value), then docker compose -p agentbus-proof up -d --build succeeds while another compose project using this same file is already running_
- [x] DEPLOY-1 · DEPLOY-1: Dockerfile -- multi-stage build, pinned Go builder, minimal runtime image — deploy, P1
  Multi-stage Dockerfile: a builder stage on a PINNED Go image tag (pin the exact digest/tag actually used by go.mod's current toolchain -- go1.19 today, until DEPLOY-4 bumps it; re-pin when DEPLOY-4 lands, do not silently float to `latest`), a minimal runtime image (distroless or alpine -- justify the choice in DECISIONS.md per invariant 8), and a non-root user for the runtime stage (never run agent-bus as root in the container). Declare the data-dir as a VOLUME: durability (invariants 4/5/6) lives there, and it must survive `docker compose down` (without `-v`) and a container replace/restart. Wire the existing CLI flags/env the binary already accepts (see cmd/agent-bus -h) rather than inventing container-specific config. This is an ENABLER task, independent of the toolchain bump (DEPLOY-4) -- build against whatever go.mod currently pins; re-pin the builder image when DEPLOY-4 lands.
  _Proof: docker build -t agent-bus:test . && docker run --rm agent-bus:test -h_

### EPIC DOCS — Documentation

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
- [x] DOCS-1 · DOCS-1: README.md + DECISIONS.md seed — docs, P0
  README.md -- what agent-bus is, quickstart (build, run one bus, enrol two agents, exchange a message via the wrappers). DECISIONS.md -- seeded with its append-only-dated-entry convention and a placeholder for the enrolment signing-scheme decision. Written early so later tasks have somewhere to record decisions.
  _Proof: test -s README.md && test -s DECISIONS.md_
- [ ] DOCS-3 · DOCS-3: CONTRACTS.md -- route/flag/env-var/record-type table — docs, P1
  A single table of every route, CLI flag, env var, and durable record type, with the convention that every future task updates it in the same commit that changes any of those surfaces (CLAUDE.md step 9).
  _Proof: test -s CONTRACTS-CLI.md && test -s CONTRACTS-HTTP.md && test -s CONTRACTS-ONDISK.md && test -s CONTRACTS-AGENT.md && grep -qF "CONTRACTS-CLI.md" CONTRACTS.md && grep -qF "CONTRACTS-HTTP.md" CONTRACTS.md && grep -qF "CONTRACTS-ONDISK.md" CONTRACTS.md && grep -qF "CONTRACTS-AGENT.md" CONTRACTS.md_

### EPIC DUR — Durability: WAL, two-phase commit, recovery, audit log

- [ ] None · DUR-4-FU-DOCS: document the WAL recovery surface AND the narrowed invariants 4 and 6 AND at-least-once delivery in PROTOCOL.md/CONTRACTS.md — docs, P0
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
  _Proof: test -f PROTOCOL.md && grep -q 'at-least-once' PROTOCOL.md && grep -q 'always restart' PROTOCOL.md && grep -q 'RepairTail' CONTRACTS-ONDISK.md_
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
- [ ] DUR-12-FU-AUDITUPGRADE · DUR-12-FU-AUDITUPGRADE: version 1 audit logs have no upgrade path -- must land before the audit log ships (blocks DUR-5) — durability, P2
  P2, durability. MUST BE DONE BEFORE THE AUDIT LOG SHIPS (blocks DUR-5). Reviewer P2-3: upgradeV1 is reachable only from wal.Open, which is WAL-only, so a version 1 AUDIT log has no upgrade path and OpenWriter (writer.go:67) now refuses it outright. Harmless today (no KindAudit file exists outside tests) and a live landmine the moment the audit log lands.
- [-] None · DUR-4-FU-TOOLING: Operator tooling for a WAL that refuses to start — durability, P2
  DISSOLVED 2026-08-02 BY USER DECISION -- ALWAYS-RESTART IS THE ESCAPE HATCH.
  
  This task existed because "refuse to start" was the designed answer to several WAL damage classes and
  an operator facing a refused boot had no runbook and no tooling. The user has decided the bus ALWAYS
  restarts (DECISIONS.md, 2026-08-02: *"always be able to restart, prefer to discard messages and/or
  corruption, with logging"*). The decision text says so explicitly: "This also removes the
  permanent-refuse-to-start DoS, and with it the need for the operator escape hatch that was previously
  recommended: always-restart *is* the escape hatch."
  
  The premise is gone, so the task is superseded rather than done -- nothing was built.
  
  WHAT WAS IN IT THAT IS STILL WANTED, AND WHERE IT WENT -- so this is not a silent loss of three good
  ideas:
   - (1) A read-only WAL dumper (offset / index / record-type / length / MAC-ok per frame). Still
     useful, but as an ORDINARY diagnostic, not an emergency tool. It belongs in the merged CLI epic as
     a subcommand (see CLI-6 'log' and CLI-8 'doctor'), NOT as a scripts/bus-*.sh wrapper -- invariant 7
     was amended on 2026-08-02 and the Go CLI replaces the shell wrappers.
   - (2) Counters for tail-repaired / repair-refused / commit-records-discarded-by-repair. This is now
     MORE important, not less: under always-restart the discard is the normal path, and the whole point
     of the decision is that every discard must be OBSERVABLE. It is folded into DUR-11's added scope
     (discard + SPECIFIC log + continue) and into CORE-5 (metrics/inspect endpoint).
   - (3) "A bus that repairs its tail on EVERY boot is the signature of failing media." Still true and
     still worth alarming on; it rides on (2)'s counters and belongs with CORE-5.
  
  DO NOT REVIVE THIS TASK. If the dumper is wanted, file it against the CLI epic.
  
  --- ORIGINAL DESCRIPTION ---
  "Refuse to start" is now the designed answer to several WAL damage classes (see internal/wal/recover.go, DUR-4/DUR-10/DUR-11) and there is no runbook or tooling to diagnose it. Needs: (1) a read-only WAL dumper (offset / index / record-type / length / CRC-ok, one line per frame); (2) metrics/log counters for tail-repaired, repair-refused, and commit-records-discarded-by-repair; (3) an alarm-worthy signature: a bus that repairs its tail on EVERY boot is the signature of failing media. Ship as a scripts/bus-*.sh wrapper per invariant 7. Depends on DUR-9.
- [ ] None · Startup scans the WAL twice (soon three times) -- bound the cost — durability, P2
  Startup replay currently scans the WAL twice: the log.go replay pass and the writer.go open pass. DUR-4 (corrupt-tail detection) adds a third scan. This is fine at small WAL sizes but does not bound startup cost as the log grows. Relates to DUR-7 (snapshot/compaction follow-up), which is the real long-term fix for unbounded replay time -- this task is narrower: either (a) collapse the passes where safe, or (b) explicitly document/measure the cost and defer the real fix to DUR-7, whichever the implementer judges correct after reading the current pass structure post-DUR-4.
  _Proof: go test -bench=BenchmarkWALOpen ./internal/wal_
- [ ] DUR-12-FU-DOUBLEBACKUP · DUR-12-FU-DOUBLEBACKUP: crash between os.Link and os.Rename in upgradeV1 can leave a second .v1-<ns> backup on redo — durability, P3
  P3, durability. Reviewer P2-4: a crash between os.Link and os.Rename in upgradeV1 (recover.go:528) yields a second <log>.v1-<ns> backup on redo. Harmless (hard links to one inode) but it contradicts the "exactly 1 backup" invariant a test asserts; wants a comment or a guard.
- [x] DUR-2 · DUR-2: Two-phase prepare->commit write path — durability, P0
  Implement prepare(record)->commit(id) as two distinct fsynced WAL appends; in-memory state is applied ONLY after the commit record is durable. A response is never sent to the caller until commit-fsync completes (invariant 4). This is the write path every mutating route (enrol, send, broadcast, leave) goes through.
  _Proof: go test -race ./internal/wal/_
- [ ] DUR-4 · DUR-4: Corrupt-tail detection & truncation — durability, P0, blocked
  POLICY REVERSED 2026-08-02 BY USER DECISION -- READ THIS BEFORE ACTING ON ANY OLDER TEXT HERE.
  
  THE SENTENCE THIS TASK WAS FILED ON IS NOW WRONG. It said: "A corrupt record anywhere but the tail is
  a fatal startup error, not a truncation." The user has decided the opposite (DECISIONS.md, 2026-08-02,
  "Availability over retention: the bus ALWAYS restarts"): *"always be able to restart, prefer to
  discard messages and/or corruption, with logging"*. Recovery must ALWAYS reach a running server.
  Damaged records ANYWHERE may be discarded -- each with its own specific log entry. Invariant 6 is
  narrowed: truncation is no longer restricted to a verified-corrupt TAIL. Invariant 4 is narrowed:
  acknowledged data may be discarded when found corrupt (we still never lose it through our OWN write
  path). The defect was never that data was discarded -- it is that the discard was SILENT. Every
  discard must be OBSERVABLE.
  
  ANYONE IMPLEMENTING FROM THIS TASK MUST NOT BUILD THE OLD POLICY. The line that still holds:
  NON-DAMAGE errors -- permission denied, I/O failure, the data-directory lock already held -- stay
  FATAL. Do not turn an unreadable disk into a silently empty bus.
  
  WHERE THE REMAINING WORK LIVES. This task's own code shipped at 6f22a99 and has been rewritten twice
  since (d06c704, dad04aa, c362152). It is kept open only because it was completed over an unresolved
  reviewer CHANGES-REQUIRED and a security PASS-WITH-CONCERNS. Both of those are now resolved or
  re-homed:
    - The reviewer P0 was landed as comment-only corrections at c362152 under DUR-10, which is now DONE
      (reviewer and security gates both ran; that was the whole point of DUR-10).
    - Security's two HIGH findings are DUR-11 (884d3da4), IN FLIGHT, re-scoped to the always-restart
      policy: finding (a) (index-anchored search -- one damaged record must never mass-delete later
      INTACT records) stands as a real bug; finding (b) is no longer an invariant-4 violation, and its
      residual is the SILENCE plus the false "provably never fsynced" doc comments.
    - Security's later MEDIUM (CRC32C is GF(2)-linear, so the completeness "proof" is forgeable by an
      ordinary remote client) is DUR-12, the CRC32C -> HMAC-SHA256 keyed MAC change, holding reserved
      ondisk-format-version=2 and BLOCKED on where the MAC key lives.
    - The "no operator override exists" escalation (c3a27591) is DISSOLVED: always-restart IS the
      escape hatch.
  
  THIS TASK CLOSES WHEN DUR-11 CLOSES. It carries no independent implementation work any more; it is
  the parent record. Do not dispatch an implementer here -- dispatch to DUR-11.
  
  --- ORIGINAL DESCRIPTION, retained for the record. Its last sentence is REVERSED, see above. ---
  During replay, a checksum mismatch or short read at the END of the WAL (the torn record a crash mid-write leaves behind) is detected, logged, and the file is truncated at the last verified-good record boundary -- the ONLY truncation ever permitted (invariant 6). A corrupt record anywhere but the tail is a fatal startup error, not a truncation.
  _Proof: go test -race -run TestWALRepairTail ./internal/wal_
- [ ] DUR-12-FU-VERSIONFLIP · DUR-12-FU-VERSIONFLIP: single-bit version-field flip on a v2 log misidentifies it as v1 and quarantines it — durability, P2
  P2, durability. Reviewer P2-1: a version 2 log whose version FIELD alone flips 2->1 is misidentified as v1, nothing verifies under the v1 codec, and the ErrMACKeyMismatch guard at recover.go:306 is skipped because of !c.isV1() -- so an intact log is QUARANTINED and the bus starts empty. Bytes are preserved and it is logged at ERROR, and it is strictly MORE available than the pre-DUR-12 behaviour (which was fatal), so it is not a regression. Fix: in repairLog, when c.isV1() && HeaderDamaged && !Salvageable, try the v2 header tag first to disambiguate.
- [ ] DUR-12-FU-READONLYKEY · DUR-12-FU-READONLYKEY: read-only fsck paths (ScanAll, Replay) create wal-mac.key as a side effect — durability, P3
  P3, durability. Reviewer P2-2: reader.go:34 (ScanAll) and replay.go:94 (Replay) will CREATE wal-mac.key for a log that exists but is unidentifiable, although both are documented as read-only paths. A read-only fsck should not have a file-creating side effect.
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
- [x] DUR-8 · DUR-8: Exclusive lock on the bus data directory (stop two servers destroying one WAL) — durability, P0
  There is NO lock on the bus data directory today. `grep -rn 'flock\|LOCK_EX\|lockfile' --include=*.go` over the whole source returns exactly ONE hit and it is a comment, not an implementation: internal/wal/log.go:274. That comment block (lines ~268-276) already states the problem in the code's own words: "THIS IS NOT A LOCK, and must not be mistaken for one. It only catches a change inside the window between the two passes; two servers started on the same data directory can both replay the same bytes, both agree, and then both append at the same offsets, which destroys the log. Excluding a second process needs a real lock on the data directory (an flock held for the Log's lifetime) and is a follow-up." So this is a known, documented, unimplemented gap. Today the only thing preventing two servers colliding on one data dir is a convention line in CLAUDE.md ("Never run two agents against the same bus data directory") enforced by nothing in code.
  
  Why P0: the failure mode is silent, unrecoverable, and corrupts the append-only durable store that invariants 4, 5 and 6 (nothing acked before durable; memory is serving copy, disk is truth; append-only audit log) all rest on. There is no recovery path from two interleaved writers landing at the same offsets -- the audit trail is destroyed, not merely wrong.
  
  Scope:
  - An exclusive lock acquired at startup BEFORE replay begins, held for the Log's lifetime, released on clean shutdown.
  - A clear, actionable error when another process already holds it (name the data dir in the error; do not just fail obscurely).
  - Explicitly state and TEST the stale-lock-after-crash behaviour. An flock is released by the kernel when the holding process dies, so a crash should leave no stale lock -- but the task must ASSERT that rather than assume it. A durability claim needs a kill test: SIGKILL a holder, then prove a fresh process can acquire the lock.
  - A test that a second Open on the same dir fails while the first is live.
  
  Sequencing: BLOCKED until DUR-4 lands. The lock goes in internal/wal/log.go Open, which the DUR-4 agent is editing right now. Do not start this while DUR-4 is in_progress.
  _Proof: go test -race -run "TestDirLock|TestAcquire|TestSecondAcquireFailsFast|TestReadHolderPID|TestBusyError" ./internal/dirlock && go test -race -run TestRunRefusesALockedDataDir ./cmd/agent-bus_
- [ ] DUR-12-FU-CONTRACTS · DUR-12-FU-CONTRACTS: land the six CONTRACTS-ONDISK.md rows deferred from DUR-12 — docs, P2
  P2, docs. Land the six CONTRACTS-ONDISK.md rows quoted in the DUR-12 kind=report note (author feature-runner) on task cbc9ab0c-3b34-48d0-acd8-5eabd4dc4a02: on-disk format version 1 vs 2, wal-mac.key file (mode 0600, 64 lowercase hex chars, wal.MACKeyFileName), exported constant value changes (wal.FormatVersion 1->2, wal.FileHeaderSize 16->48, wal.FrameHeaderSize 20->48, new wal.MACSize=32), new errors.Is-checkable sentinels (wal.ErrMACKeyMissing, wal.ErrMACKeyMalformed, wal.ErrMACKeyMismatch), startup failure modes that REFUSE TO START, and upgrade artefacts left in the data dir (<log>.upgrade, <log>.v1-<unix-nanos>). State in the description that reviewer raised this as P1-1 and that CONTRACTS-ONDISK.md:12 still reads "None yet -- no durable store, no WAL record types...", which is now false. proof_cmd must GLOB so it survives any further CONTRACTS split, and it is CONFIRMED RED right now (exit 1, verified before filing): grep -q 'wal-mac.key' CONTRACTS*.md && grep -q 'ondisk-format-version = 2' CONTRACTS*.md && echo CONTRACTS_ONDISK_OK
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
- [x] DUR-9 · DUR-9: Wire the WAL into server startup (open, replay, hold for process lifetime, expose to handlers) — durability, P0
  THE DURABILITY PLANE IS NOT WIRED TO THE SERVER. Verified 2026-08-02: `grep -rn 'internal/wal' cmd/ internal/httpapi/` matches only a COMMENT in cmd/agent-bus/main.go:165 -- there is no import; `wal.Open` has ZERO non-test callers in the whole repo; and internal/httpapi/server.go:101-102 registers exactly two routes (/healthz, /v1/info) beside a well-tested WAL library that no request path touches. DUR-1..DUR-4 are all `done` and NONE of their behaviour is live in the binary. That is the single biggest gap between what the backlog claims and what the process does.
  
  SCOPE (this task only wires what already exists -- do NOT add new WAL features):
  1. Server startup opens the WAL for the configured -data-dir exactly once (wal.Open), holds the *wal.Log for the process lifetime, and closes it on the SIGINT/SIGTERM shutdown path already in main.go.
  2. Startup REPLAYS on open and applies the recovered state before the listener starts accepting -- serving must never begin from an unreplayed store (invariant 5: memory is the serving copy, disk is the truth).
  3. A failed open/replay is a FATAL startup error with a non-zero exit and a clear operator message -- never a silent start with an empty store. The one permitted exception is the verified torn tail DUR-4 already repairs.
  4. The Log is exposed to handlers (a field on the httpapi Server / a narrow interface), so later epics (MSG, AUTH, IDEM, SIGN) have a durable store to commit into. No handler needs to USE it in this task.
  5. Startup logs, at info, the data dir, the number of records replayed and the repair outcome.
  
  GATED ON DUR-8 (exclusive data-dir lock, in flight 2026-08-02). Both edit cmd/agent-bus/main.go startup, and the ORDER is load-bearing: acquire the exclusive data-dir lock FIRST, then open the WAL -- opening a WAL a second process already holds is exactly the corruption DUR-8 exists to prevent. Do not start this until DUR-8 is done, or you will collide in main.go and get the ordering backwards.
  
  NOT IN SCOPE: the audit log (DUR-5), snapshots (DUR-7), any new route, any message being written. This is wiring.
  
  PROOF NOTES: the proof_cmd is non-vacuous BY CONSTRUCTION and FAILS TODAY (verified: proof-check.sh --quiet -> verdict=FAIL exit=1, stops at clause 1 because main.go has no wal import). Clause 2 is the DUR-3 anti-vacuity guard (assert at least one test actually RUNs before trusting the run). The last clauses are the invariant-7 end-to-end check: a real server brought up through scripts/bus-serve.sh must leave a non-empty bus.wal in its data dir -- 'the handler is written' is not the same as 'a running server does it'. Uses an isolated AGENT_BUS_RUN_DIR/port, never the tracked data/ dir.
  _Proof: grep -q '"github.com/dodgymike/agent-bus/internal/wal"' cmd/agent-bus/main.go && test $(go test -run TestServerOpensWALOnStart -v ./... 2>&1 | grep -c "=== RUN") -gt 0 && go test -race -run TestServerOpensWALOnStart ./... && rm -rf /tmp/agent-bus-dur9 && AGENT_BUS_RUN_DIR=/tmp/agent-bus-dur9 AGENT_BUS_LISTEN=127.0.0.1:8091 bash scripts/bus-serve.sh start && test -s /tmp/agent-bus-dur9/data/bus.wal && AGENT_BUS_RUN_DIR=/tmp/agent-bus-dur9 AGENT_BUS_LISTEN=127.0.0.1:8091 bash scripts/bus-serve.sh stop_
- [ ] None · Stale references to deleted test name TestServerOpensWALOnStartRefusesACorruptLog in DECISIONS.md and AGENT_LOG.md — durability, P2
  DECISIONS.md:508 and AGENT_LOG.md:173 still name the test TestServerOpensWALOnStartRefusesACorruptLog, which tested the OLD refuse-to-start-on-corruption policy. That test was deleted/replaced by TestServerQuarantinesACorruptLogAndStartsAnyway (cmd/agent-bus/wal_startup_test.go:315), which asserts the current (2026-08-02, availability-over-retention) policy. Fix: update both references to name the current test, keeping the surrounding historical narrative intact (do not rewrite history, just correct the pointer to a test that still exists). NOTE: SPEC.md:1190 has the same stale reference but SPEC.md is a generated mirror of the Spec Server -- do NOT hand-edit it as part of this task; it will self-correct once the underlying task text is fixed and the mirror is regenerated, or via its own text update if it is carried directly in a task description.
  _Proof: ! grep -rn "TestServerOpensWALOnStartRefusesACorruptLog" DECISIONS.md AGENT_LOG.md_
- [~] DUR-11 · DUR-11: Close security's two OPEN HIGH truncation holes -- anchor the tail veto on record INDEX, and stop truncating a checksum-failing LAST acknowledged record — durability, P0, in progress
  RE-SCOPED 2026-08-02 BY THE USER DECISION "THE BUS ALWAYS RESTARTS" (DECISIONS.md, 2026-08-02, section 1).
  STATUS DELIBERATELY UNCHANGED -- a feature-runner is in flight against this task under exactly the policy
  below. Read this whole section before the historical text further down, which was written against the
  OLD refuse-to-start policy and is retained only as the record of how the findings were discovered.
  
  THE POLICY THIS TASK NOW IMPLEMENTS.
  
  (a) FINDING (a) STANDS AS A REAL BUG, unchanged and still P0. One damaged record must never cause the
      MASS DELETION of later records that are themselves INTACT. The veto's forward search must be
      ANCHORED ON RECORD INDEX, not on end-of-file. Security's probe: one flipped bit in a mid-file
      length field plus one junk byte at EOF deleted 8 committed records, NextIndex 41 -> 33, silently.
      Anchoring on EOF gives ZERO protection in precisely the case RepairTail exists for -- a genuine
      torn tail -- because the veto only fires when the file ends exactly on a record boundary.
  
  (b) FINDING (b) IS NO LONGER AN INVARIANT-4 VIOLATION. Discarding a checksum-failing LAST record is
      now SANCTIONED behaviour: "always be able to restart, prefer to discard messages and/or
      corruption, with logging". Invariant 4 is narrowed accordingly -- acknowledged data may be
      discarded when it is found corrupt; we do not lose acknowledged data through our OWN write path,
      but we will not hold the bus hostage to damaged media.
      THE REMAINING DEFECT IN (b) IS THE SILENCE AND THE FALSE DOC COMMENTS, NOT THE DISCARD.
      - Every discard must be OBSERVABLE: a specific log record naming what was discarded (offset,
        record index, record type, byte count, and why), not a bare boolean or a silent success.
      - The doc comments that claim the discard is "provably" of a never-fsynced record are FALSE and
        must go. Reviewer flagged them as P0 on DUR-4; the implementer already narrowed the worst of
        them at c362152, but the claim must not survive anywhere: "the frame is torn" does NOT imply
        "its fsync never completed". The code and the comments must agree.
      There is no longer a design call to make here and no DECISIONS.md entry is owed for it -- the user
      has decided. Do NOT re-open the refuse-vs-truncate debate.
  
  (c) ADDED SCOPE -- CONVERT EVERY DAMAGE-CLASS REFUSAL INTO DISCARD + SPECIFIC LOG + CONTINUE.
      Recovery must ALWAYS reach a running server. Sweep internal/wal (RepairTail, truncatableTail,
      inspectTail, and every error path that today propagates out of wal.Open as fatal, plus the
      fatal-on-repair-refusal handling in cmd/agent-bus/main.go) and turn each DAMAGE-class error into:
      discard the damaged record(s), log loudly and specifically what was discarded, keep running.
      Truncation is no longer restricted to a verified-corrupt TAIL (invariant 6 narrowed): damaged
      records ANYWHERE may be discarded -- with a log entry EACH.
      THE LINE, AND IT MATTERS: NON-DAMAGE ERRORS STAY FATAL. Permission denied, an I/O failure, the
      data-directory lock already held, a missing/unwritable data dir -- these are not damaged records
      and must still refuse to start with a clear operator message and a non-zero exit. Do not turn an
      unreadable disk into a silently empty bus. Note cmd/agent-bus/wal_startup_test.go currently has
      TestServerOpensWALOnStartRefusesACorruptLog, which asserts the OLD policy for a garbage file
      HEADER -- decide explicitly whether a bad file header is damage (discard/reinitialise + log) or a
      non-damage refusal, say which in the commit message, and make the test assert whichever you chose.
      This also removes the permanent refuse-to-start DoS, and with it the operator escape hatch that
      was previously recommended: always-restart IS the escape hatch (DUR-4-FU-TOOLING is superseded).
  
  OUT OF SCOPE, EXPLICITLY: the CHECKSUM SCHEME and the ON-DISK FORMAT. CRC32C is being replaced by an
  HMAC-SHA256 keyed MAC under a separate P0 task holding the reserved ondisk-format-version=2. Do not
  touch format.go's checksum construction, do not bump FormatVersion, and do not try to fix the
  GF(2)-linearity forgery here. Expect the torn-tail heuristic to get SIMPLER, not more complex, once a
  strong MAC can distinguish damage from truth -- so do not build elaborate new heuristics that the MAC
  task will have to unwind.
  
  TESTS. Keep TestCrashInjectionSingleBitCorruptionSweep (internal/wal/crash_injection_test.go) green
  and EXTEND the net: the torn-tail-PLUS-mid-file-corruption combination that finding (a) exploits has
  no coverage today, and every new discard path needs a test asserting the SERVER STILL STARTS and the
  specific log line was emitted. A discard with no log line is the bug, so assert the log, not just the
  absence of an error. Needs the mandated reviewer AND security gates.
  
  --- HISTORICAL TEXT, retained as the discovery record. Its "DESIGN CALL REQUIRED" paragraph and its
  --- refuse-to-start framing are SUPERSEDED by the policy above. ---
  
  FILED BY spec-keeper so two DEMONSTRATED, STILL-OPEN silent-data-loss findings are not lost inside a task that is already marked done. Source: the security agent's kind=response on DUR-4 (PASS-WITH-CONCERNS, 2026-08-02T14:13:06), which was posted BEFORE DUR-4 was flipped done and was never resolved or waived. Both findings were reproduced with probes against a /tmp copy, not argued from the code. CRITICALLY, security re-ran its probes against the WORKING-TREE fix (the laterRecordInTail veto that DUR-10 covers) and both holes SURVIVE it -- DUR-10 is a strict improvement but does NOT close these.
  
  FINDING (a) HIGH -- the veto's anchor is the wrong thing. laterRecordInTail only fires when the file ends EXACTLY on a record boundary, i.e. when there is NO torn tail. Probe: one flipped bit in a mid-file length field PLUS one junk byte appended at EOF (or one byte truncated off) => 8 committed records deleted, NextIndex 41 -> 33, no error, Open+Replay succeed silently. RECOMMENDED FIX (security's): anchor the forward search on the record INDEX, not on end-of-file.
  
  FINDING (b) HIGH -- a checksum-failing LAST record is assumed torn. A single flipped bit in the PAYLOAD of the final record -- a complete, fsynced, ACKNOWLEDGED record -- is byte-indistinguishable from a torn write and is truncated away. Probe: replay applied 2 -> 1, NextIndex 5 -> 4.
  
  SEQUENCING: DUR-10 is now DONE (review debt paid; reviewer CHANGES-REQUIRED comment-only, security PASS-WITH-CONCERNS, comment fixes landed at c362152). Proof command validated by scripts/proof-check.sh: verdict=PASS class=test tests_run=60 top_level=17 skipped=1 failed=0 empty_pkgs=0 (re-run 2026-08-02 against HEAD) -- it is a real net today, and must be EXTENDED by this task rather than merely kept passing.
  _Proof: go test -race -run 'TestCrashInjection|TestWALRepairTail' ./internal/wal_
- [x] None · Startup summary silently omits whole-log quarantine (quarantined/discard_count/discarded_bytes never logged) — durability, P0
  cmd/agent-bus/main.go:275-285 -- the "write-ahead log opened" startup summary logs only rec.Repaired.Truncated and rec.Repaired.Removed as "repaired"/"repaired_bytes". When wal.Open takes the quarantine path instead (Quarantined/DiscardCount/DiscardedBytes set, Truncated left false), the startup summary prints repaired=false repaired_bytes=0 and never mentions the discard at all -- the only record of a start that just ATE AN ENTIRE LOG is internal/wal own error line, which an operator may not be watching. DECISIONS.md 2026-08-02 ("Availability over retention") is explicit that "the defect was never that data was discarded; it is that the discard was SILENT" -- this is that exact defect, one layer up, at the operator-visible summary line. Fix: add quarantined=/discarded_bytes=/discard_count= (or equivalent) fields to the lg.Info("write-ahead log opened", ...) call so a quarantine is as observable in the startup summary as a repair is. Surfaced during DUR-11 follow-up review; do not fold into DUR-11 itself (it is still open, owned by the user).
  _Proof: bash scripts/proof-check.sh 'go test -race -run TestStartupSummaryLogsQuarantineFields ./cmd/agent-bus'_
- [ ] DUR-5 · DUR-5: Append-only message audit log — durability, P0
  A second, separate append-only file (distinct from the WAL) that every message (broadcast + DM) is written to as part of the same commit, independent of the WAL's own record-keeping -- the audit trail invariant 6 calls out explicitly. The audit record is METADATA AND ROUTING INFO ONLY -- message id, sequence, sender, recipient(s), bus path traversed, timestamp, size, and a content hash of the body -- and never the message body itself. The WAL is NOT affected by this change: it still carries whatever it needs to reconstruct state on replay, including bodies if replay requires them; only this separate audit log is metadata-only. Rationale: agent-bus is getting Signal-style end-to-end encryption with forward secrecy (CRYPTO epic); an audit log holding plaintext becomes unwritable the moment PFS lands, and one holding ciphertext the bus can never decrypt is dead weight -- so the audit trail is deliberately a routing/provenance record, not a content archive, and the content hash preserves the ability to prove WHAT was sent without retaining it. Never edited or truncated except by the verified-corrupt-tail rule. Forward-compatibility requirement: the record must be shaped so the CRYPTO epic can add an encrypted-envelope descriptor field later WITHOUT an on-disk format break (e.g. reserve/permit additional optional fields in the JSON payload).
  _Proof: go test -race -run TestAuditLog ./internal/wal_
- [ ] DUR-7 · DUR-7: Snapshot/compaction follow-up (bounds WAL replay time) — durability, P3
  Low-priority follow-up. As the WAL grows unbounded, startup replay time grows with it; add periodic snapshotting of in-memory state plus safe truncation of the WAL prefix the snapshot covers, so recovery time is bounded by (snapshot load + tail replay) rather than full history. Not required for correctness, only for long-run startup latency.
  _Proof: go test -race -run TestSnapshotCompaction ./internal/wal_
- [x] DUR-10 · DUR-10: Review the RepairTail truncation veto -- half is already in `main` UNREVIEWED (landed inside DUR-8's commit d06c704) and the rest has been rewritten since — durability, P0
  VERDICT 2026-08-02 (spec-keeper): THE REVIEW DEBT IS PAID -- this task is (b) satisfied by the gates that actually ran, and it is being completed on that basis. It is NOT (a) an outstanding gate and NOT (c) obsolete: the gates ran, produced findings, and the findings were landed.
  
  EVIDENCE, verified first-hand this pass:
  - `git log --oneline -- internal/wal/recover.go` -> c362152, dad04aa, d06c704, 6f22a99. d06c704 is the never-gated half; dad04aa is the rewrite; c362152 ("WAL: correct comments that claimed a proof where the code has a heuristic (DUR-10)") is the comment-only landing of the reviewer's P0.
  - REVIEWER GATE RAN (kind=response, 2026-08-02 15:06): CHANGES-REQUIRED, COMMENT-ONLY -- "the code is approved, it is strictly safer than what d06c704 left in main, and every finding is a comment or a scope/test-coverage nit, not a code defect". It re-probed DUR-11 finding (a) over 35 cases with zero silent losses, and mutation-tested rather than argued.
  - SECURITY GATE RAN (kind=response, 2026-08-02 15:23): PASS-WITH-CONCERNS, byte-verified against dad04aa, ~345k probe cases. Its NEW MEDIUM finding (CRC32C is GF(2)-linear, so lengthOnlyDamage's completeness "proof" is forgeable by an ordinary remote client) is NOT dropped: it is the direct motivation for the 2026-08-02 decision to replace CRC32C with an HMAC-SHA256 keyed MAC, and it is carried by the MAC task, not by this one.
  - IMPLEMENTER landed the reviewer's P0/P1 comment corrections at c362152 with ZERO executable lines changed (`git diff -U0` had no non-comment +/- lines).
  - Every agent that touched this task posted kind=report + kind=model; reviewer and security also posted kind=response.
  
  WHAT MOVED OUT OF SCOPE, and where it went. The description below still describes "recovery REFUSES TO START rather than cutting" as the designed failure mode. THAT POLICY IS NOW REVERSED by the user decision of 2026-08-02 ("Availability over retention: the bus ALWAYS restarts" -- DECISIONS.md). Converting every damage-class refusal into discard + specific log + continue is DUR-11's scope (884d3da4), in flight. Replacing the CRC32C checksum with a keyed MAC is the MAC task's scope. Neither is a reason to keep this review-debt task open: the debt was "this code reached main without a reviewer or a security gate", and that is now false.
  
  --- ORIGINAL DESCRIPTION FOLLOWS (retained verbatim; read the reversal above before acting on any "refuse to start" language in it) ---
  
  REVIEW CODE THAT IS ALREADY PARTLY IN `main` AND IS STILL MOVING. This task's premise has been
  CORRECTED TWICE -- read this paragraph before anything else. It was originally filed (by spec-keeper on behalf of
  backlog-triage, pass 4b) as "review-and-land an uncommitted fix". That framing is now WRONG on both halves: the code
  is no longer uncommitted, and what is in the tree is no longer the code that was described. See the kind=response
  note of 2026-08-02 for the correction and who is responsible for the original error.
  
  WHAT IS ACTUALLY TRUE NOW (each fact verified first-hand by spec-keeper, commands quoted).
  
  (1) HALF OF IT IS ALREADY IN `main`, COMMITTED WITHOUT ANY REVIEW, UNDER ANOTHER TASK'S TITLE.
  `git show --stat d06c704` -- a commit titled "Exclusive lock on the bus data directory (DUR-8)" -- includes
  internal/wal/recover.go (+152), internal/wal/format.go (+22), internal/wal/doc.go (+8),
  internal/wal/crash_injection_test.go (+616) and internal/wal/recover_test.go (+102). None of that is DUR-8's work.
  `git log --oneline -- internal/wal/recover.go` returns exactly two commits: 6f22a99 (DUR-4) and d06c704. So the
  truncation change rode into main under an unrelated task's title. DUR-8's own agents said so: DUR-8's reviewer note
  records verbatim "Deliberately ignored the in-flight internal/wal/** ... belonging to parallel agents", and DUR-8's
  security audit lists only internal/dirlock files in its scope. THE PRODUCTION WAL CHANGE IN `main` HAS THEREFORE HAD
  NO REVIEWER GATE AND NO SECURITY GATE. That -- not "landing a patch" -- is the debt this task exists to pay.
  
  (2) IT HAS SINCE BEEN SUBSTANTIALLY REWRITTEN AGAIN, AND THAT REWRITE IS UNCOMMITTED.
  `git diff --cached --stat internal/wal/` shows a further recover.go +311/-, recover_test.go +141, doc.go +13
  (staged; `git diff` unstaged for internal/wal is empty, so the working tree == the staged version).
  The function the original description told a reviewer to look at, `laterRecordInTail`, NO LONGER EXISTS. It has been
  refactored into `inspectTail` (internal/wal/recover.go:347), with `tailHasRecordsAfterIt` now at :461 and
  `truncatableTail` at :244; RepairTail is at :118 and calls inspectTail as the second gate at ~:150.
  A REVIEWER MUST REVIEW THE CURRENT WORKING TREE, NOT MERELY THE DIFF AT d06c704. Reviewing d06c704 alone would
  review code that has already been replaced.
  
  THE BUG BEING FIXED (unchanged, P0 -- silent loss of acknowledged records on the append-only log).
  truncatableTail decides from the damaged frame's OWN header. A single flipped bit in a NON-FINAL frame's length
  field that overshoots EOF but stays <= MaxPayloadSize is byte-for-byte the same shape as a genuine torn tail, so
  recovery started SUCCESSFULLY and silently deleted checksum-valid COMMIT records. Reproduced against the pre-fix
  sources at 10 single-bit offsets [17 121 160 236 275 276 408 409 447 448]; at offset 17 recovery served 0 of 4
  acknowledged entries (RepairTail Truncated:true At:16 Removed:573). Violates invariant 4 (nothing acknowledged is
  ever lost) and invariant 6 (truncation only of a verified-corrupt tail).
  
  THE SHAPE OF THE FIX AS IT NOW STANDS (describes `inspectTail`, the CURRENT code, not the superseded
  laterRecordInTail). RepairTail applies inspectTail as a SECOND gate, only AFTER truncatableTail has already said
  "tail-shaped". inspectTail reads the region [at, size) that the cut would discard and applies two proofs:
  (a) lengthOnlyDamage -- recompute the damaged frame's checksum on the hypothesis that its true payload is the bytes
  actually present; if it verifies, the record is COMPLETE and only its length field is corrupt, so it may have been
  fsynced and acknowledged; (b) a forward search for any complete, checksum-verifying record inside the discard region
  whose INDEX continues the file's sequence. Anchoring (b) on record index rather than on end-of-file is a DELIBERATE
  change from the earlier version and the code comments say so. A candidate cap (maxTailCandidates=4096) bounds the
  checksum work because the region is attacker-influenced. Any refusal is logged and returned as a fatal error:
  recovery REFUSES TO START rather than cutting.
  
  WHAT THIS TASK REQUIRES (unchanged in substance; the target has moved).
  (1) REVIEWER GATE on the CURRENT internal/wal/recover.go working tree -- is the veto still strictly additive versus
  6f22a99, is refusing-to-start the right failure mode versus truncating, and does the rewrite from laterRecordInTail
  to inspectTail preserve the strict-subset property that justified landing it at all? The "purely additive, zero
  removed lines in truncatableTail" argument was checked against the FIRST version and MUST BE RE-CHECKED against the
  +311/- rewrite; do not carry it forward on trust.
  (2) SECURITY GATE -- a remote-influenced WAL byte must not be able to turn recovery into either data loss OR a
  permanent startup denial of service. The maxTailCandidates cap and the attacker-influenced-region reasoning in
  inspectTail's comments are squarely in scope.
  (3) ONE clean logical commit of the remaining uncommitted recover.go/recover_test.go/doc.go changes, with a message
  that says plainly that the earlier half landed under DUR-8's title.
  (4) CONTRACTS.md / PROTOCOL.md touch-up only if the described recovery contract moved.
  
  CROSS-REFS. DUR-4 (done at 6f22a99) is the task this file was last completed against, and its reviewer verdict there
  was CHANGES-REQUIRED, still unresolved. DUR-6 (done at e63ced5) owns the TESTS that ride with this code and
  explicitly does NOT cover this production change. DUR-11 (884d3da4-bceb-4ac2-93a2-e147c77f9dca) carries two HIGH
  findings this fix may or may not still leave open -- they were written against laterRecordInTail and must be
  re-probed against inspectTail. Do not let DUR-10's reviewer re-litigate DUR-11's scope.
  
  PROOF RE-VERIFIED against the CURRENT working tree by spec-keeper on 2026-08-02 after the rewrite:
  scripts/proof-check.sh --quiet "go test -race -run 'TestCrashInjection|TestWALRepairTail' ./internal/wal" ->
  verdict=PASS class=test exit=0 tests_run=42 top_level=14 skipped=1 failed=0 empty_pkgs=0. Not vacuous. The permanent
  regression net is TestCrashInjectionSingleBitCorruptionSweep in internal/wal/crash_injection_test.go.
  _Proof: go test -race -run 'TestCrashInjection|TestWALRepairTail' ./internal/wal_
- [ ] None · CONTRACTS.md:55 is stale -- says no WAL record types/wire version exist yet, false as of DUR-1/2/3 — docs, P2
  Verified: CONTRACTS.md:55 still reads "None yet -- no durable store, no WAL record types, no wire protocol version exists in this wave," which is false as of DUR-1/DUR-2/DUR-3 landing: record types 1=PREPARE, 2=COMMIT, 3=ABORT, 4=AUDIT, and ondisk-format-version=1 are all reserved (via the reservations API) and in use in the codebase. Update that section to list them accurately. FLAG: CONTRACTS.md is being edited by another agent right now as part of the parallel DUR wave -- re-read the file fresh before editing to avoid clobbering a concurrent change; this is a targeted one-section fix, not a full rewrite.
  _Proof: ! grep -qF "None yet" CONTRACTS-ONDISK.md && grep -qE "PREPARE[^A-Za-z]*=[^A-Za-z]*1|1[^A-Za-z]*=[^A-Za-z]*PREPARE" CONTRACTS-ONDISK.md && grep -qE "COMMIT[^A-Za-z]*=[^A-Za-z]*2|2[^A-Za-z]*=[^A-Za-z]*COMMIT" CONTRACTS-ONDISK.md && grep -qE "ABORT[^A-Za-z]*=[^A-Za-z]*3|3[^A-Za-z]*=[^A-Za-z]*ABORT" CONTRACTS-ONDISK.md && grep -qE "AUDIT[^A-Za-z]*=[^A-Za-z]*4|4[^A-Za-z]*=[^A-Za-z]*AUDIT" CONTRACTS-ONDISK.md && grep -qi "ondisk-format-version" CONTRACTS-ONDISK.md_
- [x] DUR-1 · DUR-1: WAL record framing + writer — durability, P0
  Define the on-disk WAL record format (length-prefixed + CRC32 checksum per record, monotonic record index) in internal/wal, and implement the append-only writer: Append(record) writes framed bytes and fsyncs before returning. The single building block every other DUR task builds on.
  _Proof: go test -race ./internal/wal/_
- [x] DUR-12 · DUR-12: Replace CRC32C with an HMAC-SHA256 keyed MAC (ON-DISK FORMAT CHANGE, reserved ondisk-format-version=2) -- UNBLOCKED, key lives in data dir mode 0600 — durability, P0
  ON-DISK FORMAT CHANGE. THE RESERVED FORMAT VERSION IS **ondisk-format-version = 2**, allocated
  2026-08-02 from the Spec Server `ondisk-format-version` namespace by spec-keeper (the same namespace
  internal/wal/format.go:14-19 already cites for FormatVersion = 1). DO NOT PICK A DIFFERENT NUMBER AND
  DO NOT LET ANOTHER FORMAT CHANGE REUSE IT. Note ID-2-WIRING-SCHEMA may ALSO need a format bump if it
  chooses Option B -- it must reserve its OWN value. Format changes are ORDERED.
  
  BLOCKED -- AND THE BLOCKER IS A QUESTION THE USER HAS NOT ANSWERED.
  
    WHERE DOES THE MAC KEY LIVE?
  
    A key stored beside the WAL in the data directory defends against the attack that motivated this
    change -- an ordinary REMOTE CLIENT crafting a payload -- but it does NOT defend against an
    attacker who already has DATA-DIRECTORY WRITE ACCESS, because that attacker can read the key and
    recompute any MAC at will. The candidate answers (key file in the data dir at 0600; key outside the
    data dir; key from an env var / operator-supplied at start; OS keyring; derived from a passphrase)
    trade off differently on unattended restart, containerised deployment, key rotation and backup, and
    the choice determines whether a lost key means a bus that cannot read its own log. THIS IS A
    PRODUCT DECISION, NOT AN IMPLEMENTATION DETAIL. Do not start coding until it is answered and
    recorded in DECISIONS.md. Also settle, in the same decision: what happens on a MISSING or WRONG key
    at startup -- under the always-restart policy that is arguably a NON-DAMAGE error (the media is
    fine, the operator misconfigured it) and should stay FATAL rather than discard the entire log.
  
  WHY THIS CHANGE, in one line the implementer must not lose: CRC32C is an error-detecting code, not an
  integrity primitive -- it is UNKEYED and GF(2)-LINEAR, and security DEMONSTRATED end-to-end (DUR-10
  kind=response, 2026-08-02) that an ordinary remote client, submitting nothing but printable-ASCII
  JSON in its own message body, could solve for bytes that make a TORN prefix of its own record satisfy
  recovery's completeness "proof". A keyed MAC eliminates that BY CONSTRUCTION: a client cannot compute
  a MAC over a key it does not hold. User decision, 2026-08-02, verbatim: "don't use crc! use a
  hash/hmac/more modern approach. We're not optimising for efficiency, we're optimising for integrity
  and security".
  
  CONSTRUCTION -- INVARIANT 9 IS ABSOLUTE HERE. Use the Go stdlib's high-level API: `crypto/hmac` +
  `crypto/sha256`, via hmac.New / hmac.Equal. NEVER hand-roll, "adapt" or assemble a MAC out of
  primitives; never compare MACs with bytes.Equal or ==. This outranks invariant 8's stdlib-first bias
  and any argument from performance -- broken crypto fails SILENTLY, so "our tests pass" is not
  evidence. No third-party dependency is needed or wanted.
  
  SCOPE.
  1. Replace the CRC32C field in the frame with an HMAC-SHA256 tag over the header-plus-payload bytes
     the CRC covered today (define the covered range EXACTLY, in PROTOCOL.md, and make it unambiguous:
     the length field MUST be inside the covered range or the length-inflation class of attack survives
     the change).
  2. Bump FormatVersion 1 -> 2 in internal/wal/format.go, using the RESERVED value above.
  3. A RECOVERY PATH FOR LOGS ALREADY WRITTEN IN THE CRC32C FORMAT IS MANDATORY. Format changes are
     ordered: a version-1 file must be recognised by its header and either read with the old verifier
     or converted, with the behaviour stated explicitly and tested. Today `verifyFileHeader`
     (internal/wal/format.go:328) rejects any version != FormatVersion outright, so a naive bump turns
     every existing bus into one that will not read its own log. Decide and document the upgrade story
     (read-v1-verify-with-CRC then write v2 going forward, or an explicit one-shot conversion) and
     whether a v2 reader may ever DOWNGRADE-write v1 (it should not).
  4. DUR-4's TORN-TAIL HEURISTIC SHOULD GET **SIMPLER**, NOT MORE COMPLEX. Much of inspectTail /
     lengthOnlyDamage / truncatableTail exists to compensate for a weak, forgeable checksum. Under a
     strong MAC, "this frame verifies" becomes trustworthy, so the plausible-boundary search and the
     completeness "proof" should shrink or disappear. Actively look for code to DELETE here; a change
     that only adds is a sign the opportunity was missed. Coordinate with DUR-11, which is rewriting
     the same functions for the always-restart policy -- DUR-11 lands FIRST and this task must not
     collide with it in internal/wal/recover.go.
  5. Key handling per the decision above, plus rotation: at minimum say what happens when the key
     changes, even if the answer is "not supported yet, and here is the error you get".
  6. CONTRACTS.md + PROTOCOL.md updated with the new frame layout, the covered range, the version-2
     header and the v1 compatibility statement.
  
  TESTS REQUIRED. A negative test that a frame whose payload was altered fails verification; a test
  that a v1 (CRC32C) log is still readable per the chosen compatibility story; a test that a WRONG key
  does not silently pass; and the crash-injection sweep kept green against the new format. Constant-time
  comparison must be asserted by CODE REVIEW (hmac.Equal), not by a timing test.
  
  PROOF. `go test -race -run 'TestWALFrameMACRejectsAlteredPayload|TestWALReadsFormatVersion1Log' ./internal/wal && go test -race ./internal/wal`
  VACUOUS TODAY BY CONSTRUCTION -- neither test exists; they are this task's to write, and they are
  named for the two things that must not be got wrong (forgery rejection, and not bricking existing
  logs). MUST NOT BE COMPLETED ON A VACUOUS VERDICT: scripts/proof-check.sh must report PASS with
  tests_run > 0, and its verdict must be quoted in test_summary.
  _Proof: go test -race -run 'TestWALFrameMACRejectsAlteredPayload|TestWALReadsFormatVersion1Log|TestWALWrongMACKeyIsFatal|TestWALMissingMACKeyOnV1LogIsNotFatal' ./internal/wal && go test -race ./internal/wal_
- [ ] None · Shutdown-timeout path can release the data-dir lock while handlers are still running — durability, P2
  In cmd/agent-bus/main.go waitAndShutdown, when srv.Shutdown exceeds shutdownGrace the code calls srv.Close(), which does NOT wait for in-flight handlers to return. run()'s deferred lock.Release() then drops the data-directory flock while a handler may still be running. Harmless TODAY because no handler writes to the data dir -- but it becomes a real hole the moment DUR-9 puts WAL writes behind those handlers: a second server could acquire the lock while the first is still mid-write. Fix direction: hold the lock until every handler has genuinely returned, or make the data-dir writers refuse to run once shutdown has passed the grace period. Also fold in the DUR-8 security pass's remaining P2: internal/dirlock.Acquire could fstat the opened lock file and require S_ISREG, closing the FIFO/directory-at-the-lock-path cases (both already fail closed today -- FIFO via EINVAL on Truncate, directory via EISDIR -- but the flock is taken on the FIFO first, and O_RDWR-on-a-FIFO-not-blocking is Linux-specific).
  
  Filed by the DUR-8 feature-runner. Related to DUR-9, which will edit the same sequence in run().
  _Proof: TBD by whoever picks this up -- a crash/shutdown-injection test asserting the lock is held until all in-flight handlers return, plus a dirlock test asserting Acquire refuses a non-regular file at the lock path_
- [x] DUR-6 · DUR-6: Crash-injection test suite for the write path — durability, P0
  A test harness that writes, then simulates a crash (kill / truncate / corrupt the file at a chosen byte offset) at each stage of the two-phase write path -- before prepare fsync, between prepare and commit, mid-commit-write, after commit fsync -- and asserts recovery always yields a valid PREFIX of the accepted history: nothing acknowledged is ever lost, nothing unacknowledged is ever visible. The load-bearing evidence for invariants 4/5.
  _Proof: go test -race -run 'TestCrashInjection|TestWALCrash|TestWALReplayCrash' ./internal/wal_
- [x] DUR-3 · DUR-3: Replay/recovery on start — durability, P0
  On startup, replay the WAL from the beginning, reconstructing in-memory state (roster, sequence counters, message store) by applying only records that reached commit -- any prepare without a matching commit is discarded. Uncommitted prepares must never be visible after a restart.
  _Proof: test $(go test -race -run TestWALReplay -v ./internal/wal 2>&1 | grep -c RUN) -gt 0 && go test -race -run TestWALReplay ./internal/wal_
- [ ] DUR-12-FU-V1LAUNDER · DUR-12-FU-V1LAUNDER: v1-format WAL laundering re-signs forged CRC32C records with the real MAC key — security, P1
  P1, security, HIGH (from DUR-12 security gate, VERIFIED BY RUNNING IT). internal/wal/log.go:256-273 branches to the version 1 path on detectFormat alone, and internal/wal/mackey.go:372-374 returns a keyless v1 codec without consulting the key file. So an attacker who can drop a CRC32C-forged version 1 file at bus.wal gets its records re-framed and SIGNED WITH THE REAL MAC KEY -- forging without ever touching wal-mac.key. Capability required is directory w+x (replace a file), which does NOT require reading the 0600 key. It grants no new class of attacker (directory write already allows planting a key+log pair wholesale) but it destroys FORENSICS: forged records become indistinguishable from genuine ones even to someone holding the original key. THE OBVIOUS FIX IS UNSAFE AS STATED AND THE TASK MUST SAY SO: "refuse the v1 path when a key file already exists" strands a legitimate crash-mid-upgrade redo, which leaves exactly that state (key created, log still v1, rename not yet done). A correct fix must distinguish those two, e.g. by staging the key file and only moving it into place after the upgrade rename. Directly relevant to the in-flight Dockerfile/docker-compose work: a bind-mounted volume with loose permissions is the enabler, and MkdirAll does not tighten an existing directory. Reference: PROTOCOL.md section 7 "Known residual".
- [ ] None · Fix WAL recovery reissuing a discarded tail record index (invariant 1 violation, NOT a narrowing) -- BLOCKED on DUR-12 — durability, P0, blocked
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

### EPIC ID — Server-authoritative id minting

- [~] ID-2-WIRING-SEAL-FU-NAMESUFFIXES · ID-2-WIRING-SEAL-FU-NAMESUFFIXES: NameSuffixes has the identical inert-floor-guard defect, unfixed — ids, P0, in progress
  `ID-2-WIRING-SEAL` fixed `internal/ids.Sequence` but deliberately left `internal/ids.NameSuffixes` (agentmint.go) alone as out of scope. `NameSuffixes.RaiseFloor` carries the SAME inert guard -- `if last != 0 && atLeast <= last` at agentmint.go:~298 -- which fires only once a suffix has been issued, so during the window in which the per-name floors are actually derived, every value including a far-too-low one is accepted silently. Its own doc comment already admits this in the same words `Sequence`'s used to ("during the window where the floor is actually derived ... RaiseFloor is therefore a check on a caller that keeps computing floors after it has started serving"), and `go vet` cannot flag a dropped `RaiseFloor` error (proven in ID2_WIRING_DEEPDIVE.md sec 3.4).
  
  This is arguably WORSE than the message-sequence case, and agentmint.go's own doc says why: "re-minting an agent id is worse than re-minting a message id because the agent id is the routing and authorization subject." A reissued agent id means two agents sharing one routing/authorization identity.
  
  The fix is the same shape as ID-2-WIRING-SEAL -- a `Seal()` gate, born unsealed on BOTH `NewNameSuffixes` and `ResumeNameSuffixes`, `NextSuffix` refusing with `ErrFloorUnproven` until sealed, `RaiseFloor` refusing with `ErrFloorSealed` after -- but the per-name shape needs a design call this task must make explicitly: is the seal GLOBAL (one seal for the whole map, which is what a single startup derivation pass implies) or PER-NAME (a name's floor is sealed when that name's derivation completes)? Global is almost certainly right, because names are discovered by the same single replay pass and a per-name seal would let an unknown-at-startup name mint from an unproven floor of 0 -- but say so deliberately rather than by default. Reuse the existing `ErrFloorUnproven` / `ErrFloorSealed` sentinels; do not add parallel ones.
  
  Also update `internal/ids/doc.go`'s `agentmint.go` bullet, which ID-2-WIRING-SEAL deliberately left describing the unfixed state, and add the `NameSuffixes` rows wherever ID-2-WIRING-SEAL-FU-CONTRACTS lands the `Sequence` ones.
  
  Note the interaction with AUTH-3 (restoring the per-name suffix floors from replay): that task is the CALLER that must derive the floors and call `Seal()`, so these two want to be sequenced together.
  
  proof_cmd is VACUOUS TODAY BY CONSTRUCTION -- the named test `TestNameSuffixesRefusesToIssueFromAnUnsealedFloor` does not exist; writing it is the point. This task must NOT be completed on a VACUOUS `scripts/proof-check.sh` verdict; it must report PASS with tests_run > 0.
  _Proof: go test -race -run TestNameSuffixesRefusesToIssueFromAnUnsealedFloor ./internal/ids_
- [x] ID-1 · ID-1: Bus id minting + persistence — id, P0
  On first start with an empty data-dir, generate a bus id (opaque random/ULID-style string), persist it to a file in data-dir, and load the SAME id on every subsequent restart rather than regenerating. Exposed via GET /v1/info. This is the root of invariant 2's `<bus-id>.<agent-id>` namespacing.
  _Proof: go test -race ./internal/ids/_
- [ ] ID-4 · ID-4: Id-counter recovery property test — id, P1
  Cross-cutting test (depends on the WAL replay task): enrol several agents and send several messages, kill the process, restart, and assert every counter (sequence, per-name agent suffix) resumes strictly above its last-issued value -- table-driven across several kill points.
  _Proof: go test -race -run TestIDCounterRecovery ./internal/ids_
- [x] ID-3 · ID-3: Agent id minting `<bus-id>.<name>-<n>` — id, P0
  STATUS CORRECTION 2026-08-02 (spec-keeper) -- NOT COMPLETABLE YET, AND THE REASON IS NOT THE CODE.
  
  The CODE IS IN `main` and its proof PASSES. But the MANDATED reviewer and security gates NEVER RAN,
  and there is no justification for the skip in AGENT_LOG.md. Completing it now would repeat exactly
  the failure DUR-10 exists to record: production code reaching `main` with no gate.
  
  VERIFIED FIRST-HAND THIS PASS (commands quoted, nothing taken on the task's word):
  - `git log --oneline -- internal/ids/agentid.go internal/ids/agentmint.go` -> ONE commit, 10dd7f4
    "Agent id minting <bus-id>.<name>-<n> (ID-3)": internal/ids/agentid.go +239, agentid_test.go +391,
    agentmint.go +389, doc.go +14/-2. `git status --porcelain` is EMPTY -- nothing left uncommitted.
  - `scripts/proof-check.sh 'go test -race -run TestAgentIDMinting ./internal/ids'` ->
    verdict=PASS class=test exit=0 tests_run=80 top_level=9 skipped=0 failed=0 empty_pkgs=0.
    NOT vacuous; 9 top-level TestAgentIDMinting* tests exist in internal/ids/agentid_test.go.
  - Task journal: `main` posted kind=request; spec-keeper posted report+model; implementer posted
    report+model. THERE IS NO kind=response FROM reviewer AND NONE FROM security, and no
    reviewer/test-engineer/security note of any kind. `grep -n 'ID-3' AGENT_LOG.md` -> NO MATCHES, so
    the skip is not justified there either. The likely cause is the session-token kill recorded in
    this task's own first spec-keeper note; the dispatched chain did not survive to its gates.
  
  REMAINING SCOPE OF THIS TASK -- pay the gate debt on ALREADY-COMMITTED code (10dd7f4). No rewrite.
  1. REVIEWER GATE on internal/ids/agentid.go, agentmint.go and doc.go as committed at 10dd7f4.
     Focus: is the `<bus-id>.<name>-<n>` grammar unambiguous under every input the parser accepts
     (invariant 2 -- the '.' separator is what makes cross-bus routing parseable); is the per-name
     counter genuinely durable and monotonic across restart (invariant 1 -- ids are never reused,
     including across restarts); is the suffix spelling pinned to the sequence spelling so the two
     cannot drift.
  2. SECURITY GATE. The short name is UNTRUSTED CLIENT INPUT that ends up inside a routing identifier.
     Focus: id spoofing / separator injection (can a crafted name make one agent's id parse as
     another's, or as a bus-qualified id it does not own), length bounds and the oversized-id
     non-echo path, and any Unicode/normalisation trick that makes two distinct names collide.
  3. AGENT_LOG.md entry for ID-3 (there is none), recording the outcome and the fact that the gates
     ran after the commit rather than before it.
  4. If either gate finds a defect, fix it in a SEPARATE follow-up commit -- do not amend 10dd7f4.
  
  COMPLETION BAR: this task may be completed once both gates have posted kind=response (plus
  kind=report + kind=model) and AGENT_LOG.md carries an ID-3 entry. commit_sha will be 10dd7f4 plus
  any follow-up sha. The proof_cmd below is already validated PASS and does not need to change.
  
  --- ORIGINAL DESCRIPTION (delivered by 10dd7f4) ---
  Server mints the fully-qualified agent id at enrolment: client submits a desired short name, server appends a durable per-name counter suffix (-1, -2, ...) so a reused name never collides with a previous holder, and prefixes the bus id. Client never chooses its own id (invariant 1).
  
  SCOPE NOTE carried forward: CODE-ONLY, like ID-2. No enrolment wiring -- AUTH-1 owns that and is
  in flight separately. Nothing in production calls the minting code yet.
  _Proof: go test -race ./internal/ids_
- [~] ID-2-WIRING-SCHEMA · ID-2-WIRING-SCHEMA: DECIDE and record where the message sequence high-water mark lives on disk (blocks the floor derivation) — durability, P0, in progress
  SPLIT OUT OF ID-2-WIRING (838677e6). This is a DECISION task -- docs only, no code -- and it is the thing actually blocking the floor derivation. See ID2_WIRING_DEEPDIVE.md sec 3.5, 4.2 and 4.4 (committed 2f89fc1) for the ranked options and the disproof test.
  
  THE PROBLEM. ids.Resume(floor) needs the highest sequence EVER WRITTEN TO DISK -- committed, aborted AND dangling. Today the sequence lives inside the caller-written PREPARE body (wal.Entry.Body), the WAL deliberately does not interpret Body, wal.Replay hands its callback COMMITTED entries only, and Recovered exposes no message-sequence high-water mark (Recovered.NextIndex is the WAL RECORD index, a different counter). So there is no way to derive the floor without first deciding WHERE the number lives.
  
  THE DECISION (record it in DECISIONS.md, dated, appended -- the file is contended, add a new section rather than editing lines):
    Option A' -- the WAL offers every PREPARE to an observer during the EXISTING replay pass; the sequence stays in the caller's body and the ids/msg layer decodes it. No on-disk format change; also removes the third startup scan before it is ever added (see task 2a961fcc).
    Option B  -- promote the sequence to a WAL-level field (Entry.Seq / preparePayload.Seq, Recovered.HighestSequence). This IS an on-disk format change and therefore REQUIRES a reservation from the `ondisk-format-version` namespace (NEVER pick the number) plus a downgrade note.
  Record the chosen option, the rejected ones, and the sec-4.4 disproof test.
  
  ORDERING WARNING: the CRC32C -> HMAC-SHA256 MAC task is ALSO an on-disk format change and has ALREADY reserved ondisk-format-version=2. If this task chooses Option B it must reserve its OWN value; format changes are ORDERED and two agents must never share one version number.
  
  BLOCKS: ID-2-WIRING (838677e6) and ID-2-WIRING-OBSERVER.
  
  PROOF. `grep -q 'message sequence high-water mark' DECISIONS.md` -- verdict=FAIL class=file-assertion exit=1 TODAY, which is correct and non-vacuous: it fails precisely because the decision is unrecorded, and flips to PASS when it is written. The chosen wording must therefore contain that exact phrase.
  _Proof: grep -q 'message sequence high-water mark' DECISIONS.md_
- [ ] None · ID-2-WIRING: Derive the sequence resume floor from ALL prepares, never from committed history — id, P0, blocked
  RE-SCOPED 2026-08-02 (spec-keeper) AFTER THE DEEP-DIVE. This task is now T4 of the deep-dive's own breakdown -- 'derive, prove and SEAL the sequence floor in main' -- and it is BLOCKED, not in progress.
  
  WHY. The deep-diver (dispatched as DESIGN INVESTIGATION ONLY) produced ID2_WIRING_DEEPDIVE.md, committed at 2f89fc1, and its verdict is: THE PREMISE IS CONFIRMED BUT THE TASK AS ORIGINALLY FILED CANNOT BE IMPLEMENTED YET, AND IS NOT EXPLOITABLE TODAY. The sequence number lives in the caller-written PREPARE body, no message-body schema exists, and nothing in production mints a sequence at all -- so there is no code path to harden. Implementing as specced would either invent the MSG-epic body schema or change the prepare payload format, and the backlog settles neither. It becomes a genuine P0 the instant the first MSG write path lands.
  
  VERIFIED FIRST-HAND BY SPEC-KEEPER before re-scoping: `git log --oneline -- ID2_WIRING_DEEPDIVE.md` -> 2f89fc1 ("ID2_WIRING_DEEPDIVE.md: the task as filed cannot be implemented yet"), and `git status --porcelain` is EMPTY -- so the INVESTIGATION is committed and NO production code was written, exactly as dispatched. The task's own code deliverable is therefore NOT delivered, which is why this is blocked rather than done.
  
  IT ALSO HAD NO proof_cmd AT ALL, which under the 2026-08-02 process decision ("a missing proof_cmd blocks completion, at least as hard as a vacuous one") made it uncompletable by definition. One is now recorded -- see PROOF below.
  
  THE WORK WAS SPLIT. Three sibling tasks now carry the separable parts; this task is the last of the four and depends on the other three:
    ID-2-WIRING-SEAL     P0 -- Sequence refuses to issue from an unsealed floor. internal/ids only. NO dependencies; startable NOW.
    ID-2-WIRING-SCHEMA   P0 -- DECIDE and record where the sequence high-water mark lives on disk. Docs only. THIS IS THE BLOCKER.
    ID-2-WIRING-OBSERVER P0 -- wal offers every prepare (incl. dangling) to an observer in the existing replay pass. Depends on SCHEMA choosing Option A'.
  
  REMAINING SCOPE OF THIS TASK (T4). In cmd/agent-bus/main.go, after wal.Open, fold the observer over EVERY prepare, construct ids.Resume(floor), RaiseFloor from any other source, then Seal() -- and return a NON-NIL ERROR from run() on ANY failure: the scan errored, a message prepare's body had no seq or a zero seq, RaiseFloor returned non-nil, or Seal() returned non-nil. Log the derived floor at INFO beside the existing "write-ahead log opened" line.
  
  THE LANDMINE THIS TASK MUST COVER: a scan that FAILED must not be indistinguishable from an EMPTY log. Floor 0 from a failed derivation must refuse to start, not resume as a fresh bus. Note this is a NON-DAMAGE error (a derivation we cannot prove), so it stays FATAL and is NOT touched by the 2026-08-02 always-restart decision, which sanctions discarding DAMAGED RECORDS -- not guessing at an id floor. Reissuing a burned id is silent corruption of the audit trail, not a discarded message.
  
  --- ORIGINAL DESCRIPTION (still accurate as the statement of the hazard) ---
  ids.Resume(highestOnDisk) requires the highest sequence EVER WRITTEN TO DISK -- committed, aborted AND dangling. The obvious wiring produces exactly the value that is forbidden: wal.Replay(path, fn) hands fn COMMITTED entries only, and wal.Recovered exposes no message-sequence high-water mark at all (Recovered.NextIndex is the WAL RECORD index, a different counter that also advances for commits and aborts). Concrete break: allocate seq 100, write the PREPARE, fsync it, crash before the COMMIT. 100 is burned and an audit record for it may exist, but replay never surfaces it, the floor comes back 99, and the next send is minted as <bus-id>-100 -- two different messages sharing one id in the append-only audit trail, and any dedup keyed on message id conflates them (invariants 1 and 10). An attacker able to induce crashes in the prepare->commit window chooses what lands on the reissued id.
  
  Cross-reference: ID-2 (a3a5edc4-0a34-4691-b1a6-c1206218ac65, completed CODE-ONLY). internal/ids/sequence.go's doc comment already spells all of this out.
  
  PROOF. `go test -race -run TestRunRefusesAnUnprovableSequenceFloor ./cmd/agent-bus` -- VACUOUS TODAY BY CONSTRUCTION (the test does not exist; it is this task's to write, modelled on cmd/agent-bus/wal_startup_test.go). MUST NOT BE COMPLETED ON A VACUOUS VERDICT: scripts/proof-check.sh must report PASS with tests_run > 0.
  _Proof: go test -race -run TestRunRefusesAnUnprovableSequenceFloor ./cmd/agent-bus_
- [x] ID-2-WIRING-SEAL · ID-2-WIRING-SEAL: Sequence refuses to issue from an UNSEALED floor (the only half implementable today) — ids, P0
  SPLIT OUT OF ID-2-WIRING (838677e6) on the deep-diver's recommendation -- see ID2_WIRING_DEEPDIVE.md sec 4.1 and sec 5/T1, committed at 2f89fc1. This is the ONLY half of ID-2-WIRING that can start immediately: it touches internal/ids ONLY and depends on nothing.
  
  THE DEFECT. internal/ids/sequence.go's RaiseFloor guard is INERT AT STARTUP. It only fires once something has been issued (last != 0), so in exactly the window where the floor is derived, every value -- including one far too low -- is accepted silently. Worse (deep-dive sec 3.4, verified first-hand there): `go vet` CANNOT be made to catch a bare `s.RaiseFloor(x)` that drops the error, so the mistake is invisible to the toolchain.
  
  REQUIRED.
  - Add Seal(), ErrFloorUnproven and ErrFloorSealed to internal/ids/sequence.go.
  - Next() returns (0, ErrFloorUnproven) until Seal() has been called. RaiseFloor returns ErrFloorSealed after.
  - BOTH constructors are born UNSEALED (New and Resume) -- a fresh bus must seal explicitly too, so 'floor 0 because the log was empty' and 'floor 0 because derivation failed' can never be confused.
  - Update sequence.go's doc comment ('When it may be called') and the 5 existing tests.
  - Update CONTRACTS.md.
  
  NOT IN SCOPE: anything in cmd/agent-bus, anything in internal/wal, and the floor DERIVATION itself (that is ID-2-WIRING, which stays blocked on ID-2-WIRING-SCHEMA).
  
  PROOF. `go test -race -run TestSequenceRefusesToIssueFromAnUnsealedFloor ./internal/ids && go test -race ./internal/ids`. VACUOUS TODAY BY CONSTRUCTION -- the named test does not exist yet, which is the point: it is the test this task must write. The deep-diver ran the equivalent test against a scratch prototype and recorded verdict=PASS class=test exit=0 tests_run=5 top_level=1, so the command is executable and non-vacuous the moment the test is written. DO NOT COMPLETE THIS TASK ON A VACUOUS VERDICT; scripts/proof-check.sh must report PASS with tests_run > 0.
  _Proof: go test -race -run TestSequenceRefusesToIssueFromAnUnsealedFloor ./internal/ids && go test -race ./internal/ids_
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
- [x] ID-2 · ID-2: Monotonic sequence allocator (drives message ids) — id, P0
  A durable, strictly monotonic sequence counter (internal/ids) that the WAL commit path advances -- every allocated sequence number is durable before it is handed out. Message ids are `<bus-id>-<seq>`. The counter never re-issues a sequence number: on replay it resumes strictly ABOVE the highest sequence ever written to disk, whether that record reached commit or was only a discarded prepare. Under normal operation (every prepare commits) the committed sequence stream is contiguous; a crash between prepare-fsync and commit-fsync burns one number and leaves a gap in the committed stream -- that gap is expected and correct, not a bug, because reusing the burned number would let two different messages share the same `<bus-id>-<seq>` message id, and the audit log (a superset of committed history) would then contain both under that one id. Counter state is restored by the WAL replay task so a restart never re-issues a previously-issued sequence number.
  _Proof: go test -race ./internal/ids_
- [ ] ID-2-WIRING-OBSERVER · ID-2-WIRING-OBSERVER: wal offers EVERY prepare (committed, aborted AND dangling) to an observer during the existing replay pass — durability, P0
  SPLIT OUT OF ID-2-WIRING (838677e6). See ID2_WIRING_DEEPDIVE.md sec 5/T3 (committed 2f89fc1).
  
  BLOCKED ON ID-2-WIRING-SCHEMA choosing Option A'. If SCHEMA chooses Option B instead, this task is SUPERSEDED and replaced by ID-2-WIRING-HEADER (add Entry.Seq + preparePayload.Seq, expose Recovered.HighestSequence, RESERVE a fresh ondisk-format-version value -- never pick it -- bump FormatVersion, fix replay_test.go:1109's unknown-field fixture, ship a downgrade note; proof `go test -race -run 'TestWALRecoveredHighestSequence|TestWALFormatVersionRefusal' ./internal/wal`).
  
  REQUIRED (Option A' shape). Add wal.ReplayWithPrepares(path, fn, onPrepare); Replay delegates with a nil observer so no existing caller changes. onPrepare fires for EVERY prepare in file order -- committed, aborted and dangling -- BEFORE resolution. The wal package still does not interpret Body; it hands the bytes up. Update CONTRACTS.md and PROTOCOL.md.
  
  THE ASSERTION THAT MATTERS: the observer must see the DANGLING prepare's entry. That is the whole point -- assert a floor of 100 from a log whose only seq-100 record never committed. A test that only observes committed prepares proves nothing.
  
  PROOF. `go test -race -run TestWALReplayObservesEveryPrepare ./internal/wal && go test -race ./internal/wal`. VACUOUS TODAY BY CONSTRUCTION (the test does not exist). The deep-diver's scratch equivalent (TestFloorFromPrepareObserverInOnePass) is proven PASS, so the command is executable once written. DO NOT COMPLETE ON A VACUOUS VERDICT.
  _Proof: go test -race -run TestWALReplayObservesEveryPrepare ./internal/wal && go test -race ./internal/wal_

### EPIC IDEM — Duplicate detection and idempotency (invariant 10)

- [-] IDEM-7 · IDEM-7: Exactly-once application on the relay path -- dedupe on the ORIGIN's identity, complementing (never replacing) RELAY-3 loop prevention — relay, P2
  GATED on IDEM-2; lands with RELAY-2/RELAY-3. WHY THIS IS WHERE IDEMPOTENCY EARNS ITS KEEP (invariant 10, verbatim): "a cyclic peer topology plus at-least-once delivery means duplicates are not an edge case but the normal steady state." A bus with two peers that both peer with a third receives the same message twice as a matter of routine, not as a failure. (1) DEDUPE ON THE ORIGIN'S IDENTITY, NOT THE FORWARDING PEER'S: two different peers legitimately forward the SAME origin message, so keying on the sending peer's own idempotency key would treat them as two messages. The dedupe identity must be the origin bus's message identity -- which per invariant 2 is already globally unambiguous because it is namespaced by bus id -- carried unchanged across every hop. (2) IT MUST NOT BE FORGEABLE BY AN INTERMEDIATE: interacts directly with SIGN-7. If a lying peer can rewrite the dedupe identity, it can split one message into two deliveries (duplicate injection) or collide two messages into one (suppression). Prefer an identity that is inside, or verifiably derived from, SIGN-1's signed bytes, and say explicitly what an intermediate CAN still do -- the traversed bus path is metadata outside the signature (SIGN-7), so loop prevention is an availability mechanism, not a security one. (3) COMPLEMENT, NEVER SUBSTITUTE: RELAY-3's traversed-bus-path check stops a message CIRCULATING; this stops it being APPLIED twice. Neither replaces the other -- a message can arrive twice by two loop-free paths, and a buggy or malicious peer can strip the path. Do not let an implementer delete one because the other exists; state the argument in the code comment and in PROTOCOL.md. (4) The far bus mints its OWN local sequence for its own recipients (SIGN-7), so 'applied once' means one local delivery and one local sequence, not the origin's numbers. (5) RELAY-4's retry/backoff is the duplicate SOURCE this defends against, so test them together: a peer that acks late and retries must not produce a second delivery, including across a restart of the receiving bus.
  _Proof: go test -race -run TestRelayAppliesOnce ./internal/relay -- a cyclic 3-bus topology delivers one message once_
- [-] IDEM-2 · IDEM-2: Durable applied-key store -- committed in the SAME two-phase transaction as the effect, rebuilt by WAL replay — durability, P1
  GATED on IDEM-1. Invariant 10 says the server's memory of applied keys "survives restart (it is part of the recovered state, not an in-memory cache)" -- this task is that guarantee. THE ONE THING THAT MAKES IT CORRECT: the applied-key record MUST be committed in the SAME two-phase (prepare -> commit -> fsync) transaction as the effect it records. If the message commits and the key record does not, a crash in that window plus a client retry produces a DUPLICATE -- precisely the bug idempotency exists to prevent, and it would be invisible in normal testing because the window is small. Do not implement it as a separate write, and do not order it 'after' the effect. (2) STORE THE RESULT, NOT JUST THE KEY: a retry must return the ORIGINAL response (message id, sequence, timestamp), so the record holds the (caller, operation, key) tuple, the payload fingerprint from IDEM-1, the minted result, and the commit time. A key with no stored result cannot satisfy IDEM-4. (3) RESERVE the on-disk record-type number via POST /api/v1/projects/agent-bus/reservations {"namespace":"record-type"} -- never hand-pick it; that is the classic parallel-agent collision, and DUR-1's framing already has neighbours. Bump the on-disk format version the same way if the framing changes. (4) RECOVERY: replay on start rebuilds the applied-key map alongside the rest of the serving state (invariant 5: memory is the serving copy, disk is the truth); recovery must yield a state that is a prefix of accepted history, so a key whose effect was not committed must NOT appear as applied. (5) CRASH-INJECTION TEST IS MANDATORY per CLAUDE.md: kill between prepare and commit, and between commit and ack, then assert what a post-restart retry does. 'The code looks right' is not evidence for a durability claim.
  _Proof: go test -race -run TestAppliedKeyDurability ./internal/store ./internal/wal -- includes a crash-injection case_
- [-] IDEM-6 · IDEM-6: Idempotent enrol, leave, and peer-enrol — auth, P2
  GATED on IDEM-1/IDEM-2. Invariant 10 covers EVERY mutating operation, not just messaging. ENROL is the interesting one: a retried enrolment must return the SAME server-minted agent id and the SAME credential -- it must not mint a second agent. Ids are never reused (invariant 1), so a double-applied enrolment burns an id and leaves a phantom agent in the roster that nothing will ever collect, and the client ends up holding a credential for an identity its peers were never told about. It is also the operation with NO authenticated caller yet, so it uses the alternative key scope IDEM-1 settled (the presented enrolment key, or bus-wide) -- implement exactly that, and make sure the scope cannot be abused by an unauthenticated caller to squat or probe keys. RE-ENROLMENT WITH A DIFFERENT PUBLIC KEY under the same idempotency key is a different-payload violation (IDEM-5), not a retry -- important, because it is also how an attacker would try to take over an identity. LEAVE (AUTH-4): naturally idempotent, but must return success rather than an error on a second call, and must not double-apply revocation side effects (key_epoch bumps in CRYPTO-4 -- a second bump would needlessly invalidate freshly-issued bundles). PEER-ENROL (RELAY-1): two buses enrolling each other concurrently, and a peer retrying after a timeout, must converge on ONE peering, not two half-configured ones. All three persist their applied-key records through IDEM-2's store so they survive restart, and all three keep working after roster recovery (AUTH-3).
  _Proof: go test -race -run TestIdempotentEnrol ./internal/auth ./internal/httpapi_
- [ ] IDEM-12 · IDEM-12: Idempotent send/broadcast -- retries return the original result, no new sequence, no second audit record — core, P1
  GATED on IDEM-10, IDEM-11, MSG-2 (POST /v1/broadcast) and MSG-3 (POST /v1/send). Wire the idempotency key into both routes: on a request whose (agent, key) already has an applied-key record, look it up (IDEM-11) BEFORE doing any normal send work. THE CARVE-OUT THAT MUST NOT BE COLLAPSED (state this in the code comments and the task's own tests, not just in this description): same key + SAME payload is a LEGITIMATE RETRY -- the ack was probably lost in flight. Return the ORIGINAL message id and sequence number verbatim, allocate NO new sequence (invariant 1: sequences are server-minted and never duplicated for one logical operation), write NO second record to the append-only audit log (invariant 6 -- a retry must not create a phantom second entry for what is, from the audit trail's point of view, one logical send), do NOT return an error, and do NOT disconnect the client. This is the entire point of idempotency: punishing a well-behaved retrying client breaks exactly the client doing the right thing. ONLY same key + DIFFERENT payload is a violation, and that path is IDEM-14's job, not this task's -- this task implements the happy path only. 'Same payload' comparison MUST be exact/content-addressed (e.g. compare a hash of the canonical request body), not fuzzily approximated. This task's own narrow test must show: a same-key-same-payload retry of both /v1/send and /v1/broadcast returns identical id+sequence on the second call, and the audit log gains no second entry for it. Broader exactly-once coverage (retry storms, concurrency) lives in IDEM-16/IDEM-17, not here.
  
  --- FOLDED IN FROM THE WITHDRAWN DUPLICATE EPIC (IDEM-4, superseded 2026-08-02), reconciliation pass 2. This content was unique to the withdrawn set and is now in THIS task's scope, not merely offered as a note: (a) THE CONCURRENT IN-FLIGHT CASE, which is where implementations usually break: two requests with the same key arrive concurrently because the client retried before the first ack landed, so the first operation is committed-in-progress and there is NO stored result yet. A naive check-then-act double-applies. Handle it with a single-flight reservation on the key taken inside the SAME critical section that mints the sequence: the second caller either blocks and then returns the first's result, or receives an explicitly retriable 'in progress' response -- pick one and document it. (b) MARK A REPLAYED ACK: give the caller a way to tell a replayed ack from a fresh one (a response field or header) for debugging and for the wrapper's logging -- but the rest of the body must be byte-identical to the original. (c) BROADCAST DEDUPES ON THE OPERATION, NOT ON PER-RECIPIENT DELIVERY: a retried broadcast must not fan out a second time to ANYONE, including recipients whose delivery failed on the first attempt. (d) SIGN-6 INTERACTION: a message rejected for a missing or invalid signature was NEVER applied, so its key must not be recorded as applied -- a corrected resend under the same key is a new operation, not a retry. State this explicitly, or an implementer will record keys before validation and permanently burn them.
  _Proof: go test -race -run TestIdempotentSend ./internal/hub/... ./internal/httpapi/... ; then, against a throwaway bus with its own data dir under /tmp, the same scripts/bus-send.sh call issued TWICE with one idempotency key returns the SAME message id and sequence both times_
- [-] IDEM-1 · IDEM-1: The idempotency-key contract -- format, transport, scope, and which operations require one — core, P1
  Implements the wire half of CLAUDE.md invariant 10 ("duplicate detection and idempotency, everywhere"), recorded in DECISIONS.md via commit bfe391c. FIRST TASK OF THE EPIC -- everything else depends on this contract, so land it before any dedupe logic. SPECIFY AND ENFORCE: (1) TRANSPORT -- one canonical way to carry the key (an `Idempotency-Key` request header is the conventional choice; if it goes in the body instead, say why and be consistent). One place, never two. (2) WHICH OPERATIONS -- every MUTATING operation: enrol, send, broadcast, leave, peer-enrol, and relay ingest. Read-only routes (/v1/agents, /v1/wait, /v1/messages, /healthz, /v1/info) do NOT take one. (3) MISSING KEY IS AN ERROR (4xx), never a server-generated substitute: silently minting a key per attempt would make every retry look new and quietly defeat the entire epic. (4) VALIDATION -- opaque to the server, but bounded: a documented max length (e.g. 128 bytes) and a restricted charset, rejected with a clear error otherwise. Invariant 1 applies with full force: the key is CLIENT-supplied, so it is input to VALIDATE and never an identity to trust -- it must NEVER become, seed, or be derivable into a message id, an agent id, or a sequence number, all of which stay server-minted. (5) SCOPE -- the dedupe identity is the tuple (authenticated caller's fully-qualified <bus-id>.<agent-id>, operation, key), NOT the bare key. Per-caller scoping is required for two reasons: two agents independently choosing "1" must not collide, and without it one agent can burn another's keys and suppress their real messages -- a trivial griefing attack. CALL OUT THE AWKWARD CASE: enrolment has no authenticated caller yet, so its key needs a different scope (the presented enrolment key, or bus-wide) -- decide it here, and hand IDEM-6 the answer. (6) PAYLOAD FINGERPRINT -- define the canonical hash of the request payload that is stored with the key, because invariant 10 turns on distinguishing same-key-same-payload (legitimate retry) from same-key-different-payload (protocol violation). Specify exactly which bytes are hashed, the same way SIGN-1 pins its signed bytes; an ambiguous fingerprint makes the distinction unreliable in both directions. Use crypto/sha256 (stdlib). Document all of it in PROTOCOL.md/CONTRACTS.md via IDEM-9.
  _Proof: go test -race -run TestIdempotencyKeyContract ./internal/httpapi_
- [ ] IDEM-18 · IDEM-18: Wrappers generate the idempotency key ONCE and reuse it across retries, + AGENT_PROTOCOL.md / PROTOCOL.md / CONTRACTS.md — agentif, P1
  GATED on IDEM-10 (key contract) and IDEM-12 (idempotent send/broadcast). Filed 2026-08-02 as the one gap left after merging two concurrently-filed IDEM epics (see the IDEM epic note): IDEM-10..17 cover the server side thoroughly and say nothing about the agent-facing side, which invariant 7 makes non-optional -- agents never hand-write HTTP, so the idempotency key is the WRAPPER's responsibility, not the calling agent's. THE SINGLE MOST LIKELY WAY THIS EPIC SHIPS BROKEN: a wrapper that generates a FRESH key on every attempt. Every retry then looks like a brand-new operation, the server dedupes nothing, duplicates flow exactly as before -- and every server-side test in IDEM-16/IDEM-17 keeps passing, because none of them exercise the wrapper. DELIVER: (1) each mutating wrapper (bus-enrol, bus-send, bus-broadcast, bus-leave, bus-peer) generates ONE key per logical operation, holds it for the whole retry loop, and reuses it verbatim on every attempt. (2) Key generation is real randomness -- no PIDs, no timestamps, no counters that reset across restarts, all of which collide in exactly the multi-process, post-crash situations this epic exists for. (3) A test that FORCES a retry (first attempt killed or refused) and asserts exactly ONE message resulted -- run through scripts/bus-*.sh against a running throwaway bus with its own data dir under /tmp, never hand-written curl: if the wrapper doesn't retry idempotently, the feature doesn't work. (4) AGENT_PROTOCOL.md: agents call the wrapper and do NOT craft keys themselves; what a replayed-ack response means; and that after an IDEM-14 disconnect, reconnecting and retrying with the SAME key is CORRECT, while reusing a key for different content is a protocol violation that will disconnect them again. (5) PROTOCOL.md: the key's transport, the per-agent scope tuple, the payload fingerprint, and -- stated honestly -- IDEM-11's retention window as the BOUNDARY of the guarantee: duplicates are suppressed within the window, and a retry arriving after its key is evicted is applied as a new operation. The system does not provide unconditional exactly-once and the docs must not imply it does. (6) CONTRACTS.md: the header/field, every new error code, the record type IDEM-11 reserved, and any flag/env var bounding retention.
  _Proof: scripts/bus-send.sh forced to retry against a running throwaway bus produces exactly ONE message; grep -q 'idempotency' AGENT_PROTOCOL.md CONTRACTS-HTTP.md PROTOCOL.md_
- [ ] IDEM-17 · IDEM-17: Crash-injection test -- restart mid-retry-window still yields exactly one effect — test, P0
  PRIORITY P0, matching DUR-6's own P0 for the identical reason: per CLAUDE.md's durability discipline, 'the code looks right' is not evidence for a durability claim, and this is IDEM-11's crash-injection test -- kept as its own task, separate from IDEM-16's functional suite, the same way DUR-6 is kept separate from the rest of the DUR epic. GATED on IDEM-11 (durable applied-key store) and reuses the DUR-3/DUR-6 crash-injection harness pattern rather than inventing a second one. Test shape: issue a mutating request (send/broadcast at minimum) carrying an idempotency key, kill the process at a chosen point in the write path -- at minimum BEFORE the applied-key record is committed, and separately AFTER it is committed but before the ack reaches the client (both are the interesting crash points, matching DUR-2's two-phase prepare/commit boundary) -- restart, replay the WAL, then retry the SAME request with the SAME key and payload. Assert exactly ONE effect survives regardless of which crash point was hit: if the crash was pre-commit, the post-restart retry is correctly treated as a FRESH operation (nothing was durably applied) and produces exactly one effect; if the crash was post-commit, the post-restart retry is recognized via the recovered applied-key store and returns the ORIGINAL result with no second effect. THE FAILURE MODE THIS TEST EXISTS TO CATCH: a crash landing between 'operation applied' and 'applied-key record durably written' that, on restart, forgets the key was ever used and lets a retry silently re-apply -- that is a torn record by invariant 10's own definition even though invariant 5's general prefix-of-history property might otherwise look satisfied by the rest of the state. This is exactly the kind of claim CLAUDE.md says an ordinary test suite cannot detect by inspection alone.
  _Proof: go test -race -run TestIdemCrashInjectionRestart ./internal/store/... ./internal/wal/..._
- [~] IDEM-11 · IDEM-11: Durable applied-key store, recovered via WAL replay, with a bounded retention window — core, P0, in progress
  PRIORITY P0 (escalated from the epic default of P1): every other IDEM task's correctness depends on this store actually being durable, and per invariant 5/10 a store that LOOKS idempotent under normal operation but silently reverts to double-applying after a restart is the exact failure mode invariant 10 exists to prevent -- the same reasoning that makes DUR-1/DUR-2 P0 rather than P1. The store answering 'have I already applied this (agent, key) pair, and if so what was the result' MUST be durable, NOT an in-memory-only cache: a restart must not turn a duplicate into a second effect (invariant 10 explicitly, plus invariant 5 -- memory is the serving copy, disk is the truth). GATED on DUR-1 (WAL record framing), DUR-2 (two-phase prepare->commit write path) and DUR-3 (replay/recovery on start, currently in_progress -- do NOT touch DUR-3 itself, this task only depends on its contract). Applied-key records are written through the SAME prepare->commit path as every other durable write (invariant 4) and rebuilt by replaying the WAL on start, exactly like message history and the roster; the write-path half of this task can be developed against DUR-3's documented contract in parallel, but the recovery half cannot land until DUR-3 does. RETENTION is the sharp edge and MUST be decided, not left vague: keys cannot be kept forever (unbounded growth on an append-only durable store is DUR-7's snapshot/compaction problem, multiplied by one record per mutating call ever made). Choose ONE concrete bounded window -- by wall-clock time (e.g. a fixed 24h TTL), by count (e.g. the last N keys per agent), or by sequence range (e.g. keys older than current-sequence-minus-W) -- and record the choice plus its rationale in DECISIONS.md; a configurable-with-no-default is not an acceptable substitute for picking one. Explicitly specify and implement the behaviour for a retry that arrives AFTER its key's window has expired: it MUST FAIL CLOSED -- rejected as unrecognized/expired with a distinct, documented error -- never silently re-applied as if it were a fresh operation and never silently treated as already-seen when it in fact was not. Depends on IDEM-10 for the key shape being stored. BLOCKS IDEM-12 through IDEM-15.
  
  --- FOLDED IN FROM THE WITHDRAWN DUPLICATE EPIC (IDEM-2 and IDEM-3, superseded 2026-08-02), reconciliation pass 2. This content was unique to the withdrawn set and is now in THIS task's scope, not merely offered as a note: (a) SAME-TRANSACTION IS THE LOAD-BEARING REQUIREMENT: the applied-key record MUST commit in the SAME two-phase (prepare -> commit -> fsync) transaction as the effect it records. Not a second write, not ordered 'after' the effect. If the message commits and the key record does not, a crash in that window plus a client retry produces exactly the duplicate this epic exists to prevent -- and it stays invisible in ordinary testing because the window is small. (b) STORE THE RESULT, NOT JUST THE KEY: the record holds the scope tuple, IDEM-10's payload fingerprint, the MINTED RESULT (message id, sequence, timestamp) and the commit time. A key with no stored result cannot satisfy IDEM-12's 'return the original result verbatim'. (c) RESERVE THE ON-DISK RECORD-TYPE NUMBER via POST /api/v1/projects/agent-bus/reservations {"namespace":"record-type"} -- never hand-pick it; that is the classic parallel-agent collision, and DUR-1's framing already has neighbours. Bump the on-disk format version the same way if the framing changes. (d) RECOVERY MUST BE PREFIX-CONSISTENT: a key whose effect was NOT committed must not appear as applied after replay (invariant 5). (e) DERIVE THE RETENTION WINDOW, DO NOT PICK A ROUND NUMBER: it must EXCEED the maximum client retry horizon or the guarantee is a lie in exactly the case that matters. The realistic worst cases to derive it from are a peer reconnecting after an outage (RELAY-4's backoff ceiling) and a long-poll client resuming after a network partition. (f) EVICTION MUST BE CONSISTENT ACROSS MEMORY AND DISK: evicting in memory while the record survives on disk (or the reverse) makes behaviour depend on whether a restart happened since -- the worst kind of intermittent bug. State how eviction interacts with DUR-7 snapshot/compaction: a snapshot must neither silently reinstate evicted keys nor drop live ones. (g) MAKE THE BOUND OBSERVABLE: expose the applied-key count and the oldest-key age wherever CORE-5's inspect/metrics endpoint lands, so the bound is verified in production rather than assumed.
  
  --- CONTRADICTION RAISED BY THE MERGE (2026-08-02), MUST BE RESOLVED BY WHOEVER IMPLEMENTS THIS TASK: the paragraph above says a retry arriving after its key's window expired MUST FAIL CLOSED (rejected as unrecognized/expired), while withdrawn IDEM-3 and the surviving IDEM-18 doc task both state the honest guarantee as 'duplicates are suppressed within the retention window' -- i.e. a retry arriving after eviction IS applied as a NEW operation and produces a second effect. Both cannot ship. THE MECHANISM PROBLEM THAT DECIDES IT: keys are opaque client-supplied strings (IDEM-10), so a server that has evicted a key CANNOT distinguish it from a key it has never seen -- and every legitimate first attempt is a key it has never seen. Fail-closed is therefore only implementable if this task ALSO specifies a mechanism that makes expiry detectable (e.g. a retained eviction watermark plus a verifiable mint-time carried with the key); designing that mechanism is in scope here, assuming it is not. So: either (i) specify that mechanism and keep fail-closed, or (ii) adopt the bounded-window statement and document the boundary honestly. Record the choice and its rationale in DECISIONS.md, and make IDEM-18's PROTOCOL.md wording and IDEM-16's past-the-window test match it -- both of those currently assume (ii).
  _Proof: go test -race -run TestAppliedKeyStore ./internal/store/... ./internal/wal/..._
- [-] IDEM-5 · IDEM-5: Same key + DIFFERENT payload is a protocol violation -- reject, log, and disconnect the offending client — security, P1
  GATED on IDEM-1 (payload fingerprint) and IDEM-2 (stored fingerprint). This is the case the user's instruction targeted: a client reusing an idempotency key for DIFFERENT content is either a serious bug or an attack -- it is trying to make the server believe new content was already-acked content, or to suppress a message by pre-burning its key. REJECT it with a distinct, unambiguous error code (not the generic validation error -- an operator must be able to grep for this), LOG it at a level a human actually sees, with the caller identity, the operation, the key and both fingerprints, and DISCONNECT the offending client. DEFINE 'DISCONNECT' CONCRETELY, because on an HTTP server it is not obvious: at minimum close the connection without keep-alive reuse. DECIDE AND JUSTIFY THE BLAST RADIUS -- does it also invalidate the token / revoke enrolment (AUTH-4), or only drop the connection? The user asked for a disconnect; the choice between 'drop the TCP connection' and 'evict the agent' is a real security/availability trade-off and belongs in DECISIONS.md, not in an implementer's head. THE LINE THAT MUST NOT BE CROSSED: this path must NEVER fire for same-key-same-payload (IDEM-4). Getting that backwards turns a correctness feature into an outage for well-behaved clients, so both directions get their own named test. INTERACTIONS: (a) a disconnected long-poll client (POLL-1) will reconnect immediately -- make sure the rejection is sticky enough not to become a self-inflicted reconnect storm, or is cheap enough not to matter; say which. (b) Replay of an already-accepted SIGNED message by a peer or third party is the related-but-distinct case in invariant 10 -- it is rejected and disconnects the sender too, but its freshness check is SIGN-4's sequence+cursor, not the fingerprint; keep the two paths distinct and cross-reference them rather than merging them.
  _Proof: go test -race -run TestKeyReuseDifferentPayloadDisconnects ./internal/httpapi_
- [ ] IDEM-13 · IDEM-13: Idempotent enrol / leave / peer-enrol — core, P1
  GATED on IDEM-10, IDEM-11, AUTH-1 (POST /v1/enroll), AUTH-4 (POST /v1/leave) and RELAY-1 (peer enrolment). Extends the IDEM-12 discipline to the non-messaging mutating operations invariant 10 names explicitly: enrol, leave, peer-enrol. Same-key-same-request-shape returns the original result rather than erroring or re-minting -- e.g. re-presenting the same enrolment public key with the same idempotency key after a lost ack returns the SAME signed credential/token, not a second one and not an 'already enrolled' error that would force the agent down a spurious re-enrolment path. Same-key-different-content is a violation and is IDEM-14's job, not this task's. Each of the three routes has its own notion of 'same request' worth being explicit about in CONTRACTS.md: enrol's identity is the presented public key; leave's is the agent being revoked; peer-enrol's is the peer bus id plus its offered credential. Because enrol issues a signed credential (invariant 3), pay particular attention to NOT minting a second valid token for a retried enrol -- a client holding two live tokens for one identity is a small security smell worth avoiding even when neither token is individually wrong. Document each route's comparison basis in CONTRACTS.md alongside its existing route entry.
  
  --- FOLDED IN FROM THE WITHDRAWN DUPLICATE EPIC (IDEM-6, superseded 2026-08-02), reconciliation pass 2. This content was unique to the withdrawn set and is now in THIS task's scope, not merely offered as a note: (a) WHY A DOUBLE-APPLIED ENROL IS WORSE THAN A DUPLICATE MESSAGE: ids are never reused (invariant 1), so minting a second agent burns an id permanently and leaves a PHANTOM agent in the roster that nothing will ever collect, while the client ends up holding a credential for an identity its peers were never told about. (b) THE UNAUTHENTICATED SCOPE: enrol has no authenticated caller yet, so it uses the alternative key scope IDEM-10 settles (the presented enrolment public key, or bus-wide) -- implement exactly that, and ensure it cannot be used by an unauthenticated caller to squat or probe another party's keys. (c) RE-ENROLMENT WITH A DIFFERENT PUBLIC KEY under the same idempotency key is a different-payload VIOLATION (IDEM-14), not a retry -- important, because that is precisely how an attacker would attempt an identity takeover. (d) LEAVE MUST NOT DOUBLE-APPLY ITS SIDE EFFECTS: return success (not an error) on a second call, and do not repeat revocation side effects -- notably CRYPTO-4's key_epoch bump, where a second bump needlessly invalidates freshly-issued bundles. (e) PEER-ENROL MUST CONVERGE: two buses enrolling each other concurrently, and a peer retrying after a timeout, must end up with ONE peering, not two half-configured ones. (f) All three operations persist their applied-key records through IDEM-11's store so they survive restart, and all three must still behave after roster recovery (AUTH-3). PRIORITY NOTE: kept at P1 (the withdrawn counterpart was P2); the merge preserves the STRONGER priority of the two batches, never the weaker.
  _Proof: go test -race -run TestIdempotentEnrol ./internal/auth/... ./internal/relay/..._
- [ ] IDEM-15 · IDEM-15: Relay duplicate suppression via idempotency keys — relay, P2
  GATED on IDEM-10, IDEM-11, RELAY-2 (message relay across peers) and RELAY-3 (loop prevention via traversed-bus path). Relay is where idempotency earns its keep: a cyclic peer topology combined with at-least-once delivery (invariant 4's guarantee, extended across the relay plane) means a relayed message can legitimately arrive at a bus by two different paths, or be resent by a peer retrying after a lost ack -- duplicates are the NORMAL steady state here, not an edge case. Apply the same applied-key check IDEM-12 uses to inbound relayed messages: a relayed message carries (or is assigned, at the originating bus) an idempotency key, and a receiving bus that has already applied that key suppresses the duplicate exactly as a duplicate direct send is suppressed -- no second delivery to local agents, no second audit record. STATE THIS EXPLICITLY, because RELAY-3 alone reads as sufficient and it is NOT: RELAY-3's traversed-bus-path loop prevention COMPLEMENTS this and is NEVER a substitute for it. RELAY-3 stops a message from being re-relayed back through a bus it has already visited (a topology-shape defence); it does nothing about a message that legitimately reaches the same bus via two DIFFERENT paths that never revisit any bus, which only idempotency catches. A relay implementation with RELAY-3 but without this task will silently double-deliver in exactly that topology. Priority is P2, matching the RELAY epic's own priority band, since it cannot land before RELAY-2/RELAY-3 exist. Test alongside RELAY-5's crash/loop integration test in IDEM-17, not as a replacement for it.
  
  --- FOLDED IN FROM THE WITHDRAWN DUPLICATE EPIC (IDEM-7, superseded 2026-08-02), reconciliation pass 2. This content was unique to the withdrawn set and is now in THIS task's scope, not merely offered as a note: (a) DEDUPE ON THE ORIGIN'S IDENTITY, NOT THE FORWARDING PEER'S. Two different peers legitimately forward the SAME origin message; keying suppression on the sending peer's own idempotency key treats those as two distinct messages and delivers both. The dedupe identity must be the ORIGIN bus's message identity -- already globally unambiguous per invariant 2 because it is <bus-id>-namespaced -- carried UNCHANGED across every hop. This is the single most important sentence in this task and it was absent before the merge. (b) IT MUST NOT BE FORGEABLE BY AN INTERMEDIATE (see SIGN-7): if a lying peer can rewrite the dedupe identity, it can split one message into two deliveries (duplicate injection) or collide two distinct messages into one (silent suppression). Prefer an identity that is inside, or verifiably derived from, SIGN-1's signed bytes -- and state explicitly what an intermediate CAN still do: the traversed-bus path is metadata OUTSIDE the signature, so RELAY-3's loop prevention is an availability mechanism, not a security one. (c) 'APPLIED ONCE' MEANS ONCE LOCALLY: the receiving bus mints its OWN local delivery sequence for its own recipients (SIGN-7), so the assertion is one local delivery and one local sequence consumed -- not that the origin's numbers are reused. (d) RELAY-4's RETRY/BACKOFF IS THE DUPLICATE SOURCE this defends against, so test them together: a peer that acks late and retries must not produce a second delivery, INCLUDING across a restart of the receiving bus -- which is where the durability of the relay-side applied-key record (IDEM-11) is actually exercised. (e) Put the complement-never-substitute argument in the CODE COMMENT and in PROTOCOL.md, not only in this task, so a later implementer does not delete one defence because the other exists. CROSS-REFERENCE: SIGN-7 point (5) now points at THIS task (it referenced the withdrawn IDEM-7 until the merge).
  _Proof: go test -race -run TestRelayIdempotentSuppression ./internal/relay/..._
- [-] IDEM-9 · IDEM-9: Wrappers generate the key ONCE and reuse it on retry, + AGENT_PROTOCOL.md / PROTOCOL.md / CONTRACTS.md — agentif, P1
  GATED on IDEM-1/IDEM-4. Invariant 7: agents never hand-write HTTP, so the idempotency key is the wrappers' job, not the agent's. THE SINGLE MOST LIKELY WAY THIS EPIC SHIPS BROKEN: a wrapper that generates a FRESH key on every attempt. Every retry then looks like a new operation, the server dedupes nothing, and the whole epic is dead weight while every test that only exercises the server keeps passing. So: each scripts/bus-*.sh mutating wrapper (bus-enrol, bus-send, bus-broadcast, bus-leave, bus-peer) generates ONE key per logical operation, holds it for the entire retry loop, and reuses it verbatim on every attempt -- and there is a test that FORCES a retry (kill/refuse the first attempt) and asserts one message resulted. Key generation must be a real random id (no PIDs, no timestamps, no counters that reset -- all of which collide across restarts and processes). DOCUMENT: AGENT_PROTOCOL.md -- agents call the wrapper and do NOT craft keys themselves; what a replayed-ack response means; what a disconnect means and that reconnecting with the SAME key is correct while reusing it for different content is a protocol violation that will disconnect them again. PROTOCOL.md -- the header, the scope tuple, the payload fingerprint, and IDEM-3's retention window stated honestly as the boundary of the guarantee. CONTRACTS.md -- the header, every new error code, the record type IDEM-2 reserved, and any new flag/env var for the retention bound. Verify through the wrappers against a running throwaway bus with its own data dir under /tmp, not hand-written curl -- if the wrapper doesn't retry idempotently, the feature doesn't work.
  _Proof: scripts/bus-send.sh forced to retry against a running throwaway bus produces exactly ONE message; grep -q 'Idempotency-Key' AGENT_PROTOCOL.md CONTRACTS.md_
- [ ] IDEM-14 · IDEM-14: Idempotency violation path -- key reuse with a different payload rejects, logs as security, and disconnects — core, P1
  GATED on IDEM-10, IDEM-11, and at least one of IDEM-12/IDEM-13 landing first (the happy path must exist before the violation path can be distinguished from it). Implements invariant 10's violation clause: when a client reuses an (agent, key) pair the applied-key store (IDEM-11) already has a record for, but the NEW request's payload does NOT match the original, the server must (1) REJECT the request, (2) log it as a SECURITY event -- not a routine 4xx; same severity class as an auth failure, discoverable the way the security agent expects to find things -- and (3) DISCONNECT the offending client. THE CARVE-OUT THAT MUST NOT BE COLLAPSED (restate it here explicitly, do not assume the reader has IDEM-12's copy in front of them): this path fires ONLY for same-key-DIFFERENT-payload. Same-key-SAME-payload is IDEM-12/IDEM-13's legitimate-retry path and must NEVER reach this code -- an implementation that disconnects on every duplicate key regardless of payload is WRONG and will disconnect well-behaved retrying clients, precisely the bug invariant 10's text calls out by name. TWO DECISIONS THIS TASK MUST PIN DOWN and record in DECISIONS.md, because CLAUDE.md's invariant 10 text leaves them open: (a) the EXACT HTTP status code returned for the rejected request (409 Conflict is the natural fit for 'conflicts with a prior request under this key' -- pick and justify one, don't reuse a generic 400); (b) whether 'disconnect' means merely dropping the current connection/long-poll (the agent can reconnect and retry with a fresh key) or FULL CREDENTIAL REVOCATION requiring re-enrolment (the agent's AUTH-1 token is invalidated, same blast radius as AUTH-4's leave path) -- these have very different consequences and the choice must be deliberate, not whichever was easiest to wire up. Also applies conceptually to 'replay of an already-accepted signed message' per invariant 10's third bullet -- SIGN-4/SIGN-5 own the signature-replay detection mechanics; this task's reject/log/disconnect plumbing is the natural place that behaviour hooks into, so cross-reference SIGN-4 rather than building a second, divergent disconnect path.
  
  --- FOLDED IN FROM THE WITHDRAWN DUPLICATE EPIC (IDEM-5, superseded 2026-08-02), reconciliation pass 2. This content was unique to the withdrawn set and is now in THIS task's scope, not merely offered as a note: (a) DEFINE 'DISCONNECT' CONCRETELY -- on an HTTP server it is not self-evident: at minimum, close the connection without keep-alive reuse. That is the MECHANICS, a separate axis from the blast-radius decision this task already carries (drop the connection vs revoke the credential); both must be written down. (b) THE ERROR MUST BE GREPPABLE: a distinct code, not the generic validation error, plus a log line an operator actually sees carrying the caller identity, the operation, the key, and BOTH payload fingerprints (the stored one and the offending one). (c) DO NOT CREATE A SELF-INFLICTED RECONNECT STORM: a disconnected long-poll client (POLL-1) reconnects immediately, so the rejection must be either sticky enough to stop the loop or cheap enough not to matter -- say which. (d) KEEP THE SIGNED-REPLAY PATH DISTINCT: replay of an already-accepted SIGNED message also rejects and disconnects, but its freshness check is SIGN-4's sequence+cursor, NOT the payload fingerprint. Reuse this task's reject/log/disconnect plumbing, but do not merge the two detectors into one path -- cross-reference them instead. (e) BOTH DIRECTIONS GET THEIR OWN NAMED TEST: it fires on same-key-different-payload, and it provably does NOT fire on same-key-same-payload. Getting that backwards turns a correctness feature into an outage for exactly the well-behaved clients that retry correctly.
  _Proof: go test -race -run TestIdempotencyViolation ./internal/..._
- [x] IDEM-10 · IDEM-10: Idempotency key -- format, client-supplied untrusted validation, scoped per-agent — core, P1
  Define the idempotency key carried on every mutating request per invariant 10 (CLAUDE.md, 2026-08-02): enrol, send, broadcast, leave, peer-enrol, relay. The key is CLIENT-SUPPLIED and therefore UNTRUSTED input per invariant 1 -- validate it, never trust it. Pick and document an EXACT byte length cap (e.g. <=128 bytes) and an EXACT charset restriction (e.g. printable ASCII or a documented allow-list), and reject any request whose key field would trigger unbounded allocation (over-cap keys are rejected before the rest of the body is read, the same fail-fast discipline AUTH-6 established for the mux). Keys MUST be scoped PER-AGENT: the applied-key lookup this task feeds (IDEM-11) is keyed by (agent id, idempotency key), NEVER by key alone. State explicitly why this matters: without per-agent scoping, one agent could either collide with another agent's key space (corrupting its retry bookkeeping) or PROBE another agent's key space -- 'does key X already exist for some agent?' becomes an oracle leaking information about another agent's traffic, the same class of cross-agent leak invariant 2's <bus-id>.<agent-id> namespacing exists to prevent elsewhere. Deliverable: a written spec (CONTRACTS.md and/or PROTOCOL.md) naming the wire field name, the length cap, the charset, and the per-agent scoping rule, PLUS validation code shared by every mutating handler so the rule cannot be implemented inconsistently route-by-route (a single validateIdempotencyKey(agentID, key) helper, not five copies). BLOCKS IDEM-11 through IDEM-15, which all consume this key shape. No dependency on DUR-3.
  
  --- FOLDED IN FROM THE WITHDRAWN DUPLICATE EPIC (IDEM-1, superseded 2026-08-02), reconciliation pass 2. This content was unique to the withdrawn set and is now in THIS task's scope, not merely offered as a note: (a) TRANSPORT -- pick ONE canonical carrier for the key and use it everywhere; an `Idempotency-Key` request HEADER is the conventional choice, and if it goes in the body instead, say why. One place, never two: a key that can arrive by two routes is a key that will eventually disagree with itself. (b) A MISSING KEY ON A MUTATING ROUTE IS AN ERROR (4xx) and the server MUST NOT mint a substitute per attempt -- silently generating one makes every retry look like a new operation and defeats this entire epic while every server-side test keeps passing. (c) READ-ONLY ROUTES DO NOT TAKE ONE -- name them (/v1/agents, /v1/wait, /v1/messages, /healthz, /v1/info) so the rule is exhaustive in both directions rather than only listing what does require a key. (d) INVARIANT 1, STATED EXPLICITLY: the key is client-supplied input to VALIDATE, and it must NEVER become, seed, or be derivable into a message id, an agent id, or a sequence number -- all of those stay server-minted. (e) THE SCOPE TUPLE SHOULD ALSO CARRY THE OPERATION: the withdrawn task scoped dedupe by (fully-qualified <bus-id>.<agent-id>, OPERATION, key) rather than (agent, key). Decide which and record why -- without the operation component, one agent reusing a key across two different routes collides with itself. (f) ENROLMENT IS THE AWKWARD CASE AND IS SETTLED HERE, not deferred to IDEM-13: enrol has no authenticated caller yet, so its dedupe scope must be something else (the presented enrolment public key, or bus-wide). Decide it in this task, hand IDEM-13 the answer, and make sure the chosen scope cannot be used by an UNAUTHENTICATED caller to squat or probe keys. (g) DEFINE THE PAYLOAD FINGERPRINT HERE: the canonical hash (crypto/sha256, stdlib) that IDEM-11 stores next to the key, pinning EXACTLY which bytes are hashed, the same way SIGN-1 pins its signed bytes. IDEM-12's legitimate-retry path and IDEM-14's violation path both turn on same-payload-vs-different-payload, so an ambiguous fingerprint makes that distinction unreliable in BOTH directions -- it belongs in this contract task, not re-invented per route. Documentation of all of the above lands via IDEM-18 (the agent-facing wrapper + docs task filed by the merge).
  _Proof: go test -race -run TestIdempotencyKey ./internal/... && test -z "$("$(go env GOROOT)/bin/gofmt" -l .)"_
- [ ] IDEM-16 · IDEM-16: Exactly-once test suite -- retry storm, concurrent race under -race, and key-reuse-different-payload disconnect — test, P1
  GATED on IDEM-12, IDEM-13, IDEM-14. Functional/concurrency coverage proving invariant 10's guarantees under `-race` (CLAUDE.md: concurrency here is the product, a data race is a P0). Required, each as its OWN named test so a future regression names exactly which property broke: (1) RETRY STORM -- fire N (e.g. 50) requests sharing one (agent, key, payload) and assert exactly ONE effect resulted: one sequence allocated, one audit record written, all N responses are byte-identical to the original result, and none of the N connections was disconnected. (2) CONCURRENT RACE -- run under `go test -race`, launching the identical-payload retries genuinely concurrently (goroutines released via a barrier, not serialized one after another) so the applied-key check-then-write path's OWN race safety is exercised, not just its logic in isolation; a naive check-then-insert without a lock/CAS looks correct read serially but double-applies under real concurrency, and this test must be able to catch that. (3) KEY-REUSE-DIFFERENT-PAYLOAD -- reuse an (agent, key) with a different payload and assert IDEM-14's full behaviour: rejection with the pinned status code, the security-event log entry, and the disconnect (whichever form IDEM-14 decided). STATE THE CARVE-OUT EXPLICITLY in the test names/comments so a future reader cannot miscopy the storm test's assertions into the disconnect test or vice versa. Exercise via the actual HTTP routes (send/broadcast at minimum; enrol/leave/peer-enrol if IDEM-13 landed first), not by calling internal functions directly, so this proves the wire behaviour the AGENTIF wrappers actually depend on. Kept separate from IDEM-17's crash-injection test the same way DUR's functional tests are kept separate from DUR-6.
  
  --- FOLDED IN FROM THE WITHDRAWN DUPLICATE EPIC (IDEM-8, superseded 2026-08-02), reconciliation pass 2. This content was unique to the withdrawn set and is now in THIS task's scope, not merely offered as a note: (a) ASSERT EXACTLY ONE OF EVERYTHING, NOT MERELY 'NO ERROR': one WAL record, one append-only audit entry (invariant 6), one delivery to the recipient, one sequence consumed. A test that only inspects the response body passes against an implementation that quietly writes two durable records. (b) ADD A RETRIED-BROADCAST CASE: each recipient receives it exactly once, including a recipient whose first-attempt delivery failed. (c) ADD A POST-VIOLATION INTEGRITY CASE: after IDEM-14 rejects and disconnects a key-reuse-with-different-payload attempt, the ORIGINAL message is still intact, still in history, and still deliverable -- a violation must not damage the operation it collided with. (d) ADD A PAST-THE-RETENTION-WINDOW CASE asserting IDEM-11's DOCUMENTED behaviour explicitly, so the honest boundary of the guarantee is pinned by a test rather than left to the reader. NOTE that IDEM-11 currently carries an unresolved contradiction about what that behaviour is (fail-closed vs applied-as-a-new-operation); write this test against whatever DECISIONS.md records, and do NOT write it against whichever one the implementation happens to do.
  _Proof: go test -race -run 'TestIdemRetryStorm|TestIdemConcurrentRace|TestIdemViolationDisconnect' ./internal/..._
- [-] IDEM-8 · IDEM-8: Proof suite -- a retried send produces exactly one message, including across a crash and under concurrency — durability, P1
  GATED on IDEM-2/IDEM-4/IDEM-5 (may be written in parallel against them). Invariant 10 is a correctness claim, and a correctness claim without a test that would FAIL if it were violated is a slogan. Every scenario asserts EXACTLY ONE of everything -- one WAL record, one audit-log entry, one delivery to the recipient, one sequence consumed -- not merely 'no error'. REQUIRED SCENARIOS, each its own named test so a regression names the property that broke: (1) SIMPLE RETRY -- send, ack lost, resend with the same key and payload: one message, and the second response is byte-identical to the first. (2) CRASH BETWEEN EFFECT AND ACK -- crash-injection per CLAUDE.md: kill the server after the message commits but before the client sees the ack, restart, replay, then retry with the same key: still one message. THIS IS THE TEST THAT PROVES IDEM-2's same-transaction claim; without it that claim is unverified. (3) CRASH BETWEEN PREPARE AND COMMIT -- retry after restart produces exactly one message and recovery is a prefix of accepted history (invariant 5). (4) CONCURRENT DUPLICATES -- N goroutines fire the same key simultaneously under -race: one message, N identical responses, no data race. (5) KEY REUSE WITH DIFFERENT PAYLOAD -- rejected and disconnected (IDEM-5), and, importantly, the ORIGINAL message is still intact and deliverable afterwards. (6) PAST THE RETENTION WINDOW -- assert IDEM-3's documented behaviour explicitly, so the honest boundary of the guarantee is pinned by a test rather than left to the reader. (7) BROADCAST -- a retried broadcast delivers to each recipient exactly once. Table-driven where it helps; keep the narrowest check runnable in seconds.
  _Proof: go test -race -run TestExactlyOnce ./internal/... -- one subtest per scenario, each asserting exactly one durable record_
- [-] IDEM-4 · IDEM-4: Idempotent send and broadcast -- a legitimate retry returns the ORIGINAL result and produces no second message — msg, P1
  GATED on IDEM-1/IDEM-2. The core behaviour, on the paths that matter most (MSG-2 broadcast, MSG-3 send). SAME KEY + SAME PAYLOAD IS A LEGITIMATE RETRY -- the ack was probably lost in flight. Return the ORIGINAL result verbatim: the same message id, the same sequence, the same 2xx status. Do NOT re-apply, do NOT mint a new id or sequence, do NOT return an error or a 409, and do NOT disconnect. Invariant 10 is explicit that punishing this case would break exactly the clients doing the right thing; the disconnect rule belongs to IDEM-5's different-payload case and must not leak into this one. MARK THE RESPONSE so a caller can tell a replayed ack from a fresh one (a response field or header) -- useful for debugging and for the wrapper's logging, but the body must otherwise be identical. THE SUBTLE CASE, which is where implementations usually break: TWO CONCURRENT IN-FLIGHT REQUESTS WITH THE SAME KEY -- the client retried before the first ack landed, so the first operation is committed-in-progress and there is no stored result yet. A naive check-then-act double-applies. Handle it with a single-flight reservation on the key inside the same critical section that mints the sequence: the second caller either blocks and returns the first's result, or gets an explicitly retriable 'in progress' response -- pick one, document it, and TEST IT UNDER -race, since concurrency is this project's product. BROADCAST SPECIFICS: dedupe on the broadcast OPERATION, not per-recipient delivery -- a retried broadcast must not fan out a second time to anyone, including recipients whose delivery failed the first time. Interacts with SIGN-6: a message rejected for a missing/invalid signature was never applied, so its key is NOT recorded as applied and a corrected resend under the same key is a new operation, not a retry -- state this.
  _Proof: go test -race -run TestRetriedSendReturnsOriginal ./internal/httpapi ./internal/store_
- [-] IDEM-3 · IDEM-3: Bounded dedupe window -- retention policy, eviction, and the honest statement of what happens past the window — durability, P1
  GATED on IDEM-2. An applied-key store that never forgets grows without bound and eventually is the process's memory footprint and the WAL's replay time; one that forgets carelessly resurrects duplicates. This task bounds it and STATES THE RESULTING GUARANTEE HONESTLY. DELIVER: (1) the retention policy -- a time window, a count cap, or both; whichever is chosen, the bound must be provable and testable, not aspirational. (2) THE WINDOW MUST EXCEED THE MAXIMUM CLIENT RETRY HORIZON, or the guarantee is a lie in exactly the case that matters: a peer reconnecting after a long outage (RELAY-4's backoff ceiling) and a long-poll client resuming after a network partition are the realistic worst cases -- derive the number from them, do not pick a round one. (3) EVICTION MUST BE CONSISTENT ACROSS MEMORY AND DISK: evicting in memory while the record survives on disk (or the reverse) makes behaviour depend on whether a restart happened since, which is the worst kind of intermittent bug. State how eviction interacts with DUR-7 (snapshot/compaction) -- a snapshot must not silently reinstate evicted keys, nor drop live ones. (4) SAY PLAINLY IN PROTOCOL.md what happens to a retry that arrives AFTER its key is evicted: it is applied as a NEW operation and produces a second message. That is the true guarantee -- 'duplicates are suppressed within the retention window' -- and it must be documented as such rather than described as unconditional exactly-once, which the system does not and cannot provide. (5) Expose the current applied-key count/oldest-key age wherever CORE-5's inspect/metrics endpoint lands, so the bound is observable in production rather than assumed.
  _Proof: go test -race -run TestDedupeWindowBound ./internal/store_

### EPIC MSG — Messaging surface

- [x] MSG-4 · MSG-4: Cursor semantics + GET /v1/messages history — messaging, P1
  Define an opaque cursor (wraps a per-agent-visible sequence position, not a raw offset a client could forge into another agent's stream) with encode/decode/validate, and implement the paginated history endpoint using it -- this is the SAME cursor type the long-poll wait endpoint consumes.
  _Proof: go test -race -run TestMessageHistoryCursor ./internal/hub_
- [x] MSG-3 · MSG-3: POST /v1/send -- direct message — messaging, P1
  Targeted single-recipient send to a fully-qualified agent id; 404 on unknown recipient. Same durable write path as broadcast, delivered only to that agent's history/wait stream.
  _Proof: go test -race -run TestDirectMessageSend ./internal/hub_
- [x] MSG-2 · MSG-2: POST /v1/broadcast — messaging, P1
  Any enrolled agent posts a message visible to the whole roster. Goes through the two-phase write path and the audit log before the 200 is returned; assigns a message id via the sequence allocator.
  _Proof: go test -race -run TestBroadcastSend ./internal/hub_
- [ ] None · Acceptance criterion for the first durable-write HTTP handler (MSG-2/MSG-3): wal.ErrClosed/wal.ErrPoisoned must 5xx and MUST NOT acknowledge — httpapi, P1
  internal/wal.ErrClosed (format.go:156, "reported by Append and Sync after Close") and wal.ErrPoisoned both propagate all the way up through Log.Write/Begin/Commit (log.go:343-351, :388-392, :446) as ordinary Go errors -- correctly, at the wal layer: nothing there is ever swallowed. But VERIFIED THIS PASS: no HTTP handler exists yet that calls DurableLog.Write at all. The DurableLog interface was wired onto httpapi.Server by DUR-9 (internal/httpapi/server.go:34-38, "Write is the whole of invariant 4 as a handler needs it") and durable_test.go proves the wiring end to end with a fakeDurable, but grep confirms `.Durable().Write(` / `s.durable.Write(` has zero call sites anywhere in internal/httpapi -- the only two live routes are /healthz and /v1/info, neither of which writes anything durable. The first real write handler is POST /v1/send (MSG-3) and POST /v1/broadcast (MSG-2), both still `todo`.
  
  Filing this NOW, ahead of MSG-2/MSG-3, so the constraint is not lost or improvised differently by whichever agent picks up the first write handler: invariant 4 ("nothing is acknowledged before it is durable") means that when Durable().Write returns wal.ErrClosed (server is shutting down / already closed the log) or wal.ErrPoisoned (a torn write, see writer.go:145-150 -- "the writer refuses to keep going instead of trading a recoverable file for an unrecoverable one"), the handler MUST map that to a 5xx response (503 for ErrClosed -- the server is draining, a retry against another bus/instance may succeed; 500 for ErrPoisoned -- the log itself is suspect) and MUST NOT write any 2xx body, partial or otherwise, that a caller could read as "the message was sent". A response body written before the error is known, or any code path that treats a non-nil error from Write as anything but a hard failure, breaks invariant 4 the first time it happens.
  
  DONE means: when MSG-2/MSG-3 land, their handler tests include a negative case using the SAME fakeDurable pattern durable_test.go already established (fakeDurable{err: wal.ErrClosed} and fakeDurable{err: wal.ErrPoisoned}) asserting the response status is >=500 and the response body is the standard ErrorResponse shape, never the success shape.
  
  proof_cmd validated via scripts/proof-check.sh: verdict=FAIL (exit 1) today -- no reference to wal.ErrClosed or wal.ErrPoisoned exists anywhere in internal/httpapi yet, because no handler calls Write. This grep is a necessary-but-not-sufficient floor (a real handler MUST reference these sentinels to branch on them); the actual proof once MSG-2/MSG-3 land is the negative-path handler test described above, not this grep alone.
  _Proof: grep -rq "wal.ErrClosed\|wal.ErrPoisoned" internal/httpapi/*.go_
- [x] MSG-5 · MSG-5: Messaging durability integration test — messaging, P1
  Crash-injection test for the messaging path specifically: simulate a crash mid-broadcast and mid-DM at each write-path stage, restart, and assert a message is either fully present (in history, in the audit log, and delivered to any waiter that should have seen it) or fully absent -- never partially visible. SCOPE NOTE (added for invariant 10 / the IDEM epic): this task proves single-write atomicity -- one accepted send/broadcast is never torn into a partial state by a crash. It deliberately does NOT cover the retry-across-the-crash-boundary case (client resends the identical idempotency-keyed request after an ambiguous/lost ack and must still get exactly ONE effect, not a second message) -- that is IDEM-17's job (durable applied-key store crash-injection test) and IDEM-12's happy-path job (idempotent send/broadcast), gated on IDEM-11. Do not fold idempotency-key retry logic into this test's harness; keep it about torn-vs-whole for a single logical write, and let IDEM-17 own exactly-once-under-retry.
  _Proof: go test -race -run TestMessagingCrashRecovery ./internal/hub_
- [x] MSG-1 · MSG-1: GET /v1/agents -- roster listing — messaging, P1
  Authenticated endpoint returning the current enrolled-agent roster as fully-qualified `<bus-id>.<agent-id>` entries (name, id, enrolled-at). Read path only, no durability concerns beyond the already-recovered roster.
  _Proof: go test -race -run TestListAgents ./internal/hub_

### EPIC POLL — HTTP long-poll wait endpoint

- [x] POLL-1 · POLL-1: GET /v1/wait -- long-poll endpoint — poll, P1
  Agent calls with its last-seen cursor; if messages exist beyond it, respond immediately; otherwise park the request until a new message arrives OR a configurable timeout elapses, at which point return 200 with an empty batch (not an error) and the same cursor.
  _Proof: go test -race -run TestLongPollWait ./internal/hub_
- [x] POLL-3 · POLL-3: Poll concurrency test suite (goroutine leak + thundering herd) — poll, P1
  Two properties under -race: (1) a client disconnect mid-wait releases the parked goroutine promptly -- no leak, asserted via goroutine-count before/after; (2) thundering herd -- many agents parked on the same bus, one new broadcast wakes every eligible waiter exactly once, no duplicate or missed delivery.
  _Proof: go test -race -run TestPollConcurrency ./internal/hub_
- [x] POLL-2 · POLL-2: Wake-on-new-message wiring — poll, P1
  The hub notifies every parked waiter whose cursor is behind a newly committed message -- wiring between the two-phase commit path and the waiter registry, so wake-up happens only after the write is durable, never before.
  _Proof: go test -race -run TestWaiterWakeup ./internal/hub_

### EPIC RATCHET — Ratchet crypto: adopt, do not invent

- [-] RATCHET-4 · RATCHET-4: Broadcast fan-out under pairwise ratchets — crypto, P1
  A double ratchet is inherently 1:1, but MSG-2 broadcasts to the whole roster. Evaluate the real options -- N pairwise ratchet sends, a sender-key/group-messaging construction, or a per-message symmetric key wrapped per recipient -- with cost, forward-secrecy and complexity for each. Sender-key schemes are a KNOWN sharp edge: getting one wrong silently loses PFS. Recommend, and say explicitly what an implementer must NOT improvise.
- [-] RATCHET-8 · RATCHET-8: Record the decision, then gate the CRYPTO epic on it — crypto, P1
  Turn the deep dive's recommendation into a dated DECISIONS.md entry (library, version, what we implement, what we explicitly never implement, the fan-out approach, the state-durability rule), update CRYPTO-1/2 to reference it rather than re-deciding, and confirm with spec-keeper that no CRYPTO implementation task starts before this lands. The point of the gate is that the expensive, dangerous work is not begun on an assumption.
- [x] RATCHET-7 · RATCHET-7: Choose and supply-chain-review the Ed25519 implementation (stdlib crypto/ed25519 vs a libsodium binding) — security, P1
  RESCOPED 2026-08-02 (sign-only). This is the LAST undecided crypto question in the epic and it GATES the implementation of SIGN-1/SIGN-2/CRYPTO-10, all of which currently name Go's crypto/ed25519 as the presumptive answer -- this task confirms or overrides that, once, in writing. DECISIONS.md deliberately left it open: "whether to use stdlib crypto/ed25519 or a cgo libsodium binding is left to the implementing task; both satisfy invariant 9". DECIDE BETWEEN EXACTLY TWO OPTIONS -- do not open a wider search, and under invariant 9 do not consider any option that involves implementing a primitive ourselves: (a) Go stdlib crypto/ed25519 -- zero new modules, no cgo, works on the box's go1.19.4, is the RFC 8032 reference-implementation lineage upstreamed into the stdlib, and is a high-level Sign/Verify API (exactly the 'wraps as much of the problem as possible' invariant 9 asks for); its supply chain IS the Go toolchain, so the review becomes 'how is the builder image's Go version pinned and how do we learn about Go security releases' (ties to DEPLOY-1). (b) a cgo libsodium binding -- matches the user's word 'libsodium' literally, but adds a C library to the runtime image, cgo to the build, and a binding maintainer to the trust chain. REVIEW BOTH ON: provenance and who can push a release, release signing / checksum verification, transitive dependency footprint, cgo and native build requirements against the multi-stage Docker image (DEPLOY-1's minimal runtime -- a cgo binary is not static and will not run on a scratch/distroless base without care), CVE history, and our exposure if it is abandoned. DELIVERABLES: the choice, the exact pinned version, how we learn about advisories (name the mechanism -- e.g. govulncheck in the DEPLOY-5 container check, GitHub advisory watch), and a dated DECISIONS.md entry containing all of it. Invariant 8 requires a justification for any third-party dependency; a crypto dependency requires this stronger form. NOTE the honest asymmetry when weighing: 'it is what the user said' is a reason to take libsodium seriously, but the user's controlling requirement was standard, audited, high-level sign/verify -- not a specific vendor -- so either option satisfies the instruction as long as the reasoning is recorded.
  _Proof: grep -q 'RATCHET-7' DECISIONS.md && grep -q '2026-08-02 .* Ed25519 is Go stdlib' DECISIONS.md && test "$(go list -m all)" = 'github.com/dodgymike/agent-bus'_
- [ ] RATCHET-2 · RATCHET-2: Threat model -- what Ed25519 signing defends against, and explicitly what it does not — crypto, P1
  RESCOPED 2026-08-02 per user instruction ("keep it simple, standard sign/verify; encryption later"): this is no longer a ratchet/PFS threat model, it is the threat model for a SIGN-ONLY design. Write down the adversary before further work lands. Who is the attacker -- a compromised bus, a compromised relay peer bus, a network observer, another enrolled agent, someone who later obtains the disk? WHAT SIGNING BUYS: message AUTHENTICITY (this body really was produced by the holder of this messaging private key) and INTEGRITY (this body was not modified in transit), verified by the RECIPIENT -- so a compromised or malicious bus cannot forge a message purporting to be from an agent it does not control, even though the bus relays every message. This is the whole security value of keeping the AUTH keypair (CRYPTO-1/AUTH-1, authenticates to the bus) and the MESSAGING keypair (CRYPTO-3, authenticates to peers) separate -- state that explicitly. WHAT SIGNING DOES NOT BUY, STATE THIS PLAINLY AND WITHOUT HEDGING: NO CONFIDENTIALITY. Without encryption, the bus and any relay peer on a multi-bus path (RELAY-2/3) CAN and WILL read every message body, in cleartext, always. This is now an ACCEPTED property of the system per direct user instruction, not an oversight to be apologized for -- but it must be legible to every future reader of PROTOCOL.md, not discovered by surprise. NO forward secrecy (a compromised messaging private key lets an attacker forge NEW messages as that agent going forward, and there is no ratchet to bound the blast radius -- key rotation via key_epoch, CRYPTO-4, is the only mitigation). NO replay defence from the signature alone (covered separately by SIGN-4's sequence+cursor -- reference it, do not re-derive it here). State plainly which threats are OUT of scope for this rescoped epic (traffic analysis / metadata exposure, a fully compromised endpoint agent, a malicious bus dropping/reordering/duplicating messages -- signing does not stop any of these, only forging content undetected). Without this document the sign/verify choice is unfalsifiable and 'we signed it' becomes a slogan rather than a security property.
  _Proof: grep -rqi 'no confidentiality' THREAT_MODEL.md PROTOCOL.md_
- [-] RATCHET-3 · RATCHET-3: Do we need full Signal semantics? -- the cheaper-alternative check — crypto, P1
  Deliberate devil's advocate against the whole epic, so the decision is made on merit. Full X3DH + double ratchet buys asynchronous session setup, PFS, and post-compromise recovery. Agent-bus may not need all three. Compare against simpler, well-audited options (static keypairs + NaCl box, or an AEAD with scheduled rekeying) on security delivered per unit of complexity, and against the fact that complexity ITSELF is a security risk here. Recommend. It is entirely legitimate for this task to conclude the full ratchet is warranted -- but the case must be made, not assumed.
- [-] RATCHET-1 · RATCHET-1: DEEP DIVE -- how to get a double ratchet WITHOUT writing our own crypto — crypto, P3
  THE GATING TASK. Produce RATCHET_DEEPDIVE.md. Governing constraint, stated up front and never relaxed: we do not implement primitives, X3DH, or the ratchet ourselves. Rolling your own is the single highest-risk thing this project could do -- the failure mode is silent (it still encrypts, it still decrypts, it is simply broken), and no ordinary test suite detects it. REQUIRED CONTENT: (1) an explicit survey of the ACTUAL options for Go -- official libsignal (Rust) via cgo/FFI, maintained pure-Go double-ratchet implementations, age/NaCl-style alternatives, and the honest question of whether full Signal semantics are needed at all versus a simpler audited AEAD scheme with periodic rekeying; (2) for EACH option: maintenance status, audit history, API misuse-resistance, licence, cgo/build implications for the Docker image, and what it does NOT give us; (3) a clear recommendation with the runner-up and the conditions under which we would switch; (4) what we would still have to write ourselves under each option -- session storage, key lifecycle, fan-out -- since that glue is where implementations usually fail even with a good library; (5) an explicit list of things we will NEVER hand-roll. NO CODE. The output is evidence and a recommendation for the user to accept.
- [-] RATCHET-5 · RATCHET-5: Ratchet state durability vs invariants 4/5 -- the key-reuse trap — crypto, P1
  Ratchet state is mutable and MUST NOT be replayed: rewinding it can cause key/nonce reuse, which is catastrophic and total for AEAD confidentiality. This collides head-on with invariant 5 (rebuild memory by replaying the durable store). Determine the safe pattern -- ratchet state as a durably-checkpointed side-store that is never rewound by WAL replay, monotonic counters that only advance, and what recovery does when state is lost or ambiguous (fail closed and force a new session, never guess). This must be settled BEFORE any ratchet code is written.
- [ ] RATCHET-6 · RATCHET-6: RFC 8032 Ed25519 known-answer tests wired into the sign/verify implementation — crypto, P1
  RESCOPED 2026-08-02 per user instruction ("keep it simple, standard sign/verify; encryption later"): the construction under test is now Ed25519 (crypto/ed25519, Go stdlib), not a Double Ratchet. MANDATORY, not nice-to-have, per invariant 9 (never write your own crypto -- confirming correct USE of an audited primitive is exactly the discipline invariant 9 demands, since a verifier that accepts everything passes every positive test ever written and 'it round-trips with itself' is not evidence). RFC 8032 publishes canonical Ed25519 test vectors (seed/public key/message/expected signature tuples, including the well-known empty-message and edge-case vectors used across every conformant implementation). Wire a representative set of these into the test suite for whatever function/subcommand SIGN-1/SIGN-2/CRYPTO-10 end up calling crypto/ed25519 through, asserting BYTE-EXACT expected signatures (not just 'it verifies its own output' -- a self-consistent but non-conformant implementation would pass that trivially and still be wrong). This proves our INTEGRATION calls the stdlib correctly (right key format, right message bytes, right signature encoding), not merely that it compiles. Note Go's crypto/ed25519 is itself the reference implementation lineage (adiantum team / Adam Langley's Go ed25519, upstreamed) so a mismatch here would indicate a bug in OUR canonicalisation/wiring (SIGN-1), not in the library.
  _Proof: go test -race -run TestEd25519RFC8032Vectors ./internal/..._

### EPIC RELAY — Bus-to-bus federation

- [ ] RELAY-4 · RELAY-4: Peer-down retry/backoff — relay, P2
  If a peer is unreachable, relay to it retries with backoff on a background path rather than blocking the local sender's response -- a slow/dead peer must never make a local broadcast/DM slow or fail.
  _Proof: go test -race -run TestPeerRetryBackoff ./internal/relay_
- [ ] RELAY-2 · RELAY-2: Message relay + ongoing roster sync across peers — relay, P2
  A broadcast/DM whose target is (or might be, for broadcast) on a peer bus is forwarded to that peer using the fully-qualified agent id; roster changes (new enrolment, leave) are pushed to peers incrementally after the initial exchange so routing tables stay current.
  _Proof: go test -race -run TestMessageRelay ./internal/relay_
- [x] RELAY-1 · RELAY-1: Peer enrolment + initial agent-list exchange — relay, P2
  A bus-to-bus handshake (POST /v1/peer/enroll or similar) where two buses mutually authenticate and exchange bus ids plus their current rosters, so each learns the other's fully-qualified agent ids for routing (invariant 2).
  _Proof: go test -race -run TestPeerEnrollment ./internal/relay_
- [ ] RELAY-3 · RELAY-3: Loop prevention via traversed-bus path — relay, P2
  Every relayed message carries the list of bus ids it has already traversed; a bus that sees itself in that list drops the message instead of re-relaying it -- required the moment peer topology has a cycle.
  _Proof: go test -race -run TestRelayLoopPrevention ./internal/relay_
- [ ] RELAY-5 · RELAY-5: Relay crash/loop integration test — relay, P2
  Multi-bus (3+) topology test with a cycle in the peer graph: send a broadcast, simulate a crash on one bus mid-relay, restart it, and assert every agent across the topology sees the message exactly once -- no loop, no duplicate, no loss. SCOPE NOTE (added for invariant 10 / the IDEM epic): a topology with only a CYCLE mostly exercises RELAY-3's traversed-bus-path loop prevention, which is a different mechanism from IDEM-15's key-based duplicate suppression. Per invariant 10, loop prevention COMPLEMENTS idempotency and is never a substitute for it, so this test's topology MUST also include a diamond/two-disjoint-path shape (the message reaches one bus via two different peers, neither path revisiting a bus it already traversed) -- RELAY-3 does nothing for that case, and only IDEM-15's applied-key check catches the resulting duplicate. Assert exactly-once delivery is achieved via IDEM-15's suppression for the disjoint-path case and via RELAY-3 for the cyclic-path case, and that removing either mechanism (test it with a build tag or a stub) breaks its respective case -- so the test proves the two defences are genuinely both required, not just both present. Gated on RELAY-3, IDEM-15.
  _Proof: go test -race -run TestRelayCrashLoopIntegration ./internal/relay_

### EPIC SIGN — SIGN: message authenticity & integrity (Ed25519 sign/verify, no encryption yet)

- [ ] SIGN-2 · SIGN-2: Sign on the send path (Ed25519 detached signature travels with the message) — crypto, P1
  GATED on SIGN-1 (canonical format) and CRYPTO-3 (messaging keypair minted at enrolment). Supersedes the encryption-specific CRYPTO-6 (Double Ratchet encrypt on the DM path), which is superseded outright -- there is no ratchet and no ciphertext. What this task implements instead: the sender signs SIGN-1's canonical serialisation of the outgoing message with its messaging Ed25519 PRIVATE key (crypto/ed25519.Sign -- stdlib, audited, high-level API; invariant 9 -- no custom signing code) and the resulting detached signature travels alongside the plaintext body in the envelope. The private key never leaves the agent's machine and is never sent to the bus. Because SIGN-1 may require the signature to cover the server-minted message id/sequence (see SIGN-1's open question), specify and implement the exact ordering this requires: either (a) the client obtains the id/sequence first (e.g. a reserve-then-send two-step) and then signs, or (b) the client signs everything it controls and the durable record binds the server-minted id/sequence to that signature non-repudiably without them being literally inside the signed bytes -- pick the option SIGN-1 settled on. Wire this into the SAME Go binary used by scripts/bus-send.sh (invariant 7 -- shell cannot do Ed25519, so add a subcommand, e.g. `agent-bus sign`, that the wrapper shells out to) -- ship the wrapper change and any AGENT_PROTOCOL.md update IN THIS SAME TASK. The bus stores and forwards the signature as opaque bytes; it MAY optionally check the signature is well-formed (right length) but verification is the RECIPIENT's job (SIGN-1's epic note on why -- a malicious bus must not be able to forge on behalf of a sender it does not control, and equally must not be trusted to police messages against senders it does not control either). No new key material beyond CRYPTO-3's existing messaging keypair. Test via scripts/bus-send.sh against a running throwaway bus, not hand-written curl.
  
  ACCEPTANCE CRITERION ADDED 2026-08-02 (RATCHET-7 fallout, verified first-hand by backlog-triage by reading this box's own stdlib source at crypto/ed25519/ed25519.go under GOROOT): ed25519.Verify PANICS (does not return false) when len(publicKey) != ed25519.PublicKeySize -- a remote DoS trap, asymmetric with malformed-signature handling (a bad signature safely returns false, a malformed key does not). Any verification this task's send path performs or triggers downstream (including recipient-side verification against a sender's messaging public key, and any self-check before accepting a signature as well-formed) must length-check the public key against ed25519.PublicKeySize BEFORE calling ed25519.Verify, and must fail closed on mismatch rather than panic. REQUIRED TEST: a negative test feeding a wrong-size and a nil/empty public key through this path, proving no panic. See also the standalone cross-cutting task filed to track this trap across all Verify call sites (AUTH-1, CRYPTO-10, SIGN-2).
  _Proof: go test -race -run TestSendSigns ./internal/... ; scripts/bus-send.sh against a running throwaway bus produces a message whose signature verifies against the sender's registered messaging pubkey_
- [ ] SIGN-4 · SIGN-4: Replay/freshness -- server-minted monotonic sequence + recipient-side cursor — crypto, P1
  GATED on SIGN-1. A signature alone does NOT provide a freshness/replay defence: a validly-signed message can be replayed VERBATIM by anyone who saw it once (including a malicious bus), and Ed25519 verification of a replayed message succeeds every time because nothing about the signature changes. Do not let an implementer assume signing solves this -- it does not, and the SIGN epic description says so explicitly. This task specifies and implements the defence: the bus mints a monotonic sequence number per recipient (or per conversation -- decide and document which, consistent with invariant 1: ids/sequences are server-minted, never client-supplied) INSIDE SIGN-1's signed bytes, and the recipient maintains a durable delivery cursor (highest sequence accepted so far, per sender or per conversation) that MUST only advance, never rewind (same shape as the durable-store invariants 4/5: the cursor is part of the recipient's serving state, rebuilt by replay on restart). A message whose sequence is <= the cursor is rejected as a replay BEFORE the body is handed to the calling agent, even if its signature verifies. State plainly what this does and does not cover: it defeats verbatim replay of a message already delivered; it does NOT provide encryption or hide metadata (accepted per RATCHET-2's rescope). Tests: replaying the exact same signed envelope after successful delivery is rejected; out-of-order delivery within a reasonable window is handled sanely (define the policy -- reject strictly increasing-only, or allow a bounded reorder window, and say why); a cursor is durable across a recipient-side restart (crash-injection style test per CLAUDE.md's durability discipline, since this is exactly invariant-4/5 territory even though it lives on the recipient side, not the bus's WAL).
  _Proof: go test -race -run TestReplayRejectedByCursor ./internal/..._
- [x] SIGN-1 · SIGN-1: Canonical signing format for messages (Ed25519 detached signatures) — crypto, P1
  RESCOPE (supersedes the Signal/ratchet direction, user instruction 2026-08-02: "ok, let's keep it simple and just use standard message auth/integrity using libsodium. encryption can come later"). GOVERNED BY INVARIANT 9 (never write your own crypto; always use a well-known, standard, audited library that wraps as much of the problem as possible -- this OVERRIDES invariant 8 where they conflict). This task specifies the EXACT bytes a sender signs and a recipient verifies -- the sharp edge of the whole epic: if sender and verifier serialise differently, verification fails intermittently or, worse, a field outside the signed bytes becomes silently forgeable. Deliverable: a written spec (in PROTOCOL.md or a dedicated section) plus a canonicalize() function pinned by test vectors, naming EXACTLY which fields are covered and in what order/encoding -- at minimum: message id (server-minted), sequence (server-minted, monotonic), fully-qualified sender id (<bus-id>.<agent-id>), fully-qualified recipient id(s) (sorted, for determinism), timestamp, and the message body. State explicitly which fields are server-minted vs sender-supplied, since a server-minted field being outside the signed bytes would let a malicious bus reorder/misattribute messages undetected -- so the id and sequence MUST be inside the signed bytes even though the sender does not choose them (the sender signs the server's assignment as part of the accept flow, OR the design places signing before minting and the signature covers only sender-known fields with the id/seq bound separately by the durable record -- DECIDE and document which, do not leave it ambiguous). We do NOT invent a signing construction: canonical bytes are handed to the library's Sign/Verify API (Go stdlib crypto/ed25519 -- crypto/ed25519.Sign / crypto/ed25519.Verify, the audited, high-level, misuse-resistant sign/verify API for RFC 8032 Ed25519) and NOTHING else -- no custom padding, no hand-rolled length framing beyond a documented fixed field order, no bespoke hashing construction assembled ourselves. Include a table of the exact byte layout (fixed-order concatenation with length-prefixed variable fields, or a documented canonical JSON form -- pick ONE, deterministic, and say why) and a handful of worked test vectors (input struct -> exact signed bytes -> hex) that SIGN-2/SIGN-5 and CRYPTO-10 depend on. BLOCKS every other SIGN task and the CRYPTO-3/4/10 rescopes' implementation.
  
  CONSTRAINT ADDED 2026-08-02 (RATCHET-7 fallout): Ed25519 signs the message itself, never a digest -- crypto/ed25519's Sign/Verify API rejects pre-hashed input for Ed25519 (there is no PureEdDSA-over-a-hash mode exposed; feeding it a hash instead of the message is a misuse of the API, not a supported shortcut). Because DUR-5 defines an audit-log content hash and SIGN-2 defines the signature, and the two are specced in separate epics, they will drift apart unless pinned together here: SIGN-1's canonicalize() output -- the exact canonical byte sequence -- MUST be the single shared input that (a) SIGN-2 passes to ed25519.Sign/ed25519.Verify UNHASHED, and (b) DUR-5 hashes for its audit-log content hash. Do not let DUR-5 hash a differently-serialised or differently-ordered view of the same logical message; if DUR-5's audit record needs additional fields beyond what SIGN-1 signs, those extra fields must be clearly out-of-band from (not silently substituted for) the canonical signed bytes. State this explicitly in the PROTOCOL.md deliverable and cross-reference DUR-5 by name so the two epics do not drift.
  _Proof: go test -race -run TestCanonicalize ./internal/..._
- [ ] SIGN-5 · SIGN-5: MANDATORY negative-test suite -- prove the verifier rejects everything it must — crypto, P1
  GATED on SIGN-1/SIGN-2 and CRYPTO-10's verify implementation existing (may run in parallel with CRYPTO-10 against a stub). MANDATORY, not nice-to-have, per invariant 9: broken or misused crypto fails SILENTLY -- it still 'verifies', and provides none of the protection it appears to. 'Our tests pass' is never evidence for a crypto change; a verifier that accepts everything passes every positive test ever written. This task exists specifically to make that failure mode impossible to ship undetected. Required cases, EACH proven REJECTED with a distinct assertion (not just 'an error occurred' -- assert the specific failure path fired): (1) TAMPERED BODY -- flip one byte of the signed body, signature must fail; (2) SWAPPED SENDER -- a validly-signed message re-labelled as if from a different sender must fail (proves the sender id is inside the signed bytes per SIGN-1, not just alongside them); (3) REPLAYED MESSAGE -- re-deliver an already-accepted signed envelope verbatim, must be rejected by SIGN-4's cursor even though the signature itself verifies; (4) WRONG KEY -- verify against a public key that is NOT the signer's (e.g. a different enrolled agent's real key), must fail; (5) TRUNCATED SIGNATURE -- a short/malformed signature byte string must be rejected cleanly (no panic, no out-of-bounds read -- crypto/ed25519.Verify is documented to handle this safely, confirm it and pin the confirmation in a test) . Add any other rejection case the implementation surfaces (e.g. corrupted/garbage public key bytes). Every case must have its own named test, not be folded into one assertion, so a future regression names exactly which property broke.
  _Proof: go test -race -run TestVerifyRejects ./internal/... -- one subtest per rejection case, each asserting non-zero exit / verify-failure, none asserting success_
- [ ] SIGN-8 · SIGN-8: Agent-side messaging key material -- `agent-bus keygen`, key file location/permissions, bus-enrol.sh wiring, AGENT_PROTOCOL.md — agentif, P1
  The AGENT-SIDE half of CRYPTO-3, which is server-side only (it registers a public key the agent must already have). Nobody owns generating and protecting the private half, and AGENTIF-2 (scripts/bus-enrol.sh) predates the whole signing decision and knows nothing about keys -- so as it stands an agent has no way to obtain a messaging identity. Invariant 7: agents never hand-write HTTP and never hand-roll key handling either; shell cannot do Ed25519, so add an `agent-bus keygen` subcommand to the same Go binary (crypto/ed25519.GenerateKey with crypto/rand -- invariant 9, no custom key derivation, no hand-rolled entropy) and have the wrapper shell out to it. DELIVER: (1) a documented default key location outside the repo, overridable by one env var, with the private key written 0600 inside a 0700 directory, created atomically; refuse to run -- loudly, non-zero exit -- if an existing key file is group- or world-readable (the same refusal CRYPTO-10 makes on the verify side, so the two agree). (2) The private key is NEVER printed to stdout, NEVER logged, and NEVER sent to the bus -- only the 32-byte public half goes over the wire, at enrolment. Add its path pattern to .gitignore (related: CORE-10, which notes the stop hook stages with `git add -A`; a messaging private key landing in a commit is the worst realistic outcome of this epic). (3) bus-enrol.sh generates the keypair if absent and registers the public half, and is IDEMPOTENT: a second run must NOT silently overwrite an existing private key. Silent re-keying is the dangerous failure -- it orphans the already-registered public key and trips every verifier's TOFU pin (CRYPTO-4) as if the bus were MITM-ing, so an accident becomes indistinguishable from an attack. Re-keying must be explicit and human-driven. (4) State plainly how this file differs from the AUTH credential from AUTH-1 (the bearer token that authenticates TO the bus): two files, two lifetimes, two purposes -- the token proves you to the bus, the messaging key proves you to your PEERS, and only the second one a compromised bus cannot forge. (5) AGENT_PROTOCOL.md entry ships IN THIS TASK (invariant 7); CONTRACTS.md gains the subcommand, the env var and the file path. Rotation and revocation are OUT of scope -- CRYPTO-4's key_epoch and AUTH-4 own them -- but say what re-enrolment does. Verify the way an agent would: through scripts/bus-enrol.sh against a running throwaway bus with its own data dir under /tmp, not hand-written curl.
  _Proof: scripts/bus-enrol.sh against a running throwaway bus creates a 0600 private key, registers the public half, and a SECOND run neither overwrites it nor silently re-keys ; go test -race -run TestKeyfilePerms ./internal/..._
- [ ] SIGN-7 · SIGN-7: Cross-bus relay preserves the signed envelope byte-exact -- an intermediate bus can neither forge nor strip a signature — crypto, P1
  GATED on SIGN-1; implementation lands with RELAY-2/RELAY-3. RAISED TO P1 DESPITE THE RELAY EPIC BEING P2 BECAUSE IT CHANGES A SIGN-1 DECISION: SIGN-1 must not be completed until the question below is answered, or the canonical format will have to be redesigned after code depends on it. THE COLLISION: SIGN-1 wants the server-minted message id and sequence INSIDE the signed bytes (so a malicious bus cannot reorder or misattribute messages undetected). But those are minted by the ORIGIN bus, while the receiving bus needs its own local sequence for its own recipients' cursors (SIGN-4) and, per invariant 1, does not accept ids minted by a client -- and a peer bus IS a client from its perspective. If the far bus re-mints and substitutes, EVERY relayed signature fails at the far end; if it adopts the origin's numbers wholesale, it has ceded id authority to a peer. RESOLVE IT EXPLICITLY. The likely answer -- state it or a better one, and make SIGN-1 match: the signed bytes carry the ORIGIN's fully-qualified sender id and the ORIGIN's message id, which per invariant 2 are already bus-namespaced and therefore globally unambiguous and not the far bus's to mint, while the receiving bus mints its own LOCAL delivery sequence OUTSIDE the signed bytes and binds it in its durable record. (2) NO FORGERY: an intermediate bus cannot forge a message because it does not hold the sender's messaging private key -- but ONLY if the recipient verifies against a key it trusts. CROSS-BUS KEY TRUST IS AN OPEN HOLE: CRYPTO-4's bundle is attested by the LOCAL bus, so bus B attesting a key for bus A's agent means bus B can simply lie and substitute its own key. Decide and document: relay A's attestation intact (bundle signed by A's bus key) and pin A's BUS key at peering time, or TOFU the agent's messaging key at first contact and alarm on change, or both. Without this, cross-bus signatures verify against whatever the nearest bus says, which is worth nothing. (3) NO STRIPPING: SIGN-6's mandatory-signature ingest rule applies to the relay ingest path EXACTLY as it applies to /v1/send. A relayed message arriving with no signature, or with a re-signed one, is rejected -- an unauthenticated downgrade must not be reachable through a peer. (4) NO MUTATION: the relay forwards the signed bytes verbatim. Any normalisation on the path (re-encoding JSON, reordering keys, trimming whitespace, re-framing the body) breaks verification at the far end -- which is a strong argument for SIGN-1 choosing a length-prefixed binary canonical form, or for the relay carrying the exact signed byte string as an opaque blob. Say which. (5) Complements RELAY-3 (traversed-bus-path loop prevention) and IDEM-15 (relay duplicate suppression -- exactly-once APPLICATION on the relay path; this gloss pointed at IDEM-7 until the 2026-08-02 duplicate-epic merge superseded IDEM-1..9 and folded IDEM-7's origin-identity dedupe and non-forgeability content into IDEM-15): the bus path is metadata OUTSIDE the signature and grows on every hop, so it can never be inside the signed bytes -- state that explicitly, since it means the path is unauthenticated and a lying peer can rewrite it (loop prevention is availability, not security). TESTS: signed on A, verifies for a recipient on B; strip the signature in transit -> rejected at B's ingest; mutate one byte of a signed field in transit -> the recipient's verification fails and the body is never delivered; the far bus's local sequence differs from the origin's without breaking verification.
  _Proof: go test -race -run TestRelayPreservesSignature ./internal/relay ; a message signed on bus A verifies unmodified for a recipient on bus B using the DEPLOY-3 two-bus Compose profile_
- [ ] SIGN-6 · SIGN-6: A signature is MANDATORY on the wire -- ingest policy and fail-closed handling of missing/malformed/unverifiable signatures — crypto, P1
  GATED on SIGN-1 (canonical bytes) and SIGN-2 (signing on send). SIGN-1..5 specify how to sign and how to verify; NOTHING yet specifies what the bus does with a message that is not signed, or what a recipient does with one that fails to verify. That gap is not cosmetic: if either side treats "no signature" as "unsigned but fine", an attacker strips the signature and the entire epic is theatre. THIS TASK CLOSES IT. (1) THE SIGNATURE FIELD IS REQUIRED, NOT OPTIONAL. There is no unsigned message type, no allow_unsigned flag, no --insecure escape hatch, no legacy path; if one is ever argued for it needs its own dated DECISIONS.md entry. (2) INGEST POLICY on POST /v1/send and /v1/broadcast (MSG-2/MSG-3): the bus does NOT verify authenticity -- it must not be trusted to police messages on behalf of senders it does not control (SIGN-2), and the trust decisions live with the recipient (CRYPTO-4 TOFU pins) -- but it DOES enforce, and reject 4xx on failure: signature present; signature exactly 64 bytes (Ed25519); the claimed sender equals the AUTHENTICATED caller (invariants 1 and 2 -- a client-asserted identity is input to validate, never an identity to trust, so no caller may inject a message attributed to another agent no matter how well-formed the signature looks). (3) A REJECTED MESSAGE MUST LEAVE NO TRACE: no WAL record, no audit-log entry beyond a rejection event, no delivery, no ack -- the mirror image of invariant 4. DECIDE AND DOCUMENT whether a rejected send consumes a sequence number: if it does, recipients see gaps and SIGN-4's cursor must tolerate them; if it does not, sequence minting must happen after validation. Pick one, say why, make SIGN-4 consistent. (4) RECEIVE PATH: GET /v1/wait and GET /v1/messages return the signature with every message so the recipient can verify (CRYPTO-10). Verification failure is FAIL-CLOSED -- the body is NEVER handed to the calling agent -- and LOUD: log message id, sender, and which check failed; never swallow it. (5) THE POISON-MESSAGE WEDGE, the subtle one: if a message that fails verification also blocks the recipient's cursor from advancing, one bad message wedges that agent FOREVER and a malicious bus gets a trivial denial of service. Recommended policy to specify and test: the cursor advances past the unverifiable message (it was durably delivered and cannot be un-sent), the body is discarded rather than delivered, and the event is recorded so the failure is visible. Whatever is chosen, prove the poller cannot be wedged. (6) Interacts with invariant 10 (IDEM epic): a rejection must not turn into a client retry loop that produces duplicates -- a rejection is terminal for that idempotency key, not a transient error. TESTS: unsigned send rejected with no durable record; 64-byte-length check (63 and 65 bytes both rejected); sender-mismatch rejected; relay ingest is subject to the SAME check (see SIGN-7 -- a relay path that skips it is the obvious backdoor); a recipient handed one unverifiable message still makes progress on the next good one.
  _Proof: go test -race -run TestUnsignedRejected ./internal/httpapi ./internal/store ; scripts/bus-send.sh with the signature stripped is rejected by a running throwaway bus and leaves NO durable record_
- [ ] SIGN-3 · SIGN-3: Broadcast signature covers the recipient set (prevents split-content broadcasts) — crypto, P2
  GATED on SIGN-1/SIGN-2. Replaces the encryption-specific scope of superseded CRYPTO-8 (broadcast fan-out under authenticated encryption / Sender Keys) and superseded RATCHET-4 (broadcast fan-out under pairwise ratchets) -- neither ratchets nor per-recipient encryption apply anymore, but the underlying risk they both flagged is REAL and still applies to a signature-only design: MSG-2 broadcasts to N agents as N separate deliveries, and without an extra check a malicious SENDER could put DIFFERENT content in each recipient's copy under the same broadcast id, and no individual recipient could tell (each copy's own per-message signature verifies fine in isolation). Fix: the sender additionally signs (invariant 9 -- crypto/ed25519.Sign, no custom construction) a digest over (broadcast_id, hash-of-body, the SORTED set of recipient fully-qualified ids), included in every recipient's envelope alongside the per-message signature from SIGN-2. A recipient who wants the 'everyone got the same broadcast' guarantee can compare this digest against other recipients' copies (e.g. via bus-trace tooling or by agents comparing out of band); document that comparison, don't just produce the digest and leave it unused. Use a standard, audited hash for hash-of-body (crypto/sha256, stdlib) -- not a bespoke construction. Tests: every recipient's digest for one broadcast is identical; a tampered per-recipient body still fails SIGN-2's per-message signature; a forged/mismatched digest is rejected. ADDED 2026-08-02 (invariant 7, epic-completion pass): the broadcast wrapper ships IN THIS TASK -- scripts/bus-broadcast.sh (AGENTIF-4) must produce both signatures via the `agent-bus sign` subcommand SIGN-2 adds, and AGENT_PROTOCOL.md must document the recipient-set digest and how two recipients compare it. A digest that no wrapper emits and no agent can check is not a defence. Verify through scripts/bus-broadcast.sh against a running throwaway bus, not hand-written curl.
  _Proof: go test -race -run TestBroadcastDigestSignature ./internal/..._

