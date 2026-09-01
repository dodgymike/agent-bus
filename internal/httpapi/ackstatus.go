package httpapi

import (
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dodgymike/agent-bus/internal/ack"
	"github.com/dodgymike/agent-bus/internal/hub"
)

// RouteAckStatus is the sender-visible delivery status route (ACK-CONTRACT.md
// §13.1, ACK-9): GET /v1/ack/<correlation-key>.
//
// It is registered as a SUBTREE — note the trailing slash — because this build
// targets go1.19, whose http.ServeMux has no path wildcards. The correlation key
// is the remainder of the path, taken with strings.TrimPrefix and treated as
// UNTRUSTED INPUT: it is a client-supplied string that happens to be shaped like
// a server-minted id, never an identity (invariant 1).
const RouteAckStatus = "/v1/ack/"

// maxAckKeyBytes bounds the path remainder this route will even look at.
//
// An id.MessageID is "<bus-id>-<seq>" and is far shorter than this; the bound
// exists so a caller cannot make the bus hash and compare a megabyte of path on
// an authenticated GET. An over-long key is answered EXACTLY as an unknown one —
// same status, same body, same wait behaviour — so the bound costs the uniform
// answer nothing. (Unbounded input on a route has been a real finding in this
// repo; a bound that changed the answer would have been a second one.)
const maxAckKeyBytes = 512

// ackStatusPollInterval is how often a parked ?wait= request re-reads the table.
//
// This route parks by POLLING rather than by registering a waiter, and that is a
// deliberate choice against a cleverer one (invariant 8). A notification
// registry inside ack.Store would have to be woken from Accept, Settle and
// MarkInFlight — three paths that already run under the global write lock inside
// Hub.publish — to save at most one interval of latency on a transition that
// takes a network round trip and an fsync anyway. The cost is bounded and
// visible: one timer per parked request, one mutex acquisition per interval per
// parked request, and the whole thing is capped by hub.MaxPollTimeout.
const ackStatusPollInterval = 200 * time.Millisecond

// maxParkedAckStatusPerAgent bounds how many ?wait= requests ONE principal may
// have parked on this bus at once.
//
// # WHY A CAP IS NEEDED HERE AND WAS NOT OBVIOUS
//
// This route parks even when there is nothing to report — it MUST, because
// returning early would leak existence through timing (parkForAckSettlement).
// The consequence is that a probe costs the caller nothing and needs no valid
// key: every request is guaranteed to hold a connection and a goroutine for the
// full ceiling. Each parked request also wakes every ackStatusPollInterval onto
// ack.Store's single global mutex — the same mutex Accept takes inside
// Hub.publish — so the cost lands on every WRITER on the bus, not just on the
// caller. There is no global connection limit anywhere in this tree to fall back
// on.
//
// The value is 32, a RESOURCE bound: it caps how many ack-status probes one
// authenticated agent can park at once. It is spelled here rather than imported
// so this route's bound is not silently re-tuned by a change to the messaging
// poll. Do NOT conflate it with hub.MaxWaitersPerAgent: since
// POLL-CONCURRENT-WAITERS the /v1/wait MESSAGE poll is single-active (== 1) for
// a CORRECTNESS reason — two message polls on one id split delivery — which is a
// different limit for a different reason, not this one.
//
// # THE REFUSAL MUST NOT DEPEND ON THE KEY
//
// It is decided from the principal's own parked count and nothing else, so it
// tells a caller about its own concurrency and nothing about any message. The
// obvious wrong fix — returning early for an unknown key to make probing cheap
// — would rebuild the oracle out of time.
const maxParkedAckStatusPerAgent = 32

// ackWaiterCount is the per-principal parked-request counter.
//
// Keyed on the AUTHENTICATED agent id, which is what makes it safe: the key is
// a proven identity, so a flooder can only fill its own bucket. It fails closed
// and evicts nothing — evicting would let one of an agent's connections kill
// another's, which is amplification rather than self-harm (hub.Wait's reasoning,
// internal/hub/wait.go:258-266).
type ackWaiterCount struct {
	mu sync.Mutex
	n  map[string]int
}

func newAckWaiterCount() *ackWaiterCount { return &ackWaiterCount{n: map[string]int{}} }

