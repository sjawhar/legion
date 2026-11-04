package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// mainIssueChildrenQuery is the children statement main runs, held here letter for letter as
// the oracle of the comparison below. It is a copy of the text this change replaced, not text
// built from the package's own constant: a statement built from that would move with it, and
// the two sides would agree however the rollup changed. parentDepthCap is written as the 32
// main spliced into it for the same reason.
const mainIssueChildrenQuery = `
		with recursive subtree as (
			select key as root, key, 1 as depth from issues where parent_key = $1
			union
			select s.root, i.key, s.depth + 1
			from issues i join subtree s on i.parent_key = s.key
			where s.depth < 32
		), rollup as (
			select s.root,
			       count(distinct s.key) as total,
			       count(distinct s.key) filter (where i.status = 'done') as done,
			       max(i.updated_at) as active_at
			from subtree s join issues i on i.key = s.key
			group by s.root
		)
		select c.key, c.title, c.status, r.done, r.total, r.active_at,
		       coalesce((select json_agg(json_build_object('url', l.url, 'kind', l.kind) order by l.url)
		                 from issue_external_links l where l.issue_key = c.key), '[]')
		from issues c join rollup r on r.root = c.key
		where c.parent_key = $1 order by c.key
	`

// issueChildrenFixture is a root with two children, one of which carries a subtree three levels
// deep holding a done issue and an issue with external links: the rollup counts, the newest
// updated_at and the links are each decided by a different row, so a wrong rollup shows.
type issueChildrenFixture struct {
	database *store.Store
	root     string
}

func newIssueChildrenFixture(t *testing.T) issueChildrenFixture {
	t.Helper()
	handler, database := newTestHandlerWithStore(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "CORE", "name": "Core",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create the project: status=%d body=%s", response.Code, response.Body.String())
	}
	create := func(title string, fields map[string]any) model.Issue {
		t.Helper()
		body := map[string]any{"project": "CORE", "title": title, "force": true}
		for name, value := range fields {
			body[name] = value
		}
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", body, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("create %q: status=%d body=%s", title, response.Code, response.Body.String())
		}
		return decodeBody[model.Issue](t, response)
	}
	root := create("Root", nil)
	branch := create("Branch", map[string]any{"parent": root.Key})
	grandchild := create("Grandchild", map[string]any{"parent": branch.Key})
	create("Great-grandchild", map[string]any{"parent": grandchild.Key})
	create("Leaf", map[string]any{"parent": root.Key})
	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+branch.Key, map[string]any{
		"external_links": []map[string]string{
			{"url": "https://github.com/owner/repo/pull/12", "kind": "github_pr"},
			{"url": "https://github.com/owner/repo/pull/7", "kind": "github_pr"},
		},
	}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("link the branch: status=%d body=%s", response.Code, response.Body.String())
	}
	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+grandchild.Key, map[string]string{
		"status": "done",
	}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("close the grandchild: status=%d body=%s", response.Code, response.Body.String())
	}
	return issueChildrenFixture{database: database, root: root.Key}
}

// issueChildrenRows runs one children statement and returns its rows as text, one string per
// row in the order the statement gave them, which is how the two statements' answers are
// compared here.
func issueChildrenRows(t *testing.T, database *store.Store, statement, key string) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), statement, key)
	if err != nil {
		t.Fatalf("run the children statement: %v", err)
	}
	defer rows.Close()
	read := []string{}
	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			t.Fatalf("read a child row: %v", err)
		}
		read = append(read, fmt.Sprint(values...))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the children: %v", err)
	}
	return read
}

// The children statement answers, row for row and in the same order, what main's statement
// answered over the same issues: the same subtree counts, the same newest updated_at and the
// same external links. A leaf with no descendant is read as well as a branch with a subtree,
// because the walk now carries the columns the rollup counts.
func TestTheIssueChildrenAnswerWhatMainsStatementDid(t *testing.T) {
	fixture := newIssueChildrenFixture(t)
	want := issueChildrenRows(t, fixture.database, mainIssueChildrenQuery, fixture.root)
	if len(want) != 2 {
		t.Fatalf("main's children statement answered %v; want the branch and the leaf", want)
	}
	if got := issueChildrenRows(t, fixture.database, issueChildrenQuery, fixture.root); !slices.Equal(got, want) {
		t.Fatalf("the children statement answered\n%v\nwant main's rows\n%v", got, want)
	}
}

