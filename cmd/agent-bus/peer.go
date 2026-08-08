package main

// The operator-facing federation subcommand: `agent-bus peer add|list|remove`.
//
// # Why this is a SERVER SUBCOMMAND and not an HTTP route
//
// DECISIONS.md, 2026-08-08 FEDERATION (e): "Peer configuration is an offline
// `agent-bus peer` subcommand under the dirlock, not a new online admin route",
// following the `invite mint` / E4 precedent. The authority to say who this bus
// federates with, and whose signing keys it pins, is FILESYSTEM ACCESS to the
// data directory. Nothing new is exposed on the wire and no new privilege tier
// exists, which is the whole point: an online re-peering route would have to be
// authenticated by something, and the only thing that can authorise "change this
// bus's trust anchors" is the operator who owns the disk. What that costs is
// stated in the same ruling: a topology change needs a restart.
//
// # THE BUS MUST BE STOPPED — for `add` and `remove`, and for `list` too
//
// `add` and `remove` append through internal/wal's two-phase path, and the data
// directory takes an EXCLUSIVE dirlock precisely so two processes never append
// to one log. `list` writes nothing — it replays with wal.Replay, which repairs
// nothing and creates nothing — but it takes the same lock anyway, because a
// read racing an append can see a half-written tail record and would then either
// report a peer that is not yet durable or fail with a corruption error on a
// perfectly healthy bus. One rule ("stop the bus to configure peering") is also
// easier to get right than three.
//
// # ROUTE AND TRUST ARE INDEPENDENT, AND THE FLAGS MUST KEEP THEM THAT WAY
//
// This is the single most important property of this command's surface, and it
// comes straight from the topology the FEDERATION epic exists for:
//
//	laptop(A) <-> internet(B) <-> this machine(C)
//
// C NEVER PEERS WITH A. It has no address for A and no reason to acquire one.
// But C must PIN A's bus signing key, because a message ORIGINATING at A is
// verified by C against that pin and B is explicitly not allowed to vouch for it
// (internal/relay/signed.go: "presentation is not attestation"). So:
//
//	agent-bus peer add -bus-id busA -signing-key <b64>      # trust, NO route
//	agent-bus peer add -bus-id busB -url https://b:8443     # route, NO trust
//
// Both are legal on their own, and each writes exactly one record kind.
// -signing-key never implies a route and -url never implies a pin. A surface
// that required them together would foreclose the case the whole epic exists
// for, and the mistake would only be discovered at RELAY-17.
//
// # WHAT `-route-for` MEANS ON DISK
//
// `-route-for busC` installs a STATIC NEXT-HOP route (DECISIONS.md FEDERATION
// (f): static routing, not a routing protocol). It is expressed with the record
// set internal/relay already ships, and the encoding is worth stating plainly
// because nothing in the record says it:
//
//	a route record's bus id is the DESTINATION bus, and its base URL is the
//	address to DIAL to reach that destination.
//
// For a directly-peered bus those are the same machine. For a non-adjacent one
// they are not: `peer add -bus-id busB -url https://b:8443 -route-for busC`
// writes a route record for busC carrying busB's address, i.e. "traffic for busC
// leaves via the peer at https://b:8443". THE RECORD DOES NOT REMEMBER THAT THE
// NEXT HOP IS busB — only the address. This command reports the via-bus in its
// own output because it knows it from the same command line, and `peer list`
// does not, because on disk it is not there. A fourth bus needs its own operator
// entry on every bus that must reach it; that is the recorded trade, not an
// oversight.
//
// ONE CONSEQUENCE FOR WHOEVER CONSUMES THESE RECORDS (RELAY-20/RELAY-24): for a
// -route-for entry the ADDRESS BELONGS TO A DIFFERENT BUS THAN THE RECORD'S BUS
// ID. Anything that later keys a per-peer credential off the record's bus id —
// a TLS certificate pin, a client certificate, a peer principal — would be
// pinning busC's identity against a connection that terminates at busB, and
// would break every non-adjacent hop. The identity on the wire is the NEXT
// HOP's; the record's bus id is the DESTINATION. They are not the same field
// and must not be treated as one.
//
// # THE REPLAY PRECONDITION, WHICH THIS FILE IS THE PLACE TO HONOUR
//
// relay.PeerStoreOptions.Durable states it: THE CALLER MUST REPLAY THE LOG INTO
// THE STORE BEFORE THE FIRST WRITE. PeerStore.configSeq — the bus-wide monotonic
// high-water mark the whole record design rests on — is rebuilt ONLY from the
// records its Apply is handed, so a store wired to an un-replayed log mints
// config_seq 1 over a log that already holds 1..N, and on the NEXT replay the
// superseded generation arrives first at an equal sequence and WINS. A security
// gate reproduced exactly that during RELAY-10.
//
// The package cannot enforce it. This file does, structurally rather than by
// remembering to: the store is constructed with a deferredLog (invite.go), whose
// Write ERRORS while the log is nil, and the log is handed to it only after
// wal.Open — which replays into the Applier before it returns — has succeeded.
// A write is therefore unreachable before replay has completed, and it fails
// loudly rather than silently minting 1 if that ever stops being true.
// TestPeerAddReplaysTheLogBeforeTheFirstWrite pins the observable half.
//
// # SCOPE: CONFIGURATION ONLY. NOTHING SERVES THIS YET.
//
// Said plainly, because "federation works now" is the easiest wrong conclusion
// to draw from a working `peer add`. Records written here are durable and are
// replayed, but no running bus reads them: relay.Handler is registered on no mux
// and PeerStore is constructed nowhere in the server (RELAY-24 is the
// composition root). This command configures a topology that is not yet served.

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dodgymike/agent-bus/internal/dirlock"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/relay"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// peerCommandName is the single non-flag argument main() intercepts before
// server flag parsing. Pinned as a constant so the dispatch in main.go and the
// usage text cannot drift apart.
const peerCommandName = "peer"

// Exit codes for `agent-bus peer`. These are a CONTRACT (invariant 7: an agent
// or a provisioning script shelling out branches on them) and are documented in
// CONTRACTS-CLI.md.
//
// 0-4 are DELIBERATELY THE SAME NUMBERS, WITH THE SAME MEANINGS, as
// `agent-bus invite` — an operator scripting the two commands should not have to
// hold two tables in their head, and 3 ("stop the bus") and 4 ("this directory
// has no bus identity") have the same remedies here as there.
const (
	// exitPeerOK: every requested change is durable (or was already the
	// configuration on disk, which writes nothing and is still success).
	exitPeerOK = 0
	// exitPeerFailed: a change failed. Anything that HAD become durable before
	// the failure is listed in the output — see peerError.Applied — because an
	// operator must know what is on disk before retrying.
	exitPeerFailed = 1
	// exitPeerUsage: bad flags, an unknown subcommand, a malformed bus id, a
	// -url that is not a bare https origin, or a combination that would do
	// nothing. Matches the server binary's "2 on invalid flags/config".
	exitPeerUsage = 2
	// exitPeerBusRunning: the data directory is locked by a live process.
	// Remedy: stop the bus, configure peering, start it again.
	exitPeerBusRunning = 3
	// exitPeerNoIdentity: the data directory does not hold a usable bus
	// identity — it is missing, is not a directory, or has no bus-id file. The
	// remedy splits: start the bus once if it has never run, restore from
	// backup if it has. NOTHING IS WRITTEN on any of these paths.
	exitPeerNoIdentity = 4
	// exitPeerUnknown: `remove` found NONE of the record kinds it was asked to
	// withdraw. It is reported only when NOTHING was withdrawn, so it always
	// means nothing was written: if one requested kind existed and the other did
	// not, the one that existed IS withdrawn, the command succeeds, and the
	// absent kind is named in `not_found`. Both gates reproduced the earlier
	// behaviour, where `remove -route -trust` on a bus with a pin and no route
	// aborted on the route and LEFT THE TRUST ANCHOR PINNED while exiting with
	// the code a script is told it may ignore.
	// Separate from 1 because the remedy is different and mechanical — run
	// `peer list` and check the spelling — and because a provisioning script
	// that removes-then-adds needs to tell "there was nothing to remove" (which
	// is fine) from "the removal failed" (which is not). Nothing is written.
	exitPeerUnknown = 5
)

