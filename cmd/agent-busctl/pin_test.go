package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/dodgymike/agent-bus/client"
)

// testPins are distinct, valid (64 lowercase hex) bus certificate
// fingerprints, deliberately more than client.MaxBusPins so the "at the cap"
// tests never hardcode the cap's numeric value.
var testPins = []string{
	strings.Repeat("a", 64),
	strings.Repeat("b", 64),
	strings.Repeat("c", 64),
	strings.Repeat("d", 64),
}

func init() {
	if len(testPins) <= client.MaxBusPins {
		panic("testPins must hold more than client.MaxBusPins distinct fingerprints")
	}
}

// runCLI drives agent-busctl exactly as the process does (see root.run), with
// no stub bus: every `pin` operation is a pure store mutation, so nothing here
// needs a network round trip.
func runCLI(t *testing.T, lookupEnv func(string) (string, bool), args ...string) cliResult {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), args, &stdout, &stderr, lookupEnv)
	return cliResult{Code: code, Stdout: stdout.String(), Stderr: stderr.String()}
}

// seedPinIdentity builds a credential store at a fresh t.TempDir() holding one
// CURRENT identity under agentID, accepting exactly fingerprints (nil for a
// plaintext-loopback identity, matching how Identity.BusFingerprints
// documents an enrolment with no certificate). It uses only the exported
// client.Store API, matching how whoami_test.go's seedTwoIdentities seeds a
// store.
func seedPinIdentity(t *testing.T, agentID string, fingerprints []string) string {
	t.Helper()
	return seedPinIdentityAt(t, agentID, "https://127.0.0.1:1", fingerprints)
}

// seedPinIdentityAt is seedPinIdentity with the bus URL chosen, so a test can
// build the PLAINTEXT case that `pin add` refuses.
func seedPinIdentityAt(t *testing.T, agentID, busURL string, fingerprints []string) string {
	t.Helper()
	dir := t.TempDir()
	s, err := client.OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	cred := client.Credential{
		Identity: client.Identity{
			AgentID:         agentID,
			BusID:           "bus-x",
			Name:            "agent",
			BusURL:          busURL,
			PublicKey:       "cHVi",
			EnrolledAt:      "2026-08-02T00:00:00Z",
			BusFingerprints: fingerprints,
		},
		PrivateKeySeed: "c2VlZA==",
	}
	if err := s.PromotePending("", cred, true); err != nil {
		t.Fatalf("PromotePending: %v", err)
	}
	return dir
}

// seedTwoPinIdentities builds a store with TWO identities, currentID selected
// as the STORED current one, each starting with one (distinct) pin. It is the
// fixture for the --as / AGENT_BUS_AGENT_ID selection tests: an operation must
// land on the identity the flag or env var names, never on the stored current
// one by accident.
func seedTwoPinIdentities(t *testing.T) (dir, currentID, otherID string) {
	t.Helper()
	dir = t.TempDir()
	s, err := client.OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	currentID = "bus-x.current-1"
	otherID = "bus-x.other-1"
	credCurrent := client.Credential{
		Identity: client.Identity{
			AgentID: currentID, BusID: "bus-x", Name: "current", BusURL: "https://127.0.0.1:1",
			PublicKey: "cHVi", EnrolledAt: "2026-08-02T00:00:00Z",
			BusFingerprints: []string{testPins[0]},
		},
		PrivateKeySeed: "c2VlZA==",
	}
	credOther := client.Credential{
		Identity: client.Identity{
			AgentID: otherID, BusID: "bus-x", Name: "other", BusURL: "https://127.0.0.1:1",
			PublicKey: "cHVi", EnrolledAt: "2026-08-02T00:00:00Z",
			BusFingerprints: []string{testPins[1]},
		},
		PrivateKeySeed: "c2VlZA==",
	}
	if err := s.PromotePending("", credCurrent, true); err != nil {
		t.Fatalf("PromotePending(current): %v", err)
	}
	if err := s.PromotePending("", credOther, false); err != nil {
		t.Fatalf("PromotePending(other): %v", err)
	}
	return dir, currentID, otherID
}

