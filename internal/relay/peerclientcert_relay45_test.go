package relay

// RELAY-45's evidence at the DURABLE layer: binding an INBOUND peer bus's TLS
// client certificate to exactly one adjacent bus principal.
//
// The claim under test is an AUTHORISATION claim resting on a DURABILITY one, so
// these tests exercise real certificates through a real *wal.Log and a real
// withdrawal floor. Three things they are built to catch, all of which are
// silent if they regress:
//
//   - the OUTBOUND next-hop pin (RELAY-41) leaking into inbound identity, which
//     would resolve an inbound busB connection to busC — correct data read
//     backwards;
//   - a revoked binding coming back, which is what makes "revocation" a claim
//     rather than a mechanism;
//   - a second construction of the fingerprint, which never matches anything and
//     presents as a peering configuration fault rather than as a hashing bug.

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/buscert"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// pcBusC is a third bus id, needed because the interesting failures in this file
// are all THREE-party: two bindings that must not be confusable, or a route
// record for a destination that is not the hop.
const pcBusC = "bus-ps-third"

// TestInboundPeerPrincipalBinding is the headline: an operator binds the
// adjacent bus's client certificate, that bus's certificate resolves to that
// bus and to nothing else, the binding SURVIVES A RESTART through the durable
// log, and revoking it takes the admission away.
func TestInboundPeerPrincipalBinding(t *testing.T) {
	dir := t.TempDir()
	remoteCert, remoteFP := psCertFor(t, psRemoteBus)

	st, lg := psOpenStore(t, dir, nil, nil)

	// The binding travels on the TRUST record, keyed by the bus principal — not
	// on a route record, whose bus id is the DESTINATION and may name a bus that
	// is not the one on the wire.
	rec, err := st.PutTrust(BusTrust{
		BusID:                        psRemoteBus,
		SigningKeys:                  []ed25519.PublicKey{psKey(1)},
		PeerClientTLSCertFingerprint: remoteFP,
	})
	if err != nil {
		t.Fatalf("PutTrust with an inbound peer client certificate binding: %v", err)
	}
	if rec.PeerClientTLSCertFingerprint != remoteFP {
		t.Fatalf("stored binding = %s, want %s", rec.PeerClientTLSCertFingerprint, remoteFP)
	}

	got, err := st.InboundPeerPrincipal(remoteCert)
	if err != nil {
		t.Fatalf("InboundPeerPrincipal(the bound certificate): %v", err)
	}
	if got != psRemoteBus {
		t.Fatalf("InboundPeerPrincipal = %q, want %q", got, psRemoteBus)
	}
	// INVARIANT 2: a peer principal names a BUS, never a fully-qualified agent.
	// A '.' here would mean something had handed back an agent id, and every
	// caller comparing it against a bus id would silently stop matching.
	if strings.Contains(got, ".") {
		t.Fatalf("InboundPeerPrincipal = %q; a peer principal is a BARE bus id, never a qualified <bus-id>.<agent-id>", got)
	}

	// A no-op re-bind must be recognised as one, and must NOT be mistaken for a
	// no-op when only the certificate changed — that is the rotation case, and
	// swallowing it would leave the old certificate admitting connections while
	// the operator was told the new one had been recorded.
	rotatedCert, rotatedFP := psCertFor(t, psRemoteBus)
	again, err := st.PutTrust(BusTrust{
		BusID:                        psRemoteBus,
		SigningKeys:                  []ed25519.PublicKey{psKey(1)},
		PeerClientTLSCertFingerprint: remoteFP,
	})
	if err != nil {
		t.Fatalf("re-applying the identical binding: %v", err)
	}
	if again.ConfigSeq != rec.ConfigSeq {
		t.Fatalf("re-applying the identical binding wrote a new generation (config_seq %d -> %d); it must be a no-op", rec.ConfigSeq, again.ConfigSeq)
	}
	rotated, err := st.PutTrust(BusTrust{
		BusID:                        psRemoteBus,
		SigningKeys:                  []ed25519.PublicKey{psKey(1)},
		PeerClientTLSCertFingerprint: rotatedFP,
	})
	if err != nil {
		t.Fatalf("re-binding a rotated client certificate: %v", err)
	}
	if rotated.ConfigSeq == rec.ConfigSeq {
		t.Fatalf("re-binding a ROTATED certificate was swallowed as a no-op at config_seq %d; only the keys were unchanged", rec.ConfigSeq)
	}
	if _, err := st.InboundPeerPrincipal(remoteCert); !errors.Is(err, ErrUnknownInboundPeerCert) {
		t.Fatalf("after rotation the OLD certificate resolved with err = %v, want ErrUnknownInboundPeerCert; a superseded credential must stop admitting", err)
	}
	if got, err := st.InboundPeerPrincipal(rotatedCert); err != nil || got != psRemoteBus {
		t.Fatalf("after rotation the NEW certificate resolved to (%q, %v), want (%q, nil)", got, err, psRemoteBus)
	}

	if err := lg.Close(); err != nil {
		t.Fatalf("closing the log: %v", err)
	}

	// RESTART. A fresh store over the SAME directory replays the log; the
	// binding is configuration, so it must be exactly where it was.
	st2, lg2 := psOpenStore(t, dir, nil, nil)
	defer lg2.Close()
	if got, err := st2.InboundPeerPrincipal(rotatedCert); err != nil || got != psRemoteBus {
		t.Fatalf("after a restart the bound certificate resolved to (%q, %v), want (%q, nil); the binding did not survive replay", got, err, psRemoteBus)
	}

	// REVOCATION. RemoveTrust fsyncs the withdrawal floor OUTSIDE the log before
	// the tombstone is written (RELAY-34), so this is the property that makes
	// "revoked" mean something a discarded tail cannot undo.
	if _, err := st2.RemoveTrust(psRemoteBus); err != nil {
		t.Fatalf("RemoveTrust: %v", err)
	}
	if _, err := st2.InboundPeerPrincipal(rotatedCert); !errors.Is(err, ErrUnknownInboundPeerCert) {
		t.Fatalf("after RemoveTrust the certificate resolved with err = %v, want ErrUnknownInboundPeerCert", err)
	}
	if err := lg2.Close(); err != nil {
		t.Fatalf("closing the log after revocation: %v", err)
	}

	// And it stays revoked across ANOTHER restart, which is the half a
	// log-only withdrawal cannot promise.
	st3, lg3 := psOpenStore(t, dir, nil, nil)
	defer lg3.Close()
	if _, err := st3.InboundPeerPrincipal(rotatedCert); !errors.Is(err, ErrUnknownInboundPeerCert) {
		t.Fatalf("after a restart a REVOKED binding resolved with err = %v, want ErrUnknownInboundPeerCert; a revoked transport credential came back", err)
	}
}

