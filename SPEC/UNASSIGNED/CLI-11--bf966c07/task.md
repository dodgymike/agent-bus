# CLI-11: export the bus signing public key from the operator CLI

| Field | Value |
| --- | --- |
| Public id | `bf966c07-5f99-4fe6-bb23-52868ed04c33` |
| Key | CLI-11 |
| Epic | [UNASSIGNED](../epic.md) |
| Status | done |
| Priority | P1 |
| Component | cli |
| Section | backlog |
| Tags | — |
| Created | 2026-08-14T11:08:39.812734+00:00 |
| Updated | 2026-08-14T13:17:20.916072+00:00 |
| Completed | 2026-08-14T13:17:20.916055+00:00 |

## Proof command

```sh
go test -race -run TestCLIExportBusSigningPublicKey ./cmd/agent-bus
```

## Description

P1 blocking follow-up for RELAY-25. Add a compiled `cmd/agent-bus` operator subcommand (BINARY DECIDED 2026-08-14, corrected from an earlier `agent-busctl` record -- see below) that reads the existing bus signing PUBLIC key from the selected data directory and exports it for federation pinning/verification. It MUST never expose or print the private key; refuse missing/malformed/wrong-type key material with stable documented nonzero exit codes. Serve human output and --json, no prompts, and document the command, JSON schema, and exit codes in AGENT_PROTOCOL.md in the same task. Existing bus identity/key source is authoritative; do not mint client ids or weaken TLS pinning. Owner direction recorded on RELAY-25 2026-08-14. This task blocks RELAY-25.

BINARY DECISION, RECORDED SO IT IS NOT RE-LITIGATED (coordinator, 2026-08-14): `cmd/agent-bus`, NOT `cmd/agent-busctl`. `agent-busctl` is a pure HTTP client importing only `client/`, with no data-dir or dirlock plumbing; this command reads LOCAL key material under an EXCLUSIVE lock -- the `invite mint` / `peer add` shape, both of which already live in `cmd/agent-bus`. codex-1 is separately fixing scripts/fed-smoke.sh:158 (which currently calls $CTL, the agent-busctl variable) to match.

CORRECTION 2026-08-14 -- MUST NOT MINT (found by the implementer, who correctly refused to write code until this was resolved): internal/buscert.LoadOrCreate (internal/buscert/buscert.go:281-311) GENERATES a fresh certificate, TLS key and signing key when all three files are absent -- there is no load-only entry point in the package today. This command MUST refuse a data dir with no existing key material rather than silently minting a brand-new bus identity via LoadOrCreate -- check for existing key material FIRST (stat the three files LoadOrCreate itself checks) and refuse with a stable nonzero exit code before ever calling LoadOrCreate, so this command can never be the thing that mints a bus's identity. Minting a federation-wide identity event as a side effect of an EXPORT command would be exactly backwards. A test/proof must assert BOTH the nonzero exit AND that the data directory is unchanged afterward -- asserting only the exit code would pass even if the command minted material and then failed for an unrelated reason.

CORRECTION 2026-08-14 -- OFFLINE ONLY, not "against a running bus": the original wording ("Exercise through the compiled CLI against a running bus") is impossible for this command's actual shape -- `invite mint` and `peer add` take the data directory's EXCLUSIVE lock, and a running bus holds it, so this command (which shares that lock discipline) cannot run against a live bus either. scripts/fed-smoke.sh already exports with all three buses stopped, which is the correct and only supported shape; exercise this command the same way -- bus stopped, direct data-dir access, never against a live listener. Do not use curl, PEM scraping, raw WAL reads, or scripts/bus-*.sh wrappers other than the sanctioned bus-serve.sh lifecycle calls needed to seed real key material for a proof.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [RELAY-25](../../RELAY/RELAY-25--10491a01/task.md) — RELAY-25: fed-smoke.sh: the epic's deliverable -- three-bus loopback federation smoke test (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [4b51635d-336f-4f25-94c2-64c53578859d](../../AGENTIF/AGENT_PROTOCOL.md-is-missing-the-CLI-11-key-export-publi--4b51635d/task.md) — AGENT_PROTOCOL.md is missing the CLI-11 (key export-public) and CLI-6 (log) sections -- b… (todo)
- [CLI-11-FU-BUSIDBOUND](../CLI-11-FU-BUSIDBOUND--82f9e452/task.md) — CLI-11-FU-BUSIDBOUND: internal/ids reads the bus-id file with an unbounded os.ReadFile (todo)
- [CLI-11-FU-LOADONLY](../CLI-11-FU-LOADONLY--b140724b/task.md) — CLI-11-FU-LOADONLY: load-only accessors for bus key material and the bus id, so a READ ca… (todo)
- [CLI-11-FU-STATERR](../CLI-11-FU-STATERR--555967a6/task.md) — CLI-11-FU-STATERR: invite mint tells an operator to restore a file that is present but un… (todo)
- [CLI-6](../../CLI/CLI-6--47001cb4/task.md) — CLI-6: log -- read the append-only audit log (metadata only; also absorbs the WAL-dumper… (done)
- [CLI-6-FU-FOLLOW](../../CLI/CLI-6-FU-FOLLOW--03a09254/task.md) — CLI-6-FU-FOLLOW: decide what log --follow means for an offline, dirlock-taking reader (todo)
- [RELAY-24-BLOCKER-HUBINGEST-FU-AUDITHASH-DOC](../../RELAY/RELAY-24-BLOCKER-HUBINGEST-FU-AUDITHASH-DOC--7126f08b/task.md) — RELAY-24-BLOCKER-HUBINGEST-FU-AUDITHASH-DOC: Record the relayed audit content-hash decisi… (done)
- [de0fc1df-a948-4b44-95a4-4b9d01cab267](../../TOOLING/DECISIONS.md-HTML-comment-section-fences-are-imbalanced--de0fc1df/task.md) — DECISIONS.md HTML-comment section fences are imbalanced (6 BEGIN / 8 END) -- introduced b… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
