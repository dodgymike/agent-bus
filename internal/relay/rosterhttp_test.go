package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/idem"
	"github.com/dodgymike/agent-bus/internal/ids"
)

// rosterResponder is a RosterHandler on a TLS test server backed by a real
// Registry, so the status mapping is exercised against the real errors rather
// than against hand-made ones.
type rosterResponder struct {
	srv *httptest.Server
	reg *Registry

	keys *idem.Store

	mu      sync.Mutex
	applied []RosterUpdate
	retries int
}

func newRosterResponder(t *testing.T, busID string, cfg func(*RosterConfig)) *rosterResponder {
	t.Helper()
	reg, err := NewRegistry(RegistryOptions{BusID: busID})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	r := &rosterResponder{reg: reg, keys: idem.NewStore(idem.StoreOptions{})}
	c := RosterConfig{
		BusID: busID,
		// The wiring site this package deliberately does not own: it consults
		// the applied-key table FIRST (invariant 10), so a legitimate retry —
		// same key, same payload — replays the original result instead of
		// falling through to the version check and being punished with a 409.
		Apply: func(_ context.Context, u RosterUpdate, key string, fp idem.Fingerprint) error {
			sc, err := idem.NewAgentScope(u.BusID, idem.OpPeerEnrol, key)
			if err != nil {
				return err
			}
			r.mu.Lock()
			switch _, outcome := r.keys.Lookup(sc, fp); outcome {
			case idem.OutcomeRetry:
				r.retries++
				r.mu.Unlock()
				return nil // the original result, replayed. Nothing re-applied.
			case idem.OutcomeViolation:
				r.mu.Unlock()
				return ErrIdempotencyViolation
			}
			r.mu.Unlock()

			if err := reg.ApplyRosterUpdate(u); err != nil {
				return err
			}
			r.mu.Lock()
			defer r.mu.Unlock()
			if err := r.keys.Remember(idem.Record{
				Agent: u.BusID, Op: idem.OpPeerEnrol, Key: key, Fingerprint: fp,
				Result: json.RawMessage(`{"applied":true}`), Seq: u.Version,
				CommittedAt: time.Now().UTC(),
			}); err != nil {
				return err
			}
			r.applied = append(r.applied, u)
			return nil
		},
	}
	if cfg != nil {
		cfg(&c)
	}
	h, err := NewRosterHandler(c)
	if err != nil {
		t.Fatalf("NewRosterHandler: %v", err)
	}
	r.srv = httptest.NewTLSServer(h)
	t.Cleanup(r.srv.Close)
	return r
}

func (r *rosterResponder) appliedUpdates() []RosterUpdate {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]RosterUpdate(nil), r.applied...)
}

func (r *rosterResponder) post(t *testing.T, contentType, key string, body []byte) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, r.srv.URL+PeerRosterPath, strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if key != "" {
		req.Header.Set(idem.HeaderName, key)
	}
	resp, err := r.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	buf, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusOK {
		return resp.StatusCode, ""
	}
	var e ErrorBody
	if err := json.Unmarshal(buf, &e); err != nil {
		t.Fatalf("error body %q is not JSON: %v", string(buf), err)
	}
	return resp.StatusCode, e.Error
}

func (r *rosterResponder) postUpdate(t *testing.T, key string, u RosterUpdate) (int, string) {
	t.Helper()
	buf, err := json.Marshal(u)
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	return r.post(t, "application/json", key, buf)
}

