package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/roles"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
)

// roleForwardAckTimeout bounds the wait for the stream's acknowledgement of a role forward's
// stored copy, which is what counts the forward as reached: no holder answers a forward. A
// forward the stream does not acknowledge holds back its own owner's later events for that long
// and nothing else: the batch waiting on it settles and releases every other owner as their own
// sends come in, and the batches beside it read and publish the owners it does not hold
// (Outbox.scan, lanes).
const roleForwardAckTimeout = 5 * time.Second

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
// session.
type roleForward struct {
	holder  string
	keptFor string
	skip    string
	err     error
}

// roleResolution is one role's holder as a batch resolved it.
type roleResolution struct {
	resolution roles.Resolution
	err        error
}

// resolveRole resolves role's holder as the outbox reads it, within ctx: the role and session
// buckets directly (Deps.RoleClaims, Deps.Sessions), any registered session its holder, and a
// claim whose holder is gone released and that release logged. It is the one resolution a batch's
// open (resolveRoleHolders) and the handover behind a keep (handOverKept) share, so the liveness
// rule and that line are written once.
func resolveRole(ctx context.Context, deps Deps, role string) (roles.Resolution, error) {
	if deps.RoleClaims == nil || deps.Sessions == nil {
		return roles.Resolution{}, errNoRoleReaders
	}
	// Any registered session is the holder here: one that has stopped heartbeating is found
	// stale by forwardOf, and stays pending rather than losing its claim.
	resolution, err := roles.ResolveHolder(ctx, deps.RoleClaims, deps.Sessions, role, roles.Registered)
	if err == nil && resolution.State == roles.Lapsed {
		slog.Info("dispatch outbox: a role's holder is gone", "role", role, "holder", resolution.Holder,
			"claim_released", resolution.Release == store.ExpiredRoleClaimReleased)
	}
	return resolution, err
}

// resolveRoleHolders resolves the holder of every role a batch's events route to and have not
// reached yet, once per role and every role at once, within ctx (resolveRole), and gives each
// such role destination its forward (forwardOf). Events that are not due are resolved too: their
// unreached role destinations hold later events back from the holder's agent subject.
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
			var result roleResolution
			result.resolution, result.err = resolveRole(ctx, deps, role)
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
//   - otherwise a forward to the holder, stored on the holder's agent subject, which the stream
//     captures, under its own Nats-Msg-Id (send), so one copy is stored however often it is
//     sent; the stream's acknowledgement of that copy is what counts the forward as having
//     reached the holder, for every holder alike, because every holder is delivered its copy
//     from its own consumer by the listener's relay and answers the forward with nothing.
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
	return &roleForward{holder: holder}
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

// send is the router of a role destination: it sends item, routed to that destination, as forward
// names, within ctx. handed is this batch's record of what its own handovers have already
// delivered to a role's holder, which a keep's handover reads and writes (keptHandovers,
// sendChains).
//
// A keep stores it on the role's kept lane and hands it on to a holder that claimed the role
// under the keep (keep, handOverKept). Everything else is forwarded to the holder (forward).
func (f *roleForward) send(ctx context.Context, deps Deps, sender bus.Sender, item contracts.Envelope, handed *keptHandovers) <-chan error {
	if f.keptFor != "" {
		return f.keep(ctx, deps, sender, item, handed)
	}
	return f.forward(sender, item)
}

