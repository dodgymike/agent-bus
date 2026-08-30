// Command agent-bus runs the agent-bus server: a small, durable inter-agent
// message bus that Claude Code agents enrol with, long-poll, and send through.
//
// Flags are parsed into a Config, validated up front, and handed to
// internal/httpapi. The process serves until SIGINT or SIGTERM, then cancels
// the server-lifetime context (releasing any parked long-polls) and drains
// in-flight requests within a bounded grace period.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dodgymike/agent-bus/internal/ack"
	"github.com/dodgymike/agent-bus/internal/auth"
	"github.com/dodgymike/agent-bus/internal/dirlock"
	"github.com/dodgymike/agent-bus/internal/httpapi"
	"github.com/dodgymike/agent-bus/internal/hub"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/invite"
	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/relay"
	"github.com/dodgymike/agent-bus/internal/store"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// version is the reported build version. Override at build time with
// -ldflags "-X main.version=$(git describe --tags --always)".
var version = "dev"

// Defaults for the command-line flags.
const (
	defaultListen      = "127.0.0.1:8080"
	defaultDataDir     = "./data"
	defaultPollTimeout = 30 * time.Second
	defaultLogLevel    = "info"

	// defaultAuthRateLimitPerSecond and defaultAuthRateLimitBurst configure the
	// per-source token bucket in front of the three unauthenticated credential
	// routes (AUTH-1-FU-RATELIMIT). They cap what ONE source can do to
	// /v1/enroll, /v1/session/begin and /v1/session/complete.
	//
	// The numbers: security measured ~137 req/s from a single source as enough
	// to sustain the session-table denial. 5 req/s sustained is ~27x below that,
	// so a lone source can no longer keep MaxSessions (16384) or MaxRosterEntries
	// (4096) exhausted; a burst of 60 absorbs a legitimate cluster of agents
	// bootstrapping at once from ONE address -- enrol + session/begin +
	// session/complete is 3 requests per agent, so 60 covers ~20 simultaneous
	// agents behind a shared NAT before any of them is throttled. Both are
	// operator-tunable (-auth-rate-limit, -auth-rate-burst); set the burst to 0
	// to disable.
	defaultAuthRateLimitPerSecond = 5.0
	defaultAuthRateLimitBurst     = 60

	// enrolmentInviteRequired turns on INVITE-ONLY ENROLMENT for the shipped
	// server (invariant 3): an enrolment presenting no invite is refused 403.
	//
	// # It is a CONSTANT, and it is used TWICE on purpose
	//
	// It is passed to auth.Options.RequireInvite AND logged as
	// enrolment_invite_required in the startup line, so the behaviour and the
	// announcement of the behaviour cannot disagree. They disagreed before: the
	// startup line hard-coded `false` beside a service that hard-coded nothing at
	// all, and stayed false-by-construction after the gate was designed. One
	// symbol makes that a compile-time impossibility rather than a review item.
	//
	// It is deliberately NOT a flag. An operator-settable "-require-invite=false"
	// would be a documented way to reopen the anonymous enrolment route that
	// permanently exhausts the roster (4096 slots, never reclaimed, ids never
	// reused per invariant 1) -- the exact P0 this closes. Invariant 3 says
	// invites are the ONLY way onto the bus; a flag that switches an invariant
	// off is not a configuration option, it is a vulnerability with a help
	// string. A deployment that genuinely wants open enrolment can build its own
	// server from the packages, which is the honest way to own that decision.
	enrolmentInviteRequired = true

	// shutdownGrace bounds the drain of in-flight requests after the
	// server-lifetime context has been cancelled.
	//
	// # THE CONTRACT A POLL HANDLER MUST HONOUR (CORE-11)
	//
	// shutdownGrace (10s) is SHORTER than defaultPollTimeout (30s), so a parked
	// long-poll OUTLIVES the graceful-shutdown window. Read that again: the grace
	// period cannot, on its own, drain a poll that has just parked.
	//
	// What makes shutdown work anyway is the ORDERING in waitAndShutdown:
	// cancelRoot() fires FIRST, and rootCtx is the parent of every request
	// context via http.Server.BaseContext, so cancelling it cancels the requests
	// and the handlers return before Shutdown starts counting. The grace period
	// then only has to cover handlers already on their way out.
	//
	// So the requirement, which is a CONTRACT and not an implementation detail:
	//
	//	A POLL HANDLER MUST select on ctx.Done() (r.Context()) ALONGSIDE its own
	//	timeout. It MUST NOT block on a bare time.After(pollTimeout).
	//
	// A handler that ignores ctx.Done() hangs for its full poll timeout, blows
	// through shutdownGrace, and is killed mid-response -- the client sees a
	// truncated reply on shutdown rather than a clean empty poll. Nothing in the
	// type system enforces this, which is exactly why it is written down: the
	// safety here is a property of two numbers and one statement order, and it
	// was previously true only by accident.
	//
	// # Why the numbers were NOT simply reordered
	//
	// The obvious alternative is to raise shutdownGrace above defaultPollTimeout.
	// It was not chosen. -poll-timeout is OPERATOR-CONFIGURABLE, so any constant
	// here can be exceeded by a flag and the ordering would be a coincidence
	// again; and it would make every shutdown wait for the slowest poll, turning
	// a 10s drain into a 30s+ one on a bus whose polls are idle by definition.
	// Cancelling the contexts is both faster and correct for any poll timeout.
	shutdownGrace = 10 * time.Second

	// readHeaderTimeout bounds how long a client may take to send its request
	// headers; it deliberately does NOT bound the whole request, because
	// long-polls are meant to be slow.
	readHeaderTimeout = 15 * time.Second

	// idleTimeout bounds how long an idle KEEP-ALIVE connection is held open
	// between requests (CORE-9).
	//
	// It bounds a connection that is doing NOTHING, which is why it is safe here
	// while a read or write timeout is not: a parked long-poll is an in-flight
	// request, not an idle connection, so this timer is not running during one.
	//
	// The value is comfortably above the default long-poll (defaultPollTimeout,
	// 30s) so that an agent looping on /v1/wait re-uses its connection instead of
	// re-handshaking TLS on every poll. That is a real cost on this bus: every
	// connection is TLS and there is no plaintext listener (invariant 11).
	//
	// Deliberately NOT justified by mutual TLS. ClientAuth is
	// tls.RequestClientCert (cmd/agent-bus/tlslisten.go, MTLS-CLIENTAUTH,
	// a97f854): a certificate is REQUESTED and never REQUIRED, and one that IS
	// presented authenticates nobody by itself -- admitClientCertificate does no
	// chain verification and resolves no principal. So a comment resting on
	// client certificates would still assert an authentication property this
	// server does not have, in a place a later auditor would trust. Requiring
	// one would make the handshake more expensive, which would strengthen this
	// value rather than change it.
	//
	// What this does NOT bound: an ACTIVE attacker. A client that sends a
	// trivial request more often than every 120s refreshes the timer for ever,
	// so this is a bound on ABANDONED keep-alives, not a connection cap. There
	// is no concurrent-connection limit; the loopback listen default is what
	// bounds who can reach the port.
	idleTimeout = 120 * time.Second

	// maxHeaderBytes bounds the header memory a single connection can make the
	// server allocate before any handler runs (CORE-9). net/http's own default
	// is 1 MB; 64 KB is far more than any legitimate request to this bus needs,
	// and every credential that travels in a header here -- the session token --
	// is a short opaque handle.
	//
	// This bounds HEADERS ONLY. Request BODIES are bounded separately and
	// per-handler with http.MaxBytesReader inside internal/httpapi's JSON-decode
	// helper, because the right body limit differs per route and a single number
	// here could not express it.
	maxHeaderBytes = 64 << 10
)

// Config is the fully validated runtime configuration.
type Config struct {
	// Listen is the TCP address the HTTP server binds, e.g. "127.0.0.1:8080"
	// (default, loopback-only) or ":8080" (all interfaces).
	Listen string
	// DataDir is the directory holding the durable store and the append-only
	// log. Created if missing.
	DataDir string
	// PollTimeout is the ceiling on a single long-poll wait.
	PollTimeout time.Duration
	// LogLevel is the parsed minimum log severity.
	LogLevel logging.Level
	// BusID is the TEST-ONLY bus id override; empty means "server decides".
	//
	// Invariant 1: the SERVER is authoritative on every id. An operator- or
	// client-supplied bus id is input to be validated, never an identity to be
	// trusted. This field exists solely so tests get a deterministic bus id;
	// it is not a supported production knob. The real id is minted and
	// persisted by internal/ids (ids.LoadOrCreateBusID); this override only
	// seeds that on a first start with an empty -data-dir, or must match what
	// is already persisted there.
	BusID string
	// AuthRateLimitPerSecond is the sustained per-source refill rate, in requests
	// per second, of the token bucket in front of the three unauthenticated
	// credential routes (AUTH-1-FU-RATELIMIT). AuthRateLimitBurst is that
	// bucket's capacity. A burst <= 0 DISABLES the limiter. See httpapi.AuthRateLimit.
	AuthRateLimitPerSecond float64
	AuthRateLimitBurst     int
	// BackfillSuffixFloors is the operator's ONE-TIME opt-in permitting the
	// agent-id suffix floors to be BACKFILLED from the durable log on a data
	// directory that has history but no `agent-suffixes` file.
	//
	// Default false, and that default is the safety property: without it, that
	// directory is REFUSED at startup rather than resumed from suffix 1, because
	// the floors file is the only authoritative witness of what ids the directory
	// has issued and a derivation from the log is structurally a lower bound (see
	// openSuffixAllocator). Never needed in normal operation.
	BackfillSuffixFloors bool
}

func main() {
	// Subcommand dispatch, BEFORE flag parsing, and deliberately the smallest
	// form of it that works: exactly one intercepted word. Everything else
	// reaches parseFlags unchanged, so `agent-bus -listen …` behaves exactly as
	// it always has. This costs nothing today because parseFlags already refuses
	// any non-flag argument ("unexpected argument"), so no invocation that used
	// to work is being redirected.
	//
	// `agent-bus invite mint` is the operator's invite-minting surface, and it is
	// a subcommand on the SERVER binary rather than an HTTP route because the
	// minting authority is FILESYSTEM ACCESS to the data directory (DECISIONS.md
	// E4). See cmd/agent-bus/invite.go.
	if len(os.Args) > 1 && os.Args[1] == inviteCommandName {
		os.Exit(runInviteCommand(os.Args[2:], os.Stdout, os.Stderr))
	}

	// `agent-bus healthcheck` is the liveness probe for a TLS-ONLY bus
	// (MTLS-VERIFY). It is on the SERVER binary for the same reason `invite` is
	// -- its input is filesystem access to the data directory, not a network
	// privilege -- and because the runtime image ships this binary and no HTTP
	// client that can be told to trust one self-signed certificate. See
	// cmd/agent-bus/healthcheck.go. It takes no lock and writes nothing, so
	// unlike `invite mint` it is meant to run against a RUNNING bus.
	if len(os.Args) > 1 && os.Args[1] == healthcheckCommandName {
		os.Exit(runHealthcheckCommand(os.Args[2:], os.Stdout, os.Stderr))
	}

	// `agent-bus peer add|list|remove` is the operator's federation-configuration
	// surface (RELAY-12). It is a subcommand for `invite mint`'s reason, recorded
	// in DECISIONS.md FEDERATION (e): peer configuration is offline, under the
	// dirlock, and adds NO online admin route and no new privilege tier. See
	// cmd/agent-bus/peer.go.
	if len(os.Args) > 1 && os.Args[1] == peerCommandName {
		os.Exit(runPeerCommand(os.Args[2:], os.Stdout, os.Stderr))
	}

	// `agent-bus key export-public` prints this bus's SIGNING PUBLIC KEY, the
	// value a peer bus pins with `peer add -signing-key` (CLI-11). It is on this
	// binary for `invite mint`'s and `peer add`'s reason -- its input is
	// filesystem access to the data directory, not a network privilege -- and it
	// takes the same exclusive lock, so like those two it needs the bus STOPPED.
	// It exports the PUBLIC half only and never creates key material; see
	// cmd/agent-bus/key.go.
	if len(os.Args) > 1 && os.Args[1] == keyCommandName {
		os.Exit(runKeyCommand(os.Args[2:], os.Stdout, os.Stderr))
	}

	// `agent-bus log` prints the append-only MESSAGE AUDIT TRAIL (CLI-6):
	// METADATA AND ROUTING ONLY — message id, sequence, sender, recipients, the
	// ordered bus path, timestamp, size and content hash. It NEVER prints bodies,
	// because the trail does not contain them (invariant 6). It is on this binary
	// for `invite mint`'s reason -- its input is filesystem access to the data
	// directory, not a network privilege -- and it takes the same exclusive lock,
	// so like `peer` and `key` it needs the bus STOPPED. It writes nothing and
	// repairs nothing; see cmd/agent-bus/auditlog.go.
	if len(os.Args) > 1 && os.Args[1] == logCommandName {
		os.Exit(runLogCommand(os.Args[2:], os.Stdout, os.Stderr))
	}

	// `agent-bus operator keygen|add|list|revoke` is the OPERATOR/ADMIN PRINCIPAL
	// surface (AUTH-10). It is on this binary for `invite mint`'s reason -- the
	// authority behind it is FILESYSTEM ACCESS to the data directory, not a
	// network privilege -- and it takes the same exclusive lock, so like `peer`,
	// `key` and `log` it needs the bus STOPPED. See cmd/agent-bus/operator.go.
	//
	// THIS LINE IS WHY THE SUBCOMMAND EXISTS AT ALL (AUTH-10-WIRING). Until it
	// landed, `agent-bus operator …` fell through to parseFlags and was refused
	// as an unexpected argument, so `operator revoke` -- the ONLY mechanism in
	// the design for taking an operator's authority away -- could not be invoked
	// by anyone. A capability with no reachable subcommand is not a capability
	// (invariant 7).
	if len(os.Args) > 1 && os.Args[1] == operatorCommandName {
		os.Exit(runOperatorCommand(os.Args[2:], os.Stdout, os.Stderr))
	}

	// `agent-bus outbox` is the RELAY DELIVERY DRAIN GATE (RELAY-54): the
	// read-only view of the durable outbox, so an operator can answer "is
	// anything still owed to a peer, and has anything been abandoned?" BEFORE
	// restarting a bus. Until it landed, an abandoned outbox job was invisible to
	// every subcommand -- `log` reads bus.audit, which is a different artefact --
	// so the drain a rollout needs could not be verified at all, which is why
	// RELAY-51 rejected drain-and-restart as a rollout order. A capability with
	// no reachable subcommand is not a capability (invariant 7).
	//
	// It is on this binary for `invite mint`'s reason -- the authority behind it
	// is FILESYSTEM ACCESS to the data directory, not a network privilege -- and
	// it takes the same exclusive lock, so like `peer`, `key`, `log` and
	// `operator` it needs the bus STOPPED. That does NOT defeat the gate: the
	// stop is the restart's first half anyway. See cmd/agent-bus/outbox.go.
	if len(os.Args) > 1 && os.Args[1] == outboxCommandName {
		os.Exit(runOutboxCommand(os.Args[2:], os.Stdout, os.Stderr))
	}

	cfg, err := parseFlags(os.Args[0], os.Args[1:], os.Stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		fmt.Fprintf(os.Stderr, "agent-bus: %v\n", err)
		os.Exit(2)
	}
	if err := run(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "agent-bus: %v\n", err)
		os.Exit(1)
	}
}

