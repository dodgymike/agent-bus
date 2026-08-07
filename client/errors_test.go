package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestExitCodeNil pins the one case that is not a *client.Error at all: a nil
// error must map to ExitOK, the opposite of every classified failure.
func TestExitCodeNil(t *testing.T) {
	if got := ExitCode(nil); got != ExitOK {
		t.Fatalf("ExitCode(nil) = %d, want %d", got, ExitOK)
	}
	if got := KindOf(nil); got != KindInternal {
		t.Fatalf("KindOf(nil) = %q, want %q", got, KindInternal)
	}
}

// TestIdempotencyKeyOfFollowsTheUnwrapChain pins the accessor a caller uses to
// recover the key from a failed write.
//
// It follows Unwrap for the same reason KindOf and IsFatalUnavailable do: an
// *Error that has been wrapped once with fmt.Errorf on its way up a call stack
// is still the same failure, and a caller that lost the key to a %w would be
// left unable to retry the send safely — which is the entire point of carrying
// the key on the error at all.
func TestIdempotencyKeyOfFollowsTheUnwrapChain(t *testing.T) {
	const key = "busctl-0123456789abcdef"

	tests := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"not our error", errors.New("something else"), ""},
		{"bare", &Error{Kind: KindNetwork, Op: "send", Message: "boom", IdempotencyKey: key}, key},
		{"wrapped once", fmt.Errorf("sending: %w", &Error{Kind: KindServer, Op: "send", Message: "boom", IdempotencyKey: key}), key},
		{"wrapped twice", fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", &Error{Kind: KindServer, Op: "send", IdempotencyKey: key})), key},
		{"our error without a key", &Error{Kind: KindUsage, Op: "send", Message: "no body"}, ""},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			if got := IdempotencyKeyOf(tt.err); got != tt.want {
				t.Fatalf("IdempotencyKeyOf(%v) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}

// TestNewErrorPayloadCarriesIdempotencyKey checks the key reaches the --json
// failure document under the documented field name.
//
// The field name is the contract surface (CONTRACTS-CLI.md): an agent that
// captured only stdout recovers the retry handle by reading `idempotency_key`,
// exactly as it reads it from a successful send. It is omitempty because most
// failures have no key — a usage error was never going to be applied — and a
// key field that is present but empty invites a retry under the empty string.
func TestNewErrorPayloadCarriesIdempotencyKey(t *testing.T) {
	const key = "operator-chosen-key-1"

	withKey := NewErrorPayload(&Error{
		Kind:           KindServer,
		Op:             "send",
		Message:        "the bus reported an internal error",
		Remedy:         "retry with --idempotency-key " + key,
		Status:         http.StatusInternalServerError,
		IdempotencyKey: key,
	})
	if withKey.IdempotencyKey != key {
		t.Fatalf("ErrorPayload.IdempotencyKey = %q, want %q", withKey.IdempotencyKey, key)
	}
	encoded, err := json.Marshal(withKey)
	if err != nil {
		t.Fatalf("marshalling the failure payload: %v", err)
	}
	var fields map[string]interface{}
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("the failure payload is not a JSON object: %v (%s)", err, encoded)
	}
	if got, _ := fields["idempotency_key"].(string); got != key {
		t.Fatalf("the failure JSON carries idempotency_key = %v, want %q (%s)", fields["idempotency_key"], key, encoded)
	}
	if ok, _ := fields["ok"].(bool); ok {
		t.Fatalf(`the failure JSON has "ok": true (%s)`, encoded)
	}

	// A failure that never carried a key must not grow an empty one.
	without, err := json.Marshal(NewErrorPayload(&Error{Kind: KindUsage, Op: "send", Message: "no message body"}))
	if err != nil {
		t.Fatalf("marshalling the keyless failure payload: %v", err)
	}
	var keyless map[string]interface{}
	if err := json.Unmarshal(without, &keyless); err != nil {
		t.Fatalf("the keyless failure payload is not a JSON object: %v (%s)", err, without)
	}
	if _, present := keyless["idempotency_key"]; present {
		t.Fatalf("a failure with no idempotency key still emitted the field: %s", without)
	}
}

// TestHTTPStatusMapsToKindAndExitCode drives a real Enrol against a fake bus
// that answers with a fixed status, and checks the resulting error's Kind and
// ExitCode against the table CONTRACTS-CLI.md documents. This exercises the
// mapping through the real path (statusError, in transport.go) rather than
// re-implementing the switch in the test.
func TestHTTPStatusMapsToKindAndExitCode(t *testing.T) {
	tests := []struct {
		status   int
		wantKind Kind
		wantExit int
	}{
		{http.StatusUnauthorized, KindAuth, ExitAuth},
		{http.StatusForbidden, KindAuth, ExitAuth},
		{http.StatusNotFound, KindRejected, ExitRejected},
		{http.StatusConflict, KindRejected, ExitRejected},
		{http.StatusRequestEntityTooLarge, KindRejected, ExitRejected},
		{http.StatusTooManyRequests, KindServer, ExitServer},
		{http.StatusInternalServerError, KindServer, ExitServer},
		{http.StatusServiceUnavailable, KindServer, ExitServer},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(fmt.Sprintf("%d", tt.status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "boom"})
			}))
			defer srv.Close()

			// Attempts: 1 so a retryable status (429/503) is still observed on
			// the first try instead of being retried away within the test.
			c, err := New(Config{
				BusURL:      srv.URL,
				IdentityDir: t.TempDir(),
				Retry:       RetryPolicy{Attempts: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			_, err = c.Enrol(context.Background(), EnrolOptions{Name: "a", Save: true})
			if err == nil {
				t.Fatalf("Enrol against a %d response = nil error, want one", tt.status)
			}
			if got := KindOf(err); got != tt.wantKind {
				t.Fatalf("KindOf(err) = %q, want %q (err: %v)", got, tt.wantKind, err)
			}
			if got := ExitCode(err); got != tt.wantExit {
				t.Fatalf("ExitCode(err) = %d, want %d (err: %v)", got, tt.wantExit, err)
			}
		})
	}
}
