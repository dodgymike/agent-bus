# EPIC DOCS — Documentation

[← all epics](../../SPEC.md)

**14 open / 17 total.** Full records live in `SPEC/DOCS/<task>/task.md`.

_Relations (real)_ are authoritative edges from the Spec Server. _Referenced (derived)_ are guesses parsed out of description free text — useful, but not a dependency list.

## Open tasks (14)

| Key | Title | Status | Prio | Task | Relations (real) | Referenced (derived) |
| --- | --- | --- | --- | --- | --- | --- |
| DOCS-2 | DOCS-2: PROTOCOL.md -- wire protocol + on-disk format | todo | P0 | [task.md](DOCS-2--41c52cfa/task.md) | — | [DUR-4-FU-DOCS](../DUR/DUR-4-FU-DOCS--0b6d5c11/task.md) [DUR-12](../DUR/DUR-12--cbc9ab0c/task.md) [804fa84c-e97b-4737-8866-801f87468da4](../DUR/Document-what-the-bus-does-with-an-UNKNOWN-WAL-record-ty--804fa84c/task.md) |
| MTLS-VERIFY-FU-DOCSCHEME | MTLS-VERIFY-FU-DOCSCHEME: README + AGENT_PROTOCOL still tell agents to dial http:// a bus… | todo | P0 | [task.md](MTLS-VERIFY-FU-DOCSCHEME--cb4fd330/task.md) | blocks [HANDOVER-README](../HANDOVER/HANDOVER-README--1dc9cf90/task.md) | [MTLS-LISTENER](../MTLS/MTLS-LISTENER--17e70a7e/task.md) |
| 4a6e7001-ca2a-430a-a5e6-39e922d7325f | CONTRACTS-AGENT.md/AGENT_PROTOCOL.md document the removed log-scrape as bus-serve.sh star… | todo | P1 | [task.md](CONTRACTS-AGENT.md-AGENT_PROTOCOL.md-document-the-remove--4a6e7001/task.md) | follow-up of [10e93262-8e34-4738-b435-bfe23d880057](../MTLS/Derive-the-bus-fingerprint-from-the-certificate-not-the--10e93262/task.md) | [10e93262-8e34-4738-b435-bfe23d880057](../MTLS/Derive-the-bus-fingerprint-from-the-certificate-not-the--10e93262/task.md) |
| DOCS-3 | DOCS-3: CONTRACTS.md -- route/flag/env-var/record-type table | todo | P1 | [task.md](DOCS-3--a24bb214/task.md) | — | [CONTRACTS-SPLIT](CONTRACTS-SPLIT--360a2679/task.md) |
| DUR-11-FU-CONTRACTS | DUR-11-FU-CONTRACTS: CONTRACTS.md still documents the reverted refuse-to-start WAL policy… | todo | P1 | [task.md](DUR-11-FU-CONTRACTS--5b178dde/task.md) | — | [DUR-11](../DUR/DUR-11--884d3da4/task.md) [DUR-12](../DUR/DUR-12--cbc9ab0c/task.md) [CONTRACTS-SPLIT](CONTRACTS-SPLIT--360a2679/task.md) [e120153b-9d8a-4b6a-bd4e-89431954496b](../DUR/Fix-WAL-recovery-reissuing-a-discarded-tail-record-index--e120153b/task.md) [db350e39-3dde-4166-b241-b21fa4635359](../DUR/Whole-log-quarantine-reissued-EVERY-sequence-number-ever--db350e39/task.md) |
| IDEM-11-FU-PAPERTRAIL | IDEM-11-FU-PAPERTRAIL: DECISIONS.md and CONTRACTS-HTTP.md state the OPPOSITE of what IDEM… | todo | P1 | [task.md](IDEM-11-FU-PAPERTRAIL--c416a458/task.md) | — | [IDEM-11](../IDEM/IDEM-11--8e2c4de3/task.md) [IDEM-10](../IDEM/IDEM-10--b28e5153/task.md) [IDEM-11-FU-DOWNGRADE](../DUR/IDEM-11-FU-DOWNGRADE--84f5ad57/task.md) |
| MSG-FU-SUFFIXFLOOR-FU-DOCS | MSG-FU-SUFFIXFLOOR-FU-DOCS: PROTOCOL.md and internal/ids docs still say the suffix wiring… | todo | P1 | [task.md](MSG-FU-SUFFIXFLOOR-FU-DOCS--e5fa08ba/task.md) | blocks [CONTEXT-PROTOCOL-WALFLOOR-DEDUP](../CONTEXT/CONTEXT-PROTOCOL-WALFLOOR-DEDUP--1e9cec15/task.md) | [MSG-FU-SUFFIXFLOOR](../ID/MSG-FU-SUFFIXFLOOR--94159d93/task.md) |
| 0ba2372a-09f7-4f05-bd33-98a5f80e0e6f | Journal catch-up: DECISIONS.md + AGENT_LOG.md entries owed by INVITE-MINT and MTLS-ROTATE | todo | P2 | [task.md](Journal-catch-up-DECISIONS.md-AGENT_LOG.md-entries-owed--0ba2372a/task.md) | blocks [CONTEXT-LOG-RETIRE](../CONTEXT/CONTEXT-LOG-RETIRE--116179c8/task.md) | [INVITE-MINT](../INVITE/INVITE-MINT--1d0d0e60/task.md) [MTLS-ROTATE](../MTLS/MTLS-ROTATE--c2e8df5b/task.md) |
| 83850937-a3c9-4b90-8ac6-19655233cb13 | DECISIONS.md carries the pre-correction (wrong) accepted-limit sentence for the MAC key;… | todo | P2 | [task.md](DECISIONS.md-carries-the-pre-correction-wrong-accepted-l--83850937/task.md) | — | [DUR-12](../DUR/DUR-12--cbc9ab0c/task.md) |
| DISCOVERY-DOC-FU-README | DISCOVERY-DOC-FU-README: README.md still documents the old three-field /v1/info body | todo | P2 | [task.md](DISCOVERY-DOC-FU-README--be3c84f3/task.md) | blocks [HANDOVER-README](../HANDOVER/HANDOVER-README--1dc9cf90/task.md) | [DISCOVERY-DOC](../CORE/DISCOVERY-DOC--2d7ce37b/task.md) |
| a695f85f-0c69-42a8-a653-deed4960a610 | PROTOCOL.md §8 cites Spec Server task id INVITE-PEERGUARD (f5d91dbe) as if it were a comm… | todo | P2 | [task.md](PROTOCOL.md-8-cites-Spec-Server-task-id-INVITE-PEERGUARD--a695f85f/task.md) | — | [MTLS-RELAYGUARD](../MTLS/MTLS-RELAYGUARD--8192c3c7/task.md) [INVITE-PEERGUARD](../INVITE/INVITE-PEERGUARD--f5d91dbe/task.md) [DOCS-2](DOCS-2--41c52cfa/task.md) [e120153b-9d8a-4b6a-bd4e-89431954496b](../DUR/Fix-WAL-recovery-reissuing-a-discarded-tail-record-index--e120153b/task.md) [db350e39-3dde-4166-b241-b21fa4635359](../DUR/Whole-log-quarantine-reissued-EVERY-sequence-number-ever--db350e39/task.md) |
| f0ef1ed9-cbcb-4ddd-9dec-394e1800ae78 | Stale CONTRACTS.md pointers after the CONTRACTS-SPLIT: README.md:88, AGENT_PROTOCOL.md:12… | todo | P2 | [task.md](Stale-CONTRACTS.md-pointers-after-the-CONTRACTS-SPLIT-RE--f0ef1ed9/task.md) | blocks [CONTEXT-DRIFT-WRAPPERS](../CONTEXT/CONTEXT-DRIFT-WRAPPERS--1a9bf503/task.md)<br>blocks [HANDOVER-README](../HANDOVER/HANDOVER-README--1dc9cf90/task.md) | [CONTRACTS-SPLIT](CONTRACTS-SPLIT--360a2679/task.md) [DUR-11-FU-CONTRACTS](DUR-11-FU-CONTRACTS--5b178dde/task.md) |
| 6b44ee89-612a-4d3d-9c39-1302c07d3c39 | AGENT_PROTOCOL.md error-block label says remedy: but the CLI prints try: | todo | P3 | [task.md](AGENT_PROTOCOL.md-error-block-label-says-remedy-but-the--6b44ee89/task.md) | — | [MTLS-ROTATE](../MTLS/MTLS-ROTATE--c2e8df5b/task.md) |
| 88781750-0005-4c2f-8375-2d93dc1560b8 | DECISIONS.md:1302 cites a superseded bus-serve.sh line for the plaintext-probe follow-on | todo | P3 | [task.md](DECISIONS.md-1302-cites-a-superseded-bus-serve.sh-line-f--88781750/task.md) | follow-up of [10e93262-8e34-4738-b435-bfe23d880057](../MTLS/Derive-the-bus-fingerprint-from-the-certificate-not-the--10e93262/task.md) | [MTLS-LISTENER](../MTLS/MTLS-LISTENER--17e70a7e/task.md) [MTLS-VERIFY](../MTLS/MTLS-VERIFY--9dab7303/task.md) [10e93262-8e34-4738-b435-bfe23d880057](../MTLS/Derive-the-bus-fingerprint-from-the-certificate-not-the--10e93262/task.md) |

