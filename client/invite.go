package client

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"syscall"
)

// Invite is the operator-minted invite blob as the CLIENT consumes it — the
// trust anchor of DECISIONS.md E6.
//
// The four fields that make it a trust anchor are BusID, BusAddress,
// BusCertFingerprint and InviteSecret: together they are what lets an agent
// reach the right bus, verify its certificate BEFORE the first connection
// (invariant 11 — no CA and deliberately no trust-on-first-use), and prove it
// was invited (invariant 3 — enrolment is invite-only). Whoever can substitute
// this blob can point an agent at a bus of their choosing, so it travels over a
// channel whose integrity you trust, and it never travels on argv.
//
// # The json tags mirror cmd/agent-bus/invite.go's inviteBlob EXACTLY
//
// That struct is the producer; this one is the consumer. The two must agree
// byte for byte on every key spelled here.
//
// json.Decoder.DisallowUnknownFields is deliberately NOT used. The mint already
// emits `ok`, `created_at`, `label` and `transport_insecure`, which are
// operator-facing and none of this client's business, and it may add more. A
// client that refused a blob because the mint gained a field would break on the
// next mint change, in the field, at the one moment an operator is trying to get
// an agent onto a bus. Extra keys are ignored; the ones below are required and
// are validated.
//
// InviteSecret is a BEARER CREDENTIAL. See String: it is never rendered, never
// logged and never put in an error by anything in this file.
type Invite struct {
	// InviteID is the server-minted id of the invite (invariant 1). It is a
	// NAME, not a credential — safe to log, to quote in a ticket, and to report
	// in EnrolResult so an operator can see which invite was spent.
	InviteID string `json:"invite_id"`

	// BusID is the bus's server-minted id, carried so an operator reading the
	// blob can see which bus it names.
	//
	// IT IS NOT CHECKED AGAINST ANYTHING, and nothing here should imply it is.
	// Validate does not constrain it, and Enrol does NOT compare it with the
	// bus_id the server returns — the SERVER is authoritative on ids (invariant
	// 1), so this field is a claim by whoever wrote the blob, not a fact. The
	// certificate fingerprint is what actually binds this invite to a specific
	// bus (invariant 11); this is a label beside it.
	//
	// It therefore reaches output only through String, which safeText-bounds it,
	// because an unvalidated field must never be interpolated raw. Adding the
	// cross-check against the server's answer is a genuine improvement and is
	// filed as a follow-up; until it lands, do not write a comment here that
	// claims a confirmation the code does not perform.
	BusID string `json:"bus_id"`

	// BusAddress is the bus base URL this invite is redeemed against. It is the
	// address enrolment uses: with an invite, --bus is unnecessary, and a --bus
	// that disagrees is REFUSED rather than silently preferred (see Enrol).
	BusAddress string `json:"bus_address"`

	// BusCertFingerprint is the bus's TLS certificate fingerprint, 64 lowercase
	// hex characters. This is the anti-TOFU half of the blob: the client knows
	// which certificate to expect before it dials.
	BusCertFingerprint string `json:"bus_cert_fingerprint"`

	// InviteSecret is the invite's plaintext bearer credential. Whoever holds it
	// can enrol an agent onto this bus.
	InviteSecret string `json:"invite_secret"`

	// ExpiresAt is when the bus stops accepting this invite, RFC3339. It is
	// carried for the operator's benefit; the BUS is authoritative on expiry and
	// this client does not pre-judge it against a local clock.
	ExpiresAt string `json:"expires_at"`
}

// MaxInviteFileBytes bounds an invite file before it reaches json.Decode.
//
// A minted blob is a few hundred bytes. 64 KiB is two orders of magnitude of
// headroom and still finite: an invite arrives from OUTSIDE — it is handed over
// by an operator, copied through a chat window, or dropped in by a deployment
// system — so it is untrusted input, and a client that streams an
// attacker-chosen file into memory has handed over its process. A file larger
// than this is REFUSED rather than truncated: a truncated JSON document either
// fails to parse (noisy, but confusing) or, worse, parses as something the
// operator did not write.
const MaxInviteFileBytes = 64 << 10

