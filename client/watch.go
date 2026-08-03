package client

import (
	"context"
	"strconv"
	"time"
)

// WatchOptions configures Watch.
type WatchOptions struct {
	// Cursor is an explicit start position. It OVERRIDES anything persisted.
	//
	// It may be combined with Persist: starting somewhere specific and then
	// persisting forward from there is a legitimate thing to want, and refusing
	// the combination would mean a caller resuming from a cursor it printed
	// earlier could never re-enter the normal persisted flow.
	Cursor string

	// Replay ignores the persisted cursor and starts at position 0 — the whole
	// retained window (1 day / 1 GiB, whichever binds first). It does not clear
	// the persisted cursor; with Persist set, the position is overwritten as the
	// replay advances past it.
	Replay bool

	// Limit caps each batch, 1..MaxBatchLimit. 0 lets the bus choose.
	Limit int

	// PollTimeout is how long each long poll parks. 0 means DefaultPollTimeout.
	// Above MaxPollTimeout is refused, not clamped.
	PollTimeout time.Duration

	// Persist stores the cursor between batches through the credential store's
	// cursor file, so a restarted watch resumes where it left off.
	Persist bool

	// Max stops cleanly after this many messages have been handed to handle.
	// 0 is unbounded.
	Max int

	// For stops cleanly after this much wall-clock time. 0 is unbounded. It is
	// applied as a context DEADLINE, so an in-flight poll is cut short rather
	// than overrunning by up to a poll timeout.
	For time.Duration

	// OnPoll is an optional heartbeat, called after EVERY successful poll —
	// including a timed-out one, which is the whole point: it is how a caller
	// shows that a quiet bus is quiet rather than broken. May be nil.
	//
	// It is called BEFORE the batch's messages are handed to handle, and must
	// not block for long: the watch loop is stopped while it runs.
	OnPoll func(Batch)

	// OnRetry is called before each backoff after a TRANSIENT failure, with the
	// error and the delay about to be slept. May be nil.
	//
	// It exists so the CLI can print a one-line stderr notice ("bus
	// unreachable, retrying in 1.2s") without this package deciding to log
	// anything itself. A watch never gives up on a transient failure, so without
	// this hook a bus outage would be completely silent.
	OnRetry func(error, time.Duration)
}

// WatchStats is what a finished watch reports.
type WatchStats struct {
	// Delivered is how many messages were handed to handle and accepted.
	Delivered int `json:"delivered"`

	// Polls is how many successful polls were made, including timed-out ones.
	Polls int `json:"polls"`

	// Cursor is the position reached: the one to resume from. It is the
	// position of the last batch every message of which was accepted by handle,
	// never further.
	Cursor string `json:"cursor"`
}

