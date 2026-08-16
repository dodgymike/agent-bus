# RELAY-24-BLOCKER-HUBINGEST-FU-AUDITHASH-DOC: Record the relayed audit content-hash decision in DECISIONS.md and amend PROTOCOL.md 8.6

| Field | Value |
| --- | --- |
| Public id | `7126f08b-bd87-487e-90aa-a84fdb18976b` |
| Key | RELAY-24-BLOCKER-HUBINGEST-FU-AUDITHASH-DOC |
| Epic | [RELAY](../epic.md) |
| Status | done |
| Priority | P0 |
| Component | docs |
| Section | backlog |
| Tags | — |
| Created | 2026-08-14T15:11:46.935467+00:00 |
| Updated | 2026-08-14T16:12:36.030391+00:00 |
| Completed | 2026-08-14T16:12:36.030376+00:00 |

## Proof command

```sh
grep -qF 'the relayed audit content hash is taken under the ORIGIN' DECISIONS.md && grep -qF '#### 8.6.1 The RELAYED case' PROTOCOL.md && echo DOCS_OK
```

## Description

Filed 2026-08-14 from the RELAY-24-BLOCKER-HUBINGEST reviewer gate. COMMIT-BLOCKING for that task.

[CORRECTED 2026-08-14 -- the original grounding quote below was retracted by the reviewer as imprecise. Do NOT cite PROTOCOL.md:730-733 ("never folded into the canonical bytes and never substituted for them") for this task: that sentence governs the OUT-OF-BAND FIELDS it names (bus path, local delivery sequence, byte size), NOT the message-id/sequence substitution. It would not survive a careful reader.]

THE FACT: a relayed message's LOCAL record has a LOCAL message id and a FOREIGN sender, and signing.Canonicalize refuses that pair UNCONDITIONALLY (internal/signing/canonical.go:254, `senderBus != originBus`, an EXACT compare). So a relayed record has NO canonical bytes of its own. The audit content hash is therefore computed over the ORIGIN's canonical bytes -- byte-identical to what relay.RelayedMessage.CanonicalBytes builds and to what internal/relay already verified the origin signature against. The reviewer verified the derivation field-by-field and by execution, and confirmed NO better in-boundary option exists.

WHY IT BLOCKS THE COMMIT (CORRECTED GROUNDING): the substitution REVERSES A RECORDED POSITION, so it cannot land on a code comment alone. The correct grounding is DECISIONS.md:4288-4295, the SIGN-3 broadcast entry (2026-08-08c amendment):
  "So `internal/hub/audit.go` refuses rather than substituting a value. Any value chosen there would settle SIGN-3 by accident -- in a file nobody would think to read when they came to settle it properly -- and would then be written into an append-only trail that cannot be edited afterwards."
That is a DATED DECISION naming the exact file this change makes substitute. The reviewer notes the corrected grounding makes the requirement STRONGER, not weaker: the substitution is defensible ONLY because the relayed case is the opposite of the broadcast case -- the correct bytes exist, are already computed by relay.RelayedMessage.CanonicalBytes, and were already verified against a signature. Broadcast has no such bytes; relay does.

THE REVIEWER HAS ALREADY WRITTEN THE FULL READY-TO-APPLY TEXT for both documents -- a PROTOCOL.md §8.6 appendix and a dated DECISIONS.md section. The documentation agent should ASK THE ORCHESTRATOR for this text rather than redrafting it. Key points the text must preserve:
  - The §8.6 rule itself is UNCHANGED -- this is that rule APPLIED to a message signed on another bus, not a second rule.
  - Reader-facing consequence: a relayed record's hash does NOT reproduce from its own message_id/sequence, but DOES reproduce from the origin's pair, which is durably stored as the message record's `idempotency_key` (IngestRelayed passes it as the key).
  - The discriminator is the SENDER's bus half, and NEVER the bus path -- because internal/hub/buspath_test.go publishes a 3-hop path with a LOCAL sender, so the bus path cannot be what distinguishes local from relayed.
  - The SIGN-3 broadcast refusal STANDS UNCHANGED: hub.Broadcast still fails closed, and this task does not touch that.
  - Hashing the bare body remains forbidden.

