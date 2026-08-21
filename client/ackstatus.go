package client

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// routeAckStatus is the sender-visible delivery status route
// (ACK-CONTRACT.md §13.1). The correlation key is appended to it.
//
// # WHAT THE PATH ESCAPING DOES AND DOES NOT DO
//
// The transport escapes request.path AS A PATH, which percent-encodes a space,
// a '?' and a '#' — so a key cannot smuggle a query string or a fragment onto
// the URL. It does NOT escape '/' or '.', because those are legal path
// characters: `ack-status ../healthz` really does put "/v1/ack/../healthz" in
// the request line. An earlier version of this comment claimed otherwise, and a
// safety claim that is false is worse than no claim.
//
// That is harmless, and this client deliberately does not police it. The
// request goes to the caller's OWN bus under the caller's OWN credentials;
// http.ServeMux normalises dot-segments server-side, so the worst outcome is
// that some other route answers; and validateAckStatus then REFUSES anything
// that is not a well-formed delivery-status body. A caller reaching another
// route on its own bus has bypassed nothing — it could have called that route
// directly.
const routeAckStatus = "/v1/ack/"

// MaxCorrelationKeyLen mirrors the bus's own bound on the path remainder of
// GET /v1/ack/<correlation-key>.
//
// A correlation key is a server-minted message id, "<bus-id>-<seq>", which is
// far shorter than this. The bound is here so an obvious mistake — a whole file
// pasted where a key belongs — is a local usage error instead of a request. It
// does NOT validate the key's SHAPE: an ill-formed key is answered `unknown` by
// the bus, exactly like one that never existed, and a client that pre-judged the
// shape would tell its caller something the bus deliberately does not.
const MaxCorrelationKeyLen = 512

// Ack states, as they appear on the wire (ACK-CONTRACT.md §8.1, §13.2). The set
// is CLOSED: the bus refuses an unrecognised spelling rather than defaulting
// one, and so does this client.
const (
	AckAccepted      = "accepted"
	AckInFlight      = "in_flight"
	AckDelivered     = "delivered"
	AckRefused       = "refused"
	AckUndeliverable = "undeliverable"

	// AckUnknown is a REPORTING value, never a durable state. It is the single
	// answer the bus gives for four different situations — the key never
	// existed, the key was swept, the key belongs to another sender, or the key
	// is malformed — because telling them apart would confirm that a message
	// exists (§13.3). Do not write code that tries to narrow it.
	AckUnknown = "unknown"
)

// ackStates is the closed set, used to refuse a spelling this client does not
// know rather than pass it through to a caller that would branch on it.
var ackStates = map[string]struct{}{
	AckAccepted: {}, AckInFlight: {}, AckDelivered: {},
	AckRefused: {}, AckUndeliverable: {}, AckUnknown: {},
}

// AckRow is one row of delivery status, per recipient (§13.2).
//
// It carries NO bus path, NO peer identity, NO poll activity and NO free text.
// The bus does not send those (§13.3), and this struct must never grow a field
// for them: a client that had somewhere to put a hop list would be a reason for
// somebody to start sending one.
type AckRow struct {
	CorrelationKey string `json:"correlation_key,omitempty"`
	Recipient      string `json:"recipient,omitempty"`

	// State is one of the six spellings above. Always present.
	State string `json:"state"`

	// Class is the closed 12-member NACK enum, present only on a negative
	// terminal. There is no free-text reason and there must never be one
	// (invariant 6).
	Class string `json:"class,omitempty"`

	// AttestedBy labels WHAT authenticated a terminal outcome: "peer_bus" or
	// "recipient_signature_unverified". THERE IS NO VALUE MEANING "VERIFIED"
	// and none can be produced by this system — the bus carries a message
	// signature as opaque bytes and never verifies it, and no endpoint
	// distributes agents' messaging public keys, so a sender cannot verify a
	// recipient's attestation either (§6.3). Render this label; never present
	// it as proof.
	AttestedBy string `json:"attested_by,omitempty"`

	AcceptedAt string `json:"accepted_at,omitempty"`
	SettledAt  string `json:"settled_at,omitempty"`
}