// MaxInviteIDLen is the hard byte cap on an invite id, PINNED here to match
// internal/invite.MaxInviteIDLen, which is the source of truth.
//
// It is duplicated rather than imported because the client package must not
// depend on internal/ (invariant 7: an agent has to be able to EMBED this
// package, and Go forbids importing internal/ from another module). The server
// remains authoritative: this check exists only so an oversized value is
// refused before it is put on the wire.
const MaxInviteIDLen = 64

// MaxSecretLen bounds a PRESENTED invite secret, PINNED here to match
// internal/invite.MaxSecretLen, which is the source of truth. Pinned for the
// same reason as MaxInviteIDLen, and enforced here for the same narrow purpose.
const MaxSecretLen = 256

// maxBusAddressLen bounds the invite's bus_address. A real one is
// "https://127.0.0.1:8080"; 2048 is the de-facto ceiling for a URL and two
// orders of magnitude of headroom over anything an operator would mint.
//
// It exists because bus_address is the ONE invite field with no alphabet of its
// own — every other field is an id, a secret, a hex fingerprint or a timestamp,
// all of which are already bounded — and MaxInviteFileBytes alone would admit
// one of ~64 KiB. Wrapping the places this package PRINTS it (safeText) is not
// sufficient on its own: a long-but-well-formed https URL parses, so it reaches
// the transport's own "cannot reach the bus at <url>" message and the credential
// store, neither of which this file can reach. Bounding it at the door is what
// makes "no invite-derived byte reaches a terminal unbounded" true rather than
// true-in-the-places-we-remembered.
//
// REFUSED, not truncated, and the value is not echoed: it is oversized by
// definition, exactly as the invite_id and invite_secret cases above.
const maxBusAddressLen = 2048

// inviteOp names invite-loading failures in *Error.Op.
const inviteOp = "invite"

// String renders the invite WITHOUT its secret.
//
// This exists so an accidental %v, %s, log line or error interpolation cannot
// print a bearer credential. Go picks a Stringer up automatically for both
// Invite and *Invite (value receiver), so the redaction applies even where
// nobody remembered to think about it — which is the only kind of redaction
// that holds.
//
// The three fields it DOES render go through safeText for the same "even where
// nobody remembered" reason. This method is reached on an UNVALIDATED invite —
// a %v in a decode path runs before Validate — and all three are chosen by
// whoever produced the blob. An unbounded, un-neutralised %v is precisely the
// forgery sanitize.go exists to stop, and it is the easiest one to reach by
// accident.
func (i Invite) String() string {
	id := safeText(i.InviteID, MaxInviteIDLen)
	if id == "" {
		id = "(no id)"
	}
	bus := safeText(i.BusID, maxDetailBytes)
	if bus == "" {
		bus = "(no bus id)"
	}
	addr := safeText(i.BusAddress, maxDetailBytes)
	if addr == "" {
		addr = "(no address)"
	}
	return "invite " + id + " for bus " + bus + " at " + addr + " (secret redacted)"
}

// GoString redacts the %#v verb too. Without it, fmt prints the struct field by
// field — secret included — and %#v is exactly what a debugging line reaches
// for.
func (i Invite) GoString() string { return "client.Invite{" + i.String() + "}" }