// TestInboundPeerPrincipalRejectsWrongAndUnboundCert is the negative surface, in
// one table plus the cases that need their own store. Every one of them must
// FAIL CLOSED: an error and NO principal, never a fallback.
func TestInboundPeerPrincipalRejectsWrongAndUnboundCert(t *testing.T) {
	dir := t.TempDir()
	st, lg := psOpenStore(t, dir, nil, nil)
	defer lg.Close()

	remoteCert, remoteFP := psCertFor(t, psRemoteBus)
	strangerCert, _ := psCertFor(t, pcBusC)

	if _, err := st.PutTrust(BusTrust{
		BusID:                        psRemoteBus,
		SigningKeys:                  []ed25519.PublicKey{psKey(1)},
		PeerClientTLSCertFingerprint: remoteFP,
	}); err != nil {
		t.Fatalf("PutTrust: %v", err)
	}

	// A bus we TRUST for signing but have NOT bound a certificate for. Its
	// signing pin must not stand in for a transport identity.
	if _, err := st.PutTrust(BusTrust{BusID: psOriginBus, SigningKeys: []ed25519.PublicKey{psKey(2)}}); err != nil {
		t.Fatalf("PutTrust for the signing-only bus: %v", err)
	}
	originCert, _ := psCertFor(t, psOriginBus)

	for _, tc := range []struct {
		name string
		cert *x509.Certificate
		want error
	}{
		{"a certificate no record binds", strangerCert, ErrUnknownInboundPeerCert},
		{"a bus whose SIGNING key is pinned but whose certificate is not bound", originCert, ErrUnknownInboundPeerCert},
		{"no certificate at all", nil, ErrUnknownInboundPeerCert},
		{"a certificate carrying no DER to fingerprint", &x509.Certificate{}, ErrUnknownInboundPeerCert},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := st.InboundPeerPrincipal(tc.cert)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if got != "" {
				t.Fatalf("a refused lookup returned principal %q; a failure must yield NO principal", got)
			}
		})
	}

	// DUPLICATE BINDING: one certificate, a second bus. Refused at the door.
	if _, err := st.PutTrust(BusTrust{
		BusID:                        pcBusC,
		SigningKeys:                  []ed25519.PublicKey{psKey(3)},
		PeerClientTLSCertFingerprint: remoteFP,
	}); !errors.Is(err, ErrPeerClientCertAlreadyBound) {
		t.Fatalf("binding one certificate to a second bus: err = %v, want ErrPeerClientCertAlreadyBound", err)
	}
	// The refusal must be TOTAL: nothing written, and the original binding still
	// resolves to the original bus.
	if _, ok := st.LookupTrust(pcBusC); ok {
		t.Fatalf("the refused duplicate binding still wrote a trust record for %s", pcBusC)
	}
	if got, err := st.InboundPeerPrincipal(remoteCert); err != nil || got != psRemoteBus {
		t.Fatalf("after a refused duplicate the original resolved to (%q, %v), want (%q, nil)", got, err, psRemoteBus)
	}

	// A store with NO data directory cannot consult the durable withdrawal
	// floor, so it must refuse to answer at all rather than admit a peer whose
	// binding may have been revoked.
	floorless, _ := psStore(t, nil)
	if err := floorless.Apply(psCommitted(t, BusTrustRecord{
		BusID:                        psRemoteBus,
		ConfigSeq:                    1,
		State:                        PeerRecordActive,
		SigningKeys:                  []ed25519.PublicKey{psKey(1)},
		PeerClientTLSCertFingerprint: remoteFP,
		UpdatedAt:                    time.Now().UTC(),
	}, 1)); err != nil {
		t.Fatalf("Apply into the floorless store: %v", err)
	}
	if _, ok := floorless.LookupTrust(psRemoteBus); !ok {
		t.Fatalf("the floorless store did not hold the record, so the next assertion would be vacuous")
	}
	if _, err := floorless.InboundPeerPrincipal(remoteCert); err == nil {
		t.Fatalf("a store built without a data directory resolved an inbound principal; it cannot see the withdrawal floor, so a revoked binding could come back")
	}

	// AMBIGUITY, reached the only way it can be: a log that already holds two
	// records binding one fingerprint. PutTrust refuses to create this, so it is
	// forced through Apply, which is what a foreign or older binary's log looks
	// like on replay.
	ambiguous, sink := psStore(t, func(o *PeerStoreOptions) { o.Dir = t.TempDir() })
	at := time.Now().UTC()
	for i, busID := range []string{psRemoteBus, pcBusC} {
		if err := ambiguous.Apply(psCommitted(t, BusTrustRecord{
			BusID:                        busID,
			ConfigSeq:                    uint64(i + 1),
			State:                        PeerRecordActive,
			SigningKeys:                  []ed25519.PublicKey{psKey(byte(i + 1))},
			PeerClientTLSCertFingerprint: remoteFP,
			UpdatedAt:                    at,
		}, uint64(2*i+1))); err != nil {
			t.Fatalf("Apply(%s): %v", busID, err)
		}
	}
	got, err := ambiguous.InboundPeerPrincipal(remoteCert)
	if !errors.Is(err, ErrAmbiguousInboundPeerCert) {
		t.Fatalf("two bindings for one certificate: err = %v, want ErrAmbiguousInboundPeerCert; resolving it by picking one is choosing which bus to impersonate", err)
	}
	if got != "" {
		t.Fatalf("an ambiguous lookup returned principal %q", got)
	}
	// Invariant 6's rule applied to a refusal: it must be LOUD, and it must name
	// both candidates, or an operator cannot find the misconfiguration.
	if logged := sink.String(); !strings.Contains(logged, psRemoteBus) || !strings.Contains(logged, pcBusC) {
		t.Fatalf("the ambiguity refusal did not name both bound buses in the log:\n%s", logged)
	}
}

