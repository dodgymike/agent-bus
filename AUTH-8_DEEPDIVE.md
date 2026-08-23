# AUTH-8 — The balance between usability and abuse protection

**Task:** AUTH-8 (`b65948b7-cd1d-4728-9f8e-10a76a1e50a3`), a study, not a code change.
**HEAD:** `da2d58d` ("httpapi: per-source rate limiting on the three unauthenticated auth routes").
**Scope:** read-only. No product code changed. Recommendations that warrant code are filed as Spec
Server tasks under epic AUTH and referenced by id below.

## 0. The question

agent-bus has accreted admission/abuse controls one task at a time: invite-only enrolment, a 4096
roster cap, a global session cap, a per-agent ACTIVE-session cap, and — this session — enrol-key
uniqueness (409) and per-source rate limiting (429). Do they compose into a coherent
usability-vs-abuse posture, or do they fight each other and leave gaps?

Short answer: **they mostly compose, and the composition is deliberate and documented** — the
per-agent PENDING cap that WOULD have fought the others was removed on purpose
(AUTH-1-FU-PENDINGCAP, done). Three real frictions remain, all recoverable-vs-brick usability
issues rather than security holes, and two are already filed. The largest residual is a shared-source
rate-limit bucket (Docker bridge / tunnel) that throttles honest co-located agents, plus the
long-standing gap that enrolment takes no proof-of-possession of either key.

Invariants read IN FULL for this study: **INVARIANTS.md invariant 3** (invite-only enrolment;
sessions are opaque, revocable, server-side handles; the allow-list is THE security boundary and is
not to be widened), **invariant 10** (idempotency; a merely-buggy client must not be disconnected; a
refusal is not a disconnect; the three cases that must not be collapsed), **invariant 11** (mTLS is
required, mutual; the session-token / client-certificate cross-check; `InsecureSkipVerify` permitted
in exactly one place). Invariant 1 (server authoritative on ids) and invariant 8 (stdlib-first, scan
over second index) are cited where load-bearing.

---

## 1. Control inventory

Every control below sits on, or in front of, the **unauthenticated** surface — the three credential
routes `/v1/enroll`, `/v1/session/begin`, `/v1/session/complete` (`authmw.go:76-83` allow-list) —
because those routes ISSUE the credential and so cannot themselves be protected by one (invariant 3).
That is why every admission bound "fails closed": it is the only thing between an anonymous caller
and unbounded server memory (`service.go:20-28`).

