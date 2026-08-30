// Package idem_test exercises the exported surface only (ValidateKey,
// ValidateIdempotencyKey, NewAgentScope, NewEnrolScope, FromRequest,
// ComputeFingerprint), the same posture internal/auth's tests use: match on
// errors.Is against a sentinel, never on error text.
package idem_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/dodgymike/agent-bus/internal/idem"
)

// TestIdempotencyKey is the proof command of record for IDEM-10
// (proof_cmd: `go test -race -run TestIdempotencyKey ./internal/...`). It is
// table-driven and covers, per the task's required cases: at-cap accepted,
// one-byte-over rejected, every disallowed charset class rejected, empty key
// rejected, and same key + two different agent ids treated as DISTINCT.
func TestIdempotencyKey(t *testing.T) {
	t.Run("length and charset", func(t *testing.T) {
		atCap := strings.Repeat("k", idem.MaxKeyLen)
		overCap := strings.Repeat("k", idem.MaxKeyLen+1)

		tests := []struct {
			name    string
			key     string
			wantErr error // nil means accepted
		}{
			{"empty key rejected", "", idem.ErrMissingKey},
			{"single char accepted", "k", nil},
			{"at cap (128 bytes) accepted", atCap, nil},
			{"one byte over cap rejected", overCap, idem.ErrInvalidKey},
			{"far over cap rejected without scanning charset", strings.Repeat("k", idem.MaxKeyLen*100), idem.ErrInvalidKey},
			{"uppercase letters accepted", "ABCxyz", nil},
			{"digits accepted", "0123456789", nil},
			{"dot accepted", "a.b.c", nil},
			{"underscore accepted", "a_b_c", nil},
			{"hyphen accepted", "a-b-c", nil},
			{"uuid-shaped key accepted", "b28e5153-e433-4dd8-9f5a-342ad978d322", nil},

			// Every disallowed charset class, one representative each.
			{"space rejected", "a b", idem.ErrInvalidKey},
			{"slash rejected", "a/b", idem.ErrInvalidKey},
			{"at-sign rejected", "a@b", idem.ErrInvalidKey},
			{"colon rejected", "a:b", idem.ErrInvalidKey},
			{"plus rejected", "a+b", idem.ErrInvalidKey},
			{"equals rejected", "a=b", idem.ErrInvalidKey},
			{"backslash rejected", `a\b`, idem.ErrInvalidKey},
			{"quote rejected", `a"b`, idem.ErrInvalidKey},
			{"newline rejected", "a\nb", idem.ErrInvalidKey},
			{"null byte rejected", "a\x00b", idem.ErrInvalidKey},
			{"control byte rejected", "a\x1bb", idem.ErrInvalidKey},
			{"non-ascii (multi-byte utf8) rejected", "aéb", idem.ErrInvalidKey}, // "aéb"
			{"percent rejected", "a%20b", idem.ErrInvalidKey},
			{"comma rejected", "a,b", idem.ErrInvalidKey},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				err := idem.ValidateKey(tc.key)
				if tc.wantErr == nil {
					if err != nil {
						t.Fatalf("ValidateKey(%q) = %v, want accepted", tc.key, err)
					}
					return
				}
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("ValidateKey(%q) = %v, want error wrapping %v", tc.key, err, tc.wantErr)
				}
			})
		}
	})

	t.Run("ValidateIdempotencyKey requires a non-empty agent id", func(t *testing.T) {
		if err := idem.ValidateIdempotencyKey("", "k"); !errors.Is(err, idem.ErrInvalidAgent) {
			t.Fatalf("ValidateIdempotencyKey(\"\", \"k\") = %v, want ErrInvalidAgent", err)
		}
		if err := idem.ValidateIdempotencyKey("bus-1.alice-1", "k"); err != nil {
			t.Fatalf("ValidateIdempotencyKey(agent, valid key) = %v, want accepted", err)
		}
		if err := idem.ValidateIdempotencyKey("bus-1.alice-1", ""); !errors.Is(err, idem.ErrMissingKey) {
			t.Fatalf("ValidateIdempotencyKey(agent, \"\") = %v, want ErrMissingKey", err)
		}
	})

	t.Run("same key, two different agent ids, are DISTINCT scopes", func(t *testing.T) {
		const key = "shared-key-value"

		scopeA, err := idem.NewAgentScope("bus-1.alice-1", idem.OpSend, key)
		if err != nil {
			t.Fatalf("NewAgentScope(alice): %v", err)
		}
		scopeB, err := idem.NewAgentScope("bus-1.bob-2", idem.OpSend, key)
		if err != nil {
			t.Fatalf("NewAgentScope(bob): %v", err)
		}

		if scopeA == scopeB {
			t.Fatalf("scopes for the SAME key under two different agent ids compared EQUAL: %+v == %+v; per-agent scoping is broken", scopeA, scopeB)
		}
		if scopeA.Agent() == scopeB.Agent() {
			t.Fatalf("scopeA.Agent() == scopeB.Agent() (%q); the two agent ids were supposed to differ", scopeA.Agent())
		}

		// Demonstrate the property IDEM-11 actually relies on: distinct
		// scopes never alias as map keys, so agent A's retry bookkeeping can
		// never observe or be corrupted by agent B's use of the same raw key.
		table := map[idem.Scope]string{
			scopeA: "alice's result",
			scopeB: "bob's result",
		}
		if len(table) != 2 {
			t.Fatalf("map has %d entries, want 2 distinct entries for the two scopes", len(table))
		}
		if table[scopeA] != "alice's result" {
			t.Fatalf("table[scopeA] = %q, want alice's own entry (bob's write must not have clobbered it)", table[scopeA])
		}

		// Same agent, same key, same operation: this one SHOULD collide (a
		// genuine retry), proving the test above is discriminating on agent
		// identity and not on struct layout.
		scopeAAgain, err := idem.NewAgentScope("bus-1.alice-1", idem.OpSend, key)
		if err != nil {
			t.Fatalf("NewAgentScope(alice) again: %v", err)
		}
		if scopeAAgain != scopeA {
			t.Fatalf("re-building the identical (agent, op, key) tuple produced a different Scope: %+v != %+v", scopeAAgain, scopeA)
		}
	})

	t.Run("operation is part of the scope tuple", func(t *testing.T) {
		const key = "same-key"
		sendScope, err := idem.NewAgentScope("bus-1.alice-1", idem.OpSend, key)
		if err != nil {
			t.Fatalf("NewAgentScope(send): %v", err)
		}
		broadcastScope, err := idem.NewAgentScope("bus-1.alice-1", idem.OpBroadcast, key)
		if err != nil {
			t.Fatalf("NewAgentScope(broadcast): %v", err)
		}
		if sendScope == broadcastScope {
			t.Fatalf("one agent reusing the same raw key across two different operations produced the SAME scope; doc.go point 3 requires the operation to disambiguate this")
		}
	})

	t.Run("NewAgentScope rejects an empty agent id", func(t *testing.T) {
		if _, err := idem.NewAgentScope("", idem.OpSend, "k"); !errors.Is(err, idem.ErrInvalidAgent) {
			t.Fatalf("NewAgentScope(\"\", ...) err = %v, want ErrInvalidAgent", err)
		}
	})

	t.Run("NewAgentScope rejects OpEnrol (no authenticated caller yet)", func(t *testing.T) {
		if _, err := idem.NewAgentScope("bus-1.alice-1", idem.OpEnrol, "k"); !errors.Is(err, idem.ErrInvalidOperation) {
			t.Fatalf("NewAgentScope(..., OpEnrol, ...) err = %v, want ErrInvalidOperation", err)
		}
	})

	t.Run("NewAgentScope rejects an unrecognised operation", func(t *testing.T) {
		if _, err := idem.NewAgentScope("bus-1.alice-1", idem.Operation("bogus"), "k"); !errors.Is(err, idem.ErrInvalidOperation) {
			t.Fatalf("NewAgentScope(..., bogus op, ...) err = %v, want ErrInvalidOperation", err)
		}
	})

	t.Run("NewEnrolScope builds the bus-wide enrol scope and rejects an invalid key", func(t *testing.T) {
		scope, err := idem.NewEnrolScope("k")
		if err != nil {
			t.Fatalf("NewEnrolScope: %v", err)
		}
		if !scope.EnrolBusWide() {
			t.Fatalf("NewEnrolScope's result reports EnrolBusWide() = false, want true")
		}
		if scope.Operation() != idem.OpEnrol {
			t.Fatalf("NewEnrolScope's result Operation() = %q, want %q", scope.Operation(), idem.OpEnrol)
		}
		if scope.Agent() != "" {
			t.Fatalf("NewEnrolScope's result Agent() = %q, want empty (bus-wide, no authenticated caller)", scope.Agent())
		}
		if _, err := idem.NewEnrolScope(""); !errors.Is(err, idem.ErrMissingKey) {
			t.Fatalf("NewEnrolScope(\"\") err = %v, want ErrMissingKey", err)
		}
	})

	t.Run("the zero-value Scope is not a valid enrol scope or any agent's scope", func(t *testing.T) {
		var zero idem.Scope
		enrolScope, err := idem.NewEnrolScope("k")
		if err != nil {
			t.Fatalf("NewEnrolScope: %v", err)
		}
		if zero == enrolScope {
			t.Fatalf("the zero-value Scope compared equal to a legitimately constructed enrol scope; a key-only/no-op lookup must never alias a real one")
		}
	})

	t.Run("FromRequest reads the Idempotency-Key header and never fabricates one", func(t *testing.T) {
		h := http.Header{}
		if _, err := idem.FromRequest(h); !errors.Is(err, idem.ErrMissingKey) {
			t.Fatalf("FromRequest(no header) err = %v, want ErrMissingKey", err)
		}

		h.Set(idem.HeaderName, "abc-123")
		got, err := idem.FromRequest(h)
		if err != nil {
			t.Fatalf("FromRequest(valid header): %v", err)
		}
		if got != "abc-123" {
			t.Fatalf("FromRequest returned %q, want %q", got, "abc-123")
		}

		h.Set(idem.HeaderName, strings.Repeat("k", idem.MaxKeyLen+1))
		if _, err := idem.FromRequest(h); !errors.Is(err, idem.ErrInvalidKey) {
			t.Fatalf("FromRequest(oversized header) err = %v, want ErrInvalidKey", err)
		}
	})

	t.Run("MutatingOperations enumerates exactly the mutating routes", func(t *testing.T) {
		want := map[idem.Operation]bool{
			idem.OpEnrol: true, idem.OpSend: true, idem.OpBroadcast: true,
			idem.OpLeave: true, idem.OpPeerEnrol: true, idem.OpRelay: true,
			idem.OpConversationCreate: true,
		}
		if len(idem.MutatingOperations) != len(want) {
			t.Fatalf("MutatingOperations has %d entries, want %d", len(idem.MutatingOperations), len(want))
		}
		for _, op := range idem.MutatingOperations {
			if !want[op] {
				t.Fatalf("MutatingOperations contains unexpected operation %q", op)
			}
			delete(want, op)
		}
		if len(want) != 0 {
			t.Fatalf("MutatingOperations is missing operations: %v", want)
		}
	})
}

// TestComputeFingerprint pins the collision-safety property doc.go point 8
// promises: field boundaries never blur under naive concatenation.
func TestComputeFingerprint(t *testing.T) {
	ab_c := idem.ComputeFingerprint([]byte("ab"), []byte("c"))
	a_bc := idem.ComputeFingerprint([]byte("a"), []byte("bc"))
	if ab_c == a_bc {
		t.Fatalf("ComputeFingerprint([]byte(\"ab\"), []byte(\"c\")) == ComputeFingerprint([]byte(\"a\"), []byte(\"bc\")); field boundaries are ambiguous")
	}

	same1 := idem.ComputeFingerprint([]byte("x"), []byte("y"))
	same2 := idem.ComputeFingerprint([]byte("x"), []byte("y"))
	if same1 != same2 {
		t.Fatalf("ComputeFingerprint is not deterministic for identical input")
	}

	empty := idem.ComputeFingerprint()
	if empty == same1 {
		t.Fatalf("ComputeFingerprint() (no fields) collided with a non-empty input")
	}
}