// acquire takes a slot for agentID, or reports false when the agent is at its
// limit. The release func is safe to call exactly once.
func (c *ackWaiterCount) acquire(agentID string) (release func(), ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n[agentID] >= maxParkedAckStatusPerAgent {
		return nil, false
	}
	c.n[agentID]++
	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		// The map entry is DELETED at zero, not left at 0. A bus with many
		// agents over a long uptime would otherwise accumulate one entry per
		// principal that ever waited, which is a slow leak with a bound written
		// on the wrong thing.
		if c.n[agentID] <= 1 {
			delete(c.n, agentID)
			return
		}
		c.n[agentID]--
	}, true
}

// ackStateUnknown is the REPORTING value for "there is nothing to show you"
// (ACK-CONTRACT.md §13.2).
//
// It is not a state, it is never written durably, and internal/ack has no enum
// member for it on purpose — ack.ParseState REFUSES this spelling by name, so a
// record saying "I don't know" cannot round-trip through the log and overwrite a
// real terminal outcome with ignorance. It exists only on the wire.
//
// # WHY THIS FILE SPELLS IT ITSELF RATHER THAN IMPORTING ack.go's CONSTANT
//
// The recipient half of this plane (ACK-6, ack.go) exports AckStateUnknown with
// the same value, and reaching for it would be tidier. It is deliberately NOT
// used here: ackstatus.go would then FAIL TO COMPILE without ack.go, which in
// turn does not compile without internal/hub changes of its own — so ACK-9
// could not land, or be reverted, on its own. A consumer that cannot build
// without its neighbour is the "consumer before its definition" failure this
// repo has already had, and one duplicated four-letter string literal is a much
// cheaper price than that coupling.
//
// The two spellings cannot silently drift apart: both halves have tests that
// pin the literal word in the response body, so a change to either side goes
// red on its own side.
const ackStateUnknown = "unknown"

// AckStatusSource is the sender-filtered read side of the delivery lifecycle
// table. *ack.Store satisfies it.
//
// It has exactly ONE method and takes no recipient, which is the security
// property rather than a convenience: an interface that exposed a
// (key, recipient) lookup would let a handler iterate candidate recipients, and
// that loop IS the existence oracle §13.3 forbids. The narrowest possible
// interface is what makes the wrong handler unwritable.
type AckStatusSource interface {
	// StatusRows returns the rows for correlationKey that were sent by sender,
	// and nil for every other case — never existed, swept, somebody else's,
	// malformed. It returns no error, because any error value would tell those
	// cases apart.
	StatusRows(correlationKey, sender string) []ack.Record
}

// AckStatusRow is one row of §13.2. Every field except State is omitempty, so
// the `unknown` answer is the single smallest object this shape can produce:
// {"state":"unknown"}.
//
// # WHAT THIS SHAPE MAY NOT GROW
//
// §13.3: the response MUST NOT disclose the traversed bus_path, the peer bus
// that refused, the recipient's poll activity, or anything about the recipient's
// roster membership. The sender learns the outcome for recipients IT NAMED and
// nothing else about the federation. ack.Record carries no such field, so the
// omission is structural today — but this struct is where it would be
// reintroduced, and a "helpful" hop list here would leak the topology of the
// federation to any agent that can send one message.
//
// There is also NO sender field. The sender already knows who it is; a row it
// is not allowed to see is one it never receives.
type AckStatusRow struct {
	// CorrelationKey is the STORED record's key, echoed only when a real row
	// exists. It is never the caller's input reflected back: reflecting input
	// would make a malformed key distinguishable from an unknown one by the
	// body alone.
	CorrelationKey string `json:"correlation_key,omitempty"`

	// Recipient is the fully-qualified "<bus-id>.<agent-id>" this row is about
	// (invariant 2), and it is one the SENDER NAMED — the row exists because
	// the sender addressed it, so it discloses no roster the sender did not
	// already supply.
	Recipient string `json:"recipient,omitempty"`

	// State is "accepted", "in_flight", "delivered", "refused",
	// "undeliverable" or "unknown".
	State string `json:"state"`

	// Class is the closed 12-member enum, present iff State is a negative
	// terminal. There is no free-text reason field anywhere in this shape and
	// there must never be one (invariant 6): a reason string sourced from a
	// recipient is a message body by another name.
	Class string `json:"class,omitempty"`

	// AttestedBy labels WHAT authenticated the outcome — "peer_bus" or
	// "recipient_signature_unverified". There is no value meaning "verified",
	// because nothing in this system can produce one (§6.3). The name is long
	// on purpose; a shorter one would read as a verification claim.
	AttestedBy string `json:"attested_by,omitempty"`

	AcceptedAt string `json:"accepted_at,omitempty"`
	SettledAt  string `json:"settled_at,omitempty"`
}

