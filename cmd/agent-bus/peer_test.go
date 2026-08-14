package main

// Tests for `agent-bus peer add|list|remove` (RELAY-12).
//
// The subcommand is exercised END TO END against a real data directory, a real
// dirlock and a real write-ahead log — no stubs. Two properties cannot be proved
// against a fake and are the reason for that:
//
//   - a route and a trust pin are INDEPENDENT records. The A <-> B <-> C
//     topology the FEDERATION epic exists for needs a trust entry with NO route
//     (C pins A while never peering with A) and a route with NO trust (a bus we
//     relay through). Both are asserted on disk, not in memory.
//   - the store's log MUST be replayed before its first write. An un-replayed
//     store mints config_seq 1 over a log that already holds 1..N and the
//     superseded generation wins on the next replay; the only way to see that is
//     to run the command twice against one directory and read the records back.

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/buscert"
	"github.com/dodgymike/agent-bus/internal/dirlock"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/relay"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// initPeerDataDir builds a data directory holding a bus id and NOTHING ELSE.
//
// It deliberately does not create a certificate: unlike `invite mint`, peer
// configuration pins no certificate of ours, so a directory without one must be
// accepted. If that ever changes this helper fails every test in the file, which
// is the point.
func initPeerDataDir(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	busID, err := ids.LoadOrCreateBusID(dir, "bus-peertest")
	if err != nil {
		t.Fatalf("LoadOrCreateBusID: %v", err)
	}
	return dir, busID
}

