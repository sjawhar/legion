package metrics

import "testing"

// The three tests here are dispatch://INFRA-1077's acceptance A2, which ran the shipped metrics
// over a copy of production Dispatch's INFRA events (dispatch://INFRA-979's
// flow979-corpus-tar-gz) beside an independent reading of the Design. Metric 1 agreed to the last
// digit on all 289 finished issues. Metric 3 did not: the route answered a median of 1 review
// round and a send-back share of 0.0346 where the corpus's own status history gives 3 and 0.8166,
// and metric 4 called two filings out of the filer's tree that are in it.
//
// Each case below is the smallest fixture that shows the rule, written from the Design.

// TestReviewRoundsStillCountStatusEntriesWhenTheOnlyObservationFollowsTheClose: the first pass of
// the sampler writes one `phase` observation for every issue the daemon's state names, and the
// daemon's state names every issue it has recorded, closed ones included — in the A2 corpus all
// 289 finished issues carry exactly one mark, `"done"`, stamped after they closed. `reviewRounds`
// gates on whether the issue carries any `phase` observation at all, so that one trailing mark
// switches the status fallback off for the issue's whole life; `observedPhases` meanwhile drops
// the mark (its span runs from the mark to the close, which is already past, so it has no
// duration) and hands back the event-log fallback, which holds one `reviewing` span at most. The
// Design's rule is temporal — "entries into `reviewing`, or into status `needs_review` before
// observations" — so an issue whose observations begin after it closed has none for any hour of
// its life and its rounds are its status entries.
func TestReviewRoundsStillCountStatusEntriesWhenTheOnlyObservationFollowsTheClose(t *testing.T) {
	issue := newIssue("INFRA-1").worked("implementer", hour(0)).closedAt(hour(8)).
		statusAt(hour(0), "in_progress").
		statusAt(hour(2), "needs_review").
		statusAt(hour(3), "in_progress").
		statusAt(hour(5), "needs_review").
		statusAt(hour(6), "in_progress").
		statusAt(hour(7), "needs_review").
		statusAt(hour(8), "done").
		// The one mark the first sampler pass leaves on an issue that closed hours ago.
		observeString("phase", hour(12), "done")

	report := Compute(fixture(hour(-24), hour(24), issue), ScopeAll, hour(24))

	row := report.Rework.Issues[0]
	if row.ReviewRounds != 3 {
		t.Errorf("review rounds = %d, want 3: the issue has three entries into needs_review and no observation of any hour of its life",
			row.ReviewRounds)
	}
	if report.Rework.MedianReviewRounds != 3 {
		t.Errorf("median review rounds = %g, want 3", report.Rework.MedianReviewRounds)
	}
}

// TestASendBackIsCountedFromTheStatusHistoryBeforeObservations: "the share of issues sent back at
// least once (a move from `testing`, `reviewing` or `accepting` to `implementing`)". Before
// observations the Design reads those phases off the status the workflow wrote, as it does for
// the review rounds beside them — `needs_review` is the status of both reviewing and accepting,
// and `testing` is its own. `sentBackOnce` reads the phase timeline alone, and the event-log
// fallback places one span per phase in workflow order, so no fallback timeline can hold a
// send-back: over the A2 corpus 219 of 289 finished issues record a `needs_review` →
// `in_progress` move while only 10 showed one in the timeline.
func TestASendBackIsCountedFromTheStatusHistoryBeforeObservations(t *testing.T) {
	sentBack := newIssue("INFRA-1").worked("implementer", hour(0)).closedAt(hour(9)).
		statusAt(hour(0), "in_progress").
		statusAt(hour(2), "testing").
		statusAt(hour(3), "needs_review").
		// The reviewer asked for changes: back to implementing.
		statusAt(hour(4), "in_progress").
		statusAt(hour(6), "testing").
		statusAt(hour(7), "needs_review").
		statusAt(hour(9), "done")
	straight := newIssue("INFRA-2").worked("implementer", hour(0)).closedAt(hour(9)).
		statusAt(hour(0), "in_progress").
		statusAt(hour(2), "testing").
		statusAt(hour(3), "needs_review").
		statusAt(hour(9), "done")

	report := Compute(fixture(hour(-24), hour(24), sentBack, straight), ScopeAll, hour(24))

	for _, row := range report.Rework.Issues {
		want := row.Key == "INFRA-1"
		if row.SentBack != want {
			t.Errorf("%s sent_back = %t, want %t", row.Key, row.SentBack, want)
		}
	}
	if report.Rework.SentBackShare != 0.5 {
		t.Errorf("sent-back share = %g, want 0.5: one of the two issues went back to implementing after a review",
			report.Rework.SentBackShare)
	}
}

