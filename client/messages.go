package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Route paths for the messaging and polling surface. Pinned here as literals
// for the same reason routeEnroll is (see enrol.go and doc.go): this package
// must not import internal/, so it mirrors internal/httpapi's RouteAgents,
// RouteSend, RouteBroadcast, RouteMessages and RouteWait rather than importing
// them. A divergence fails loudly and immediately — a 404 on the first call —
// which is the right direction for a duplicated constant to break in.
const (
	routeAgents    = "/v1/agents"
	routeSend      = "/v1/send"
	routeBroadcast = "/v1/broadcast"
	routeMessages  = "/v1/messages"
	routeWait      = "/v1/wait"
)

// Protocol limits, PINNED here as literals mirroring the server's definitions.
//
// They are duplicated, not imported, for the invariant-7 reason above. They are
// duplicated at all — rather than simply letting the bus refuse — because every
// one of them turns a round trip and a terse 400 into a local, remedial error
// that names the actual number the caller exceeded. The server remains
// authoritative: these checks only ever refuse EARLIER than it does, never
// admit something it would reject.
const (
	// MaxBodyBytes is the largest message body the bus accepts, DECODED. It
	// mirrors store.MaxBodyBytes. The wire carries standard base64, so the
	// encoded form is about a third larger again — this bound is on the bytes
	// the caller hands us, which is the number a caller can act on.
	MaxBodyBytes = 64 << 10

	// MaxBatchLimit is the ceiling on a requested batch size, mirroring
	// hub.MaxBatchLimit. Above it the bus answers 400.
	MaxBatchLimit = 256

	// DefaultBatchLimit is the batch size the bus applies when no limit is
	// requested, mirroring hub.DefaultBatchLimit.
	//
	// This client deliberately sends NO limit when the caller asks for none,
	// rather than sending this value: the default belongs to the bus, and a
	// client that pins it locally would freeze an old default the day the bus
	// changed its own. It is exported so a caller can size a buffer or explain
	// its throughput without hard-coding 64 itself.
	DefaultBatchLimit = 64

	// MaxPollTimeout is the hard ceiling on one long poll, mirroring
	// hub.MaxPollTimeout. A request above it is REFUSED with a 400, not clamped
	// — and this client refuses it locally for the same reason (see Read).
	MaxPollTimeout = 5 * time.Minute

	// DefaultPollTimeout is how long a poll parks when the caller names no
	// timeout, mirroring hub.DefaultPollTimeout.
	DefaultPollTimeout = 30 * time.Second
)

// maxCursorLen mirrors hub.MaxCursorLen: the bus refuses to decode a cursor
// longer than this, so sending one is a guaranteed 400, and STORING one is a
// hostile bus growing a local file (see cursorstore.go).
const maxCursorLen = 512

// maxBatchResponseBytes is the response-body ceiling for the two read routes.
//
// The generic 1 MiB bound (transport.go's maxResponseBytes) is not enough here
// and the failure would be baffling: store.MaxBatchBytes lets ONE batch carry a
// full 1 MiB of message BODY, which the wire then base64-encodes to ~1.4 MiB
// before the per-message metadata is added. Truncating that at 1 MiB does not
// produce a short read, it produces invalid JSON — reported as "the bus
// returned a 200 whose body is not the expected JSON", which points a reader at
// the wrong problem entirely. 4 MiB is ~3x the largest legal batch and is still
// finite, which is the property that matters.
const maxBatchResponseBytes = 4 << 20

// minPollSlack is the smallest margin added to a long poll's own timeout when
// deriving the request deadline. See Read.
const minPollSlack = 10 * time.Second

