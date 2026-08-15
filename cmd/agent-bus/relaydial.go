package main

// relaydial.go builds the OUTBOUND, PINNED, MUTUALLY-AUTHENTICATED http.Client
// the relay forwarder dials peers with (RELAY-24-BLOCKER-EGRESS).
//
// # IT CONTAINS NO TLS POLICY OF ITS OWN, AND THAT IS THE WHOLE POINT
//
// agent-bus certificates are self-signed and there is no CA and no
// trust-on-first-use anywhere in the design (invariant 11), so the only way
// crypto/tls can check a 32-byte pin is to disable the default chain check and
// supply VerifyPeerCertificate. That literal is permitted in EXACTLY ONE FILE —
// client/pin.go — so writing it here would be a SECOND occurrence, which
// invariant 11 refuses on its own terms.
//
// AND IT WOULD NOT GET PAST THIS PACKAGE'S OWN GUARD, WHICH IS STRICTER THAN THE
// ONE IN client/. cmd/agent-bus is NOT unscanned: scanPlaintextListener
// (cmd/agent-bus/tlslisten_test.go, driven by TestCmdHasNoPlaintextListener)
// parses every non-test .go file in this directory and flags any reference to
// the identifier OUTRIGHT — there is no paired-VerifyPeerCertificate exception
// here, so injecting one into this file fails that test. The direction
// client/guard_test.go genuinely does not cover is internal/relay, which is what
// makes a pinned dialler there the strictly worse option (invariant 11: "pushed
// into a package the guard does not scan").
//
// So this file writes none. It calls client.PinnedTLSConfig, which is a thin
// export of the same function client/pin.go has always had, and it adds ZERO new
// InsecureSkipVerify occurrences to the tree. See DECISIONS.md (2026-08-15).
//
// # THE PIN IS RESOLVED BY ADDRESS, AT DIAL TIME. READ THIS BEFORE SIMPLIFYING
//
// relay.Client holds ONE *http.Client for ALL peers, while every peer has its
// OWN relay.PeerRecord.NextHopTLSCertFingerprint. Putting every peer's pin into
// one BusPinSet on one tls.Config would mean peer A's certificate is accepted
// when dialling peer B — a cross-peer confusion hole produced by entirely
// correct data, combined wrongly. It would also pass every test that only ever
// checks that a peer connects.
//
// NextHopTLSCertFingerprint is the OUTBOUND, ADDRESS-KEYED pin: it names the
// certificate served by whatever answers at the record's BaseURL, which for a
// non-adjacent destination is a DIFFERENT BUS from the record's BusID (see the
// field's own doc). Its mirror image — relay.BusTrustRecord.PeerClientTLSCertFingerprint
// — is INBOUND and BUS-PRINCIPAL-keyed, and conflating the two is a refuted
// design that appears to work and is not secure.
//
// The keying here therefore follows the field: http.Transport.DialTLSContext is
// handed the ADDRESS being dialled, that address selects a pin set, and the
// handshake is verified against that set alone.
//
// # AN ADDRESS WITH NO CONFIGURED PIN FAILS CLOSED
//
// It is refused before a socket is opened, with an error naming the address and
// the remedy. There is deliberately no fall-through to an unpinned or default
// config: that fall-through is precisely how a pinning layer comes to be present
// in the code and absent on the wire, with every positive test still green.

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/dodgymike/agent-bus/client"
	"github.com/dodgymike/agent-bus/internal/buscert"
	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/relay"
)

// peerDialTimeout bounds ONE TCP connect plus its TLS handshake.
//
// It is a belt to the forwarder's own per-attempt context (ForwarderOptions.Timeout,
// which is already the deadline on the whole request): this bound exists so a
// dialler built for some other caller, or a peer that completes the TCP connect
// and then goes silent mid-handshake, cannot park a goroutine indefinitely.
const peerDialTimeout = 15 * time.Second

// peerPinsByAddress is the outbound pin table: dial address -> the set of
// certificates acceptable AT THAT ADDRESS.
//
// The key is the canonical dial address (see peerDialAddress), NOT a bus id, for
// the reason in this file's header.
type peerPinsByAddress map[string]client.BusPinSet