NEEDED: (1) a dated DECISIONS.md entry recording the substitution and its (corrected) justification; (2) an amendment to PROTOCOL.md 8.6 that states the relayed exception normatively. Both files are OUTSIDE internal/hub, so this is a documentation-agent task, not an implementer one.

PROOF NOTE: this is a doc proof, the MORE dangerous family. Pin the SPECIFIC added lines (the DECISIONS.md heading text and the exact 8.6 sentence), and CONFIRM THE PROOF IS RED BEFORE THE FIX -- an incidental match elsewhere in either file is not evidence. Tighten the stored proof_cmd to the exact anchors once the wording is chosen.

[PROOF CORRECTED 2026-08-14 -- the previously stored proof_cmd was flagged by the reviewer gate as the DANGEROUS grep-based doc-proof family CLAUDE.md warns about: `grep -n 'origin' PROTOCOL.md | sed -n '/8.6/p'` matches any incidental "origin" near an "8.6", and PROTOCOL.md 8.5 immediately above is dense with the word "origin" -- it would pass on an incidental match without the amendment ever being written. Corrected proof_cmd (pins the exact added headings, -qF for literal-text match, not pattern):
  grep -qF 'the relayed audit content hash is taken under the ORIGIN' DECISIONS.md && grep -qF '#### 8.6.1 The RELAYED case' PROTOCOL.md && echo DOCS_OK

PROOF DISCIPLINE: this proof MUST be confirmed RED before the edit and GREEN after. A doc proof that was never observed failing is not evidence that it can fail. Run it through `bash scripts/proof-check.sh` and quote the verdict. Both strings are literal text from the supplied amendment: the DECISIONS.md heading line and the PROTOCOL.md 8.6.1 subsection heading.

CONFIRMED INSERTION POINTS (verified against the live files 2026-08-14, before either edit lands):
  - PROTOCOL.md: pure INSERTION between line 733 ('substituted for them.') and line 735 ('### 8.7 Test vectors are a published artefact'). Nothing deleted or edited.
  - DECISIONS.md: APPEND at end of file. The file is 4805 lines and ends with '<!-- ===== END 2026-08-14 INVITE-GATE ===== -->'. The SIGN-3 entry being reversed is at DECISIONS.md:4291 and must stay exactly as written, unedited.
  - Date for the new DECISIONS.md section: 2026-08-14 (confirmed correct -- the file already carries four 2026-08-14 sections: MTLS-CLIENTAUTH:4482, RELAY-45:4570, CLI-11:4644, INVITE-GATE:4718).
  - WARNING for the editor: DECISIONS.md is in `MM` state with other agents' sections in flight concurrently. Per CLAUDE.md's pathspec-commit trap, check `git status --porcelain -- DECISIONS.md` and diff the WORKTREE (`git diff HEAD -- DECISIONS.md`), never just the index, before committing.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CLI-11](../../UNASSIGNED/CLI-11--bf966c07/task.md) — CLI-11: export the bus signing public key from the operator CLI (done)
- [INVITE-GATE](../../INVITE/INVITE-GATE--05a5216d/task.md) — INVITE-GATE: POST /v1/enroll REQUIRES a valid invite and fails closed; invite consumption… (done)
- [MTLS-CLIENTAUTH](../../MTLS/MTLS-CLIENTAUTH--cc9558a8/task.md) — MTLS-CLIENTAUTH: request a client certificate on every connection WITHOUT a CA -- tls.Req… (done)
- [RELAY-24-BLOCKER-HUBINGEST](../RELAY-24-BLOCKER-HUBINGEST--9ee98866/task.md) — RELAY-24-BLOCKER-HUBINGEST: internal/hub exported relay-ingest entry point -- foreign sen… (done)
- [RELAY-45](../RELAY-45--4be32336/task.md) — RELAY-45: Bind inbound peer TLS certificate to the adjacent bus principal (done)
- [SIGN-3](../../SIGN/SIGN-3--f2daa6bc/task.md) — SIGN-3: Broadcast signature covers the recipient set (prevents split-content broadcasts) (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