// Message is one message read back off the bus.
//
// The json tags are the WIRE shape (CONTRACTS-HTTP.md, "A <message> on the read
// path is") and are also this type's --json contract, so they do not change.
type Message struct {
	// MessageID is the server-minted "<bus-id>-<seq>" (invariant 1). It is the
	// key an idempotent handler should deduplicate on: delivery is
	// at-least-once, so a message may legitimately arrive twice.
	MessageID string `json:"message_id"`

	// Seq is the server-minted monotonic sequence. Together with the recipient
	// cursor it is the freshness half of the replay defence (invariant 10) — a
	// signature alone cannot stop a verbatim resend.
	Seq uint64 `json:"seq"`

	// From is the fully-qualified sender id (invariant 2).
	From string `json:"from"`

	// Broadcast reports that this went to every agent except the sender.
	Broadcast bool `json:"broadcast"`

	// To lists the recipients: exactly one for a direct message, empty for a
	// broadcast.
	To []string `json:"to"`

	// BusPath is the bus ids this message has traversed, oldest first. It is
	// what prevents a relay loop in a cyclic peer topology.
	BusPath []string `json:"bus_path"`

	// SentAt is the bus's timestamp, verbatim.
	SentAt string `json:"sent_at"`

	// Size is the body length in bytes, as the bus recorded it.
	Size int `json:"size"`

	// ContentSHA256 is the hex SHA-256 of the DECODED body.
	ContentSHA256 string `json:"content_sha256"`

	// Body is the DECODED message body.
	//
	// It is []byte rather than string on purpose: encoding/json round-trips a
	// []byte as a standard-base64 STRING, which is exactly the wire form the bus
	// uses, so the decoding is the standard library's rather than something this
	// package hand-rolls — and a caller gets bytes, not a string it has to
	// remember to decode. A body is arbitrary bytes; it is NOT run through
	// safeText, because mangling a payload would be worse than printing it, and
	// deciding how to render it belongs to whoever consumes it.
	Body []byte `json:"body"`
}

// SendOptions is the input to Send.
type SendOptions struct {
	// To is the fully-qualified recipient `<bus-id>.<agent-id>` (invariant 2).
	To string

	// Body is the message payload, DECODED. At most MaxBodyBytes.
	Body []byte

	// IdempotencyKey makes the send safe to retry (invariant 10). Leave it
	// empty and Send mints a fresh random one.
	//
	// Supply one only to RETRY a specific earlier send, with a BYTE-IDENTICAL
	// body. Same key + same payload is a legitimate retry and is answered from
	// the bus's applied-key table; same key + DIFFERENT payload is a protocol
	// violation that earns a 409 AND a disconnection.
	IdempotencyKey string
}

// BroadcastOptions is the input to Broadcast. See SendOptions for the fields'
// meaning; a broadcast has no recipient because it goes to every agent on the
// bus except the sender.
type BroadcastOptions struct {
	Body           []byte
	IdempotencyKey string
}

// SendResult is what the bus returns for an accepted send or broadcast.
//
// It is returned ONLY after the message is committed through the two-phase
// write path and fsynced (invariant 4): a SendResult in hand means the message
// is durable, not merely received.
//
// The json tags are a documented contract surface (CONTRACTS-CLI.md).
type SendResult struct {
	MessageID string   `json:"message_id"`
	Seq       uint64   `json:"seq"`
	From      string   `json:"from"`
	Broadcast bool     `json:"broadcast"`
	To        []string `json:"to"`
	SentAt    string   `json:"sent_at"`

	// ContentSHA256 is the hex SHA-256 of the decoded body, as the bus computed
	// it. A caller that wants end-to-end assurance can compare it with its own.
	ContentSHA256 string `json:"content_sha256"`

	// Replayed reports that the bus answered from its applied-key table rather
	// than writing a second message — i.e. this was a retry and the ORIGINAL
	// result came back. It is NOT an error: it is idempotency working, and the
	// whole point of invariant 10 is that a well-behaved client can retry.
	Replayed bool `json:"replayed"`

	// IdempotencyKey is the key this send was applied under, minted here when
	// the caller supplied none.
	//
	// It is REPORTED, not merely used, because it is the only handle that can
	// retry this exact logical send later. A caller that loses the answer to a
	// network failure and then invents a fresh key has not retried the send, it
	// has sent a second message.
	IdempotencyKey string `json:"idempotency_key"`
}

