# Derive the bus fingerprint from the certificate, not the log; correct the CONTRACTS-CLI expiry claim

| Field | Value |
| --- | --- |
| Public id | `10e93262-8e34-4738-b435-bfe23d880057` |
| Key | _(null in the export)_ |
| Epic | [MTLS](../epic.md) |
| Status | done |
| Priority | P1 |
| Component | security |
| Section | backlog |
| Tags | — |
| Created | 2026-08-07T21:00:46.117556+00:00 |
| Updated | 2026-08-23T19:10:32.246210+00:00 |
| Completed | 2026-08-23T19:10:32.246192+00:00 |

## Proof command

```sh
bash -lc 'set -euo pipefail; T=$(mktemp -d); port=$(python3 -c "import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()"); export AGENT_BUS_RUN_DIR="$T/run" AGENT_BUS_DATA_DIR="$T/data" AGENT_BUS_LISTEN="127.0.0.1:$port" AGENT_BUS_LOG_LEVEL=info; trap "scripts/bus-serve.sh stop >/dev/null 2>&1 || true" EXIT; fake=ba5eba11ba5eba11ba5eba11ba5eba11ba5eba11ba5eba11ba5eba11ba5eba11; mkdir -p "$AGENT_BUS_RUN_DIR"; (while :; do printf "bus_cert_fingerprint=%s\n" "$fake" >>"$AGENT_BUS_RUN_DIR/agent-bus.log"; sleep 0.02; done) & poison=$!; out=$(scripts/bus-serve.sh start 2>&1); kill "$poison" 2>/dev/null || true; wait "$poison" 2>/dev/null || true; printed=$(printf "%s\n" "$out" | sed -n "s/^agent-bus:[[:space:]]*fingerprint[[:space:]]*//p" | head -1); cert=$(openssl x509 -in "$AGENT_BUS_DATA_DIR/bus-tls.crt" -outform DER | sha256sum | cut -d" " -f1); test -n "$printed"; test "$printed" = "$cert"; test "$printed" != "$fake"'
```

## Status note

Commit d34f73c landed P1-1 (bus-serve.sh fingerprint derived from certificate, not writable log) and P1-1b (read_pid validation) -- proof-check verdict=PASS class=other, live-attack proof confirms the wrapper resists a forged bus_cert_fingerprint= log line. P1-2 (CONTRACTS-CLI.md expiry-claim rewrite) is staged in the working tree but not yet committed. Task stays in_progress until CONTRACTS-CLI.md lands in a commit.

## Description

Two P1 security-gate findings already in main at commit 9f2878a (they reached main via a pathspec-less `git commit --amend` while the code was gated CHANGES-REQUESTED).

P1-1: scripts/bus-serve.sh derived the paste-ready --bus-fingerprint value by grepping bus_cert_fingerprint= out of the mutable log file and taking tail -1. A local attacker who can write that log (e.g. pre-creating /tmp/agent-bus/) makes the operator pin the ATTACKER's certificate -- the exact MITM that "no trust-on-first-use" exists to prevent. Fix: derive the fingerprint from $CERT_FILE (the authoritative self-signed leaf) and delete the log-scrape path entirely.

P1-1b (same file, pre-existing): read_pid did not validate the pidfile contents, so -1 could reach `kill -TERM -1` (signals every process the user owns). Fix: accept only a plain positive decimal.

P1-2: CONTRACTS-CLI.md asserted client-side certificate expiry is NOT checked and that MTLS-EXPIRY is "in flight, not in main", citing a proof that `git show HEAD:client/pin.go` matches no NotAfter/ErrBusCertificateExpired/ParseCertificate. It matches all three at HEAD -- MTLS-EXPIRY landed in 9f2878a. Fix: rewrite the paragraph to state what is true at HEAD.

Files: scripts/bus-serve.sh, CONTRACTS-CLI.md (+ AGENT_LOG.md append).

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **follow-up** [320d4a73-8b75-4f87-afca-ba23ec69a590](../No-regression-guard-exists-for-the-bus-fingerprint-trust--320d4a73/task.md)
- **follow-up** [4a6e7001-ca2a-430a-a5e6-39e922d7325f](../../DOCS/CONTRACTS-AGENT.md-AGENT_PROTOCOL.md-document-the-remove--4a6e7001/task.md)
- **follow-up** [88781750-0005-4c2f-8375-2d93dc1560b8](../../DOCS/DECISIONS.md-1302-cites-a-superseded-bus-serve.sh-line-f--88781750/task.md)
- **follow-up** [ae594fa8-03bb-4d51-aa31-641f5ddcae66](../../AGENTIF/RUN_DIR-created-with-no-ownership-check-enables-binary-s--ae594fa8/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [MTLS-EXPIRY](../MTLS-EXPIRY--3604af80/task.md) — MTLS-EXPIRY: the client never checks the pinned bus certificate validity period -- the 36… (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [320d4a73-8b75-4f87-afca-ba23ec69a590](../No-regression-guard-exists-for-the-bus-fingerprint-trust--320d4a73/task.md) — No regression guard exists for the bus-fingerprint trust-anchor fix (todo)
- [4a6e7001-ca2a-430a-a5e6-39e922d7325f](../../DOCS/CONTRACTS-AGENT.md-AGENT_PROTOCOL.md-document-the-remove--4a6e7001/task.md) — CONTRACTS-AGENT.md/AGENT_PROTOCOL.md document the removed log-scrape as bus-serve.sh star… (todo)
- [7befde72-488e-4cf4-a05b-b16e2c2ffd15](../../PROCESS/Integrator-flips-the-task-to-done-atomically-after-a-suc--7befde72/task.md) — Integrator flips the task to done atomically after a successful commit -- close the commi… (todo)
- [88781750-0005-4c2f-8375-2d93dc1560b8](../../DOCS/DECISIONS.md-1302-cites-a-superseded-bus-serve.sh-line-f--88781750/task.md) — DECISIONS.md:1302 cites a superseded bus-serve.sh line for the plaintext-probe follow-on (todo)
- [ae594fa8-03bb-4d51-aa31-641f5ddcae66](../../AGENTIF/RUN_DIR-created-with-no-ownership-check-enables-binary-s--ae594fa8/task.md) — RUN_DIR created with no ownership check -- enables binary swap and pidfile symlink attack (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
