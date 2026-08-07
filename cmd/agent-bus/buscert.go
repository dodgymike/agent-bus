package main

// Startup load of the bus's own cryptographic identity (MTLS-BUSCERT): the
// self-signed BUS CERTIFICATE it presents, the private key inside it, and the
// SEPARATE Ed25519 signing key that attests agent key bundles to peer buses.
//
// The material itself is internal/buscert. Everything in this file is the
// startup half of it: choose the subject alternative names, call LoadOrCreate
// once, and turn "this call minted fresh key material" into exactly one loud,
// once-per-data-directory log line. It is factored out of main.go the way
// suffixfloors.go is, so main.go holds one call site and one summary field.
//
// # SCOPE: GENERATE AND LOAD ONLY. This does NOT serve TLS.
//
// Stated first because it is the thing most likely to be "finished" by mistake.
// Nothing here touches http.Server.TLSConfig, net.Listen, srv.Serve, -listen or
// its default; after this step a plaintext GET /healthz behaves exactly as it
// did before. Switching the listener to TLS is MTLS-LISTENER and it MUST NOT
// land before the client can speak TLS (MTLS-CLIENTCERT) -- server-side
// enforcement ahead of client-side capability is not a rough edge, it is a
// total outage, and this repo has already had one: signature enforcement landed
// first and every send failed with curl exit 7. So the Material returned here
// is deliberately used for LOGGING ONLY today. It is returned rather than
// discarded because the tasks that consume it (the listener, the invite blob's
// pinned fingerprint, peer attestation) all want this exact value, loaded once,
// by the composition root.
//
// # WHY IT RUNS WHERE IT RUNS
//
// After dirlock.Acquire, and that is not negotiable: LoadOrCreate WRITES three
// files into the data directory on a virgin dir, and a start refused AT THE LOCK
// must have touched nothing but bus.lock (TestRunRefusesALockedDataDir). Two
// processes generating key material into one directory is also exactly the race
// the lock exists to stop -- the loser's certificate would be renamed over the
// winner's, and the fingerprint an already-issued invite blob pins would be dead.
//
// After ids.LoadOrCreateBusID, because the bus id is the certificate's
// CommonName. That name is descriptive only -- nothing authenticates on it, the
// pin is over the fingerprint -- but a certificate whose subject names a
// different bus than the one serving it is a debugging trap for free.
//
// BEFORE wal.Open, and that ordering is a deliberate fail-fast. Every failure
// below is FATAL, and wal.Open is the step that may REPAIR or QUARANTINE the
// log. A start that is going to refuse over an expired certificate or a
// group-readable key should refuse before recovery has moved a single byte of
// the log, not after.
//
// # EVERY FAILURE IS FATAL, and none of them regenerates anything
//
// This copies the precedent set for the WAL MAC key (internal/wal/mackey.go,
// DECISIONS.md 2026-08-07): an unusable key file is a refusal to start with a
// message naming the path, never a silent regeneration. internal/buscert is
// where that rule is implemented and argued at length; the short version is that
// a fresh TLS key breaks every client that pinned the old fingerprint (there is
// no TOFU -- E6) and a fresh signing key kills the pin held by every peer bus, a
// federation-wide event. Neither is something a process may decide to do because
// a file was not where it expected. There is NO fallback path here and none may
// be added.

import (
	"fmt"
	"net"
	"time"

	"github.com/dodgymike/agent-bus/internal/buscert"
	"github.com/dodgymike/agent-bus/internal/logging"
)

