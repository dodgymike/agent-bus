// Command agent-busctl is the agent-bus client.
//
// It is the ONLY sanctioned way to talk to a bus: nobody hand-writes HTTP
// (invariant 7), and this binary replaces the scripts/bus-*.sh wrappers as
// their subcommands land.
//
// It is deliberately a THIN shell over the importable package
// github.com/dodgymike/agent-bus/client. Anything implemented here and not
// there is something an agent that EMBEDS the client cannot reach, so this
// package contains flag parsing, help text and rendering — and no protocol
// logic at all.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	// A Ctrl-C or a SIGTERM cancels the operation's context, so an in-flight
	// request is abandoned cleanly rather than the process being torn down
	// mid-write to the credential store.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	os.Exit(runWithTTY(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, os.LookupEnv,
		isTerminal(os.Stdout), isTerminal(os.Stdin)))
}

// isTerminal reports whether f is attached to a character device — a terminal.
//
// Two commands change BEHAVIOUR on the answer, so this is not cosmetic:
//
//   - `watch` prints its live human feed only to a terminal. A pipe or a
//     redirect is a MACHINE, and a machine gets NDJSON whether or not it
//     remembered to pass --json. A long poll whose output is buffered into a
//     human-shaped block that only completes at exit is useless.
//   - `send` reads the body from stdin when no other source is given, and only
//     announces that it is doing so when stdin is a terminal. That way an agent
//     shelling out can never hang on a prompt it cannot see.
//
// Stat + ModeCharDevice is the stdlib-only check (invariant 8); an ioctl-based
// isatty would be a dependency for a sharper answer than either caller needs.
// It is deliberately conservative at the edges: /dev/null is also a character
// device, so `agent-busctl send x </dev/null` takes the "terminal" branch — it prints
// the notice, reads EOF immediately and fails with "empty body". It never hangs.
// An unstattable descriptor reports false: guessing "terminal" would make `send`
// park on a stdin nobody is going to write to.
func isTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
