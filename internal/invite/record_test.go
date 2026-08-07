package invite_test

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/idem"
	"github.com/dodgymike/agent-bus/internal/invite"
)

// ---------------------------------------------------------------------------
// The record: what it accepts, and — mostly — what it refuses
// ---------------------------------------------------------------------------

// baseRecordJSON builds a MINIMAL VALID record of the given state as a map, so
// each table case below can express itself as a one-field delta from something
// that is known to decode.
//
// The map form matters: DecodeRecord uses DisallowUnknownFields, so a case that
// adds a key is testing the strict decoder and a case that deletes one is
// testing validate. Both are only meaningful if the base itself is accepted,
// which TestInviteRecordBaseFixturesAreValid pins separately.
func baseRecordJSON(state string) map[string]interface{} {
	m := map[string]interface{}{
		"id":            "inv-aaaaaaaaaaaaaaaa",
		"bus":           testBusID,
		"secret_sha256": hexDigest("the record fixture secret"),
		"label":         "a fixture",
		"created_at":    "2026-08-07T12:00:00Z",
		"expires_at":    "2026-08-08T12:00:00Z",
		"state":         state,
	}
	switch state {
	case "redeemed":
		m["redeemed_at"] = "2026-08-07T12:05:00Z"
		m["redeemed_by"] = testAgentID
		m["redeem_key"] = testKey
		m["redeem_fp"] = hexFingerprint("payload")
		m["result"] = testResult
		m["cert_sha256"] = hex.EncodeToString(testCertFingerprint[:])
	case "revoked":
		m["revoked_at"] = "2026-08-07T12:05:00Z"
		m["revoked_reason"] = "withdrawn"
	}
	return m
}

func hexFingerprint(payload string) string {
	f := fingerprintOf(payload)
	return hex.EncodeToString(f[:])
}

// renderRecordJSON applies overrides to a base and marshals it. An override
// whose value is nil DELETES the key, which is how the "missing field" cases are
// expressed.
func renderRecordJSON(t *testing.T, state string, overrides map[string]interface{}) []byte {
	t.Helper()
	m := baseRecordJSON(state)
	for k, v := range overrides {
		if v == nil {
			delete(m, k)
			continue
		}
		m[k] = v
	}
	return mustJSON(t, m)
}

// TestInviteRecordBaseFixturesAreValid is the control for the table below.
//
// Without it, a base fixture that had itself become invalid would make EVERY
// malformed case pass for the wrong reason — the table would be asserting that
// a broken record is rejected while proving nothing about the specific defect
// each case names.
func TestInviteRecordBaseFixturesAreValid(t *testing.T) {
	for _, state := range []string{"open", "redeemed", "revoked"} {
		state := state
		t.Run(state, func(t *testing.T) {
			body := renderRecordJSON(t, state, nil)
			rec, err := invite.DecodeRecord(body)
			if err != nil {
				t.Fatalf("the %s base fixture does not decode: %v\n%s", state, err, body)
			}
			if rec.State.String() != state {
				t.Fatalf("the %s base fixture decoded as %s", state, rec.State)
			}
			// It also has to survive the round trip the store performs on every
			// live write (Store.canonical): encode, decode, and hold the same
			// record — that is what makes a live Apply and a replayed Apply
			// identical.
			again, err := rec.Encode()
			if err != nil {
				t.Fatalf("re-encoding the %s fixture: %v", state, err)
			}
			back, err := invite.DecodeRecord(again)
			if err != nil {
				t.Fatalf("the re-encoded %s fixture does not decode: %v", state, err)
			}
			assertRecordEqual(t, "the "+state+" fixture after a round trip", back, rec)
			if third, err := back.Encode(); err != nil || !bytes.Equal(third, again) {
				t.Fatalf("encoding is not idempotent for the %s fixture: (%s, %v) != %s", state, third, err, again)
			}
		})
	}
}