// runPeer invokes the subcommand and returns its exit code plus both streams.
func runPeer(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := runPeerCommand(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// newSigningKey returns a fresh Ed25519 public key and its base64 spelling, the
// way an operator would copy it off the peer bus.
func newSigningKey(t *testing.T) (ed25519.PublicKey, string) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	return pub, base64.StdEncoding.EncodeToString(pub)
}

// walPeerConfig replays the durable log and returns every peer-configuration
// record in it, IN LOG ORDER.
//
// This is the durability assertion the whole task rests on: it reads what is on
// disk rather than what the command printed, and it sees every generation,
// including superseded ones, which is what makes the config_seq test meaningful.
func walPeerConfig(t *testing.T, dir string) ([]relay.PeerRecord, []relay.BusTrustRecord) {
	t.Helper()
	var (
		routes []relay.PeerRecord
		trust  []relay.BusTrustRecord
	)
	_, err := wal.Replay(filepath.Join(dir, wal.WALFileName), func(c wal.Committed) error {
		switch c.Entry.Kind {
		case relay.PeerRecordKind:
			rec, err := relay.DecodePeerRecord(c.Entry.Body)
			if err != nil {
				t.Errorf("decoding a %q record off disk: %v", c.Entry.Kind, err)
				return nil
			}
			routes = append(routes, rec)
		case relay.BusTrustRecordKind:
			rec, err := relay.DecodeBusTrustRecord(c.Entry.Body)
			if err != nil {
				t.Errorf("decoding a %q record off disk: %v", c.Entry.Kind, err)
				return nil
			}
			trust = append(trust, rec)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("wal.Replay: %v", err)
	}
	return routes, trust
}

// listPeers runs `peer list --json` and returns the parsed result.
func listPeers(t *testing.T, dir string) peerListResult {
	t.Helper()
	code, stdout, stderr := runPeer(t, "list", "-data-dir", dir, "-json")
	if code != exitPeerOK {
		t.Fatalf("peer list exit = %d, want %d\nstdout: %s\nstderr: %s", code, exitPeerOK, stdout, stderr)
	}
	var out peerListResult
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("peer list --json is not one JSON object: %v\ngot: %s", err, stdout)
	}
	return out
}

// TestPeerAddPersistsRouteAndRejectsSelf is the task's proof: a route reaches
// disk, and a bus may not peer with itself (invariant 2) — with nothing written
// by the refusal.
func TestPeerAddPersistsRouteAndRejectsSelf(t *testing.T) {
	t.Parallel()
	dir, localBusID := initPeerDataDir(t)

	code, stdout, stderr := runPeer(t, "add",
		"-data-dir", dir,
		"-bus-id", "bus-b",
		"-url", "https://b.example:8443",
		"-json")
	if code != exitPeerOK {
		t.Fatalf("peer add exit = %d, want %d\nstdout: %s\nstderr: %s", code, exitPeerOK, stdout, stderr)
	}
	var res peerResult
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("--json output is not one JSON object: %v\ngot: %s", err, stdout)
	}
	if !res.OK {
		t.Error(`--json success output has "ok": false`)
	}
	if res.BusID != localBusID {
		t.Errorf("result bus_id = %q, want this bus's own id %q", res.BusID, localBusID)
	}
	if len(res.Changes) != 1 || res.Changes[0].Kind != "route" || res.Changes[0].BusID != "bus-b" {
		t.Fatalf("changes = %+v, want exactly one route for bus-b", res.Changes)
	}
	if got := res.Changes[0].BaseURL; got != "https://b.example:8443" {
		t.Errorf("route base_url = %q, want the address that was passed", got)
	}
	if res.Changes[0].ConfigSeq == 0 {
		t.Error("config_seq is 0, which is the unset value; the first configuration this bus writes is 1")
	}

	// DURABLE, not merely reported: the record is read back off the log.
	routes, trust := walPeerConfig(t, dir)
	if len(routes) != 1 {
		t.Fatalf("the log holds %d route records, want 1", len(routes))
	}
	if routes[0].BusID != "bus-b" || routes[0].BaseURL != "https://b.example:8443" || routes[0].State != relay.PeerRecordActive {
		t.Errorf("durable route = %+v, want an ACTIVE route for bus-b at https://b.example:8443", routes[0])
	}
	if len(trust) != 0 {
		t.Errorf("the log holds %d trust records, want 0: -url must never imply a pin", len(trust))
	}

	t.Run("a bus may not peer with ITSELF, and the refusal writes nothing", func(t *testing.T) {
		code, stdout, stderr := runPeer(t, "add",
			"-data-dir", dir,
			"-bus-id", localBusID,
			"-url", "https://self.example:8443",
			"-json")
		if code != exitPeerUsage {
			t.Fatalf("self-peer exit = %d, want %d\nstdout: %s\nstderr: %s", code, exitPeerUsage, stdout, stderr)
		}
		var e peerError
		if err := json.Unmarshal([]byte(stdout), &e); err != nil {
			t.Fatalf("--json failure output is not one JSON object: %v\ngot: %s", err, stdout)
		}
		if e.OK {
			t.Error(`--json failure output has "ok": true`)
		}
		if len(e.Applied) != 0 {
			t.Errorf("a refused self-peer reports %d applied changes, want none", len(e.Applied))
		}
		routes, _ := walPeerConfig(t, dir)
		if len(routes) != 1 {
			t.Errorf("the log holds %d route records after a refused self-peer, want the 1 from before", len(routes))
		}
	})

	t.Run("-route-for may not name THIS bus either", func(t *testing.T) {
		code, _, stderr := runPeer(t, "add",
			"-data-dir", dir,
			"-bus-id", "bus-b",
			"-url", "https://b.example:8443",
			"-route-for", localBusID)
		if code != exitPeerUsage {
			t.Fatalf("-route-for self exit = %d, want %d\nstderr: %s", code, exitPeerUsage, stderr)
		}
		routes, _ := walPeerConfig(t, dir)
		for _, r := range routes {
			if r.BusID == localBusID {
				t.Fatalf("a route record was written for THIS bus (%s); a self-route is a loop and a namespace collision at once", localBusID)
			}
		}
	})

	t.Run("-route-for may not name the peer it routes through", func(t *testing.T) {
		code, _, stderr := runPeer(t, "add",
			"-data-dir", dir,
			"-bus-id", "bus-b",
			"-url", "https://b.example:8443",
			"-route-for", "bus-b")
		if code != exitPeerUsage {
			t.Fatalf("exit = %d, want %d\nstderr: %s", code, exitPeerUsage, stderr)
		}
	})
}

// TestPeerAddListRemove covers the three subcommands together, and its first two
// subtests are the ones that matter most: TRUST WITHOUT A ROUTE and A ROUTE
// WITHOUT TRUST. Those are the non-adjacent case the split-record design exists
// for, and they are the pair most likely to be silently broken by a surface that
// couples the two flags.
func TestPeerAddListRemove(t *testing.T) {
	t.Parallel()

	t.Run("a TRUST entry survives with NO route", func(t *testing.T) {
		t.Parallel()
		dir, _ := initPeerDataDir(t)
		key, keyB64 := newSigningKey(t)

		code, stdout, stderr := runPeer(t, "add", "-data-dir", dir, "-bus-id", "bus-a", "-signing-key", keyB64, "-json")
		if code != exitPeerOK {
			t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, exitPeerOK, stdout, stderr)
		}

		routes, trust := walPeerConfig(t, dir)
		if len(routes) != 0 {
			t.Errorf("the log holds %d route records, want 0: -signing-key must never imply a route", len(routes))
		}
		if len(trust) != 1 {
			t.Fatalf("the log holds %d trust records, want 1", len(trust))
		}
		if trust[0].BusID != "bus-a" || trust[0].State != relay.PeerRecordActive {
			t.Fatalf("durable trust = %+v, want an ACTIVE pin for bus-a", trust[0])
		}
		if len(trust[0].SigningKeys) != 1 || !bytes.Equal(trust[0].SigningKeys[0], key) {
			t.Errorf("durable pin set = %v, want exactly the key that was passed", trust[0].SigningKeys)
		}

		// And it is visible as trust with no route, which is what an operator
		// checking the A <-> B <-> C bootstrap will actually look at.
		out := listPeers(t, dir)
		if len(out.Routes) != 0 {
			t.Errorf("peer list reports %d routes, want 0", len(out.Routes))
		}
		if len(out.Trust) != 1 || out.Trust[0].BusID != "bus-a" {
			t.Fatalf("peer list trust = %+v, want exactly bus-a", out.Trust)
		}
		if len(out.Trust[0].SigningKeys) != 1 || out.Trust[0].SigningKeys[0] != keyB64 {
			t.Errorf("peer list pin set = %v, want [%s]", out.Trust[0].SigningKeys, keyB64)
		}
	})

	t.Run("a ROUTE survives with NO trust", func(t *testing.T) {
		t.Parallel()
		dir, _ := initPeerDataDir(t)

		code, stdout, stderr := runPeer(t, "add", "-data-dir", dir, "-bus-id", "bus-b", "-url", "https://b.example:8443", "-json")
		if code != exitPeerOK {
			t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, exitPeerOK, stdout, stderr)
		}
		out := listPeers(t, dir)
		if len(out.Trust) != 0 {
			t.Errorf("peer list reports %d trust entries, want 0", len(out.Trust))
		}
		if len(out.Routes) != 1 || out.Routes[0].BusID != "bus-b" || out.Routes[0].BaseURL != "https://b.example:8443" {
			t.Fatalf("peer list routes = %+v, want exactly bus-b at https://b.example:8443", out.Routes)
		}
	})

	t.Run("both together write TWO independent records", func(t *testing.T) {
		t.Parallel()
		dir, _ := initPeerDataDir(t)
		_, keyB64 := newSigningKey(t)

		code, stdout, stderr := runPeer(t, "add", "-data-dir", dir,
			"-bus-id", "bus-b", "-url", "https://b.example:8443", "-signing-key", keyB64, "-json")
		if code != exitPeerOK {
			t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, exitPeerOK, stdout, stderr)
		}
		routes, trust := walPeerConfig(t, dir)
		if len(routes) != 1 || len(trust) != 1 {
			t.Fatalf("the log holds %d route and %d trust records, want 1 and 1", len(routes), len(trust))
		}
		// TRUST IS WRITTEN FIRST. Of the two half-states a crash between the
		// writes can leave, "pinned but not routed" is inert and "routed but
		// unverifiable" is not.
		if trust[0].ConfigSeq >= routes[0].ConfigSeq {
			t.Errorf("trust config_seq %d is not below the route's %d; the trust record must be written first", trust[0].ConfigSeq, routes[0].ConfigSeq)
		}
	})

	t.Run("-route-for installs a static next-hop route", func(t *testing.T) {
		t.Parallel()
		dir, _ := initPeerDataDir(t)

		code, stdout, stderr := runPeer(t, "add", "-data-dir", dir,
			"-bus-id", "bus-b", "-url", "https://b.example:8443", "-route-for", "bus-c", "-json")
		if code != exitPeerOK {
			t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, exitPeerOK, stdout, stderr)
		}
		var res peerResult
		if err := json.Unmarshal([]byte(stdout), &res); err != nil {
			t.Fatalf("--json output is not one JSON object: %v\ngot: %s", err, stdout)
		}
		if len(res.Changes) != 2 {
			t.Fatalf("changes = %+v, want two routes (the peer, then the destination through it)", res.Changes)
		}
		if res.Changes[1].BusID != "bus-c" || res.Changes[1].NextHopBusID != "bus-b" {
			t.Errorf("the -route-for change = %+v, want bus-c with next_hop_bus_id bus-b", res.Changes[1])
		}

		// The DURABLE record carries the next hop's ADDRESS and does not record
		// the via-bus — see peer.go's file comment. Asserted so nobody later
		// believes the store remembers it.
		routes, _ := walPeerConfig(t, dir)
		if len(routes) != 2 {
			t.Fatalf("the log holds %d route records, want 2", len(routes))
		}
		var dest relay.PeerRecord
		for _, r := range routes {
			if r.BusID == "bus-c" {
				dest = r
			}
		}
		if dest.BusID != "bus-c" {
			t.Fatalf("no durable route for bus-c; the log holds %+v", routes)
		}
		if dest.BaseURL != "https://b.example:8443" {
			t.Errorf("the static route for bus-c dials %q, want the next hop's address https://b.example:8443", dest.BaseURL)
		}
	})

	t.Run("remove withdraws ONE record kind and leaves the other", func(t *testing.T) {
		t.Parallel()
		dir, _ := initPeerDataDir(t)
		_, keyB64 := newSigningKey(t)
		if code, _, stderr := runPeer(t, "add", "-data-dir", dir,
			"-bus-id", "bus-b", "-url", "https://b.example:8443", "-signing-key", keyB64); code != exitPeerOK {
			t.Fatalf("setup add exit = %d: %s", code, stderr)
		}

		code, stdout, stderr := runPeer(t, "remove", "-data-dir", dir, "-bus-id", "bus-b", "-route", "-json")
		if code != exitPeerOK {
			t.Fatalf("remove -route exit = %d, want %d\nstdout: %s\nstderr: %s", code, exitPeerOK, stdout, stderr)
		}
		out := listPeers(t, dir)
		if len(out.Routes) != 0 {
			t.Errorf("peer list still reports %d routes after remove -route", len(out.Routes))
		}
		if len(out.Trust) != 1 {
			t.Fatalf("remove -route withdrew the TRUST pin as well; the two records are independent and neither implies the other")
		}

		// And the mirror: withdrawing trust does not resurrect or touch a route.
		if code, _, stderr := runPeer(t, "remove", "-data-dir", dir, "-bus-id", "bus-b", "-trust"); code != exitPeerOK {
			t.Fatalf("remove -trust exit = %d: %s", code, stderr)
		}
		out = listPeers(t, dir)
		if len(out.Trust) != 0 || len(out.Routes) != 0 {
			t.Errorf("after withdrawing both, peer list reports %d routes and %d trust entries, want 0 and 0", len(out.Routes), len(out.Trust))
		}
	})

	t.Run("remove of an unknown bus exits 5 and writes nothing", func(t *testing.T) {
		t.Parallel()
		dir, _ := initPeerDataDir(t)
		code, stdout, stderr := runPeer(t, "remove", "-data-dir", dir, "-bus-id", "bus-nope", "-route", "-json")
		if code != exitPeerUnknown {
			t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, exitPeerUnknown, stdout, stderr)
		}
		routes, trust := walPeerConfig(t, dir)
		if len(routes)+len(trust) != 0 {
			t.Errorf("a refused removal wrote %d route and %d trust records", len(routes), len(trust))
		}
	})

	t.Run("remove REQUIRES -route or -trust", func(t *testing.T) {
		t.Parallel()
		dir, _ := initPeerDataDir(t)
		code, _, stderr := runPeer(t, "remove", "-data-dir", dir, "-bus-id", "bus-b")
		if code != exitPeerUsage {
			t.Fatalf("exit = %d, want %d\nstderr: %s", code, exitPeerUsage, stderr)
		}
		if !strings.Contains(stderr, "-route") || !strings.Contains(stderr, "-trust") {
			t.Errorf("the refusal does not name both flags; it said: %s", stderr)
		}
	})

	t.Run("an add that installs neither a route nor a pin is refused", func(t *testing.T) {
		t.Parallel()
		dir, _ := initPeerDataDir(t)
		code, _, stderr := runPeer(t, "add", "-data-dir", dir, "-bus-id", "bus-b")
		if code != exitPeerUsage {
			t.Fatalf("exit = %d, want %d\nstderr: %s", code, exitPeerUsage, stderr)
		}
	})

	t.Run("-route-for without -url is refused", func(t *testing.T) {
		t.Parallel()
		dir, _ := initPeerDataDir(t)
		_, keyB64 := newSigningKey(t)
		code, _, stderr := runPeer(t, "add", "-data-dir", dir, "-bus-id", "bus-b", "-signing-key", keyB64, "-route-for", "bus-c")
		if code != exitPeerUsage {
			t.Fatalf("exit = %d, want %d\nstderr: %s", code, exitPeerUsage, stderr)
		}
	})

	t.Run("re-adding the same configuration writes nothing and says so", func(t *testing.T) {
		t.Parallel()
		dir, _ := initPeerDataDir(t)
		args := []string{"add", "-data-dir", dir, "-bus-id", "bus-b", "-url", "https://b.example:8443", "-json"}
		if code, _, stderr := runPeer(t, args...); code != exitPeerOK {
			t.Fatalf("first add exit = %d: %s", code, stderr)
		}
		code, stdout, stderr := runPeer(t, args...)
		if code != exitPeerOK {
			t.Fatalf("second add exit = %d, want %d\nstderr: %s", code, exitPeerOK, stderr)
		}
		var res peerResult
		if err := json.Unmarshal([]byte(stdout), &res); err != nil {
			t.Fatalf("--json output is not one JSON object: %v\ngot: %s", err, stdout)
		}
		if len(res.Changes) != 1 || !res.Changes[0].Unchanged {
			t.Errorf("changes = %+v, want one change flagged unchanged", res.Changes)
		}
		routes, _ := walPeerConfig(t, dir)
		if len(routes) != 1 {
			t.Errorf("the log holds %d route records after re-applying one peering, want 1: an unchanged add must append nothing", len(routes))
		}
	})
}

