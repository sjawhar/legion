package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// seedFinishPoll is how often seed-finish reads a count it waits on.
const seedFinishPoll = 2 * time.Second

// The queries seed-finish runs. The block-index tables do not exist in a build older than
// migration 0121, which owes nothing; Postgres resolves every table a statement names before it
// runs, so the existence check is its own query.
const (
	sqlUnpublishedEvents = "select count(*) from events where published_at is null"
	sqlBlockIndexTables  = "select to_regclass('public.document_block_versions') is not null and to_regclass('public.block_grammar_owed') is not null"
	sqlBlockIndexOwed    = "select (select count(*) from document_block_versions where version is null) + (select count(*) from block_grammar_owed)"
	sqlVacuumAnalyze     = "vacuum analyze"
	// seed-refs writes under idempotency keys, so Dispatch keeps one request_keys row per write.
	// They protect retries during the seed only. Left in the template, Dispatch's sweep of keys
	// older than a day would delete them inside the window of every step on a seed older than
	// that, and not on a younger one. A build older than migration 0137 has no such table.
	sqlRequestKeysTable = "select to_regclass('public.request_keys') is not null"
	sqlRequestKeysDel   = "delete from request_keys"
	sqlRequestKeysCount = "select count(*) from request_keys"
)

// pgSession is what seed-finish uses of a Postgres connection; *pgx.Conn is one.
type pgSession interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// seedFinisher runs the steps after `loadgen seed`, while Dispatch still runs: each wait polls its
// count every poll and fails once it has waited timeout; now and sleep are the clock it waits on.
type seedFinisher struct {
	timeout, poll time.Duration
	now           func() time.Time
	sleep         func(context.Context, time.Duration) bool
	lg            *log.Logger
}

func runSeedFinish(args []string) error {
	fs := flag.NewFlagSet("seed-finish", flag.ExitOnError)
	pg := fs.String("pg", "", "direct Postgres URL of the seeded dispatch database (required)")
	timeout := fs.Duration("timeout", 15*time.Minute, "how long each wait may take before it fails")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: loadgen seed-finish -pg <postgres url> [-timeout 15m]")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)
	switch {
	case fs.NArg() > 0:
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	case *pg == "":
		return fmt.Errorf("-pg is required")
	case *timeout <= 0:
		return fmt.Errorf("-timeout must be more than 0, not %s", *timeout)
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, *pg)
	if err != nil {
		return fmt.Errorf("connect to -pg: %w", err)
	}
	defer conn.Close(context.Background())
	f := seedFinisher{
		timeout: *timeout, poll: seedFinishPoll, now: time.Now,
		sleep: func(ctx context.Context, d time.Duration) bool { return sleepUntil(ctx, time.Now().Add(d)) },
		lg:    log.New(os.Stderr, "loadgen seed-finish: ", 0),
	}
	return f.finish(ctx, conn)
}

// finish waits until the outbox has published every seed event, then, when the build has the
// block-index tables, until Dispatch's block-index backfill owes nothing: a seed that still owed
// it would hand every step about 100 s of that backfill inside its window, and production in
// steady state owes none. Then, when the build has request_keys, it deletes the seed's
// idempotency keys and checks that none is left; then it runs vacuum analyze and installs the
// commit-time trigger (commitTriggerSQL).
func (f seedFinisher) finish(ctx context.Context, db pgSession) error {
	f.lg.Print("waiting for the outbox to publish every seed event")
	if err := f.waitZero(ctx, db, sqlUnpublishedEvents, "the outbox still holds %d unpublished events"); err != nil {
		return err
	}
	var indexed bool
	if err := db.QueryRow(ctx, sqlBlockIndexTables).Scan(&indexed); err != nil {
		return fmt.Errorf("%s: %w", sqlBlockIndexTables, err)
	}
	if indexed {
		f.lg.Print("waiting for Dispatch's block-index backfill to finish")
		if err := f.waitZero(ctx, db, sqlBlockIndexOwed, "Dispatch still owes %d block-index entries"); err != nil {
			return err
		}
	} else {
		f.lg.Print("no block-index tables (a build older than migration 0121): no backfill to wait for")
	}
	var keyed bool
	if err := db.QueryRow(ctx, sqlRequestKeysTable).Scan(&keyed); err != nil {
		return fmt.Errorf("%s: %w", sqlRequestKeysTable, err)
	}
	if keyed {
		f.lg.Print("deleting the seed's idempotency keys (request_keys)")
		if _, err := db.Exec(ctx, sqlRequestKeysDel); err != nil {
			return fmt.Errorf("%s: %w", sqlRequestKeysDel, err)
		}
		var left int64
		if err := db.QueryRow(ctx, sqlRequestKeysCount).Scan(&left); err != nil {
			return fmt.Errorf("%s: %w", sqlRequestKeysCount, err)
		}
		if left != 0 {
			return fmt.Errorf("request_keys still holds %d rows after the delete; the template would carry them", left)
		}
	} else {
		f.lg.Print("no request_keys table (a build older than migration 0137): no idempotency keys to delete")
	}
	f.lg.Print("vacuum analyze")
	if _, err := db.Exec(ctx, sqlVacuumAnalyze); err != nil {
		return fmt.Errorf("%s: %w", sqlVacuumAnalyze, err)
	}
	f.lg.Print("installing the commit-time trigger (loadtest.event_commits)")
	if _, err := db.Exec(ctx, commitTriggerSQL); err != nil {
		return fmt.Errorf("install the commit-time trigger (commit-trigger.sql): %w", err)
	}
	f.lg.Print("done")
	return nil
}

// waitZero reads the count query until it is 0. Once it has waited f.timeout and the count is
// still above 0, it fails with pending (a format of that count) and how long it waited.
func (f seedFinisher) waitZero(ctx context.Context, db pgSession, query, pending string) error {
	deadline := f.now().Add(f.timeout)
	for {
		var n int64
		if err := db.QueryRow(ctx, query).Scan(&n); err != nil {
			return fmt.Errorf("%s: %w", query, err)
		}
		if n == 0 {
			return nil
		}
		if !f.now().Before(deadline) {
			return fmt.Errorf(pending+" after %s", n, seconds(f.timeout))
		}
		if !f.sleep(ctx, f.poll) {
			return ctx.Err()
		}
	}
}

// seconds is d as whole seconds, "900 s".
func seconds(d time.Duration) string { return fmt.Sprintf("%.0f s", d.Seconds()) }