// pinListJSON runs `pin list --json` against dir and decodes it as
// pinListResult, failing the test on any error along the way. It is the
// common assertion path for "what does the store actually hold now".
func pinListJSON(t *testing.T, dir string) pinListResult {
	t.Helper()
	res := runCLI(t, emptyEnv, "--identity", dir, "--json", "pin", "list")
	if res.Code != client.ExitOK {
		t.Fatalf("pin list --json: code = %d, want %d; stdout=%q stderr=%q", res.Code, client.ExitOK, res.Stdout, res.Stderr)
	}
	var parsed pinListResult
	if err := json.Unmarshal([]byte(res.Stdout), &parsed); err != nil {
		t.Fatalf("pin list --json: stdout not parseable as pinListResult: %v (%q)", err, res.Stdout)
	}
	return parsed
}

// assertFingerprints fails the test unless got is exactly want, in order.
func assertFingerprints(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("fingerprints = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("fingerprints = %v, want %v", got, want)
		}
	}
}

// TestPinListReflectsAcceptSet checks `pin list` (human and --json) against an
// identity with zero, one and two pinned certificates: the human rendering
// names every pinned certificate (or says "plaintext" for none), the ROLLOVER
// notice appears only at two, and the JSON shape carries agent_id, bus_url, a
// NEVER-null bus_fingerprints array, and max_bus_fingerprints.
func TestPinListReflectsAcceptSet(t *testing.T) {
	const agentID = "bus-x.agent-1"
	cases := []struct {
		name         string
		fingerprints []string
	}{
		{"no pins (plaintext loopback enrolment)", nil},
		{"one pin", []string{testPins[0]}},
		{"two pins", []string{testPins[0], testPins[1]}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dir := seedPinIdentity(t, agentID, tc.fingerprints)

			human := runCLI(t, emptyEnv, "--identity", dir, "pin", "list")
			if human.Code != client.ExitOK {
				t.Fatalf("pin list: code = %d, want %d; stdout=%q stderr=%q", human.Code, client.ExitOK, human.Stdout, human.Stderr)
			}
			switch len(tc.fingerprints) {
			case 0:
				if !strings.Contains(human.Stdout, "plaintext") {
					t.Fatalf("pin list on an unpinned identity does not mention plaintext: %q", human.Stdout)
				}
			default:
				for _, fp := range tc.fingerprints {
					if !strings.Contains(human.Stdout, fp) {
						t.Fatalf("pin list human output does not mention pinned certificate %s: %q", fp, human.Stdout)
					}
				}
			}
			wantRollover := len(tc.fingerprints) > 1
			gotRollover := strings.Contains(human.Stdout, "ROLLOVER")
			if gotRollover != wantRollover {
				t.Fatalf("pin list ROLLOVER notice present=%v, want %v (fingerprints=%v): %q", gotRollover, wantRollover, tc.fingerprints, human.Stdout)
			}

			jsonRes := runCLI(t, emptyEnv, "--identity", dir, "--json", "pin", "list")
			if jsonRes.Code != client.ExitOK {
				t.Fatalf("pin list --json: code = %d, want %d; stdout=%q stderr=%q", jsonRes.Code, client.ExitOK, jsonRes.Stdout, jsonRes.Stderr)
			}
			var rawFields map[string]json.RawMessage
			if err := json.Unmarshal([]byte(jsonRes.Stdout), &rawFields); err != nil {
				t.Fatalf("pin list --json: stdout not a JSON object: %v (%q)", err, jsonRes.Stdout)
			}
			fpRaw, ok := rawFields["bus_fingerprints"]
			if !ok {
				t.Fatalf("pin list --json: bus_fingerprints field is missing: %q", jsonRes.Stdout)
			}
			if string(fpRaw) == "null" {
				t.Fatalf("pin list --json: bus_fingerprints is null, want an array (possibly empty): %q", jsonRes.Stdout)
			}

			var parsed pinListResult
			if err := json.Unmarshal([]byte(jsonRes.Stdout), &parsed); err != nil {
				t.Fatalf("pin list --json: cannot decode pinListResult: %v (%q)", err, jsonRes.Stdout)
			}
			if parsed.AgentID != agentID {
				t.Fatalf("agent_id = %q, want %q", parsed.AgentID, agentID)
			}
			if parsed.BusURL == "" {
				t.Fatalf("bus_url is empty: %q", jsonRes.Stdout)
			}
			assertFingerprints(t, parsed.BusFingerprints, tc.fingerprints)
			if parsed.MaxBusFingerprints != client.MaxBusPins {
				t.Fatalf("max_bus_fingerprints = %d, want %d (client.MaxBusPins)", parsed.MaxBusFingerprints, client.MaxBusPins)
			}
		})
	}
}

