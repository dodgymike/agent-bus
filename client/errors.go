package client

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Kind classifies a failure into one of a small, CLOSED set of categories.
//
// The set is closed on purpose. It is the vocabulary a caller branches on —
// the CLI maps it to a process exit code, an embedding agent switches on it
// directly — so adding a member is a compatibility event and removing one is a
// breaking change. Anything that does not fit an existing member is
// KindInternal, which is honest, rather than a new member invented at the call
// site.
type Kind string

const (
	// KindInternal is a bug or an unforeseen condition on our side. It is the
	// zero value on purpose: an error that forgot to classify itself reads as
	// "we do not know", never as a specific, actionable category.
	KindInternal Kind = ""

	// KindUsage is a malformed invocation: a missing required flag, an
	// unparseable duration, an unknown subcommand. The caller can fix it
	// without touching the bus.
	KindUsage Kind = "usage"

	// KindConfig is a well-formed invocation that cannot proceed because the
	// local environment is not ready: no identity has been enrolled, the named
	// identity is not in the store, the store is unreadable.
	KindConfig Kind = "config"

	// KindAuth is a credential failure: the bus rejected the session, the
	// signature did not verify, the token expired or was revoked. Distinct from
	// KindConfig because the remedy is different — re-authenticate or re-enrol,
	// rather than pick an identity.
	KindAuth Kind = "auth"

	// KindNetwork is a failure to reach the bus at all: connection refused,
	// DNS failure, timeout, or (once invariant 11 lands) a certificate that
	// does not match the pinned fingerprint. Nothing was necessarily applied.
	KindNetwork Kind = "network"

	// KindServer is a failure the bus reported about itself: a 5xx, or a
	// capacity refusal that survived our retries. Retrying later is reasonable.
	KindServer Kind = "server"

	// KindRejected is a request the bus understood and refused on its merits:
	// an invalid name, an unknown route, an idempotency-key conflict. Retrying
	// the same request unchanged will fail the same way.
	KindRejected Kind = "rejected"

	// KindEmpty is "nothing to report" — a successful operation whose result
	// set is empty. It exists so an agent can branch on an empty long-poll
	// without parsing text, and it is deliberately NOT an error in the sense of
	// something having gone wrong.
	KindEmpty Kind = "empty"
)

// Process exit codes. These are a CONTRACT: an agent shelling out branches on
// them, so a value never changes meaning and a retired value is never reused.
// They are mirrored in CONTRACTS-CLI.md.
//
// 2 is usage rather than 1 because that is what Go's flag package and
// cmd/agent-bus already do, and one convention across both binaries is worth
// more than a tidier numbering.
const (
	ExitOK       = 0 // success
	ExitError    = 1 // internal/unclassified failure
	ExitUsage    = 2 // malformed invocation
	ExitConfig   = 3 // local identity/config not ready
	ExitAuth     = 4 // credential rejected, expired or revoked
	ExitNetwork  = 5 // the bus could not be reached
	ExitServer   = 6 // the bus reported a failure of its own
	ExitRejected = 7 // the bus understood the request and refused it
	ExitEmpty    = 8 // succeeded with nothing to report
)

