// Package e2e holds end-to-end acceptance harnesses that drive agent-bus the
// way an agent does: through the COMPILED CLI (`cmd/agent-busctl`), against
// real servers started through the sanctioned lifecycle script, over verified
// mutual TLS.
//
// THERE ARE DELIBERATELY NO HTTP CALLS IN THIS PACKAGE (invariant 7). Not
// net/http, not curl, not a scripts/bus-*.sh wrapper — those are retired and
// only bus-serve.sh (server lifecycle) survives. If something asserted here
// cannot be reached through a compiled subcommand, that is a MISSING HALF OF A
// FEATURE to report, never a licence to hand-write a request.
package e2e

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Command plumbing
// ---------------------------------------------------------------------------

type runResult struct {
	stdout string
	stderr string
	code   int
}

// run executes a command and returns its streams and exit status. It NEVER
// fails the test by itself: the exit status is DATA here, because several
// assertions below are precisely about which non-zero code the CLI returns.
func run(t *testing.T, extraEnv []string, name string, args ...string) runResult {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), extraEnv...)
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	res := runResult{stdout: out.String(), stderr: errb.String()}
	switch e := err.(type) {
	case nil:
		res.code = 0
	case *exec.ExitError:
		res.code = e.ExitCode()
	default:
		// Could not launch at all (binary missing, permission denied). This is
		// the fail-LOUD direction the harness contract demands: a command that
		// never ran must never look like a passing one.
		t.Fatalf("could not execute %s %v: %v", name, args, err)
	}
	return res
}

// mustRun is run() plus "exit 0 or the test dies with the diagnostic".
func mustRun(t *testing.T, what string, extraEnv []string, name string, args ...string) string {
	t.Helper()
	res := run(t, extraEnv, name, args...)
	if res.code != 0 {
		t.Fatalf("%s failed (exit %d)\ncmd: %s %s\nstdout:\n%s\nstderr:\n%s",
			what, res.code, name, strings.Join(args, " "), res.stdout, res.stderr)
	}
	return res.stdout
}

func decode(t *testing.T, what, doc string, into interface{}) {
	t.Helper()
	if err := json.Unmarshal([]byte(strings.TrimSpace(doc)), into); err != nil {
		t.Fatalf("%s did not return decodable JSON: %v\ndocument:\n%s", what, err, doc)
	}
}

// repoRoot walks up from the test's working directory to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %q; this test must run inside the module", dir)
		}
		dir = parent
	}
}

// freePorts binds n loopback sockets at once, records the kernel-assigned
// ports, then releases them all.
//
// Ports are ALLOCATED, never hard-coded: scripts/fed-smoke.sh owns 9101-9103
// and may be running concurrently. Ephemeral ports do not overlap that range.
func freePorts(t *testing.T, n int) []int {
	t.Helper()
	listeners := make([]net.Listener, 0, n)
	ports := make([]int, 0, n)
	for i := 0; i < n; i++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("could not reserve a loopback port: %v", err)
		}
		listeners = append(listeners, l)
		ports = append(ports, l.Addr().(*net.TCPAddr).Port)
	}
	for _, l := range listeners {
		_ = l.Close()
	}
	return ports
}

// ---------------------------------------------------------------------------
// JSON shapes of the compiled surface
// ---------------------------------------------------------------------------

type inviteDoc struct {
	OK          bool   `json:"ok"`
	BusID       string `json:"bus_id"`
	Fingerprint string `json:"bus_cert_fingerprint"`
}

type keyDoc struct {
	OK        bool   `json:"ok"`
	PublicKey string `json:"public_key"`
}

type enrolDoc struct {
	OK      bool   `json:"ok"`
	AgentID string `json:"agent_id"`
}

type sendDoc struct {
	OK        bool   `json:"ok"`
	MessageID string `json:"message_id"`
	Replayed  bool   `json:"replayed"`
}

type watchRecord struct {
	MessageID string   `json:"message_id"`
	From      string   `json:"from"`
	To        []string `json:"to"`
	Text      string   `json:"text"`
	Broadcast bool     `json:"broadcast"`
	BusPath   []string `json:"bus_path"`
}

type ackDoc struct {
	OK             bool   `json:"ok"`
	CorrelationKey string `json:"correlation_key"`
	Recipient      string `json:"recipient"`
	Outcome        string `json:"outcome"`
	Accepted       bool   `json:"accepted"`
	Duplicate      bool   `json:"duplicate"`
	State          string `json:"state"`
	Class          string `json:"class"`
}

type ackStatusRow struct {
	State          string `json:"state"`
	CorrelationKey string `json:"correlation_key"`
	Recipient      string `json:"recipient"`
	Class          string `json:"class"`
	AttestedBy     string `json:"attested_by"`
	// AcceptedAt is stamped when the row is opened; SettledAt only when a
	// TERMINAL outcome lands. The pair is what distinguishes "durable on this
	// bus and nothing more" (plane A) from "the addressed agent accepted it"
	// (plane C) — two facts the word "delivered" collapses, so the harness
	// asserts the timestamps and not just the state name.
	AcceptedAt string `json:"accepted_at"`
	SettledAt  string `json:"settled_at"`
}

type ackStatusDoc struct {
	OK   bool           `json:"ok"`
	Rows []ackStatusRow `json:"rows"`
}

// ---------------------------------------------------------------------------
// The three-bus fixture
// ---------------------------------------------------------------------------

type bus struct {
	name    string // A, B, C
	root    string
	runDir  string
	dataDir string
	listen  string
	url     string
	server  string // the server binary bus-serve.sh built into runDir/bin
	busID   string
	certFP  string
	signKey string
}

// agent is one enrolled principal: its own identity directory and its
// server-minted fully-qualified id (invariant 2).
type agent struct {
	identity string
	id       string
}

type fixture struct {
	repo string
	ctl  string // the compiled agent CLI — THE client
	a    *bus
	b    *bus
	c    *bus

	sender    agent // on bus A — the cross-bus sender
	recipient agent // on bus C — the cross-bus recipient
	local     agent // on bus C — a SECOND agent, for same-bus sends
}

func (f *fixture) serve(t *testing.T, bs *bus, action string) runResult {
	t.Helper()
	env := []string{
		"AGENT_BUS_RUN_DIR=" + bs.runDir,
		"AGENT_BUS_DATA_DIR=" + bs.dataDir,
		"AGENT_BUS_LISTEN=" + bs.listen,
	}
	return run(t, env, filepath.Join(f.repo, "scripts", "bus-serve.sh"), action)
}

func (f *fixture) mustServe(t *testing.T, bs *bus, action string) {
	t.Helper()
	res := f.serve(t, bs, action)
	if res.code != 0 {
		t.Fatalf("bus %s: bus-serve.sh %s failed (exit %d)\nstdout:\n%s\nstderr:\n%s",
			bs.name, action, res.code, res.stdout, res.stderr)
	}
}

