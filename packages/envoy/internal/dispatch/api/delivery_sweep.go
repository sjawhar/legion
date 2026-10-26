package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The delivery sweep (INFRA-258). A write that delivers to sessions - a send to one session, the
// live view's model and thinking switch, a broadcast's recipients, an issue message to a session,
// a comment's and a document comment's mentions, and a retry from an agent card - claims each
// attempt before its send and settles it after. A process that dies between the two leaves the
// attempt pending under a claim no process holds; once that claim lapses (claimLapsed, a minute)
// the attempt is owed. Nobody has to ask again for it: every pass of the maintenance loop
// (RunSuggestionOutcomeSweep) sends each owed attempt, once, under its own attempt number and so
// its own idempotency key, as a repeat under the write's key does, so the listener drops a frame
// that did land (DELIVERY_DUPLICATE_WINDOW_MS in packages/contracts).
//
// An attempt is owed when it is its message's, or its mention's, latest attempt, still pending,
// its claim lapsed, and opened over a lease ago: an unclaimed attempt the write itself opened (a
// broadcast recipient's, a mention's) is its live sender's for the first minute. A pending attempt
// a later retry overtook is that retry's to answer. A keyed send whose process died between its
// message's commit and its first attempt has no attempt at all; its key's recovery point names the
// message, and its first attempt is owed once the key's holder is gone.
//
// Of two senders that both find an attempt owed - this sweep and a repeat under the write's key, or
// the sweeps of two processes - the one that claims it sends and the other finds it taken and
// sends nothing (deliverMessageResuming, claimCommentDeliveryAttempt).

// deliverySweepBatch bounds how many attempts of each kind one pass resumes, so a pass after a
// crash that stranded many is bounded too; the rest are the next pass's.
const deliverySweepBatch = 100

// owedDelivery is one attempt the sweep resumes: a message's, bound to its number, or a comment
// mention's, which the mention's claim finds by its target.
type owedDelivery struct {
	messageID string
	attempt   int
	delivery  string
	// requestedBy is who asked for the message attempt, nil for one the write opened before any
	// row recorded a requester; the attempt's own receipt names them.
	requestedBy *model.Actor
	commentID   string
	target      string
}

// resumeOwedDeliveries is one pass of the delivery sweep: it reads every owed attempt and sends
// each, broadcastDeliveryWorkers at a time, returning once every one has settled. The sends run on
// a context the pass's own cancellation does not reach and are counted in the server's background
// wait, so a shutdown that cuts the pass short lets the sends it started finish. Only a read of
// what is owed fails it; a send the listener refuses is recorded on its attempt.
func (s *server) resumeOwedDeliveries(ctx context.Context) error {
	if s.deps.Envoy == nil {
		return nil
	}
	s.detached.start()
	defer s.detached.done()
	owed, err := s.owedDeliveries(ctx)
	if err != nil {
		return err
	}
	if len(owed) == 0 {
		return nil
	}
	slog.Info("dispatch: resume deliveries a process left owed", "attempts", len(owed))
	sending := context.WithoutCancel(ctx)
	pending := make(chan owedDelivery)
	var wait sync.WaitGroup
	for range min(broadcastDeliveryWorkers, len(owed)) {
		wait.Go(func() {
			for delivery := range pending {
				// A mark of this worker's own: workers sharing one would refuse each other a
				// connection (store.ErrNestedAcquire).
				worker, err := store.ForConcurrentRead(sending)
				if err == nil {
					err = s.resumeOwedDelivery(worker, delivery)
				}
				if err != nil {
					slog.Error("dispatch: resume an owed delivery", "message", delivery.messageID,
						"comment", delivery.commentID, "target", delivery.target, "attempt", delivery.attempt, "error", err)
				}
			}
		})
	}
	for _, delivery := range owed {
		pending <- delivery
	}
	close(pending)
	wait.Wait()
	return nil
}