// Watch long-polls the bus and hands each message to handle until the caller
// stops it.
//
// # The cursor advances ONLY after the caller has been handed the messages
//
// This is the load-bearing property of the whole loop, and the loop's shape
// exists to guarantee it:
//
//	poll → hand EVERY message in the batch to handle → only if every one of
//	them returned nil, adopt the batch's returned cursor and (with Persist)
//	write it durably.
//
// If handle returns an error the watch STOPS and returns it WITHOUT advancing
// or persisting the cursor, so the batch — all of it, including the messages
// handle already accepted — is delivered again next time. If the process is
// killed mid-batch, or after handle returned but before the cursor reached
// disk, the same thing happens: THE WHOLE BATCH IS RE-DELIVERED.
//
// That is deliberate, and it is the only safe direction. Delivery on this bus is
// AT-LEAST-ONCE (CONTRACTS-HTTP.md says so plainly), and the recipient-side
// cursor is the freshness half of the replay defence in invariant 10. Advancing
// the cursor before the caller has the messages would convert at-least-once into
// at-most-once and silently DROP messages on any crash; advancing it after
// re-delivers them. Re-deliver, never skip.
//
// The consequence lands on the agent author, so it is stated bluntly: A HANDLER
// MUST BE IDEMPOTENT, KEYED ON Message.MessageID. A handler that appends to a
// file, posts to an API or spends money will do it twice on the unlucky restart,
// and no amount of care in this loop can prevent that — the acknowledgement
// window between "handle returned" and "cursor is durable" cannot be closed from
// this side of the wire.
//
// # What stops the watch, and what does not
//
// Stops CLEANLY, returning the stats gathered so far and a NIL error:
//   - ctx is cancelled (a Ctrl-C is the successful end of a tail, not a failure)
//   - Max messages have been delivered
//   - For has elapsed
//
// Stops with an ERROR:
//   - handle returned one (returned verbatim, cursor not advanced)
//   - the bus is fatally unavailable — IsFatalUnavailable, i.e. a 503 with no
//     Retry-After, the non-durable or poisoned hub. An operator must see that;
//     backing off forever on it turns a visible fault into a silent one.
//   - KindUsage, KindConfig, KindRejected, or a KindAuth that survived the
//     transparent re-handshake authorizedRequest already performs. None of
//     those improve by being retried, and looping on an auth failure looks like
//     credential guessing.
//   - the cursor could not be persisted (see below)
//
// Does NOT stop it:
//   - a timed-out poll. It is a 200 and it is the steady state of a quiet bus:
//     OnPoll is called, nothing is logged as an error, and the next poll starts
//     immediately — the poll itself WAS the wait, so sleeping after it would
//     halve the responsiveness for nothing.
//   - a transient failure: KindNetwork, or a non-fatal KindServer including a
//     503 with Retry-After (whose hinted delay is honoured). These back off with
//     the same jittered schedule the transport uses and retry INDEFINITELY,
//     with no cap on consecutive failures. That is on purpose: a watch that
//     dies because the bus was restarted is a watch nobody can rely on. Each
//     failure is reported through OnRetry so the caller can say something.
//
// A failure to PERSIST the cursor is returned rather than swallowed. A watch
// that cannot write its position still works, but its recorded position drifts
// silently further behind the real one until a restart replays everything since
// the last successful write — an operator needs to know their store is broken
// while it is still one batch behind.
//
// Session refresh and re-authentication are invisible: every poll goes through
// authorizedRequest, which refreshes an expiring session and re-handshakes once
// after a bus restart. Nothing here re-implements it.
func (c *Client) Watch(ctx context.Context, opts WatchOptions, handle func(Message) error) (WatchStats, error) {
	const op = "watch"

	var stats WatchStats

	if handle == nil {
		return stats, newError(KindInternal, op, "Watch was called with no handler", "")
	}
	if opts.Limit < 0 || opts.Limit > MaxBatchLimit {
		// Validate once, here, rather than discovering it on the first poll:
		// the same check inside Read would be reported as a read failure after
		// a session handshake had already been paid for.
		return stats, usagef(op, "use a limit between 1 and "+strconv.Itoa(MaxBatchLimit)+", or 0 to let the bus choose",
			"limit %d is not between 0 and %d", opts.Limit, MaxBatchLimit)
	}
	poll := opts.PollTimeout
	if poll == 0 {
		poll = DefaultPollTimeout
	}
	if _, err := pollTimeoutSeconds(op, poll); err != nil {
		return stats, err
	}
	if opts.Max < 0 {
		return stats, usagef(op, "use a positive message count, or 0 for an unbounded watch", "max %d is negative", opts.Max)
	}
	if opts.For < 0 {
		return stats, usagef(op, "use a positive duration, or 0 for an unbounded watch", "duration %s is negative", opts.For)
	}

	if ctx == nil {
		ctx = context.Background()
	}
	if opts.For > 0 {
		// A DEADLINE rather than a timer checked between polls: the in-flight
		// poll must be cut short when the time is up, or a `--for 5s` watch with
		// a 30s poll would sit there for another 25 seconds after it was
		// supposed to have finished.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.For)
		defer cancel()
	}

	// The persisted cursor is scoped to (agent id, resolved bus URL) — see
	// cursorRecord for why both halves are needed. Resolve them only when they
	// are actually wanted, so a caller that supplied an explicit cursor and does
	// not persist never touches the store.
	var agentID, busURL string
	needsStore := opts.Persist || (opts.Cursor == "" && !opts.Replay)
	if needsStore {
		cred, err := c.credential()
		if err != nil {
			return stats, err
		}
		base, err := c.resolveBusURL()
		if err != nil {
			return stats, err
		}
		agentID, busURL = cred.AgentID, base.String()
	}

	cursor := opts.Cursor
	if cursor == "" && !opts.Replay && needsStore {
		stored, err := c.store.Cursor(agentID, busURL)
		if err != nil {
			return stats, err
		}
		cursor = stored
	}
	stats.Cursor = cursor

	failures := 0
	for {
		if ctx.Err() != nil {
			return stats, nil
		}

		batch, err := c.Read(ctx, ReadOptions{
			Cursor: cursor,
			Limit:  batchLimit(opts.Limit, opts.Max, stats.Delivered),
			Wait:   poll,
		})
		if err != nil {
			// Our own deadline or cancellation, surfacing as a transport
			// failure. It is a clean stop, not something to retry.
			if ctx.Err() != nil {
				return stats, nil
			}
			if !watchShouldRetry(err) {
				return stats, err
			}
			failures++
			if failures > maxBackoffAttempt {
				failures = maxBackoffAttempt
			}
			delay := c.backoff(failures, err)
			if opts.OnRetry != nil {
				opts.OnRetry(err, delay)
			}
			if serr := c.sleep(ctx, delay); serr != nil {
				// sleep only fails when ctx ended: a clean stop.
				return stats, nil
			}
			continue
		}
		failures = 0
		stats.Polls++
		if opts.OnPoll != nil {
			opts.OnPoll(batch)
		}

		// Hand over EVERY message before the cursor moves. An error here stops
		// the watch with the cursor exactly where it was, so the whole batch is
		// re-delivered. See the doc comment.
		for _, m := range batch.Messages {
			if herr := handle(m); herr != nil {
				return stats, herr
			}
			stats.Delivered++
		}

		// Adopt the new position only now. An empty or timed-out batch returns
		// the cursor unchanged, so this is also the no-op it looks like on a
		// quiet bus — and the equality check keeps a 30-second poll loop from
		// rewriting the cursor file for ever.
		if batch.Cursor != "" && batch.Cursor != cursor {
			cursor = batch.Cursor
			stats.Cursor = cursor
			if opts.Persist {
				if perr := c.store.SetCursor(agentID, busURL, cursor); perr != nil {
					return stats, perr
				}
			}
		}

		if opts.Max > 0 && stats.Delivered >= opts.Max {
			return stats, nil
		}

		// NEVER BUSY-LOOP. A timed-out poll needs no sleep — the poll WAS the
		// wait — and a batch that delivered something should be followed
		// immediately (More says there is more waiting). What is left is the
		// case that should not happen: an empty batch that returned promptly
		// without the timeout flag. Backing off there costs nothing when it
		// never occurs and stops a bus that answers instantly with nothing from
		// spinning a core.
		if len(batch.Messages) == 0 && !batch.TimedOut {
			failures++
			if failures > maxBackoffAttempt {
				failures = maxBackoffAttempt
			}
			if serr := c.sleep(ctx, c.backoff(failures, nil)); serr != nil {
				return stats, nil
			}
		}
	}
}

