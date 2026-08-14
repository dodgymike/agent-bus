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
// # Admission control is asymmetric by design
//
// BeginSession keys nothing on agentID: on that unauthenticated route agentID
// is an attacker-supplied VICTIM identifier, so any per-agent bucket there —
// evict or refuse — is a lockout primitive (AUTH-1-FU-PENDINGCAP). CompleteSession
// does key a cap on it (AUTH-1-FU-ACTIVECAP, MaxActiveSessionsPerAgent, default
// 32): by then the key is a PROVEN identity, established only by an Ed25519
// signature made with that agent's own enrolment private key, so a flooder can
// only ever fill its own bucket. The two checks look like the same shape; they
// are not the same primitive, and a future change must not delete one on the
// strength of the other's rationale.
//
// # What is durable, what is not, and what is actually WIRED
//
// Three different questions, and conflating them is how a backlog ends up
// claiming a thing is shipped:
//
//   - THE ROSTER CAN BE DURABLE. WALRoster records every enrolment through
//     internal/wal's two-phase path — prepare fsync, commit fsync, then apply —
//     and rebuilds itself by replay at start. Which roster is in force is
//     decided by whoever calls NewService: pass Options.Roster.
//   - AGENT ID SUFFIX FLOORS ARE NOT THIS PACKAGE'S JOB. Stopping a restart from
//     re-minting a live agent id belongs to ids.OpenNameSuffixes, which writes
//     each name's floor AHEAD of issuing it. This package's
//     EnrolmentSuffixesInWAL is NOT that mechanism and must never be sealed into
//     an allocator — it reports only what the ENROLMENT records contain, which
//     is a strict subset of the agent ids on disk. Read its doc before using it;
//     an earlier version of this paragraph claimed it made a restart safe, and
//     that claim was false.
//   - THE IDEMPOTENCY TABLE IS NOT DURABLE. Service.idem is a plain map, lost
//     on restart, so a retry that straddles a restart re-applies and mints a
//     second agent id for one agent. Invariant 10's durability half is not met
//     on this route; IDEM-11 owns closing it.
//   - IT IS WIRED (AUTH-7). cmd/agent-bus/main.go builds a WALRoster, opens the
//     durable log WITH THAT ROSTER AS THE APPLIER — which is what rebuilds it by
//     replay and what makes a live enrolment reach the serving copy — attaches
//     it, and passes it as Options.Roster. The shipped binary therefore takes
//     the durable path, and its acceptance proof is a real process killed with
//     SIGKILL and restarted (TestTwoAgentsKeepTalkingAcrossARestartWithoutRe-
//     Enrolling in cmd/agent-bus). This paragraph read "NOTHING IS WIRED YET"
//     until that landed; if it is ever true again, say so here first.
//
// Sessions are a deliberate exception and stay memory-only permanently: a
// session is a short-lived credential with a one-hour ceiling, and losing it on
// restart costs an agent one cheap challenge/response round trip. Persisting
// live credentials across restarts would buy nothing and would put replayable
// material on disk. WALRoster writes exactly one record kind and no session
// ever reaches disk.
//
// # Seams for the rest of the AUTH epic
//
//   - Authenticate is the entry point AUTH-2's middleware will call. This
//     package deliberately does not enforce a token on any route.
//   - Roster is an injected interface, which is what let the WAL-backed
//     implementation land without reshaping any caller.
//   - RosterEntry carries the FULL enrolment field set decided by DECISIONS.md
//     2026-08-07 (ENROL-SHAPE), including MessagingPublicKey, InviteID and
//     CertBindings. They were on disk from the first record so the SIGN, INVITE
//     and MTLS epics would not each force a migration — and a migration here
//     means re-enrolling every agent. ALL THREE ARE NOW POPULATED, none of them
//     is reserved, and none needed a migration: MessagingPublicKey by RELAY-13,
//     InviteID by INVITE-GATE, CertBindings by MTLS-BIND (2026-08-14), which
//     binds the client certificate presented on the enrolling connection to the
//     server-minted agent id.
//   - AgentIDForCertFingerprint (Roster) and AgentIDForClientCertificate
//     (Service) are the READ half of that binding, and the seam invariant 11's
//     cross-check is built on: they resolve a certificate fingerprint to one
//     agent id, or REFUSE. They decide no request themselves — making a route
//     enforce the answer is MTLS-CROSSCHECK's task.
//   - Revocation (AUTH-4) is a scan over the session table; see the note on
//     Service.sessions for why no per-agent index exists.
package auth