// Terminal reports that this row can no longer change: delivered, refused or
// undeliverable. Terminal is ABSORBING — a terminal row is never revisited,
// never reopened and never downgraded.
func (r AckRow) Terminal() bool {
	switch r.State {
	case AckDelivered, AckRefused, AckUndeliverable:
		return true
	}
	return false
}

// Negative reports a negative terminal: the recipient refused it, or this bus
// will never deliver it.
func (r AckRow) Negative() bool {
	return r.State == AckRefused || r.State == AckUndeliverable
}

// AckStatus is the answer to one status request: one row per recipient.
//
// Rows is never empty. When the bus has nothing to show, it holds exactly one
// row whose State is AckUnknown — which is what makes "not yours" and "no such
// key" the same answer instead of merely similar ones.
type AckStatus struct {
	Rows []AckRow `json:"rows"`
}

// Unknown reports the uniform answer: nothing is visible to this sender.
//
// IT IS FOUR SITUATIONS AT ONCE and cannot be narrowed — the key never existed,
// it was swept past its retention window, it belongs to another sender, or it is
// malformed. §13.3 makes them one answer on purpose.
func (s AckStatus) Unknown() bool {
	return len(s.Rows) == 1 && s.Rows[0].State == AckUnknown
}

// Settled reports that there is something to report AND it can no longer change:
// at least one row, every one of them terminal. An unknown answer is NOT
// settled, because "nothing retained" is the absence of an outcome rather than
// one.
func (s AckStatus) Settled() bool {
	if len(s.Rows) == 0 || s.Unknown() {
		return false
	}
	for _, r := range s.Rows {
		if !r.Terminal() {
			return false
		}
	}
	return true
}

// AnyNegative reports that at least one recipient refused it or that this bus
// will never deliver it.
func (s AckStatus) AnyNegative() bool {
	for _, r := range s.Rows {
		if r.Negative() {
			return true
		}
	}
	return false
}

// AckStatusOptions selects the message and the waiting behaviour.
type AckStatusOptions struct {
	// CorrelationKey is the message id THIS BUS MINTED for the send, as
	// SendResult.MessageID reports it. It is the origin bus's server-minted id
	// (§3) and it is bus-namespaced, so it is globally unambiguous with no
	// registry (invariants 1 and 2).
	CorrelationKey string

	// Wait parks on the bus until every row is terminal, or until the duration
	// elapses. Zero answers immediately with a snapshot.
	//
	// A parked request that times out is a SUCCESS carrying the current state,
	// never an error: on a message that has not settled yet, that is the normal
	// outcome.
	Wait time.Duration
}

