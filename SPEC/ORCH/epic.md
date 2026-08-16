# EPIC ORCH — ORCH: deployment topologies beyond Compose — sidecar, Kubernetes, standalone

[← all epics](../../SPEC.md)

**6 open / 6 total.** Full records live in `SPEC/ORCH/<task>/task.md`.

_Relations (real)_ are authoritative edges from the Spec Server. _Referenced (derived)_ are guesses parsed out of description free text — useful, but not a dependency list.

## Open tasks (6)

| Key | Title | Status | Prio | Task | Relations (real) | Referenced (derived) |
| --- | --- | --- | --- | --- | --- | --- |
| ORCH-1 | ORCH-1: DECIDE the network posture for sidecar/k8s — CONSENT-GATED, and the compose ratio… | todo | P2 | [task.md](ORCH-1--e22449ec/task.md) | _not fetched_ | [ORCH-2](ORCH-2--5ffeb926/task.md) [ORCH-3](ORCH-3--d75a3b68/task.md) [ORCH-4](ORCH-4--282a2e9c/task.md) [ORCH-5](ORCH-5--c4634621/task.md) [MTLS-LISTENER](../MTLS/MTLS-LISTENER--17e70a7e/task.md) [INVITE-GATE-ENFORCE](../INVITE/INVITE-GATE-ENFORCE--8297d7e2/task.md) |
| ORCH-6 | ORCH-6: STANDALONE — verify and document what DEPLOY already ships; do NOT rebuild it | todo | P2 | [task.md](ORCH-6--6cfe7288/task.md) | _not fetched_ | [ORCH-1](ORCH-1--e22449ec/task.md) [DEPLOY-1](../DEPLOY/DEPLOY-1--fa0c5a4e/task.md) [DEPLOY-2](../DEPLOY/DEPLOY-2--14f8ec3b/task.md) [DEPLOY-2-FU-CONTAINERNAME](../DEPLOY/DEPLOY-2-FU-CONTAINERNAME--e9dd20b4/task.md) [DEPLOY-REDEPLOY](../DEPLOY/DEPLOY-REDEPLOY--f801d128/task.md) [CLI-BUSCTL-IMAGE](../CLI/CLI-BUSCTL-IMAGE--9be2105d/task.md) |
| ORCH-2 | ORCH-2: certificate and invite PROVISIONING under an orchestrator — no CA, no TOFU, 0600,… | todo | P3 | [task.md](ORCH-2--5ffeb926/task.md) | _not fetched_ | [ORCH-1](ORCH-1--e22449ec/task.md) [INVMINT-7](../INVMINT/INVMINT-7--174c7ba9/task.md) [INVMINT-6](../INVMINT/INVMINT-6--cedb8d6f/task.md) |
| ORCH-3 | ORCH-3: first-boot ordering — a bus's first-ever boot can enrol NOBODY, and an orchestrat… | todo | P3 | [task.md](ORCH-3--d75a3b68/task.md) | _not fetched_ | [ORCH-1](ORCH-1--e22449ec/task.md) [ORCH-2](ORCH-2--5ffeb926/task.md) [INVMINT-6](../INVMINT/INVMINT-6--cedb8d6f/task.md) |
| ORCH-4 | ORCH-4: restart semantics under an orchestrator — sessions are IN-MEMORY, so every pod re… | todo | P3 | [task.md](ORCH-4--282a2e9c/task.md) | _not fetched_ | [ORCH-1](ORCH-1--e22449ec/task.md) [INVMINT-6](../INVMINT/INVMINT-6--cedb8d6f/task.md) |
| ORCH-5 | ORCH-5: the sidecar and Kubernetes manifests themselves | todo | P3 | [task.md](ORCH-5--c4634621/task.md) | _not fetched_ | [ORCH-1](ORCH-1--e22449ec/task.md) [ORCH-2](ORCH-2--5ffeb926/task.md) [ORCH-3](ORCH-3--d75a3b68/task.md) [ORCH-4](ORCH-4--282a2e9c/task.md) [DEPLOY-2](../DEPLOY/DEPLOY-2--14f8ec3b/task.md) [DEPLOY-REDEPLOY](../DEPLOY/DEPLOY-REDEPLOY--f801d128/task.md) +2 more |