// forward is the one request a role destination makes of its holder's agent subject, which the
// route's own send makes and the handover behind a keep makes again for the copy it hands over
// (send, handOverKeptCopy).
//
// It goes to the holder as a listener's role forward carries it: its topic the role's, its
// dedupe key contracts.RoleForwardDedupePrefix and the original's, its event id unchanged, so the
// holder's dedupe applies as to any role forward. It goes to the holder's agent subject with a
// Request under the forward's MsgId (bus.ForwardMsgID), so the stream, which captures agent
// subjects, stores one copy however often it is sent. The stream's acknowledgement of that copy
// is the answer, and what counts the forward as having reached the holder, for every holder
// alike: a holder is delivered its stored copy from its own consumer by the listener's relay and
// answers the request with nothing (INFRA-771).
func (f *roleForward) forward(sender bus.Sender, item contracts.Envelope) <-chan error {
	item.DedupeKey = contracts.RoleForwardDedupePrefix + item.DedupeKey
	subject := contracts.AgentSubject(f.holder)
	return sender.Request(subject, item, bus.RequestOptions{
		MsgID:   bus.ForwardMsgID(item.DedupeKey, subject),
		Timeout: roleForwardAckTimeout,
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
// and the handover is behind it; the handover is given the time of this publish, which is the
// copy's own age and what its forward carries (contracts.Envelope.KeptAt). The publish is made on
// the batch's own goroutine, which issues every send of a window without waiting for any of them
// (sendChains), so the wait for the acknowledgement and the handover behind it run on a goroutine
// of their own, and the record of what those handovers delivered is shared between them (handed,
// keptHandovers).
func (f *roleForward) keep(ctx context.Context, deps Deps, sender bus.Sender, item contracts.Envelope, handed *keptHandovers) <-chan error {
	keptAt := time.Now()
	stored := sender.PublishStoredAsync(bus.KeptRoleSubject(f.keptFor), item, bus.KeptMsgID(item.DedupeKey, f.keptFor))
	answer := make(chan error, 1)
	go func() {
		err := <-stored
		if err == nil {
			handOverKept(ctx, deps, sender, f.keptFor, item, keptAt, handed)
		}
		answer <- err
	}()
	return answer
}

// handOverKept hands the copy just kept on role's kept lane to the session holding the role now,
// when one claimed it while the keep was in flight and that copy is the one the role's drain
// would hand over next.
//
// The batch resolves a role's holder as it opens (resolveRoleHolders) and publishes the keep at
// its end (sendChains), and a claim that lands between the two leaves the copy on a lane with a
// live holder and nothing draining it: that claim's own drain read the lane before the copy was
// on it and ended (cmd/listener/role_keep.go, drainKeptRole), and nothing walks it again until
// the listener's sweep, up to its interval later (sweepKeptRoleLanes), while the issue's next
// event resolves that holder live and is forwarded straight to its agent subject. The holder
// would be handed event N + 1 minutes before event N. The listener closes the same race at its
// own keep, by starting a handover after every one of them (keepForNextHolder); the outbox is
// that lane's second writer and has no drain of its own, so it reads the claim and that lane
// again - it holds the role readers already - and forwards the copy itself, under the id the
// drain's forward of it would carry (bus.ForwardMsgID of the role-forward dedupe key and the
// holder's agent subject, roleForward.msgID in cmd/listener/role_lane.go). Whichever of the two
// copies arrives second is collapsed by the stream's duplicate window and by the holder's own
// dedupe key, so the holder takes exactly one, in order, without waiting for the sweep. It needs
// no NATS grant the outbox does not hold, no change to the lane's cursor and no second publisher.
//
// The reads are deps.KeptLanes, the bus client cmd/dispatch passes: a Deps with none makes no
// handover at all, and its kept copy waits for the role's next claim, keep or sweep, as a Deps
// with no role reader resolves no holder (errNoRoleReaders).
//
// The resolution is made first and is the route's own (resolveRole, forwardOf), so a holder that
// is the event's actor, one that has stopped heartbeating, a claim that lapsed again and a role
// still unclaimed are each read here as they are read there, and none of them is forwarded to:
// the copy waits on the lane for the role's next claim, keep or sweep, as it did before the
// handover, nothing is logged for it, and nothing is recorded as handed over, so the batch's next
// copy of that role finds this one in front of it and declines, as it should. A copy is kept only
// for a role no live holder can take, so most keeps end on that one claim read, and a claim whose
// holder is gone is released here as the route's own resolution releases it.
//
// The copy is handed over only when it is the first on the lane past the role's drain cursor and
// past what this batch has handed to that same holder, with no walk's claim outstanding there
// (keptCopyLeadsTheLane), because a drain sends a lane strictly in order from its cursor on: a
// handover made while older copies of the role are still on the lane would overtake them, and
// since a role token names one tree role of one issue those copies are that issue's own, so its
// holder would take a later event of it before its earlier ones. Leaving such a copy to the
// drain costs nothing: the claim that landed under the keep starts a drain of its own, which
// finds the copies before it and this one behind them and sends the lot in order, so nothing on
// that lane waits for the sweep either. A lane the copy just kept is alone on is the race above,
// where that claim's drain read the lane empty, and the copy is then the first past the cursor,
// so the handover fires exactly as it did without the guard. The outbox grows no second drain for
// this: the cursor, the claim and the ordering stay the listener's to own, and two writers of
// them are how the two would drift apart.
//
// The copy stays on the lane as it is and no cursor is written for it: the cursor and the drain
// stay the authority for a holder that claims later, which is why what this batch's own handovers
// delivered, and to which holder, is recorded (handed, keptHandovers, whose doc holds why).
//
// The handover is made before the keep's answer rather than beside it, so that the holder's later
// events - which the batch after this one forwards to its agent subject, once this owner is
// settled and given back (publishBatch, lanes) - follow it, which a handover of its own would
// race. What it costs every acknowledged keep is the role's claim read; a session read, the
// lane's two reads and a forward follow only for a keep whose role a session claimed meanwhile.
// A keep is keyed on its role topic, no holder being known when the batch planned its chains
// (destination.key), so an event of the same owner that this batch sends to that session on a key
// of its own - an author or follower route among them - races the handover as any two keys of one
// owner do; before it, the kept copy waited for the sweep and lost to every such event.
//
// A read that failed - the lane, its cursor, the claim or the holder's session - ends the
// handover at Warn on the one line this writes, and so does a forward that failed; a read this
// process's own stop cancelled is not one of them (lostToContextEnd), the batch saying that
// itself. Nothing follows such a failure: the route was reached when the stream acknowledged the
// keep, so the event is published and nothing retries the handover, and it is not counted among
// the destinations a publish did not reach (countPublishFailure), which are the ones the outbox
// then sends again. What it costs is this issue's order at that holder: the copy reaches it only
// at the role's next claim, keep or the listener's sweep, up to its interval later, behind every
// event the holder takes meanwhile.
//
// The forward carries the time of the keep itself (contracts.Envelope.KeptAt), as the drain's
// forward of a copy carries the stream's store time of it, so a holder can judge the message's
// age however it was reached. It is the copy the holder sees: every holder is reached through the
// stream, which collapses the two forwards into whichever arrives first, and that is this one
// unless a drain that read the cursor before this handover reaches the agent subject ahead of it.
// A keep the outbox retried stamps the retry's own time, the stream having kept the copy the
// first publish stored.
func handOverKept(ctx context.Context, deps Deps, sender bus.Sender, role string, item contracts.Envelope, keptAt time.Time, handed *keptHandovers) {
	if err := handOverKeptCopy(ctx, deps, sender, role, item, keptAt, handed); err != nil && !lostToContextEnd(ctx, err) {
		slog.Warn("dispatch outbox: the copy kept for a role was not handed to the holder that claimed it",
			"role", role, "event_id", item.EventID, "error", err)
	}
}

// handOverKeptCopy is the handover's reads and its forward, whose failure handOverKept logs: one
// line for all of them, since what each costs the copy is the same and the batch's own stop
// quiets them alike (lostToContextEnd). Nothing to hand over is no failure: a role no live holder
// can take, a lane another copy leads and a Deps with no lane reader each come back nil with
// nothing sent.
//
// The holder is resolved before the lane is read, because what this batch has already handed over
// counts only for the holder it reached (keptHandovers.handedTo), and because a keep is made for
// a role no live holder can take: the resolution that still finds none ends the handover on one
// claim read rather than on the lane's two.
func handOverKeptCopy(ctx context.Context, deps Deps, sender bus.Sender, role string, item contracts.Envelope, keptAt time.Time, handed *keptHandovers) error {
	if deps.KeptLanes == nil {
		return nil
	}
	resolution, err := resolveRole(ctx, deps, role)
	if err != nil {
		return fmt.Errorf("resolve the holder of role %s, which a copy was just kept for: %w", role, err)
	}
	handover := forwardOf(role, item, resolution, nil, time.Now())
	if handover.keptFor != "" || handover.skip != "" || handover.err != nil {
		return nil
	}
	leads, seq, err := keptCopyLeadsTheLane(ctx, deps.KeptLanes, role, item, handed.handedTo(role, handover.holder))
	if err != nil || !leads {
		return err
	}
	stamped := keptAt.UnixMilli()
	item.KeptAt = &stamped
	if err := <-handover.forward(sender, item); err != nil {
		return fmt.Errorf("forward role %s's kept copy of %s to %s: %w", role, item.EventID, handover.holder, err)
	}
	handed.handed(role, handover.holder, seq)
	return nil
}

// keptHandovers is what one batch's own handovers have taken off the kept lanes it writes: per
// role, the holder this batch last forwarded a copy of that role to with a handover of its own,
// and that copy's stream sequence (handOverKept). A handover writes no cursor and leaves its
// copy on the lane, so without this record that copy still leads the lane and the batch's next
// copy of the same role - the second event of one issue, sent in turn on the role topic's own
// chain - would find it in front of itself and decline, and wait for the listener's sweep
// behind a copy this batch had already delivered to that holder (keptCopyLeadsTheLane).
//
// The holder is part of the record because the record stands for "this holder already has
// everything up to here", which is false for any other: a role whose claim changed hands between
// two of this batch's keeps resolves a holder that was handed none of those copies, and reading
// the lane from past them would hand it the later copy while the earlier ones are still on the
// lane for its own drain to send - the holder would take the issue's later event first. Only the
// record of the holder a handover resolves now is read (handedTo); for any other it is the drain
// cursor alone that says what leads the lane, which is what the lane said before this record
// existed.
//
// It is the batch's own, made in sendChains and dropped with it, because what it records is what
// this batch delivered with no cursor behind it: the batch after this one resolves the role's
// holder live and forwards to its agent subject with no keep at all, and a drain's cursor is the
// authority again for whatever is left on the lane.
//
// Keeps of one role sit on one chain and are sent in turn, but the handover behind each keep runs
// on a goroutine of its own (roleForward.keep) and the handovers of different roles run side by
// side, so every read and write of the map takes the mutex.
type keptHandovers struct {
	mu    sync.Mutex
	after map[string]keptHandover
}

// keptHandover is the last copy of one role this batch handed over itself: the holder it reached
// and the stream sequence it was found at on that role's kept lane.
type keptHandover struct {
	holder   string
	sequence uint64
}

// newKeptHandovers is the record one batch keeps (sendChains).
func newKeptHandovers() *keptHandovers {
	return &keptHandovers{after: map[string]keptHandover{}}
}

// handedTo is the lane sequence of the last copy of role this batch handed to holder itself, 0
// while it has handed that holder none, which is where keptCopyLeadsTheLane reads the lane from
// when the role's drain cursor stands no further on. A record of another holder answers 0: the
// copies it covers are ones this holder has not been sent, and they lead the lane for it.
func (h *keptHandovers) handedTo(role, holder string) uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	if record := h.after[role]; record.holder == holder {
		return record.sequence
	}
	return 0
}

// handed records that the copy of role stored at seq reached holder, so the batch's next copy of
// that role reads the lane from past it while holder is still the role's. A record of another
// holder is replaced rather than merged, the new one standing for what this holder has.
//
// Only a copy a forward reached the holder with is recorded. A copy the resolution held pending -
// the role unclaimed again, its holder the event's own actor, its claim lapsed, its heartbeat
// stale - and one whose forward failed are still on the lane for a drain to send, so a later copy
// must find them in front of it and decline: recording a copy nobody took would let that later
// copy overtake it at the holder.
func (h *keptHandovers) handed(role, holder string, seq uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if record := h.after[role]; record.holder == holder && record.sequence >= seq {
		return
	}
	h.after[role] = keptHandover{holder: holder, sequence: seq}
}

// KeptLaneReader reads a role's kept lane and its drain cursor: the one capability beyond
// publishing that the handover behind a keep needs (handOverKept, keptCopyLeadsTheLane). The
// cursor read is the one the listener's own pass over a lane makes as well
// (cmd/listener/role_keep.go, readKeptLaneHead), so the two writers of a lane decode one body one
// way; the lane read differs, a pass reading the lane's last copy and this the first past the
// cursor. bus.Client answers both over the stored-message API the outbox's own grant covers
// (infra/core/permissions.go, DispatchPermissions), and is what cmd/dispatch passes.
type KeptLaneReader interface {
	// NextStoredMessage is the first message the stream holds at or after seq on subject.
	NextStoredMessage(ctx context.Context, subject string, seq uint64) (bus.StoredMessage, error)
	// ReadKeptRoleCursor is role's drain cursor as the stream holds it, the zero cursor for a
	// lane no walk has written one for (bus.Client.ReadKeptRoleCursor).
	ReadKeptRoleCursor(ctx context.Context, role string) (bus.StoredKeptRoleCursor, error)
}

// keptCopyLeadsTheLane reports whether the copy of item just kept on role's kept lane is the one
// a drain of that lane would hand over next, and the stream sequence it was found at, which the
// caller records once that copy has reached the holder (handOverKept, keptHandovers.handed).
//
// The lane is read from past both the role's drain cursor and handed, the last copy of this role
// the batch handed to the holder this handover resolved (keptHandovers.handedTo): a handover
// moves no cursor, so the copies this batch has already delivered to that holder are still on
// the lane in front of this one and would otherwise hold it back. The cursor is read first - a
// lane no walk has written one for has none, and its first copy is the first past it
// (bus.Client.ReadKeptRoleCursor) - and then the first copy stored after the further of the two,
// which is this one when the lane holds nothing else unreached. The copies carry no header of
// their own here, so the one found is this copy when its dedupe key is item's:
// every writer of the lane stores the envelope as its sender sent it, under that key (keep,
// keepRoleMessage). A copy no read can decode is not this one either - the lane takes the raw
// role publishes the listener kept as well, and this copy was decoded to be sent - so it is a
// lane this copy does not lead rather than a failure, and it is the drain that passes such a copy
// over and logs it, once (walkKeptRole).
//
// A claim outstanding on the cursor is one walk's hold on the lane, judged by the rule the
// listener judges it by (bus.KeptRoleCursor.ClaimOutstanding): that walk may be forwarding the
// copy it claimed, which this handover must not overtake. A claim a killed drain left behind
// therefore declines this handover until it is past bus.KeptRoleClaimWindow, which costs the copy
// its place in this issue's order at the holder until then - bounded, not permanent, since the
// next walk takes a claim that old over and the listener's sweep guarantees a walk.
//
// The reads are a pass's, not a lock: a walk that advances the cursor between them, or right
// after them, leaves the handover forwarding a copy that walk is forwarding too, which the
// stream's duplicate window and the holder's dedupe key collapse as they collapse the two writers'
// copies.
func keptCopyLeadsTheLane(ctx context.Context, reader KeptLaneReader, role string, item contracts.Envelope, handed uint64) (bool, uint64, error) {
	cursor, err := reader.ReadKeptRoleCursor(ctx, role)
	if err != nil {
		return false, 0, err
	}
	if cursor.Cursor.ClaimOutstanding(cursor.Written, time.Now) {
		return false, 0, nil
	}
	after := max(cursor.Cursor.Kept, handed)
	next, err := reader.NextStoredMessage(ctx, bus.KeptRoleSubject(role), after+1)
	if errors.Is(err, natsgo.ErrMsgNotFound) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, fmt.Errorf("read role %s's kept lane after %d: %w", role, after, err)
	}
	var head contracts.Envelope
	if err := json.Unmarshal(next.Data, &head); err != nil {
		return false, 0, nil
	}
	return head.DedupeKey == item.DedupeKey, next.Sequence, nil
}
