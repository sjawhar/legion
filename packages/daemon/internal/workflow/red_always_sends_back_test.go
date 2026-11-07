package workflow

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// redRounds is how many rounds of red CI this proof drives: more than the three the removed
// `max_fix_attempts` default allowed, so a cap of any shape would have stopped the work before the
// last round.
const redRounds = 6

// Every red CI verdict sends the work back to the implementer, however many came before it: the
// daemon counts no fix attempts and caps none since dispatch://INFRA-956 R4, so nothing marks the
// pull request blocked and nothing leaves the issue standing where only an edit of the store would
// move it. The proof drives six rounds on one pull request — the implementer pushes a head and
// completes, CI settles red there — and holds that each round ends in implementing with the
// architect told one more checks-red and no pr-blocked at all.
func TestEveryRedSendsTheWorkBackHoweverManyCameBefore(t *testing.T) {
	const required = "pr-checks-result"
	pool := migratedPool(t)
	ctx := context.Background()
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U"})
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
		Number: 42, Branch: "legion/LEGION-208", HeadSHA: "h0", Required: []string{required}})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
	// The review round cap is a rule of its own and still stands: it is set above this drive so
	// the only notice a round can earn here is the red's own.
	engine := New(record.NewStore(), Config{Project: "LEGION", Steps: config.DesignGateRootIssues.Steps(), ReviewRoundCap: redRounds + 1,
		Linger: time.Hour, ReviewAppLogin: testReviewApp, Repositories: testRepositories,
		Clock: func() time.Time { return time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC) }}, nil)

	for round := 1; round <= redRounds; round++ {
		head := fmt.Sprintf("h%d", round)
		// The implementer pushes its fix and completes, which hands the work to the tester.
		applyRefusingNothing(t, pool, engine, syncedTo(head))
		applyRefusingNothing(t, pool, engine, pushedFromTo(fmt.Sprintf("h%d", round-1), head, "legion-implementer[bot]"))
		if _, err := intake.ApplyFact(ctx, pool, "api", fmt.Sprintf("implement-%d", round),
			intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim",
				Commit: head, Summary: fmt.Sprintf("fix %d", round), HandoffRecord: fmt.Sprintf("implement-%d", round), HandoffVersion: 1}, engine); err != nil {
			t.Fatalf("round %d: apply the implementer's completion: %v", round, err)
		}
		if got := issuePhase(t, pool); got != phase.Testing {
			t.Fatalf("round %d: after the implementer's completion the issue is in %s, want testing", round, got)
		}

		applyRefusingNothing(t, pool, engine, settledAt(head, required, round, required))

		if got := issuePhase(t, pool); got != phase.Implementing {
			t.Fatalf("round %d: after the red at %s the issue is in %s, want implementing: a red sends the work back however many came before it", round, head, got)
		}
		checksRed, blocked := 0, 0
		for _, kind := range noticeKinds(t, pool, "LEGION-208") {
			switch kind {
			case "checks-red":
				checksRed++
			case "pr-blocked":
				blocked++
			}
		}
		if checksRed != round || blocked != 0 {
			t.Fatalf("round %d: the architect was told %d checks-red and %d pr-blocked notices, want %d and none", round, checksRed, blocked, round)
		}
		if posted := messageBodies(t, pool); len(posted) != 0 {
			t.Fatalf("round %d: Dispatch messages %q, want none: no count is posted on the issue", round, posted)
		}
		if rounds := implementerRounds(t, pool); rounds != round {
			t.Fatalf("round %d: the implementer's row counts %d returns, want %d", round, rounds, round)
		}
	}
}
