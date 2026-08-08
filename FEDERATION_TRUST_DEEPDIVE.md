# FEDERATION_TRUST_DEEPDIVE — cross-bus trust for `laptop(A) ↔ internet(B) ↔ this machine(C)`

**Task:** F2 · RELAY-7 (FEDERATION wave 1). Gates RELAY-14 (`internal/attest`) and RELAY-17
(`CrossBusTrust` implementation).
**Status:** design investigation. **No code was written or changed by this task.** Read-only apart
from this file.
**Date:** 2026-08-08.

---

## 0. Scope, and what this document did NOT cover

Read in full: `internal/relay/signed.go`, `internal/relay/doc.go`, `internal/signing/canonical.go`,
`internal/signing/sign.go`, `internal/relay/message.go` (lines 60–390 and 430–780),
`internal/auth/roster.go` (55–185), `client/keyring.go` (1–135), `internal/buscert/buscert.go`
(symbol index + the doc comments on the signing key), `DECISIONS.md` 2026-08-07 §"Cross-bus key
trust" and §"The bus TLS key and the bus SIGNING key are SEPARATE", `INVARIANTS.md` invariant 9.

**Bounded searches, stated so the reader knows what was not swept:**

- `internal/relay/registry.go` was read at lines 139–220, 460–600 only — not end to end.
- `internal/httpapi`, `internal/hub`, `internal/store` were NOT read. Claims about the local
  ingest/egress path below are inferred from `internal/relay`'s own doc comments and are labelled
  as such.
- `CRYPTO_DEEPDIVE.md` was grepped, not read. Its `key_epoch` / bundle design (lines 594–724,
  1124–1130) is a **prior art reference**, not a settled contract, and this document does not
  attempt to settle CRYPTO-4.
- Five other agents are editing `DECISIONS.md`, `internal/relay/registry.go`,
  `internal/relay/client.go`, `internal/relay/peerstore.go`, `internal/store/`, `internal/hub/` and
  `CONTRACTS-ONDISK.md` concurrently. Every line number below is against the tree at
  `HEAD = 711e282` plus the uncommitted working tree at the time of reading; a concurrent edit may
  have moved them.

---

## 1. Symptom

Relay ingest cannot accept a single message. Not "is untested", not "is unwired" — **cannot**, by
construction, on a bus that is otherwise fully configured.

`ValidateRelayRequest` is the ONLY constructor of a `RelayedMessage`, it takes `CrossBusTrust` as a
required parameter, and step 13 runs `VerifyRelayed` before it returns:

```go
// internal/relay/message.go:524
func ValidateRelayRequest(localBusID, idempotencyKey string, req RelayRequest, trust CrossBusTrust) (RelayedMessage, error) {
...
// internal/relay/message.go:687-690
	if err := VerifyRelayed(m, trust); err != nil {
		return RelayedMessage{}, err
	}
	return m, nil
```

`VerifyRelayed` fetches the origin bus's pin **first and unconditionally**
(`internal/relay/signed.go:283-289`), and a nil trust is a refusal
(`internal/relay/signed.go:266-268`). `CrossBusTrust` has **no PRODUCTION implementation and no default**
(the only implementation anywhere is `testTrust`, a faithful in-test fake at
`internal/relay/signed_test.go:138` — see landmine L8), and `signed.go:146-149` says that is
deliberate:

> This package ships NO implementation of this interface and NO default. That omission is
> deliberate and is not an oversight to be helpfully corrected: a default is the one thing every
> wiring site would reach for, and there is no default that is safe.

So today the outcome is `ErrUnpeeredBus` → `CodeUnpeeredBus` → **403**
(`internal/relay/relayhttp.go:311-321`). The package's own doc states the same conclusion in as
many words (`internal/relay/doc.go:189-191`):

> Until it does, `CrossBusTrust.PinnedBusSigningKeys` has NO SOURCE OF TRUTH, every relayed message
> is `ErrUnpeeredBus` by construction, and the relay ingest cannot be served at all.

The triggering evidence for this deep-dive is therefore not a log line — nothing serves the route
(`internal/relay/guards_test.go:36`: "NOTHING outside internal/relay may import internal/relay") —
it is the code path itself, which has exactly one terminal outcome.

---

## 2. Evidence

### 2.1 The bus signing key EXISTS on disk and SIGNS NOTHING

This is the single most useful thing found, and it corrects `internal/relay/doc.go`'s item 8.

`internal/buscert` already loads/creates a **separate** Ed25519 signing key, distinct from the TLS
key, and `cmd/agent-bus` already wires it:

| artefact | evidence |
| --- | --- |
| the key file | `CONTRACTS-ONDISK.md:1152` — `bus-signing.key`, `0600`, PKCS#8 Ed25519, "a SEPARATE key that attests agent key bundles; PINNED BY PEER BUSES at peering" |
| the accessors | `internal/buscert/buscert.go:199` `SigningPublicKey()`, `:207` `SigningPrivateKey()` |
| the anti-conflation guard | `internal/buscert/buscert.go:401` refuses to start when the signing key file is byte-identical to the TLS key |
| wired into the server | `cmd/agent-bus/buscert.go:84-85` `openBusCertMaterial` → `buscert.LoadOrCreate` |

And the callers:

```
$ grep -rn "SigningPrivateKey" --include=*.go .
internal/buscert/buscert.go:203:  (doc comment)
internal/buscert/buscert.go:207:  func (m *Material) SigningPrivateKey() ed25519.PrivateKey { return m.signingKey }
internal/buscert/buscert_test.go:279
internal/buscert/buscert_test.go:687
internal/buscert/buscert_test.go:689
```

**Zero non-test callers.** The key that the entire cross-bus trust model rests on is generated,
fsynced, backed up, guarded against conflation with the TLS key — and has never produced a
signature outside its own unit test. That is a *good* position to be in: the hard part (a
separate, durable, correctly-permissioned attestation key with an operator story) is already done.

Correction to record: `internal/relay/doc.go:182-183` is titled "THE PEERING HANDSHAKE CARRIES NO
BUS SIGNING KEY, SO NO PIN CAN EVER BE ESTABLISHED". The first clause is still true
(`internal/relay/peer.go:104-129` — `PeerEnrollRequest`/`PeerEnrollResponse` carry only `bus_id` and
`agents`). The second clause is **too strong**: a pin can be established without the handshake, and
§4.1 argues it must be.

### 2.2 Nothing populates an agent's messaging public key — so no bus can attest anything

`internal/auth/roster.go:101-113`:

```go
	// MessagingPublicKey is the agent's second Ed25519 key, for message
	// signing.
	//
	// RESERVED: NOTHING POPULATES IT YET. The SIGN epic (SIGN/CRYPTO-3) is the
	// task that will. Until then it is always empty, and Decode therefore
	// validates it only when it is present — empty IS the reserved state, not a
	// malformed key.
	MessagingPublicKey ed25519.PublicKey
```

