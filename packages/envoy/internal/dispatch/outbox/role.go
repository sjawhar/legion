package outbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/roles"
	"github.com/sjawhar/envoy/internal/session"
)

// roleReceiptTimeout bounds the wait for a role holder's answer to a forward: its empty receipt,
// or the stream's acknowledgement of the stored copy for a holder with a serving port. A holder
// that never answers holds back its own owner's later events for that long and nothing else: the
// batch waiting on it settles and releases every other owner as their own sends come in, and the
// batches beside it read and publish the owners it does not hold (Outbox.scan, lanes).
const roleReceiptTimeout = 5 * time.Second

// errNoRoleReaders is a role route's holder resolved with no role-claim or session reader in Deps.
var errNoRoleReaders = errors.New("the outbox has no role-claim or session reader")

// roleForward is how a role route of one event reaches the role's holder: Dispatch resolves the
// holder itself (roles.ResolveHolder, the one resolution the listener's role routes and its role
// lane use) and forwards to the holder's agent subject, so nothing of Dispatch's crosses
// notifications.role.*. Exactly one of three holds:
//
//   - skip names why the destination counts as reached with nothing sent: the role has no claim,
//     the claim's holder is gone - its own last registration is past the gone horizon - or the
//     holder is the event's own actor (the listener's echo skip);
//   - err names why the destination stays pending, backed off with an attempt counted and
//     retried: the claim or the holder's session could not be read, the holder is unresponsive,
//     or nothing dates the holder at all;
//   - otherwise the envelope is forwarded to holder with a Request (sendChains).
//
// No arm of it takes anything from the holder: a claim stands until the listener's role-claim
// reaper finds its holder gone, so a stalled holder's role is still its own when it answers again
// (dispatch://INFRA-392).
//
// holder is the claim's holder whenever it is known, so the destination's key is the holder's agent
// subject (destination.key) even while it stays pending. streamAck is a holder with a serving port.
type roleForward struct {
	holder    string
	streamAck bool
	skip      string
	err       error
}

// roleResolution is one role's holder as a batch resolved it.
type roleResolution struct {
	resolution roles.Resolution
	err        error
}

// resolveRoleHolders resolves the holder of every role a batch's events route to and have not
// reached yet, once per role and every role at once, within ctx, and gives each such role
// destination its forward (forwardOf). It reads the role and session buckets directly
// (Deps.RoleClaims, Deps.Sessions) and writes to neither: a holder that is not answering keeps
// its claim. Events that are not due are resolved too: their unreached role destinations hold
// later events back from the holder's agent subject.
func resolveRoleHolders(ctx context.Context, deps Deps, events []*batchEvent) {
	var routed []string
	seen := map[string]struct{}{}
	for _, event := range events {
		for _, dest := range event.destinationsOf {
			if _, ok := event.delivered[dest.subject]; ok || dest.role == "" {
				continue
			}
			if _, ok := seen[dest.role]; !ok {
				seen[dest.role] = struct{}{}
				routed = append(routed, dest.role)
			}
		}
	}
	if len(routed) == 0 {
		return
	}
	resolved := make(map[string]roleResolution, len(routed))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, role := range routed {
		wg.Go(func() {
			result := roleResolution{err: errNoRoleReaders}
			if deps.RoleClaims != nil && deps.Sessions != nil {
				result.resolution, result.err = roles.ResolveHolder(ctx, deps.RoleClaims, deps.Sessions, role)
			}
			if result.err == nil && result.resolution.State == roles.Lapsed {
				slog.Info("dispatch outbox: a role's holder is not answering", "role", role,
					"holder", result.resolution.Holder, "holder_state", result.resolution.HolderState,
					"last_seen", result.resolution.LastSeen, "claim_released", false)
			}
			mu.Lock()
			resolved[role] = result
			mu.Unlock()
		})
	}
	wg.Wait()
	for _, event := range events {
		for index := range event.destinationsOf {
			dest := &event.destinationsOf[index]
			if result, ok := resolved[dest.role]; ok {
				dest.forward = forwardOf(event.item, result.resolution, result.err)
			}
		}
	}
}

