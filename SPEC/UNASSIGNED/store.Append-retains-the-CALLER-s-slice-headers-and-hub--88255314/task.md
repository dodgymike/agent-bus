# store.Append retains the CALLER's slice headers, and hub.publish keeps using the same Message after appending it -- the write side aliases the serving copy

| Field | Value |
| --- | --- |
| Public id | `88255314-6658-4bba-b1cd-76ebeec9806a` |
| Key | _(null in the export)_ |
| Epic | [UNASSIGNED](../epic.md) |
| Status | todo |
| Priority | — |
| Component | store |
| Section | backlog |
| Tags | — |
| Created | 2026-08-16T11:22:01.153511+00:00 |
| Updated | 2026-08-16T11:22:01.153511+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -count=1 -run TestPublishDoesNotHandOutSlicesAliasingTheServingCopy ./internal/hub
```

## Description

Found 2026-08-16 by the SECURITY gate on RELAY-24-FU-STOREMSGLOOKUP-SIGCOPY, reported OUT OF THAT TASK'S BOUNDARY (that task fixed the READ side only, and its proof covers only the read side).

== WHAT SIGCOPY DID AND DID NOT CLOSE ==

SIGCOPY made store.copyMessage clone all four slices (Body, Recipients, BusPath, Signature), so nothing leaving the store through Since, ByID or ByOriginMessageID aliases the serving copy any more. That is the READ side and it is done.

The WRITE side is untouched and still aliases:

1. `Store.Append(m Message)` (internal/store/store.go:374, retained at :403) keeps the caller's slice HEADERS verbatim. The Message it retains points at the caller's arrays.
2. `hub.publish` then goes on using the SAME `m` after appending it:
   - internal/hub/hub.go:1854  `h.store.Append(m)`
   - internal/hub/hub.go:1871  `Result{... Recipients: m.Recipients ...}`
   - internal/hub/hub.go:1903  `h.forwardOnward(m)`

So the hub's post-append `m`, the Result handed to the HTTP layer, and everything downstream of forwardOnward all hold slices that alias the in-memory serving copy WITHOUT having passed through copyMessage. A mutation through any of them edits the record every later reader sees.

== SEVERITY: MEDIUM, AND LATENT ==

The security gate verified that NOTHING mutates them today. This is a footgun awaiting its next caller, not a live hole. Do not write it up as an exploited defect. It bears on invariant 5 (memory is the serving copy, disk is the truth) for the same reason SIGCOPY did: a single write through an aliased header desyncs the served copy from what was durably written, and no check in this server would notice.

== NOTE FOR WHOEVER TAKES THIS ==

The fix is NOT simply 'copy on the way into Append'. Decide deliberately between:
  (a) Append deep-copies on the way in -- costs an allocation per message on the hot write path, under the hub's write lock, and makes the store's ownership rule unconditional; or
  (b) the CALLER is documented as surrendering ownership of the slices it passes to Append, and hub.publish stops reusing `m` afterwards (it would need its own copy for Result and forwardOnward).

Option (b) is closer to the current shape and cheaper, but it moves the obligation to every future caller of Append, which is exactly the failure mode that produced SIGCOPY. Record the decision in DECISIONS.md.

== A DEPENDENT THAT MUST NOT BE 'TIDIED' FIRST ==

cmd/agent-bus/relayegress.go:435-439 copies Signature/Body/Recipients defensively onto the cross-bus envelope. Its stated REASON is now stale (it cites 'store.copyMessage does NOT deep-copy Signature', which is false since SIGCOPY) but its CONCLUSION is correct and the copy is LOAD-BEARING: on the live forward path envelope() receives the hub's post-Append `m` from forwardOnward, which never passed through copyMessage. Only the RESUME path (main.go RecoverMessage -> ByOriginMessageID) is covered by SIGCOPY. DELETING THAT COPY AS 'NOW REDUNDANT' WOULD BE A REGRESSION. The comment's wording should be corrected by the agent that owns cmd/agent-bus, either here or in its own task.

== PROOF ==

A test in internal/hub that appends a message through the real publish path, mutates the slices reachable from the returned Result (and/or from the value handed to forwardOnward), and asserts the store's serving copy is UNCHANGED via Store.Since / Store.ByID. It must be observed RED before the fix -- a test never seen failing is not evidence.

RELATES: RELAY-24-FU-STOREMSGLOOKUP-SIGCOPY (6e13a7d9-6ff0-49bb-a102-6ee1b69e9b51), RELAY-24-FU-STOREMSGLOOKUP.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [RELAY-24-FU-STOREMSGLOOKUP](../../RELAY/RELAY-24-FU-STOREMSGLOOKUP--c6530638/task.md) — RELAY-24-FU-STOREMSGLOOKUP: internal/store needs a lookup-by-message-id and an OriginMess… (done)
- [RELAY-24-FU-STOREMSGLOOKUP-SIGCOPY](../../RELAY/RELAY-24-FU-STOREMSGLOOKUP-SIGCOPY--6e13a7d9/task.md) — RELAY-24-FU-STOREMSGLOOKUP-SIGCOPY: store.copyMessage does not deep-copy Signature -- ali… (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [19595b60-16c8-4ce2-a67a-dcb8a1804ce1](../store.Message.WithOriginMessageID-s-doc-undercounts-what--19595b60/task.md) — store.Message.WithOriginMessageID's doc undercounts what the returned copy shares: it say… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