// TestInviteMalformedRecordIsRejected is the rejection table for a record read
// back off disk.
//
// A record off the log is UNTRUSTED INPUT even though this server wrote it,
// because "this server wrote it" is exactly the claim corruption disproves. The
// worst failure a lenient decoder produces is not a crash: it is a SPENT invite
// reinstated as an open one, which is a second admission to the bus.
//
// Every case also asserts the oversized-value discipline where it applies — an
// attacker must not get to choose a multiple of its own input back out of an
// operator's log.
func TestInviteMalformedRecordIsRejected(t *testing.T) {
	longID := "inv-" + strings.Repeat("a", invite.MaxInviteIDLen)
	longBus := strings.Repeat("B", invite.MaxBusIDLen+1)
	longLabel := strings.Repeat("L", invite.MaxLabelLen+1)
	longReason := strings.Repeat("R", invite.MaxReasonLen+1)
	longAgent := strings.Repeat("g", idem.MaxAgentLen+1)
	longKey := strings.Repeat("k", idem.MaxKeyLen+1)
	longResult := json.RawMessage(`"` + strings.Repeat("x", idem.MaxResultBytes) + `"`)
	zeroDigestHex := strings.Repeat("0", 2*invite.DigestSize)
	nonZeroFP := hexFingerprint("some payload")

	cases := []struct {
		name string
		// raw is used verbatim when non-empty; otherwise state+overrides render
		// the body.
		raw       string
		state     string
		overrides map[string]interface{}
		want      error
		// notEchoed, when non-empty, must NOT appear in the error text.
		notEchoed string
	}{
		// --- the decoder itself -------------------------------------------
		{name: "not JSON at all", raw: `this is not json`, want: invite.ErrInvalidRecord},
		{name: "a truncated object", raw: `{"id":"inv-aaaaaaaaaaaaaaaa",`, want: invite.ErrInvalidRecord},
		{name: "an empty body", raw: ``, want: invite.ErrInvalidRecord},
		{name: "a JSON array", raw: `[]`, want: invite.ErrInvalidRecord},
		{name: "a JSON null", raw: `null`, want: invite.ErrInvalidRecord},
		{name: "an unknown field", state: "open", overrides: map[string]interface{}{"surprise": 1}, want: invite.ErrInvalidRecord},
		{name: "an unknown field that looks like a future one", state: "open", overrides: map[string]interface{}{"revoked_by": "someone"}, want: invite.ErrInvalidRecord},

		// --- the identity ---------------------------------------------------
		{name: "a missing id", state: "open", overrides: map[string]interface{}{"id": nil}, want: invite.ErrInvalidRecord},
		{name: "an id with no prefix", state: "open", overrides: map[string]interface{}{"id": "aaaaaaaaaaaaaaaa"}, want: invite.ErrInvalidRecord},
		{name: "an id with an uppercase character", state: "open", overrides: map[string]interface{}{"id": "inv-AAAAAAAAAAAAAAAA"}, want: invite.ErrInvalidRecord},
		{name: "an id carrying the qualifier separator", state: "open", overrides: map[string]interface{}{"id": "inv-aaaaaaaa.aaaaaaa"}, want: invite.ErrInvalidRecord},
		{name: "an id shorter than the pattern allows", state: "open", overrides: map[string]interface{}{"id": "inv-aaaa"}, want: invite.ErrInvalidRecord},
		{name: "an oversized id", state: "open", overrides: map[string]interface{}{"id": longID}, want: invite.ErrInvalidRecord, notEchoed: longID},
		{name: "a missing bus", state: "open", overrides: map[string]interface{}{"bus": nil}, want: invite.ErrInvalidRecord},
		{name: "an oversized bus", state: "open", overrides: map[string]interface{}{"bus": longBus}, want: invite.ErrInvalidRecord, notEchoed: longBus},

		// --- the secret digest ----------------------------------------------
		{name: "an all-zero secret digest", state: "open", overrides: map[string]interface{}{"secret_sha256": zeroDigestHex}, want: invite.ErrInvalidRecord},
		{name: "a missing secret digest", state: "open", overrides: map[string]interface{}{"secret_sha256": nil}, want: invite.ErrInvalidRecord},
		{name: "a secret digest that is not hex", state: "open", overrides: map[string]interface{}{"secret_sha256": strings.Repeat("z", 2*invite.DigestSize)}, want: invite.ErrInvalidRecord},
		{name: "a short secret digest", state: "open", overrides: map[string]interface{}{"secret_sha256": hex.EncodeToString([]byte("sixteen bytes!!!"))}, want: invite.ErrInvalidRecord},

		// --- the window -----------------------------------------------------
		{name: "created_at is not RFC3339Nano", state: "open", overrides: map[string]interface{}{"created_at": "yesterday afternoon"}, want: invite.ErrInvalidRecord},
		{name: "a missing expires_at", state: "open", overrides: map[string]interface{}{"expires_at": nil}, want: invite.ErrInvalidRecord},
		{name: "expires_at equal to created_at", state: "open", overrides: map[string]interface{}{"expires_at": "2026-08-07T12:00:00Z"}, want: invite.ErrInvalidRecord},
		{name: "expires_at before created_at", state: "open", overrides: map[string]interface{}{"expires_at": "2026-08-06T12:00:00Z"}, want: invite.ErrInvalidRecord},

		// --- the state ------------------------------------------------------
		{name: "a missing state", state: "open", overrides: map[string]interface{}{"state": nil}, want: invite.ErrInvalidRecord},
		{name: "an empty state", state: "open", overrides: map[string]interface{}{"state": ""}, want: invite.ErrInvalidRecord},
		{name: "a state that is not one of the three", state: "open", overrides: map[string]interface{}{"state": "expired"}, want: invite.ErrInvalidRecord},
		{name: "a numeric-looking state", state: "open", overrides: map[string]interface{}{"state": "2"}, want: invite.ErrInvalidRecord},

		// --- an OPEN record carrying terminal fields ------------------------
		{name: "open with redeemed_at", state: "open", overrides: map[string]interface{}{"redeemed_at": "2026-08-07T12:05:00Z"}, want: invite.ErrInvalidRecord},
		{name: "open with redeemed_by", state: "open", overrides: map[string]interface{}{"redeemed_by": testAgentID}, want: invite.ErrInvalidRecord},
		{name: "open with redeem_key", state: "open", overrides: map[string]interface{}{"redeem_key": testKey}, want: invite.ErrInvalidRecord},
		{name: "open with a redeem fingerprint", state: "open", overrides: map[string]interface{}{"redeem_fp": nonZeroFP}, want: invite.ErrInvalidRecord},
		{name: "open with a stored result", state: "open", overrides: map[string]interface{}{"result": testResult}, want: invite.ErrInvalidRecord},
		{name: "open with a certificate fingerprint", state: "open", overrides: map[string]interface{}{"cert_sha256": hex.EncodeToString(testCertFingerprint[:])}, want: invite.ErrInvalidRecord},
		{name: "open with revoked_at", state: "open", overrides: map[string]interface{}{"revoked_at": "2026-08-07T12:05:00Z"}, want: invite.ErrInvalidRecord},
		{name: "open with revoked_reason", state: "open", overrides: map[string]interface{}{"revoked_reason": "withdrawn"}, want: invite.ErrInvalidRecord},

		// --- a REDEEMED record that is not self-consistent ------------------
		{name: "redeemed without redeemed_at", state: "redeemed", overrides: map[string]interface{}{"redeemed_at": nil}, want: invite.ErrInvalidRecord},
		{name: "redeemed without redeemed_by", state: "redeemed", overrides: map[string]interface{}{"redeemed_by": nil}, want: invite.ErrInvalidRecord},
		{name: "redeemed without a redeem_key", state: "redeemed", overrides: map[string]interface{}{"redeem_key": nil}, want: invite.ErrInvalidRecord},
		{name: "redeemed with a redeem_key outside the charset", state: "redeemed", overrides: map[string]interface{}{"redeem_key": "a key with spaces"}, want: invite.ErrInvalidRecord},
		{name: "redeemed with an oversized redeemed_by", state: "redeemed", overrides: map[string]interface{}{"redeemed_by": longAgent}, want: invite.ErrInvalidRecord, notEchoed: longAgent},
		{name: "redeemed with an oversized redeem_key", state: "redeemed", overrides: map[string]interface{}{"redeem_key": longKey}, want: invite.ErrInvalidRecord, notEchoed: longKey},
		{name: "redeemed with an oversized result", state: "redeemed", overrides: map[string]interface{}{"result": longResult}, want: invite.ErrResultTooLarge, notEchoed: strings.Repeat("x", idem.MaxResultBytes)},
		{name: "redeemed with a redeem fingerprint that is not hex", state: "redeemed", overrides: map[string]interface{}{"redeem_fp": strings.Repeat("q", 2*idem.FingerprintSize)}, want: invite.ErrInvalidRecord},
		{name: "redeemed with a short certificate fingerprint", state: "redeemed", overrides: map[string]interface{}{"cert_sha256": hex.EncodeToString([]byte("short"))}, want: invite.ErrInvalidRecord},
		{name: "redeemed also carrying revoked_at", state: "redeemed", overrides: map[string]interface{}{"revoked_at": "2026-08-07T12:06:00Z"}, want: invite.ErrInvalidRecord},
		{name: "redeemed also carrying revoked_reason", state: "redeemed", overrides: map[string]interface{}{"revoked_reason": "withdrawn"}, want: invite.ErrInvalidRecord},

		// --- a REVOKED record that is not self-consistent -------------------
		{name: "revoked without revoked_at", state: "revoked", overrides: map[string]interface{}{"revoked_at": nil}, want: invite.ErrInvalidRecord},
		{name: "revoked with an oversized reason", state: "revoked", overrides: map[string]interface{}{"revoked_reason": longReason}, want: invite.ErrInvalidRecord, notEchoed: longReason},
		{name: "revoked also carrying redeemed_by", state: "revoked", overrides: map[string]interface{}{"redeemed_by": testAgentID}, want: invite.ErrInvalidRecord},
		{name: "revoked also carrying a stored result", state: "revoked", overrides: map[string]interface{}{"result": testResult}, want: invite.ErrInvalidRecord},
		{name: "revoked also carrying a certificate fingerprint", state: "revoked", overrides: map[string]interface{}{"cert_sha256": hex.EncodeToString(testCertFingerprint[:])}, want: invite.ErrInvalidRecord},
		{name: "an oversized label", state: "revoked", overrides: map[string]interface{}{"label": longLabel}, want: invite.ErrInvalidRecord, notEchoed: longLabel},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			var body []byte
			if tc.state == "" {
				body = []byte(tc.raw)
			} else {
				body = renderRecordJSON(t, tc.state, tc.overrides)
			}
			rec, err := invite.DecodeRecord(body)
			if !errors.Is(err, tc.want) {
				t.Fatalf("DecodeRecord returned (%+v, %v), want %v\n--- body ---\n%s", rec, err, tc.want, body)
			}
			if rec.ID != "" || rec.State != 0 {
				t.Errorf("DecodeRecord returned a partially-populated record alongside its error: %+v", rec)
			}
			if tc.notEchoed != "" && strings.Contains(err.Error(), tc.notEchoed) {
				t.Errorf("the oversized value was echoed back in the error; untrusted input must not be quoted into an operator's log")
			}
			if len(err.Error()) > 4096 {
				t.Errorf("the error string is %d bytes; an operator's log must not be sizeable by whoever wrote the damaged bytes", len(err.Error()))
			}
		})
	}
}