// LoadInviteFile reads and validates an invite blob from a file.
//
// This is the path the CLI uses (`agent-busctl enrol --invite-file <path>`),
// and a FILE rather than a flag value because the blob carries a bearer
// credential: a secret on argv is visible in the process list to every local
// user and lands in shell history.
//
// The file must be a regular file with no group or world permission bits. Both
// checks are made against the OPEN FILE (f.Stat), not the path, so there is no
// window in which the thing checked and the thing read are different files.
//
// # Why the open is NON-BLOCKING
//
// The regular-file check below says "reading one can block forever", and until
// this flag was added that refusal could not be REACHED for the case it names:
// opening the read end of a fifo blocks until a writer appears, so
// `--invite-file <a fifo>` parked with no output and no timeout. That is
// invariant 7's "an agent shelling out must never meet a prompt" failure in its
// worst form — a supervisor waits forever with nothing to explain it.
//
// O_NONBLOCK returns immediately for a fifo with no writer and is ignored for a
// regular file on every platform we ship to, so it changes nothing about the
// path this function is FOR. It is preferred over an os.Stat pre-check because
// a pre-check leaves the race it was added to close: a path that becomes a fifo
// between the stat and the open still hangs. The authoritative check stays on
// the OPEN FILE, so the TOCTOU-free property is unchanged.
//
// Portability cost, stated rather than discovered: this pulls in syscall, whose
// O_NONBLOCK is defined on linux, the BSDs, darwin, solaris and windows, but
// NOT on plan9. agent-bus ships in a Linux container (CLAUDE.md, "Runtime
// target") and the client package is embedded from ordinary developer
// platforms, so that is an acceptable trade — but it IS a trade, and a plan9
// port would have to give this an os.Stat pre-check behind a build tag.
func LoadInviteFile(path string) (*Invite, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, wrapError(KindConfig, inviteOp,
				"no invite file at "+path,
				"pass --invite-file <path> pointing at the JSON blob `agent-bus invite mint -json` produced, or '-' to read it from stdin",
				err)
		}
		return nil, wrapError(KindConfig, inviteOp,
			"cannot read the invite file "+path,
			"check the path and that this user can read it",
			err)
	}
	defer func() { _ = f.Close() }()

	fi, err := f.Stat()
	if err != nil {
		return nil, wrapError(KindConfig, inviteOp,
			"cannot inspect the invite file "+path, "check the path and its permissions", err)
	}
	if !fi.Mode().IsRegular() {
		// A fifo, a device or a directory. Not an invite, and reading one can
		// block forever or return something nobody wrote.
		return nil, newError(KindConfig, inviteOp,
			"the invite file "+path+" is not a regular file",
			"point --invite-file at the JSON blob the mint wrote, or pass '-' to read it from stdin")
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		// The invite holds a bearer credential, so a file any other local user
		// can read is a credential any other local user can spend. Refused, not
		// tightened: unlike the credential store — which this client OWNS and
		// therefore repairs from 0775 to 0700 — the invite file belongs to the
		// operator, and silently chmod-ing someone else's file would hide the
		// fact that it may already have been read.
		return nil, newError(KindConfig, inviteOp,
			fmt.Sprintf("the invite file %s is mode %04o, so other local users can read it, and it holds a bearer credential", path, perm),
			"chmod 0600 "+path+" — and if it was readable while an untrusted user had access to this machine, treat the invite as spent: revoke it and mint another")
	}

	inv, err := ParseInvite(f)
	if err != nil {
		// The path is added HERE rather than inside ParseInvite, which does not
		// know it. Nothing from the file's CONTENT is added with it.
		var e *Error
		if errors.As(err, &e) {
			e.Message = e.Message + " (" + path + ")"
		}
		return nil, err
	}
	return inv, nil
}

// ParseInvite decodes and validates an invite blob from r. It is the stdin and
// the embedder path; LoadInviteFile is the file one.
//
// At most MaxInviteFileBytes are read, and content AFTER the JSON object is
// refused: two concatenated blobs would otherwise let the first one decide
// which bus is contacted while a reader of the file sees the second, and a
// trailing fragment means the operator handed over something other than what
// the mint printed.
//
// # No error from this function contains any of r's content
//
// encoding/json's own errors quote the input — a SyntaxError names the offending
// character — so they are translated to an offset and a type rather than
// wrapped. The blob holds a bearer credential, and errors are printed to
// terminals, piped into logs and pasted into tickets.
func ParseInvite(r io.Reader) (*Invite, error) {
	if r == nil {
		return nil, newError(KindInternal, inviteOp, "no invite reader", "")
	}
	// Read the bound PLUS ONE so an oversized file is detected rather than
	// silently truncated at the limit.
	buf, err := io.ReadAll(io.LimitReader(r, MaxInviteFileBytes+1))
	if err != nil {
		return nil, wrapError(KindConfig, inviteOp, "cannot read the invite", "check the file or the pipe it was read from", err)
	}
	if len(buf) > MaxInviteFileBytes {
		return nil, newError(KindConfig, inviteOp,
			"the invite is larger than "+strconv.Itoa(MaxInviteFileBytes)+" bytes",
			"a minted invite is a few hundred bytes; check that --invite-file points at the JSON blob from `agent-bus invite mint -json` and not at something else")
	}
	if len(bytes.TrimSpace(buf)) == 0 {
		return nil, newError(KindConfig, inviteOp,
			"the invite is empty",
			"pass the JSON blob `agent-bus invite mint -json` produced")
	}

	var inv Invite
	dec := json.NewDecoder(bytes.NewReader(buf))
	if err := dec.Decode(&inv); err != nil {
		return nil, invalidInviteJSON(err)
	}
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); err != io.EOF {
		return nil, newError(KindConfig, inviteOp,
			"the invite has content after the JSON object",
			"pass exactly one invite: a file holding two concatenated blobs is ambiguous, and only the first would be redeemed")
	}

	if err := inv.Validate(); err != nil {
		return nil, err
	}
	return &inv, nil
}

