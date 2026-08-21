# DOC-TRUTH_DEEPDIVE — where this repo's documentation lies about its own code

**Operator-requested 2026-08-16. AUDIT ONLY — nothing was fixed, nothing was committed.**
`git status --porcelain` was identical before and after this investigation except for this file.

---

## 0. How to read this document

- **Every finding names `file:line` on BOTH sides** — the doc line and the code line that refutes it.
  A claim with only one side is labelled a HYPOTHESIS.
- **Severity is judged by who gets hurt**, not by how wrong the sentence is. An agent or operator
  driven into a broken state outranks a maintainer reading a wrong number.
- **Four classes**, as requested: `STALE-NOTYET` (true when written, closed since, never relinked),
  `OVERCLAIM` (says something is done that is not), `WRONG-DETAIL` (true claim, wrong
  number/file/flag/route), `RETIRED-TOOLING` (points at a wrapper invariant 7 retired).
- **Provenance is marked.** `[V]` = I verified it myself at the code. `[V-RUN]` = I proved it by
  running the binary. `[S]` = surfaced by a sub-agent and spot-checked by me. `[S-only]` = surfaced
  by a sub-agent, NOT independently re-verified — treat as a strong lead, not a settled fact.

### The concurrency caveat that changed two findings

Two agents were editing this worktree throughout. At audit time the following were **dirty or
untracked**:

```
 M .gitignore  AGENT_LOG.md  AGENT_PROTOCOL.md  CONTRACTS-AGENT.md  CONTRACTS-CLI.md
 M DECISIONS.md  client/client.go  client/config.go  client/session.go
 M cmd/agent-busctl/root.go  cmd/agent-busctl/whoami.go
?? client/sessionstore.go  client/sessionstore_test.go  cmd/agent-busctl/sessionlogout.go
```

Every finding in a dirty file was **re-verified against `git show HEAD:<path>`**, and line numbers
for those files are given as `HEAD:<n>` where they differ from the worktree. This caught one false
positive (§4, REFUTED-2) and one near-miss, both recorded below rather than silently dropped.

---

## 1. Symptom

The repo's own header warning describes the defect class exactly:

> *"a stale 'not yet implemented' note is more dangerous than no note, because it reads as freshly
> checked."* — `CLAUDE.md:27-29`

Between **2026-08-07 and 2026-08-15** six load-bearing capabilities shipped in quick succession —
the TLS listener, mTLS binding, the mTLS cross-check, the durable roster, invite-gated enrolment, and
relay ingress + egress + onward multi-hop. The prose that had honestly described each as *unbuilt*
was written into **eleven different files**, and in most cases the shipping task updated the code and
the composition root but not the paragraphs elsewhere that had been reasoning about the absence.

The result is not scattered nits. It is **five coherent false narratives**, each repeated in three to
seven independent places, so a reader who cross-checks finds mutual confirmation of a falsehood:

| Narrative | Times asserted | Truth |
| --- | --- | --- |
| "Enrolment is not invite-gated; an un-invited enrol is still accepted" | 16 | 403 since `3cedcb7` |
| "Relay is written but registered on no mux / imported by nothing / not on any wire" | 17 | Mounted, live, three-bus traffic |
| "The client verifies message signatures on read" | 3 | Nothing on the read path verifies |
| "The bus does not request a client certificate" | 2 | `tls.RequestClientCert` since `a97f854` |
| "`cmd/agent-bus` injects the MEMORY roster / `internal/store` is a stub" | 3 | Durable since AUTH-7 |

**Two symptoms are worse than a stale file:**

1. **One stale claim is not in a file at all — it is served over HTTP to every caller**
   (Finding 3, proven below).
