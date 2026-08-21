# INVARIANTS — the standing design contract, with the reasoning

Every change to agent-bus is measured against these eleven invariants. A change that weakens one
needs an explicit decision recorded in `DECISIONS.md`.

**This file holds the RULE and the REASONING. `CLAUDE.md` holds the one-line rules only.** The split
exists because `CLAUDE.md` is injected into every agent on every spawn, while this file is read when
a task actually touches the plane it governs.

**Do not "simplify" the reasoning out of this file.** It is long because the short version failed:
each of the longer passages below was written after an agent violated the rule that the one-line
version states perfectly well. Invariant 11's warning about `InsecureSkipVerify` exists because
deleting that line looks like hardening, silently disables certificate pinning, and leaves every
test passing. Invariant 1's exists because a salvage path reused an index it had already handed out.
If you are about to shorten a paragraph here, you are probably about to reintroduce the defect it
was written to prevent.

**If you are working on a task that touches one of these planes, read that invariant IN FULL before
you start.** The one-line version in `CLAUDE.md` is a reminder, not a specification.

> Status note: this file currently states the DESIGN CONTRACT — what must be true. It does not yet
> state what is ENFORCED IN CODE TODAY, and several invariants are only partly enforced (notably 3,
> 7, 10 and 11). Spec task `HANDOVER-MAP-DOC` adds a per-invariant status and evidence row. Until it
> lands, do not read this file as a description of the running system. The section immediately below
> is the partial, hand-maintained stand-in for that row.

---

## Enforcement status — what is actually true in code today

Relocated from `CLAUDE.md` on 2026-08-21 by the doc refactor (Spec task `f4bd3c9f`), then CORRECTED:
the peer-plane entry reverses the wording it replaces and the rotation entry is new, so this is not a
verbatim move. `CLAUDE.md` keeps the one-line warning and points here for the evidence. This list is
NOT exhaustive: it records the gaps somebody has checked, not every gap that exists. Do not build on
an invariant's guarantee without confirming it holds in the code you are about to touch.

- **mTLS is not REQUIRED, and what a certificate proves depends on the plane (invariant 11).** The
  server REQUESTS but never REQUIRES a client certificate — `ClientAuth: tls.RequestClientCert`,
  `a97f854`. On the AGENT plane a certificate alone authenticates nobody: the session token does
  that, and the cross-check between the two is what the invariant asks for. **On the PEER plane the
  certificate alone AUTHORISES** — `RequirePeerPrincipal` reads no token and no header
  (`internal/httpapi/peerprincipal.go:9-24`), and `internal/httpapi/crosscheck.go:69-72` records
  this as a NAMED NARROWING of invariant 11 (`DECISIONS.md`, 2026-08-14, FEDERATION AMENDMENT ruling
  (i)), "not as compliance with it". A blanket "a certificate authenticates nobody" claim is FALSE
  for the entire federation ingress; `DOC-TRUTH_DEEPDIVE.md` row 26 filed it against the wording
  this entry replaces.
- **Recipients do not verify message signatures on the read path (invariants 9, 10).** The signature
  is carried but not checked end-to-end by the receiving agent. **The verifier already EXISTS** —
  `verifySignedMessage`, `client/canonical.go:515`, documented FAIL-CLOSED at `:479` — it simply has
  no non-test caller, and `client/wedge_test.go:495 TestReadDoesNotYetVerifyReceivedMessages` pins
  the gap deliberately. This is a WIRING gap, not a missing capability. Do not write a second
  implementation: invariant 9 forbids it, and the seam to call is already there.
- **Enrol idempotency is in-memory only (invariant 10).** It does not survive a restart, so the
  invariant's "durable across restart" clause is not met on that path.
- **Certificate rotation is NOT implemented server-side (invariant 11).** The invariant requires
  serving TWO certificates during rollover. `cmd/agent-bus/tlslisten.go:134` serves exactly one
  (`Certificates: []tls.Certificate{cert}`, no `GetCertificate`), and
  `internal/buscert/buscert.go:65-70` states it plainly: "there is no rotation machinery yet ... so
  this expiry is a SCHEDULED OUTAGE". The CLIENT accept-set is real, which is what makes this easy
  to misread as done. `DOC-TRUTH_DEEPDIVE.md` row 27.
