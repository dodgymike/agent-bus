package client

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPreTLSMigrationBootstrapsFingerprintAndClientCertificate(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	cred, pub := newTestCredential(t, "bus-x.agent-1")
	cred.BusURL = "http://127.0.0.1:18080"
	cred.BusFingerprints = nil
	if err := store.PromotePending("", cred, true); err != nil {
		t.Fatalf("PromotePending: %v", err)
	}

	busCert := newSelfSignedBusCert(t)
	pin := fingerprintOf(busCert)
	var enrollHits int32
	var bootstrapHits int32
	var presentedClientCert string

	bus := newMutualTLSBus(t, busCert, tls.RequireAnyClientCert, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case routeEnroll:
			atomic.AddInt32(&enrollHits, 1)
			w.WriteHeader(http.StatusInternalServerError)
		case routeSessionBegin:
			var body sessionBeginRequest
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("session begin body: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if body.AgentID != cred.AgentID {
				t.Errorf("session begin agent = %q, want %q", body.AgentID, cred.AgentID)
			}
			writeClientTestJSON(w, sessionBeginResponse{
				AgentID:            cred.AgentID,
				Token:              "migration-challenge-token",
				ChallengeExpiresAt: time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano),
			})
		case routeSessionComplete:
			b, _ := io.ReadAll(r.Body)
			var body sessionCompleteRequest
			if err := json.Unmarshal(b, &body); err != nil {
				t.Errorf("session complete body: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			sig, err := base64.StdEncoding.DecodeString(body.Signature)
			if err != nil {
				t.Errorf("signature is not base64: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if !ed25519.Verify(pub, []byte(SessionSigningContext+body.Token), sig) {
				t.Errorf("session signature did not verify against stored auth key")
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			writeClientTestJSON(w, sessionCompleteResponse{
				AgentID:             cred.AgentID,
				ExpiresAt:           time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano),
				LifetimeSeconds:     3600,
				RefreshAfterSeconds: 2700,
			})
		case routeClientCertBootstrap:
			atomic.AddInt32(&bootstrapHits, 1)
			if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer ") {
				t.Errorf("bootstrap Authorization = %q, want bearer", got)
			}
			if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
				t.Errorf("bootstrap request arrived without a client certificate")
				w.WriteHeader(http.StatusForbidden)
				return
			}
			presentedClientCert = fingerprintOfDER(r.TLS.PeerCertificates[0].Raw)
			var req clientCertBootstrapRequestBody
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("bootstrap body: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if req.IdempotencyKey == "" {
				t.Errorf("bootstrap omitted idempotency_key")
			}
			sig, err := base64.StdEncoding.DecodeString(req.Signature)
			if err != nil {
				t.Errorf("bootstrap signature is not base64: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			fp, err := ParseBusFingerprint(presentedClientCert)
			if err != nil {
				t.Errorf("presented client fingerprint parse: %v", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			if !ed25519.Verify(pub, clientCertificateBootstrapSigningBytes("migration-challenge-token", req.IdempotencyKey, fp), sig) {
				t.Errorf("bootstrap signature did not verify against stored auth key and presented cert fingerprint")
				w.WriteHeader(http.StatusForbidden)
				return
			}
			writeClientTestJSON(w, clientCertBootstrapResponseBody{
				AgentID:               cred.AgentID,
				ClientCertFingerprint: presentedClientCert,
				BoundAt:               time.Now().UTC().Format(time.RFC3339Nano),
				AlreadyBound:          false,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	c, err := New(Config{BusURL: bus.URL(), IdentityDir: dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := c.BootstrapClientCertificate(context.Background(), pin.String())
	if err != nil {
		t.Fatalf("BootstrapClientCertificate: %v", err)
	}
	if res.AgentID != cred.AgentID {
		t.Fatalf("AgentID = %q, want unchanged %q", res.AgentID, cred.AgentID)
	}
	if res.BusURL != bus.URL() {
		t.Fatalf("BusURL = %q, want %q", res.BusURL, bus.URL())
	}
	if got := res.BusFingerprints; len(got) != 1 || got[0] != pin.String() {
		t.Fatalf("BusFingerprints = %#v, want [%s]", got, pin)
	}
	if res.ClientCertFingerprint == "" || res.ClientCertFingerprint != presentedClientCert {
		t.Fatalf("ClientCertFingerprint = %q, want presented %q", res.ClientCertFingerprint, presentedClientCert)
	}
	if atomic.LoadInt32(&enrollHits) != 0 {
		t.Fatalf("migration called /v1/enroll %d time(s); it must not spend an invite or mint a replacement identity", enrollHits)
	}
	if atomic.LoadInt32(&bootstrapHits) != 1 {
		t.Fatalf("bootstrap hits = %d, want 1", bootstrapHits)
	}
	stored, err := store.Resolve(cred.AgentID)
	if err != nil {
		t.Fatalf("Resolve migrated identity: %v", err)
	}
	if stored.AgentID != cred.AgentID {
		t.Fatalf("stored AgentID = %q, want unchanged %q", stored.AgentID, cred.AgentID)
	}
	if stored.BusURL != bus.URL() {
		t.Fatalf("stored BusURL = %q, want %q", stored.BusURL, bus.URL())
	}
	if got := stored.BusFingerprints; len(got) != 1 || got[0] != pin.String() {
		t.Fatalf("stored BusFingerprints = %#v, want [%s]", got, pin)
	}
}

func TestPreTLSMigrationFailureLeavesLegacyIdentityRetryable(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	cred, pub := newTestCredential(t, "bus-x.agent-1")
	cred.BusURL = "http://127.0.0.1:18080"
	cred.BusFingerprints = nil
	if err := store.PromotePending("", cred, true); err != nil {
		t.Fatalf("PromotePending: %v", err)
	}

	busCert := newSelfSignedBusCert(t)
	pin := fingerprintOf(busCert)
	bus := newMutualTLSBus(t, busCert, tls.RequireAnyClientCert, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case routeSessionBegin:
			writeClientTestJSON(w, sessionBeginResponse{
				AgentID:            cred.AgentID,
				Token:              "failure-challenge-token",
				ChallengeExpiresAt: time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano),
			})
		case routeSessionComplete:
			var body sessionCompleteRequest
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("session complete body: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			sig, err := base64.StdEncoding.DecodeString(body.Signature)
			if err != nil || !ed25519.Verify(pub, []byte(SessionSigningContext+body.Token), sig) {
				t.Errorf("session signature failed: %v", err)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			writeClientTestJSON(w, sessionCompleteResponse{
				AgentID:             cred.AgentID,
				ExpiresAt:           time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano),
				LifetimeSeconds:     3600,
				RefreshAfterSeconds: 2700,
			})
		case routeClientCertBootstrap:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"not yet"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	c, err := New(Config{BusURL: bus.URL(), IdentityDir: dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.BootstrapClientCertificate(context.Background(), pin.String()); err == nil {
		t.Fatal("BootstrapClientCertificate succeeded against a failing server, want error")
	}
	stored, err := store.Resolve(cred.AgentID)
	if err != nil {
		t.Fatalf("Resolve legacy identity: %v", err)
	}
	if stored.BusURL != "http://127.0.0.1:18080" {
		t.Fatalf("failed bootstrap mutated BusURL = %q, want legacy HTTP URL", stored.BusURL)
	}
	if len(stored.BusFingerprints) != 0 {
		t.Fatalf("failed bootstrap stored fingerprints = %#v, want none", stored.BusFingerprints)
	}
}

func writeClientTestJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(v)
}