// invalidInviteJSON translates an encoding/json error into one that reports
// WHERE the document is wrong without reproducing WHAT it says. See ParseInvite.
func invalidInviteJSON(err error) *Error {
	var syntax *json.SyntaxError
	if errors.As(err, &syntax) {
		return newError(KindConfig, inviteOp,
			"the invite is not valid JSON at byte offset "+strconv.FormatInt(syntax.Offset, 10),
			"pass the JSON blob `agent-bus invite mint -json` produced, unedited")
	}
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		field := typeErr.Field
		if field == "" {
			field = "(top level)"
		}
		return newError(KindConfig, inviteOp,
			"the invite field "+field+" is not a "+typeErr.Type.String(),
			"pass the JSON blob `agent-bus invite mint -json` produced, unedited")
	}
	// Deliberately NOT wrapped: any other encoding/json error may quote the
	// document, and the document holds a bearer credential.
	return newError(KindConfig, inviteOp,
		"the invite is not valid JSON",
		"pass the JSON blob `agent-bus invite mint -json` produced, unedited")
}

// Validate checks an invite is usable BEFORE anything is written or dialled.
//
// It is exported because an embedding agent may build an Invite from its own
// secret store rather than from a file, and would otherwise have no way to make
// the same checks the CLI makes. Enrol calls it regardless of how the invite
// arrived, so an embedder cannot skip it by accident.
//
// The bus is authoritative on every one of these (invariant 1): the local checks
// exist so a mistake produces a remedial message here instead of a round trip
// that spends nothing and says little. Expiry is deliberately NOT judged locally
// — that is the bus's decision against its own clock.
func (i *Invite) Validate() error {
	if i == nil {
		return newError(KindInternal, inviteOp, "no invite", "")
	}
	switch {
	case i.InviteID == "":
		return newError(KindConfig, inviteOp,
			"the invite has no invite_id",
			"pass the JSON blob `agent-bus invite mint -json` produced, unedited")
	case len(i.InviteID) > MaxInviteIDLen:
		// The value is NOT echoed: it is oversized by definition, and this
		// mirrors internal/invite/id.go's own refusal to quote it.
		return newError(KindConfig, inviteOp,
			"the invite_id is "+strconv.Itoa(len(i.InviteID))+" bytes, but an invite id is at most "+strconv.Itoa(MaxInviteIDLen),
			"this is not an invite this bus minted; ask the operator for a fresh one")
	case !serverIDPattern.MatchString(i.InviteID):
		// REJECTED, not sanitised — the same judgement validateServerField makes
		// about an id the bus returns, and for a stronger reason here. The invite
		// arrives from OUTSIDE (invariant 11: whoever can substitute this blob
		// points the agent at a bus of their choosing), and this id is then
		// printed by `agent-busctl enrol`, carried in EnrolResult and repeated in
		// every refusal below. A value of
		//
		//	"\x1b[2K\ragent-busctl: verified OK"
		//
		// erases the line it is printed on and writes a fabricated success line
		// in its place. Rewriting it quietly would leave us naming an invite the
		// operator cannot find; refusing says which blob is wrong.
		//
		// serverIDPattern is deliberately the same alphabet the server's ids use:
		// a real invite_id is `inv-26s3mdstb4uag3h2`. The echo is bounded and
		// neutralised, because the whole point is that this value is hostile.
		return newError(KindConfig, inviteOp,
			"the invite_id contains characters an invite id cannot contain: "+safeText(i.InviteID, 60),
			"this is not an invite this bus minted; ask the operator for a fresh one")
	case i.InviteSecret == "":
		return newError(KindConfig, inviteOp,
			"the invite has no invite_secret",
			"pass the JSON blob `agent-bus invite mint -json` produced, unedited — the secret is the credential, and an invite without one cannot be redeemed")
	case len(i.InviteSecret) > MaxSecretLen:
		// Length only. The secret itself never appears in an error.
		return newError(KindConfig, inviteOp,
			"the invite_secret is longer than "+strconv.Itoa(MaxSecretLen)+" bytes",
			"this is not an invite this bus minted; ask the operator for a fresh one")
	case i.BusAddress == "":
		return newError(KindConfig, inviteOp,
			"invite "+i.InviteID+" has no bus_address",
			"the invite is what names the bus; ask the operator to mint one with -bus-address")
	case len(i.BusAddress) > maxBusAddressLen:
		// Length only — see maxBusAddressLen for why the bound is here and not
		// only at the places that print it.
		return newError(KindConfig, inviteOp,
			"invite "+i.InviteID+" has a bus_address of "+strconv.Itoa(len(i.BusAddress))+" bytes, but a bus address is at most "+strconv.Itoa(maxBusAddressLen),
			"this is not an invite this bus minted; ask the operator for a fresh one")
	}

	u, err := i.busURL()
	if err != nil {
		return err
	}
	if i.BusCertFingerprint == "" {
		if u.Scheme == "https" {
			// transportSecurity would refuse this later, but its message names
			// --bus-fingerprint, and with --invite-file the thing that is wrong
			// is the INVITE. Say so here, while the invite is still in hand.
			return newError(KindConfig, inviteOp,
				"invite "+i.InviteID+" names the https bus "+safeText(u.String(), maxDetailBytes)+" but carries no bus_cert_fingerprint",
				"bus certificates are self-signed and there is deliberately no trust-on-first-use: the invite must carry the fingerprint (invariant 11). Ask the operator to mint a fresh invite")
		}
		return nil
	}
	if _, err := i.fingerprint(); err != nil {
		return err
	}
	return nil
}