// maxPeerSigningKeyChars bounds a -signing-key value BEFORE it is base64
// decoded. A legal value is exactly 44 characters (32 raw bytes, standard
// base64 with padding); 128 leaves room for whitespace an operator's shell may
// have carried in and can never refuse a legal key. The guard exists so an
// enormous argv value cannot choose the size of the diagnostic we print about
// refusing it — the same discipline internal/relay applies to file-derived text.
const maxPeerSigningKeyChars = 128

// runPeerCommand dispatches `agent-bus peer <subcommand>`.
//
// It returns an exit code rather than calling os.Exit so that it is testable in
// process; main() is the only place that exits.
func runPeerCommand(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, peerUsage)
		return exitPeerUsage
	}
	switch args[0] {
	case "add":
		return runPeerAdd(args[1:], stdout, stderr)
	case "list":
		return runPeerList(args[1:], stdout, stderr)
	case "remove":
		return runPeerRemove(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		fmt.Fprint(stdout, peerUsage)
		return exitPeerOK
	default:
		// The unknown subcommand is NOT echoed: it is unvalidated argv on its way
		// to a terminal. The usage text below says what is legal, which is the
		// useful half anyway.
		fmt.Fprint(stderr, "agent-bus peer: unknown subcommand\n\n")
		fmt.Fprint(stderr, peerUsage)
		return exitPeerUsage
	}
}

const peerUsage = `agent-bus peer — configure this bus's federation: routes and pinned bus signing keys

USAGE
  agent-bus peer add    -data-dir <dir> -bus-id <busID> [-url <https origin>]
                        [-signing-key <base64> ...] [-route-for <busID> ...] [-json]
  agent-bus peer list   [-data-dir <dir>] [-json]
  agent-bus peer remove -data-dir <dir> -bus-id <busID> (-route | -trust | -route -trust) [-json]

THE BUS MUST NOT BE RUNNING. Every subcommand takes the data directory's
exclusive lock, which a running bus holds. Stop the bus, configure peering,
start it again — a topology change needs a restart (DECISIONS.md, FEDERATION (e)).

A ROUTE AND A TRUST PIN ARE INDEPENDENT, and that is the point of this surface.
  -url          alone installs a ROUTE with no trust  (a bus we relay THROUGH)
  -signing-key  alone installs TRUST with no route    (a bus whose messages we
                verify but which we have no address for — the A <-> B <-> C case,
                where C pins A's signing key while never peering with A)
Passing both installs both, as two separate durable records.

STATIC NEXT-HOP ROUTING, not a routing protocol (DECISIONS.md, FEDERATION (f)).
A route record's bus id is the DESTINATION and its base URL is the address to
DIAL to reach it, so:

  agent-bus peer add -bus-id busB -url https://b.example:8443 -route-for busC

means "busB lives at that address, and traffic for busC leaves via it". A fourth
bus needs its own -route-for entry on every bus that must reach it.

FLAGS (add)
  -data-dir <dir>      the bus's data directory (default "./data"). It must
                       already hold this bus's bus-id file; this command NEVER
                       creates one, because a regenerated bus id renames the bus
                       away from every agent id it has ever issued.
  -bus-id <busID>      REQUIRED. The peer bus this entry is about. It may not be
                       this bus's own id (invariant 2).
  -url <origin>        a BARE https origin — scheme, host, optional port, and
                       nothing else. No path, query, fragment or userinfo: the
                       relay path is appended at every dial, so a stored path
                       would ride along on all of them.
  -signing-key <b64>   a pinned Ed25519 BUS SIGNING key, standard base64.
                       Repeatable, at most 2 — and TWO MEANS A ROLLOVER IS IN
                       PROGRESS (the outgoing key and the incoming one), not a
                       general-purpose accept list. Repeating -signing-key
                       REPLACES the pin set; it does not add to it.
  -route-for <busID>   install a static next-hop route for another bus through
                       -url. Repeatable. Requires -url; may not name this bus,
                       and may not name -bus-id (that route is what -url already
                       installs).
  -json                emit one JSON object on stdout.
  -log-level <lvl>     severity floor for recovery/durability lines on stderr
                       (default "warn").

FLAGS (list)
  -data-dir, -json, -log-level as above. Writes nothing and creates nothing.

FLAGS (remove)
  -data-dir, -bus-id, -json, -log-level as above, plus:
  -route               withdraw the ROUTE for -bus-id.
  -trust               withdraw the pinned SIGNING KEYS for -bus-id.
  At least one of -route/-trust is REQUIRED, and neither is implied by the
  other: withdrawing a route you meant to keep breaks federation loudly, while
  leaving a key pinned that you meant to revoke fails silently, so this command
  will not guess which you meant.

EXIT CODES
  0  every requested change is durable (or was already the configuration on disk)
  1  a change failed; anything already durable is listed in the output
  2  usage: bad flag, unknown subcommand, malformed bus id, bad -url, or a
     combination that would do nothing
  3  the data directory is locked — a bus is running; stop it and retry
  4  the data directory holds no bus-id file, so this bus has no identity.
     Start the bus once if it has never run; restore from backup if it has.
     Nothing is written.
  5  remove: NONE of the requested records exist on this bus, so nothing was
     withdrawn and nothing is written. If one of -route/-trust existed and the
     other did not, the one that existed is withdrawn and this is exit 0.

NOTHING SERVES THIS YET. Records written here are durable and are replayed, but
no running bus reads them — the relay handler is registered on no listener and
the peer store is not yet wired into server startup (RELAY-24).
`

// peerChange is one durable configuration change, as this command reports it.
//
// It is the COMMAND'S OUTPUT and not a durable shape: NextHopBusID in
// particular is knowledge this invocation has from its own command line and
// that the record on disk does not carry (see the file comment).
type peerChange struct {
	// Kind is "route" or "trust" — the two independent record kinds.
	Kind string `json:"kind"`

	// BusID is the bus the record is ABOUT: for a route, the destination; for
	// trust, the bus whose keys are pinned.
	BusID string `json:"bus_id"`

	// State is "active" or "removed", the record's durable lifecycle state.
	State string `json:"state"`

	// BaseURL is the address to dial to reach BusID. Empty on a trust record
	// and on a withdrawal (a tombstone carries no live configuration).
	BaseURL string `json:"base_url,omitempty"`

	// SigningKeys are the pinned keys, standard base64, in the operator's
	// order. Empty on a route record and on a withdrawal.
	SigningKeys []string `json:"signing_keys,omitempty"`

	// NextHopBusID names the bus whose address this route dials, and is set
	// ONLY for a -route-for entry. It is NOT on disk: a route record carries an
	// address, not a via-bus.
	NextHopBusID string `json:"next_hop_bus_id,omitempty"`

	// ConfigSeq is this bus's own monotonic configuration sequence number for
	// the generation now on disk.
	ConfigSeq uint64 `json:"config_seq"`

	// UpdatedAt is when that generation was written, RFC3339Nano UTC.
	UpdatedAt string `json:"updated_at"`

	// Unchanged is true when the store found this exact configuration already
	// applied and therefore wrote NOTHING. Reported rather than hidden: an
	// operator re-running a provisioning script must be able to see that the
	// second run was a no-op, and ConfigSeq then names the EARLIER generation.
	Unchanged bool `json:"unchanged,omitempty"`
}