// TestPinAddSucceedsAndIsIdempotent checks `pin add` grows the accept-set, the
// human output names which identity now accepts what, and re-adding a
// fingerprint already held succeeds without duplicating it (a well-behaved
// retry after an interrupted rollover must not be punished).
func TestPinAddSucceedsAndIsIdempotent(t *testing.T) {
	const agentID = "bus-x.agent-1"
	dir := seedPinIdentity(t, agentID, []string{testPins[0]})

	res := runCLI(t, emptyEnv, "--identity", dir, "pin", "add", testPins[1])
	if res.Code != client.ExitOK {
		t.Fatalf("pin add: code = %d, want %d; stdout=%q stderr=%q", res.Code, client.ExitOK, res.Stdout, res.Stderr)
	}
	if !strings.Contains(res.Stdout, agentID+" now accepts "+testPins[1]) {
		t.Fatalf("pin add human output does not say which identity now accepts what: %q", res.Stdout)
	}
	assertFingerprints(t, pinListJSON(t, dir).BusFingerprints, []string{testPins[0], testPins[1]})

	// Idempotent repeat: same key (fingerprint), same effect — succeeds, and
	// the set is not duplicated.
	repeat := runCLI(t, emptyEnv, "--identity", dir, "pin", "add", testPins[1])
	if repeat.Code != client.ExitOK {
		t.Fatalf("pin add (repeat): code = %d, want %d; stdout=%q stderr=%q", repeat.Code, client.ExitOK, repeat.Stdout, repeat.Stderr)
	}
	assertFingerprints(t, pinListJSON(t, dir).BusFingerprints, []string{testPins[0], testPins[1]})
}

func TestPinBootstrapRequiresExplicitHTTPSBus(t *testing.T) {
	const agentID = "bus-x.agent-1"
	dir := seedPinIdentityAt(t, agentID, "http://127.0.0.1:18080", nil)

	missingBus := runCLI(t, emptyEnv, "--identity", dir, "pin", "bootstrap", testPins[0])
	if missingBus.Code != client.ExitUsage {
		t.Fatalf("pin bootstrap without --bus: code = %d, want %d; stdout=%q stderr=%q", missingBus.Code, client.ExitUsage, missingBus.Stdout, missingBus.Stderr)
	}
	if !strings.Contains(missingBus.Stderr, "--bus https://") {
		t.Fatalf("pin bootstrap without --bus did not print the HTTPS remedy: %q", missingBus.Stderr)
	}
	assertFingerprints(t, pinListJSON(t, dir).BusFingerprints, nil)

	plaintextBus := runCLI(t, emptyEnv, "--identity", dir, "--bus", "http://127.0.0.1:18090", "pin", "bootstrap", testPins[0])
	if plaintextBus.Code != client.ExitUsage {
		t.Fatalf("pin bootstrap with plaintext --bus: code = %d, want %d; stdout=%q stderr=%q", plaintextBus.Code, client.ExitUsage, plaintextBus.Stdout, plaintextBus.Stderr)
	}
	if !strings.Contains(plaintextBus.Stderr, "requires an https bus URL") || !strings.Contains(plaintextBus.Stderr, "bus_cert_fingerprint") {
		t.Fatalf("pin bootstrap with plaintext --bus did not print the HTTPS/fingerprint remedy: %q", plaintextBus.Stderr)
	}
	assertFingerprints(t, pinListJSON(t, dir).BusFingerprints, nil)
}

