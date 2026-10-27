package main

import (
	"slices"
	"testing"
)

const msNs = int64(1e6)

func TestJoinDelivery(t *testing.T) {
	writes := []writeRec{
		// A plain comment answered 201.
		{Marker: 1, Kind: kindComment, Issue: "LGA-1", InWindow: true, Start: 990 * msNs, Arrival: 1000 * msNs, Response: 1100 * msNs, Status: 201},
		// An edit that hit the client timeout, but committed.
		{Marker: 2, Kind: kindEdit, Issue: "LGA-2", InWindow: true, Start: 2000 * msNs, Arrival: 2000 * msNs, Response: 32000 * msNs, TransportErr: "context deadline exceeded"},
		// An anchored comment whose transaction also appended a document snapshot (id 30), and
		// carried its marker on two comment.created rows (31, 32): 31 is the primary event.
		{Marker: 3, Kind: kindAnchored, Issue: "LGA-1", InWindow: true, Start: 3000 * msNs, Arrival: 3000 * msNs, Response: 3400 * msNs, Status: 201},
		// Before the window: neither offered nor timed.
		{Marker: 4, Kind: kindComment, Issue: "LGA-1", InWindow: false, Arrival: 10 * msNs, Response: 20 * msNs, Status: 201},
		// A reply refused with 500 that committed nothing.
		{Marker: 5, Kind: kindReply, Issue: "LGA-2", InWindow: true, Arrival: 5000 * msNs, Response: 5100 * msNs, Status: 500},
	}
	commits := []commitRow{
		{EventID: 10, Type: "comment.created", Marker: 1, CommitNs: 1050 * msNs},
		{EventID: 20, Type: "artifact.version", Marker: 2, CommitNs: 2500 * msNs},
		{EventID: 30, Type: "artifact.version", Marker: 3, CommitNs: 3020 * msNs},
		{EventID: 32, Type: "comment.created", Marker: 3, CommitNs: 3020 * msNs},
		{EventID: 31, Type: "comment.created", Marker: 3, CommitNs: 3020 * msNs},
		{EventID: 40, Type: "comment.created", Marker: 4, CommitNs: 15 * msNs},
	}
	receipts := []receipt{
		{EventID: 10, Recv: 1200 * msNs}, {EventID: 10, Recv: 1300 * msNs}, {EventID: 10, Recv: 1250 * msNs, Agent: true},
		{EventID: 20, Recv: 2600 * msNs}, {EventID: 20, Recv: 2600 * msNs}, {EventID: 20, Recv: 2600 * msNs},
		{EventID: 31, Recv: 3100 * msNs}, {EventID: 31, Recv: 3100 * msNs},
		{EventID: 30, Recv: 3050 * msNs}, {EventID: 32, Recv: 3060 * msNs},
		{EventID: 40, Recv: 30 * msNs},
		{EventID: 99, Recv: 4000 * msNs},
	}
	watchers := map[string]int{"LGA-1": 2, "LGA-2": 3}

	got := joinDelivery(writes, commits, receipts, watchers, 10)

	if got.Offered != 4 || got.Committed != 3 {
		t.Fatalf("offered %d committed %d, want 4 and 3 (the timed-out edit committed; the 500 did not)", got.Offered, got.Committed)
	}
	if edit := got.ByKind[kindEdit]; edit.Committed != 1 || edit.Offered != 1 {
		t.Errorf("edit committed %d of %d, want 1 of 1", edit.Committed, edit.Offered)
	}
	if reply := got.ByKind[kindReply]; reply.Committed != 0 || reply.Offered != 1 {
		t.Errorf("reply committed %d of %d, want 0 of 1", reply.Committed, reply.Offered)
	}
	if got.ExpectedIssueTopicReceipts != 7 || got.IssueTopicReceipts != 7 || got.Missing != 0 {
		t.Errorf("receipts got %d expected %d missing %d, want 7, 7, 0", got.IssueTopicReceipts, got.ExpectedIssueTopicReceipts, got.Missing)
	}
	if got.AgentTopicReceipts != 1 {
		t.Errorf("agent-topic receipts %d, want 1", got.AgentTopicReceipts)
	}
	if got.OtherEvents != 2 || got.OtherReceipts != 2 {
		t.Errorf("other events %d receipts %d, want 2 and 2 (the snapshot and the second comment.created)", got.OtherEvents, got.OtherReceipts)
	}
	if got.WriteErrors.Transport != 1 || got.WriteErrors.Non2xx != 1 {
		t.Errorf("write errors %+v, want 1 transport and 1 non-2xx", got.WriteErrors)
	}
	wantArrival := stat{N: 7, P50: 300, P90: 600, P99: 600, Max: 600} // 100 100 200 300 600 600 600
	if got.DeliveryMsFromArrival != wantArrival {
		t.Errorf("delivery from arrival %+v, want %+v", got.DeliveryMsFromArrival, wantArrival)
	}
	wantCommit := stat{N: 7, P50: 100, P90: 150, P99: 150, Max: 250} // 80 80 100 100 100 150 250
	if got.DeliveryMsFromCommit != wantCommit {
		t.Errorf("delivery from commit %+v, want %+v", got.DeliveryMsFromCommit, wantCommit)
	}
	if anchored := got.ByKind[kindAnchored].DeliveryMsFromCommit; anchored.N != 2 || anchored.Max != 80 {
		t.Errorf("anchored delivery from commit %+v, want two receipts of event 31 at 80 ms", anchored)
	}
	if comment := got.ByKind[kindComment].WriteMs; comment.N != 1 || comment.Max != 100 {
		t.Errorf("comment write ms %+v, want one 2xx write of 100 ms from arrival", comment)
	}
	if got.ClockAnomalies != 0 {
		t.Errorf("clock anomalies %d, want 0", got.ClockAnomalies)
	}
	if got.CommittedPerS != 0.3 {
		t.Errorf("committed per s %g, want 0.3", got.CommittedPerS)
	}
}

func TestJoinDeliveryCountsClockAnomalies(t *testing.T) {
	writes := []writeRec{{Marker: 7, Kind: kindUpload, Issue: "LGB-3", InWindow: true, Arrival: 5000 * msNs, Response: 5200 * msNs, Status: 201}}
	commits := []commitRow{{EventID: 70, Type: "artifact.version", Marker: 7, CommitNs: 4990 * msNs}}
	receipts := []receipt{{EventID: 70, Recv: 5300 * msNs}, {EventID: 70, Recv: 5310 * msNs}}

	got := joinDelivery(writes, commits, receipts, map[string]int{"LGB-3": 2}, 1)

	if got.ClockAnomalies != 2 {
		t.Fatalf("clock anomalies %d, want 2: both receipts' event committed before its write arrived", got.ClockAnomalies)
	}
	if !slices.Equal([]int{got.IssueTopicReceipts, got.ExpectedIssueTopicReceipts}, []int{2, 2}) {
		t.Fatalf("receipts got %d expected %d, want 2 and 2", got.IssueTopicReceipts, got.ExpectedIssueTopicReceipts)
	}
}