// issueChildrenBacklog is how many issues the plan test writes beside the fixture's five: a
// parent and a child of its own each. A plan is a judgement about data: over five issues the
// whole table is one page, so reading it whole and hashing it is the planner's right answer and
// the statement's own shape says nothing. The test read whichever plan the database it was
// handed happened to make - a clone of the template, whose issues has never been analysed
// (pg_class.reltuples = -1, where the planner assumes a table of ten pages), takes the index;
// a database pgclone emptied and analysed for the next test (reltuples = 0, a table the planner
// knows is empty) merges the whole of issues_parent_key into the walk - which is how it failed
// in CI run 41897075339.
//
// A backlog of pairs is what the walk's own read turns on, not only its size: the planner costs
// the recursive term over a work table it estimates at ten times the first term's rows, so it
// is how many children an issue has on average that decides whether walking a key at a time
// beats hashing the table. One child each is this schema's shape - a tree, not a fan - and
// under it the keyed walk is the cheaper plan, whatever the statistics started as. The analyze
// makes the choice the planner's own rather than an artefact of when it last looked.
const issueChildrenBacklog = 1500

// seedIssueChildrenBacklog writes that backlog: issueChildrenBacklog pairs, each a parent of
// its own with one child, so the rows the children statement answers with are still the
// fixture's alone. The child of each pair is what fills issues_parent_key - the index is
// partial on there being a parent - and carries an external link, so the links the statement
// reads are a keyed read of a table with rows in it rather than the whole of a table holding
// two.
func seedIssueChildrenBacklog(t *testing.T, database *store.Store) {
	t.Helper()
	ctx := context.Background()
	for _, statement := range []struct {
		what string
		sql  string
	}{
		{"the backlog's parents", `
			insert into issues (key, project_key, number, title, status, created_by, rank)
			select 'CORE-R' || n, 'CORE', 100000 + n, 'Backlog parent ' || n, 'todo',
			       '{"kind":"user","id":"alice"}', 'U'
			from generate_series(1, $1) as n`},
		{"the backlog's children", `
			insert into issues (key, project_key, number, title, status, parent_key, created_by, rank)
			select 'CORE-B' || n, 'CORE', 200000 + n, 'Backlog child ' || n,
			       case when n % 5 = 0 then 'done' else 'todo' end,
			       'CORE-R' || n, '{"kind":"user","id":"alice"}', 'U'
			from generate_series(1, $1) as n`},
		{"the backlog's external links", `
			insert into issue_external_links (issue_key, url, kind)
			select 'CORE-B' || n, 'https://github.com/owner/repo/pull/' || (100000 + n), 'github_pr'
			from generate_series(1, $1) as n`},
	} {
		if _, err := database.Pool.Exec(ctx, statement.sql, issueChildrenBacklog); err != nil {
			t.Fatalf("%s: %v", statement.what, err)
		}
	}
	if _, err := database.Pool.Exec(ctx, `analyze issues, issue_external_links`); err != nil {
		t.Fatalf("analyze the backlog: %v", err)
	}
}

// Every read the children statement makes is keyed, each issue it reads is reached through
// issues_parent_key, and no read of an issue goes by its own key. Joining the walk's keys back
// to issues for their status and updated_at is that second read: the planner has no estimate of
// a recursive CTE's size, so a statement that re-read each walked key would hash the whole table
// under a node that carries no index condition. Naming the index holds the statement to one read
// of each issue.
func TestTheIssueChildrenReadEveryIssueThroughTheParentIndex(t *testing.T) {
	fixture := newIssueChildrenFixture(t)
	seedIssueChildrenBacklog(t, fixture.database)
	plan := planOf(t, fixture.database, issueChildrenQuery, fixture.root)
	if unkeyed := explaintest.ScansWithoutIndexCond(t, plan); len(unkeyed) > 0 {
		t.Errorf("the children statement reads %v, which no index serves:\n%s", unkeyed, plan)
	}
	// Every scan of the plan reads through an index that leads with the column the scan is
	// keyed on; a scan carrying no condition at all is the check above's to name.
	explaintest.NewIndexKeys(fixture.database.Pool).RequireIndexKeyed(t, plan,
		explaintest.AnyIndex, "the children statement", explaintest.CondOptional)
	const index = "issues_parent_key"
	walked := false
	for _, scan := range explaintest.IndexScans(t, plan) {
		switch scan.Index {
		case index:
			walked = true
		case "issues_pkey":
			t.Errorf("the children statement reads %s, a second read of an issue the walk already read:\n%s", scan, plan)
		}
	}
	if !walked {
		t.Errorf("the children statement reads no issue through %s:\n%s", index, plan)
	}
}
