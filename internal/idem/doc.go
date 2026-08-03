// Package idem defines the SHAPE of the client-supplied idempotency key
// invariant 10 (CLAUDE.md, 2026-08-02) requires on every mutating call, and
// validates it. This is task IDEM-10: the key format, its untrusted-input
// validation (invariant 1: a client-supplied value is checked, never trusted),
// and its PER-AGENT scoping. It does NOT implement the durable applied-key
// store (IDEM-11), the retry-returns-original-result behaviour (IDEM-12), or
// the key-reuse-different-payload violation/disconnect path (IDEM-14) — those
// tasks consume the types defined here and must not re-derive them.
//
// # Why this lives in doc.go and not CONTRACTS.md / PROTOCOL.md
//
// The task's own instructions call for the spec to land in CONTRACTS.md
// and/or PROTOCOL.md. Both files were owned by other in-flight waves at the
// time this task ran (the MSG/POLL wave owns CONTRACTS.md, the SIGN-1 agent
// owns PROTOCOL.md), so writing there was out of this task's file-ownership
// boundary. The full normative spec is written here instead — genuinely the
// right home for a shared validation helper's contract — and the orchestrator
// carries the CONTRACTS.md paragraph below verbatim into that file once it is
// free. Nothing here contradicts that paragraph; it is a superset.
//
// The exact CONTRACTS.md entry to paste in when that file is free:
//
//	### Idempotency key (invariant 10)
//
//	Every MUTATING route requires an `Idempotency-Key` request header:
//	`POST /v1/enroll`, `POST /v1/send`, `POST /v1/broadcast`, `POST /v1/leave`,
//	`POST /v1/peer/enroll`, `POST /v1/relay`. A missing header on one of these
//	routes is a 400 — the server never mints a substitute key on the caller's
//	behalf. The key is at most 128 bytes and matches `[A-Za-z0-9._-]+`; a key
//	failing either check is a 400 and is not echoed back if it is oversized.
//	Read-only routes (`GET /v1/agents`, `GET /v1/wait`, `GET /v1/messages`,
//	`GET /healthz`, `GET /v1/info`) do not take one and MUST reject it if
//	present with a 400 (a client sending one there is confused about what is
//	being retried). The applied-key lookup is scoped by
//	(fully-qualified `<bus-id>.<agent-id>`, operation, key) — never by key
//	alone — so one agent can never collide with or probe another agent's key
//	space. See `internal/idem` (IDEM-10) for the validator and scope type, and
//	IDEM-11 for the durable store this key shape feeds.
//
// # 1. The wire field name and carrier: ONE canonical carrier
//
// The key travels as the `Idempotency-Key` HTTP request header (HeaderName in
// this package), never in the request body. This is the conventional choice
// (Stripe, GitHub and most HTTP APIs with this feature use a header), and it
// is also the one that makes deliverable 2 below actually achievable: an HTTP
// header's size is bounded by the server's own header-size limit
// (net/http's Server.MaxHeaderBytes) independently of any body-parsing at
// all, so an oversized key is rejected by the TRANSPORT before a single byte
// of a — possibly large — JSON body is read into memory. A key living in the
// body would require reading (at least part of) the body before it could be
// validated, reopening exactly the unbounded-allocation-before-validation
// hole AUTH-6 closed for the mux. One carrier, never two: a key that could
// arrive by either route would eventually disagree with itself (a header
// value and a body field diverging on retry), so this package defines no body
// field and no fallback.
//
// # 2. Length cap and charset: EXACT, and chosen to match existing precedent
//
// MaxKeyLen = 128 bytes. KeyCharset = `[A-Za-z0-9._-]` (ASCII letters,
// digits, dot, underscore, hyphen). Both numbers are NOT a fresh choice: they
// are exactly auth.MaxIdempotencyKeyLen and the charset
// auth.validateIdempotencyKey already enforces for enrolment (which shipped
// in a concurrent wave, ahead of this task landing). Task IDEM-10's own
// mandate is that the rule "cannot be implemented inconsistently
// route-by-route" — picking a different cap or charset here would create
// exactly that drift on day one, between the one route that already validates
// and every route that will use this package. See the FOLLOW-UP note at the
// bottom: internal/auth's copy should eventually import this package instead
// of maintaining a parallel definition.
//
// Rationale for the values themselves, restated for a package that stands on
// its own:
//
//   - 128 bytes comfortably holds a UUID (36), a ULID (26), or a
//     hash-based idempotency key (64 hex chars for SHA-256) with room to
//     spare, while remaining small enough that even DefaultMaxIdempotencyEntries
//     (16384, see auth.Options) worth of remembered keys is a bounded, modest
//     amount of memory — 16384 * 128B ≈ 2MiB for the raw keys alone.
//   - The charset is restricted to bytes that are safe to write into the
//     server log UNESCAPED, because every accepted key WILL be logged (it is
//     part of the audit trail, invariant 6, and of any later idempotency
//     violation reported). Printable-ASCII-including-space was considered and
//     rejected: space and most punctuation have no reason to appear in a
//     machine-generated retry token, and admitting them buys nothing but a
//     wider log-injection and terminal-escape surface (the same reasoning
//     httpapi.SanitizeRequestID and auth.validateIdempotencyKey already
//     applied). `[A-Za-z0-9._-]` is exactly wide enough for a UUID, a ULID, or
//     a hash, and no wider.
//
// Fail-fast: ValidateKey checks len(key) > MaxKeyLen BEFORE it scans the
// charset, so an oversized key is rejected in O(1) rather than after an O(n)
// regex-equivalent scan — the string's length is already known to the Go
// runtime, so this check allocates nothing and costs nothing proportional to
// key size. Combined with the header carrier (point 1), an over-cap key is
// rejected before any body parsing happens at all, which is what makes the
// bound genuinely unbounded-allocation-proof rather than merely "checked
// eventually".
//
// # 3. Per-agent (and per-operation) scoping, enforced by the type system
//
// The lookup key IDEM-11 will build is NEVER a bare string. It is the Scope
// type in this package, whose fields are unexported: the ONLY way to obtain a
// Scope is through NewAgentScope or NewEnrolScope, both of which require a
// non-key component (an agent id, or the fixed enrol discriminant) before
// they will construct one. There is no exported constructor that takes only
// a key, so a caller cannot accidentally — or deliberately — build a
// key-only lookup even by mistake; the compiler enforces the tuple shape.
//
// Why this matters, stated explicitly per the task brief: without per-agent
// scoping, one agent's idempotency key can collide with another agent's,
// which is bad in TWO distinct ways, not one:
//
//   - COLLISION: agent A retries an operation with key "k"; agent B, choosing
//     keys the same way (sequential integers, a shared client library's
//     default), independently uses "k" for something unrelated. With a
//     key-only table, B's request either corrupts A's retry bookkeeping (A's
//     later legitimate retry replays B's result) or is rejected as
//     ErrIdempotencyKeyReused for content it never sent — either way, A and B
//     are both wrong about their own history through no fault of their own.
//   - PROBING: even without an accidental collision, a key-only table makes
//     "does key X already exist?" answerable by ANY agent for ANY key,
//     including one it never generated. That turns the idempotency table into
//     an oracle over another agent's traffic pattern — presence/absence of a
//     key, timing of when it started existing — the exact class of
//     cross-agent leak invariant 2's `<bus-id>.<agent-id>` namespacing exists
//     to prevent everywhere else in this system. Scoping by agent closes both
//     failure modes with the same fix: B's "k" and A's "k" are different Scope
//     values, so B's requests about "k" can never observe or affect A's.
//
// # The scope tuple also carries the OPERATION, not just (agent, key)
//
// The withdrawn duplicate epic's folded-in content asks this explicitly:
// without an operation component, one agent reusing the same key across two
// different mutating routes (e.g. retrying a `send` with key "k", then later
// calling `broadcast` and, by coincidence or a buggy client reusing a
// counter, also using "k") collides with ITSELF — the second call is either
// misread as a retry of the first (wrong: different operation, so almost
// certainly a different payload, which correctly trips
// ErrIdempotencyKeyReused) or, worse, satisfied from the first call's stored
// result if the check is loose about what "the same operation" means. Scope
// is therefore (agent, Operation, key), a 3-tuple, not 2. This costs nothing
// — Operation is a fixed, small enum (below) — and removes an entire class of
// same-agent, cross-route confusion for free.
//
// # 4. Enrolment is the awkward case, and is settled HERE
//
// Enrol has no authenticated caller: invariant 3 says the credential is
// ISSUED by enrolment, so there is no proven agent id yet to scope by, and
// scoping by the client's UNAUTHENTICATED, self-reported name would let any
// caller squat or collide another name's retry bookkeeping just by asking for
// it — worse than the general case, not better.
//
// This package's answer: NewEnrolScope(key) builds a Scope discriminated as
// "the enrol operation, bus-wide" — every enrol attempt on this bus shares
// one key space, regardless of the (unauthenticated, unverified) name or
// public key presented. This is deliberately what auth.Service already does
// today (its idem map is keyed by the raw IdempotencyKey string alone, one
// map per Service instance = one bus): NewEnrolScope formalises that existing
// behaviour as a typed Scope rather than introducing a THIRD, different
// answer for the same question.
//
// This is recorded here as the decision, WITH an open caveat IDEM-13 must
// resolve, not paper over: a bus-wide, content-blind scope means an
// unauthenticated caller CAN squat a key — present idempotency key "k" for a
// bogus enrolment first, and a legitimate client's later genuine retry of key
// "k" is rejected as ErrIdempotencyKeyReused (a false "reused with different
// payload", surfaced to a legitimate client as a 409 instead of a 4xx it
// could not have avoided). Scoping additionally by the PRESENTED PUBLIC KEY —
// (bus, "enrol", public-key-bytes, key) — would close the squat (two
// different callers can never share a scope because they necessarily present
// different key material, and a caller retrying genuinely presents the same
// public key both times), at the cost of a slightly richer scope tuple for
// this one operation. NewEnrolScope's signature is deliberately narrow
// (bus-wide only) so this decision is easy to revisit without a call-site
// rewrite: IDEM-13 should read this paragraph before wiring auth.Service onto
// this package, and either accept the documented squat risk explicitly or add
// the public-key component before enrol's real dedupe table goes live.
// Whichever it picks, it must not be silently different from what ships.
//
// # 5. A missing key on a mutating route is an error, never a substitute
//
// FromRequest is the only way this package extracts a key from an inbound
// request, and it returns ErrMissingKey when the header is absent or empty —
// it never generates one. There is no exported function anywhere in this
// package that mints, derives or defaults an idempotency key; the only keys
// that exist are ones a client supplied. This is a structural guarantee, not
// a runtime check: a caller cannot "fall back" to a generated key because no
// function to generate one is exposed to fall back to. A caller integrating
// this package that wants that behaviour has to write it themselves, in code
// that is not this package's — making the anti-pattern visible in a diff
// instead of hidden behind a helper name that sounds safe.
//
// # 6. Routes, both directions, exhaustively
//
// MUTATING (a key is REQUIRED; ErrMissingKey on absence):
//
//	enrol · send · broadcast · leave · peer-enrol · relay
//
// (see the Operation constants below — one per route, used as the middle
// element of Scope)
//
// READ-ONLY (a key MUST NOT be presented; reject with 4xx if one is, since a
// client sending an idempotency key on a read is confused about what
// operation it thinks it is retrying):
//
//	GET /v1/agents · GET /v1/wait · GET /v1/messages · GET /healthz ·
//	GET /v1/info
//
// This package does not wire the read-only rejection itself (there is no HTTP
// layer here) — that lands with the httpapi route handlers that consume
// FromRequest. It is enumerated here so the rule is exhaustive in both
// directions rather than a list of what requires a key with everything else
// left to guesswork.
//
// # 7. Invariant 1, stated explicitly for this key
//
// The idempotency key is client-supplied input to VALIDATE (this file), and
// it must NEVER become, seed, or be made derivable into a message id, an
// agent id, or a sequence number. All of those stay exclusively server-minted
// per invariant 1. In particular, a future implementer must resist the
// tempting shortcut of using the idempotency key (or a hash of it) as part of
// a message id: that would let a CLIENT choose bits of a server-authoritative
// identifier, which is precisely what invariant 1 forbids.
//
// # 8. The payload fingerprint (feeds IDEM-12's retry check and IDEM-14's
// violation check)
//
// Fingerprint hashes the semantic fields of a request with crypto/sha256
// (stdlib; see CLAUDE.md's absolute no-hand-rolled-crypto rule — SHA-256 via
// crypto/sha256 is a plain hash used only for content-equality comparison
// here, not a MAC or a signature, so the stdlib package is exactly the right
// tool and there is no misuse-resistance property being skipped). It accepts
// the fields as separate byte slices and length-prefixes each one (an 8-byte
// big-endian length, then the bytes) before feeding it to the hash, so field
// boundaries can never be ambiguous: Fingerprint([]byte("ab"), []byte("c"))
// and Fingerprint([]byte("a"), []byte("bc")) hash to DIFFERENT digests even
// though naive concatenation would make them equal. This matters because
// IDEM-12's legitimate-retry path and IDEM-14's violation path both turn on
// same-payload-vs-different-payload, and an ambiguous fingerprint makes that
// distinction unreliable in BOTH directions — a same-fields-different-meaning
// collision would let a genuine payload CHANGE slip through as a "retry", and
// (harmlessly, but confusingly) a byte-identical resend split across
// different field boundaries would look like a "different payload".
//
// The route spellings above were CORRECTED on 2026-08-02 (IDEM-11): this block
// originally said `POST /v1/enrol` and `POST /v1/peer-enrol`, neither of which
// exists. The real routes are `/v1/enroll` (CONTRACTS-HTTP.md) and
// `/v1/peer/enroll` (relay.PeerEnrollPath). Note that the Operation CONSTANT
// idem.OpPeerEnrol is still the string "peer-enrol" and is deliberately NOT
// changed: it is a scope LABEL, not a route, and it is already baked into
// fingerprints and durable applied-key records.
//
// Each route that uses Fingerprint must document, at ITS call site, the fixed
// field list and order it hashes — e.g. send might hash
// [senderID, recipientID, body]; that per-operation list is that operation's
// task to define (IDEM-11 and the route tasks after it), not this one's: this
// task defines the collision-safe MECHANISM, not every route's field list.
//
// # 9. The DURABLE APPLIED-KEY STORE (IDEM-11) — added 2026-08-02
//
// The store IDEM-10 described as future work now lives in this package:
// Record (record.go) is one durably-remembered applied operation, and Store
// (store.go) is the table of them.
//
//   - DURABILITY. A Record is encoded (Record.Encode) BEFORE the durable write
//     and rides in wal.Entry.Idem — the SAME two-phase prepare→commit→fsync
//     transaction as the effect it records, because a wal transaction carries
//     exactly one Entry. It is therefore impossible for the effect to be
//     durable while its applied-key record is not; a crash in that gap plus a
//     client retry is precisely the duplicate invariant 10 exists to prevent,
//     and the gap would be small enough to be invisible in ordinary testing.
//   - RECOVERY. The table is REBUILT by WAL replay (hub.Apply), which is what
//     makes it recovered state rather than an in-memory cache. Logs written
//     before the field existed still rebuild, from the message record's own
//     idempotency key — see hub.Apply's back-compat path.
//   - RETENTION, DERIVED. retention.go derives RetentionWindow term by term
//     (peer outage budget + max session lifetime + max parked poll + the
//     client's own retry horizon, doubled) rather than picking a round number,
//     and MaxEntries from an explicit per-record size budget.
//   - THE GUARANTEE. "Duplicates are suppressed within the retention window" —
//     NOT unconditional exactly-once. A retry arriving after its key expired is
//     applied as a NEW operation. Store's doc comment states in full why
//     fail-closed is not implementable over opaque client-supplied keys, and
//     what makes the boundary honest (a derived window, plus Stats.Expired and
//     Stats.OldestAge so the bound is observable rather than assumed).
//
// # FOLLOW-UP (not this task's scope, recorded so it is not lost)
//
// internal/auth already ships its own local validateIdempotencyKey(key) for
// enrolment (unexported, in internal/auth/service.go), built in a concurrent
// wave before this package existed. It happens to already agree with
// MaxKeyLen and KeyCharset here (see point 2), which is why this package
// matched rather than diverged from it — but it is a SECOND, independent
// definition of the same rule, exactly the drift invariant 10 and this task
// exist to prevent. IDEM-13 (or a dedicated follow-up) should replace
// internal/auth's local copy with a call into this package
// (ValidateIdempotencyKey / NewEnrolScope) once internal/auth is not
// mid-flight in another wave. auth/service.go itself notes its local copy
// exists "because internal/httpapi imports this package, and depending back
// on it would be a cycle" — that reasoning is about httpapi, not about this
// package: internal/idem has zero internal imports (see the import block in
// key.go), so internal/auth importing internal/idem introduces no cycle.
package idem
