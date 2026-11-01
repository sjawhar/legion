package enroll

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// ServingClock keeps this broker process's broker_serving row (migration 0019) beating while it
// serves, so that time no broker was serving is not counted against a lease. A lease can only be
// renewed through a broker; when every broker is down, or none can reach the database, every
// session's lease runs on with nobody able to renew it, and without the clock the first sweep
// after the outage would end each one whose lease ran out meanwhile, locking its pod out until it
// is relaunched. With it, enrollment_lease_live holds such a lease live until a broker beats again,
// and that beat moves the lease on by the time nobody served.
//
// Enrollments is the enrollment service whose Store it beats on and whose Lease, the enrollment
// lease, is the most a given-back lease may run ahead of the beat that gives it back. Interval is
// how often it beats, and also how long one beat may take. NotServingAfter is the longest gap
// after a beat that still counts as a broker serving; it must be well past Interval, so a beat
// that is merely late is not an outage, and well short of the part of its lease a renewing session
// always holds, so no live session's lease runs out inside a gap the clock tolerates.
//
// Each ServingClock is one process in broker_serving: it draws a random id on its first beat and
// beats on that row alone.
type ServingClock struct {
	Enrollments               *Service
	Interval, NotServingAfter time.Duration

	processOnce sync.Once
	process     string
}

// servingLock is the advisory lock a beat that found a gap takes before it gives the gap back, so
// that two brokers coming back at once give it back once. store.Migrate's lock is 8330001.
const servingLock int64 = 8330019

// servingPrune is how far behind the latest row a row may fall before a beat deletes it: a process
// that is gone (stopped, crashed, replaced) leaves its row this long at most.
const servingPrune = time.Hour

// Run beats at once, then every Interval on its own ticker, until ctx ends. A beat that fails is
// retried at the next tick; a run of failures is logged once, when it starts, so a database that
// stays away for an hour fills no log, and once more when the clock beats again. timed_out says
// whether the failure was the beat running out of its bound (its deadline, a lock it waited on
// past lock_timeout, or a statement past statement_timeout) rather than an error the database
// answered at once.
func (c *ServingClock) Run(ctx context.Context) {
	ticker := time.NewTicker(c.Interval)
	defer ticker.Stop()
	failing := false
	for {
		err := c.Beat(ctx)
		switch {
		case ctx.Err() != nil:
		case err != nil && !failing:
			slog.Warn("broker: the serving clock could not beat; it retries every interval and logs again once it beats",
				"error", err, "timed_out", beatTimedOut(err))
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

// beatTimedOut reports whether err is a beat running out of its bound: its context's deadline,
// lock_timeout (SQLSTATE 55P03, lock_not_available) or statement_timeout (57014, query_canceled,
// which is also what a statement pgx cancels on the deadline answers).
func beatTimedOut(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || pgconn.Timeout(err) {
		return true
	}
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "55P03" || pgErr.Code == "57014")
}

// processID answers this clock's process id, drawn once.
func (c *ServingClock) processID() string {
	c.processOnce.Do(func() { c.process = uuid.NewString() })
	return c.process
}

// Beat records that this broker is serving now, in one transaction bounded by Interval: the whole
// beat runs under a deadline of Interval, and its transaction sets lock_timeout and
// statement_timeout to it, so a beat held up on a lock or a slow statement fails within that bound
// rather than stalling unseen, and idle_in_transaction_session_timeout too, so that the server
// ends a beat whose connection went half-open and releases its row.
//
// While a broker is serving (the latest row's gap is within its threshold, or there is no row)
// one statement writes this process's row, locking that row alone: a beat held up on its own row
// stalls no other broker's beat, and the others keep the latest row current. Otherwise no broker
// served in between: the beat takes servingLock and reads the latest row again, and if the gap is
// still there every unended lease that was still running at the latest row's beat gets that gap
// back, moved on by it but no further than the enrollment lease past now, which a session that
// renewed during the gap already holds. A second broker that waited on the lock reads the row the
// first wrote and finds no gap, so the time is given back once. Either way this process's row
// moves to now, never backward, with this process's NotServingAfter, and rows more than
// servingPrune behind the latest are deleted, skipping any another transaction holds.
// enrollment_lease_live reads the rows without a lock, so no read of a lease ever waits on a beat.
func (c *ServingClock) Beat(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, c.Interval)
	defer cancel()
	process := c.processID()
	tx, err := c.Enrollments.Store.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	bound := strconv.FormatInt(max(c.Interval.Milliseconds(), 1), 10) + "ms"
	if _, err := tx.Exec(ctx, `select set_config('lock_timeout', $1, true), set_config('statement_timeout', $1, true),
		set_config('idle_in_transaction_session_timeout', $1, true)`, bound); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `with latest as (
			select last_serving_at, not_serving_after from broker_serving order by last_serving_at desc limit 1
		), beat as (select clock_timestamp() at)
		insert into broker_serving (process, last_serving_at, not_serving_after)
		select $1, beat.at, $2::interval from beat
		where not exists (select from latest where beat.at - latest.last_serving_at > latest.not_serving_after)
		on conflict (process) do update set
			last_serving_at = greatest(broker_serving.last_serving_at, excluded.last_serving_at),
			not_serving_after = excluded.not_serving_after`, process, c.NotServingAfter)
	if err != nil {
		return err
	}
	var gaveBack bool
	var gap time.Duration
	var last time.Time
	var restored int64
	if tag.RowsAffected() == 0 {
		if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock($1)`, servingLock); err != nil {
			return err
		}
		// Read the latest row and the clock only now the lock is held, so a beat that waited on
		// another reads the row that one wrote.
		var at time.Time
		var lastAt *time.Time
		var gapAt *time.Duration
		if err := tx.QueryRow(ctx, `select beat.at, latest.last_serving_at, beat.at - latest.last_serving_at,
				coalesce(beat.at - latest.last_serving_at > latest.not_serving_after, false)
			from (select clock_timestamp() at) beat
			left join lateral (
				select last_serving_at, not_serving_after from broker_serving order by last_serving_at desc limit 1
			) latest on true`).Scan(&at, &lastAt, &gapAt, &gaveBack); err != nil {
			return err
		}
		if gaveBack {
			last, gap = *lastAt, *gapAt
			tag, err := tx.Exec(ctx, `update enrollments set lease_expires_at = least(lease_expires_at + $1::interval, $2::timestamptz + $3::interval)
				where revoked_at is null and lease_expires_at > $4::timestamptz`, gap, at, c.Enrollments.Lease, last)
			if err != nil {
				return err
			}
			restored = tag.RowsAffected()
		}
		if _, err := tx.Exec(ctx, `insert into broker_serving (process, last_serving_at, not_serving_after) values ($1, $2, $3::interval)
			on conflict (process) do update set
				last_serving_at = greatest(broker_serving.last_serving_at, excluded.last_serving_at),
				not_serving_after = excluded.not_serving_after`, process, at, c.NotServingAfter); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `delete from broker_serving where process in (
			select process from broker_serving
			where last_serving_at < (select max(last_serving_at) from broker_serving) - $1::interval
			for update skip locked)`, servingPrune); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if gaveBack {
		slog.Info("broker: gave back the time no broker was serving to every lease still running when the last broker served",
			"gap", gap, "last_serving_at", last, "enrollments", restored)
	}
	return nil
}
