# Enforce data-directory permissions at startup, and bound the message-seq floor

| Field | Value |
| --- | --- |
| Public id | `be447589-6583-4d5c-a9d4-ec9d9fef0f1c` |
| Key | _(null in the export)_ |
| Epic | [CORE](../epic.md) |
| Status | done |
| Priority | P0 |
| Component | security |
| Section | backlog |
| Tags | — |
| Created | 2026-08-07T21:30:08.925674+00:00 |
| Updated | 2026-08-08T13:28:29.904657+00:00 |
| Completed | 2026-08-08T13:28:29.904642+00:00 |

## Proof command

```sh
go test -race -count=1 -run 'TestRunRefuses|TestRunTightens|TestRunAccepts|TestSeqFloor' ./cmd/agent-bus/... ./internal/hub/...
```

## Description

FINDING (reported by an independent security agent with a working exploit, confirmed against the tree at HEAD 9f2878a):
A forged message-seq-floor file permanently bricks all sends and needs no key. Writing the file with floor = 2^64-1 plus a VALID SHA-256 over the body (the digest is UNKEYED, so computing it is one line of Python) makes the bus boot HEALTHY -- /healthz ok, roster intact, replay fine, no warning -- while every /v1/mint 500s forever with "hub: allocating a message sequence: ids: sequence exhausted: math.MaxUint64 has been issued and a sequence number is never reused". Restart does not help; the file persists. The bus serves, enrols and issues sessions, and cannot deliver a single message.

WHY THE EXISTING "unkeyed is defensible" RULING IS WRONG (three verified points):
1. "forge-low is equivalent to delete" is true but irrelevant. Deleting the file RECOVERS (the bus logs that it recreated the floor from what the log proves and resumes). Forge-HIGH BRICKS. Opposite outcomes, so collapsing forge to delete only checks the harmless direction.
2. The justification in seqfloorfile.go encodeSeqFloor comment equates two independent permissions: it says the digest need not be keyed because "an attacker with write access to the data directory can read the WAL MAC key sitting next to it anyway". Directory-write and file-read are DIFFERENT bits. Unlinking and replacing a file needs DIRECTORY write, not file write, so the 0600 on message-seq-floor does not protect it -- while wal-mac.key being 0600 does stop that same attacker reading it. There is therefore a real attacker who can forge the unkeyed seq floor but CANNOT forge the keyed WAL index floor.
3. Nothing enforces the 0700 the whole argument assumes. cmd/agent-bus/main.go:214 calls os.MkdirAll(cfg.DataDir, 0o700), and MkdirAll does NOT chmod an ALREADY-EXISTING directory. Empirically a pre-created 0777 data dir stays 0777, healthy, with no check and no warning. The live data dir is 0775 today. Meanwhile client/store.go and client/clientcert.go DO stat, tighten to 0700 and warn the operator. The client protects its credential directory; the server does not protect its own data directory at all. That asymmetry is the defect.

SCOPE OF THE FIX:
- PRIMARY: at server startup, stat the data directory and act on group- or other-writable modes. This closes the whole class in one place rather than per-file (message-seq-floor, wal-index-floor, bus-id, agent-suffixes, wal-mac.key). The refuse-vs-tighten-and-warn choice must be justified in DECISIONS.md, and the operator must be told the actual mode and the remedy.
- SECONDARY: bound the floor in internal/hub/seqfloorfile.go. A floor at 2^64-1 has no legitimate cause -- from 0, in MintBatchSize steps, no real bus reaches it in any lifetime. Reject an implausibly-high floor as corrupt-or-tampered with a named remedy, converting a silent permanent brick into a loud one-step-remedy refusal.
- EXPLICITLY NOT: simply keying message-seq-floor with the WAL MAC and calling it fixed. Keying does not fix points 2 or 3 (the MAC key is co-located; a dir-writer who can read 0600 files gets it anyway). Keying is worth doing for consistency but as a SEPARATE, honestly-labelled change.