- **Enrolment IS invite-gated (invariant 3), as of `3cedcb7`, 2026-08-15** —
  `enrolmentInviteRequired = true`, `cmd/agent-bus/main.go:67`. Verified by forge rather than by
  code reading: 220 refused enrolments (20 via the CLI, 200 raw) grew the data dir by **0 bytes**,
  and a name refused 20 times and then enrolled legitimately received suffix **1, not 21**. The gate
  therefore sits ABOVE the id mint, and invariant 1's never-reuse rule is never engaged by a refusal.

**Why this list is written as evidence and not as a claim.** `CLAUDE.md` asserted the OPPOSITE of
that last item — "enrolment is not yet invite-gated" — for several hours after the gate shipped. A
stale "not yet implemented" note is more dangerous than no note at all, because it reads as freshly
checked. When you correct an item here, search for its twins in the same change; see
`PITFALLS.md` §5.

*(This entry carried "Known still-stale twin: `client/enrol.go:64` repeats the old claim" until
2026-08-21. That warning was itself five days stale: `ad03e13` (DOCS-22, 2026-08-16) corrected
`client/enrol.go:63-67`, which now says the shipped bus REQUIRES an invite. It was caught by the
reviewer gate on the very change that relocated it, which is the hazard this section is about.
`DOC-TRUTH_DEEPDIVE.md` row 35 still cites the old wording and is stale for the same reason.)*

## The eleven invariants

These are the load-bearing invariants. Every change is measured against them; a change that weakens
one needs an explicit decision recorded in `DECISIONS.md`.

### Invariant 1 — Server-authoritative ids, never reused

1. **The server is AUTHORITATIVE on every id.** Bus ids, agent ids, message ids, and sequence
   numbers are minted by the server and never by a client. A client-supplied id is input to be
   validated, never an identity to be trusted. **Ids are never reused, including across restarts —
   and this was reaffirmed WITHOUT narrowing on 2026-08-02.** Recovery may not reissue an index it
   has already handed out, even for a record it discards: a salvage path that reuses the index of a
   damaged tail record is a DEFECT to fix, not a licence to narrow this invariant. When recovery
   discards a record, the sequence advances past the hole; it never rewinds. Contrast invariant 4,
   which WAS deliberately narrowed — this one was not.
   **Enforcement point narrowed 2026-08-14 (`SIGN-1-FU-OUTOFORDER-POISON`, `DECISIONS.md`) — the
   STORE's check, NOT this invariant:** `store.Append` no longer requires strictly-increasing
   sequences; it enforces that no sequence is served twice (via a retained-window sequence index)
   and that the slice stays sorted by DELIVERY POSITION — amended 2026-08-14 by
   `SIGN-1-FU-REORDER-WATERMARK`, which moved the serving copy off sequence order onto the WAL
   commit index; the slice is no longer sequence-sorted,
   `head` uses `max` and never rewinds, and allocation is structurally independent of `store.head`.
### Invariant 2 — Fully-qualified agent ids

2. **Every agent id is fully qualified: `<bus-id>.<agent-id>`.** That namespacing is what makes
   cross-bus routing and agent-list exchange unambiguous. Buses have ids for the same reason.
### Invariant 3 — Invite-only enrolment; client signs a server-provided token