`internal/auth/service.go:360` confirms it at the enrolment site: "MessagingPublicKey, InviteID and
CertBindings are left ZERO." `internal/auth/roster.go:307-319` validates it only when non-empty.

**This is a hard blocker on RELAY-14 that the wave-1 plan does not name.** Bus A cannot attest
`A.writer-7 → <key>` because A does not know `A.writer-7`'s messaging key. The attestation
mechanism designed below is correct and implementable, and it will attest nothing until enrolment
carries the messaging key. See §6, task T1.

### 2.3 The RECIPIENT's trust root is not the bus, and never was

`client/keyring.go:15-37` is unambiguous:

> No messaging keypair is registered at enrolment, and CRYPTO-4 — the server-attested key-bundle
> endpoint — does not exist. There is therefore NO WAY to obtain a sender's messaging public key
> FROM THE BUS. A recipient can verify a sender only if it obtained that sender's key OUT OF BAND
> and put it here, with `agent-busctl trust <agent-id> <base64-key>`.
> …
> There is no trust-on-first-use, no "trust the key the bus handed over", no verification-optional
> switch and no `--insecure`, and none may be added.

This matters for scoping RELAY-7 correctly, and it is easy to get backwards. **Bus-level
`CrossBusTrust` verification is INGEST ADMISSION CONTROL, not the recipient's authenticity
guarantee.** The recipient agent at C verifies end-to-end against its own `DirKeyRing`
(`client/keyring.go:120-128`), which C's bus cannot influence.

So the honest statement of what `CrossBusTrust` buys, and what it does not:

- **Buys:** a peer bus cannot inject forged traffic *attributed to another bus's agents* into C's
  durable store, C's audit trail (invariant 6), C's idempotency table
  (`internal/relay/doc.go:145-152` — the applied-key table is metered by the asserted origin
  sender, which is a resource-exhaustion surface), or C's agents' cursors. It also prevents the
  message-id pre-poisoning attack spelled out at `internal/relay/doc.go:128-143`.
- **Does not buy:** the recipient's assurance of who sent the body. That is the client's keyring,
  and it stays that way.

Nothing below weakens the client-side model. The two layers are independent and both fail closed.

### 2.4 The routing table has no room for a trust pin, and does not persist

`internal/relay/registry.go:139-145`:

```go
type peerState struct {
	busID   string
	baseURL string
	roster  map[string]struct{}
	version uint64
	updated time.Time
}
```

No signing key, no fingerprint. `Registry` itself (`registry.go:179-190`) is a `map` behind an
`RWMutex` and persists nothing. `Route` (`registry.go:495-512`) resolves strictly by the bus half
of a fully-qualified id against `r.peers`, i.e. **against directly-known peers only**.

### 2.5 A peer may only assert ids in its own namespace

`internal/relay/doc.go:33-41`:

> In particular a peer may only assert ids inside its OWN namespace. An incoming id whose bus half
> is our bus id — or differs from it only by ASCII case — is rejected as id spoofing…

