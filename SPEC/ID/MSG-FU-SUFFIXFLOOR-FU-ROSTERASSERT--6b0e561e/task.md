# MSG-FU-SUFFIXFLOOR-FU-ROSTERASSERT: assert a freshly-minted agent-suffix is not already in the roster -- the roster backstop against forge-low is accidental, not designed

| Field | Value |
| --- | --- |
| Public id | `6b0e561e-9336-4528-b77d-1c3c43478723` |
| Key | _(null in the export)_ |
| Epic | [ID](../epic.md) |
| Status | todo |
| Priority | P1 |
| Component | auth |
| Section | backlog |
| Tags | security, id-authority, follow-up |
| Created | 2026-08-07T21:51:13.422698+00:00 |
| Updated | 2026-08-08T10:29:39.805290+00:00 |
| Completed | — |

## Description

FINDING (independent security agent, reproduced with a working exploit, confirmed against the tree). agent-suffixes (cmd/agent-bus/suffixfloors.go / internal/ids) is UNKEYED -- no legacy path needed, the live format is already keyless. Rewinding it to an older value with a valid recomputed digest is accepted SILENTLY, no rewind warning: openSuffixAllocator cross-checks the persisted floors against the WAL only when the floors file is ABSENT (suffixfloors.go:143-169; the files own comment names the omission at line 166: a floors file that EXISTS but has been rewound to an older version ... is not detected today).

THE REPORTERS OWN THESIS WAS THEN REFUTED, AND THAT IS THE FINDING. Rewinding and re-enrolling a fully-rostered name (e.g. worker-2) does NOT reuse the id: the bus tries to reissue it, and auth.ErrDuplicateAgentID (internal/auth/errors.go:88, roster.go:252, walroster.go:231) rejects it. The floor then self-heals upward. So forge-low against a rostered name costs only a couple of burned suffixes, not id reuse.

THE PRECISELY-BOUNDED RESIDUAL: reuse is reachable only for a suffix that was BURNED but is NOT in the roster -- exactly the set the floor exists to protect and the roster structurally cannot cover:
(a) a dangling/aborted enrol prepare -- number fsynced, crash before commit. internal/auth/crash_test.go:TestAuthCrashInjectionTornPrepare demonstrates worker-7 issued and reported as worker:1 after a SIGKILL, i.e. a suffix can be durably burned with no committed roster entry. Note this case is NOT fixed by 6f4c17ef (MSG-FU-SUFFIXFLOOR-FU-STREAMSCAN)s planned every-start WAL cross-check as currently scoped: that derivation folds committed store.RecordKind (and, per 477b8eeb, committed auth enrolment) records only. ID-2-WIRING-OBSERVER (c31f6999-da4e-400d-ab55-178b82e2a42e, still todo) is the task that would expose dangling/aborted prepares to any observer during replay at all -- until it lands, no WAL-derived floor, however often it is recomputed, can see this class of burned suffix.
(b) once AUTH-4 (a853261d-2829-4101-906d-31a8a81eb59f, POST /v1/leave) lands, a departed agent -- the roster removes the entry on leave by design, so a rewound floor plus re-enrol binds a fresh keypair to an id a previous holder used, with no roster entry left to object.

For either case: a rewound floor reissues the suffix, the roster has nothing to compare against, and enrol SUCCEEDS -- a new keypair silently inherits an id with prior history.

THE LOAD-BEARING ASK, in the reporters own wording: the roster backstop is protecting you by accident of ordering, not by design intent stated at the floor. A defence nobody knows is load-bearing is one refactor from removal, and the plausible refactor is named: an idempotent re-enrol that overwrites the pubkey, which somebody will reasonably reach for (it is the natural fix for the ErrDuplicateAgentID case above under retry-safety expectations) and would silently convert a contained forge-low into full id reuse.

DO. Add a NAMED assertion at the enrol path, independent of and in addition to any WAL-derived floor cross-check: a freshly minted suffix must not already be present in the roster BEFORE it is treated as fresh -- if it is, the floor was rewound (or narrower than reality): refuse the enrol and log LOUDLY at ERROR naming the name/suffix/expected-vs-found. This converts todays accidental 500 (ErrDuplicateAgentID, which only fires because Put happens to check) into a NAMED, tested detection that survives a future idempotent-re-enrol refactor.

