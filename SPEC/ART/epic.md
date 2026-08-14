# EPIC ART — ART: Secure artifact transfer across local and federated agent buses

[← all epics](../../SPEC.md)

**18 open / 18 total.** Full records live in `SPEC/ART/<task>/task.md`.

_Relations (real)_ are authoritative edges from the Spec Server. _Referenced (derived)_ are guesses parsed out of description free text — useful, but not a dependency list.

## Open tasks (18)

| Key | Title | Status | Prio | Task | Relations (real) | Referenced (derived) |
| --- | --- | --- | --- | --- | --- | --- |
| ART-1 | ART-1: Artifact-transfer semantics, threat model and lifecycle | todo | P0 | [task.md](ART-1--55490a33/task.md) | blocks [ART-10](ART-10--39a7d2e3/task.md)<br>blocks [ART-11](ART-11--756fdfa2/task.md)<br>blocks [ART-12](ART-12--89ac19f3/task.md)<br>blocks [ART-13](ART-13--7ffa42c5/task.md)<br>blocks [ART-14](ART-14--46b4219e/task.md)<br>blocks [ART-15](ART-15--b88c1c59/task.md)<br>+11 more (see task.md) | — |
| ART-11 | ART-11: Artifact end-to-end ACK/NACK correlation | todo | P0 | [task.md](ART-11--756fdfa2/task.md) | blocked by [ACK-1](../ACK/ACK-1--e0ac42e1/task.md)<br>blocked by [ART-1](ART-1--55490a33/task.md)<br>blocked by [ART-12](ART-12--89ac19f3/task.md)<br>blocked by [ART-3](ART-3--64a11268/task.md)<br>blocked by [ART-5](ART-5--7c470cb1/task.md)<br>blocked by [ART-9](ART-9--f6864354/task.md)<br>+2 more (see task.md) | — |
| ART-12 | ART-12: Artifact encryption, key distribution and privacy decision | todo | P0 | [task.md](ART-12--89ac19f3/task.md) | blocked by [ART-1](ART-1--55490a33/task.md)<br>blocks [ART-11](ART-11--756fdfa2/task.md)<br>blocks [ART-7](ART-7--5286d0c9/task.md) | — |
| ART-13 | ART-13: Artifact filesystem and content-safety boundary | todo | P0 | [task.md](ART-13--7ffa42c5/task.md) | blocked by [ART-1](ART-1--55490a33/task.md)<br>blocks [ART-18](ART-18--ef028209/task.md)<br>blocks [ART-7](ART-7--5286d0c9/task.md)<br>blocks [ART-8](ART-8--9cc8fc4e/task.md) | — |
| ART-18 | ART-18: Single-bus and three-bus artifact failure/restart/partition acceptance | todo | P0 | [task.md](ART-18--ef028209/task.md) | blocked by [ACK-12](../ACK/ACK-12--17406b3a/task.md)<br>blocked by [ART-1](ART-1--55490a33/task.md)<br>blocked by [ART-10](ART-10--39a7d2e3/task.md)<br>blocked by [ART-11](ART-11--756fdfa2/task.md)<br>blocked by [ART-13](ART-13--7ffa42c5/task.md)<br>blocked by [ART-14](ART-14--46b4219e/task.md)<br>+7 more (see task.md) | [DEPLOY-3](../DEPLOY/DEPLOY-3--9eaf2d19/task.md) |
| ART-2 | ART-2: Versioned authenticated artifact manifest and canonical metadata | todo | P0 | [task.md](ART-2--d8b2b551/task.md) | blocked by [ART-1](ART-1--55490a33/task.md)<br>blocks [ART-3](ART-3--64a11268/task.md)<br>blocks [ART-4](ART-4--3e01fc93/task.md)<br>blocks [ART-5](ART-5--7c470cb1/task.md)<br>blocks [ART-6](ART-6--62a0a213/task.md) | — |
| ART-3 | ART-3: Artifact authorization, recipient consent and anti-enumeration | todo | P0 | [task.md](ART-3--64a11268/task.md) | blocked by [ART-1](ART-1--55490a33/task.md)<br>blocked by [ART-2](ART-2--d8b2b551/task.md)<br>blocks [ART-11](ART-11--756fdfa2/task.md)<br>blocks [ART-7](ART-7--5286d0c9/task.md) | — |
| ART-4 | ART-4: Artifact quotas, fairness and backpressure accounting | todo | P0 | [task.md](ART-4--3e01fc93/task.md) | blocked by [ART-1](ART-1--55490a33/task.md)<br>blocked by [ART-2](ART-2--d8b2b551/task.md)<br>blocks [ART-10](ART-10--39a7d2e3/task.md)<br>blocks [ART-6](ART-6--62a0a213/task.md)<br>blocks [ART-7](ART-7--5286d0c9/task.md) | — |
| ART-6 | ART-6: Durable sender and receiver transfer state | todo | P0 | [task.md](ART-6--62a0a213/task.md) | blocked by [ART-1](ART-1--55490a33/task.md)<br>blocked by [ART-2](ART-2--d8b2b551/task.md)<br>blocked by [ART-4](ART-4--3e01fc93/task.md)<br>blocks [ART-10](ART-10--39a7d2e3/task.md)<br>blocks [ART-7](ART-7--5286d0c9/task.md)<br>blocks [ART-8](ART-8--9cc8fc4e/task.md)<br>+1 more (see task.md) | — |
| ART-8 | ART-8: Ordering-independent verified assembly and atomic completion | todo | P0 | [task.md](ART-8--9cc8fc4e/task.md) | blocked by [ART-1](ART-1--55490a33/task.md)<br>blocked by [ART-13](ART-13--7ffa42c5/task.md)<br>blocked by [ART-6](ART-6--62a0a213/task.md)<br>blocked by [ART-7](ART-7--5286d0c9/task.md)<br>blocks [ART-18](ART-18--ef028209/task.md)<br>blocks [ART-9](ART-9--f6864354/task.md) | — |
| ART-9 | ART-9: Artifact at-least-once deduplication and replay/substitution defense | todo | P0 | [task.md](ART-9--f6864354/task.md) | blocked by [ART-1](ART-1--55490a33/task.md)<br>blocked by [ART-6](ART-6--62a0a213/task.md)<br>blocked by [ART-7](ART-7--5286d0c9/task.md)<br>blocked by [ART-8](ART-8--9cc8fc4e/task.md)<br>blocks [ART-10](ART-10--39a7d2e3/task.md)<br>blocks [ART-11](ART-11--756fdfa2/task.md)<br>+1 more (see task.md) | — |
| ART-10 | ART-10: Resume, cancel, expiry, retention and cleanup | todo | P1 | [task.md](ART-10--39a7d2e3/task.md) | blocked by [ART-1](ART-1--55490a33/task.md)<br>blocked by [ART-4](ART-4--3e01fc93/task.md)<br>blocked by [ART-6](ART-6--62a0a213/task.md)<br>blocked by [ART-9](ART-9--f6864354/task.md)<br>blocks [ART-15](ART-15--b88c1c59/task.md)<br>blocks [ART-18](ART-18--ef028209/task.md) | — |
| ART-14 | ART-14: Git patch and bundle adapters with verification-only apply flow | todo | P1 | [task.md](ART-14--46b4219e/task.md) | blocked by [ART-1](ART-1--55490a33/task.md)<br>blocks [ART-15](ART-15--b88c1c59/task.md)<br>blocks [ART-18](ART-18--ef028209/task.md) | — |
| ART-15 | ART-15: Artifact CLI/API/watch observability | todo | P1 | [task.md](ART-15--b88c1c59/task.md) | blocked by [ART-1](ART-1--55490a33/task.md)<br>blocked by [ART-10](ART-10--39a7d2e3/task.md)<br>blocked by [ART-11](ART-11--756fdfa2/task.md)<br>blocked by [ART-14](ART-14--46b4219e/task.md)<br>blocks [ART-18](ART-18--ef028209/task.md) | — |
| ART-5 | ART-5: Artifact wire protocol and compatibility negotiation | todo | P1 | [task.md](ART-5--7c470cb1/task.md) | blocked by [ART-1](ART-1--55490a33/task.md)<br>blocked by [ART-2](ART-2--d8b2b551/task.md)<br>blocks [ART-11](ART-11--756fdfa2/task.md)<br>blocks [ART-7](ART-7--5286d0c9/task.md) | — |
| ART-7 | ART-7: Chunk upload, relay and download transport | todo | P1 | [task.md](ART-7--5286d0c9/task.md) | blocked by [ART-1](ART-1--55490a33/task.md)<br>blocked by [ART-12](ART-12--89ac19f3/task.md)<br>blocked by [ART-13](ART-13--7ffa42c5/task.md)<br>blocked by [ART-3](ART-3--64a11268/task.md)<br>blocked by [ART-4](ART-4--3e01fc93/task.md)<br>blocked by [ART-5](ART-5--7c470cb1/task.md)<br>+4 more (see task.md) | — |
| ART-16 | ART-16: Artifact audit and metrics | todo | P2 | [task.md](ART-16--a311e367/task.md) | blocked by [ART-1](ART-1--55490a33/task.md)<br>blocks [ART-18](ART-18--ef028209/task.md) | — |
| ART-17 | ART-17: Artifact documentation and operational contract | todo | P2 | [task.md](ART-17--0495302c/task.md) | blocked by [ART-1](ART-1--55490a33/task.md)<br>blocks [ART-18](ART-18--ef028209/task.md) | — |

## Closed tasks (0) — done, cancelled, superseded

_None._

## Epic description

Planning epic for secure small inline and resumable chunked artifact transfer, including federated buses. Git format-patch and bundles are adapters only, never a Git replacement or automatic apply. Creating this epic implements nothing.

Reuse ACK terminal outcomes, RELAY-25 federation/retention/body constraints and DEPLOY-3 topology; no task may silently change their limits.

Open decisions / risks (ART-1 resolves before implementation): maximum inline/artifact/chunk sizes; durable storage quota and retention; push versus pull; bus-local storage versus external blob-store trust boundary; encryption and key distribution; broadcast prohibited versus explicit aggregation; malware/content-scanning external boundary; partial-transfer charging/backpressure; Unicode filename normalization and metadata disclosure; atomic completion/publish; sender versus recipient deletion/cancel authority; cross-bus ownership and partition resume semantics.

---

_Generated by `scripts/gen-spec-mirror.sh`; never hand-edit._
