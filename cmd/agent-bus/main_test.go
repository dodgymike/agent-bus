package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/dirlock"
	"github.com/dodgymike/agent-bus/internal/logging"
)

// TestParseFlags covers the accept path and every rejection validate() owes the
// operator: parseFlags deliberately never calls os.Exit so this is testable.
func TestParseFlags(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		args    []string
		want    Config
		errWant string // substring; empty means "must succeed"
	}{
		{
			name: "defaults",
			args: nil,
			want: Config{
				Listen:                 defaultListen,
				DataDir:                defaultDataDir,
				PollTimeout:            defaultPollTimeout,
				LogLevel:               logging.LevelInfo,
				BusID:                  "",
				AuthRateLimitPerSecond: defaultAuthRateLimitPerSecond,
				AuthRateLimitBurst:     defaultAuthRateLimitBurst,
			},
		},
		{
			name: "all flags set",
			args: []string{
				"-listen", "127.0.0.1:0",
				"-data-dir", "/tmp/agent-bus-test",
				"-poll-timeout", "5s",
				"-log-level", "DEBUG",
				"-bus-id", "bus_test-01",
				"-auth-rate-limit", "12.5",
				"-auth-rate-burst", "40",
			},
			want: Config{
				Listen:                 "127.0.0.1:0",
				DataDir:                "/tmp/agent-bus-test",
				PollTimeout:            5 * time.Second,
				LogLevel:               logging.LevelDebug,
				BusID:                  "bus_test-01",
				AuthRateLimitPerSecond: 12.5,
				AuthRateLimitBurst:     40,
			},
		},
		{
			// Burst 0 is the documented "disabled" state and needs no rate.
			name: "rate limiting disabled by zero burst",
			args: []string{"-auth-rate-burst", "0"},
			want: Config{
				Listen:                 defaultListen,
				DataDir:                defaultDataDir,
				PollTimeout:            defaultPollTimeout,
				LogLevel:               logging.LevelInfo,
				AuthRateLimitPerSecond: defaultAuthRateLimitPerSecond,
				AuthRateLimitBurst:     0,
			},
		},
		{
			// A bucket that can hold tokens but never refills would 429 forever
			// once drained, locking enrolment out; rejected at parse time.
			name:    "positive burst with non-positive rate",
			args:    []string{"-auth-rate-limit", "0", "-auth-rate-burst", "30"},
			errWant: "-auth-rate-limit must be positive when -auth-rate-burst is positive",
		},
		{
			name:    "bad log level",
			args:    []string{"-log-level", "chatty"},
			errWant: "invalid -log-level",
		},
		{
			name:    "zero poll timeout",
			args:    []string{"-poll-timeout", "0s"},
			errWant: "-poll-timeout must be positive",
		},
		{
			name:    "negative poll timeout",
			args:    []string{"-poll-timeout", "-1s"},
			errWant: "-poll-timeout must be positive",
		},
		{
			name:    "empty listen",
			args:    []string{"-listen", ""},
			errWant: "-listen must not be empty",
		},
		{
			name:    "listen without port",
			args:    []string{"-listen", "localhost"},
			errWant: "invalid -listen",
		},
		{
			name:    "empty data dir",
			args:    []string{"-data-dir", ""},
			errWant: "-data-dir must not be empty",
		},
		{
			// Invariant 2: '.' is the <bus-id>.<agent-id> separator, so it can
			// never appear inside a bus id.
			name:    "bus id containing a dot",
			args:    []string{"-bus-id", "evil.bus"},
			errWant: "invalid -bus-id",
		},
		{
			name:    "bus id containing a space and a quote",
			args:    []string{"-bus-id", `bus id"x`},
			errWant: "invalid -bus-id",
		},
		{
			name:    "bus id too long",
			args:    []string{"-bus-id", strings.Repeat("b", 65)},
			errWant: "invalid -bus-id",
		},
		{
			name:    "stray positional argument",
			args:    []string{"serve"},
			errWant: `unexpected argument "serve"`,
		},
		{
			name:    "unknown flag",
			args:    []string{"-nope"},
			errWant: "flag provided but not defined",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseFlags("agent-bus", tt.args, io.Discard)
			if tt.errWant != "" {
				if err == nil {
					t.Fatalf("parseFlags(%q) = %+v, want error containing %q", tt.args, got, tt.errWant)
				}
				if !strings.Contains(err.Error(), tt.errWant) {
					t.Fatalf("parseFlags(%q) error = %q, want it to contain %q", tt.args, err, tt.errWant)
				}
				if got != (Config{}) {
					t.Errorf("parseFlags(%q) returned %+v alongside an error, want the zero Config", tt.args, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseFlags(%q) unexpected error: %v", tt.args, err)
			}
			if got != tt.want {
				t.Fatalf("parseFlags(%q) = %+v, want %+v", tt.args, got, tt.want)
			}
		})
	}
}

// TestParseFlagsHelp checks -h is reported as flag.ErrHelp (main exits 0 on it)
// rather than as a configuration error.
func TestParseFlagsHelp(t *testing.T) {
	t.Parallel()

	if _, err := parseFlags("agent-bus", []string{"-h"}, io.Discard); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("parseFlags(-h) error = %v, want flag.ErrHelp", err)
	}
}

