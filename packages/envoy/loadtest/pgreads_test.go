package main

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"
	"time"
)

// unreachablePG is a Postgres URL that refuses every connect.
const unreachablePG = "postgres://postgres@127.0.0.1:1/dispatch?sslmode=disable"

// Each reader of the run's database, given a Postgres it cannot reach, reports the failure instead
// of an empty result that reads as measured: the commit rows, the lock probe, the ungranted-lock
// sampler, the transaction-id probe and the relation names. (The 5 s sampler's pg_stat_activity:
// TestSummarizeResourcesLeavesFailedReadsOutOfTheStats; the Postgres version:
// TestMetaRecordsAFailedPostgresVersionRead.)

func TestReadCommitsNamesTheTableWhenItCannotRead(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rows, err := readCommits(ctx, unreachablePG, 1, []int64{1, 2})
	if err == nil || rows != nil {
		t.Fatalf("readCommits on an unreachable Postgres = %v, %v; want no rows and an error", rows, err)
	}
	if !strings.Contains(err.Error(), "loadtest.event_commits") {
		t.Fatalf("error %q does not name loadtest.event_commits", err)
	}
	// With no write in the window there is no row to read, and nothing depends on one.
	if rows, err := readCommits(ctx, unreachablePG, 1, nil); err != nil || rows != nil {
		t.Fatalf("readCommits with no markers = %v, %v; want nothing read and no error", rows, err)
	}
}

func TestLockProbesCountAFailedConnect(t *testing.T) {
	var logged bytes.Buffer
	lg := log.New(&logged, "", 0)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	now := time.Now()

	lp := &lockProbe{}
	lp.loop(ctx, unreachablePG, now, now, lg)
	ls := &lockSampler{}
	ls.loop(ctx, unreachablePG, now, now, lg)
	lock, ungranted := summarizeLock(lp.samples, lp.errors), summarizeUngranted(ls.samples, ls.errors)
	if lock.Samples != 0 || lock.Errors != 1 || ungranted.Samples != 0 || ungranted.Errors != 1 {
		t.Fatalf("lock probe %d samples %d errors, ungranted-lock sampler %d samples %d errors; want 0 and 1 each",
			lock.Samples, lock.Errors, ungranted.Samples, ungranted.Errors)
	}
	md := renderTables([]stepData{{name: "unreachable", sum: summary{Config: summaryConfig{Step: "unreachable"}, CommitOrderLock: lock, UngrantedLocks: ungranted}}})
	for _, title := range []string{"Commit-order lock", "Ungranted locks"} {
		cells := rowCells(section(md, title), "unreachable")
		if cells == nil || cells[0] != "unreachable (1 failed reads)" || cells[1] != "-" {
			t.Errorf("section %q: the probe that never connected reads %q, want '-' with the failed-reads mark", title, cells)
		}
	}
}

func TestXIDRateReturnsAFailedRead(t *testing.T) {
	var logged bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	now := time.Now()
	rate, err := xidRate(ctx, unreachablePG, now, now, log.New(&logged, "", 0))
	if rate != nil || err == nil {
		t.Fatalf("xidRate on an unreachable Postgres = %v, %v; want no rate and an error", rate, err)
	}
	if !strings.Contains(logged.String(), err.Error()) {
		t.Fatalf("run.log does not hold the error %q:\n%s", err, logged.String())
	}
}

// A relation in another database that resolveRelations cannot name keeps a visible target,
// <database>:<oid>, never an empty one.
func TestResolveRelationsFallsBackToDatabaseAndOID(t *testing.T) {
	ls := &lockSampler{samples: []ungrantedSample{{waits: []lockWait{
		{locktype: "relation", mode: "AccessShareLock", relDB: "otherdb", relOID: 16384},
		{locktype: "advisory", mode: "ExclusiveLock", target: "commit_order_lock"},
	}}}}
	ls.resolveRelations(unreachablePG)
	if got := ls.samples[0].waits[0].target; got != "otherdb:16384" {
		t.Fatalf("the relation that could not be named has target %q, want otherdb:16384", got)
	}
	if got := ls.samples[0].waits[1].target; got != "commit_order_lock" {
		t.Fatalf("a named target changed to %q", got)
	}
}
