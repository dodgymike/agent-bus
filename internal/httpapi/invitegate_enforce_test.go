package httpapi_test

// INVITE-GATE-ENFORCE, at the HTTP layer: the status code, the body, the log,
// and the advertised bit that has to agree with all three.
//
// internal/auth/invitegate_enforce_test.go covers the refusal as POLICY. This
// file covers what an agent on the wire actually experiences, which is a
// separate claim: auth.ErrInviteRequired reaching writeAuthError's `default`
// arm would answer 500 "internal error" for a deliberate policy refusal, and
// every policy-level test would still pass.
//
// Invariants exercised: 3 (invite-only enrolment; enrolment is one of the three
// routes that necessarily cannot authenticate, so the refusal must not depend on
// a credential), 10 (the refusal does NOT disconnect, and a legitimate retry of
// an INVITED enrolment still replays its original 201 rather than being refused).

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/dodgymike/agent-bus/internal/httpapi"
)

// newIGBusGated is newIGBus with invite-only enrolment ON, wired the way
// cmd/agent-bus wires it: the gate AND an invite store, never one without the
// other.
func newIGBusGated(t *testing.T) *igBus {
	t.Helper()
	return newIGBusOpts(t, true, true)
}

// igGatedEnrolBody is a well-formed enrolment carrying NO invite — the exact
// wire shape a pre-gate client, or an anonymous attacker, produces.
func igGatedEnrolBody(name, key string) string {
	pub := base64.StdEncoding.EncodeToString(make([]byte, 32))
	return igEnrolBody(name, pub, key, "", "")
}

// TestInviteGateEnrolWithoutAnInviteIsRefused403 is the wire-level closure of
// the roster-exhaustion DoS.
//
// 403 specifically, and each rejected alternative is a real failure mode:
// 500 (writeAuthError's default arm) would report a deliberate refusal as a
// server fault and tell the caller nothing; 503 (ErrCapacity's arm) carries
// Retry-After and would invite a client to hammer a route that will never accept
// it; 401 would promise a credential exists that would work on this request.
func TestInviteGateEnrolWithoutAnInviteIsRefused403(t *testing.T) {
	b := newIGBusGated(t)

	rec := postJSON(t, b.srv, httpapi.RouteEnroll, igGatedEnrolBody("anon", "gate-403-1"))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST %s with no invite answered %d, want %d.\n"+
			"500 in particular means auth.ErrInviteRequired fell through to writeAuthError's default arm and a policy refusal is being reported as a server fault.\nbody: %s",
			httpapi.RouteEnroll, rec.Code, http.StatusForbidden, rec.Body.String())
	}

	// The body NAMES THE REMEDY. This bus's client story is an agent reading a
	// message and acting on it (invariant 7), so a bare "forbidden" leaves an
	// agent unable to tell "blocked" from "misconfigured".
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("the refusal body is not JSON: %v (%s)", err, rec.Body.String())
	}
	if !strings.Contains(body.Error, "invite") {
		t.Errorf("the refusal says %q, which does not mention an invite; an agent cannot act on it", body.Error)
	}

	// NO SECRET AND NO INTERNAL DETAIL leaks into the refusal.
	if strings.Contains(strings.ToLower(body.Error), "internal") {
		t.Errorf("the refusal says %q, which reports a policy decision as an internal error", body.Error)
	}

	// NOTHING WAS CREATED. The roster is the resource under attack.
	if got := b.roster.Len(); got != 0 {
		t.Fatalf("the roster holds %d entries after a refused enrolment, want 0", got)
	}
}

