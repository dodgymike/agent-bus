# CLI-VALIDATE: envelope/schema validation in the CLIENT before a message is handed to the caller (was AGENTIF-9, was a bash+jq check)

| Field | Value |
| --- | --- |
| Public id | `b890e3d6-79df-4950-aac4-5ba1dc88b30e` |
| Key | AGENTIF-9 |
| Epic | [CLI](../epic.md) |
| Status | todo |
| Priority | P1 |
| Component | agentif |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T10:00:39.112686+00:00 |
| Updated | 2026-08-02T18:03:51.162261+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run 'TestClientRejectsMalformedEnvelope' ./client/...
```

## Description

RE-SCOPED 2026-08-02 FROM A SHELL-WRAPPER CHECK TO A CLIENT-PACKAGE CHECK. The user's original
instruction stands verbatim -- "add a mechanism to validate messages in the agent script before
accepting them" -- but the "agent script" is now the Go CLI and its reusable client package
(DECISIONS.md 2026-08-02: the Go CLI replaces the .sh files; invariant 7 amended). Moved to the CLI
epic.

WHY THIS GETS *EASIER* AND MUST NOT BE DROPPED: the original framing worried about `bash` + `jq`
trusting server JSON blindly and feeding it into `eval`/interpolation. A typed Go client removes the
shell-injection half of that outright -- but NOT the half that actually matters: a malformed,
truncated, or unexpectedly-shaped response from a MISBEHAVING OR RELAY-HOPPED BUS must be rejected
before it reaches the calling agent. Under invariant 2 a message may have crossed a bus you do not
directly trust, and under the 2026-08-02 relay decision relay auth is bi-directional precisely because
an intermediate bus is not automatically trusted.

REQUIRED: strict decoding (reject unknown/missing fields rather than zero-valuing them), bounds on
every length, validation that the fully-qualified `<bus-id>.<agent-id>` parses and that the claimed
sender is well-formed, and a typed error the caller can branch on. FAIL CLOSED: on a validation
failure return an error and NOTHING usable, never a partially-populated message. Applies on every
inbound path -- watch/long-poll, message history, roster listing, peer exchange.

LAYERING, unchanged: CRYPTO-10 covers the CRYPTOGRAPHIC verification layer (signature/MAC/decrypt),
wired in once the CRYPTO epic lands. THIS task is the layer underneath it and INDEPENDENT of it --
needed from day one, and still needed after CRYPTO-10 exists, because a signature over a malformed
envelope is still a malformed envelope.

PROOF. `go test -race -run 'TestClientRejectsMalformedEnvelope' ./client/...` -- table-driven over
truncated JSON, wrong types, missing required fields, oversized fields, and an unparseable qualified
id; each case must yield an error AND no partially-populated result. FAILS TODAY by construction (the
client package does not exist). The OLD proof_cmd was prose, not a command
("bash scripts/bus-wait.sh (against a throwaway server) fed a malformed/truncated response -- exits
non-zero, prints nothing usable to stdout"), so it could not have been run by proof-check.sh at all.

--- ORIGINAL DESCRIPTION ---
Origin: user instruction 2026-08-02, "add a mechanism to validate messages in the agent script before accepting them" -- split into two layers. CRYPTO-10 covers the CRYPTO-verification layer (MAC/decrypt, wired in once the CRYPTO epic lands). THIS task covers the layer underneath that and independent of it: basic envelope/schema validation of what a shell wrapper accepts from the server BEFORE it hands the payload to the calling agent, needed from day one (AGENTIF-3/4/5/6/7/8 all parse server JSON today with no such check specified).

A shell wrapper (bash + jq/curl) that trusts server JSON blindly is fragile and, on a compromised/misbehaving/relay-hopped bus (invariant 2: multiple buses relay to each other -- a message may have crossed a bus you don't directly trust), a foot-gun: a malformed or unexpected-shaped response fed straight into `msg=$(...)`, `eval`, or interpolated into a follow-up curl call can corrupt state or worse. Scope, for every scripts/bus-*.sh wrapper that consumes a server response (bus-agents.sh, bus-broadcast.sh, bus-send.sh, bus-wait.sh, bus-leave.sh, bus-peer.sh):
- Validate the response is well-formed JSON before doing anything else with it (a wrapper must not treat a non-2xx or non-JSON body as if it were data).
- Validate the expected top-level shape/required fields are present and are the expected JSON type (e.g. `id` is a string, `messages` is an array) before extracting and printing/using any field -- reject with a clear non-zero exit and a stderr message on anything else, printing nothing usable to stdout on failure (same "fail loud, fail closed" contract CRYPTO-10 uses for the crypto layer, so the two layers compose instead of conflicting).
- Cap/guard against absurd sizes (a pathological huge response should not be slurped unbounded into a bash variable).
- Document the validation contract (accepted shape, exit codes) in AGENT_PROTOCOL.md per invariant 7 -- ships in the same task as the wrapper behaviour it documents.

Does NOT cover cryptographic verification, decryption, or replay/sender-identity checks -- that is CRYPTO-10, layered on top of this once it lands. This task is not gated on the CRYPTO epic and should land first since every wrapper needs it regardless of whether E2E crypto is ever enabled.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


_None recorded._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [AGENTIF-3](../../AGENTIF/AGENTIF-3--6f1ebe02/task.md) — AGENTIF-3: scripts/bus-agents.sh + AGENT_PROTOCOL.md entry (superseded)
- [CRYPTO-10](../../CRYPTO/CRYPTO-10--68ff679d/task.md) — CRYPTO-10: \`agent-bus verify\` helper + scripts/bus-*.sh validate-before-accept + AGENT_PR… (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [AGENTIF-9](../../AGENTIF/AGENTIF-9--4f78ecb1/task.md) — AGENTIF-9: Envelope/schema validation in scripts/bus-*.sh before accepting a server respo… (cancelled)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
