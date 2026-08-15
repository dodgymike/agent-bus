# CONTEXT-STALE-INPLACE: DECISIONS.md section 2 and the Dockerfile CMD block state a superseded container-isolation claim IN PLACE, corrected only far below

| Field | Value |
| --- | --- |
| Public id | `ec7fc25e-0b2f-4f30-8b68-bb01e13f2344` |
| Key | CONTEXT-STALE-INPLACE |
| Epic | [CONTEXT](../epic.md) |
| Status | todo |
| Priority | P2 |
| Component | docs |
| Section | backlog |
| Tags | — |
| Created | 2026-08-15T14:46:46.843897+00:00 |
| Updated | 2026-08-15T14:46:46.843897+00:00 |
| Completed | — |

## Proof command

```sh
bash -c 'fail=0; P="reachable by strictly nobody outside it"; if grep -qF "$P" DECISIONS.md; then if grep -A6 -F "$P" DECISIONS.md | grep -qiE "SUPERSEDED|not an isolation boundary|governs off-host|-p governs"; then :; else echo "DECISIONS.md: the namespace-isolation claim is stated IN PLACE with no correction within 6 lines"; fail=1; fi; fi; Q="NOT WEAKENING INVARIANT 11"; if grep -qF "$Q" Dockerfile; then if grep -A12 -F "$Q" Dockerfile | grep -qiE "NOT AN ISOLATION BOUNDARY|not an isolation boundary|governs off-host"; then :; else echo "Dockerfile: the invariant-11 paragraph asserts the namespace boundary with no correction within 12 lines"; fail=1; fi; fi; if [ "$fail" -ne 0 ]; then echo STALE_IN_PLACE; exit 1; fi; echo INPLACE_OK'
```

## Description

Filed 2026-08-15 by spec-keeper during the RELAY/DEPLOY reconciliation. VERIFIED FIRST-HAND against committed HEAD 9938eb2 (git show, not the SPEC/ mirror and not a report).

== THE DEFECT: A SUPERSEDED CLAIM LEFT STANDING IN PLACE ==

`DECISIONS.md:6230`, inside the section 2 paragraph headed "**Why this does not narrow invariant 11**", states that a container binding `:8080` in an unpublished network namespace is

    "reachable by strictly nobody outside it -- the same property loopback buys on a host"

**That is FALSE on Linux.** The host owns an interface on the docker bridge and routes that subnet, so any local process can dial the container IP directly with no `-p` at all. Measured during the DEPLOY-6 security gate, twice and independently: a probe from the host to an unpublished bus at `https://172.20.0.2:8080/healthz` COMPLETED ITS TLS HANDSHAKE and failed only on hostname verification ("certificate is valid for 127.0.0.1, ::1, not 172.20.0.2") -- a NAME check, not a reachability failure. An agent verifying by pinned fingerprint (which is exactly what invariant 11 prescribes) would have connected. `-p` governs OFF-HOST reach only.

== WHY THIS IS NOT ALREADY FIXED ==

It IS corrected -- 119 lines further down. `DECISIONS.md:6349` carries a dated addendum, "**\"No `-p`\" is not an isolation boundary on Linux.**", and both `CONTRACTS-CLI.md` and the `Dockerfile` carry the correction inline. So THE COMMIT AS A WHOLE IS TRUTHFUL. **But a reader of section 2 alone gets the false claim**, and section 2 is where an agent looks to find out why the container CMD deviates from the binary default -- i.e. precisely the reader most likely to act on it.

The same shape is in the `Dockerfile` itself: the block at line 214, "WHY FIXING IT HERE IS NOT WEAKENING INVARIANT 11", asserts the namespace boundary, and only a bullet list further down (around line 235) says "NOT AN ISOLATION BOUNDARY ON LINUX" and "-p governs reach". Self-correcting, but THE STRONG CLAIM READS FIRST.

== THE GENUINE TENSION, WHICH IS THE POINT OF THIS TASK ==

`DECISIONS.md` is append-only BY CONVENTION, and that convention is the reason the author appended an addendum instead of editing section 2 -- which was the CORRECT call under the rule as written. This is therefore not a slip to scold, it is a real conflict between two things this repo values:

  * append-only history (never rewrite what was decided, and when)
  * no false statement readable in place (a reader must not be able to reach a superseded claim without its correction)

Both are defensible; today they collide and truthfulness loses. RESOLVE THE TENSION, do not just patch these two files. The suggested resolution -- an INLINE SUPERSEDED MARKER -- preserves the original text verbatim (so history is intact) while making it impossible to read the claim without the correction. That is additive, so it does not violate append-only in substance. If a different resolution is chosen, record it in DECISIONS.md.

== SCOPE ==

1. `DECISIONS.md` section 2, at the "reachable by strictly nobody outside it" sentence: add an inline dated SUPERSEDED marker pointing at the addendum. Do NOT delete or reword the original sentence.
2. `Dockerfile`, the "NOT WEAKENING INVARIANT 11" block: add the qualifier at the TOP of the block, so the strong claim cannot be read without it.
3. Record the append-only-vs-truth-in-place decision in DECISIONS.md.

== SAME FAMILY AS `CONTEXT-STALE-NOTYET` (67b42913) ==

Related, deliberately: that task wants a `doc-check` FORBID mode so a "not yet implemented" note cannot outlive its truth. This is the mirror image -- text that is FALSE IN PLACE with the correction elsewhere. A forbid/pair mode that can express "this phrase must not appear WITHOUT that phrase nearby" would cover BOTH, so the two should be designed together. `scripts/doc-check.sh` is currently UNTRACKED at HEAD, so neither can be mechanised until it lands.

== PROOF ==

The stored proof_cmd is a real doc proof, VALIDATED BOTH WAYS by the filer before filing -- observed RED at HEAD 9938eb2 (exit 1, naming both files) and GREEN against a simulated fix that adds an inline correction to each. It does not demand a particular wording: it passes if the claim is gone, OR if a correction appears within 6 lines (DECISIONS.md) / 12 lines (Dockerfile) of it. Run it from the repo root.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **relates to** [CONTEXT-STALE-NOTYET](../CONTEXT-STALE-NOTYET--67b42913/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CONTEXT-STALE-NOTYET](../CONTEXT-STALE-NOTYET--67b42913/task.md) — CONTEXT-STALE-NOTYET: a doc-check \`forbid\` mode, so a "not yet implemented" note cannot o… (todo)
- [DEPLOY-6](../../DEPLOY/DEPLOY-6--e12b75cd/task.md) — DEPLOY-6: host-reachable Dockerfile CMD + THREE-BUS-DOCKER.md federated runbook (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