// Error is the one error type this package returns. Every failure path
// produces one, so a caller never has to pattern-match on error strings.
//
// The split between Message and Remedy is the "errors that name the remedy
// rather than the stack" requirement of invariant 7, made structural: Message
// says what happened, Remedy says what to do about it, and the CLI renders
// them on separate lines. A Remedy is optional but strongly preferred — if you
// cannot name one, that is usually a sign the error is KindInternal.
type Error struct {
	// Kind is the branch-on category. See the Kind constants.
	Kind Kind

	// Op names the operation that failed, e.g. "enrol" or "session begin". It
	// is a short, stable identifier, never a sentence.
	Op string

	// Message is what went wrong, in one line, addressed to a human.
	Message string

	// Remedy is what to do about it, in one line. Empty when there is
	// genuinely nothing useful to say.
	Remedy string

	// Status is the HTTP status the bus answered with, or 0 when the failure
	// happened before or without a response.
	Status int

	// Err is the underlying cause, if any. Exposed through Unwrap so
	// errors.Is/As still work through this type.
	Err error

	// IdempotencyKey is the key a failed MUTATING operation was sent under, or
	// "" when the failure happened before a key existed (a local validation
	// error) or the operation carries none.
	//
	// It is on the ERROR, not only on the result, because the failures where it
	// matters most are the ones that have no result. A network error or a 5xx on
	// a send is genuinely AMBIGUOUS — the message may or may not have been
	// applied — and the key is the only thing that makes the retry the SAME
	// logical send rather than a second message (invariant 10). A caller told
	// only "it failed" has no safe way to try again: inventing a fresh key is
	// not a retry, it is a duplicate.
	//
	// It is exported rather than reported only through Remedy text so that an
	// embedding agent can act on it without parsing a sentence.
	IdempotencyKey string

	// retryAfter is the delay the bus asked for in a Retry-After header, or 0.
	// Unexported because it is scheduling advice consumed by this package's
	// backoff, not a fact a caller should branch on — the Kind is.
	retryAfter time.Duration

	// fatal marks a failure that LOOKS retryable by Kind but is not. Two
	// conditions set it:
	//
	//   - a 503 with no Retry-After, which the bus emits only when its hub
	//     cannot durably accept messages (see statusError);
	//   - a message whose body disagrees with the size or content_sha256 beside
	//     it (see verifyMessageBody). Retrying re-reads the same cursor and gets
	//     the same damaged message, for ever.
	//
	// It is unexported and read through IsFatalUnavailable rather than being a
	// Kind of its own, because the Kind set is a CLOSED vocabulary a caller
	// branches on (and the CLI maps to an exit code): this is still KindServer
	// to everything that switches on Kind, with one extra bit for the two places
	// that must not loop on it — the transport's retry loop and a long-running
	// watch.
	fatal bool
}

// Error implements error.
func (e *Error) Error() string {
	var b strings.Builder
	if e.Op != "" {
		b.WriteString(e.Op)
		b.WriteString(": ")
	}
	if e.Message != "" {
		b.WriteString(e.Message)
	} else if e.Err != nil {
		b.WriteString(e.Err.Error())
	} else {
		b.WriteString("unknown error")
	}
	return b.String()
}

// Unwrap exposes the underlying cause to errors.Is and errors.As.
func (e *Error) Unwrap() error { return e.Err }

// newError builds an *Error. It exists so every construction site in this
// package looks the same and none of them forgets the Kind.
func newError(kind Kind, op, message, remedy string) *Error {
	return &Error{Kind: kind, Op: op, Message: message, Remedy: remedy}
}

// wrapError builds an *Error around a cause.
func wrapError(kind Kind, op, message, remedy string, cause error) *Error {
	return &Error{Kind: kind, Op: op, Message: message, Remedy: remedy, Err: cause}
}

// KindOf reports the Kind of err, following the Unwrap chain.
//
// A nil error is KindEmpty's opposite and reports "" (KindInternal) — callers
// should check for nil before asking, and ExitCode does exactly that.
func KindOf(err error) Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return KindInternal
}

