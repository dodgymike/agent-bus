# CRYPTO-7: Ratchet-state durability and recovery (CRASH-INJECTION TEST REQUIRED)

| Field | Value |
| --- | --- |
| Public id | `f90d7889-46cd-429e-9c73-4eccbaaeddec` |
| Key | CRYPTO-7 |
| Epic | [CRYPTO](../epic.md) |
| Status | deferred |
| Priority | P3 |
| Component | durability |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T09:41:21.042503+00:00 |
| Updated | 2026-08-02T13:07:10.932228+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run TestRatchetStateCrash ./internal/store ./internal/crypto
```

## Status note

PARKED / DEFERRED -- NOT CANCELLED (2026-08-02). Encryption is deferred, not abandoned: the user's instruction was "ok, let's keep it simple and just use standard message auth/integrity using libsodium. encryption can come later" -- "later" means later, so this task is kept INTACT for revival rather than deleted. Status is `deferred` (deliberately NOT `todo`) so claim-next can never hand it out: no agent may start X3DH / Double Ratchet / AEAD work off the back of it. BLOCKED ON: a fresh, explicit user decision to add encryption, recorded in DECISIONS.md -- the 2026-08-02 "Message auth/integrity only" entry says to revisit encryption as a NEW decision and never by reviving the superseded ratchet plan. When/if that happens, re-read this task critically: its ratchet-era assumptions (go.mau.fi/libsignal, status-im/doubleratchet's default replay acceptance, the go1.19 constraint) are stale, and invariant 9 still forbids writing any of it ourselves. In the meantime the shipped design is the SIGN epic (Ed25519 detached signatures; authenticity + integrity, NO confidentiality). Specifically superseded-in-the-meantime by SIGN-4 (recipient-side durable replay cursor), which is the only durable per-peer crypto state a sign-only design has -- far smaller than a ratchet checkpoint store.

## Description

GATED: do not start until CRYPTO-1 (design spike) is done and its DECISIONS.md entry is recorded -- the spike chooses the crypto library/primitives, the audit-log trade-off, the ratchet-state durability model, the broadcast/relay scheme and the key-trust model. Implementing before that decision exists is guessing. THE HARD ONE. Double Ratchet state is MUTABLE per-session state that advances with every message; the store is append-only and recovery REPLAYS it (invariants 5 and 6). If ratchet state is lost the session breaks; if it is REPLAYED or ROLLED BACK on recovery you get key and NONCE REUSE, which is a catastrophic AEAD failure -- two ciphertexts under one key/nonce leaks plaintext. Implement exactly the persistence model CRYPTO-1 specified: how state is encoded, how the state advance is committed and fsynced RELATIVE to the two-phase message commit (DUR-2), and how replay reconstructs the ratchet without re-advancing or rewinding it. Per CLAUDE.md, a durability claim needs a CRASH-INJECTION TEST, not code review: write, kill at each chosen point in the write path, and assert what recovery yields. At minimum prove: (a) no key/nonce is EVER reused across a crash, at any injection point; (b) an acknowledged message is decryptable after recovery; (c) a message killed before commit leaves neither a half-advanced ratchet nor an acked-but-lost message; (d) the recovered state is a PREFIX of the accepted history. Also bound and persist the skipped-message-key store. Any on-disk record type number or wire protocol version this needs MUST be reserved via POST /api/v1/projects/agent-bus/reservations -- never hand-picked. Coordinate with DUR-2/DUR-3/DUR-6 rather than forking a second write path.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CRYPTO-1](../CRYPTO-1--30570fb9/task.md) — CRYPTO-1: DESIGN SPIKE -- Signal-style E2E crypto for agent-bus (CRYPTO_DEEPDIVE.md + DEC… (done)
- [DUR-2](../../DUR/DUR-2--4132b879/task.md) — DUR-2: Two-phase prepare-&gt;commit write path (done)
- [DUR-3](../../DUR/DUR-3--d8a991ea/task.md) — DUR-3: Replay/recovery on start (done)
- [DUR-6](../../DUR/DUR-6--d56a997d/task.md) — DUR-6: Crash-injection test suite for the write path (done)
- [SIGN-4](../../SIGN/SIGN-4--33fa35d8/task.md) — SIGN-4: Replay/freshness -- enforced SERVER-SIDE at ingest, never by recipient-side seque… (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CRYPTO-5](../CRYPTO-5--9f3f8065/task.md) — CRYPTO-5: X3DH session establishment between two agents (deferred)
- [CRYPTO-6](../CRYPTO-6--260e6003/task.md) — CRYPTO-6: Double Ratchet encrypt/decrypt on the direct-message send path (deferred)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
