package httpapi_test

// Tests for the DUR-9 half that lives in this package: Options.Durable is
// carried onto the Server and handed back by Durable(), and a nil Durable --
// the zero Options, and every existing test in this package -- changes nothing
// about the two routes that exist today.
//
// Deliberately no real log ON DISK here: DurableLog is a one-method interface
// precisely so this package's tests never have to open one. The wal package is
// imported only for its wire types (wal.Entry, wal.Committed) -- the values the
// interface traffics in -- while the log itself is a fake, so these tests stay
// independent of the durability layer's on-disk format, its fsync cost and its
// recovery behaviour. Those belong to internal/wal's own tests.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dodgymike/agent-bus/internal/httpapi"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// fakeDurable is a stand-in DurableLog. It records what it was asked to write
// so a future handler's use of the write path is observable, and it is
// comparable by POINTER, which is what lets the identity assertion below prove
// the exact value supplied is the exact value returned.
type fakeDurable struct {
	writes []wal.Entry
	next   uint64
	err    error
}

func (f *fakeDurable) Write(e wal.Entry) (wal.Committed, error) {
	if f.err != nil {
		return wal.Committed{}, f.err
	}
	f.writes = append(f.writes, e)
	f.next += 2 // one PREPARE + one COMMIT per write, as the real log does
	return wal.Committed{PrepareIndex: f.next - 1, CommitIndex: f.next, Entry: e}, nil
}

// TestServerDurable pins the wiring: whatever main passes as Options.Durable is
// what Server.Durable() returns, and nothing else invents one.
func TestServerDurable(t *testing.T) {
	t.Run("returns exactly the supplied log", func(t *testing.T) {
		want := &fakeDurable{}
		srv := httpapi.New(httpapi.Options{Durable: want})

		got := srv.Durable()
		if got == nil {
			t.Fatal("Durable() = nil after New(Options{Durable: ...}); the durable write path was dropped on the floor")
		}
		// Pointer identity, not merely "non-nil": a regression that wrapped,
		// copied or replaced the log with a stand-in would still be non-nil but
		// would no longer be the ONE write path main opened and replayed
		// (invariant 4 -- a second write path is a second, unfsynced truth).
		if got != httpapi.DurableLog(want) {
			t.Fatalf("Durable() = %p, want the supplied %p", got, want)
		}

		// The interface a handler will actually use is reachable through the
		// accessor, so the epic that adds writing handlers has a live write
		// path rather than an opaque field.
		committed, err := got.Write(wal.Entry{Kind: "test", Body: json.RawMessage(`{"n":1}`)})
		if err != nil {
			t.Fatalf("Write through the returned DurableLog: %v", err)
		}
		if committed.CommitIndex <= committed.PrepareIndex {
			t.Fatalf("commit index %d must follow prepare index %d", committed.CommitIndex, committed.PrepareIndex)
		}
		if len(want.writes) != 1 || want.writes[0].Kind != "test" {
			t.Fatalf("fake recorded %+v, want exactly one entry of kind \"test\"", want.writes)
		}
	})

	t.Run("nil by default", func(t *testing.T) {
		// There is deliberately NO default write path: a no-op stand-in would
		// be a write path that silently loses data. A caller must check.
		if got := httpapi.New(httpapi.Options{}).Durable(); got != nil {
			t.Fatalf("New(Options{}).Durable() = %v, want nil; there must be no implicit durable log", got)
		}
	})

	t.Run("does not touch the existing routes", func(t *testing.T) {
		// DUR-9 carries the log; it wires it into no route. Both servers --
		// with and without a Durable -- must answer the two live endpoints
		// identically, and neither may panic on a nil Durable.
		cases := []struct {
			name    string
			durable httpapi.DurableLog
		}{
			{"nil durable", nil},
			{"with durable", &fakeDurable{}},
		}
		paths := []string{"/healthz", "/v1/info"}

		bodies := make(map[string]string)
		for _, tc := range cases {
			srv := httpapi.New(httpapi.Options{
				Identity: testIdentity("bus-durable-test"),
				Version:  "v0.0.0-test",
				Durable:  tc.durable,
			})
			for _, path := range paths {
				req := httptest.NewRequest(http.MethodGet, path, nil)
				rec := httptest.NewRecorder()
				srv.ServeHTTP(rec, req)

				if rec.Code != http.StatusOK {
					t.Fatalf("%s: GET %s status = %d, want 200 (body=%q)", tc.name, path, rec.Code, rec.Body.String())
				}
				if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
					t.Fatalf("%s: GET %s Content-Type = %q, want application/json", tc.name, path, ct)
				}

				var generic map[string]interface{}
				if err := json.Unmarshal(rec.Body.Bytes(), &generic); err != nil {
					t.Fatalf("%s: GET %s body %q: %v", tc.name, path, rec.Body.String(), err)
				}
				// A durable log is process-internal state; an unauthenticated
				// response must not grow a field because one was supplied (no
				// data dir, no log path, no index high-water mark).
				for k := range generic {
					switch path {
					case "/healthz":
						if k != "status" {
							t.Fatalf("%s: GET /healthz leaked field %q: %v", tc.name, k, generic)
						}
					case "/v1/info":
						if k != "bus_id" && k != "version" && k != "uptime_seconds" {
							t.Fatalf("%s: GET /v1/info leaked field %q: %v", tc.name, k, generic)
						}
					}
				}
				if path == "/healthz" {
					// Byte-for-byte: /healthz has no clock in it, so the two
					// bodies must be identical whether or not a log is wired.
					if prev, ok := bodies[path]; ok && prev != rec.Body.String() {
						t.Fatalf("GET %s body changed with Durable set: %q -> %q", path, prev, rec.Body.String())
					}
					bodies[path] = rec.Body.String()
				}
			}
		}
	})
}
