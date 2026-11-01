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

// Every read the children statement makes is keyed, each issue it reads is reached through
// issues_parent_key, and no read of an issue goes by its own key. Joining the walk's keys back
// to issues for their status and updated_at is that second read: on a database with issues in
// it the planner has no estimate of a recursive CTE's size, so it hashes the whole table under
// a node that carries no index condition rather than key one issues_pkey lookup per walked
// key, and on a fixture this small it keys them and the whole-table read hides. Naming the
// index holds the statement to one read of each issue either way.
func TestTheIssueChildrenReadEveryIssueThroughTheParentIndex(t *testing.T) {
	fixture := newIssueChildrenFixture(t)
	plan := planWithoutSeqScans(t, fixture.database, issueChildrenQuery, fixture.root)
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
