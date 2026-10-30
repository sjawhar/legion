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
	"github.com/sjawhar/envoy/internal/store"
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
// holder itself (roles.ResolveHolder, the resolution the listener's role lane uses) and forwards
// to the holder's agent subject, so nothing of Dispatch's crosses notifications.role.*. Exactly one
// of three holds:
//
//   - skip names why the destination counts as reached with nothing sent: the role has no claim,
//     the claim's holder was gone and its claim is released, or the holder is the event's own actor
//     (the listener's echo skip);
//   - err names why the destination stays pending, backed off with an attempt counted and retried:
//     the claim or the holder's session could not be read, the release failed, the claim churned
//     past the resolution's bound, the claim was kept although its holder is gone, or the holder has
//     not heartbeated within session.ClaimStaleAfter;
//   - otherwise the envelope is forwarded to holder with a Request (sendChains).
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
// (Deps.RoleClaims, Deps.Sessions), releasing a claim whose holder is gone. Events that are not due
// are resolved too: their unreached role destinations hold later events back from the holder's
// agent subject.
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
				// Any registered session is the holder here: one that has stopped heartbeating
				// is found stale by forwardOf, and stays pending rather than losing its claim.
				result.resolution, result.err = roles.ResolveHolder(ctx, deps.RoleClaims, deps.Sessions, role, roles.Registered)
			}
			if result.err == nil && result.resolution.State == roles.Lapsed {
				slog.Info("dispatch outbox: a role's holder is gone", "role", role, "holder", result.resolution.Holder,
					"claim_released", result.resolution.Release == store.ExpiredRoleClaimReleased)
			}
			mu.Lock()
			resolved[role] = result
			mu.Unlock()
		})
	}
	wg.Wait()
	now := time.Now()
	for _, event := range events {
		for index := range event.destinationsOf {
			dest := &event.destinationsOf[index]
			if result, ok := resolved[dest.role]; ok {
				dest.forward = forwardOf(event.item, result.resolution, result.err, now)
			}
		}
	}
}

// forwardOf maps a role's resolution to how item reaches it, as the listener's role lane maps the
// same resolution to its delivery outcomes:
//
//   - no claim (the listener's no_holder): reached, nothing sent;
//   - a claim whose holder's session is gone: the claim is released, and a release that finds a
//     fresh claim in its place resolves the role once more, two reads of the claim in all
//     (roles.ResolveHolder, resolutionAttempts); once released, or found already gone, reached
//     with nothing sent; a claim the store kept (the listener's "holder is not live") and one
//     replaced again by the second read (roles.Churned) stay pending;
//   - a read that failed (the listener's lookup failures): pending;
//   - the holder is item's own actor session (the listener's echo skip): reached, nothing sent;
//   - a holder whose last heartbeat is older than session.ClaimStaleAfter: pending, where the
//     listener's delivery releases the claim; the session is still registered, so the outbox keeps
//     its claim and retries until it heartbeats again or its entry expires and the claim is
//     released;
//   - otherwise a forward to the holder.
//
// The listener's capability check (frameDeliveryMode) does not apply: a Dispatch envelope's payload
// is the event itself, which has no top-level delivery key, so the listener reads it as an untagged
// send that every holder takes.
func forwardOf(item contracts.Envelope, resolution roles.Resolution, err error, now time.Time) *roleForward {
	if err != nil {
		return &roleForward{holder: resolution.Holder, err: fmt.Errorf("resolve the role's holder: %w", err)}
	}
	switch resolution.State {
	case roles.Unclaimed:
		return &roleForward{skip: "the role has no claim"}
	case roles.Lapsed:
		if resolution.Release == store.ExpiredRoleClaimReleased || resolution.Release == store.ExpiredRoleClaimMissing {
			return &roleForward{skip: "the role's holder is gone and its claim released"}
		}
		return &roleForward{holder: resolution.Holder, err: fmt.Errorf("the role's holder %s is gone and its claim was kept", resolution.Holder)}
	case roles.Churned:
		return &roleForward{err: errors.New("the role's claim changed on every read")}
	}
	holder := resolution.Holder
	if item.SourceSession != "" && item.SourceSession == holder {
		return &roleForward{holder: holder, skip: "the role's holder is the event's actor"}
	}
	entry := resolution.Entry
	if !roles.Fresh(entry, now) {
		return &roleForward{holder: holder, err: fmt.Errorf("the role's holder %s has not heartbeated within %s", holder, session.ClaimStaleAfter)}
	}
	return &roleForward{holder: holder, streamAck: entry.Port != 0}
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
