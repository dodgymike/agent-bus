package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/client"
)

// INVITE-CLIENT, the CLI half: `agent-busctl enrol --invite-file <path>`.
//
// The property under test is the reason the flag takes a PATH and not a blob.
// The invite secret is a bearer credential (invariant 3: it is the only way onto
// the bus), and argv is public — visible in the process list to every local user
// and recorded in shell history. So there must be no flag that accepts it, and
// no error path that echoes it back. Invariant 7 is the other half: an agent
// shelling out must never meet a prompt, which is why `--invite-file -` refuses
// a terminal instead of reading one.

// cliInviteSecret is the SENTINEL: long, unique, and unmistakable in a process
// listing or an error message.
const cliInviteSecret = "INVITE-SENTINEL-cli-3f9d2a71-8b4c-4e5f-a012-do-not-leak-abcdef012345"

// cliInviteID is the invite's NAME, which IS safe to print and which the CLI is
// expected to report so an operator can see which single-use invite was spent.
const cliInviteID = "inv-01H8XCLIINVITE"

// cliInviteBus is a bus that answers POST /v1/enroll and records what arrived.
//
// Plaintext loopback, deliberately: an invite whose bus_address is an http://
// loopback URL carries no certificate fingerprint (client.Invite.Validate
// permits that ONLY for plaintext loopback), which keeps these tests about the
// CLI's argv/stdin/permission surface rather than about TLS pinning — that half
// is covered in client/inviteclient_test.go against a real https bus.
type cliInviteBus struct {
	mu     sync.Mutex
	hits   int
	bodies []map[string]interface{}

	srv *httptest.Server
}

func newCLIInviteBus(t *testing.T) *cliInviteBus {
	t.Helper()
	b := &cliInviteBus{}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var m map[string]interface{}
		_ = json.Unmarshal(raw, &m)
		b.mu.Lock()
		b.hits++
		b.bodies = append(b.bodies, m)
		b.mu.Unlock()

		if r.URL.Path != "/v1/enroll" {
			http.NotFound(w, r)
			return
		}
		stubWriteJSON(w, http.StatusCreated, map[string]interface{}{
			"agent_id":    "bus-testbus.planner-1",
			"bus_id":      "bus-testbus",
			"name":        "planner",
			"enrolled_at": time.Now().UTC().Format(time.RFC3339Nano),
		})
	}))
	t.Cleanup(b.srv.Close)
	return b
}

func (b *cliInviteBus) URL() string { return b.srv.URL }

func (b *cliInviteBus) calls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.hits
}

func (b *cliInviteBus) lastBody(t *testing.T) map[string]interface{} {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.bodies) == 0 {
		t.Fatalf("the bus saw no enrol requests, so there is nothing to assert on")
	}
	return b.bodies[len(b.bodies)-1]
}

// cliInviteBlob renders the invite blob the mint would have handed the operator.
func cliInviteBlob(t *testing.T, busURL string) []byte {
	t.Helper()
	raw, err := json.Marshal(client.Invite{
		InviteID:     cliInviteID,
		BusID:        "bus-testbus",
		BusAddress:   busURL,
		InviteSecret: cliInviteSecret,
		ExpiresAt:    time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		t.Fatalf("marshalling the test invite: %v", err)
	}
	return raw
}

// writeInviteFile writes the blob at mode, the way an operator following the
// help text would (`chmod 0600 invite.json`).
func writeInviteFile(t *testing.T, blob []byte, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "invite.json")
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatalf("writing the invite file: %v", err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod %04o: %v", mode, err)
	}
	return path
}

// countingReader records whether stdin was read AT ALL, which is the assertion
// the TTY refusal actually needs: "it returned an error" would also be satisfied
// by a command that consumed the invite first and complained afterwards.
type countingReader struct {
	r     io.Reader
	reads int32
}

func (c *countingReader) Read(p []byte) (int, error) {
	atomic.AddInt32(&c.reads, 1)
	return c.r.Read(p)
}

func (c *countingReader) count() int32 { return atomic.LoadInt32(&c.reads) }