ALSO NOTE (lower priority, same root cause): forge-HIGH on agent-suffixes bricks enrolment for that ONE name only (the floor jumps ahead, so every mint for that name is starved), not the whole bus -- smaller blast radius than the wal-index-floor / message-seq-floor findings, but the same unkeyed root and the same remedy family (bound the accepted value; see be447589-6583-4d5c-a9d4-ec9d9fef0f1c and 259b7033-2191-423f-bb7b-cff8c6b59dc1, the sibling floor-bound tasks).

SCOPE: internal/ids, internal/auth, cmd/agent-bus/suffixfloors.go. Coordinate with the live MSG-FU-SUFFIXFLOOR-* family (6f4c17ef streaming scan, 477b8eeb enrolment-record fold, e5fa08ba docs, d5ed5ccc unseal) -- this is a NEW, narrower assertion that is correct even before any of those land, and remains valuable defense-in-depth after they do.

ACCEPTANCE. A test: mint an agent, durably burn its suffix via a torn/aborted prepare (or a committed-then-left entry once AUTH-4 exists) so the roster has no entry for it, rewind/replace the agent-suffixes floors file to a value at-or-below that suffix with a valid checksum, attempt to enrol the same name -- the enrol MUST be refused with a named, ERROR-logged reason, not silently reissue the suffix. go test -race ./internal/ids ./internal/auth ./cmd/agent-bus green.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [259b7033-2191-423f-bb7b-cff8c6b59dc1](../../DUR/Bound-the-wal-index-floor-reserved-value-the-same-way-as--259b7033/task.md) — Bound the wal-index-floor reserved value the same way as the message-seq floor (todo)
- [AUTH-4](../../AUTH/AUTH-4--a853261d/task.md) — AUTH-4: POST /v1/leave -- leave / revocation (todo)
- [ID-2-WIRING-OBSERVER](../ID-2-WIRING-OBSERVER--c31f6999/task.md) — ID-2-WIRING-OBSERVER: wal offers EVERY prepare (committed, aborted AND dangling) to an ob… (todo)
- [MSG-FU-SUFFIXFLOOR](../MSG-FU-SUFFIXFLOOR--94159d93/task.md) — MSG-FU-SUFFIXFLOOR: resume per-name agent-id suffix counters from disk (agent ids are now… (in_progress)
- [MSG-FU-SUFFIXFLOOR-FU-DOCS](../../DOCS/MSG-FU-SUFFIXFLOOR-FU-DOCS--e5fa08ba/task.md) — MSG-FU-SUFFIXFLOOR-FU-DOCS: PROTOCOL.md and internal/ids docs still say the suffix wiring… (todo)
- [MSG-FU-SUFFIXFLOOR-FU-ENROLRECORDS](../MSG-FU-SUFFIXFLOOR-FU-ENROLRECORDS--477b8eeb/task.md) — MSG-FU-SUFFIXFLOOR-FU-ENROLRECORDS: fold ENROLMENT records into the legacy-dir suffix bac… (todo)
- [MSG-FU-SUFFIXFLOOR-FU-STREAMSCAN](../../DUR/MSG-FU-SUFFIXFLOOR-FU-STREAMSCAN--6f4c17ef/task.md) — MSG-FU-SUFFIXFLOOR-FU-STREAMSCAN: export a streaming raw WAL scan and reinstate the every… (todo)
- [MSG-FU-SUFFIXFLOOR-FU-UNSEAL](../MSG-FU-SUFFIXFLOOR-FU-UNSEAL--d5ed5ccc/task.md) — MSG-FU-SUFFIXFLOOR-FU-UNSEAL: make ids.NewNameSuffixes born-unsealed (or delete it) now t… (todo)
- [be447589-6583-4d5c-a9d4-ec9d9fef0f1c](../../CORE/Enforce-data-directory-permissions-at-startup-and-bound--be447589/task.md) — Enforce data-directory permissions at startup, and bound the message-seq floor (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