// TestInboundPeerPrincipalRouteForIsolation is the anti-regression test for the
// premise a review had to refute twice: that RELAY-41's next-hop pin could
// supply inbound peer identity.
//
// The topology is the one that makes it wrong. `peer add -bus-id busB -url
// https://b:8443 -tls-fingerprint <fpB> -route-for busC` writes fpB — busB's,
// the NEXT HOP'S — onto BOTH busB's route and busC's, so one fingerprint sits on
// two records with two different bus ids. Reading those records as an inbound
// principal map would resolve an inbound busB connection to busC.
//
// The test proves the inbound answer is INDEPENDENT of every one of those
// records: it is busB before, during and after the route table says anything at
// all.
func TestInboundPeerPrincipalRouteForIsolation(t *testing.T) {
	dir := t.TempDir()
	st, lg := psOpenStore(t, dir, nil, nil)
	defer lg.Close()

	// busB is the adjacent hop. Its certificate serves BOTH jobs in the real
	// deployment — it is the server certificate we pin for dialling AND the
	// client certificate it presents when it dials us — which is exactly why the
	// two must be looked up in different tables and never inverted into one map.
	bCert, bFP := psCertFor(t, psRemoteBus)

	if _, err := st.PutTrust(BusTrust{
		BusID:                        psRemoteBus,
		SigningKeys:                  []ed25519.PublicKey{psKey(1)},
		PeerClientTLSCertFingerprint: bFP,
	}); err != nil {
		t.Fatalf("binding busB's inbound client certificate: %v", err)
	}

	assertResolvesToB := func(ctx string) {
		t.Helper()
		got, err := st.InboundPeerPrincipal(bCert)
		if err != nil {
			t.Fatalf("%s: InboundPeerPrincipal: %v", ctx, err)
		}
		if got != psRemoteBus {
			t.Fatalf("%s: InboundPeerPrincipal = %q, want %q — inbound identity moved because a ROUTE record changed", ctx, got, psRemoteBus)
		}
	}
	assertResolvesToB("with no route records at all")

	// The adjacent hop's own route, carrying its own certificate as the NEXT-HOP
	// pin. Legitimate, and irrelevant to inbound identity.
	if _, err := st.Put(PeerConfig{BusID: psRemoteBus, BaseURL: psURLGen1, NextHopTLSCertFingerprint: bFP}); err != nil {
		t.Fatalf("Put(busB route): %v", err)
	}
	assertResolvesToB("after busB's own route record")

	// THE AMBIGUITY THAT MAKES FINGERPRINT-FIRST WRONG NEXT DOOR: a `-route-for`
	// record whose BUS ID IS busC and whose pin is busB's. If anything read the
	// route table to answer "who is on this connection", this record is what
	// would make it answer busC.
	if _, err := st.Put(PeerConfig{BusID: pcBusC, BaseURL: psURLGen1, NextHopTLSCertFingerprint: bFP}); err != nil {
		t.Fatalf("Put(busC route-for via busB): %v", err)
	}
	assertResolvesToB("after a -route-for record for busC carrying busB's next-hop pin")

	// Sanity: the two route records really do share one fingerprint under two
	// bus ids. Without this the test above could pass because the fixture was
	// wrong rather than because the code is right.
	bRoute, okB := st.Lookup(psRemoteBus)
	cRoute, okC := st.Lookup(pcBusC)
	if !okB || !okC || bRoute.NextHopTLSCertFingerprint != cRoute.NextHopTLSCertFingerprint {
		t.Fatalf("the fixture did not produce one next-hop pin on two bus ids (busB ok=%v pin=%s, busC ok=%v pin=%s)",
			okB, bRoute.NextHopTLSCertFingerprint, okC, cRoute.NextHopTLSCertFingerprint)
	}

	// Changing the NEXT-HOP pin cannot move inbound identity either — including
	// changing it to a certificate belonging to a DIFFERENT bus.
	cCert, cFP := psCertFor(t, pcBusC)
	if _, err := st.Put(PeerConfig{BusID: pcBusC, BaseURL: psURLGen1, NextHopTLSCertFingerprint: cFP}); err != nil {
		t.Fatalf("re-pinning the busC route: %v", err)
	}
	assertResolvesToB("after the busC route's next-hop pin changed")

	// Removing busB's ROUTE removes nowhere to dial, and nothing else. The
	// inbound principal is a property of the trust binding, so busB may still
	// call US — which is the A <-> B <-> C case where one side is behind NAT.
	if _, err := st.Remove(psRemoteBus); err != nil {
		t.Fatalf("Remove(busB route): %v", err)
	}
	assertResolvesToB("after busB's route was removed entirely")

	// And the converse: busC's certificate resolves to NOBODY, although busC now
	// has a route record whose NEXT-HOP PIN IS THAT VERY CERTIFICATE. A route is
	// not a peering, and an outbound pin is not an inbound credential — this is
	// the single assertion that would go red if anything ever read the route
	// table to answer "who is on this connection".
	if _, err := st.InboundPeerPrincipal(cCert); !errors.Is(err, ErrUnknownInboundPeerCert) {
		t.Fatalf("busC's certificate resolved although only a ROUTE record names it: err = %v, want ErrUnknownInboundPeerCert", err)
	}
}

