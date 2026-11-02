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
// of four holds:
//
//   - keptFor names a role no live holder can take, whose copy is kept on that role's kept lane
//     for the holder that claims it next (keptOrSkipped, send), and is empty for every other
//     forward;
//   - skip names why the destination counts as reached with nothing sent: the holder is the
//     event's own actor (the listener's echo skip), or the role token is one no claim can name,
//     which is kept for nobody;
//   - err names why the destination stays pending, backed off with an attempt counted and retried:
//     the claim or the holder's session could not be read, the release failed, the claim churned
//     past the resolution's bound, the claim was kept although its holder is gone, or the holder has
//     not heartbeated within session.ClaimStaleAfter;
//   - otherwise the envelope is forwarded to holder with a Request (sendChains).
//
// holder is the claim's holder whenever it is known, so the destination's key is the holder's agent
// subject (destination.key) even while it stays pending; a keep carries none, since it reaches no
// session. streamAck is a holder with a serving port.
type roleForward struct {
	holder    string
	keptFor   string
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
				dest.forward = forwardOf(dest.role, event.item, result.resolution, result.err, now)
			}
		}
	}
}

// forwardOf maps a role's resolution to how item reaches it, as the listener's role lane maps the
// same resolution to its delivery outcomes:
//
//   - no claim (the listener's no_holder): kept on the role's kept lane for its next holder;
//   - a claim whose holder's session is gone: the claim is released, and a release that finds a
//     fresh claim in its place resolves the role once more, two reads of the claim in all
//     (roles.ResolveHolder, resolutionAttempts); once released, or found already gone, kept as an
//     unclaimed role is; a claim the store kept (the listener's "holder is not live") and one
//     replaced again by the second read (roles.Churned) stay pending;
//   - a read that failed (the listener's lookup failures): pending;
//   - the holder is item's own actor session (the listener's echo skip): reached, nothing sent;
//   - a holder whose last heartbeat is older than session.ClaimStaleAfter: pending, where the
//     listener's delivery releases the claim; the session is still registered, so the outbox keeps
//     its claim and retries until it heartbeats again or its entry expires and the claim is
//     released;
//   - otherwise a forward to the holder.
//
// A claim the restart grace retained is the one no-holder case that is not kept: that holder
// keeps its claim and writes no new one, so a copy kept for it would wait for a claim that never
// comes. It stays the pending error above.
//
// The listener's capability check (frameDeliveryMode) does not apply: a Dispatch envelope's payload
// is the event itself, which has no top-level delivery key, so the listener reads it as an untagged
// send that every holder takes.
func forwardOf(role string, item contracts.Envelope, resolution roles.Resolution, err error, now time.Time) *roleForward {
	if err != nil {
		return &roleForward{holder: resolution.Holder, err: fmt.Errorf("resolve the role's holder: %w", err)}
	}
	switch resolution.State {
	case roles.Unclaimed:
		return keptOrSkipped(role, "the role has no claim")
	case roles.Lapsed:
		if resolution.Release == store.ExpiredRoleClaimReleased || resolution.Release == store.ExpiredRoleClaimMissing {
			return keptOrSkipped(role, "the role's holder is gone and its claim released")
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

// keptOrSkipped is how a role route no live holder can take reaches the role's next holder: a
// keep of the copy on the role's kept lane, which the holder that claims the role next is handed
// (cmd/listener/role_keep.go), and which a holder that claimed the role while the keep was in
// flight is handed by the keep itself (handOverKept). The keep carries no holder, so the route is
// keyed on its role topic as the skip it replaced was, and holds back no session's later events.
//
// One role token is not kept for and skips as it did before the kept lane, under skipped: one no
// claim can ever name (roles.Valid, the pattern POST /v1/roles/set refuses), since a copy kept
// under such a token waits for a claim that cannot be made and expires unread. That is the rule
// the listener's own keep follows (keepForNextHolder), which the two writers of a role's kept
// lane must agree on.
func keptOrSkipped(role, skipped string) *roleForward {
	if !roles.Valid(role) {
		return &roleForward{skip: skipped}
	}
	return &roleForward{keptFor: role}
}

// send sends item, routed to a role destination, as forward names, within ctx.
//
// A keep stores it on the role's kept lane and hands it on to a holder that claimed the role
// under the keep (keep, handOverKept).
//
// A forward goes to the holder as a listener's role forward carries it: its topic the role's, its
// dedupe key contracts.RoleForwardDedupePrefix and the original's, its event id unchanged, so the
// holder's receipt and dedupe apply as to any role forward. It goes to the holder's agent subject
// with a Request under the forward's MsgId (bus.ForwardMsgID), so the stream, which captures agent
// subjects, stores one copy however often it is sent. The answer is the holder's empty receipt for
// a self-subscribed holder (or one with neither a port nor a subscription of its own, which nothing
// answers, so it stays pending). For a holder with a serving port it is the stream's
// acknowledgement of the stored copy, since a holder delivered by its listener from the stream
// never answers the request itself; that is true once the listener delivers to such holders from
// the stream and acknowledges each stored copy.
func (f *roleForward) send(ctx context.Context, deps Deps, sender bus.Sender, item contracts.Envelope) <-chan error {
	if f.keptFor != "" {
		return f.keep(ctx, deps, sender, item)
	}
	item.DedupeKey = contracts.RoleForwardDedupePrefix + item.DedupeKey
	subject := contracts.AgentSubject(f.holder)
	return sender.Request(subject, item, bus.RequestOptions{
		MsgID:           bus.ForwardMsgID(item.DedupeKey, subject),
		AcceptStreamAck: f.streamAck,
		Timeout:         roleReceiptTimeout,
	})
}

// keep stores item on the role's kept lane (bus.KeptRoleSubject) with one JetStream publish,
// under the id every writer of that lane keeps this message under (bus.KeptMsgID, the dedupe key
// and the role), so the stream holds one copy of it however often the outbox retries and whether
// the listener kept a copy of it too. The envelope is stored as it is, its topic the role's and
// its dedupe key the original's, which is what the listener's own keep stores and what the
// drain's forward to the next holder reads (keepRoleMessage, walkKeptRole). The stream's
// acknowledgement is the answer: acknowledged, the route is reached; anything else leaves the
// item pending for the outbox's own retry, which the MsgId makes idempotent at the stream.
//
// Acknowledged, the copy is handed to a holder that claimed the role while the keep was in
// flight, before that answer is given (handOverKept), so the route's own answer stays the keep's
// and the handover is behind it. The publish is made on the batch's own goroutine, which issues
// every send of a window without waiting for any of them (sendChains), so the wait for the
// acknowledgement and the handover behind it run on a goroutine of their own.
func (f *roleForward) keep(ctx context.Context, deps Deps, sender bus.Sender, item contracts.Envelope) <-chan error {
	stored := sender.PublishStoredAsync(bus.KeptRoleSubject(f.keptFor), item, bus.KeptMsgID(item.DedupeKey, f.keptFor))
	answer := make(chan error, 1)
	go func() {
		err := <-stored
		if err == nil {
			handOverKept(ctx, deps, sender, f.keptFor, item)
		}
		answer <- err
	}()
	return answer
}

// handOverKept hands the copy just kept on role's kept lane to the session holding the role now,
// when one claimed it while the keep was in flight.
//
// The batch resolves a role's holder as it opens (resolveRoleHolders) and publishes the keep at
// its end (sendChains), and a claim that lands between the two leaves the copy on a lane with a
// live holder and nothing draining it: that claim's own drain read the lane before the copy was
// on it and ended (cmd/listener/role_keep.go, drainKeptRole), and nothing walks it again until
// the listener's sweep, up to its interval later (sweepKeptRoleLanes), while the issue's next
// event resolves that holder live and is forwarded straight to its agent subject. The holder
// would be handed event N + 1 minutes before event N. The listener closes the same race at its
// own keep, by starting a handover after every one of them (keepForNextHolder); the outbox is
// that lane's second writer and has no drain of its own, so it reads the claim again - it holds
// the role readers already - and forwards the copy itself, under the id the drain's forward of it
// would carry (bus.ForwardMsgID of the role-forward dedupe key and the holder's agent subject,
// roleForward.msgID in cmd/listener/role_lane.go). Whichever of the two copies arrives second is
// collapsed by the stream's duplicate window and by the holder's own dedupe key, so the holder
// takes exactly one, in order, without waiting for the sweep. It needs no NATS grant the outbox
// does not hold, no change to the lane's cursor and no second publisher.
//
// The copy stays on the lane as it is: the cursor and the drain stay the authority for a holder
// that claims later.
//
// The handover is made before the keep's answer rather than beside it, so that the holder's later
// events - which the batch after this one forwards to its agent subject, once this owner is
// settled and given back (publishBatch, lanes) - follow it, which a handover of its own would
// race. What it costs the route is one claim read, one session read and one forward, and only
// for a keep whose role is claimed. A keep is keyed on its role topic, no holder being known when
// the batch planned its chains (destination.key), so an event of the same owner that this batch
// sends to that session on a key of its own - an author or follower route among them - races the
// handover as any two keys of one owner do; before it, the kept copy waited for the sweep and
// lost to every such event.
//
// The resolution is the route's own (forwardOf), so a holder that is the event's actor, one that
// has stopped heartbeating, a claim that lapsed again and a role still unclaimed are each read
// here as they are read there, and none of them is forwarded to. A handover that failed is
// logged and nothing else: the route was reached when the stream acknowledged the keep, and the
// copy on the lane is what the role's next claim, keep or sweep hands over.
func handOverKept(ctx context.Context, deps Deps, sender bus.Sender, role string, item contracts.Envelope) {
	if deps.RoleClaims == nil || deps.Sessions == nil {
		return
	}
	resolution, err := roles.ResolveHolder(ctx, deps.RoleClaims, deps.Sessions, role, roles.Registered)
	handover := forwardOf(role, item, resolution, err, time.Now())
	if handover.holder == "" || handover.keptFor != "" || handover.skip != "" || handover.err != nil {
		return
	}
	if err := <-handover.send(ctx, deps, sender, item); err != nil {
		slog.Warn("dispatch outbox: the copy kept for a role did not reach the holder that claimed it",
			"role", role, "holder", handover.holder, "event_id", item.EventID, "error", err)
	}
}