// TestPeerAddReplaysTheLogBeforeTheFirstWrite is the precondition
// relay.PeerStoreOptions.Durable states and the package cannot enforce: THE LOG
// MUST BE REPLAYED INTO THE STORE BEFORE THE FIRST WRITE.
//
// Each invocation of the command is a fresh process's worth of state: a new
// store, a new log handle, a config_seq high-water mark starting at zero. If
// replay did not happen first, the SECOND invocation would mint config_seq 1
// again over a log that already holds 1 — and on the next replay the superseded
// generation, arriving first at an equal sequence, would WIN. A security gate
// reproduced exactly that during RELAY-10.
//
// So the assertion is on the numbers on disk, and then on what a replay of the
// whole log actually reconstructs.
func TestPeerAddReplaysTheLogBeforeTheFirstWrite(t *testing.T) {
	t.Parallel()
	dir, busID := initPeerDataDir(t)

	if code, _, stderr := runPeer(t, "add", "-data-dir", dir, "-bus-id", "bus-b", "-url", "https://b1.example:8443"); code != exitPeerOK {
		t.Fatalf("first add exit = %d: %s", code, stderr)
	}
	if code, _, stderr := runPeer(t, "add", "-data-dir", dir, "-bus-id", "bus-c", "-url", "https://c.example:8443"); code != exitPeerOK {
		t.Fatalf("second add exit = %d: %s", code, stderr)
	}
	// A NEW GENERATION for a bus already configured — the case where a rewound
	// sequence would actually lose the operator's current address.
	if code, _, stderr := runPeer(t, "add", "-data-dir", dir, "-bus-id", "bus-b", "-url", "https://b2.example:8443"); code != exitPeerOK {
		t.Fatalf("third add exit = %d: %s", code, stderr)
	}

	routes, _ := walPeerConfig(t, dir)
	if len(routes) != 3 {
		t.Fatalf("the log holds %d route records, want 3", len(routes))
	}
	for i, r := range routes {
		if r.ConfigSeq != uint64(i+1) {
			t.Fatalf("record %d (bus %s, %s) has config_seq %d, want %d.\n"+
				"A repeated or rewound sequence is the signature of a store that was NOT replayed before its first write.\n"+
				"all records: %+v", i, r.BusID, r.BaseURL, r.ConfigSeq, i+1, routes)
		}
	}

	// What the numbers are FOR: replaying the whole log must reconstruct the
	// operator's CURRENT address for bus-b, not the superseded one.
	store, err := relay.NewPeerStore(relay.PeerStoreOptions{BusID: busID})
	if err != nil {
		t.Fatalf("relay.NewPeerStore: %v", err)
	}
	if _, err := wal.Replay(filepath.Join(dir, wal.WALFileName), store.Apply); err != nil {
		t.Fatalf("wal.Replay: %v", err)
	}
	rec, ok := store.Lookup("bus-b")
	if !ok || rec.State != relay.PeerRecordActive {
		t.Fatalf("after replay, bus-b = %+v (found=%v), want an active route", rec, ok)
	}
	if rec.BaseURL != "https://b2.example:8443" {
		t.Errorf("after replay, bus-b dials %q, want the LATEST configured address https://b2.example:8443", rec.BaseURL)
	}
}

// TestPeerAddURLRulesMatchTheDurableRecord pins the CLI's -url check against the
// rule the durable record itself enforces.
//
// The check is duplicated in peer.go because relay's is unexported and belongs
// to the record. This is what stops the duplicate drifting: LOOSER would push a
// refusal past the lock to exit 1, and STRICTER would refuse an address the
// store would happily have stored.
func TestPeerAddURLRulesMatchTheDurableRecord(t *testing.T) {
	t.Parallel()
	cases := []string{
		"https://b.example:8443",
		"https://b.example",
		"https://b.example/",
		"https://[::1]:8443",
		"HTTPS://B.example:8443",
		"http://b.example:8443",
		"http://127.0.0.1:8443",
		"https://b.example/relay",
		"https://b.example:8443/",
		"https://b.example?",
		"https://b.example?x=1",
		"https://b.example#frag",
		"https://user:pw@b.example",
		"https://",
		"",
		"://b.example",
		"b.example:8443",
		"https://b.example/../../x",
	}
	for _, raw := range cases {
		raw := raw
		t.Run(raw, func(t *testing.T) {
			canonical, cliErr := validatePeerBareOrigin(raw)

			// The record's own verdict on the value the CLI would have stored;
			// on a CLI refusal, on the raw value, since that is what would have
			// reached the store had the CLI not refused.
			candidate := canonical
			if cliErr != nil {
				candidate = strings.TrimSpace(raw)
			}
			_, recErr := relay.PeerRecord{
				BusID:     "bus-b",
				ConfigSeq: 1,
				State:     relay.PeerRecordActive,
				BaseURL:   candidate,
				UpdatedAt: peerTestClock,
			}.Encode()

			switch {
			case cliErr == nil && recErr != nil:
				t.Errorf("the CLI accepted %q (as %q) but the durable record refuses it: %v", raw, canonical, recErr)
			case cliErr != nil && recErr == nil:
				t.Errorf("the CLI refused %q (%s) but the durable record would have stored it", raw, cliErr.Error())
			}
		})
	}
}

