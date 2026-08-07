// Package buscert owns the bus's two long-lived keypairs and the certificate
// that carries one of them: it loads them from the data directory, and mints
// them exactly once, on a virgin data directory.
//
// # Two keypairs, and why they are not one
//
// A bus holds TWO independent Ed25519 keypairs (DECISIONS.md, 2026-08-07, "The
// bus TLS key and the bus SIGNING key are SEPARATE"):
//
//   - The TLS key authenticates the CONNECTION. It lives inside a self-signed
//     certificate whose fingerprint travels in the invite blob (E6) and is
//     PINNED BY CLIENTS. There is no CA and no trust-on-first-use anywhere.
//   - The SIGNING key attests AGENT KEY BUNDLES. It is PINNED BY PEER BUSES at
//     peering time, and is what makes a relayed signature verifiable at the far
//     end.
//
// One key would be simpler and is the wrong answer, because the two rotations
// have incompatible blast radii. Rotating the TLS key affects the clients of
// this bus, and the two-certificate rollover (E3) makes that non-disruptive.
// Rotating the signing key invalidates the pins held by EVERY PEER BUS, which
// is a federation-wide event. Sharing one key would drag every routine TLS
// rotation up to that cost, and the predictable result is that neither key ever
// gets rotated. Separating them also keeps the failure domains apart: a
// compromised TLS key impersonates the bus to clients; a compromised signing
// key forges attestations for every agent on the bus. One key makes the lesser
// compromise automatically become the greater one.
//
// The two keys are generated from two independent calls to
// ed25519.GenerateKey(rand.Reader). Neither is EVER derived from the other.
//
// # The three files
//
//	bus-tls.crt      0644  PUBLIC   PEM CERTIFICATE  the self-signed leaf
//	bus-tls.key      0600  SECRET   PEM PRIVATE KEY  PKCS#8 Ed25519, the TLS key
//	bus-signing.key  0600  SECRET   PEM PRIVATE KEY  PKCS#8 Ed25519, the signing key
//
// The certificate is public BY CONSTRUCTION — it is sent to every client in
// every handshake — so its mode is not security-relevant, and forcing 0600 on
// it would collide with an operator-supplied certificate mounted 0644 (E7).
// Both key files are 0600 and are REFUSED if any group or other bit is set.
//
// # A backup that omits a secret yields a bus that cannot do its job
//
// After this package lands the data directory holds THREE long-lived secrets:
// wal-mac.key (owned by internal/wal), bus-tls.key and bus-signing.key. Stated
// explicitly because it is the operational failure that is easy to walk into:
//
//   - without wal-mac.key, no record in the WAL or the audit log verifies;
//   - without bus-tls.key, the bus cannot present the certificate its clients
//     pinned, and cannot be started against that certificate at all;
//   - without bus-signing.key, every attestation this bus ever made is
//     unverifiable and every peer's pin is dead.
//
// None of the three can be regenerated. A data-directory backup that copies the
// logs but skips the keys restores a bus that cannot do its job.
//
// # Generation happens ONCE, on a virgin directory
//
// LoadOrCreate generates only when ALL THREE files are absent. If some are
// present and some are not, that is FATAL (ErrIncomplete) and names exactly
// which are missing and which are present. A missing file is NEVER regenerated
// next to surviving ones, because minting a new TLS key silently breaks every
// client that pinned the old fingerprint, and minting a new signing key
// invalidates the pins held by every peer bus — a federation-wide event. A
// half-written first start lands in the same place; the remedy there is for an
// OPERATOR to remove the named files and restart. Removing them is a deliberate
// operator act and is never something the bus does on its own.
//
// # The fingerprint construction is fixed
//
// A bus certificate's fingerprint is sha256.Sum256(cert.Raw) — the DER of the
// leaf — rendered as lowercase hex. That exact construction is fixed by the
// ENROL-SHAPE decision and is already mirrored at internal/auth.CertBinding and
// internal/invite.Record.CertFingerprint. A fingerprint over the SPKI, or over a
// PEM encoding, would be a SECOND INCOMPATIBLE IDENTITY for the same
// certificate, so this package offers exactly one and no alternative.
//
// # What this package deliberately is not
//
//   - UNWIRED as shipped. Nothing imports it yet; MTLS-LISTENER wires it. When
//     it does, the load must happen AFTER the data-directory lock is taken in
//     cmd/agent-bus/main.go and BEFORE the listener is created, because
//     TestRunRefusesALockedDataDir pins that a lock-refused start touches
//     nothing in the data directory but bus.lock — and generation writes three
//     files.
//   - No operator-supplied certificates. E7 decided they are allowed; that is
//     NOT implemented here. Today the material is always self-generated.
//   - No rotation. E3's two-certificates-during-rollover is a separate task and
//     does not exist yet, so certificate expiry is a scheduled outage. The
//     mitigation available now is that NotAfter is exported, so startup can log
//     the remaining life while there is still time to act on it.
//   - No plaintext or insecure hatch of any kind, not a flag, not an env var,
//     not a build tag, not a test hook (E7, 2026-08-02). Options.Now exists only
//     so a test can drive the validity window; it is the single hook and there
//     will not be a second.
package buscert
