# EPIC UNASSIGNED — UNASSIGNED

[← all epics](../../SPEC.md)

**15 open / 21 total.** Full records live in `SPEC/UNASSIGNED/<task>/task.md`.

_Relations (real)_ are authoritative edges from the Spec Server. _Referenced (derived)_ are guesses parsed out of description free text — useful, but not a dependency list.

## Open tasks (15)

| Key | Title | Status | Prio | Task | Relations (real) | Referenced (derived) |
| --- | --- | --- | --- | --- | --- | --- |
| 8fb219ca-1236-4058-9020-afd52a7e93f3 | WAL checkpoint follow-up: exhaustive in-operation crash-path evidence | todo | P1 | [task.md](WAL-checkpoint-follow-up-exhaustive-in-operation-crash-p--8fb219ca/task.md) | _not fetched_ | [RELAY-19](../RELAY/RELAY-19--24e0bd11/task.md) [a1cbef29-400a-4a1e-9638-cc14d38a7ebf](WAL-foundation-authenticated-multi-applier-checkpoints-o--a1cbef29/task.md) [617ffe5a-db42-4aeb-89bb-d9b0889f6c19](../RELAY/Bound-retained-outbox-tombstone-resources-without-reopen--617ffe5a/task.md) |
| SPEC-API-LIST-SILENT-TRUNCATION | Task-list API silently truncates at 200 with no total, no next and no working pagination… | todo | P1 | [task.md](SPEC-API-LIST-SILENT-TRUNCATION--82f35b73/task.md) | _not fetched_ | [CORE-1](../CORE/CORE-1--eea035e4/task.md) [DUR-12-FU-KEYMODE](../DUR/DUR-12-FU-KEYMODE--f8bae169/task.md) [DUR-12-VERIFY](../DUR/DUR-12-VERIFY--f602c92e/task.md) [RELAY-11-FU-INGEST-LOOPGUARD](../RELAY/RELAY-11-FU-INGEST-LOOPGUARD--a41c273c/task.md) [RELAY-24-BLOCKER-HUBINGEST](../RELAY/RELAY-24-BLOCKER-HUBINGEST--9ee98866/task.md) [RELAY-44](../RELAY/RELAY-44--cec27a90/task.md) +2 more |
| dd2cdc20-8920-4e5b-bf0a-668f439cc3a6 | Reservation counters silently drift stale and hand out COLLIDING task keys (RELAY, DOCS,… | todo | P1 | [task.md](Reservation-counters-silently-drift-stale-and-hand-out-C--dd2cdc20/task.md) | _not fetched_ | [RELAY-55](../RELAY/RELAY-55--0a571a02/task.md) [RELAY-52](../RELAY/RELAY-52--67c6248d/task.md) [DOCS-30](../DOCS/DOCS-30--a311a067/task.md) [DOCS-6](../DOCS/DOCS-6--76879ad1/task.md) [ACK-18](../ACK/ACK-18--ac5f5fb2/task.md) [ACK-15](../ACK/ACK-15--a63b133d/task.md) +10 more |
| 017304e6-a088-40c9-b6c2-5cac4bc0fb66 | proof-check.sh head_token word-splits the LITERAL text of a VAR=$(...) proof_cmd, mis-ref… | todo | P2 | [task.md](proof-check.sh-head_token-word-splits-the-LITERAL-text-o--017304e6/task.md) | _not fetched_ | [dd2cdc20-8920-4e5b-bf0a-668f439cc3a6](Reservation-counters-silently-drift-stale-and-hand-out-C--dd2cdc20/task.md) |
| 48be31d6-7642-42ab-a5d4-fe2f2aa5d54a | Worktree-isolation sandbox bash guard false-matches the substring "complete" in the Spec… | todo | P2 | [task.md](Worktree-isolation-sandbox-bash-guard-false-matches-the--48be31d6/task.md) | _not fetched_ | — |
| 8ef2c753-daf1-4433-86e3-4eee4ad470dc | AST guard: assert a doc comment attaches to the declaration it names (repo-wide godoc-att… | todo | P2 | [task.md](AST-guard-assert-a-doc-comment-attaches-to-the-declarati--8ef2c753/task.md) | _not fetched_ | [RELAY-13](../RELAY/RELAY-13--97f3f1b4/task.md) [RELAY-9-FU-CODEGUARD](../RELAY/RELAY-9-FU-CODEGUARD--1e9b54d2/task.md) |
| CLI-11-FU-BUSIDBOUND | CLI-11-FU-BUSIDBOUND: internal/ids reads the bus-id file with an unbounded os.ReadFile | todo | P2 | [task.md](CLI-11-FU-BUSIDBOUND--82f9e452/task.md) | _not fetched_ | [CLI-11](CLI-11--bf966c07/task.md) |
| CLI-11-FU-LOADONLY | CLI-11-FU-LOADONLY: load-only accessors for bus key material and the bus id, so a READ ca… | todo | P2 | [task.md](CLI-11-FU-LOADONLY--b140724b/task.md) | _not fetched_ | [CLI-11](CLI-11--bf966c07/task.md) |
| CLI-11-FU-STATERR | CLI-11-FU-STATERR: invite mint tells an operator to restore a file that is present but un… | todo | P2 | [task.md](CLI-11-FU-STATERR--555967a6/task.md) | _not fetched_ | [CLI-11](CLI-11--bf966c07/task.md) |
| CLI-ENROL-E2E-SIGTERM-STARTUP-RACE | TestCLIEnrolEndToEnd SIGTERMs the priming bus ~1300 lines before its signal handler exists | todo | P2 | [task.md](CLI-ENROL-E2E-SIGTERM-STARTUP-RACE--1691873b/task.md) | _not fetched_ | [ACK-5](../ACK/ACK-5--5991ee1a/task.md) |
| CLIENT-CREDSTORE-CONCURRENCY-FLAKE | client.TestStoreConcurrentMutationsLoseNothing is flaky and may be reporting a real lost… | todo | P2 | [task.md](CLIENT-CREDSTORE-CONCURRENCY-FLAKE--c3095855/task.md) | _not fetched_ | [ACK-8](../ACK/ACK-8--bc12541b/task.md) |
| CLIENT-DOC-PHANTOM-VERIFYRECEIVED | CLIENT-DOC-PHANTOM-VERIFYRECEIVED: two comments name verifyReceivedMessage as the only ca… | todo | P2 | [task.md](CLIENT-DOC-PHANTOM-VERIFYRECEIVED--4a975a81/task.md) | _not fetched_ | [ACK-12-FU-WATCH-CORRELATION-KEY](../ACK/ACK-12-FU-WATCH-CORRELATION-KEY--f423959c/task.md) [ACK-12-FU-WATCH-CORRELATION-KEY-FU-RELAYVERIFY](../ACK/ACK-12-FU-WATCH-CORRELATION-KEY-FU-RELAYVERIFY--7e23e90f/task.md) |
| RELAY-13-FU-KEYGEN | RELAY-13-FU-KEYGEN: 3 error-message remedy strings name the nonexistent agent-busctl keyg… | todo | P2 | [task.md](RELAY-13-FU-KEYGEN--518b18c0/task.md) | _not fetched_ | [RELAY-13](../RELAY/RELAY-13--97f3f1b4/task.md) [SIGN-8](../SIGN/SIGN-8--71ef73d5/task.md) |
| 19595b60-16c8-4ce2-a67a-dcb8a1804ce1 | store.Message.WithOriginMessageID's doc undercounts what the returned copy shares: it say… | todo | — | [task.md](store.Message.WithOriginMessageID-s-doc-undercounts-what--19595b60/task.md) | _not fetched_ | [RELAY-24-FU-STOREMSGLOOKUP-SIGCOPY](../RELAY/RELAY-24-FU-STOREMSGLOOKUP-SIGCOPY--6e13a7d9/task.md) [88255314-6658-4bba-b1cd-76ebeec9806a](store.Append-retains-the-CALLER-s-slice-headers-and-hub--88255314/task.md) |
| 88255314-6658-4bba-b1cd-76ebeec9806a | store.Append retains the CALLER's slice headers, and hub.publish keeps using the same Mes… | todo | — | [task.md](store.Append-retains-the-CALLER-s-slice-headers-and-hub--88255314/task.md) | _not fetched_ | [RELAY-24-FU-STOREMSGLOOKUP-SIGCOPY](../RELAY/RELAY-24-FU-STOREMSGLOOKUP-SIGCOPY--6e13a7d9/task.md) [RELAY-24-FU-STOREMSGLOOKUP](../RELAY/RELAY-24-FU-STOREMSGLOOKUP--c6530638/task.md) |

## Closed tasks (6) — done, cancelled, superseded

| Key | Title | Status | Prio | Task | Relations (real) | Referenced (derived) |
| --- | --- | --- | --- | --- | --- | --- |
| CLI-11 | CLI-11: export the bus signing public key from the operator CLI | done | P1 | [task.md](CLI-11--bf966c07/task.md) | _not fetched_ | [RELAY-25](../RELAY/RELAY-25--10491a01/task.md) |
| a1cbef29-400a-4a1e-9638-cc14d38a7ebf | WAL foundation: authenticated multi-applier checkpoints over shared bus.wal | done | P1 | [task.md](WAL-foundation-authenticated-multi-applier-checkpoints-o--a1cbef29/task.md) | _not fetched_ | [RELAY-19](../RELAY/RELAY-19--24e0bd11/task.md) [617ffe5a-db42-4aeb-89bb-d9b0889f6c19](../RELAY/Bound-retained-outbox-tombstone-resources-without-reopen--617ffe5a/task.md) [8fb219ca-1236-4058-9020-afd52a7e93f3](WAL-checkpoint-follow-up-exhaustive-in-operation-crash-p--8fb219ca/task.md) |
| ZZ-LOCKTEST | ZZ-LOCKTEST: verify If-Match CAS | cancelled | P3 | [task.md](ZZ-LOCKTEST--e091e451/task.md) | _not fetched_ | — |
| ZZB-firsthal | probe | cancelled | — | [task.md](ZZB-firsthal--74cb9c06/task.md) | _not fetched_ | [ORCH-1](../ORCH/ORCH-1--e22449ec/task.md) [DEPLOY-1](../DEPLOY/DEPLOY-1--fa0c5a4e/task.md) [DEPLOY-2](../DEPLOY/DEPLOY-2--14f8ec3b/task.md) [DEPLOY-2-FU-CONTAINERNAME](../DEPLOY/DEPLOY-2-FU-CONTAINERNAME--e9dd20b4/task.md) [DEPLOY-REDEPLOY](../DEPLOY/DEPLOY-REDEPLOY--f801d128/task.md) |
| ZZB-secondha | probe | cancelled | — | [task.md](ZZB-secondha--95c580aa/task.md) | _not fetched_ | [CLI-BUSCTL-IMAGE](../CLI/CLI-BUSCTL-IMAGE--9be2105d/task.md) |
| e36661b0-687e-465e-b72f-e33245088e38 | keypatch-probe (spec-keeper bug repro, safe to cancel) | cancelled | — | [task.md](keypatch-probe-spec-keeper-bug-repro-safe-to-cancel--e36661b0/task.md) | _not fetched_ | — |

---

_Generated by `scripts/gen-spec-mirror.sh`; never hand-edit._
