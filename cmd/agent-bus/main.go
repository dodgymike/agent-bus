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

	"github.com/dodgymike/agent-bus/internal/dirlock"
	"github.com/dodgymike/agent-bus/internal/httpapi"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// version is the reported build version. Override at build time with
// -ldflags "-X main.version=$(git describe --tags --always)".
var version = "dev"

// Defaults for the command-line flags.
const (
	defaultListen      = ":8080"
	defaultDataDir     = "./data"
	defaultPollTimeout = 30 * time.Second
	defaultLogLevel    = "info"

	// shutdownGrace bounds the drain of in-flight requests after the
	// server-lifetime context has been cancelled.
	shutdownGrace = 10 * time.Second

	// readHeaderTimeout bounds how long a client may take to send its request
	// headers; it deliberately does NOT bound the whole request, because
	// long-polls are meant to be slow.
	readHeaderTimeout = 15 * time.Second
)

// Config is the fully validated runtime configuration.
type Config struct {
	// Listen is the TCP address the HTTP server binds, e.g. ":8080".
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
}

func main() {
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
	fs.StringVar(&cfg.Listen, "listen", defaultListen, "TCP address to listen on, e.g. \":8080\" or \"127.0.0.1:8080\"")
	fs.StringVar(&cfg.DataDir, "data-dir", defaultDataDir, "directory holding the durable store and the append-only log")
	fs.DurationVar(&cfg.PollTimeout, "poll-timeout", defaultPollTimeout, "maximum time a long-poll waits before returning empty, e.g. 30s")
	fs.StringVar(&logLevel, "log-level", defaultLogLevel, "minimum log severity ("+logging.Levels+")")
	fs.StringVar(&cfg.BusID, "bus-id", "", "TEST-ONLY: force the bus id. The server is authoritative on every id (invariant 1); a supplied bus id is not a trusted identity. Never use this in production")

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
	lock, err := dirlock.Acquire(cfg.DataDir)
	if err != nil {
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
	// The Applier is deliberately nil, and that is an honest statement of where
	// this project is: there is no in-memory serving copy yet (internal/store is
	// still a doc.go stub), so there is nothing for committed entries to be
	// applied TO. The replay is therefore a durability fsck -- it verifies every
	// frame, resolves prepares against commits, and establishes the high-water
	// index -- and it rebuilds no state, because there is no state to rebuild.
	// When the store lands, it is passed here as the Applier and this comment
	// goes with it.
	walLog, err := wal.Open(wal.LogOptions{Dir: cfg.DataDir, Logger: lg})
	if err != nil {
		// FATAL, and deliberately so: run() returning non-nil makes main() print
		// the error and exit 1. A log we cannot open or replay means we do not
		// know what the accepted history is, and serving from an empty store
		// would silently present a bus with no memory of durable, acknowledged
		// writes. Never degrade to that. The one damage case that is survivable
		// -- a provably torn tail, bytes whose write never completed an fsync --
		// is already repaired inside wal.Open/RepairTail, so anything reaching
		// here is damage that recovery has refused to guess about; there is no
		// second-guessing it at this layer.
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
	)

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
	})

	srv := &http.Server{
		Handler:           handler,
		BaseContext:       func(net.Listener) context.Context { return rootCtx },
		ReadHeaderTimeout: readHeaderTimeout,
	}

	// Bind before serving so a busy port is a startup error, not a background one.
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listening on %q: %w", cfg.Listen, err)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.Serve(ln)
	}()

	lg.Info("server started",
		"addr", ln.Addr().String(),
		"bus_id", busID,
		"data_dir", cfg.DataDir,
		"poll_timeout", cfg.PollTimeout.String(),
		"log_level", cfg.LogLevel.String(),
		"version", version,
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