// TestPinAddAtCapRefused checks that growing an accept-set already at
// client.MaxBusPins is refused (exit 2), the remedy names `pin remove`, and
// the store is left completely unchanged — no partial write, no eviction.
func TestPinAddAtCapRefused(t *testing.T) {
	const agentID = "bus-x.agent-1"
	capped := append([]string(nil), testPins[:client.MaxBusPins]...)
	overflow := testPins[client.MaxBusPins]
	dir := seedPinIdentity(t, agentID, capped)

	res := runCLI(t, emptyEnv, "--identity", dir, "pin", "add", overflow)
	if res.Code != client.ExitUsage {
		t.Fatalf("pin add at the cap: code = %d, want %d (usage); stdout=%q stderr=%q", res.Code, client.ExitUsage, res.Stdout, res.Stderr)
	}
	if !strings.Contains(res.Stderr, "pin remove") {
		t.Fatalf("pin add at the cap does not name `pin remove` as the remedy: %q", res.Stderr)
	}
	assertFingerprints(t, pinListJSON(t, dir).BusFingerprints, capped)
}

// TestPinRemove covers the three outcomes of `pin remove`: narrowing a
// two-pin set to one, refusing to remove the LAST pin (naming `logout`), and
// refusing a fingerprint that is not currently held (naming `pin list`). In
// every refusal case the store is left unchanged.
func TestPinRemove(t *testing.T) {
	const agentID = "bus-x.agent-1"

	t.Run("narrows a two-pin set to one", func(t *testing.T) {
		dir := seedPinIdentity(t, agentID, []string{testPins[0], testPins[1]})
		res := runCLI(t, emptyEnv, "--identity", dir, "pin", "remove", testPins[0])
		if res.Code != client.ExitOK {
			t.Fatalf("pin remove: code = %d, want %d; stdout=%q stderr=%q", res.Code, client.ExitOK, res.Stdout, res.Stderr)
		}
		if !strings.Contains(res.Stdout, agentID+" no longer accepts "+testPins[0]) {
			t.Fatalf("pin remove human output does not say which identity no longer accepts what: %q", res.Stdout)
		}
		assertFingerprints(t, pinListJSON(t, dir).BusFingerprints, []string{testPins[1]})
	})

	t.Run("refuses to remove the last pin", func(t *testing.T) {
		dir := seedPinIdentity(t, agentID, []string{testPins[0]})
		res := runCLI(t, emptyEnv, "--identity", dir, "pin", "remove", testPins[0])
		if res.Code != client.ExitUsage {
			t.Fatalf("pin remove (last pin): code = %d, want %d (usage); stdout=%q stderr=%q", res.Code, client.ExitUsage, res.Stdout, res.Stderr)
		}
		if !strings.Contains(res.Stderr, "logout") {
			t.Fatalf("pin remove (last pin) does not name `logout` as the remedy: %q", res.Stderr)
		}
		assertFingerprints(t, pinListJSON(t, dir).BusFingerprints, []string{testPins[0]})
	})

	t.Run("refuses a fingerprint not currently held", func(t *testing.T) {
		dir := seedPinIdentity(t, agentID, []string{testPins[0]})
		res := runCLI(t, emptyEnv, "--identity", dir, "pin", "remove", testPins[1])
		if res.Code != client.ExitUsage {
			t.Fatalf("pin remove (unheld): code = %d, want %d (usage); stdout=%q stderr=%q", res.Code, client.ExitUsage, res.Stdout, res.Stderr)
		}
		if !strings.Contains(res.Stderr, "pin list") {
			t.Fatalf("pin remove (unheld) does not name `pin list` as the remedy: %q", res.Stderr)
		}
		assertFingerprints(t, pinListJSON(t, dir).BusFingerprints, []string{testPins[0]})
	})
}

// colonSeparated renders hexStr as byte pairs joined by colons, the way
// several TLS tools print a fingerprint — and NOT the form this CLI accepts.
func colonSeparated(hexStr string) string {
	var b strings.Builder
	for i := 0; i < len(hexStr); i += 2 {
		if i > 0 {
			b.WriteByte(':')
		}
		b.WriteString(hexStr[i : i+2])
	}
	return b.String()
}