3. **Enrolment is INVITE-ONLY (2026-08-02), and the CLIENT signs a SERVER-PROVIDED session
   token.** No agent may enrol without redeeming an operator-minted invite. This closes the root
   cause of a whole family of pre-auth attacks rather than patching them one at a time: an
   unauthenticated enrolment route let an attacker mint its own agents, and from there exhaust the
   session table, lock out a named agent, or enumerate the roster. Invites must be single-use,
   expiring, and revocable, and redeeming one is the ONLY way onto the bus — including for peer
   buses.

   On the credential itself: Note the direction — an earlier wording had
   this backwards ("the server signs the agent's key"), which is neither the decision nor the code.
   At enrolment the agent presents its Ed25519 **public** key and the server records it. To get a
   credential the agent asks for a session, the **server** provides the token value, the agent
   **signs that value** with its private key, and the server verifies against the recorded public
   key. The client never chooses the bytes it signs — a client-chosen challenge permits
   pre-computation and proves far less. Sessions last **at most one hour**; the client refreshes at
   75% of lifetime. Tokens are **opaque server-side handles, not signed claims**, which is precisely
   what makes immediate revocation possible — stateless claims cannot be revoked before they expire.
   Sessions do NOT survive a restart. Every route authenticates EXCEPT the **six** on the explicit
   allow-list in `internal/httpapi/authmw.go` (`unauthenticatedRoutes`, exported as `httpapi.UnauthenticatedRoutes()`), whose own doc comment justifies each one
   individually and is the authority if this paragraph and it ever disagree:

   - the three that necessarily cannot authenticate, because they are how a credential is obtained:
     `/v1/enroll` (no identity exists yet), session-begin (called with NO session at all — it is the
     request that ASKS for a token to sign), and session-complete (subtler and the one that looks
     skippable: the caller does hold a token, but a PENDING one, which `auth.Service.Authenticate`
     rejects exactly like an unknown one — so a bearer requirement here would be *unsatisfiable*,
     not strict; the real authentication on that route is the Ed25519 signature over the
     server-chosen token);
   - `/healthz` — liveness, called by probes before any agent exists, returns no state at all;
   - `/v1/info` — pre-enrolment discovery, deliberately limited to bus id, version, uptime and the
     discovery path;
   - `/v1/discovery` — the protocol document. **This one was missing from this list until
     2026-08-21**, and its omission is the more instructive half of the error: it is anonymous for a
     circular reason that makes it necessary — it is HOW A CALLER HOLDING ONLY A URL LEARNS TO
     ENROL, so requiring the credential it explains would make it unreachable by everyone who needs
     it. It is safe to serve anonymously because it carries no bus state: a compile-time constant
     document plus the bus id `/v1/info` already serves the same caller, and its endpoint list is
     NOT derived from the registered routes, so it does not disclose which optional surfaces this
     build serves.

   **UNDERCOUNTING AN ALLOW-LIST IS NOT A HARMLESS DOC BUG.** *(Historical, reconciled 2026-08-21;
   this is a record of a defect that HAS been fixed, not a live one.)* Until 2026-08-21 this repo
   carried three different counts of this allow-list at the same time — the code's, a smaller one
   here and in `CLAUDE.md`, and smaller again in the failure message of a test under
   `internal/httpapi`, which stated a count of its own. Every one of them read as freshly checked,
   which is why all three survived so long. They have since been reconciled against the code: this
   file and `CLAUDE.md` cite that list BY NAME rather than by line number, and the test message now defers to
   `httpapi.UnauthenticatedRoutes()` instead of naming a number. The lesson outlives the defect,
   because a count in prose goes stale in silence while the list itself cannot. An
   auditor reconciling the code against the docs finds an entry the docs do not mention and cannot
   tell, from the docs alone, whether it is a documented exemption or an ungated route somebody
   added quietly — which is exactly the question this invariant exists to answer. The middleware is
   **default-deny**, so a route added tomorrow is authenticated the moment it is registered; that
   property is what keeps the failure bounded, and it is the reason the allow-list, not the prose,
   is the security boundary. The `/v1/peer/` routes are NOT on it and must never be added: they are
   authenticated by TLS client certificate through `RequirePeerPrincipal`, so allow-listing one
   would not document an existing exemption, it would CREATE an ungated federation ingress.
### Invariant 4 — Nothing acknowledged before durable

4. **Nothing is acknowledged before it is durable.** A send returns success only after the message
   is committed via the two-phase (prepare → commit) write path and fsynced. Never trade that for
   latency. **Narrowing (2026-08-02):** this guarantees we never lose acknowledged data through our
   own write path. It does NOT promise acknowledged data survives damaged media — see invariant 6,
   where availability wins and the discard is logged.
