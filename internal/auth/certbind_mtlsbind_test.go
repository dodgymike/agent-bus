package auth_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/auth"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// Guards for MTLS-BIND: enrolment binds the presenting client certificate to the
// server-minted agent id (invariants 1, 3 and 11).
//
// # EACH GUARD IS MUTATION-TESTED ALONE, WHICH IS WHY THEY ARE SEPARATE TESTS
//
// The requirement on this task was explicit: a SINGLE mutation must be able to
// turn a guard red. RELAY-20's equivalent property was protected by two
// independent mechanisms, so no one mutation went red and a security auditor
// flagged it as a latent trap rather than defence in depth. Every test below
// names the mutation that kills it, and every one of those mutations was
// applied on its own and observed to fail EXACTLY the named test — see the
// task's report note for the transcript.
//
// The fingerprints here are synthetic [32]byte values, deliberately. This
// package stores a DIGEST and never parses a certificate; the guard that the
// digest is computed the one true way (buscert.FingerprintOf, over the DER
// exactly as it arrived) belongs to the layer that holds the certificate, and it
// is in internal/httpapi's clientcert_mtlsbind_test.go.

// fpN builds a distinct, non-zero fingerprint. Non-zero matters: the zero
// [32]byte is what an absent binding would leave behind, so a test using it
// could pass on a code path that bound nothing at all.
func fpN(n byte) [32]byte {
	var fp [32]byte
	for i := range fp {
		fp[i] = n
	}
	return fp
}

// mtlsEntry is a FULLY VALID roster entry with the given id and bindings.
//
// Name is parsed back out of the id rather than written by hand, because the
// DURABLE roster refuses an entry whose Name disagrees with the name inside its
// AgentID (validateRosterEntry). A hand-written name makes every WAL-backed test
// here fail on record validation before it ever reaches the rule under test —
// which is exactly what happened while these were being written, and is worth
// the two lines to prevent recurring.
func mtlsEntry(t *testing.T, agentID string, bindings ...auth.CertBinding) auth.RosterEntry {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	_, name, _, err := ids.ParseAgentID(agentID)
	if err != nil {
		t.Fatalf("test fixture: agent id %q does not parse: %v", agentID, err)
	}
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	return auth.RosterEntry{
		AgentID:       agentID,
		Name:          name,
		AuthPublicKey: pub,
		Epoch:         now,
		EnrolledAt:    now,
		CertBindings:  bindings,
	}
}

// mtlsService builds a Service over a MemoryRoster the test also holds a
// reference to, so a test can read the STORED entry rather than infer it from
// the response — the binding is not in the enrolment response and never should
// be (it is derived from the connection, and the client already knows its own
// certificate).
func mtlsService(t *testing.T) (*auth.Service, *auth.MemoryRoster) {
	t.Helper()
	r := auth.NewMemoryRoster()
	svc, _ := newService(t, auth.Options{Roster: r})
	return svc, r
}

// enrolReqWithCert builds a valid enrolment request with a FRESH keypair,
// carrying fp as the certificate presented on the connection (nil for none).
func enrolReqWithCert(t *testing.T, name, idemKey string, fp *[32]byte) auth.EnrolRequest {
	t.Helper()
	pub, _ := newKeypair(t)
	return auth.EnrolRequest{
		Name:                  name,
		PublicKey:             pub,
		IdempotencyKey:        idemKey,
		ClientCertFingerprint: fp,
	}
}

// mtlsMustGet reads the stored roster entry for agentID, failing the test when
// it is absent.
func mtlsMustGet(t *testing.T, r *auth.MemoryRoster, agentID string) auth.RosterEntry {
	t.Helper()
	e, ok := r.Get(agentID)
	if !ok {
		t.Fatalf("agent %q is not in the roster after enrolment", agentID)
	}
	return e
}

func liveBinding(fp [32]byte) auth.CertBinding {
	return auth.CertBinding{Fingerprint: fp, BoundAt: time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)}
}

func retiredBinding(fp [32]byte) auth.CertBinding {
	at := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	retired := at.Add(time.Hour)
	return auth.CertBinding{Fingerprint: fp, BoundAt: at, RetiredAt: &retired}
}