// TestInboundPeerPrincipalRefusesACertificateBoundToOurOwnBus covers the branch
// a reviewer gate found untested: the one guarding "anyone holding this
// certificate could speak as US".
//
// It is DEFENCE IN DEPTH and the test says so rather than pretending otherwise.
// A record naming our own bus id cannot reach the table at all — applyLocked
// runs ValidatePeerBusID against the local id and refuses it, and so does the
// write path — so the re-check inside InboundPeerPrincipal is unreachable
// through today's wiring. It is kept because the value it guards is about to
// become an authorisation subject, and this test pins BOTH halves: the record is
// refused at the door, AND the certificate resolves to nobody.
func TestInboundPeerPrincipalRefusesACertificateBoundToOurOwnBus(t *testing.T) {
	cert, fp := psCertFor(t, psLocalBus)
	// A Durable is supplied — one that would FAIL if it were ever reached — so
	// that the PutTrust assertion below measures the bus-id refusal and not
	// ErrPeerNotDurable, which write() checks first.
	st, sink := psStore(t, func(o *PeerStoreOptions) {
		o.Dir = t.TempDir()
		o.Durable = ipFailingLog{err: errors.New("no write should reach the log in this test")}
	})

	// Apply never returns an error for a refused record — invariant 6's rule is
	// that a bad record is DISCARDED AND LOGGED, not that recovery stops — so
	// the refusal is asserted through the table and the log.
	if err := st.Apply(psCommitted(t, BusTrustRecord{
		BusID:                        psLocalBus,
		ConfigSeq:                    1,
		State:                        PeerRecordActive,
		SigningKeys:                  []ed25519.PublicKey{psKey(1)},
		PeerClientTLSCertFingerprint: fp,
		UpdatedAt:                    time.Now().UTC(),
	}, 1)); err != nil {
		t.Fatalf("Apply returned an error rather than discarding-and-logging: %v", err)
	}
	if _, ok := st.LookupTrust(psLocalBus); ok {
		t.Fatalf("a trust record naming OUR OWN bus id entered the table; a peer may never assert our namespace (invariant 2)")
	}
	if logged := sink.String(); !strings.Contains(logged, psLocalBus) {
		t.Fatalf("the discard was SILENT; invariant 6 requires it to be logged loudly and specifically:\n%s", logged)
	}

	got, err := st.InboundPeerPrincipal(cert)
	if !errors.Is(err, ErrUnknownInboundPeerCert) {
		t.Fatalf("a certificate bound to our own bus id resolved to (%q, %v), want ErrUnknownInboundPeerCert; anyone holding it could speak as us", got, err)
	}

	// The write path refuses it too, so an operator cannot create the state
	// either. ErrBusIDCollision is ValidatePeerBusID's own sentinel.
	if _, err := st.PutTrust(BusTrust{
		BusID:                        psLocalBus,
		SigningKeys:                  []ed25519.PublicKey{psKey(1)},
		PeerClientTLSCertFingerprint: fp,
	}); !errors.Is(err, ErrBusIDCollision) {
		t.Fatalf("PutTrust for our own bus id: err = %v, want ErrBusIDCollision", err)
	}

	// AND THE LAST LINE OF DEFENCE ITSELF, reached the only way it can be:
	// by putting the record into the table BEHIND both refusals above.
	//
	// This is a WHITE-BOX probe and it is here because the branch it covers is
	// otherwise unreachable — deleting the ValidatePeerBusID re-check inside
	// InboundPeerPrincipal left the whole package green, which means the check
	// that stops "anyone holding this certificate speaks as US" had no evidence
	// behind it at all. A defence whose removal nothing notices is a comment
	// wearing the costume of a control. (Reviewer gate, RELAY-45 round 2.)
	st.mu.Lock()
	st.trust.entries[strings.ToLower(psLocalBus)] = BusTrustRecord{
		BusID:                        psLocalBus,
		ConfigSeq:                    2,
		State:                        PeerRecordActive,
		SigningKeys:                  []ed25519.PublicKey{psKey(1)},
		PeerClientTLSCertFingerprint: fp,
		UpdatedAt:                    time.Now().UTC(),
	}
	st.mu.Unlock()

	if got, err := st.InboundPeerPrincipal(cert); !errors.Is(err, ErrUnknownInboundPeerCert) {
		t.Fatalf("with a self-binding forced into the table, InboundPeerPrincipal returned (%q, %v), want ErrUnknownInboundPeerCert; the last check before a value becomes an authorisation subject did not fire", got, err)
	}
}

