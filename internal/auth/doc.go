// Package auth owns enrolment credentials and session authentication.
//
// # The shape of the protocol
//
// An agent generates an Ed25519 keypair for itself and presents only the PUBLIC
// half at enrolment. The server mints the fully-qualified agent id (invariant 1)
// and records the public key against it. It never sees, stores, derives or
// transports the private half, so there is no material anywhere on this server
// that could FORGE an agent's calls — only material that lets it VERIFY them.
// That asymmetry is the whole point, and it is why a symmetric shared secret is
// not acceptable here.
//
// Authentication is then challenge/response, and the direction of the signature
// matters: the SERVER provides an opaque token, the CLIENT signs that token with
// its enrolment private key, and the server verifies the signature against the
// recorded public key. The server signs NOTHING. An earlier draft of this
// package said the server signs the agent's key and hands back a signed
// credential; that design is SUPERSEDED, and nothing here mints, holds or needs
// a bus signing secret.
//
//	POST /v1/enroll           name + public key      -> server-minted agent id
//	POST /v1/session/begin    agent id               -> opaque token (pending)
//	POST /v1/session/complete token + signature      -> the token is now live
//
// The token is an OPAQUE SERVER-SIDE HANDLE — random bytes from crypto/rand,
// meaningful only as a key into this server's session table. It is not a signed
// claim, it carries no fields, and a client must not try to parse one. The
// session table is keyed by the token's SHA-256, so the server's own memory
// never holds a directly replayable copy of a live credential.
//
// Completion is naturally idempotent: re-completing an already-active session
// re-verifies the signature and returns the SAME expiry, never a fresh one, so
// one signature can never be leveraged into an unbounded session.
//
// # What is NOT durable yet
//
// State in this package is IN MEMORY ONLY. Enrolment is therefore NOT
// crash-safe: the roster, the idempotency table and the session table all live
// in the process and are lost on restart, and nothing here writes to the WAL or
// participates in the two-phase write path. Read no durability claim into any of
// it — AUTH-3 makes enrolment durable and restores the roster and the per-name
// suffix floors on start.
//
// Sessions are a deliberate exception that stays memory-only even after AUTH-3:
// a session is a short-lived credential with a one-hour ceiling, and losing it
// on restart costs an agent one cheap challenge/response round trip. Persisting
// live credentials across restarts would buy nothing and would put replayable
// material on disk.
//
// # Seams for the rest of the AUTH epic
//
//   - Authenticate is the entry point AUTH-2's middleware will call. This
//     package deliberately does not enforce a token on any route.
//   - Roster is an injected interface so AUTH-3 can supply a WAL-backed
//     implementation without reshaping any caller.
//   - Revocation (AUTH-4) is a scan over the session table; see the note on
//     Service.sessions for why no per-agent index exists.
package auth
