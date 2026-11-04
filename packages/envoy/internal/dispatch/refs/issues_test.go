package refs

import (
	"context"
	"fmt"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// issueRepoCap is the depth the walk has to stop at, held here as the number rather than read
// from IssueDepthCap: a chain measured by the constant would move with it, and the walk could
// reach any depth with these tests still passing. It is the same 32-link rule the API's parent
// cap refuses past and migration 0113's census counts against.
const issueRepoCap = 32

// issueRepoChainDefault is the repository the chain's project maps as its default. No issue in
// the chain names a repository of its own, so it is the answer for every issue whose walk
// finishes, and the wrong answer for every issue whose walk does not.
const issueRepoChainDefault = "acme/widgets"

// newIssueRepoChain writes a project whose default repository is issueRepoChainDefault and a
// chain of length issues under it, CORE-1 the root and CORE-n n-1 links below it, and returns
// the database holding them. The rows go in as SQL because no API write builds a chain this deep
// (api's refuseParentTooDeep) and migration 0113 refuses to apply while one exists: such a chain
// is left by a direct database write, or by an earlier release serving against the rollout rule,
// and that is what the walk has to survive.
func newIssueRepoChain(t *testing.T, length int) *store.Store {
	t.Helper()
	database := storetest.Open(t)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the chain fixture: %v", err)
	}
	defer tx.Rollback(ctx)
	createReferenceProject(t, tx, "CORE")
	if _, err := tx.Exec(ctx, `
		insert into repo_projects (repo, project, is_default, created_by)
		values ($1, 'CORE', true, '{"kind":"user","id":"alice"}')
	`, issueRepoChainDefault); err != nil {
		t.Fatalf("map %s as the project's default: %v", issueRepoChainDefault, err)
	}
	if _, err := tx.Exec(ctx, `
		insert into issues (key, project_key, number, title, parent_key, created_by, rank)
		select 'CORE-' || n, 'CORE', n, 'Chain ' || n,
		       case when n = 1 then null else 'CORE-' || (n - 1) end,
		       '{"kind":"user","id":"alice"}', lpad(n::text, 8, '0')
		from generate_series(1, $1::int) n
		order by n
	`, length); err != nil {
		t.Fatalf("write a chain of %d issues: %v", length, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the chain fixture: %v", err)
	}
	return database
}

// repoName renders a resolved repository in a failure message.
func repoName(repo *string) string {
	if repo == nil {
		return "none"
	}
	return *repo
}

// The walk IssueRepository runs climbs at most issueRepoCap links, so an issue deeper than that
// has ancestors it never reads and no repository it can name. The read refuses it rather than
// answering with the project's default, which the unread ancestors may well override. The issue
// exactly issueRepoCap links below the root is still answered, so the refusal is the cap's own
// boundary and not a fixture that fails for every issue.
func TestIssueRepositoryRefusesAChainPastTheDepthCap(t *testing.T) {
	database := newIssueRepoChain(t, issueRepoCap+2)
	ctx := context.Background()

	atCap := fmt.Sprintf("CORE-%d", issueRepoCap+1)
	resolved, err := IssueRepository(ctx, database.Pool, atCap)
	if err != nil {
		t.Fatalf("resolve %s, %d links below its root: %v", atCap, issueRepoCap, err)
	}
	if resolved == nil || *resolved != issueRepoChainDefault {
		t.Fatalf("%s, %d links below its root, resolved to %s, want the project's default %s",
			atCap, issueRepoCap, repoName(resolved), issueRepoChainDefault)
	}

	pastCap := fmt.Sprintf("CORE-%d", issueRepoCap+2)
	resolved, err = IssueRepository(ctx, database.Pool, pastCap)
	if err == nil {
		t.Fatalf("%s, %d links below its root, resolved to %s, want the read refused",
			pastCap, issueRepoCap+1, repoName(resolved))
	}
	if resolved != nil {
		t.Fatalf("the refusal of %s also answered %s, want no repository with it", pastCap, repoName(resolved))
	}
}

// A parent chain that loops has no root and so no repository to resolve to. The walk runs out of
// links as it does on a chain past the cap, and the read refuses it for the same reason rather
// than answering with the project's default. The same issue resolves before the loop is closed,
// so the refusal is the loop's.
func TestIssueRepositoryRefusesALoopedParentChain(t *testing.T) {
	database := newIssueRepoChain(t, 2)
	ctx := context.Background()

	resolved, err := IssueRepository(ctx, database.Pool, "CORE-2")
	if err != nil {
		t.Fatalf("resolve CORE-2 under an unlooped parent: %v", err)
	}
	if resolved == nil || *resolved != issueRepoChainDefault {
		t.Fatalf("CORE-2 under an unlooped parent resolved to %s, want the project's default %s",
			repoName(resolved), issueRepoChainDefault)
	}

	// CORE-2's parent is CORE-1; making CORE-1's parent CORE-2 closes the loop, which no API
	// write can do.
	if _, err := database.Pool.Exec(ctx, `update issues set parent_key = 'CORE-2' where key = 'CORE-1'`); err != nil {
		t.Fatalf("loop the chain: %v", err)
	}
	for _, key := range []string{"CORE-1", "CORE-2"} {
		resolved, err := IssueRepository(ctx, database.Pool, key)
		if err == nil {
			t.Errorf("%s in a looped chain resolved to %s, want the read refused", key, repoName(resolved))
		} else if resolved != nil {
			t.Errorf("the refusal of %s also answered %s, want no repository with it", key, repoName(resolved))
		}
	}
}
