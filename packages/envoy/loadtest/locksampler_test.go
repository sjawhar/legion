package main

import (
	"bytes"
	"context"
	"log"
	"reflect"
	"testing"
	"time"
)

func TestSummarizeUngranted(t *testing.T) {
	commit := func(query, blocker string) lockWait {
		return lockWait{locktype: "advisory", target: "commit_order_lock", mode: "ExclusiveLock", query: query, blockerQuery: blocker}
	}
	notify := lockWait{locktype: "object", target: "notify_commit_lock", mode: "AccessExclusiveLock", query: "COMMIT", blockerQuery: "COMMIT"}
	row := lockWait{locktype: "transactionid", target: "row_wait", mode: "ShareLock", query: "update issues", blockerQuery: ""}
	samples := []ungrantedSample{
		{},
		{waits: []lockWait{commit("insert into events", "select pg_advisory_xact_lock"), commit("insert into events", "select pg_advisory_xact_lock"), notify}},
		{waits: []lockWait{commit("insert into events", "COMMIT")}},
		{waits: []lockWait{commit("select pg_advisory_xact_lock", "COMMIT"), commit("insert into events", "COMMIT"), commit("insert into events", "COMMIT"), row}},
	}

	got := summarizeUngranted(samples, 0)

	want := ungrantedSummary{
		Samples:      4,
		ShareWithAny: 0.75,
		Groups: []ungrantedGroup{
			{
				LockType: "advisory", Target: "commit_order_lock", Mode: "ExclusiveLock",
				// Waiters per sample: 0, 2, 1, 3.
				WaitersP50: 1, WaitersMax: 3, ShareWithWaiter: 0.75,
				TopPairs: []waitPair{
					{WaiterQuery: "insert into events", BlockerQuery: "COMMIT", Count: 3},
					{WaiterQuery: "insert into events", BlockerQuery: "select pg_advisory_xact_lock", Count: 2},
					{WaiterQuery: "select pg_advisory_xact_lock", BlockerQuery: "COMMIT", Count: 1},
				},
			},
			// One sample each: the tie orders by lock type.
			{
				LockType: "object", Target: "notify_commit_lock", Mode: "AccessExclusiveLock",
				WaitersP50: 0, WaitersMax: 1, ShareWithWaiter: 0.25,
				TopPairs: []waitPair{{WaiterQuery: "COMMIT", BlockerQuery: "COMMIT", Count: 1}},
			},
			{
				LockType: "transactionid", Target: "row_wait", Mode: "ShareLock",
				WaitersP50: 0, WaitersMax: 1, ShareWithWaiter: 0.25,
				TopPairs: []waitPair{{WaiterQuery: "update issues", BlockerQuery: "", Count: 1}},
			},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("summary\n got %+v\nwant %+v", got, want)
	}
}

func TestSummarizeUngrantedWithNoSamples(t *testing.T) {
	got := summarizeUngranted(nil, 0)
	if got.Samples != 0 || got.ShareWithAny != 0 || got.Groups == nil || len(got.Groups) != 0 {
		t.Fatalf("summary %+v, want 0 samples and an empty, non-nil group list", got)
	}
}

// TestLockSamplerCountsAFailedConnect: an ungranted-lock sampler that never connects has no sample
// and one failed read, and its table row shows "-" with the mark "(1 failed read)".
func TestLockSamplerCountsAFailedConnect(t *testing.T) {
	var logged bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	now := time.Now()
	ls := &lockSampler{}
	ls.loop(ctx, unreachablePG, now, now, log.New(&logged, "", 0))
	ungranted := summarizeUngranted(ls.samples, ls.errors)
	if ungranted.Samples != 0 || ungranted.Errors != 1 {
		t.Fatalf("ungranted-lock sampler %d samples %d errors; want 0 and 1", ungranted.Samples, ungranted.Errors)
	}
	md := renderTables([]stepData{{name: "unreachable", sum: summary{Config: summaryConfig{Step: "unreachable"}, UngrantedLocks: ungranted}}})
	if cells := rowCells(section(md, "Ungranted locks"), "unreachable"); cells == nil || cells[0] != "unreachable (1 failed read)" || cells[1] != "-" {
		t.Errorf("the sampler that never connected reads %q, want '-' with the failed-read mark", cells)
	}
}

// TestResolveRelationsFallsBackToDatabaseAndOID: a relation in another database that
// resolveRelations cannot name keeps a visible target, <database>:<oid>, never an empty one.
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
