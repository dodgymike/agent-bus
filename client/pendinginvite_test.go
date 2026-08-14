package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// INVITE-CLIENT-FU-PENDINGINVITE — the pending enrolment record now carries the
// INVITE ID, and a resume that presents a different invite is refused HERE,
// before anything is sent and before anything is dropped.
//
// # This is a DATA-LOSS guard, not tidiness
//
// The sequence, all of it reachable at HEAD before this change:
//
//  1. `enrol --invite-file A` writes a pending record — the ONLY copy of BOTH
//     private key seeds (auth and messaging) — and posts /v1/enroll.
//  2. The answer is lost, or arrives as a 5xx. The material is deliberately
//     KEPT, because the bus may have applied the enrolment: that is what
//     `--idempotency-key` exists to resume.
//  3. The operator resumes with `--idempotency-key K` but reaches for a
//     DIFFERENT invite file, B. Nothing local objected, so the request went out
//     with the original name and keys but a new invite_id.
//  4. The server's idempotency fingerprint covers invite_id, so this is "same
//     key + DIFFERENT payload" — invariant 10's protocol violation — answered
//     409. That is KindRejected, which landed in enrolFailed's DEFAULT branch
//     and called DropPending.
//
// Step 4 destroyed the only copy of the private keys of an attempt the bus may
// already have applied. If it had been applied, the bus holds an agent id whose
// auth public key nobody can sign for and whose messaging public key it attests
// to peers — unrecoverable, by a mistake made on the RETRY. A private key is
// not something a client may drop to tidy up after a refusal.
//
// Invariants read IN FULL before writing this: 3 (enrolment is invite-only and
// the invite is the trust anchor — so the invite id is part of what an
// enrolment ASSERTS) and 10 (same key + SAME payload is a legitimate retry that
// must return the original result; same key + DIFFERENT payload is a protocol
// violation that is rejected and logged and must NOT disconnect — so the
// question this file answers is what the CLIENT must PRESERVE when the server
// correctly rejects it).
//
// The fix has three parts, and each is asserted below:
//
//   - pendingEnrolment records the invite ID (never the secret), so the
//     mismatch is refused LOCALLY, with no round trip and nothing dropped;
//   - a RESUMED attempt never drops its key material on a refusal, whatever the
//     status. The local check cannot cover a record written before the id was
//     recorded, and "keep a stale record for at most pendingTTL" is
//     incomparably cheaper than "destroy a live private key";
//   - a 409 never drops, RESUMED OR NOT. "resumed" means the store already held
//     a record; the property that matters is that a request under this key
//     reached the bus, and an in-call retry (the enrol request is sent
//     retryable: true) achieves that with no record having pre-existed. A 409 is
//     the bus saying it already holds the key. The security gate reproduced
//     that sequence against the first version of this fix.

// pendingInviteKey is the idempotency key every test here resumes under.
const pendingInviteKey = "busctl-pendinginvite-resume"

// otherInviteID and otherInviteSecret are the SECOND invite — the wrong one the
// operator reaches for on the retry. The secret is a second sentinel so the
// leak checks below cannot pass by matching only the first.
const (
	otherInviteID     = "inv-01H8XOTHERINVITE"
	otherInviteSecret = "INVITE-SENTINEL-9e4d17b2-second-invite-do-not-leak-fedcba9876543210"
)

// otherInvite is a well-formed invite for the SAME bus, with a different id and
// a different secret. Same bus on purpose: a different address would be refused
// by inviteEndpoint for an unrelated reason and would prove nothing about the
// invite id.
func otherInvite(busURL, fingerprint string) *Invite {
	inv := testInvite(busURL, fingerprint)
	inv.InviteID = otherInviteID
	inv.InviteSecret = otherInviteSecret
	return inv
}

