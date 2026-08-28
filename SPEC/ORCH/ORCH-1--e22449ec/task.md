# ORCH-1: DECIDE the network posture for sidecar/k8s — CONSENT-GATED, and the compose rationale is partly stale

| Field | Value |
| --- | --- |
| Public id | `e22449ec-a008-435c-9f6d-e72e1f2c804a` |
| Key | ORCH-1 |
| Epic | [ORCH](../epic.md) |
| Status | todo |
| Priority | P2 |
| Component | PROCESS |
| Section | backlog |
| Tags | — |
| Created | 2026-08-15T08:00:50.092329+00:00 |
| Updated | 2026-08-15T08:00:50.092329+00:00 |
| Completed | — |

## Proof command

```sh
grep -q '^## 2026-.*ORCH: network posture' DECISIONS.md && grep -q 'invariant 11' DECISIONS.md && grep -qE 'loopback default stays|loopback default is retained' DECISIONS.md && echo ORCH1_DECISION_RECORDED
```

## Description

Decide and record how a sidecar / k8s deployment reaches the bus, BEFORE any manifest is written. BLOCKS
ORCH-2, ORCH-3, ORCH-4, ORCH-5.

## This is consent-gated, not a config tweak

Invariant 11: TLS required, no plaintext listener, self-signed certificates, MUTUAL TLS, no CA and NO TOFU
(the invite blob carries the fingerprint), and **the loopback default `-listen 127.0.0.1:8080` STAYS**. Never
disable certificate verification and never ship a flag that does, not even documented.

An existing (superseded) MTLS task records the standing rule: "Exposing the bus on a non-loopback interface,
and any change to authn/authz defaults, are CONSENT-GATED actions per this project's operating rules."
A sidecar's entire premise is that a neighbouring container reaches the bus. So this is THE decision of the
epic. It must never arrive as an incidental `-listen` change inside a manifest task.

## Start from what docker-compose.yml already worked out — do not rediscover it

Its SECURITY CONSTRAINT block establishes the non-obvious part: binding the HOST side of `ports:` to loopback
does NOT contain exposure. "`-listen=0.0.0.0` makes the bus answer on its container's address on the shared
compose bridge network, so EVERY OTHER SERVICE on this same docker-compose network (now or added later) can
reach it ... because bridge-network traffic never goes through the published-port mapping at all. `ports:`
only ever gates HOST access." The k8s analogue is sharper still: containers in ONE POD share a network
namespace, so a true pod-sidecar can reach `127.0.0.1:8080` WITH NO BIND CHANGE AT ALL. **That may make the
in-pod sidecar the one topology that needs no posture change -- check it first, because if it holds it is by
far the cheapest answer and it keeps the loopback default intact.** A DaemonSet/Service/cross-pod shape is a
different question and does need the decision.

## TWO PARTS OF THAT BLOCK'S RATIONALE ARE STALE — decide on current facts

(i) It ends by saying other services "can reach it over cleartext". **FALSE since MTLS-LISTENER**: the bus
serves TLS and ONLY TLS and refuses to start without usable key material -- as the same block says higher up.
The block contradicts itself and should be corrected as part of this task.
(ii) It gives as "the remaining reason not to widen this bind" that "there is still no client-certificate
REQUIREMENT ... So anything that can reach the port can still attempt enrolment." Attempting is still true;
SUCCEEDING is not. `INVITE-GATE-ENFORCE` (`3cedcb7`) set `enrolmentInviteRequired = true`
(`cmd/agent-bus/main.go:66`), so an enrolment with no valid invite is REFUSED.

**NONE OF THAT IS AN ARGUMENT TO WIDEN THE BIND.** mTLS is still only REQUESTED, never REQUIRED
(`tls.RequestClientCert`, `a97f854`) and a presented certificate authenticates nobody by itself, so a widened
bind still exposes the enrolment and session surface to anything that can route to it. The point is that the
decision must be made against TRUE facts. Also note CLAUDE.md's own invariant preamble still says "enrolment
is NOT yet invite-gated (`InviteRequired: false`)", which is ALSO stale -- do not take it as evidence.

## Deliverable

A dated `## 2026-XX-XX — ORCH: network posture` section in `DECISIONS.md` that names invariant 11, states that
the loopback default stays as the shipped default, states per-topology (in-pod sidecar / cross-pod / compose
neighbour / standalone) how the bus is reached and what authenticates the reacher, records operator consent
for any non-loopback bind, and corrects the two stale sentences above.

## Proof

RED baseline observed by spec-keeper at filing (2026-08-15): no `ORCH: network posture` heading in
`DECISIONS.md`. Note `invariant 11` alone would match incidentally elsewhere in a 5000-line file -- the
heading grep is the load-bearing half, and the third clause pins the loopback commitment. Confirm RED
first.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [INVITE-GATE-ENFORCE](../../INVITE/INVITE-GATE-ENFORCE--8297d7e2/task.md) — INVITE-GATE-ENFORCE: enforce invite-only enrolment (P0: anonymous roster exhaustion) (in_progress)
- [MTLS-LISTENER](../../MTLS/MTLS-LISTENER--17e70a7e/task.md) — MTLS-LISTENER: serve TLS ONLY and REFUSE TO START without a usable cert/key -- there is n… (done)
- [ORCH-2](../ORCH-2--5ffeb926/task.md) — ORCH-2: certificate and invite PROVISIONING under an orchestrator — no CA, no TOFU, 0600,… (todo)
- [ORCH-3](../ORCH-3--d75a3b68/task.md) — ORCH-3: first-boot ordering — a bus's first-ever boot can enrol NOBODY, and an orchestrat… (todo)
- [ORCH-4](../ORCH-4--282a2e9c/task.md) — ORCH-4: restart semantics under an orchestrator — sessions are IN-MEMORY, so every pod re… (todo)
- [ORCH-5](../ORCH-5--c4634621/task.md) — ORCH-5: the sidecar and Kubernetes manifests themselves (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [ORCH-2](../ORCH-2--5ffeb926/task.md) — ORCH-2: certificate and invite PROVISIONING under an orchestrator — no CA, no TOFU, 0600,… (todo)
- [ORCH-3](../ORCH-3--d75a3b68/task.md) — ORCH-3: first-boot ordering — a bus's first-ever boot can enrol NOBODY, and an orchestrat… (todo)
- [ORCH-4](../ORCH-4--282a2e9c/task.md) — ORCH-4: restart semantics under an orchestrator — sessions are IN-MEMORY, so every pod re… (todo)
- [ORCH-5](../ORCH-5--c4634621/task.md) — ORCH-5: the sidecar and Kubernetes manifests themselves (todo)
- [ORCH-6](../ORCH-6--6cfe7288/task.md) — ORCH-6: STANDALONE — verify and document what DEPLOY already ships; do NOT rebuild it (todo)
- [ZZB-firsthal](../../UNASSIGNED/ZZB-firsthal--74cb9c06/task.md) — probe (cancelled)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