// TestValidateNoBusIDOverride pins that validate() no longer validates a
// placeholder default id: with the ID epic wired in (internal/ids), the real
// bus id is minted and persisted from the data dir at run() time, so an
// unset -bus-id has nothing to validate here. This replaces the old
// TestValidateDefaultBusID, which asserted validateBusID(httpapi.DefaultBusID)
// and Config.resolveBusID() — both removed now that main.go delegates id
// validation to ids.ValidateBusID and no longer needs a placeholder resolver.
func TestValidateNoBusIDOverride(t *testing.T) {
	t.Parallel()

	cfg := Config{Listen: defaultListen, DataDir: defaultDataDir, PollTimeout: defaultPollTimeout}
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate() with no -bus-id = %v, want nil", err)
	}
}

// TestShutdownReleasesLongPoll is the regression guard for the ordering inside
// waitAndShutdown: the server-lifetime context MUST be cancelled before
// Shutdown, because Shutdown waits for active handlers and a parked long-poll
// only returns when its request context is done. If the two statements are
// swapped, Shutdown blocks for the whole shutdownGrace and this test fails on
// its own (much shorter) deadline instead of hanging the suite.
func TestShutdownReleasesLongPoll(t *testing.T) {
	// The bound must be well under shutdownGrace so a reversed ordering FAILS.
	const bound = 2 * time.Second
	if bound >= shutdownGrace {
		t.Fatalf("test bound %s must be well under shutdownGrace %s", bound, shutdownGrace)
	}

	rootCtx, cancelRoot := context.WithCancel(context.Background())
	defer cancelRoot()

	var parkOnce, releaseOnce sync.Once
	parked := make(chan struct{})   // closed once the handler is parked
	released := make(chan struct{}) // closed once the handler observes cancellation

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parkOnce.Do(func() { close(parked) })
		<-r.Context().Done() // the long-poll park
		releaseOnce.Do(func() { close(released) })
		w.WriteHeader(http.StatusNoContent)
	})

	srv := &http.Server{
		Handler:           handler,
		BaseContext:       func(net.Listener) context.Context { return rootCtx },
		ReadHeaderTimeout: readHeaderTimeout,
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	// Park a request inside the handler.
	reqDone := make(chan struct{})
	go func() {
		defer close(reqDone)
		resp, err := http.Get("http://" + ln.Addr().String() + "/wait")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()

	select {
	case <-parked:
	case <-time.After(bound):
		t.Fatal("handler never reached the park point")
	}

	sigCh := make(chan os.Signal, 1)
	sigCh <- syscall.SIGTERM

	lg := logging.New(io.Discard, logging.LevelError)

	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- waitAndShutdown(lg, srv, cancelRoot, sigCh, serveErr) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("waitAndShutdown() = %v, want nil", err)
		}
	case <-time.After(bound):
		t.Fatalf("waitAndShutdown did not return within %s: the parked long-poll was not released "+
			"before Shutdown (cancelRoot must run BEFORE srv.Shutdown)", bound)
	}

	select {
	case <-released:
	case <-time.After(bound):
		t.Fatal("parked handler was never released by the shutdown path")
	}

	if elapsed := time.Since(start); elapsed >= bound {
		t.Fatalf("shutdown took %s, want well under the %s grace period", elapsed, shutdownGrace)
	}

	select {
	case <-reqDone:
	case <-time.After(bound):
		t.Fatal("client request never completed")
	}
}

// TestRunRefusesALockedDataDir is the regression guard for the wiring in run():
// dirlock.Acquire must happen AFTER os.MkdirAll but BEFORE ids.LoadOrCreateBusID
// (DUR-8). It holds the lock itself, then calls run() and asserts run() fails
// fast with ErrLocked instead of blocking or proceeding. run() never reaches
// net.Listen here, so this cannot hang and binds no port.
func TestRunRefusesALockedDataDir(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	lock, err := dirlock.Acquire(dir)
	if err != nil {
		t.Fatalf("dirlock.Acquire(%q) = %v, want a held lock to provoke run()'s refusal", dir, err)
	}
	defer lock.Release()

	cfg := Config{
		Listen:      "127.0.0.1:0",
		DataDir:     dir,
		PollTimeout: time.Second,
		LogLevel:    logging.LevelError,
	}

	err = run(cfg)
	if err == nil {
		t.Fatalf("run() with a pre-locked -data-dir %q = nil, want an error", dir)
	}
	if !errors.Is(err, dirlock.ErrLocked) {
		t.Fatalf("run() error = %v, want errors.Is(err, dirlock.ErrLocked)", err)
	}
	if !strings.Contains(err.Error(), dir) {
		t.Fatalf("run() error %q does not name the data dir %q", err.Error(), dir)
	}

	// Regression guard for DUR-9's "lock before wal.Open" ordering: if run()
	// ever moves the Acquire call after any data-dir I/O (e.g. after
	// ids.LoadOrCreateBusID or a future wal.Open), that I/O would happen before
	// the (correctly still-failing) lock check and leave extra files behind
	// even though we hold the lock throughout. The ONLY entry a refused run()
	// may have touched is the lock file itself, which already existed because
	// we created it above by acquiring the lock.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("os.ReadDir(%q) = %v", dir, err)
	}
	if len(entries) != 1 || entries[0].Name() != dirlock.LockFileName {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("data dir %q contains %v after a refused run(), want only %q; "+
			"a regression here means the data directory is read/written OUTSIDE the lock",
			dir, names, dirlock.LockFileName)
	}
}