// resumeOwedDelivery sends one owed attempt, as a repeat under its write's key sends it.
func (s *server) resumeOwedDelivery(ctx context.Context, owed owedDelivery) error {
	if owed.commentID != "" {
		comment, err := s.loadComment(ctx, s.deps.Store.Pool, owed.commentID)
		if err != nil {
			return err
		}
		created, err := s.loadCommentCreatedEvent(ctx, s.deps.Store.Pool, comment.ID)
		if err != nil {
			return err
		}
		// As the comment's own first delivery (initial), so an attempt a live sender took in the
		// meantime is left to it and no attempt of the sweep's own is opened.
		_, err = s.deliverResolvedCommentMention(ctx, comment, created,
			ResolvedMention{Target: owed.target, Delivery: owed.delivery}, comment.Author, true)
		return err
	}
	message, err := s.loadMessage(ctx, s.deps.Store.Pool, "", owed.messageID)
	if err != nil {
		return err
	}
	actor := message.Author
	if owed.requestedBy != nil {
		actor = *owed.requestedBy
	}
	if _, err := s.deliverMessageResuming(ctx, message, owed.delivery, nil, actor, nil, owed.attempt); err != nil &&
		!errors.Is(err, errAttemptNotPending) {
		return err
	}
	return nil
}

// owedDeliveries reads what the sweep resumes: the owed message attempts, the first attempt of
// every keyed send that has none, and the owed comment mention attempts, oldest first.
func (s *server) owedDeliveries(ctx context.Context) ([]owedDelivery, error) {
	window := contracts.DeliveryDuplicateWindow.Seconds()
	var owed []owedDelivery
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select d.message_id::text, d.attempt, d.delivery, d.requested_by
		from message_deliveries d
		where d.state = 'pending' and `+claimLapsed+`
		  and d.created_at < now() - interval '1 minute'
		  and d.created_at > now() - make_interval(secs => $1)
		  and d.attempt = (select max(attempt) from message_deliveries l where l.message_id = d.message_id)
		order by d.created_at
		limit $2
	`, window, deliverySweepBatch)
	if err != nil {
		return nil, fmt.Errorf("read the owed message attempts: %w", err)
	}
	messages, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (owedDelivery, error) {
		var delivery owedDelivery
		var requestedBy []byte
		if err := row.Scan(&delivery.messageID, &delivery.attempt, &delivery.delivery, &requestedBy); err != nil {
			return owedDelivery{}, err
		}
		if requestedBy != nil {
			delivery.requestedBy = &model.Actor{}
			if err := json.Unmarshal(requestedBy, delivery.requestedBy); err != nil {
				return owedDelivery{}, fmt.Errorf("decode delivery requester: %w", err)
			}
		}
		return delivery, nil
	})
	if err != nil {
		return nil, fmt.Errorf("read the owed message attempts: %w", err)
	}
	owed = append(owed, messages...)

	// A keyed send whose key's holder is gone - it never answered, and its lease lapsed or was
	// released - with a targeted message and no attempt at all.
	rows, err = s.deps.Store.Pool.Query(ctx, `
		select m.id::text, k.recovery ->> 'delivery'
		from request_keys k
		join messages m on m.id = (k.recovery ->> 'message_id')::uuid
		where k.recovery ? 'message_id' and k.status is null
		  and (k.leased_at is null or k.leased_at < now() - make_interval(secs => $1))
		  and k.created_at > now() - make_interval(secs => $2)
		  and m.target is not null
		  and not exists (select 1 from message_deliveries d where d.message_id = m.id)
		order by k.created_at
		limit $3
	`, s.keyLease().Seconds(), contracts.IdempotencyKeyTTL.Seconds(), deliverySweepBatch)
	if err != nil {
		return nil, fmt.Errorf("read the keyed sends with no attempt: %w", err)
	}
	unattempted, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (owedDelivery, error) {
		delivery := owedDelivery{attempt: 1}
		err := row.Scan(&delivery.messageID, &delivery.delivery)
		return delivery, err
	})
	if err != nil {
		return nil, fmt.Errorf("read the keyed sends with no attempt: %w", err)
	}
	owed = append(owed, unattempted...)

	rows, err = s.deps.Store.Pool.Query(ctx, `
		select d.comment_id::text, d.target, d.delivery
		from comment_deliveries d
		where d.state = 'pending' and `+claimLapsed+`
		  and d.created_at < now() - interval '1 minute'
		  and d.created_at > now() - make_interval(secs => $1)
		  and d.attempt = (
		    select max(attempt) from comment_deliveries l where l.comment_id = d.comment_id and l.target = d.target
		  )
		order by d.created_at
		limit $2
	`, window, deliverySweepBatch)
	if err != nil {
		return nil, fmt.Errorf("read the owed comment mention attempts: %w", err)
	}
	comments, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (owedDelivery, error) {
		var delivery owedDelivery
		err := row.Scan(&delivery.commentID, &delivery.target, &delivery.delivery)
		return delivery, err
	})
	if err != nil {
		return nil, fmt.Errorf("read the owed comment mention attempts: %w", err)
	}
	return append(owed, comments...), nil
}