// TestInviteMalformedRecordIsRejectedOnTheWayOut is the OTHER direction, and it
// is the one that keeps a bad record from ever becoming durable.
//
// validate runs before the write so that a record that cannot be stored fails
// the operation with NOTHING written, rather than being discovered at replay —
// when the effect is already durable and every remaining option is bad.
func TestInviteMalformedRecordIsRejectedOnTheWayOut(t *testing.T) {
	base := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	valid := invite.Record{
		ID:           "inv-aaaaaaaaaaaaaaaa",
		BusID:        testBusID,
		SecretDigest: invite.HashSecret("out-bound fixture"),
		CreatedAt:    base,
		ExpiresAt:    base.Add(time.Hour),
		State:        invite.StateOpen,
	}
	if _, err := valid.Encode(); err != nil {
		t.Fatalf("the valid outbound fixture does not encode: %v; every case below would then pass for the wrong reason", err)
	}

	mutate := func(f func(*invite.Record)) invite.Record {
		r := valid
		f(&r)
		return r
	}

	cases := []struct {
		name string
		rec  invite.Record
		want error
	}{
		{"an invalid id", mutate(func(r *invite.Record) { r.ID = "nope" }), invite.ErrInvalidRecord},
		{"an empty bus", mutate(func(r *invite.Record) { r.BusID = "" }), invite.ErrInvalidRecord},
		{"an all-zero secret digest", mutate(func(r *invite.Record) { r.SecretDigest = [invite.DigestSize]byte{} }), invite.ErrInvalidRecord},
		{"a zero created_at", mutate(func(r *invite.Record) { r.CreatedAt = time.Time{} }), invite.ErrInvalidRecord},
		{"a zero expires_at", mutate(func(r *invite.Record) { r.ExpiresAt = time.Time{} }), invite.ErrInvalidRecord},
		{"a window that never opens", mutate(func(r *invite.Record) { r.ExpiresAt = r.CreatedAt }), invite.ErrInvalidRecord},
		{"a state outside the enum", mutate(func(r *invite.Record) { r.State = invite.State(9) }), invite.ErrInvalidRecord},
		{"a zero state", mutate(func(r *invite.Record) { r.State = 0 }), invite.ErrInvalidRecord},
		{"an oversized label", mutate(func(r *invite.Record) { r.Label = strings.Repeat("L", invite.MaxLabelLen+1) }), invite.ErrInvalidRecord},
		{"an open record carrying a redemption", mutate(func(r *invite.Record) { r.RedeemedBy = testAgentID }), invite.ErrInvalidRecord},
		{"an open record carrying a revocation", mutate(func(r *invite.Record) { r.RevokedAt = base }), invite.ErrInvalidRecord},
		// Each of these mutates ONE field away from an otherwise-complete
		// redeemed record, which is why every case below sets RedeemFingerprint:
		// a redeemed record must carry one (validate refuses an all-zero
		// fingerprint, because Begin's triage would read it as a legitimate
		// retry from a request carrying no fingerprint), so omitting it would
		// make every case here pass for THAT reason instead of the one named.
		{"a redeemed record with no redeemed_by", mutate(func(r *invite.Record) {
			r.State = invite.StateRedeemed
			r.RedeemedAt = base
			r.RedeemKey = testKey
			r.RedeemFingerprint = idem.ComputeFingerprint([]byte("fixture"))
		}), invite.ErrInvalidRecord},
		{"a redeemed record with no key", mutate(func(r *invite.Record) {
			r.State = invite.StateRedeemed
			r.RedeemedAt = base
			r.RedeemedBy = testAgentID
			r.RedeemFingerprint = idem.ComputeFingerprint([]byte("fixture"))
		}), invite.ErrInvalidRecord},
		{"a redeemed record with no redeem fingerprint", mutate(func(r *invite.Record) {
			r.State = invite.StateRedeemed
			r.RedeemedAt = base
			r.RedeemedBy = testAgentID
			r.RedeemKey = testKey
		}), invite.ErrInvalidRecord},
		{"a redeemed record with an oversized result", mutate(func(r *invite.Record) {
			r.State = invite.StateRedeemed
			r.RedeemedAt = base
			r.RedeemedBy = testAgentID
			r.RedeemKey = testKey
			r.RedeemFingerprint = idem.ComputeFingerprint([]byte("fixture"))
			r.Result = json.RawMessage(`"` + strings.Repeat("x", idem.MaxResultBytes) + `"`)
		}), invite.ErrResultTooLarge},
		{"a redeemed record whose result is not JSON", mutate(func(r *invite.Record) {
			r.State = invite.StateRedeemed
			r.RedeemedAt = base
			r.RedeemedBy = testAgentID
			r.RedeemKey = testKey
			r.RedeemFingerprint = idem.ComputeFingerprint([]byte("fixture"))
			r.Result = json.RawMessage(`{not json`)
		}), invite.ErrInvalidRecord},
		{"a revoked record with no revoked_at", mutate(func(r *invite.Record) { r.State = invite.StateRevoked }), invite.ErrInvalidRecord},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			body, err := tc.rec.Encode()
			if !errors.Is(err, tc.want) {
				t.Fatalf("Encode returned (%s, %v), want %v", body, err, tc.want)
			}
			if body != nil {
				t.Errorf("Encode returned %d bytes alongside its error; a refused record must produce nothing that could be written", len(body))
			}
		})
	}
}

