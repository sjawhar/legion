package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghbranch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A recorded issue's repository is its record's, so its event asks GitHub and Dispatch nothing,
// whatever repository Dispatch resolves it to now: a reopen whose edited repository GitHub can no
// longer read is applied, for the workflow to keep the record's repository and say the edit was not
// applied. A retained event of a recorded issue, which names no repository, takes the record's.
func TestARecordedIssuesEventIsPreparedFromItsRecord(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-301", Tree: "LEGION-301", Project: "LEGION", Title: "existing", Phase: phase.Done,
		Generation: 1, Status: "done", Rank: "A", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"})
	github := newNamedGitHub(t, map[string]string{"acme/widgets": "acme/widgets@main"})
	repos := newRepositories("LEGION")
	repos.set([]mappedRepository{{Repo: ghrepo.MustParse("acme/widgets")}, {Repo: ghrepo.MustParse("zeta/tools")}}, nil)
	branches := newDefaultBranches(roleTokens{}, github.url, defaultBranchTTL, time.Now)
	// zeta/tools was read once, long enough ago that preparing an issue on it reads it again, and
	// GitHub now answers 404 for it.
	branches.read["zeta/tools"] = defaultBranchRead{repository: ghbranch.Repository{Name: ghrepo.MustParse("zeta/tools"), DefaultBranch: "main"},
		at: time.Now().Add(-2 * defaultBranchTTL)}
	client := &detailDispatch{resolved: map[string]string{"LEGION-301": "zeta/tools"}}
	w := &workflowRuntime{pool: pool, records: records, dispatch: client, repositories: repos, defaultBranches: branches}

	reopen := intake.DispatchIssue{Key: "LEGION-301", Seq: 2, Status: "todo", HandedOver: true, ResolvedRepo: "zeta/tools"}
	got, err := w.prepareRepository(ctx, reopen)
	if err != nil || got != reopen {
		t.Fatalf("prepareRepository of a reopen resolving elsewhere = %#v, %v; want the event as it is, applied", got, err)
	}
	retained := intake.DispatchIssue{Key: "LEGION-301", Seq: 3, Status: "todo", HandedOver: true, ResolvedRepoMissing: true}
	got, err = w.prepareRepository(ctx, retained)
	if want := (intake.DispatchIssue{Key: "LEGION-301", Seq: 3, Status: "todo", HandedOver: true, ResolvedRepo: "acme/widgets"}); err != nil || got != want {
		t.Fatalf("prepareRepository of a retained event = %#v, %v; want %#v", got, err, want)
	}
	if reads := client.readCount(); reads != 0 {
		t.Fatalf("Dispatch was read %d times for a recorded issue, want none", reads)
	}
}
