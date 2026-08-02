// Package hub will own message fan-out and the long-poll waiter registry.
//
// It tracks enrolled agents, their per-agent queues and the waiters parked on
// an HTTP long-poll, and it releases those waiters on new traffic, on the poll
// deadline, or on server shutdown (the server-lifetime context reaches every
// handler through http.Server.BaseContext).
//
// Stub: the hub/poll epics supply the implementation.
package hub