// interruptedEnrolment performs step 1-2 above: an enrolment the bus answers
// with a transient 503, leaving a pending record holding both seeds.
//
// It returns the record as it stands on disk, so a later assertion can compare
// the key material byte for byte rather than merely checking that SOMETHING is
// still there.
func interruptedEnrolment(t *testing.T, rec *inviteBus, bus *tlsBus, pin BusFingerprint, dir string) pendingEnrolment {
	t.Helper()

	rec.status, rec.errBody, rec.retryAfter = http.StatusServiceUnavailable, "at capacity", "1"
	inv := testInvite(bus.URL(), pin.String())
	c := inviteClient(t, dir, nil)

	_, err := c.Enrol(context.Background(), EnrolOptions{
		Name: "planner", Invite: inv, Save: true, IdempotencyKey: pendingInviteKey,
	})
	if err == nil {
		t.Fatalf("the first attempt succeeded; it must fail, or there is no in-flight record to resume and every assertion below is vacuous")
	}
	if KindOf(err) != KindServer {
		t.Fatalf("the first attempt failed as %q, want %q — only KindNetwork/KindServer keep the pending record, so any other kind makes this setup vacuous", KindOf(err), KindServer)
	}

	// Back to answering normally: the retry's outcome must be decided by the
	// invite it presents, not by a bus still returning 503.
	rec.status, rec.errBody, rec.retryAfter = 0, "", ""

	p := requirePending(t, c, bus, "after the interrupted first attempt")
	if p.PrivateKeySeed == "" || p.MessagingKeySeed == "" {
		t.Fatalf("the pending record is missing key material (auth seed empty=%v, messaging seed empty=%v); there is nothing for the tests below to protect",
			p.PrivateKeySeed == "", p.MessagingKeySeed == "")
	}
	return p
}

// pendingBusURL is the canonical URL the pending record is keyed by.
func pendingBusURL(t *testing.T, bus *tlsBus) string {
	t.Helper()
	u, err := parseBusURL(bus.URL())
	if err != nil {
		t.Fatalf("parsing the stub bus URL: %v", err)
	}
	return u.String()
}

// requirePending reads the in-flight record back out of the store, failing if
// it is gone.
func requirePending(t *testing.T, c *Client, bus *tlsBus, when string) pendingEnrolment {
	t.Helper()
	p, ok, err := c.store.FindPending(pendingInviteKey, pendingBusURL(t, bus))
	if err != nil {
		t.Fatalf("reading the pending record %s: %v", when, err)
	}
	if !ok {
		t.Fatalf("the pending record for idempotency key %q is GONE %s. That record is the only copy of both private key seeds of an attempt the bus may already have applied: the identity it minted can never authenticate again.",
			pendingInviteKey, when)
	}
	return p
}

// assertKeyMaterialIntact compares the material on disk with what attempt one
// wrote. Equality is the assertion, not mere presence: a record that survived
// with different seeds would be just as unrecoverable as one that was deleted.
func assertKeyMaterialIntact(t *testing.T, before, after pendingEnrolment) {
	t.Helper()
	if after.PrivateKeySeed != before.PrivateKeySeed {
		t.Errorf("the stored AUTH private key seed changed; the bus may hold the public half of the original, which is now unrecoverable")
	}
	if after.MessagingKeySeed != before.MessagingKeySeed {
		t.Errorf("the stored MESSAGING private key seed changed; the bus attests the public half to peer buses, so every peer would reject everything this agent signs")
	}
	if after.PublicKey != before.PublicKey {
		t.Errorf("the stored public key changed: %q -> %q", before.PublicKey, after.PublicKey)
	}
}