## Closed tasks (0) — done, cancelled, superseded

_None._

## Epic description

A container usable as a SIDECAR in docker-compose / Kubernetes, or STANDALONE. Operator request, verbatim,
2026-08-15: "a docker container that can be used as a sidecar for a docker-compose / k8s, or standalone".
FILED ONLY -- not started. Critical path is relay egress.

## EXTEND-vs-NEW RULING (spec-keeper, 2026-08-15): NEW EPIC, and one third of the ask ALREADY EXISTS

Checked against DEPLOY by reading all 7 of its task records in the `SPEC/` mirror. The ask splits three ways
and the three parts have DIFFERENT answers -- filing all three the same way would have been the mistake:

1. **STANDALONE: LARGELY ALREADY EXISTS. DO NOT REBUILD IT.** DEPLOY-1 (multi-stage Dockerfile, pinned Go
   builder, minimal runtime image) is DONE. DEPLOY-2 (compose, named volume, healthcheck) is DONE.
   DEPLOY-2-FU-CONTAINERNAME is DONE. The image exists and runs. What is missing is VERIFICATION AND
   DOCUMENTATION of the standalone shape, and even that partly overlaps DEPLOY-REDEPLOY (open), whose
   acceptance is already "two distinct agents actually enrol against the freshly recreated bus and exchange at
   least one message... VERIFICATION BY EXECUTION, not container health". ORCH-6 is therefore scoped to
   verify-and-document ONLY, and explicitly forbids re-doing DEPLOY's work.
2. **SIDECAR and 3. KUBERNETES: GENUINELY NEW, and not DEPLOY's charter.** Prior art search over all 621 task
   files: `grep -rliE 'kubernetes|k8s|sidecar|helm|statefulset' SPEC/` returns exactly ONE file, and it is
   SUPERSEDED (an old MTLS task listing "a reverse proxy / sidecar in front of the server" as a REJECTED
   option). Nothing live covers it.

Why NOT simply extend DEPLOY: DEPLOY's charter is stated in its own description as "agent-bus ships as a
container and runs under Docker Compose (USER INSTRUCTION, 2026-08-02)", and CLAUDE.md's "Runtime target:
Docker Compose" section says the same. Kubernetes is a NEW DEPLOYMENT TARGET, and a sidecar is a NEW NETWORK
POSTURE. Quietly widening a user-instructed charter by dropping tasks into DEPLOY would hide a scope change
that deserves to be visible -- and the network-posture half is CONSENT-GATED (see below). DEPLOY keeps doing
what it says: build the container, run it under Compose. ORCH owns TOPOLOGY.

## THE LOAD-BEARING PROBLEM: NETWORK POSTURE (ORCH-1 BLOCKS ALMOST EVERYTHING)

Invariant 11: TLS is required, there is no plaintext listener, certificates are self-signed, TLS is MUTUAL,
there is NO CA and NO TRUST-ON-FIRST-USE -- the invite blob carries the bus's certificate fingerprint. The
loopback default `-listen 127.0.0.1:8080` STAYS. Never disable certificate verification, and never ship a flag
that does, not even a documented one.

A sidecar changes the network posture by definition, and `docker-compose.yml` already contains a long, reasoned
SECURITY CONSTRAINT block saying so. Its key finding, which ORCH-1 must start from rather than rediscover:
binding the HOST side of `ports:` to loopback does NOT contain it -- "`-listen=0.0.0.0` makes the bus answer on
its container's address on the shared compose bridge network, so EVERY OTHER SERVICE on this same
docker-compose network (now or added later) can reach it ... because bridge-network traffic never goes through
the published-port mapping at all. `ports:` only ever gates HOST access." A sidecar's whole point is that a
neighbouring container reaches it, so this is the decision, not a detail.

