package enroll

import (
	"context"
	"log/slog"
	"time"

	"github.com/sjawhar/envoy/internal/broker/store"
)

// ServingClock keeps the broker_serving row (migration 0019) beating while this broker serves, so
// that time no broker was serving is not counted against a lease. A lease can only be renewed
// through a broker; when every broker is down, or none can reach the database, every session's
// lease runs on with nobody able to renew it, and without the clock the first sweep after the
// outage would end each one whose lease ran out meanwhile, locking its pod out until it is
// relaunched. With it, enrollment_lease_live holds such a lease live until a broker beats again,
// and that beat moves the lease on by the time nobody served.
//
// Interval is how often it beats. NotServingAfter is the longest gap between two beats that still
// counts as a broker serving; it must be well past Interval, so a beat that is merely late is not
// an outage, and well short of the part of its lease a renewing session always holds, so no live
// session's lease runs out inside a gap the clock tolerates. Lease is the enrollment lease, the
// most a given-back lease may run ahead of the beat that gives it back.
type ServingClock struct {
	Store                            *store.Store
	Interval, NotServingAfter, Lease time.Duration
}

// Run beats at once, then every Interval on its own ticker, until ctx ends. A beat that fails is
// retried at the next tick; a run of failures is logged once, when it starts, so a database that
// stays away for an hour fills no log, and once more when the clock beats again.
func (c *ServingClock) Run(ctx context.Context) {
	ticker := time.NewTicker(c.Interval)
	defer ticker.Stop()
	failing := false
	for {
		err := c.Beat(ctx)
		switch {
		case ctx.Err() != nil:
		case err != nil && !failing:
			slog.Warn("broker: the serving clock could not beat; it retries every interval and logs again once it beats", "error", err)
			failing = true
		case err == nil && failing:
			slog.Info("broker: the serving clock beats again")
			failing = false
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Beat records that a broker is serving now, in one transaction. When the last beat is further
// back than the threshold the last beating process recorded, no broker served in between: every
// unended lease that was still running at the last beat gets that gap back, moved on by it but no
// further than Lease past now, which a session that renewed during the gap already holds. The
// clock then moves to now, never backward, and the threshold to this process's NotServingAfter.
//
// Beat alone locks the row, taken before it reads the gap, so two brokers beating at once give the
// time back once: the second waits, then finds the gap the first one closed. enrollment_lease_live
// reads the row without a lock, so no read of a lease ever waits on a beat. A missing row (deleted
// by hand) is recreated as of now and gives nothing back.
func (c *ServingClock) Beat(ctx context.Context) error {
	tx, err := c.Store.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `insert into broker_serving (last_serving_at) values (clock_timestamp()) on conflict do nothing`); err != nil {
		return err
	}
	var last time.Time
	if err := tx.QueryRow(ctx, `select last_serving_at from broker_serving for update`).Scan(&last); err != nil {
		return err
	}
	// Read the clock only now the row is held, so a beat that waited on another reads the gap
	// that one left.
	var at time.Time
	var gap time.Duration
	var notServing bool
	if err := tx.QueryRow(ctx, `select at, at - last_serving_at, not_serving_after is not null and at - last_serving_at > not_serving_after
		from broker_serving, clock_timestamp() at`).Scan(&at, &gap, &notServing); err != nil {
		return err
	}
	var restored int64
	if notServing {
		tag, err := tx.Exec(ctx, `update enrollments set lease_expires_at = least(lease_expires_at + $1::interval, $2::timestamptz + $3::interval)
			where revoked_at is null and lease_expires_at > $4::timestamptz`, gap, at, c.Lease, last)
		if err != nil {
			return err
		}
		restored = tag.RowsAffected()
	}
	if _, err := tx.Exec(ctx, `update broker_serving set last_serving_at = greatest(last_serving_at, $1::timestamptz), not_serving_after = $2::interval`,
		at, c.NotServingAfter); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if notServing {
		slog.Info("broker: gave back the time no broker was serving to every lease still running when the last broker served",
			"gap", gap, "last_serving_at", last, "enrollments", restored)
	}
	return nil
}