// AgentSummary is one entry of the bus's roster. It carries NO key material.
type AgentSummary struct {
	// AgentID is the fully-qualified `<bus-id>.<agent-id>`.
	AgentID string `json:"agent_id"`

	// BusID is DERIVED, not received: /v1/agents does not carry a bus id per
	// entry, and invariant 2 says the part before the first '.' of a
	// fully-qualified id IS the bus. It is split out here so a caller does not
	// have to re-derive it (and get the "first dot, not the last" detail wrong)
	// at every call site. An entry whose id carries no bus prefix is refused
	// rather than reported with an empty BusID — see Agents.
	BusID string `json:"bus_id"`

	Name       string `json:"name"`
	EnrolledAt string `json:"enrolled_at"`
}

// AgentList is the bus's roster.
type AgentList struct {
	Agents []AgentSummary `json:"agents"`

	// Count is len(Agents), RECOMPUTED locally rather than taken from the wire.
	// The bus sends a count too, but a value that can disagree with the slice
	// beside it is a value a consumer will eventually trust over the slice; a
	// hostile bus claiming 10000 agents while sending three should not be able
	// to say so through this type.
	Count int `json:"count"`
}

// ReadOptions is the input to Read.
type ReadOptions struct {
	// Cursor is the opaque position returned by a previous batch. Empty means
	// position 0 — "I have seen nothing" — which reads back through the whole
	// RETAINED window (1 day / 1 GiB, whichever binds first).
	Cursor string

	// Limit caps the batch size, 1..MaxBatchLimit. 0 lets the bus apply its own
	// default (DefaultBatchLimit).
	Limit int

	// Wait selects the form of the read:
	//
	//	0  → history: GET /v1/messages, which never parks and returns whatever
	//	     is available right now, possibly nothing.
	//	>0 → long poll: GET /v1/wait with that timeout, which parks until a
	//	     visible message arrives or the deadline passes.
	//
	// It is refused above MaxPollTimeout rather than clamped, mirroring the bus.
	Wait time.Duration
}

// Batch is one page of messages plus the position to resume from.
type Batch struct {
	Messages []Message `json:"messages"`

	// Cursor is the position to pass to the NEXT call. An empty batch returns
	// the cursor unchanged, byte for byte, which is what makes a timed-out long
	// poll resumable — a cursor is never advanced past messages the caller was
	// not handed.
	Cursor string `json:"cursor"`

	// More reports that the batch was cut short (by limit or by the 1 MiB batch
	// byte budget) and another call will return more immediately.
	More bool `json:"more"`

	// TimedOut reports that a long poll reached its deadline with nothing to
	// deliver. It is NOT an error and it is not an anomaly: on a quiet bus it is
	// the steady state, and the bus answers it with a 200.
	TimedOut bool `json:"timed_out"`
}

// sendRequestBody mirrors httpapi.SendRequestBody. The server rejects unknown
// fields, so this struct is exactly the wire shape and nothing more.
//
// Body is []byte and is marshalled by encoding/json as a standard-base64
// string, which is the wire form the bus parses.
type sendRequestBody struct {
	To             string `json:"to"`
	Body           []byte `json:"body"`
	IdempotencyKey string `json:"idempotency_key"`
}

// broadcastRequestBody mirrors httpapi.BroadcastRequestBody.
type broadcastRequestBody struct {
	Body           []byte `json:"body"`
	IdempotencyKey string `json:"idempotency_key"`
}

// Send delivers a direct message to one agent and returns once it is DURABLE.
//
// # Idempotency is the property this call turns on (invariant 10)
//
// The idempotency key is minted ONCE, here, before anything is marshalled — not
// per attempt. That matters because the transport retries: do() marshals the
// body a single time and replays those exact bytes on every attempt, so all
// attempts of one Send carry ONE key and one payload. The bus therefore sees
// "same key + same payload", answers the second attempt from its applied-key
// table, and writes exactly one message. If the key were minted per attempt, a
// retry after a lost acknowledgement would be a SECOND message; if the payload
// varied between attempts it would be "same key + different payload", which is
// a protocol violation that disconnects the client.
//
// That is also why the request is marked retryable at all. A POST is not safe
// to repeat in general; it is safe here precisely because it carries the key.
//
// A 409 — the key reused with different content — is surfaced as its own loud
// KindRejected error rather than the transport's generic wording, because the
// bus's answer to it is a disconnection and the caller needs to know it must
// use a fresh key rather than keep retrying.
func (c *Client) Send(ctx context.Context, opts SendOptions) (SendResult, error) {
	const op = "send"

	to := strings.TrimSpace(opts.To)
	if err := validateRecipient(op, to); err != nil {
		return SendResult{}, err
	}
	if err := validateSendBody(op, opts.Body); err != nil {
		return SendResult{}, err
	}
	key, err := resolveIdempotencyKey(op, opts.IdempotencyKey)
	if err != nil {
		return SendResult{}, err
	}
	return c.submit(ctx, op, routeSend, sendRequestBody{
		To:             to,
		Body:           opts.Body,
		IdempotencyKey: key,
	}, key)
}

