package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// fakeSeedDB answers seed-finish's queries: each count query its counts in turn (the last one
// again once they run out), the block-index table check `indexed`, the request_keys table check
// `keyed`. ran is every statement in order.
type fakeSeedDB struct {
	counts  map[string][]int64
	indexed bool
	keyed   bool
	ran     []string
}

type fakeRow struct{ v any }

func (r fakeRow) Scan(dest ...any) error {
	switch d := dest[0].(type) {
	case *int64:
		*d = r.v.(int64)
	case *bool:
		*d = r.v.(bool)
	default:
		return fmt.Errorf("fakeRow: cannot scan into %T", d)
	}
	return nil
}

func (db *fakeSeedDB) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	db.ran = append(db.ran, sql)
	switch sql {
	case sqlBlockIndexTables:
		return fakeRow{db.indexed}
	case sqlRequestKeysTable:
		return fakeRow{db.keyed}
	}
	seq := db.counts[sql]
	if len(seq) > 1 {
		db.counts[sql] = seq[1:]
	}
	return fakeRow{seq[0]}
}

func (db *fakeSeedDB) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	db.ran = append(db.ran, sql)
	return pgconn.CommandTag{}, nil
}

// fakeFinisher is a seedFinisher on a fake clock that each sleep advances; it logs into logged.
func fakeFinisher(timeout time.Duration, logged *bytes.Buffer) (*seedFinisher, *time.Time) {
	clock := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	f := &seedFinisher{
		timeout: timeout, poll: seedFinishPoll,
		now:   func() time.Time { return clock },
		sleep: func(_ context.Context, d time.Duration) bool { clock = clock.Add(d); return true },
		lg:    log.New(logged, "", 0),
	}
	return f, &clock
}

// count is how many times sql is in ran.
func count(ran []string, sql string) int {
	n := 0
	for _, s := range ran {
		if s == sql {
			n++
		}
	}
	return n
}

// TestSeedFinishWaitsForTheOutboxAndTheBackfillThenVacuumsAndInstallsTheTrigger: each wait polls
// every 2 s until its count is 0, in the shell's order, and the phases are logged.
func TestSeedFinishWaitsForTheOutboxAndTheBackfillThenVacuumsAndInstallsTheTrigger(t *testing.T) {
	var logged bytes.Buffer
	f, clock := fakeFinisher(15*time.Minute, &logged)
	start := *clock
	db := &fakeSeedDB{indexed: true, keyed: true, counts: map[string][]int64{sqlUnpublishedEvents: {5, 3, 0}, sqlBlockIndexOwed: {7, 0}, sqlRequestKeysCount: {0}}}
	if err := f.finish(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	want := []string{sqlUnpublishedEvents, sqlUnpublishedEvents, sqlUnpublishedEvents, sqlBlockIndexTables, sqlBlockIndexOwed, sqlBlockIndexOwed,
		sqlRequestKeysTable, sqlRequestKeysDel, sqlRequestKeysCount, sqlVacuumAnalyze, commitTriggerSQL}
	if !slices.Equal(db.ran, want) {
		t.Fatalf("statements run %q, want %q", db.ran, want)
	}
	if waited := clock.Sub(start); waited != 3*seedFinishPoll {
		t.Errorf("waited %s, want three polls of 2 s", waited)
	}
	for _, phase := range []string{"waiting for the outbox", "block-index backfill", "idempotency keys", "vacuum analyze", "installing the commit-time trigger"} {
		if !strings.Contains(logged.String(), phase) {
			t.Errorf("the log lacks %q:\n%s", phase, logged.String())
		}
	}
}

// TestSeedFinishSkipsTheBackfillWaitOnABuildWithoutItsTables: a build older than migration 0121 has
// no block-index tables, so seed-finish never queries them.
func TestSeedFinishSkipsTheBackfillWaitOnABuildWithoutItsTables(t *testing.T) {
	var logged bytes.Buffer
	f, _ := fakeFinisher(15*time.Minute, &logged)
	db := &fakeSeedDB{counts: map[string][]int64{sqlUnpublishedEvents: {0}}}
	if err := f.finish(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if count(db.ran, sqlBlockIndexOwed) != 0 || count(db.ran, sqlRequestKeysDel) != 0 || count(db.ran, sqlVacuumAnalyze) != 1 || count(db.ran, commitTriggerSQL) != 1 {
		t.Fatalf("statements run %q: want no block-index count, no key delete, one vacuum and one trigger install", db.ran)
	}
}

// TestSeedFinishRefusesATemplateThatStillHoldsIdempotencyKeys: keys left after the delete would
// be swept inside a later step's window, so seed-finish fails before vacuum and the trigger.
func TestSeedFinishRefusesATemplateThatStillHoldsIdempotencyKeys(t *testing.T) {
	var logged bytes.Buffer
	f, _ := fakeFinisher(15*time.Minute, &logged)
	db := &fakeSeedDB{keyed: true, counts: map[string][]int64{sqlUnpublishedEvents: {0}, sqlRequestKeysCount: {3}}}
	err := f.finish(context.Background(), db)
	if err == nil || !strings.Contains(err.Error(), "request_keys still holds 3 rows") {
		t.Fatalf("error %v, want the 3 rows request_keys still holds named", err)
	}
	if count(db.ran, sqlVacuumAnalyze)+count(db.ran, commitTriggerSQL) != 0 {
		t.Errorf("vacuum or the trigger ran after the keys were left: %q", db.ran)
	}
}

// TestSeedFinishFailsAfterTheTimeoutNamingWhatIsPending: each wait fails once it has waited
// -timeout, saying which wait and how many are still pending, and nothing after it runs.
func TestSeedFinishFailsAfterTheTimeoutNamingWhatIsPending(t *testing.T) {
	for _, c := range []struct {
		counts  map[string][]int64
		want    string
		pending string
	}{
		{map[string][]int64{sqlUnpublishedEvents: {4}}, "the outbox still holds 4 unpublished events after 10 s", sqlUnpublishedEvents},
		{map[string][]int64{sqlUnpublishedEvents: {0}, sqlBlockIndexOwed: {9}}, "Dispatch still owes 9 block-index entries after 10 s", sqlBlockIndexOwed},
	} {
		var logged bytes.Buffer
		f, clock := fakeFinisher(10*time.Second, &logged)
		start := *clock
		db := &fakeSeedDB{indexed: true, counts: c.counts}
		err := f.finish(context.Background(), db)
		if err == nil || err.Error() != c.want {
			t.Errorf("error %v, want %q", err, c.want)
		}
		if waited := clock.Sub(start); waited < 10*time.Second || waited > 10*time.Second+seedFinishPoll {
			t.Errorf("%s: failed after %s of waiting, want 10 s", c.want, waited)
		}
		// Polls at 0, 2, ..., 10 s: six reads of the pending count.
		if n := count(db.ran, c.pending); n != 6 {
			t.Errorf("%s: read the count %d times, want 6", c.want, n)
		}
		if count(db.ran, sqlVacuumAnalyze)+count(db.ran, commitTriggerSQL) != 0 {
			t.Errorf("%s: vacuum or the trigger ran after the wait failed: %q", c.want, db.ran)
		}
	}
}