// ctlRun invokes the compiled agent CLI as a given enrolled principal.
func (f *fixture) ctlRun(t *testing.T, who agent, args ...string) runResult {
	t.Helper()
	full := []string{"--identity", who.identity}
	if who.id != "" {
		full = append(full, "--as", who.id)
	}
	full = append(full, args...)
	return run(t, nil, f.ctl, full...)
}

func (f *fixture) mustCtl(t *testing.T, what string, who agent, args ...string) string {
	t.Helper()
	res := f.ctlRun(t, who, args...)
	if res.code != 0 {
		t.Fatalf("%s failed (exit %d)\ncmd: agent-busctl --identity %s --as %s %s\nstdout:\n%s\nstderr:\n%s",
			what, res.code, who.identity, who.id, strings.Join(args, " "), res.stdout, res.stderr)
	}
	return res.stdout
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

// setup builds the CLI, mints three bus identities, wires the A -> B -> C
// federation offline, restarts downstream-first, and enrols the principals.
// Every step is a compiled command; nothing scrapes a certificate file and
// nothing reads a fingerprint out of a log (invariant 11).
func setup(t *testing.T) *fixture {
	t.Helper()
	repo := repoRoot(t)

	ports := freePorts(t, 3)
	base := t.TempDir()

	newBus := func(name string, port int) *bus {
		root := filepath.Join(base, "bus-"+name)
		bs := &bus{
			name:    name,
			root:    root,
			runDir:  filepath.Join(root, "run"),
			dataDir: filepath.Join(root, "data"),
			listen:  fmt.Sprintf("127.0.0.1:%d", port),
			url:     fmt.Sprintf("https://127.0.0.1:%d", port),
		}
		bs.server = filepath.Join(bs.runDir, "bin", "agent-bus")
		// Each bus gets its OWN run dir and its OWN data dir. Two buses on one
		// data directory is forbidden, and the tracked ./data is never a fixture.
		mustMkdir(t, bs.runDir)
		mustMkdir(t, bs.dataDir)
		return bs
	}

	f := &fixture{
		repo: repo,
		a:    newBus("A", ports[0]),
		b:    newBus("B", ports[1]),
		c:    newBus("C", ports[2]),
	}

	binDir := filepath.Join(base, "bin")
	mustMkdir(t, binDir)
	f.ctl = filepath.Join(binDir, "agent-busctl")

	// THE CLIENT IS THE COMPILED CLI. Build it once, from this checkout.
	buildCmd := exec.Command("go", "build", "-o", f.ctl, "./cmd/agent-busctl")
	buildCmd.Dir = repo
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("BLOCKED: could not build cmd/agent-busctl: %v\n%s", err, out)
	}

	all := []*bus{f.a, f.b, f.c}

	// Stop every bus on the way out, before the temp roots go. t.Cleanup is
	// LIFO, so this registration — made before any bus starts, and before
	// t.TempDir's own removal is due — runs ahead of the directory teardown.
	t.Cleanup(func() {
		for _, bs := range all {
			_ = f.serve(t, bs, "stop")
		}
	})

	// First start mints each bus's server-authoritative identity, its TLS
	// certificate and its signing key (invariant 1: the SERVER owns the ids).
	for _, bs := range all {
		f.mustServe(t, bs, "start")
	}
	for _, bs := range all {
		f.mustServe(t, bs, "stop")
	}

	// Invite minting and peer configuration are offline operations on the
	// stopped data dir. The invite blob is the trust anchor (invariant 11: no
	// CA, no TOFU) and it carries the bus certificate fingerprint, so the
	// fingerprints below come from a compiled command, never from bus-tls.crt.
	mint := func(bs *bus, label string) string {
		raw := mustRun(t, "invite mint on bus "+bs.name, nil, bs.server,
			"invite", "mint", "-data-dir", bs.dataDir, "-bus-address", bs.url,
			"-label", label, "-json")
		var doc inviteDoc
		decode(t, "invite mint on bus "+bs.name, raw, &doc)
		if !doc.OK || doc.BusID == "" || doc.Fingerprint == "" {
			t.Fatalf("invite mint on bus %s returned an unusable document: %s", bs.name, raw)
		}
		// Re-minting on the same bus must never change its identity.
		if bs.busID != "" && (bs.busID != doc.BusID || bs.certFP != doc.Fingerprint) {
			t.Fatalf("bus %s changed identity between invites: %s/%s then %s/%s",
				bs.name, bs.busID, bs.certFP, doc.BusID, doc.Fingerprint)
		}
		bs.busID = doc.BusID
		bs.certFP = doc.Fingerprint
		return strings.TrimSpace(raw)
	}

	inviteSender := mint(f.a, "e2e-ack-sender")
	inviteRecipient := mint(f.c, "e2e-ack-recipient")
	inviteLocal := mint(f.c, "e2e-ack-local-sender")
	// B's invite exists ONLY as a compiled way to read B's server-minted bus id
	// and certificate fingerprint. No agent redeems it.
	_ = mint(f.b, "e2e-ack-peer-metadata")

	// Invite blobs carry bearer secrets: 0600 files, never argv.
	newAgentDir := func(name, blob string) (string, string) {
		dir := filepath.Join(base, "identity-"+name)
		mustMkdir(t, dir)
		path := filepath.Join(dir, "invite.json")
		if err := os.WriteFile(path, []byte(blob+"\n"), 0o600); err != nil {
			t.Fatalf("writing invite to %s: %v", path, err)
		}
		return dir, path
	}
	senderDir, senderInvite := newAgentDir("sender", inviteSender)
	recipientDir, recipientInvite := newAgentDir("recipient", inviteRecipient)
	localDir, localInvite := newAgentDir("local", inviteLocal)

	for _, bs := range all {
		raw := mustRun(t, "key export-public on bus "+bs.name, nil, bs.server,
			"key", "export-public", "--data-dir", bs.dataDir, "--json")
		var doc keyDoc
		decode(t, "key export-public on bus "+bs.name, raw, &doc)
		if !doc.OK || doc.PublicKey == "" {
			t.Fatalf("key export-public on bus %s returned an unusable document: %s", bs.name, raw)
		}
		bs.signKey = doc.PublicKey
	}

	// -tls-fingerprint (on the ROUTE) is OUTBOUND: the server certificate the
	// hop at -url presents when WE dial IT.
	// -peer-client-fingerprint (on the TRUST record) is INBOUND: the client
	// certificate that bus presents when IT dials US.
	// They are OPPOSITE DIRECTIONS. Do not collapse them.
	route := func(bs *bus, peer *bus, routeFor ...string) {
		args := []string{"peer", "add", "-data-dir", bs.dataDir,
			"-bus-id", peer.busID, "-url", peer.url,
			"-tls-fingerprint", peer.certFP, "-json"}
		for _, dest := range routeFor {
			args = append(args, "-route-for", dest)
		}
		mustRun(t, fmt.Sprintf("peer add (route) on bus %s for %s", bs.name, peer.name), nil, bs.server, args...)
	}
	trust := func(bs *bus, peer *bus, inboundFP string) {
		args := []string{"peer", "add", "-data-dir", bs.dataDir,
			"-bus-id", peer.busID, "-signing-key", peer.signKey, "-json"}
		if inboundFP != "" {
			args = append(args, "-peer-client-fingerprint", inboundFP)
		}
		mustRun(t, fmt.Sprintf("peer add (trust) on bus %s for %s", bs.name, peer.name), nil, bs.server, args...)
	}

	// A reaches C through B; A pins C's signing key independently of that route.
	route(f.a, f.b, f.c.busID)
	trust(f.a, f.b, f.b.certFP)
	trust(f.a, f.c, "") // A never accepts an inbound connection FROM C here.

	// B is adjacent to both endpoints, so both its trust records bind inbound.
	route(f.b, f.a)
	trust(f.b, f.a, f.a.certFP)
	route(f.b, f.c)
	trust(f.b, f.c, f.c.certFP)

	// C routes to B only; A is signing-trust only.
	route(f.c, f.b)
	trust(f.c, f.b, f.b.certFP)
	trust(f.c, f.a, "")

	// Downstream first: an upstream forwarder must not race an unavailable hop.
	f.mustServe(t, f.c, "start")
	f.mustServe(t, f.b, "start")
	f.mustServe(t, f.a, "start")

	enrol := func(dir, inviteFile, name string) agent {
		raw := mustRun(t, "enrol "+name, nil, f.ctl,
			"--identity", dir, "--json", "enrol",
			"--invite-file", inviteFile, "--name", name)
		var doc enrolDoc
		decode(t, "enrol "+name, raw, &doc)
		if !doc.OK || doc.AgentID == "" {
			t.Fatalf("enrol %s returned an unusable document: %s", name, raw)
		}
		return agent{identity: dir, id: doc.AgentID}
	}
	f.sender = enrol(senderDir, senderInvite, "e2e-ack-sender")
	f.recipient = enrol(recipientDir, recipientInvite, "e2e-ack-recipient")
	f.local = enrol(localDir, localInvite, "e2e-ack-local-sender")

	t.Logf("topology: A=%s (%s)  B=%s (%s)  C=%s (%s)",
		f.a.busID, f.a.listen, f.b.busID, f.b.listen, f.c.busID, f.c.listen)
	t.Logf("cross-bus sender=%s  recipient=%s  same-bus sender=%s",
		f.sender.id, f.recipient.id, f.local.id)
	return f
}