| # | Control | Location (file:line) | Default | Fail mode | What a LEGIT agent sees at the boundary |
|---|---------|----------------------|---------|-----------|------------------------------------------|
| A | Invite-only enrolment gate | `service.go:630-632`; wired `cmd/agent-bus/main.go:84,1000` (`enrolmentInviteRequired=true`) | ON (shipped) | fail-closed refuse | `403`-class `ErrInviteRequired` if no invite presented. Recoverable: obtain an invite, `agent-busctl enrol --invite-file`. A pre-gate client hits it once on first call after operator flips it. Connection KEPT (invariant 10). |
| B | Roster cap | `service.go:729-731`, `DefaultMaxRosterEntries=4096` (`service.go:31`) | 4096 | fail-closed refuse (`ErrCapacity`) | `503` + `Retry-After: 5` (`auth.go:884-887`). BUT nothing frees a roster slot and ids never reuse (invariant 1), so once full the 5s is a lie — see §3. Behind the invite gate, an anonymous flood can no longer reach it. |
| C | Idempotency-table cap | `service.go:733-735`, `DefaultMaxIdempotencyEntries=16384` (`service.go:34`) | 16384 | fail-closed refuse | `503`+`Retry-After:5`. Memory-only, no expiry yet (IDEM-11 owns durability+retention, `service.go:960-979`). |
| D | Global session-table cap | `session.go:284-286`, `DefaultMaxSessions=16384` (`service.go:37`) | 16384 | fail-closed refuse (`ErrCapacity`) | `503`+`Retry-After:5`. Self-healing: pending drains in `ChallengeTTL=2m`, active in `SessionLifetime=1h`. Untargeted, unamplified. |
| E | Per-agent ACTIVE-session cap | `session.go:451-459`, `DefaultMaxActiveSessionsPerAgent=32` (`service.go:61`) | 32 | fail-closed refuse, **never evicts** | `503`+`Retry-After:5` (`auth.go:884`). Retry-After is WRONG here: recovery needs up to `SessionLifetime=1h`, not 5s. Filed: AUTH-1-FU-ACTIVECAP-RETRYAFTER (`03a8512b`). |
| F | Per-agent PENDING cap | **REMOVED** (AUTH-1-FU-PENDINGCAP, done); rationale `session.go:229-240` | n/a | n/a | Nothing. Its removal is the single most important composition decision here — see §2. |
| G | Enrol auth-key uniqueness | `authkey.go` + advisory pre-mint read `service.go:807-815`; authoritative in `Roster.Put` | always on | fail-closed refuse (`ErrAuthKeyBound`) | `409` "enrolment public key already bound; enrol with a fresh keypair" (`auth.go:832,848`). Recoverable only by generating a new keypair. |
| H | Enrol cert-fingerprint uniqueness | `service.go:770-780` (advisory pre-mint) + `Roster.Put` authoritative | always on when a cert is presented | fail-closed refuse (`ErrCertFingerprintBound`) | `409` "client certificate already bound; enrol with a fresh client keypair" (`auth.go:830`). |
| I | Messaging-key ≠ auth-key, length checks | `service.go:647-687` | always on | fail-closed refuse (`ErrInvalidPublicKey`) | `400`. Placed BEFORE the mint so a malformed key burns no agent-id suffix (invariant 1). |
| J | Idempotency-key required+shaped | `service.go:637`, `validateIdempotencyKey` `service.go:1006` | required | fail-closed refuse | `400`. Missing key ⇒ refused, because a retry without one mints a second id (invariant 10). |
| K | Per-source rate limit | `internal/httpapi/ratelimit.go`; wired `main.go:1753-1755` | `-auth-rate-limit=5/s`, `-auth-rate-burst=60` (`main.go:61-62`) | fail-**open** if disabled (burst≤0); throttle-with-429 when on | `429` + `Retry-After` it can OBEY (`ratelimit.go:216-232`). Never a disconnect (invariant 10, `ratelimit.go:78-81`). |
| L | mTLS transport + session/cert cross-check | listener `RequestClientCert` (`tlslisten.go`); cross-check `authmw.go:431` `enforceCertBinding` | mTLS required to start; cross-check per-agent | fail-closed on a mismatched pair | An agent WITH a cert binding must present a matching cert or its authenticated request is refused (invariant 11). Absent cert still accepted at enrol (staging state, `service.go:368-384`). |
| M | Bearer-token shape/allow-list (default-deny) | `authmw.go:172-202`, `76-83` | default-deny | fail-closed refuse | `401`. Identical body for unknown/pending/expired (no enumeration oracle, `authmw.go:253-256`). |

Two things to note about the inventory the task framing gets slightly wrong:

- **"pending caps" no longer exist.** The per-agent PENDING cap was deliberately removed
  (control F). Only the GLOBAL session cap (D) and the per-agent ACTIVE cap (E) remain. This matters
  because the removal is the key to why the surviving controls do not fight — see §2.1.
- **The rate limiter is the only control that fails OPEN**, and only when explicitly disabled with
  `-auth-rate-burst 0`. Every internal/auth admission bound fails closed.

---

## 2. Interaction analysis — what composes, what fights, what is redundant

### 2.1 COMPOSE (defence in depth, layered by cost-to-attacker)

The controls form a deliberate funnel on the anonymous surface, cheapest-charge-to-attacker first:

1. **Rate limit (K)** charges the *source* — the only actor a flood can be billed to. It sits IN
   FRONT of the allow-list without widening it (`ratelimit.go:22-24`, invariant 3).
2. **Invite gate (A)** charges the *credential*: with it on, the anonymous enrol path never reaches
   the roster at all (`service.go:612-617`). This is what closed the roster-brick P0 (`1c4d3dea`,
   done; reasoning at `service.go:612-617`).
3. **Roster / session / idempotency caps (B,C,D,E)** are the last-resort global memory bounds if a
   caller gets past 1 and 2 (e.g. an invited caller, or a build with the limiter disabled).