FILE OWNERSHIP for the agent working it: cmd/agent-bus/**, internal/hub/**, plus dated appends to DECISIONS.md and AGENT_LOG.md. NOT internal/wal/**, client/**, cmd/agent-busctl/**, scripts/**, CONTRACTS-CLI.md, internal/auth/**, internal/httpapi/**, README.md, AGENT_PROTOCOL.md, PROTOCOL.md, CONTRACTS-ONDISK.md.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocked by** [9fd58deb-6fb8-4d4e-8bf1-6df01329c3b2](../../DUR/Expose-on-wal.Recovered-the-highest-index-a-record-actua--9fd58deb/task.md)
- **follow-up** [0d0c54a0-85b8-4117-bb1f-2e7c56df153a](../../DUR/existedAtOpen-is-not-a-snapshot-it-returns-a-field-persi--0d0c54a0/task.md)
- **follow-up** [3e43c52c-ae62-4b8c-aabb-1b9f7f62d82f](../The-data-dir-permission-gate-checks-MODE-but-not-OWNERSH--3e43c52c/task.md)
- **follow-up** [4f276d2a-88d5-45fd-90e1-810429b3fb78](../../DUR/maxPlausibleSeqFloor-is-enforced-on-the-READ-path-only-h--4f276d2a/task.md)
- **follow-up** [7fbe58ec-6b27-43dd-b0c1-986d7c702870](../../DUR/Settle-the-message-seq-floor-KEYING-question-as-an-expli--7fbe58ec/task.md)
- **follow-up** [9bf6f55f-f069-429d-b004-82fba72d45c2](../../DUR/maxPlausibleSeqFloor-is-2-56-which-exceeds-the-JSON-safe--9bf6f55f/task.md)
- **follow-up** [c47379ae-9873-4800-a442-03e34a7f1294](../invite-mint-bypasses-the-data-directory-permission-gate--c47379ae/task.md)
- **follow-up** [d9cfaa61-d643-44eb-b38f-22dbd29e6692](../../DUR/Close-the-two-coverage-gaps-the-security-gates-declared--d9cfaa61/task.md)
- **follow-up of** [259b7033-2191-423f-bb7b-cff8c6b59dc1](../../DUR/Bound-the-wal-index-floor-reserved-value-the-same-way-as--259b7033/task.md)
- **supersedes** [72d7f10d-5f4a-4ad7-a680-e548c331eb20](../os.MkdirAll-cfg.DataDir-0o700-at-main.go-157-never-tight--72d7f10d/task.md)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [0d0c54a0-85b8-4117-bb1f-2e7c56df153a](../../DUR/existedAtOpen-is-not-a-snapshot-it-returns-a-field-persi--0d0c54a0/task.md) — existedAtOpen() is not a snapshot -- it returns a field persistLocked mutates, so reorder… (todo)
- [259b7033-2191-423f-bb7b-cff8c6b59dc1](../../DUR/Bound-the-wal-index-floor-reserved-value-the-same-way-as--259b7033/task.md) — Bound the wal-index-floor reserved value the same way as the message-seq floor (todo)
- [2a38cdec-528f-47ef-8f38-7f83465b0213](../../DUR/CONTRACTS-ONDISK.md-and-four-sibling-Go-comments-oversta--2a38cdec/task.md) — CONTRACTS-ONDISK.md and four sibling Go comments overstate the seq-floor migration guard:… (todo)
- [3e43c52c-ae62-4b8c-aabb-1b9f7f62d82f](../The-data-dir-permission-gate-checks-MODE-but-not-OWNERSH--3e43c52c/task.md) — The data-dir permission gate checks MODE but not OWNERSHIP, and follows symlinks -- and i… (deferred)
- [4ae04e3b-4a24-45fe-8521-c548c930c1db](../../DUR/Rewrite-the-seq-floor-migration-WARN-and-its-comment-Log--4ae04e3b/task.md) — Rewrite the seq-floor migration WARN (and its comment/LogRepaired doc) so it claims only… (done)
- [4f276d2a-88d5-45fd-90e1-810429b3fb78](../../DUR/maxPlausibleSeqFloor-is-enforced-on-the-READ-path-only-h--4f276d2a/task.md) — maxPlausibleSeqFloor is enforced on the READ path only -- hub can persist a floor its own… (deferred)
- [7fbe58ec-6b27-43dd-b0c1-986d7c702870](../../DUR/Settle-the-message-seq-floor-KEYING-question-as-an-expli--7fbe58ec/task.md) — Settle the message-seq-floor KEYING question as an explicit follow-up, replacing the 'wor… (todo)
- [9bf6f55f-f069-429d-b004-82fba72d45c2](../../DUR/maxPlausibleSeqFloor-is-2-56-which-exceeds-the-JSON-safe--9bf6f55f/task.md) — maxPlausibleSeqFloor is 2^56, which exceeds the JSON safe-integer range 2^53 -- seq ships… (todo)
- [9fd58deb-6fb8-4d4e-8bf1-6df01329c3b2](../../DUR/Expose-on-wal.Recovered-the-highest-index-a-record-actua--9fd58deb/task.md) — Expose on wal.Recovered the highest index a record actually CONSUMED (todo)
- [MSG-FU-SUFFIXFLOOR-FU-ROSTERASSERT](../../ID/MSG-FU-SUFFIXFLOOR-FU-ROSTERASSERT--6b0e561e/task.md) — MSG-FU-SUFFIXFLOOR-FU-ROSTERASSERT: assert a freshly-minted agent-suffix is not already i… (todo)
- [c47379ae-9873-4800-a442-03e34a7f1294](../invite-mint-bypasses-the-data-directory-permission-gate--c47379ae/task.md) — invite mint bypasses the data-directory permission gate entirely -- the invite blob is th… (deferred)
- [d9cfaa61-d643-44eb-b38f-22dbd29e6692](../../DUR/Close-the-two-coverage-gaps-the-security-gates-declared--d9cfaa61/task.md) — Close the two coverage gaps the security gates declared UNVERIFIED on the seq-floor/data-… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