// Broadcast delivers a message to every agent on the bus EXCEPT the sender, and
// returns once it is durable. See Send for the idempotency contract, which is
// identical.
func (c *Client) Broadcast(ctx context.Context, opts BroadcastOptions) (SendResult, error) {
	const op = "broadcast"

	if err := validateSendBody(op, opts.Body); err != nil {
		return SendResult{}, err
	}
	key, err := resolveIdempotencyKey(op, opts.IdempotencyKey)
	if err != nil {
		return SendResult{}, err
	}
	return c.submit(ctx, op, routeBroadcast, broadcastRequestBody{
		Body:           opts.Body,
		IdempotencyKey: key,
	}, key)
}

// submit is the shared write path of Send and Broadcast.
//
// The key is threaded through as a parameter rather than read back out of the
// marshalled body so that the FAILURE path can report it too: a send that
// failed with a network error may or may not have been applied, and the key is
// the only thing that lets the caller retry it as the same logical send instead
// of producing a second message.
func (c *Client) submit(ctx context.Context, op, route string, body interface{}, key string) (SendResult, error) {
	ctx, cancel := c.contextWithTimeout(ctx)
	defer cancel()

	// Decoding straight into SendResult is safe because its json tags are the
	// wire shape for every field the bus sends. The two fields the bus does NOT
	// send — replayed and idempotency_key — are overwritten unconditionally
	// below, so a bus that put them in the body cannot influence either.
	var result SendResult
	resp, err := c.authorizedRequest(ctx, request{
		method: http.MethodPost,
		path:   route,
		op:     op,
		body:   body,
		out:    &result,
		// Safe to repeat: the payload was marshalled once, before the retry
		// loop, and carries the idempotency key. See Send.
		retryable: true,
	})
	if err != nil {
		return SendResult{IdempotencyKey: key}, annotateIdempotencyConflict(op, key, err)
	}

	// The bus is authoritative on ids (invariant 1) — authoritative, not
	// unvalidated. Everything here is printed to a terminal and some of it is
	// stored, and a hostile bus chooses every byte of it. See sanitize.go.
	if err := validateServerField(op, "message id", result.MessageID); err != nil {
		return SendResult{IdempotencyKey: key}, err
	}
	if err := validateServerField(op, "sender id", result.From); err != nil {
		return SendResult{IdempotencyKey: key}, err
	}
	for _, to := range result.To {
		if err := validateServerField(op, "recipient id", to); err != nil {
			return SendResult{IdempotencyKey: key}, err
		}
	}
	if result.ContentSHA256 != "" {
		if err := validateServerField(op, "content hash", result.ContentSHA256); err != nil {
			return SendResult{IdempotencyKey: key}, err
		}
	}
	if err := validateServerTimestamp(op, "sent_at", result.SentAt); err != nil {
		return SendResult{IdempotencyKey: key}, err
	}

	// A replay is signalled OUT OF BAND because the body of a replay is
	// byte-identical to the original by design. It is not an error.
	result.Replayed = strings.EqualFold(resp.Header.Get(idempotencyReplayedHeader), "true")
	result.IdempotencyKey = key
	return result, nil
}