**TWO PARTS OF THAT BLOCK'S STATED RATIONALE ARE NOW STALE, AND ORCH-1 MUST DECIDE ON CURRENT FACTS:**
(i) it ends by saying other services "can reach it over cleartext" -- FALSE since MTLS-LISTENER; the bus serves
TLS and ONLY TLS and refuses to start without usable key material, as the same block says higher up. The block
contradicts itself.
(ii) it gives as "the remaining reason not to widen this bind" that "there is still no client-certificate
REQUIREMENT ... So anything that can reach the port can still attempt enrolment." Attempting is still true,
but SUCCEEDING is not: INVITE-GATE-ENFORCE (`3cedcb7`) set `enrolmentInviteRequired = true`
(`cmd/agent-bus/main.go:66`), so enrolment without a valid invite is now REFUSED.
NONE OF THIS IS AN ARGUMENT TO WIDEN THE BIND. The loopback default stays per invariant 11 and per CLAUDE.md,
and exposing the bus on a non-loopback interface is a CONSENT-GATED action in this project (recorded in the
superseded MTLS task: "Exposing the bus on a non-loopback interface, and any change to authn/authz defaults,
are CONSENT-GATED actions per this project's operating rules"). It means the decision must be taken against
what is TRUE TODAY, and it must be an EXPLICIT DECISION, never an incidental flag change in a manifest.

## THE BOOTSTRAP COLLISION -- THE PART MOST LIKELY TO BE MISSED

- **Enrolment is invite-only** (`3cedcb7`), and **`agent-bus invite mint` needs the bus STOPPED**, because it
  takes the data directory's EXCLUSIVE dirlock which a running bus holds. A container that starts a bus and
  expects agents to enrol must reckon with this. The VERIFIED working pattern: prime once, stop, pre-mint a
  POOL, start, then agents enrol against the running bus indefinitely (measured: two agents enrolled, zero
  restarts between them, a spent invite correctly refused). Invite files must be `0600` or the CLI refuses
  them. **The no-stop fix is owned by the `INVMINT` epic -- wire a `relates` edge and DO NOT DUPLICATE IT.**
- **A bus's FIRST-EVER BOOT can enrol nobody.** An invite pins a certificate fingerprint that only a completed
  start produces. Under an orchestrator that is a hard ORDERING constraint, not a race to paper over with a
  retry loop: ORCH-3.
- **Sessions are IN-MEMORY ONLY.** Every restart invalidates every bearer token and each agent must re-run the
  handshake. It does NOT re-enrol -- the roster is durable. Under k8s, pod restarts are routine, so a
  client-side consequence that is rare under Compose becomes a normal operating event: ORCH-4.
- **`agent-busctl` is NOT in the built image** (CLI-BUSCTL-IMAGE, open). A sidecar whose neighbour is supposed
  to talk to the bus needs the client SOMEWHERE. Depend on that task; do not re-solve it here.

## IN THIS EPIC vs STAYS IN DEPLOY

IN: deployment TOPOLOGY (sidecar / k8s / standalone), the network-posture decision, certificate and invite
PROVISIONING under an orchestrator, first-boot ordering, restart semantics, k8s manifests.
STAYS IN DEPLOY: the Dockerfile, the single-bus compose file, the multi-bus peered compose profile (DEPLOY-3),
the Go toolchain pin (DEPLOY-4), the container build/test check (DEPLOY-5), DEPLOY-REDEPLOY.
STAYS IN INVMINT: minting invites without stopping the bus.
OUT: any "skip verification" flag (FORBIDDEN by invariant 11, even documented); re-litigating Docker Compose as
the runtime target or the `go.mod` container pin -- CLAUDE.md settles both.

Reservations: epic key `ORCH` = `epic-key` #7; task keys ORCH-1..ORCH-6 = `task-key-ORCH` #1..#6 (fresh
namespace, deliberately unseeded). Filed by spec-keeper 2026-08-15, FILE ONLY.

---

_Generated by `scripts/gen-spec-mirror.sh`; never hand-edit._
