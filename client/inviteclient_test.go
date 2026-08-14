package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// INVITE-CLIENT — the client half of the invite: EnrolOptions.Invite, the two
// wire fields it adds, and the file/parse surface that loads one.
//
// The property every test in this file is ultimately about is the one that
// cannot be recovered from once it is broken: the invite SECRET is a bearer
// credential — whoever holds it can enrol an agent onto the bus (invariant 3) —
// so it belongs in exactly one place, the request body, and nowhere else. Not in
// an error, not in a log line, not in EnrolResult, not in a %v of anything, and
// not on argv (that half is cmd/agent-busctl/inviteclient_test.go).
//
// Invariants read in full before writing these: 3 (enrolment is invite-only and
// the invite is the only way onto the bus), 10 (same key + same payload is a
// legitimate retry — which is why the no-invite body must stay byte-identical),
// 11 (the invite carries the bus's certificate fingerprint; no CA and no
// trust-on-first-use, so the FIRST connection is the one that must be
// verifiable), and 7 (the CLI is the client, so everything here has to be
// reachable by an embedder too).

// inviteTestSecret is the SENTINEL. It is long, unique and unmistakable so that
// a leak check cannot pass by accident, and so that a substring match against a
// remedy sentence full of ordinary English cannot collide with it.
const inviteTestSecret = "INVITE-SENTINEL-b7f3a91c-4d2e-4f6a-9c81-do-not-leak-0123456789abcdef"

// inviteTestID is the invite's NAME. Unlike the secret it is safe to print, and
// several tests below assert that it IS printed — an operator has to be able to
// see which single-use invite an agent spent.
const inviteTestID = "inv-01H8XTESTINVITE"

// testInvite builds a well-formed invite for bus, carrying the sentinel secret.
func testInvite(busURL, fingerprint string) *Invite {
	return &Invite{
		InviteID:           inviteTestID,
		BusID:              "bus-testbus",
		BusAddress:         busURL,
		BusCertFingerprint: fingerprint,
		InviteSecret:       inviteTestSecret,
		ExpiresAt:          time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano),
	}
}

// inviteBlob renders an invite the way `agent-bus invite mint -json` does, so a
// file test parses the producer's shape rather than a hand-written approximation
// of it.
func inviteBlob(t *testing.T, inv *Invite) []byte {
	t.Helper()
	raw, err := json.Marshal(inv)
	if err != nil {
		t.Fatalf("marshalling the test invite: %v", err)
	}
	return raw
}

// inviteBus is a bus that records what arrived at POST /v1/enroll and answers
// however the test told it to.
//
// It records the HOST as well as the body: "the invite named the bus" is a claim
// about where the request WENT, and a test that only inspects the body would
// pass just as well on a client that sent it somewhere else.
type inviteBus struct {
	mu     sync.Mutex
	hits   int
	bodies []map[string]interface{}
	hosts  []string

	// status, errBody, retryAfter and replayed configure the answer. A zero
	// status means the ordinary 201.
	status     int
	errBody    string
	retryAfter string
	replayed   bool
}

func (b *inviteBus) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var m map[string]interface{}
	_ = json.Unmarshal(raw, &m)

	b.mu.Lock()
	b.hits++
	b.bodies = append(b.bodies, m)
	b.hosts = append(b.hosts, r.Host)
	status, errBody, retryAfter, replayed := b.status, b.errBody, b.retryAfter, b.replayed
	b.mu.Unlock()

	if r.URL.Path != routeEnroll {
		http.NotFound(w, r)
		return
	}
	if status != 0 && (status < 200 || status > 299) {
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, `{"error":%q}`, errBody)
		return
	}
	if replayed {
		w.Header().Set(idempotencyReplayedHeader, "true")
	}
	stubWriteJSON(w, http.StatusCreated, enrolResponseBody{
		AgentID:    "bus-testbus.planner-1",
		BusID:      "bus-testbus",
		Name:       "planner",
		EnrolledAt: time.Now().UTC().Format(time.RFC3339Nano),
	})
}

func (b *inviteBus) calls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.hits
}

func (b *inviteBus) lastBody(t *testing.T) map[string]interface{} {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.bodies) == 0 {
		t.Fatalf("the bus saw no requests, so there is no body to assert on — this proof would be vacuous")
	}
	return b.bodies[len(b.bodies)-1]
}

func (b *inviteBus) lastHost(t *testing.T) string {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.hosts) == 0 {
		t.Fatalf("the bus saw no requests, so there is no host to assert on")
	}
	return b.hosts[len(b.hosts)-1]
}

// newInviteTLSBus starts an https bus with a freshly minted self-signed
// certificate, and returns it with the fingerprint an invite would carry.
//
// https rather than plaintext because that is the case invariant 11 is about:
// the invite carries the fingerprint precisely so the FIRST connection can be
// verified, and a plaintext test would exercise the one path where there is
// nothing to verify.
func newInviteTLSBus(t *testing.T) (*inviteBus, *tlsBus, BusFingerprint) {
	t.Helper()
	cert := newSelfSignedBusCert(t)
	rec := &inviteBus{}
	srv := newTLSBus(t, cert, rec)
	return rec, srv, fingerprintOf(cert)
}