### Invariant 5 — Memory serves, disk is truth

5. **Memory is the serving copy; disk is the truth.** State is held in memory for speed and rebuilt
   by replaying the durable store on start. A crash at any point must recover to a state that is a
   prefix of the accepted history — no torn records, no acknowledged-but-lost messages.
### Invariant 6 — Append-only log: metadata and routing only

6. **Every message is also written to an append-only log — METADATA AND ROUTING INFO ONLY.** The log
   is the audit trail: message id, sequence, sender, recipient(s), bus path traversed, timestamp,
   size, and content hash. It does **not** record message bodies. That is a deliberate decision
   (2026-08-02) taken so the audit trail stays compatible with end-to-end encrypted, forward-secret
   payloads — a log holding plaintext would be unwritable the moment PFS lands, and a log holding
   ciphertext it can never decrypt would be dead weight. The log is append-only in the strict sense: no in-place edits.
   **Recovery ALWAYS reaches a running server (2026-08-02): damaged records are discarded and the
   bus starts.** It must never refuse to boot over corruption — a bus held hostage by one bad sector
   is worse than a bus that has lost a message and said so. The absolute requirement is that every
   discard is LOGGED, loudly and specifically: silent discard is the actual defect (it was rated P0),
   not discard itself. Integrity is protected by a keyed MAC (`crypto/hmac` + `crypto/sha256`), never
   a CRC — a CRC is unkeyed and linear, and a remote client was shown able to forge one.
### Invariant 7 — The compiled Go CLI is THE client

7. **Nobody hand-writes HTTP — the compiled Go CLI is THE client.** Every capability ships with a CLI
   subcommand and an `AGENT_PROTOCOL.md` entry **in the same task**. A feature without its subcommand
   is not done. The CLI **replaces** the `scripts/bus-*.sh` wrappers (decided 2026-08-02); shell
   wrappers are no longer the delivery vehicle, and the ones that exist are to be retired as their
   subcommands land. It does all the heavy lifting: key generation and storage, session-token refresh,
   long-polling with cursor management, reconnect/backoff, and verification of inbound messages.

   It has **three audiences, and all three are requirements, not aspirations**:
   - **A human**, interactively: readable default output, sane defaults, `--help` that answers the
     common question, and errors that name the remedy rather than the stack.
   - **An agent**, shelling out: `--json` on every command, stable documented exit codes, never an
     interactive prompt, and credentials from config/env rather than a TTY. The long-poll command
     streams newline-delimited JSON so it can be piped and consumed incrementally.
   - **An agent, embedding it**: the CLI is a thin shell over a reusable Go client package. That
     package therefore CANNOT live under `internal/` — an importable client is the whole point of
     "embed", and putting it in `internal/` silently forecloses it.
### Invariant 8 — Simple beats clever

8. **Simple beats clever.** Go stdlib first. A third-party dependency needs a justification in
   `DECISIONS.md`.
### Invariant 9 — Never write your own crypto

9. **NEVER write your own crypto.** This is absolute and overrides every other preference in this
   file, including invariant 8's stdlib-first bias and any argument from simplicity, elegance,
   dependency count, or performance. Always use a well-known, standard, audited crypto library, and
   pick the one that **wraps as much of the problem as possible** — prefer a high-level,
   misuse-resistant API (`crypto_sign`-style sign/verify, sealed boxes) over assembling primitives
   yourself. Specifically forbidden without explicit user consent recorded in `DECISIONS.md`:
   implementing or "adapting" a cipher, hash, MAC, KDF, signature scheme, key exchange, or ratchet;
   hand-rolling a padding, nonce, or IV scheme; inventing a bespoke construction out of otherwise-
   good primitives. The reason this outranks everything else is that broken crypto **fails
   silently** — it still encrypts, it still verifies, it simply provides none of the protection it
   appears to. No ordinary test suite detects it, so "our tests pass" is not evidence. When no
   suitable library exists, the answer is to change the requirement or stop and ask — never to
   write it yourself.