// openBusCertMaterial loads the bus's certificate and its two private keys from
// dataDir, generating them only on a data directory that holds none of the
// three, and announces the outcome.
//
// The caller must treat any error as FATAL.
func openBusCertMaterial(dataDir, busID, listen string, lg *logging.Logger) (*buscert.Material, error) {
	material, err := buscert.LoadOrCreate(dataDir, buscert.Options{
		BusID: busID,
		Hosts: certHosts(listen),
	})
	if err != nil {
		// buscert's own errors already name the offending PATH and the remedy,
		// which is the first thing an operator needs. This wrapper adds ONLY the
		// standing policy, and says nothing the caller in main.go already says:
		// the two together read "preparing the bus certificate and key material:
		// it is NEVER regenerated ...: buscert: <path> <what is wrong>". An
		// earlier version repeated "the bus certificate and key material in
		// <dir>" here, so the phrase appeared twice before the useful path did.
		return nil, fmt.Errorf("it is NEVER regenerated over a partial or unusable set, because a new TLS key breaks every client that pinned the old certificate fingerprint and a new signing key invalidates the pin held by every peer bus: %w", err)
	}

	// The LEVEL is contract, not decoration, and it follows openSuffixAllocator's
	// precedent exactly: the steady state is INFO, and the once-per-data-directory
	// event that changes what clients must pin is louder.
	//
	// Generated() is true exactly once in the life of a data directory. On a dir
	// that was supposed to already hold material, it means the material was LOST
	// -- and because generation only happens when all three files are absent,
	// this line is also the only notice an operator gets that the fingerprint
	// every client pins has just changed.
	fields := []interface{}{
		"data_dir", dataDir,
		"cert", material.CertPath(),
		"fingerprint", material.Fingerprint().String(),
		"not_after", material.NotAfter().UTC().Format(time.RFC3339),
	}
	if material.Generated() {
		// The two key FILE NAMES are named in the prose, not in fields, and the
		// sentence they sit in says what they are. A field like
		// signing_key=<path> next to cert=<path> reads as one more path to copy
		// around; these two are the bus's identity to its clients and to the
		// whole federation, and the only place they may ever be copied to is a
		// backup that is protected like the keys themselves.
		lg.Warn("bus certificate and signing key GENERATED: this data directory held none of "+
			buscert.CertFileName+", "+buscert.TLSKeyFileName+" or "+buscert.SigningKeyFileName+
			", so a fresh self-signed bus certificate and TWO new private keys were written into it. "+
			"Expected exactly ONCE per data directory. The fingerprint below is what clients must pin, so re-issue any invite blob carrying an older one. "+
			buscert.TLSKeyFileName+" and "+buscert.SigningKeyFileName+" are SECRET (mode 0600) and are NOT regenerable: back them up with wal-mac.key -- a backup missing any one of the three restores a bus that cannot do its job. "+
			"If you see this line on a directory that already had key material, STOP: the material was lost, every pinned client will now fail to connect, and restoring the old files from backup is the only way back",
			fields...)
	} else {
		lg.Info("bus certificate and signing key loaded", fields...)
	}
	return material, nil
}

// certHosts turns the -listen address into the EXTRA subject alternative names
// the certificate needs, on top of the loopback set buscert always includes.
//
// The host half of -listen is added because a client verifying this certificate
// checks the name it dialled against the SANs, and Go dropped the CommonName
// fallback in 1.15 -- so a bus bound to 10.0.0.5 whose certificate names only
// loopback is unusable by anything but itself, and the failure surfaces at the
// client as a name-mismatch with nothing in this bus's log.
//
// The WILDCARD BINDS -- ":8080" (empty host), "0.0.0.0:8080", "[::]:8080" --
// contribute nothing. "Every interface" is not a name any client dials, and
// 0.0.0.0 as an IP SAN matches nothing. They are dropped rather than encoded,
// leaving the loopback set, which is the default bind.
//
// # The trap this cannot close, stated rather than hidden
//
// SANs are baked in at GENERATION, and material is never regenerated. Moving a
// bus from a loopback bind to a public address AFTER its first start therefore
// leaves a certificate that does not name the new address, and the remedy is the
// same as any other certificate change: a deliberate operator act, replacing the
// certificate and key together and re-issuing the invites that pinned the old
// fingerprint. Rotation (E3) is a separate, not-yet-written task. Nothing here
// may paper over that by re-minting on a changed -listen: that would hand every
// restart with a different flag the power to break every pinned client.
func certHosts(listen string) []string {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		// Unreachable from run(): Config.validate has already rejected a -listen
		// that will not split. Treated as "no extra name" rather than as an
		// error, because a certificate with the loopback set is still correct and
		// this is not the place to re-litigate flag validation.
		return nil
	}
	if host == "" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		return nil
	}
	return []string{host}
}