// ipFailingLog is a durable log whose Write always fails. It exists to reach the
// one window in which the DURABLE WITHDRAWAL FLOOR is the only thing standing
// between a revoked binding and an admission.
type ipFailingLog struct{ err error }

func (l ipFailingLog) Write(wal.Entry) (wal.Committed, error) { return wal.Committed{}, l.err }

// TestInboundPeerPrincipalHonoursTheWithdrawalFloorNotJustTheTombstone is the
// test that makes the floor check in the resolution path load-bearing, and it
// was written because REMOVING that check left every other test in this file
// green.
//
// The reason is worth stating: after a successful RemoveTrust the table holds a
// TOMBSTONE, and a tombstone is excluded by the ordinary "is it active" filter,
// so a resolver that consulted no floor at all would still refuse. The floor
// only decides the case where an ACTIVE record and a recorded withdrawal
// coexist, which is exactly what RELAY-34 exists for — a discarded tombstone, a
// rolled-back snapshot, or (reached here) a withdrawal whose floor was fsynced
// and whose LOG ENTRY THEN FAILED.
//
// PeerStore.write fsyncs the floor BEFORE the tombstone reaches the log, so in
// that window memory still holds the ACTIVE binding while disk already says the
// operator withdrew it. Disk wins: the peer must not be admitted.
func TestInboundPeerPrincipalHonoursTheWithdrawalFloorNotJustTheTombstone(t *testing.T) {
	cert, fp := psCertFor(t, psRemoteBus)
	st, _ := psStore(t, func(o *PeerStoreOptions) {
		o.Dir = t.TempDir()
		o.Durable = ipFailingLog{err: errors.New("the log is unwritable")}
	})

	// The ACTIVE binding arrives by replay, so it is in the table without any
	// write having succeeded.
	if err := st.Apply(psCommitted(t, BusTrustRecord{
		BusID:                        psRemoteBus,
		ConfigSeq:                    1,
		State:                        PeerRecordActive,
		SigningKeys:                  []ed25519.PublicKey{psKey(1)},
		PeerClientTLSCertFingerprint: fp,
		UpdatedAt:                    time.Now().UTC(),
	}, 1)); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got, err := st.InboundPeerPrincipal(cert); err != nil || got != psRemoteBus {
		t.Fatalf("before the withdrawal the certificate resolved to (%q, %v), want (%q, nil); the rest of this test would be vacuous", got, err, psRemoteBus)
	}

	// The revocation: its floor is fsynced, and then the log write fails. The
	// operator is told the withdrawal did not complete.
	if _, err := st.RemoveTrust(psRemoteBus); err == nil {
		t.Fatalf("RemoveTrust succeeded against an unwritable log; this test needs the failure to reach the window it is about")
	}

	// THE TABLE STILL HOLDS THE ACTIVE RECORD, and no public read path can show
	// that — every one of them consults the floor, which is the whole point.
	// LookupTrust therefore already answers "absent", and it is asserted here so
	// that a future change which folded a TOMBSTONE in on a failed write would
	// make this test fail loudly rather than quietly stop exercising the floor.
	if rec, ok := st.LookupTrust(psRemoteBus); ok {
		t.Fatalf("LookupTrust returned %+v after a failed withdrawal; the floor should be hiding the still-active record, and this test would no longer be about the floor", rec)
	}

	// And it must NOT admit: the durable floor says this configuration was
	// withdrawn, and the durable floor is the truth.
	got, err := st.InboundPeerPrincipal(cert)
	if !errors.Is(err, ErrUnknownInboundPeerCert) {
		t.Fatalf("a binding at or below the durable withdrawal floor resolved to (%q, %v), want ErrUnknownInboundPeerCert; a revoked transport credential was admitted from memory", got, err)
	}
}