// assertQualified enforces invariant 2: every agent id is <bus-id>.<agent-id>,
// never shortened. The correlation assertions below depend on it.
func assertQualified(t *testing.T, what, agentID, busID string) {
	t.Helper()
	if !strings.HasPrefix(agentID, busID+".") {
		t.Fatalf("%s: agent id %q is not qualified by its bus id %q (invariant 2)", what, agentID, busID)
	}
	if strings.TrimSpace(strings.TrimPrefix(agentID, busID+".")) == "" {
		t.Fatalf("%s: agent id %q has an empty local part (invariant 2)", what, agentID)
	}
}

// assertMessageIDShape enforces the MESSAGE-id form "<bus-id>-<seq>": the
// minting bus, a HYPHEN, and a decimal sequence (internal/ids/messageid.go).
//
// THIS IS A DIFFERENT ID FAMILY FROM assertQualified, AND CONFUSING THE TWO IS
// A TEST BUG THAT LOOKS EXACTLY LIKE A PRODUCT BUG. An AGENT id is
// "<bus-id>.<agent-id>" — a DOT. An earlier revision of this harness ran the
// agent-id check over a message id and failed with `agent id "bus-…-9" is not
// qualified by its bus id "bus-…" (invariant 2)`, naming a perfectly valid
// server-minted id as an invariant violation. A harness that cries invariant
// breach over its own mistake is worse than no harness: the next reader spends
// the afternoon in internal/ids.
//
// The sequence is checked digit by digit rather than by calling
// ids.ParseMessageID, because a harness that validates ids with the very parser
// under test passes on a bug in that parser. The "one id, one spelling" rules
// (decimal only, no leading zero, sequences start at 1) are therefore restated
// here independently — two spellings of one id defeat idempotency, invariant 10.
func assertMessageIDShape(t *testing.T, what, messageID, busID string) {
	t.Helper()
	prefix := busID + "-"
	if !strings.HasPrefix(messageID, prefix) {
		t.Fatalf("%s: message id %q is not namespaced by its minting bus %q — a message id is %q<seq> (invariant 1: the SERVER owns every id)",
			what, messageID, busID, prefix)
	}
	seq := strings.TrimPrefix(messageID, prefix)
	if seq == "" {
		t.Fatalf("%s: message id %q carries no sequence after %q", what, messageID, prefix)
	}
	for i := 0; i < len(seq); i++ {
		if seq[i] < '0' || seq[i] > '9' {
			t.Fatalf("%s: message id %q has the non-decimal sequence %q; the form is <bus-id>-<decimal seq>",
				what, messageID, seq)
		}
	}
	if seq[0] == '0' {
		t.Fatalf("%s: message id %q has a leading zero in the sequence %q; sequence 0 is never allocated and each id has exactly ONE spelling",
			what, messageID, seq)
	}
}

// send publishes one direct message and returns the id the ACCEPTING bus
// minted for it (invariant 1: the server owns every id).
func (f *fixture) send(t *testing.T, from agent, to, payload, idemKey string) string {
	t.Helper()
	raw := f.mustCtl(t, "send "+idemKey, from,
		"--json", "send", "--idempotency-key", idemKey, to, payload)
	var doc sendDoc
	decode(t, "send "+idemKey, raw, &doc)
	if !doc.OK || doc.MessageID == "" {
		t.Fatalf("send %s returned an unusable document: %s", idemKey, raw)
	}
	if doc.Replayed {
		t.Fatalf("the first send under key %q reported replayed=true", idemKey)
	}
	return doc.MessageID
}