// TestInviteGateRefusalDoesNotDisconnect is invariant 10's two questions,
// asserted rather than argued.
//
// A merely BUGGY client reaches this line constantly — every agent built against
// the pre-gate bus does, on its first call after the gate is turned on — and
// /v1/enroll is unauthenticated, so the socket identifies no principal to
// punish. Dropping it would destroy every other request pipelined there.
func TestInviteGateRefusalDoesNotDisconnect(t *testing.T) {
	b := newIGBusGated(t)

	rec := postJSON(t, b.srv, httpapi.RouteEnroll, igGatedEnrolBody("anon", "gate-nodisconnect-1"))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST %s answered %d, want %d", httpapi.RouteEnroll, rec.Code, http.StatusForbidden)
	}
	if got := rec.Header().Get("Connection"); strings.EqualFold(got, "close") {
		t.Fatalf("the invite-required refusal set Connection: %q.\n"+
			"Invariant 10: only replay of an already-accepted SIGNED message disconnects. A buggy client reaches this line trivially and this route is unauthenticated, so the socket names no principal to punish.", got)
	}
}

// TestInviteGateEnrolWithAnInviteStillSucceedsWhenGated is what stops the fix
// from being "refuse everything": the gate must not break the path it exists to
// mandate.
func TestInviteGateEnrolWithAnInviteStillSucceedsWhenGated(t *testing.T) {
	b := newIGBusGated(t)
	m := b.mint(t, "gated-admit")

	pub := base64.StdEncoding.EncodeToString(make([]byte, 32))
	rec := postJSON(t, b.srv, httpapi.RouteEnroll, igEnrolBody("welcome", pub, "gate-admit-1", m.ID, m.Secret))

	if rec.Code != http.StatusCreated {
		t.Fatalf("POST %s WITH a valid invite answered %d, want %d; the gate must admit an invited enrolment.\nbody: %s",
			httpapi.RouteEnroll, rec.Code, http.StatusCreated, rec.Body.String())
	}
	if got := b.roster.Len(); got != 1 {
		t.Fatalf("the roster holds %d entries after an admitted enrolment, want 1", got)
	}
}