// TestPeerRosterSync is RELAY-2's other half: a peer's roster stays current
// between handshakes, and every way that surface can be abused is refused.
func TestPeerRosterSync(t *testing.T) {
	t.Run("a known peer's roster is kept current", func(t *testing.T) {
		remote := newRosterResponder(t, localBus, nil)
		if err := remote.reg.UpsertPeer(PeerRoster{BusID: peerBus, Agents: []string{peerBus + ".beta-1"}}); err != nil {
			t.Fatalf("UpsertPeer: %v", err)
		}

		local := newInitiator(t, peerBus, nil, remote.srv)
		err := local.PushRoster(context.Background(), remote.srv.URL, RosterUpdate{
			BusID: peerBus, Version: 1,
			Added:   []string{peerBus + ".gamma-1"},
			Removed: []string{peerBus + ".beta-1"},
		}, "roster-sync-1")
		if err != nil {
			t.Fatalf("PushRoster: %v", err)
		}

		agents, version, ok := remote.reg.Roster(peerBus)
		if !ok || version != 1 || len(agents) != 1 || agents[0] != peerBus+".gamma-1" {
			t.Fatalf("roster = %v/%d/%v, want [%s.gamma-1]/1/true", agents, version, ok, peerBus)
		}
		if n := len(remote.appliedUpdates()); n != 1 {
			t.Errorf("Apply was called %d times, want 1", n)
		}
	})

	// INVARIANT 10, THE HALF THAT IS EASY TO LOSE. This regression-tests a real
	// defect: the handler used to validate the idempotency key, log it and throw
	// it away, so a peer whose ack was lost in flight retried, fell through to
	// the version check (u.Version <= st.version) and was answered 409 STALE.
	// That punishes precisely the peers retrying correctly, which is the
	// behaviour invariant 10 exists to prevent.
	t.Run("a retried update is replayed, not punished as stale", func(t *testing.T) {
		remote := newRosterResponder(t, localBus, nil)
		if err := remote.reg.UpsertPeer(PeerRoster{BusID: peerBus}); err != nil {
			t.Fatalf("UpsertPeer: %v", err)
		}
		update := RosterUpdate{BusID: peerBus, Version: 1, Added: []string{peerBus + ".gamma-1"}}

		if status, code := remote.postUpdate(t, "same-key", update); status != http.StatusOK {
			t.Fatalf("the first push gave %d/%q, want 200", status, code)
		}
		// Byte-identical retry under the SAME key: the ack was lost, not the
		// update. It must be answered 200 and must NOT be re-applied.
		if status, code := remote.postUpdate(t, "same-key", update); status != http.StatusOK {
			t.Fatalf("the RETRY gave %d/%q, want 200: same key + same payload is a legitimate retry that returns the ORIGINAL result, errors nothing and disconnects nobody (invariant 10)", status, code)
		}
		if n := len(remote.appliedUpdates()); n != 1 {
			t.Errorf("the update was applied %d times, want exactly 1", n)
		}
		remote.mu.Lock()
		retries := remote.retries
		remote.mu.Unlock()
		if retries != 1 {
			t.Errorf("the applied-key table reported %d retries, want 1; if this is 0 the key never reached Apply and the 200 above is an accident", retries)
		}

		// Same key, DIFFERENT payload is the other half of the rule: a protocol
		// violation, not a retry.
		changed := RosterUpdate{BusID: peerBus, Version: 1, Added: []string{peerBus + ".delta-1"}}
		if status, code := remote.postUpdate(t, "same-key", changed); status != http.StatusConflict || code != CodeIdempotencyViolation {
			t.Errorf("same key with new content gave %d/%q, want %d/%q", status, code, http.StatusConflict, CodeIdempotencyViolation)
		}
	})

	t.Run("an unknown peer is 403, not 404", func(t *testing.T) {
		// 403 rather than 404 so this route is not a peer-enumeration oracle.
		remote := newRosterResponder(t, localBus, nil)
		status, code := remote.postUpdate(t, "k1", RosterUpdate{BusID: peerBus, Version: 1, Added: []string{peerBus + ".x-1"}})
		if status != http.StatusForbidden || code != CodeUnknownPeer {
			t.Errorf("got %d/%q, want %d/%q: an update must never create a peer", status, code, http.StatusForbidden, CodeUnknownPeer)
		}
	})

	t.Run("a stale update is 409", func(t *testing.T) {
		remote := newRosterResponder(t, localBus, nil)
		if err := remote.reg.UpsertPeer(PeerRoster{BusID: peerBus}); err != nil {
			t.Fatalf("UpsertPeer: %v", err)
		}
		if status, code := remote.postUpdate(t, "k1", RosterUpdate{BusID: peerBus, Version: 5}); status != http.StatusOK {
			t.Fatalf("seeding version 5 gave %d/%q", status, code)
		}
		status, code := remote.postUpdate(t, "k2", RosterUpdate{BusID: peerBus, Version: 3})
		if status != http.StatusConflict || code != CodeStaleRoster {
			t.Errorf("got %d/%q, want %d/%q: the update was well-formed, it simply lost a race — 400 would read as 'you sent rubbish'", status, code, http.StatusConflict, CodeStaleRoster)
		}
	})

	t.Run("a peer claiming our namespace is refused before the callback", func(t *testing.T) {
		remote := newRosterResponder(t, localBus, nil)
		for _, claimed := range []string{localBus, strings.ToUpper(localBus)} {
			status, code := remote.postUpdate(t, "k1", RosterUpdate{BusID: claimed, Version: 1})
			if status != http.StatusBadRequest || code != CodeBusIDCollision {
				t.Errorf("bus_id %q gave %d/%q, want %d/%q", claimed, status, code, http.StatusBadRequest, CodeBusIDCollision)
			}
		}
		if n := len(remote.appliedUpdates()); n != 0 {
			t.Errorf("Apply was called %d times for a spoofed bus id, want 0: the id rules must not depend on the callback remembering to apply them", n)
		}
	})

	t.Run("rejects malformed transport-level requests", func(t *testing.T) {
		remote := newRosterResponder(t, localBus, func(c *RosterConfig) { c.MaxRequestBytes = 64 })

		t.Run("wrong method", func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, remote.srv.URL+PeerRosterPath, nil)
			if err != nil {
				t.Fatalf("building request: %v", err)
			}
			resp, err := remote.srv.Client().Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("GET gave %d, want %d", resp.StatusCode, http.StatusMethodNotAllowed)
			}
		})

		t.Run("wrong content type", func(t *testing.T) {
			if status, code := remote.post(t, "text/plain", "k", []byte(`{}`)); status != http.StatusUnsupportedMediaType || code != CodeUnsupportedMediaType {
				t.Errorf("got %d/%q, want %d/%q", status, code, http.StatusUnsupportedMediaType, CodeUnsupportedMediaType)
			}
		})

		t.Run("missing idempotency key", func(t *testing.T) {
			if status, code := remote.post(t, "application/json", "", []byte(`{"bus_id":"`+peerBus+`","version":1}`)); status != http.StatusBadRequest || code != CodeInvalidIdempotencyKey {
				t.Errorf("got %d/%q, want %d/%q: a roster push is a mutating call (invariant 10)", status, code, http.StatusBadRequest, CodeInvalidIdempotencyKey)
			}
		})

		t.Run("unknown field", func(t *testing.T) {
			if status, code := remote.post(t, "application/json", "k", []byte(`{"bus_id":"`+peerBus+`","trust_me":1}`)); status != http.StatusBadRequest || code != CodeInvalidRequest {
				t.Errorf("got %d/%q, want %d/%q", status, code, http.StatusBadRequest, CodeInvalidRequest)
			}
		})

		t.Run("oversized body", func(t *testing.T) {
			big, err := json.Marshal(RosterUpdate{BusID: peerBus, Version: 1, Added: []string{peerBus + "." + strings.Repeat("a", 60) + "-1"}})
			if err != nil {
				t.Fatalf("marshalling: %v", err)
			}
			if status, code := remote.post(t, "application/json", "k", big); status != http.StatusRequestEntityTooLarge || code != CodePayloadTooLarge {
				t.Errorf("got %d/%q, want %d/%q", status, code, http.StatusRequestEntityTooLarge, CodePayloadTooLarge)
			}
		})
	})

	t.Run("the client refuses to describe another bus's roster", func(t *testing.T) {
		remote := newRosterResponder(t, localBus, nil)
		local := newInitiator(t, peerBus, nil, remote.srv)
		err := local.PushRoster(context.Background(), remote.srv.URL, RosterUpdate{BusID: thirdBus, Version: 1}, "k")
		if err == nil {
			t.Fatal("PushRoster published a third bus's roster")
		}
		if n := len(remote.appliedUpdates()); n != 0 {
			t.Errorf("the peer was contacted %d times, want 0", n)
		}
	})

	t.Run("NewRosterHandler refuses a config whose failure would be silent", func(t *testing.T) {
		apply := func(context.Context, RosterUpdate, string, idem.Fingerprint) error { return nil }
		for _, tc := range []struct {
			name string
			cfg  RosterConfig
		}{
			{"no bus id", RosterConfig{Apply: apply}},
			{"no apply callback", RosterConfig{BusID: localBus}},
			{"negative byte cap", RosterConfig{BusID: localBus, Apply: apply, MaxRequestBytes: -1}},
		} {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				if _, err := NewRosterHandler(tc.cfg); err == nil {
					t.Fatal("NewRosterHandler accepted an incomplete config")
				}
			})
		}
	})
}

