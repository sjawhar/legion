package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// pendingEvent is one row of a batch, read out before anything is published: an open cursor holds
// its pooled connection until it closes, and publishing a batch reads followers and authors through
// the same pool, so the rows are drained first and the work done after.
type pendingEvent struct {
	event model.Event
	// owner is the issue key, document id or project key whose events this one is ordered among,
	// and empty for an event with none.
	owner string
	// due is whether the event may be sent now: it was never tried, or its back-off has passed.
	// An event that is not due is read only as a blocker of its owner's later events
	// (planChains): it is never sent, and nothing is recorded for it.
	due bool
	// nextAttempt is when an event that is not due falls due, on the database's clock.
	nextAttempt  *time.Time
	attempts     int
	destinations []string
	slug         string
	routing      routing
	// candidates are the owner's events that are due and that the batch's choice of owners found
	// (readBatch), and windowSize how many of the owner's events the batch read before it kept the
	// first batchSize due ones: a candidate a window read whole does not hold is one past it.
	candidates []int64
	windowSize int
	// failure is why the row cannot be published as it was read - it did not scan, or its actor or
	// payload JSON did not decode - and failureLog is the predicate it reports under. Such an
	// event has no destinations: it is backed off, and holds back no other event.
	failure    error
	failureLog string
}

// routing is what the batch read off the event's issue when it publishes, not what it was when the
// event was written: the owner's route (the issue's, or the issue's whose document the event is
// about), and the session holding the issue's claim ("" when the issue is unclaimed or a person
// holds it, since a person has no Envoy topic). Every batch reads them again (readBatchSQL), so a
// retry after the route or the claim moved reaches the new destination.
type routing struct {
	route    *string
	claimant string
}

// readBatchSQL chooses a batch and reads it, writing nothing. It chooses owners that have a due
// event: the lowest-id events never tried (events_outbox_fresh) and the earliest back-offs that
// have fallen due (events_outbox_retry), batchSize of each, grouped by owner, the owner first
// whose due event has the lowest id. For each chosen owner it reads that owner's unpublished events
// in id order (events_outbox_owner), batchSize at most, due or not: an event not due holds the
// later ones back from its unreached destinations. An event with no owner has no destination and
// forms no chain, and is read alone. It reads no event of an owner $2 names, the owners the
// batches running beside this one hold in flight (lanes), so no two batches ever take one owner
// and an owner waiting on a send holds back only its own events. A pending subscription command
// (api/subscribers.go) is in no index this reads, and neither is a backed-off event of an owner
// the batch does not choose. Every predicate on pending is spelled as the indexes spell it
// (migration 0151), which is what lets Postgres prove the indexes' predicates from the query's.
//
// Of the rows read, the batch keeps every row before its batchSize-th due one in id order, and that
// row: at most batchSize events are sent per batch, and each owner keeps a prefix of its events, the
// owners' events interleaved as they were written, so a backlog over many owners makes short chains
// rather than long ones for a few. The choice carries only each row's id and back-off: the event's
// own columns, and the issue (its route and claim, routing), document and project columns a
// publish needs, are joined onto the kept rows. A kept row's event is read by its id in a lateral
// subquery with a limit, which
// Postgres does not flatten into a join, so each is one lookup of events_pkey: as a join, the
// planner, which expects many more kept rows than a batch keeps, merges them with a walk of every
// event's id.
const readBatchSQL = `
	with fresh as (
		select id, coalesce(issue_key, artifact_id::text, project_key) as owner, next_attempt_at
		from events
		where published_at is null and next_attempt_at is null
		  and (payload ->> 'pending') is distinct from 'true'
		  and coalesce(issue_key, artifact_id::text, project_key, '') <> all($2::text[])
		order by id
		limit $1
	), retried as (
		select id, coalesce(issue_key, artifact_id::text, project_key) as owner, next_attempt_at
		from events
		where published_at is null and next_attempt_at is not null
		  and (payload ->> 'pending') is distinct from 'true'
		  and coalesce(issue_key, artifact_id::text, project_key, '') <> all($2::text[])
		  and next_attempt_at <= now()
		order by next_attempt_at, id
		limit $1
	), due as (
		select id, owner, next_attempt_at from fresh
		union all
		select id, owner, next_attempt_at from retried
	), owners as (
		select owner, min(id) as first, array_agg(id order by id) as candidates
		from due
		where owner is not null
		group by owner
		order by first
		limit $1
	), chosen as (
		select o.first, o.candidates, w.id, w.next_attempt_at
		from owners o
		cross join lateral (
			select e.id, e.next_attempt_at
			from events e
			where coalesce(e.issue_key, e.artifact_id::text, e.project_key) = o.owner
			  and e.published_at is null
			  and (e.payload ->> 'pending') is distinct from 'true'
			order by e.id
			limit $1
		) w
		union all
		select d.id, array[d.id], d.id, d.next_attempt_at
		from due d
		where d.owner is null
	), ranked as (
		select chosen.*,
		       (next_attempt_at is null or next_attempt_at <= now()) as due,
		       count(*) filter (where next_attempt_at is null or next_attempt_at <= now())
		           over (order by id) as sendable,
		       count(*) over (partition by first) as window_size
		from chosen
	)
	select e.id, e.issue_key, e.artifact_id::text, coalesce(e.project_key, i.project_key, ar.project_key, ''),
	       e.seq, e.type, e.actor, e.notify, e.created_at, e.payload, e.attempt_count,
	       e.published_destinations, coalesce(ar.slug, ''), coalesce(i.route, ai.route),
	       coalesce(case when i.claimed_by ->> 'kind' = 'session' then i.claimed_by ->> 'id' end, ''),
	       coalesce(e.issue_key, e.artifact_id::text, e.project_key), r.due, r.next_attempt_at, r.candidates,
	       r.window_size
	from ranked r
	cross join lateral (select * from events where id = r.id limit 1) e
	left join issues i on i.key = e.issue_key
	left join artifacts ar on ar.id = e.artifact_id
	left join issues ai on ai.key = ar.issue_key
	where r.sendable - r.due::int < $1
	order by r.first, r.id
`

