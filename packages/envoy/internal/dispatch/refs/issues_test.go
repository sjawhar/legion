package refs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

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

// mainIssueRepoLateral is main's own SQL, held here letter for letter as the oracle of the
// comparison below. It is a copy of the text this change replaced, not text built from the
// package's constants: a lateral built from those would move with them, and the two sides would
// agree however the resolution rule changed. IssueDepthCap is written as the 32 main spliced
// into it for the same reason.
const mainIssueRepoLateral = `
	left join lateral (
		with recursive chain as (
			select i.parent_key, i.repo, 0 as depth
			union all
			select p.parent_key, p.repo, c.depth + 1
			from issues p join chain c on p.key = c.parent_key
			where c.repo is null and c.depth < 32
		)
		select
			coalesce(
				(select c.repo from chain c where c.repo is not null order by c.depth limit 1),
				(select d.repo from repo_projects d where d.project = i.project_key and d.is_default)
			) as resolved,
			exists (
				select 1 from chain c
				where c.depth = 32 and c.repo is null and c.parent_key is not null
			) as too_deep
	) repo on true`

// issueRepoOwnRepo is the repository CORE-100 names as its own: a mapping of its project that
// is not the project's default, so an issue resolving to it resolved through the chain and not
// through the default. issueRepoLoopDefault is project LOOP's own default, which its looped
// issues must not fall back to; a repository maps to one project (repo_projects' primary key is
// the repository alone), so each project's mapping is its own.
const (
	issueRepoOwnRepo     = "acme/gadgets"
	issueRepoLoopDefault = "acme/loops"
)

// newIssueRepoGround is one database holding every case this rule answers differently:
// project CORE's chain of issueRepoCap+2 issues, whose deepest issue the walk cannot finish and
// whose issue at the cap it can; CORE-100, which names a repository of its own, and CORE-101
// below it, which inherits that one; project LOOP, whose two issues are each other's parent
// though the project maps a default the walk must not fall back to; and project BARE, which
// maps no default at all.
func newIssueRepoGround(t *testing.T) *store.Store {
	t.Helper()
	database := newIssueRepoChain(t, issueRepoCap+2)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the ground fixture: %v", err)
	}
	defer tx.Rollback(ctx)
	createReferenceProject(t, tx, "LOOP")
	createReferenceProject(t, tx, "BARE")
	if _, err := tx.Exec(ctx, `
		insert into repo_projects (repo, project, is_default, created_by) values
			($1, 'CORE', false, '{"kind":"user","id":"alice"}'),
			($2, 'LOOP', true, '{"kind":"user","id":"alice"}')
	`, issueRepoOwnRepo, issueRepoLoopDefault); err != nil {
		t.Fatalf("map the ground's repositories: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		insert into issues (key, project_key, number, title, parent_key, repo, created_by, rank) values
			('CORE-100', 'CORE', 100, 'Owns a repository', null, $1, '{"kind":"user","id":"alice"}', '00000100'),
			('CORE-101', 'CORE', 101, 'Inherits from above', 'CORE-100', null, '{"kind":"user","id":"alice"}', '00000101'),
			('LOOP-1', 'LOOP', 1, 'Loop head', null, null, '{"kind":"user","id":"alice"}', '00000001'),
			('LOOP-2', 'LOOP', 2, 'Loop tail', 'LOOP-1', null, '{"kind":"user","id":"alice"}', '00000002'),
			('BARE-1', 'BARE', 1, 'No default above', null, null, '{"kind":"user","id":"alice"}', '00000001'),
			('BARE-2', 'BARE', 2, 'No default below', 'BARE-1', null, '{"kind":"user","id":"alice"}', '00000002')
	`, issueRepoOwnRepo); err != nil {
		t.Fatalf("write the ground's issues: %v", err)
	}
	// Closing the loop is a write no API makes, so it goes in after both rows exist.
	if _, err := tx.Exec(ctx, `update issues set parent_key = 'LOOP-2' where key = 'LOOP-1'`); err != nil {
		t.Fatalf("loop LOOP's chain: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the ground fixture: %v", err)
	}
	return database
}

// issueRepoMainAnswer is what main's lateral resolved for the key, rendered as
// issueRepoSetAnswer renders IssueRepository's answer: "refused" for a chain the walk could not
// finish, the repository otherwise, "none" for no repository and for a key no issue has.
func issueRepoMainAnswer(t *testing.T, database *store.Store, key string) string {
	t.Helper()
	var resolved *string
	var tooDeep bool
	err := database.Pool.QueryRow(context.Background(), `
		select repo.resolved, repo.too_deep
		from issues i`+mainIssueRepoLateral+`
		where i.key = $1
	`, key).Scan(&resolved, &tooDeep)
	if errors.Is(err, pgx.ErrNoRows) {
		return "none"
	}
	if err != nil {
		t.Fatalf("read main's lateral for %q: %v", key, err)
	}
	if tooDeep {
		return "refused"
	}
	return repoName(resolved)
}

// issueRepoSetAnswer is what IssueRepository resolves for the key, rendered the same way. An
// error that does not name the parent chain fails the test rather than reading as a refusal: a
// statement broken in any other way would otherwise pass for every key whose chain has no
// answer.
func issueRepoSetAnswer(t *testing.T, database *store.Store, key string) string {
	t.Helper()
	resolved, err := IssueRepository(context.Background(), database.Pool, key)
	if err != nil {
		if !strings.Contains(err.Error(), "its parent chain is deeper than") {
			t.Fatalf("resolve %q: %v", key, err)
		}
		if resolved != nil {
			t.Errorf("the refusal of %q also answered %s, want no repository with it", key, repoName(resolved))
		}
		return "refused"
	}
	return repoName(resolved)
}

// IssueRepository runs the terms the issue reads run, for one key, so it answers key for key
// what main's lateral walk of that one chain answered: over a loop, a chain at the cap, a chain
// past it, an issue that names its own repository, an issue inheriting one from above, a
// project that maps no default, and a key no issue has. The spread check below says the ground
// still holds all four answers, so an agreement on "none" everywhere cannot pass for one.
func TestIssueRepositoryAnswersWhatMainsLateralDid(t *testing.T) {
	database := newIssueRepoGround(t)
	seen := map[string]bool{}
	for _, ground := range []struct{ key, is string }{
		{"CORE-1", "the chain's root, which takes its project's default"},
		{fmt.Sprintf("CORE-%d", issueRepoCap+1), "the issue exactly at the depth cap"},
		{fmt.Sprintf("CORE-%d", issueRepoCap+2), "the issue one link past the cap"},
		{"CORE-100", "an issue naming a repository of its own"},
		{"CORE-101", "an issue inheriting the repository above it"},
		{"LOOP-1", "an issue in a parent loop"},
		{"LOOP-2", "the other issue in that loop"},
		{"BARE-1", "an issue of a project that maps no default"},
		{"BARE-2", "an issue below it"},
		{"CORE-9999", "a key no issue has"},
		{"", "the empty key"},
	} {
		want := issueRepoMainAnswer(t, database, ground.key)
		seen[want] = true
		if got := issueRepoSetAnswer(t, database, ground.key); got != want {
			t.Errorf("%q, %s: the terms answered %s, main's lateral %s", ground.key, ground.is, got, want)
		}
	}
	for _, answer := range []string{"refused", "none", issueRepoChainDefault, issueRepoOwnRepo} {
		if !seen[answer] {
			t.Errorf("no issue of the ground resolved to %s, so the comparison never covered it", answer)
		}
	}
}