// TestPinMalformedFingerprintRejected checks every spelling of "not a
// fingerprint" is refused as a usage error (exit 2) — for both `pin add` and
// `pin remove` — and confirms the store is left untouched in every case.
//
// The leading-space case is built by replacing the fingerprint's own leading
// hex character with a space rather than merely padding a valid fingerprint:
// Client.AddBusPin/RemoveBusPin call strings.TrimSpace before parsing, so
// padding a valid 64-character fingerprint with surrounding whitespace is
// tolerated BY DESIGN (a shell copy-paste artefact, not a different value).
// Replacing a character keeps the total length the same as a genuine
// fingerprint while still being unrecoverably malformed after trimming, which
// is what actually exercises the rejection path.
func TestPinMalformedFingerprintRejected(t *testing.T) {
	valid := testPins[0]
	cases := []struct {
		name string
		fp   string
	}{
		{"uppercase", strings.ToUpper(valid)},
		{"too short", valid[:len(valid)-1]},
		{"too long", valid + "a"},
		{"non-hex", strings.Repeat("g", 64)},
		{"colon-separated", colonSeparated(valid)},
		{"leading space replacing a hex character", " " + valid[1:]},
	}
	for _, action := range []string{"add", "remove"} {
		for _, tc := range cases {
			t.Run(action+"/"+tc.name, func(t *testing.T) {
				dir := seedPinIdentity(t, "bus-x.agent-1", []string{valid})
				res := runCLI(t, emptyEnv, "--identity", dir, "pin", action, tc.fp)
				if res.Code != client.ExitUsage {
					t.Fatalf("pin %s %q: code = %d, want %d (usage); stdout=%q stderr=%q", action, tc.fp, res.Code, client.ExitUsage, res.Stdout, res.Stderr)
				}
				assertFingerprints(t, pinListJSON(t, dir).BusFingerprints, []string{valid})
			})
		}
	}
}

// TestPinAddIsGatedOnTheSCHEME, not on the set being empty.
//
// The two cases pull in opposite directions and the distinction is the one the
// security gate insisted on:
//
//   - An identity that enrolled over a PLAINTEXT bus has no certificate to
//     extend, so a pin there would be a check that never runs — the same reason
//     --bus-fingerprint is refused on an http URL. REFUSED.
//   - An identity on an HTTPS bus with an EMPTY set must be allowed to gain
//     one. Enrolment cannot produce that state, but a DOWNGRADE can: an older
//     binary that writes the store drops the `bus_fingerprints` field it does
//     not know. Refusing here would leave logout plus a full re-enrolment as the
//     only recovery — which is precisely the outcome MTLS-ROTATE exists to
//     remove. Adding a pin to an unpinned https identity strictly NARROWS what
//     it accepts (from "refused" to "exactly this one"), so there is nothing to
//     protect against.
func TestPinAddIsGatedOnTheScheme(t *testing.T) {
	t.Run("a plaintext identity is refused", func(t *testing.T) {
		dir := seedPinIdentityAt(t, "bus-x.agent-1", "http://127.0.0.1:8080", nil)
		res := runCLI(t, emptyEnv, "--identity", dir, "pin", "add", testPins[0])
		if res.Code != client.ExitUsage {
			t.Fatalf("pin add on a plaintext identity: code = %d, want %d (usage); stdout=%q stderr=%q", res.Code, client.ExitUsage, res.Stdout, res.Stderr)
		}
		assertFingerprints(t, pinListJSON(t, dir).BusFingerprints, nil)
	})

	t.Run("an https identity that lost its pin can recover without re-enrolling", func(t *testing.T) {
		dir := seedPinIdentity(t, "bus-x.agent-1", nil)
		res := runCLI(t, emptyEnv, "--identity", dir, "pin", "add", testPins[0])
		if res.Code != client.ExitOK {
			t.Fatalf("pin add on an unpinned https identity: code = %d, want %d; stdout=%q stderr=%q", res.Code, client.ExitOK, res.Stdout, res.Stderr)
		}
		assertFingerprints(t, pinListJSON(t, dir).BusFingerprints, []string{testPins[0]})
	})
}

// TestPinBadInvocationsExitUsage checks malformed pin invocations — a missing
// subcommand, an unknown one, extra arguments, and the wrong operand count —
// all exit 2 without needing an identity: these are caught before the CLI
// ever opens the credential store.
func TestPinBadInvocationsExitUsage(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"no subcommand", []string{"pin"}},
		{"unknown subcommand", []string{"pin", "bogus"}},
		{"list with an extra argument", []string{"pin", "list", "extra-arg"}},
		{"add with zero operands", []string{"pin", "add"}},
		{"add with two operands", []string{"pin", "add", testPins[0], testPins[1]}},
		{"remove with zero operands", []string{"pin", "remove"}},
		{"remove with two operands", []string{"pin", "remove", testPins[0], testPins[1]}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"--identity", t.TempDir()}, tc.args...)
			res := runCLI(t, emptyEnv, args...)
			if res.Code != client.ExitUsage {
				t.Fatalf("run(%v) = %d, want %d (usage); stdout=%q stderr=%q", tc.args, res.Code, client.ExitUsage, res.Stdout, res.Stderr)
			}
		})
	}
}