// TestPeerCommandRefusals covers the exit-code contract for the failures whose
// remedies differ, since a caller that cannot tell them apart has to parse
// English.
func TestPeerCommandRefusals(t *testing.T) {
	t.Parallel()

	t.Run("a locked data directory is exit 3, not a corrupted log", func(t *testing.T) {
		t.Parallel()
		dir, _ := initPeerDataDir(t)
		lock, err := dirlock.Acquire(dir)
		if err != nil {
			t.Fatalf("dirlock.Acquire: %v", err)
		}
		defer func() {
			if err := lock.Release(); err != nil {
				t.Errorf("lock.Release: %v", err)
			}
		}()

		for _, args := range [][]string{
			{"add", "-data-dir", dir, "-bus-id", "bus-b", "-url", "https://b.example:8443"},
			{"list", "-data-dir", dir},
			{"remove", "-data-dir", dir, "-bus-id", "bus-b", "-route"},
		} {
			code, _, stderr := runPeer(t, args...)
			if code != exitPeerBusRunning {
				t.Errorf("`peer %s` against a locked dir exited %d, want %d\nstderr: %s", args[0], code, exitPeerBusRunning, stderr)
			}
		}
	})

	t.Run("a directory with no bus id is exit 4 and is left untouched", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		code, _, stderr := runPeer(t, "add", "-data-dir", dir, "-bus-id", "bus-b", "-url", "https://b.example:8443")
		if code != exitPeerNoIdentity {
			t.Fatalf("exit = %d, want %d\nstderr: %s", code, exitPeerNoIdentity, stderr)
		}
		// NOTHING was written — not even bus.lock, which the server reads as
		// "this directory has history" and which would make the operator's very
		// first start refuse to boot.
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("os.ReadDir: %v", err)
		}
		if len(entries) != 0 {
			names := make([]string, 0, len(entries))
			for _, e := range entries {
				names = append(names, e.Name())
			}
			t.Errorf("the refusal left %v in the data directory; it must write nothing at all", names)
		}
	})

	t.Run("a missing data directory is exit 4 and is never created", func(t *testing.T) {
		t.Parallel()
		missing := filepath.Join(t.TempDir(), "typo")
		code, _, stderr := runPeer(t, "list", "-data-dir", missing)
		if code != exitPeerNoIdentity {
			t.Fatalf("exit = %d, want %d\nstderr: %s", code, exitPeerNoIdentity, stderr)
		}
		if _, err := os.Stat(missing); !os.IsNotExist(err) {
			t.Errorf("the data directory was created by a refusal (stat err = %v)", err)
		}
	})

	t.Run("a malformed bus id, a bad key and a bad URL are all usage errors", func(t *testing.T) {
		t.Parallel()
		dir, _ := initPeerDataDir(t)
		for name, args := range map[string][]string{
			"empty bus id":     {"add", "-data-dir", dir, "-url", "https://b.example:8443"},
			"oversized bus id": {"add", "-data-dir", dir, "-bus-id", strings.Repeat("b", relay.MaxPeerBusIDLen+1), "-url", "https://b.example:8443"},
			"not base64":       {"add", "-data-dir", dir, "-bus-id", "bus-b", "-signing-key", "not base64!!"},
			"wrong-size key":   {"add", "-data-dir", dir, "-bus-id", "bus-b", "-signing-key", base64.StdEncoding.EncodeToString([]byte("short"))},
			"three keys":       {"add", "-data-dir", dir, "-bus-id", "bus-b", "-signing-key", keyOf(t), "-signing-key", keyOf(t), "-signing-key", keyOf(t)},
			"plaintext url":    {"add", "-data-dir", dir, "-bus-id", "bus-b", "-url", "http://b.example:8443"},
			"url with a path":  {"add", "-data-dir", dir, "-bus-id", "bus-b", "-url", "https://b.example:8443/relay"},
			"duplicate route":  {"add", "-data-dir", dir, "-bus-id", "bus-b", "-url", "https://b.example:8443", "-route-for", "bus-c", "-route-for", "bus-c"},
		} {
			code, _, stderr := runPeer(t, args...)
			if code != exitPeerUsage {
				t.Errorf("%s: exit = %d, want %d\nstderr: %s", name, code, exitPeerUsage, stderr)
			}
		}
		routes, trust := walPeerConfig(t, dir)
		if len(routes)+len(trust) != 0 {
			t.Errorf("usage refusals wrote %d route and %d trust records", len(routes), len(trust))
		}
	})

	t.Run("help goes to stdout, an unknown subcommand to stderr", func(t *testing.T) {
		t.Parallel()
		code, stdout, _ := runPeer(t, "-h")
		if code != exitPeerOK || !strings.Contains(stdout, "agent-bus peer") {
			t.Errorf("`peer -h` exit = %d, stdout = %q", code, stdout)
		}
		code, stdout, stderr := runPeer(t, "wibble")
		if code != exitPeerUsage {
			t.Errorf("unknown subcommand exit = %d, want %d", code, exitPeerUsage)
		}
		if stdout != "" {
			t.Errorf("an unknown subcommand wrote to stdout: %q", stdout)
		}
		if strings.Contains(stderr, "wibble") {
			t.Error("the unknown subcommand was echoed back; unvalidated argv must not reach a terminal")
		}
	})
}

// keyOf returns one fresh base64 signing key, for tables that only need a
// well-formed value.
func keyOf(t *testing.T) string {
	t.Helper()
	_, b64 := newSigningKey(t)
	return b64
}

// peerTestClock is a fixed, non-zero timestamp for record-level assertions. A
// zero time is refused by the record itself (tombstone retention is computed
// from it), so the URL table above would otherwise fail for the wrong reason.
var peerTestClock = time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)