// TestPeerClientCertBindingIsRefusedOnATombstoneAndInMalformedSpellings pins the
// record-level rules an operator surface and a corrupt log both have to meet:
// one textual spelling, no all-zero value, and NO LIVE CREDENTIAL ON A WITHDRAWN
// PRINCIPAL.
func TestPeerClientCertBindingIsRefusedOnATombstoneAndInMalformedSpellings(t *testing.T) {
	at := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	_, fp := psCertFor(t, psRemoteBus)

	// A tombstone that still binds a certificate is refused in BOTH directions:
	// on the way out, so it can never be written, and on the way in, so a
	// hand-edited or foreign record cannot reintroduce it.
	tomb := BusTrustRecord{
		BusID:                        psRemoteBus,
		ConfigSeq:                    9,
		State:                        PeerRecordRemoved,
		PeerClientTLSCertFingerprint: fp,
		UpdatedAt:                    at,
	}
	if _, err := tomb.Encode(); !errors.Is(err, ErrInvalidPeerRecord) {
		t.Fatalf("encoding a tombstone that binds a certificate: err = %v, want ErrInvalidPeerRecord", err)
	}

	// Built by hand rather than by Encode, precisely because Encode refuses it.
	handWritten := map[string]interface{}{
		"v": PeerRecordVersion, "rec": BusTrustRecordKind,
		"bus_id": psRemoteBus, "config_seq": 9, "state": "removed",
		"peer_client_tls_cert_sha256": fp.String(),
		"updated_at":                  at.Format(time.RFC3339Nano),
	}
	body, err := json.Marshal(handWritten)
	if err != nil {
		t.Fatalf("marshalling the hand-written record: %v", err)
	}
	if _, err := DecodeBusTrustRecord(body); !errors.Is(err, ErrInvalidPeerRecord) {
		t.Fatalf("decoding a tombstone that binds a certificate: err = %v, want ErrInvalidPeerRecord", err)
	}

	// The active record's own round trip: exactly one lowercase-hex spelling on
	// disk, under a key that says PEER CLIENT and not NEXT HOP.
	active := psTrust(psRemoteBus, 3, at, psKey(1))
	active.PeerClientTLSCertFingerprint = fp
	encoded, err := active.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &raw); err != nil {
		t.Fatalf("unmarshalling the encoded record: %v", err)
	}
	if _, wrong := raw["next_hop_tls_cert_sha256"]; wrong {
		t.Fatalf("a TRUST record carries next_hop_tls_cert_sha256; the inbound binding must never share the outbound key:\n%s", encoded)
	}
	var spelled string
	if err := json.Unmarshal(raw["peer_client_tls_cert_sha256"], &spelled); err != nil {
		t.Fatalf("peer_client_tls_cert_sha256 is not a JSON string: %v", err)
	}
	if spelled != fp.String() || spelled != strings.ToLower(spelled) || len(spelled) != 2*buscert.DigestSize {
		t.Fatalf("peer_client_tls_cert_sha256 = %q, want the %d-character LOWERCASE hex form %q", spelled, 2*buscert.DigestSize, fp)
	}
	back, err := DecodeBusTrustRecord(encoded)
	if err != nil {
		t.Fatalf("DecodeBusTrustRecord: %v", err)
	}
	if back.PeerClientTLSCertFingerprint != fp {
		t.Fatalf("round trip lost the binding: got %s, want %s", back.PeerClientTLSCertFingerprint, fp)
	}

	// The one validator, and the spellings it refuses. Uppercase is rejected
	// rather than folded (buscert.ParseFingerprint's rule), and the all-zero
	// digest is rejected rather than read as "absent" — which is the fail-OPEN
	// reading of a credential field.
	for _, tc := range []struct{ name, text string }{
		{"empty", ""},
		{"all zero", strings.Repeat("0", 2*buscert.DigestSize)},
		{"uppercase", strings.ToUpper(fp.String())},
		{"too short", fp.String()[:2*buscert.DigestSize-1]},
		{"not hex", strings.Repeat("z", 2*buscert.DigestSize)},
		{"colon separated", strings.Join([]string{fp.String()[:2], fp.String()[2:]}, ":")},
	} {
		t.Run("refuses a "+tc.name+" fingerprint", func(t *testing.T) {
			if _, err := ParsePeerClientTLSFingerprint(tc.text); !errors.Is(err, ErrInvalidPeerRecord) {
				t.Fatalf("ParsePeerClientTLSFingerprint(%q) err = %v, want ErrInvalidPeerRecord", tc.text, err)
			}
		})
	}

	// And the same refusals reached through the DURABLE decoder, so a record on
	// disk cannot carry a spelling the validator rejects.
	for _, tc := range []struct{ name, text string }{
		{"all zero", strings.Repeat("0", 2*buscert.DigestSize)},
		{"uppercase", strings.ToUpper(fp.String())},
	} {
		t.Run("the decoder refuses a "+tc.name+" fingerprint", func(t *testing.T) {
			// A REAL pinned key, so the ONLY thing wrong with this record is the
			// fingerprint. With an empty key list validate would refuse it for
			// the other reason and the assertion would pass vacuously.
			doc := map[string]interface{}{
				"v": PeerRecordVersion, "rec": BusTrustRecordKind,
				"bus_id": psRemoteBus, "config_seq": 3, "state": "active",
				"bus_signing_keys":            []string{base64.StdEncoding.EncodeToString(psKey(1))},
				"peer_client_tls_cert_sha256": tc.text,
				"updated_at":                  at.Format(time.RFC3339Nano),
			}
			b, err := json.Marshal(doc)
			if err != nil {
				t.Fatalf("marshalling: %v", err)
			}
			if _, err := DecodeBusTrustRecord(b); !errors.Is(err, ErrInvalidPeerRecord) {
				t.Fatalf("DecodeBusTrustRecord with a %s fingerprint: err = %v, want ErrInvalidPeerRecord", tc.name, err)
			}
		})
	}
}

