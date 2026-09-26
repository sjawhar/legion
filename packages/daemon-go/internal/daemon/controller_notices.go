package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// controllerNoticeRetries bounds the tries after a delivery that fails: 1 s doubling, six times,
// about a minute. A controller claims its role a moment after it registers, so the first request
// usually meets no holder and the next try delivers; after the last, what is held waits for the
// next request, which the controller watch makes every sweep while it finds the controller alive.
const controllerNoticeRetries = 6

// controllerNotices delivers the controller notices the outbox held because no session held the
// controller role (record.ControllerNotice): the Go form of the shipped daemon's
// controllerPendingNotices, drained on /controller/ready (packages/daemon/src/daemon/events.ts).
// The Go controller makes no ready call — it registers, then claims its role — so a delivery is
// asked for when it registers, when the controller watch finds it holding the role (a daemon
// restart, a role regained after a lapse), and when a notice is held. One goroutine delivers, so no
// notice is published twice at once, and each is removed once the listener takes it.
type controllerNotices struct {
	pool      *pgxpool.Pool
	records   record.Store
	publisher notify.Publisher
	// dispatchProject is the Dispatch project whose held notices these are: the database is shared.
	dispatchProject string
	// topic is the controller's role topic, from the project token.
	topic string
	wake  chan struct{}
	log   *slog.Logger
}

func newControllerNotices(pool *pgxpool.Pool, records record.Store, publisher notify.Publisher, project, dispatchProject string, log *slog.Logger) *controllerNotices {
	return &controllerNotices{
		pool: pool, records: records, publisher: publisher, dispatchProject: dispatchProject,
		topic: controllerTopic(project), wake: make(chan struct{}, 1), log: log,
	}
}

// controllerTopic is the role topic of project's controller, where a controller-kind notice goes
// (record.Notice.ForController). project is the project token.
func controllerTopic(project string) string {
	return roleTopicPrefix + string(claim.ControllerToken(project))
}

// deliver asks for every held notice to be delivered now. It never blocks: a request made while
// another is waiting is the same request.
func (c *controllerNotices) deliver() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// run delivers on each request until ctx ends. A delivery that fails — no session holds the role
// yet, or the listener or the database refuses — is tried again, controllerNoticeRetries times, and
// a request starts the count again. What is not delivered stays held in Postgres: nothing here
// holds the outbox, whose rows finished when their notices were held.
func (c *controllerNotices) run(ctx context.Context) {
	retry := time.NewTimer(0)
	retry.Stop()
	defer retry.Stop()
	attempt := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.wake:
			attempt = 0
			retry.Stop()
		case <-retry.C:
		}
		err := c.deliverHeld(ctx)
		if err == nil || ctx.Err() != nil {
			continue
		}
		level := slog.LevelWarn
		if errors.Is(err, notify.ErrNoHolder) {
			level = slog.LevelDebug
		}
		c.log.Log(ctx, level, "held controller notices not delivered", "attempt", attempt+1, "error", err)
		if attempt == controllerNoticeRetries {
			continue
		}
		retry.Reset(time.Second << attempt)
		attempt++
	}
}

// deliverHeld publishes the held notices oldest first, each under the dedupe key its outbox row
// published it with, and removes each the listener takes. It stops at the first failure, so the
// rest keep their order for the next try. A notice published whose removal then fails is published
// again next time under the same key, which the listener drops for a session that took it.
func (c *controllerNotices) deliverHeld(ctx context.Context) error {
	var held []record.ControllerNotice
	if err := pgx.BeginFunc(ctx, c.pool, func(tx pgx.Tx) error {
		var err error
		held, err = c.records.ControllerNotices(ctx, tx, c.dispatchProject)
		return err
	}); err != nil {
		return fmt.Errorf("read the held controller notices: %w", err)
	}
	for _, notice := range held {
		if err := c.publisher.Publish(ctx, c.topic, noticeMessage(notice.Notice, notice.Issue), notice.Notice, notice.DedupeKey); err != nil {
			return fmt.Errorf("deliver the held controller notice %s: %w", notice.DedupeKey, err)
		}
		if err := pgx.BeginFunc(ctx, c.pool, func(tx pgx.Tx) error {
			return c.records.DeliveredControllerNotice(ctx, tx, notice.ID)
		}); err != nil {
			return fmt.Errorf("record the held controller notice %s delivered: %w", notice.DedupeKey, err)
		}
		c.log.Info("delivered a held controller notice", "issue", notice.Issue, "kind", notice.Notice.Kind, "dedupeKey", notice.DedupeKey)
	}
	return nil
}