Invite-gating and rate-limiting are **complementary, not redundant** (the task asks): the gate stops
*roster* growth by an anonymous caller but does nothing to stop an anonymous caller hammering
`/v1/session/begin` (which needs only a known agent id, no invite) to fill the *session* table
(control D flood, described `session.go:242-267`). Only the rate limit charges that flood. Conversely
the rate limit alone would still let a caller within its 5/s+60-burst budget slowly grow the roster
forever — the gate is what makes that impossible. Each closes what the other cannot.

**The PENDING-cap removal (F) is what makes E and D compose instead of fight.** The reasoning
(`session.go:229-267`, `session.go:400-424`) is sound and I re-verified it:

- On `/v1/session/begin` the agent id is **attacker-supplied** (unauthenticated). Any per-agent
  bucket keyed on it is a **lockout primitive**: an attacker who knows a victim's id drives requests
  into the victim's bucket, and whether the bucket evicts or refuses at its limit, the victim is
  denied. So there is *correctly* no per-agent cap there.
- On `/v1/session/complete` the agent id is a **proven identity** (someone produced a valid Ed25519
  signature over the server-chosen token with that agent's private key, `session.go:411-416`). A
  per-agent cap here is self-inflicted-only. Safe.

This is the one genuinely subtle composition decision in the codebase, and it is correct. The
asymmetry (no per-agent cap on the unauthenticated route, a per-agent cap on the proven-identity
route) is load-bearing and documented at `service.go:130-143`.

The **idempotency-before-admission ordering** (`service.go:699-722`) also composes cleanly with the
roster cap: a retry of an already-accepted enrolment keeps succeeding even when the roster is full,
because the agent is already in the roster and the replay admits nobody new. Refusing the retry at
the cap would violate invariant 10 (a legitimate retry must return the original result).

### 2.2 FIGHT (usability frictions — all recoverable-vs-brick, none a security hole)

**(i) Shared-source rate-limit bucket starves co-located honest agents.** The limiter keys on
`remoteHost(r)` — the TCP peer with port stripped, proxy headers deliberately ignored
(`ratelimit.go:193-201`). The honest limitation is stated in the code: behind the Docker bridge every
container is `172.17.0.1`; behind a NAT, reverse proxy or SSH tunnel many agents collapse to ONE key
and ONE bucket, so they throttle each other. At the shipped `5/s`, `burst 60`, a cluster of, say, 20
co-located agents each doing the ordinary two-sessions-per-hour refresh (`service.go:48-54`) is fine
in steady state — but a **synchronised reconnect storm** (bus restart, orchestrator boot, a batch of
agents starting together) collapses to one bucket and the 61st request in a burst gets a 429. The
agents that lose are honest and co-located. This is the interaction the task flags, and it is real.
The 429 IS obeyable (Retry-After), so it self-heals — it degrades throughput, it does not brick — but
it means the abuse control's blast radius includes legitimate neighbours. This is NOT already filed as
a distinct task. **Filed: AUTH-8-FU-RATELIMIT-SHAREDSRC (`fe0245a3`), §5 rec (1).**

**(ii) Active-session cap (E) 503 tells the client the wrong recovery time.** `CompleteSession`
returns `ErrCapacity` when an agent already holds 32 active sessions, and the HTTP layer maps every
`ErrCapacity` to `503 Retry-After: 5` (`auth.go:884-887`, `capacityRetryAfterSeconds="5"`
`auth.go:62`). But this refusal is NOT relieved in 5 seconds — nothing evicts, so it clears only when
one of the agent's OWN sessions expires, up to `SessionLifetime=1h` away (`session.go:441-446`). A
well-behaved client that obeys the Retry-After will hot-loop at 5-second intervals for up to an hour.
Already filed: **AUTH-1-FU-ACTIVECAP-RETRYAFTER (`03a8512b`)**, still `todo`. No new task needed;
referenced in the doc.

**(iii) Enrol-key / cert uniqueness (G, H) brick a client that reused a keypair, with no
in-band recovery.** A `409 "enrol with a fresh keypair"` is the correct answer for the abuse it stops
(one keypair holding two identities, `authkey.go:10-27`) and it correctly does NOT disconnect
(invariant 10 — a buggy client that re-enrols with the same key is refused, not dropped). But for a
legitimate agent that legitimately wants to RE-enrol the same identity (lost its session, roster has
no leave route yet — AUTH-4 is still `todo`), the only escape is "generate a new keypair", which
mints a NEW agent id and orphans the old roster slot forever (ids never reuse, invariant 1; nothing
frees a roster slot). This is a latent brick, but it is really a symptom of the missing
leave/re-enrol route (AUTH-4) and the missing operator reclaim path (AUTH-ROSTER-RECLAIM `b418638c`,
todo), both already filed. No new task; noted as a landmine in §4.

### 2.3 REDUNDANT (belt-and-braces, intentionally)

- **Advisory pre-mint uniqueness reads (G,H) vs authoritative `Roster.Put` checks.** Deliberate and
  correct: the pre-mint read (`service.go:770-815`) is advisory and holds only `enrolMu`, so it
  cannot be the authority; `Roster.Put` decides under the roster lock in the same critical section as
  the insert. The redundancy exists so the OVERWHELMINGLY COMMON refusal costs no burned agent-id
  suffix (invariant 1 — a suffix spent on a refused enrol is never reclaimed). This is a good
  redundancy: removing the advisory read would re-open an unbounded suffix-burn loop
  (`service.go:782-800`).
- **Messaging-key length check in Enrol (I) vs `validateRosterEntryKeys` in Put.** Same pattern —
  the early one adds ORDERING (refuse before the mint), not a second decision (`service.go:650-664`).
- **Rate limit (K) vs invite gate (A) — NOT redundant**, see §2.1. Both anonymous-surface controls,
  but they charge different actors and stop different floods.

---

## 3. Honest gap list — abuse still unmitigated

Confirmed against code. Distinguishes already-filed from newly-found.

**Already filed (verified still open, not re-filed):**

- **G1 — No proof-of-possession of EITHER key at enrolment.** Enrol records `PublicKey` and
  `MessagingPublicKey` as presented; nothing proves the enroller holds the matching private key.
  For the AUTH key this is AUTH-1-FU-POPKEY (`6e3083b0`, todo). For the MESSAGING key it is NOT
  covered by that task — `service.go:681-684` says so explicitly (G9): "It cannot stop an enroller
  registering some OTHER agent's public key as its messaging key… That gap is real and is NOT covered
  by AUTH-1-FU-POPKEY… no task covers this one yet." **This was a genuinely un-filed gap.** Filed: AUTH-8-FU-MSGKEY-POP (`576a794d`), §5 rec (2).
- **G2 — Session-table O(n) scans + refuse-not-evict CPU/lock amplification.** Every
  `CompleteSession` does an O(n) active-count scan under `sessMu` (`session.go:451-456`); every
  begin/complete sweeps O(n) (`sweepLocked`). Filed: AUTH-1-FU-SESSIONSCALE (`067b80cf`, todo).
- **G3 — `Authenticate` takes an exclusive `sessMu` on every authenticated request's hot path.**
  Filed: AUTH-2-FU-SESSMU (`160b765b`, todo).
- **G4 — Long-poll outlives its session (revocation not immediate for a parked poll).** Bounded by
  one `MaxPollTimeout=5m` × up to `MaxWaitersPerAgent=32` batches (`authmw.go:274-297`). Filed:
  AUTH-2-FU-POLLEXPIRY (`03d7ca66`, todo).
- **G5 — Rate-limiter GC sweep is O(n) under the mutex once per 60s.** Filed: `855dd855` (todo).
- **G6 — Idempotency table is memory-only and never expires** (`service.go:960-979`). A retry across
  a restart re-applies and mints a second id. Owned by IDEM-11.
- **G7 — Roster DoS availability analysis not yet documented as NON-self-healing** (the roster,
  unlike the session table, never drains). Filed: AUTH-3-FU-ROSTERDOS-DOCS (`d5197abb`, todo), plus
  the reclaim escape hatch AUTH-ROSTER-RECLAIM (`b418638c`) and its name-bound requirement
  (`f505fb57`).

**Newly found (not previously filed):**

- **G8 — Shared-source rate-limit starvation.** §2.2(i), and AUTH-8-FU-RATELIMIT-SHAREDSRC (`fe0245a3`). The limiter's own comment names the
  weakness but no task exists to give co-located honest agents a way out (e.g. an authenticated-agent
  fast-path that is not billed to the shared IP bucket, or a documented "run agents on distinct
  source identities / raise the burst" operator guidance). Filed: §5 rec (1).
- **G9 — Messaging-key proof-of-possession** (the AUTH key half of G1 is filed; the messaging half is
  not). §3 G1, `service.go:681-684`. Filed: AUTH-8-FU-MSGKEY-POP (`576a794d`), §5 rec (2).
- **G10 — No composed budget across the three credential routes.** The rate limiter buckets each
  SOURCE across all three routes together (one bucket per source, `ratelimit.go:57-61` guards the set
  but `allow(key,...)` uses a single bucket per key). But there is no GLOBAL request-rate ceiling and
  no per-route weighting: a source's 60-burst can be spent entirely on `/v1/session/begin` (the
  cheapest amplifier — fills table D) as easily as on enroll. Combined with the un-capped global
  session table, a distributed flood from many sources (each under its own 5/s budget) still fills
  table D. This is the residual the active-cap and rate-limit both explicitly decline to solve
  (`session.go:242-255`, `426-438`). It is a KNOWN, ACCEPTED residual, documented by AUTH-8-FU-POSTURE-DOCS (`46ede035`) (distributed floods need
  network-layer defence), not a bug — documented here so it is not rediscovered. **Not filed as code**
  — it is a documentation/operator-guidance item; folded into §5 rec (3).

**Non-gap (verified, do NOT "fix"):** the `409 no-matching-reservation` indistinguishability
(invariant 10 — a test asserts it) is not in scope here but confirms the pattern: some ambiguities
are deliberately left un-resolved and guarded by a test that goes red if "fixed".

---

## 4. Latent landmines found along the way

- **The active-session cap's correctness rests on an id never being rebound to a different key**
  (`session.go:418-424`). Today the only writer is enrolment (fresh id every time) and auth-key
  uniqueness (G) now enforces one-key-one-id. But when AUTH-4 (leave/revocation) or a re-key route
  lands, an id COULD be re-enrolled under a new key, at which point the per-agent active bucket
  becomes third-party-consumable and the cap must be re-argued. Anyone implementing AUTH-4 must read
  `session.go:400-424` first.
- **Enrol-key uniqueness (G) + no leave route (AUTH-4 todo) = a legitimate re-enrol bricks the
  identity.** §2.2(iii). Not a new task — it is the join of two already-filed gaps — but call it out
  in the AUTH-4 task so leave/re-enrol is designed WITH the uniqueness rule, not against it.
- **The rate limiter fails OPEN when disabled** (`-auth-rate-burst 0`). That is the documented escape
  hatch, but it means an operator who disables it to work around G8 (shared-source starvation) removes
  the ONLY control charging a session-begin flood (D). The workaround for one friction re-opens
  another. Operator guidance (§5 rec 3) should say so.
- **`capacityRetryAfterSeconds="5"` is shared** by every `ErrCapacity` arm (`auth.go:884`), so the
  fix for the active-cap Retry-After (`03a8512b`) must NOT change the value globally — the roster and
  global-session caps genuinely do relieve in ~seconds; only the per-agent active cap does not. The
  fix has to distinguish the arms, which is exactly what `03a8512b` describes.

---

## 5. Prioritised recommendations

Ranked by (harm × likelihood) ÷ cost. Tasks filed via the Spec Server API under epic AUTH; ids below.

**P1 — Give co-located honest agents a path around the shared-source rate-limit bucket (G8).**
Highest-likelihood friction: any Docker-bridge or tunneled deployment (the shipped runtime — the live
bus is a container, per MEMORY) collapses all agents to `172.17.0.1`. Options to evaluate in the task:
(a) an authenticated-request fast-path so only the three UNauthenticated routes are billed to the IP
bucket (already true — but a reconnect storm is all on those three routes, so this alone is
insufficient); (b) a higher default burst tuned to expected co-located fan-out with operator guidance;
(c) per-`(source, requested-agent-id)` sub-bucketing on begin/complete so one agent's storm does not
starve its neighbours — but note the invariant-3 lockout hazard on the unauthenticated `begin` route
(the id is attacker-supplied) means this can only key on the PROVEN id at `complete`, not `begin`.
**Filed: AUTH-8-FU-RATELIMIT-SHAREDSRC (`fe0245a3`).**

**P2 — Close (or explicitly accept) messaging-key proof-of-possession (G9).** An enroller can register
another agent's messaging public key. Consequence: signed-message verification is bound to a key the
enroller may not hold. Lower likelihood than G8 but a real integrity gap that the AUTH-key POP task
(`6e3083b0`) does NOT cover. Task should either add a POP challenge over the messaging key at enrol or
record a DECISIONS.md acceptance with the exact residual. **Filed: AUTH-8-FU-MSGKEY-POP (`576a794d`).**

**P3 — Operator-guidance doc for the composed anonymous-surface posture (G10 + landmines).** No code:
one section in `CONTRACTS-HTTP.md` (or a new `docs/` note) that states the composed model — what each
control charges, that the rate limiter fails open when disabled, that distributed floods need
network-layer defence, and the shared-source caveat. This is the "does it compose" answer made
operational. **Filed: AUTH-8-FU-POSTURE-DOCS (`46ede035`).**

**Already-filed, no new task (tracked, referenced):** AUTH-1-FU-ACTIVECAP-RETRYAFTER (`03a8512b`),
AUTH-1-FU-SESSIONSCALE (`067b80cf`), AUTH-2-FU-SESSMU (`160b765b`), AUTH-2-FU-POLLEXPIRY (`03d7ca66`),
rate-limiter GC (`855dd855`), AUTH-1-FU-POPKEY auth-key half (`6e3083b0`), roster-DoS docs
(`d5197abb`), roster reclaim (`b418638c`), AUTH-4 leave/revocation (`a853261d`).

---

## 6. Cost / risk / rollback

This deliverable changed no product code, so there is nothing to roll back. The doc is additive.

The filed follow-up tasks are all optional hardening/usability; none is a regression fix. Each carries
its own `proof_cmd` sketch. Risk of the recommendations themselves:

- P1 (rate-limit sub-bucketing) touches an unauthenticated hot path and interacts with invariant 3 —
  must go through the full chain (spec-keeper → implementer → test-engineer → reviewer → **security**)
  because it is a control-plane change to the abuse surface.
- P2 (messaging-key POP) touches the crypto/enrol path — invariant 9 (never write crypto; POP is an
  Ed25519 challenge, use stdlib) and invariant 1 apply; security gate mandatory.
- P3 is docs-only; security gate SKIPPED unless it touches a control-plane/guard file (per the roster
  carve-out).

---

## Appendix — evidence index (file:line)

- Invite gate + placement + roster-brick close: `internal/auth/service.go:630-632`, `591-632`,
  `612-617`; wiring `cmd/agent-bus/main.go:84`, `1000`.
- Roster/idempotency/global-session/active caps: `service.go:729-735`, `session.go:284-286`,
  `session.go:451-459`; defaults `service.go:31,34,37,61`.
- Pending-cap removal rationale: `session.go:229-267`, `session.go:400-424`.
- Enrol-key uniqueness: `internal/auth/authkey.go`, advisory read `service.go:807-815`.
- Cert-fingerprint uniqueness + binding: `service.go:770-780`, `851-878`.
- Messaging-key ≠ auth-key + length, pre-mint ordering: `service.go:647-687`.
- Per-source rate limiter, source key + honest limitation: `internal/httpapi/ratelimit.go:82-95`,
  `122-158`, `193-234`; defaults `cmd/agent-bus/main.go:61-62`, `1753-1755`.
- Allow-list (the security boundary, invariant 3): `internal/httpapi/authmw.go:76-83`, `298-373`.
- Cross-check (invariant 11): `authmw.go:417-433`.
- HTTP status mapping (409 / 503+Retry-After:5 / 429): `internal/httpapi/auth.go:62`, `505-527`,
  `783-887`; `ratelimit.go:216-232`.
- Lifetimes: `SessionLifetime=1h`, `ChallengeTTL=2m`, refresh at 0.75 (`session.go:24,30,35`);
  `MaxPollTimeout=5m`, `MaxWaitersPerAgent=32` (`internal/hub/hub.go:88,99`).