// readBatch reads a batch (readBatchSQL), in owner order and each owner's events in id order. It
// writes no row. stopped reports a row that failed to scan, which ends pgx's cursor: the rows after
// it are not read, and the batch is the rows before it and that row, backed off. The read leaves
// the owners the batches running beside this one hold alone, and takes the owners of the rows it
// read, both under the lanes' one lock (lanes.reserve): publishBatch gives each of them back.
func readBatch(ctx context.Context, deps Deps) ([]pendingEvent, bool, error) {
	return deps.lanes.reserve(func(inFlight []string) ([]pendingEvent, bool, error) {
		return readRows(ctx, deps, inFlight)
	})
}

// readRows is readBatch's statement, run with no other batch's statement beside it (lanes), and
// its rows drained before it gives the gate back: an open cursor holds one of the outbox pool's
// two connections.
func readRows(ctx context.Context, deps Deps, inFlight []string) (pending []pendingEvent, stopped bool, err error) {
	deps.lanes.lock()
	defer deps.lanes.unlock()
	rows, err := deps.Store.Pool.Query(ctx, readBatchSQL, batchSize, inFlight)
	if err != nil {
		return nil, false, fmt.Errorf("read the outbox's batch: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var item pendingEvent
		var actor, payload []byte
		var destinations []pgtype.Text
		var owner *string
		if err := rows.Scan(
			&item.event.ID,
			&item.event.IssueKey,
			&item.event.ArtifactID,
			&item.event.Project,
			&item.event.Seq,
			&item.event.Type,
			&actor,
			&item.event.Notify,
			&item.event.CreatedAt,
			&payload,
			&item.attempts,
			&destinations,
			&item.slug,
			&item.routing.route,
			&item.routing.claimant,
			&owner,
			&item.due,
			&item.nextAttempt,
			&item.candidates,
			&item.windowSize,
		); err != nil {
			// pgx scans positionally and treats a failure as fatal to the cursor, so this row ends
			// the batch and rows.Err() below repeats the same error. e.id is the first column, so
			// the row scanned that far before it failed, and it is carried on as a failure, backed
			// off like any other event that cannot be published.
			item.failure = fmt.Errorf("read event: %w", err)
			item.failureLog = "dispatch outbox: read event"
			item.due = true
			pending = append(pending, item)
			stopped = true
			continue
		}
		if owner != nil {
			item.owner = *owner
		}
		for _, destination := range destinations {
			if !destination.Valid {
				item.failure = errors.New("read event: published_destinations holds a null")
				item.failureLog = "dispatch outbox: read event"
				break
			}
			item.destinations = append(item.destinations, destination.String)
		}
		if item.failure == nil {
			if err := json.Unmarshal(actor, &item.event.Actor); err != nil {
				item.failure = fmt.Errorf("decode event actor: %w", err)
				item.failureLog = "dispatch outbox: decode event actor"
			} else if err := json.Unmarshal(payload, &item.event.Payload); err != nil {
				item.failure = fmt.Errorf("decode event payload: %w", err)
				item.failureLog = "dispatch outbox: decode event payload"
			}
		}
		pending = append(pending, item)
	}
	// Closed before anything else reads the pool: a cursor still open holds one of its
	// connections, and a caller asking for a second is refused (store.ErrNestedAcquire). Close is
	// idempotent, so the deferred one above still stands.
	rows.Close()
	if err := rows.Err(); err != nil && !stopped {
		return nil, false, fmt.Errorf("read the outbox's batch: %w", err)
	}
	return pending, stopped, nil
}

// destination is one subject an event is sent to, named by label and id in what a failure reports
// (`publish route "role:reviewer": …`), since a subject alone does not say which of the event's
// destinations it was. subject is also what published_destinations records once it is reached. A
// role route's subject is its role topic and role its role; forward is how the batch reaches the
// role's holder (resolveRoleHolders), nil for every other destination, which is published to
// subject itself.
type destination struct {
	subject string
	label   string
	id      string
	role    string
	forward *roleForward
}

// ownerTopicKey is the destination key of an owner's topic family (destinationKey).
const ownerTopicKey = "owner"

// destinationKey names the subject family a session receives subject on, which is what the order
// of an owner's events is kept on (planChains). Every subject an owner's events go to under
// notifications.dispatch. - an issue's, a document's or a project's, one per event type - is one
// family, the owner's topic family. Each notifications.agent.<session> subject is one, whichever
// route reaches it: the issue's session route, an author or follower route, a previous claimant's
// notice. A role route's key is its holder's (destination.key).
func destinationKey(subject string) string {
	if strings.HasPrefix(subject, dispatchTopicPrefix) {
		return ownerTopicKey
	}
	return subject
}

// key is the destination's key (destinationKey). A role route's forward goes to its holder's agent
// subject, so its key is that subject, the key of every other route to the same session: a role
// forward and a session, author or follower route to one session share one chain, and an earlier
// event whose role route has not reached the holder holds the holder's later events back. What the
// role route records once reached is its role topic, not the holder's subject: a role route reached
// is reached for good, so a retry of its event skips the forward even when the role has moved to
// another holder since, and the role topic is the one subject every later read of the event knows
// that route by (planChains, resolveRoleHolders). A role route with no holder known - its claim
// could not be read, or churned - is keyed on its role topic.
func (d destination) key() string {
	if d.forward != nil && d.forward.holder != "" {
		return contracts.AgentSubject(d.forward.holder)
	}
	return destinationKey(d.subject)
}

// batchEvent is one event of a batch with what the batch made of it: its envelope and
// destinations, its sends, the blockers that held a destination of it back, and its outcome.
type batchEvent struct {
	pendingEvent
	item contracts.Envelope
	// delivered is the set of the event's published_destinations (publishedDestinationSet).
	// destinationsOf are every destination the event has; resolveErr is why the list ends short,
	// when it does, and resolveLost that the error is the batch's context ending (a
	// recipient read the process's stop cancelled), which releases the event rather than backing
	// it off; mentionPending is a session's copy left out of the list for a mention still being
	// sent, which blocks the event (eventDestinations, settleEvent). A copy so left out is no
	// destination of the event, so it holds none of the owner's later events back.
	delivered      map[string]struct{}
	destinationsOf []destination
	resolveErr     error
	resolveLost    bool
	mentionPending bool
	// incomplete is that the list ends at a recipient read that failed (unansweredRecipients): the
	// sessions that read would add are unknown, so the event holds its owner's later events back
	// from every session key its list neither names nor it already reached (planChains).
	incomplete bool
	sends      []*chainSend
	// held are the earlier events of the owner that held a destination of this one back without a
	// send of this batch: one that is not due, or one whose list is incomplete.
	held    []blocker
	outcome outcome
}

// blocker is what held an event's send to a destination back: an earlier event of its owner that
// had that destination unreached, or whose list is incomplete and neither names nor reached it. It
// is either an event of the batch, whose outcome then says when the held one is tried again - its
// send to the destination failed, or its recipient read did - or one that is not due, until its
// nextAttempt.
type blocker struct {
	event *batchEvent
	until *time.Time
}

// sendState is what one send of a chain came to.
type sendState int

const (
	// unsent: the send was never made, because the batch stopped first (NATS unreachable, the
	// publisher hold's deadline passed, the process stopping).
	unsent sendState = iota
	// reached: NATS accepted the envelope, or refused it in a way no retry changes (bus.ErrRefused).
	reached
	// failed: NATS answered the send with a failure of the envelope: a denial, no acknowledgement
	// on a connection still current, or another error.
	failed
	// lost: the send met a lost connection, the hold's deadline or the process stopping, so whether
	// it landed is unknown and no fault of the event.
	lost
	// held: an earlier send of the chain failed, so this one was not made.
	held
)

// chainSend is one send of a chain: event's envelope to dest. Its answer is classified once, as it
// settles (sendChains): state, and the two facts the batch reads back - fenced, the send refused
// for the publisher hold's deadline (bus.ErrWindowDeadline), and connection, the send failed for
// the connection, lost to it (bus.ErrUnavailable) or not acknowledged on it
// (bus.ErrNotAcknowledged).
type chainSend struct {
	event              *batchEvent
	dest               destination
	answer             <-chan error
	state              sendState
	err                error
	fenced, connection bool
	// heldBy is the event whose failed send held this one back.
	heldBy *batchEvent
}

// sent reports whether the send counts as one the batch made: its answer is in, it was not refused
// for the hold's deadline, and it is not a role route reached with nothing to send
// (roleForward.skip), which delivers nothing to anyone.
func (s *chainSend) sent() bool {
	skipped := s.dest.forward != nil && s.dest.forward.skip != ""
	return s.state != unsent && s.state != held && !s.fenced && !skipped
}

// chain is the sends of one owner's events to one destination key, in id order. Its next send is
// made only once the one before it reached its destination. owner is whose lane the chain runs
// in: a batch settles and releases an owner once every chain of that owner has finished
// (sendChains, publishBatch).
type chain struct {
	owner string
	sends []*chainSend
	next  int
}

// planChains orders each owner's sends. An owner's events are walked in id order, and each
// destination an event has not reached yet goes on its owner's chain for that destination's key -
// unless an earlier event of the owner that is not due has that key unreached, or an earlier event
// whose list is incomplete may still owe that key (below), in which case the event is held back
// from it (batchEvent.held). Every destination key of every owner is a chain of its own, and a
// chain's sends go one at a time (sendChains), so the order holds per (owner, destination key):
// event N+1 of an owner reaches a destination only after every earlier unpublished event of that
// owner bound for it has. A destination that stalls - its send failed, or its event is waiting out
// a back-off with that destination unreached - holds back that destination's later events of that
// owner and nothing else.
//
// The one hold wider than a destination is an event whose recipient read failed
// (batchEvent.incomplete): its list ends at the read, so it may be bound for any session the read
// would name. Every later event of its owner is held back from each session key - an agent
// subject, or a role route's key - that the incomplete event neither names in its list nor already
// reached (batchEvent.delivered), behind a blocker that falls due when the incomplete event is
// tried again: the event itself when it is due, whose back-off or release its outcome carries,
// and its nextAttempt when it is not. A key the list names and the event has not reached chains as
// any other, and a key the event already reached holds nothing back: a destination reached counts
// by the subject it recorded, not by its key now, so a role route reached names its role topic and
// not the agent subject of whoever holds the role in this batch, a session the failed read may yet
// name. The owner's topic comes before every read (eventDestinations), so the owner's topic family
// is never held this way. So a recipient read that fails holds back, until the retry of the event
// that asked it, its owner's later session copies on the keys that event could still owe, and
// nothing else; an event whose reads all answered holds back only the destinations it has not
// reached.
//
// The order is kept per destination key, not per session: a session that subscribes to an issue's
// topic family and is also routed to by its events receives on two keys, so when event N's topic
// publish fails and N+1's agent publish succeeds it sees N+1's first copy before N's. Both are
// JetStream publishes on the same stream, so it takes a stream failing one subject and not the
// other.
//
// A role route that counts as reached with nothing to send (roleForward.skip) goes on no chain: a
// due event's is reached at once, and a not-due event's holds nothing back, since nothing of it is
// delivered to anyone. A role route that stays pending (roleForward.err) goes on its key's chain
// like any send, fails when its turn comes, and holds the rest of that chain back.
//
// An event with no owner has no destination and forms no chain; an event that cannot be read has
// none either, and holds nothing back.
func planChains(events []*batchEvent) []*chain {
	// unknownSessions is an incomplete event's hold: the keys of the destinations its list names
	// and it has not reached, and of the subjects it recorded reached (named), and what holds the
	// owner's later events back from every other session key.
	type unknownSessions struct {
		named map[string]struct{}
		by    blocker
	}
	var chains []*chain
	byOwner := map[string]map[string]*chain{}
	blocked := map[string]map[string]blocker{}
	unknown := map[string][]unknownSessions{}
	for _, event := range events {
		if event.failure != nil || event.owner == "" {
			continue
		}
		if byOwner[event.owner] == nil {
			byOwner[event.owner] = map[string]*chain{}
			blocked[event.owner] = map[string]blocker{}
		}
		for _, dest := range event.destinationsOf {
			if _, ok := event.delivered[dest.subject]; ok {
				continue
			}
			if dest.forward != nil && dest.forward.skip != "" {
				if event.due {
					event.sends = append(event.sends, &chainSend{event: event, dest: dest, state: reached})
				}
				continue
			}
			key := dest.key()
			if !event.due {
				if _, ok := blocked[event.owner][key]; !ok {
					blocked[event.owner][key] = blocker{until: event.nextAttempt}
				}
				continue
			}
			held := len(event.held)
			if by, ok := blocked[event.owner][key]; ok {
				event.held = append(event.held, by)
			}
			for _, earlier := range unknown[event.owner] {
				if _, ok := earlier.named[key]; !ok {
					event.held = append(event.held, earlier.by)
				}
			}
			if len(event.held) > held {
				continue
			}
			keyed := byOwner[event.owner][key]
			if keyed == nil {
				keyed = &chain{owner: event.owner}
				byOwner[event.owner][key] = keyed
				chains = append(chains, keyed)
			}
			send := &chainSend{event: event, dest: dest}
			keyed.sends = append(keyed.sends, send)
			event.sends = append(event.sends, send)
		}
		if event.incomplete {
			hold := unknownSessions{named: make(map[string]struct{}, len(event.destinationsOf)+len(event.delivered)), by: blocker{event: event}}
			if !event.due {
				hold.by = blocker{until: event.nextAttempt}
			}
			for _, dest := range event.destinationsOf {
				if _, ok := event.delivered[dest.subject]; ok {
					continue
				}
				hold.named[dest.key()] = struct{}{}
			}
			for subject := range event.delivered {
				hold.named[destinationKey(subject)] = struct{}{}
			}
			unknown[event.owner] = append(unknown[event.owner], hold)
		}
	}
	return chains
}

// sendChains runs chains through sender, one window: the first send of every chain at once, in the
// order the chains were planned, and each chain's next send as soon as the answer to the one before
// it came back as reached, so the chains proceed side by side and each one in order. A role route
// is forwarded to its holder with a Request (roleForward.send), or answered at once with why it
// stays pending. A failed send holds back the rest of its chain. A send that meets a lost
// connection or a passed deadline (bus.ErrUnavailable; bus.ErrWindowDeadline, the window refusing
// a send past the publisher hold's deadline) stops the batch: no further send of any chain is
// made. So does ctx ending; the answers already in are still read, and every other send in flight
// is lost. Each send that did not reach its destination is counted once as it settles, under the
// reason it came to (deps.countPublishFailure); a send the hold's deadline fenced and one ctx's
// end cut count nothing, neither being an outcome NATS gave.
//
// settled is called once per owner, as soon as the last chain of that owner has finished - its
// sends all answered, or the rest of them held back or never made - so the batch can settle,
// record and release that owner while the other owners' chains are still sending (publishBatch).
func sendChains(ctx context.Context, deps Deps, sender bus.Sender, chains []*chain, settled func(owner string)) {
	stopped := false
	var inflight []*chain
	left := map[string]int{}
	for _, c := range chains {
		left[c.owner]++
	}
	finish := func(c *chain) {
		left[c.owner]--
		if left[c.owner] == 0 {
			settled(c.owner)
		}
	}
	issue := func(c *chain) {
		if stopped || ctx.Err() != nil {
			finish(c)
			return
		}
		send := c.sends[c.next]
		routed := send.event.item
		routed.Topic = send.dest.subject
		switch forward := send.dest.forward; {
		case forward == nil:
			send.answer = sender.PublishAsync(routed)
		case forward.err != nil:
			send.answer = failedAnswer(forward.err)
		default:
			send.answer = forward.send(sender, routed)
		}
		inflight = append(inflight, c)
	}
	settle := func(index int, err error) {
		c := inflight[index]
		inflight = append(inflight[:index], inflight[index+1:]...)
		send := c.sends[c.next]
		send.err = err
		send.fenced = errors.Is(err, bus.ErrWindowDeadline)
		switch {
		case err == nil:
			send.state = reached
		case errors.Is(err, bus.ErrRefused):
			// Refused the same way however often it is retried (the envelope is past the server's
			// max payload, or its subject past NATS's limit or holding whitespace, as an unbounded
			// document slug or a bearer's session id can make it), so it is logged once and counts
			// as reached. It did not reach NATS, so it is still counted a refusal.
			deps.countPublishFailure("refused")
			slog.Error("dispatch outbox: destination refused", "event_id", send.event.event.ID, "topic", send.dest.subject, "error", err)
			send.state = reached
		case send.fenced, errors.Is(err, bus.ErrUnavailable):
			// NATS unreachable is the outage the publish-failure metric exists for, so the send
			// that met it is counted a failure, once; a send the hold's deadline refused never
			// left this process and is not.
			if !send.fenced {
				deps.countPublishFailure("failed")
			}
			send.state = lost
			send.connection = errors.Is(err, bus.ErrUnavailable)
			stopped = true
		case ctx.Err() != nil && errors.Is(err, ctx.Err()):
			send.state = lost
		default:
			// Counted under the reason the send came to (countPublishFailure): a send the server
			// refused under this connection's own publish permissions is a denial; a role forward
			// its holder's resolution held pending sent nothing at all, and is counted under that
			// holder rather than as a failure of the bus; a forward NATS took whose holder sent no
			// receipt is unanswered, which the request reports as a receipt timeout with its
			// publish confirmed; every other failure, an unconfirmed publish among them, is a
			// failure.
			switch forward := send.dest.forward; {
			case errors.Is(err, bus.ErrPublishDenied):
				deps.countPublishFailure("denied")
			case forward != nil && forward.err != nil:
				deps.countPublishFailure("holder")
			case errors.Is(err, bus.ErrReceiptTimeout) && !errors.Is(err, bus.ErrPublishUnconfirmed):
				deps.countPublishFailure("unanswered")
			default:
				deps.countPublishFailure("failed")
			}
			send.state = failed
			send.connection = errors.Is(err, bus.ErrNotAcknowledged)
		}
		c.next++
		if send.state != reached {
			if send.state == failed {
				for _, rest := range c.sends[c.next:] {
					rest.state, rest.heldBy = held, send.event
				}
			}
			c.next = len(c.sends)
			finish(c)
			return
		}
		if c.next < len(c.sends) {
			issue(c)
			return
		}
		finish(c)
	}
	for _, c := range chains {
		issue(c)
	}
	// Each chain has at most one send in flight, so the select's cases are at most every chain's
	// and ctx's.
	cases := make([]reflect.SelectCase, 0, len(chains)+1)
	for len(inflight) > 0 {
		// An answer already in is taken first, in the order the chains were planned, so a batch
		// whose answers are all in proceeds the same way every time.
		if index, err, ok := firstAnswered(inflight); ok {
			settle(index, err)
			continue
		}
		if ctx.Err() != nil {
			for len(inflight) > 0 {
				settle(0, ctx.Err())
			}
			break
		}
		cases = cases[:0]
		for _, c := range inflight {
			cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(c.sends[c.next].answer)})
		}
		cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(ctx.Done())})
		chosen, value, _ := reflect.Select(cases)
		if chosen == len(inflight) {
			continue
		}
		var err error
		if !value.IsNil() {
			err = value.Interface().(error)
		}
		settle(chosen, err)
	}
}

// firstAnswered returns the first chain of inflight whose current send's answer is in, and that
// answer.
func firstAnswered(inflight []*chain) (int, error, bool) {
	for index, c := range inflight {
		select {
		case err := <-c.sends[c.next].answer:
			return index, err, true
		default:
		}
	}
	return 0, nil, false
}
