package main

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

const msNs = int64(1e6)

func TestJoinDelivery(t *testing.T) {
	writes := []writeRec{
		// A plain comment answered 201. Each committed write was sent 10 ms before it arrived.
		{Marker: 1, Kind: kindComment, Issue: "LGA-1", InWindow: true, Sent: 990 * msNs, Arrival: 1000 * msNs, Response: 1100 * msNs, Status: 201},
		// An edit that hit the client timeout, but committed.
		{Marker: 2, Kind: kindEdit, Issue: "LGA-2", InWindow: true, Sent: 1990 * msNs, Arrival: 2000 * msNs, Response: 32000 * msNs, TransportErr: "context deadline exceeded"},
		// An anchored comment whose transaction also appended a document snapshot (id 30), and
		// carried its marker on two comment.created rows (31, 32): 31 is the primary event.
		{Marker: 3, Kind: kindAnchored, Issue: "LGA-1", InWindow: true, Sent: 2990 * msNs, Arrival: 3000 * msNs, Response: 3400 * msNs, Status: 201},
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
	wantSent := stat{N: 7, P50: 310, P90: 610, P99: 610, Max: 610} // 110 110 210 310 610 610 610
	if got.DeliveryMsFromSent != wantSent {
		t.Errorf("delivery from sent %+v, want %+v", got.DeliveryMsFromSent, wantSent)
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

// TestReadCommitsNamesTheTableWhenItCannotRead: given a Postgres it cannot reach, readCommits
// returns no rows and an error naming loadtest.event_commits, never an empty result that reads as
// no commits; with no write in the window it reads nothing and fails nothing.
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

// unitWindow is one unit session's life, as a session that started a second into the step and
// stopped nine seconds in recorded it: its subscribe went out at 1,000 ms and was acknowledged at
// 1,100 ms, its unsubscribe at 9,000 ms and 9,100 ms.
func unitWindow() sessionWindow {
	return sessionWindow{Class: deliveryClassUnit, ID: "ses-lg-unit-00001", Issues: []string{"LOOP-1"},
		SubSent: 1000 * msNs, SubAck: 1100 * msNs, UnsubSent: 9000 * msNs, UnsubAck: 9100 * msNs}
}

// loopWrite is one committed comment on issue, with the commit time the case places in a
// session's life, and the receipts of its event by the sessions that took it.
func loopWrite(marker, event int64, issue string, commitNs int64) ([]writeRec, []commitRow) {
	return []writeRec{{Marker: marker, Kind: kindComment, Issue: issue, InWindow: true,
			Sent: commitNs - 20*msNs, Arrival: commitNs - 10*msNs, Response: commitNs + 10*msNs, Status: 201}},
		[]commitRow{{EventID: event, Type: primaryEventType[kindComment], Marker: marker, CommitNs: commitNs}}
}

// TestJoinDeliveryExpectationRule: a loop session is owed a write that committed after the
// listener acknowledged its subscribe and more than the receipt deadline before it acknowledged
// its unsubscribe. Every other place in its life is its own column: before the subscribe went out
// and after the unsubscribe landed it was owed nothing, and inside either edge the write is a
// boundary, which is neither expected nor lost.
func TestJoinDeliveryExpectationRule(t *testing.T) {
	const deadline = time.Second
	for _, c := range []struct {
		name     string
		commitNs int64
		received bool
		want     classDelivery
	}{
		{"committed before the subscribe went out", 900 * msNs, false, classDelivery{Sessions: 1}},
		{"committed while the subscribe was in flight", 1050 * msNs, false, classDelivery{Sessions: 1, Boundary: 1}},
		{"committed after the subscribe and well before the unsubscribe, and lost", 5000 * msNs, false,
			classDelivery{Sessions: 1, Expected: 1, Lost: 1}},
		{"committed after the subscribe and well before the unsubscribe, and received", 5000 * msNs, true,
			classDelivery{Sessions: 1, Expected: 1, Received: 1}},
		{"committed inside the receipt deadline of the unsubscribe", 8500 * msNs, false,
			classDelivery{Sessions: 1, Boundary: 1}},
		{"committed after the unsubscribe was acknowledged", 9200 * msNs, false, classDelivery{Sessions: 1}},
	} {
		t.Run(c.name, func(t *testing.T) {
			writes, commits := loopWrite(1, 50, "LOOP-1", c.commitNs)
			var receipts []receipt
			if c.received {
				receipts = []receipt{{EventID: 50, Recv: c.commitNs + 100*msNs, Session: unitWindow().ID}}
			}
			got := deliveryJoin{writes: writes, commits: commits, receipts: receipts,
				windows: []sessionWindow{unitWindow()}, deadline: deadline, durationS: 10}.join()
			if unit := got.ByClass[deliveryClassUnit]; unit != c.want {
				t.Fatalf("unit class %+v, want %+v", unit, c.want)
			}
			// The step's own totals follow the rule: what the sessions were owed is expected, and
			// what never arrived is missing.
			if got.ExpectedIssueTopicReceipts != c.want.Expected || got.Missing != c.want.Lost {
				t.Fatalf("expected %d missing %d, want %d and %d",
					got.ExpectedIssueTopicReceipts, got.Missing, c.want.Expected, c.want.Lost)
			}
		})
	}
}

// TestJoinDeliveryCountsASecondReceiptAsADuplicate: one session taking one event twice is one
// receipt and one duplicate, and the delivery latency is the first receipt's alone.
func TestJoinDeliveryCountsASecondReceiptAsADuplicate(t *testing.T) {
	w := unitWindow()
	writes, commits := loopWrite(1, 50, "LOOP-1", 5000*msNs)
	receipts := []receipt{
		{EventID: 50, Recv: 5200 * msNs, Session: w.ID},
		{EventID: 50, Recv: 5900 * msNs, Session: w.ID},
	}

	got := deliveryJoin{writes: writes, commits: commits, receipts: receipts,
		windows: []sessionWindow{w}, deadline: time.Second, durationS: 10}.join()

	want := classDelivery{Sessions: 1, Expected: 1, Received: 1, Duplicate: 1}
	if unit := got.ByClass[deliveryClassUnit]; unit != want {
		t.Fatalf("unit class %+v, want %+v", unit, want)
	}
	if got.IssueTopicReceipts != 1 || got.Missing != 0 {
		t.Fatalf("receipts %d missing %d, want 1 and 0: the duplicate is no second receipt", got.IssueTopicReceipts, got.Missing)
	}
	if from := got.DeliveryMsFromCommit; from.N != 1 || from.Max != 200 {
		t.Fatalf("delivery from commit %+v, want one sample of 200 ms, the first receipt's", from)
	}
}

// TestJoinDeliveryCountsTheClassesApart: a write both a unit session and the driver session
// follow is expected twice, once in each class, and each class's loss is its own.
func TestJoinDeliveryCountsTheClassesApart(t *testing.T) {
	unit := unitWindow()
	driver := sessionWindow{Class: deliveryClassDriver, ID: "ses-lg-driver", Issues: []string{"LOOP-1", "LOOP-2"},
		SubSent: 10 * msNs, SubAck: 20 * msNs}
	writes, commits := loopWrite(1, 50, "LOOP-1", 5000*msNs)
	other, otherCommits := loopWrite(2, 60, "LOOP-2", 6000*msNs)
	receipts := []receipt{
		{EventID: 50, Recv: 5100 * msNs, Session: driver.ID},
		{EventID: 60, Recv: 6100 * msNs, Session: driver.ID},
	}

	got := deliveryJoin{writes: append(writes, other...), commits: append(commits, otherCommits...),
		receipts: receipts, windows: []sessionWindow{unit, driver}, deadline: time.Second, durationS: 10}.join()

	wantUnit := classDelivery{Sessions: 1, Expected: 1, Lost: 1}
	wantDriver := classDelivery{Sessions: 1, Expected: 2, Received: 2}
	if u := got.ByClass[deliveryClassUnit]; u != wantUnit {
		t.Errorf("unit class %+v, want %+v: the unit followed LOOP-1 alone and took nothing", u, wantUnit)
	}
	if d := got.ByClass[deliveryClassDriver]; d != wantDriver {
		t.Errorf("driver class %+v, want %+v: the driver followed both issues and took both", d, wantDriver)
	}
	if got.ExpectedIssueTopicReceipts != 3 || got.IssueTopicReceipts != 2 || got.Missing != 1 {
		t.Fatalf("expected %d receipts %d missing %d, want 3, 2 and 1",
			got.ExpectedIssueTopicReceipts, got.IssueTopicReceipts, got.Missing)
	}
}

// TestJoinDeliveryWithoutSessionsCarriesNoClass: a step that ran the flat population alone counts
// what it always has, and summary.json carries no by_class at all.
func TestJoinDeliveryWithoutSessionsCarriesNoClass(t *testing.T) {
	writes, commits := loopWrite(1, 50, "LGA-1", 5000*msNs)
	got := joinDelivery(writes, commits, []receipt{{EventID: 50, Recv: 5100 * msNs}}, map[string]int{"LGA-1": 2}, 10)
	if got.ByClass != nil {
		t.Fatalf("by_class %+v, want none", got.ByClass)
	}
	if got.ExpectedIssueTopicReceipts != 2 || got.IssueTopicReceipts != 1 || got.Missing != 1 {
		t.Fatalf("expected %d receipts %d missing %d, want 2, 1 and 1", got.ExpectedIssueTopicReceipts, got.IssueTopicReceipts, got.Missing)
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "by_class") {
		t.Fatalf("a step with no loop session records a class:\n%s", b)
	}
}
