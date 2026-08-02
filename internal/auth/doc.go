// Package auth will own enrolment credentials and request authentication.
//
// An agent presents a key at enrolment; the server signs it and returns a
// token the agent authenticates with on every subsequent call (invariant 3).
// Every route except enrolment (and the unauthenticated liveness/discovery
// endpoints) authenticates through this package.
//
// Stub: the auth epic supplies the implementation.
package auth