// inviteClient builds a Client with NO bus URL and NO fingerprint unless the
// test supplies one — the state an agent is actually in when it redeems an
// invite, having never spoken to this bus before.
func inviteClient(t *testing.T, dir string, mutate func(*Config)) *Client {
	t.Helper()
	cfg := Config{
		IdentityDir: dir,
		Retry:       RetryPolicy{Attempts: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
	}
	if mutate != nil {
		mutate(&cfg)
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// assertNoSecretInError is the headline security assertion, applied to one
// error.
//
// It renders the error every way a caller or a log line plausibly might —
// including %#v, which does NOT go through Error() and therefore prints the
// struct field by field — because the leak that matters is the one nobody
// remembered to look for.
func assertNoSecretInError(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: got a nil error, so this leak check would be vacuous", what)
	}
	rendered := []string{
		err.Error(),
		fmt.Sprintf("%v", err),
		fmt.Sprintf("%s", err),
		fmt.Sprintf("%+v", err),
		fmt.Sprintf("%#v", err),
	}
	var e *Error
	if errors.As(err, &e) {
		rendered = append(rendered,
			e.Message, e.Remedy,
			fmt.Sprintf("%v", e), fmt.Sprintf("%s", e),
			fmt.Sprintf("%+v", e), fmt.Sprintf("%#v", e),
		)
		payload, merr := json.Marshal(NewErrorPayload(err))
		if merr != nil {
			t.Fatalf("%s: marshalling the error payload: %v", what, merr)
		}
		rendered = append(rendered, string(payload))
	}
	for i, s := range rendered {
		if strings.Contains(s, inviteTestSecret) {
			t.Fatalf("%s: rendering #%d of the error reproduces the invite SECRET, a bearer credential: %s", what, i, s)
		}
	}
}

// assertInviteRedacted checks the Invite itself never renders its secret, for
// both the value and the pointer and for every verb fmt might reach for.
func assertInviteRedacted(t *testing.T, inv Invite) {
	t.Helper()
	rendered := map[string]string{
		"String()":      inv.String(),
		"GoString()":    inv.GoString(),
		"%v Invite":     fmt.Sprintf("%v", inv),
		"%s Invite":     fmt.Sprintf("%s", inv),
		"%+v Invite":    fmt.Sprintf("%+v", inv),
		"%#v Invite":    fmt.Sprintf("%#v", inv),
		"%v *Invite":    fmt.Sprintf("%v", &inv),
		"%s *Invite":    fmt.Sprintf("%s", &inv),
		"%+v *Invite":   fmt.Sprintf("%+v", &inv),
		"%#v *Invite":   fmt.Sprintf("%#v", &inv),
		"%v []*Invite":  fmt.Sprintf("%v", []*Invite{&inv}),
		"%v map value":  fmt.Sprintf("%v", map[string]Invite{"i": inv}),
		"%v in a slice": fmt.Sprintf("%v", []Invite{inv}),
	}
	for verb, s := range rendered {
		if strings.Contains(s, inv.InviteSecret) && inv.InviteSecret != "" {
			t.Fatalf("%s renders the invite SECRET: %s", verb, s)
		}
	}
}

// forgedSuccessLine is the payload a terminal-escape forgery is actually FOR.
// Preceded by an erase-line and a carriage return it overwrites whatever was
// printed and leaves a fabricated success behind — see client/sanitize.go.
const forgedSuccessLine = "agent-busctl: verified OK"

// forgingInviteID is that forgery spelled as an invite_id. It is 28 bytes, well
// under MaxInviteIDLen, so it reaches the CHARSET check rather than the length
// one — which is the whole point: without the charset check this value is
// accepted, stored, and reprinted by every later command.
const forgingInviteID = "\x1b[2K\r" + forgedSuccessLine

// assertNoTerminalForgery asserts that none of the renderings in `rendered`
// carries a RAW ESC or CR, and that no LINE of any of them BEGINS with the
// forged text.
//
// It deliberately does NOT assert the forged WORDS are absent. safeText
// NEUTRALISES control characters; it does not censor English, and a check that
// demanded "agent-busctl: verified OK" never appear would be asserting a
// property the implementation does not have (and must not have — quoting the
// offending value back is how an operator identifies which blob is wrong). The
// security property is that the bytes which MOVE THE CURSOR are gone, so the
// text can only ever appear mid-line, visibly quoted inside a refusal.
func assertNoTerminalForgery(t *testing.T, what string, rendered map[string]string) {
	t.Helper()
	if len(rendered) == 0 {
		t.Fatalf("%s: nothing was rendered, so this check would be vacuous", what)
	}
	for label, s := range rendered {
		if strings.Contains(s, "\x1b") {
			t.Errorf("%s: %s carries a raw ESC, which can erase and rewrite the line it is printed on: %q", what, label, s)
		}
		if strings.Contains(s, "\r") {
			t.Errorf("%s: %s carries a raw CR, which returns the cursor to the start of the line: %q", what, label, s)
		}
		for _, line := range strings.Split(s, "\n") {
			if strings.HasPrefix(line, forgedSuccessLine) {
				t.Errorf("%s: %s has a line BEGINNING with the forged success text, which is indistinguishable from a real one: %q", what, label, line)
			}
		}
	}
}

// errorRenderings renders err every way a terminal or a log line plausibly
// might. It is the same set assertNoSecretInError walks, reused so the control
// checks cover exactly the surface the secret checks do.
func errorRenderings(err error) map[string]string {
	rendered := map[string]string{
		"Error()": err.Error(),
		"%v":      fmt.Sprintf("%v", err),
		"%s":      fmt.Sprintf("%s", err),
		"%+v":     fmt.Sprintf("%+v", err),
		"%#v":     fmt.Sprintf("%#v", err),
	}
	var e *Error
	if errors.As(err, &e) {
		rendered["Message"] = e.Message
		rendered["Remedy"] = e.Remedy
	}
	return rendered
}

// inviteRenderings is every verb fmt might reach for on an Invite and on an
// *Invite. Both, because the Stringer has a VALUE receiver and only the value
// form would be checked if one were forgotten.
func inviteRenderings(inv Invite) map[string]string {
	return map[string]string{
		"String()":    inv.String(),
		"GoString()":  inv.GoString(),
		"%v Invite":   fmt.Sprintf("%v", inv),
		"%s Invite":   fmt.Sprintf("%s", inv),
		"%+v Invite":  fmt.Sprintf("%+v", inv),
		"%#v Invite":  fmt.Sprintf("%#v", inv),
		"%v *Invite":  fmt.Sprintf("%v", &inv),
		"%s *Invite":  fmt.Sprintf("%s", &inv),
		"%+v *Invite": fmt.Sprintf("%+v", &inv),
		"%#v *Invite": fmt.Sprintf("%#v", &inv),
	}
}

// TestClientEnrolWithInvite is the whole client-side invite surface.
//
// Everything lives under this one name deliberately: it is the recorded proof
// command for INVITE-CLIENT, and a property parked in a differently-named test
// is a property the proof does not cover.
func TestClientEnrolWithInvite(t *testing.T) {
	t.Run("the wire carries invite_id and invite_secret beside the ordinary fields", func(t *testing.T) {
		rec, bus, pin := newInviteTLSBus(t)
		inv := testInvite(bus.URL(), pin.String())
		c := inviteClient(t, t.TempDir(), nil)

		res, err := c.Enrol(context.Background(), EnrolOptions{Name: "planner", Invite: inv, Save: true, MakeCurrent: true})
		if err != nil {
			t.Fatalf("Enrol with an invite: %v", err)
		}
		if rec.calls() != 1 {
			t.Fatalf("the bus saw %d enrol requests, want 1", rec.calls())
		}

		// The RAW decoded body, not a Go struct: what is under test is the JSON
		// that actually left this process.
		body := rec.lastBody(t)
		assertEnrolBodyFields(t, body,
			"name", "public_key", "messaging_public_key", "idempotency_key", "invite_id", "invite_secret")
		if got, _ := body["invite_id"].(string); got != inv.InviteID {
			t.Errorf("invite_id on the wire = %q, want %q", got, inv.InviteID)
		}
		if got, _ := body["invite_secret"].(string); got != inv.InviteSecret {
			t.Errorf("invite_secret on the wire = %q, want the invite's secret — the bus cannot redeem an invite it is not shown", got)
		}
		if res.AgentID != "bus-testbus.planner-1" {
			t.Errorf("AgentID = %q, want the SERVER-MINTED id (invariant 1)", res.AgentID)
		}
	})

	t.Run("without an invite the body has neither key at all", func(t *testing.T) {
		// omitempty is LOAD-BEARING, not tidiness (invariant 10). A no-invite
		// body that gained `"invite_id":""` would no longer be byte-identical to
		// the body an in-flight enrolment sent before these fields existed, and
		// its retry would become "same key + DIFFERENT payload" — a 409 it could
		// never escape. So the assertion is key ABSENCE, not empty-string
		// equality.
		rec, bus, pin := newInviteTLSBus(t)
		c := inviteClient(t, t.TempDir(), func(cfg *Config) {
			cfg.BusURL = bus.URL()
			cfg.BusFingerprint = pin.String()
		})

		if _, err := c.Enrol(context.Background(), EnrolOptions{Name: "planner", Save: true}); err != nil {
			t.Fatalf("Enrol without an invite: %v", err)
		}
		body := rec.lastBody(t)
		assertEnrolBodyFields(t, body, "name", "public_key", "messaging_public_key", "idempotency_key")
		if _, present := body["invite_id"]; present {
			t.Errorf("the no-invite body carries an invite_id key (%v); omitempty must keep it absent entirely", body["invite_id"])
		}
		if _, present := body["invite_secret"]; present {
			t.Errorf("the no-invite body carries an invite_secret key; omitempty must keep it absent entirely")
		}
	})

	t.Run("EnrolResult reports the invite id and never the secret", func(t *testing.T) {
		rec, bus, pin := newInviteTLSBus(t)
		inv := testInvite(bus.URL(), pin.String())

		withInvite, err := inviteClient(t, t.TempDir(), nil).
			Enrol(context.Background(), EnrolOptions{Name: "planner", Invite: inv, Save: true})
		if err != nil {
			t.Fatalf("Enrol with an invite: %v", err)
		}
		if withInvite.InviteID != inviteTestID {
			t.Errorf("EnrolResult.InviteID = %q, want %q — an operator has to be able to see which single-use invite was spent", withInvite.InviteID, inviteTestID)
		}

		noInvite, err := inviteClient(t, t.TempDir(), func(cfg *Config) {
			cfg.BusURL = bus.URL()
			cfg.BusFingerprint = pin.String()
		}).Enrol(context.Background(), EnrolOptions{Name: "planner", Save: true})
		if err != nil {
			t.Fatalf("Enrol without an invite: %v", err)
		}
		if noInvite.InviteID != "" {
			t.Errorf("EnrolResult.InviteID = %q on the no-invite path, want \"\"", noInvite.InviteID)
		}

		// The whole serialised result, not the fields we thought to check: the
		// point is to catch a field nobody remembered to look for.
		raw, err := json.Marshal(withInvite)
		if err != nil {
			t.Fatalf("marshalling EnrolResult: %v", err)
		}
		if strings.Contains(string(raw), inviteTestSecret) {
			t.Fatalf("EnrolResult marshals the invite SECRET: %s", raw)
		}
		if !strings.Contains(string(raw), inviteTestID) {
			t.Fatalf("EnrolResult does not carry the invite id, so the check above proves nothing: %s", raw)
		}
		if rec.calls() != 2 {
			t.Fatalf("the bus saw %d enrol requests, want 2", rec.calls())
		}
	})

	t.Run("the invite alone names the bus, with no --bus and no --bus-fingerprint", func(t *testing.T) {
		// This is exactly what scripts/fed-smoke.sh does: it passes neither
		// --bus nor --bus-fingerprint and expects the invite to supply both.
		rec, bus, pin := newInviteTLSBus(t)
		inv := testInvite(bus.URL(), pin.String())
		c := inviteClient(t, t.TempDir(), nil)

		res, err := c.Enrol(context.Background(), EnrolOptions{Name: "planner", Invite: inv, Save: true, MakeCurrent: true})
		if err != nil {
			t.Fatalf("Enrol with an invite and an otherwise empty config: %v", err)
		}
		if rec.calls() != 1 {
			t.Fatalf("the bus saw %d enrol requests, want 1", rec.calls())
		}
		want, err := url.Parse(inv.BusAddress)
		if err != nil {
			t.Fatalf("parsing the invite's bus_address: %v", err)
		}
		if got := rec.lastHost(t); got != want.Host {
			t.Fatalf("the request arrived with Host %q, want the invite's bus_address host %q", got, want.Host)
		}
		if res.BusURL != want.String() {
			t.Errorf("the stored identity records bus_url %q, want the invite's %q", res.BusURL, want.String())
		}
		// The pin came from the invite and is recorded, so no later command has
		// to be told again (invariant 11: the trusted path is the easy path).
		if got := res.BusFingerprints; len(got) != 1 || got[0] != pin.String() {
			t.Errorf("the identity records fingerprints %v, want exactly [%s] — the invite's", got, pin)
		}
	})

	t.Run("a bus or fingerprint that disagrees with the invite is refused, not preferred", func(t *testing.T) {
		other := fingerprintOf(newSelfSignedBusCert(t))

		cases := []struct {
			name     string
			mutate   func(cfg *Config, busURL string, pin BusFingerprint)
			wantErr  bool
			wantKind Kind
			wantMsg  string
		}{
			{
				name: "--bus that disagrees",
				mutate: func(cfg *Config, busURL string, pin BusFingerprint) {
					cfg.BusURL = "https://127.0.0.1:1"
					cfg.BusFingerprint = pin.String()
				},
				wantErr: true, wantKind: KindUsage, wantMsg: "--bus says",
			},
			{
				name: "--bus that matches is merely redundant",
				mutate: func(cfg *Config, busURL string, pin BusFingerprint) {
					cfg.BusURL = busURL
				},
			},
			{
				name: "--bus-fingerprint that disagrees",
				mutate: func(cfg *Config, busURL string, pin BusFingerprint) {
					cfg.BusFingerprint = other.String()
				},
				wantErr: true, wantKind: KindUsage, wantMsg: "--bus-fingerprint says",
			},
			{
				name: "--bus-fingerprint that matches is merely redundant",
				mutate: func(cfg *Config, busURL string, pin BusFingerprint) {
					cfg.BusFingerprint = pin.String()
				},
			},
		}

		for _, tc := range cases {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				rec, bus, pin := newInviteTLSBus(t)
				inv := testInvite(bus.URL(), pin.String())
				c := inviteClient(t, t.TempDir(), func(cfg *Config) { tc.mutate(cfg, bus.URL(), pin) })

				_, err := c.Enrol(context.Background(), EnrolOptions{Name: "planner", Invite: inv, Save: true})
				if !tc.wantErr {
					if err != nil {
						t.Fatalf("Enrol: %v — a value that AGREES with the invite is redundant, not an error", err)
					}
					if rec.calls() != 1 {
						t.Fatalf("the bus saw %d enrol requests, want 1", rec.calls())
					}
					return
				}
				if err == nil {
					t.Fatalf("Enrol = nil error, want the disagreement refused rather than resolved by precedence")
				}
				if KindOf(err) != tc.wantKind {
					t.Fatalf("KindOf(err) = %q, want %q", KindOf(err), tc.wantKind)
				}
				// ZERO requests. "It returned an error" would also be satisfied
				// by a client that sent the enrolment to the wrong bus first.
				if rec.calls() != 0 {
					t.Fatalf("the bus saw %d enrol requests, want 0 — a disagreement must be refused before anything is sent", rec.calls())
				}
				var e *Error
				if !errors.As(err, &e) {
					t.Fatalf("error is not a *client.Error: %v", err)
				}
				if !strings.Contains(e.Message, tc.wantMsg) {
					t.Fatalf("message = %q, want it to name both sources (%q)", e.Message, tc.wantMsg)
				}
				if !strings.Contains(e.Message, inv.InviteID) {
					t.Fatalf("message = %q, want it to name the invite so the operator knows which one disagrees", e.Message)
				}
				assertNoSecretInError(t, tc.name, err)
			})
		}
	})

	t.Run("a pin disagreement names its own source, and only the FLAG path offers a command that accepts it", func(t *testing.T) {
		// REGRESSION for the pinSource split (client/client.go resolvePinsWith).
		//
		// Both rows drive the SAME refusal — an asserted fingerprint that
		// disagrees with a set this store already holds for this bus — and differ
		// only in where the fingerprint came from. That is exactly the pair a
		// future "simplification" would collapse back into one shared message,
		// and collapsing it either way is a real defect:
		//
		//   - Sharing the FLAG wording on the INVITE path hands the operator
		//     `agent-busctl pin add <the invite's fingerprint>` with the value
		//     already filled in. Under invariant 11's own threat model that value
		//     is the one an attacker chose, so a correct refusal becomes a
		//     one-command defeat of the pin. It also names --bus-fingerprint, a
		//     flag the caller never passed (invariant 7: an error names a remedy
		//     that WORKS).
		//   - Sharing the INVITE wording on the flag path removes the legitimate
		//     rollover remedy from the one case where it is right.
		//
		// So the invite row asserts an ABSENCE by name, and the flag row asserts
		// the same strings are still PRESENT. Neither is meaningful without the
		// other.
		other := fingerprintOf(newSelfSignedBusCert(t))

		cases := []struct {
			name string
			// useInvite drives the disagreement through an invite blob rather
			// than through --bus-fingerprint.
			useInvite   bool
			wantPresent []string
			wantAbsent  []string
		}{
			{
				name:      "the INVITE disagrees with the stored pin",
				useInvite: true,
				wantPresent: []string{
					"invite " + inviteTestID, // WHICH blob is wrong
					"trust anchor",
					"OUT OF BAND",
				},
				wantAbsent: []string{
					// The headline security property: no command that would
					// accept the disagreeing fingerprint, and no flag the caller
					// did not pass.
					"pin add",
					"--bus-fingerprint",
					"logout",
				},
			},
			{
				name: "--bus-fingerprint disagrees with the stored pin (UNCHANGED)",
				wantPresent: []string{
					"--bus-fingerprint says",
					"pin add",
					"logout",
				},
				wantAbsent: []string{
					// The flag path must not have picked up the invite wording
					// either: there is no invite here to name.
					"invite " + inviteTestID,
				},
			},
		}

		for _, tc := range cases {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				rec, bus, pin := newInviteTLSBus(t)
				dir := t.TempDir()

				// Enrol once so the store holds an accept-set for THIS bus. The
				// disagreement below is only reachable against a stored pin.
				first, err := inviteClient(t, dir, nil).Enrol(context.Background(), EnrolOptions{
					Name: "planner", Invite: testInvite(bus.URL(), pin.String()), Save: true, MakeCurrent: true,
				})
				if err != nil {
					t.Fatalf("the setup enrolment failed, so the disagreement below is unreachable: %v", err)
				}
				if got := first.BusFingerprints; len(got) != 1 || got[0] != pin.String() {
					t.Fatalf("the setup enrolment stored fingerprints %v, want [%s]", got, pin)
				}
				if rec.calls() != 1 {
					t.Fatalf("the bus saw %d enrol requests during setup, want 1", rec.calls())
				}

				var err2 error
				if tc.useInvite {
					// A SECOND invite for the same bus, naming a different
					// certificate — the substituted-blob case invariant 11 is about.
					bad := testInvite(bus.URL(), other.String())
					_, err2 = inviteClient(t, dir, nil).Enrol(context.Background(), EnrolOptions{
						Name: "planner-2", Invite: bad, Save: true,
					})
				} else {
					c := inviteClient(t, dir, func(cfg *Config) {
						cfg.BusURL = bus.URL()
						cfg.BusFingerprint = other.String()
					})
					_, err2 = c.Enrol(context.Background(), EnrolOptions{Name: "planner-2", Save: true})
				}
				if err2 == nil {
					t.Fatalf("Enrol = nil error, want the disagreement refused rather than resolved by precedence")
				}
				if KindOf(err2) != KindUsage {
					t.Fatalf("KindOf(err) = %q, want %q", KindOf(err2), KindUsage)
				}
				// HARD refusal: nothing reached the bus. "It returned an error"
				// would also be satisfied by a client that dialled first.
				if rec.calls() != 1 {
					t.Fatalf("the bus saw %d enrol requests, want 1 (the setup one) — a pin disagreement must be refused before anything is sent", rec.calls())
				}

				var e *Error
				if !errors.As(err2, &e) {
					t.Fatalf("error is not a *client.Error: %v", err2)
				}
				combined := e.Message + " " + e.Remedy
				for _, want := range tc.wantPresent {
					if !strings.Contains(combined, want) {
						t.Errorf("the refusal does not contain %q:\n  message: %s\n  remedy:  %s", want, e.Message, e.Remedy)
					}
				}
				for _, unwanted := range tc.wantAbsent {
					if strings.Contains(combined, unwanted) {
						t.Errorf("the refusal contains %q, which this path must not offer:\n  message: %s\n  remedy:  %s", unwanted, e.Message, e.Remedy)
					}
				}
				// Both messages must still name BOTH fingerprints — the refusal
				// is useless if the operator cannot see what disagrees with what.
				if !strings.Contains(e.Message, other.String()) || !strings.Contains(e.Message, pin.String()) {
					t.Errorf("message = %q, want it to name both the asserted and the stored certificate", e.Message)
				}
				assertNoSecretInError(t, tc.name, err2)

				// The stored accept-set is UNCHANGED. A refusal that widened the
				// set on its way out would be worse than no refusal at all.
				ids, _, err := inviteClient(t, dir, nil).Identities()
				if err != nil {
					t.Fatalf("Identities: %v", err)
				}
				if len(ids) != 1 {
					t.Fatalf("the store holds %d identities after the refusal, want 1 — nothing may be enrolled", len(ids))
				}
				if got := ids[0].BusFingerprints; len(got) != 1 || got[0] != pin.String() {
					t.Fatalf("the stored accept-set is now %v, want the original [%s] — a refused enrolment must not widen the pin", got, pin)
				}
			})
		}
	})

	t.Run("Validate refuses an unusable invite before anything is dialled", func(t *testing.T) {
		valid := func() Invite { return *testInvite("https://127.0.0.1:8080", strings.Repeat("ab", 32)) }

		// A well-formed https URL padded with a path, at EXACTLY maxBusAddressLen
		// and one clear step over it. Both parse; both reach the same fingerprint
		// checks. So the only thing that can separate them is the bound itself —
		// which is what makes the accepted row load-bearing rather than decorative.
		const addrPrefix = "https://127.0.0.1:8080/"
		atBoundAddr := addrPrefix + strings.Repeat("a", maxBusAddressLen-len(addrPrefix))
		overBoundAddr := addrPrefix + strings.Repeat("a", maxBusAddressLen)
		if len(atBoundAddr) != maxBusAddressLen || len(overBoundAddr) <= maxBusAddressLen {
			t.Fatalf("the bus_address fixtures are %d and %d bytes against a bound of %d; the rows below would not test the bound",
				len(atBoundAddr), len(overBoundAddr), maxBusAddressLen)
		}

		cases := []struct {
			name     string
			mutate   func(*Invite)
			wantErr  bool
			wantKind Kind
			wantMsg  string
			// wantAlso are further strings the MESSAGE must contain, for the rows
			// where one substring is not enough to pin the behaviour.
			wantAlso []string
			// wantAbsent must NOT appear in the rendered error — the oversized
			// values, which are refused precisely because they are not ours.
			wantAbsent string
			// wantNoControls asserts the refusal cannot forge terminal output:
			// no raw ESC, no raw CR, and no LINE beginning with the forged text.
			wantNoControls bool
		}{
			{name: "a well-formed invite is accepted", mutate: func(i *Invite) {}},
			{
				name:    "empty invite_id",
				mutate:  func(i *Invite) { i.InviteID = "" },
				wantErr: true, wantKind: KindConfig, wantMsg: "no invite_id",
			},
			{
				name:    "invite_id over MaxInviteIDLen",
				mutate:  func(i *Invite) { i.InviteID = "inv-" + strings.Repeat("z", MaxInviteIDLen) },
				wantErr: true, wantKind: KindConfig, wantMsg: "an invite id is at most",
				wantAbsent: strings.Repeat("z", 16),
			},
			{
				// REGRESSION for the serverIDPattern check on invite_id. The
				// invite arrives from OUTSIDE, and this id is printed by `enrol`,
				// carried in EnrolResult and repeated in every refusal below —
				// so a value that erases the line it is printed on and writes a
				// fabricated success in its place is REJECTED, not rewritten.
				//
				// Delete the charset case from Validate and this row goes red at
				// once: an id of pure control characters is otherwise accepted,
				// because it is neither empty nor over MaxInviteIDLen.
				name:    "invite_id spelling an ANSI erase and a forged success line",
				mutate:  func(i *Invite) { i.InviteID = forgingInviteID },
				wantErr: true, wantKind: KindConfig, wantMsg: "cannot contain",
				wantNoControls: true,
			},
			{
				name:    "empty invite_secret",
				mutate:  func(i *Invite) { i.InviteSecret = "" },
				wantErr: true, wantKind: KindConfig, wantMsg: "no invite_secret",
			},
			{
				name:    "invite_secret over MaxSecretLen",
				mutate:  func(i *Invite) { i.InviteSecret = inviteTestSecret + strings.Repeat("x", MaxSecretLen) },
				wantErr: true, wantKind: KindConfig, wantMsg: "longer than",
				wantAbsent: strings.Repeat("x", 16),
			},
			{
				name:    "empty bus_address",
				mutate:  func(i *Invite) { i.BusAddress = "" },
				wantErr: true, wantKind: KindConfig, wantMsg: "no bus_address",
			},
			{
				// REGRESSION for maxBusAddressLen. bus_address is the one invite
				// field with no alphabet of its own, and MaxInviteFileBytes alone
				// would admit one of ~64 KiB — which then reaches the transport's
				// own "cannot reach the bus at <url>" message and the credential
				// store, neither of which client/invite.go can sanitise.
				//
				// REFUSED, not truncated, and the value is NOT echoed: only its
				// length and the bound.
				name:    "bus_address over maxBusAddressLen",
				mutate:  func(i *Invite) { i.BusAddress = overBoundAddr },
				wantErr: true, wantKind: KindConfig, wantMsg: "a bus address is at most",
				wantAlso: []string{
					strconv.Itoa(len(overBoundAddr)), // the length it found
					strconv.Itoa(maxBusAddressLen),   // the bound it exceeded
				},
				wantAbsent: strings.Repeat("a", 64),
			},
			{
				// The other side of the bound. Same shape, same scheme, same
				// fingerprint — one byte's difference in length is the ONLY
				// thing separating this row from the one above, so a refusal
				// arriving from some other check would fail here.
				name:   "bus_address at exactly maxBusAddressLen is accepted",
				mutate: func(i *Invite) { i.BusAddress = atBoundAddr },
			},
			{
				name:    "bus_address with no scheme",
				mutate:  func(i *Invite) { i.BusAddress = "bus.example:8080" },
				wantErr: true, wantKind: KindConfig, wantMsg: "cannot use",
			},
			{
				name:    "bus_address with an unsupported scheme",
				mutate:  func(i *Invite) { i.BusAddress = "ftp://bus.example" },
				wantErr: true, wantKind: KindConfig, wantMsg: "cannot use",
			},
			{
				name:    "plaintext bus_address to a non-loopback host",
				mutate:  func(i *Invite) { i.BusAddress = "http://bus.example:8080" },
				wantErr: true, wantKind: KindConfig, wantMsg: "cannot use",
			},
			{
				name: "https bus_address with no fingerprint (invariant 11: no TOFU)",
				mutate: func(i *Invite) {
					i.BusAddress = "https://bus.example:8080"
					i.BusCertFingerprint = ""
				},
				wantErr: true, wantKind: KindConfig, wantMsg: "no bus_cert_fingerprint",
			},
			{
				name:    "malformed bus_cert_fingerprint",
				mutate:  func(i *Invite) { i.BusCertFingerprint = "not-a-fingerprint" },
				wantErr: true, wantKind: KindConfig, wantMsg: "bus_cert_fingerprint this client cannot use",
			},
			{
				name:    "uppercase bus_cert_fingerprint",
				mutate:  func(i *Invite) { i.BusCertFingerprint = strings.ToUpper(strings.Repeat("ab", 32)) },
				wantErr: true, wantKind: KindConfig, wantMsg: "bus_cert_fingerprint this client cannot use",
			},
		}

		for _, tc := range cases {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				inv := valid()
				tc.mutate(&inv)
				err := inv.Validate()
				if !tc.wantErr {
					if err != nil {
						t.Fatalf("Validate() = %v, want nil — without a positive row the refusals prove nothing", err)
					}
					return
				}
				if err == nil {
					t.Fatalf("Validate() = nil, want a refusal")
				}
				if KindOf(err) != tc.wantKind {
					t.Fatalf("KindOf(err) = %q, want %q", KindOf(err), tc.wantKind)
				}
				var e *Error
				if !errors.As(err, &e) {
					t.Fatalf("error is not a *client.Error: %v", err)
				}
				if !strings.Contains(e.Message, tc.wantMsg) {
					t.Fatalf("message = %q, want it to contain %q", e.Message, tc.wantMsg)
				}
				for _, want := range tc.wantAlso {
					if !strings.Contains(e.Message, want) {
						t.Errorf("message = %q, want it to contain %q", e.Message, want)
					}
				}
				if tc.wantAbsent != "" && strings.Contains(e.Message+" "+e.Remedy, tc.wantAbsent) {
					t.Fatalf("the error echoes the oversized value back: %q / %q", e.Message, e.Remedy)
				}
				if tc.wantNoControls {
					assertNoTerminalForgery(t, tc.name, errorRenderings(err))
					assertNoTerminalForgery(t, tc.name+" (the invite itself)", inviteRenderings(inv))
				}
				assertNoSecretInError(t, tc.name, err)
				assertInviteRedacted(t, inv)
			})
		}

		// A nil invite is an internal error, not a config one: nothing the
		// operator did produced it.
		var nilInvite *Invite
		if err := nilInvite.Validate(); err == nil {
			t.Fatalf("(*Invite)(nil).Validate() = nil, want a refusal")
		} else if KindOf(err) != KindInternal {
			t.Fatalf("KindOf(err) = %q for a nil invite, want %q", KindOf(err), KindInternal)
		}
	})

	t.Run("String bounds and neutralises the fields Validate never checks", func(t *testing.T) {
		// REGRESSION for the safeText calls in Invite.String.
		//
		// bus_id and expires_at are carried for the operator's benefit and are
		// deliberately NOT validated (see the BusID doc comment: it is a CLAIM by
		// whoever wrote the blob, cross-checked against nothing). So for these two
		// fields String's safeText is the ONLY thing standing between an
		// attacker-chosen 60 KiB of ANSI and a terminal — and String is reached on
		// an UNVALIDATED invite, because a %v in a decode path runs before
		// Validate ever does.
		//
		// Drop safeText from String and this row goes red twice over: the
		// rendering becomes ~120 KiB and it carries raw ESC and CR.
		hostile := strings.Repeat(forgingInviteID+"A", 2000)
		if len(hostile) < 60<<10 {
			t.Fatalf("the hostile fixture is only %d bytes; it must be large enough that an UNBOUNDED rendering is unmistakable", len(hostile))
		}
		inv := Invite{
			InviteID:           inviteTestID,
			BusID:              hostile,
			BusAddress:         "https://127.0.0.1:8080",
			BusCertFingerprint: strings.Repeat("ab", 32),
			InviteSecret:       inviteTestSecret,
			ExpiresAt:          hostile,
		}

		// maxDetailBytes per bounded field plus the fixed scaffolding. Generous
		// — the point is that the rendering cannot GROW with the input, not that
		// it hits an exact byte count.
		const renderCeiling = 4 * maxDetailBytes
		for label, s := range inviteRenderings(inv) {
			if len(s) > renderCeiling {
				t.Errorf("%s renders %d bytes from a %d-byte invite, want at most %d — an unbounded field can fill a scrollback",
					label, len(s), len(hostile), renderCeiling)
			}
		}
		assertNoTerminalForgery(t, "a hostile bus_id and expires_at", inviteRenderings(inv))
		assertInviteRedacted(t, inv)

		// Non-vacuity: the field IS rendered, merely bounded and neutralised. A
		// String that dropped bus_id entirely would satisfy every check above.
		if s := inv.String(); !strings.Contains(s, "[2K") {
			t.Fatalf("Invite.String() = %q, want it to still SHOW the (neutralised) bus_id — otherwise the bound above proves nothing", s)
		}
	})

	t.Run("LoadInviteFile refuses a file other local users can read", func(t *testing.T) {
		blob := inviteBlob(t, testInvite("https://127.0.0.1:8080", strings.Repeat("ab", 32)))

		// The rule the implementation applies is perm&0o077 != 0. 0700 therefore
		// PASSES — it has no group or world bits — and that is what is asserted,
		// because the property being protected is "no OTHER local user can read
		// this", not "the mode is exactly 0600".
		cases := []struct {
			mode   os.FileMode
			accept bool
		}{
			{0o600, true},
			{0o400, true},
			{0o500, true},
			{0o700, true},
			{0o640, false},
			{0o604, false},
			{0o644, false},
			{0o660, false},
			{0o666, false},
			{0o610, false},
			{0o601, false},
			{0o777, false},
		}

		for _, tc := range cases {
			tc := tc
			t.Run(fmt.Sprintf("mode %04o", tc.mode), func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "invite.json")
				if err := os.WriteFile(path, blob, 0o600); err != nil {
					t.Fatalf("writing the invite file: %v", err)
				}
				// chmod AFTER the write, so the umask cannot silently make a
				// "refused" row unreachable.
				if err := os.Chmod(path, tc.mode); err != nil {
					t.Fatalf("chmod %04o: %v", tc.mode, err)
				}
				inv, err := LoadInviteFile(path)
				if tc.accept {
					if err != nil {
						t.Fatalf("LoadInviteFile on a %04o file: %v, want it accepted", tc.mode, err)
					}
					if inv == nil || inv.InviteSecret != inviteTestSecret {
						t.Fatalf("LoadInviteFile returned %+v, want the parsed invite", inv)
					}
					return
				}
				if err == nil {
					t.Fatalf("LoadInviteFile on a %04o file = nil error; the invite holds a bearer credential another local user could read", tc.mode)
				}
				if KindOf(err) != KindConfig {
					t.Fatalf("KindOf(err) = %q, want %q", KindOf(err), KindConfig)
				}
				var e *Error
				if !errors.As(err, &e) {
					t.Fatalf("error is not a *client.Error: %v", err)
				}
				if !strings.Contains(e.Message, fmt.Sprintf("%04o", tc.mode)) {
					t.Errorf("message = %q, want it to name the mode it found", e.Message)
				}
				if !strings.Contains(e.Remedy, "chmod 0600") {
					t.Errorf("remedy = %q, want it to name the chmod", e.Remedy)
				}
				assertNoSecretInError(t, "permission refusal", err)
			})
		}

		t.Run("a directory is not an invite", func(t *testing.T) {
			dir := t.TempDir()
			_, err := LoadInviteFile(dir)
			if err == nil {
				t.Fatalf("LoadInviteFile(<a directory>) = nil error, want a refusal")
			}
			if KindOf(err) != KindConfig {
				t.Fatalf("KindOf(err) = %q, want %q", KindOf(err), KindConfig)
			}
			if !strings.Contains(err.Error(), "not a regular file") {
				t.Fatalf("error = %q, want it to say the path is not a regular file", err)
			}
		})

		t.Run("a fifo is not an invite", func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "invite.fifo")
			if err := syscall.Mkfifo(path, 0o600); err != nil {
				t.Skipf("mkfifo is unavailable here: %v", err)
			}
			// NO writer, deliberately. An earlier version of this row opened one
			// concurrently, because LoadInviteFile used os.Open and the read end
			// of a fifo blocks until a writer arrives — so the call parked
			// forever instead of reaching the refusal this row is about, and the
			// cooperating writer hid exactly the failure that mattered (an agent
			// shelling out gets no output and no timeout, invariant 7).
			// LoadInviteFile now opens O_NONBLOCK, so the refusal is reachable
			// with nothing on the other end, which is the case a hostile or
			// mistaken path actually presents.
			//
			// The call is made on a goroutine under a DEADLINE, because "it
			// returns the right error" and "it returns at all" are different
			// claims and only the second one is about the bug this row exists
			// for. Without O_NONBLOCK the open blocks FOREVER — not slowly — so
			// the deadline can be generous without being flaky: it is never
			// approached on a working implementation and never met on a broken
			// one.
			type fifoResult struct{ err error }
			done := make(chan fifoResult, 1)
			go func() {
				_, err := LoadInviteFile(path)
				done <- fifoResult{err}
			}()
			var err error
			select {
			case r := <-done:
				err = r.err
			case <-time.After(30 * time.Second):
				t.Fatalf("LoadInviteFile(<a fifo with no writer>) had not returned after 30s; an agent shelling out gets no output and no timeout (invariant 7)")
			}
			if err == nil {
				t.Fatalf("LoadInviteFile(<a fifo>) = nil error, want a refusal")
			}
			if KindOf(err) != KindConfig {
				t.Fatalf("KindOf(err) = %q, want %q", KindOf(err), KindConfig)
			}
			if !strings.Contains(err.Error(), "not a regular file") {
				t.Fatalf("error = %q, want it to say the path is not a regular file", err)
			}
		})

		t.Run("a missing file names the remedy", func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "nothing-here.json")
			_, err := LoadInviteFile(path)
			if err == nil {
				t.Fatalf("LoadInviteFile(<missing>) = nil error, want a refusal")
			}
			if KindOf(err) != KindConfig {
				t.Fatalf("KindOf(err) = %q, want %q", KindOf(err), KindConfig)
			}
			var e *Error
			if !errors.As(err, &e) {
				t.Fatalf("error is not a *client.Error: %v", err)
			}
			if !strings.Contains(e.Message, path) {
				t.Errorf("message = %q, want it to name the path that is missing", e.Message)
			}
			if !strings.Contains(e.Remedy, "--invite-file") {
				t.Errorf("remedy = %q, want it to name the flag that fixes it (invariant 7: errors name the remedy)", e.Remedy)
			}
		})
	})

	t.Run("ParseInvite bounds its input and refuses an ambiguous document", func(t *testing.T) {
		good := string(inviteBlob(t, testInvite("https://127.0.0.1:8080", strings.Repeat("ab", 32))))

		// Padding to EXACTLY one byte over the bound, after a COMPLETE JSON
		// object: a client that truncated at the limit instead of refusing
		// would parse this happily, so the row cannot pass by accident.
		oversized := good + strings.Repeat(" ", MaxInviteFileBytes+1-len(good))
		undersized := good + strings.Repeat(" ", MaxInviteFileBytes-len(good)-1)

		cases := []struct {
			name    string
			input   string
			accept  bool
			wantMsg string
		}{
			{name: "a minted blob", input: good, accept: true},
			{name: "trailing whitespace is fine", input: undersized, accept: true},
			{
				name:   "unknown keys are ignored, deliberately",
				input:  `{"ok":true,"created_at":"2026-08-14T00:00:00Z","label":"planner box","transport_insecure":false,"future_field":{"nested":[1,2,3]},` + good[1:],
				accept: true,
			},
			{name: "over the size bound", input: oversized, wantMsg: "larger than"},
			{name: "empty", input: "", wantMsg: "is empty"},
			{name: "whitespace only", input: "   \n\t ", wantMsg: "is empty"},
			{name: "two concatenated blobs", input: good + good, wantMsg: "content after the JSON object"},
			{name: "a trailing fragment", input: good + `{"invite_id":`, wantMsg: "content after the JSON object"},
			{name: "not an object at all", input: `[1,2,3]`, wantMsg: "not a"},
		}

		for _, tc := range cases {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				inv, err := ParseInvite(strings.NewReader(tc.input))
				if tc.accept {
					if err != nil {
						t.Fatalf("ParseInvite: %v, want it accepted", err)
					}
					if inv.InviteID != inviteTestID || inv.InviteSecret != inviteTestSecret {
						t.Fatalf("ParseInvite returned %v, want the minted values", inv)
					}
					return
				}
				if err == nil {
					t.Fatalf("ParseInvite = nil error, want a refusal")
				}
				if KindOf(err) != KindConfig {
					t.Fatalf("KindOf(err) = %q, want %q", KindOf(err), KindConfig)
				}
				if !strings.Contains(err.Error(), tc.wantMsg) {
					t.Fatalf("error = %q, want it to contain %q", err, tc.wantMsg)
				}
				assertNoSecretInError(t, tc.name, err)
			})
		}

		t.Run("a nil reader is an internal error, not a panic", func(t *testing.T) {
			if _, err := ParseInvite(nil); err == nil {
				t.Fatalf("ParseInvite(nil) = nil error, want a refusal")
			} else if KindOf(err) != KindInternal {
				t.Fatalf("KindOf(err) = %q, want %q", KindOf(err), KindInternal)
			}
		})
	})

	t.Run("the secret never escapes, on any path", func(t *testing.T) {
		// encoding/json's own errors QUOTE the input — a SyntaxError names the
		// offending character, an UnmarshalTypeError names the value's type —
		// so a blob that is malformed INSIDE the secret's string literal is
		// exactly the leak invalidInviteJSON exists to prevent.
		malformed := []struct {
			name  string
			input string
		}{
			{
				name:  "an invalid escape inside the secret",
				input: `{"invite_id":"` + inviteTestID + `","invite_secret":"` + inviteTestSecret + `\q"}`,
			},
			{
				name:  "an unterminated string starting with the secret",
				input: `{"invite_id":"` + inviteTestID + `","invite_secret":"` + inviteTestSecret,
			},
			{
				name:  "a control character inside the secret",
				input: `{"invite_id":"` + inviteTestID + `","invite_secret":"` + inviteTestSecret + "\x01" + `"}`,
			},
			{
				name:  "the secret in the wrong JSON type",
				input: `{"invite_id":"` + inviteTestID + `","invite_secret":{"leaked":"` + inviteTestSecret + `"},"bus_address":"https://127.0.0.1:8080"}`,
			},
			{
				name:  "the secret in an array",
				input: `{"invite_id":"` + inviteTestID + `","invite_secret":["` + inviteTestSecret + `"]}`,
			},
		}
		for _, tc := range malformed {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				_, err := ParseInvite(strings.NewReader(tc.input))
				if err == nil {
					t.Fatalf("ParseInvite = nil error, want a refusal")
				}
				assertNoSecretInError(t, tc.name, err)
			})
		}

		// And every refusal the BUS can produce. The invite is present on each,
		// so annotateInviteRefusal and enrolFailed both run over it.
		for _, status := range []int{
			http.StatusBadRequest,
			http.StatusForbidden,
			http.StatusConflict,
			http.StatusInternalServerError,
			http.StatusServiceUnavailable,
		} {
			status := status
			t.Run(fmt.Sprintf("the bus answers %d", status), func(t *testing.T) {
				rec, bus, pin := newInviteTLSBus(t)
				rec.status = status
				rec.errBody = "invite not accepted"
				inv := testInvite(bus.URL(), pin.String())
				c := inviteClient(t, t.TempDir(), nil)

				_, err := c.Enrol(context.Background(), EnrolOptions{Name: "planner", Invite: inv, Save: true})
				if err == nil {
					t.Fatalf("Enrol against a bus answering %d = nil error, want a refusal", status)
				}
				assertNoSecretInError(t, fmt.Sprintf("status %d", status), err)
				assertInviteRedacted(t, *inv)
			})
		}

		// The Invite type itself, one more time and explicitly: String and
		// GoString are the two functions that stand between a bearer credential
		// and a debugging line nobody thought about.
		inv := testInvite("https://127.0.0.1:8080", strings.Repeat("ab", 32))
		assertInviteRedacted(t, *inv)
		if !strings.Contains(inv.String(), inv.InviteID) {
			t.Errorf("Invite.String() = %q, want it to name the invite id (which is a NAME, not a credential)", inv.String())
		}
		if !strings.Contains(inv.String(), "redacted") {
			t.Errorf("Invite.String() = %q, want it to say the secret was redacted rather than silently omitting it", inv.String())
		}
		empty := Invite{}
		if got := empty.String(); strings.Contains(got, "%!") || got == "" {
			t.Errorf("Invite{}.String() = %q, want a readable rendering of an empty invite", got)
		}
	})

	t.Run("a bus refusal is classified and told not to retry", func(t *testing.T) {
		// The mapping asserted here is client/errors.go's, cross-checked against
		// the EXIT CODES block in cmd/agent-busctl/enrol.go:
		//   3 the invite file cannot be used   (KindConfig)
		//   4 the bus rejected the credential, invite included (KindAuth)
		//   6 the bus reported an error of its own (KindServer)
		//   7 the bus understood the request and refused it (KindRejected)
		cases := []struct {
			name       string
			status     int
			retryAfter string
			errBody    string
			wantKind   Kind
			wantExit   int
			wantFatal  bool
			wantMsg    []string
			wantRemedy []string
		}{
			{
				name: "403 the bus refused the invite", status: http.StatusForbidden,
				errBody: "invite not accepted", wantKind: KindAuth, wantExit: ExitAuth,
				wantMsg: []string{"refused invite " + inviteTestID, "invite not accepted"},
				// Every clause of statusError's ordinary 401/403 remedy is wrong
				// here: there is no session, and retrying a single-use invite
				// cannot help.
				wantRemedy: []string{"single-use", "Retrying will not change it", "fresh invite"},
			},
			{
				name: "409 an idempotency conflict", status: http.StatusConflict,
				errBody:  "idempotency key reused with different content",
				wantKind: KindRejected, wantExit: ExitRejected,
				wantMsg: []string{"the bus refused the request"},
			},
			{
				name: "503 with no Retry-After is not transient", status: http.StatusServiceUnavailable,
				errBody:  "the write path is poisoned",
				wantKind: KindServer, wantExit: ExitServer, wantFatal: true,
				wantMsg:    []string{"cannot durably accept"},
				wantRemedy: []string{"invariant 4"},
			},
			{
				name: "503 with Retry-After is capacity", status: http.StatusServiceUnavailable,
				retryAfter: "1", errBody: "at capacity",
				wantKind: KindServer, wantExit: ExitServer,
				wantMsg: []string{"at capacity"},
			},
		}

		for _, tc := range cases {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				rec, bus, pin := newInviteTLSBus(t)
				rec.status, rec.errBody, rec.retryAfter = tc.status, tc.errBody, tc.retryAfter
				inv := testInvite(bus.URL(), pin.String())
				c := inviteClient(t, t.TempDir(), nil)

				_, err := c.Enrol(context.Background(), EnrolOptions{Name: "planner", Invite: inv, Save: true})
				if err == nil {
					t.Fatalf("Enrol = nil error, want the %d surfaced", tc.status)
				}
				if KindOf(err) != tc.wantKind {
					t.Fatalf("KindOf(err) = %q, want %q", KindOf(err), tc.wantKind)
				}
				if got := ExitCode(err); got != tc.wantExit {
					t.Fatalf("ExitCode(err) = %d, want %d (the code cmd/agent-busctl/enrol.go documents)", got, tc.wantExit)
				}
				if got := IsFatalUnavailable(err); got != tc.wantFatal {
					t.Fatalf("IsFatalUnavailable = %v, want %v", got, tc.wantFatal)
				}
				var e *Error
				if !errors.As(err, &e) {
					t.Fatalf("error is not a *client.Error: %v", err)
				}
				if e.Status != tc.status {
					t.Errorf("Status = %d, want %d", e.Status, tc.status)
				}
				for _, want := range tc.wantMsg {
					if !strings.Contains(e.Message, want) {
						t.Errorf("message = %q, want it to contain %q", e.Message, want)
					}
				}
				for _, want := range tc.wantRemedy {
					if !strings.Contains(e.Remedy, want) {
						t.Errorf("remedy = %q, want it to contain %q", e.Remedy, want)
					}
				}
				if tc.status == http.StatusForbidden && strings.Contains(e.Remedy, "re-enrol with") {
					t.Errorf("remedy = %q still carries statusError's session wording; enrolment has no session and retrying a spent invite cannot help", e.Remedy)
				}
				assertNoSecretInError(t, tc.name, err)
			})
		}
	})

	t.Run("a replayed redemption is a retry, not a second spend", func(t *testing.T) {
		// Invariant 10: same key + SAME payload is a legitimate retry, answered
		// from the bus's idempotency table. The invite secret is deliberately
		// absent from the server's fingerprint, so this must NOT look like a
		// second spend of a single-use invite.
		rec, bus, pin := newInviteTLSBus(t)
		rec.replayed = true
		inv := testInvite(bus.URL(), pin.String())
		dir := t.TempDir()
		c := inviteClient(t, dir, nil)

		res, err := c.Enrol(context.Background(), EnrolOptions{
			Name: "planner", Invite: inv, Save: true, MakeCurrent: true, IdempotencyKey: "invite-replay-key",
		})
		if err != nil {
			t.Fatalf("Enrol: %v", err)
		}
		if !res.Replayed {
			t.Fatalf("Replayed = false, want true when the bus sets %s: true", idempotencyReplayedHeader)
		}
		if res.InviteID != inviteTestID {
			t.Errorf("InviteID = %q on a replay, want %q", res.InviteID, inviteTestID)
		}
		if rec.calls() != 1 {
			t.Fatalf("the bus saw %d enrol requests, want 1", rec.calls())
		}
		ids, _, err := c.Identities()
		if err != nil {
			t.Fatalf("Identities: %v", err)
		}
		if len(ids) != 1 {
			t.Fatalf("the store holds %d identities after a replayed redemption, want 1 — a replay must not create a second enrolment", len(ids))
		}
	})
}
