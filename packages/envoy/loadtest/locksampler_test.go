package main

import (
	"reflect"
	"testing"
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
