# DEPLOY-6: host-reachable Dockerfile CMD + THREE-BUS-DOCKER.md federated runbook

| Field | Value |
| --- | --- |
| Public id | `e12b75cd-c17e-4f73-b4b5-0b04dd868455` |
| Key | DEPLOY-6 |
| Epic | [DEPLOY](../epic.md) |
| Status | done |
| Priority | P1 |
| Component | deploy |
| Section | backlog |
| Tags | docker, runbook, three-bus |
| Created | 2026-08-15T13:48:15.036916+00:00 |
| Updated | 2026-08-15T14:43:29.594048+00:00 |
| Completed | 2026-08-15T14:43:29.594032+00:00 |

## Proof command

```sh
docker build -t agent-bus:proof . && docker rm -f abproof >/dev/null 2>&1; docker volume rm abproof-data >/dev/null 2>&1; docker volume create abproof-data >/dev/null && docker run -d --name abproof -v abproof-data:/data -p 127.0.0.1:18099:8080 agent-bus:proof >/dev/null && sleep 8 && docker run --rm --network host -v abproof-data:/data agent-bus:proof healthcheck -data-dir=/data -addr=127.0.0.1:18099 -timeout=3s && grep -q -- '-route-for' docs/THREE-BUS-DOCKER.md; rc=$?; docker rm -f abproof >/dev/null 2>&1; docker volume rm abproof-data >/dev/null 2>&1; exit $rc
```

## Status note

code complete, not committed; reviewer + security gates both returned CHANGES-REQUIRED on round 1, all findings applied, round-2 re-verification in flight; awaiting integrator commit

## Description

Two-part task, both parts required.

(a) FIX THE DOCKERFILE: today's CMD is `-listen=127.0.0.1:8080` -- loopback INSIDE the container, so a bare `docker run -t <image>` with a published port (`-p 8080:8080`) connects to nothing on the host. Change the container's default listen address so a bare `docker run -t <image>` (with a published port) is actually reachable from the host -- e.g. default CMD to `-listen=0.0.0.0:8080` (or an equivalent host-reachable bind), while preserving the ability to override via the existing CLI flags/env the binary already accepts. Do NOT invent new container-specific config (per DEPLOY-1's original constraint). This is squarely DEPLOY's territory (Dockerfile/compose), explicitly NOT ORCH-6's -- ORCH-6 is doc-only and explicitly forbids touching the Dockerfile, and it documents the CURRENT loopback-only shape as an accepted constraint ("the price of the constraint, not an oversight"). If ORCH-6 lands first documenting loopback-only, this task's Dockerfile change supersedes that framing and ORCH-6's README section must be revisited (coordinate via a note on ORCH-6 when this lands).

(b) NEW RUNBOOK `docs/THREE-BUS-DOCKER.md`: clear, complete instructions for standing up a three-bus federated setup (adjacency A<->B<->C, i.e. B peers with both A and C, A and C do not peer directly) using the (fixed) image. Must cover the awkward bootstrap sequence per bus: start once (mints identity + cert) -> stop -> mint invite pool + `bus-peer add` (or current peering mechanism per RELAY epic) -> start again -> enrol/peer against the now-running bus. Include the exact commands (`docker run`/`docker compose`, `agent-busctl` invocations) an operator or agent would actually type, and call out any TLS/invite/fingerprint gotchas (invariant 11: no CA, no TOFU, invite blob carries cert fingerprint). If the no-stop mint-invite-while-running fix (INVMINT, referenced by ORCH-6) has landed by the time this is worked, prefer documenting that path and note the old stop/start dance as legacy.

Relates to but does not duplicate: ORCH-6 (doc-only, single standalone bus, explicitly forbids Dockerfile edits -- read in full before starting, do not let this task's Dockerfile change bleed into ORCH-6's README section or vice versa) and DEPLOY-3 (Compose-profile multi-bus for RELAY testing; its proof_cmd references the RETIRED scripts/bus-peer.sh per invariant 7 and should be flagged/fixed separately -- do not copy that pattern here). This task's deliverable is a docker-run-first runbook doc, not a Compose profile.

MANDATORY: read INVARIANTS.md invariant 11 (TLS/mTLS/invite fingerprint) and invariant 7 (CLI-only, no bus-*.sh wrappers) before starting -- the runbook must drive everything through agent-busctl, never a hand-written curl or a retired bus-*.sh wrapper.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [DEPLOY-1](../DEPLOY-1--fa0c5a4e/task.md) — DEPLOY-1: Dockerfile -- multi-stage build, pinned Go builder, minimal runtime image (done)
- [DEPLOY-3](../DEPLOY-3--9eaf2d19/task.md) — DEPLOY-3: multi-bus Compose profile (2+ peered buses) for RELAY end-to-end testing (todo)
- [ORCH-6](../../ORCH/ORCH-6--6cfe7288/task.md) — ORCH-6: STANDALONE — verify and document what DEPLOY already ships; do NOT rebuild it (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CONTEXT-STALE-INPLACE](../../CONTEXT/CONTEXT-STALE-INPLACE--ec7fc25e/task.md) — CONTEXT-STALE-INPLACE: DECISIONS.md section 2 and the Dockerfile CMD block state a supers… (todo)
- [DEPLOY-7](../DEPLOY-7--5f965453/task.md) — DEPLOY-7: gate docker build . in the normal check loop so a broken image build surfaces a… (todo)
- [DOCS-4](../../DOCS/DOCS-4--a24c33cd/task.md) — DOCS-4: CLAUDE.md is stale on invariant 3 -- enrolment IS invite-gated at HEAD (done)
- [RELAY-25-FU-CORRELATION-FU-GUARDFIELD](../../RELAY/RELAY-25-FU-CORRELATION-FU-GUARDFIELD--44601098/task.md) — RELAY-25-FU-CORRELATION-FU-GUARDFIELD: the 'no message_id on non-record lines' guard name… (todo)
- [TOOLING-2](../../TOOLING/TOOLING-2--87d9e8d1/task.md) — TOOLING-2: make docs/THREE-BUS-DOCKER.md's bash blocks a repeatable executable check (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