// TestPeerRemoveDoesNotAbandonTheSecondWithdrawal is the regression test for the
// P1 both gates reproduced.
//
// In the A <-> B <-> C line, C pins A's bus signing key and has NO ROUTE to A.
// The natural "revoke everything about A" is `remove -bus-id busA -route -trust`
// — and the first version returned on the route's ErrUnknownPeer, LEAVING THE
// TRUST ANCHOR PINNED while exiting with the code a provisioning script is told
// it may treat as benign. That is the fail-silent direction the mandatory
// -route/-trust design exists to prevent.
func TestPeerRemoveDoesNotAbandonTheSecondWithdrawal(t *testing.T) {
	t.Parallel()

	t.Run("a pinned bus with NO route is fully revoked", func(t *testing.T) {
		t.Parallel()
		dir, _ := initPeerDataDir(t)
		_, keyB64 := newSigningKey(t)
		if code, _, stderr := runPeer(t, "add", "-data-dir", dir, "-bus-id", "bus-a", "-signing-key", keyB64); code != exitPeerOK {
			t.Fatalf("setup add exit = %d: %s", code, stderr)
		}

		code, stdout, stderr := runPeer(t, "remove", "-data-dir", dir, "-bus-id", "bus-a", "-route", "-trust", "-json")
		if code != exitPeerOK {
			t.Fatalf("exit = %d, want %d — the absent ROUTE must not abandon the TRUST withdrawal\nstdout: %s\nstderr: %s", code, exitPeerOK, stdout, stderr)
		}
		var res peerResult
		if err := json.Unmarshal([]byte(stdout), &res); err != nil {
			t.Fatalf("--json output is not one JSON object: %v\ngot: %s", err, stdout)
		}
		if len(res.NotFound) != 1 || res.NotFound[0] != "route" {
			t.Errorf("not_found = %v, want [route]: the absent kind must be named, not dropped", res.NotFound)
		}
		if out := listPeers(t, dir); len(out.Trust) != 0 {
			t.Fatalf("THE TRUST ANCHOR IS STILL PINNED after a -route -trust removal: %+v", out.Trust)
		}
	})

	t.Run("the mirror: a routed bus with NO pin", func(t *testing.T) {
		t.Parallel()
		dir, _ := initPeerDataDir(t)
		if code, _, stderr := runPeer(t, "add", "-data-dir", dir, "-bus-id", "bus-b", "-url", "https://b.example:8443"); code != exitPeerOK {
			t.Fatalf("setup add exit = %d: %s", code, stderr)
		}
		code, stdout, _ := runPeer(t, "remove", "-data-dir", dir, "-bus-id", "bus-b", "-route", "-trust", "-json")
		if code != exitPeerOK {
			t.Fatalf("exit = %d, want %d\nstdout: %s", code, exitPeerOK, stdout)
		}
		if out := listPeers(t, dir); len(out.Routes) != 0 {
			t.Errorf("the route survived: %+v", out.Routes)
		}
	})

	t.Run("exit 5 still fires when NEITHER kind exists, and writes nothing", func(t *testing.T) {
		t.Parallel()
		dir, _ := initPeerDataDir(t)
		code, _, _ := runPeer(t, "remove", "-data-dir", dir, "-bus-id", "bus-typo", "-route", "-trust")
		if code != exitPeerUnknown {
			t.Fatalf("exit = %d, want %d", code, exitPeerUnknown)
		}
		routes, trust := walPeerConfig(t, dir)
		if len(routes)+len(trust) != 0 {
			t.Errorf("a wholly-unknown removal wrote %d route and %d trust records", len(routes), len(trust))
		}
	})

	t.Run("removing THIS bus is a usage error, matching add", func(t *testing.T) {
		t.Parallel()
		dir, localBusID := initPeerDataDir(t)
		code, _, _ := runPeer(t, "remove", "-data-dir", dir, "-bus-id", localBusID, "-route")
		if code != exitPeerUsage {
			t.Errorf("exit = %d, want %d", code, exitPeerUsage)
		}
	})
}

// TestPeerAddSigningKeySetRules pins the pin-set rules that are refused BEFORE
// the lock, so an operator typo is exit 2 and writes nothing rather than
// arriving as a generic failure after the record refuses it.
func TestPeerAddSigningKeySetRules(t *testing.T) {
	t.Parallel()
	dir, _ := initPeerDataDir(t)
	_, k1 := newSigningKey(t)
	_, k2 := newSigningKey(t)

	t.Run("two DISTINCT keys are a rollover and are accepted", func(t *testing.T) {
		code, _, stderr := runPeer(t, "add", "-data-dir", dir, "-bus-id", "bus-roll", "-signing-key", k1, "-signing-key", k2)
		if code != exitPeerOK {
			t.Fatalf("exit = %d, want %d\nstderr: %s", code, exitPeerOK, stderr)
		}
		out := listPeers(t, dir)
		if len(out.Trust) != 1 || len(out.Trust[0].SigningKeys) != 2 {
			t.Fatalf("trust = %+v, want one bus with two pinned keys", out.Trust)
		}
		// ORDER IS THE OPERATOR'S and is part of the record.
		if out.Trust[0].SigningKeys[0] != k1 || out.Trust[0].SigningKeys[1] != k2 {
			t.Errorf("pin order = %v, want the order they were passed", out.Trust[0].SigningKeys)
		}
	})

	t.Run("-signing-key REPLACES the pin set, it does not append", func(t *testing.T) {
		if code, _, stderr := runPeer(t, "add", "-data-dir", dir, "-bus-id", "bus-roll", "-signing-key", k2); code != exitPeerOK {
			t.Fatalf("exit = %d: %s", code, stderr)
		}
		out := listPeers(t, dir)
		if len(out.Trust[0].SigningKeys) != 1 || out.Trust[0].SigningKeys[0] != k2 {
			t.Errorf("pin set = %v, want exactly the one key just passed — narrowing a pin set fails CLOSED and must not silently append", out.Trust[0].SigningKeys)
		}
	})

	t.Run("a repeated key is exit 2, not a collapsed set", func(t *testing.T) {
		code, _, _ := runPeer(t, "add", "-data-dir", dir, "-bus-id", "bus-dup", "-signing-key", k1, "-signing-key", k1)
		if code != exitPeerUsage {
			t.Errorf("exit = %d, want %d", code, exitPeerUsage)
		}
	})

	t.Run("an all-zero key is exit 2", func(t *testing.T) {
		zero := base64.StdEncoding.EncodeToString(make([]byte, ed25519.PublicKeySize))
		code, _, _ := runPeer(t, "add", "-data-dir", dir, "-bus-id", "bus-zero", "-signing-key", zero)
		if code != exitPeerUsage {
			t.Errorf("exit = %d, want %d", code, exitPeerUsage)
		}
	})
}

// ---------------------------------------------------------------------------
// RELAY-41: -tls-fingerprint, the per-NEXT-HOP certificate pin
// ---------------------------------------------------------------------------

// peerTestCert mints a REAL self-signed bus certificate and returns its
// fingerprint both as the typed value and in the spelling an operator copies
// onto a command line.
//
// It computes the fingerprint with buscert.FingerprintOf and with nothing else.
// That is the load-bearing detail of this whole file: the value `peer add`
// stores will be compared, on a live connection, against
// buscert.FingerprintOf of the certificate the hop at -url SERVES WHEN THIS BUS
// DIALS IT — outbound and server-side, never an inbound client certificate (see
// relay.PeerRecord.NextHopTLSCertFingerprint). If this test invented its own
// digest — over the PEM, over the SPKI, in a different case — it would pass
// while the product refused every peer connection as unknown, because a
// mismatched pin does not report a mismatch: it reports an unknown peer.
func peerTestCert(t *testing.T, busID string) (buscert.Fingerprint, string) {
	t.Helper()
	m, err := buscert.LoadOrCreate(t.TempDir(), buscert.Options{BusID: busID})
	if err != nil {
		t.Fatalf("buscert.LoadOrCreate(%s): %v", busID, err)
	}
	fp := buscert.FingerprintOf(m.Certificate())
	return fp, fp.String()
}

