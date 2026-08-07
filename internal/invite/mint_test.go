package invite

// Tests for INVITE-MINT's half of invariant 1: the invite id and the invite
// secret are minted BY THE SERVER from crypto/rand, and a client-supplied value
// is never accepted as either.
//
// These are deliberately STRUCTURAL as well as behavioural. A test that only
// mints and checks the output would still pass on the day somebody adds a
// MintRequest.Secret field, because the existing call sites would keep leaving
// it empty — the regression would ship green. So the reflection tests below
// assert what MintRequest is ALLOWED TO CONTAIN, which is the property that
// actually has to hold.

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// mintTestStore builds a Store over a REAL wal.Log in a throwaway directory, so
// every mint below goes through the actual two-phase prepare/commit/fsync path
// rather than a stub that could not fail. It returns the store and the WAL path,
// because two of the assertions below read the log's bytes off disk.
func mintTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	lg := logging.New(&bytes.Buffer{}, logging.LevelError)

	// The store is the log's applier and the log is the store's durable path, so
	// one of the two has to be supplied late — the same cycle cmd/agent-bus
	// resolves with deferredLog.
	var log *wal.Log
	dl := durableFunc(func(e wal.Entry) (wal.Committed, error) { return log.Write(e) })
	s, err := NewStore(StoreOptions{BusID: "bus-mint-test", Durable: dl, Logger: lg})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	log, err = wal.Open(wal.LogOptions{Dir: dir, Logger: lg, Applier: s})
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })
	return s, log.Path()
}

// durableFunc adapts a function to DurableLog.
type durableFunc func(wal.Entry) (wal.Committed, error)

func (f durableFunc) Write(e wal.Entry) (wal.Committed, error) { return f(e) }

// TestInviteMintIsServerAuthoritative is invariant 1 for the invite: every id
// and every secret comes from the SERVER's crypto/rand, they are unique, and
// the plaintext secret never reaches the durable log.
func TestInviteMintIsServerAuthoritative(t *testing.T) {
	t.Parallel()
	s, walPath := mintTestStore(t)

	const n = 24
	idPattern := regexp.MustCompile(InviteIDPattern)
	ids := make(map[string]bool, n)
	secrets := make(map[string]bool, n)
	var minted []Minted

	// Every request is IDENTICAL. Anything that differs between the results is
	// therefore something the SERVER chose, not something the caller supplied —
	// which is the property under test, stated as an experiment rather than as a
	// claim about the implementation.
	req := MintRequest{Label: "identical for every mint", TTL: time.Hour}
	for i := 0; i < n; i++ {
		m, err := s.Mint(req)
		if err != nil {
			t.Fatalf("Mint #%d: %v", i, err)
		}
		minted = append(minted, m)

		if !idPattern.MatchString(m.ID) {
			t.Errorf("mint #%d: id %q does not match %s", i, m.ID, InviteIDPattern)
		}
		if ids[m.ID] {
			t.Errorf("mint #%d: id %q was issued twice; ids are never reused (invariant 1)", i, m.ID)
		}
		ids[m.ID] = true

		if secrets[m.Secret] {
			t.Errorf("mint #%d: secret repeated; a repeated bearer credential is a forgeable admission ticket", i)
		}
		secrets[m.Secret] = true

		// The secret is SecretBytes of crypto/rand in base64.RawURLEncoding, and
		// it must decode to exactly that many bytes: a short secret would be a
		// silently weakened credential that every other test still passes.
		raw, err := base64.RawURLEncoding.DecodeString(m.Secret)
		if err != nil {
			t.Fatalf("mint #%d: secret is not base64.RawURLEncoding: %v", i, err)
		}
		if len(raw) != SecretBytes {
			t.Errorf("mint #%d: secret decodes to %d bytes, want %d", i, len(raw), SecretBytes)
		}
		if len(m.Secret) != EncodedSecretLen {
			t.Errorf("mint #%d: secret is %d characters, want %d", i, len(m.Secret), EncodedSecretLen)
		}

		// The stored digest must be the digest OF THE RETURNED SECRET. If these
		// ever diverge the invite is unredeemable and nothing else notices.
		if want := HashSecret(m.Secret); m.SecretDigest != want {
			t.Errorf("mint #%d: stored digest is not HashSecret(returned secret)", i)
		}

		// The bus comes from the STORE, never from the request: an invite that
		// let its requester name a bus could be presented to a different one.
		if m.BusID != "bus-mint-test" {
			t.Errorf("mint #%d: BusID = %q, want the store's bus id", i, m.BusID)
		}
		if m.State != StateOpen {
			t.Errorf("mint #%d: State = %s, want %s", i, m.State, StateOpen)
		}
	}
	if len(ids) != n || len(secrets) != n {
		t.Fatalf("got %d distinct ids and %d distinct secrets from %d mints", len(ids), len(secrets), n)
	}

	// THE SECRET IS NEVER DURABLE. Only its digest is. This reads the actual WAL
	// bytes rather than trusting Record.Encode, because the point is what landed
	// on disk — a log holding live bearer credentials would be a credential store
	// nobody thinks of as one, and it is append-only, so it can never be redacted.
	body, err := os.ReadFile(walPath)
	if err != nil {
		t.Fatalf("reading the WAL at %s: %v", walPath, err)
	}
	for i, m := range minted {
		if bytes.Contains(body, []byte(m.Secret)) {
			t.Errorf("mint #%d: the PLAINTEXT SECRET appears in the write-ahead log at %s; only HashSecret's digest may be durable", i, walPath)
		}
	}
	// And the ids ARE durable — otherwise the search above would pass vacuously
	// on a log that recorded nothing at all.
	for i, m := range minted {
		if !bytes.Contains(body, []byte(m.ID)) {
			t.Fatalf("mint #%d: invite id %q is NOT in the write-ahead log, so the secret-absence check above proves nothing", i, m.ID)
		}
	}
}