// AckStatus fetches sender-visible delivery status for one message
// (ACK-CONTRACT.md §13, ACK-9).
//
// # ONLY THE SENDER SEES A ROW, AND THE REFUSAL IS INVISIBLE
//
// The bus filters on the AUTHENTICATED principal, and answers 200 with a single
// `unknown` row for a key belonging to anybody else — the same answer as for a
// key that never existed. So this method never returns a permission error, and
// there is nothing here to translate into one. If you are tempted to infer "it
// exists but is not mine" from a timing difference, note what is actually
// claimed: the CONTENT is indistinguishable; the timing is not claimed to be.
//
// # A HOP ACK IS NOT A DELIVERY
//
// `delivered` means the recipient APPLICATION acknowledged the message. A peer
// bus taking responsibility for a hop does NOT advance this state and is not
// reported here at all: the transport layer's "another bus has it" and the
// application layer's "an agent got it" are different facts, and collapsing them
// is the single most dangerous mistake available on this surface.
func (c *Client) AckStatus(ctx context.Context, opts AckStatusOptions) (AckStatus, error) {
	const op = "ack-status"

	key := opts.CorrelationKey
	switch {
	case key == "":
		return AckStatus{}, usagef(op, "pass the message id the bus returned when you sent the message",
			"a correlation key is required")
	case len(key) > MaxCorrelationKeyLen:
		return AckStatus{}, usagef(op, "pass the message id the bus returned when you sent the message, e.g. bus-abc123-42",
			"correlation key is %d bytes, the limit is %d", len(key), MaxCorrelationKeyLen)
	case strings.ContainsAny(key, " \t\r\n"):
		// Refused locally because whitespace in a key is always a shell or
		// copy-paste accident, and sending it would spend a round trip to be
		// told `unknown` — which reads as "the message is gone" rather than
		// "you typed it wrong". This judges only the CALLER'S OWN input and
		// discloses nothing about the bus.
		return AckStatus{}, usagef(op, "remove the whitespace from the correlation key",
			"a correlation key contains no whitespace")
	}

	q := url.Values{}
	var cancel context.CancelFunc
	if opts.Wait > 0 {
		secs, err := pollTimeoutSeconds(op, opts.Wait)
		if err != nil {
			return AckStatus{}, err
		}
		q.Set("wait", strconv.Itoa(secs))

		if ctx == nil {
			ctx = context.Background()
		}
		// The same deadline arithmetic Read uses for its parked form, and for
		// the same reason: Config.Timeout defaults to 30s while the bus's
		// ceiling is 5 minutes, so a caller asking for a long wait would have
		// its own request killed first, every time, looking exactly like a
		// broken connection.
		slack := c.cfg.Timeout
		if slack < minPollSlack {
			slack = minPollSlack
		}
		ctx, cancel = context.WithTimeout(ctx, time.Duration(secs)*time.Second+slack)
	} else {
		ctx, cancel = c.contextWithTimeout(ctx)
	}
	defer cancel()

	var out AckStatus
	if _, err := c.authorizedRequest(ctx, request{
		method: http.MethodGet,
		path:   routeAckStatus + key,
		query:  q,
		op:     op,
		out:    &out,
		// Safe to repeat by construction: it reads a durable row, moves
		// nothing, and advances no state on our behalf.
		retryable: true,
	}); err != nil {
		return AckStatus{}, err
	}
	if err := validateAckStatus(op, &out); err != nil {
		return AckStatus{}, err
	}
	return out, nil
}

// validateAckStatus checks everything that will be printed or branched on. See
// sanitize.go for why the bus is not trusted to produce safe text.
//
// An UNRECOGNISED STATE IS AN ERROR, never a default. The set is closed by
// contract and the bus's own decoder refuses an unrecognised spelling rather
// than guessing; a client that passed one through would let a typo or a
// corrupted row render as a delivery outcome, and a caller branching on
// `== "delivered"` would silently take the wrong branch forever.
func validateAckStatus(op string, s *AckStatus) error {
	if len(s.Rows) == 0 {
		return newError(KindServer, op, "the bus returned no delivery status rows", "retry, and report this to the bus operator: the route must return at least one row, and exactly one with state \"unknown\" when there is nothing to report")
	}
	for i := range s.Rows {
		r := &s.Rows[i]
		if _, ok := ackStates[r.State]; !ok {
			return newError(KindServer, op, "the bus reported a delivery state this client does not know", "upgrade agent-busctl; the delivery state set is closed and an unrecognised spelling is refused rather than guessed at")
		}
		// The three id-shaped fields go through validateServerField, which
		// enforces the character set an id may use. The two timestamps go
		// through validateServerTimestamp instead: an RFC3339 instant contains
		// ':' and '+', which an id may not, so checking them with the id
		// validator would reject every legal row.
		//
		// EMPTY IS SKIPPED. Every field here is omitempty on the wire — a
		// positive terminal carries no class (§5.4), a non-terminal row carries
		// no settled_at, and the `unknown` row carries nothing but a state.
		for _, f := range []struct{ name, value string }{
			{"correlation key", r.CorrelationKey},
			{"recipient", r.Recipient},
			{"class", r.Class},
			{"attested by", r.AttestedBy},
		} {
			if f.value == "" {
				continue
			}
			if err := validateServerField(op, f.name, f.value); err != nil {
				return err
			}
		}
		for _, f := range []struct{ name, value string }{
			{"accepted at", r.AcceptedAt},
			{"settled at", r.SettledAt},
		} {
			if err := validateServerTimestamp(op, f.name, f.value); err != nil {
				return err
			}
		}
	}
	return nil
}