// TestPeerAddTLSFingerprintRoundTripsOnDisk is RELAY-41's proof, and its first
// subtest is the anti-regression test the task exists for.
//
// THE CONSTRAINT: a route record's bus id is the DESTINATION and its address is
// the NEXT HOP, and for a bus reached through another those are DIFFERENT
// BUSES. The certificate presented on a connection to that address is the NEXT
// HOP's, so the pin must be keyed to -url and never to -bus-id. A
// destination-keyed pin would compare the intermediate bus's certificate
// against the destination's fingerprint and would refuse every non-adjacent hop
// — the entire A -> B -> C topology this epic exists to build.
//
// Two REAL, DIFFERENT certificates are used so that "it stored the next hop's"
// is distinguishable from "it stored the destination's". With one certificate
// every assertion here would pass under either keying, which is precisely how
// this class of bug survives a test suite.
func TestPeerAddTLSFingerprintRoundTripsOnDisk(t *testing.T) {
	t.Parallel()

	const (
		hopBus  = "bus-hop"  // busB — the bus we dial
		destBus = "bus-dest" // busC — reached THROUGH busB
		hopURL  = "https://b.example:8443"
	)

	t.Run("the pin is keyed to the NEXT HOP, not to the destination", func(t *testing.T) {
		t.Parallel()
		dir, _ := initPeerDataDir(t)
		hopFP, hopFPHex := peerTestCert(t, hopBus)
		destFP, _ := peerTestCert(t, destBus)
		if hopFP == destFP {
			t.Fatal("fixture error: the two certificates share a fingerprint, so this test could not tell next-hop keying from destination keying")
		}

		code, stdout, stderr := runPeer(t, "add",
			"-data-dir", dir,
			"-bus-id", hopBus,
			"-url", hopURL,
			"-tls-fingerprint", hopFPHex,
			"-route-for", destBus,
			"-json")
		if code != exitPeerOK {
			t.Fatalf("peer add exit = %d, want %d\nstdout: %s\nstderr: %s", code, exitPeerOK, stdout, stderr)
		}

		// DURABLE, not merely reported: read back off the write-ahead log.
		routes, _ := walPeerConfig(t, dir)
		if len(routes) != 2 {
			t.Fatalf("the log holds %d route records, want 2 (the hop's own route and the destination's route through it)", len(routes))
		}
		byBus := map[string]relay.PeerRecord{}
		for _, r := range routes {
			byBus[r.BusID] = r
		}

		// THE ASSERTION THE TASK IS ABOUT.
		dest, ok := byBus[destBus]
		if !ok {
			t.Fatalf("no route record for the destination %s; got %v", destBus, byBus)
		}
		if dest.BaseURL != hopURL {
			t.Fatalf("the %s route dials %q, want the NEXT HOP's address %q", destBus, dest.BaseURL, hopURL)
		}
		if dest.NextHopTLSCertFingerprint != hopFP {
			t.Fatalf("the %s ROUTE record pins %s, but the connection it describes terminates at %s and presents %s's certificate (%s).\n"+
				"A pin keyed to the record's BUS ID would refuse every non-adjacent hop.",
				destBus, dest.NextHopTLSCertFingerprint, hopBus, hopBus, hopFP)
		}
		if dest.NextHopTLSCertFingerprint == destFP {
			t.Fatalf("the %s route pins the DESTINATION's own certificate; nothing ever presents it on that connection", destBus)
		}

		// And the hop's own route carries the same pin — one address, one
		// certificate, whatever the record's bus id says.
		hop, ok := byBus[hopBus]
		if !ok {
			t.Fatalf("no route record for the hop %s; got %v", hopBus, byBus)
		}
		if hop.NextHopTLSCertFingerprint != hopFP {
			t.Fatalf("the %s route pins %s, want %s", hopBus, hop.NextHopTLSCertFingerprint, hopFP)
		}

		// The --json report says the same thing, under a key that names the next
		// hop so a consumer cannot read it as the destination's certificate.
		var res peerResult
		if err := json.Unmarshal([]byte(stdout), &res); err != nil {
			t.Fatalf("--json output is not one JSON object: %v\ngot: %s", err, stdout)
		}
		for _, c := range res.Changes {
			if c.Kind != "route" {
				continue
			}
			if c.NextHopTLSCertFingerprint != hopFPHex {
				t.Errorf("reported pin for %s = %q, want the next hop's %q", c.BusID, c.NextHopTLSCertFingerprint, hopFPHex)
			}
		}
		if !strings.Contains(stdout, `"next_hop_tls_cert_sha256"`) {
			t.Errorf("--json output does not carry next_hop_tls_cert_sha256: %s", stdout)
		}
	})

	t.Run("peer list surfaces it in --json and in human output", func(t *testing.T) {
		t.Parallel()
		dir, _ := initPeerDataDir(t)
		_, hopFPHex := peerTestCert(t, hopBus)

		code, stdout, stderr := runPeer(t, "add", "-data-dir", dir, "-bus-id", hopBus, "-url", hopURL, "-tls-fingerprint", hopFPHex, "-route-for", destBus)
		if code != exitPeerOK {
			t.Fatalf("peer add exit = %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
		}

		out := listPeers(t, dir)
		if len(out.Routes) != 2 {
			t.Fatalf("peer list reports %d routes, want 2", len(out.Routes))
		}
		for _, r := range out.Routes {
			if r.NextHopTLSCertFingerprint != hopFPHex {
				t.Errorf("peer list --json route %s reports pin %q, want %q", r.BusID, r.NextHopTLSCertFingerprint, hopFPHex)
			}
		}

		code, human, stderr := runPeer(t, "list", "-data-dir", dir)
		if code != exitPeerOK {
			t.Fatalf("peer list exit = %d\nstderr: %s", code, stderr)
		}
		if !strings.Contains(human, hopFPHex) {
			t.Fatalf("human `peer list` output does not show the pinned fingerprint:\n%s", human)
		}
	})

	t.Run("an UNPINNED route is visible as unpinned rather than blank", func(t *testing.T) {
		t.Parallel()
		dir, _ := initPeerDataDir(t)
		if code, stdout, stderr := runPeer(t, "add", "-data-dir", dir, "-bus-id", hopBus, "-url", hopURL); code != exitPeerOK {
			t.Fatalf("peer add exit = %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
		}
		out := listPeers(t, dir)
		if len(out.Routes) != 1 || out.Routes[0].NextHopTLSCertFingerprint != "" {
			t.Fatalf("an unpinned route reports a pin: %+v", out.Routes)
		}
		code, human, _ := runPeer(t, "list", "-data-dir", dir)
		if code != exitPeerOK || !strings.Contains(human, "no certificate pinned") {
			t.Fatalf("human `peer list` does not say the hop is unpinned:\n%s", human)
		}
	})

	t.Run("re-pinning a rotated certificate WRITES; re-applying the same one does not", func(t *testing.T) {
		t.Parallel()
		dir, _ := initPeerDataDir(t)
		_, firstHex := peerTestCert(t, hopBus)

		if code, _, stderr := runPeer(t, "add", "-data-dir", dir, "-bus-id", hopBus, "-url", hopURL, "-tls-fingerprint", firstHex); code != exitPeerOK {
			t.Fatalf("first add exit = %d\nstderr: %s", code, stderr)
		}
		// The SAME pin at the SAME address is a no-op and is reported as one.
		code, stdout, _ := runPeer(t, "add", "-data-dir", dir, "-bus-id", hopBus, "-url", hopURL, "-tls-fingerprint", firstHex, "-json")
		if code != exitPeerOK {
			t.Fatalf("re-apply exit = %d: %s", code, stdout)
		}
		var same peerResult
		if err := json.Unmarshal([]byte(stdout), &same); err != nil {
			t.Fatalf("--json: %v", err)
		}
		if len(same.Changes) != 1 || !same.Changes[0].Unchanged {
			t.Fatalf("re-applying an identical pin was reported as a fresh write: %+v", same.Changes)
		}

		// A ROTATED certificate at an UNCHANGED address must be written. This is
		// the case a no-op predicate comparing only the URL would swallow,
		// leaving the old certificate trusted while reporting success.
		rotatedFP, rotatedHex := peerTestCert(t, hopBus)
		code, stdout, _ = runPeer(t, "add", "-data-dir", dir, "-bus-id", hopBus, "-url", hopURL, "-tls-fingerprint", rotatedHex, "-json")
		if code != exitPeerOK {
			t.Fatalf("rotate exit = %d: %s", code, stdout)
		}
		var rotated peerResult
		if err := json.Unmarshal([]byte(stdout), &rotated); err != nil {
			t.Fatalf("--json: %v", err)
		}
		if len(rotated.Changes) != 1 || rotated.Changes[0].Unchanged {
			t.Fatalf("re-pinning a rotated certificate was reported as unchanged: %+v", rotated.Changes)
		}
		out := listPeers(t, dir)
		if len(out.Routes) != 1 || out.Routes[0].NextHopTLSCertFingerprint != rotatedHex {
			t.Fatalf("after rotation the store still reports %+v, want the rotated pin %s", out.Routes, rotatedFP)
		}
	})

	t.Run("a withdrawal drops the pin with the route", func(t *testing.T) {
		t.Parallel()
		dir, _ := initPeerDataDir(t)
		_, hopFPHex := peerTestCert(t, hopBus)
		if code, _, stderr := runPeer(t, "add", "-data-dir", dir, "-bus-id", hopBus, "-url", hopURL, "-tls-fingerprint", hopFPHex); code != exitPeerOK {
			t.Fatalf("add exit = %d\nstderr: %s", code, stderr)
		}
		if code, _, stderr := runPeer(t, "remove", "-data-dir", dir, "-bus-id", hopBus, "-route"); code != exitPeerOK {
			t.Fatalf("remove exit = %d\nstderr: %s", code, stderr)
		}
		routes, _ := walPeerConfig(t, dir)
		for _, r := range routes {
			if r.State == relay.PeerRecordRemoved && r.NextHopTLSCertFingerprint != (buscert.Fingerprint{}) {
				t.Fatalf("the tombstone for %s still carries a certificate pin; withdrawing a route must withdraw the credential that dialled it", r.BusID)
			}
		}
	})
}

// TestPeerAddTLSFingerprintRefusals covers every way the flag is refused, and
// each refusal must write NOTHING — the exit-2 discipline this command already
// holds for -url and -signing-key.
func TestPeerAddTLSFingerprintRefusals(t *testing.T) {
	t.Parallel()
	_, validHex := peerTestCert(t, "bus-hop")
	_, signingKeyB64 := newSigningKey(t)

	for _, tc := range []struct {
		name string
		args []string
		// why states the property, so a failure names the property rather than
		// the flag.
		why string
	}{
		{
			name: "without -url there is no hop to pin",
			args: []string{"-bus-id", "bus-hop", "-tls-fingerprint", validHex},
			why:  "a certificate pin belongs to an ADDRESS; keyed to a bus id instead it would be the destination-keyed pin that breaks every non-adjacent hop",
		},
		{
			name: "with -signing-key but no -url, which is the trust-only entry",
			// The A <-> B <-> C case: a bus whose signing key we pin and which
			// we NEVER DIAL. It has no connection, so it has no certificate to
			// pin — and accepting one here is how a pin ends up keyed to a bus
			// rather than to a hop.
			args: []string{"-bus-id", "bus-hop", "-signing-key", signingKeyB64, "-tls-fingerprint", validHex},
			why:  "a bus we only pin a SIGNING KEY for is never dialled, so there is no hop whose certificate could be pinned",
		},
		{
			name: "UPPERCASE hex",
			args: []string{"-bus-id", "bus-hop", "-url", "https://b.example:8443", "-tls-fingerprint", strings.ToUpper(validHex)},
			why:  "one fingerprint must have exactly one spelling, or two equal pins eventually compare unequal",
		},
		{
			name: "a sha256: prefix",
			args: []string{"-bus-id", "bus-hop", "-url", "https://b.example:8443", "-tls-fingerprint", "sha256:" + validHex},
			why:  "the stored value must be byte-comparable with buscert.FingerprintOf's output",
		},
		{
			name: "truncated",
			args: []string{"-bus-id", "bus-hop", "-url", "https://b.example:8443", "-tls-fingerprint", validHex[:32]},
			why:  "a short pin is not a weaker pin, it is a different value that matches nothing",
		},
		{
			name: "all zero",
			args: []string{"-bus-id", "bus-hop", "-url", "https://b.example:8443", "-tls-fingerprint", strings.Repeat("0", 64)},
			why:  "the zero value is the record's marker for NO PIN, so it would be stored as an unpinned hop while the operator was told the pin was written",
		},
		{
			name: "not hex",
			args: []string{"-bus-id", "bus-hop", "-url", "https://b.example:8443", "-tls-fingerprint", strings.Repeat("z", 64)},
			why:  "a malformed pin must be refused before the lock is taken, not discovered at the durable write",
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, _ := initPeerDataDir(t)
			args := append([]string{"add", "-data-dir", dir}, tc.args...)
			code, stdout, stderr := runPeer(t, args...)
			if code != exitPeerUsage {
				t.Fatalf("exit = %d, want %d (usage): %s\nstdout: %s\nstderr: %s", code, exitPeerUsage, tc.why, stdout, stderr)
			}
			// NOTHING WRITTEN: a usage refusal happens before the data
			// directory is touched, so the log must not exist at all.
			if _, err := os.Stat(filepath.Join(dir, wal.WALFileName)); !os.IsNotExist(err) {
				t.Fatalf("a refused `peer add` wrote a log: %v", err)
			}
		})
	}

	t.Run("the without--url refusal names the remedy", func(t *testing.T) {
		t.Parallel()
		dir, _ := initPeerDataDir(t)
		code, _, stderr := runPeer(t, "add", "-data-dir", dir, "-bus-id", "bus-hop", "-tls-fingerprint", validHex)
		if code != exitPeerUsage {
			t.Fatalf("exit = %d, want %d\nstderr: %s", code, exitPeerUsage, stderr)
		}
		// The message must send the operator to -url. A generic "this add would
		// do nothing" would be true and useless, and would leave them guessing
		// that the pin belongs on -bus-id.
		if !strings.Contains(stderr, "-url") {
			t.Fatalf("the refusal does not name -url as the remedy:\n%s", stderr)
		}
	})

	t.Run("an oversized value is refused without being echoed", func(t *testing.T) {
		t.Parallel()
		dir, _ := initPeerDataDir(t)
		huge := strings.Repeat("a", 4096)
		code, stdout, stderr := runPeer(t, "add", "-data-dir", dir, "-bus-id", "bus-hop", "-url", "https://b.example:8443", "-tls-fingerprint", huge)
		if code != exitPeerUsage {
			t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, exitPeerUsage, stdout, stderr)
		}
		// An enormous argv value must not choose the size of the diagnostic we
		// print about refusing it.
		if strings.Contains(stdout+stderr, huge[:64]) {
			t.Fatalf("the refusal echoed the oversized value back to the terminal:\nstdout: %s\nstderr: %s", stdout, stderr)
		}
	})
}