// peerResult is the --json success shape for `add` and `remove`.
type peerResult struct {
	// OK is true on this type by construction; it exists so a caller consuming
	// --json branches on one field for both success and failure, which
	// peerError mirrors.
	OK bool `json:"ok"`

	// BusID is THIS bus's id — the local one, not a peer's. It is here because
	// every record below is scoped to it, and a provisioning script writing to
	// several data directories needs to know which bus it just configured.
	BusID string `json:"bus_id"`

	// Changes is what is now on disk, in the order it was written.
	Changes []peerChange `json:"changes"`

	// NotFound names the requested record kinds this bus held nothing for
	// ("route", "trust"). It is reported rather than dropped because a
	// `remove -route -trust` where only one kind existed still withdrew the
	// other, and the operator must be able to see which half was absent. When
	// NO requested kind exists the command fails with exit 5 instead, so a
	// mistyped bus id is never reported as a partial success.
	NotFound []string `json:"not_found,omitempty"`
}

// peerListResult is the --json shape for `list`.
type peerListResult struct {
	OK    bool   `json:"ok"`
	BusID string `json:"bus_id"`

	// Routes and Trust are the ACTIVE records only, each sorted by bus id.
	// Tombstones are excluded: they are bookkeeping that stops a withdrawn
	// configuration being resurrected by a duplicated record, never something
	// to dial or to verify against. They are deliberately NOT listed, because a
	// withdrawn peer appearing in `peer list` is exactly the ambiguity the
	// state field would then have to resolve for every reader.
	Routes []peerChange `json:"routes"`
	Trust  []peerChange `json:"trust"`
}

// peerError is the --json failure shape. `ok` is the field a caller branches on,
// so it is present in both shapes and in the same position — the same contract
// `agent-bus invite` publishes.
type peerError struct {
	OK       bool   `json:"ok"`
	Error    string `json:"error"`
	Remedy   string `json:"remedy,omitempty"`
	ExitCode int    `json:"exit_code"`

	// Applied is what became DURABLE before the failure. A configuration change
	// is several records, and a failure part-way through leaves the earlier ones
	// on disk — saying so is the difference between an operator retrying safely
	// and one guessing.
	Applied []peerChange `json:"applied,omitempty"`
}

// peerCmdError carries the exit code and the remedy for the failures that have
// a specific one, plus whatever had already become durable, so the command maps
// them without matching error text.
type peerCmdError struct {
	code    int
	remedy  string
	msg     string
	cause   error
	applied []peerChange
}

func (e *peerCmdError) Error() string {
	if e.cause == nil {
		return e.msg
	}
	return e.msg + ": " + e.cause.Error()
}

func (e *peerCmdError) Unwrap() error { return e.cause }

// peerFail reports a failure in whichever mode was asked for and returns the
// exit code, so every failure path is one line at the call site.
//
// In --json mode the object goes to STDOUT, not stderr: an agent or script that
// redirected stderr away still gets a parseable answer, which is the whole
// reason --json exists (invariant 7's second audience).
func peerFail(stdout, stderr io.Writer, asJSON bool, sub string, e *peerCmdError) int {
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(peerError{OK: false, Error: e.Error(), Remedy: e.remedy, ExitCode: e.code, Applied: e.applied}); err == nil {
			return e.code
		}
	}
	fmt.Fprintf(stderr, "agent-bus peer %s: %s\n", sub, e.Error())
	if e.remedy != "" {
		fmt.Fprintf(stderr, "  remedy: %s\n", e.remedy)
	}
	for _, c := range e.applied {
		fmt.Fprintf(stderr, "  ALREADY DURABLE: %s\n", describePeerChange(c))
	}
	return e.code
}

// usagePeerError is the exit-2 shape, used for everything decided before the
// data directory is touched.
func usagePeerError(msg, remedy string) *peerCmdError {
	return &peerCmdError{code: exitPeerUsage, msg: msg, remedy: remedy}
}

// stringListFlag collects a repeatable string flag in the order it was given.
// Order matters for -signing-key: the pin set is stored in the operator's order
// and two sets differing only in order are two different generations.
type stringListFlag []string

func (f *stringListFlag) String() string { return strings.Join(*f, ",") }

func (f *stringListFlag) Set(v string) error {
	*f = append(*f, v)
	return nil
}

// ---------------------------------------------------------------------------
// add
// ---------------------------------------------------------------------------

// peerAddRequest is a validated `peer add`, everything checked that can be
// checked WITHOUT the data directory. Splitting it out is what lets the usage
// errors (exit 2) all happen before a lock is taken and before a byte is
// written.
type peerAddRequest struct {
	busID     string
	baseURL   string
	keys      []ed25519.PublicKey
	routeFor  []string
	keysGiven bool
}

func runPeerAdd(args []string, stdout, stderr io.Writer) int {
	var (
		dataDir  string
		busID    string
		baseURL  string
		keys     stringListFlag
		routeFor stringListFlag
		asJSON   bool
		logLevel string
	)
	fs := flag.NewFlagSet("agent-bus peer add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	// A NO-OP Usage, for invite.go's reason: flag calls Usage both for -h and
	// for a bad flag, but requested help is OUTPUT (stdout) and an error is
	// diagnostics (stderr). The two cases are separated at the Parse call.
	fs.Usage = func() {}
	fs.StringVar(&dataDir, "data-dir", defaultDataDir, "the bus's data directory; it must already hold this bus's bus-id file")
	fs.StringVar(&busID, "bus-id", "", "REQUIRED: the peer bus this entry is about")
	fs.StringVar(&baseURL, "url", "", "the peer's address: a BARE https origin, e.g. https://b.example:8443")
	fs.Var(&keys, "signing-key", "a pinned Ed25519 bus signing key, standard base64; repeatable, at most 2 (two means a rollover)")
	fs.Var(&routeFor, "route-for", "install a static next-hop route for this bus through -url; repeatable")
	fs.BoolVar(&asJSON, "json", false, "emit the result as one JSON object on stdout")
	fs.StringVar(&logLevel, "log-level", "warn", "minimum log severity for durability lines on stderr ("+logging.Levels+")")

	// THERE IS NO FLAG THAT SUPPLIES A CONFIG SEQUENCE OR ANY OTHER ID, and none
	// may be added. Invariant 1: the server is authoritative on every id and on
	// the configuration sequence; relay.PeerStore mints config_seq from its own
	// replayed high-water mark. Operator input names WHICH bus and WHERE it
	// lives — it never influences a minted number.

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, peerUsage)
			return exitPeerOK
		}
		fmt.Fprint(stderr, peerUsage)
		return exitPeerUsage
	}
	if fs.NArg() > 0 {
		// NOT echoed: unvalidated argv on its way to a terminal.
		fmt.Fprint(stderr, "agent-bus peer add: unexpected argument\n")
		return exitPeerUsage
	}

	lvl, err := logging.ParseLevel(logLevel)
	if err != nil {
		return peerFail(stdout, stderr, asJSON, "add", usagePeerError(fmt.Sprintf("invalid -log-level: %v", err), "use one of "+logging.Levels))
	}
	req, cmdErr := validatePeerAdd(busID, baseURL, keys, routeFor)
	if cmdErr != nil {
		return peerFail(stdout, stderr, asJSON, "add", cmdErr)
	}

	lg := logging.New(stderr, lvl)
	localBusID, changes, cmdErr := applyPeerAdd(dataDir, req, lg)
	if cmdErr != nil {
		return peerFail(stdout, stderr, asJSON, "add", cmdErr)
	}
	return writePeerResult(stdout, stderr, asJSON, "add", dataDir, localBusID, changes, nil)
}