// newPeerPinsByAddress builds the outbound pin table from the durable peer
// configuration.
//
// # WHY THE VALUE IS A SET AND NOT ONE FINGERPRINT
//
// Two legitimate reasons, and they are different from one another:
//
//   - ROTATION. A bus serves TWO certificates during a rollover (invariant 11),
//     and client.BusPinSet exists to model exactly that: normally one member,
//     two for the duration. An operator mid-rollover writes both.
//   - NEXT-HOP SHARING. Route records are keyed by DESTINATION, so N records
//     with N different BusIDs can share ONE BaseURL when they route through one
//     adjacent bus. Every one of those records pins the SAME next hop, so they
//     should agree — but PeerRecord's own doc warns that the duplicated value
//     CAN diverge, typically when a rotation was applied to some records and not
//     others.
//
// A divergence is therefore reported at WARN rather than silently unioned away
// or fatally refused: refusing would take a whole federation down over one
// stale record, and staying silent would let an accept-set grow one forgotten
// certificate at a time. The union at ONE address is still a set of certificates
// an operator explicitly configured FOR THAT HOP; it never widens what is
// accepted at any OTHER address.
//
// # THE UNION IS CAPPED AT client.MaxBusPins, AND OVER THAT THE ADDRESS IS REFUSED
//
// "It never widens any OTHER address" is true and is NOT sufficient, and an
// earlier version of this comment stopped there. client.NewBusPinSet documents
// itself as deliberately NOT enforcing MaxBusPins — construction is not the
// operator act, growth is — so building the union through it bypassed the bound
// entirely, on a path where the input is N route records rather than one
// operator decision. client/pinset.go gives the reason the bound exists: an
// unbounded, never-pruned accept-set degenerates into "accept every certificate
// this bus has ever had", so a key compromised two rotations ago is still
// honoured, forever, with nothing looking wrong. The intended topology — N
// destinations routed through ONE adjacent hop — is exactly the shape that grows
// such a set, so this is reachable rather than theoretical.
//
// MaxBusPins is 2 because that is the WIDTH OF A ROLLOVER (a bus serves two
// certificates for the duration, invariant 11), not a headroom figure. A third
// distinct certificate at one address is therefore not a rollover; it is stale
// configuration, and the address is REFUSED — the same fail-closed answer an
// unpinned address gets, and for the same reason. It is not TRUNCATED to the
// first two: truncation would decide on the operator's behalf, silently, which
// certificate stops being trusted, which is precisely what MaxBusPins's doc
// refuses to do.
//
// A record with NO pin contributes nothing (the zero value means absent, and
// client.NewBusPinSet drops it). If that leaves an address with an empty set,
// the address is not in the table at all and dialling it fails closed.
func newPeerPinsByAddress(peers []relay.PeerRecord, lg *logging.Logger) peerPinsByAddress {
	byAddr := make(map[string][]client.BusFingerprint, len(peers))
	unpinned := make(map[string][]string)
	for _, rec := range peers {
		addr, err := peerDialAddress(rec.BaseURL)
		if err != nil {
			// NOT fatal for the whole table: one unparseable base URL must not
			// stop every other peer being dialled. It cannot be dialled either
			// way — relay.Client refuses the same URL — so the honest outcome is
			// to say so and carry on.
			lg.Error("a configured peer route has a base URL this bus cannot turn into a dial address, so NOTHING will be forwarded to it; the route is otherwise intact and returns as soon as it is corrected",
				"peer_bus", rec.BusID,
				"base_url", rec.BaseURL,
				"err", err.Error(),
				"remedy", "re-run `agent-bus peer add` for this bus with a bare https origin (scheme, host and optional port, nothing else)",
			)
			continue
		}
		// The ZERO VALUE MEANS ABSENT — PeerRecord.NextHopTLSCertFingerprint's
		// own doc, following invite.Record.CertFingerprint. A pin is OPTIONAL on
		// an active route precisely because records written before the field
		// existed carry none, so this is a legitimate state and not damage.
		if rec.NextHopTLSCertFingerprint == (buscert.Fingerprint{}) {
			unpinned[addr] = append(unpinned[addr], rec.BusID)
			continue
		}
		// A DIRECT CONVERSION, not a re-hash. buscert.Fingerprint and
		// client.BusFingerprint are both [32]byte holding sha256 over the leaf's
		// DER — client/pin.go documents itself as a pinned mirror of
		// internal/buscert precisely because the client package may not import
		// internal/ (invariant 7). Converting the array is what keeps the ONE
		// construction; hashing anything a second time here would produce a
		// second identity for one certificate.
		byAddr[addr] = append(byAddr[addr], client.BusFingerprint(rec.NextHopTLSCertFingerprint))
	}

	table := make(peerPinsByAddress, len(byAddr))
	// Addresses refused for exceeding the pin bound. Tracked so the unpinned
	// report below does not tell an operator "this address has NO pin" about an
	// address that has too MANY — two opposite faults with opposite remedies.
	overPinned := make(map[string]struct{})
	for addr, fps := range byAddr {
		set := client.NewBusPinSet(fps...)
		if set.IsEmpty() {
			continue
		}
		if set.Len() > client.MaxBusPins {
			// FAIL CLOSED, and do NOT truncate. See this function's doc.
			lg.Error("REFUSING a peer address whose configured next-hop certificates exceed the pin bound; NOTHING will be forwarded through it. An accept-set wider than a rollover is not a rollover, and honouring it would mean accepting every certificate this hop has ever been given — including one retired because it was compromised",
				"dial_address", addr,
				"pinned_certificates", set.Len(),
				"max_pinned_certificates", client.MaxBusPins,
				"fingerprints", strings.Join(set.Strings(), ","),
				"remedy", "the routes through this address disagree about its certificate. Decide which one is CURRENT out of band, then re-run `agent-bus peer add` for every route through this address with that -tls-fingerprint; at most two distinct values may be configured at once, and only for the duration of a rollover",
			)
			overPinned[addr] = struct{}{}
			continue
		}
		if set.Len() > 1 {
			lg.Warn("more than one next-hop certificate is pinned for a single peer address; every one of them will be accepted when dialling it. That is correct DURING a certificate rollover and is stale configuration at any other time",
				"dial_address", addr,
				"pinned_certificates", set.Len(),
				"fingerprints", strings.Join(set.Strings(), ","),
				"remedy", "once the rollover has completed, re-run `agent-bus peer add` for every route through this address with the CURRENT -tls-fingerprint, so the retired certificate stops being accepted",
			)
		}
		table[addr] = set
	}

	// Reported per ADDRESS rather than per record: several records sharing one
	// unpinned hop are one operator action, not N.
	for addr, buses := range unpinned {
		if _, over := overPinned[addr]; over {
			// Already refused, loudly, for the opposite reason. A second line
			// saying it has no pin would send the operator the wrong way.
			continue
		}
		if _, pinned := table[addr]; pinned {
			// Some record at this address carries a pin and some do not. The
			// address IS dialable; the point worth making is that the records
			// disagree, which is the same stale-configuration smell as above.
			lg.Warn("some routes through this peer address carry no next-hop TLS certificate pin while others do; the pinned set is used for all of them",
				"dial_address", addr,
				"unpinned_routes", strings.Join(buses, ","),
			)
			continue
		}
		lg.Error("a configured peer address has NO next-hop TLS certificate pin, so this bus will REFUSE TO DIAL IT and nothing will be forwarded through it. There is no CA and no trust-on-first-use (invariant 11): an unpinned peer cannot be authenticated, and connecting anyway would be the silent hole the pin exists to close",
			"dial_address", addr,
			"routes", strings.Join(buses, ","),
			"remedy", "stop the bus and re-run `agent-bus peer add -bus-id <destination> -url <that address> -tls-fingerprint <64 lowercase hex>`, where the fingerprint is sha256 of the DER of the certificate the bus AT THAT ADDRESS SERVES TO US (outbound, keyed to the address) — it is NOT -peer-client-fingerprint, which is the inbound direction",
		)
	}
	return table
}