// runCLIArgv drives the CLI's normal entry point over EXACTLY the argv slice it
// is given, and hands that slice back so a test can assert on what was passed.
func runCLIArgv(t *testing.T, stdin io.Reader, stdinIsTTY bool, argv []string) (cliResult, []string) {
	t.Helper()
	var stdout, stderr strings.Builder
	code := runWithTTY(context.Background(), argv, stdin, &stdout, &stderr, emptyEnv, false, stdinIsTTY)
	return cliResult{Code: code, Stdout: stdout.String(), Stderr: stderr.String()}, argv
}

// TestEnrolInviteFileArgv is the task's argv test: the invite secret must never
// be reachable from the command line.
//
// Everything lives under this one name because it is the recorded proof command
// for the CLI half of INVITE-CLIENT.
func TestEnrolInviteFileArgv(t *testing.T) {
	t.Run("no flag accepts the invite or its secret", func(t *testing.T) {
		// The GLOBAL flags, walked structurally. A future --invite-secret added
		// to the global set would fail here.
		fs, _ := newCommandFlagSet("enrol", &globals{})
		fs.VisitAll(func(f *flag.Flag) {
			switch f.Name {
			case "invite", "invite-secret", "invite-blob", "invite-token", "invite-json":
				t.Errorf("the global flag set defines --%s, which would put a bearer credential on argv", f.Name)
			}
			if strings.Contains(strings.ToLower(f.Usage), "secret") {
				t.Errorf("global flag --%s advertises %q — no flag may invite a secret value", f.Name, f.Usage)
			}
		})

		// The SUBCOMMAND's flags are registered inside runEnrol and are not
		// reachable from here, so they are probed through the parser itself,
		// which is the surface an agent actually meets. A flag that does not
		// exist is answered "flag provided but not defined", exit 2.
		bus := newCLIInviteBus(t)
		for _, name := range []string{"invite", "invite-secret", "invite-blob", "invite-token", "invite-json"} {
			res, _ := runCLIArgv(t, nil, false, []string{
				"--identity", t.TempDir(), "--bus", bus.URL(),
				"enrol", "--name", "planner", "--" + name, cliInviteSecret,
			})
			if res.Code != client.ExitUsage {
				t.Errorf("enrol --%s exited %d, want %d — no flag may take the invite itself", name, res.Code, client.ExitUsage)
			}
			if !strings.Contains(res.Stderr, "not defined") {
				t.Errorf("enrol --%s stderr = %q, want the flag reported as undefined", name, res.Stderr)
			}
			if strings.Contains(res.Stdout+res.Stderr, cliInviteSecret) {
				t.Errorf("enrol --%s echoed the secret it was handed back to the terminal", name)
			}
		}
		if bus.calls() != 0 {
			t.Errorf("the bus saw %d requests while probing undefined flags, want 0", bus.calls())
		}

		// And --invite-file DOES exist, so the probe above can tell a missing
		// flag from a present one rather than passing on every name.
		res, _ := runCLIArgv(t, nil, false, []string{
			"--identity", t.TempDir(), "enrol", "--name", "planner",
			"--invite-file", filepath.Join(t.TempDir(), "absent.json"),
		})
		if res.Code == client.ExitUsage || strings.Contains(res.Stderr, "not defined") {
			t.Fatalf("enrol --invite-file was reported as an undefined flag (exit %d, stderr %q); the probe above proves nothing", res.Code, res.Stderr)
		}
		if res.Code != client.ExitConfig {
			t.Fatalf("enrol --invite-file <missing> exited %d, want %d (the invite file cannot be used)", res.Code, client.ExitConfig)
		}
	})

	t.Run("the secret travels in a file, never on argv", func(t *testing.T) {
		bus := newCLIInviteBus(t)
		blob := cliInviteBlob(t, bus.URL())
		path := writeInviteFile(t, blob, 0o600)
		dir := t.TempDir()

		argv := []string{"--identity", dir, "enrol", "--invite-file", path, "--name", "planner", "--json"}
		res, passed := runCLIArgv(t, nil, false, argv)
		if res.Code != client.ExitOK {
			t.Fatalf("enrol --invite-file exited %d, want 0; stdout=%q stderr=%q", res.Code, res.Stdout, res.Stderr)
		}

		// The argv the CLI was actually driven with: the PATH is there, the
		// secret is not, in any element.
		var sawPath bool
		for _, arg := range passed {
			if arg == path {
				sawPath = true
			}
			if strings.Contains(arg, cliInviteSecret) {
				t.Fatalf("argv element %q carries the invite SECRET; every local user can read this in the process list", arg)
			}
		}
		if !sawPath {
			t.Fatalf("argv %v does not contain the invite path, so this check proves nothing", passed)
		}

		// And the secret DID reach the bus, from the file — otherwise "not on
		// argv" would be satisfied by a client that simply never sent it.
		if got, _ := bus.lastBody(t)["invite_secret"].(string); got != cliInviteSecret {
			t.Fatalf("invite_secret on the wire = %q, want the secret from the file", got)
		}

		// The real process command line, for good measure: nothing in this
		// process's argv may carry the sentinel either. It is its OWN subtest so
		// that the skip on a non-Linux box skips only this check, and not the
		// argv assertions above, which are the load-bearing ones and are
		// portable.
		t.Run("the real process command line", func(t *testing.T) {
			if runtime.GOOS != "linux" {
				t.Skip("/proc/self/cmdline is Linux-only; the argv assertions above already ran")
			}
			cmdline, err := os.ReadFile("/proc/self/cmdline")
			if err != nil {
				t.Skipf("cannot read /proc/self/cmdline: %v", err)
			}
			if strings.Contains(string(cmdline), cliInviteSecret) {
				t.Fatalf("/proc/self/cmdline carries the invite secret: %q", strings.ReplaceAll(string(cmdline), "\x00", " "))
			}
		})
	})

	t.Run("--invite-file - reads stdin, and refuses a terminal without reading it", func(t *testing.T) {
		bus := newCLIInviteBus(t)
		blob := cliInviteBlob(t, bus.URL())

		t.Run("piped stdin is accepted", func(t *testing.T) {
			stdin := &countingReader{r: strings.NewReader(string(blob))}
			res, _ := runCLIArgv(t, stdin, false, []string{
				"--identity", t.TempDir(), "enrol", "--invite-file", "-", "--name", "planner", "--json",
			})
			if res.Code != client.ExitOK {
				t.Fatalf("enrol --invite-file - exited %d, want 0; stdout=%q stderr=%q", res.Code, res.Stdout, res.Stderr)
			}
			if stdin.count() == 0 {
				t.Fatalf("stdin was never read, so the invite cannot have come from it")
			}
			if got, _ := bus.lastBody(t)["invite_id"].(string); got != cliInviteID {
				t.Fatalf("invite_id on the wire = %q, want %q", got, cliInviteID)
			}
		})

		t.Run("a terminal is refused and NOTHING is read", func(t *testing.T) {
			stdin := &countingReader{r: strings.NewReader(string(blob))}
			res, _ := runCLIArgv(t, stdin, true, []string{
				"--identity", t.TempDir(), "enrol", "--invite-file", "-", "--name", "planner", "--json",
			})
			if res.Code != client.ExitUsage {
				t.Fatalf("enrol --invite-file - with stdin on a terminal exited %d, want %d", res.Code, client.ExitUsage)
			}
			if stdin.count() != 0 {
				t.Fatalf("stdin was read %d times; invariant 7 forbids an interactive read, so nothing may be consumed", stdin.count())
			}
			var payload map[string]interface{}
			if err := json.Unmarshal([]byte(res.Stdout), &payload); err != nil {
				t.Fatalf("--json failure is not a JSON object: %v (%q)", err, res.Stdout)
			}
			if ok, _ := payload["ok"].(bool); ok {
				t.Fatalf("the failure object says ok=true: %v", payload)
			}
			if kind, _ := payload["kind"].(string); kind != string(client.KindUsage) {
				t.Fatalf("kind = %q, want %q", kind, client.KindUsage)
			}
			if !strings.Contains(res.Stdout, "terminal") {
				t.Errorf("the error does not say stdin is a terminal: %q", res.Stdout)
			}
		})
	})

	t.Run("a group- or world-readable invite file is refused with exit 3", func(t *testing.T) {
		bus := newCLIInviteBus(t)
		blob := cliInviteBlob(t, bus.URL())
		path := writeInviteFile(t, blob, 0o644)

		res, _ := runCLIArgv(t, nil, false, []string{
			"--identity", t.TempDir(), "enrol", "--invite-file", path, "--name", "planner", "--json",
		})
		if res.Code != client.ExitConfig {
			t.Fatalf("enrol on a 0644 invite exited %d, want %d", res.Code, client.ExitConfig)
		}
		if bus.calls() != 0 {
			t.Fatalf("the bus saw %d requests, want 0 — the file is refused before anything is sent", bus.calls())
		}
		var payload map[string]interface{}
		if err := json.Unmarshal([]byte(res.Stdout), &payload); err != nil {
			t.Fatalf("--json failure is not a JSON object: %v (%q)", err, res.Stdout)
		}
		if kind, _ := payload["kind"].(string); kind != string(client.KindConfig) {
			t.Fatalf("kind = %q, want %q", kind, client.KindConfig)
		}
		if code, _ := payload["exit_code"].(float64); int(code) != client.ExitConfig {
			t.Errorf("exit_code = %v, want %d", payload["exit_code"], client.ExitConfig)
		}
		if strings.Contains(res.Stdout+res.Stderr, cliInviteSecret) {
			t.Fatalf("the refusal echoes the invite SECRET: %q", res.Stdout+res.Stderr)
		}
		if strings.Contains(res.Stdout+res.Stderr, string(blob)) {
			t.Fatalf("the refusal echoes the file's CONTENTS back: %q", res.Stdout+res.Stderr)
		}
		if !strings.Contains(res.Stdout, "0644") {
			t.Errorf("the refusal does not name the mode it found: %q", res.Stdout)
		}
	})

	t.Run("a hostile invite field cannot forge terminal output or fill a scrollback", func(t *testing.T) {
		// REGRESSION, at the terminal rather than at the API. client's own tests
		// assert Validate refuses these; this asserts what an OPERATOR actually
		// sees when it does — which is the surface the escape sequence is aimed
		// at, and the one no unit test on *client.Error can reach.
		//
		// HUMAN mode deliberately, not --json: encoding/json escapes every byte
		// below 0x20 already, so the JSON path is safe by construction and would
		// pass whether or not the sanitisation exists.
		const forged = "agent-busctl: verified OK"

		// Mirrors client's unexported maxBusAddressLen. The exact bound is owned
		// and tested in client/inviteclient_test.go; this only needs a value
		// comfortably over it.
		const overBoundAddressLen = 4096

		cases := []struct {
			name   string
			mutate func(*client.Invite)
			// wantAbsent must not be echoed back to the terminal at all.
			wantAbsent string
		}{
			{
				name: "invite_id spelling an ANSI erase and a forged success line",
				mutate: func(i *client.Invite) {
					i.InviteID = "\x1b[2K\r" + forged
				},
			},
			{
				name: "bus_address far over the bound",
				mutate: func(i *client.Invite) {
					i.BusAddress = "http://127.0.0.1:1/" + strings.Repeat("a", overBoundAddressLen)
				},
				wantAbsent: strings.Repeat("a", 128),
			},
		}

		for _, tc := range cases {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				bus := newCLIInviteBus(t)
				inv := client.Invite{
					InviteID:     cliInviteID,
					BusID:        "bus-testbus",
					BusAddress:   bus.URL(),
					InviteSecret: cliInviteSecret,
					ExpiresAt:    time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano),
				}
				tc.mutate(&inv)
				blob, err := json.Marshal(inv)
				if err != nil {
					t.Fatalf("marshalling the hostile invite: %v", err)
				}
				path := writeInviteFile(t, blob, 0o600)

				res, _ := runCLIArgv(t, nil, false, []string{
					"--identity", t.TempDir(), "enrol", "--invite-file", path, "--name", "planner",
				})
				if res.Code != client.ExitConfig {
					t.Fatalf("enrol exited %d, want %d — the invite is unusable; stdout=%q stderr=%q",
						res.Code, client.ExitConfig, res.Stdout, res.Stderr)
				}
				if bus.calls() != 0 {
					t.Fatalf("the bus saw %d requests, want 0 — the invite is refused before anything is dialled", bus.calls())
				}

				combined := res.Stdout + res.Stderr
				if combined == "" {
					t.Fatalf("the CLI printed nothing at all, so these checks would be vacuous")
				}
				// The bytes that MOVE THE CURSOR are what forge output. The
				// forged WORDS are not asserted absent: quoting the offending
				// value back is how an operator identifies which blob is wrong,
				// and sanitisation neutralises controls rather than censoring
				// English.
				if strings.Contains(combined, "\x1b") {
					t.Errorf("the CLI printed a raw ESC, which erases and rewrites the line: %q", combined)
				}
				if strings.Contains(combined, "\r") {
					t.Errorf("the CLI printed a raw CR, which returns the cursor to the start of the line: %q", combined)
				}
				for _, line := range strings.Split(combined, "\n") {
					if strings.HasPrefix(line, forged) {
						t.Errorf("a line BEGINS with the forged success text, which is indistinguishable from a real one: %q", line)
					}
				}
				if tc.wantAbsent != "" && strings.Contains(combined, tc.wantAbsent) {
					t.Errorf("the refusal echoes the oversized value back to the terminal: %q", combined)
				}
				if strings.Contains(combined, cliInviteSecret) {
					t.Fatalf("the refusal echoes the invite SECRET: %q", combined)
				}
			})
		}
	})

	t.Run("the removed --invite flag is an unknown flag and never echoes its value", func(t *testing.T) {
		bus := newCLIInviteBus(t)
		blob := string(cliInviteBlob(t, bus.URL()))

		for _, argv := range [][]string{
			{"--identity", t.TempDir(), "enrol", "--name", "planner", "--invite", blob},
			{"--identity", t.TempDir(), "enrol", "--name", "planner", "--invite=" + blob},
			{"--identity", t.TempDir(), "enrol", "--name", "planner", "--invite", cliInviteSecret},
		} {
			res, _ := runCLIArgv(t, nil, false, argv)
			if res.Code != client.ExitUsage {
				t.Errorf("enrol --invite exited %d, want %d (an unknown flag is still exit 2)", res.Code, client.ExitUsage)
			}
			combined := res.Stdout + res.Stderr
			if strings.Contains(combined, cliInviteSecret) {
				t.Errorf("the usage error echoed the blob it was given back to the terminal: %q", combined)
			}
			if !strings.Contains(combined, "not defined") {
				t.Errorf("stderr = %q, want the flag reported as undefined", combined)
			}
		}
		if bus.calls() != 0 {
			t.Errorf("the bus saw %d requests, want 0", bus.calls())
		}
	})

	t.Run("success reports the invite id and never the secret", func(t *testing.T) {
		bus := newCLIInviteBus(t)
		blob := cliInviteBlob(t, bus.URL())

		t.Run("--json", func(t *testing.T) {
			path := writeInviteFile(t, blob, 0o600)
			res, _ := runCLIArgv(t, nil, false, []string{
				"--identity", t.TempDir(), "enrol", "--invite-file", path, "--name", "planner", "--json",
			})
			if res.Code != client.ExitOK {
				t.Fatalf("enrol exited %d, want 0; stdout=%q stderr=%q", res.Code, res.Stdout, res.Stderr)
			}
			var payload map[string]interface{}
			if err := json.Unmarshal([]byte(res.Stdout), &payload); err != nil {
				t.Fatalf("stdout is not a JSON object: %v (%q)", err, res.Stdout)
			}
			if ok, _ := payload["ok"].(bool); !ok {
				t.Errorf(`"ok" = %v, want true`, payload["ok"])
			}
			if id, _ := payload["agent_id"].(string); id == "" {
				t.Errorf("agent_id is empty: %v", payload)
			}
			if got, _ := payload["invite_id"].(string); got != cliInviteID {
				t.Errorf("invite_id = %q, want %q — an operator has to see which single-use invite was spent", got, cliInviteID)
			}
			if strings.Contains(res.Stdout+res.Stderr, cliInviteSecret) {
				t.Fatalf("the success output carries the invite SECRET: %q", res.Stdout+res.Stderr)
			}
		})

		t.Run("human", func(t *testing.T) {
			path := writeInviteFile(t, blob, 0o600)
			res, _ := runCLIArgv(t, nil, false, []string{
				"--identity", t.TempDir(), "enrol", "--invite-file", path, "--name", "planner",
			})
			if res.Code != client.ExitOK {
				t.Fatalf("enrol exited %d, want 0; stdout=%q stderr=%q", res.Code, res.Stdout, res.Stderr)
			}
			if !strings.Contains(res.Stdout, "invite     "+cliInviteID) {
				t.Errorf("human output does not print the invite id line: %q", res.Stdout)
			}
			if strings.Contains(res.Stdout+res.Stderr, cliInviteSecret) {
				t.Fatalf("the human output carries the invite SECRET: %q", res.Stdout+res.Stderr)
			}
		})
	})
}