// validatePeerAdd checks everything that can be checked without the data
// directory, so that a usage refusal never takes the lock and never writes.
//
// It does NOT check a bus id against THIS bus's id — that fact lives on disk and
// is checked in applyPeerAdd, before any write, once the local id is known.
func validatePeerAdd(busID, baseURL string, keys, routeFor []string) (peerAddRequest, *peerCmdError) {
	var req peerAddRequest

	if err := validatePeerCLIBusID("-bus-id", busID); err != nil {
		return req, err
	}
	req.busID = busID

	if baseURL != "" {
		canonical, err := validatePeerBareOrigin(baseURL)
		if err != nil {
			return req, err
		}
		req.baseURL = canonical
	}

	req.keysGiven = len(keys) > 0
	if len(keys) > relay.MaxPinnedBusSigningKeys {
		return req, usagePeerError(
			fmt.Sprintf("%d -signing-key values, but at most %d may be pinned", len(keys), relay.MaxPinnedBusSigningKeys),
			"more than one pinned key means a signing-key ROLLOVER is in progress — the outgoing key and the incoming one — and the pin set is not a general-purpose accept list")
	}
	for i, raw := range keys {
		key, err := parsePeerSigningKey(i, raw)
		if err != nil {
			return req, err
		}
		// The record refuses an all-zero key and a repeated one too, but only
		// AFTER the lock is taken, which would report an operator typo as a
		// generic exit-1 failure. Both are refused here so the whole class stays
		// exit 2 and writes nothing.
		if isAllZeroKey(key) {
			return req, usagePeerError(
				fmt.Sprintf("-signing-key %d is all zero, which is either an uninitialised value or a copy that lost its content", i+1),
				"copy the peer bus's signing key again; an all-zero key is also a small-order point and would pin something that verifies nothing")
		}
		for j := range req.keys {
			if bytes.Equal(req.keys[j], key) {
				// Refused rather than deduplicated: in a two-key set a duplicate
				// means the operator believes a ROLLOVER is in progress when only
				// one key is really pinned, and collapsing it silently hides that.
				return req, usagePeerError(
					fmt.Sprintf("-signing-key %d repeats -signing-key %d; a pin set is a set", i+1, j+1),
					"two pinned keys mean a rollover window — the outgoing key and the incoming one — so they must differ")
			}
		}
		req.keys = append(req.keys, key)
	}

	// An add that installs NEITHER a route NOR a pin is refused rather than
	// quietly succeeding: it would report "ok" having changed nothing, which is
	// the shape of a provisioning script that silently does not configure the
	// federation it believes it configured.
	if req.baseURL == "" && !req.keysGiven {
		return req, usagePeerError(
			"this add would install neither a route nor a trust pin, so it would do nothing",
			"pass -url to install a route, -signing-key to pin a bus signing key, or both — they are independent, and either alone is a legitimate entry")
	}

	seen := make(map[string]struct{}, len(routeFor))
	for _, dest := range routeFor {
		if err := validatePeerCLIBusID("-route-for", dest); err != nil {
			return req, err
		}
		if strings.EqualFold(dest, req.busID) {
			return req, usagePeerError(
				fmt.Sprintf("-route-for names %q, which is also -bus-id", dest),
				"the route to the peer itself is what -url installs; -route-for is for a bus reached THROUGH that peer")
		}
		// Refused rather than deduplicated, for the reason a repeated pinned key
		// is: a duplicate means the operator believes they configured two
		// destinations when they configured one, and collapsing it silently
		// hides that.
		folded := strings.ToLower(dest)
		if _, dup := seen[folded]; dup {
			return req, usagePeerError(
				fmt.Sprintf("-route-for names %q more than once", dest),
				"list each destination bus once; two spellings differing only by ASCII case are the same routing key and are refused for the same reason")
		}
		seen[folded] = struct{}{}
		req.routeFor = append(req.routeFor, dest)
	}
	if len(req.routeFor) > 0 && req.baseURL == "" {
		return req, usagePeerError(
			"-route-for was given without -url, so there is no next hop to route through",
			"pass -url with the address of the peer that carries the traffic, e.g. -bus-id busB -url https://b.example:8443 -route-for busC")
	}
	return req, nil
}

// applyPeerAdd does the durable work: lock the data directory, load (never
// create) this bus's id, replay the log into a peer store, and write each
// requested record.
//
// The ORDER of the writes is deliberate and is a safety property, not a style
// choice. TRUST IS WRITTEN FIRST, then the peer's own route, then the
// -route-for routes. Every write is durable on its own, so a failure part-way
// through leaves the earlier ones on disk — and of the two possible half-states,
// "a bus we pin but do not route to" is inert, while "a bus we route to but
// cannot verify" is the one that matters the moment RELAY-17 lands. The routes
// through a next hop go last for the same reason: a destination route is useless
// until the hop it names exists.
func applyPeerAdd(dataDir string, req peerAddRequest, lg *logging.Logger) (string, []peerChange, *peerCmdError) {
	var changes []peerChange

	store, closeStore, localBusID, cmdErr := openPeerStore(dataDir, true, lg)
	if cmdErr != nil {
		return "", nil, cmdErr
	}
	defer closeStore()

	// EVERY bus id is checked against OUR OWN before ANYTHING is written, so a
	// self-peer refusal leaves the log untouched. relay.PeerStore refuses it
	// too — this is not the security boundary, it is the difference between one
	// clear refusal and a half-applied configuration.
	for _, candidate := range append([]string{req.busID}, req.routeFor...) {
		if err := relay.ValidatePeerBusID(localBusID, candidate); err != nil {
			return localBusID, nil, &peerCmdError{
				code:   exitPeerUsage,
				msg:    fmt.Sprintf("refusing %q", candidate),
				remedy: "a bus may not peer with itself, and a bus id is the namespace that makes cross-bus routing unambiguous (invariant 2); check -data-dir names the bus you meant",
				cause:  err,
			}
		}
	}

	if req.keysGiven {
		// The no-op predicate is evaluated BEFORE the write, against the same
		// question the store asks itself: is this exact pin set already active?
		// It cannot be answered from the returned record, because a Put and a
		// no-op both return a record that matches what was asked for.
		unchanged := trustAlreadyPinned(store, req.busID, req.keys)
		rec, err := store.PutTrust(relay.BusTrust{BusID: req.busID, SigningKeys: req.keys})
		if err != nil {
			return localBusID, nil, &peerCmdError{
				code:  exitPeerFailed,
				msg:   fmt.Sprintf("pinning bus signing keys for %s", req.busID),
				cause: err,
			}
		}
		changes = append(changes, trustChange(rec, unchanged))
	}

	if req.baseURL != "" {
		unchanged := routeAlreadyActive(store, req.busID, req.baseURL)
		rec, err := store.Put(relay.PeerConfig{BusID: req.busID, BaseURL: req.baseURL})
		if err != nil {
			return localBusID, nil, &peerCmdError{
				code:    exitPeerFailed,
				msg:     fmt.Sprintf("recording the route to %s", req.busID),
				cause:   err,
				applied: changes,
			}
		}
		changes = append(changes, routeChange(rec, "", unchanged))
	}

	for _, dest := range req.routeFor {
		unchanged := routeAlreadyActive(store, dest, req.baseURL)
		rec, err := store.Put(relay.PeerConfig{BusID: dest, BaseURL: req.baseURL})
		if err != nil {
			return localBusID, nil, &peerCmdError{
				code:    exitPeerFailed,
				msg:     fmt.Sprintf("recording the static next-hop route for %s via %s", dest, req.busID),
				cause:   err,
				applied: changes,
			}
		}
		changes = append(changes, routeChange(rec, req.busID, unchanged))
	}
	return localBusID, changes, nil
}

// ---------------------------------------------------------------------------
// list
// ---------------------------------------------------------------------------