### Invariant 10 — Duplicate detection and idempotency everywhere

10. **Duplicate detection and idempotency, everywhere.** Every mutating operation — enrol, send,
    broadcast, leave, peer-enrol, relay — carries a client-supplied idempotency key and is safe to
    retry. The server durably remembers which keys it has already applied, and that memory survives
    restart (it is part of the recovered state, not an in-memory cache). No operation may be applied
    twice.

    **The distinction that makes this correct, and must not be collapsed:**
    - **Same key + same payload = a legitimate retry.** The ack was probably lost in flight. Return
      the ORIGINAL result, do not re-apply, do not error, and do NOT disconnect. This is the whole
      point of idempotency: it exists so a well-behaved client can safely retry, and punishing that
      would break exactly the clients doing the right thing.
    - **Same key + DIFFERENT payload = a protocol violation.** The client is reusing a key for new
      content, which is either a serious bug or an attack. **Reject it and log it, but do NOT
      disconnect** (narrowed 2026-08-08, by user decision, after the behaviour was measured at the
      raw socket). The key is the caller's OWN — keys are scoped per agent — so this is
      overwhelmingly a client that lost track of its keys, and dropping the socket destroys every
      other request it had pipelined there, including its parked long-poll. That is an abuse defence
      landing on the party most likely to be honest.
    - **Replay of an already-accepted signed message** (by a peer, a relay, or a third party) is
      rejected outright **and disconnects the sender**. A signature does not stop replay — a valid
      signed message can be resent verbatim — so freshness is enforced **server-side at ingest**, by
      refusing an already-accepted signed message. It is **not** derived from sequence ordering
      (corrected 2026-08-14, SIGN-1-FU-REORDER-WATERMARK): the sequence is minted at reservation time
      and may be spent out of order, so it is an **identity**, never a freshness or ordering token,
      and the delivery cursor is an opaque server-assigned **position**, not a sequence. A recipient
      that filters on sequence ordering re-implements the very defect that task fixed, client-side,
      and silently loses messages the bus has already acknowledged. This is the one party the
      disconnect is for: it presents material it was never issued. On `/v1/send` it is detected by
      the `sender` inside the signed bytes not being the authenticated principal, and **the
      disconnect fires ONLY when that claim is a well-formed fully-qualified `<bus-id>.<agent-id>`**
      — an absent, unqualified or whitespace-padded claim names nobody, is still refused, and must
      NOT disconnect.
    - **Before adding ANY disconnect, ask two questions.** Can a merely BUGGY client reach this
      line? And does this connection carry only ONE principal's traffic? The second is not yet
      load-bearing but becomes so the moment relay ingest lands, where a peer bus legitimately
      presents `sender != principal` for many agents at once. Both questions exist because the first
      implementation of this narrowing *reproduced the very bug it was fixing* — it disconnected on
      any sender mismatch, so an empty sender, a dropped bus prefix and a trailing space each
      dropped an honest client's socket.
    - **One ambiguity is deliberately left un-disconnected**: `409 no-matching-reservation` is
      byte-identical for a third party spending someone else's mint and for an agent re-presenting
      its OWN spent reservation. The minting agent is not recoverable at that point, so disconnecting
      would punish the honest case. A test asserts that indistinguishability, so it goes RED the day
      it becomes resolvable.

    Relay is where this earns its keep: a cyclic peer topology plus at-least-once delivery means
    duplicates are not an edge case but the normal steady state, and loop-prevention via the traversed
    bus path is a *complement* to idempotency, never a substitute for it.

### Invariant 11 — TLS required, mutual, self-signed, no TOFU