// Agents returns the bus's roster.
//
// Every field is validated before it is returned, because this data is printed
// to a terminal and a hostile bus controls all of it (sanitize.go). A field that
// fails validation FAILS THE WHOLE CALL rather than dropping that one entry: the
// roster is small, and a malformed entry does not mean "one bad agent", it means
// the thing on the other end is not the bus we think it is — silently returning
// the rest would hide exactly that.
func (c *Client) Agents(ctx context.Context) (AgentList, error) {
	const op = "agents"

	ctx, cancel := c.contextWithTimeout(ctx)
	defer cancel()

	var list AgentList
	if _, err := c.authorizedRequest(ctx, request{
		method: http.MethodGet,
		path:   routeAgents,
		op:     op,
		out:    &list,
		// A GET that reads a roster changes nothing; repeating it is safe by
		// construction.
		retryable: true,
	}); err != nil {
		return AgentList{}, err
	}

	for i := range list.Agents {
		a := &list.Agents[i]
		if err := validateServerField(op, "agent id", a.AgentID); err != nil {
			return AgentList{}, err
		}
		if err := validateServerField(op, "name", a.Name); err != nil {
			return AgentList{}, err
		}
		if err := validateServerTimestamp(op, "enrolled_at", a.EnrolledAt); err != nil {
			return AgentList{}, err
		}
		// The wire carries no bus id on this route. Invariant 2 says every agent
		// id is `<bus-id>.<agent-id>`, so the prefix up to the FIRST '.' is the
		// bus — first, not last, because an agent id may itself contain dots
		// while a bus id may not.
		busID, _, ok := splitQualifiedID(a.AgentID)
		if !ok {
			return AgentList{}, newError(KindServer, op,
				"the bus listed an agent id that is not fully qualified: "+safeText(a.AgentID, 60),
				"every agent id is `<bus-id>.<agent-id>` (invariant 2); check that --bus points at an agent-bus server")
		}
		a.BusID = busID
	}

	// The bus already sorts by agent id. Sorting again costs nothing on a list
	// this size and means the ORDER of this package's output is a property of
	// this package, not of a remote service's current implementation.
	sort.Slice(list.Agents, func(i, j int) bool { return list.Agents[i].AgentID < list.Agents[j].AgentID })
	if list.Agents == nil {
		list.Agents = []AgentSummary{}
	}
	list.Count = len(list.Agents)
	return list, nil
}

// Read fetches one batch of messages, either as history or as a long poll.
//
// # The two forms, and why the deadline differs between them
//
// With Wait == 0 this is GET /v1/messages: it never parks, and it is bounded by
// Config.Timeout like every other request/response call.
//
// With Wait > 0 this is GET /v1/wait, which PARKS on the bus for up to that
// long. Such a call must NOT inherit Config.Timeout: the default is 30s and the
// bus's ceiling is 5 minutes, so a caller asking for a 5-minute poll would have
// its request killed at 30 seconds — every time, on a quiet bus, looking exactly
// like a broken connection. The wait form therefore gets its own deadline of
// the poll timeout PLUS a slack margin (the larger of minPollSlack and
// Config.Timeout) to cover the round trip and the bus's own scheduling, and a
// caller's tighter deadline still wins because context.WithTimeout never
// extends a parent.
//
// # A timed-out poll is a SUCCESS
//
// The bus answers a poll that found nothing with 200, an empty message list,
// timed_out true, and the cursor unchanged. That is returned as an ordinary
// Batch, never as an error: on a quiet bus it is the steady state, and treating
// it as a failure is how a watcher ends up logging an error every 30 seconds
// for a bus that is working perfectly.
func (c *Client) Read(ctx context.Context, opts ReadOptions) (Batch, error) {
	op, route := "read", routeMessages
	if opts.Wait != 0 {
		op, route = "wait", routeWait
	}

	q := url.Values{}
	if opts.Cursor != "" {
		if len(opts.Cursor) > maxCursorLen {
			return Batch{}, usagef(op,
				"pass a cursor this bus issued, or none at all to start from the beginning of the retained window",
				"cursor is %d bytes, the limit is %d", len(opts.Cursor), maxCursorLen)
		}
		q.Set("cursor", opts.Cursor)
	}
	switch {
	case opts.Limit < 0:
		return Batch{}, usagef(op, "use a limit between 1 and "+strconv.Itoa(MaxBatchLimit)+", or 0 to let the bus choose",
			"limit %d is negative", opts.Limit)
	case opts.Limit > MaxBatchLimit:
		return Batch{}, usagef(op, "use a limit of at most "+strconv.Itoa(MaxBatchLimit)+", and page with the returned cursor",
			"limit %d is above the bus's ceiling of %d", opts.Limit, MaxBatchLimit)
	case opts.Limit > 0:
		q.Set("limit", strconv.Itoa(opts.Limit))
	}

	var cancel context.CancelFunc
	if opts.Wait != 0 {
		secs, err := pollTimeoutSeconds(op, opts.Wait)
		if err != nil {
			return Batch{}, err
		}
		q.Set("timeout", strconv.Itoa(secs))

		if ctx == nil {
			ctx = context.Background()
		}
		slack := c.cfg.Timeout
		if slack < minPollSlack {
			slack = minPollSlack
		}
		ctx, cancel = context.WithTimeout(ctx, time.Duration(secs)*time.Second+slack)
	} else {
		ctx, cancel = c.contextWithTimeout(ctx)
	}
	defer cancel()

	var batch Batch
	if _, err := c.authorizedRequest(ctx, request{
		method: http.MethodGet,
		path:   route,
		query:  q,
		op:     op,
		out:    &batch,
		// A GET with a cursor is safe to repeat by construction: it reads a
		// position, it moves nothing, and the bus advances no state on our
		// behalf. Re-reading the same position returns the same messages.
		retryable: true,
		// A full batch is far larger than the generic response bound allows.
		maxResponse: maxBatchResponseBytes,
	}); err != nil {
		return Batch{}, err
	}
	if err := validateBatch(op, &batch); err != nil {
		return Batch{}, err
	}
	if batch.Messages == nil {
		batch.Messages = []Message{}
	}
	return batch, nil
}