// TestPinAsFlagSelectsIdentity checks `pin --as <agent-id> add <fingerprint>`
// applies to the NAMED identity, not the stored current selection — and
// leaves the current identity's own accept-set untouched.
//
// --as must precede the action word ("add"/"list"/"remove"): the flag package
// stops parsing flags at the first non-flag argument, so `pin add --as <id>
// <fp>` treats "--as" as a third operand rather than as a flag. This is
// exercised directly below (as a bad-invocation case) so the ordering
// requirement stays covered, not just assumed.
func TestPinAsFlagSelectsIdentity(t *testing.T) {
	dir, currentID, otherID := seedTwoPinIdentities(t)

	res := runCLI(t, emptyEnv, "--identity", dir, "pin", "--as", otherID, "add", testPins[2])
	if res.Code != client.ExitOK {
		t.Fatalf("pin --as %s add: code = %d, want %d; stdout=%q stderr=%q", otherID, res.Code, client.ExitOK, res.Stdout, res.Stderr)
	}
	if !strings.Contains(res.Stdout, otherID+" now accepts "+testPins[2]) {
		t.Fatalf("pin --as %s add: human output does not name %s: %q", otherID, otherID, res.Stdout)
	}

	otherList := runCLI(t, emptyEnv, "--identity", dir, "--as", otherID, "--json", "pin", "list")
	if otherList.Code != client.ExitOK {
		t.Fatalf("pin list --as %s: code = %d, want %d; stdout=%q stderr=%q", otherID, otherList.Code, client.ExitOK, otherList.Stdout, otherList.Stderr)
	}
	var otherParsed pinListResult
	if err := json.Unmarshal([]byte(otherList.Stdout), &otherParsed); err != nil {
		t.Fatalf("pin list --as %s --json: %v (%q)", otherID, err, otherList.Stdout)
	}
	assertFingerprints(t, otherParsed.BusFingerprints, []string{testPins[1], testPins[2]})

	currentList := runCLI(t, emptyEnv, "--identity", dir, "--json", "pin", "list")
	if currentList.Code != client.ExitOK {
		t.Fatalf("pin list (stored current): code = %d, want %d; stdout=%q stderr=%q", currentList.Code, client.ExitOK, currentList.Stdout, currentList.Stderr)
	}
	var currentParsed pinListResult
	if err := json.Unmarshal([]byte(currentList.Stdout), &currentParsed); err != nil {
		t.Fatalf("pin list (stored current) --json: %v (%q)", err, currentList.Stdout)
	}
	if currentParsed.AgentID != currentID {
		t.Fatalf("stored current identity = %q, want %q (the --as add must not have changed the selection)", currentParsed.AgentID, currentID)
	}
	assertFingerprints(t, currentParsed.BusFingerprints, []string{testPins[0]})
}

// TestPinAsFlagOrderingAfterActionIsUsageError pins down the ordering
// requirement TestPinAsFlagSelectsIdentity relies on: `pin add --as <id>
// <fp>` is a usage error, not a silent no-op, because "--as" and "<id>" are
// swallowed as extra positional operands once flag parsing has already
// stopped at "add".
func TestPinAsFlagOrderingAfterActionIsUsageError(t *testing.T) {
	dir, _, otherID := seedTwoPinIdentities(t)
	res := runCLI(t, emptyEnv, "--identity", dir, "pin", "add", "--as", otherID, testPins[2])
	if res.Code != client.ExitUsage {
		t.Fatalf("pin add --as %s <fp>: code = %d, want %d (usage); stdout=%q stderr=%q", otherID, res.Code, client.ExitUsage, res.Stdout, res.Stderr)
	}
}

