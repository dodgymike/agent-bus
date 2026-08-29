# MTLS-MIGRATE: pin add cannot migrate a pre-TLS (http-enrolled) identity onto TLS; its own remedy mints a new id, contradicting DECISIONS.md E3

| Field | Value |
| --- | --- |
| Public id | `59883178-6bcd-4996-91aa-3c5c3322d6ea` |
| Key | _(null in the export)_ |
| Epic | [MTLS](../epic.md) |
| Status | done |
| Priority | P0 |
| Component | client |
| Section | backlog |
| Tags | field-evidence, migration, tls |
| Created | 2026-08-07T21:18:12.234038+00:00 |
| Updated | 2026-08-23T21:09:40.690020+00:00 |
| Completed | 2026-08-23T21:09:40.690003+00:00 |

## Proof command

```sh
go test -race -run "^TestPreTLSMigration|^TestPinBootstrap" ./client ./cmd/agent-busctl ./internal/httpapi
```

## Status note

Docs correction complete 2026-08-23: CONTRACTS-HTTP/CLI/AGENT_PROTOCOL/DECISIONS now match shipped cert/session cross-check ordering and replay body semantics; clean-overlay proof/docs/build passed; awaiting reviewer/documentation re-gates and integrator commit.

## Description

Field evidence (2026-08-07), from a real external agent in another repo that migrated across
tonight's plaintext->TLS switch live:

```
$ agent-busctl pin add 68e8...7b14
agent-busctl: pin: identity ...mic-array-1 enrolled against http://127.0.0.1:18080, which is a
  plaintext URL and presents no certificate
  try: ... enrol against its https URL with `agent-busctl enrol --bus <https-url> ...`
```

Every identity enrolled BEFORE the TLS switch is now in a dead end. The agent id still works, but
only if `--bus https://...` AND `--bus-fingerprint` are passed on EVERY invocation, forever -- there
is no way to persist either onto an http-enrolled identity (identities.json has no bus_fingerprints
recorded for it, because none existed at enrolment time). `pin add` refuses outright, and its own
printed remedy is to re-enrol -- which mints a NEW agent id (invariant 1: ids are never reused) and
abandons the old one, with no continuity path.

That is the exact outcome DECISIONS.md's E3 entry (line ~1224, "Rotation serves TWO certificates
during rollover") says must never be the only route: "Rotation must never require every client to
re-enrol -- that would make routine key hygiene indistinguishable from a security incident." The
2026-08-07 re-bind decision (DECISIONS.md ~line 2503) restates the same principle for the CLIENT's
own cert and explicitly enumerates two situations -- (1) cert approaching NotAfter with a still-valid
session token -> re-bind route (not yet built, see MTLS re-bind route task, public_id 7a197025), and
(2) cert already lapsed with no prior renewal -> re-enrol is accepted as the correct, deliberate
answer, new id and all.

THIS FINDING IS A THIRD, DISTINCT SITUATION NOT COVERED BY EITHER: an identity that was enrolled
entirely over plaintext HTTP, before mTLS/TLS existed at all, and therefore never had ANY client
certificate or bus-fingerprint pin recorded -- there is nothing here to "renew" (situation 1) and the
identity's AUTH keypair has NOT lapsed (situation 2 does not apply either; the agent id and its
Ed25519 AUTH key are still valid and working). `pin add`'s job (per CONTRACTS-ONDISK/CONTRACTS-CLI and
client/pinset.go) is narrower still: it only lets an identity that ALREADY has a recorded bus
fingerprint recover from a dropped accept-set (MTLS-ROTATE's MaxBusPins=2 evict path) -- it has no
notion of bootstrapping a FIRST bus-fingerprint pin onto an identity from a pre-TLS era, nor of
provisioning that identity's first client certificate for mTLS (MTLS-CLIENTCERT/MTLS-BIND, which as
scoped only cover the FIRST binding for a brand-new enrolment, not a retrofit onto an existing id).

Cross-reference and disambiguation (do this explicitly in the implementation task):
- MTLS re-bind route (7a197025, todo, NEEDS USER RATIFICATION) = renews an EXISTING client cert for an
  identity that already has one, authenticated by its still-valid session token. Does NOT help here --
  there is no existing client cert to renew, and the whole problem is bootstrapping the first one plus
  the first bus-fingerprint pin.
- MTLS-BIND (b6378bda, todo) = binds a client-cert fingerprint to a server-minted agent id at FIRST
  enrolment (new agent, new invite). Does NOT help here either -- it is scoped to brand-new agents, not
  retrofitting a pre-TLS agent id that must be preserved.
- This finding needs its OWN task: a migration/bootstrap path that lets a pre-TLS (http-enrolled)
  identity, still holding a working AUTH key and a live session, acquire (a) a first client
  certificate and (b) a first pinned bus fingerprint, and bind both to its EXISTING, unchanged agent
  id -- without spending a fresh invite and without minting a new id. If no such path can be made safe
  (e.g. because binding a first client cert to an existing id needs the same anti-spoofing care as
  MTLS-BIND's invite-authorised first binding), the alternative is to say so explicitly and fix `pin
  add`'s error text so it stops recommending an action (re-enrol) that the project's own decisions say
  must not be the only route -- right now the tool's own advice contradicts DECISIONS.md.

Rated P0: it is a migration dead-end affecting 100% of pre-TLS identities the moment TLS is required,
it contradicts a recorded decision (E3) at the exact moment that decision is supposed to be protecting
users, and the CLI's own printed remedy makes the outcome worse, not better.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


_None recorded._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [7a197025-93f9-470b-a69b-bad494eeae94](../MTLS-re-bind-route-an-agent-renews-its-client-certificat--7a197025/task.md) — MTLS re-bind route: an agent renews its client certificate against its EXISTING agent id,… (todo)
- [MTLS-BIND](../MTLS-BIND--b6378bda/task.md) — MTLS-BIND: enrolment binds the presenting client-cert fingerprint to the SERVER-MINTED ag… (done)
- [MTLS-CLIENTCERT](../MTLS-CLIENTCERT--0bc7a2eb/task.md) — MTLS-CLIENTCERT: the client generates and stores its own TLS keypair + self-signed certif… (done)
- [MTLS-ROTATE](../MTLS-ROTATE--c2e8df5b/task.md) — MTLS-ROTATE: a client accepts a SET of pinned bus certificates so a rotation does not for… (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [HANDOVER-CHECK](../../HANDOVER/HANDOVER-CHECK--0f909b6c/task.md) — HANDOVER-CHECK: one command that tells you the health of this repo, plus its recorded out… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
