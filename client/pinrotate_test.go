package client

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// rotationAgentID is the id the rotation stub mints. Fully qualified
// (invariant 2), because everything downstream of enrolment assumes it.
const rotationAgentID = "bus-rotate.planner"

// rotationHandler answers enrolment AND the session handshake, so a test can
// ask the one question that matters here — "did a real TLS connection to this
// bus complete and authenticate?" — rather than inferring it from an error
// string.
//
// It verifies the session signature for real (ed25519.Verify over
// SessionSigningContext + the SERVER-CHOSEN token), like stubBus does, so a
// success genuinely means the whole handshake ran over the rotated certificate.
//
// pub carries the enrolled public key, stored by the test after enrolment.
func rotationHandler(t *testing.T, hits *int32, pub *atomic.Value) http.Handler {
	t.Helper()
	var issued int64
	var tokens sync.Map
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		var raw []byte
		if r.Body != nil {
			raw, _ = io.ReadAll(r.Body)
		}
		switch r.URL.Path {
		case routeEnroll:
			stubWriteJSON(w, http.StatusOK, enrolResponseBody{
				AgentID:    rotationAgentID,
				BusID:      "bus-rotate",
				Name:       "planner",
				EnrolledAt: "2026-08-07T12:00:00Z",
			})
		case routeSessionBegin:
			token := fmt.Sprintf("rotation-token-%d", atomic.AddInt64(&issued, 1))
			tokens.Store(token, true)
			stubWriteJSON(w, http.StatusOK, sessionBeginResponse{
				AgentID:            rotationAgentID,
				Token:              token,
				ChallengeExpiresAt: time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano),
			})
		case routeSessionComplete:
			var body sessionCompleteRequest
			if err := json.Unmarshal(raw, &body); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			sig, err := base64.StdEncoding.DecodeString(body.Signature)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			key, _ := pub.Load().(ed25519.PublicKey)
			if key == nil || !ed25519.Verify(key, []byte(SessionSigningContext+body.Token), sig) {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			stubWriteJSON(w, http.StatusOK, sessionCompleteResponse{
				AgentID:             rotationAgentID,
				ExpiresAt:           time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano),
				LifetimeSeconds:     3600,
				RefreshAfterSeconds: 2700,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

// storedPinsOnDisk re-reads the credential file and returns the accept-set
// recorded for agentID.
//
// It opens a FRESH Store rather than asking a live Client, because the property
// under test is what is PERSISTED. A client that had quietly widened its
// in-memory set would pass a check against itself.
func storedPinsOnDisk(t *testing.T, dir, agentID string) []string {
	t.Helper()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore(%s): %v", dir, err)
	}
	cred, err := s.Resolve(agentID)
	if err != nil {
		t.Fatalf("Resolve(%s): %v", agentID, err)
	}
	return cred.BusFingerprints
}

func samePins(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestClientAcceptsEitherPinnedCertificateDuringRotation is the whole point of
// MTLS-ROTATE.
//
// DECISIONS.md E3: a rotating bus SERVES TWO CERTIFICATES during rollover so
// clients can re-pin without downtime, because "rotation must never require
// every client to re-enrol — that would make routine key hygiene
// indistinguishable from a security incident".
//
// Before this task an identity held exactly ONE fingerprint, so the first
// routine rotation after the TLS listener ships would have wedged every
// enrolled agent at once. That is not merely an outage: a wedged fleet is the
// pressure under which somebody proposes letting --bus-fingerprint override the
// stored pin, which converts a DETECTED certificate substitution into an
// ACCEPTED one. The fix is to make rotation work, never to soften the check —
// so this test asserts BOTH halves, and the second half is the one that must
// never be relaxed:
//
//   - either pinned certificate is accepted, with NO re-enrolment: the agent id
//     and the private key are the same ones before and after;
//   - a THIRD certificate is still refused, at every point, including while two
//     are accepted.
func TestClientAcceptsEitherPinnedCertificateDuringRotation(t *testing.T) {
	outgoing := newSelfSignedBusCert(t)
	incoming := newSelfSignedBusCert(t)
	impostor := newSelfSignedBusCert(t)
	for _, pair := range [][2]tls.Certificate{{outgoing, incoming}, {outgoing, impostor}, {incoming, impostor}} {
		if fingerprintOf(pair[0]).Equal(fingerprintOf(pair[1])) {
			t.Fatal("two freshly minted certificates share a fingerprint; this test would prove nothing")
		}
	}

	var hits int32
	var pub atomic.Value
	bus := newTLSBus(t, outgoing, rotationHandler(t, &hits, &pub))
	dir := t.TempDir()

	// Enrol against the OUTGOING certificate, exactly as an agent would today.
	enroller, err := New(Config{BusURL: bus.URL(), BusFingerprint: fingerprintOf(outgoing).String(), IdentityDir: dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	enrolled, err := enroller.Enrol(context.Background(), EnrolOptions{Name: "planner", Save: true, MakeCurrent: true})
	if err != nil {
		t.Fatalf("Enrol against the outgoing certificate: %v", err)
	}
	pubBytes, err := base64.StdEncoding.DecodeString(enrolled.PublicKey)
	if err != nil {
		t.Fatalf("decoding the enrolled public key: %v", err)
	}
	pub.Store(ed25519.PublicKey(pubBytes))

	if got := storedPinsOnDisk(t, dir, rotationAgentID); !samePins(got, fingerprintOf(outgoing).String()) {
		t.Fatalf("after enrol the identity accepts %q, want exactly the outgoing certificate", got)
	}

	// The operator confirms the INCOMING fingerprint out of band and accepts it
	// alongside the outgoing one. This is the whole operator-facing surface of
	// the task: one command, no logout, no re-enrolment.
	adder, err := New(Config{IdentityDir: dir})
	if err != nil {
		t.Fatalf("New (pin add): %v", err)
	}
	if _, err := adder.AddBusPin("", fingerprintOf(incoming).String()); err != nil {
		t.Fatalf("AddBusPin: %v", err)
	}
	if got := storedPinsOnDisk(t, dir, rotationAgentID); !samePins(got, fingerprintOf(outgoing).String(), fingerprintOf(incoming).String()) {
		t.Fatalf("accept-set is %q, want the outgoing certificate then the incoming one", got)
	}

	// A FRESH client for each leg, so nothing is served from a connection
	// pooled while the other certificate was in force: each must succeed on its
	// own merits, through a real handshake.
	connect := func(t *testing.T, when string) (SessionInfo, error) {
		t.Helper()
		c, nerr := New(Config{IdentityDir: dir})
		if nerr != nil {
			t.Fatalf("New (%s): %v", when, nerr)
		}
		return c.EnsureSession(context.Background())
	}

	t.Run("the outgoing certificate still authenticates", func(t *testing.T) {
		bus.serve(outgoing)
		if _, err := connect(t, "outgoing"); err != nil {
			t.Fatalf("the certificate the identity enrolled against was refused after a second pin was added: %v", err)
		}
	})

	t.Run("the incoming certificate authenticates WITHOUT a re-enrolment", func(t *testing.T) {
		bus.serve(incoming)
		if _, err := connect(t, "incoming"); err != nil {
			t.Fatalf("the rotated certificate was refused even though it is pinned: %v — this is the wedged fleet MTLS-ROTATE exists to prevent", err)
		}
		// Nothing was re-enrolled: same identity, same key, one record.
		ids, current, lerr := adder.Identities()
		if lerr != nil {
			t.Fatalf("Identities: %v", lerr)
		}
		if len(ids) != 1 {
			t.Fatalf("the store holds %d identities, want 1 — a rotation must not create a second enrolment", len(ids))
		}
		if ids[0].AgentID != enrolled.AgentID || current != enrolled.AgentID {
			t.Errorf("identity is now %q (current %q), want the original %q", ids[0].AgentID, current, enrolled.AgentID)
		}
		if ids[0].PublicKey != enrolled.PublicKey {
			t.Error("the identity's key changed across the rotation; a re-enrolment happened somewhere")
		}
	})

	t.Run("a THIRD certificate is still refused while two are accepted", func(t *testing.T) {
		bus.serve(impostor)
		_, err := connect(t, "impostor")
		if err == nil {
			t.Fatal("an unpinned certificate was accepted while two were pinned; widening the set must not weaken the check")
		}
		if !errors.Is(err, ErrBusFingerprintMismatch) {
			t.Fatalf("error does not match ErrBusFingerprintMismatch: %v", err)
		}
		// The message names BOTH accepted certificates and the presented one,
		// so an operator mid-rollover can see it is neither of the two rather
		// than hunting for a mismatch against a single value.
		for _, want := range []string{
			fingerprintOf(outgoing).String(),
			fingerprintOf(incoming).String(),
			fingerprintOf(impostor).String(),
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the message does not name %s: %v", want, err)
			}
		}
		assertRemedyNames(t, err, "out of band", "bus_cert_fingerprint", "pin add", "pin remove")
	})

	t.Run("retiring the outgoing certificate ends the rollover", func(t *testing.T) {
		remover, nerr := New(Config{IdentityDir: dir})
		if nerr != nil {
			t.Fatalf("New (pin remove): %v", nerr)
		}
		if _, err := remover.RemoveBusPin("", fingerprintOf(outgoing).String()); err != nil {
			t.Fatalf("RemoveBusPin: %v", err)
		}
		if got := storedPinsOnDisk(t, dir, rotationAgentID); !samePins(got, fingerprintOf(incoming).String()) {
			t.Fatalf("after retiring the outgoing certificate the identity accepts %q, want only the incoming one", got)
		}
		// The retired certificate is now refused — which is the point of
		// retiring it. A set that only ever grows becomes "accept every
		// certificate this bus has ever had".
		bus.serve(outgoing)
		if _, err := connect(t, "retired"); err == nil {
			t.Fatal("the retired certificate is still accepted; `pin remove` did not narrow anything")
		} else if !errors.Is(err, ErrBusFingerprintMismatch) {
			t.Fatalf("error does not match ErrBusFingerprintMismatch: %v", err)
		}
		bus.serve(incoming)
		if _, err := connect(t, "incoming after retire"); err != nil {
			t.Fatalf("the surviving certificate was refused: %v", err)
		}
	})

	t.Run("the accept-set is BOUNDED", func(t *testing.T) {
		// Back to two, then a third is refused rather than silently evicting.
		c, nerr := New(Config{IdentityDir: dir})
		if nerr != nil {
			t.Fatalf("New: %v", nerr)
		}
		if _, err := c.AddBusPin("", fingerprintOf(outgoing).String()); err != nil {
			t.Fatalf("AddBusPin (back to two): %v", err)
		}
		c2, nerr := New(Config{IdentityDir: dir})
		if nerr != nil {
			t.Fatalf("New: %v", nerr)
		}
		_, err := c2.AddBusPin("", fingerprintOf(impostor).String())
		if err == nil {
			t.Fatalf("a %drd certificate was accepted into the set; an unbounded accept-set becomes 'every certificate this bus ever had'", MaxBusPins+1)
		}
		if KindOf(err) != KindUsage {
			t.Errorf("Kind = %q, want %q", KindOf(err), KindUsage)
		}
		assertRemedyNames(t, err, "pin remove")
		if got := storedPinsOnDisk(t, dir, rotationAgentID); len(got) != MaxBusPins {
			t.Errorf("the refused add changed the stored set to %q; a refusal must write nothing", got)
		}
	})

	t.Run("the LAST pin cannot be removed", func(t *testing.T) {
		c, nerr := New(Config{IdentityDir: dir})
		if nerr != nil {
			t.Fatalf("New: %v", nerr)
		}
		if _, err := c.RemoveBusPin("", fingerprintOf(outgoing).String()); err != nil {
			t.Fatalf("RemoveBusPin (down to one): %v", err)
		}
		c2, nerr := New(Config{IdentityDir: dir})
		if nerr != nil {
			t.Fatalf("New: %v", nerr)
		}
		if _, err := c2.RemoveBusPin("", fingerprintOf(incoming).String()); err == nil {
			t.Fatal("the last pin was removed; that leaves an https identity that cannot connect at all — a lockout dressed as a tidy-up")
		} else {
			assertRemedyNames(t, err, "logout")
		}
		if got := storedPinsOnDisk(t, dir, rotationAgentID); !samePins(got, fingerprintOf(incoming).String()) {
			t.Errorf("the refused removal changed the stored set to %q", got)
		}
	})

	if atomic.LoadInt32(&hits) == 0 {
		t.Fatal("the bus was never reached at all; this test proved nothing about a real handshake")
	}
}

// TestPinIsNeverLearnedFromAHandshake is the permanent guard on the property
// that makes the accept-set safe to widen at all.
//
// A pin enters the set ONLY by an explicit operator act — the invite's
// fingerprint at enrolment, or AddBusPin with a value confirmed OUT OF BAND. A
// pin learned at handshake time is trust-on-first-use wearing a different hat,
// and DECISIONS.md E6 abolished TOFU on purpose: the invite blob carries the
// fingerprint precisely so the client knows what to expect BEFORE its first
// connection.
//
// It is easy to get this wrong in a way no ordinary test notices. "Remember the
// certificate we just saw so the next command works" is a one-line change that
// makes every positive test pass MORE often, and the only thing it breaks is
// the guarantee. So this asserts it twice, behaviourally and structurally:
//
//  1. Across a REFUSED handshake and a SUCCESSFUL one, the persisted accept-set
//     is byte-for-byte unchanged.
//  2. The file that can see certificate bytes (pin.go) cannot reach the
//     credential store, and it is the only non-test file that derives a
//     fingerprint from DER. Learning a pin would require breaking one of those,
//     in a diff that says so.
func TestPinIsNeverLearnedFromAHandshake(t *testing.T) {
	t.Run("a refused handshake teaches the store nothing", func(t *testing.T) {
		enrolledCert := newSelfSignedBusCert(t)
		rotated := newSelfSignedBusCert(t)

		var hits int32
		var pub atomic.Value
		bus := newTLSBus(t, enrolledCert, rotationHandler(t, &hits, &pub))
		dir := t.TempDir()

		c, err := New(Config{BusURL: bus.URL(), BusFingerprint: fingerprintOf(enrolledCert).String(), IdentityDir: dir})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		enrolled, err := c.Enrol(context.Background(), EnrolOptions{Name: "planner", Save: true, MakeCurrent: true})
		if err != nil {
			t.Fatalf("Enrol: %v", err)
		}
		pubBytes, err := base64.StdEncoding.DecodeString(enrolled.PublicKey)
		if err != nil {
			t.Fatalf("decoding the enrolled public key: %v", err)
		}
		pub.Store(ed25519.PublicKey(pubBytes))

		before := storedPinsOnDisk(t, dir, rotationAgentID)
		if !samePins(before, fingerprintOf(enrolledCert).String()) {
			t.Fatalf("after enrol the identity accepts %q, want exactly the enrolled certificate", before)
		}

		// The bus presents a DIFFERENT certificate. This is the moment a
		// TOFU-shaped implementation would "helpfully" remember it.
		bus.serve(rotated)
		stale, err := New(Config{IdentityDir: dir})
		if err != nil {
			t.Fatalf("New (stale): %v", err)
		}
		if _, err := stale.EnsureSession(context.Background()); err == nil {
			t.Fatal("the rotated certificate was accepted without ever being pinned")
		}
		if got := storedPinsOnDisk(t, dir, rotationAgentID); !samePins(got, before...) {
			t.Fatalf("the accept-set changed to %q after a REFUSED handshake with %s; a pin was learned from the wire, which is trust-on-first-use",
				got, fingerprintOf(rotated))
		}

		// Retry, repeatedly: a learn-on-second-attempt bug would show here and
		// not above.
		for i := 0; i < 3; i++ {
			retry, nerr := New(Config{IdentityDir: dir})
			if nerr != nil {
				t.Fatalf("New (retry %d): %v", i, nerr)
			}
			if _, err := retry.EnsureSession(context.Background()); err == nil {
				t.Fatalf("attempt %d accepted the unpinned certificate", i+2)
			}
		}
		if got := storedPinsOnDisk(t, dir, rotationAgentID); !samePins(got, before...) {
			t.Fatalf("the accept-set changed to %q after repeated refused handshakes", got)
		}

		// And a SUCCESSFUL handshake does not append either — the accepted
		// certificate is already in the set, and nothing re-records it.
		bus.serve(enrolledCert)
		ok, err := New(Config{IdentityDir: dir})
		if err != nil {
			t.Fatalf("New (success): %v", err)
		}
		if _, err := ok.EnsureSession(context.Background()); err != nil {
			t.Fatalf("the pinned certificate was refused: %v", err)
		}
		if got := storedPinsOnDisk(t, dir, rotationAgentID); !samePins(got, before...) {
			t.Fatalf("the accept-set changed to %q after a SUCCESSFUL handshake", got)
		}
	})

	t.Run("an https bus with no pin is refused rather than learned from", func(t *testing.T) {
		cert := newSelfSignedBusCert(t)
		var hits int32
		var pub atomic.Value
		bus := newTLSBus(t, cert, rotationHandler(t, &hits, &pub))
		dir := t.TempDir()

		c, err := New(Config{BusURL: bus.URL(), IdentityDir: dir})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if _, err := c.Enrol(context.Background(), EnrolOptions{Name: "planner", Save: true, MakeCurrent: true}); err == nil {
			t.Fatal("enrolled against an https bus with no pin; the empty accept-set must mean ABSENT, never ANY")
		}
		if got := atomic.LoadInt32(&hits); got != 0 {
			t.Errorf("the bus saw %d requests; an unpinned https bus must never be contacted at all", got)
		}
	})

	t.Run("pin.go can see certificates and cannot reach the store", func(t *testing.T) {
		// The structural half. To learn a pin from a handshake, the code that
		// holds the raw certificate would have to persist it — so the file that
		// holds raw certificates must have no way to write anything down.
		banned := map[string]string{
			"Store":          "the credential store",
			"Credential":     "a stored credential",
			"AddBusPin":      "the pin-writing path",
			"RemoveBusPin":   "the pin-writing path",
			"PromotePending": "the credential-writing path",
			"ReadFile":       "the filesystem",
		}
		// Prove the matcher can fire before trusting it to pass: store.go, which
		// legitimately does all of this, must trip every one of these names.
		for name := range banned {
			if !fileUsesIdentifier(t, "store.go", name) {
				t.Fatalf("store.go does not use %q, so this guard cannot distinguish 'pin.go is clean' from 'the name no longer exists'", name)
			}
		}
		for name, what := range banned {
			if fileUsesIdentifier(t, pinFile, name) {
				t.Errorf("client/%s references %q and therefore can reach %s. It is the one file that holds raw certificate bytes; giving it a way to persist one is how a pin gets LEARNED from a handshake, which is trust-on-first-use (DECISIONS.md E6). Keep the derivation and the storage apart.",
					pinFile, name, what)
			}
		}
	})

	t.Run("the pin-WRITING calls are confined to named files", func(t *testing.T) {
		// The security gate's finding: confining derivation to pin.go is not
		// enough on its own, because BusFingerprintError.Presented deliberately
		// carries a derived fingerprint OUT of pin.go — an operator has to be
		// told what was presented. An auto-heal is therefore three lines
		// (errors.As, then AddBusPin) written somewhere else.
		//
		// So the WRITE side is confined too, in BOTH packages — cmd/agent-busctl
		// is where the gate observed the previous version of this guard did not
		// look at all. Widening either list is a diff that says, in one word,
		// "something new can now change what certificates are trusted".
		allowed := map[string]map[string]bool{
			"AddBusPin": {
				filepath.Clean("client.go"):                          true, // Client.AddBusPin, the operator entry point
				filepath.Clean("store.go"):                           true, // Store.AddBusPin, the write itself
				filepath.Join("..", "cmd", "agent-busctl", "pin.go"): true, // `agent-busctl pin add`
			},
			"RemoveBusPin": {
				filepath.Clean("client.go"):                          true,
				filepath.Clean("store.go"):                           true,
				filepath.Join("..", "cmd", "agent-busctl", "pin.go"): true,
			},
		}
		for name, permitted := range allowed {
			var seen []string
			walkGoFiles(t, false, func(path string, _ []byte) {
				if fileUsesIdentifier(t, path, name) {
					seen = append(seen, filepath.Clean(path))
				}
			})
			if len(seen) == 0 {
				t.Fatalf("%s is referenced by no file at all; this guard would pass forever on a tree where pinning had been deleted", name)
			}
			for _, path := range seen {
				if !permitted[path] {
					t.Errorf("%s is called from %s, which is not one of the files permitted to change what certificates an identity trusts. If this is a deliberate new operator path, add it here — and if it is an automatic one reacting to a handshake, it is trust-on-first-use (DECISIONS.md E6) and must not exist.",
						name, path)
				}
			}
		}
		// NOTHING THAT WRITES A PIN MAY ALSO SEE WHAT A HANDSHAKE PRESENTED.
		//
		// This is the exact tightening the security gate asked for on its
		// second pass. Confining derivation to pin.go is not enough on its own,
		// because BusFingerprintError.Presented deliberately carries a derived
		// fingerprint out of pin.go — an operator has to be told what the bus
		// showed. So the auto-heal is three lines wherever BOTH halves are in
		// reach, and the first version of this guard applied the ban only to
		// cmd/agent-busctl/pin.go, leaving client.go and store.go — the other
		// two files on the write list — free to do it.
		//
		// Neither references these identifiers today, so the ban costs nothing
		// and is exact.
		for path := range allowed["AddBusPin"] {
			for _, name := range []string{"BusFingerprintError", "Presented", "isPinError"} {
				if fileUsesIdentifier(t, path, name) {
					t.Errorf("%s references %q AND writes pins. That pairing is one `errors.As` away from auto-healing a mismatch by pinning whatever the bus just presented, which is trust-on-first-use (DECISIONS.md E6). Keep sight of the presented certificate out of the files that can persist one.",
						path, name)
				}
			}
		}
	})

	t.Run("only pin.go derives a fingerprint from certificate bytes", func(t *testing.T) {
		// busFingerprintOfDER is the only way to turn wire bytes into a pin. If
		// it is reachable from a file that writes the store, the separation
		// above is worth nothing.
		const derivation = "busFingerprintOfDER"
		var users []string
		walkGoFiles(t, false, func(path string, _ []byte) {
			if fileUsesIdentifier(t, path, derivation) {
				users = append(users, filepath.Clean(path))
			}
		})
		if len(users) != 1 || users[0] != filepath.Clean(pinFile) {
			t.Errorf("%s is used by %v; it must be used by client/%s alone, so that deriving a pin from a live certificate is confined to the file that cannot store one.",
				derivation, users, pinFile)
		}
	})
}

// TestLegacySinglePinIsMigrated: a credential store written by the MTLS-PIN
// build carries `bus_fingerprint` (one string) and no `bus_fingerprints`.
//
// It must keep working WITHOUT a re-enrolment, for exactly the reason
// MessagingKeySeed gives: refusing to load a credential over a field it never
// had would lock an agent out of a bus it is legitimately enrolled on. The
// migration is one-way — once anything writes the store the legacy key is gone
// — and a DOWNGRADE therefore leaves the old binary seeing no pin at all, which
// makes it REFUSE https rather than connect unverified. That is the safe
// direction and it is why the one-way migration is acceptable.
func TestLegacySinglePinIsMigrated(t *testing.T) {
	cert := newSelfSignedBusCert(t)
	pin := fingerprintOf(cert)
	dir := t.TempDir()

	legacy := `{
  "version": 1,
  "current": "bus-legacy.planner",
  "identities": [
    {
      "agent_id": "bus-legacy.planner",
      "bus_id": "bus-legacy",
      "name": "planner",
      "bus_url": "https://127.0.0.1:8443",
      "bus_fingerprint": "` + pin.String() + `",
      "public_key": "MCowBQYDK2VwAyEA",
      "enrolled_at": "2026-08-07T12:00:00Z",
      "private_key_seed": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
    }
  ]
}
`
	if err := os.WriteFile(filepath.Join(dir, storeFileName), []byte(legacy), storeFileMode); err != nil {
		t.Fatalf("seeding a legacy store: %v", err)
	}

	if got := storedPinsOnDisk(t, dir, "bus-legacy.planner"); !samePins(got, pin.String()) {
		t.Fatalf("the legacy single pin loaded as %q, want exactly [%s]", got, pin)
	}

	// It is a real pin, not merely a loaded string: it resolves for the bus it
	// was recorded against.
	c, err := New(Config{IdentityDir: dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	u, pins, err := c.endpoint()
	if err != nil {
		t.Fatalf("endpoint: %v", err)
	}
	if u.String() != "https://127.0.0.1:8443" {
		t.Errorf("resolved bus %q", u)
	}
	if !pins.Equal(NewBusPinSet(pin)) {
		t.Errorf("resolved accept-set %s, want %s", pins, pin)
	}

	// And a rollover can be joined from a migrated identity without re-enrolling.
	rotated := fingerprintOf(newSelfSignedBusCert(t))
	adder, err := New(Config{IdentityDir: dir})
	if err != nil {
		t.Fatalf("New (pin add): %v", err)
	}
	if _, err := adder.AddBusPin("", rotated.String()); err != nil {
		t.Fatalf("AddBusPin on a migrated identity: %v", err)
	}
	if got := storedPinsOnDisk(t, dir, "bus-legacy.planner"); !samePins(got, pin.String(), rotated.String()) {
		t.Fatalf("accept-set is %q, want the migrated pin then the rotated one", got)
	}

	// The legacy key is gone from the rewritten file: one fact, one spelling.
	raw, err := os.ReadFile(filepath.Join(dir, storeFileName))
	if err != nil {
		t.Fatalf("reading the rewritten store: %v", err)
	}
	if strings.Contains(string(raw), `"bus_fingerprint"`) {
		t.Errorf("the rewritten store still carries the legacy `bus_fingerprint` key alongside `bus_fingerprints`; two spellings of one fact eventually disagree:\n%s", raw)
	}
}

// TestDamagedAcceptSetCanStillBeRepaired covers the two failure modes the
// security gate found in a hand-edited or downgraded store, and they are the
// same failure in two shapes: an error message that names a repair command the
// store's own state prevents from running.
//
//   - An OVER-CAP set is refused at connect time and the remedy says
//     `agent-busctl pin remove`. That command must therefore work.
//   - A set with ONE unreadable entry must not take `pin remove` down with it.
//     Removal only ever NARROWS, so proceeding past a garbage entry cannot admit
//     anything, whereas refusing locks the store shut.
func TestDamagedAcceptSetCanStillBeRepaired(t *testing.T) {
	a := fingerprintOf(newSelfSignedBusCert(t))
	b := fingerprintOf(newSelfSignedBusCert(t))
	c := fingerprintOf(newSelfSignedBusCert(t))

	seed := func(t *testing.T, pins []string) string {
		t.Helper()
		dir := t.TempDir()
		s, err := OpenStore(dir)
		if err != nil {
			t.Fatalf("OpenStore: %v", err)
		}
		cred := Credential{
			Identity: Identity{
				AgentID: "bus-damaged.planner", BusID: "bus-damaged", Name: "planner",
				BusURL: "https://127.0.0.1:8443", PublicKey: "cHVi",
				EnrolledAt: "2026-08-07T12:00:00Z", BusFingerprints: pins,
			},
			PrivateKeySeed: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		}
		if err := s.PromotePending("", cred, true); err != nil {
			t.Fatalf("PromotePending: %v", err)
		}
		return dir
	}

	t.Run("an over-cap set is refused at connect time but repairable", func(t *testing.T) {
		dir := seed(t, []string{a.String(), b.String(), c.String()})
		cl, err := New(Config{IdentityDir: dir})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if _, _, err := cl.endpoint(); err == nil {
			t.Fatalf("a set of 3 was honoured; the cap of %d is not enforced on read", MaxBusPins)
		} else {
			assertRemedyNames(t, err, "pin list", "pin remove")
		}
		// The remedy must actually work — the whole point of refusing at connect
		// time rather than at load.
		repair, err := New(Config{IdentityDir: dir})
		if err != nil {
			t.Fatalf("New (repair): %v", err)
		}
		if _, err := repair.RemoveBusPin("", c.String()); err != nil {
			t.Fatalf("the repair command the error names does not work: %v", err)
		}
		fixed, err := New(Config{IdentityDir: dir})
		if err != nil {
			t.Fatalf("New (after repair): %v", err)
		}
		if _, got, err := fixed.endpoint(); err != nil {
			t.Fatalf("still refused after the repair: %v", err)
		} else if !got.Equal(NewBusPinSet(a, b)) {
			t.Errorf("accept-set after repair is %s, want %s and %s", got, a, b)
		}
	})

	t.Run("one unreadable entry does not disable the repair", func(t *testing.T) {
		dir := seed(t, []string{a.String(), "not-a-fingerprint", b.String()})
		repair, err := New(Config{IdentityDir: dir})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if _, err := repair.RemoveBusPin("", b.String()); err != nil {
			t.Fatalf("a single garbage entry disabled `pin remove`, which is the command the operator is told to run: %v", err)
		}
		// The garbage went with it — removal rewrites the set from what could be
		// read — and the result is a set that resolves.
		if got := storedPinsOnDisk(t, dir, "bus-damaged.planner"); !samePins(got, a.String()) {
			t.Fatalf("accept-set is %q, want only %s", got, a)
		}
		fixed, err := New(Config{IdentityDir: dir})
		if err != nil {
			t.Fatalf("New (after repair): %v", err)
		}
		if _, got, err := fixed.endpoint(); err != nil {
			t.Fatalf("still refused after the repair: %v", err)
		} else if !got.Equal(NewBusPinSet(a)) {
			t.Errorf("accept-set after repair is %s, want %s", got, a)
		}
	})

	t.Run("a set this client cannot read is never honoured as NO pin", func(t *testing.T) {
		// The opposite direction, and the one that must stay strict: a damaged
		// set on the CONNECT path is a hard error, never a fallback to unpinned.
		dir := seed(t, []string{"not-a-fingerprint"})
		cl, err := New(Config{IdentityDir: dir})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if _, got, err := cl.endpoint(); err == nil {
			t.Fatalf("an unreadable accept-set resolved to %s instead of failing; a damaged store must never become an unpinned connection", got)
		} else if KindOf(err) != KindConfig {
			t.Errorf("Kind = %q, want %q", KindOf(err), KindConfig)
		}
	})
}

// fileUsesIdentifier reports whether path's Go source refers to name as an
// IDENTIFIER — not in a comment, and not as a substring of a longer name.
//
// Comments are excluded deliberately: these guards are about what the code can
// reach, and a guard that a doc comment can trip is a guard the next author
// deletes for being noisy.
func fileUsesIdentifier(t *testing.T, path, name string) bool {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == name {
			found = true
		}
		return !found
	})
	return found
}
