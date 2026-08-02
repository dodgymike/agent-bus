// Command busctl is the agent-bus client.
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

	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr, os.LookupEnv))
}