// parseFlags parses argv into a validated Config. It never calls os.Exit, so
// it stays testable; flag.ErrHelp is returned for -h.
func parseFlags(prog string, args []string, out io.Writer) (Config, error) {
	var (
		cfg      Config
		logLevel string
	)

	fs := flag.NewFlagSet(prog, flag.ContinueOnError)
	fs.SetOutput(out)
	// The subcommand is announced in -h, because an operator who cannot find it
	// has no way to bootstrap invite-only enrolment and nothing else in the
	// binary hints that a subcommand exists at all. Printed AFTER the flags so
	// the server's own usage stays the first thing read.
	fs.Usage = func() {
		fmt.Fprintf(out, "Usage of %s:\n", prog)
		fs.PrintDefaults()
		fmt.Fprintf(out, "\nSubcommands:\n  %s %s mint    mint a single-use, expiring invite (requires the bus to be STOPPED)\n"+
			"                        run `%s %s mint -h` for details\n", prog, inviteCommandName, prog, inviteCommandName)
		fmt.Fprintf(out, "  %s %s    probe GET /healthz over TLS, trusting only this data dir's certificate\n"+
			"                        (safe against a RUNNING bus; run `%s %s -h` for details)\n", prog, healthcheckCommandName, prog, healthcheckCommandName)
		fmt.Fprintf(out, "  %s %s add|list|remove\n"+
			"                        configure federation: peer routes and pinned bus signing keys\n"+
			"                        (requires the bus to be STOPPED; run `%s %s -h` for details)\n", prog, peerCommandName, prog, peerCommandName)
		fmt.Fprintf(out, "  %s %s export-public\n"+
			"                        print this bus's SIGNING PUBLIC KEY, the value a peer pins\n"+
			"                        (requires the bus to be STOPPED; run `%s %s export-public -h` for details)\n", prog, keyCommandName, prog, keyCommandName)
		fmt.Fprintf(out, "  %s %s\n"+
			"                        read the append-only message audit trail: metadata only —\n"+
			"                        routing and provenance, never message bodies\n"+
			"                        (requires the bus to be STOPPED; run `%s %s -h` for details)\n", prog, logCommandName, prog, logCommandName)
		fmt.Fprintf(out, "  %s %s keygen|add|list|revoke\n"+
			"                        manage OPERATOR principals: the admin identity an agent\n"+
			"                        credential can never satisfy. `%s %s revoke` is the only\n"+
			"                        way to take an operator's authority away\n"+
			"                        (requires the bus to be STOPPED; run `%s %s -h` for details)\n", prog, operatorCommandName, prog, operatorCommandName, prog, operatorCommandName)
		fmt.Fprintf(out, "  %s %s\n"+
			"                        the RELAY DRAIN GATE: what this bus still owes its peers,\n"+
			"                        and what it has ABANDONED. The exit code IS the answer --\n"+
			"                        6 means jobs are still pending, so do not restart yet\n"+
			"                        (requires the bus to be STOPPED; run `%s %s -h` for details)\n", prog, outboxCommandName, prog, outboxCommandName)
	}
	fs.StringVar(&cfg.Listen, "listen", defaultListen, "TCP address to listen on, e.g. \"127.0.0.1:8080\" (default, loopback-only) or \":8080\" (all interfaces)")
	fs.StringVar(&cfg.DataDir, "data-dir", defaultDataDir, "directory holding the durable store and the append-only log")
	fs.DurationVar(&cfg.PollTimeout, "poll-timeout", defaultPollTimeout, "maximum time a long-poll waits before returning empty, e.g. 30s")
	fs.Float64Var(&cfg.AuthRateLimitPerSecond, "auth-rate-limit", defaultAuthRateLimitPerSecond, "sustained per-source request rate for the unauthenticated credential routes (/v1/enroll, /v1/session/begin, /v1/session/complete); a throttled source gets 429 + Retry-After, never a dropped connection")
	fs.IntVar(&cfg.AuthRateLimitBurst, "auth-rate-burst", defaultAuthRateLimitBurst, "per-source burst capacity for the unauthenticated credential routes; set to 0 to DISABLE per-source rate limiting entirely")
	fs.StringVar(&logLevel, "log-level", defaultLogLevel, "minimum log severity ("+logging.Levels+")")
	fs.StringVar(&cfg.BusID, "bus-id", "", "TEST-ONLY: force the bus id. The server is authoritative on every id (invariant 1); a supplied bus id is not a trusted identity. Never use this in production")
	// NO BACKQUOTES in this usage string, deliberately: flag.UnquoteUsage reads
	// the first backquoted word as the flag's VALUE PLACEHOLDER and strips the
	// quotes, so "`agent-suffixes`" would render -h as though this boolean took
	// an agent-suffixes argument.
	fs.BoolVar(&cfg.BackfillSuffixFloors, "backfill-suffix-floors", false, "one-time operator opt-in permitting the agent-id suffix floors to be BACKFILLED from the durable log on a data directory that has history but no agent-suffixes file; never needed in normal operation")

	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	if fs.NArg() > 0 {
		return Config{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	lvl, err := logging.ParseLevel(logLevel)
	if err != nil {
		return Config{}, fmt.Errorf("invalid -log-level: %w", err)
	}
	cfg.LogLevel = lvl

	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// validate fails fast on a configuration that would otherwise leave the server
// half-configured.
func (c Config) validate() error {
	if c.Listen == "" {
		return errors.New("-listen must not be empty")
	}
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return fmt.Errorf("invalid -listen %q: %w", c.Listen, err)
	}
	if c.DataDir == "" {
		return errors.New("-data-dir must not be empty")
	}
	if c.PollTimeout <= 0 {
		return fmt.Errorf("-poll-timeout must be positive, got %s", c.PollTimeout)
	}
	// A positive burst with a non-positive rate is a bucket that can hold tokens
	// but never refills them: it would answer 429 to every request after the
	// first burst, forever, which locks enrolment out entirely. Reject it at
	// flag-parse time rather than shipping a bus nobody can join. A burst <= 0
	// is the documented "disabled" state and needs no rate.
	if c.AuthRateLimitBurst > 0 && c.AuthRateLimitPerSecond <= 0 {
		return fmt.Errorf("-auth-rate-limit must be positive when -auth-rate-burst is positive (got rate %g, burst %d); set -auth-rate-burst 0 to disable rate limiting", c.AuthRateLimitPerSecond, c.AuthRateLimitBurst)
	}
	// Invariant 1: a supplied bus id is input to be VALIDATED, fail fast at
	// flag-parse time rather than deep inside run(). ids.ValidateBusID is the
	// ONE definition of a legal bus id, shared with ids.LoadOrCreateBusID.
	// The real (non-override) bus id comes from the data dir, so there is no
	// placeholder left to validate here when -bus-id is unset.
	if c.BusID != "" {
		if err := ids.ValidateBusID(c.BusID); err != nil {
			return fmt.Errorf("invalid -bus-id: %w", err)
		}
	}
	return nil
}

func run(cfg Config) error {
	lg := logging.New(os.Stderr, cfg.LogLevel)

	// The data dir is created here so a bad path fails at startup rather than
	// on the first durable write. 0o700: the store holds agent credentials.
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return fmt.Errorf("preparing -data-dir %q: %w", cfg.DataDir, err)
	}

	// ...and MkdirAll does NOTHING to a directory that already exists: no
	// chmod, no check, no warning. So the 0o700 above is a statement about
	// directories WE create and nothing else, and a pre-created 0777 data
	// directory sailed straight through it until this call was added.
	//
	// It runs HERE, before dirIsEmpty reads the directory and before
	// dirlock.Acquire writes into it, because every step below trusts that the
	// files in this directory cannot be substituted by another local user.
	// Checking later would also mean a refusal that has already written key
	// material into a world-writable directory, which no refusal can take back.
	if err := enforceDataDirPermissions(cfg.DataDir, lg); err != nil {
		return err
	}

	// Was this data dir EMPTY when the process started? Read here and nowhere
	// else: this is the last instant at which the answer is knowable, because
	// dirlock.Acquire below writes bus.lock and everything after it writes more.
	//
	// It exists for exactly one consumer, openSuffixAllocator, and it is the
	// difference between two cases that are otherwise indistinguishable and want
	// opposite OUTCOMES: a genuinely first start (no floors file, and that is
	// expected -- proceed, at WARN) versus a data dir whose floors file has been
	// LOST (no floors file, and agent ids this bus already issued would now be
	// re-minted -- REFUSE, unless -backfill-suffix-floors says otherwise). The
	// obvious discriminator -- "does the log hold records" -- is NOT sufficient
	// today and was measured to be: enrolment is still memory-only, so a bus can
	// issue alpha-1 and alpha-2, be restarted with agent-suffixes deleted, and
	// still present a COMPLETELY EMPTY log. Emptiness of the directory catches
	// that; the record count does not.
	dataDirWasEmpty, err := dirIsEmpty(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("inspecting -data-dir %q: %w", cfg.DataDir, err)
	}

	// Take the exclusive lock on the data dir BEFORE anything reads or writes
	// it -- before the bus id is loaded, and so before any future WAL replay,
	// which is the whole point: replay must happen inside the lock. Two servers
	// on one data directory both replay the same bytes, both agree, and then
	// both append at the same offsets, destroying the log; the agreement check
	// in internal/wal/log.go explicitly says it "IS NOT A LOCK" and names this
	// as the follow-up. Non-blocking: a second server fails fast and loudly
	// here rather than blocking or, worse, proceeding.
	//
	// The lock is advisory and held for the process's lifetime. It is released
	// by the kernel if we die, so a crash leaves a lock FILE but no LOCK and the
	// next start just works; Release never unlinks the file, because unlinking
	// races two starters into two holders (see internal/dirlock).
	lock, lockErr := dirlock.Acquire(cfg.DataDir)
	if err := lockErr; err != nil {
		// No %q of the dir here: every dirlock error already names it, and
		// repeating the path twice in one line makes the refusal harder to read,
		// not clearer.
		return fmt.Errorf("locking the data directory: %w", err)
	}
	lg.Debug("data directory locked", "data_dir", cfg.DataDir, "lock_file", lock.Path())
	defer func() {
		if err := lock.Release(); err != nil {
			lg.Error("releasing data directory lock failed", "data_dir", cfg.DataDir, "err", err)
			return
		}
		lg.Debug("data directory lock released", "data_dir", cfg.DataDir)
	}()

	// The server mints and persists its own bus id on first start, and loads
	// the identical id on every subsequent start (invariant 1). -bus-id only
	// seeds that on an empty data dir, or must match what is already there.
	busID, err := ids.LoadOrCreateBusID(cfg.DataDir, cfg.BusID)
	if err != nil {
		return fmt.Errorf("resolving bus id: %w", err)
	}
	if cfg.BusID != "" {
		// Runtime signal, not just a doc comment: the server is authoritative
		// on every id (invariant 1), so an operator forcing one is a test-only
		// configuration that should never be running in production.
		lg.Warn("TEST-ONLY -bus-id override in use; the server is authoritative on ids, do not use this in production", "bus_id", busID)
	}

	// The bus's own cryptographic identity: the self-signed bus certificate, the
	// key inside it, and the SEPARATE signing key (MTLS-BUSCERT). Generated only
	// on a data directory holding none of the three, loaded on every start after
	// that, and FATAL on anything else -- see cmd/agent-bus/buscert.go, which also
	// explains why this sits after the lock and the bus id but before wal.Open.
	//
	// THIS IS WHAT THE LISTENER SERVES (MTLS-LISTENER). The certificate loaded
	// here is presented on every handshake below; there is no plaintext listener
	// and no flag that makes one (invariant 11). So a failure here is not "the
	// bus starts without TLS", it is "the bus does not start" -- which is the
	// whole point of the refusal, and why every error on this path is FATAL.
	busMaterial, err := openBusCertMaterial(cfg.DataDir, busID, cfg.Listen, lg)
	if err != nil {
		return fmt.Errorf("preparing the bus certificate and key material in %q: %w", cfg.DataDir, err)
	}

	// Open the durable write path, and REPLAY it, here: strictly after the
	// exclusive lock above and strictly before the server begins serving.
	//
	// After the lock, because replay reads and RepairTail may truncate the very
	// bytes a second server would be appending to; opening the log first and
	// locking second would defeat the lock entirely. Nothing may move
	// dirlock.Acquire later for the same reason: a start refused AT THE LOCK has
	// then touched nothing in the data dir beyond bus.lock, which is exactly what
	// TestRunRefusesALockedDataDir asserts. (A start refused LATER -- here, say,
	// on an unreplayable log -- has legitimately written <data-dir>/bus-id
	// already, because ids.LoadOrCreateBusID runs above. That is fine; it is the
	// lock-refusal case that must stay clean.)
	//
	// Before the server serves, because wal.Open does not return until the log
	// has been replayed, and every path that can answer a request -- srv.Serve
	// below -- is started after this call returns. That is what guarantees no
	// request is ever served from an unreplayed store (invariant 5: disk is the
	// truth, memory is only the serving copy). Note the guarantee is about
	// SERVING, not about binding: nothing here promises the socket is unbound
	// during replay, only that nothing answers on it.
	//
	// THE APPLIER IS A MULTIPLEXER OVER THE ENROLMENT ROSTER AND THE INVITE
	// TABLE (INVITE-GATE), and the three-step order below is fixed (see
	// auth.WALRoster's type doc, which spells it out):
	//
	//	1. build the roster AND the invite store  -- they are the appliers, so they
	//	                                             must exist before the log
	//	2. wal.Open(Applier: multiplexer)         -- replay REBUILDS both, inside Open
	//	3. Attach(log) on each                    -- only now may either accept a
	//	                                             live write
	//
	// Step 2 is what makes enrolment and invite state survive a restart (AUTH-7
	// for the roster, INVITE-STORE for the invites), and step 3 is what makes a
	// LIVE write durable: wal.Log.Write hands every committed entry to the log's
	// applier after the commit fsync, so these appliers must be reachable from
	// THAT applier or a Put would write to disk and never reach the serving copy.
	// WALRoster.Put checks for exactly that mis-wiring and refuses.
	//
	// The multiplexer is auth.MultiplexApplier: it dispatches by wal.Entry.Kind
	// and EXPANDS a composite "agent+invite" entry -- the one an INVITED
	// enrolment writes -- into its enrolment half and its invite half, so an
	// enrolment and the invite it spent are ONE transaction. It is NOT
	// internal/wal's MultiApplier (that is the checkpoint dispatcher, which is
	// deliberately not wired here) and, unlike it, it stays SILENT about kinds it
	// does not own -- which is what keeps the message and seqfloor records below
	// from being treated as damage.
	//
	// The hub is deliberately NOT an applier here. Its Apply is safe on the
	// replay path only -- it inserts applied-key records through idem.Recover,
	// which skips the per-agent fair share because a record on disk proves
	// admission already succeeded -- so registering it for live commits would
	// make publish's own admission control dead code. It is given a read-only
	// replay pass of its own below instead. See (*hub.Hub).Apply.
	authRoster := auth.NewWALRoster(lg)
	// The OPERATOR/ADMIN PRINCIPAL registry (AUTH-10), built here for the reason
	// every applier above and below it is: it must exist BEFORE wal.Open, because
	// replay is what rebuilds it, and it is handed its log in the third step.
	// Until then every mutating call refuses -- which is correct, because the
	// server never mutates it at all: `agent-bus operator add|revoke` is an
	// OFFLINE operator action taken under the data directory's exclusive lock
	// (DECISIONS.md E4's reasoning, as for `invite mint` and `peer add`). The
	// server is a READER of this plane.
	//
	// IT IS ATTACHED BELOW, unlike the peer store, which is deliberately left
	// with no durable log for this registry's exact "offline operator action"
	// reason. BE PRECISE ABOUT WHY, because the intuitive justification is
	// backwards: an UNATTACHED registry does NOT silently succeed in memory --
	// auth.OperatorRegistry.Add and .Revoke both refuse with ErrNotAttached.
	// Unattached is the FAIL-CLOSED state. Attaching is therefore the LESS
	// restrictive choice, not the safer one, and it is taken because
	// auth.OperatorRegistry's type doc prescribes the three-step order as its
	// contract, not because it closes a hole.
	//
	// DO NOT CREDIT OperatorRegistry.write's serving-copy check with more than it
	// does: it reads back AFTER the durable commit, so it DETECTS a registry that
	// is attached but missing from the applier map -- exactly the AUTH-10-WIRING
	// defect -- and cannot PREVENT the write it detects. Its own error says so
	// ("committed durably but the serving registry does not reflect it"). A
	// post-commit detector, not a pre-write guard.
	//
	// WHAT MAKES IT SAFE IS THE THING TO PRESERVE: this registry is handed to NO
	// COMPONENT. run() holds the concrete pointer, but only to call Attach, Len
	// and LiveLen on it; the sole reference that ESCAPES run() is the wal.Applier
	// in the map below, an interface exposing Apply alone. So nothing that serves
	// traffic can reach Add or Revoke -- not because the interface forbids it,
	// but because no component was given the registry to call them on -- and
	// admitting an operator still means stopping the bus. THE MOMENT A CONSUMER
	// TAKES THIS REGISTRY (AUTH-7, INVMINT, CONV-AUTHZ-ADMIN), it finds an armed
	// write path with the ErrNotAttached guard already spent; that consumer owes
	// its own authorisation check and must not read this Attach as one.
	//
	// REGISTERING IT IS THE WHOLE POINT OF AUTH-10-WIRING. auth.MultiplexApplier
	// is SILENT about kinds it does not own -- which is what keeps the "message"
	// and "seqfloor" records of a NEIGHBOURING plane from being read as damage --
	// so until auth.OperatorRecordKind appeared in the map below, every operator
	// record in this log was passed over at replay WITHOUT A WORD. That is the
	// silent discard invariant 6 rates as the defect, and it is worse in one
	// direction than the other: a skipped `add` merely fails closed, but a
	// skipped REVOCATION leaves a revoked operator LIVE, which is fail-OPEN.
	operatorRegistry := auth.NewOperatorRegistry(lg)
	// Built here, BEFORE wal.Open, because it is one of the appliers replay feeds
	// -- and with no Durable: it gets its log in step 3, once wal.Open has
	// returned. Until then every mutating call on it refuses with ErrNotDurable,
	// which is the correct order: recovery finishes before the first live mint or
	// redemption.
	inviteStore, err := invite.NewStore(invite.StoreOptions{BusID: busID, Logger: lg})
	if err != nil {
		return fmt.Errorf("creating the invite store: %w", err)
	}
	// The FEDERATION CONFIGURATION store, built here for the same reason as the
	// two above: it is an applier, so it must exist before the log.
	//
	// IT IS BUILT WITH NO Durable, DELIBERATELY, and that is not the invite
	// store's "it gets one in step 3" — it never gets one. Peer configuration is
	// an OFFLINE operator action (`agent-bus peer add`, which refuses to run
	// against a locked data directory; DECISIONS.md FEDERATION (e)), so the
	// SERVER never records peer configuration: every mutating call on a store with
	// no durable log fails with relay.ErrPeerNotDurable.
	//
	// "NEVER RECORDS PEER CONFIGURATION" IS THE EXACT CLAIM, and it is narrower
	// than "writes nothing". PeerStore.Apply still reconciles the RELAY-34
	// withdrawal floor while replaying, which can fsync <data-dir>/peer-withdrawal-
	// floor — that path is gated on Dir, not on Durable. It only ever RAISES the
	// floor, i.e. it can only widen what is treated as revoked, which is the
	// fail-closed direction; but this comment previously said the type system
	// forbade all writes, and it does not.
	//
	// Dir IS passed, and is required rather than decorative: the RELAY-34
	// withdrawal floor lives beside the log, and a store built without it would
	// ignore a durably recorded revocation and show a REVOKED pinned bus signing
	// key as still pinned — fail-open on exactly the record an operator revokes
	// in an emergency. relay.PeerStore.PinnedBusSigningKeys refuses to answer at
	// all without it.
	//
	// # A STORE THAT CANNOT BE BUILT DISABLES FEDERATION; IT DOES NOT STOP THE BUS
	//
	// The one thing that realistically fails here is the RELAY-34 withdrawal
	// floor: a corrupt or unreadable floor file makes NewPeerStore refuse, and
	// that file is beside the log in every data directory, including those of the
	// overwhelming majority of buses that federate with nobody. Returning an error
	// here would turn damaged federation state into a TOTAL OUTAGE of messaging,
	// enrolment and everything else — for a fault that cannot affect any of them.
	//
	// So it is FAIL-CLOSED AND AVAILABLE, which is the same trade invariant 6
	// makes for a damaged log ("recovery ALWAYS reaches a running server;
	// every discard is logged loudly and specifically"). Without a peer store this
	// build serves NO peer route, verifies NO relayed message and authenticates NO
	// peer bus, so a revocation we could not read cannot be disregarded — there is
	// nothing left for it to protect.
	// The DELIVERY LIFECYCLE TABLE (ACK-2). It is built here, BEFORE wal.Open,
	// for the reason every applier is: replay must find it in the applier map,
	// or its records are passed over in complete silence by the multiplexer --
	// the silent discard invariant 6 rates as the defect. It is attached to the
	// log in the third step below, and until then every write refuses with
	// ack.ErrNotDurable.
	//
	// IT IS NOT GATED ON THE PEER STORE, unlike the three federation kinds
	// below. The rows it holds are LOCAL sender-visible acceptance -- "this bus
	// committed and fsynced the message" -- which a bus with no peers produces
	// on every send. Gating it on federation would leave a standalone bus
	// writing no status at all, which is the one topology this build actually
	// exercises.
	ackStore := ack.NewStore(ack.Options{Logger: lg})
	// The CONVERSATION store (CONV-CREATE-CLI). It is an applier of the same
	// three-step shape as the tables above: built HERE, before wal.Open, so replay
	// finds it in the applier map — otherwise the multiplexer passes over every
	// "conversation" record in the log in complete silence, the discard invariant
	// 6 rates as the defect. It is handed its durable log in the third step below
	// (Attach), and until then every create refuses with
	// store.ErrConversationNotDurable. It carries its OWN applied-key table
	// (invariant 10); its create records and their applied-key records are
	// replayed together by ConversationStore.Apply.
	//
	// A construction failure is FATAL: it validates only its own bounds against
	// this bus's id (there is no operator file behind it), so a failure is a
	// wiring fault in this build, and starting without the applier would skip
	// every conversation record in the log.
	conversationStore, err := store.NewConversationStore(store.ConversationStoreOptions{BusID: busID, Logger: lg})
	if err != nil {
		return fmt.Errorf("creating the conversation store: %w", err)
	}
	appliers := map[string]wal.Applier{
		auth.RecordKind:              authRoster,
		auth.OperatorRecordKind:      operatorRegistry,
		invite.RecordKind:            inviteStore,
		ack.RecordKind:               ackStore,
		store.ConversationRecordKind: conversationStore,
	}
	// The RELAY DELIVERY OUTBOX is the third applier of this shape, and it is
	// registered here for the reason the other two are: it is an applier, so it
	// must exist before the log (RELAY-24-BLOCKER-EGRESS item (d)).
	//
	// Until this line, relay.OutboxRecordKind was in NO applier map at all.
	// auth.MultiplexApplier stays SILENT about kinds it does not own — which is
	// what keeps message and seqfloor records from being read as damage — so an
	// outbox record in the log was passed over without a word. That is exactly
	// the silent discard invariant 6 rates as the defect: the record IS the durable
	// proof that this bus owes a peer a delivery, and a replay that skips it
	// cannot tell "nothing is owed" from "I did not look".
	//
	// It is built with NO Durable and receives its log in the third step below,
	// following invite.Store: replay must finish before the first live enqueue,
	// and until Attach every mutating call refuses with ErrOutboxNotDurable.
	//
	// IT IS GATED ON THE PEER STORE, on the same conditional as the other two
	// federation kinds. A bus with no peer store serves no peer route, verifies
	// no relayed message and can dial nobody, so it can never forward: an outbox
	// for it would be dead weight AND a table nothing could ever drain. On that
	// path the records are counted by skippedPeerRecords and reported by name and
	// number after replay, exactly as the peer and trust kinds are — visible
	// rather than silent.
	var (
		skippedPeerRecords *unreplayedPeerRecords
		relayOutbox        *relay.Outbox
	)
	peerStore, err := relay.NewPeerStore(relay.PeerStoreOptions{BusID: busID, Dir: cfg.DataDir, Logger: lg})
	if err != nil {
		lg.Error("FEDERATION IS DISABLED FOR THIS RUN: the peer configuration store could not be built, so this bus will serve no peer route, verify no relayed message and authenticate no peer bus. Messaging and enrolment are UNAFFECTED and start normally",
			"bus_id", busID,
			"data_dir", cfg.DataDir,
			"err", err.Error(),
			"remedy", "this is usually a damaged or unreadable "+relay.PeerWithdrawalFloorFileName+" beside the log; restore it from backup and restart, then re-apply any peer configuration with `agent-bus peer add`",
		)
		peerStore = nil
		// The records are still in the log, and they must not vanish from the
		// operator's view along with the store. Counted here, reported by name and
		// number once replay has finished (invariant 6: a discard is logged loudly
		// AND SPECIFICALLY; an unregistered kind is otherwise passed over in
		// complete silence by the multiplexer).
		skippedPeerRecords = &unreplayedPeerRecords{}
		appliers[relay.PeerRecordKind] = skippedPeerRecords
		appliers[relay.BusTrustRecordKind] = skippedPeerRecords
		appliers[relay.OutboxRecordKind] = skippedPeerRecords
	} else {
		// BOTH federation kinds dispatch to the ONE store, which owns both tables
		// (routes and trust) and keys its config_seq high-water mark across them.
		// Registering them here is what satisfies relay.PeerStoreOptions.Durable's
		// replay-before-first-write precondition STRUCTURALLY rather than by
		// remembering to: wal.Open does not return until the whole log has been
		// fed to this applier, and there is no write path to race it with.
		appliers[relay.PeerRecordKind] = peerStore
		appliers[relay.BusTrustRecordKind] = peerStore

		relayOutbox, err = relay.NewOutbox(relay.OutboxOptions{BusID: busID, Logger: lg})
		if err != nil {
			// FATAL, and NOT the same trade the peer store above makes. That one
			// degrades to "federation is disabled" because a damaged withdrawal
			// floor must never take messaging down with it. This constructor
			// validates only its own bounds against this bus's id -- there is no
			// operator file behind it to be damaged -- so a failure here is a
			// WIRING fault in this build, and starting without the applier would
			// silently skip every outbox record in the log.
			return fmt.Errorf("creating the relay delivery outbox: %w", err)
		}
		appliers[relay.OutboxRecordKind] = relayOutbox
	}
	applier, err := auth.NewMultiplexApplier(lg, appliers)
	if err != nil {
		return fmt.Errorf("creating the write-ahead log applier: %w", err)
	}
	walLog, err := wal.Open(wal.LogOptions{Dir: cfg.DataDir, Logger: lg, Applier: applier})
	if err != nil {
		// FATAL, but this is now a NARROW case, not the general one. Under the
		// always-restart decision (2026-08-02) recovery repairs or QUARANTINES
		// damaged records and the bus starts -- see the quarantine fields logged
		// below. So reaching here does not mean "the log is damaged"; it means
		// recovery could not even complete, e.g. the file is unreadable or the
		// quarantine itself failed.
		//
		// An earlier version of this comment claimed the one survivable case was
		// "a provably torn tail, bytes whose write never completed an fsync".
		// Every clause of that is now false: damage does NOT imply the write was
		// incomplete (media rot damages fully-written, fsynced, acknowledged
		// records), and torn tails are no longer the only thing survived.
		return fmt.Errorf("opening the write-ahead log in %q: %w", cfg.DataDir, err)
	}
	// Registered AFTER the lock's deferred Release so LIFO runs them in the
	// right order: close the log (flushing and releasing its file handle) while
	// the data dir is still locked, and only then drop the lock. The reverse
	// would leave a window where another server may acquire the dir while this
	// one still holds the WAL open.
	defer func() {
		// Reported, never swallowed -- a failing Close is a durability signal.
		// It does not overwrite run()'s return value: by this point the process
		// is already on its way out, and the reason it is leaving is more useful
		// to an operator than a close error is.
		if err := walLog.Close(); err != nil {
			lg.Error("closing the write-ahead log failed", "data_dir", cfg.DataDir, "path", walLog.Path(), "err", err)
			return
		}
		// NOT noise, do not delete: this line is the only OBSERVABLE proof that
		// Close actually ran on the shutdown path, and that it ran BEFORE the
		// lock's "data directory lock released" line (the LIFO close-then-unlock
		// order the comment above claims). Without it, deleting the whole defer
		// leaves every test green, because the kernel closes the fd at process
		// exit and the log file looks identical either way.
		lg.Debug("write-ahead log closed", "data_dir", cfg.DataDir, "path", walLog.Path())
	}()

	// One line, at INFO, naming what recovery found. wal.Open already logs its
	// own "wal replayed" line when the file held records (and warns per dangling
	// prepare), so this is the startup-visible summary that also fires for an
	// empty log: proof in the operator's log that a replay ran before we served.
	//
	// quarantined/discard_count/discarded_bytes are ALWAYS present here, not only
	// on the quarantine path, because DiscardCount/DiscardedBytes are exact on
	// every repair outcome (see wal.Repair) and "0" on the truncate/rewrite
	// paths is itself informative. This is deliberately in addition to, not
	// instead of, wal's own ERROR-level "wal quarantined..." line: that line is
	// what an operator grepping for "quarantin" finds, this one is what an
	// operator reading ONLY the startup summary must not be able to miss. Before
	// this fix a whole-log quarantine (Quarantined/DiscardCount/DiscardedBytes
	// set, Truncated left false) printed repaired=false repaired_bytes=0 here --
	// indistinguishable from a clean start with nothing replayed. See
	// DECISIONS.md 2026-08-02 ("Availability over retention"): the defect was
	// never the discard, it was the silence.
	if skippedPeerRecords != nil {
		// TWO LINES, NOT ONE, BECAUSE THE TWO REMEDIES ARE DIFFERENT. See
		// unreplayedPeerRecords: configuration comes back on the next start,
		// an owed delivery does not come back at all.
		if n := skippedPeerRecords.ConfigCount(); n > 0 {
			lg.Error("FEDERATION CONFIGURATION IN THE LOG WAS NOT RESTORED: this bus has peer-route or peer-trust records on disk, and the peer store that would hold them could not be built, so they were replayed into nothing. Nothing is lost FROM THE LOG: every one of these records returns intact as soon as the store can be built",
				"bus_id", busID,
				"config_records_skipped", n,
				"remedy", "see the peer-configuration-store error above; fix it and restart",
			)
		}
		if n := skippedPeerRecords.OutboxCount(); n > 0 {
			lg.Error("CROSS-BUS DELIVERIES THIS BUS OWED A PEER WERE NOT RESTORED: relay delivery-outbox records are on disk, and the outbox that would hold them could not be built because the peer store failed, so they were replayed into nothing. THIS IS NOT THE SAME AS THE CONFIGURATION ABOVE: nothing in this run owes those deliveries, so they will not be retried and no peer will receive them until they are re-sent by their original senders (invariant 6 -- the discard is recorded here rather than passed over in silence)",
				"bus_id", busID,
				"outbox_records_skipped", n,
				"remedy", "fix the peer-configuration-store error above and restart, which restores the outbox table from the same records; deliveries whose retry horizon has since passed are gone and must be re-sent by the originating agents",
			)
		}
	}

	rec := walLog.Recovered()
	lg.Info("write-ahead log opened",
		"data_dir", cfg.DataDir,
		"path", rec.Path,
		"records_replayed", rec.Records,
		"applied", rec.Applied,
		"aborted", rec.Aborted,
		"dangling", len(rec.Dangling),
		"next_index", rec.NextIndex,
		"repaired", rec.Repaired.Truncated,
		"repaired_bytes", rec.Repaired.Removed,
		"quarantined", rec.Repaired.Quarantined,
		"discard_count", rec.Repaired.DiscardCount,
		"discarded_bytes", rec.Repaired.DiscardedBytes,
	)

	// The enrolment and session authority. It is built AFTER the bus id is
	// resolved (every agent id it mints is qualified with that id, invariant 2)
	// and after the WAL is open, because the suffix allocator below reads the
	// replayed log and the recovery outcome.
	//
	// The allocator is ids.DurableNameSuffixes, via openSuffixAllocator: it
	// persists and fsyncs each name's floor BEFORE issuing the suffix that floor
	// authorises, so a restart resumes strictly above every suffix this data dir
	// has ever issued (invariant 1: ids are never reused, INCLUDING ACROSS
	// RESTARTS). Agent ids are durable inside store message records -- Sender
	// and Recipients are fully-qualified ids and the WAL never compacts -- so a
	// counter that restarted at 1 would re-mint ids that are already on disk,
	// handing a NEW agent holding a DIFFERENT keypair a previous agent's routing
	// and authorization identity.
	//
	// ids.NewNameSuffixes() -- the FRESH counter this used to build -- is gone
	// from cmd/ and MUST NOT come back, on any path, including as a fallback for
	// a failed open or a failed seal. OpenNameSuffixes already handles a fresh
	// data dir (it reports Existed() == false and yields an empty floor map), so
	// a fallback buys nothing and silently restores the defect while looking
	// fixed. Every failure below is FATAL, deliberately: a loud, recoverable
	// outage beats silent identity reuse.
	alloc, err := openSuffixAllocator(cfg.DataDir, walLog, busID, dataDirWasEmpty, cfg.BackfillSuffixFloors, lg)
	if err != nil {
		return fmt.Errorf("preparing the agent id suffix allocator: %w", err)
	}
	minter, err := ids.NewAgentIDMinter(busID, alloc)
	if err != nil {
		return fmt.Errorf("creating the agent id minter: %w", err)
	}
	// Attach LAST of the three steps, and before anything can serve: from here
	// the roster may accept a Put, and every Put is prepared, committed and
	// fsynced before Enrol returns (invariant 4).
	if err := authRoster.Attach(walLog); err != nil {
		return fmt.Errorf("attaching the durable enrolment roster to the write-ahead log: %w", err)
	}
	// The same third step for the invite table, and FATAL for the same reason: a
	// store with no log refuses every mint and every redemption (ErrNotDurable),
	// because single use held only in memory is decorative -- a restart would
	// forget which invites were spent.
	if err := inviteStore.Attach(walLog); err != nil {
		return fmt.Errorf("attaching the durable invite store to the write-ahead log: %w", err)
	}
	// The same third step for the relay delivery outbox, on the builds that have
	// one. FATAL for the same reason: an outbox with no log reports deliveries as
	// owed while nothing reaches disk (ErrOutboxNotDurable), and a relay hop
	// remembered only in memory evaporates on the crash it exists to survive.
	//
	// IT IS ENQUEUED INTO. This comment said "NOTHING ENQUEUES INTO IT IN THIS
	// BUILD" until the forwarder was constructed below: the relay.Forwarder built
	// in the egress block takes this outbox, so every cross-bus delivery is
	// written here as `pending` and FSYNCED BEFORE it is offered to any peer
	// queue, and settled `delivered`/`abandoned` when its outcome is known. The
	// three-step order is what makes that legal -- replay ran inside wal.Open
	// above, this line makes the table writable, and the first live enqueue
	// cannot happen until the hub is serving, which is after both.
	if relayOutbox != nil {
		if err := relayOutbox.Attach(walLog); err != nil {
			return fmt.Errorf("attaching the durable relay delivery outbox to the write-ahead log: %w", err)
		}
	}
	// The same third step for the delivery lifecycle table, and FATAL for the
	// same reason: an unattached table refuses every write with
	// ack.ErrNotDurable, so a bus that reached here with one would answer
	// `unknown` about every message it ever accepted while looking perfectly
	// healthy. Replay ran inside wal.Open above; this line makes the table
	// writable, and the first live row cannot be written until the hub is
	// serving, which is after both.
	if err := ackStore.Attach(walLog); err != nil {
		return fmt.Errorf("attaching the durable delivery lifecycle table to the write-ahead log: %w", err)
	}
	// The same third step for the conversation store, and FATAL for the same
	// reason: an unattached store refuses every create with
	// store.ErrConversationNotDurable, so a bus that reached here with one would
	// answer 503 to every conversation create while looking healthy. Replay ran
	// inside wal.Open above; this line makes the store writable, and the first
	// live create cannot be written until the server is serving, which is after
	// this.
	if err := conversationStore.Attach(walLog); err != nil {
		return fmt.Errorf("attaching the durable conversation store to the write-ahead log: %w", err)
	}
	// The same third step for the operator registry -- and NOT for the same
	// reason as the four above, which is why this comment does not say "the same
	// reason". Those tables are written by a running server, so an unattached one
	// would be a bus refusing live traffic. This registry is never written by the
	// server at all: `operator add|revoke` is offline, under the exclusive lock
	// this process is holding. So this line enables nothing that was failing, and
	// it must not be read as opening an online admin write path.
	//
	// It is here because auth.OperatorRegistry's type doc makes the
	// build -> replay -> Attach order its contract, and a registry left half-way
	// through that order is a shape no other applier in this function is in. The
	// honest cost is stated beside the construction above: unattached is the
	// FAIL-CLOSED state (Add and Revoke refuse with ErrNotAttached), so attaching
	// spends a guard rather than adding one. It is safe only while nothing on the
	// server side holds this registry -- today nothing does.
	//
	// FATAL, like the four above: a second Attach or a nil log is a wiring fault
	// in this build, and starting with one would leave the operator plane in a
	// state no test covers.
	if err := operatorRegistry.Attach(walLog); err != nil {
		return fmt.Errorf("attaching the durable operator registry to the write-ahead log: %w", err)
	}
	// One line, at INFO, and worded so it can NEVER be read as "enrolment is
	// open": it is not, as of INVITE-GATE-ENFORCE. This line said the exact
	// OPPOSITE until the gate landed -- "ENROLMENT IS NOT GATED ... an enrolment
	// presenting NO invite is still accepted" with enrolment_invite_required
	// false -- and it was true when written. A startup line that survives the
	// change it describes is worse than no line: an operator who reads it takes
	// the absence of a gate on trust and never checks.
	//
	// invites_recovered is proof the table was rebuilt by the replay that just
	// ran, and it is the number that matters on a gated bus: it is how many
	// agents can still get on.
	lg.Info("invite table recovered and REDEEMABLE, and ENROLMENT IS NOW INVITE-ONLY (invariant 3): POST /v1/enroll presenting NO invite is refused 403, and a presented invite is spent atomically with the enrolment it authorises in ONE durable transaction. Agents already enrolled are UNAFFECTED and do not re-enrol. To admit a NEW agent you must STOP the bus, run `agent-bus invite mint`, and restart -- minting takes the data directory's exclusive lock that this process is holding",
		"bus_id", busID,
		"invites_recovered", inviteStore.Len(),
		"enrolment_invite_required", enrolmentInviteRequired,
	)
	// The operator plane's own recovery line, and it is EVIDENCE, not decoration:
	// these two numbers are non-zero only because the registry above is in the
	// applier map, so the line going quiet is how an operator would notice the
	// AUTH-10-WIRING defect coming back. Before that registration both counts
	// were structurally 0 on a data dir holding any number of operator records.
	//
	// BOTH numbers, because they answer different questions and the difference is
	// the one that matters after a revocation: operators_recovered is how much
	// history the registry replayed, live_operators is how many principals can
	// authenticate right now. A revoked operator raises the first and not the
	// second, so "2 recovered, 1 live" is what proves a REVOCATION survived the
	// restart rather than just the two adds that preceded it.
	lg.Info("operator registry recovered from the append-only log: operator records are now REPLAYED rather than passed over, so a revocation taken with `agent-bus operator revoke` survives this restart. Operators are an OFFLINE-managed principal: this process never writes one -- `agent-bus operator add|revoke` takes the data directory's exclusive lock that this process is holding, so admitting or revoking an operator means STOPPING the bus, running the subcommand, and starting it again",
		"bus_id", busID,
		"operators_recovered", operatorRegistry.Len(),
		"live_operators", operatorRegistry.LiveLen(),
	)
	// THE GATE IS TURNED ON HERE, at the composition root, and this is the only
	// place in the tree that decides it (auth.Options.RequireInvite defaults
	// false so an embedder that wires no invite store keeps working).
	//
	// It is safe to set unconditionally ONLY because inviteStore above is built
	// unconditionally and is handed to httpapi as Options.Invites below. Requiring
	// an invite on a bus with no invite store is an UNENROLLABLE bus -- 501 to a
	// presented invite, 403 to none -- which httpapi.New logs loudly if it ever
	// sees. Do not make one of those two conditional without the other.
	authSvc, err := auth.NewService(auth.Options{
		Minter:        minter,
		Roster:        authRoster,
		RequireInvite: enrolmentInviteRequired,
	})
	if err != nil {
		return fmt.Errorf("creating the auth service: %w", err)
	}

	// Operator-visible, at WARN, on every start, and NARROWED AGAIN by AUTH-7.
	//
	// This line used to say the roster and the sessions were both in memory only
	// and that an accepted enrolment must not be treated as durable. The ROSTER
	// half of that became FALSE the moment authRoster was wired above: enrolment
	// is now recorded through the two-phase path, fsynced before Enrol returns,
	// and rebuilt by replay. A startup line that lies about durability is worse
	// than no line at all -- it is read exactly by the operator deciding whether
	// to trust the bus with something -- so what remains is the SESSION half,
	// stated precisely and no wider.
	//
	// The session half is NOT a defect and must not be "fixed" by persisting
	// sessions. A session is a short-lived bearer credential with a one-hour
	// ceiling; losing one costs an agent a single challenge/response round trip,
	// while writing live credentials to disk would put replayable material there
	// for no benefit (see auth's doc.go). What an agent must NOT have to do
	// after a restart is RE-ENROL, and it no longer does.
	//
	// The level stays WARN rather than dropping to INFO: an operator restarting
	// a bus needs to know every in-flight token just became invalid, because
	// every agent will re-handshake at once and anything holding a token will see
	// one 401 first.
	//
	// NARROWED by MSG-FU-SUFFIXFLOOR. This line used to end "and agent id
	// suffixes restart from 1 for every name", which became FALSE the moment
	// openSuffixAllocator landed above -- the floors are now persisted and
	// fsynced ahead of every suffix, so they survive restart. A false WARN in
	// the startup log is its own defect: an operator who reads it would take
	// precautions against a hazard that no longer exists and would distrust the
	// clause that is still true. The rest of the sentence stands unchanged --
	// the roster and the sessions really are in memory only.
	//
	// The clause was DELETED rather than negated, and that is deliberate. The
	// security gate showed that the obvious replacement -- "agent id suffixes are
	// durable and never restart from 1" -- is itself false in the one case an
	// operator most needs the truth. That case has since CHANGED, and this
	// comment is corrected rather than left standing: a data dir whose floors
	// file has been lost no longer resumes names from 1, it REFUSES TO START
	// (AUTH-3-FU-FAILOPEN), and it starts only with -backfill-suffix-floors. A
	// single unconditional sentence still cannot be right in every case, so the
	// claim continues to live entirely in the "agent-id suffix floors" line that
	// openSuffixAllocator emits, which knows which case it is in and picks INFO,
	// WARN or ERROR accordingly. This line says only what is unconditionally
	// true.
	//
	// AUTH-3 and AUTH-7 have both landed, so nothing here points at a follow-up
	// any more: the roster is durable, and the session table is a deliberate,
	// permanent exception rather than a gap waiting to be closed.
	lg.Warn("SESSIONS are in-memory only: every bearer token is invalidated by this start, and each enrolled agent must run the session handshake again before its first authenticated call. It does NOT have to re-enrol -- the roster IS durable (fsynced through the two-phase write path and rebuilt by replay), so agent ids, public keys and each agent's ORIGINAL enrolment instant survive a restart and a crash. Persisting sessions is deliberately NOT planned: they are short-lived credentials, and writing live ones to disk would store replayable material for the price of one round trip saved. For agent id suffix durability -- a SEPARATE question, answered per data directory -- read the \"agent-id suffix floors\" line above; on an ordinary start it is emitted at INFO, and it is raised to WARN or ERROR precisely when there is something to act on",
		"bus_id", busID,
		"roster_durable", true,
		"agents_recovered", authRoster.Len(),
		"sessions_durable", false,
	)

	// -----------------------------------------------------------------------
	// THE FEDERATION EGRESS (RELAY-24-BLOCKER-EGRESS)
	// -----------------------------------------------------------------------
	//
	// Built BEFORE the hub because hub.Open takes BOTH halves of it: the
	// RemoteRouter that admits a recipient behind a peer bus, and the Egress that
	// carries the committed message there. The ingress (newFederation, below the
	// hub) is assembled from the SAME registry, passed in — see
	// federationOptions.Registry for why a second table would be a defect.
	//
	// h IS DECLARED HERE AND ASSIGNED BELOW, and two closures in this block are
	// LATE-BOUND over it (the relay client's local roster, and the forwarder's
	// RecoverMessage). That ordering dependency is load-bearing and is stated at
	// each closure: both are called only after hub.Open has returned.
	var (
		h *hub.Hub

		// remoteRouter and hubEgress are INTERFACE-typed on purpose. Declaring
		// them as *relay.Registry / *relayEgress and handing the nil pointer to
		// hub.Options on a non-federated build would produce a NON-NIL interface
		// holding a nil pointer -- hub.forwardOnward would then call Forward on
		// it and panic, on a bus that is not federated at all. The nil-interface
		// checks in the hub are only meaningful if the value really is nil.
		remoteRouter hub.RemoteRouter
		hubEgress    hub.Egress

		// onwardForwarder is the SAME forwarder, seen through the seam the relay
		// INGRESS uses to carry a peer's message to a further hop (RELAY-47). It
		// is interface-typed for the identical reason as the two above, and the
		// trap is identical: assigning a nil *relay.Forwarder into it would
		// produce a non-nil interface holding a nil pointer, and relay.Acceptor's
		// `if a.onward != nil` gate would then call through it on a bus that does
		// not federate at all.
		onwardForwarder relay.OnwardForwarder

		relayRegistry  *relay.Registry
		relayForwarder *relay.Forwarder
		egressAdapter  *relayEgress

		// ackBackPropagator is the EMITTING half of the acknowledgement plane's
		// backward path (ACK-5): it is told an upstream bus id and resolves that
		// id's ADDRESS through the peer registry. Nil on a bus with no peer
		// store, exactly like the three above, because a back-propagator with no
		// registry could only get an address from the frame -- which is the SSRF
		// the peer surface exists to refuse (ACK-CONTRACT.md §9.4).
		ackBackPropagator *relay.BackPropagator
	)
	// Gated on the peer store, exactly as the ingress and the outbox are: with no
	// peer store this bus can authenticate no peer, verify no relayed message and
	// hold no route, so an egress path for it would be a forwarder with nowhere
	// to send. relayOutbox is non-nil on precisely this branch.
	if peerStore != nil {
		relayRegistry, err = relay.NewRegistry(relay.RegistryOptions{BusID: busID, Logger: lg})
		if err != nil {
			// FATAL: this constructor validates only this bus's own id, so a
			// failure is a wiring fault in this build, not operator data.
			return fmt.Errorf("creating the peer routing table: %w", err)
		}

		// THE OUTBOUND, PINNED, MUTUAL-TLS CLIENT. See relaydial.go: the pin is
		// resolved by the ADDRESS being dialled, an address with no configured
		// pin is REFUSED rather than dialled unverified, and the one permitted
		// InsecureSkipVerify in this tree (client/pin.go) is reused rather than
		// copied -- this adds ZERO new occurrences of it (invariant 11,
		// DECISIONS.md 2026-08-15).
		//
		// ourLeaf is the bus's OWN certificate. buscert mints one leaf carrying
		// both ServerAuth and ClientAuth and DECISIONS.md rules "one identity,
		// both directions", so what this bus SERVES is what it PRESENTS when it
		// dials a peer.
		ourLeaf := busMaterial.TLSCertificate()
		peerHTTP := newPinnedPeerHTTPClient(newPeerPinsByAddress(peerStore.ActivePeers(), lg), &ourLeaf, lg)

		relayClient, err := relay.NewClient(relay.ClientConfig{
			BusID: busID,
			// LATE-BOUND over h, which is assigned by hub.Open below. It is only
			// ever called from Client.Enroll -- an OPERATOR-initiated handshake,
			// long after startup -- never from the relay POST the forwarder makes.
			// A refactor that calls it earlier breaks this silently, so the check
			// is explicit rather than a nil dereference.
			LocalRoster: func() []string {
				if h == nil {
					return nil
				}
				agents := h.Agents()
				out := make([]string, 0, len(agents))
				for _, a := range agents {
					out = append(out, a.AgentID)
				}
				return out
			},
			HTTPClient: peerHTTP,
			Logger:     lg,
		})
		if err != nil {
			return fmt.Errorf("creating the peer relay client: %w", err)
		}

		relayForwarder, err = relay.NewForwarder(relay.ForwarderOptions{
			BusID:    busID,
			Registry: relayRegistry,
			Client:   relayClient,
			// THE REGISTRY'S OWN METHOD, not a closure over it. PeerBaseURL is
			// called from every per-peer worker goroutine on every attempt and
			// MUST be safe for concurrent use; the method takes the registry's
			// RLock and its own doc names itself as the intended value here.
			PeerBaseURL: relayRegistry.PeerBaseURL,
			// The durable delivery table, replayed by wal.Open above and made
			// writable by Attach. Supplying it makes RecoverMessage REQUIRED --
			// NewForwarder refuses one without the other, in both directions.
			Outbox: relayOutbox,
			// LATE-BOUND over h AND over egressAdapter, both assigned below. It is
			// called ONLY from Resume(), which this function calls after both
			// assignments -- see the Resume call site. A refactor that moves
			// Resume earlier breaks this silently, which is why the guard below
			// reports rather than dereferences nil.
			RecoverMessage: func(originMessageID string) (relay.RelayedMessage, bool, error) {
				if h == nil || egressAdapter == nil {
					return relay.RelayedMessage{}, false, errors.New("the hub and the egress adapter are not constructed yet; Resume ran before the wiring completed, which is a startup-ordering fault in this build")
				}
				// THE DECISION ITSELF IS A NAMED FUNCTION, not this closure's body
				// (RELAY-48). This closure is reachable only by starting a whole
				// server, so a body written inline here can be exercised only through
				// a full boot -- and the one thing it has to get right is what happens
				// AFTER A CRASH. recoverRelayEnvelope takes exactly the two things it
				// needs, so a crash test calls the SAME code the server calls rather
				// than a re-implementation of it that could agree with the test and
				// disagree with production.
				//
				// busID, NOT cfg.BusID: cfg.BusID is the TEST-ONLY -bus-id flag and is
				// EMPTY in every production run, while busID is what
				// ids.LoadOrCreateBusID resolved and is the hop this bus actually
				// stamps onto the messages it ingests (invariant 1). Passing the flag
				// would compare every stored bus path's final hop against "" and
				// abandon every resumed relayed job -- the precise defect RELAY-48
				// fixes, reintroduced by a plausible-looking identifier.
				return recoverRelayEnvelope(busID, h.Store(), egressAdapter.envelope, originMessageID)
			},
			Logger: lg,
		})
		if err != nil {
			return fmt.Errorf("creating the cross-bus relay forwarder: %w", err)
		}
		// REGISTERED AFTER the WAL's own deferred Close, so LIFO runs this one
		// FIRST. That order is required, not tidy: the forwarder writes delivery
		// settlements THROUGH the WAL, so a settlement landing after walLog.Close
		// would fail, and one landing after the data-directory lock was released
		// would be a write into a directory another server may already own. Same
		// reasoning as the walLog.Close defer above, one layer out.
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
			defer cancel()
			if err := relayForwarder.Close(ctx); err != nil {
				lg.Warn("the cross-bus relay forwarder did not drain within the shutdown grace period; in-flight peer requests were cancelled. Anything still owed stays PENDING in the durable outbox and is re-offered after the next start",
					"grace", shutdownGrace.String(), "err", err.Error())
				return
			}
			lg.Debug("cross-bus relay forwarder closed", "bus_id", busID)
		}()

		egressAdapter, err = newRelayEgress(relayEgressOptions{
			BusID: busID,
			// The BUS SIGNING key, which mints the origin attestation. It is NOT
			// the TLS key and the two are never conflated.
			SigningKey: busMaterial.SigningPrivateKey(),
			// The LIVE roster, read through on every forward: an agent that
			// re-keys must be attested under its new key.
			Roster:    authRoster,
			Forwarder: relayForwarder,
			// THE SAME REGISTRY THE FORWARDER ROUTES ON, and it must stay the
			// same instance: the adapter reads it ONLY as a conservative gate on
			// whether to mint an attestation at all (relayEgress.routesToSomePeer),
			// and that gate's safety rests on being a superset of what
			// relay.Forwarder.targets will decide from the same table.
			Router: relayRegistry,
			Logger: lg,
		})
		if err != nil {
			return fmt.Errorf("assembling the cross-bus egress adapter: %w", err)
		}
		// THE ACKNOWLEDGEMENT PLANE'S BACKWARD HALF (ACK-5). It is built on this
		// branch and no other, for the same reason the forwarder is: it dials
		// peers, so it needs the pinned mutual-TLS client and the ONE routing
		// table, and a bus with neither has nowhere to send an acknowledgement.
		ackBackPropagator, err = relay.NewBackPropagator(relay.BackPropagatorConfig{
			BusID: busID,
			// THE SAME pinned, mutual-TLS peer client the forwarder relays
			// through. One transport, one set of pins, one answer to "who did we
			// just talk to" (invariant 11).
			Sender: relayClient,
			// THE REGISTRY'S OWN METHOD, not a closure over it -- the same rule
			// relay.ForwarderOptions.PeerBaseURL states above: it is called
			// concurrently and takes the registry's RLock, and hand-writing a
			// closure over the registry's internals is the defect that method was
			// added to fix. IT IS THE ONLY SOURCE OF AN ADDRESS ON THIS PATH.
			PeerBaseURL: relayRegistry.PeerBaseURL,
			Logger:      lg,
		})
		if err != nil {
			// FATAL: this constructor validates only this build's own wiring, so
			// a failure here is a fault in this binary, not operator data.
			return fmt.Errorf("creating the acknowledgement back-propagator: %w", err)
		}
		remoteRouter = relayRegistry
		hubEgress = egressAdapter
		// THE THIRD CONSUMER OF THE ONE FORWARDER (RELAY-47). The egress adapter
		// above carries messages this bus ORIGINATED; this seam carries messages
		// it RECEIVED from a peer and owes to a further hop. They are two callers
		// of one Enqueue, deliberately: one outbox, one set of per-peer queues,
		// one place that resolves targets and applies the split horizon.
		onwardForwarder = relayForwarder
	}

	// The messaging core, built HERE rather than inside internal/httpapi (which
	// used to construct one for itself; see httpapi.Options.Hub). This is the
	// composition root, so it is the one place that can hold the durable log, the
	// authoritative roster and the hub at the same time -- and the one place a
	// failure to build any of them can still be FATAL, which is the whole reason
	// the arrangement moved.
	//
	// Replay is a SECOND, READ-ONLY pass over the same file. It is not the same
	// pass wal.Open just made: that one fed the roster (the log's applier), and
	// this one rebuilds the message store, the applied-key table and the sequence
	// floor. They stay separate because the hub must not be the log's applier --
	// see the wal.Open comment above -- and because hub.Open needs the recovery
	// outcome (NextIndex, Quarantined) that wal.Open only produces by returning.
	// A second wal.Open on this directory would be a second WRITER and is not an
	// option; wal.Replay is read-only.
	//
	// The roster is passed as a LIVE VIEW, never a snapshot. hubRoster reads
	// through to authRoster on every call, so an agent that enrols a minute from
	// now is on the hub's roster a minute from now. A []hub.Agent captured here
	// would be a roster frozen at boot: every agent enrolled afterwards would
	// authenticate and then be refused as an unknown sender, which is the AUTH-7
	// failure with a new cause.
	//
	// DataDir is passed because the hub keeps ONE durable file of its own there:
	// the message sequence floor (hub.SeqFloorFileName), the only record of a
	// minted-but-unspent sequence that survives a WAL quarantine. It is the same
	// directory the WAL, the bus id and the agent-suffix floors live in, and it is
	// already created and LOCKED above -- the hub must never be handed a directory
	// another process may be writing.
	h, err = hub.Open(hub.Options{
		BusID:   busID,
		DataDir: cfg.DataDir,
		Durable: walLog,
		Replay: func(fn func(wal.Committed) error) (wal.Recovered, error) {
			return wal.Replay(walLog.Path(), fn)
		},
		NextIndex:   rec.NextIndex,
		Quarantined: rec.Repaired.Quarantined,
		// LogRepaired answers ONE question for the hub: did recovery physically
		// remove records from this log? It is the predicate that decides whether
		// a MISSING message-seq-floor file may be rebuilt from the log — safe on
		// the ordinary upgrade path, an invariant-1 violation when the log has
		// holes. See describeLogRepair for what counts and, more importantly,
		// what deliberately does not.
		LogRepaired: describeLogRepair(rec),
		Roster:      hubRoster{roster: authRoster},
		Logger:      lg,
		PollTimeout: cfg.PollTimeout,

		// The DURABLE SENDER-VISIBLE DELIVERY LIFECYCLE TABLE (ACK-2). Attached
		// above, so it is writable before the hub can serve a single send.
		//
		// What this wires on, stated narrowly: a LOCAL, non-broadcast send now
		// writes one `accepted` row per recipient, in a second two-phase
		// transaction after the message's own commit. NOTHING READS THOSE ROWS
		// YET -- GET /v1/ack is ACK-9 -- so the observable effect of this line
		// today is one additional fsync cycle per RECIPIENT -- one per send, since
		// hub.SendRequest carries a single `To` -- and a durable record an
		// operator can read out of the WAL. It never fails a send: see
		// hub.AckRecorder and Hub.recordAcceptance for why that asymmetry is
		// deliberate.
		Acks: ackStore,

		// THE EGRESS PAIR (RELAY-24-BLOCKER-EGRESS). Both are nil on a bus with
		// no peer store, and a nil pair is behaviourally identical to the bus
		// before either seam existed: every recipient this bus does not hold is
		// refused with the honest 404 it always got.
		//
		// RemoteRouter WAS DELIBERATELY LEFT UNWIRED until now. hub.RemoteRouter's
		// "DO NOT INJECT A ROUTER EARLY" note makes it a PRECONDITION that no
		// router may be wired until the egress path that carries an admitted
		// message onward exists and is DURABLE -- a router wired sooner does not
		// make a bus federated, it makes it accept messages it has no way to
		// deliver, silently, having removed the 404 that was protecting the
		// client. That precondition is MET as of this build: the forwarder is
		// constructed above, its outbox is the durable relay delivery table
		// replayed by wal.Open and attached before this line, and the Egress
		// below is what carries an admitted message to it.
		RemoteRouter: remoteRouter,
		Egress:       hubEgress,
	})
	if err != nil {
		// FATAL. A bus that cannot rebuild its message store must not serve one
		// (invariant 5), and a bus that starts with no messaging is the silent
		// half-outage AUTH-7 exists to make impossible.
		return fmt.Errorf("opening the messaging hub: %w", err)
	}

	// THE BACK-PROPAGATION ADAPTER (ACK-5), assembled HERE because it is the
	// first point at which both halves exist: the emitting back-propagator (built
	// on the peer-store branch above) and the hub whose store answers "which hop
	// handed us the message under this correlation key".
	//
	// Two consumers, ONE instance: the AGENT surface reaches it when a local
	// recipient acknowledges a message that was RELAYED here, and the PEER
	// surface reaches it when a downstream peer propagates an outcome for a key
	// this bus did not originate. They are two callers of one backward hop, in
	// the same sense the forwarder has three callers of one Enqueue -- a second
	// instance would be a second place that decides who this bus contacts.
	//
	// httpAckTransit is INTERFACE-TYPED and is assigned ONLY on the branch that
	// actually built an adapter, exactly as remoteRouter and hubEgress are above:
	// a nil *ackTransit stored straight into httpapi.Options.AckTransit would be
	// a NON-nil interface holding a nil pointer, and the route's `== nil` gate
	// would call dutifully through it on a bus that does not federate at all.
	// federationOptions.AckTransit is a concrete pointer and does not have that
	// trap, which is why it takes the pointer directly.
	var (
		ackTransitAdapter *ackTransit
		httpAckTransit    httpapi.AckTransit
	)
	if ackBackPropagator != nil {
		ackTransitAdapter, err = newAckTransit(busID, func(correlationKey string) ([]string, bool) {
			// LATE-BOUND over h, which is assigned just above; the guards are
			// the same belt the LocalRoster and RecoverMessage closures wear,
			// for the same reason -- a refactor that moves this call earlier
			// would otherwise break it as a nil dereference inside a request.
			//
			// It reads the BODY-FREE accessor on purpose: this closure sits on a
			// path an authenticated agent drives once per POST /v1/ack, and a
			// routing question gets a routing-only answer (invariant 6). The
			// path it returns is the STORED one -- origin-first, ending at THIS
			// bus -- which is the shape relay.UpstreamHop requires; handing it a
			// wire path would let a peer-fabricated path choose who we contact.
			if h == nil {
				return nil, false
			}
			st := h.Store()
			if st == nil {
				return nil, false
			}
			p, ok := st.RelayProvenanceByOriginMessageID(correlationKey)
			return p.BusPath, ok
		}, ackBackPropagator, lg)
		if err != nil {
			// FATAL, like every other assembly failure here: a bus that federates
			// messages but silently drops the acknowledgements coming back is the
			// half-outage that looks like a quiet network for a day.
			return fmt.Errorf("assembling the acknowledgement back-propagation adapter: %w", err)
		}
		httpAckTransit = ackTransitAdapter
	}

	// STAGE 2 OF THE THREE-STAGE STARTUP ORDERING: peer-store replay (done, by
	// wal.Open) -> REGISTRY RESTORE (here) -> forwarder Resume (next).
	//
	// # AN EMPTY ROSTER IS THE CORRECT VALUE, NOT A STUB
	//
	// Each peer is seeded with Agents: nil, and that is not "we will fill it in
	// later". Registry.Route resolves by the BUS HALF of a fully-qualified id and
	// NEVER by roster membership -- the Registry doc says so in as many words,
	// and it is invariant 2 doing its job: "<bus-id>.<agent-id>" NAMES ITS OWN
	// OWNER. Roster membership is a DISCOVERY and LISTING convenience, exposed
	// separately as Knows, and a routing table that required it would drop every
	// message to an agent that enrolled on a peer since our last sync.
	//
	// So a peer whose roster we have never exchanged is ROUTABLE the moment its
	// address is configured. The roster arrives later, if ever, through the
	// handshake (Registry.UpsertPeer, which preserves the base URL set here).
	//
	// The ADDRESS is operator configuration by design -- SetPeerBaseURL's own doc
	// -- never something a peer asserts about itself, so it comes from the
	// durable peer store and from nowhere else.
	//
	// peersSeeded is hoisted out of the block because the federation summary
	// below reports it: "the forwarder is wired" and "there is a peer to forward
	// to" are DIFFERENT states, and an operator must be able to tell them apart.
	var peersSeeded int
	if relayRegistry != nil {
		var seeded, refused int
		for _, rec := range peerStore.ActivePeers() {
			if err := relayRegistry.UpsertPeer(relay.PeerRoster{BusID: rec.BusID, Agents: nil}); err != nil {
				// NOT FATAL, and named. One unroutable peer must not stop the
				// others being seeded, but a peer silently absent from the routing
				// table is a send to it answering 404 with nothing to explain why.
				refused++
				lg.Error("a configured peer could NOT be seeded into the routing table, so no message will be routed to it",
					"peer_bus", rec.BusID, "err", err.Error())
				continue
			}
			if err := relayRegistry.SetPeerBaseURL(rec.BusID, rec.BaseURL); err != nil {
				refused++
				lg.Error("a configured peer was seeded into the routing table but its base URL was refused, so it is routable and undialable: messages for it will be accepted, recorded in the durable outbox and never sent",
					"peer_bus", rec.BusID, "base_url", rec.BaseURL, "err", err.Error())
				continue
			}
			seeded++
		}
		peersSeeded = seeded
		lg.Info("peer routing table seeded from the durable peer configuration; each peer is routable by the BUS HALF of a recipient id, with an empty roster until a handshake exchanges one",
			"bus_id", busID, "peers_seeded", seeded, "peers_refused", refused)
	}

	// STAGE 3: re-offer the deliveries this bus still owed when it last stopped.
	//
	// AFTER THE SEED AND BEFORE THE SERVER SERVES, and both halves matter.
	// Resume resolves every recovered job through PeerBaseURL, so a Resume
	// against an empty registry sees EVERY peer as unknown and takes the
	// no-route arm for the whole backlog. That arm is fail-safe by design (the
	// jobs stay durably owed rather than being abandoned), but relying on it
	// would mean a bus that re-offers nothing on every boot and says so only at
	// Warn. Resuming before serving is what keeps a live Enqueue from racing the
	// pass and double-queueing a job.
	//
	// LOGGED AT INFO ALWAYS, INCLUDING ZERO. "Nothing was owed" and "the resume
	// never ran" are the two states an operator most needs to tell apart, and a
	// line that only appears when the number is non-zero cannot distinguish them.
	if relayForwarder != nil {
		resumed, err := relayForwarder.Resume()
		if err != nil {
			// NOT FATAL: nothing is lost. Every job that was not re-offered is
			// still PENDING in the durable outbox and is re-offered after the next
			// start. Refusing to serve messaging over it would be a much larger
			// outage than the one being reported.
			lg.Error("the relay delivery outbox could NOT be fully resumed; deliveries this bus still owed have not all been re-offered. They remain durably owed and return after the next start",
				"bus_id", busID, "re_offered", resumed, "err", err.Error())
		} else {
			lg.Info("relay delivery outbox resumed: deliveries this bus still owed at its last shutdown are back on their peers' queues",
				"bus_id", busID, "re_offered", resumed)
		}
	}

	// THE FEDERATION INGRESS (RELAY-24), assembled AFTER the hub because the
	// relay's local half is the hub, and after the peer store has been replayed
	// by wal.Open above.
	//
	// A build with no peer that could authenticate serves NO peer route at all.
	// That is httpapi's mount rule 2 applied one level up: registering three
	// routes that answer 403 to every caller would advertise "this bus federates"
	// to a stranger while serving nobody, and the difference between "not
	// configured" and "configured wrong" must be visible in OUR log rather than
	// on the wire. The gate is the INBOUND CLIENT CERTIFICATE BINDING, not the
	// presence of peer records: that binding is the only thing
	// PeerStore.InboundPeerPrincipal resolves.
	var (
		peerSurface    *httpapi.PeerSurface
		peerPrincipals httpapi.InboundPeerPrincipals
	)
	bindable := bindablePeerCount(peerStore)
	switch {
	case peerStore == nil:
		// Already reported at ERROR where the store failed to build, with the
		// remedy. Nothing to add here, and nothing below may touch the nil store.
	case bindable > 0:
		fed, err := newFederation(federationOptions{
			BusID: busID,
			// THE SAME registry the hub routes on and the forwarder dials
			// through, built above. See federationOptions.Registry: a second
			// table here would leave a peer that had just handshaked routable on
			// one and unknown on the other.
			Registry: relayRegistry,
			Local:    hubIngest{h: h},
			// THE ONWARD SEAM (RELAY-47). Nil on a bus with no peer store, which
			// is the documented LEAF configuration; non-nil here means a message
			// this bus accepts for a THIRD bus's agents is carried further rather
			// than stopping. It is the same *relay.Forwarder the egress adapter
			// holds, reached through an interface-typed variable so a
			// non-federated build passes a genuinely nil seam.
			Onward: onwardForwarder,
			Peers:  peerStore,
			// THE SAME outbox the egress forwarder writes obligations to, and
			// THE SAME ack store the applier map already holds. Both are passed
			// rather than rebuilt for the reason Registry is: a second copy of
			// either would let a hop be owed on one table and unknown on the
			// other, and the ACK plane's anti-forgery rule (ACK-CONTRACT.md §6.2)
			// is precisely a lookup in the first of them.
			//
			// relayOutbox is non-nil on this branch — it is built on the same
			// peer-store branch that made `bindable > 0` reachable — and
			// newFederation refuses a nil one rather than serving an ACK route
			// that could bind nothing.
			Outbox:       relayOutbox,
			AckLifecycle: ackStore,
			// THE SAME backward hop the agent surface uses (ACK-5). An
			// acknowledgement arriving here for a key this bus RELAYED but did
			// not originate has no row to settle, and is carried one hop further
			// back instead of being refused. Non-nil on this branch, because the
			// back-propagator is built on the same peer-store branch that made
			// `bindable > 0` reachable.
			AckTransit: ackTransitAdapter,
			// The applied-key table's own pressure line, read live. It is what
			// makes the per-peer share a BOUND rather than a speed limit: below
			// the line a peer over its share is denying nobody anything and is
			// admitted, exactly as internal/idem treats its own per-agent share.
			UnderPressure: func() bool { return h.IdempotencyStats().UnderPressure },
			LocalAgents: func() []string {
				agents := h.Agents()
				out := make([]string, 0, len(agents))
				for _, a := range agents {
					out = append(out, a.AgentID)
				}
				return out
			},
			Logger: lg,
		})
		if err != nil {
			// FATAL. An operator who has configured peering is entitled to have
			// it work or to be told why not; starting with federation silently
			// off is the half-outage that looks like a network problem for a day.
			return fmt.Errorf("assembling the federation ingress: %w", err)
		}
		peerSurface = fed.Surface()
		peerPrincipals = peerStore
		// THIS LINE SAID SOMETHING FALSE UNTIL RELAY-24-BLOCKER-EGRESS. It read
		// "it does NOT yet forward any onward, because the egress forwarder is
		// not wired in this build", with onward_relay=false, and that was true
		// when written -- the forwarder had no production caller at all. It is
		// corrected rather than deleted, and it now reports THREE separate facts
		// that an operator must not have to infer from one another:
		//
		//   egress_forwarder_wired  a locally-originated message addressed to an
		//                           agent on a peer bus is accepted, recorded in
		//                           the durable outbox and sent. Wired says the
		//                           MACHINERY exists.
		//   peers_seeded            how many peers it can actually reach. A bus
		//                           with a wired forwarder and ZERO seeded peers
		//                           forwards NOTHING, and that is a completely
		//                           different operational state from a bus that
		//                           has no forwarder. Reporting only the first
		//                           would make the two look identical.
		//   onward_relay            TRUE as of RELAY-47, and it is a NARROWER
		//                           claim than it looks. "Onward" here is the
		//                           RELAY sense -- carrying a message that arrived
		//                           FROM a peer to a FURTHER hop
		//                           (relay.AcceptOptions.Onward). It is a
		//                           DIFFERENT seam from egress_forwarder_wired
		//                           above, which is about messages ORIGINATED
		//                           HERE, and the two are reported apart because
		//                           they can differ: a build could have a
		//                           forwarder and no onward wiring, which is
		//                           exactly what this bus was until RELAY-47. Do
		//                           not let a future rewording blur these two
		//                           meanings of "forward".
		//
		//                           It reports the WIRING, not a routing promise
		//                           and not a durability promise, and both limits
		//                           are stated rather than left to be discovered:
		//
		//                           A message with no route to its destination
		//                           bus, one whose next hop is already on its
		//                           traversed path, or one at the 64-hop limit is
		//                           carried no further. onwardRelay.Enqueue logs
		//                           the message that reaches NO peer at all. A
		//                           message that reaches SOME of its destinations
		//                           and not others is NOT individually logged --
		//                           relay.Forwarder.targets counts a no-route
		//                           recipient without a line, and "queued fewer
		//                           copies than destinations" is not a sound
		//                           detector, because the egress split horizon
		//                           legitimately drops a destination already on
		//                           the traversed path: an A->B relay naming
		//                           recipients on both A and C counts two foreign
		//                           buses at B and correctly queues one copy.
		//                           Filed as RELAY-50 rather than papered over
		//                           with a heuristic that fires on correct
		//                           traffic.
		//
		//                           The ONWARD hop IS crash-safe as of RELAY-48:
		//                           the durable message record now carries the
		//                           origin's message id AND the origin's
		//                           attestation, so Resume rebuilds the envelope
		//                           and RE-OFFERS the hop instead of abandoning
		//                           it. The ONE residual is a message
		//                           relay-ingested by a PRE-RELAY-48 binary: its
		//                           record has no attestation, this bus may not
		//                           mint one for another bus's agent (invariant
		//                           2), and that message's onward hop is
		//                           unrecoverable. It is settled abandoned with
		//                           the reason logged, one job at a time.
		//
		// This line said "It does NOT carry a message RECEIVED from one peer
		// onward to a further hop -- multi-hop onward relay is not implemented, so
		// this bus is a leaf", with onward_relay=false, and that was true when
		// written. It is corrected rather than deleted. It then said "THE ONWARD
		// HOP IS NOT YET CRASH-SAFE ... because an intermediate cannot rebuild the
		// origin's signed envelope from durable state", which RELAY-48 falsified
		// in this same file; a stale operator-facing claim reads as freshly
		// checked, which is worse than no claim.
		lg.Info("FEDERATION is served: peer routes are registered behind the TLS client certificate principal. This bus ACCEPTS relayed messages for its own agents, it FORWARDS messages its own agents originate to agents on a peer bus through the durable relay outbox, and it CARRIES a message received from one peer onward to a further hop when the destination routes to a different peer. THE ONWARD HOP IS CRASH-SAFE (RELAY-48): a hop still owed when this bus stops is RE-OFFERED at the next start, rebuilt from the durable record, which carries the origin's message id and the origin's attestation. The one exception is a message ingested by a pre-RELAY-48 binary, whose record has no attestation and whose onward hop cannot be rebuilt; that is settled abandoned with the reason logged. A message with no route, one whose next hop has already seen it, or one at the traversed-path limit is carried no further; one that reaches no peer at all is logged individually, one that reaches only some of its destinations is not",
			"bus_id", busID,
			"bindable_peers", bindable,
			"trusted_buses", len(peerStore.TrustedBuses()),
			"configured_routes", len(peerStore.ActivePeers()),
			"egress_forwarder_wired", relayForwarder != nil,
			"peers_seeded", peersSeeded,
			"onward_relay", onwardForwarder != nil,
		)
	case len(peerStore.TrustedBuses()) > 0 || len(peerStore.ActivePeers()) > 0:
		// Configured, but not usable. LOUD, because from outside this is
		// indistinguishable from a bus that was never meant to federate, and the
		// operator has already done most of the work.
		// INGRESS ONLY. The clause naming egress was added with the forwarder
		// (RELAY-24-BLOCKER-EGRESS): the two directions are configured by
		// DIFFERENT records and now genuinely fail independently, so an operator
		// reading this line must not conclude that outbound forwarding is off as
		// well. It is not -- the peers seeded above are dialable, and messages to
		// them are accepted, recorded and sent -- and saying only half of that
		// would send an operator hunting for the wrong fault.
		lg.Error("FEDERATION INGRESS IS NOT SERVED although peering is configured: no adjacent bus has an INBOUND CLIENT CERTIFICATE bound to it, so no peer could authenticate and no peer HTTP route is registered. EGRESS IS UNAFFECTED and is reported by the two egress fields on this line: outbound forwarding is configured by the peer ROUTE record (-url and -tls-fingerprint), inbound authentication by the peer TRUST record (-peer-client-fingerprint), and they fail independently",
			"bus_id", busID,
			"trusted_buses", len(peerStore.TrustedBuses()),
			"configured_routes", len(peerStore.ActivePeers()),
			"egress_forwarder_wired", relayForwarder != nil,
			"peers_seeded", peersSeeded,
			"remedy", "stop the bus and run `agent-bus peer add -data-dir "+cfg.DataDir+" -bus-id <peer bus id> -signing-key <that bus's pinned Ed25519 signing key, base64> -peer-client-fingerprint <64 lowercase hex>`, once per adjacent bus. The fingerprint is sha256 of the DER of the certificate THAT peer presents AS A TLS CLIENT WHEN IT DIALS THIS BUS (inbound, keyed to -bus-id): it is NOT -tls-fingerprint, which pins the certificate the bus at -url serves to US when WE dial IT — different direction, different certificate, and neither substitutes for the other. -peer-client-fingerprint requires -signing-key in the same invocation, because the binding is written on the TRUST record; a trust record is rewritten whole, so re-state the flag every time you re-add that bus",
		)
	default:
		// The ordinary case for a bus nobody has peered. One line at INFO, so an
		// operator who expected federation can see that this build simply has no
		// peer configuration rather than having refused it.
		lg.Info("federation is not configured: this bus has no peer records and no peer trust records, so no peer route is registered",
			"bus_id", busID)
	}

	// rootCtx is the SERVER-LIFETIME context. http.Server.BaseContext hands it
	// to every connection, so each request context descends from it: cancelling
	// it releases handlers parked on a long-poll instead of leaving them
	// blocked on a channel while Shutdown waits for them forever. The POLL epic
	// only has to select on r.Context().Done().
	rootCtx, cancelRoot := context.WithCancel(context.Background())
	defer cancelRoot()

	handler := httpapi.New(httpapi.Options{
		Identity:    ids.BusIdentity(busID),
		Logger:      lg,
		Version:     version,
		StartedAt:   time.Now(),
		PollTimeout: cfg.PollTimeout,
		// The HTTP layer holds the log for the process lifetime so the handlers
		// that land later write through it (invariant 4) instead of minting a
		// second write path. No handler reads it in this task.
		Durable: walLog,
		// Registers /v1/enroll, /v1/session/begin and /v1/session/complete.
		// It authenticates NO other route -- that is AUTH-2.
		Auth: authSvc,
		// Makes an invite PRESENTED to /v1/enroll redeemable, atomically with the
		// enrolment.
		//
		// IT IS REQUIRED, NOT MERELY ACCEPTED, and this comment said the opposite
		// ("It does NOT require one: an enrolment carrying no invite is still
		// accepted") until INVITE-GATE-ENFORCE. The requirement itself lives on
		// authSvc above (auth.Options.RequireInvite, enrolmentInviteRequired);
		// what this line contributes is the other half of the pair -- the store
		// that makes a presented invite redeemable at all. Wiring one without the
		// other yields an unenrollable bus, which httpapi.New logs loudly.
		Invites: inviteStore,
		// Registers the messaging surface: /v1/agents, /v1/broadcast, /v1/send,
		// /v1/messages and /v1/wait. Every one of them authenticates.
		Hub: h,
		// Registers GET /v1/ack/<correlation-key>, the SENDER-VISIBLE delivery
		// status route (ACK-9, ACK-CONTRACT.md §13). It is the same *ack.Store
		// the relay wiring settles outcomes into and the same one the WAL
		// replays, so a status read and a peer ACK can never disagree about a
		// row — there is one table, not a serving copy of one.
		AckStatus: ackStore,
		// Registers POST /v1/conversations (CONV-CREATE-CLI): mint a durable,
		// idempotent conversation. It is the same *store.ConversationStore the WAL
		// replays and attaches above, so a create and a recovery can never
		// disagree about a conversation — one table, not a serving copy of one. It
		// authenticates like every other messaging route and takes the creator
		// from the session, never a request field (invariant 1).
		Conversations: conversationStore,
		// The BACKWARD hop for a TRANSIT acknowledgement (ACK-5,
		// ACK-CONTRACT.md §9.4): a local recipient acknowledging a message that
		// was RELAYED here settles no row on this bus, so POST /v1/ack carries
		// the outcome one hop back toward the origin and answers only after that
		// hop has answered -- which is what keeps invariant 4 true end to end
		// when the durable row lives on another bus.
		//
		// Nil on a build with no peer, and then that route answers 501 rather
		// than claiming to have carried anything. It is assigned from an
		// INTERFACE-typed variable set only where an adapter was really built;
		// see the typed-nil note on Options.AckTransit.
		AckTransit: httpAckTransit,
		// THE FEDERATION INGRESS. Both are nil on a build with no bindable peer,
		// and the mount then registers nothing at all — see the switch above.
		// They are supplied as a PAIR: a surface without the resolver would be
		// three routes answering 403 to everyone, which mountPeerSurface refuses
		// to register for exactly that reason.
		Peer:           peerSurface,
		PeerPrincipals: peerPrincipals,
		// PER-SOURCE RATE LIMIT on the three unauthenticated credential routes
		// (AUTH-1-FU-RATELIMIT). Enabled by default; -auth-rate-burst 0 disables
		// it. A throttled source is answered 429 + Retry-After and never
		// disconnected (invariant 10). It sits in front of the allow-list and
		// does not change its membership (invariant 3).
		AuthRateLimit: httpapi.AuthRateLimit{
			PerSecond: cfg.AuthRateLimitPerSecond,
			Burst:     cfg.AuthRateLimitBurst,
		},
	})

	// THE RESOURCE BOUNDS ON THE SERVER (CORE-9).
	//
	// # ReadTimeout AND WriteTimeout ARE DELIBERATELY UNSET. DO NOT "COMPLETE THE SET".
	//
	// This is the guardrail comment, and it is the point of the task that added
	// these fields. Both of those are ABSOLUTE DEADLINES ON THE WHOLE
	// REQUEST/RESPONSE, measured from when the connection is accepted -- they are
	// not idle timers. A long-poll on /v1/wait parks for up to -poll-timeout
	// (defaultPollTimeout, 30s) BY DESIGN, so any WriteTimeout shorter than that
	// kills the poll mid-flight and any ReadTimeout shorter than that kills it
	// before the handler ever answers. The failure is not a clean error either:
	// the client sees a truncated response or a reset connection on the bus's
	// core mechanic, intermittently, only under the timing that matters.
	//
	// "Add a sensible timeout to the HTTP server" is exactly the well-intentioned
	// hardening change a later contributor makes without realising it breaks
	// long-polling, so the absence of those two fields is RECORDED HERE rather
	// than left to be inferred from their absence. If a request-lifetime bound is
	// ever genuinely needed, it belongs per-handler on the request context, where
	// the poll handler can opt out -- not on the server, where it cannot.
	//
	// What IS set: ReadHeaderTimeout bounds the slow-headers attack (a request
	// that never finishes its headers occupies a connection and no handler),
	// IdleTimeout bounds idle keep-alives, and MaxHeaderBytes bounds header
	// memory. All three bound a connection that is NOT serving a request, so none
	// of them can fire during a long-poll.
	srv := &http.Server{
		Handler:           handler,
		BaseContext:       func(net.Listener) context.Context { return rootCtx },
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
	}

	// THE ONE LISTENER, AND IT IS TLS (MTLS-LISTENER, invariant 11).
	//
	// The TLS configuration is built BEFORE the bind, deliberately, and the
	// reason is simply that a refused start must never have BOUND THE PORT AT
	// ALL.
	//
	// An earlier version of this comment justified the ordering with TIME_WAIT.
	// That was wrong and the reviewer gate caught it: a listening socket closed
	// without ever accepting a connection does not enter TIME_WAIT, and Go sets
	// SO_REUSEADDR on listeners regardless. The real property is the plain one --
	// between the bind and the refusal there would be a window in which this
	// process holds the address while having already decided not to serve, and
	// anything probing the port in that window sees a socket that accepts and
	// then dies. TestRunRefusesToStartWithoutUsableCert pins it by proving the
	// address is still unbound after run() returns an error.
	//
	// busTLSConfig returns an error rather than a degraded config -- there is no
	// plaintext listener to fall back to, so there is nothing to degrade TO.
	tlsCfg, err := busTLSConfig(busMaterial)
	if err != nil {
		return fmt.Errorf("preparing the TLS configuration from the material in %q: %w", cfg.DataDir, err)
	}

	// Bind before serving so a busy port is a startup error, not a background one.
	rawLn, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listening on %q: %w", cfg.Listen, err)
	}
	// tls.NewListener rather than srv.ServeTLS(ln, "", ""): the certificate is
	// already loaded and parsed by internal/buscert, and ServeTLS's only extra
	// service is re-reading it from two file paths, which would be a SECOND load
	// of the same material and a second chance for the two to disagree. Every
	// connection srv.Serve accepts below is therefore a *tls.Conn; net/http
	// completes the handshake before the first byte of a request is read, bounds
	// it by ReadHeaderTimeout above, and populates r.TLS from it.
	//
	// NOTHING MAY BE SERVED ON rawLn. It exists for exactly one statement -- being
	// wrapped on the next line -- and a second srv.Serve(rawLn) anywhere would be
	// a plaintext listener on the same port, serving every route in the clear
	// while every test in this package still passed against the TLS one.
	//
	// TestCmdHasNoPlaintextListener enforces that mechanically: it fails if the
	// result of net.Listen is used anywhere other than as the argument to
	// tls.NewListener. (An earlier version of this comment claimed that guard
	// existed when it did not -- the reviewer gate caught the claim, and the check
	// was written rather than the claim deleted.)
	ln := tls.NewListener(rawLn, tlsCfg)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.Serve(ln)
	}()

	// bus_cert_fingerprint is PUBLIC by construction -- it is the digest of a
	// certificate that will be sent to every client on every handshake -- and it
	// is the value an operator has to hand to a client to pin (E6, no TOFU), so
	// the startup summary is exactly where it belongs.
	//
	// tls=true is stated EXPLICITLY, and it is now a FACT ABOUT THE LISTENER
	// rather than a claim about intent: the addr below is a TLS listener or this
	// line was never reached. It stays in the summary because an operator reading
	// one line has to be able to tell which scheme to dial, and because the field
	// flipping from false to true in a deployment's logs is the single clearest
	// marker of the cutover this change forces on every existing deployment.
	//
	// client_auth is reported alongside it so the summary cannot be read as
	// "mutual TLS is on". It is not: MTLS-CLIENTAUTH (a97f854) moved this field
	// from "none" to "requested", and "requested" means a certificate is asked
	// for, never required, and authenticates nobody on its own. REQUIRING one is
	// a later task, and it must not precede a client that can present one.
	//
	// tls_min_version and client_auth are DERIVED FROM tlsCfg, not written as
	// literals beside it. The reviewer gate found the literal form: with it,
	// flipping ClientAuth to RequireAnyClientCert left the summary still saying
	// client_auth=none and every test still passing -- a startup line that
	// misreports the policy it is describing, under a comment claiming it states
	// a fact about the listener. Deriving them makes that impossible: the only
	// way to change what is logged is to change what is served.
	lg.Info("server started",
		"addr", ln.Addr().String(),
		"scheme", "https",
		"bus_id", busID,
		"data_dir", cfg.DataDir,
		"poll_timeout", cfg.PollTimeout.String(),
		"log_level", cfg.LogLevel.String(),
		"version", version,
		"tls", true,
		"tls_min_version", tlsVersionName(tlsCfg.MinVersion),
		"client_auth", clientAuthName(tlsCfg.ClientAuth),
		"bus_cert_fingerprint", busMaterial.Fingerprint().String(),
	)

	return waitAndShutdown(lg, srv, cancelRoot, sigCh, serveErr)
}

// waitAndShutdown blocks until Serve fails or a shutdown signal arrives, then
// drains the server. It is a separate function so the cancel-before-Shutdown
// ordering below has a regression guard (see TestShutdownReleasesLongPoll):
// reversing those two statements reintroduces a hang that nothing else catches.
func waitAndShutdown(lg *logging.Logger, srv *http.Server, cancelRoot context.CancelFunc, sigCh <-chan os.Signal, serveErr <-chan error) error {
	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serving: %w", err)
		}
		return nil
	case sig := <-sigCh:
		lg.Info("shutdown signal received", "signal", sig.String())
	}

	// Release long-poll waiters FIRST: Shutdown blocks until active handlers
	// return, so a handler parked on a channel would stall the drain.
	cancelRoot()

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancelShutdown()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		lg.Warn("graceful shutdown incomplete, closing", "err", err, "grace", shutdownGrace.String())
		if cerr := srv.Close(); cerr != nil {
			lg.Error("closing server failed", "err", cerr)
		}
	}

	// Serve always returns once the listener is closed.
	if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serving: %w", err)
	}
	lg.Info("server stopped")
	return nil
}
