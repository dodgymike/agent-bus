package client

import (
	"context"
	"net/http"
)

// routeLeave is the agent self-leave route (AUTH-4). It mirrors
// internal/httpapi.RouteLeave; this package does not import internal/ (invariant
// 7 keeps client/ embeddable), so the path is duplicated as a constant. The two
// are pinned equal by TestClientLeaveEndToEnd (internal/httpapi/composition_test.go),
// which drives this method against the real server — a disagreement surfaces
// there as a 404, since no unit test on either half can compare the constants.
const routeLeave = "/v1/leave"

// leaveResponseBody is the 200 shape of POST /v1/leave, matching
// httpapi.LeaveResponseBody field for field.
type leaveResponseBody struct {
	AgentID         string `json:"agent_id"`
	Left            bool   `json:"left"`
	AlreadyLeft     bool   `json:"already_left"`
	SessionsDropped int    `json:"sessions_dropped"`
}

// LeaveResult reports what Leave did. Its json tags are a documented contract
// surface (CONTRACTS-CLI.md) — the CLI `leave` subcommand emits it verbatim
// under --json.
type LeaveResult struct {
	// AgentID is the agent that left — the identity this client acted as.
	AgentID string `json:"agent_id"`

	// ServerNotified is true when the bus accepted the departure. Unlike
	// LogoutResult.ServerNotified (which is always false — logout is local only),
	// this is the whole point of leave: the roster tombstone is durable.
	ServerNotified bool `json:"server_notified"`

	// AlreadyLeft is true when the bus reported the agent was already gone — an
	// idempotent retry (invariant 10).
	AlreadyLeft bool `json:"already_left"`

	// SessionsDropped is how many live sessions the bus dropped for the agent.
	SessionsDropped int `json:"sessions_dropped"`

	// LocallyRemoved lists the agent ids whose credentials were deleted from the
	// local store after the bus was told. A left identity can never authenticate
	// again, so its local key material is destroyed — the same destruction
	// `logout` performs, but only AFTER the bus has confirmed the departure.
	LocallyRemoved []string `json:"locally_removed"`

	// Current is the identity selected locally afterwards, or "" when none
	// remains — matching LogoutResult.Current.
	Current string `json:"current_agent_id"`
}

// Leave tells the BUS to durably remove the current identity from the roster
// (POST /v1/leave), then destroys that identity's local credential.
//
// It is the server-side counterpart to Logout, which is local only. The order is
// load-bearing: the bus is told FIRST, and the local credential is deleted only
// after a 2xx. If the bus call fails, nothing local is destroyed, so the caller
// can retry — POST /v1/leave is idempotent (invariant 10), and repeating it is
// safe.
//
// A restart of the bus does not undo a leave: the removal is a durable tombstone
// (invariants 4, 6), and the agent id is never re-issued (invariant 1) — a fresh
// enrolment under the same name gets a new server-minted id.
func (c *Client) Leave(ctx context.Context) (LeaveResult, error) {
	const op = "leave"

	// The identity we are about to leave — read BEFORE the request so the result
	// can name it even on the already-left path, and so the local removal targets
	// exactly this id.
	id, err := c.Identity()
	if err != nil {
		return LeaveResult{}, err
	}

	ctx, cancel := c.contextWithTimeout(ctx)
	defer cancel()

	var body leaveResponseBody
	if _, err := c.authorizedRequest(ctx, request{
		method: http.MethodPost,
		path:   routeLeave,
		op:     op,
		out:    &body,
		// SAFE TO REPEAT: POST /v1/leave is idempotent server-side — a second
		// leave of an already-departed agent returns 200 with already_left=true
		// and writes nothing (invariant 10). do() replays the same empty body.
		retryable: true,
	}); err != nil {
		return LeaveResult{}, err
	}

	// The bus has confirmed the departure. NOW destroy the local credential and
	// any persisted session for this identity, exactly as Logout does — a left
	// identity can never authenticate again, so its key material is useless and
	// keeping it invites confusion.
	removed, current, rerr := c.store.Remove(id.AgentID)
	if rerr != nil {
		// The bus WAS told and the departure is durable; only the local cleanup
		// failed. Report success on the server half and surface the local error so
		// the caller can delete the key by hand.
		return LeaveResult{
			AgentID:         body.AgentID,
			ServerNotified:  true,
			AlreadyLeft:     body.AlreadyLeft,
			SessionsDropped: body.SessionsDropped,
		}, rerr
	}
	c.forgetPersistedSessions(removed)
	c.forgetIdentity()

	agentID := body.AgentID
	if agentID == "" {
		agentID = id.AgentID
	}
	return LeaveResult{
		AgentID:         agentID,
		ServerNotified:  true,
		AlreadyLeft:     body.AlreadyLeft,
		SessionsDropped: body.SessionsDropped,
		LocallyRemoved:  removed,
		Current:         current,
	}, nil
}