// AckStatusResponseBody is the 200 body of GET /v1/ack/<correlation-key>.
//
// Rows is NEVER empty and NEVER null: when nothing is visible it holds exactly
// one row, {"state":"unknown"}. That is what makes the four §13.3 cases produce
// a BYTE-IDENTICAL response rather than merely a similar one — an empty array
// for "not yours" and a populated one for "yours" would be the oracle written
// in a different alphabet, and a client that had to distinguish null from []
// would be one refactor away from restoring it.
type AckStatusResponseBody struct {
	Rows []AckStatusRow `json:"rows"`
}

// ackUnknownBody is the uniform answer, built fresh per call so no caller can
// mutate a shared slice.
func ackUnknownBody() AckStatusResponseBody {
	return AckStatusResponseBody{Rows: []AckStatusRow{{State: ackStateUnknown}}}
}

// handleAckStatus serves GET /v1/ack/<correlation-key> (ACK-9, §13).
//
// # THE UNIFORM ANSWER IS ENFORCED HERE AND NOWHERE ELSE
//
// ACK-2 flagged that nothing in the record shape owns §13.3. This handler owns
// it, and it does so by having exactly ONE failure vocabulary after
// authentication: 200 with the unknown row. There is no 400 for a malformed key,
// no 403 for somebody else's key, no 404 for a key that never existed and none
// for one that was swept. A 403 would confirm the message exists, which is the
// oracle ACK-4 is required to close.
//
// Authentication still runs FIRST, and it is the one thing this route does
// answer with a different status. An anonymous caller gets 401 from
// authMiddleware before reaching this function — the same posture handleBroadcast
// takes when it authenticates before answering 501, because "a route that told an
// ANONYMOUS caller what it does and does not implement would be describing the
// messaging surface to somebody with no business knowing it exists"
// (messages.go). 401-vs-200 discloses that this build serves an ack surface at
// all, which is public in /v1/discovery and in the protocol docs; it discloses
// nothing about any message.
//
// # WHAT THIS ROUTE DOES NOT SAY, RESTATED
//
// A hop ACK does NOT advance the sender-visible state. The transport layer's
// "another bus took responsibility" and the application layer's "an agent
// actually got it" are two different facts (§1), and this route reports only the
// second. That separation is ack.Store's — there is no hop-ACK method to call —
// and this handler must not reintroduce it by inferring anything from routing.
func (s *Server) handleAckStatus(w http.ResponseWriter, r *http.Request) {
	if !s.requireGET(w, r) {
		return
	}
	sender, ok := s.ackStatusPrincipal(w, r)
	if !ok {
		return
	}
	wait, ok := s.readAckWaitParam(w, r)
	if !ok {
		return
	}

	// Taken from the path, percent-decoded by net/http, and NOT validated here.
	// Validation lives in ack.Store.StatusRows, where a malformed key and a miss
	// produce the same nil — a validation refusal at this layer would need its
	// own answer, and any answer it could give would be a fourth observable case.
	rawKey := strings.TrimPrefix(r.URL.Path, RouteAckStatus)

	lookup := func() []ack.Record {
		if len(rawKey) > maxAckKeyBytes {
			// Same nil as every other miss, so the bound is invisible to a
			// caller. It exists to stop the work, not to change the answer.
			return nil
		}
		return s.ackStatus.StatusRows(rawKey, sender)
	}

	rows := lookup()
	if wait > 0 && !ackRowsSettled(rows) {
		// The slot is taken BEFORE parking and DEFERRED, so it is released on
		// every exit — the deadline, an early settlement, a client that
		// vanishes mid-wait (parkForAckSettlement returns on
		// r.Context().Done()), AND a panic in between.
		//
		// The defer is the load-bearing part, not tidiness. LoggingMiddleware
		// RECOVERS panics without re-panicking (middleware.go), so a bare
		// release() after the park would be skipped by a panic anywhere in
		// lookup -> StatusRows -> sweepLocked, leaking the slot and its map
		// entry for the life of the process — and 32 such leaks would lock this
		// principal out of ?wait= permanently. internal/hub/wait.go:275-277
		// defers for exactly this reason and says so.
		//
		// Deferring to the END of the handler rather than to the end of the
		// park holds the slot across writeJSON as well. That is microseconds of
		// a bound measured in minutes, and it buys the panic safety outright.
		release, admitted := s.ackWaiters.acquire(sender)
		if !admitted {
			s.log.Info("ack status wait refused: the principal is at its parked-request limit",
				"request_id", RequestIDFromContext(r.Context()),
				"agent_id", sender,
				"limit", maxParkedAckStatusPerAgent,
			)
			w.Header().Set("Retry-After", "1")
			s.writeJSON(w, r, http.StatusTooManyRequests, ErrorResponse{
				Error: "too many delivery-status waits parked for this agent; poll without ?wait, or wait for one of them to finish",
			})
			return
		}
		defer release()
		rows, ok = s.parkForAckSettlement(w, r, lookup, wait)
		if !ok {
			return
		}
	}

	if len(rows) == 0 {
		// The whole of §13.3, in one branch: never existed, swept, somebody
		// else's, malformed and over-long all arrive here.
		s.writeJSON(w, r, http.StatusOK, ackUnknownBody())
		return
	}
	s.writeJSON(w, r, http.StatusOK, renderAckRows(rows))
}