// peerDialAddress turns a peer's base URL into the address http.Transport will
// hand to DialTLSContext.
//
// It must produce the SAME string net/http's own canonicalAddr does — hostname
// (brackets stripped from IPv6) plus an explicit port, defaulted to 443 for
// https — or every lookup misses and every peer fails closed. The result is
// lowercased, and the lookup lowercases too: DNS names are case-insensitive, and
// a table keyed on the operator's spelling would refuse a dial that differed
// only in case.
//
// A host that is not already ASCII is NOT punycoded here, so a unicode hostname
// yields a key net/http will never ask for and that address fails closed. That
// is the safe direction, it is logged when it happens, and the remedy is to
// configure the ASCII form.
func peerDialAddress(baseURL string) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("unparseable: %v", err)
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return "", fmt.Errorf("scheme is %q, but a bus-to-bus link is always https (invariant 11)", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return "", fmt.Errorf("no host")
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	return strings.ToLower(net.JoinHostPort(host, port)), nil
}

// newPinnedPeerHTTPClient returns the *http.Client the relay forwarder and the
// relay handshake client dial peers with.
//
// ourLeaf is what THIS bus PRESENTS as a TLS client. It is the bus's own
// certificate — buscert mints ONE leaf carrying both ServerAuth and ClientAuth,
// and DECISIONS.md's "one identity, both directions" rules that the certificate
// the bus SERVES is the certificate it PRESENTS when it dials — so there is no
// second key to load and no second identity for a peer to bind.
//
// The returned client has NO Timeout of its own: the forwarder bounds every
// attempt with a context (ForwarderOptions.Timeout), and a client-level timeout
// would be a second, independent deadline that could silently pre-empt it.
func newPinnedPeerHTTPClient(pins peerPinsByAddress, ourLeaf *tls.Certificate, lg *logging.Logger) *http.Client {
	dialer := &net.Dialer{Timeout: peerDialTimeout, KeepAlive: 30 * time.Second}
	tr := &http.Transport{
		// NO PROXY, EXPLICITLY. A proxied https connection is established with
		// CONNECT and would bypass DialTLSContext entirely, taking the pin with
		// it. Peer links are direct or they do not happen.
		Proxy: nil,

		// THE PIN CHECK. Every outbound peer connection in this process goes
		// through this callback, so there is no second path that could dial a
		// peer with a different policy.
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			key := strings.ToLower(addr)
			set, ok := pins[key]
			if !ok || set.IsEmpty() {
				// FAIL CLOSED, BEFORE THE SOCKET. Named at Error because a
				// federation that silently forwards nothing is the failure this
				// whole task exists to remove.
				lg.Error("REFUSING to dial a peer address with no pinned next-hop TLS certificate; nothing is forwarded to it",
					"dial_address", key,
					"remedy", "stop the bus and re-run `agent-bus peer add` for that route with -tls-fingerprint set",
				)
				return nil, fmt.Errorf("relay egress: refusing to dial %s: this bus holds no pinned next-hop TLS certificate for that address, and there is no CA and no trust-on-first-use to fall back on (invariant 11)", key)
			}
			raw, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			// client.PinnedTLSConfig is the ONE pinned configuration in this
			// tree (client/pin.go). It disables the default chain check — there
			// is no CA — and replaces it with the pin check in the same
			// composite literal, and it deliberately sets NO ClientSessionCache,
			// so VerifyPeerCertificate runs on EVERY connection. crypto/tls does
			// not re-verify certificates on a resumed handshake, so adding a
			// cache anywhere on this path would bypass the pin silently.
			conn := tls.Client(raw, client.PinnedTLSConfig(set, ourLeaf))
			if err := conn.HandshakeContext(ctx); err != nil {
				_ = raw.Close()
				return nil, fmt.Errorf("relay egress: pinned TLS handshake with %s failed: %w", key, err)
			}
			return conn, nil
		},

		MaxIdleConns:        32,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     90 * time.Second,
	}
	return &http.Client{Transport: tr}
}