// forwardOf maps a role's resolution to how item reaches it, as the listener's role lane maps the
// same resolution to its delivery outcomes:
//
//   - no claim (the listener's no_holder): reached, nothing sent;
//   - a holder whose own record is past the gone horizon (session.Gone, dated by that record or
//     by what the lookup remembers of it): reached with nothing sent, and its claim left where it
//     is for the listener's role-claim reaper, which is the one release there is;
//   - a holder past the live window and within that horizon (session.Unresponsive), where the
//     listener's delivery reports delivery_failed: pending, so the event waits for the session to
//     answer again rather than being published to nobody;
//   - a holder nothing dates - no record in the session bucket and no memory of one - pending
//     too: a bucket's maximum age can be shorter than the gone horizon, so a record it no longer
//     holds says the bucket forgot the session, not that the session died;
//   - a read that failed (the listener's lookup failures): pending;
//   - the holder is item's own actor session (the listener's echo skip): reached, nothing sent;
//   - otherwise a forward to the holder.
//
// The listener's capability check (frameDeliveryMode) does not apply: a Dispatch envelope's payload
// is the event itself, which has no top-level delivery key, so the listener reads it as an untagged
// send that every holder takes.
func forwardOf(item contracts.Envelope, resolution roles.Resolution, err error) *roleForward {
	if err != nil {
		return &roleForward{holder: resolution.Holder, err: fmt.Errorf("resolve the role's holder: %w", err)}
	}
	holder := resolution.Holder
	switch resolution.State {
	case roles.Unclaimed:
		return &roleForward{skip: "the role has no claim"}
	case roles.Lapsed:
		if resolution.LastSeen == 0 {
			return &roleForward{holder: holder, err: fmt.Errorf(
				"the role's holder %s has no session record and nothing dates it", holder)}
		}
		if resolution.HolderState == session.Gone {
			return &roleForward{holder: holder, skip: "the role's holder is gone"}
		}
		return &roleForward{holder: holder, err: fmt.Errorf(
			"the role's holder %s is %s: it has not heartbeated since %s",
			holder, resolution.HolderState, time.UnixMilli(resolution.LastSeen).UTC().Format(time.RFC3339))}
	}
	if item.SourceSession != "" && item.SourceSession == holder {
		return &roleForward{holder: holder, skip: "the role's holder is the event's actor"}
	}
	return &roleForward{holder: holder, streamAck: resolution.Entry.Port != 0}
}

// forward sends item, routed to a role destination whose holder forward names, as a listener's
// role forward carries it: its topic the role's, its dedupe key contracts.RoleForwardDedupePrefix
// and the original's, its event id unchanged, so the holder's receipt and dedupe apply as to any
// role forward. It goes to the holder's agent subject with a Request under the forward's MsgId
// (bus.ForwardMsgID), so the stream, which captures agent subjects, stores one copy however often
// it is sent. The answer is the holder's empty receipt for a self-subscribed holder (or one with
// neither a port nor a subscription of its own, which nothing answers, so it stays pending). For a
// holder with a serving port it is the stream's acknowledgement of the stored copy, since a holder
// delivered by its listener from the stream never answers the request itself; that is true once
// the listener delivers to such holders from the stream and acknowledges each stored copy.
func (f *roleForward) send(sender bus.Sender, item contracts.Envelope) <-chan error {
	item.DedupeKey = contracts.RoleForwardDedupePrefix + item.DedupeKey
	subject := contracts.AgentSubject(f.holder)
	return sender.Request(subject, item, bus.RequestOptions{
		MsgID:           bus.ForwardMsgID(item.DedupeKey, subject),
		AcceptStreamAck: f.streamAck,
		Timeout:         roleReceiptTimeout,
	})
}