// TestPeerClientCertBindingIsInvisibleToAnOlderBinaryOnlyByRefusal documents the
// downgrade cost where it is checked rather than only where it is written: the
// field is additive, so this binary reads every record an older one wrote, and
// an older binary REFUSES a record carrying this field rather than silently
// dropping the binding. Refusal is the safe direction; silent dropping would
// un-bind a peer without saying so.
func TestPeerClientCertBindingIsInvisibleToAnOlderBinaryOnlyByRefusal(t *testing.T) {
	at := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)

	// The "older binary" is modelled by the strictness this decoder already has:
	// DisallowUnknownFields. A record with an unknown key is refused, which is
	// the same answer an older build gives to peer_client_tls_cert_sha256.
	doc := map[string]interface{}{
		"v": PeerRecordVersion, "rec": BusTrustRecordKind,
		"bus_id": psRemoteBus, "config_seq": 3, "state": "active",
		"bus_signing_keys":        []string{base64.StdEncoding.EncodeToString(psKey(1))},
		"a_field_from_the_future": "x",
		"updated_at":              at.Format(time.RFC3339Nano),
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	if _, err := DecodeBusTrustRecord(b); !errors.Is(err, ErrInvalidPeerRecord) {
		t.Fatalf("an unknown field decoded without refusal: err = %v", err)
	}

	// A v1 record WITHOUT the field still decodes: the version was deliberately
	// not bumped, so every trust record already on disk stays readable.
	old := psTrust(psRemoteBus, 4, at, psKey(1))
	encoded, err := old.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if strings.Contains(string(encoded), "peer_client_tls_cert_sha256") {
		t.Fatalf("a record with no binding still wrote the key; absent must have exactly one spelling:\n%s", encoded)
	}
	back, err := DecodeBusTrustRecord(encoded)
	if err != nil {
		t.Fatalf("decoding a record with no binding: %v", err)
	}
	if back.PeerClientTLSCertFingerprint != (buscert.Fingerprint{}) {
		t.Fatalf("a record with no binding decoded to fingerprint %s, want the zero value", back.PeerClientTLSCertFingerprint)
	}
}