// ackStatusPrincipal resolves the authenticated caller for this route.
//
// It reads the principal from the CONTEXT, where authMiddleware put it after
// internal/auth resolved a live session — never from a header, a query parameter
// or the path. This is the ONE identity §13.3's filter is applied against, and a
// client-supplied one would make the whole filter decorative (invariant 1).
//
// It deliberately does NOT require a hub: this route reads a durable table, not
// the messaging surface, and coupling it to hub availability would make the
// status of already-accepted messages unreadable exactly when the bus is least
// healthy.
func (s *Server) ackStatusPrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	if s.ackStatus == nil {
		// Unreachable: the route is registered only when the source is
		// non-nil. Checked anyway, because "the route exists so the dependency
		// must" is the assumption that turns a wiring change into a nil
		// dereference on a live server.
		s.log.Error("the ack status route was reached on a server with no ack lifecycle table",
			"request_id", RequestIDFromContext(r.Context()),
		)
		s.writeJSON(w, r, http.StatusServiceUnavailable, ErrorResponse{Error: "delivery status is not available on this server"})
		return "", false
	}
	agentID := AgentIDFromContext(r.Context())
	if agentID == "" {
		// Also unreachable: authMiddleware is default-deny and this route is
		// off the allow-list. It fails CLOSED rather than filtering on an empty
		// sender, which would match no row today and would match EVERY row the
		// day a blank sender ever reached the table.
		s.log.Error("the ack status route was reached with no authenticated principal",
			"request_id", RequestIDFromContext(r.Context()),
		)
		s.writeUnauthorized(w, r, wwwAuthenticateInvalidToken, "authentication required")
		return "", false
	}
	return agentID, true
}

// readAckWaitParam parses ?wait= (whole seconds), the long-poll form of §13.1.
//
// Absent or empty means "answer now". An out-of-range value is REFUSED rather
// than silently clamped, exactly as readTimeoutParam refuses one on /v1/wait: a
// caller that asked for an hour and was quietly given five minutes would
// conclude the server had dropped its request.
//
// The ceiling is hub.MaxPollTimeout — the same ceiling as any parked poll, so
// this route cannot become a way to hold a connection longer than the bus's own
// limit allows.
func (s *Server) readAckWaitParam(w http.ResponseWriter, r *http.Request) (time.Duration, bool) {
	raw := r.URL.Query().Get("wait")
	if raw == "" {
		return 0, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		s.writeJSON(w, r, http.StatusBadRequest, ErrorResponse{Error: "wait must be a positive whole number of seconds"})
		return 0, false
	}
	d := time.Duration(n) * time.Second
	if d > hub.MaxPollTimeout {
		s.writeJSON(w, r, http.StatusBadRequest, ErrorResponse{
			Error: "wait must be at most " + strconv.Itoa(int(hub.MaxPollTimeout/time.Second)) + " seconds",
		})
		return 0, false
	}
	return d, true
}

