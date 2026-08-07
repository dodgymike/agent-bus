// Package client is the agent-bus client library: the one place HTTP is
// constructed for talking to a bus.
//
// # Why this package is not under internal/
//
// Invariant 7 requires the CLI to have three audiences, and the third is "an
// agent EMBEDDING it". Go forbids any other module from importing a path
// containing internal/, so a client under internal/ could never be embedded —
// the requirement would be silently foreclosed by a directory name. This
// package therefore lives at the module root and its exported surface is a
// PUBLIC API subject to compatibility care. See DECISIONS.md, 2026-08-02.
//
// The corollary binds in the other direction too: this package must NOT import
// anything under internal/. Constants it shares with the server —
// SessionSigningContext, AgentNamePattern, the route paths — are PINNED here
// as literals, with a comment naming the server-side definition they mirror.
// Divergence fails closed (a signature simply does not verify), which is the
// right direction for a duplicate to fail in.
//
// # The shape of a session
//
//	enrol   → generate an Ed25519 key pair, send ONLY the public half, receive
//	          the SERVER-MINTED fully-qualified id `<bus-id>.<agent-id>`
//	session → ask the bus for a token, sign THE TOKEN THE BUS CHOSE, present it
//	          back; the resulting bearer token lasts at most an hour and is
//	          refreshed at 75% of its lifetime
//
// The client never chooses its own id (invariant 1) and never chooses the
// bytes it signs (invariant 3).
//
// # What is stored, and what is not
//
// The Ed25519 private key is stored, as a seed, in a 0600 file inside a 0700
// directory under the user's config directory — never in the repository. The
// SESSION TOKEN is never stored: it is a bearer credential with an hour's life
// that does not survive a bus restart anyway, so persisting it would trade a
// stealable token at rest for two saved round trips.
//
// # Messaging and polling
//
// Send and Broadcast mint an idempotency key ONCE per logical send and carry
// that one key through every transport retry, so a retry after a lost
// acknowledgement is answered from the bus's applied-key table rather than
// producing a second message (invariant 10). Read fetches one batch, either as
// history or as a long poll; Watch is the loop built on it.
//
// The rule that governs the read side is that THE CURSOR ADVANCES ONLY AFTER THE
// CALLER HAS BEEN HANDED THE MESSAGES. Delivery is at-least-once, so the safe
// direction on any failure is to re-deliver rather than skip — which means an
// agent's handler must be IDEMPOTENT, keyed on Message.MessageID. Watch's doc
// comment spells out exactly which crash re-delivers what.
//
// # Errors
//
// Every failure is an *Error carrying a Kind, a message and, wherever one
// exists, a REMEDY. ExitCode maps a Kind to the process exit code documented
// in CONTRACTS-CLI.md, so an embedding agent that re-exposes this client as a
// subprocess produces exactly the codes a caller expects without copying a
// switch statement that will drift.
//
// # Transport, and the two halves of mutual TLS
//
// newHTTPClient is the single seam where a transport is constructed, and
// pinnedTLSConfig (pin.go) is the single place DEFAULT VERIFICATION IS
// REPLACED. Invariant 11 makes TLS mandatory, with self-signed certificates,
// mutual authentication and the bus's fingerprint pinned from the invite blob.
//
// Both halves now exist, in two files, and they are worth keeping apart in your
// head because they fail differently:
//
//   - VERIFYING the bus — pin.go. The fingerprint from the invite decides
//     whether the certificate on the other end is the one it was supposed to
//     be (MTLS-PIN, 2026-08-07).
//   - PRESENTING our own — clientcert.go. An Ed25519 self-signed certificate,
//     minted on first use and stored 0600 beside the credential store
//     (MTLS-CLIENTCERT, 2026-08-07). It is offered through
//     GetClientCertificate in the SAME pinnedTLSConfig literal — never in
//     newHTTPClient's unpinned fallback, which the pinned branch replaces
//     wholesale, so a certificate put there is silently dropped on every real
//     connection.
//
// The bus does NOT ask for a client certificate yet (MTLS-CLIENTAUTH is the
// task that starts asking), so the presenting half is offered and ignored
// today. That order is deliberate and must not be reversed: a bus that requires
// a client certificate before any client can present one locks out every
// enrolled agent.
//
// The rule to know: an https bus whose certificate fingerprint this client has
// not been told IN ADVANCE is REFUSED. There is no trust-on-first-use, not as a
// flag, not as a fallback, not for tests. The fingerprint comes from the invite
// (today: Config.BusFingerprint, AGENT_BUS_FINGERPRINT, or the selected
// identity, which recorded it at enrolment), and a bus that presents a
// certificate outside the accept-set is a hard, unretried failure.
//
// # Rotation: an identity accepts a SET of certificates, bounded at MaxBusPins
//
// A bus rotating its key serves TWO certificates during rollover (DECISIONS.md
// E3), so an identity holds a SET of accepted fingerprints rather than one
// (MTLS-ROTATE, 2026-08-07). Without that, the first routine rotation would
// wedge every enrolled agent at once — and a wedged fleet is the pressure under
// which somebody proposes letting --bus-fingerprint override the stored pin,
// which would turn a DETECTED substitution into an ACCEPTED one. Rotation is
// made to work; the check is not softened.
//
// A pin enters the set ONLY by an explicit operator act: the invite's
// fingerprint at enrolment, or Client.AddBusPin (`agent-busctl pin add`) with a
// value confirmed OUT OF BAND. It is NEVER learned from a handshake — that is
// TOFU in a different hat — and Client.RemoveBusPin ends the rollover, because
// a set that only ever grows becomes "accept every certificate this bus has
// ever had". TestPinIsNeverLearnedFromAHandshake guards it. Its BEHAVIOURAL
// half is the load-bearing one: the persisted set is unchanged across refused
// and successful handshakes alike. Its structural half raises the cost — pin.go
// holds raw certificate bytes and cannot reach the credential store, it is the
// only non-test file deriving a fingerprint from DER, and the pin-writing calls
// are confined to named files — but it is a cost, not a proof; see BusPinSet.
//
// An explicit --bus-fingerprint NARROWS the stored set to that one certificate
// and never widens it; naming a certificate the identity does not accept is a
// refusal, not a precedence question (Client.resolvePins).
//
// CERTIFICATE VERIFICATION IS NEVER DISABLED — it is REPLACED. pinnedTLSConfig
// turns off the stdlib's default chain check, which cannot succeed against a
// self-signed certificate with no CA anywhere in the design, and substitutes an
// exact-certificate comparison that is strictly stronger than the CA and
// hostname checks it stands in for. Read pinnedTLSConfig's doc comment before
// touching it: the difference between "we replaced verification" and "we
// removed verification" is one callback, and the second still completes
// handshakes and still passes every positive test. Three guards in
// guard_test.go hold the two halves together, and there is no Config field,
// flag or environment variable that relaxes any of it (DECISIONS.md,
// 2026-08-02 "E7", amended 2026-08-07 by MTLS-PIN).
package client