// busURL returns the bus this invite names, through the same parseBusURL every
// other address goes through — so an invite cannot reach an address a --bus
// could not.
func (i *Invite) busURL() (*url.URL, error) {
	u, err := parseBusURL(i.BusAddress)
	if err != nil {
		// parseBusURL's remedy names --bus / AGENT_BUS_URL, which the caller did
		// not use and cannot fix. Re-point it at the invite.
		var e *Error
		if errors.As(err, &e) {
			// parseBusURL quotes the address it could not use, and the address
			// is INVITE-supplied: MaxInviteFileBytes allows one of ~64 KiB, and
			// a URL is one of the few fields with no alphabet of its own. So the
			// composed message goes through safeText — bounded to
			// maxDetailBytes, controls neutralised — before it reaches a
			// terminal. Same treatment the transport gives a server-supplied
			// error detail, for the same reason.
			return nil, &Error{
				Kind:    KindConfig,
				Op:      inviteOp,
				Message: "invite " + i.InviteID + " names a bus_address this client cannot use: " + safeText(e.Message, maxDetailBytes),
				Remedy:  "ask the operator to mint a fresh invite with a full base URL such as -bus-address https://127.0.0.1:8080",
				Err:     e.Err,
			}
		}
		return nil, err
	}
	return u, nil
}

// fingerprint returns the certificate pin this invite carries, or the zero
// fingerprint when it carries none (legal only for a plaintext loopback bus —
// see Validate).
func (i *Invite) fingerprint() (BusFingerprint, error) {
	if i.BusCertFingerprint == "" {
		return BusFingerprint{}, nil
	}
	fp, err := ParseBusFingerprint(i.BusCertFingerprint)
	if err != nil {
		var e *Error
		if errors.As(err, &e) {
			return BusFingerprint{}, &Error{
				Kind:    KindConfig,
				Op:      inviteOp,
				Message: "invite " + i.InviteID + " carries a bus_cert_fingerprint this client cannot use: " + safeText(e.Message, maxDetailBytes),
				Remedy:  "ask the operator to mint a fresh invite; do NOT substitute a fingerprint from anywhere else — the invite is the trust anchor (invariant 11)",
				Err:     e.Err,
			}
		}
		return BusFingerprint{}, err
	}
	return fp, nil
}