// waitForDelivery runs bounded replay-watches on C until the payload shows up,
// and fails LOUDLY — never skips — if it never does.
func (f *fixture) waitForDelivery(t *testing.T, payload, from string) watchRecord {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for attempt := 1; ; attempt++ {
		matches := f.watchMatches(t, payload, from)
		if len(matches) > 1 {
			// --replay --no-cursor replays the whole retained window, so a
			// SECOND copy of the logical message WOULD appear here. This is
			// therefore the exactly-once check on the recipient-visible surface
			// (invariant 10: with a cyclic topology and at-least-once hops,
			// duplicates are the steady state and must be suppressed).
			t.Fatalf("the recipient observed %d copies of %q; it must be delivered exactly once",
				len(matches), payload)
		}
		if len(matches) == 1 {
			return matches[0]
		}
		if time.Now().After(deadline) {
			t.Fatalf("DELIVERY GATE FAILED: the recipient on bus C never observed %q from %s "+
				"within 60s. This gate is the OBSERVED RELAY itself and deliberately not /healthz: "+
				"a bus reports healthy while every /v1/peer/ path 404s, which has produced a "+
				"confident false pass before.", payload, from)
		}
		t.Logf("watch attempt %d saw no delivery of %q yet; retrying", attempt, payload)
	}
}

// watchMatches runs ONE bounded `watch --replay --no-cursor` and returns the
// records matching the payload, sender and audience.
func (f *fixture) watchMatches(t *testing.T, payload, from string) []watchRecord {
	t.Helper()
	res := f.ctlRun(t, f.recipient,
		"--json", "watch", "--replay", "--no-cursor", "--for", "15s", "--poll-timeout", "1s")
	// 0 = streamed something, 8 = the bounded window ended with nothing to
	// report. Both are legitimate outcomes of a time-boxed watch.
	if res.code != 0 && res.code != 8 {
		t.Fatalf("recipient watch failed with exit %d (want 0, or the bounded-timeout 8)\nstdout:\n%s\nstderr:\n%s",
			res.code, res.stdout, res.stderr)
	}
	var out []watchRecord
	scanner := bufio.NewScanner(strings.NewReader(res.stdout))
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var rec watchRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("the recipient watch stream emitted a line that is not NDJSON: %v\nline: %s", err, line)
		}
		if rec.Text != payload || rec.From != from || rec.Broadcast {
			continue
		}
		if len(rec.To) != 1 || rec.To[0] != f.recipient.id {
			continue
		}
		out = append(out, rec)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("reading the recipient watch stream: %v", err)
	}
	return out
}

// ack settles (or refuses) one message AS THE RECIPIENT on bus C. A nil refuse
// asserts `delivered`; a non-nil one asserts that refusal class verbatim,
// INCLUDING the empty string, which must be an error and not a silent ack.
func (f *fixture) ack(t *testing.T, messageID string, refuse *string) (runResult, ackDoc) {
	t.Helper()
	args := []string{"--json", "ack", messageID}
	if refuse != nil {
		args = append(args, "--refuse", *refuse)
	}
	res := f.ctlRun(t, f.recipient, args...)
	var doc ackDoc
	if strings.TrimSpace(res.stdout) != "" {
		// The CLI prints the result BEFORE it decides the exit code, so a
		// non-zero exit still carries a decodable document.
		if err := json.Unmarshal([]byte(strings.TrimSpace(res.stdout)), &doc); err != nil {
			t.Fatalf("ack %s printed undecodable JSON: %v\nstdout:\n%s", messageID, err, res.stdout)
		}
	}
	return res, doc
}

func (f *fixture) ackStatus(t *testing.T, who agent, key string) (runResult, ackStatusDoc) {
	t.Helper()
	res := f.ctlRun(t, who, "--json", "ack-status", key)
	var doc ackStatusDoc
	if strings.TrimSpace(res.stdout) != "" {
		if err := json.Unmarshal([]byte(strings.TrimSpace(res.stdout)), &doc); err != nil {
			t.Fatalf("ack-status %s printed undecodable JSON: %v\nstdout:\n%s", key, err, res.stdout)
		}
	}
	return res, doc
}

// soleRow returns the single row a status document must carry, or fails.
func soleRow(t *testing.T, what string, doc ackStatusDoc) ackStatusRow {
	t.Helper()
	if len(doc.Rows) != 1 {
		t.Fatalf("%s: ack-status returned %d rows, want exactly 1 (%+v)", what, len(doc.Rows), doc)
	}
	return doc.Rows[0]
}

// normalisedJSON re-encodes a document into a canonical form — encoding/json
// marshals map keys in sorted order — so two answers can be compared for
// EQUALITY rather than merely for "both of them said unknown".
//
// The distinction matters for the status oracle below. Two responses that agree
// on `state` but differ in any other field are still an oracle: the difference
// is the disclosure. Only byte-equality of the whole document proves the caller
// learned nothing.
func normalisedJSON(t *testing.T, what, doc string) string {
	t.Helper()
	var v interface{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(doc)), &v); err != nil {
		t.Fatalf("%s is not decodable JSON: %v\ndocument:\n%s", what, err, doc)
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("%s could not be re-encoded: %v", what, err)
	}
	return string(b)
}

func str(s string) *string { return &s }

// ---------------------------------------------------------------------------
// The acceptance test
// ---------------------------------------------------------------------------

const (
	payloadRelayed      = "ack-12:e2e:a-to-c:relayed:v1"
	payloadLocalDeliver = "ack-12:e2e:c-to-c:delivered:v1"
	payloadLocalRefuse  = "ack-12:e2e:c-to-c:refused:v1"
)

