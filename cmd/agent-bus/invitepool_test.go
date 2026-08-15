package main

// THE END-TO-END INVITE POOL — the test-harness consequence of INVITE-GATE-ENFORCE.
//
// The shipped bus is invite-only (invariant 3), so the end-to-end tests in this
// package can no longer enrol by POSTing a name and a key. Every enrolment needs
// a single-use invite, and an invite can only be minted while the bus is
// STOPPED, because `agent-bus invite mint` takes the data directory's exclusive
// dirlock that a running bus holds.
//
// That ordering is the whole reason this file exists. A test cannot mint at the
// moment it enrols — the bus is running by then — so it mints AHEAD of the start
// it enrols against, and draws from the result. The invites are durable WAL
// records, so they survive every restart the test performs afterwards and one
// preparation covers the whole test.
//
// # This is the operator's real flow, not a test-only shortcut
//
// e2ePrepareInvites goes through runMint, which is the same `invite mint`
// subcommand an operator runs — not a hand-built record and not invite.Store
// called directly. It is exactly the documented sequence:
//
//	start once (the directory gains its identity AND its certificate)
//	stop the bus  ->  agent-bus invite mint  ->  start the bus  ->  enrol
//
// The priming start is not ceremony: an invite pins the bus's CERTIFICATE
// fingerprint (invariant 11 — no CA, no trust-on-first-use), so there is nothing
// to mint against until a certificate exists, and `invite mint` says so and
// refuses. See the comment on that check.
//
// # Why a POOL and not an argument on enrolNewAgent
//
// Seventeen call sites enrol, several of them repeatedly against one data
// directory across several restarts (suffixrestart_test.go enrols the same name
// three times to watch the suffix floor advance). Threading an invite through
// every one of those would have rewritten tests that are about WAL recovery and
// suffix floors, burying what they actually assert. The pool keeps those call
// sites byte-for-byte unchanged: the only edit a test needs is one
// e2ePrepareInvites line before its first startServer.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"

	"github.com/dodgymike/agent-bus/internal/buscert"
)

// e2eInvite is one unspent invite: the id names it, the secret is the bearer
// credential that redeems it.
type e2eInvite struct {
	id     string
	secret string
}

var (
	e2eInviteMu   sync.Mutex
	e2eInvitePool = map[string][]e2eInvite{}
)

// e2ePrepareInvites mints n single-use invites into dataDir and parks them for
// the enrolment helpers to draw on.
//
// IT MUST BE CALLED BEFORE THE FIRST startServer ON THAT DIRECTORY. Minting
// takes the exclusive dirlock, so calling it against a running bus fails — which
// is exactly the constraint an operator lives with, and the reason the failure
// message below names it.
//
// n should be the number of enrolments the test performs, counting every restart
// (invites are durable and unspent ones carry across), plus a little slack:
// running out mid-test surfaces as a confusing enrolment failure rather than as
// a preparation error, so e2eTakeInvite says so explicitly.
func e2ePrepareInvites(t *testing.T, dataDir string, n int) {
	t.Helper()

	// THE DIRECTORY MUST HAVE RUN A BUS ONCE, and writing a bus-id file here is
	// NOT enough — that was the first attempt and `invite mint` refused it with
	// exit 4: "this data directory holds no bus certificate ... so this bus has
	// no identity for an invite to pin".
	//
	// That refusal is correct and is invariant 11 doing its job. An invite blob
	// carries the bus's CERTIFICATE FINGERPRINT, because there is no CA and no
	// trust-on-first-use: the fingerprint in the invite is the only thing that
	// lets the enrolling client know it is talking to the right bus. An invite
	// minted before a certificate existed could pin nothing, and `invite mint`
	// will not manufacture a certificate precisely because a regenerated one
	// would rename the bus away from its own identity.
	//
	// So the directory is brought to life exactly as the operator's documented
	// sequence does — start once, stop, mint — and only when it has not been
	// started already.
	if _, err := os.Stat(filepath.Join(dataDir, buscert.CertFileName)); err != nil {
		boot := startServer(t, dataDir)
		boot.awaitServerStarted(t)
		boot.signal(t, syscall.SIGTERM)
		if code := boot.awaitExit(t, shutdownTimeout); code != 0 {
			t.Fatalf("the priming start of %s exited %d, want 0; the directory needs one clean run to hold a certificate an invite can pin\n%s", dataDir, code, boot.stderr())
		}
	}

	minted := make([]e2eInvite, 0, n)
	for i := 0; i < n; i++ {
		code, stdout, stderr := runMint(t,
			"-data-dir", dataDir,
			"-bus-address", "https://127.0.0.1:8443",
			"-ttl", "1h",
			"-label", "end-to-end enrolment fixture",
			"-json")
		if code != exitInviteOK {
			t.Fatalf("`invite mint` (%d of %d) exit = %d, want %d.\n"+
				"If this is the 'data directory is locked' exit, the bus is already RUNNING on %s: e2ePrepareInvites must be called BEFORE the first startServer.\nstdout: %s\nstderr: %s",
				i+1, n, code, exitInviteOK, dataDir, stdout, stderr)
		}
		var blob inviteBlob
		if err := json.Unmarshal([]byte(stdout), &blob); err != nil {
			t.Fatalf("`invite mint --json` output is not one JSON object: %v\ngot: %s", err, stdout)
		}
		if blob.InviteID == "" || blob.InviteSecret == "" {
			t.Fatalf("the minted blob is missing the id or the secret: %s", stdout)
		}
		minted = append(minted, e2eInvite{id: blob.InviteID, secret: blob.InviteSecret})
	}

	e2eInviteMu.Lock()
	defer e2eInviteMu.Unlock()
	e2eInvitePool[dataDir] = append(e2eInvitePool[dataDir], minted...)
}

// e2eTakeInvite draws one unspent invite for dataDir.
//
// It fails LOUDLY and specifically when the pool is empty, because the
// alternative — enrolling without an invite — now produces a 403 several frames
// away from the actual mistake, in a test whose subject is usually something
// else entirely.
func e2eTakeInvite(t *testing.T, dataDir string) e2eInvite {
	t.Helper()

	e2eInviteMu.Lock()
	defer e2eInviteMu.Unlock()

	pool := e2eInvitePool[dataDir]
	if len(pool) == 0 {
		t.Fatalf("no unspent invite left for data directory %s.\n"+
			"This bus is INVITE-ONLY (invariant 3): every enrolment needs its own single-use invite. Call e2ePrepareInvites(t, dir, n) BEFORE the first startServer, with n at least the number of enrolments this test performs across all of its restarts.", dataDir)
	}
	inv := pool[0]
	e2eInvitePool[dataDir] = pool[1:]
	return inv
}
