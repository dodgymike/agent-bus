package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/dodgymike/agent-bus/client"
)

func clientCertCommand() command {
	return command{
		name:    "client-cert",
		summary: "show this agent's own TLS certificate, minting it on first use",
		help: `agent-busctl client-cert — this agent's own TLS certificate.

USAGE
  agent-busctl client-cert [--identity <dir>] [--json]

WHERE IT LIVES
  ` + client.ClientTLSDirName + `/ inside the credential store directory
  (--identity <dir>, env ` + client.EnvIdentityDir + `).

WHAT IT DOES
  Prints the TLS certificate this agent PRESENTS to the bus: where it is
  stored, its SHA-256 fingerprint and how long it is valid for. If no
  certificate exists yet, one is minted here — a fresh Ed25519 key and a
  self-signed certificate, written 0600 inside a 0700 directory next to the
  credential store. The private key never leaves this machine.

  Nothing is sent to the bus. Every other command mints the same certificate
  on its first TLS connection, so you rarely need to run this; it exists to
  answer "which certificate am I presenting" and "where is it".

WHAT IT IS FOR
  agent-bus uses MUTUAL TLS: the bus proves which certificate it is with the
  fingerprint from your invite, and this is the certificate you prove yourself
  with in the other direction. The bus does not require one yet, so a fresh
  certificate changes nothing you can observe today — it is offered on every
  connection and, for now, ignored.

  The certificate's SUBJECT means nothing. Your identity is the agent id the
  bus minted for you; what the bus will bind is this FINGERPRINT.

RE-RUNNING IS SAFE
  This command never replaces material that is already there. That is not
  politeness: a new certificate has a new fingerprint, so regenerating one
  would silently break any binding the bus already holds. Retiring a
  certificate is therefore a deliberate act — move the whole directory aside
  and enrol again.

WHEN IT REFUSES
  If the directory holds a key but no certificate (or the reverse), this stops
  rather than repairing it. The missing half cannot be regenerated from the
  surviving one without changing the fingerprint, so an automatic "repair"
  would look like a fix and would in fact revoke your TLS identity.

EXIT CODES
  0 ok
  2 bad usage
  3 the credential store is unreadable, unwritable, or holds damaged material
`,
		run: runClientCert,
	}
}

// clientCertResult is the JSON shape of `client-cert`.
type clientCertResult struct {
	// Fingerprint is the SHA-256 of the certificate DER, lowercase hex — the
	// same spelling `pin` uses for the bus's certificate, so one `jq` filter
	// reads either end.
	Fingerprint string `json:"fingerprint"`

	CertPath string `json:"cert_path"`
	KeyPath  string `json:"key_path"`

	NotBefore string `json:"not_before"`
	NotAfter  string `json:"not_after"`

	// Created reports whether THIS invocation minted the material. An agent
	// scripting enrolment can branch on it: true means the fingerprint above
	// has never been seen by any bus.
	Created bool `json:"created"`

	// Expired is carried as its own field rather than left to the caller to
	// derive from NotAfter, because a caller that has to parse a timestamp to
	// discover a fault usually does not.
	Expired bool `json:"expired"`
}

func runClientCert(ctx context.Context, env *cliEnv, args []string) error {
	fs, diagnostics := newCommandFlagSet("client-cert", env.g)
	if err := fs.Parse(args); err != nil {
		if err == flagErrHelp {
			return err
		}
		return flagError("agent-busctl client-cert", diagnostics, err)
	}
	if err := requireNoArgs("client-cert", fs.Args()); err != nil {
		return err
	}
	env.out.json = env.g.json

	c, err := env.client()
	if err != nil {
		return err
	}

	cc, err := c.ClientCertificate()
	if err != nil {
		return err
	}

	// Warnings are NOT printed here. They used to be, and that was this
	// command's own private arrangement back when it was the only reader of
	// them; now that run() drains Client.Warnings after every command, printing
	// them here as well emitted each one TWICE. One drain, one place.

	now := time.Now()
	res := clientCertResult{
		Fingerprint: cc.Fingerprint(),
		CertPath:    cc.CertPath,
		KeyPath:     cc.KeyPath,
		NotBefore:   cc.Leaf.NotBefore.UTC().Format(time.RFC3339),
		NotAfter:    cc.Leaf.NotAfter.UTC().Format(time.RFC3339),
		Created:     cc.Created,
		Expired:     cc.IsExpired(now),
	}

	return env.out.Emit(res, func(w io.Writer) {
		fmt.Fprintf(w, "%s\n", res.Fingerprint)
		fmt.Fprintf(w, "  cert     %s\n", res.CertPath)
		fmt.Fprintf(w, "  key      %s (never leaves this machine)\n", res.KeyPath)
		fmt.Fprintf(w, "  valid    %s to %s\n", res.NotBefore, res.NotAfter)
		if res.Expired {
			fmt.Fprintf(w, "  EXPIRED  the bus will refuse this once it starts checking; move %s aside and enrol again\n", cc.Dir)
		}
		if res.Created {
			fmt.Fprintf(w, "  minted   just now — no bus has seen this certificate yet\n")
		}
	})
}