## Closed tasks (3) — done, cancelled, superseded

| Key | Title | Status | Prio | Task | Relations (real) | Referenced (derived) |
| --- | --- | --- | --- | --- | --- | --- |
| DOCS-1 | DOCS-1: README.md + DECISIONS.md seed | done | P0 | [task.md](DOCS-1--909e2152/task.md) | — | — |
| CONTRACTS-SPLIT | CONTRACTS-SPLIT: split CONTRACTS.md into per-plane files (pure move) + retarget every pro… | done | P1 | [task.md](CONTRACTS-SPLIT--360a2679/task.md) | — | [AUTH-1-FU-LISTENADDR](../AUTH/AUTH-1-FU-LISTENADDR--c27f9439/task.md) [LISTENADDR-FU-CONTRACTS](LISTENADDR-FU-CONTRACTS--b0a5630b/task.md) [DUR-11-FU-CONTRACTS](DUR-11-FU-CONTRACTS--5b178dde/task.md) [ID-2-WIRING-SEAL](../ID/ID-2-WIRING-SEAL--8c9b6489/task.md) [ID-2-WIRING-OBSERVER](../ID/ID-2-WIRING-OBSERVER--c31f6999/task.md) [AUTH-1-FU-ACTIVECAP](../AUTH/AUTH-1-FU-ACTIVECAP--2d92b699/task.md) +1 more |
| LISTENADDR-FU-CONTRACTS | LISTENADDR-FU-CONTRACTS: CONTRACTS.md CLI-flag table still shows -listen default :8080 | done | P1 | [task.md](LISTENADDR-FU-CONTRACTS--b0a5630b/task.md) | — | [AUTH-1-FU-LISTENADDR](../AUTH/AUTH-1-FU-LISTENADDR--c27f9439/task.md) [AUTH-1-FU-PENDINGCAP](../AUTH/AUTH-1-FU-PENDINGCAP--687ad8c9/task.md) |

## Epic description

README, DECISIONS.md seed, PROTOCOL.md (wire protocol + on-disk format), CONTRACTS.md.

---

_Generated by `scripts/gen-spec-mirror.sh`; never hand-edit._