11. **TLS is the required transport. There is no plaintext listener.** Decided 2026-08-02. Every
    HTTP surface — client and bus-to-bus relay — is served over TLS, and the server refuses to
    start rather than fall back to plaintext. This is not defence in depth layered on something
    already safe: without it the session token, which is a **bearer credential**, crosses the wire
    in clear, and an on-path observer can read it or kill a pending challenge. The loopback default
    (invariant: `-listen 127.0.0.1:8080`) stays — it bounds exposure, it does not replace TLS, and a
    bus deliberately exposed on a real interface needs both.

    Consequences that must be designed, not assumed:
    - **Certificates are SELF-SIGNED and TLS is MUTUAL (decided 2026-08-02).** Both ends present a
      certificate and both verify. There is no CA, and **there is no trust-on-first-use either**:
      the **invite blob carries the bus's certificate fingerprint** alongside the bus id, address and
      invite secret, so the client knows what to expect BEFORE its first connection. The agent's
      client-certificate fingerprint is bound to its server-minted agent id at enrolment. A bus runs
      on a laptop with no certificate authority anywhere in the picture.

      **Consequence: the invite blob is now the trust anchor, so the integrity of the channel it
      travels over is load-bearing.** Whoever can substitute an invite can point an agent at a bus of
      their choosing. That is a real requirement on invite distribution, not a footnote — and it is
      the price of eliminating the TOFU window, which is the right trade.
    - **Certificate rotation serves TWO certificates during rollover** so clients can re-pin without
      downtime. Rotation must never require every client to re-enrol.
    - **mTLS and the session token are BOTH required, and they do different jobs.** mTLS proves which
      key holder is on the connection; the session token is the revocable, time-bounded application
      credential. Do not let one silently replace the other — but DO cross-check them: a session
      token presented over a connection whose client certificate belongs to a different agent must
      be rejected, which is a stronger property than either mechanism gives alone.
    - **The CLI must make the trusted path the easy path.** Whatever the scheme, `bus enrol` against
      a fresh bus has to work without the user hand-editing a trust store.
    - Never disable certificate verification to make something work, and never ship a flag that
      does it silently — we read that as forbidding such a flag AT ALL, since a documented hole is
      not better than a hidden one, it is a hole with a manual. Per invariant 9 the TLS stack is
      stdlib `crypto/tls` — configured, never reimplemented.
    - **`InsecureSkipVerify: true` is permitted in EXACTLY ONE FILE — `client/pin.go` — and only
      paired with `VerifyPeerCertificate` (narrowed 2026-08-07, MTLS-PIN).** The earlier absolute
      ban could not survive contact with this invariant's own requirements: self-signed, **no CA**,
      **no TOFU**. Go's default chain verification cannot succeed and cannot be configured to — there
      is no root to chain to, and the client holds a 32-byte fingerprint rather than the certificate,
      so it cannot build an `x509.CertPool` either. `crypto/tls` supports exactly one way to
      substitute a verification policy: disable the default chain check and supply
      `VerifyPeerCertificate`. A ban with no exception would not have prevented the exception — it
      would have pushed it into a package the guard does not scan, which is strictly worse than one
      loud, reviewed occurrence.

      **Read this before "fixing" that line: deleting it, or deleting the callback beside it, does
      not harden anything — it silently disables pinning.** A `tls.Config` with the callback removed
      still compiles, still completes handshakes, still returns working connections, and verifies
      nothing. Every positive test passes either way.

      What replaces the ban is stricter and mechanical, in `client/guard_test.go`: the literal must
      appear in exactly one file **exactly once** (counted, so naming it in prose there fails too);
      an **AST** walk — not a grep — requires any composite literal setting it `true` to set
      `VerifyPeerCertificate` non-nil **in the same literal**, bans setting it by assignment (an
      assignment can be conditional and far from the literal), and requires **at least one** such
      paired literal to exist, so the guard cannot pass on a tree where pinning was deleted.

      What is given up, exactly: CA chain building (there is none, by design) and **hostname
      verification** — for which the pin substitutes and is strictly stronger, since a name check
      asks "does this certificate claim this address" and the pin asks "is this the exact certificate
      the invite named". **Certificate expiry is NOT checked** — a real gap owned by `MTLS-VERIFY`.
      Full reasoning in `DECISIONS.md` (2026-08-07, MTLS-PIN §2), which supersedes the absolute
      wording at `DECISIONS.md:1290` and `:2461` in place.