// TestEnrolResumeWithDifferentInviteIsRefusedLocally is the recorded proof
// command for INVITE-CLIENT-FU-PENDINGINVITE.
func TestEnrolResumeWithDifferentInviteIsRefusedLocally(t *testing.T) {
	t.Run("a different invite is refused with no round trip and no key material dropped", func(t *testing.T) {
		rec, bus, pin := newInviteTLSBus(t)
		dir := t.TempDir()
		before := interruptedEnrolment(t, rec, bus, pin, dir)

		c := inviteClient(t, dir, nil)
		_, err := c.Enrol(context.Background(), EnrolOptions{
			Name:           "planner",
			Invite:         otherInvite(bus.URL(), pin.String()),
			Save:           true,
			IdempotencyKey: pendingInviteKey,
		})
		if err == nil {
			t.Fatalf("resuming with a DIFFERENT invite succeeded; the payload the bus fingerprints covers invite_id, so this is same key + different payload (invariant 10)")
		}
		if KindOf(err) != KindUsage {
			t.Errorf("KindOf(err) = %q, want %q — this is the caller's own mistake, caught here, not a bus refusal", KindOf(err), KindUsage)
		}
		if got := ExitCode(err); got != ExitUsage {
			t.Errorf("ExitCode(err) = %d, want %d", got, ExitUsage)
		}

		// The whole point: it never left the process. A round trip would spend
		// the second invite's reservation and earn a 409 whose handling used to
		// destroy the key material.
		if got := rec.calls(); got != 1 {
			t.Errorf("the bus saw %d enrol requests, want 1 (the interrupted first attempt only) — the mismatch must be refused locally", got)
		}

		e := requirePendingInviteError(t, err)
		for _, want := range []string{pendingInviteKey, inviteTestID, otherInviteID} {
			if !strings.Contains(e.Message+" "+e.Remedy, want) {
				t.Errorf("the refusal names neither the key nor both invites; it must say which invite the stored attempt belongs to.\nmessage: %q\nremedy:  %q\nmissing: %q", e.Message, e.Remedy, want)
			}
		}
		if e.IdempotencyKey != pendingInviteKey {
			t.Errorf("Error.IdempotencyKey = %q, want %q: an empty key documents that no key ever existed, which is false here and is the handle that resumes the attempt", e.IdempotencyKey, pendingInviteKey)
		}

		assertKeyMaterialIntact(t, before, requirePending(t, c, bus, "after a refused resume"))
		// BOTH secrets: the stored attempt's and the one presented on the
		// retry. The refusal names two invites, so it is exactly the kind of
		// message that grows a credential by accident.
		assertNoSecretInError(t, "a resume with the wrong invite", err)
		assertSecondSecretAbsent(t, "a resume with the wrong invite", err)
	})

	t.Run("resuming with NO invite is refused too", func(t *testing.T) {
		// The other direction of the same payload change: the stored attempt
		// asserted invite_id A and this one asserts none, which the bus
		// fingerprints just as differently.
		rec, bus, pin := newInviteTLSBus(t)
		dir := t.TempDir()
		before := interruptedEnrolment(t, rec, bus, pin, dir)

		c := inviteClient(t, dir, func(cfg *Config) {
			cfg.BusURL = bus.URL()
			cfg.BusFingerprint = pin.String()
		})
		_, err := c.Enrol(context.Background(), EnrolOptions{
			Name: "planner", Save: true, IdempotencyKey: pendingInviteKey,
		})
		if err == nil {
			t.Fatalf("resuming WITHOUT the invite succeeded; the stored attempt presented one, so this payload differs")
		}
		if KindOf(err) != KindUsage {
			t.Errorf("KindOf(err) = %q, want %q", KindOf(err), KindUsage)
		}
		if got := rec.calls(); got != 1 {
			t.Errorf("the bus saw %d enrol requests, want 1 — refuse locally", got)
		}
		assertKeyMaterialIntact(t, before, requirePending(t, c, bus, "after a resume that dropped the invite"))
	})

	t.Run("the SAME invite still resumes and is not over-refused", func(t *testing.T) {
		// The guard must not break the one action that recovers an interrupted
		// enrolment. Invariant 10: same key + same payload is a LEGITIMATE
		// retry, and the bus answers it from its own record.
		rec, bus, pin := newInviteTLSBus(t)
		dir := t.TempDir()
		before := interruptedEnrolment(t, rec, bus, pin, dir)
		rec.replayed = true

		c := inviteClient(t, dir, nil)
		res, err := c.Enrol(context.Background(), EnrolOptions{
			Name:           "planner",
			Invite:         testInvite(bus.URL(), pin.String()),
			Save:           true,
			MakeCurrent:    true,
			IdempotencyKey: pendingInviteKey,
		})
		if err != nil {
			t.Fatalf("resuming with the SAME invite failed: %v", err)
		}
		if !res.Replayed {
			t.Errorf("Replayed = false; the bus answered from its idempotency table")
		}
		if got := rec.calls(); got != 2 {
			t.Fatalf("the bus saw %d enrol requests, want 2 — a legitimate retry must reach the bus", got)
		}
		cred, ok, err := c.store.FindApplied(pendingInviteKey, pendingBusURL(t, bus))
		if err != nil || !ok {
			t.Fatalf("the resumed enrolment was not stored (ok=%v, err=%v)", ok, err)
		}
		if cred.PrivateKeySeed != before.PrivateKeySeed || cred.MessagingKeySeed != before.MessagingKeySeed {
			t.Errorf("the promoted credential does not carry the ORIGINAL key material; the retry sent the original public keys, so a different private half would be an identity that can never authenticate")
		}
	})

	t.Run("a record written before the invite id was recorded still resumes", func(t *testing.T) {
		// Deliberate: an EMPTY stored invite id is AMBIGUOUS — it is either a
		// genuinely un-invited attempt or a record written by a build that did
		// not have this field. Refusing would strand a legitimate resume of the
		// second kind, so the mismatch goes to the bus exactly as it did
		// before. The material is still safe, because the drop guard below
		// covers it.
		rec, bus, pin := newInviteTLSBus(t)
		dir := t.TempDir()
		before := interruptedEnrolment(t, rec, bus, pin, dir)

		c := inviteClient(t, dir, nil)
		clearStoredInviteID(t, c)

		if _, err := c.Enrol(context.Background(), EnrolOptions{
			Name:           "planner",
			Invite:         testInvite(bus.URL(), pin.String()),
			Save:           true,
			IdempotencyKey: pendingInviteKey,
		}); err != nil {
			t.Fatalf("resuming a legacy pending record with an invite failed: %v", err)
		}
		if got := rec.calls(); got != 2 {
			t.Errorf("the bus saw %d enrol requests, want 2 — an unknown stored invite id must NOT be refused locally", got)
		}
		cred, ok, err := c.store.FindApplied(pendingInviteKey, pendingBusURL(t, bus))
		if err != nil || !ok {
			t.Fatalf("the resumed legacy enrolment was not stored (ok=%v, err=%v)", ok, err)
		}
		if cred.PrivateKeySeed != before.PrivateKeySeed {
			t.Errorf("the legacy resume did not reuse the stored key material")
		}
	})

	t.Run("a RESUMED attempt keeps its key material when the bus refuses it", func(t *testing.T) {
		// The second half of the fix, and the one the local check cannot
		// deliver: the ambiguous legacy record above still reaches the bus, and
		// the bus answers the payload change with the 409 that used to call
		// DropPending. A refusal is never evidence that the EARLIER attempt was
		// not applied, so a resumed attempt's material is kept.
		for _, tc := range []struct {
			name    string
			status  int
			errBody string
		}{
			{"409 the key was used with different content", http.StatusConflict, "idempotency key already used with a different payload"},
			{"409 another redemption is in flight", http.StatusConflict, "another redemption of this invite is in flight; retry"},
			{"403 the bus refused the invite", http.StatusForbidden, "invite not accepted"},
		} {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				rec, bus, pin := newInviteTLSBus(t)
				dir := t.TempDir()
				before := interruptedEnrolment(t, rec, bus, pin, dir)

				c := inviteClient(t, dir, nil)
				clearStoredInviteID(t, c)
				rec.status, rec.errBody = tc.status, tc.errBody

				_, err := c.Enrol(context.Background(), EnrolOptions{
					Name:           "planner",
					Invite:         testInvite(bus.URL(), pin.String()),
					Save:           true,
					IdempotencyKey: pendingInviteKey,
				})
				if err == nil {
					t.Fatalf("the bus answered %d but Enrol returned no error", tc.status)
				}
				if got := rec.calls(); got != 2 {
					t.Fatalf("the bus saw %d enrol requests, want 2 — the refusal under test never happened", got)
				}
				assertKeyMaterialIntact(t, before, requirePending(t, c, bus, "after the bus refused a RESUMED attempt"))

				// And the caller is told the material is still there, by the
				// key that reaches it. An error that keeps a record nobody is
				// told about stores the key without making it recoverable.
				e := requirePendingInviteError(t, err)
				if e.IdempotencyKey != pendingInviteKey {
					t.Errorf("Error.IdempotencyKey = %q, want %q", e.IdempotencyKey, pendingInviteKey)
				}
			})
		}
	})

	t.Run("a FRESH attempt whose retry meets the bus KEEPS its key material", func(t *testing.T) {
		// The hole the security gate reproduced, and the reason "resumed" alone
		// is not the right predicate.
		//
		// The enrol request is sent retryable: true, so do() retries a
		// transport failure in-call. Attempt 1 can reach the bus, be applied or
		// begin a redemption, and have its ANSWER lost; attempt 2 then lands
		// inside attempt 1's own [Begin, Commit) window and is answered 409
		// "another redemption of this invite is in flight". No pending record
		// pre-existed, so resumed is FALSE — and the seeds on disk are
		// nonetheless the private half of a request the bus has already seen.
		//
		// The property that matters is "a request under this key reached the
		// bus", and a 409 is the bus SAYING exactly that.
		cert := newSelfSignedBusCert(t)
		var mu sync.Mutex
		calls := 0
		bus := newTLSBus(t, cert, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			calls++
			n := calls
			mu.Unlock()
			if n == 1 {
				// The answer is lost, not the request: the bus HAS it. Aborting
				// the handler drops the connection, which the client sees as a
				// transport failure and retries — exactly the sequence, driven
				// through the real retry loop rather than simulated.
				panic(http.ErrAbortHandler)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"another redemption of this invite is in flight; retry"}` + "\n"))
		}))
		pin := fingerprintOf(cert)
		dir := t.TempDir()

		c := inviteClient(t, dir, func(cfg *Config) {
			// The invite tests otherwise pin Attempts: 1, which cannot reach
			// this path at all — that is why the hole survived the first round
			// of tests (security gate finding 2).
			cfg.Retry = RetryPolicy{Attempts: 2, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}
		})
		_, err := c.Enrol(context.Background(), EnrolOptions{
			Name:           "planner",
			Invite:         testInvite(bus.URL(), pin.String()),
			Save:           true,
			IdempotencyKey: pendingInviteKey,
		})
		if err == nil {
			t.Fatalf("Enrol returned no error; the bus answered 409 on the retry")
		}
		mu.Lock()
		got := calls
		mu.Unlock()
		if got != 2 {
			t.Fatalf("the bus saw %d requests, want 2 — the in-call retry never happened, so this test does not exercise the reported sequence", got)
		}
		if _, ok, ferr := c.store.FindPending(pendingInviteKey, pendingBusURL(t, bus)); ferr != nil || !ok {
			t.Fatalf("the pending record is GONE after a 409 on a FRESH attempt (ok=%v, err=%v). A request under this key HAD reached the bus — that is what a 409 means — so these seeds may be the private half of an enrolment it already applied.", ok, ferr)
		}
	})

	t.Run("a FRESH attempt still drops its key material when the bus refuses it", func(t *testing.T) {
		// The mirror image, and the reason the guard is not simply switched
		// off: a 403 says the INVITE was refused, and an invite is refused
		// before anything is applied, so material this call minted seconds ago
		// belongs to nothing. Dropping it is right, and a permanent refusal
		// must not leave a record behind for pendingTTL.
		rec, bus, pin := newInviteTLSBus(t)
		rec.status, rec.errBody = http.StatusForbidden, "invite not accepted"
		dir := t.TempDir()

		c := inviteClient(t, dir, nil)
		_, err := c.Enrol(context.Background(), EnrolOptions{
			Name:           "planner",
			Invite:         testInvite(bus.URL(), pin.String()),
			Save:           true,
			IdempotencyKey: pendingInviteKey,
		})
		if err == nil {
			t.Fatalf("the bus answered 403 but Enrol returned no error")
		}
		if _, ok, ferr := c.store.FindPending(pendingInviteKey, pendingBusURL(t, bus)); ferr != nil || ok {
			t.Errorf("a FRESH refused attempt left a pending record behind (ok=%v, err=%v); only a resumed attempt's material is protected", ok, ferr)
		}
	})
}

// assertSecondSecretAbsent checks the SECOND invite's secret against exactly
// the surface assertNoSecretInError covers for the first — including the
// marshalled --json payload, which errorRenderings does NOT include and which
// is what an agent actually parses.
func assertSecondSecretAbsent(t *testing.T, what string, err error) {
	t.Helper()
	rendered := errorRenderings(err)
	payload, merr := json.Marshal(NewErrorPayload(err))
	if merr != nil {
		t.Fatalf("%s: marshalling the error payload: %v", what, merr)
	}
	rendered["--json payload"] = string(payload)
	for label, s := range rendered {
		if strings.Contains(s, otherInviteSecret) {
			t.Fatalf("%s: %s reproduces the SECOND invite's SECRET, a bearer credential: %s", what, label, s)
		}
	}
}

// TestInviteConflictNeutralisesAStoredInviteID pins the safeText on the id that
// came off DISK.
//
// The presented id has already passed Invite.Validate's charset rule by the
// time ClaimEnrolment sees it, so a test that only supplies a hostile invite
// FILE proves nothing about the stored side — and Store.load() validates no
// fields, so the store is the one route by which a control character reaches
// this message. The security gate observed that deleting either safeText in
// inviteConflict passed every test; this is the one that notices.
func TestInviteConflictNeutralisesAStoredInviteID(t *testing.T) {
	rec, bus, pin := newInviteTLSBus(t)
	dir := t.TempDir()
	interruptedEnrolment(t, rec, bus, pin, dir)

	c := inviteClient(t, dir, nil)
	// A store edited to carry an ANSI erase and a forged success line.
	if err := c.store.update(func(d *storeData) error {
		for i := range d.Pending {
			if d.Pending[i].IdempotencyKey == pendingInviteKey {
				d.Pending[i].InviteID = forgingInviteID
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("rewriting the stored invite id: %v", err)
	}

	_, err := c.Enrol(context.Background(), EnrolOptions{
		Name:           "planner",
		Invite:         otherInvite(bus.URL(), pin.String()),
		Save:           true,
		IdempotencyKey: pendingInviteKey,
	})
	if err == nil {
		t.Fatalf("the resume was not refused, so there is no message to check and this proof is vacuous")
	}
	if got := rec.calls(); got != 1 {
		t.Fatalf("the bus saw %d requests, want 1: the refusal must still be local", got)
	}
	assertNoTerminalForgery(t, "a refusal quoting a hostile STORED invite id", errorRenderings(err))
}

// clearStoredInviteID rewrites the pending record's invite id to "", modelling
// a record written by a build that did not have the field.
func clearStoredInviteID(t *testing.T, c *Client) {
	t.Helper()
	if err := c.store.update(func(d *storeData) error {
		for i := range d.Pending {
			if d.Pending[i].IdempotencyKey == pendingInviteKey {
				d.Pending[i].InviteID = ""
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("rewriting the pending record: %v", err)
	}
}

// TestPendingEnrolmentRecordsTheInviteIDAndNeverTheSecret pins the on-disk
// half: the id is a NAME and is recorded so a resume can be checked; the SECRET
// is a bearer credential and must never reach the disk.
func TestPendingEnrolmentRecordsTheInviteIDAndNeverTheSecret(t *testing.T) {
	rec, bus, pin := newInviteTLSBus(t)
	dir := t.TempDir()
	p := interruptedEnrolment(t, rec, bus, pin, dir)

	if p.InviteID != inviteTestID {
		t.Errorf("the pending record stores invite id %q, want %q", p.InviteID, inviteTestID)
	}

	// The raw bytes on disk, not the struct: what matters is what a reader of
	// the file can see.
	raw, err := os.ReadFile(filepath.Join(dir, storeFileName))
	if err != nil {
		t.Fatalf("reading %s: %v", storeFileName, err)
	}
	if !strings.Contains(string(raw), inviteTestID) {
		t.Fatalf("the store file does not contain the invite id, so the secret check below would be vacuous")
	}
	if strings.Contains(string(raw), inviteTestSecret) {
		t.Fatalf("the store file contains the invite SECRET — a bearer credential that must live only in the request body")
	}
	if strings.Contains(p.String(), inviteTestSecret) {
		t.Fatalf("pendingEnrolment.String leaks the invite secret")
	}
	if !strings.Contains(p.String(), inviteTestID) {
		t.Errorf("pendingEnrolment.String drops the invite id; it is a name, not a credential, and it is what identifies which attempt this is")
	}
}

// TestEnrolFailedKeepsResumedMaterialForANonClientError covers enrolFailed's
// OTHER drop site: the early return for a failure that is not a *client.Error.
//
// It is a real branch and it was the one left untested (reviewer gate). It also
// carries the least information of any failure path — an unclassified error is
// the WEAKEST possible evidence that an earlier attempt was not applied — so it
// is the last place that may delete an earlier attempt's private keys.
func TestEnrolFailedKeepsResumedMaterialForANonClientError(t *testing.T) {
	const key = "enrol-nonclient-error"
	const busURL = "https://bus.example"

	seed := func(t *testing.T) *Client {
		t.Helper()
		c, err := New(Config{IdentityDir: t.TempDir()})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if _, err := c.store.ClaimEnrolment(pendingEnrolment{
			IdempotencyKey: key,
			Name:           "planner",
			BusURL:         busURL,
			PublicKey:      "cHViLWtleQ==",
			PrivateKeySeed: "c2VlZC1ieXRlcy1oZXJlLWZvci10ZXN0aW5nLW9ubHk=",
			CreatedAt:      time.Now().UTC().Format(time.RFC3339Nano),
		}, time.Now()); err != nil {
			t.Fatalf("seeding a pending record: %v", err)
		}
		return c
	}

	for _, tc := range []struct {
		name      string
		resumed   bool
		wantKept  bool
		wantWhyNo string
	}{
		{name: "resumed keeps the material", resumed: true, wantKept: true},
		{name: "fresh drops it", resumed: false, wantKept: false},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			c := seed(t)
			opaque := errors.New("something unclassified went wrong")

			got := c.enrolFailed("enrol", key, busURL, EnrolOptions{Save: true}, tc.resumed, opaque)
			if !errors.Is(got, opaque) {
				t.Fatalf("enrolFailed returned %v, want the original error passed through unchanged", got)
			}

			_, found, ferr := c.store.FindPending(key, busURL)
			if ferr != nil {
				t.Fatalf("FindPending: %v", ferr)
			}
			if found != tc.wantKept {
				if tc.wantKept {
					t.Fatalf("the pending record was DROPPED for a resumed attempt on an unclassified error; those seeds may be the only copy for an enrolment the bus already applied")
				}
				t.Fatalf("a FRESH attempt left its pending record behind; material minted moments ago belongs to no earlier attempt and must not linger for pendingTTL")
			}
		})
	}
}

// requirePendingInviteError unwraps err to the package's one error type.
func requirePendingInviteError(t *testing.T, err error) *Error {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("error is not a *client.Error: %v", err)
	}
	return e
}