// TestMaxRosterUpdateBytesFitsAMaximumUpdate pins the derivation in registry.go
// for the same reason TestMaxHandshakeBytesFitsAMaximumRoster pins its own: the
// byte cap must never be the thing that rejects an update the entry cap allows.
func TestMaxRosterUpdateBytesFitsAMaximumUpdate(t *testing.T) {
	longName := strings.Repeat("a", 63)
	added := make([]string, MaxRosterUpdateEntries/2)
	removed := make([]string, MaxRosterUpdateEntries/2)
	for i := range added {
		added[i] = fmt.Sprintf("%s.b%s-%d", peerBus, longName, i+1)
		removed[i] = fmt.Sprintf("%s.c%s-%d", peerBus, longName, i+1)
	}
	if _, _, _, err := ids.ParseAgentID(added[0]); err != nil {
		t.Fatalf("test built an invalid id: %v", err)
	}
	buf, err := json.Marshal(RosterUpdate{BusID: strings.Repeat("p", 64), Version: 1 << 62, Added: added, Removed: removed})
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	if len(buf) > MaxRosterUpdateBytes {
		t.Fatalf("a maximum-size roster update encodes to %d bytes, over the %d byte cap; the caps contradict each other", len(buf), MaxRosterUpdateBytes)
	}
}

// TestRosterErrorBodiesNeverEchoPeerInput extends the posture
// TestErrorBodiesNeverEchoPeerInput establishes for the handshake.
func TestRosterErrorBodiesNeverEchoPeerInput(t *testing.T) {
	remote := newRosterResponder(t, localBus, nil)
	if err := remote.reg.UpsertPeer(PeerRoster{BusID: peerBus}); err != nil {
		t.Fatalf("UpsertPeer: %v", err)
	}
	marker := "canary-roster-marker"
	buf, err := json.Marshal(RosterUpdate{BusID: peerBus, Version: 1, Added: []string{peerBus + "." + marker + ".Bad-1"}})
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, remote.srv.URL+PeerRosterPath, strings.NewReader(string(buf)))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(idem.HeaderName, "canary-key")
	resp, err := remote.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; this test only means something on a rejection", resp.StatusCode)
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if strings.Contains(string(raw), marker) {
		t.Fatalf("error body %q echoes peer-supplied input", string(raw))
	}
	if !strings.Contains(string(raw), CodeInvalidRosterUpdate) {
		t.Fatalf("error body %q does not carry the stable %q code", string(raw), CodeInvalidRosterUpdate)
	}
}