// TestTheHoursAfterTheFirstReviewReadTheStatusHistoryBeforeObservations: metric 3's table "adds
// the hours of implementing and testing after the first review". Those hours are read off the
// phase timeline, and the pre-observation fallback places one span per phase in workflow order,
// so implementing and testing both end before reviewing begins and the intersection is empty.
// The review rounds and the send-back beside them now read the Dispatch status history, which
// is the spec's third Rule detail; the hours did not move with them, and over the A2 corpus that
// leaves 50.04 h of testing after a first review across 289 finished issues of which 236 were
// sent back at least once. The fixture below is one of those issues.
func TestTheHoursAfterTheFirstReviewReadTheStatusHistoryBeforeObservations(t *testing.T) {
	issue := newIssue("INFRA-1").worked("implementer", hour(0)).closedAt(hour(9)).
		statusAt(hour(0), "in_progress").
		statusAt(hour(2), "testing").
		// The first review.
		statusAt(hour(3), "needs_review").
		// Sent back: two hours of implementing and one of testing follow it.
		statusAt(hour(4), "in_progress").
		statusAt(hour(6), "testing").
		statusAt(hour(7), "needs_review").
		statusAt(hour(9), "done").
		observeString("phase", hour(12), "done")

	report := Compute(fixture(hour(-24), hour(24), issue), ScopeAll, hour(24))

	row := report.Rework.Issues[0]
	if row.ImplementingHoursAfterFirst != 2 {
		t.Errorf("implementing hours after the first review = %g, want 2: the issue was in in_progress from hour 4 to hour 6, after the review at hour 3",
			row.ImplementingHoursAfterFirst)
	}
	if row.TestingHoursAfterFirst != 1 {
		t.Errorf("testing hours after the first review = %g, want 1: the issue was in testing from hour 6 to hour 7",
			row.TestingHoursAfterFirst)
	}
}

// TestAFilingUnderAGrandchildOfTheFilersRootIsInTheTree: metric 4 classes each filing "by the
// parent it had at its `issue.created` event", into "a child of the filer's tree" or "any other
// issue an architect or phase worker filed", and the Design's terms make a tree "a root with the
// issues under it" at any depth. Dispatch's `issue.created` payload carries `ancestors: []` for
// an issue filed under a child, so a classification that reads the event's parent and its
// ancestor list alone cannot see the chain: in the A2 corpus INFRA-566 and INFRA-638, filed by
// `Legion architect · INFRA-115` under INFRA-242, whose own parent is INFRA-115, were called out
// of the tree.
func TestAFilingUnderAGrandchildOfTheFilersRootIsInTheTree(t *testing.T) {
	root := newIssue("INFRA-1").
		worked("architect", hour(0)).
		worked("implementer", hour(5)).
		closedAt(hour(20))
	child := newIssue("INFRA-2").under("INFRA-1")
	grandchild := newIssue("INFRA-3").under("INFRA-2")
	population := fixture(hour(-24), hour(48), root, child, grandchild)
	population.Filed = []Filed{
		// Filed under the filer's child, after the filer's first implementer event, with the
		// empty ancestor list Dispatch's own event carries.
		{Key: "INFRA-3", At: hour(10), Filer: SessionTitle{Role: "architect", Issue: "INFRA-1"}, Parent: "INFRA-2", Ancestors: nil},
	}

	report := Compute(population, ScopeAll, hour(48))

	if len(report.Splitting.Filed) != 1 {
		t.Fatalf("filed rows = %d, want 1", len(report.Splitting.Filed))
	}
	if class := report.Splitting.Filed[0].Class; class != ClassInTreeLater {
		t.Errorf("INFRA-3 is classed %q, want %q: it is under INFRA-2, which is under the filer's own issue",
			class, ClassInTreeLater)
	}
}