// TestPinEnvAgentIDSelectsIdentity checks AGENT_BUS_AGENT_ID (not the --as
// flag) selects the identity a pin operation applies to. This guards the
// exact bug named in pin.go's own comment on `ref := c.Config().AgentID`:
// reading env.g.as alone would miss the environment carrier entirely.
func TestPinEnvAgentIDSelectsIdentity(t *testing.T) {
	dir, currentID, otherID := seedTwoPinIdentities(t)
	envFor := func(agentID string) func(string) (string, bool) {
		return func(key string) (string, bool) {
			if key == client.EnvAgentID {
				return agentID, true
			}
			return "", false
		}
	}

	res := runCLI(t, envFor(otherID), "--identity", dir, "pin", "add", testPins[2])
	if res.Code != client.ExitOK {
		t.Fatalf("pin add (AGENT_BUS_AGENT_ID=%s): code = %d, want %d; stdout=%q stderr=%q", otherID, res.Code, client.ExitOK, res.Stdout, res.Stderr)
	}
	if !strings.Contains(res.Stdout, otherID+" now accepts "+testPins[2]) {
		t.Fatalf("pin add (AGENT_BUS_AGENT_ID=%s): human output does not name %s: %q", otherID, otherID, res.Stdout)
	}

	otherList := runCLI(t, envFor(otherID), "--identity", dir, "--json", "pin", "list")
	var otherParsed pinListResult
	if err := json.Unmarshal([]byte(otherList.Stdout), &otherParsed); err != nil {
		t.Fatalf("pin list (AGENT_BUS_AGENT_ID=%s) --json: %v (%q)", otherID, err, otherList.Stdout)
	}
	assertFingerprints(t, otherParsed.BusFingerprints, []string{testPins[1], testPins[2]})

	currentList := runCLI(t, emptyEnv, "--identity", dir, "--json", "pin", "list")
	var currentParsed pinListResult
	if err := json.Unmarshal([]byte(currentList.Stdout), &currentParsed); err != nil {
		t.Fatalf("pin list (stored current) --json: %v (%q)", err, currentList.Stdout)
	}
	if currentParsed.AgentID != currentID {
		t.Fatalf("stored current identity = %q, want %q", currentParsed.AgentID, currentID)
	}
	assertFingerprints(t, currentParsed.BusFingerprints, []string{testPins[0]})
}

// TestPinWhoamiShowsAllPinsAndRolloverWarning checks `whoami` and `whoami
// --json` against an identity mid-rollover (two pins held): the human output
// lists both certificates and warns a rollover is in progress, and the JSON
// carries both fingerprints in insertion order.
func TestPinWhoamiShowsAllPinsAndRolloverWarning(t *testing.T) {
	const agentID = "bus-x.agent-1"
	dir := seedPinIdentity(t, agentID, []string{testPins[0], testPins[1]})

	human := runCLI(t, emptyEnv, "--identity", dir, "whoami")
	if human.Code != client.ExitOK {
		t.Fatalf("whoami: code = %d, want %d; stdout=%q stderr=%q", human.Code, client.ExitOK, human.Stdout, human.Stderr)
	}
	for _, fp := range []string{testPins[0], testPins[1]} {
		if !strings.Contains(human.Stdout, fp) {
			t.Fatalf("whoami human output does not mention pinned certificate %s: %q", fp, human.Stdout)
		}
	}
	if !strings.Contains(human.Stdout, "ROLLOVER") {
		t.Fatalf("whoami with two pins does not warn of a rollover in progress: %q", human.Stdout)
	}

	jsonRes := runCLI(t, emptyEnv, "--identity", dir, "whoami", "--json")
	if jsonRes.Code != client.ExitOK {
		t.Fatalf("whoami --json: code = %d, want %d; stdout=%q stderr=%q", jsonRes.Code, client.ExitOK, jsonRes.Stdout, jsonRes.Stderr)
	}
	var parsed whoamiResult
	if err := json.Unmarshal([]byte(jsonRes.Stdout), &parsed); err != nil {
		t.Fatalf("whoami --json: stdout not parseable: %v (%q)", err, jsonRes.Stdout)
	}
	if parsed.AgentID != agentID {
		t.Fatalf("agent_id = %q, want %q", parsed.AgentID, agentID)
	}
	assertFingerprints(t, parsed.BusFingerprints, []string{testPins[0], testPins[1]})
}