// TestThreeBusEndToEndAckNack drives a three-bus federation (A sender -> B
// transit -> C recipient) entirely through the compiled CLI and asserts the
// ACK plane AS IT ACTUALLY IS at HEAD — including, explicitly, the two places
// it does not yet reach.
//
// WHAT IS TRUE TODAY, in one paragraph, because it is not what the CLI help
// text implies. Message relay A -> B -> C works and delivers exactly once with
// a complete bus_path. The delivery-lifecycle (ack) plane, however, is a
// SINGLE-BUS surface: `Hub.recordAcceptance` returns early for a relayed
// message (internal/hub/hub.go, `if h.acks == nil || relayed || broadcast`), so
// the receiving bus opens NO lifecycle row and the recipient of a relayed
// message cannot acknowledge it at all — `agent-busctl ack` answers `unknown`
// with exit 8 for BOTH the origin id and the receiving bus's own id. On the
// origin bus the row exists and stays at `accepted` forever, because
// `relay.Client.PeerAck` and `ack.Store.MarkInFlight` both have no non-test
// caller.
//
// So the ack/nack/absorbing semantics are asserted here on a SAME-BUS message
// between two agents on bus C, where the plane is live, and the cross-bus gaps
// are asserted as gaps. No leaf may t.Skip: a parent that PASSES because every
// leaf skipped is scored VACUOUS, and a gap that passes silently is how
// un-fireable guards get shipped.
//
// THERE IS DELIBERATELY NO -short GUARD. One existed and was REMOVED, and the
// reason is stated from a MEASUREMENT rather than from a worry.
//
// Measured 2026-08-21, on the revision that still had the guard, under
// `go test -race -short`:
//
//	--- SKIP: TestThreeBusEndToEndAckNack (0.00s)
//	PASS
//	ok  github.com/dodgymike/agent-bus/tests/e2e  0.027s
//	proof-check: verdict=VACUOUS ... tests_run=1 skipped=1 failed=0
//
// So proof-check.sh DOES catch it — that guard was a latent trap, not an active
// false pass, and this comment must not claim otherwise. The trap is the two
// lines above the verdict: a lane that runs bare `go test` instead of
// proof-check.sh sees `PASS`, `ok`, exit 0, and a P0 acceptance test that
// exercised nothing. Every one of the five vacuity failures this repository has
// already had looked exactly like that.
//
// If this harness is too slow for some lane, exclude the PACKAGE there, loudly
// and visibly. Do not teach the test to disappear on a flag.
func TestThreeBusEndToEndAckNack(t *testing.T) {
	f := setup(t)

	var (
		relayedOriginID     string // the id bus A minted — what the SENDER holds
		relayedRecipientID  string // the id bus C minted — what the RECIPIENT sees
		localDeliveredID    string // same-bus message, settled `delivered`
		localRefusedID      string // same-bus message, settled `refused`
		localDeliveredSeen  bool
		localRefusedSeen    bool
		relayedDeliverySeen bool
	)

	t.Run("send_relays_a_to_c", func(t *testing.T) {
		assertQualified(t, "cross-bus sender", f.sender.id, f.a.busID)
		assertQualified(t, "recipient", f.recipient.id, f.c.busID)
		assertQualified(t, "same-bus sender", f.local.id, f.c.busID)

		relayedOriginID = f.send(t, f.sender, f.recipient.id, payloadRelayed, "ack-12-e2e-relayed-v1")
		rec := f.waitForDelivery(t, payloadRelayed, f.sender.id)
		relayedRecipientID = rec.MessageID
		relayedDeliverySeen = true

		want := []string{f.a.busID, f.b.busID, f.c.busID}
		if len(rec.BusPath) != len(want) {
			t.Fatalf("bus_path = %v, want the full three-hop path %v", rec.BusPath, want)
		}
		for i := range want {
			if rec.BusPath[i] != want[i] {
				t.Fatalf("bus_path = %v, want %v", rec.BusPath, want)
			}
		}

		// EVERY BUS MINTS ITS OWN ID (invariant 1). The origin id A returned to
		// the sender is namespaced to A; the id the recipient sees is minted by
		// C and namespaced to C. They are DIFFERENT ids for one logical
		// message, and no assertion here may assume otherwise.
		//
		// BOTH ID FAMILIES ARE ASSERTED IN THIS SUBTEST, and they are not the
		// same shape: the assertQualified calls above check the AGENT form
		// <bus-id>.<agent-id> (a DOT); these check the MESSAGE form
		// <bus-id>-<seq> (a HYPHEN). See assertMessageIDShape for why mixing
		// them up is the specific mistake this harness has already made once.
		assertMessageIDShape(t, "origin id minted by bus A", relayedOriginID, f.a.busID)
		assertMessageIDShape(t, "recipient-visible id minted by bus C", relayedRecipientID, f.c.busID)
		if relayedOriginID == relayedRecipientID {
			t.Fatalf("origin and recipient-visible ids are both %q; each bus mints its own (invariant 1)",
				relayedOriginID)
		}
		t.Logf("one logical message, two ids: origin on A = %s, recipient-visible on C = %s",
			relayedOriginID, relayedRecipientID)
	})

	// -----------------------------------------------------------------------
	// THE FIRST HONEST GAP ASSERTION — read this before "fixing" it.
	//
	// The delivery-lifecycle plane does NOT span the relay. Hub.recordAcceptance
	// bails on a relayed message, so bus C holds no lifecycle row for anything
	// that arrived over a peer link, and the recipient CANNOT acknowledge it —
	// with either id. `agent-busctl ack` help says the message id "identifies
	// the message across every hop it took to reach you"; today no id works,
	// because there is nothing on this bus to settle.
	//
	// This is asserted rather than merely written down, because a gap nobody
	// asserts is indistinguishable from a feature that works. It is EXPECTED TO
	// GO RED the day relay ingest opens a lifecycle row, at which point it must
	// be INVERTED to assert that the recipient CAN ack a relayed message — not
	// deleted, and not loosened.
	// -----------------------------------------------------------------------
	t.Run("relayed_message_cannot_yet_be_acked_on_the_receiving_bus", func(t *testing.T) {
		if !relayedDeliverySeen {
			t.Fatalf("no relayed message to probe: send_relays_a_to_c did not establish one")
		}
		for _, probe := range []struct {
			label string
			id    string
		}{
			{"the id bus C minted and served to the recipient", relayedRecipientID},
			{"the origin id bus A minted for the sender", relayedOriginID},
		} {
			res, doc := f.ack(t, probe.id, nil)
			t.Logf("ack on C using %s (%s): exit=%d doc=%+v", probe.label, probe.id, res.code, doc)
			if res.code != 8 {
				t.Fatalf("ack on C using %s (%s) exited %d, want 8 (nothing to record). "+
					"If relay ingest now opens a delivery-lifecycle row, this subtest has done its "+
					"job: INVERT it to assert that a relayed message CAN be acknowledged, and update "+
					"the comment above it. Do not delete it.\nstdout:\n%s\nstderr:\n%s",
					probe.label, probe.id, res.code, res.stdout, res.stderr)
			}
			if doc.State != "unknown" {
				t.Fatalf("ack on C using %s reported state %q, want %q", probe.label, doc.State, "unknown")
			}
			if doc.Accepted {
				t.Fatalf("ack on C using %s reported accepted=true while state is unknown: %+v",
					probe.label, doc)
			}
			// The recipient is still named in full even when there is nothing
			// to settle (invariant 2).
			if doc.Recipient != f.recipient.id {
				t.Fatalf("ack echoed recipient %q, want the fully-qualified %q", doc.Recipient, f.recipient.id)
			}
		}
	})

	t.Run("recipient_ack_settles_locally", func(t *testing.T) {
		// SAME-BUS message: bus C both accepts and delivers it, so the
		// lifecycle row exists and the ack plane is live. This is the ONLY
		// shape in which an application-level delivery notification works at
		// HEAD, and it is asserted here so the plane's real semantics are
		// proven rather than merely reported absent.
		localDeliveredID = f.send(t, f.local, f.recipient.id, payloadLocalDeliver, "ack-12-e2e-local-delivered-v1")
		rec := f.waitForDelivery(t, payloadLocalDeliver, f.local.id)
		localDeliveredSeen = true

		assertMessageIDShape(t, "same-bus id minted by bus C", localDeliveredID, f.c.busID)

		// One bus, one mint: the sender's id and the recipient's id ARE the
		// same here — the exact contrast with the relayed case above.
		if rec.MessageID != localDeliveredID {
			t.Fatalf("same-bus message: sender saw %q and recipient saw %q; one bus mints one id",
				localDeliveredID, rec.MessageID)
		}
		if len(rec.BusPath) != 1 || rec.BusPath[0] != f.c.busID {
			t.Fatalf("same-bus bus_path = %v, want exactly [%s]", rec.BusPath, f.c.busID)
		}

		// BEFORE the ack the row exists and says only what plane A can say:
		// this bus has committed the message (invariant 4). It is `accepted`,
		// it is stamped accepted_at, and it is settled by nobody. Reading it
		// here is what turns the check below into a TRANSITION rather than a
		// snapshot that would also pass if the row had been born `delivered`.
		bres, bdoc := f.ackStatus(t, f.local, localDeliveredID)
		if bres.code != 0 {
			t.Fatalf("ack-status before the ack exited %d, want 0\nstderr:\n%s", bres.code, bres.stderr)
		}
		before := soleRow(t, "the same-bus sender BEFORE the recipient acks", bdoc)
		if before.State != "accepted" {
			t.Fatalf("before the recipient acked, the row is %q; want %q — an unacknowledged message must "+
				"never present as delivered", before.State, "accepted")
		}
		if before.AcceptedAt == "" {
			t.Fatalf("an accepted row carries no accepted_at: %+v", before)
		}
		if before.SettledAt != "" {
			t.Fatalf("an unsettled row carries settled_at %q; only a TERMINAL outcome stamps it", before.SettledAt)
		}
		if before.AttestedBy != "" {
			t.Fatalf("an unsettled row is attested by %q; nobody has asserted anything yet", before.AttestedBy)
		}

		res, doc := f.ack(t, localDeliveredID, nil)
		if res.code != 0 {
			t.Fatalf("ack exited %d, want 0\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
		}
		if !doc.OK || !doc.Accepted {
			t.Fatalf("ack was not accepted: %+v", doc)
		}
		if doc.Outcome != "delivered" {
			t.Fatalf("ack outcome = %q, want %q — delivered is the default assertion", doc.Outcome, "delivered")
		}
		if doc.State != "delivered" {
			t.Fatalf("ack state = %q, want %q", doc.State, "delivered")
		}
		if doc.Duplicate {
			t.Fatalf("the FIRST ack reported duplicate=true: %+v", doc)
		}
		if doc.Recipient != f.recipient.id {
			t.Fatalf("ack recipient = %q, want the fully-qualified %q (invariant 2)", doc.Recipient, f.recipient.id)
		}
		if doc.Class != "" {
			t.Fatalf("a delivered ack carried class %q; class appears only on a negative terminal", doc.Class)
		}

		// ONLY THE SENDER SEES A ROW. The same-bus sender is the sender here,
		// so it is the principal that reads the settled outcome back.
		sres, sdoc := f.ackStatus(t, f.local, localDeliveredID)
		if sres.code != 0 {
			t.Fatalf("ack-status for the same-bus sender exited %d, want 0\nstderr:\n%s", sres.code, sres.stderr)
		}
		row := soleRow(t, "same-bus sender after a delivered ack", sdoc)
		if row.State != "delivered" {
			t.Fatalf("ack-status state = %q, want %q (%+v)", row.State, "delivered", sdoc)
		}
		if row.Recipient != f.recipient.id {
			t.Fatalf("ack-status recipient = %q, want %q (invariant 2)", row.Recipient, f.recipient.id)
		}
		// attested_by IS A LABEL, NOT A PROOF, and this is the only value a
		// recipient assertion can carry: the signature's SHAPE was checked and
		// its authenticity was checked by nobody. There is no "verified" value
		// and this system cannot produce one.
		if row.AttestedBy != "recipient_signature_unverified" {
			t.Fatalf("ack-status attested_by = %q, want %q", row.AttestedBy, "recipient_signature_unverified")
		}
		// THE TRANSITION, not a snapshot: same accepted_at, newly stamped
		// settled_at. accepted_at must NOT move — the acceptance is a fact about
		// an earlier moment and a settlement does not rewrite it.
		if row.AcceptedAt != before.AcceptedAt {
			t.Fatalf("settling rewrote accepted_at from %q to %q; the acceptance happened when it happened",
				before.AcceptedAt, row.AcceptedAt)
		}
		if row.SettledAt == "" {
			t.Fatalf("a delivered row carries no settled_at: %+v — the terminal outcome must be stamped", row)
		}
	})

	t.Run("recipient_nack_is_authenticated_and_classed", func(t *testing.T) {
		localRefusedID = f.send(t, f.local, f.recipient.id, payloadLocalRefuse, "ack-12-e2e-local-refused-v1")
		f.waitForDelivery(t, payloadLocalRefuse, f.local.id)
		localRefusedSeen = true

		// `--refuse` WITH AN EMPTY VALUE IS EXIT 2 AND MUST NOT ACK. An unset
		// $CLASS must never be silently promoted to `delivered`: a terminal
		// outcome is ABSORBING and cannot be taken back.
		emptyRes, emptyDoc := f.ack(t, localRefusedID, str(""))
		if emptyRes.code != 2 {
			t.Fatalf("ack --refuse '' exited %d, want 2 (bad usage)\nstdout:\n%s\nstderr:\n%s",
				emptyRes.code, emptyRes.stdout, emptyRes.stderr)
		}
		if emptyDoc.Accepted || emptyDoc.State != "" {
			t.Fatalf("ack --refuse '' recorded something: %+v", emptyDoc)
		}
		// And it settled NOTHING: the sender's row is still open, so a real
		// refusal is still possible below. This is what makes the exit-2 check
		// more than a string comparison.
		pres, pdoc := f.ackStatus(t, f.local, localRefusedID)
		if pres.code != 0 {
			t.Fatalf("ack-status after the empty refusal exited %d, want 0\nstderr:\n%s", pres.code, pres.stderr)
		}
		if row := soleRow(t, "after an empty --refuse", pdoc); row.State != "accepted" {
			t.Fatalf("after `--refuse ''` the row is %q; it must still be %q, unsettled",
				row.State, "accepted")
		}

		// The real refusal: a CLASS, never free text. A recipient says THAT it
		// refused, never in its own words WHY.
		res, doc := f.ack(t, localRefusedID, str("recipient_refused_policy"))
		if res.code != 0 {
			t.Fatalf("ack --refuse recipient_refused_policy exited %d, want 0\nstdout:\n%s\nstderr:\n%s",
				res.code, res.stdout, res.stderr)
		}
		if doc.Outcome != "refused" || doc.State != "refused" {
			t.Fatalf("refusal outcome/state = %q/%q, want refused/refused (%+v)", doc.Outcome, doc.State, doc)
		}
		if doc.Class != "recipient_refused_policy" {
			t.Fatalf("refusal class = %q, want %q", doc.Class, "recipient_refused_policy")
		}
		if !doc.Accepted || doc.Duplicate {
			t.Fatalf("the first refusal must be accepted and not a duplicate: %+v", doc)
		}
		if doc.Recipient != f.recipient.id {
			t.Fatalf("refusal recipient = %q, want %q (invariant 2)", doc.Recipient, f.recipient.id)
		}

		sres, sdoc := f.ackStatus(t, f.local, localRefusedID)
		if sres.code != 0 {
			t.Fatalf("ack-status after a refusal exited %d, want 0\nstderr:\n%s", sres.code, sres.stderr)
		}
		row := soleRow(t, "same-bus sender after a refusal", sdoc)
		if row.State != "refused" || row.Class != "recipient_refused_policy" {
			t.Fatalf("ack-status after refusal = state %q class %q, want refused/recipient_refused_policy (%+v)",
				row.State, row.Class, sdoc)
		}
		if row.AttestedBy != "recipient_signature_unverified" {
			t.Fatalf("a refusal's attested_by = %q, want %q — a NACK is attested exactly as an ACK is",
				row.AttestedBy, "recipient_signature_unverified")
		}
	})

	t.Run("terminal_outcome_is_absorbing", func(t *testing.T) {
		if !localDeliveredSeen || !localRefusedSeen {
			t.Fatalf("no settled messages to re-acknowledge; earlier subtests did not establish them")
		}

		// SAME KEY + SAME OUTCOME IS A LEGITIMATE RETRY (invariant 10): it is
		// accepted, `duplicate` is true, nothing is re-applied, and NOBODY IS
		// DISCONNECTED. The ack was probably lost in flight, and punishing the
		// retry would break exactly the clients doing the right thing.
		res, doc := f.ack(t, localDeliveredID, nil)
		if res.code != 0 {
			t.Fatalf("re-acking the SAME outcome exited %d, want 0 — a retry must not be punished\nstderr:\n%s",
				res.code, res.stderr)
		}
		if !doc.Duplicate {
			t.Fatalf("re-acking the same outcome did not report duplicate=true: %+v", doc)
		}
		if doc.State != "delivered" {
			t.Fatalf("re-ack state = %q, want the original %q to stand", doc.State, "delivered")
		}

		// Changing your mind is NOT safe: exit 7, and the FIRST outcome stands.
		changed, changedDoc := f.ack(t, localDeliveredID, str("recipient_refused_policy"))
		if changed.code != 7 {
			t.Fatalf("changing a settled outcome exited %d, want 7 (already terminal, DIFFERENT outcome)\nstdout:\n%s\nstderr:\n%s",
				changed.code, changed.stdout, changed.stderr)
		}
		if changedDoc.State != "" && changedDoc.State != "delivered" {
			t.Fatalf("a refusal attempt on a delivered message left state %q; terminal is ABSORBING",
				changedDoc.State)
		}

		// Absorbing in the other direction too: a refused message cannot be
		// upgraded to delivered.
		back, backDoc := f.ack(t, localRefusedID, nil)
		if back.code != 7 {
			t.Fatalf("upgrading a refused message to delivered exited %d, want 7\nstdout:\n%s\nstderr:\n%s",
				back.code, back.stdout, back.stderr)
		}
		if backDoc.State != "" && backDoc.State != "refused" {
			t.Fatalf("a delivered attempt on a refused message left state %q; terminal is ABSORBING",
				backDoc.State)
		}
		// Re-refusing with the SAME class is the retry case again.
		again, againDoc := f.ack(t, localRefusedID, str("recipient_refused_policy"))
		if again.code != 0 || !againDoc.Duplicate || againDoc.State != "refused" {
			t.Fatalf("re-refusing with the same class must be a benign duplicate: exit=%d doc=%+v",
				again.code, againDoc)
		}

		// NOT DISCONNECTED. The same principal still transacts after a
		// duplicate AND after a conflicting attempt. Neither may drop the
		// connection: only replay of an already-accepted SIGNED message does,
		// and neither of these is that.
		after, afterDoc := f.ackStatus(t, f.local, localDeliveredID)
		if after.code != 0 {
			t.Fatalf("could not read ack-status after a duplicate and a conflicting ack (exit %d) — "+
				"neither may disconnect (invariant 10)\nstderr:\n%s", after.code, after.stderr)
		}
		if row := soleRow(t, "after the conflicting attempt", afterDoc); row.State != "delivered" {
			t.Fatalf("after the conflicting attempt the state is %q, want the original delivered to stand",
				row.State)
		}
		refusedAfter, refusedDoc := f.ackStatus(t, f.local, localRefusedID)
		if refusedAfter.code != 0 {
			t.Fatalf("could not read ack-status for the refused message afterwards (exit %d)\nstderr:\n%s",
				refusedAfter.code, refusedAfter.stderr)
		}
		if row := soleRow(t, "refused message afterwards", refusedDoc); row.State != "refused" {
			t.Fatalf("the refused message is now %q; terminal is ABSORBING", row.State)
		}
	})

	// -----------------------------------------------------------------------
	// THE SECOND HONEST GAP ASSERTION — read this before "fixing" it.
	//
	// relay.Client.PeerAck (internal/relay/ackhttp.go) is BUILT AND UNWIRED: it
	// has no non-test caller. ack.Store.MarkInFlight is likewise unwired. So a
	// recipient's outcome on bus C does not travel back up C -> B -> A, and the
	// sender's row on A never leaves `accepted` — it does not even reach
	// `in_flight`, despite a hop having been owed and taken.
	//
	// Nothing bounces either: retry-to-exhaustion and return-to-sender
	// (ACK-13 / ACK-7 / ACK-14) are open, so no TERMINAL state of any kind can
	// reach the origin bus. That absence is asserted here rather than left as a
	// skipped placeholder, because a skipped leaf makes its parent PASS while
	// exercising nothing.
	//
	// This subtest is EXPECTED TO GO RED the day ACK-5 wires the upstream
	// peer-ACK emitter, at which point it must be INVERTED to assert
	// propagation — not deleted, and not loosened.
	// -----------------------------------------------------------------------
	t.Run("ack_does_not_yet_propagate_to_origin_bus", func(t *testing.T) {
		if !relayedDeliverySeen {
			t.Fatalf("no relayed message to query: send_relays_a_to_c did not establish one")
		}

		res, doc := f.ackStatus(t, f.sender, relayedOriginID)
		t.Logf("the SENDER's view on bus A of origin id %s, after the message was delivered to "+
			"the recipient on bus C: exit=%d doc=%+v", relayedOriginID, res.code, doc)

		if res.code != 0 {
			t.Fatalf("ack-status on A exited %d, want 0 — the sender's own row must be readable\nstderr:\n%s",
				res.code, res.stderr)
		}
		row := soleRow(t, "the sender's view on bus A", doc)
		if row.CorrelationKey != relayedOriginID {
			t.Fatalf("ack-status on A keyed %q, want the origin id %q", row.CorrelationKey, relayedOriginID)
		}
		if row.Recipient != f.recipient.id {
			t.Fatalf("ack-status on A names recipient %q, want the fully-qualified cross-bus %q (invariant 2)",
				row.Recipient, f.recipient.id)
		}
		// EXACTLY `accepted`: durable and fsynced on the origin bus, and
		// acknowledged by nobody.
		if row.State != "accepted" {
			t.Fatalf("the sender's row on bus A is %q (class %q, attested_by %q), want exactly %q. "+
				"If the upstream peer-ACK emitter (ACK-5) or the in-flight transition is now wired, "+
				"this subtest has done its job: INVERT it to assert propagation, and update the "+
				"comment above it. Do not delete it and do not loosen it.",
				row.State, row.Class, row.AttestedBy, "accepted")
		}
		if row.AttestedBy != "" {
			t.Fatalf("the sender's row carries attested_by %q; nothing has attested anything to the "+
				"origin bus, so this must be empty", row.AttestedBy)
		}
		if row.Class != "" {
			t.Fatalf("the sender's row carries class %q; an unsettled row has no reason class", row.Class)
		}
	})

	// -----------------------------------------------------------------------
	// THE STATUS ORACLE (ACK-CONTRACT.md §13.3) — a SECURITY property, and the
	// only subtest here whose value is entirely in what the answers do NOT say.
	//
	// "Only the ORIGINAL SENDER may read a row. Every other case — key never
	// existed, key swept, key belongs to someone else — returns the SAME
	// answer: 200 with state `unknown`."
	//
	// A 403, or a 200 that differed in any field, would confirm that a guessed
	// id names a real message: an id-guessing oracle over the whole messaging
	// surface. THE INDISTINGUISHABILITY IS THE PROPERTY, so this asserts that
	// the answers are EQUAL — not merely that each one says `unknown`, which is
	// the weaker check that would pass even if the real key leaked a recipient.
	//
	// Do not "improve" this by making the not-found case more helpful.
	// -----------------------------------------------------------------------
	t.Run("status_is_a_uniform_oracle_for_anyone_but_the_sender", func(t *testing.T) {
		if !localDeliveredSeen || !relayedDeliverySeen {
			t.Fatalf("the oracle probes need an established real key; earlier subtests did not provide one")
		}

		// Every probe is made by the RECIPIENT on bus C. It is the strongest
		// available prober: for the first key it is not a stranger at all — it
		// is the agent the message was addressed to, and it settled that very
		// message minutes ago. If ANY principal but the sender could read a
		// row, this is the one that would.
		probes := []struct {
			label string
			key   string
		}{
			{"a REAL settled row on this bus, owned by another sender", localDeliveredID},
			{"an id this bus minted for a relayed message, which has no row at all", relayedRecipientID},
			{"a well-formed id minted by a DIFFERENT bus", relayedOriginID},
			{"a fabricated id naming a bus that does not exist", "bus-doesnotexist0000-99"},
		}

		var first, firstLabel string
		for i, probe := range probes {
			res, doc := f.ackStatus(t, f.recipient, probe.key)
			t.Logf("oracle probe %q (%s): exit=%d stdout=%s", probe.key, probe.label, res.code, strings.TrimSpace(res.stdout))

			// WITHOUT --wait every outcome is exit 0, INCLUDING unknown. A bare
			// ack-status can never report failure through its exit code, so an
			// assertion that read only the status would be blind here; the state
			// field is the answer.
			if res.code != 0 {
				t.Fatalf("ack-status for %s exited %d, want 0 — without --wait every answer, unknown included, is exit 0\nstderr:\n%s",
					probe.label, res.code, res.stderr)
			}
			row := soleRow(t, "oracle probe: "+probe.label, doc)
			if row.State != "unknown" {
				t.Fatalf("ack-status for %s (%s) reported state %q, want %q. Only the ORIGINAL SENDER may read a "+
					"row (ACK-CONTRACT.md §13.3); any other answer is an id-guessing oracle.",
					probe.label, probe.key, row.State, "unknown")
			}
			// Non-disclosure: the unknown answer names nothing. Not the
			// recipient, not the reason class, not the attestation, and not even
			// the key back — §13.3 forbids disclosing the traversed bus_path,
			// the peer that refused, and the recipient's roster membership.
			if row.Recipient != "" || row.Class != "" || row.AttestedBy != "" || row.CorrelationKey != "" {
				t.Fatalf("the unknown answer for %s disclosed a field: %+v — it must carry state and nothing else",
					probe.label, row)
			}

			got := normalisedJSON(t, "ack-status for "+probe.label, res.stdout)
			if i == 0 {
				first, firstLabel = got, probe.label
				continue
			}
			if got != first {
				t.Fatalf("THE STATUS ANSWER IS AN ORACLE. %q (%s) answered\n  %s\nbut %q (%s) answered\n  %s\n"+
					"§13.3 requires these to be indistinguishable: a caller must not be able to tell a real "+
					"message it does not own from one that never existed.",
					probes[0].key, firstLabel, first, probe.key, probe.label, got)
			}
		}

		// And the row IS readable — by its sender, on the same bus, in the same
		// state of the world. Without this the subtest above would also pass on
		// a bus that had simply lost the row, or on an ack-status that answered
		// `unknown` to everybody.
		ownerRes, ownerDoc := f.ackStatus(t, f.local, localDeliveredID)
		if ownerRes.code != 0 {
			t.Fatalf("the original sender could not read its own row (exit %d)\nstderr:\n%s",
				ownerRes.code, ownerRes.stderr)
		}
		ownerRow := soleRow(t, "the original sender's own row", ownerDoc)
		if ownerRow.State != "delivered" {
			t.Fatalf("the original sender sees state %q for its own settled message, want %q — "+
				"the uniform `unknown` above must be an AUTHORIZATION answer, not an empty bus",
				ownerRow.State, "delivered")
		}
	})
}