// TestInviteMintRejectsClientSuppliedSecret pins that there is NO WAY to supply
// an invite id or an invite secret — not through the store's API, and not
// through the CLI flag set.
//
// "Rejects" is enforced here by ABSENCE, which is the strongest form available:
// a request type with no field for a secret cannot carry one, cannot forget to
// validate it, and cannot have the validation removed by a later edit.
func TestInviteMintRejectsClientSuppliedSecret(t *testing.T) {
	t.Parallel()

	t.Run("MintRequest carries no id or secret field", func(t *testing.T) {
		rt := reflect.TypeOf(MintRequest{})
		allowed := map[string]bool{"Label": true, "TTL": true}
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			if !allowed[f.Name] {
				t.Errorf("MintRequest has an unexpected field %q (%s).\n"+
					"INVARIANT 1: the server is authoritative on the invite id and the invite secret.\n"+
					"If this field can carry, seed, influence or derive either of them, it must not exist:\n"+
					"an invite whose secret its requester could choose is not a credential, and an id its\n"+
					"requester could choose lets an attacker anticipate or collide with a future invite.\n"+
					"If it is genuinely unrelated (another operator note, another bound), add it to the\n"+
					"allowed set here DELIBERATELY, so the decision is recorded rather than assumed.",
					f.Name, f.Type)
			}
		}
	})

	t.Run("identical requests yield different ids and secrets", func(t *testing.T) {
		s, _ := mintTestStore(t)
		// Whatever the caller controls is held constant, so anything the caller
		// could influence would come out the same. It does not.
		req := MintRequest{Label: "same label", TTL: time.Hour}
		a, err := s.Mint(req)
		if err != nil {
			t.Fatalf("Mint: %v", err)
		}
		b, err := s.Mint(req)
		if err != nil {
			t.Fatalf("Mint: %v", err)
		}
		if a.ID == b.ID {
			t.Error("two mints of an identical request produced the SAME invite id; the id must come from crypto/rand, not from the request")
		}
		if a.Secret == b.Secret {
			t.Error("two mints of an identical request produced the SAME secret; the secret must come from crypto/rand, not from the request")
		}
		if a.SecretDigest == b.SecretDigest {
			t.Error("two mints of an identical request produced the same secret digest")
		}
	})

	t.Run("the label cannot smuggle itself into the id or the secret", func(t *testing.T) {
		s, _ := mintTestStore(t)
		// The one caller-controlled string is checked against the two
		// server-authoritative values. This is what a naive "derive the id from
		// the request" implementation would fail.
		const marker = "zzmarkerzz"
		m, err := s.Mint(MintRequest{Label: marker, TTL: time.Hour})
		if err != nil {
			t.Fatalf("Mint: %v", err)
		}
		if strings.Contains(m.ID, marker) {
			t.Errorf("invite id %q contains caller-supplied text; the id must not be derived from the request", m.ID)
		}
		if strings.Contains(m.Secret, marker) {
			t.Error("the invite secret contains caller-supplied text; the secret must be crypto/rand alone")
		}
	})

	t.Run("Minted.String redacts the secret", func(t *testing.T) {
		s, _ := mintTestStore(t)
		m, err := s.Mint(MintRequest{TTL: time.Hour})
		if err != nil {
			t.Fatalf("Mint: %v", err)
		}
		// Minted embeds a Record, which makes it exactly the shape somebody
		// reaches for when logging "the invite that was just created". If %v
		// printed the secret, the CLI's own error paths would leak it.
		for _, rendered := range []string{
			m.String(),
			fmt.Sprintf("%v", m),
			fmt.Sprintf("%s", m),
			fmt.Sprintf("%#v", m),
		} {
			if strings.Contains(rendered, m.Secret) {
				t.Errorf("a formatted Minted contains the PLAINTEXT SECRET: %s", rendered)
			}
			if !strings.Contains(rendered, "REDACTED") {
				t.Errorf("a formatted Minted does not say the secret was redacted: %s", rendered)
			}
		}
	})

	t.Run("the mint subcommand exposes no id or secret flag", func(t *testing.T) {
		// The store's API is only half the surface; the CLI is the other half,
		// and invariant 7 makes the CLI THE client. A -invite-secret flag would
		// put a bearer credential in a shell history and a process list even
		// though MintRequest could not carry it.
		//
		// Read as SOURCE rather than by driving the flag set, because
		// cmd/agent-bus is a main package this one cannot import. The file is
		// located relative to this package so the check cannot silently pass by
		// reading nothing.
		path := filepath.Join("..", "..", "cmd", "agent-bus", "invite.go")
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v (this test is worthless if it cannot find the subcommand)", path, err)
		}
		if !bytes.Contains(src, []byte(`"bus-address"`)) {
			t.Fatalf("%s does not define the -bus-address flag; this test is looking at the wrong file", path)
		}
		for _, forbidden := range []string{
			`"invite-secret"`, `"secret"`, `"invite-id"`, `"id"`, `"invite-token"`, `"token"`,
		} {
			if bytes.Contains(src, []byte("fs.StringVar(&"+strings.TrimSuffix(strings.TrimPrefix(forbidden, `"`), `"`))) {
				t.Errorf("%s appears to define a %s flag", path, forbidden)
			}
			// The flag NAME as a quoted string next to a flag registration is the
			// signal; check the whole file for the name appearing as a flag.
			if bytes.Contains(src, []byte(", "+forbidden+", ")) {
				t.Errorf("%s registers a flag named %s. INVARIANT 1: the server mints the invite id and secret; "+
					"a flag supplying either puts a bearer credential in a shell history and lets a caller "+
					"predict a value that must be unpredictable.", path, forbidden)
			}
		}
	})
}