// pollTimeoutSeconds converts a poll duration to the WHOLE SECONDS the bus's
// ?timeout= parameter takes.
//
// The bus rejects anything that is not a positive whole number of seconds with
// a 400, so a sub-second poll is rounded UP to 1s rather than truncated to 0.
// A value above MaxPollTimeout is REFUSED rather than silently clamped, exactly
// as the bus refuses it: a caller that asked for an hour and was quietly given
// five minutes would conclude its request had been dropped.
func pollTimeoutSeconds(op string, d time.Duration) (int, error) {
	if d < 0 {
		return 0, usagef(op, "use a positive poll timeout, e.g. 30s", "poll timeout %s is negative", d)
	}
	if d > MaxPollTimeout {
		return 0, usagef(op,
			"use a poll timeout of at most "+MaxPollTimeout.String()+" and poll again; the bus refuses a longer one rather than clamping it",
			"poll timeout %s is above the bus's ceiling of %s", d, MaxPollTimeout)
	}
	secs := int((d + time.Second - 1) / time.Second)
	if secs < 1 {
		secs = 1
	}
	return secs, nil
}

// validateBatch checks everything a batch carries that will be printed or
// stored. See sanitize.go for why the bus is not trusted to produce safe text.
func validateBatch(op string, b *Batch) error {
	if err := validateServerCursor(op, b.Cursor); err != nil {
		return err
	}
	for i := range b.Messages {
		m := &b.Messages[i]
		if err := validateServerField(op, "message id", m.MessageID); err != nil {
			return err
		}
		if err := validateServerField(op, "sender id", m.From); err != nil {
			return err
		}
		if err := validateServerTimestamp(op, "sent_at", m.SentAt); err != nil {
			return err
		}
		for _, to := range m.To {
			if err := validateServerField(op, "recipient id", to); err != nil {
				return err
			}
		}
		for _, bus := range m.BusPath {
			if err := validateServerField(op, "bus id in bus_path", bus); err != nil {
				return err
			}
		}
		if m.ContentSHA256 != "" {
			if err := validateServerField(op, "content hash", m.ContentSHA256); err != nil {
				return err
			}
		}
	}
	return nil
}

// cursorPattern is the shape a cursor the bus issued must have.
//
// The cursor is opaque and this client never parses it — the check is a SAFETY
// bound, not a re-implementation. It admits the base64url alphabet plus padding
// and the few punctuation characters a future encoding might reasonably use,
// and admits nothing that can move a terminal cursor. The length bound is what
// stops a hostile bus from handing back a value that is then written to the
// local cursor file on every poll.
var cursorPattern = regexp.MustCompile(`^[A-Za-z0-9._~=-]{1,512}$`)