// parkForAckSettlement holds the request until every visible row is terminal, or
// until the deadline, or until the client hangs up.
//
// # IT PARKS ON `unknown` TOO, AND THAT IS A SECURITY REQUIREMENT
//
// The obvious optimisation — return at once when there is nothing to show —
// would rebuild the oracle out of TIME rather than content: an immediate answer
// would mean "no such row", a parked one would mean "a row exists and has not
// settled", and a prober would read existence straight off the latency. So a
// request that finds nothing waits exactly as long as one watching a live
// non-terminal row. The cost of that decision is a parked goroutine per probe,
// bounded by hub.MaxPollTimeout and by whatever connection limit the listener
// imposes.
//
// A deadline reached with nothing settled is a SUCCESS, not an error: the caller
// gets the current rows (or the unknown row) with a 200, exactly as a timed-out
// /v1/wait returns an empty batch.
func (s *Server) parkForAckSettlement(w http.ResponseWriter, r *http.Request, lookup func() []ack.Record, wait time.Duration) ([]ack.Record, bool) {
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	ticker := time.NewTicker(ackStatusPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			// The client hung up, or the server is shutting down. Writing a
			// response would be writing to nobody, and net/http would log the
			// failed write as if it mattered. Checked on the REQUEST rather
			// than by classifying an error, so it cannot be confused with a
			// refusal the caller needs to hear about.
			s.log.Debug("ack status poll released without a response: the request context is done",
				"request_id", RequestIDFromContext(r.Context()),
			)
			return nil, false
		case <-deadline.C:
			return lookup(), true
		case <-ticker.C:
			if rows := lookup(); ackRowsSettled(rows) {
				return rows, true
			}
		}
	}
}

// ackRowsSettled reports that there is something to report AND it can no longer
// change: at least one row, every one of them terminal.
//
// An EMPTY set is deliberately NOT settled. Terminal is absorbing, but "nothing
// retained" is not an outcome — it is the absence of one, and it can still turn
// into a row the moment a send lands. Treating it as settled is exactly the
// early return the parking function refuses to make.
func ackRowsSettled(rows []ack.Record) bool {
	if len(rows) == 0 {
		return false
	}
	for _, r := range rows {
		if !r.State.Terminal() {
			return false
		}
	}
	return true
}

// renderAckRows converts durable records to the §13.2 wire shape.
//
// Class and AttestedBy are rendered from the record and never inferred: a
// positive terminal carries no class at all (§5.4 — a positive outcome has
// nothing to explain, and an optional class on it would create a channel where
// none is needed), and validate has already enforced that pairing on the way in.
func renderAckRows(rows []ack.Record) AckStatusResponseBody {
	out := make([]AckStatusRow, 0, len(rows))
	for _, r := range rows {
		row := AckStatusRow{
			CorrelationKey: r.CorrelationKey,
			Recipient:      r.Recipient,
			State:          r.State.String(),
			Class:          string(r.Class),
			AttestedBy:     string(r.AttestedBy),
		}
		// BOTH timestamps are zero-checked, not just the optional one.
		// accepted_at is present on every record validate admits, so the check
		// is unreachable today — but formatInstant on a zero time renders
		// "0001-01-01T00:00:00Z", and a client that parsed that would read a
		// missing field as a real instant from the year one. Omitting it says
		// "absent", which is what it is.
		if !r.AcceptedAt.IsZero() {
			row.AcceptedAt = formatInstant(r.AcceptedAt)
		}
		if !r.SettledAt.IsZero() {
			row.SettledAt = formatInstant(r.SettledAt)
		}
		out = append(out, row)
	}
	return AckStatusResponseBody{Rows: out}
}