func runPeerList(args []string, stdout, stderr io.Writer) int {
	var (
		dataDir  string
		asJSON   bool
		logLevel string
	)
	fs := flag.NewFlagSet("agent-bus peer list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {}
	fs.StringVar(&dataDir, "data-dir", defaultDataDir, "the bus's data directory")
	fs.BoolVar(&asJSON, "json", false, "emit the configuration as one JSON object on stdout")
	fs.StringVar(&logLevel, "log-level", "warn", "minimum log severity for recovery lines on stderr ("+logging.Levels+")")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, peerUsage)
			return exitPeerOK
		}
		fmt.Fprint(stderr, peerUsage)
		return exitPeerUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprint(stderr, "agent-bus peer list: unexpected argument\n")
		return exitPeerUsage
	}
	lvl, err := logging.ParseLevel(logLevel)
	if err != nil {
		return peerFail(stdout, stderr, asJSON, "list", usagePeerError(fmt.Sprintf("invalid -log-level: %v", err), "use one of "+logging.Levels))
	}
	lg := logging.New(stderr, lvl)

	// READ-ONLY: the store is built with NO durable log, so every mutating call
	// on it fails with relay.ErrPeerNotDurable rather than being possible at
	// all, and the log is read with wal.Replay, which repairs nothing and
	// creates nothing.
	store, closeStore, localBusID, cmdErr := openPeerStore(dataDir, false, lg)
	if cmdErr != nil {
		return peerFail(stdout, stderr, asJSON, "list", cmdErr)
	}
	defer closeStore()

	out := peerListResult{OK: true, BusID: localBusID, Routes: []peerChange{}, Trust: []peerChange{}}
	for _, rec := range store.ActivePeers() {
		out.Routes = append(out.Routes, routeChange(rec, "", false))
	}
	for _, rec := range store.TrustedBuses() {
		out.Trust = append(out.Trust, trustChange(rec, false))
	}
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(out); err != nil {
			fmt.Fprintf(stderr, "agent-bus peer list: writing JSON failed: %v\n", err)
			return exitPeerFailed
		}
		return exitPeerOK
	}
	writePeerListHuman(stdout, out)
	return exitPeerOK
}

// ---------------------------------------------------------------------------
// remove
// ---------------------------------------------------------------------------

