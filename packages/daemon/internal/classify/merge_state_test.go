package classify

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// GitHub's verdict on merging a head - its mergeability and its merge state - is its answer about
// the tree that head carries, so a move to another head leaves nothing to read: any push may have
// changed the code GitHub answered about, and a verdict of another head must never move an
// approval (dispatch://INFRA-714 R2). A synchronize that moves no head keeps it, since GitHub
// answered about that very tree. The push that links a move judges it again: the arrival replays
// with that push (replayRun), which gives the verdict back from the run's anchor, so the order
// the webhooks arrive in decides nothing once both have arrived.
func TestEveryMoveTakesTheMergeVerdictAndNoMoveKeepsIt(t *testing.T) {
	code := record.ClassifiedPush{SHA: "h3", Before: "h2"}
	tests := record.ClassifiedPush{SHA: "h3", Before: "h2", ByReviewApp: true}
	for _, tc := range []struct {
		name  string
		facts []runFact
		state record.MergeState
	}{
		{"a code push, then its head", []runFact{pushed(code), arrivesFrom("h2", "h3")}, record.MergeStateUnread},
		{"a head, then its code push", []runFact{arrivesFrom("h2", "h3"), pushed(code)}, record.MergeStateUnread},
		{"the review App's push, then its head", []runFact{pushed(tests), arrivesFrom("h2", "h3")}, record.MergeStateUnread},
		{"a head whose push has not arrived", []runFact{arrivesFrom("h2", "h3")}, record.MergeStateUnread},
		{"a synchronize back to the recorded head", []runFact{arrivesFrom("h1", "h2")}, record.MergeStateClean},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pr := record.PullRequest{HeadSHA: "h2", Failing: []string{},
				Mergeability: record.MergeabilityMergeable, MergeState: record.MergeStateClean}
			for _, fact := range tc.facts {
				pr = stored(t, fact.apply(pr))
			}
			wantMergeable := record.MergeabilityMergeable
			if tc.state == record.MergeStateUnread {
				wantMergeable = ""
			}
			if pr.MergeState != tc.state || pr.Mergeability != wantMergeable {
				t.Fatalf("after %v the verdict is %q/%q, want %q/%q", tc.facts, pr.Mergeability, pr.MergeState, wantMergeable, tc.state)
			}
		})
	}
}

// A verdict read while a head run is open is GitHub's answer for the head the run has reached -
// the engine drops a read that answers for any other head - so the push that closes the run must
// not put the anchor's older answer back over it. The anchor gives its verdict back only to a
// record holding none (withJudgement), which is the record a move no push linked yet left.
func TestAVerdictReadWhileAHeadRunIsOpenStandsOverTheAnchors(t *testing.T) {
	pr := record.PullRequest{HeadSHA: "h2", Failing: []string{},
		Mergeability: record.MergeabilityMergeable, MergeState: record.MergeStateClean}
	pr = stored(t, arrivesFrom("h2", "h3").apply(pr))
	if pr.Run == nil || pr.MergeState != record.MergeStateUnread {
		t.Fatalf("after the head arrived before its push the run is open %v with verdict %q, want it open with nothing read", pr.Run != nil, pr.MergeState)
	}
	// The read the arrival asks for, answering for h3 (workflow's askMergeState, mergeability).
	pr.Mergeability, pr.MergeState = record.MergeabilityMergeable, record.MergeStateBlocked

	pr = stored(t, pushed(record.ClassifiedPush{SHA: "h3", Before: "h2"}).apply(pr))
	if pr.Run != nil || pr.MergeState != record.MergeStateBlocked {
		t.Fatalf("after the push closed the run the verdict is %q with the run open %v, want h3's own blocked and the run closed", pr.MergeState, pr.Run != nil)
	}
}
