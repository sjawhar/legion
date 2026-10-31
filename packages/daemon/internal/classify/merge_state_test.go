package classify

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// GitHub's verdict on merging a head - its mergeability and its merge state - is a verdict about
// the tree that head carries, not about the commit. So a move the pushes say changed only the
// issue's own handoffs keeps it, the same test that carries an approval across (ApprovalStands),
// and a move they say changed code takes it, since GitHub would check something else at the new
// head and has not been asked yet. A move no recorded push links yet keeps it for now and is
// judged again by the push that links it: the arrival replays with that push (replayRun), so the
// order the webhooks arrive in decides nothing.
func TestTheMergeVerdictFollowsAHandoffMoveAndNotACodeMove(t *testing.T) {
	handoff := record.ClassifiedPush{SHA: "h3", Before: "h2", HandoffOnly: true}
	code := record.ClassifiedPush{SHA: "h3", Before: "h2"}
	forced := record.ClassifiedPush{SHA: "h3", Before: "h2", HandoffOnly: true, Forced: true}
	for _, tc := range []struct {
		name  string
		facts []runFact
		state record.MergeState
	}{
		{"a handoff push, then its head", []runFact{pushed(handoff), arrivesFrom("h2", "h3")}, record.MergeStateClean},
		{"a head, then its handoff push", []runFact{arrivesFrom("h2", "h3"), pushed(handoff)}, record.MergeStateClean},
		{"a code push, then its head", []runFact{pushed(code), arrivesFrom("h2", "h3")}, record.MergeStateUnread},
		{"a head, then its code push", []runFact{arrivesFrom("h2", "h3"), pushed(code)}, record.MergeStateUnread},
		{"a forced handoff push, then its head", []runFact{pushed(forced), arrivesFrom("h2", "h3")}, record.MergeStateUnread},
		{"a head whose push has not arrived", []runFact{arrivesFrom("h2", "h3")}, record.MergeStateClean},
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