// TestInviteRecordExpiredIsAPurePredicate pins that expiry is derived, not
// stored: the same record answers differently as the clock moves, and the answer
// never depends on State.
func TestInviteRecordExpiredIsAPurePredicate(t *testing.T) {
	base := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	r := invite.Record{ExpiresAt: base.Add(time.Hour)}

	for _, tc := range []struct {
		name string
		now  time.Time
		want bool
	}{
		{"long before", base, false},
		{"one nanosecond before", r.ExpiresAt.Add(-time.Nanosecond), false},
		{"exactly at ExpiresAt", r.ExpiresAt, false},
		{"one nanosecond after", r.ExpiresAt.Add(time.Nanosecond), true},
		{"long after", r.ExpiresAt.Add(365 * 24 * time.Hour), true},
	} {
		if got := r.Expired(tc.now); got != tc.want {
			t.Errorf("Expired(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}

	// A REDEEMED record past its own ExpiresAt is still retained and still
	// answers a legitimate retry: expiry gates a FRESH redemption only.
	spent := r
	spent.State = invite.StateRedeemed
	if !spent.Expired(r.ExpiresAt.Add(time.Hour)) {
		t.Errorf("Expired ignored a redeemed record's window; the predicate must not consult State")
	}
}

// TestInviteSecretVerification pins the constant-time comparison's CONTRACT (not
// its timing, which no ordinary test can measure): the right secret verifies and
// nothing else does, whatever its length.
func TestInviteSecretVerification(t *testing.T) {
	secret, err := invite.GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}
	if len(secret) != invite.EncodedSecretLen {
		t.Fatalf("GenerateSecret returned %d characters, want EncodedSecretLen = %d", len(secret), invite.EncodedSecretLen)
	}
	digest := invite.HashSecret(secret)

	if !invite.VerifySecret(secret, digest) {
		t.Fatalf("the correct secret did not verify")
	}
	for _, bad := range []string{
		"",
		secret[:len(secret)-1],
		secret + "A",
		flipLast(secret),
		strings.Repeat("A", len(secret)),
		strings.Repeat("s", invite.MaxSecretLen*4),
	} {
		if invite.VerifySecret(bad, digest) {
			t.Errorf("a secret of %d bytes that is not the invite's verified", len(bad))
		}
	}

	// Two mints never share a secret, and the digest is not the secret.
	other, err := invite.GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}
	if other == secret {
		t.Fatalf("two calls to GenerateSecret returned the same secret")
	}
	if strings.Contains(hex.EncodeToString(digest[:]), secret) {
		t.Fatalf("the digest contains the plaintext secret")
	}
}

// TestInviteIDValidation pins the id shape, including that the length check runs
// BEFORE the pattern (so an oversized id never reaches the regexp engine) and
// that an oversized id is never echoed back.
func TestInviteIDValidation(t *testing.T) {
	id, err := invite.GenerateInviteID()
	if err != nil {
		t.Fatalf("GenerateInviteID: %v", err)
	}
	if err := invite.ValidateInviteID(id); err != nil {
		t.Fatalf("a generated id %q fails its own validation: %v", id, err)
	}
	if !strings.HasPrefix(id, "inv-") {
		t.Fatalf("a generated id %q has no inv- prefix", id)
	}
	other, err := invite.GenerateInviteID()
	if err != nil {
		t.Fatalf("GenerateInviteID: %v", err)
	}
	if other == id {
		t.Fatalf("two calls to GenerateInviteID returned the same id")
	}

	oversized := "inv-" + strings.Repeat("a", invite.MaxInviteIDLen)
	for _, bad := range []string{
		"",
		"inv-",
		"inv-aaaa",
		"inv-AAAAAAAAAAAAAAAA",
		"inv-aaaaaaaa.aaaaaaa",
		"inv-aaaaaaaaaaaaaaa1",
		"inv-" + strings.Repeat("a", 33),
		"bus-aaaaaaaaaaaaaaaa",
		oversized,
	} {
		err := invite.ValidateInviteID(bad)
		if !errors.Is(err, invite.ErrInvalidInviteID) {
			t.Errorf("ValidateInviteID(%q) = %v, want ErrInvalidInviteID", bad, err)
		}
		if len(bad) > invite.MaxInviteIDLen && strings.Contains(err.Error(), bad) {
			t.Errorf("an oversized id was echoed back in the error")
		}
	}
}
