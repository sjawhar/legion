package api

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
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
// updated_at and the links are each decided by a different row, so a wrong rollup shows. One
// child waits on two issues and holds no claim, the other is claimed and waits on nothing, so
// each of the two values a child now carries is decided by a different row as well.
type issueChildrenFixture struct {
	database *store.Store
	handler  http.Handler
	root     string
	// branch is the claimed child, with the subtree and the external links; leaf is the child
	// waiting on blockers, whose keys are blockers, sorted as the read answers them.
	branch   string
	leaf     string
	blockers []string
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
	leaf := create("Leaf", map[string]any{"parent": root.Key})
	blockers := []string{create("First blocker", nil).Key, create("Second blocker", nil).Key}
	slices.Sort(blockers)
	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+leaf.Key, map[string]any{
		"blocked_by": blockers,
	}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("block the leaf: status=%d body=%s", response.Code, response.Body.String())
	}
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+branch.Key+"/claim",
		map[string]any{}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("claim the branch: status=%d body=%s", response.Code, response.Body.String())
	}
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
	return issueChildrenFixture{
		database: database, handler: handler, root: root.Key,
		branch: branch.Key, leaf: leaf.Key, blockers: blockers,
	}
}

// issueChildrenRows runs one children statement and returns the first columns values of each
// row as text, one string per row in the order the statement gave them, which is how the two
// statements' answers are compared here. The children statement answers two columns main's did
// not — the child's blockers and its claim — so the comparison takes the columns both answer
// and the test below checks the new ones against the read itself.
func issueChildrenRows(t *testing.T, database *store.Store, statement, key string, columns int) []string {
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
		if len(values) < columns {
			t.Fatalf("a child row answered %d columns, fewer than the %d compared: %v", len(values), columns, values)
		}
		read = append(read, fmt.Sprint(values[:columns]...))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the children: %v", err)
	}
	return read
}

// The children statement answers, row for row and in the same order, what main's statement
// answered over the same issues: the same subtree counts, the same newest updated_at and the
// same external links. A leaf with no descendant is read as well as a branch with a subtree,
// because the walk now carries the columns the rollup counts. The blockers and the claim this
// statement adds are the two columns past main's seven, which the issue read's own test checks.
func TestTheIssueChildrenAnswerWhatMainsStatementDid(t *testing.T) {
	fixture := newIssueChildrenFixture(t)
	const mainColumns = 7
	want := issueChildrenRows(t, fixture.database, mainIssueChildrenQuery, fixture.root, mainColumns)
	if len(want) != 2 {
		t.Fatalf("main's children statement answered %v; want the branch and the leaf", want)
	}
	if got := issueChildrenRows(t, fixture.database, issueChildrenStatement, fixture.root, mainColumns); !slices.Equal(got, want) {
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
	plan := planWithoutSeqScans(t, fixture.database, issueChildrenStatement, fixture.root)
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

// Each child of the issue read carries what blocks it and who holds it, in the issue's own two
// shapes: the child waiting on two issues answers their keys sorted and no claim, the claimed
// child answers the claim and an empty list, and both answers are what that child's own issue
// read gives — one shape for an issue and for a child, which is what lets the page read either.
func TestTheIssueChildrenCarryWhatBlocksThemAndWhoHoldsThem(t *testing.T) {
	fixture := newIssueChildrenFixture(t)
	children := map[string]model.IssueChild{}
	for _, child := range readIssueChildren(t, fixture.handler, fixture.root) {
		children[child.Key] = child
	}
	if len(children) != 2 {
		t.Fatalf("the root answered %d children, want the branch and the leaf", len(children))
	}

	blocked := children[fixture.leaf]
	if !slices.Equal(blocked.BlockedBy, fixture.blockers) {
		t.Errorf("the blocked child's blocked_by = %v, want %v", blocked.BlockedBy, fixture.blockers)
	}
	if blocked.Claim != nil {
		t.Errorf("the blocked child's claim = %#v, want none: nobody claimed it", blocked.Claim)
	}

	claimed := children[fixture.branch]
	if len(claimed.BlockedBy) != 0 {
		t.Errorf("the claimed child's blocked_by = %v, want an empty list: it waits on nothing", claimed.BlockedBy)
	}
	if claimed.Claim == nil || claimed.Claim.Actor.Kind != "user" || claimed.Claim.Actor.ID != "alice" {
		t.Fatalf("the claimed child's claim = %#v, want alice's", claimed.Claim)
	}

	for _, child := range []model.IssueChild{blocked, claimed} {
		own := readIssue(t, fixture.handler, child.Key)
		if !slices.Equal(child.BlockedBy, own.BlockedBy) {
			t.Errorf("%s is blocked by %v as a child and %v as an issue", child.Key, child.BlockedBy, own.BlockedBy)
		}
		if !reflect.DeepEqual(child.Claim, own.Claim) {
			t.Errorf("%s is claimed by %#v as a child and %#v as an issue", child.Key, child.Claim, own.Claim)
		}
	}
}

// readIssueChildren is the children of key's issue read, and readIssue the issue itself.
func readIssueChildren(t *testing.T, handler http.Handler, key string) []model.IssueChild {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read %s: status=%d body=%s", key, response.Code, response.Body.String())
	}
	return decodeBody[struct {
		Children []model.IssueChild `json:"children"`
	}](t, response).Children
}

func readIssue(t *testing.T, handler http.Handler, key string) model.Issue {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read %s: status=%d body=%s", key, response.Code, response.Body.String())
	}
	return decodeBody[model.Issue](t, response)
}