// maxBackoffAttempt caps the exponent fed to backoff. The window is already
// capped at Retry.MaxDelay, so this only keeps the shift itself sane over a long
// outage.
const maxBackoffAttempt = 16

// batchLimit chooses the limit for the next poll.
//
// When Max is set, the batch is capped at the number of messages still wanted,
// so Max can only ever be reached at a batch BOUNDARY. That matters for
// correctness, not just tidiness: stopping mid-batch would mean adopting a
// cursor that covers messages the caller was never handed — the one thing this
// loop must not do — or discarding the batch's position entirely.
func batchLimit(limit, max, delivered int) int {
	if max > 0 {
		remaining := max - delivered
		if remaining < 1 {
			remaining = 1
		}
		if limit == 0 || remaining < limit {
			limit = remaining
		}
	}
	if limit > MaxBatchLimit {
		limit = MaxBatchLimit
	}
	return limit
}

// watchShouldRetry reports whether a failed poll is worth retrying.
//
// The two families that are: a transport failure (KindNetwork) and a bus that
// reported a transient problem about itself (KindServer — a 5xx, or a capacity
// 503 that carried Retry-After). Everything else is either the caller's mistake
// or a condition that will not change by being asked again.
//
// IsFatalUnavailable is checked FIRST and separately, because it is KindServer
// too: a 503 with no Retry-After is the bus saying its write path is not durable,
// which is precisely the KindServer that must not be retried.
func watchShouldRetry(err error) bool {
	if IsFatalUnavailable(err) {
		return false
	}
	switch KindOf(err) {
	case KindNetwork, KindServer:
		return true
	default:
		// KindUsage, KindConfig, KindRejected, KindAuth (which already survived
		// authorizedRequest's one re-handshake), KindInternal.
		return false
	}
}