2. **The invite gate (`3cedcb7`) updated exactly one doc — `AGENT_PROTOCOL.md` — and left every
   other agent entry point behind.** All four of the first things a new agent touches now lead to a
   403: the `README.md` Quickstart (#1), `agent-busctl --help`'s closing line (#1b), the
   ready-to-paste command `bus-serve.sh` prints on every successful start (#1c), and
   `client/enrol.go`'s doc comment for embedders (#35).

---

## 2. Severity-ranked findings

### S1 — An agent or operator following the doc lands in a broken state

| # | Doc `file:line` | CLAIMS | Code ACTUALLY does | Evidence `file:line` | Class | Prov |
| --- | --- | --- | --- | --- | --- | --- |
| 1 | `README.md:113-114` | Quickstart: `agent-busctl … enrol --name planner` is how you get on the bus | **HTTP 403, exit 4**, `"this bus is invite-only … present it with agent-busctl enrol --invite-file <path>"`. The quickstart has no `--invite-file` step and no `agent-bus invite mint` step. | `cmd/agent-bus/main.go:66`; `internal/auth/service.go:631`; `internal/httpapi/auth.go:854` | STALE-NOTYET | [V-RUN] |
| 2 | `README.md:96-100` | "**What works today**": `curl -s localhost:8080/healthz` → `{"status":"ok"}` | Returns `Client sent an HTTP request to an HTTPS server.` — there is **no plaintext listener** (invariant 11). `curl` still **exits 0**, so a script "checking" this passes. | `cmd/agent-bus/main.go:1503` + `tlslisten.go` (TLS-only); `main.go:1571` logs `scheme=https tls=true` | STALE-NOTYET | [V-RUN] |
| 3 | **`internal/httpapi/discovery.go:271`** (a code string **served on the wire**) | `GET /v1/discovery` limitation 5: *"SINGLE BUS ONLY. Cross-bus relay is not served yet; a recipient on another bus is a 404."* | Cross-bus relay is served: ingress `7095231`, egress `9701611`, onward `d5018a6`. `CONTRACTS-HTTP.md:86` calls these "**verified-true-of-this-build** negative claims". | `internal/httpapi/peermount.go:304-306`; `cmd/agent-bus/main.go:585,1441` | STALE-NOTYET | [V-RUN] |
| 4 | `CONTRACTS-HTTP.md:130-131` | "the bus does **not** request a **client** certificate, so TLS authenticates the BUS to the caller and never the caller to the bus" | `ClientAuth: tls.RequestClientCert`. A presented certificate is **bound to the agent id** and a session token for a bound agent is **refused 403** unless it arrives over that certificate. | `cmd/agent-bus/tlslisten.go:152`; `internal/httpapi/crosscheck.go` (whole file) | WRONG-DETAIL | [V] |
| 5 | `client/messages.go:161-162` **and** `:384-386` | "*Read has already done that, and a message that reaches a caller in `Batch.Messages` is one that verified*" / "Messages are the **VERIFIED** messages" | **Nothing on the read path verifies.** An unsigned message from a stranger is delivered body-and-all; `Batch.Rejected` is never populated. | `client/wedge_test.go:495-525` — proof below | OVERCLAIM | [V-RUN] |
| 6 | `client/keyring.go:21-24` | On a bus where nobody exchanged keys "EVERY message is unverifiable, **every body is discarded**, and the cursor advances" | Same root as #5 — **no body is discarded**; everything is delivered. The client package documents *fail-closed* while shipping *fail-open*. | `client/wedge_test.go:495-525` | OVERCLAIM | [V] |
| 7 | `cmd/agent-busctl/enrol.go:62` (**CLI `--help`**) | "An invite is SINGLE-USE, expiring and **revocable**." | `agent-bus invite` accepts **only `mint`**. `Store.Revoke` exists at `internal/invite/store.go:449` with **zero non-test callers**. No `/v1/invite*` route. | `cmd/agent-bus/invite.go:121`; `cmd/agent-bus/invite.go:646` ("*revoke it (INVITE-REVOKE) once that surface exists*") | OVERCLAIM | [V] |
| 8 | `cmd/agent-bus/invite.go:297` | On a mint whose blob failed to write: *"The secret is now UNRECOVERABLE — **revoke** `<id>` and mint another."* | The operator has a leaked, live, single-use credential and **the remedy named does not exist**. Same for `client/invite.go:237`. | as #7 | OVERCLAIM | [V] |
| 9 | `client/store.go:272`, `client/client.go:577`, `client/keyring.go:181` | Error **remedies** tell the operator to run `agent-busctl keygen` | No `keygen` subcommand. Registry is exactly: enrol, whoami, use, logout, session, pin, client-cert, agents, send, broadcast, watch. | `cmd/agent-busctl/root.go:160-175` | OVERCLAIM | [V] |
| 10 | `client/keyring.go:23`, `client/client.go:536`, `client/canonical.go:504`, `client/messages.go:323`, `client/config.go:183` | Instructions to run `agent-busctl trust <agent-id> <base64-key>` | No `trust` subcommand. **This is the only documented way to populate the trust store**, so the doc describes the whole verification workflow through a command that cannot be run. | `cmd/agent-busctl/root.go:160-175` | OVERCLAIM | [V] |
| 11 | `CONTRACTS-HTTP.md:43`, `:90-91`, `:506`, `:726-737`, `:979` | Six passages: enrolment "is NOT gated by this change" / "`invite_required` … is `false` in this build (**verified**)" / "`agent-busctl enrol --invite` is **refused locally** … an agent **cannot redeem an invite today**" | All false. `enrolment_invite_required=true`; live `/v1/discovery` returns `"invite_required": true`; `--invite-file` is the shipped and only route onto a bus. `:90-91` also cites a literal `InviteRequired: false` in `discovery.go` that **does not exist** — the value is derived at `internal/httpapi/server.go:297`. | `cmd/agent-bus/main.go:66,797,811`; `internal/httpapi/discovery.go:325`; `cmd/agent-busctl/enrol.go:184` | STALE-NOTYET + WRONG-DETAIL | [S] |
| 12 | `CONTRACTS-CLI.md:136`, `:1768-1770`, `:1865-1868` | "`POST /v1/enroll` still accepts callers with no invite" and — explicitly — "**Do not document or build on invite-only enrolment as live.**" | Inverted. `:1768` is a standing instruction to build on the opposite of the truth. | as #11 | STALE-NOTYET | [S] |
| 13 | `CONTRACTS-ONDISK.md:1209-1210` | "**enrolment is still NOT gated**: an enrolment carrying NO invite is unaffected and still accepted" | as #11 | as #11 | STALE-NOTYET | [S-only] |
| 14 | `CONTRACTS-HTTP.md:38-49` (routes table) | Nine `POST /v1/enroll` rows; the only 403 row is body `{"error":"invite not accepted"}` | A **second, undocumented 403** with a different body is the one every un-invited caller now gets (`auth.go:854`). An agent branching on the documented body mis-handles the common case. | `internal/httpapi/auth.go:854` | WRONG-DETAIL | [S] |
| 1b | **`cmd/agent-busctl/root.go` HEAD:412-413** — the **closing line of `agent-busctl --help`** | "Enrolment **is becoming** invite-only and the bus **is becoming** TLS-only; **both are in flight.** See CONTRACTS-CLI.md for what is stable today." | Both **shipped** — invite-only `3cedcb7` (2026-08-15), TLS-only `MTLS-LISTENER` (2026-08-07). The one sentence a new agent reads to calibrate what it can rely on tells it the two hardest gates are optional. Confirmed at HEAD (file is dirty; worktree `:431-432`). | `cmd/agent-bus/main.go:66`; `cmd/agent-bus/tlslisten.go` | STALE-NOTYET | [V] |
| 1c | **`scripts/bus-serve.sh:410`** — printed on **every successful `start`** | Ready-to-paste: `agent-busctl enrol --bus https://${PROBE_ADDR} --bus-fingerprint ${fp} --name <name>` | 403. **`bus-serve.sh` contains the string "invite" zero times.** This is the one surviving `bus-*.sh` wrapper (invariant-7 exempt, server lifecycle), so it is the sanctioned way to bring a bus up — and its success message hands you a command that cannot work. `CONTRACTS-AGENT.md` HEAD:48 documents the line approvingly. | `grep -c invite scripts/bus-serve.sh` → **0**; `internal/httpapi/auth.go:854` | STALE-NOTYET | [V] |
| 1d | **`cmd/agent-busctl help broadcast`** (whole help text) | "send one message to **every other agent on the bus**", with runnable examples and a durability claim. **The word 501 does not appear anywhere in it**; nor does "refused", except about empty bodies and size limits. | `POST /v1/broadcast` answers **501 unconditionally** and never decodes the body. `AGENT_PROTOCOL.md:761-780`, `README.md:21-23` and `CONTRACTS-CLI.md:1061` all document this correctly — **only the CLI's own help, the surface invariant 7 makes authoritative for an agent, omits it.** | `internal/httpapi/messages.go:422-466`; verified by running the compiled binary | OVERCLAIM | [V] |
| 1e | `docs/THREE-BUS-DOCKER.md:448-455`, under a `:446` preamble calling these "real, currently true" | "`scripts/fed-smoke.sh` exits 1, and **that is a defect in the test** … It asserts one identical `message_id` across all three buses … **Do not treat that script's exit status as a green check for your deployment.**" | `72f0cd1` rewrote it to correlate by content digest **12 minutes before** `9938eb2` added this paragraph. `scripts/fed-smoke.sh:260` — "A MESSAGE ID IS NOT A CROSS-BUS CORRELATOR … Do not reintroduce it"; `:328-345` selects on `.content_sha256`. **The doc tells the operator to ignore the only end-to-end federation check in the repo** — so a real failure reads as the documented test defect. | `scripts/fed-smoke.sh:12-13,260,328-345` | STALE-NOTYET + OVERCLAIM | [V] |
| 1f | `CONTRACTS-AGENT.md` HEAD:29, :104 | `fed-smoke.sh` "**currently expected to fail** at the first unavailable step" | `scripts/fed-smoke.sh:12-13`: "**Each of the steps below has since landed**; the list is kept because it names the compiled command each stage depends on." | `scripts/fed-smoke.sh:12-13` | STALE-NOTYET | [V] |
| 1g | `CONTRACTS-AGENT.md` HEAD:48 | `bus-serve.sh start` prints "the certificate fingerprint **scraped from the log** (`bus_cert_fingerprint=…`)" | The script does the **opposite, deliberately**: `bus-serve.sh:402` → `cert_fingerprint()` parses **the certificate file** with `openssl x509 -fingerprint -sha256` and fails closed. `:399-401` — "cert_fingerprint reads the CERTIFICATE, never the log"; `:415` — "**Do NOT** take it from `${LOG_FILE}`: anyone who can write there can plant a `bus_cert_fingerprint=` line in it." **The doc documents the trust-anchor substitution the script exists to prevent**, and a reader copying it into their own tooling reintroduces it. | `scripts/bus-serve.sh:160-196,399-415` | WRONG-DETAIL | [S] |
| 1h | `client/transport.go:429-430` | Every 403 — including the invite-gate refusal — carries the remedy "retry, and if it persists re-enrol with `agent-busctl enrol`" | Retrying never succeeds and re-enrolling reproduces the same 403. `AGENT_PROTOCOL.md:526` correctly says "retrying it unchanged will never succeed". **An agent branching on the error text loops forever** on the single most common failure a new agent hits. | `internal/httpapi/auth.go:854`; `AGENT_PROTOCOL.md:526` | WRONG-DETAIL | [S] |

### S2 — A reviewer, security auditor or maintainer is misled into the wrong action

| # | Doc `file:line` | CLAIMS | Code ACTUALLY does | Evidence `file:line` | Class | Prov |
| --- | --- | --- | --- | --- | --- | --- |
| 15 | `CONTRACTS.md:110-125` | "`internal/relay` registers NO handler on any mux, authenticates NO peer, and is imported by **NOTHING** outside itself — enforced by `internal/relay/guards_test.go`, which fails the build if any other package imports it" **and the standing directive** "**Do not add `/v1/peer/relay` or `/v1/peer/roster` to `CONTRACTS-HTTP.md` either** — there is nothing there yet to add" | Three routes mounted; imported by **six** non-test files. The guard does the **opposite** of what is claimed — its `wiringSites` explicitly ALLOWS `internal/httpapi` and `cmd/agent-bus`. The directive tells the next documentation agent **not to document a live, network-reachable, security-sensitive ingress**. | `internal/httpapi/peermount.go:304-306`; `internal/relay/guards_test.go:52-55`; importers: `cmd/agent-bus/{main,relaywiring,relayegress,relaydial,peer}.go`, `internal/httpapi/peermount.go` | STALE-NOTYET | [V] |
| 16 | `internal/relay/handshake.go:131-132`, `internal/relay/relayhttp.go:133-134`, `internal/relay/rosterhttp.go:73-77` | Three identical **prohibitions**: "IT IS NOT REGISTERED ON ANY MUX **AND MUST NOT BE** until INVITE-PEERGUARD and MTLS-RELAYGUARD land — it authenticates nothing." `rosterhttp` adds: this surface "**MUST NOT** be served ungated". | All three **are** registered (RELAY-20). The mount site knows and compensates — `peermount.go:12-16` says so — but the three handler files were never updated. A security reviewer reading them concludes the mount is a live P0 and may revert it; an implementer concludes the peer plane is unauthenticated when `RequirePeerPrincipal` wraps it. | `internal/httpapi/peermount.go:304-306`, `:33-38` | STALE-NOTYET | [V] |
| 17 | `cmd/agent-bus/peer.go:132-138` | "**SCOPE: CONFIGURATION ONLY. NOTHING SERVES THIS YET.** … `relay.Handler` is registered on no mux and `PeerStore` is constructed **nowhere in the server** (RELAY-24 is the composition root). This command configures a topology that is **not yet served**." | `cmd/agent-bus/main.go:585` constructs the `PeerStore`; `:1441` hands the surface to `httpapi`. Said "plainly, because 'federation works now' is the easiest wrong conclusion to draw" — and it is now the right one. | `cmd/agent-bus/main.go:585,1441` | STALE-NOTYET | [V] |
| 18 | `internal/relay/message.go:30-32`, `:152-154`; `internal/relay/doc.go:448-449`; `PROTOCOL.md:1012-1018` | The relay envelope carries **no wire-protocol version field**, justified by: "because nothing serves this handler the format is **not yet on any wire**, so there is nothing to stay compatible with" / "relay registers no route and is **imported by nothing**". `PROTOCOL.md:1017` repeats it as a licence. | **This is a latent landmine, not a nit.** The format IS on the wire between running buses (fed-smoke drives three). **No relay wire-protocol version was ever reserved**, and the paragraph that would have forced the reservation is the one that is stale. Anyone acting on it changes the envelope and silently breaks live federation with no version to negotiate on. | `internal/httpapi/peermount.go:305`; `internal/relay/message.go:99`; six importers as #15 | STALE-NOTYET | [V] |
| 19 | `internal/relay/peerstore.go:47-51` | "registers no route and no subcommand: **nothing in a running bus reads it yet** … the offline `agent-bus peer` subcommand that writes one … [is a] separate task" | `agent-bus peer add\|list\|remove` ships; `main.go:585` constructs the store in the running bus; `:610-611` registers it as a WAL applier. | `cmd/agent-bus/peer.go`; `cmd/agent-bus/main.go:585,610-611` | STALE-NOTYET | [V] |
| 20 | `internal/auth/service.go:502-505` | "**`cmd/agent-bus/main.go` still injects the MEMORY roster.** Until the wiring task lands, the durable path exists and is tested but is not the one a deployed bus takes, so **no caller may present enrolment on the shipped binary as durable.**" | `main.go:504` injects `auth.NewWALRoster(lg)`, attached to the WAL at `:755`. AUTH-7 landed and is proved end-to-end by a kill-and-restart process test. This is a **durability claim, inverted** — it tells a reliability reviewer the shipped bus loses enrolments across restart. `main.go:817-825` even documents the correction, one file away. | `cmd/agent-bus/main.go:504,755,817-825`; `cmd/agent-bus/enrolrestart_test.go:240` | STALE-NOTYET | [V] |
| 21 | `CONTRACTS-ONDISK.md:479-484`, `:489-491` | "the `Applier` passed to `wal.Open` is `nil`. There is no in-memory serving copy yet — **`internal/store` is still a stub** … it rebuilds no application state, because none exists." | `main.go:554-630` builds a multiplex applier; `internal/store` is a full package at `RecordVersion = 2`. Replay rebuilds roster, invite table, peer store, outbox and message store. | `cmd/agent-bus/main.go:554,626,630`; `internal/store/message.go:50` | STALE-NOTYET | [S] |
| 22 | `CONTRACTS-HTTP.md:611-612` **and** `internal/auth/session.go:466-472` | "the seam AUTH-2's middleware **will** wrap; **nothing enforces it on any route yet**" / "This task deliberately does NOT wire it into any middleware and enforces the token on **NO route**" | `internal/httpapi/authmw.go:401` calls `Authenticate` on every non-allow-listed, non-peer request. `CONTRACTS-HTTP.md:722-724` in the **same file** says the opposite. | `internal/httpapi/authmw.go:401` | STALE-NOTYET | [S] |
| 23 | `CONTRACTS-HTTP.md:626, :628` | Sessions have "**deliberately no per-agent cap**"; "nothing caps active sessions per agent … That gap … is filed as AUTH-1-FU-ACTIVECAP" — and, in the same sentence, "while **enrolment is itself unauthenticated**" | `DefaultMaxActiveSessionsPerAgent = 32`, **enforced**, with **no eviction**: an agent at the cap stays refused for up to `SessionLifetime` (1 h). Enrolment is invite-gated. **This pair caused a real production lockout on 2026-08-15.** | `internal/auth/service.go:61`; `internal/auth/session.go:457-458` | STALE-NOTYET | [V] |
| 24 | `INVARIANTS.md:251` | Invariant 11: "**Certificate expiry is NOT checked** — a real gap owned by `MTLS-VERIFY`" | Checked, and wired into the live handshake: `pinnedTLSConfig` → `pinVerifier` → `verifyPinnedBusCertificate` → `checkBusCertificateValidity`, which returns `BusCertificateExpiredError` on `x509.Expired`. The **ownership** is also wrong: `MTLS-VERIFY` is about `bus-serve.sh`'s health probe. | `client/pin.go:261,367,410,503,527-537` | STALE-NOTYET + WRONG-DETAIL | [V] |
| 25 | `INVARIANTS.md:72-74`; `CLAUDE.md:51-52`; `AGENTS.md:51-52` | Invariant 3's allow-list: "Every route authenticates except **enrolment, session begin/complete, `/healthz` and `/v1/info`**" — five entries | `unauthenticatedRoutes` has **six**; `/v1/discovery` is missing from all three docs. `authmw.go:28` literally says "Six entries". A security reviewer auditing the anonymous surface against the invariant files a false P0 — or "fixes" it, which `authmw.go:37-45` explains would be circular. | `internal/httpapi/authmw.go:76-83`; `internal/httpapi/discovery.go:76` | WRONG-DETAIL | [S] |
| 26 | `CLAUDE.md:20-22`; `AGENTS.md:20-22` | "the server REQUESTS but never REQUIRES a client certificate … one that IS presented **authenticates nobody by itself**" | On the **peer plane the certificate alone authorises** — `RequirePeerPrincipal` reads no token and no header. This is the authentication mechanism for the entire federation ingress. `crosscheck.go:73-76` says it outright. | `internal/httpapi/peerprincipal.go:9-19,39-57`; `internal/httpapi/peermount.go:304-306` | STALE-NOTYET | [S] |
| 27 | `INVARIANTS.md:212-213`; `CLAUDE.md:99-100`; `AGENTS.md:99-100`; `cmd/agent-busctl/pin.go:71` (**CLI `--help`**) | "Rotation serves TWO certificates during rollover and must never require re-enrolment" — and the CLI help states it in the **present indicative**: "A bus rotating its certificate serves BOTH the outgoing and the incoming one for the duration of the rollover." | The **server half does not exist**: `Certificates: []tls.Certificate{cert}`, exactly one, no `GetCertificate`. `internal/buscert/buscert.go:65-67` — "there is no rotation machinery yet … this expiry is a **SCHEDULED OUTAGE**", and on expiry the remedy is to **re-issue every invite**. The *client* accept-set is real, which is what makes this easy to mis-read as done. | `cmd/agent-bus/tlslisten.go:134`; `internal/buscert/buscert.go:65-67,423-424`; `internal/buscert/doc.go:88-90` | OVERCLAIM | [S] |
| 28 | `PROTOCOL.md:30-34` | The `ondisk-format-version` registry: "version 3 is the `agent-suffixes` file and version 4 is the `wal-index-floor` file" | **5, 6 and 7 are allocated and in use** and PROTOCOL.md mentions none of them (grep: zero hits). This is the **exact parallel-agent collision** the reservation rule exists to prevent — a reader taking this as the registry assumes 5 is free. `CONTRACTS-ONDISK.md:20` has the correct list. | `internal/hub/seqfloorfile.go:50` (5); `internal/relay/peerstore.go:1310` (6); `internal/wal/checkpoint.go:110` (7) | WRONG-DETAIL | [V] |
| 29 | `PROTOCOL.md:708-718` | "**KNOWN GAP — no pin can be established today, so relay ingest cannot be served at all** … **every relayed message is `ErrUnpeeredBus` by construction**" and "nothing on the startup path imports [`internal/buscert`]" | Pins are established offline by `agent-bus peer add -signing-key`; `PinnedBusSigningKeys` returns them; ingest is served. `buscert.LoadOrCreate` **is** on the startup path — and **`PROTOCOL.md:166-168` in the same file says so**. Anyone debugging a `403 unpeered_bus` concludes it is expected rather than a missing `-signing-key`. | `cmd/agent-bus/peer.go:628`; `internal/relay/trust.go:23`; `cmd/agent-bus/buscert.go:85` | STALE-NOTYET | [S] |
| 30 | `CONTRACTS-ONDISK.md:1486`, `:1496-1502` | "The `server started` line gained … **`tls=false`**"; "**`tls=false` is not a placeholder to be edited; it is the state**"; "`TestCmdDoesNotServeTLS` is an AST-level guard … to be DELETED by MTLS-LISTENER" | Startup logs `scheme=https tls=true tls_min_version=1.2 client_auth=requested`. `TestCmdDoesNotServeTLS` is already deleted (tombstone at `cmd/agent-bus/buscert_test.go:319`). `CONTRACTS-CLI.md:37-39` says the opposite — cross-file contradiction. | `cmd/agent-bus/main.go:1563-1571` (observed live) | STALE-NOTYET | [S] |
| 31 | `CONTRACTS-HTTP.md:279-281`, `:163`; `client/keyring.go:19` | "**Nothing registers a messaging public key at enrolment** (`auth.Service.Enrol` leaves `RosterEntry.MessagingPublicKey` **ZERO**)" | It **is** registered and durable. The real gap is that **no route serves it back** — `GET /v1/agents` returns only `{agent_id, name, enrolled_at}`. The conclusion ("out-of-band only") is right; the stated **reason** is wrong, so an implementer of CRYPTO-4 goes and builds registration that already exists. `CONTRACTS-HTTP.md:504` in the same file documents it correctly. | `internal/auth/service.go:799`; `internal/httpapi/auth.go:226-228,348`; `internal/httpapi/messages.go:338-347` | STALE-NOTYET | [V] |
| 32 | `internal/auth/service.go:382-384` | Per-agent certificate enforcement "is a **later task (MTLS-CROSSCHECK), not this one**" | `internal/httpapi/crosscheck.go` shipped — 320+ lines, five named residual gaps, wired into `authMiddleware`. | `internal/httpapi/crosscheck.go:1-17,220-316` | STALE-NOTYET | [V] |
| 33 | `internal/auth/roster.go:57-58` | "**Verification, when it lands**, accepts ANY binding that is not retired" | Landed. `crosscheck.go:296` — "ANY live binding satisfies it, not the newest". | `internal/httpapi/crosscheck.go:289-296` | STALE-NOTYET | [V] |
| 34 | `internal/auth/roster.go:133-134`; `internal/invite/doc.go:15-23` | "an un-invited enrolment, which **this build still accepts** (invariant 3's invite-only end state is not yet enforced)" / "**INVITE-GATE MADE THE INVITE LIVE, AND DID NOT TURN THE GATE ON** … enrolment is **NOT YET GATED** … and the discovery document says so" | Gate on since `3cedcb7`; the discovery document says the **opposite** (`"invite_required": true`, observed live). | `cmd/agent-bus/main.go:66`; live `/v1/discovery` | STALE-NOTYET | [V] |
| 35 | `client/enrol.go:63-66` | "nil is still accepted, because the bus **still accepts an un-invited enrolment** (`httpapi.DiscoveryEnrolment.InviteRequired` is **false today**)" | False. **`CLAUDE.md:31` flags this exact line as a known stale twin and it is still unfixed.** | `cmd/agent-bus/main.go:66` | STALE-NOTYET | [V] |
| 36 | `client/config.go` **HEAD:334,339** (worktree `:386,391`) | "The bus does **not serve TLS yet**, so refusing http outright would leave no working client at all … **DELETE THIS CASE ENTIRELY when the TLS listener ships.**" | Listener shipped 2026-08-07. The comment carries **its own deletion instruction**, unexecuted — the cheapest possible fix, unshipped for nine days. | `cmd/agent-bus/tlslisten.go`; `cmd/agent-bus/main.go:1503` | STALE-NOTYET | [V] |
| 37 | `internal/buscert/doc.go:80-81` | "**UNWIRED as shipped. Nothing imports it yet**; MTLS-LISTENER wires it." | Imported by `cmd/agent-bus/buscert.go:75`, `key.go:103`, `healthcheck.go:70`, `internal/relay/peerstore.go:84`. | those four lines | STALE-NOTYET | [V] |
| 38 | `CONTRACTS-CLI.md:488-497`; `CONTRACTS-ONDISK.md:1847-1848`, `:1902-1906`, `:2036-2047`, `:2302-2307`; `CONTRACTS-HTTP.md:893-894`, `:982`, `:1056-1059`, `:1063`; `CONTRACTS.md:239-248`, `:300-306` | Eleven further "relay not wired / not mounted / not constructed / no CLI flag / no pin served" passages | All false on the same evidence as #15-#19. `-peer-client-fingerprint` **does** exist (`cmd/agent-bus/peer.go:627`) and **is** documented at `CONTRACTS-CLI.md:414`, contradicting `CONTRACTS-ONDISK.md:1902-1906` in the next file over. | `cmd/agent-bus/main.go:585,610-611,916,1188,1441`; `internal/httpapi/peermount.go:276-306`; `cmd/agent-bus/relaydial.go:155-170` | STALE-NOTYET | [S] |
| 39 | `CLAUDE.md:137-147`; `AGENTS.md:135-145` | The whole "Runtime target: Docker Compose" rationale: the Go version "is chosen to satisfy the E2E-crypto requirements (**the Signal-style ratchet needs `crypto/ecdh`, which is go1.20+**)"; "**A local `go build` may therefore fail on this box while CI/the container is green**" | `Dockerfile:15` pins `golang:1.19.4-alpine` — **below 1.20**, so it cannot provide `crypto/ecdh`; `crypto/ecdh` appears **nowhere** in the tree. `go.mod` is `go 1.19`; the host is `go1.19.4`. **Identical**, so the divergence described is impossible by construction. `Dockerfile:10-12` states the true, reversed relationship. **Directly actionable and wrong**: an agent hitting a 1.19 error is told to build in a container that will fail identically. | `Dockerfile:10-15`; `go.mod:3`; `go version` | OVERCLAIM | [V] |
| 40 | `INVARIANTS.md:143-145` | Invariant 10: "The server durably remembers which keys it has already applied, and that memory survives restart (**it is part of the recovered state, not an in-memory cache**)" | False for enrol: `Service.idem` is a plain `map[string]idempotentEnrol`. `CLAUDE.md:23` admits this; **`INVARIANTS.md` — the file agents are told to read IN FULL — does not.** | `internal/auth/service.go:191-194,252`; `internal/auth/doc.go:64` | OVERCLAIM | [S] |
| 41 | `INVARIANTS.md:95-97`; `CLAUDE.md:60-61`; `AGENTS.md:60-61` | Invariant 6: "Recovery **ALWAYS** reaches a running server … It must **never** refuse to boot over corruption" | The server deliberately refuses to boot in named cases — `ErrSeqFloorUnprovable` ("**DO NOT SIMPLY RESTART**"), quarantine ("refuses on EVERY start"), unreadable MAC key. The WAL layer states the real rule: "DAMAGE IS NEVER FATAL. **NOT BEING ABLE TO READ THE FILE STILL IS.**" This is a genuine invariant 1-vs-6 collision resolved in code and recorded in no doc — and `CLAUDE.md:56-57` pre-emptively tells the finder they have merely rediscovered the 2026-08-02 narrowing, sending them down the wrong path. | `internal/hub/hub.go:829-838`; `cmd/agent-bus/logrepair.go:41-44`; `internal/wal/doc.go:234` | OVERCLAIM | [S-only] |
| 42 | `INVARIANTS.md:59-61`; `CLAUDE.md:49`; `AGENTS.md:49` | Invariant 3: redeeming an invite is "the **ONLY** way onto the bus" | **Peer buses bypass invites entirely.** `agent-bus peer add` is authorised by filesystem access; no invite is minted or redeemed. `internal/httpapi/peermount.go:135-146` quotes the invariant sentence and concedes it. | `cmd/agent-bus/peer.go:605`; `internal/httpapi/peermount.go:135-146` | OVERCLAIM | [S-only] |
| 43 | `INVARIANTS.md:205-206` | "The agent's client-certificate fingerprint **is bound** to its server-minted agent id at enrolment" (unqualified indicative) | Optional, and **nil is the ordinary case** — the listener only *requests* a certificate. Consequence at `crosscheck.go:21-31`: an unbound agent's stolen token is still replayable. | `internal/auth/service.go:349-403`; `internal/auth/certbind.go:67-69` | OVERCLAIM | [S-only] |
| 44 | `INVARIANTS.md:174` | "The second [question] is **not yet load-bearing** but becomes so the moment relay ingest lands" | Relay ingest landed. The disconnect-safety question it defers is live **now**: a peer bus legitimately presents `sender != principal` for many agents on one connection. | `internal/httpapi/peermount.go:305`; `internal/hub/relayingest.go` | STALE-NOTYET | [S] |
| 45 | `CONTRACTS-CLI.md:801-807` | Blames a fed-smoke failure on `read_audit` at "line 191" invoking `"$CTL" log` where `CTL` is `bin/agent-busctl` | Fixed. `scripts/fed-smoke.sh:251-255` — `read_audit()` takes `$server` and runs `"$server" log`. Line 191 is a different function. **The line number, the binary and the failure claim are all wrong.** | `scripts/fed-smoke.sh:251-255` | WRONG-DETAIL | [S-only] |
| 46 | `CONTRACTS.md:28-36` | Two "known-wrong passages tracked elsewhere" pointers — `CONTRACTS-CLI.md`'s `-listen` row "still reads `:8080`", `CONTRACTS-ONDISK.md` "still documents … `RepairTail`" — plus the standing directive "**Do not 'fix while you're in there' on either passage**" | Both are **already fixed** (`CONTRACTS-CLI.md:20` reads `127.0.0.1:8080`; zero occurrences of `RepairTail` in `CONTRACTS-ONDISK.md`). A **standing directive protecting two passages that no longer exist** will make the next agent leave the pointer itself in place indefinitely. | `CONTRACTS-CLI.md:20`; `CONTRACTS-ONDISK.md:1008` | STALE-NOTYET | [S-only] |

### S3 — Wrong number, name or location; costs navigation and small mistakes

| # | Doc `file:line` | CLAIMS | Code ACTUALLY does | Class | Prov |
| --- | --- | --- | --- | --- | --- |
| 47 | `CONTRACTS-CLI.md:1050-1068` | Nine subcommand rows, asserted as "**the registry … is exactly the nine rows above**" | **Eleven.** `session` and `client-cert` are missing; **`client-cert` is documented nowhere in the file**. A positive "exactly N" assertion stops a reader looking further — this is invariant 7's second audience losing a whole capability. | OVERCLAIM | [S] |
| 48 | `CONTRACTS-CLI.md:114` | "The binary now takes exactly **ONE of TWO** subcommands" | **Five**: invite, healthcheck, peer, key, log — all five documented below that line in the same file. | WRONG-DETAIL | [S] |
| 49 | `CONTRACTS-CLI.md:22` | `-poll-timeout` is "not yet consumed by any handler" | Consumed: `main.go:293` → `hub.Options.PollTimeout` → `internal/hub/wait.go:227-228`. | STALE-NOTYET | [S] |
| 50 | `CONTRACTS-CLI.md:24` | `-bus-id` default "*(empty → placeholder `bus-local`)*" | No such placeholder outside tests; `main.go:336-345` says so explicitly. | WRONG-DETAIL | [S-only] |
| 51 | `CONTRACTS-CLI.md:18-24` | Server flag table, five flags | `-backfill-suffix-floors` (`main.go:300`) is absent from the flags contract — the one flag an operator needs when the bus refuses to start. | omission | [S-only] |
| 52 | `CONTRACTS-HTTP.md:439` | Cites `auth.MaxActiveSessionsPerAgent` | No such symbol. It is `auth.DefaultMaxActiveSessionsPerAgent` (const) / `Options.MaxActiveSessionsPerAgent` (field). | WRONG-DETAIL | [S] |
| 53 | `CONTRACTS-HTTP.md:54-58`, `:619-626` | `/v1/session/complete` statuses 200/400/401/403/404; caps table has three rows | Also returns **503 + `Retry-After: 5`** from the per-agent cap (`session.go:458` → `auth.go:872-874`), and `MaxActiveSessionsPerAgent` is a fourth cap absent from the table. Same root as #23. | WRONG-DETAIL | [S] |
| 54 | `CLAUDE.md:112`; `AGENTS.md:112` | Repo layout: "`internal/…` server packages (ids, store, wal, hub, **http**, relay, auth)" | Package is **`httpapi`**; seven omitted (`attest`, `buscert`, `dirlock`, `idem`, `invite`, `logging`, `signing`). **`client/` is absent from the layout map entirely** — the one directory whose location is itself an invariant (7). | WRONG-DETAIL | [S] |
| 55 | `CLAUDE.md:376`; `AGENTS.md:357` | "the **tracked** `data/` dir is not a test fixture" | Gitignored — `.gitignore:8` is `/data/` (confirmed at HEAD). `git ls-files data` is empty. | WRONG-DETAIL | [V] |
| 56 | `CLAUDE.md:373`; `AGENTS.md:354` | Shared append-only files include "`CONTRACTS.md`" | `CONTRACTS.md` is an index since the 2026-08-02 split; the concurrently-appended files are `CONTRACTS-{CLI,HTTP,ONDISK,AGENT}.md`. Contradicts `CLAUDE.md:334`. | WRONG-DETAIL | [S] |
| 57 | `AGENTS.md:212,215` | "`sonnet` (exact id **`Codex-sonnet-5`**)" / "`opus` (exact id **`Codex-opus-5`**)" | **No such model ids.** A `Claude`→`Codex` substitution corrupted them. `AGENTS.md:265` then requires every agent to post `kind=model; model=<exact-id>` as "the auditable cost signal" — so following AGENTS.md writes a **fabricated id into the cost audit trail**. `CLAUDE.md:392-393` deliberately gives no exact ids. | WRONG-DETAIL | [S] |
| 58 | `AGENTS.md:359` | "Agent roster (`.Codex/agents/`)" | Real path is `.codex/agents/` (lowercase, `.toml`), and it is **untracked** and not in `.gitignore`. | WRONG-DETAIL | [S-only] |
| 59 | `INVARIANTS.md:105-107` | Wrapper retirement in progress — "the ones that exist are to be retired as their subcommands land" | Complete. Only `bus-serve.sh` remains and it is **permanently exempt**. The phrasing invites someone to "finish" retiring the one file `AGENT_PROTOCOL.md:48` depends on. | STALE-NOTYET | [S] |
| 60 | `CLAUDE.md:60`; `AGENTS.md:60` | Invariant 6: "The append-only log records METADATA AND ROUTING ONLY — never message bodies" (unqualified) | **There are two logs and CLAUDE.md never says so.** `bus.audit` is clean (`internal/wal/audit.go:156` — "THERE IS NO BODY FIELD AND THERE MUST NEVER BE ONE"), but `bus.wal` carries bodies by design (`internal/hub/hub.go:1798-1802`). The code's own reading is narrower: `internal/store/message.go:227-230` scopes it to "the audit trail". Taken literally, an agent would strip the body from the WAL and delete durability (invariants 4, 5). | WRONG-DETAIL | [S] |
| 61 | `client/errors.go:43` | "`KindNetwork` is … or (**once invariant 11 lands**) a certificate that does not match the pinned fingerprint" | Landed 2026-08-07; pinning is live at `client/pin.go:410`. | STALE-NOTYET | [V] |
| 62 | `CONTRACTS-ONDISK.md:869-870` | Roster record: `msg_pub` and `invite_id` are "**reserved, unpopulated**" | Both populated (`internal/auth/record.go:131-134,264-265`). The same table was corrected for `cert_bindings` at `:893-898` and these two rows were left. A fsck author would treat a present field as damage. | STALE-NOTYET | [S] |
| 63 | `CONTRACTS-ONDISK.md:1059-1061`, `:1200-1203` | "Nothing in this section is reachable by an agent today — this package registers no HTTP route … and ships no operator wrapper (`INVITE-MINT`/`INVITE-REVOKE`)" | `agent-bus invite mint` ships and `/v1/enroll` redeems. Only `INVITE-REVOKE` is genuinely absent. | STALE-NOTYET | [S-only] |
| 64 | `CONTRACTS-HTTP.md:450-460`; `:483-486` | "transitional — until `cmd/agent-bus` sets `Options.Hub`, `httpapi.New` builds one itself" with "two honest costs" (double WAL replay, non-fatal rebuild); and "**Do not infer a wrapper or CLI subcommand exists for enrolment or sessions from this document.**" | `main.go:1435` sets `Hub: h`; the self-build path and both costs are gone. `enrol`, `session`, `logout` are all registered subcommands. | STALE-NOTYET | [S] |
| 65 | Go comment `file:line` citations that have DRIFTED | `internal/relay/registry_test.go:695` cites `forward.go:853` for "THE ADDRESS IS RE-RESOLVED ON EVERY ATTEMPT" → actually `forward.go:1272`. `internal/relay/peerstore.go:41` and `:749` cite `signed.go:178-182` for the two-key rollover → actually `signed.go:347-351`. `cmd/agent-bus/relaywiring.go:1168` cites `handshake.go:236` as the 503 mapping → the 503s are at `:231` and `:240`; `:236` is the 403 arm. `relaywiring_relay24_test.go:893` cites `rosterhttp.go:184` for "no `ErrorCode` at all" → `ErrorCode` calls are at `:146,161,170,203,206`. | WRONG-DETAIL | [V] |
| 67 | `AGENT_PROTOCOL.md` HEAD:100-102 | "The **tracked** `./data` directory in this repo is a real (if **currently empty**) durable-store location" | **Both adjectives are wrong.** `git ls-files data` → **0** (untracked, `.gitignore:8`). And it is **not empty** — it holds a live bus identity: `bus-id`, `bus-signing.key`, `bus-tls.key`, `bus-tls.crt`, `bus.wal`, `bus.audit`, `agent-suffixes`, `message-seq-floor`. The advice that follows ("never point `AGENT_BUS_DATA_DIR` at it") is right and the **real** reason is far stronger than either stated one. Compounds #55. | `git ls-files data`; `ls -a data/` | WRONG-DETAIL | [V] |
| 68 | `AGENT_PROTOCOL.md:653`; `README.md:102-104` | Unauthenticated routes: `AGENT_PROTOCOL.md` names **five**; `README.md` says "**Both** routes are unauthenticated by design … Every other route … returns a JSON error" | **Six**, including `/v1/discovery` — the **largest unauthenticated document the bus serves**. Third and fourth independent statements of the same gap as #25. Anyone auditing the pre-auth attack surface from any of these four docs misses it. | `internal/httpapi/authmw.go:76-83` | WRONG-DETAIL | [S] |
| 69 | `docs/THREE-BUS-DOCKER.md:277-282, :463` | "the only compiled command that emits [the fingerprint] today is `invite mint`" / "**No read-only command emits `bus_cert_fingerprint`.** You must mint an invite to read a public value." | The server logs it at **info on every start** (`cmd/agent-bus/main.go:1571`) and the image runs at `-log-level=info`, so `docker logs bus-b` prints it — **the same doc tells the reader to run `docker logs` at `:52`.** Cost: the operator mints a real, **unrevocable** (per the doc's own `:284`) 24 h bearer credential to read a value already on screen. | `cmd/agent-bus/main.go:1571` (observed live); `Dockerfile:274` | WRONG-DETAIL | [V] |
| 70 | `docs/THREE-BUS-DOCKER.md:121-124` | Four subcommands take the exclusive data-dir lock (`invite mint`, `peer add`, `key export-public`, `log`); "`healthcheck` is the exception" | **Six.** `peer list` (`peer.go:1159`) and `peer remove` (`:1263`) also route through `openPeerStore` → `dirlock.Acquire` (`:1409`) and exit 3. **`peer list` is exactly what you reach for when a LIVE federation misbehaves** — the doc gives no warning it needs an outage. | `cmd/agent-bus/peer.go:1159,1263,1367,1409` | WRONG-DETAIL | [V] |
| 71 | `docs/THREE-BUS-DOCKER.md:266` vs `:284-286` | Prose: "You cannot revoke it … so **mint it with a short TTL (`-ttl 1h`)**" | The runnable block at `:266` mints with **no `-ttl`** → `invite.DefaultTTL` = **24 h**. The doc's "extracted and run verbatim" property means the **24 h path is the verified one and the prose was never exercised** — the reader gets a 24 h unrevocable bearer credential while believing they asked for 1 h. | `internal/invite/retention.go:146` | WRONG-DETAIL | [S] |
| 72 | `docs/comms/METRICS.md:307-308` | "nothing that reads what the bus delivered" | `agent-busctl watch --replay` starts at position 0 and re-reads the whole retained window (1 day / 1 GiB). **The corpus this epic analysed was itself collected from `watch` NDJSON.** The narrower true claim — nothing reads traffic not addressed to you, and the log is metadata-only (invariant 6) — is what the section actually argues and is fully supported. | `cmd/agent-busctl/watch.go:129`; `internal/store/store.go:21,37` | OVERCLAIM | [S-only] |
| 73 | `docs/comms/METRICS.md:246-264,278`; `LABELLING-KEY.md:6-8`; `CONSENT.md:183` | Self-audit arithmetic and the consent record | `:260-264` gives "**0 `sec-tester-1`'s**" where the pass-2 key shows **2** and `LABELS-PASS2.csv` marks **8** — three mutually inconsistent counts. `:278`'s "7 of 53 marked genuinely marginal" is unreproducible (zero `MARGINAL` notes, three `BORDERLINE`) and is the **sole support for the ±10-point caveat**. `LABELLING-KEY.md:6-8` is a self-executing void clause — "if you are reading this in a commit that also contains labels, the guarantee is void" — and key + `LABELS.csv` landed in exactly one commit (`8a6452c`), so **it has fired**. `CONSENT.md:183` binds `speckeeper-1`'s aggregates as "PROVISIONAL — do not publish" while `speckeeper-1` is still unasked; `METRICS.md` prints them in three tables in a tracked file. **These are in the epic's own credibility machinery.** | `docs/comms/LABELLING-KEY-PASS2.md:87,91`; `docs/comms/LABELS-PASS2.csv` | WRONG-DETAIL | [S-only] |
| 74 | `docs/THREE-BUS-DOCKER.md:396-401` | "Observed on C" sample shows a 5-key `watch` record, with **no abridgement marker** | `watchRecord` emits **13** keys, including the base64 `body` and `signature` that §5 later discusses. A reader building a parser off the sample misses eight fields. | `cmd/agent-busctl/watch.go:316-345` | WRONG-DETAIL | [S-only] |
| 75 | `CONTRACTS-AGENT.md` HEAD:19 | "`scripts/` holds **exactly six files** (count corrected 2026-08-14: it said three …)" | `git ls-files scripts/` → **eight**; the working tree has **nine**. Omitted from both tables: `proof-cmd-audit.py`, `proof-cmd-audit-test.sh`, `doc-check.sh` — **while the same file documents `doc-check.sh` at `:236` and `proof-cmd-audit.py` at `:296`.** The "count corrected 2026-08-14" annotation makes a wrong number read as freshly checked — the exact failure mode this audit is about. | `git ls-files scripts/` | WRONG-DETAIL | [S] |
| 76 | `AGENT_PROTOCOL.md:19-33` (TOC) and the file as a whole | TOC lists **10** of **15** sections; `agent-busctl client-cert` is documented **nowhere** in the file | `client-cert` ships (`cmd/agent-busctl/clientcert.go:14`, live in `--help`) and is in `CONTRACTS-CLI.md`'s prose. **Invariant 7 requires the `AGENT_PROTOCOL.md` entry in the same task** — this is the missing half of a shipped capability, and it is the second doc to lose `client-cert` (see #47). Missing TOC entries: Federating two buses, The audit trail, Global flags, `--persist-session`, Sending to an agent on ANOTHER bus. | `cmd/agent-busctl/clientcert.go:14` | omission / invariant-7 gap | [S] |
| 66 | `CRYPTO_DEEPDIVE.md:454,658,663,687,753,1029,1188` | Proposes `bus-trace.sh`, `bus-send.sh`, `bus-wait.sh` as deliverables and a "**wrapper rule (the load-bearing line)**" | These wrappers are **retired** (invariant 7); only `bus-serve.sh` survives. A design study that will be implemented from proposes a surface the invariant forbids. *(Out of the literal file scope you gave; reported because it is a live design input.)* | RETIRED-TOOLING | [V] |

---

## 3. Proof artifacts

Everything below was run; verdicts are quoted, not paraphrased.

### 3.1 Finding 5 — `Client.Read` does not verify. Proven in a clean overlay of HEAD.

```
T=$(mktemp -d); git archive HEAD | tar -x -C "$T"
(cd "$T" && go build ./... && bash scripts/proof-check.sh \
   'go test -race -run TestReadDoesNotYetVerifyReceivedMessages ./client')
```

```
wedge_test.go:524: KNOWN GAP (SIGN-5, not yet implemented): Client.Read delivered an UNSIGNED
  message from a sender this client holds no key for. The verification seam (verifySignedMessage)
  exists and is proved by TestVerificationSeamCannotBeWedged, but nothing on the read path calls it.
--- PASS: TestReadDoesNotYetVerifyReceivedMessages (0.02s)
proof-check: verdict=PASS class=test exit=1... tests_run=1 top_level=1 skipped=0 failed=0
```

**`proof-check.sh` verdict: `PASS` (1 test ran, 1 passed, 0 skipped — not VACUOUS).**
Run in an overlay of HEAD, so this is not an artefact of another agent's live edits.

### 3.2 Findings 1, 2, 3 — reproduced against a throwaway bus under `/tmp`

Fresh data dir, TLS-only bus on `127.0.0.1:18096-18098`, torn down afterwards; the tracked `data/`
dir was never touched.

```
# README.md:96 — "What works today"
$ curl -s -m 5 http://localhost:18098/healthz
Client sent an HTTP request to an HTTPS server.      # NOT {"status":"ok"};  curl exit 0
$ curl -sk -m 5 https://localhost:18098/healthz
{"status":"ok"}

# README.md:113 quickstart, verbatim (no --invite-file)
$ agent-busctl --bus https://127.0.0.1:18098 --bus-fingerprint <fp> enrol --name planner --json
{"ok":false,"error":"enrol: the bus rejected this credential: this bus is invite-only; obtain an
 invite from the bus operator and present it with `agent-busctl enrol --invite-file <path>`",
 "kind":"auth","status":403,"exit_code":4}
enrol_exit=4

# GET /v1/discovery — the stale claim that ships ON THE WIRE
"invite_required": true
 - 5. SINGLE BUS ONLY. Cross-bus relay is not served yet; a recipient on another bus is a 404.
```

The last line is the single most important artifact in this audit: it is not a stale comment in a
file a human might not read, it is a **machine-readable field in the pre-enrolment discovery
document**, and `CONTRACTS-HTTP.md:86` describes the `limitations` array as "verified-true-of-this-
build negative claims" that "must be restated exactly, never softened".

### 3.3 Enforcement claim that does not enforce

`CONTRACTS-CLI.md` HEAD:1859 says the `client` package's no-`internal/`-import rule is
"**mechanically enforced** by CLI-1's proof clause `! go list -deps ./cmd/agent-busctl | grep -q
'agent-bus/internal/'`".

```
$ go list -deps ./client       | grep 'agent-bus/internal/'   # (empty — the RULE holds)
$ go list -deps -test ./client | grep 'agent-bus/internal/'   # (empty)
$ grep -rn "go list -deps" --include='*.go' --include='*.sh' --include='*.y*ml' .   # (no hits)
```

The rule holds. The **enforcement does not exist** — no test, no script, no CI config runs that
clause; there is no `.github/` at all. It was a one-shot `proof_cmd` on a closed task. `OVERCLAIM`,
low blast radius today, but it is the guard for an invariant.

---

## 4. The 8 seeded items — verdicts

| # | Seed | Verdict | Evidence |
| --- | --- | --- | --- |
| 1 | `INVARIANTS.md:251` "Certificate expiry is NOT checked" | **CONFIRMED** | Line is verbatim at `INVARIANTS.md:251`. Expiry **is** checked and **is wired into the live handshake**: `client/pin.go:261` (`VerifyPeerCertificate: pinVerifier`) → `:367` → `verifyPinnedBusCertificate:410` → `checkBusCertificateValidity:503` → `x509.Expired` → `BusCertificateExpiredError` at `:532`. **Beyond the seed:** the *ownership* is also wrong — `MTLS-VERIFY` is about `bus-serve.sh`'s health probe, not expiry. (Finding 24) |
| 2 | `client/enrol.go:63-66` un-invited enrolment | **CONFIRMED** | Verbatim at `client/enrol.go:64`. `enrolmentInviteRequired = true` at `cmd/agent-bus/main.go:66`. Still unfixed **despite `CLAUDE.md:31` naming this exact line**. **Beyond the seed:** the same falsehood is in `internal/invite/doc.go:15-23` and `internal/auth/roster.go:133-134`, which the CLAUDE.md note does *not* flag. (Findings 34-35) |
| 3 | `client/messages.go:384` "VERIFIED messages" | **CONFIRMED, and worse than seeded** | The seed found one site. There are **three**, and `:161-162` is the sharpest: *"a caller should not verify it: **Read has already done that**"*. `:389` compounds it ("Rejected are the messages that did NOT verify" — never populated), and `client/keyring.go:21-24` describes a fail-closed body-discard that does not happen. Proven PASS in a HEAD overlay (§3.1). (Findings 5-6) |
| 4 | `client/config.go:386-391` "bus does not serve TLS yet" | **CONFIRMED at HEAD** | Worktree `:386,391`; **HEAD:334,339** (file is dirty — re-verified via `git show`). The comment contains its own unexecuted deletion instruction: *"DELETE THIS CASE ENTIRELY when the TLS listener ships."* (Finding 36) |
| 5 | `keygen` / `trust` remedies | **CONFIRMED, and wider than seeded** | Registry is exactly 11 commands (`cmd/agent-busctl/root.go:160-175`); neither exists. Seed named 4 sites; there are **8**: `store.go:272`, `client.go:536`, `client.go:564`, `client.go:577`, `keyring.go:23`, `keyring.go:181`, `canonical.go:504`, `messages.go:323`, `config.go:183`. `trust` is the **only documented way to populate the trust store**. (Findings 9-10) |
| 6 | `CONTRACTS-HTTP.md` "deliberately no per-agent cap" | **CONFIRMED** | `:626` and `:628`. Cap is `DefaultMaxActiveSessionsPerAgent = 32` (`internal/auth/service.go:61`) and it is **enforced** at `internal/auth/session.go:457-458` with **no eviction**. **Beyond the seed:** the same sentence also says "**while enrolment is itself unauthenticated**" (false), the undocumented **503** on `/v1/session/complete` is the cap's refusal (`auth.go:872-874`), the caps table at `:619-626` omits the fourth cap, and `:439` names a **non-existent symbol** `auth.MaxActiveSessionsPerAgent`. (Findings 23, 52, 53) |
| 7 | `CLAUDE.md` "the tracked `data/` dir" | **CONFIRMED, and in a third file the seed did not name** | `CLAUDE.md:376`, `AGENTS.md:357` **and `AGENT_PROTOCOL.md` HEAD:100**. `.gitignore:8` is `/data/`, confirmed at HEAD (`.gitignore` is dirty); `git ls-files data` → 0. **Beyond the seed:** `AGENT_PROTOCOL.md` also calls it "currently empty" — it holds a **live bus identity** including `bus-signing.key` and `bus-tls.key`. (Findings 55, 67) |
| 8 | `cmd/agent-busctl/enrol_test.go:24` imports `internal/buscert` | **CONFIRMED as fact, REFUTED as a doc defect** | The import is real and is the only `internal/` reference under `cmd/agent-busctl`. But **no in-scope doc forbids it**: `CONTRACTS-CLI.md` HEAD:1855-1859 scopes the rule to the **`client` package**, which is clean including its tests. The genuine defect nearby is a *different* one — that the cited clause is not actually enforced anywhere, and `go list -deps` excludes tests so it could not see this import if it ran (§3.3). |

### Two things I must report as REFUTED / not-a-defect

**REFUTED-1 — `cmd/agent-busctl/enrol --help` "revocable" is a REAL and previously unfiled finding.**
You said you believed this one was real and unfiled. **You were right.** Confirmed at
`cmd/agent-busctl/enrol.go:62`, and it is worse than a help string: `cmd/agent-bus/invite.go:297`
tells an operator whose invite secret just became **unrecoverable** to "revoke `<id>` and mint
another", and `client/invite.go:237` says the same on a world-readable invite file. `Store.Revoke`
exists with zero non-test callers; `cmd/agent-bus/invite.go:646` concedes "*once that surface
exists*". This is an invariant-7 violation too — a capability with no subcommand. (Findings 7-8)

**REFUTED-2 — "Session tokens are never written to disk" is TRUE at HEAD, in BOTH files that say it.**
Two sub-agents independently flagged `CONTRACTS-CLI.md` worktree `:1567` and `AGENT_PROTOCOL.md`
worktree `:656` as contradicted by `client/sessionstore.go`. **That file is untracked**, and
`--persist-session` does not exist at HEAD:

```
$ git show HEAD:cmd/agent-busctl/root.go | grep -c persist-session   # 0
$ git show HEAD:client/config.go        | grep -c PersistSession     # 0
$ git ls-files client/sessionstore.go cmd/agent-busctl/sessionlogout.go | wc -l   # 0
```

At HEAD the statement is **correct** in both files (`CONTRACTS-CLI.md` HEAD:1513,
`AGENT_PROTOCOL.md` HEAD:593). This is exactly the transient mutation you warned about, and I record
it as **not a defect today** — but as the **sharpest landmine for the in-flight `AUTH-SESSION-PERSIST`
task**: the moment session persistence lands, both lines become false *negative security claims*, in
precisely the sections a reviewer sweeping for at-rest bearer credentials would read. **Its
documentation gate must fix both.**

Two more in-flight items I am deliberately NOT filing, for the same reason:
- The `--persist-session` help text **already warns about the 32-session cap** in the worktree — so
  #23's CLI half is being fixed right now. **Do not double-file it.**
- `scripts/doc-check.sh`, `docs/doc-budgets.tsv` and `docs/doc-preserve.tsv` are **all untracked**, so
  `git archive HEAD` yields none of them and the CLAUDE.md-mandated clean-overlay proof cannot run at
  all. A sub-agent observed `doc-check.sh budget` currently **FAIL**ing (`CLAUDE.md is 29459 B, over
  its 28781 B ceiling by 678 B`, exit 1) because `aade191` grew the file after the ceiling was set.
  That is a **live warning for the in-flight `CONTEXT-DOCCHECK` task**, not a defect in committed
  work — and note the tempting fix (raise the ceiling) converts the ratchet into headroom, which is
  the exact failure that file exists to prevent.

---

## 5. Root cause — confirmed, and the candidates

**CONFIRMED root cause: there is no link from "the gap closes" to "the paragraphs that reasoned about
the gap".** The evidence is the *shape* of the failures, not a guess:

1. **The composition root is always right; the leaves are always wrong.** `cmd/agent-bus/main.go`
   carries four explicit self-corrections (`:1301-1362`, `:817-825`, `:1556`) that say "THIS LINE
   SAID SOMETHING FALSE UNTIL <task>… It is corrected rather than deleted." Meanwhile
   `internal/auth/service.go:502`, `internal/relay/handshake.go:131` and `cmd/agent-bus/peer.go:136`
   — all describing *that same wiring* — were never touched. The task that lands a wiring change
   edits the file it wires, and nothing tells it which other files asserted the absence.
2. **Same-file contradictions prove it is not a knowledge problem.** `PROTOCOL.md:166-168` and
   `:708-718` say opposite things about `buscert` on the startup path. `CONTRACTS-HTTP.md:279` and
   `:504` say opposite things about the messaging key. `CONTRACTS-HTTP.md:611` and `:722` say
   opposite things about the auth middleware. `CONTRACTS-CLI.md:488` and `:841` say opposite things
   about RELAY-24. Someone knew; they edited the section they were in.
3. **The count clusters on six commits in nine days.** 17 relay passages, 11 invite passages. This is
   burst damage from a fast-shipping fortnight, not entropy.

**Ranked candidates for *why the link is missing*, with the disproof test for each:**

| Rank | Candidate | Disproof test |
| --- | --- | --- |
| 1 | **No mechanical detector.** Nothing greps for "not yet"/"MUST NOT BE"/"registered on no mux" and re-checks it. | **Already partly disproven-by-absence:** `scripts/doc-check.sh` + `docs/doc-budgets.tsv` + `docs/doc-preserve.tsv` are **untracked, in-flight work by another agent right now**. Read them before writing a new detector — the tool may already be arriving. |
| 2 | **A stale claim is not a test failure.** Every one of these files compiles and every test passes. `go vet` cannot see prose. | Write ONE guard for the sharpest case: an AST/grep test asserting no file containing `IT IS NOT REGISTERED ON ANY MUX` names a path that `peermount.go` mounts. It goes RED today. If it goes red, the class is mechanically detectable and rank 1 is the fix. |
| 3 | **The doc gate is scoped to "what this task changed".** `CLAUDE.md:9` mandates a `documentation` agent per task, but its brief is the diff, not "what did this make false elsewhere". | Sample the closing commits of `3cedcb7`, `7095231`, `9701611`, `d5018a6` and check whether any touched a file outside its own plane. If none did, confirmed. |
| 4 | **Task-label rot.** Prose is anchored to task keys (`RELAY-24`, `INVITE-GATE`) whose closure is invisible from the tree. | Cross-check the Spec Server for `RELAY-20/24`, `INVITE-GATE`, `AUTH-2/3/7`, `MTLS-LISTENER/CROSSCHECK`. If all are `done`, a "closed task named in prose" report finds most of §2 automatically. **I did not call the Spec API — see §7.** |
| 5 | *(Rejected)* Agents not reading before writing. | Rejected: `main.go`'s self-corrections and `peermount.go:12-16`'s careful "what changed and what did not" show high diligence **inside** the edited file. The failure is reach, not care. |

---

## 6. The fix — smallest correct changes

**Ordered by blast radius. None of these is a refactor; most are a sentence.**

**Tier 0 — on the wire, today (1 line of code):**
- `internal/httpapi/discovery.go:271` — limitation 5 is served to every caller and is false. Rewrite
  it to state what IS true (relay served; per-peer configuration required) or delete the entry.
  **This is the only Tier-0 item that is a code change, and it needs a test that pins the new string.**

**Tier 1 — agent lands in a broken state (docs only):**
- **The four entry points `3cedcb7` missed — do these together, they are one bug:**
  `README.md:107-121` (quickstart must gain `agent-bus invite mint` + `enrol --invite-file`),
  `cmd/agent-busctl/root.go` HEAD:412-413 (top-level `--help` says both gates are "in flight"),
  `scripts/bus-serve.sh:410` (the success message prints a command that 403s; the file says "invite"
  zero times), `client/enrol.go:63-66` (the embedder's doc comment).
- `README.md:8-12` (relay "not registered on any listener"), `:87-91` (TLS "until mTLS lands"),
  `:96-100` (plaintext `curl` → must become `curl -sk https://…`).
- `cmd/agent-busctl/send.go`'s **broadcast help never mentions 501** — the one surface invariant 7
  makes authoritative, and the only one of four docs that omits it.
- `client/transport.go:429-430` — the 403 remedy tells an agent to retry a refusal that can never
  succeed, contradicting `AGENT_PROTOCOL.md:526`. This is a **loop**, not a misunderstanding.
- `docs/THREE-BUS-DOCKER.md:448-455` — stop telling the operator to ignore `fed-smoke.sh`.
  **Gate on actually running it first** (§7: I could not).
- `CONTRACTS-AGENT.md` HEAD:48 — stop documenting the log-scrape that `bus-serve.sh:415` explicitly
  refuses; it is a trust-anchor substitution a reader may copy into their own tooling.
- The 11-passage invite cluster: `CONTRACTS-HTTP.md:43,90-91,506,726-737,979`;
  `CONTRACTS-CLI.md:136,1768-1770,1865-1868`; `CONTRACTS-ONDISK.md:1209-1210`;
  `internal/invite/doc.go:15-23`; `internal/auth/roster.go:133-134`; `client/enrol.go:63-66`.
- `client/messages.go:161-162,384-386,389` and `client/keyring.go:21-24` — **change the claim, not
  the code.** SIGN-5 is a real open task; the doc must say the read path does not verify. Leave
  `client/wedge_test.go` alone: it is the tripwire that turns SIGN-5 landing into a red test.
- `cmd/agent-busctl/enrol.go:62` — drop "revocable" until `INVITE-REVOKE` ships.
  `cmd/agent-bus/invite.go:297` and `client/invite.go:237` — replace the impossible remedy with the
  achievable one (let it expire; do not distribute it; `MaxInvites` pressure).
- The 8 `keygen`/`trust` remedies — every one is an **error string an operator reads at their worst
  moment**. Until the subcommands exist, they must describe the manual file operation.

**Tier 2 — reviewer/maintainer misled (docs + comments):**
- The 17-passage relay cluster (#15-#19, #38) and the three `MUST NOT BE` prohibitions (#16). The
  three handler-file prohibitions are the highest priority in this tier because they are **standing
  prohibitions that current code violates** — a security reviewer acting on them reverts the mount.
- `CONTRACTS.md:110-125` — delete the "do not add `/v1/peer/relay` to `CONTRACTS-HTTP.md`" directive
  and `:28-36`'s "do not fix while you're in there" directive. **Standing directives that outlived
  their premise are the worst subclass here**: they don't merely mislead, they *forbid the fix*.
- `internal/auth/service.go:502-505` (durability inverted) and `CONTRACTS-ONDISK.md:479-491`.
- `INVARIANTS.md` — #24 (expiry), #25 (`/v1/discovery` missing from the allow-list), #40 (idem
  durability), #41 (recovery does refuse to boot), #42/#43 (invites, cert binding), #44, #59.
  **`INVARIANTS.md` is the file agents are told to read IN FULL before touching a plane; it should be
  the most-audited file in the repo and it is currently one of the least.**
- `CLAUDE.md:137-147` / `AGENTS.md:135-145` — delete the `crypto/ecdh` rationale outright; state what
  `Dockerfile:10-12` already states.

**Tier 3 — numbers and names:** #28 (**do this one early** — the version registry is a collision
hazard of exactly the class `CLAUDE.md` opens by warning about), #47-#66.

### Latent landmines found along the way — not doc defects, flagged because they bite

1. **No relay wire-protocol version was ever reserved, and the format is now on the wire.** The
   paragraph obliging the reservation (#18) is the stale one, so the obligation evaporated silently.
   This is the highest-consequence discovery in the audit.
2. **`agent-bus invite` has no `revoke`.** A leaked invite cannot be withdrawn — only expired. Three
   error messages tell operators otherwise.
3. **The `client` package's no-`internal/` guard is not enforced by anything** (§3.3), and
   `go list -deps` would not see a test-file import even if it ran.
4. **`AGENTS.md` writes fabricated model ids into the cost audit trail** (#57).
5. **Unreproduced, reported as a HYPOTHESIS:** one throwaway first start on a pre-existing empty
   `mktemp -d` directory generated its keys and then aborted with *"this data directory has HISTORY
   but no `agent-suffixes` file"*, requiring `-backfill-suffix-floors`. **A second, controlled
   attempt on an equally pre-existing empty directory started cleanly**, so I could not reproduce it
   and I am NOT claiming a defect. If real it is a first-start trap on the exact command
   `README.md:53` gives. Disproof test: loop `mkdir $D && agent-bus -data-dir $D` 50 times and check
   for the abort.

---

## 7. Coverage — audited and CLEAN, and what I did NOT cover

**A silent area is indistinguishable from an unaudited one, so:**

**Verified CLEAN (checked against code, no defect found):**
- **Broadcast.** Docs and code agree everywhere: `README.md:19-23`, `CONTRACTS-HTTP.md:168,204`,
  `client/messages.go:663-690`, `internal/httpapi/messages.go:422-467`. 501, refused **before** the
  body is decoded, body string byte-identical to the doc's quote. Route absent from
  `discoveryEndpoints` as documented. **Confirmed serving 501 as documented.**
- **`/v1/leave` and logout honesty.** No route, no `Revoke` on `auth.Service`, no `RouteLeave`.
  `client/client.go:675-686` and `cmd/agent-busctl logout --help` both say so plainly, and
  `server_notified` is documented as always false and is. Model behaviour for this class.
- **SIGN-6 on send.** A signature IS required: `internal/httpapi/messages.go:585`.
- **Invariant 11's pin machinery.** `InsecureSkipVerify` appears in exactly one non-test file, once
  (`client/pin.go:260`), paired with `VerifyPeerCertificate` in the same literal;
  `client/guard_test.go` implements all four properties `INVARIANTS.md:241-246` claims.
- **Invariant 10's 409 indistinguishability test exists** —
  `TestCrossMintIsIndistinguishableFromAnHonestSpentReservation`,
  `internal/httpapi/disconnect_socket_test.go:436`.
- **Invariant 6's MAC** is HMAC-SHA256 on every live write; `hash/crc32` is confined to the
  read-only v1 legacy codec.
- **`-listen` default** `127.0.0.1:8080` — correct in binary help, `README.md:60`, `CONTRACTS-CLI.md:20`.
- **`scripts/`** — only `bus-serve.sh` survives of `bus-*.sh`. `gen-spec-mirror.sh`'s `--all`-is-a-no-op
  and `--no-relations` claims are accurate. `proof-check.sh` really does report
  PASS/FAIL/VACUOUS/UNVERIFIABLE, and the subtest-SKIP blind spot `CLAUDE.md:196-200` claims is fixed
  **is** fixed.
- **Agent roster** — `CLAUDE.md:380-382` lists 14 names; `.claude/agents/` holds exactly those 14.
- **All six commit SHAs cited in `CLAUDE.md`/`AGENTS.md` resolve and their subjects match.**
- **`client/` imports nothing under `internal/`,** including its tests — the invariant-7 rule holds
  (only its *enforcement claim* is wrong).
- **Large constant sweeps** — ~60 documented constants across `CONTRACTS-HTTP.md`,
  `CONTRACTS-ONDISK.md` and `PROTOCOL.md` (auth, hub, store, invite, wal, relay) matched code.
- **PROTOCOL.md §2/§3/§4/§5/§8.3/§8.4.1/§9/§11** — frame layout, offsets, magics, record types 1-4,
  MAC key path/mode, v1 legacy CRC32C codec, canonical signing layout and context string,
  `agent-suffixes` and `wal-index-floor` formats: all correct.
- **`CONTRACTS-HTTP.md` route registry, allow-list, middleware chain (byte-exact), context keys 0-3,
  TLS floor/ALPN, query parameters, documented error strings** — all correct.
- **`CONTRACTS-CLI.md` flag tables** for `agent-bus invite mint`, `healthcheck`, `peer add|list|remove`,
  `key export-public`, `log`, and every `agent-busctl` subcommand's own flags, plus client exit codes
  0-9 — all correct.

Additionally CLEAN on the agent-facing plane (this plane WAS covered — see the note below):
- **Retired tooling — clean everywhere.** No doc in scope references any `scripts/bus-*.sh` other
  than `bus-serve.sh`; no hand-written `curl` against the bus in agent instructions; no reference to
  a script that does not exist. The only wrapper finding (#1c) is about the *content* of
  `bus-serve.sh`'s success message, not its existence. **`CRYPTO_DEEPDIVE.md` (#66) is the sole
  RETIRED-TOOLING hit in the whole audit.**
- **Every flag in every documented CLI invocation exists**, verified against the compiled binaries:
  `enrol` (`--name --invite-file --idempotency-key --keep-current`), `watch` (`--replay --cursor
  --limit --poll-timeout --count --for --no-cursor`), `send` (`--file --stdin --idempotency-key`),
  `peer add` (all six), `log` (all eight), `key export-public`, `invite mint`, `healthcheck`.
  `--invite` correctly absent.
- **Exit codes** — `AGENT_PROTOCOL.md:1124-1141` matches `client/errors.go:95-109` value for value;
  403→4, 501→6, 404-on-`/v1/send`→7-not-9 all confirmed; `bus-serve.sh`'s 0/1/2, 0/1/3, 0/2 match
  `CONTRACTS-AGENT.md:52`.
- **JSON field names** — `watch` NDJSON's 13 fields match `AGENT_PROTOCOL.md:890-893` exactly
  (including `text,omitempty`); `send`, `pin`, `agents` `--json` all match. The
  `bus_fingerprint`→`bus_fingerprints` migration note is correct.
- **`docker-compose.yml` / `Dockerfile`** — no `ports:`, named `agent-bus-data` volume, healthcheck
  wiring, and the `VOLUME /data` claim all match `README.md:69-85`. (Only `README.md:87-91`'s stale
  *rationale* is wrong — #21/#38 area; the conclusion still holds.)
- **`docs/comms` data integrity** — all `METRICS.md` aggregates recompute from the CSVs, both
  frozen-corpus sha256 digests verify, the 60-line/53-message/6-duplicate accounting reproduces, and
  every `doc-preserve.tsv` phrase is present. Only the *self-audit narrative* numbers are wrong (#73).

**NOT covered — say so explicitly:**
- `CONTRACTS-ONDISK.md` lines ~180-250, ~310-460, ~530-700, ~1220-1400, ~1600-1840, ~1915-2025,
  ~2100-2300 were swept for prime-suspect phrases and cross-checked for constants/filenames, but
  their behavioural claims were not re-derived against code.
- `PROTOCOL.md` §6 (recovery/quarantine), §8.5's relay-ordering discussion, §10 — skimmed, not audited.
- JSON field-name tables were spot-checked, not exhaustively diffed against struct tags. Specifically
  **not** verified: `agent-bus log --json` NDJSON keys, `peer add --json` shape, checkpoint manifest fields.
- **Test-file comments** were only sampled (the `file:line` drift sweep, #65). There are 334 Go files;
  I read non-test doc comments systematically only for the prime-suspect phrase set.
- `DECISIONS.md` — excluded by instruction.
- `FEDERATION_TRUST_DEEPDIVE.md`, `ID2_WIRING_DEEPDIVE.md` — outside the file scope given; not read.
  `CRYPTO_DEEPDIVE.md` was grepped only for retired-wrapper references (#66).

**COULD NOT VERIFY — flagged, not guessed:**
- **Every Spec-Server task-state claim embedded in the docs.** `CONTRACTS-HTTP.md:870` asserts
  "`MTLS-CLIENTCERT` is `in_progress`, not done" — a live task-state assertion frozen into a contract
  file. Likewise `AUTH-1-FU-RATELIMIT`, `AUTH-2-FU-POLLEXPIRY`, `MTLS-CROSSCHECK-FU-CERTEXPIRY`,
  `INVITE-CLIENT`, `INVITE-REVOKE`, `INVITE-PEERGUARD`, `MTLS-RELAYGUARD`, `RELAY-4`, `RELAY-48`,
  `RELAY-50`, `CRYPTO-4`, `SIGN-3`, `SIGN-5`. **I made no Spec API calls.** Task states quoted above
  from `SPEC/` came from the generated mirror at `b6c0ed4`, which may lag the cloud.
- **Finding 41's invariant-1 sequence-reissue gap** (`cmd/agent-bus/logrepair.go:36-47` describes a
  one-shot guard that `restart: unless-stopped` defeats on the second start). The in-code comment
  reports it as *measured*. I did not re-run that forge. Its P0 task's recorded `proof_cmd` names
  `TestWALRepairDoesNotReissueDiscardedIndex`, **which exists nowhere in the repo** — that alone
  warrants a check independent of this audit.
- Whether the untracked `scripts/doc-check.sh` / `docs/doc-*.tsv` already implement the detector §6
  recommends. **Read them before building anything.**
- I did not build in Docker; finding #39 rests on `Dockerfile:15` and `go.mod:3` being textually
  identical to the host toolchain.
- **Whether `scripts/fed-smoke.sh` exits 0 today.** It needs three free loopback ports and a ~30 s
  run and refuses to reuse its `/tmp` roots. I confirmed only that the **stated cause** of #1e's
  exit-1 claim no longer exists in the file. If it still fails, it fails for an unrecorded reason —
  which makes #1e's guidance unsafe **either way**. `RELAY-25-FU-CORRELATION` is still `in_progress`
  in the mirror, so the tracker currently agrees with the stale doc.
- **`docs/THREE-BUS-DOCKER.md`'s recorded run artefacts** (Docker version, container id, IPs,
  digests) are environment-specific and were not reproduced. Nothing in the repo builds the image
  (`DEPLOY-7` todo), so that runbook is **ungated against rot** — `9938eb2`'s own "WHAT IS NOT DONE"
  says so, but the doc's §5 "Known gaps" does not.
- **`docs/comms/METRICS.md:299`'s "0-for-1020 `kind=model` notes"** — needs ~686 rate-limited Spec
  calls. Three spot-checked COMMS tasks had 0 notes: consistent, not proof. Several other
  heuristic-dependent counts there could not be checked exactly because the extraction rules are
  unstated; the underlying 2×2 counts all reproduce and the conclusions are unaffected.
- **All liaison-authored messages in `docs/comms`** (the `RETRACTS:` header, consent DMs) — the
  corpus states none are present, so the **consent record is unauditable from the artefacts**. Not a
  defect; worth knowing for a consent record.

---

## 8. SPEC-ready task breakdown

**Do NOT hand-edit `SPEC.md` or `SPEC/`.** File these via
`POST /api/v1/projects/agent-bus/tasks`, and **reserve any numbered key atomically** via
`POST /api/v1/projects/agent-bus/reservations {"namespace":"task-key-<EPIC>","reserved_by":"deep-diver"}`
— seeding the namespace past that epic's existing max first. The titles below are **descriptive and
unnumbered on purpose**, which is the safer option for one-off follow-ups.

Each task is atomic and independently verifiable. `proof_cmd` must be observed **RED before the fix** —
and for doc proofs, must pin the specific line, never an incidental match (`CLAUDE.md:222-229`).

| # | Title | Scope | `proof_cmd` sketch |
| --- | --- | --- | --- |
| T1 | **`/v1/discovery` limitation 5 is false on the wire: cross-bus relay IS served** | `internal/httpapi/discovery.go:271` + a test pinning the new string | `go test -race -run TestDiscoveryLimitationsMatchServedCapabilities ./internal/httpapi` (test to be written; must be RED first) |
| T2 | **README is unusable: quickstart 403s, "what works today" curls a TLS port in plaintext, relay declared unregistered** | `README.md:8-12,87-91,96-100,107-121` | Reproduce §3.2 end-to-end: mint invite, enrol WITH `--invite-file`, exit 0; `curl -sk https://…/healthz` → `{"status":"ok"}` |
| T3 | **Doc-truth sweep: enrolment is invite-gated (11 passages, 5 files)** | `CONTRACTS-HTTP.md`, `CONTRACTS-CLI.md`, `CONTRACTS-ONDISK.md`, `internal/invite/doc.go`, `internal/auth/roster.go`, `client/enrol.go` | grep pinning each specific line; RED before |
| T4 | **Doc-truth sweep: relay is mounted, live and imported (17 passages) — incl. 3 `MUST NOT BE` prohibitions current code violates** | `internal/relay/{handshake,relayhttp,rosterhttp,message,peerstore,doc}.go`, `cmd/agent-bus/peer.go`, `CONTRACTS.md`, `CONTRACTS-HTTP.md`, `CONTRACTS-CLI.md`, `CONTRACTS-ONDISK.md`, `PROTOCOL.md` | AST/grep guard: no file saying `IT IS NOT REGISTERED ON ANY MUX` names a path `peermount.go` mounts |
| T5 | **P0-adjacent: reserve a relay wire-protocol version — the envelope is on the wire and the paragraph obliging the reservation was stale** | reservation + version field on BOTH surfaces + `PROTOCOL.md` | reservation id recorded; round-trip test asserting the field |
| T6 | **`client` package documents fail-closed verification while shipping fail-open** | `client/messages.go:161-162,384-386,389`, `client/keyring.go:21-24`. **Do not touch `client/wedge_test.go`.** | `go test -race -run TestReadDoesNotYetVerifyReceivedMessages ./client` stays PASS; doc grep pins the corrected sentence |
| T7 | **Invite revocation is documented in three places and implemented in none** | Either ship `agent-bus invite revoke` (wraps the existing `Store.Revoke`) **or** correct `cmd/agent-busctl/enrol.go:62`, `cmd/agent-bus/invite.go:297`, `client/invite.go:237`. **Prefer shipping it** — invariant 7. | `agent-bus invite revoke <id>` against a throwaway bus makes a minted invite unredeemable |
| T8 | **8 error remedies name `agent-busctl keygen` / `trust`, which do not exist** | `client/{store,client,keyring,canonical,messages,config}.go` | `! grep -rn 'agent-busctl \(keygen\|trust\)' client/` |
| T9 | **`INVARIANTS.md` truth pass — 8 false factual claims in the file agents must read IN FULL** | `INVARIANTS.md` #24,25,40,41,42,43,44,59 | line-pinned greps, each RED before |
| T10 | **`CLAUDE.md`/`AGENTS.md`: delete the false `crypto/ecdh` toolchain rationale; fix the layout map, `data/`, `CONTRACTS.md`, `/v1/discovery`** | #25,26,39,54,55,56,60 | `! grep -n 'crypto/ecdh' CLAUDE.md AGENTS.md` + `go list ./internal/...` vs the layout block |
| T11 | **`AGENTS.md` writes fabricated model ids (`Codex-opus-5`) into the cost audit trail** | `AGENTS.md:212,215,359`, and the 5 missing safety rails (#57, #58, and the missing "clean overlay of HEAD" section) | `! grep -n 'Codex-\(opus\|sonnet\)-5' AGENTS.md` |
| T12 | **`PROTOCOL.md`'s on-disk version registry omits versions 5, 6 and 7 — a live reservation-collision hazard** | `PROTOCOL.md:30-34` | grep asserting 5/6/7 named; cross-check `CONTRACTS-ONDISK.md:20` |
| T13 | **Session per-agent cap (32, no eviction) is documented as not existing — caused a production lockout** | `CONTRACTS-HTTP.md:439,54-58,619-628`. **Coordinate: the CLI-help half is in flight right now.** | grep pins the cap row; `go test -race -run TestActiveSessionCap ./internal/auth` |
| T14 | **Retire two standing directives that outlived their premise and now FORBID the fix** | `CONTRACTS.md:28-36` and `:110-125` | grep asserting both directives removed |
| T15 | **Durability inverted: `internal/auth/service.go:502` says main injects the MEMORY roster** | `internal/auth/service.go:502-505`, `CONTRACTS-ONDISK.md:479-491` | `go test -race -run TestTwoAgentsKeepTalkingAcrossARestartWithoutReEnrolling ./cmd/agent-bus` |
| T16 | **Mechanical stale-claim detector** — likely to MERGE with the in-flight `scripts/doc-check.sh`; read it first | new guard test | must go RED on today's tree |
| T17 | **`CONTRACTS-CLI.md` claims a "mechanically enforced" import guard that nothing runs** | either wire the clause into a real guard test (preferred) or correct the claim | new guard goes RED if `client` imports `internal/` |
| T18 | **The four agent ENTRY POINTS the invite gate missed** — `README` Quickstart, `agent-busctl --help`'s closing line, `bus-serve.sh`'s success message, `client/enrol.go`'s embedder doc | `README.md:107-121`; `cmd/agent-busctl/root.go` HEAD:412-413; `scripts/bus-serve.sh:410`; `client/enrol.go:63-66`. **Highest value single task in this list** — `3cedcb7` updated one doc and left these four. | Paste each printed command verbatim against a throwaway bus; all four must reach exit 0 |
| T19 | **`agent-busctl broadcast --help` never says the route is refused (501)** | `cmd/agent-busctl/send.go` broadcast help text. **Invariant 7: the CLI help is the agent's contract.** | `agent-busctl help broadcast \| grep -q 501` — RED today |
| T20 | **`client/transport.go:429-430`: the 403 remedy tells an agent to retry a refusal that never succeeds** | `client/transport.go:427-430`; align with `AGENT_PROTOCOL.md:526` | drive an un-invited enrol; assert the remedy does not say "retry" |
| T21 | **`CONTRACTS-AGENT.md` documents the log-scrape that `bus-serve.sh` deliberately refuses** | `CONTRACTS-AGENT.md` HEAD:48 (+ `:19` count, `:29`/`:104` fed-smoke) — **security-relevant: it describes a trust-anchor substitution the script exists to prevent** | grep pins the corrected sentence; `git ls-files scripts/ \| wc -l` matches the stated count |
| T22 | **`docs/THREE-BUS-DOCKER.md` tells the operator to ignore `fed-smoke.sh`, mint an invite to read a logged value, and mis-counts the lock-taking subcommands** | `:121-124`, `:266` vs `:284-286`, `:277-282`, `:396-401`, `:448-455`, `:463`. **Gate on running `fed-smoke.sh` first** (see §7 — I could not). | `fed-smoke.sh` exit status recorded; `docker logs` shown to emit `bus_cert_fingerprint` |
| T23 | **`AGENT_PROTOCOL.md`: `client-cert` undocumented (invariant-7 gap), TOC lists 10 of 15, `./data` described as tracked-and-empty when it is neither** | `AGENT_PROTOCOL.md` HEAD:100-102, `:19-33`, `:653`; + a `client-cert` section | `grep -q 'client-cert' AGENT_PROTOCOL.md` RED today; `git ls-files data \| wc -l` = 0 |
| T24 | **`docs/comms` self-audit numbers disagree with the CSVs, and `LABELLING-KEY.md`'s self-void clause has fired** | `METRICS.md:246-264,278,307-308`; `LABELLING-KEY.md:6-8`; `CONSENT.md:183` — **the epic's own credibility machinery**; needs a ruling on whether "publish" covers "print with a caveat" | recompute each aggregate from `LABELS-PASS2.csv` and pin it |
| T25 | **Investigate `TestWALRepairDoesNotReissueDiscardedIndex` — a P0's recorded `proof_cmd` names a test that does not exist** | verify against the Spec API, not the mirror | `go test -run TestWALRepairDoesNotReissueDiscardedIndex ./internal/wal` currently reports `[no tests to run]` — i.e. **VACUOUS** |

---

## 9. Cost / risk / rollback

**Cost.** T1 and T5 are code. Everything else in Tiers 1-2 is prose, and roughly 120 lines across 14
files. The expensive part is not editing — it is **proving each fix RED-before-GREEN with a
line-pinned grep**, per `CLAUDE.md:222-229`. Budget the verification, not the typing.

**Risk of fixing.**
- *Low* for T2, T3, T8-T14: prose in files no binary reads.
- *Medium* for T4 and T6: they edit Go doc comments in packages two agents are actively touching, and
  T4 spans seven files across three planes. **Split T4 by plane** — `internal/relay` comments,
  `cmd/agent-bus` comments, `CONTRACTS-*` — so no single commit straddles agents.
- *Highest* for T1: it changes a string served over HTTP, inside a document with an asserted
  16 KiB ceiling (`internal/httpapi/discovery_test.go:38`) and a stated "restate exactly, never
  soften" rule. It needs the security gate.
- **The real risk is over-correction.** Several stale notes sit beside *genuinely open* gaps
  (SIGN-5's unverified read path; no invite revocation; no cert rotation; `Service.idem` in memory;
  no `/v1/leave`). A sweep that deletes "not yet implemented" wholesale would erase **true**
  warnings. **Every fix must decide per sentence: shipped, or still open?** That is why these are 19
  scoped tasks and not one "doc truth sweep".

**Risk of NOT fixing.** Quantified from today's evidence: an agent following `README.md:113` cannot
get onto a bus; a caller reading `/v1/discovery` believes federation does not exist; an embedder
reading `client/messages.go:161` treats unsigned attacker-controlled messages as authenticated; a
security reviewer reading `internal/relay/handshake.go:131` may revert a shipped, gated mount; and
the next person to touch the relay envelope has written permission (`PROTOCOL.md:1017`) to break
live federation with no version to negotiate on.

**Rollback.** Each task is one pathspec-scoped commit over text. `git revert` is complete and
sufficient for every doc task. T1 and T5 need the standard chain (spec-keeper → implementer →
test-engineer → reviewer → security → documentation) and T5 additionally needs a **reserved** version
number from the orchestrator — **never pick one by reading `PROTOCOL.md:30-34`, which is finding #28**.

---

*Audit performed 2026-08-16 by `deep-diver`. Read-only: no source file, doc, durable log or data
directory was modified; all reproduction used throwaway data directories under `/tmp`, torn down
afterwards. `git status --porcelain` unchanged except for this file.*