// TestCertBindingResolvesToTheOneBoundAgent is the positive read: a fingerprint
// bound live to exactly one agent resolves to that agent.
//
// MUTATION THAT KILLS IT ALONE: make certFingerprintOwner return
// ErrCertBindingUnknown unconditionally, or compare something other than the
// fingerprint.
func TestCertBindingResolvesToTheOneBoundAgent(t *testing.T) {
	r := auth.NewMemoryRoster()
	fp := fpN(0x11)
	if err := r.Put(mtlsEntry(t, "bus-under-test.alpha-1", liveBinding(fp))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// A second agent with a DIFFERENT certificate, so a lookup that ignored the
	// fingerprint and returned "the only agent" would pass without it.
	if err := r.Put(mtlsEntry(t, "bus-under-test.beta-1", liveBinding(fpN(0x22)))); err != nil {
		t.Fatalf("Put second: %v", err)
	}

	got, err := r.AgentIDForCertFingerprint(fp)
	if err != nil {
		t.Fatalf("AgentIDForCertFingerprint: unexpected error: %v", err)
	}
	if got != "bus-under-test.alpha-1" {
		t.Fatalf("resolved to %q, want %q", got, "bus-under-test.alpha-1")
	}
}

// TestCertBindingUnknownFailsClosed: a fingerprint nobody holds resolves to
// NOBODY and to an error, never to "" with a nil error — a nil error is what a
// caller checks, and returning one here would read as a successful lookup of the
// empty agent id.
//
// MUTATION THAT KILLS IT ALONE: return "" with a nil error from
// certFingerprintOwner's zero-holder arm.
func TestCertBindingUnknownFailsClosed(t *testing.T) {
	r := auth.NewMemoryRoster()
	if err := r.Put(mtlsEntry(t, "bus-under-test.alpha-1", liveBinding(fpN(0x11)))); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := r.AgentIDForCertFingerprint(fpN(0x99))
	if !errors.Is(err, auth.ErrCertBindingUnknown) {
		t.Fatalf("error = %v, want ErrCertBindingUnknown", err)
	}
	if got != "" {
		t.Fatalf("an unknown fingerprint resolved to %q; it must name nobody", got)
	}
}

// THE AMBIGUITY GUARD IS DELIBERATELY NOT HERE.
//
// A test lived at this spot that built the ambiguous state in newStubRoster and
// asserted ErrCertBindingAmbiguous. It was DELETED because mutation testing
// proved it could not fail: the stub has its own copy of the resolution logic,
// so making certFingerprintOwner's ambiguous arm return holders[0] — the exact
// defect the test named — left it GREEN. It was measuring the test double.
//
// The rule is guarded in two places that DO exercise the shipped code, and both
// go red under that mutation:
//
//   - TestCertFingerprintOwnerAmbiguousArmRefuses (certbind_internal_mtlsbind_
//     test.go) calls the real resolver directly — the only way to reach the arm
//     quickly, since no exported API can CREATE the state.
//   - TestWALRosterRecoversAnAmbiguousBindingAndRefusesToResolveIt (below)
//     reaches it the way production does: off disk, through recovery.
//
// This note is left rather than the space closed up, because "there is no test
// here" and "the test here was removed for being unfalsifiable" are different
// facts, and only one of them stops someone re-adding it.

// TestRetiredCertBindingIsNotLive: a retired binding is history, not a
// credential. It must not resolve.
//
// MUTATION THAT KILLS IT ALONE: drop the `b.RetiredAt == nil` clause from
// hasLiveCertBinding.
func TestRetiredCertBindingIsNotLive(t *testing.T) {
	r := auth.NewMemoryRoster()
	fp := fpN(0x44)
	if err := r.Put(mtlsEntry(t, "bus-under-test.alpha-1", retiredBinding(fp))); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := r.AgentIDForCertFingerprint(fp)
	if !errors.Is(err, auth.ErrCertBindingUnknown) {
		t.Fatalf("a RETIRED binding resolved: got %q, err %v; want ErrCertBindingUnknown", got, err)
	}
}

// TestRotationBothLiveBindingsResolve: during a rollover an agent legitimately
// holds TWO live bindings (invariant 11 — rotation serves two certificates and
// must never require re-enrolment), and BOTH must resolve to it.
//
// This is the guard against a "newest binding wins" shortcut, which would refuse
// the outgoing certificate for the whole rollover — silently, since the incoming
// one keeps working and only the clients that had not re-pinned yet break.
//
// MUTATION THAT KILLS IT ALONE: make hasLiveCertBinding consider only the LAST
// element of CertBindings.
func TestRotationBothLiveBindingsResolve(t *testing.T) {
	r := auth.NewMemoryRoster()
	old, incoming := fpN(0x55), fpN(0x66)
	if err := r.Put(mtlsEntry(t, "bus-under-test.alpha-1", liveBinding(old), liveBinding(incoming))); err != nil {
		t.Fatalf("Put: %v", err)
	}

	for _, fp := range [][32]byte{old, incoming} {
		got, err := r.AgentIDForCertFingerprint(fp)
		if err != nil || got != "bus-under-test.alpha-1" {
			t.Fatalf("binding %x resolved to (%q, %v), want (bus-under-test.alpha-1, nil): both certificates are live during a rollover", fp[:4], got, err)
		}
	}
}

// TestMemoryRosterRefusesAFingerprintBoundToAnotherAgent is the WRITE-side
// uniqueness rule on the in-memory roster: one certificate must never name two
// agents.
//
// MUTATION THAT KILLS IT ALONE: delete the checkCertFingerprintUnbound call from
// MemoryRoster.Put.
func TestMemoryRosterRefusesAFingerprintBoundToAnotherAgent(t *testing.T) {
	r := auth.NewMemoryRoster()
	fp := fpN(0x77)
	if err := r.Put(mtlsEntry(t, "bus-under-test.alpha-1", liveBinding(fp))); err != nil {
		t.Fatalf("Put alpha: %v", err)
	}

	err := r.Put(mtlsEntry(t, "bus-under-test.beta-1", liveBinding(fp)))
	if !errors.Is(err, auth.ErrCertFingerprintBound) {
		t.Fatalf("second Put with the same certificate: err = %v, want ErrCertFingerprintBound", err)
	}
	// AND THE REFUSAL LEFT NOTHING BEHIND. A check that refused but had already
	// inserted would be worse than none: the roster would hold the very
	// ambiguity the check exists to prevent, while reporting that it did not.
	if _, ok := r.Get("bus-under-test.beta-1"); ok {
		t.Fatal("the refused agent is in the roster; the refusal must leave no entry")
	}
	if got, err := r.AgentIDForCertFingerprint(fp); err != nil || got != "bus-under-test.alpha-1" {
		t.Fatalf("after the refusal the fingerprint resolved to (%q, %v), want (bus-under-test.alpha-1, nil)", got, err)
	}
}

// TestRosterPutChecksTheAgentIDBeforeTheCertificate pins the ORDER of the two
// refusals in Put, which is the only part of the self-skip rule that is
// observable through an exported API.
//
// It is NOT a guard on the self-skip itself. It was written as one, and mutation
// testing showed it could not fail: removing the `agentID == e.AgentID` skip
// changes nothing here, because an entry whose id is already present is refused
// by the DUPLICATE-ID check first and never reaches the certificate check. The
// self-skip is guarded directly in certbind_internal_mtlsbind_test.go
// (TestCheckCertFingerprintUnboundSkipsTheSameAgent), which is the only place it
// is falsifiable.
//
// What this DOES pin is worth keeping: the id rule wins, so a re-put of an
// existing agent reports the reason a caller can act on ("that id is taken")
// rather than a certificate collision with itself, which would be a confusing
// and wrong diagnosis of the same event.
func TestRosterPutChecksTheAgentIDBeforeTheCertificate(t *testing.T) {
	fp := fpN(0x88)
	e := mtlsEntry(t, "bus-under-test.alpha-1", liveBinding(fp))

	r := auth.NewMemoryRoster()
	if err := r.Put(e); err != nil {
		t.Fatalf("Put: %v", err)
	}
	err := r.Put(e)
	if !errors.Is(err, auth.ErrDuplicateAgentID) {
		t.Fatalf("re-putting the same entry: err = %v, want ErrDuplicateAgentID", err)
	}
	if errors.Is(err, auth.ErrCertFingerprintBound) {
		t.Fatal("the certificate rule was reported for an agent colliding with ITSELF; the id rule must be decided first")
	}
}

// TestEnrolBindsThePresentedCertificate is the task's headline: enrolment
// records the fingerprint of the certificate presented on the enrolling
// connection, as ONE LIVE binding, stamped with the enrolment instant.
//
// MUTATION THAT KILLS IT ALONE: delete the `if req.ClientCertFingerprint != nil`
// block from Service.Enrol.
func TestEnrolBindsThePresentedCertificate(t *testing.T) {
	svc, roster := mtlsService(t)
	fp := fpN(0xAA)

	res, err := svc.Enrol(enrolReqWithCert(t, "alpha", "k-1", &fp))
	if err != nil {
		t.Fatalf("Enrol: %v", err)
	}

	entry := mtlsMustGet(t, roster, res.AgentID)
	if len(entry.CertBindings) != 1 {
		t.Fatalf("CertBindings has %d entries, want exactly 1: %+v", len(entry.CertBindings), entry.CertBindings)
	}
	b := entry.CertBindings[0]
	if b.Fingerprint != fp {
		t.Fatalf("bound fingerprint %x, want %x", b.Fingerprint[:4], fp[:4])
	}
	if b.RetiredAt != nil {
		t.Fatalf("the binding an enrolment creates must be LIVE, got RetiredAt = %v", b.RetiredAt)
	}
	// One event, one instant: a binding stamped from a second clock read would
	// disagree with the entry it belongs to.
	if !b.BoundAt.Equal(entry.EnrolledAt) || !b.BoundAt.Equal(entry.Epoch) {
		t.Fatalf("BoundAt %v, EnrolledAt %v, Epoch %v: all three describe the one enrolment and must be the same instant", b.BoundAt, entry.EnrolledAt, entry.Epoch)
	}
	// And it is reachable the way a cross-check will reach it.
	if got, err := svc.AgentIDForClientCertificate(fp); err != nil || got != res.AgentID {
		t.Fatalf("AgentIDForClientCertificate = (%q, %v), want (%q, nil)", got, err, res.AgentID)
	}
}

// TestEnrolWithoutACertificateBindsNothingAndSucceeds pins the accepted-absence
// decision. The listener is tls.RequestClientCert, which REQUESTS and never
// REQUIRES, so a connection with no certificate is the ordinary case; enrolment
// must succeed and bind NOTHING.
//
// It is a guard in BOTH directions. If a later change makes a certificate
// mandatory, this goes red and the change has to argue for itself instead of
// silently locking out every client that has not grown a keypair.
//
// MUTATION THAT KILLS IT ALONE: make Enrol return an error when
// ClientCertFingerprint is nil; or bind a zero-value fingerprint when it is nil
// (the second arm below catches that one, which a success-only assertion would
// miss entirely).
func TestEnrolWithoutACertificateBindsNothingAndSucceeds(t *testing.T) {
	svc, roster := mtlsService(t)

	res, err := svc.Enrol(enrolReqWithCert(t, "alpha", "k-1", nil))
	if err != nil {
		t.Fatalf("enrolment WITHOUT a client certificate must be accepted on this build: %v", err)
	}

	entry := mtlsMustGet(t, roster, res.AgentID)
	if len(entry.CertBindings) != 0 {
		t.Fatalf("no certificate was presented, so nothing may be bound; got %+v", entry.CertBindings)
	}
	// The specific trap: binding the ZERO fingerprint would look like a binding
	// to anything that only counts elements, and would make every
	// certificate-less agent collide with every other on the uniqueness rule.
	var zero [32]byte
	if got, err := svc.AgentIDForClientCertificate(zero); err == nil {
		t.Fatalf("the zero fingerprint resolved to %q; an absent certificate must bind nothing at all", got)
	}
}

// TestCertificateDoesNotInfluenceTheMintedAgentID is INVARIANT 1, stated as a
// test: the certificate supplies a fingerprint and NOTHING else. It must not
// touch the agent id, the name or the suffix, which are the server's.
//
// Two enrolments of the same name with DIFFERENT certificates must get the ids
// the minter would have handed out anyway, in order.
//
// MUTATION THAT KILLS IT ALONE: derive any part of the id from the fingerprint
// in Enrol (for example appending a fingerprint prefix to the name before the
// Mint call, which is the plausible version of this mistake).
func TestCertificateDoesNotInfluenceTheMintedAgentID(t *testing.T) {
	withCert, _ := mtlsService(t)
	withoutCert, _ := mtlsService(t)

	fp1, fp2 := fpN(0xBB), fpN(0xCC)
	a1, err := withCert.Enrol(enrolReqWithCert(t, "alpha", "k-1", &fp1))
	if err != nil {
		t.Fatalf("Enrol 1: %v", err)
	}
	a2, err := withCert.Enrol(enrolReqWithCert(t, "alpha", "k-2", &fp2))
	if err != nil {
		t.Fatalf("Enrol 2: %v", err)
	}

	b1, err := withoutCert.Enrol(enrolReqWithCert(t, "alpha", "k-1", nil))
	if err != nil {
		t.Fatalf("Enrol 1 (no cert): %v", err)
	}
	b2, err := withoutCert.Enrol(enrolReqWithCert(t, "alpha", "k-2", nil))
	if err != nil {
		t.Fatalf("Enrol 2 (no cert): %v", err)
	}

	if a1.AgentID != b1.AgentID || a2.AgentID != b2.AgentID {
		t.Fatalf("the presented certificate changed the minted ids: with certs (%q, %q), without (%q, %q); the id is the server's and no part of it is derived from the certificate (invariant 1)",
			a1.AgentID, a2.AgentID, b1.AgentID, b2.AgentID)
	}
	if a1.AgentID == a2.AgentID {
		t.Fatalf("both enrolments minted %q; the test cannot detect anything if the minter is not advancing", a1.AgentID)
	}
}

// TestEnrolRefusesACertificateAlreadyBoundToAnotherAgent is the write-side rule
// reached the way a client reaches it: through Enrol.
//
// MUTATION THAT KILLS IT ALONE: delete the checkCertFingerprintUnbound call from
// MemoryRoster.Put (the roster behind this service). It is the same mutation as
// the roster-level test above, and both are kept: one pins the rule, this one
// pins that the ENROLMENT PATH actually runs it rather than bypassing the roster.
func TestEnrolRefusesACertificateAlreadyBoundToAnotherAgent(t *testing.T) {
	svc, _ := mtlsService(t)
	fp := fpN(0xDD)

	first, err := svc.Enrol(enrolReqWithCert(t, "alpha", "k-1", &fp))
	if err != nil {
		t.Fatalf("first Enrol: %v", err)
	}

	// A DIFFERENT agent name and a different idempotency key, so nothing but the
	// certificate is shared: this must not be mistaken for an idempotent retry.
	_, err = svc.Enrol(enrolReqWithCert(t, "beta", "k-2", &fp))
	if !errors.Is(err, auth.ErrCertFingerprintBound) {
		t.Fatalf("enrolling a second agent with the same client certificate: err = %v, want ErrCertFingerprintBound", err)
	}
	// The first agent still owns it, unambiguously.
	if got, err := svc.AgentIDForClientCertificate(fp); err != nil || got != first.AgentID {
		t.Fatalf("after the refusal the certificate resolved to (%q, %v), want (%q, nil)", got, err, first.AgentID)
	}
}

// TestEnrolRetryOverADifferentCertificateBindsNothingNew pins the idempotency
// decision recorded on EnrolRequest.ClientCertFingerprint: the fingerprint is
// deliberately NOT part of the same-key-different-payload comparison, and the
// omission fails SAFE because a replay applies nothing.
//
// So a retry arriving over a different certificate returns the original result
// and creates NO binding for the certificate it presented. This test is the
// evidence for that claim, and it goes red if the replay path ever starts
// writing.
//
// MUTATION THAT KILLS IT ALONE: make the replay arm of Enrol fall through to the
// mint/bind path instead of returning prev.result.
func TestEnrolRetryOverADifferentCertificateBindsNothingNew(t *testing.T) {
	svc, _ := mtlsService(t)
	original, attacker := fpN(0xEE), fpN(0xEF)

	// ONE request value, reused, so the retry is byte-identical in every field
	// the idempotency comparison looks at (name, both keys, invite) and differs
	// ONLY in the transport fact. Building a second request with a fresh keypair
	// would be a different PAYLOAD and would be refused as a key reuse, testing
	// nothing about the certificate.
	req := enrolReqWithCert(t, "alpha", "k-1", &original)
	first, err := svc.Enrol(req)
	if err != nil {
		t.Fatalf("first Enrol: %v", err)
	}

	retry := req
	retry.ClientCertFingerprint = &attacker
	again, err := svc.Enrol(retry)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if !again.Replayed || again.AgentID != first.AgentID {
		t.Fatalf("retry gave (%q, replayed=%v), want (%q, replayed=true)", again.AgentID, again.Replayed, first.AgentID)
	}

	// THE POINT: the second certificate is bound to nothing.
	if got, err := svc.AgentIDForClientCertificate(attacker); err == nil {
		t.Fatalf("the retry's certificate resolved to %q; a replay applies nothing and must never create a binding", got)
	}
	// And the original binding is untouched.
	if got, err := svc.AgentIDForClientCertificate(original); err != nil || got != first.AgentID {
		t.Fatalf("the original binding resolved to (%q, %v), want (%q, nil)", got, err, first.AgentID)
	}
}

// TestWALRosterRefusesAFingerprintBoundToAnotherAgentBeforeWriting is the
// uniqueness rule on the DURABLE roster, and it asserts the ORDER as well as the
// outcome.
//
// The refusal must happen BEFORE the write, for the same reason the duplicate-id
// check does: a refusal that reached the log would burn two fsyncs on a record
// that must not exist, and would then arrive at Apply — where the record is
// already durable and the only available handling is to discard it, which is a
// loud recovery-time complaint about something that should never have been
// written at all.
//
// "Before the write" is proved by RESTARTING: the roster is rebuilt from the log
// alone, so if the refused entry is absent after a restart it was never on disk.
// A test that only checked the in-memory roster could not tell a refusal that
// wrote nothing from one that wrote and then declined to apply.
//
// MUTATION THAT KILLS IT ALONE: delete the checkCertFingerprintUnbound call from
// WALRoster.put. Moving the check to AFTER the r.w.Write call kills only the
// restart half, which is why the restart half is here.
func TestWALRosterRefusesAFingerprintBoundToAnotherAgentBeforeWriting(t *testing.T) {
	dir := t.TempDir()
	fp := fpN(0x5A)

	r, l := openRoster(t, dir)
	if err := r.Put(mtlsEntry(t, "bus-under-test.alpha-1", liveBinding(fp))); err != nil {
		t.Fatalf("Put alpha: %v", err)
	}
	err := r.Put(mtlsEntry(t, "bus-under-test.beta-1", liveBinding(fp)))
	if !errors.Is(err, auth.ErrCertFingerprintBound) {
		t.Fatalf("durable Put with a certificate already bound elsewhere: err = %v, want ErrCertFingerprintBound", err)
	}
	if _, ok := r.Get("bus-under-test.beta-1"); ok {
		t.Fatal("the refused agent is in the serving roster")
	}
	if err := l.Close(); err != nil {
		t.Fatalf("closing the log: %v", err)
	}

	// RESTART: rebuild from the log alone. The refused entry must not appear,
	// which is what proves nothing was written for it.
	r2, l2 := openRoster(t, dir)
	defer l2.Close()
	if _, ok := r2.Get("bus-under-test.beta-1"); ok {
		t.Fatal("the refused enrolment came back after a restart, so it WAS written to the log; the check must run before the durable write")
	}
	if _, ok := r2.Get("bus-under-test.alpha-1"); !ok {
		t.Fatal("the accepted enrolment did NOT survive the restart; the test proves nothing if the log is not being replayed")
	}
	if got, err := r2.AgentIDForCertFingerprint(fp); err != nil || got != "bus-under-test.alpha-1" {
		t.Fatalf("after recovery the fingerprint resolved to (%q, %v), want (bus-under-test.alpha-1, nil)", got, err)
	}
}

// TestWALRosterRecoversAnAmbiguousBindingAndRefusesToResolveIt is the case
// certFingerprintOwner's ambiguous arm exists for, reached the ONLY way it is
// reachable: off disk.
//
// Two agents each holding the same fingerprint live cannot be created through
// Put — the test above is why — but a log can carry both, because Apply does not
// run the uniqueness check. It must not: Apply replays records that are ALREADY
// DURABLE, and refusing one there does not un-write it, it only turns a damaged
// log into an outage (invariant 6). So the recovered roster legitimately holds
// the ambiguity, and the READ is what declines to guess.
//
// This is the guard that makes the ambiguous arm non-theoretical. Without it,
// that arm is unreachable code and nothing would notice it being "simplified"
// into returning the first holder.
//
// MUTATION THAT KILLS IT ALONE: make certFingerprintOwner's default arm return
// holders[0].
func TestWALRosterRecoversAnAmbiguousBindingAndRefusesToResolveIt(t *testing.T) {
	dir := t.TempDir()
	fp := fpN(0x6B)

	// Written through a log with NO applier, so nothing interprets or refuses
	// the records on the way past: this is a log that a damaged or hand-edited
	// history could genuinely present at recovery.
	l := openPlainLog(t, dir)
	for _, id := range []string{"bus-under-test.alpha-1", "bus-under-test.beta-1"} {
		body, err := auth.Encode(mtlsEntry(t, id, liveBinding(fp)))
		if err != nil {
			t.Fatalf("Encode(%s): %v", id, err)
		}
		if _, err := l.Write(wal.Entry{Kind: auth.RecordKind, Body: body}); err != nil {
			t.Fatalf("writing %s: %v", id, err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatalf("closing the log: %v", err)
	}

	r, l2 := openRoster(t, dir)
	defer l2.Close()
	// Both recovered: recovery admits what is durable.
	for _, id := range []string{"bus-under-test.alpha-1", "bus-under-test.beta-1"} {
		if _, ok := r.Get(id); !ok {
			t.Fatalf("%s did not recover; recovery must not discard a durable record over this", id)
		}
	}
	// And the fingerprint names NOBODY.
	got, err := r.AgentIDForCertFingerprint(fp)
	if !errors.Is(err, auth.ErrCertBindingAmbiguous) {
		t.Fatalf("a fingerprint recovered against two agents resolved to (%q, %v), want ErrCertBindingAmbiguous", got, err)
	}
	if got != "" {
		t.Fatalf("it resolved to %q; an ambiguous certificate must name nobody rather than the first holder found", got)
	}
}

// TestRefusedCertBindingBurnsNoAgentIDSuffix is the regression guard for the
// security gate's MEDIUM finding on MTLS-BIND.
//
// The certificate-uniqueness rule lives in Roster.Put, which runs AFTER
// s.minter.Mint. Relying on it alone meant every refused enrolment still burned
// an agent-id suffix — and suffix floors are NEVER RECLAIMED and are rewritten
// and fsynced on every mint, so an attacker holding ONE enrolled certificate
// could loop with fresh names and grow that file without bound while the roster
// stayed at a single agent. Neither the roster capacity bound nor INVITE-GATE
// stops it: a refused enrolment never reaches the roster, and this path releases
// the invite reservation, so one invite drives the loop indefinitely. The gate
// reproduced it at 300 refusals -> 301 names persisted, floors 110 B -> 19.5 KB.
//
// # WHAT IT MEASURES, AND WHY THE OBVIOUS ASSERTION IS USELESS HERE
//
// The first version of this test enrolled one victim, ran refusals under FRESH
// names, then checked the victim's next suffix. It PASSED UNDER THE MUTATION and
// was therefore worthless: suffix counters are PER NAME, so burning attacker0..N
// never moves the victim's counter. The damage is not a higher suffix, it is one
// PERMANENT ENTRY PER NAME in a file nothing ever prunes.
//
// So this runs against the DURABLE allocator and measures the artefact that
// actually grows: the size of <data-dir>/agent-suffixes.
//
// MUTATION THAT KILLS IT ALONE: delete the pre-mint
// `if req.ClientCertFingerprint != nil` block from Enrol, leaving only Put's
// (still authoritative) refusal. Verified RED under exactly that mutation.
func TestRefusedCertBindingBurnsNoAgentIDSuffix(t *testing.T) {
	dir := t.TempDir()
	suffixes, err := ids.OpenNameSuffixes(dir)
	if err != nil {
		t.Fatalf("ids.OpenNameSuffixes: %v", err)
	}
	// SEALED IMMEDIATELY, and that is legitimate ONLY because this directory is
	// brand new. Seal() means "the floors are proven"; on a t.TempDir() there is
	// no history to derive them from, so the empty map IS the proof. Existed()
	// is asserted rather than assumed, because sealing a directory that DOES
	// have history is the one call that re-mints live agent ids (invariant 1),
	// and a test that did it silently would be modelling the disaster.
	if suffixes.Existed() {
		t.Fatalf("test fixture: %s already has suffix history; this test requires a fresh directory", suffixes.Path())
	}
	if err := suffixes.Seal(); err != nil {
		t.Fatalf("sealing a fresh floors file: %v", err)
	}
	minter, err := ids.NewAgentIDMinter("bus-under-test", suffixes)
	if err != nil {
		t.Fatalf("ids.NewAgentIDMinter: %v", err)
	}
	roster := auth.NewMemoryRoster()
	svc, _ := newService(t, auth.Options{Minter: minter, Roster: roster})

	fp := fpN(0x9E)
	if _, err := svc.Enrol(enrolReqWithCert(t, "victim", "k-0", &fp)); err != nil {
		t.Fatalf("first Enrol: %v", err)
	}

	// Floors() is the authoritative view of what the allocator has PERMANENTLY
	// recorded — one entry per name, never reclaimed. Counting entries is
	// stricter than measuring the file, and it names the thing that grows.
	before := len(suffixes.Floors())
	beforeStat, err := os.Stat(suffixes.Path())
	if err != nil {
		t.Fatalf("stat %s: %v", suffixes.Path(), err)
	}

	// The attack shape: a run of refusals, each under a FRESH name, because a
	// repeated name would collide on one counter instead of adding an entry.
	const refusals = 40
	for i := 0; i < refusals; i++ {
		_, err := svc.Enrol(enrolReqWithCert(t, fmt.Sprintf("attacker%d", i), fmt.Sprintf("k-a%d", i), &fp))
		if !errors.Is(err, auth.ErrCertFingerprintBound) {
			t.Fatalf("refusal %d: err = %v, want ErrCertFingerprintBound", i, err)
		}
	}

	after := len(suffixes.Floors())
	afterStat, err := os.Stat(suffixes.Path())
	if err != nil {
		t.Fatalf("stat %s after refusals: %v", suffixes.Path(), err)
	}
	if after != before {
		t.Fatalf("the suffix allocator recorded %d names before and %d after %d REFUSED enrolments; a refusal must burn no agent-id suffix, because floors are NEVER reclaimed (security gate MEDIUM, MTLS-BIND)",
			before, after, refusals)
	}
	if afterStat.Size() != beforeStat.Size() {
		t.Fatalf("the floors file grew from %d to %d bytes across %d refused enrolments", beforeStat.Size(), afterStat.Size(), refusals)
	}
	// The roster never grew either — the refusals really were refusals, so the
	// test cannot pass by the enrolments having quietly succeeded.
	if n := roster.Len(); n != 1 {
		t.Fatalf("roster holds %d agents, want 1", n)
	}
}

// TestEnrolRefusesWhenTheCertificateResolvesAmbiguously covers the pre-mint
// check's DEFAULT arm — the one that refuses anything that is not "nobody holds
// it".
//
// It exists because the security gate's re-verification found the arm UNTESTED:
// mutating `default: return EnrolResult{}, err` to fall through left the whole
// ./internal/auth suite GREEN. Put still refuses, so the impact was bounded to
// one burned suffix — but a guard no mutation can kill is not a guard, and this
// task has already shipped two of those and had to rewrite both.
//
// The ambiguous state is built through the double whose Put does NOT enforce
// uniqueness, because it is reachable in production only off disk (recovery
// replays already-durable records — invariant 6; see certFingerprintOwner).
//
// WHY AMBIGUOUS MUST REFUSE RATHER THAN PROCEED: "more than one agent holds
// this certificate" is emphatically not "the certificate is free". Reading it
// as free would bind a certificate that already resolves to nobody to a THIRD
// agent, making the ambiguity worse at exactly the moment an operator is trying
// to resolve it.
//
// MUTATION THAT KILLS IT ALONE: change the pre-mint check's `default` arm to
// fall through instead of returning the error.
func TestEnrolRefusesWhenTheCertificateResolvesAmbiguously(t *testing.T) {
	roster := newStubRoster()
	fp := fpN(0xAC)
	for _, id := range []string{"bus-under-test.alpha-1", "bus-under-test.beta-1"} {
		if err := roster.Put(mtlsEntry(t, id, liveBinding(fp))); err != nil {
			t.Fatalf("seeding %s: %v", id, err)
		}
	}
	svc, _ := newService(t, auth.Options{Roster: roster})

	_, err := svc.Enrol(enrolReqWithCert(t, "newcomer", "k-1", &fp))
	if !errors.Is(err, auth.ErrCertBindingAmbiguous) {
		t.Fatalf("enrolling with an AMBIGUOUSLY-bound certificate: err = %v, want ErrCertBindingAmbiguous", err)
	}
	// The roster is untouched: the refusal happened before anything was written,
	// and it did not quietly become a third holder.
	if n := roster.Len(); n != 2 {
		t.Fatalf("roster holds %d entries, want the 2 it was seeded with; the refusal must add nothing", n)
	}
}