func runPeerRemove(args []string, stdout, stderr io.Writer) int {
	const sub = "remove"
	var (
		dataDir   string
		busID     string
		dropRoute bool
		dropTrust bool
		asJSON    bool
		logLevel  string
	)
	fs := flag.NewFlagSet("agent-bus peer remove", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {}
	fs.StringVar(&dataDir, "data-dir", defaultDataDir, "the bus's data directory")
	fs.StringVar(&busID, "bus-id", "", "REQUIRED: the peer bus whose configuration is withdrawn")
	fs.BoolVar(&dropRoute, "route", false, "withdraw the ROUTE for -bus-id")
	fs.BoolVar(&dropTrust, "trust", false, "withdraw the pinned SIGNING KEYS for -bus-id")
	fs.BoolVar(&asJSON, "json", false, "emit the result as one JSON object on stdout")
	fs.StringVar(&logLevel, "log-level", "warn", "minimum log severity for durability lines on stderr ("+logging.Levels+")")

	if perr := fs.Parse(args); perr != nil {
		if errors.Is(perr, flag.ErrHelp) {
			fmt.Fprint(stdout, peerUsage)
			return exitPeerOK
		}
		fmt.Fprint(stderr, peerUsage)
		return exitPeerUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprint(stderr, "agent-bus peer remove: unexpected argument\n")
		return exitPeerUsage
	}
	lvl, err := logging.ParseLevel(logLevel)
	if err != nil {
		return peerFail(stdout, stderr, asJSON, sub, usagePeerError(fmt.Sprintf("invalid -log-level: %v", err), "use one of "+logging.Levels))
	}
	if cmdErr := validatePeerCLIBusID("-bus-id", busID); cmdErr != nil {
		return peerFail(stdout, stderr, asJSON, sub, cmdErr)
	}
	// NEITHER IS IMPLIED BY THE OTHER, and the command will not guess. The two
	// mistakes are not symmetric: withdrawing a route the operator meant to keep
	// breaks federation loudly and is repaired by re-adding it, while leaving a
	// signing key pinned that the operator meant to REVOKE fails silently and
	// looks exactly like a working bus. A default that removed both would make
	// the first mistake easy; a default that removed only the route would make
	// the second. One required word removes the ambiguity.
	if !dropRoute && !dropTrust {
		return peerFail(stdout, stderr, asJSON, sub, usagePeerError(
			"neither -route nor -trust was given, so there is nothing to withdraw",
			"pass -route to withdraw the route, -trust to withdraw the pinned signing keys, or both; they are independent records and removing one never removes the other"))
	}

	lg := logging.New(stderr, lvl)
	localBusID, changes, notFound, cmdErr := applyPeerRemove(dataDir, busID, dropRoute, dropTrust, lg)
	if cmdErr != nil {
		return peerFail(stdout, stderr, asJSON, sub, cmdErr)
	}
	return writePeerResult(stdout, stderr, asJSON, sub, dataDir, localBusID, changes, notFound)
}

// applyPeerRemove withdraws the requested records, leaving TOMBSTONES rather
// than deleting them — that is the store's design, and the tombstone is what
// stops a duplicated older record resurrecting a configuration the operator
// withdrew.
//
// THE ROUTE IS WITHDRAWN FIRST, then trust: the mirror of applyPeerAdd's order
// and for the same reason. A failure between the two must not leave a bus that
// is still routable but no longer verifiable.
func applyPeerRemove(dataDir, busID string, dropRoute, dropTrust bool, lg *logging.Logger) (string, []peerChange, []string, *peerCmdError) {
	var (
		changes  []peerChange
		notFound []string
	)

	store, closeStore, localBusID, cmdErr := openPeerStore(dataDir, true, lg)
	if cmdErr != nil {
		return "", nil, nil, cmdErr
	}
	defer closeStore()

	// A SELF-REFERENCE IS REFUSED HERE TOO, before anything is written, so that
	// `remove` and `add` agree on the exit code for the same mistake. Without it
	// the store's refusal arrives as a generic failure (exit 1) while the
	// documented code for a self-peer is 2.
	if err := relay.ValidatePeerBusID(localBusID, busID); err != nil {
		return localBusID, nil, nil, &peerCmdError{
			code:   exitPeerUsage,
			msg:    fmt.Sprintf("refusing %q", busID),
			remedy: "a bus has no route to and no pin for ITSELF, so there is nothing to withdraw; check -data-dir names the bus you meant",
			cause:  err,
		}
	}

	// AN ABSENT RECORD DOES NOT ABORT THE OTHER WITHDRAWAL. This is the fix for
	// a P1 both gates reproduced, and the case is the one this whole epic exists
	// for: in the A <-> B <-> C line, C pins A's signing key and has NO ROUTE to
	// A at all, so the natural "revoke everything about A" —
	// `peer remove -bus-id busA -route -trust` — hit ErrUnknownPeer on the route
	// and returned, LEAVING THE TRUST ANCHOR PINNED while exiting with the code
	// documented as the benign "there was nothing to remove". That is exactly the
	// fail-silent direction the mandatory -route/-trust design exists to prevent.
	//
	// ErrUnknownPeer is "there was nothing there", not a failure, so it is
	// collected and the next withdrawal still runs. Exit 5 is reported only when
	// NO requested record existed — which is still the typo case, because a
	// mistyped bus id has neither.
	if dropRoute {
		unchanged := routeAlreadyWithdrawn(store, busID)
		rec, err := store.Remove(busID)
		switch {
		case errors.Is(err, relay.ErrUnknownPeer):
			notFound = append(notFound, "route")
		case err != nil:
			return localBusID, nil, notFound, peerRemoveFailure(err, "route", busID, changes)
		default:
			changes = append(changes, routeChange(rec, "", unchanged))
		}
	}
	if dropTrust {
		unchanged := trustAlreadyWithdrawn(store, busID)
		rec, err := store.RemoveTrust(busID)
		switch {
		case errors.Is(err, relay.ErrUnknownPeer):
			notFound = append(notFound, "trust")
		case err != nil:
			return localBusID, nil, notFound, peerRemoveFailure(err, "trust", busID, changes)
		default:
			changes = append(changes, trustChange(rec, unchanged))
		}
	}
	if len(changes) == 0 && len(notFound) > 0 {
		return localBusID, nil, notFound, &peerCmdError{
			code: exitPeerUnknown,
			msg: fmt.Sprintf("this bus holds no %s record for %s, so nothing was withdrawn and nothing was written",
				strings.Join(notFound, " and no "), busID),
			remedy: "run `agent-bus peer list` and check the spelling; a bus id differing only by ASCII case is a DIFFERENT bus",
		}
	}
	return localBusID, changes, notFound, nil
}

// peerRemoveFailure reports a withdrawal that FAILED — never a record that was
// merely absent, which is collected by the caller instead.
func peerRemoveFailure(err error, what, busID string, applied []peerChange) *peerCmdError {
	return &peerCmdError{
		code:    exitPeerFailed,
		msg:     fmt.Sprintf("withdrawing the %s record for %s", what, busID),
		cause:   err,
		applied: applied,
	}
}

// ---------------------------------------------------------------------------
// The store, the lock and the replay precondition
// ---------------------------------------------------------------------------

// openPeerStore locks the data directory, resolves this bus's id WITHOUT ever
// creating one, and returns a peer store whose log HAS ALREADY BEEN REPLAYED.
//
// # The replay precondition is satisfied STRUCTURALLY, not by remembering to
//
// relay.PeerStoreOptions.Durable requires the log to be replayed into the store
// before the first write; an un-replayed store mints config_seq 1 over a log
// that already holds 1..N, and the superseded generation then WINS on the next
// replay. Here:
//
//   - writable=true builds the store around a deferredLog (invite.go) whose
//     Write ERRORS while the log is nil, then opens the WAL with the store as
//     its Applier — wal.Open replays the whole log before it returns — and only
//     then hands the log over. A write before replay is therefore not merely
//     avoided but unreachable, and would fail loudly rather than silently mint 1.
//   - writable=false builds the store with NO durable log at all (every mutating
//     call fails with relay.ErrPeerNotDurable) and rebuilds it with wal.Replay,
//     which is the package's read-only fsck: it repairs nothing, truncates
//     nothing, and creates no file.
//
// The returned close function releases the lock and closes the log; it is safe
// to call exactly once, and the caller should defer it.
func openPeerStore(dataDir string, writable bool, lg *logging.Logger) (*relay.PeerStore, func(), string, *peerCmdError) {
	// The data directory is NOT created, and neither is any part of the bus
	// identity in it. run() does MkdirAll because a server is entitled to start
	// a fresh bus; this command is not, and a typo in -data-dir that minted a
	// whole new bus identity would write federation configuration into a bus
	// that does not exist — while the real bus stayed unconfigured.
	info, err := os.Stat(dataDir)
	if err != nil {
		return nil, nil, "", &peerCmdError{
			code:   exitPeerNoIdentity,
			msg:    fmt.Sprintf("cannot read the data directory %q", dataDir),
			remedy: "check -data-dir; this command never creates one, because a typo would configure a bus that does not exist",
			cause:  err,
		}
	}
	if !info.IsDir() {
		return nil, nil, "", &peerCmdError{
			code:   exitPeerNoIdentity,
			msg:    fmt.Sprintf("-data-dir %q is not a directory", dataDir),
			remedy: "point -data-dir at the bus's data directory",
		}
	}
	// REFUSE a directory with no bus-id file BEFORE TAKING THE LOCK, so that
	// this refusal writes NOTHING AT ALL — including no bus.lock, which
	// dirlock.Acquire creates and which the server reads as "this directory has
	// history" (see invite.go: a lone bus.lock left by a premature mint made the
	// operator's first start refuse to boot).
	//
	// The check is what makes ids.LoadOrCreateBusID's "Create" half unreachable
	// below. That matters here for invite.go's reason: a directory whose bus-id
	// file was lost would otherwise get a FRESHLY MINTED id persisted into it,
	// renaming the bus away from every agent id it has ever issued
	// ("<bus-id>.<agent-id>", invariant 2).
	//
	// UNLIKE `invite mint` THE CERTIFICATE IS NOT REQUIRED, deliberately: peer
	// configuration pins no certificate of ours and puts none in any blob, so
	// demanding one would refuse a legitimate directory for a file this command
	// never reads.
	if cmdErr := checkPeerBusIDPresent(dataDir); cmdErr != nil {
		return nil, nil, "", cmdErr
	}

	lock, err := dirlock.Acquire(dataDir)
	if err != nil {
		if errors.Is(err, dirlock.ErrLocked) {
			return nil, nil, "", &peerCmdError{
				code:   exitPeerBusRunning,
				msg:    "the data directory is locked by a live process, which is almost certainly the bus itself",
				remedy: "stop the bus, run this command, then start it again; peer configuration is offline by design (DECISIONS.md, FEDERATION (e)) and two writers to one log would destroy it",
				cause:  err,
			}
		}
		return nil, nil, "", &peerCmdError{code: exitPeerFailed, msg: "locking the data directory", cause: err}
	}
	releaseLock := func() {
		if err := lock.Release(); err != nil {
			lg.Error("releasing data directory lock failed", "data_dir", dataDir, "err", err)
		}
	}

	// RE-CHECK under the lock. The pre-lock check exists so a refusal writes
	// nothing; this one is what makes "never creates an identity" true rather
	// than merely likely, since the pre-lock check runs while another process
	// could still be deleting bus-id.
	if cmdErr := checkPeerBusIDPresent(dataDir); cmdErr != nil {
		releaseLock()
		return nil, nil, "", cmdErr
	}

	// A LOAD, never a create: the check immediately above, taken under the lock,
	// is the only thing that makes LoadOrCreateBusID's "Create" half unreachable.
	busID, err := ids.LoadOrCreateBusID(dataDir, "")
	if err != nil {
		releaseLock()
		return nil, nil, "", &peerCmdError{code: exitPeerFailed, msg: "resolving the bus id", cause: err}
	}

	walPath := filepath.Join(dataDir, wal.WALFileName)

	if !writable {
		// Dir IS PASSED ON THE READ-ONLY PATH TOO. This store has no durable log
		// and withdraws nothing, but the withdrawal floor (RELAY-34) lives beside
		// the log and is consulted when records are folded in: a store built
		// without Dir over a data directory that HAS a floor file would ignore it
		// and could show a REVOKED pin as still pinned. `peer list` is exactly
		// where an operator checks a revocation took, so it must read the same
		// truth the write path wrote.
		store, err := relay.NewPeerStore(relay.PeerStoreOptions{BusID: busID, Dir: dataDir, Logger: lg})
		if err != nil {
			releaseLock()
			return nil, nil, "", &peerCmdError{code: exitPeerFailed, msg: "creating the peer store", cause: err}
		}
		if _, err := wal.Replay(walPath, store.Apply); err != nil {
			releaseLock()
			return nil, nil, "", &peerCmdError{
				code:   exitPeerFailed,
				msg:    "replaying the write-ahead log",
				remedy: "the bus itself repairs a damaged log at startup and this read-only path deliberately does not; start the bus once and retry",
				cause:  err,
			}
		}
		return store, releaseLock, busID, nil
	}

	// The store is the WAL's applier for this process, so replay rebuilds the
	// tables AND the config_seq high-water mark before any write. deferredLog
	// resolves the cycle — the store needs a durable log at construction and the
	// log needs the store as its applier at Open — the same shape invite.go uses.
	dl := &deferredLog{}
	// Dir IS REQUIRED FOR WITHDRAWALS (RELAY-34): a withdrawal recorded only in
	// the log can be UN-SAID by a discarded tail, and for the trust table that
	// means a revoked pinned bus signing key comes back. Without it the store
	// refuses every `peer remove` — which is the correct refusal, and the reason
	// this line is not optional.
	store, err := relay.NewPeerStore(relay.PeerStoreOptions{BusID: busID, Dir: dataDir, Durable: dl, Logger: lg})
	if err != nil {
		releaseLock()
		return nil, nil, "", &peerCmdError{code: exitPeerFailed, msg: "creating the peer store", cause: err}
	}
	walLog, err := wal.Open(wal.LogOptions{Dir: dataDir, Logger: lg, Applier: store})
	if err != nil {
		releaseLock()
		return nil, nil, "", &peerCmdError{
			code:   exitPeerFailed,
			msg:    "opening the write-ahead log",
			remedy: "the bus itself refuses to start on the same error; fix it there first",
			cause:  err,
		}
	}
	// ONLY NOW is the store able to write anything at all: every write before
	// this line would have failed on deferredLog's nil check, and wal.Open has
	// replayed the whole log into store.Apply before returning.
	dl.log = walLog

	return store, func() {
		if err := walLog.Close(); err != nil {
			lg.Error("closing the write-ahead log failed", "data_dir", dataDir, "path", walLog.Path(), "err", err)
		}
		releaseLock()
	}, busID, nil
}

// checkPeerBusIDPresent reports whether dataDir holds this bus's bus-id file.
//
// It is called TWICE — once before the lock so a refusal writes nothing at all,
// and once after it so the time-of-check/time-of-use window is closed rather
// than argued away.
func checkPeerBusIDPresent(dataDir string) *peerCmdError {
	path := filepath.Join(dataDir, busIDFileName)
	if _, err := os.Stat(path); err != nil {
		return &peerCmdError{
			code: exitPeerNoIdentity,
			msg:  fmt.Sprintf("this data directory holds no bus id file (%q), so this bus has no identity to configure federation for", path),
			remedy: "if this bus has never run, start it once against this -data-dir, stop it, then configure peering; " +
				"if it HAS run, that file has been lost and must be restored from backup — this command will not recreate it, because a regenerated id would rename the bus away from every agent id it has issued. Nothing was written by this refusal",
			cause: err,
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Input validation
// ---------------------------------------------------------------------------

// validatePeerCLIBusID checks a bus id given on the command line.
//
// The LENGTH is checked before ids.ValidateBusID, whose error quotes the id: an
// oversized value must not get to choose the size of the diagnostic we print
// about refusing it. Same discipline as internal/relay.ValidatePeerBusID.
func validatePeerCLIBusID(flagName, busID string) *peerCmdError {
	if busID == "" {
		return usagePeerError(flagName+" is required", "pass the peer's bus id, e.g. "+flagName+" bus-7f3a")
	}
	if len(busID) > relay.MaxPeerBusIDLen {
		return usagePeerError(
			fmt.Sprintf("%s is %d bytes, but a bus id is at most %d; it is not echoed here because it is oversized", flagName, len(busID), relay.MaxPeerBusIDLen),
			"pass the bus id exactly as the peer's own `agent-bus` reports it")
	}
	if err := ids.ValidateBusID(busID); err != nil {
		return &peerCmdError{
			code:   exitPeerUsage,
			msg:    flagName + " is not a valid bus id",
			remedy: "a bus id is minted by the peer's own server (invariant 1); copy it from that bus rather than composing one",
			cause:  err,
		}
	}
	return nil
}

// validatePeerBareOrigin checks -url against the SAME rule the durable record
// enforces: a bare https origin, and nothing else.
//
// It is duplicated rather than imported because relay's own check is unexported
// and belongs to the record. The duplication is self-checking rather than a
// hope: TestPeerAddURLRulesMatchTheDurableRecord runs a table of values through
// BOTH this function and relay.PeerRecord.Encode and fails if they disagree, so
// this can neither drift looser (which would push the refusal to exit 1 after
// the lock) nor stricter (which would refuse a URL the store would have stored).
//
// Note what is deliberately NOT fixed here: internal/relay/client.go's peerURL,
// which actually DIALS a peer, still accepts a path. That is RELAY-36 and
// touches every relay caller; this command's input is strictly tighter, so no
// value it writes can reach that gap.
func validatePeerBareOrigin(raw string) (string, *peerCmdError) {
	trimmed := strings.TrimSpace(raw)
	if len(trimmed) > relay.MaxPeerBaseURLLen {
		return "", usagePeerError(
			fmt.Sprintf("-url is %d bytes, but a peer base URL is at most %d; it is not echoed here because it is oversized", len(trimmed), relay.MaxPeerBaseURLLen),
			"pass a bare origin, e.g. https://b.example:8443")
	}
	remedy := "pass a BARE https origin: scheme, host and optional port, and nothing else — the relay path is appended at every dial, so a stored path, query or fragment would ride along on all of them"
	u, err := url.Parse(trimmed)
	if err != nil {
		return "", usagePeerError("-url is not a URL", remedy)
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return "", usagePeerError(
			"-url must be https: a bus-to-bus link is always TLS, and there is no plaintext listener (invariant 11)",
			remedy)
	}
	if u.Host == "" {
		return "", usagePeerError("-url has no host", remedy)
	}
	if u.Path != "" && u.Path != "/" {
		return "", usagePeerError(fmt.Sprintf("-url carries a path (%d bytes); a stored peer address is a BARE ORIGIN", len(u.Path)), remedy)
	}
	// ForceQuery is checked alongside RawQuery because it is the "https://h?"
	// case: the query is empty but the '?' survives url.String(), so appending
	// the relay path would turn it into the query string and every dial would
	// land on the peer's "/".
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.User != nil || u.Opaque != "" {
		return "", usagePeerError("-url must be a bare origin: no query, no fragment, no userinfo", remedy)
	}
	// Returned WITHOUT re-rendering through url.String(): the string an operator
	// typed is the string that becomes durable, so nothing this command does can
	// silently change the address the bus will dial. A trailing "/" is the one
	// normalisation, because "https://h" and "https://h/" are the same origin
	// and the record refuses neither — storing both would make one peer look
	// like two configurations.
	return strings.TrimSuffix(trimmed, "/"), nil
}

// parsePeerSigningKey decodes one -signing-key value.
//
// The value is a PUBLIC key, so nothing here is secret — but it is a TRUST
// ANCHOR, and every refusal below exists because a malformed one would either
// panic ed25519.Verify (wrong size) or pin something that verifies nothing.
func parsePeerSigningKey(i int, raw string) (ed25519.PublicKey, *peerCmdError) {
	trimmed := strings.TrimSpace(raw)
	if len(trimmed) > maxPeerSigningKeyChars {
		return nil, usagePeerError(
			fmt.Sprintf("-signing-key %d is %d characters, far longer than a base64 Ed25519 public key; it is not echoed here because it is oversized", i+1, len(trimmed)),
			"pass the peer bus's signing key as standard base64 — 44 characters for a 32-byte key")
	}
	key, err := base64.StdEncoding.DecodeString(trimmed)
	if err != nil {
		// The offending text is NOT echoed: its only relevant property here is
		// that it is not base64, and it is argv on its way to a terminal.
		return nil, usagePeerError(
			fmt.Sprintf("-signing-key %d is not standard base64", i+1),
			"copy the peer bus's signing key exactly as that bus reports it; standard base64 with padding, not URL-safe base64")
	}
	if len(key) != ed25519.PublicKeySize {
		return nil, usagePeerError(
			fmt.Sprintf("-signing-key %d decodes to %d bytes, want exactly %d", i+1, len(key), ed25519.PublicKeySize),
			"an Ed25519 public key is 32 bytes; a wrong-size key is refused here because ed25519.Verify PANICS on one")
	}
	return ed25519.PublicKey(key), nil
}

// isAllZeroKey reports whether a key is entirely zero bytes. A public key is
// public, so nothing here is timing-sensitive and nothing should be read as
// implying it is.
func isAllZeroKey(k ed25519.PublicKey) bool {
	return len(k) > 0 && bytes.Equal(k, make([]byte, len(k)))
}

// ---------------------------------------------------------------------------
// Reporting
// ---------------------------------------------------------------------------

// routeAlreadyActive and trustAlreadyPinned answer, BEFORE a write, the same
// question relay.PeerStore asks itself on its no-op path: is this exact
// configuration already active?
//
// They must be asked beforehand. Put and PutTrust return the record that is now
// on disk whether they wrote it or returned the existing one, so the returned
// value cannot distinguish the two — and reporting every re-run of a
// provisioning script as a fresh write would hide the one thing an operator
// wants to know from the second run.
//
// A wrong answer here changes only the wording of a report line: the store
// decides what is written, and this decides nothing.
func routeAlreadyActive(store *relay.PeerStore, busID, wantURL string) bool {
	rec, ok := store.Lookup(busID)
	return ok && rec.State == relay.PeerRecordActive && rec.BaseURL == wantURL
}

func trustAlreadyPinned(store *relay.PeerStore, busID string, want []ed25519.PublicKey) bool {
	rec, ok := store.LookupTrust(busID)
	if !ok || rec.State != relay.PeerRecordActive || len(rec.SigningKeys) != len(want) {
		return false
	}
	// ORDER INCLUDED: the pin set is stored in the operator's order, and two
	// sets differing only in order are two different generations to the store.
	for i := range want {
		if !bytes.Equal(rec.SigningKeys[i], want[i]) {
			return false
		}
	}
	return true
}

// routeAlreadyWithdrawn and trustAlreadyWithdrawn are the same question for a
// removal: the store treats removing an already-removed record as a no-op that
// writes nothing.
func routeAlreadyWithdrawn(store *relay.PeerStore, busID string) bool {
	rec, ok := store.Lookup(busID)
	return ok && rec.State == relay.PeerRecordRemoved
}

func trustAlreadyWithdrawn(store *relay.PeerStore, busID string) bool {
	rec, ok := store.LookupTrust(busID)
	return ok && rec.State == relay.PeerRecordRemoved
}

func routeChange(rec relay.PeerRecord, nextHop string, unchanged bool) peerChange {
	return peerChange{
		Kind:         "route",
		BusID:        rec.BusID,
		State:        rec.State.String(),
		BaseURL:      rec.BaseURL,
		NextHopBusID: nextHop,
		ConfigSeq:    rec.ConfigSeq,
		UpdatedAt:    rec.UpdatedAt.UTC().Format(time.RFC3339Nano),
		Unchanged:    unchanged,
	}
}

func trustChange(rec relay.BusTrustRecord, unchanged bool) peerChange {
	c := peerChange{
		Kind:      "trust",
		BusID:     rec.BusID,
		State:     rec.State.String(),
		ConfigSeq: rec.ConfigSeq,
		UpdatedAt: rec.UpdatedAt.UTC().Format(time.RFC3339Nano),
		Unchanged: unchanged,
	}
	for _, k := range rec.SigningKeys {
		c.SigningKeys = append(c.SigningKeys, base64.StdEncoding.EncodeToString(k))
	}
	return c
}

// writePeerResult emits the success shape for `add` and `remove`.
func writePeerResult(stdout, stderr io.Writer, asJSON bool, sub, dataDir, localBusID string, changes []peerChange, notFound []string) int {
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(peerResult{OK: true, BusID: localBusID, Changes: changes, NotFound: notFound}); err != nil {
			// The records ARE durable at this point; only the report failed. Say
			// so exactly, because "the change failed" would be a lie that sends an
			// operator to re-apply a configuration that is already on disk.
			fmt.Fprintf(stderr, "agent-bus peer %s: the configuration IS durable, but writing the JSON result failed: %v. Run `agent-bus peer list` to see what is on disk.\n", sub, err)
			return exitPeerFailed
		}
		return exitPeerOK
	}
	fmt.Fprintf(stdout, "Peer configuration written to %s (bus %s).\n\n", dataDir, localBusID)
	for _, c := range changes {
		fmt.Fprintf(stdout, "  %s\n", describePeerChange(c))
	}
	for _, what := range notFound {
		// REPORTED, never silent. The other withdrawal succeeded, so this is not
		// a failure — but an operator who asked to withdraw two things and got
		// one must be told which one was not there.
		fmt.Fprintf(stdout, "  %-5s %-24s no such record; nothing to withdraw\n", what, "")
	}
	fmt.Fprint(stdout, "\nThe bus reads this configuration at startup: START IT AGAIN for the change to\n"+
		"take effect. Routes and trust pins are independent records — a route is not a\n"+
		"pin, and withdrawing one never withdraws the other.\n")
	return exitPeerOK
}

// describePeerChange renders one change as a single readable line.
func describePeerChange(c peerChange) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-5s %-24s ", c.Kind, c.BusID)
	switch {
	case c.State != "active":
		b.WriteString("WITHDRAWN")
	case c.Kind == "trust":
		fmt.Fprintf(&b, "%d pinned signing key(s)", len(c.SigningKeys))
		if len(c.SigningKeys) > 1 {
			b.WriteString(" — a ROLLOVER window, not a general accept list")
		}
	default:
		b.WriteString(c.BaseURL)
		if c.NextHopBusID != "" {
			fmt.Fprintf(&b, " (static next hop: %s)", c.NextHopBusID)
		}
	}
	fmt.Fprintf(&b, "  [config_seq %d]", c.ConfigSeq)
	if c.Unchanged {
		if c.State == "active" {
			b.WriteString("  (already configured; nothing written)")
		} else {
			b.WriteString("  (already withdrawn; nothing written)")
		}
	}
	return b.String()
}

// writePeerListHuman is `peer list`'s default, readable output (invariant 7's
// first audience). It leads with the split that matters — routes and trust are
// different things — rather than with a merged table that would imply they
// travel together.
func writePeerListHuman(w io.Writer, out peerListResult) {
	fmt.Fprintf(w, "Federation configuration for bus %s.\n\n", out.BusID)

	fmt.Fprint(w, "ROUTES (where traffic for a bus is sent)\n")
	if len(out.Routes) == 0 {
		fmt.Fprint(w, "  (none)\n")
	}
	for _, c := range out.Routes {
		fmt.Fprintf(w, "  %-24s %s  [config_seq %d]\n", c.BusID, c.BaseURL, c.ConfigSeq)
	}

	fmt.Fprint(w, "\nTRUST (bus signing keys pinned for a bus — it need not have a route)\n")
	if len(out.Trust) == 0 {
		fmt.Fprint(w, "  (none)\n")
	}
	for _, c := range out.Trust {
		fmt.Fprintf(w, "  %-24s %d key(s)  [config_seq %d]\n", c.BusID, len(c.SigningKeys), c.ConfigSeq)
		for _, k := range c.SigningKeys {
			fmt.Fprintf(w, "    %s\n", k)
		}
	}

	fmt.Fprint(w, "\nA route's address is the NEXT HOP for that destination, which for a bus reached\n"+
		"through another is the intermediate bus's address — the record stores the\n"+
		"address, not the via-bus, so this listing cannot show which peer carries it.\n"+
		"Withdrawn entries are not listed.\n")
}