// TestPeerClientCertBindingSurvivesATornWALTail is the durability half stated as
// a crash property rather than as a Close: the binding is written, the log's
// TAIL is damaged the way recovery must tolerate, and what the store holds
// afterwards is a PREFIX of the accepted history — never a resurrected or
// half-applied credential.
func TestPeerClientCertBindingSurvivesATornWALTail(t *testing.T) {
	dir := t.TempDir()
	cert, fp := psCertFor(t, psRemoteBus)

	st, lg := psOpenStore(t, dir, nil, nil)
	if _, err := st.PutTrust(BusTrust{
		BusID:                        psRemoteBus,
		SigningKeys:                  []ed25519.PublicKey{psKey(1)},
		PeerClientTLSCertFingerprint: fp,
	}); err != nil {
		t.Fatalf("PutTrust: %v", err)
	}
	if err := lg.Close(); err != nil {
		t.Fatalf("closing the log: %v", err)
	}

	// Eight bytes off the tail: the same damage TestPeerStoreTrustSurvivesATornWALTail
	// uses. Recovery discards the damaged record and the bus still starts
	// (invariant 6).
	psTruncateTail(t, dir, 8)

	st2, lg2 := psOpenStore(t, dir, nil, nil)
	defer lg2.Close()
	// ANTI-VACUOUS: without an actual discard the assertions below would be
	// measured against an intact log and would prove nothing.
	if rec := lg2.Recovered(); len(rec.Dangling) == 0 && rec.DiscardCount == 0 {
		t.Fatalf("recovery reported neither a dangling prepare nor a discard (%+v); the truncation damaged nothing", rec)
	}
	// The binding's own record was the tail, so it is GONE — and gone means the
	// certificate admits nobody. What must never happen is the opposite: a
	// half-read record admitting a peer the operator never finished configuring,
	// or admitting a DIFFERENT bus.
	got, err := st2.InboundPeerPrincipal(cert)
	if err == nil {
		t.Fatalf("a torn tail left the certificate resolving to %q; a damaged record must never authorise a peer", got)
	}
	if !errors.Is(err, ErrUnknownInboundPeerCert) {
		t.Fatalf("after a torn tail: err = %v, want ErrUnknownInboundPeerCert", err)
	}

	// And the store is still WRITABLE — recovery reached a running bus — so the
	// operator can simply re-apply the binding.
	if _, err := st2.PutTrust(BusTrust{
		BusID:                        psRemoteBus,
		SigningKeys:                  []ed25519.PublicKey{psKey(1)},
		PeerClientTLSCertFingerprint: fp,
	}); err != nil {
		t.Fatalf("re-applying the binding after a torn tail: %v", err)
	}
	if got, err := st2.InboundPeerPrincipal(cert); err != nil || got != psRemoteBus {
		t.Fatalf("after re-applying, the certificate resolved to (%q, %v), want (%q, nil)", got, err, psRemoteBus)
	}
}