// IsFatalUnavailable reports whether err is a BUS-SIDE failure that RETRYING
// WILL NOT FIX, following the Unwrap chain like KindOf.
//
// Two conditions set it:
//
//   - The bus answered 503 with NO Retry-After header, which it does only when
//     the hub cannot durably accept messages (hub.ErrNotDurable /
//     hub.ErrPoisoned). Every capacity refusal on every route carries the
//     header, so the header's absence is the bus saying "this is not transient"
//     — and, per invariant 4, refusing rather than acknowledging something it
//     cannot make durable.
//   - A message arrived whose body disagrees with the size or content_sha256
//     beside it (CLI-3-FU-HASHVERIFY, verifyMessageBody). A retry re-reads the
//     same cursor and gets the same damaged message.
//
// The NAME is narrower than the condition and is kept for compatibility; read it
// as "fatal, bus-side". Both conditions share the property the name exists to
// convey and that every caller acts on, which is the one that matters: a caller
// that loops — a long poll, a watch, a supervisor — must check this and STOP.
// Backing off forever converts an operator-visible fault into a silent one, and
// in the second case into an actively misleading one: the watch would report
// "no messages arrived" while messages were arriving damaged.
func IsFatalUnavailable(err error) bool {
	var e *Error
	if errors.As(err, &e) {
		return e.fatal
	}
	return false
}

// IdempotencyKeyOf returns the key a failed mutating operation was sent under,
// following the Unwrap chain like KindOf, or "" when there is none.
//
// It follows Unwrap for the same reason the others do: an *Error wrapped once
// with fmt.Errorf on its way up a call stack is still the same failure, and a
// caller that lost the key to a %w would be left unable to retry the send
// safely — which is the entire point of carrying the key on the error.
func IdempotencyKeyOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.IdempotencyKey
	}
	return ""
}

// ExitCode maps err to the process exit code a caller should use.
//
// This mapping lives HERE, in the importable package, rather than in the CLI:
// an agent embedding the client and re-exposing it as its own subprocess must
// be able to produce exactly the codes CONTRACTS-CLI.md documents without
// copying a switch statement that will drift.
func ExitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	switch KindOf(err) {
	case KindUsage:
		return ExitUsage
	case KindConfig:
		return ExitConfig
	case KindAuth:
		return ExitAuth
	case KindNetwork:
		return ExitNetwork
	case KindServer:
		return ExitServer
	case KindRejected:
		return ExitRejected
	case KindEmpty:
		return ExitEmpty
	default:
		return ExitError
	}
}

// ErrorPayload is the JSON shape of a failure under --json. It is a documented
// contract surface (CONTRACTS-CLI.md), so field names are stable.
//
// It carries exit_code as well as kind so a consumer that captured only stdout
// can still recover the code it would have seen.
type ErrorPayload struct {
	// OK is always false. It is present so a consumer can distinguish a
	// success object from a failure object with a single field lookup, without
	// having to know which keys each subcommand emits.
	OK bool `json:"ok"`

	Error    string `json:"error"`
	Kind     Kind   `json:"kind"`
	Remedy   string `json:"remedy,omitempty"`
	Status   int    `json:"status,omitempty"`
	ExitCode int    `json:"exit_code"`

	// IdempotencyKey is the retry handle for a failed mutating operation, under
	// the SAME field name a successful send reports it under, so a consumer
	// reads it the same way whichever it got.
	//
	// omitempty because most failures never had one — a usage error was never
	// going to be applied — and a key field that is present but empty invites a
	// retry under the empty string, which is not a key at all.
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

// NewErrorPayload renders err as the documented JSON failure shape.
func NewErrorPayload(err error) ErrorPayload {
	p := ErrorPayload{
		OK:       false,
		Kind:     KindOf(err),
		ExitCode: ExitCode(err),
	}
	if err != nil {
		p.Error = err.Error()
	}
	var e *Error
	if errors.As(err, &e) {
		p.Remedy = e.Remedy
		p.Status = e.Status
		p.IdempotencyKey = e.IdempotencyKey
	}
	if p.Kind == KindInternal {
		// "" is the zero value of Kind and would be omitted from a human's
		// reading of the JSON as "no kind at all". Say what it is.
		p.Kind = "internal"
	}
	return p
}

// usagef is shorthand for the commonest error this package raises on behalf of
// the CLI: the caller asked for something malformed.
func usagef(op, remedy, format string, args ...interface{}) *Error {
	return newError(KindUsage, op, fmt.Sprintf(format, args...), remedy)
}
