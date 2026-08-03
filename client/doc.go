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
// # Transport
//
// newHTTPClient is the single seam where a transport is constructed. Invariant
// 11 makes TLS mandatory, with self-signed certificates, mutual authentication
// and the bus's fingerprint pinned from the invite blob; when the bus serves
// TLS, that configuration lands there and nowhere else.
//
// CERTIFICATE VERIFICATION IS NEVER DISABLED. tls.Config's skip-verification
// field is not set here, is not reachable through Config, and must not appear
// anywhere in this tree including tests — it is deliberately not spelled out
// in this comment either, so that grepping the tree for its name finds only
// real uses (DECISIONS.md, 2026-08-02, "E7").
package client