// TestPeerAddNeverSilentlyUnpinsAHop is the regression test for the two
// FAIL-SILENT-UNPINNED defects the security gate found in RELAY-41's first
// implementation. Both are the same shape, and it is the shape the record
// design makes easy to miss:
//
//	A ROUTE RECORD IS WRITTEN WHOLE, NEVER AS A DELTA. So whatever an `add`
//	does not say is not left unchanged — it is ERASED.
//
// Neither defect was exploitable at the time (nothing verifies the pin yet),
// and that is exactly why they had to be fixed then: RELAY-20 is what turns a
// durably-unpinned hop into a live transport-trust hole, and by then the bad
// state is already on disk.
func TestPeerAddNeverSilentlyUnpinsAHop(t *testing.T) {
	t.Parallel()

	const (
		hopBus  = "bus-hop"
		destBus = "bus-dest"
		hopURL  = "https://b.example:8443"
	)

	t.Run("omitting -tls-fingerprint on a pinned hop is REFUSED, not applied", func(t *testing.T) {
		t.Parallel()
		dir, _ := initPeerDataDir(t)
		hopFP, hopFPHex := peerTestCert(t, hopBus)

		if code, _, stderr := runPeer(t, "add", "-data-dir", dir, "-bus-id", hopBus, "-url", hopURL, "-tls-fingerprint", hopFPHex, "-route-for", destBus); code != exitPeerOK {
			t.Fatalf("first add exit = %d\nstderr: %s", code, stderr)
		}

		// The realistic accident: a colleague adds one more destination through
		// the same hop, following a runbook written before the flag existed.
		code, stdout, stderr := runPeer(t, "add", "-data-dir", dir, "-bus-id", hopBus, "-url", hopURL, "-route-for", "bus-other", "-json")
		if code != exitPeerUsage {
			t.Fatalf("an add that would erase an existing pin exited %d, want %d (usage)\nstdout: %s\nstderr: %s", code, exitPeerUsage, stdout, stderr)
		}

		// AND THE PIN IS STILL THERE. A refusal that had already written the
		// first record would be the same bug with an error message.
		out := listPeers(t, dir)
		if len(out.Routes) != 2 {
			t.Fatalf("the refusal changed the routing table: %+v", out.Routes)
		}
		for _, r := range out.Routes {
			if r.NextHopTLSCertFingerprint != hopFPHex {
				t.Fatalf("route %s lost its pin (%q) to a refused add; the certificate at %s is still %s", r.BusID, r.NextHopTLSCertFingerprint, hopURL, hopFP)
			}
		}
	})

	t.Run("an empty flag value is refused, not read as absent", func(t *testing.T) {
		t.Parallel()
		dir, _ := initPeerDataDir(t)

		// `-tls-fingerprint "$PEER_FP"` with PEER_FP unset. Treating it as
		// "the flag was not given" would write an UNPINNED route and report
		// success — a trust anchor lost to an unset shell variable.
		for _, empty := range []string{"", "   "} {
			code, stdout, stderr := runPeer(t, "add", "-data-dir", dir, "-bus-id", hopBus, "-url", hopURL, "-tls-fingerprint", empty)
			if code != exitPeerUsage {
				t.Fatalf("-tls-fingerprint %q exited %d, want %d\nstdout: %s\nstderr: %s", empty, code, exitPeerUsage, stdout, stderr)
			}
			if _, err := os.Stat(filepath.Join(dir, wal.WALFileName)); !os.IsNotExist(err) {
				t.Fatalf("a refused add wrote a log: %v", err)
			}
		}
	})

	t.Run("two DIFFERENT pins for one address are refused", func(t *testing.T) {
		t.Parallel()
		dir, _ := initPeerDataDir(t)
		_, firstHex := peerTestCert(t, hopBus)

		if code, _, stderr := runPeer(t, "add", "-data-dir", dir, "-bus-id", hopBus, "-url", hopURL, "-tls-fingerprint", firstHex, "-route-for", destBus); code != exitPeerOK {
			t.Fatalf("first add exit = %d\nstderr: %s", code, stderr)
		}

		// Rotating the pin while naming only the hop would leave every
		// -route-for destination through it still trusting the OLD certificate,
		// while telling the operator the rotation succeeded.
		_, rotatedHex := peerTestCert(t, hopBus)
		code, stdout, stderr := runPeer(t, "add", "-data-dir", dir, "-bus-id", hopBus, "-url", hopURL, "-tls-fingerprint", rotatedHex)
		if code != exitPeerUsage {
			t.Fatalf("a partial re-pin exited %d, want %d (usage)\nstdout: %s\nstderr: %s", code, exitPeerUsage, stdout, stderr)
		}
		if !strings.Contains(stderr, destBus) {
			t.Fatalf("the refusal does not name the destination left behind (%s):\n%s", destBus, stderr)
		}
		for _, r := range listPeers(t, dir).Routes {
			if r.NextHopTLSCertFingerprint != firstHex {
				t.Fatalf("the refused rotation still changed %s: %+v", r.BusID, r)
			}
		}

		// Naming EVERY destination through the hop in one invocation succeeds
		// and re-pins them together — the remedy the refusal points at.
		if code, _, stderr := runPeer(t, "add", "-data-dir", dir, "-bus-id", hopBus, "-url", hopURL, "-tls-fingerprint", rotatedHex, "-route-for", destBus); code != exitPeerOK {
			t.Fatalf("re-pinning every destination together exited %d\nstderr: %s", code, stderr)
		}
		for _, r := range listPeers(t, dir).Routes {
			if r.NextHopTLSCertFingerprint != rotatedHex {
				t.Fatalf("route %s still pins %q after a full re-pin, want %q", r.BusID, r.NextHopTLSCertFingerprint, rotatedHex)
			}
		}
	})

	t.Run("adopting pinning on an existing UNPINNED federation says so accurately", func(t *testing.T) {
		t.Parallel()
		// The commonest real case, and the one whose refusal message was wrong
		// in the first implementation: the sibling route pins NOTHING, so a
		// message claiming it "pins a DIFFERENT certificate" sends the operator
		// hunting a fingerprint that does not exist (a reviewer finding).
		dir, _ := initPeerDataDir(t)
		if code, _, stderr := runPeer(t, "add", "-data-dir", dir, "-bus-id", hopBus, "-url", hopURL, "-route-for", destBus); code != exitPeerOK {
			t.Fatalf("unpinned add exit = %d\nstderr: %s", code, stderr)
		}
		_, hopFPHex := peerTestCert(t, hopBus)
		code, _, stderr := runPeer(t, "add", "-data-dir", dir, "-bus-id", hopBus, "-url", hopURL, "-tls-fingerprint", hopFPHex)
		if code != exitPeerUsage {
			t.Fatalf("half-pinning exit = %d, want %d\nstderr: %s", code, exitPeerUsage, stderr)
		}
		if strings.Contains(stderr, "DIFFERENT certificate") {
			t.Fatalf("the refusal claims a conflicting pin that does not exist:\n%s", stderr)
		}
		if !strings.Contains(stderr, "UNPINNED") {
			t.Fatalf("the refusal does not say the sibling route is unpinned:\n%s", stderr)
		}
		// The documented remedy — name every destination in one invocation —
		// must actually work.
		if code, _, stderr := runPeer(t, "add", "-data-dir", dir, "-bus-id", hopBus, "-url", hopURL, "-tls-fingerprint", hopFPHex, "-route-for", destBus); code != exitPeerOK {
			t.Fatalf("adopting pinning across every destination exit = %d\nstderr: %s", code, stderr)
		}
	})

	t.Run("the address comparison is case-insensitive, so host case cannot bypass it", func(t *testing.T) {
		t.Parallel()
		// A bare origin is case-insensitive, but validatePeerBareOrigin
		// deliberately does not rewrite what the operator typed — so one origin
		// can be stored two ways, and a byte comparison let an UNPINNED route in
		// beside a pinned one (a security-gate finding, reproduced).
		dir, _ := initPeerDataDir(t)
		_, hopFPHex := peerTestCert(t, hopBus)
		if code, _, stderr := runPeer(t, "add", "-data-dir", dir, "-bus-id", hopBus, "-url", "https://b.example:8443", "-tls-fingerprint", hopFPHex); code != exitPeerOK {
			t.Fatalf("pinned add exit = %d\nstderr: %s", code, stderr)
		}
		code, stdout, stderr := runPeer(t, "add", "-data-dir", dir, "-bus-id", "bus-e", "-url", "https://B.EXAMPLE:8443")
		if code != exitPeerUsage {
			t.Fatalf("an unpinned route at the same origin spelled in a different case exited %d, want %d\nstdout: %s\nstderr: %s", code, exitPeerUsage, stdout, stderr)
		}
	})

	t.Run("an unpinned hop may still be added to and removed freely", func(t *testing.T) {
		t.Parallel()
		// The refusals must not make the UNPINNED world harder to work with:
		// every existing federation is unpinned, and nothing above may fire on
		// one. This is the false-positive guard.
		dir, _ := initPeerDataDir(t)
		if code, _, stderr := runPeer(t, "add", "-data-dir", dir, "-bus-id", hopBus, "-url", hopURL, "-route-for", destBus); code != exitPeerOK {
			t.Fatalf("unpinned add exit = %d\nstderr: %s", code, stderr)
		}
		if code, _, stderr := runPeer(t, "add", "-data-dir", dir, "-bus-id", hopBus, "-url", hopURL, "-route-for", "bus-third"); code != exitPeerOK {
			t.Fatalf("a second unpinned add through the same hop exit = %d\nstderr: %s", code, stderr)
		}
		if code, _, stderr := runPeer(t, "remove", "-data-dir", dir, "-bus-id", destBus, "-route"); code != exitPeerOK {
			t.Fatalf("remove exit = %d\nstderr: %s", code, stderr)
		}
	})
}
