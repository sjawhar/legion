package intake

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// mustRenames is the equivalence pairs configure, for a test that configures a pair it wrote.
func mustRenames(t *testing.T, pairs ...ghrepo.Rename) ghrepo.Renames {
	t.Helper()
	renames, err := ghrepo.NewRenames(pairs)
	if err != nil {
		t.Fatalf("NewRenames(%v): %v", pairs, err)
	}
	return renames
}

// A review or a checks settlement of a pull request already open when its repository was
// transferred is one the daemon records: the row keeps sjawhar/legion, the name the pull request
// opened under, while the event names cybermonklabs/legion, so the read asks under both names of
// the configured pair (ghrepo.Renames.Lookups), as the workflow reads the row. Without the pair
// the fact names a pull request the daemon does not record, which is acknowledged with no
// transaction and no GitHub read; so does a number neither name records. A fact that is no
// review and no settlement is recorded whatever it names.
func TestAReviewOfAPullRequestRecordedUnderTheOtherNameOfARenameIsRecorded(t *testing.T) {
	pool := migratedPool(t)
	recordPullRequest(t, pool, "LEGION-208", 42)
	renamed := mustRenames(t, ghrepo.Rename{From: ghrepo.MustParse("sjawhar/legion"), To: ghrepo.MustParse("cybermonklabs/legion")})
	for _, tc := range []struct {
		name    string
		renames ghrepo.Renames
		fact    Fact
		want    bool
	}{
		{name: "a review naming the repository it moved to, the pair configured", renames: renamed,
			fact: PullRequestReview{Repo: "cybermonklabs/legion", Number: 42, ID: 7, State: "approved", Author: "reviewer"}, want: true},
		{name: "a checks settlement naming the repository it moved to, the pair configured", renames: renamed,
			fact: PullRequestChecks{Repo: "cybermonklabs/legion", Number: 42, HeadSHA: "head-captured"}, want: true},
		{name: "a review naming the repository the row keeps, the pair configured", renames: renamed,
			fact: PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: 7, State: "approved", Author: "reviewer"}, want: true},
		{name: "a review naming the repository the row keeps, no pair configured",
			fact: PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: 7, State: "approved", Author: "reviewer"}, want: true},
		{name: "a review naming the repository it moved to, no pair configured",
			fact: PullRequestReview{Repo: "cybermonklabs/legion", Number: 42, ID: 7, State: "approved", Author: "reviewer"}},
		{name: "a checks settlement naming the repository it moved to, no pair configured",
			fact: PullRequestChecks{Repo: "cybermonklabs/legion", Number: 42, HeadSHA: "head-captured"}},
		{name: "a review of a pull request neither name records", renames: renamed,
			fact: PullRequestReview{Repo: "cybermonklabs/legion", Number: 43, ID: 7, State: "approved", Author: "reviewer"}},
		{name: "a push, which is no pull request read at all", renames: renamed,
			fact: Push{Repo: "cybermonklabs/legion", Branch: "legion/LEGION-208", Before: "head-captured", After: "pushed"}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := ConsumerSpec{Project: "LEGION", AckWait: 5 * time.Second, Renames: tc.renames}
			got, err := recordedPullRequest(context.Background(), spec, pool, tc.fact)
			if err != nil || got != tc.want {
				t.Fatalf("recordedPullRequest(%#v) = %v, %v; want %v and no error", tc.fact, got, err, tc.want)
			}
		})
	}
}