func validateServerCursor(op, cursor string) error {
	if cursor == "" {
		return nil
	}
	if !cursorPattern.MatchString(cursor) {
		return newError(KindServer, op,
			"the bus returned a cursor that is not a well-formed opaque token",
			"check that --bus points at an agent-bus server and not at another service")
	}
	return nil
}

// validateSendBody refuses a body the bus would refuse, before the round trip.
func validateSendBody(op string, body []byte) error {
	if len(body) == 0 {
		// Refusing an ambiguous or empty send rather than sending nothing: an
		// empty body is almost always a caller that read from an empty file, an
		// empty pipe or an unset variable, and the bus rejects it anyway.
		return usagef(op,
			"pass a non-empty body, e.g. --body 'text' or --body-file <path>; an empty send is refused rather than delivered",
			"no message body")
	}
	if len(body) > MaxBodyBytes {
		return usagef(op,
			fmt.Sprintf("the bus accepts at most %d bytes of body; split the payload or send a reference to it", MaxBodyBytes),
			"message body is %d bytes, the limit is %d", len(body), MaxBodyBytes)
	}
	return nil
}

// validateRecipient checks the SHAPE of a recipient id, locally.
//
// It is deliberately permissive, and this is not laziness: invariant 1 keeps the
// SERVER authoritative on ids, so a client that re-derived the id grammar would
// start refusing legitimate ids the day the server's format grew. All this does
// is catch the mistake that is worth catching locally — a bare name where a
// fully-qualified `<bus-id>.<agent-id>` belongs (invariant 2) — and refuse
// anything that could not be an id at all.
func validateRecipient(op, to string) error {
	if to == "" {
		return usagef(op, "pass --to <bus-id>.<agent-id>; list them with `busctl agents`", "no recipient")
	}
	if !serverIDPattern.MatchString(to) {
		return usagef(op,
			"a recipient is 1-256 bytes of [A-Za-z0-9._-]; find the exact id with `busctl agents`",
			"recipient %q is not a well-formed agent id", safeText(to, 60))
	}
	if _, _, ok := splitQualifiedID(to); !ok {
		return usagef(op,
			"use the fully-qualified `<bus-id>.<agent-id>`, not the short name; find it with `busctl agents`",
			"recipient %q is not fully qualified", safeText(to, 60))
	}
	return nil
}

// splitQualifiedID splits a fully-qualified agent id at the FIRST '.'
// (invariant 2: the prefix is the bus id). Both halves must be non-empty.
func splitQualifiedID(id string) (busID, agentID string, ok bool) {
	i := strings.Index(id, ".")
	if i <= 0 || i == len(id)-1 {
		return "", "", false
	}
	return id[:i], id[i+1:], true
}

// resolveIdempotencyKey returns the caller's key, validated, or a fresh one.
func resolveIdempotencyKey(op, key string) (string, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return newIdempotencyKey()
	}
	if err := validateIdempotencyKey(op, key); err != nil {
		return "", err
	}
	return key, nil
}

// annotateIdempotencyConflict replaces the transport's generic 409 wording with
// the specific, loud one this failure deserves.
//
// A 409 on a send is not "the bus refused the request". It is invariant 10's one
// unforgivable case: the SAME idempotency key presented with a DIFFERENT payload.
// The bus treats that as a protocol violation — it answers 409 with
// `Connection: close` and DISCONNECTS — so the caller must be told plainly that
// retrying will not help and that a fresh key is the fix. The generic text
// invites exactly the wrong reaction.
func annotateIdempotencyConflict(op, key string, err error) error {
	// errors.As, not a type assertion: a wrapped *Error would otherwise slip
	// through and keep the generic wording.
	var e *Error
	if !errors.As(err, &e) || e.Status != http.StatusConflict {
		return err
	}
	e.Kind = KindRejected
	e.Message = "the bus refused this " + op + ": idempotency key " + key +
		" was already used with a DIFFERENT payload"
	e.Remedy = "use a FRESH idempotency key for new content — reusing one with different content is a protocol violation and the bus disconnects the client (invariant 10); to RETRY the original message, resend it byte for byte under the same key"
	return e
}