Enforced for rosters at `registry.go:464-469` (`ErrBusIDCollision` / "bus %q may only describe its
own agents"; note `registry.go` is being edited concurrently by F3, so this line may have moved) and
for relay envelopes at `message.go:577-579` ("sender %q belongs to bus %q, but bus
%q may only speak for its own agents").

**Consequence for the topology:** B cannot tell C anything about A's agents through any existing
channel. Roster sync structurally forbids it. That is correct and must stay.

### 2.6 `Forward` carries a fixed field list verbatim

`internal/relay/message.go:721-738`. Every field is copied explicitly; there is no `...` and no
map. A field added to `RelayRequest` and not added here is **silently stripped at the first hop** —
which is precisely the A→B→C path. See §5, landmine L1.

### 2.7 `MaxRelayBytes` is a DERIVED, TEST-PINNED budget

`internal/relay/message.go:61-80` derives 256 KiB from a line-by-line accounting and states
"`TestMaxRelayBytesFitsAMaximumMessage` pins the derivation, because a bound nothing checks is a
description." Any new envelope field must be added to that accounting. See landmine L3.

---

## 3. Root cause

### 3.1 CONFIRMED root cause

**`CrossBusTrust` is a correctly-shaped seam with no data behind it.** Two distinct inputs are
missing, and they are missing for two different reasons:

1. **`PinnedBusSigningKeys(busID)` has no store.** There is no field on `peerState`
   (`registry.go:139-145`), no durable record, no CLI, no config. The bus signing key exists
   (`buscert.go:199`) but is never published or pinned.
2. **`AttestedSignerKey(fqAgentID, pins)` has no attestation to check against the pins.** The
   method's parameter list contains a subject and a set of verification keys — and *nothing to
   verify*. It can only work if the implementation already holds A's per-agent bindings, which for
   a **non-adjacent** origin it cannot obtain by any permitted route (§2.5).

Disproof test for (1): produce any code path that returns a non-empty `[]ed25519.PublicKey` from a
`PinnedBusSigningKeys` implementation sourced from durable state. None exists — `grep -rn
"PinnedBusSigningKeys" --include=*.go .` returns only `signed.go` and `signed_test.go`, and the
latter's pins come from an in-test keyring, not from disk.

Disproof test for (2): exhibit an implementation of `AttestedSignerKey` that satisfies its own
contract ("MUST NOT accept a key because a peer presented it alongside the message… MUST NOT accept
an intermediate bus's re-attestation… MUST NOT fall back to trusting a key it has merely seen
before", `signed.go:189-194`) for a sender on a bus we do not peer with. **This is impossible with
the current signature** — the argument in §4.2 is the proof.

### 3.2 Ranked candidates for "how the missing data should arrive", with disproof tests

| # | Candidate | Verdict | Disproof / confirmation test |
| --- | --- | --- | --- |
| 1 | **Operator-configured bus trust record (out of band) + attestation carried in the relay envelope** | **RECOMMENDED** | Confirmed by construction: it is the only option in this table that survives both the "no TOFU" ruling and the non-adjacent origin. See §4. |
| 2 | Bus signing key travels in the peering handshake (`PeerEnrollRequest`) and is pinned on receipt | REJECTED as the source of truth; ACCEPTED as a cross-check | Disproof: this is TOFU with a field name. `internal/relay/doc.go:207-212` says so explicitly — "adding a key field to this envelope without that channel would be trust-on-first-use wearing a field name — precisely what the ruling forbids." Second, independent disproof: **it cannot work at all for A**, because C never handshakes with A. |
| 3 | B vouches for A's agents (re-attestation) | REJECTED | `DECISIONS.md:2221` — the bundle keeps A's attestation intact, "not re-attested by any intermediate". This is the exact hole the whole decision exists to close: B is the internet-facing machine. |
| 4 | C pins A's per-AGENT messaging keys directly, out of band | REJECTED at bus scope | Disproof: agent ids are server-minted and never reused (invariant 1); A mints new agents continuously; the operator would have to redistribute to C on every enrolment at A. Note the CLIENT does exactly this (`client/keyring.go`) and it is fine there, because one agent talks to a handful of peers, not to a whole roster. |
| 5 | Per-bus key CACHE at C, populated on first sight | REJECTED | See §4.3 — four independent disproofs. |
| 6 | C fetches A's bundle from A on demand (CRYPTO-4 endpoint) | REJECTED for this topology | Disproof: A is not reachable from C. That is the topology, not an accident. Also needs peering to authenticate the fetch, which is what we do not have. |
| 7 | Roster push carries A's SIGNED attestations transitively through B | Viable as a later PREFETCH optimisation only | Not disproved, but it is a second transport for the identical artefact and it collides with §2.5's "own namespace" rule unless the artefact is verified against C's pin of A *before* it is stored. Deferred; see §6 task T9. |

---

## 4. The design

### 4.1 (i) Source of truth for `PinnedBusSigningKeys`

**Recommendation: an operator-configured BUS TRUST RECORD, keyed by bus id, copied out of band —
exactly as the invite blob carries the TLS certificate fingerprint.** I agree with the plan's
recommended direction, with **one substantive amendment**.

The argument:

- It satisfies invariant 11's "no CA and no TOFU" the same way the invite blob does. The invite
  blob is already the project's precedent for "a trust anchor travels over an operator-mediated
  channel, and the integrity of that channel is load-bearing" (`internal/invite/store.go:236`,
  `client/sanitize.go:13`).
- It is the only reading of `internal/relay/doc.go:207-212` that does not contradict itself. That
  paragraph says the key "must be delivered over the same operator-mediated channel the invite
  uses" and that putting it in the handshake without that channel is TOFU. Delivering it out of
  band and *cross-checking* the handshake value against it is the version that satisfies both
  sentences.
- Under F1/RELAY-6(e) peer configuration is already an offline `agent-bus peer` subcommand under
  the dirlock. The pin belongs in the same command and the same record — one operator action, one
  moment, one file.

**The amendment, and it is the important part: the TRUST record must be SEPARABLE from the PEER
(routing) record.**

The wave-1 plan's F5/RELAY-10 specifies a durable peer record carrying
`{bus_id, base_url, bus_signing_key, state}`. That shape works for B-at-C and B-at-A. It **does not
work for A-at-C**, because C holds no `base_url` for A, never dials A, and must never route to A —
`Route` resolving a bus id to a peer is how egress picks a next hop (`registry.go:495-512`), so
putting A in the peer table with a base URL would make C try to dial the laptop directly.

What C needs for A is a **trust-only** entry: "bus `laptop`'s signing key is `<32 bytes>`; I do not
route to it; I will accept messages that ORIGINATE there and arrive via some peer." Concretely:

```
peer record   := { bus_id, base_url, bus_signing_keys[], state }   -- routing AND trust (adjacent)
trust record  := { bus_id, bus_signing_keys[],           state }   -- trust ONLY      (non-adjacent)
```

The cheapest correct implementation is **one record type with `base_url` OPTIONAL**, plus a hard
rule that `Route` and `BroadcastTargets` skip an entry with no `base_url`. A separate record kind
is also defensible. Either way this is a **required correction to F5**, filed as §6 task T2, and it
must be settled before F5's `wal.Entry.Kind = "peer"` record shape is frozen on disk — after that
it is a migration.

**The cross-check rule, stated fail-closed — this is the sentence most likely to be
mis-implemented (gate finding P1-4).** If the peering handshake is later extended to carry the
peer's bus signing key (candidate 2), then:

> **If no out-of-band pin exists for that peer, the handshake's key value is DISCARDED and peering
> establishes NO pin. A cross-check may only ever cause a REFUSAL; it may never cause an
> ACCEPTANCE.**

The natural implementation — *"if we hold a pin, compare it; if not, take the handshake value"* —
**is trust-on-first-use**, and it is exactly what `signed.go:120-126` and
`internal/relay/doc.go:207-212` forbid. It is worth writing out because it does not look like TOFU
when you write it.

Two further requirements on the pin store:

- **`bus_signing_keys` is a LIST, not a scalar.** `signed.go:178-182` already fixes the meaning:
  more than one key exists ONLY during a signing-key rollover window, mirroring the
  two-certificate TLS rollover. A scalar field forecloses `DECISIONS.md:2243-2246`'s rotation rule
  and would have to be widened later.
- **A malformed pin must be refused, not skipped.** `signed.go:294-298` already enforces this at
  verification time; the store should refuse to *accept* one at configure time, so the operator
  learns at `agent-bus peer add` rather than at 3am on the first relayed message.

### 4.2 (ii) The **non-adjacent** origin: A→B→C

**Established: the attestation MUST travel in the relay envelope.**

The chain of forced steps, each with its evidence:

1. C must verify the message signature against the messaging key of `laptop.writer-7`
   (`signed.go:304`, `signed.go:326`).
2. C may only accept that key if A attested it under a key C pinned (`DECISIONS.md:2221-2223`,
   `signed.go:187-194`).
3. C can hold A's *bus signing key* by §4.1 — that is a single 32-byte constant per bus, copied
   once by the operator.
4. C **cannot** hold A's *per-agent bindings*. They are minted by A's server continuously
   (invariant 1: ids are server-minted and never reused), and there is no channel from A to C: A
   never peers with C, `Route` only knows direct peers (`registry.go:495-512`), and B is
   structurally forbidden from asserting ids in A's namespace (§2.5).
5. Therefore the binding must arrive **with the message**, minted by A, forwarded by B **verbatim**
   and unmodified, and verified at C against C's pin of A.

That is the definition of an envelope-carried attestation. B becomes a **courier, not a voucher** —
which is the entire security property the decision was bought for. B can drop the message, delay
it, or replace it wholesale with a message it forges from its *own* agents (which C attributes to
B, correctly); what B cannot do is produce a message C attributes to `laptop.writer-7`, because
that requires A's signing key.

**"Courier, not voucher" is scoped to CONTENT and ATTRIBUTION, and NOT to provenance** (gate
finding, question 1). `bus_path` remains unsigned and unattested and this design adds nothing
there — `internal/relay/doc.go:307-317` already rules that loop prevention is availability, never
security, and that the path "grows on every hop" so it can never be inside a signature. B can
therefore still strip its own hop, making C's audit trail record a false traversal — which matters,
because invariant 6 names "bus path traversed" as part of the audit trail — and can prepend a third
bus's id to poison loop prevention at C's next hop. Neither is new and neither is made worse; the
claim above must simply not be read as covering them.

Since A attaches the attestation **at egress, per message**, an attestation is fresh when it is
minted and there is no refresh protocol to design.

**But "expiry therefore costs nothing" is FALSE, and an earlier draft said it (gate finding
P1-5).** B forwards verbatim and **cannot re-mint** — re-minting is re-attestation, the one thing
the whole decision forbids (`DECISIONS.md:2221`). So a message queued at B across a partition
longer than `NotAfter` becomes permanently undeliverable. Two consequences, both of which are
requirements on T5/T6:

- `NotAfter` must be **derived from the maximum relay retention/retry window**, not a picked
  constant. (The plausible-sounding "24h" in an earlier draft was picked, which is the same defect
  class as picking a migration number.)
- Expiry gets its own sentinel, per binding check 4 above. Failing it inside the
  `ErrNoSignerKey`/`ErrBadSignature` family sends an operator hunting a forgery that never
  happened — the misattribution `message.go:766-781` calls "the worst kind".

The one place expiry helps unambiguously: it bounds relay replay. A replayed message whose
attestation has expired is refused even if C's applied-key table has forgotten the original.

#### The exact required change to `RelayRequest`

One new field on `internal/relay/message.go:105-185`:

```go
	// OriginAttestation is the ORIGIN bus's signed binding of Sender to the
	// messaging public key that signed this envelope. It is minted by the origin
	// bus with its BUS SIGNING KEY (internal/buscert.Material.SigningPrivateKey),
	// is FORWARDED VERBATIM by every intermediate, and is NEVER re-attested.
	OriginAttestation SignerAttestation `json:"origin_attestation"`
```

with (recommended home: `internal/attest`, see §4.5)

```go
type SignerAttestation struct {
	AgentID            string `json:"agent_id"`             // fully-qualified <bus-id>.<agent-id>
	MessagingPublicKey []byte `json:"messaging_public_key"` // 32 raw bytes
	KeyEpoch           uint64 `json:"key_epoch"`
	IssuedAtUnixMilli  int64  `json:"issued_at_unix_ms"`
	NotAfterUnixMilli  int64  `json:"not_after_unix_ms"`
	Signature          []byte `json:"signature"`            // 64 raw bytes, by the ORIGIN bus signing key
}
```

Required companion changes, all of which are bugs if omitted:

- **`RelayedMessage`** gains the same field (`message.go:220-300`), COPIED, never aliasing the
  decoded payload — the same rule `message.go:659-665` already applies to `Signature`, and for the
  identical time-of-check/time-of-use reason.
- **`Forward` (`message.go:721-738`) must carry it verbatim.** This is landmine L1.
- **`ValidateRelayRequest`** gains a shape check as **step 11b**, next to the existing signature
  length check at `message.go:631-633` and *before* the record is built: present, `AgentID` parses,
  `len(MessagingPublicKey) == ed25519.PublicKeySize`, `len(Signature) == ed25519.SignatureSize`,
  timestamps positive. A missing or malformed attestation is a **400** with a new
  `ErrMissingAttestation` sentinel — matching `signed.go:11-21`'s own taxonomy rationale, where
  "this envelope can never be verified by anyone" is a 400 and "not attributable" is a 403.
  **`AgentID` must be BOUNDED before any error quotes it** — `%q` expands a control byte to four
  characters, so an unbounded value lets a peer choose the size of the line we log about refusing
  it. This is the discipline `message.go:537-544` already applies to `origin_bus` (gate finding
  P2-5).
- **`VerifyRelayed` must refuse a zero attestation INDEPENDENTLY, as step 2b** — before the pin
  lookup at `signed.go:283` (gate finding P1-3). It is not enough that step 11b checked: `signed.go:213-220`
  establishes that `VerifyRelayed` is exported and fail-closed **standalone**, which is why
  `signed.go:271-273` re-checks the signature length that `message.go:631` already checked. Note
  the field is declared above as a VALUE type, not a pointer, deliberately: a pointer would make
  any path that reached `VerifyRelayed` without step 11b a nil dereference — a remote panic rather
  than a refusal, which is the same shape as the `ed25519.Verify` panic trap `sign.go:185-193`
  documents.
- **`relayFingerprint` (`message.go:314+`) must NOT cover the attestation.** Reasoning identical to
  `bus_path`'s: it is per-copy material, not identity-defining content, and covering it would make
  two copies carrying attestations minted a second apart an `idem.OutcomeViolation` between peers
  doing nothing wrong. Nothing is lost: substituting a *different valid* attestation for the same
  agent cannot help an attacker, because the message signature was made under the key in the
  original attestation and will not verify under any other. **This needs its own test.**
- **`MaxRelayBytes`'s derivation comment (`message.go:61-80`) must be updated** — landmine L3.

#### The exact required change to `CrossBusTrust`

Today (`signed.go:207`):

```go
AttestedSignerKey(fqAgentID string, pinnedOriginBusSigningKeys []ed25519.PublicKey) (ed25519.PublicKey, error)
```

`AttestedSignerKey` has **nowhere to receive an envelope-carried attestation**, and that is the
whole defect. The minimal correct change adds exactly one parameter:

```go
	// AttestedSignerKey returns the messaging public key that the ORIGIN bus
	// attests for fqAgentID, having verified `attestation` against one of
	// pinnedOriginBusSigningKeys AND NOTHING ELSE.
	AttestedSignerKey(fqAgentID string, attestation SignerAttestation, pinnedOriginBusSigningKeys []ed25519.PublicKey) (ed25519.PublicKey, error)
```

and `VerifyRelayed` (`signed.go:304`) passes `m.OriginAttestation` through at step 5.

**Why one parameter and not the whole `RelayedMessage`.** Passing the message would hand the
implementation the routed fields, and that re-opens exactly the space `signed.go:128-144` closed
when it replaced the one-method `SignerKeyResolver`: an implementation given the message can be
written to trust something *beside* the attestation, and the caller cannot tell from the signature
that it did not. The current design's whole virtue is that the implementation is handed the
subject, the material to check, and the only keys it may check it against — and nothing else. One
extra parameter preserves that; a message parameter destroys it.

**The three binding checks the implementation MUST perform**, and which the interface cannot force
(so they belong in `internal/attest` with tests, and in the method's doc comment):

1. `attestation.AgentID == fqAgentID`, byte for byte.

   **This check is LOAD-BEARING, not defence in depth. An earlier draft of this document said the
   opposite and was wrong; the security gate caught it (2026-08-08, P1-1) and the attack was then
   walked against the code. It is recorded here in full so nobody re-derives the mistake.**

   Anyone holding ONE A-agent's messaging private key — say `K_alice` — can impersonate EVERY OTHER
   agent on A, across the federation boundary, if this check is missing:

   - Build an envelope with `OriginBus="A"`, `MessageID="A-12345"`, **`Sender="A.bob"`**, and sign
     `signing.Canonicalize({Sender:"A.bob", …})` with `K_alice`.
   - Attach the **genuine, unmodified** attestation for `A.alice`. Attestations travel in the clear
     on every relayed message, so observing one message yields one.
   - `message.go:573-579` (check 7): `senderBus == req.OriginBus` — both `"A"` — **passes**.
   - `canonical.go:250-259`: sender's bus == the message id's bus — both `"A"` — **passes**.
   - `AttestedSignerKey("A.bob", <attestation for A.alice>, pins)` without this check: the
     attestation verifies against the pin, so it returns `K_alice`.
   - `signed.go:319` canonicalizes over `m.Sender = "A.bob"`; `signed.go:326` verifies that
     signature under `K_alice` — and the signature WAS made with `K_alice` over exactly those
     bytes, so it returns **true**.

   C attributes the message to `A.bob`, and nothing at C can detect it: C holds no roster of A's
   agents, `Route` knows only direct peers (`registry.go:495-512`), and §2.5 forbids B from telling
   C anything about A's namespace. The message signature does **not** save you — it covers
   `Sender`, but the *key it is checked against* is what this check selects. **A negative test for
   exactly this case is mandatory in T5.**
2. The bus half of `attestation.AgentID` equals `m.OriginBus`. This is what stops a peer presenting
   a validly-signed attestation from a *different* bus we also pin. `VerifyRelayed` looks the pins
   up by `m.OriginBus` (`signed.go:283`), so this is the check that ties the pin set to the
   subject.
3. `ed25519.Verify(pin, canonicalAttestationBytes, attestation.Signature)` for exactly one pin,
   trying each pin in turn (the rollover window) — and **nothing else may be tried**. Contrast
   `signed.go:249-258`: the pins are consumed entirely here at step 5, and are never tried against
   the MESSAGE signature.
4. **`NotAfter` is enforced — a MUST, not a SHOULD** (see §4.4; gate finding P1-2). With revocation
   across a non-adjacent link unsolved, expiry is the ONLY bound on a compromised agent messaging
   key, so an implementer who treats it as advisory makes every attestation eternal. Use the
   house clock-skew precedent, `clockSkewAllowance = 5 * time.Minute`
   (`internal/buscert/buscert.go:78`), and give the failure **its own sentinel**,
   `ErrAttestationExpired` — never the `ErrNoSignerKey` / `ErrBadSignature` family. An expired
   attestation is not a forgery, and `signed.go:41-55` created a separate sentinel precisely so an
   operator is not sent hunting one.

#### What a per-bus key cache cannot do here, and why

Candidate 5 in §3.2. Four independent disproofs, any one of which is fatal:

1. **There is no authenticated source to populate it from.** The only route by which A's agent keys
   can reach C is through B. Populating a cache from B's word is trust-on-first-use through the
   exact intermediate the design constrains — `signed.go:120-126`: "TOFU's exposure window is the
   moment of FIRST CONTACT, which is exactly the moment a hostile intermediate is best placed to
   act."
2. **A miss is the normal state, so the miss path is the real design — and the only available miss
   path is trusting B.** A mints new agents continuously. A cache whose fallback is "ask B" is not
   a cache, it is candidate 3 with a hit-rate optimisation.
3. **It goes stale in the unsafe direction.** A revoked or rotated key stays trusted at C until
   something invalidates it, and the invalidation channel A→C does not exist. A cache that cannot
   be invalidated is a pin the operator did not choose.
4. **It is redundant when the attestation is in the envelope.** Once every message carries a fresh
   binding, caching saves one 105-byte `ed25519.Verify` (~50µs) per message and buys nothing else.
   The pin table (32 bytes per bus, one per federation member) is the only thing worth holding, and
   it is operator-configured, not cached.

The one legitimate use of caching in this design is a **negative** one: a bus with no pin for
`m.OriginBus` short-circuits at `signed.go:283-289` before any attestation verification, which
`signed.go:245-247` already documents as the cost-ordering rationale.

### 4.3 (iii) The exact canonical byte string

**It reuses `internal/signing`'s canonicalizer rules and its unexported encoding helpers — it does
not invent an encoding.** The rules are stated normatively at `internal/signing/canonical.go:146-162`:
all integers big-endian, every variable-length field preceded by its `uint32` length, a
length-prefixed ASCII domain-separation `Context` as the FIRST field, fixed field order.

```
uint32 len || Context                    ("agent-bus/bus-attest/<V>")
uint32 len || AgentID                    (fully-qualified "<bus-id>.<agent-id>")
uint32 len || MessagingPublicKey         (32 raw bytes)
uint64        KeyEpoch
int64         IssuedAtUnixMilli          (two's complement)
int64         NotAfterUnixMilli          (two's complement)
```

Design notes, each tracking an existing decision in `internal/signing`:

- **`<V>` is a RESERVED number, not a chosen one.** `canonical.go:14-24` reserved value `1` for
  `agent-bus/msg-sig/1` through the Spec Server `signing-format-version` namespace. The
  implementing task **must** `POST /api/v1/projects/agent-bus/reservations
  {"namespace":"signing-format-version"}` and use what comes back. **The `2` in the worked example
  below is a PLACEHOLDER for a number nobody has reserved yet.**
- **The version lives inside `Context` and nowhere else** — `canonical.go:18-23`: "there is exactly
  ONE version indicator in the signed bytes and no way for two of them to disagree."
- **One claim about the bus, not two.** `AgentID`'s bus half IS the attesting bus, so there is no
  separate `BusID` field. This follows `ValidateRelayRequest` check 4's principle
  (`message.go:449-457`: "With two independent claims about where the message came from, every
  downstream consumer would have to choose one"). `signing.Canonicalize` takes the opposite tack
  (both `MessageID` and `Sequence`, cross-checked at `canonical.go:246-248`) because both halves
  were already on the wire for other reasons; here they are not, so the simpler shape wins.
- **No recipient set, no body, no message id.** An attestation is a statement about an AGENT, not
  about a message. Binding it to one message would force A to mint one per send *and* would let a
  verifier confuse the two artefacts.
- **`KeyEpoch` and `IssuedAtUnixMilli` are covered even though wave 1 enforces neither**, for the
  reason `canonical.go:189-193` gives about the signed timestamp: a field the LAYOUT already
  carries can be enforced later without a format version bump, whereas adding a field later is a
  new `Context`, i.e. a federation-wide flag day.

#### Worked example — the concrete bytes

Inputs (the public key is `0x01..0x20`, a **worked-example value, not a real key**):

| field | value |
| --- | --- |
| `Context` | `agent-bus/bus-attest/2` (22 bytes; `2` is a placeholder for the reserved version) |
| `AgentID` | `laptop.writer-7` (15 bytes) |
| `MessagingPublicKey` | `0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20` |
| `KeyEpoch` | `1` |
| `IssuedAtUnixMilli` | `1754650000000` |
| `NotAfterUnixMilli` | `1754653600000` (issued + 1h) |

The full 105-byte canonical string, hex:

```
000000166167656e742d6275732f6275732d6174746573742f32
0000000f6c6170746f702e7772697465722d37
000000200102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20
0000000000000001
00000198894a3a80
0000019889812900
```

Field by field:

| field | bytes |
| --- | --- |
| `uint32 len(Context)` = 22 | `00000016` |
| `Context` | `6167656e742d6275732f6275732d6174746573742f32` |
| `uint32 len(AgentID)` = 15 | `0000000f` |
| `AgentID` | `6c6170746f702e7772697465722d37` |
| `uint32 len(MessagingPublicKey)` = 32 | `00000020` |
| `MessagingPublicKey` | `0102…1f20` |
| `uint64 KeyEpoch` | `0000000000000001` |
| `int64 IssuedAtUnixMilli` | `00000198894a3a80` |
| `int64 NotAfterUnixMilli` | `0000019889812900` |

Total 105 bytes; the encoder consumed exactly 105, so the layout has no slack and no padding.
*(Produced by a throwaway program under the scratchpad implementing the same four helpers as
`canonical.go:294-316`; it is not in the repository and no repository file was modified to produce
it.)*

#### Domain separation, checked rather than asserted

The first field of an agent-signed message and the first field of a bus-signed attestation:

```
msg-sig  first field: 000000136167656e742d6275732f6d73672d7369672f31    ("agent-bus/msg-sig/1")
attest   first field: 000000166167656e742d6275732f6275732d6174746573742f32  ("agent-bus/bus-attest/2")
```

They differ in the length word itself (`0x13` vs `0x16`), so neither encoding can be a prefix of
the other and no byte string is a valid encoding of both. That is unambiguous length-prefixed
framing, which `canonical.go:26-32` already calls out as "framing — it is not a cryptographic
construction".

There is a **second, stronger and independent** separation here that the message format does not
have: the two artefacts are signed by **different keys**. Messages are signed by an AGENT's
messaging key; attestations by the BUS SIGNING key. Cross-protocol confusion requires one key to
sign both languages, and that never happens.

**Standing requirement this creates:** the bus signing key currently signs *nothing* (§2.1). After
this lands it signs exactly one artefact. If a future task makes it sign a second, that task owns
re-checking disjointness — the same obligation `canonical.go:38-44` places on agent keys.

### 4.4 Expiry, revocation, and what is honestly NOT solved

- **Expiry MUST be enforced at C** — binding check 4 in §4.2. It is a MUST and not a SHOULD
  precisely because revocation is unsolved (next bullet): `NotAfter` is then the *only* bound on a
  compromised agent messaging key, and a SHOULD an implementer skips makes every attestation
  eternal. `NotAfter` is derived from the maximum relay retention/retry window (see §4.2's P1-5
  note), never picked; the clock-skew allowance is `internal/buscert/buscert.go:78`'s
  `5 * time.Minute`; the failure has its own sentinel.
- **Revocation across a non-adjacent link is UNSOLVED, and this document does not solve it.** If
  `laptop.writer-7`'s key is compromised, C learns nothing until either the attestation expires or
  the operator edits C's pin for A — and editing the pin revokes *every* A agent at once, which is
  a sledgehammer. `NotAfter` bounds the exposure; it does not close it.
- **Do NOT enforce monotonic `KeyEpoch` at C in wave 1.** It is the obvious next idea and it is a
  trap: messages signed under epoch *n* can legitimately arrive after messages signed under epoch
  *n+1* (two routes, two queue depths), so a "never accept a lower epoch" rule at C would silently
  drop legitimate traffic and look exactly like a forgery. Recording the field now costs nothing;
  enforcing it needs its own design task.
- **State the consequence of that plainly (gate finding P2-6):** with `KeyEpoch` recorded but
  unenforced and revocation unsolved, a compromised messaging key stays valid *alongside* its
  replacement for the whole `NotAfter` window. Two live keys per agent, by design, for a bounded
  time. That is what makes expiry-as-a-MUST the only mitigation available in wave 1, and it is why
  T11 is a real follow-up rather than a nicety.

### 4.5 Package ownership: where the artefact lives

- **The canonical ENCODER belongs in `internal/signing`.** Its helpers (`appendLenPrefixed`,
  `appendUint32`, `appendUint64`, `canonical.go:294-316`) are unexported, so `internal/attest`
  cannot reuse them without exporting them. Add `signing.Attestation` +
  `signing.CanonicalizeAttestation` alongside `signing.Message` + `signing.Canonicalize`.

  *(An earlier draft justified this as "one package owns every canonical byte layout, so two cannot
  drift". That justification is contradicted by the tree — gate finding P2-2:
  `client/canonical.go:14-27` is an explicit PINNED MIRROR of `internal/signing`, duplicated on
  purpose because `client/` may not import `internal/`. The PLACEMENT is still right, for a
  narrower reason: per §2.3 the client never verifies attestations — attestations are a bus-to-bus
  artefact — so no mirror is needed and no second copy is created.)*

  **`FormatVersion` must NOT be reused (gate finding P2-1).** `canonical.go:24` declares one
  exported `FormatVersion`, already consumed as *the* format version in a peer-facing error string
  at `message.go:656`. Two layouts behind one constant is exactly the "two meanings on one key"
  defect `doc.go:226-234` warns about. Declare a separate `AttestationFormatVersion`, and update
  `message.go:656`'s text to name which format it means.
- **The POLICY belongs in `internal/attest` (RELAY-14).** The binding checks (§4.2), the pin
  matching, the error taxonomy, the expiry rule, and the `CrossBusTrust` implementation (RELAY-17).
- **`internal/relay` owns neither.** It owns the seam and the envelope field. Note
  `internal/relay/guards_test.go:36` forbids anything importing `internal/relay` — the dependency
  must run relay → attest → signing, never the reverse.

### 4.6 (iv) Invariant 9: **no new crypto primitive** is implied. Which side of the line, and why

**Determination: this design is on the PERMITTED side of invariant 9, and I am not escalating.**
Here is the full accounting, so a reviewer can check the claim rather than take it.

What the design uses:

| operation | implementation |
| --- | --- |
| sign an attestation | `crypto/ed25519.Sign(busSigningPriv, canonicalBytes)` — unhashed |
| verify an attestation | `crypto/ed25519.Verify(pin, canonicalBytes, sig)` |
| verify a message | unchanged; `signed.go:326` already calls `ed25519.Verify` |
| key generation | unchanged; `internal/buscert` already generates the signing key |

What the design does **not** add: no cipher, no hash function, no MAC, no KDF, no key exchange, no
ratchet, no padding scheme, no nonce scheme, no IV scheme, no signature scheme, and **no bespoke
construction assembled from good primitives**. There is no pre-hashing (`sign.go:130-135`:
crypto/ed25519 exposes no pre-hash mode, and handing it a digest is the exact silent-failure shape
invariant 9 warns about — the canonical bytes go to `Sign`/`Verify` **unhashed**). There is no
composition of two primitives whose interaction anyone would have to reason about: it is one
`Sign`, one `Verify`, over a byte string.

**Why defining a canonical byte encoding is framing and not a primitive.** SIGN-1 already
established this pattern and `canonical.go:26-32` already litigates it in the repository:

> A fixed, documented, length-prefixed ASCII prefix is framing — it is not a cryptographic
> construction, and nothing about the signature scheme depends on its content.

The security of Ed25519 does not depend on the structure of the message it signs. What the
encoding must provide is *unambiguity* (two different field tuples must never encode to the same
bytes) and *domain separation* (an artefact of one type must never be a valid encoding of another),
and both are obtained by copying `internal/signing`'s existing rules verbatim rather than by
inventing anything. §4.3 checks both properties concretely.

**This is nonetheless the closest this epic comes to the line, so the guard rails are explicit:**

- The version number is **RESERVED through the Spec Server**, never chosen (`canonical.go:14-24`).
- Changing ANY field, width or order is a **new `Context` string**, never an in-place edit —
  `canonical.go:20-23`.
- The signed bytes are **re-derived at the verifier from the fields it will act on**, never
  transported. This is `PROTOCOL.md` §8.5 and `signed.go:65-91`, and it is the rule that keeps a
  hostile intermediate from presenting one blob to the verifier and different fields to the router.
- **The one thing that WOULD require escalation, named so it is recognisable:** if anyone proposes
  deriving the attestation key from the TLS key (a KDF), or MAC-ing the attestation instead of
  signing it, or composing the message signature and the attestation signature into a single
  aggregate value — **stop and escalate**. Each of those is a construction, not framing. None is
  needed and none is proposed here.

---

## 5. Latent landmines found along the way

**L1 — `Forward` will silently strip the attestation.** `message.go:721-738` enumerates every
forwarded field explicitly. Add `OriginAttestation` to `RelayRequest` and forget `Forward`, and
A→B works while **A→B→C fails at C with `ErrNoSignerKey`** — the exact topology this task exists
for, and the only one the two-node test does not cover. This is the same shape as the
`sent_at_unix_ns` bug `message.go:139-147` documents. **Requires a dedicated three-hop test.**

**L2 — nothing populates `auth.RosterEntry.MessagingPublicKey`** (§2.2, `roster.go:101-113`,
`service.go:360`). The origin bus cannot attest a key it does not have. RELAY-14/RELAY-17 will be
implementable and untestable-end-to-end until this lands.

**L3 — `MaxRelayBytes`'s derivation is a test-pinned budget** (`message.go:61-80`,
`TestMaxRelayBytesFitsAMaximumMessage`). The attestation adds roughly 150 (agent id) + 44 (base64
key) + 88 (base64 signature) + 3 × 20 (integers) + field names ≈ 400 bytes. The 256 KiB cap has
~2.5× headroom so no cap changes, but **the comment's accounting becomes a lie if not updated**,
and this repository has been bitten repeatedly by comments that quietly stopped being true.

**L4 — `relayFitsCanonicalFormat` (`message.go:785`) asserts relay's caps are strictly inside the
signing format's.** A second such guard is needed for the attestation, or a maximum-length agent id
could produce an attestation `signing` would refuse — and the operator would be told the peer sent
no attestation when in fact our own limits drifted. `message.go:766-781` explains why that
misattribution is "the worst kind".

**L5 — `internal/relay/doc.go:182-191` item 8 is now partly wrong** and will mislead whoever picks
up INVITE-PEERGUARD: the bus signing key DOES exist (§2.1), and a pin CAN be established without
the handshake. The paragraph should be corrected by whoever owns `doc.go` — **not by this task**
(file ownership, wave-1 standing rule 1).

**L6 — `RelayedMessage.Scope()` meters the idempotency table by the peer-ASSERTED sender**
(`message.go:310-312`, called out at `doc.go:145-152`). Attestation verification does not fix this:
verification happens at `message.go:687`, and whether the scope is taken before or after is a
property of the *wiring site*. The wiring task must ensure nothing is admitted to the applied-key
table before `ValidateRelayRequest` returns successfully. **Out of scope for RELAY-7; recorded so
it is not rediscovered.**

**L8 — the tests already contain an ad-hoc attestation encoding that T4 must REPLACE** (gate
finding P2-4). `internal/relay/signed_test.go:122-123`:

```go
func attestationBytes(fqAgentID string, pub ed25519.PublicKey) []byte {
	return append([]byte(attestationContext+fqAgentID+"|"), pub...)
}
```

`testTrust` (`signed_test.go:138`) is a *faithful* fake — it really does verify this against the
pins it is handed, which is why the two-method seam is meaningfully tested. But the encoding is
concatenation with a `|` separator, and it is unambiguous **only by accident**: `|` happens to be
outside `ids.ParseAgentID`'s charset. If T4 lands `signing.CanonicalizeAttestation` and this fake
is not switched over, every relay test keeps passing against a format that is not the shipped one.
T7 must name this file explicitly.

**L9 — a new, low-value pin-confirmation oracle** (gate finding P2-3). A verifying attestation with
a failing message signature yields `ErrBadSignature`, while a non-verifying attestation yields
`ErrNoSignerKey` — so one POST confirms *which* bus signing key C has pinned for bus X. The values
are public keys and the gate authenticates before this handler is reachable, so it is accepted for
the same reason as the peer-enumeration oracle at `doc.go:381-386` — but it should be added to that
same accepted-residuals list rather than left unnamed.

**L7 — `PeerView` (`registry.go:127`) / `Peers()` (`registry.go:558`) is a snapshot type that will need the pin state**
if operators are to answer "do I trust bus X" without reading the WAL. Cosmetic, but it is the
diagnostic the first federation incident will want.

---

## 6. SPEC-ready task breakdown

Atomic tasks for the orchestrator / spec-keeper to add via `POST /projects/agent-bus/tasks`.
**No `<EPIC>-<N>` key is invented here.** Where a numbered key is wanted, reserve it from the
`task-key-RELAY` namespace (seeded past the epic's current max); otherwise use the descriptive
title and the server-assigned `public_id`. Derived keys below (`RELAY-14-*`) are unique by suffix.

| # | Title | Depends on | Notes |
| --- | --- | --- | --- |
| T1 | *Enrolment records the agent's messaging public key* | — | Unblocks everything. Populates `auth.RosterEntry.MessagingPublicKey` (`roster.go:101-113`) at `auth/service.go:360`. Touches the durable roster record (`auth/record.go:131-132` already encodes it) — **P0 for this epic; it is the true long pole.** |
| T2 | *Split TRUST from ROUTING in the durable peer record* | F5/RELAY-10 **before its record shape is frozen** | §4.1. `base_url` optional (or a second record kind); `bus_signing_keys` a LIST; `Route`/`BroadcastTargets` skip trust-only entries. **Coordinate with F5's owner — this is a correction to a wave-1 task in flight.** |
| T3 | *Reserve the attestation signing-format version* | — | `POST .../reservations {"namespace":"signing-format-version"}`. One API call. Blocks T4. |
| T4 | *`signing.Attestation` + `CanonicalizeAttestation`* | T3 | §4.3. Layout, bounds, `ErrInvalid` on every failure, table-driven tests, a golden-bytes test against §4.3's worked example. Declare a SEPARATE `AttestationFormatVersion`; do not overload `FormatVersion` (P2-1). |
| T5 | *`internal/attest`: mint + verify (RELAY-14)* | T4 | Mint with `buscert.Material.SigningPrivateKey()`; verify with the **four** binding checks of §4.2 (expiry is check 4, a MUST) + skew allowance + its own `ErrAttestationExpired`. **Mandatory negative test: the `A.alice`-signs-as-`A.bob` re-attribution case in §4.2 binding check 1.** |
| T6 | *`RelayRequest.OriginAttestation` + shape check 11b + `Forward` carries it verbatim* | T4 | §4.2. **Must include a three-hop A→B→C test (landmine L1).** Update `MaxRelayBytes`'s derivation comment (L3) and add the `relayFitsCanonicalFormat` sibling guard (L4). |
| T7 | *`CrossBusTrust.AttestedSignerKey` takes the attestation* | T6 | §4.2. Signature change + `signed.go:304` threads `m.OriginAttestation`, plus `VerifyRelayed` step 2b (independent zero-attestation refusal, P1-3). Doc comment carries all four binding checks. **Must also replace `signed_test.go`'s `attestationBytes`/`testTrust` ad-hoc encoding (landmine L8) — otherwise the suite validates a format that does not ship.** |
| T8 | *`attest.Trust` implements `CrossBusTrust` (RELAY-17)* | T2, T5, T7 | `PinnedBusSigningKeys` reads the trust store; `AttestedSignerKey` delegates to `internal/attest`. |
| T9 | *`agent-bus peer` records a bus signing key out of band* | T2 | Offline subcommand under the dirlock, per F1/RELAY-6(e). Refuse a malformed pin at configure time. `CONTRACTS-CLI.md` + `AGENT_PROTOCOL.md` in the SAME task (invariant 7). |
| T10 | *Origin bus attaches an attestation at relay egress* | T1, T5, T6 | The mint side. Where it lives depends on the egress wiring (`internal/hub` / the forwarder) — needs the wave-2 owner. |
| T11 (follow-up) | *`RELAY-14-FU-REVOKE`: revocation across a non-adjacent link* | T8 | §4.4. Explicitly UNSOLVED today. Include the "do not enforce monotonic `KeyEpoch` naively" reasoning. |
| T12 (follow-up) | *`RELAY-14-FU-PREFETCH`: roster-borne attestation prefetch* | T8 | Candidate 7 in §3.2. Optimisation only; the artefact and its verification must be byte-identical to T5's. |
| T13 (docs) | *Correct `internal/relay/doc.go` item 8* | T2 | Landmine L5. Owned by `doc.go`'s owner, not by this task. |

Suggested `proof_cmd`s must each be confirmed **RED before the fix** and run through
`bash scripts/proof-check.sh` (a `go test -run` naming an unwritten test is VACUOUS, not a pass).

---

## 7. Cost, risk, rollback

**Cost.** Per relayed message: one extra `ed25519.Verify` over 105 bytes (~50µs on this class of
hardware, unmeasured here — measure before quoting it) and ~400 extra envelope bytes (~0.4% of a
maximum 102 KiB message, ~40% of a minimal one). Verification is ordered *after* the pin lookup
(`signed.go:245-247`), so traffic from an unpinned bus still costs one map lookup.

**Risk, ranked.**

1. **HIGH — freezing F5's on-disk peer record before T2 lands.** After a `wal.Entry.Kind = "peer"`
   record exists on disk, changing its shape is a migration, and the non-adjacent trust entry has
   nowhere to live. **This is the one thing that must be coordinated this week.**
2. **MEDIUM — L1, the stripped attestation.** Two-node tests pass; the actual topology fails.
   Mitigated only by a three-hop test.
3. **MEDIUM — operator error in the out-of-band pin.** A wrong pin is a bus whose every message is
   403 with a correct-looking error. Mitigate: `agent-bus peer` prints the fingerprint for
   eyeball-comparison, the way `buscert.Fingerprint` already does
   (`internal/buscert/fingerprint.go:49`).
4. **LOW — clock skew tripping `NotAfter`.** Mitigated by the 5-minute allowance precedent.

**Rollback.** Every step is additive and independently revertable. The pre-change behaviour is
"every relayed message is 403", so **a partial rollback cannot be worse than today** — it can only
return to a state where nothing relays. There is no data migration until T2 writes a durable trust
record, and that record is additive to a `wal.Entry.Kind` that does not exist yet. The one
irreversible commitment is the reserved `signing-format-version` number (T3), which is cheap and
which is reserved precisely so it can be abandoned without collision.

---

## 8. Verification of this document

`proof_cmd`:

```
test -s FEDERATION_TRUST_DEEPDIVE.md && grep -n 'AttestedSignerKey' FEDERATION_TRUST_DEEPDIVE.md && grep -n 'non-adjacent' FEDERATION_TRUST_DEEPDIVE.md && grep -n 'no new crypto primitive' FEDERATION_TRUST_DEEPDIVE.md
```

Observed **RED before the work**, honestly and with the file genuinely absent:

```
proof-check: class: file-assertion
proof-check: FAIL — proof command exited 1
proof-check: verdict=FAIL class=file-assertion exit=1 tests_run=0 top_level=0 skipped=0 failed=0 empty_pkgs=0
```

That is a **FAIL**, not a VACUOUS — the command ran and the assertion was false. This is a
documentation proof, so per `CLAUDE.md` it pins the specific claims it certifies
(`AttestedSignerKey` = §4.2's interface change; `non-adjacent` = §4.2's A→B→C analysis; `no new
crypto primitive` = §4.6's invariant-9 determination) rather than matching incidentally.

After the work, the same command was re-run through `proof-check.sh`:
`verdict=PASS class=file-assertion exit=0`.

### Gates run

- **security — RUN. Verdict: PASS-WITH-FINDINGS, no P0.** It independently re-derived §4.3's
  worked example (`4+22 + 4+15 + 4+32 + 8 + 8 + 8 = 105`, both int64s decoding exactly) and
  **confirmed the invariant-9 determination: PERMITTED, not escalating.** It raised five P1s and
  six P2s, **every one of which is a correction to this document**, and all of them have been
  applied above: P1-1 (binding check 1 is load-bearing, not defence in depth — the earlier draft
  was wrong in the reassuring direction, §4.2), P1-2 (expiry MUST, §4.2 check 4 + §4.4), P1-3
  (`VerifyRelayed` step 2b + value type not pointer, §4.2), P1-4 (the fail-closed cross-check rule,
  §4.1), P1-5 (expiry is NOT free — B cannot re-mint, §4.2), P2-1 (`AttestationFormatVersion`,
  §4.5), P2-2 (the `client/canonical.go` mirror contradicts the old placement argument, §4.5), P2-3
  (pin-confirmation oracle, L9), P2-4 (`testTrust` exists, §1 + L8), P2-5 (bound before quote,
  §4.2), P2-6 (two live keys per agent, §4.4).
- **reviewer — NOT RUN.** Optional for a design document per this task's brief; the security gate
  is the one that carries the weight here, and it verified the file:line citations as a side
  effect.
- **P1-1 is the reason this document is more trustworthy after the gate than before it.** It was a
  genuine re-attribution hole asserted as safe. Recorded in place rather than silently corrected.