// TestInviteGateAdvertisesInviteRequired pins that the discovery document
// (GET /v1/discovery) tells the truth.
//
// This is the defect that made INVITE-GATE look finished: `invite_required` was
// a hard-coded false sitting beside an Enrol that did not enforce, and neither
// half was wrong on its own. Now the field is read from the enforcing layer, so
// an agent that trusts the document and an agent that trusts the 403 reach the
// same conclusion.
func TestInviteGateAdvertisesInviteRequired(t *testing.T) {
	for _, tc := range []struct {
		label string
		gated bool
		want  bool
	}{
		{"gated bus advertises invite_required true", true, true},
		{"ungated bus advertises invite_required false", false, false},
	} {
		t.Run(tc.label, func(t *testing.T) {
			b := newIGBusOpts(t, true, tc.gated)

			rec := getDiscovery(t, b.srv)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s answered %d, want 200", httpapi.RouteDiscovery, rec.Code)
			}

			var doc struct {
				Enrolment struct {
					InviteRequired bool   `json:"invite_required"`
					InviteNote     string `json:"invite_note"`
				} `json:"enrolment"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
				t.Fatalf("decoding the discovery document: %v", err)
			}

			if doc.Enrolment.InviteRequired != tc.want {
				t.Fatalf("enrolment.invite_required = %v, want %v; the advertised bit must be READ from auth.Service.InviteRequired(), never hard-coded",
					doc.Enrolment.InviteRequired, tc.want)
			}

			// The PROSE has to agree with the boolean. A document whose flag says
			// "required" beside a note saying enrolment is "OPEN" is worse than
			// either alone: an agent reads the sentence, not the field.
			note := doc.Enrolment.InviteNote
			if tc.want {
				if !strings.Contains(note, "INVITE-ONLY") {
					t.Errorf("the gated bus's invite_note does not say INVITE-ONLY: %q", note)
				}
				if strings.Contains(note, "currently OPEN") {
					t.Errorf("the gated bus's invite_note still says enrolment is OPEN: %q", note)
				}
				// It must tell the operator the awkward part, because it is not
				// discoverable and it is the thing that blocks onboarding.
				if !strings.Contains(note, "STOPPED") {
					t.Errorf("the gated bus's invite_note does not warn that minting requires the bus to be STOPPED: %q", note)
				}
			} else if strings.Contains(note, "INVITE-ONLY") {
				t.Errorf("the UNGATED bus's invite_note claims INVITE-ONLY: %q", note)
			}
		})
	}
}

// TestInviteGateDiscoveryDoesNotDenyClientCertificates guards the limitation
// text that is SERVED TO ANONYMOUS CALLERS.
//
// Limitation 1 told every reader "the bus does NOT request or require a CLIENT
// certificate", which stopped being true at MTLS-CLIENTAUTH
// (cmd/agent-bus/tlslisten.go sets tls.RequestClientCert) and became doubly
// untrue at MTLS-CROSSCHECK. Understating a protection in an API response is
// not a stale comment: it is the bus telling a reader the transport is weaker
// than it is, in the reassuring direction, on the document a client reads before
// deciding what to trust.
//
// The assertions are deliberately about the FALSE CLAIM and about the LIMIT of
// the true one, not about exact wording.
func TestInviteGateDiscoveryDoesNotDenyClientCertificates(t *testing.T) {
	b := newIGBusGated(t)

	rec := getDiscovery(t, b.srv)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s answered %d, want 200", httpapi.RouteDiscovery, rec.Code)
	}
	var doc struct {
		Limitations []string `json:"limitations"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decoding the discovery document: %v", err)
	}
	if len(doc.Limitations) == 0 {
		t.Fatal("the discovery document carries no limitations; this guard would pass vacuously")
	}
	all := strings.Join(doc.Limitations, "\n")

	// THE FALSE CLAIM, in the two spellings the retired text used.
	for _, banned := range []string{
		"does NOT request or require a CLIENT certificate",
		"Client-certificate authentication is being built and is not here yet",
	} {
		if strings.Contains(all, banned) {
			t.Errorf("the limitations still tell anonymous callers %q.\n"+
				"cmd/agent-bus/tlslisten.go sets ClientAuth: tls.RequestClientCert, and a bound agent's session token is refused 403 unless it arrives over the bound certificate.", banned)
		}
	}

	// AND THE LIMIT OF THE TRUE CLAIM MUST SURVIVE. The protection covers agents
	// that HAVE a binding; an agent that enrolled without a client certificate
	// still holds a pure bearer token. Over-claiming here would do the same
	// damage in the opposite direction, which is the failure mode a limitations
	// array is most prone to.
	if !strings.Contains(all, "bearer token") {
		t.Error("the limitations no longer warn that an agent with NO certificate binding holds a pure bearer token; the cross-check protects BOUND agents only and the document must not imply otherwise")
	}
}

// TestInviteGateRefusalDoesNotEchoTheRequestedNameIntoTheLog is the security
// gate's M1: POST /v1/enroll is unauthenticated and this server rate-limits
// nothing, so every field a refusal echoes into a log line is a write primitive
// whose contents AND volume an anonymous caller controls.
//
// The gate made this matter rather than creating it: on an invite-only bus the
// refusal is the COMMON path, so a raw name would put attacker-chosen bytes into
// the operator's log on every anonymous attempt. The name is bounded only by the
// 8 KiB request-body limit.
//
// The rule is the one inviteIDLogFields already applies to an invite id: a name
// that passes ids.ValidateAgentName is logged, anything else contributes its
// LENGTH only.
func TestInviteGateRefusalDoesNotEchoTheRequestedNameIntoTheLog(t *testing.T) {
	b := newIGBusGated(t)

	// Not merely long: it carries bytes that would forge a second log record if
	// they reached a logfmt line raw. A length check alone would not catch this.
	evil := `zzz" level=error msg="forged log line" injected=` + strings.Repeat("A", 300)

	rec := postJSON(t, b.srv, httpapi.RouteEnroll, igGatedEnrolBody(evil, "gate-logsafety-1"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST %s answered %d, want %d", httpapi.RouteEnroll, rec.Code, http.StatusForbidden)
	}

	logged := b.log.String()
	if strings.Contains(logged, "forged log line") {
		t.Fatalf("the refusal echoed the client-supplied name into the log verbatim.\n"+
			"/v1/enroll is unauthenticated and unrate-limited, so this is an anonymous log-write primitive; log name_len instead, exactly as inviteIDLogFields does for an invite id.\nlog:\n%s", logged)
	}
	if strings.Contains(logged, strings.Repeat("A", 100)) {
		t.Fatalf("the refusal echoed a %d-byte client-supplied name into the log.\nlog:\n%s", len(evil), logged)
	}
	// The operator is not left blind: the LENGTH is still reported, so a flood of
	// malformed names is still visible as such.
	if !strings.Contains(logged, "name_len") {
		t.Errorf("the refusal logged neither the name nor a name_len; an operator cannot see that a malformed enrolment arrived at all.\nlog:\n%s", logged)
	}

	// And a WELL-FORMED name is still logged in full -- the redaction must not
	// blind the operator in the ordinary case, which is the whole reason this is
	// a validity check and not a blanket length field.
	rec2 := postJSON(t, b.srv, httpapi.RouteEnroll, igGatedEnrolBody("planner", "gate-logsafety-2"))
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("POST %s answered %d, want %d", httpapi.RouteEnroll, rec2.Code, http.StatusForbidden)
	}
	if !strings.Contains(b.log.String(), "planner") {
		t.Errorf("a refusal of the well-formed name \"planner\" did not record it; a valid name is at most 64 bytes from a safe alphabet and is what an operator correlates on")
	}
}

// TestInviteGateUnenrollableCombinationIsReportedLoudly covers the one branch
// this change added that no in-tree wiring can reach (reviewer, N3).
//
// A service that REQUIRES an invite on a server built with NO invite store can
// never enrol anybody: handleEnroll answers 501 to a presented invite (there is
// nothing to redeem it against) and Enrol refuses every request presenting none.
// Both halves are individually correct; the combination is a dead bus.
//
// cmd/agent-bus wires both unconditionally, so this is reachable only by an
// embedder — which is precisely why it must be TESTED rather than trusted: there
// is no other path on which it would ever be observed before a deployment sat
// there refusing every enrolment.
func TestInviteGateUnenrollableCombinationIsReportedLoudly(t *testing.T) {
	// The gate ON, the invite store ABSENT.
	b := newIGBusOpts(t, false, true)

	logged := b.log.String()
	if !strings.Contains(logged, "CANNOT ENROL ANYBODY") {
		t.Fatalf("building a server whose auth service requires an invite, with NO invite store, logged no warning.\n"+
			"This combination refuses every enrolment either way (501 with an invite, 403 without), and an operator must be told at STARTUP rather than by watching enrolments fail.\nlog:\n%s", logged)
	}
	if !strings.Contains(logged, "level=error") {
		t.Errorf("the unenrollable-combination line is not at ERROR; a bus that can enrol nobody is not routine news.\nlog:\n%s", logged)
	}

	// And it is not merely a log line: the combination really is unenrollable,
	// both ways round. Without an invite: 403.
	rec := postJSON(t, b.srv, httpapi.RouteEnroll, igGatedEnrolBody("nobody", "gate-unenrollable-1"))
	if rec.Code != http.StatusForbidden {
		t.Errorf("enrol with no invite on the unenrollable combination answered %d, want %d", rec.Code, http.StatusForbidden)
	}
	// With one: 501, because there is no store to redeem it against.
	pub := base64.StdEncoding.EncodeToString(make([]byte, 32))
	rec2 := postJSON(t, b.srv, httpapi.RouteEnroll, igEnrolBody("nobody", pub, "gate-unenrollable-2", "inv-whatever", "secret"))
	if rec2.Code != http.StatusNotImplemented {
		t.Errorf("enrol WITH an invite on the unenrollable combination answered %d, want %d", rec2.Code, http.StatusNotImplemented)
	}
}
