package relay

import (
	"encoding/json"
	"net/http"

	"github.com/dodgymike/agent-bus/internal/logging"
)

// The response plumbing shared by every bus-to-bus handler in this package —
// the handshake (RELAY-1), the relay ingress and the roster sync (RELAY-2).
//
// They are free functions rather than methods on a base struct because the
// three handlers have genuinely different state and nothing but this plumbing
// in common; embedding a shared struct just to reach two helpers would couple
// their lifecycles for no benefit. Extracting them is what keeps ONE posture in
// ONE place: the stable code goes on the wire, the detail — which quotes
// peer-supplied bytes — stays in our log. Three copies would eventually be
// three postures.

// failJSON logs the detailed failure and writes the stable, non-echoing code.
//
// THE ERROR IS NEVER PUT ON THE WIRE. It quotes peer-supplied bytes (bounded
// and validated, but still the peer's), and there is no reason to hand a
// stranger a description of our parser. The code is enough for a peer operator
// to act on and enough for us to grep for. Callers that want more context in
// the log line pass a logger already decorated with it (Logger.With).
func failJSON(w http.ResponseWriter, log *logging.Logger, status int, code string, err error) {
	log.Warn("peer request rejected",
		"status", status,
		"code", code,
		"err", err.Error(),
	)
	writeJSONBody(w, log, status, ErrorBody{Error: code})
}

// writeJSONBody marshals body and writes it with status.
//
// It marshals BEFORE writing the header, so an encoding failure cannot leave a
// half-written body under a success status — the one failure mode that would
// make a peer parse garbage as an answer.
func writeJSONBody(w http.ResponseWriter, log *logging.Logger, status int, body interface{}) {
	buf, err := json.Marshal(body)
	if err != nil {
		// Every body type in this package is a plain struct of strings, ints
		// and bools, so this cannot fire; if it ever does, do not emit a
		// half-written body.
		log.Error("peer response could not be encoded", "err", err.Error())
		http.Error(w, `{"error":"`+CodeInternal+`"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(buf)
}
