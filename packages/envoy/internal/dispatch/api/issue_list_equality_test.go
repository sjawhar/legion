package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// mainIssueComponentsLateral and the laterals and statements below it are main's own SQL, held
// here letter for letter as the oracle of every comparison in this file. They are copies of the
// text this change replaced, not text built from the package's constants: a statement built
// from those would move with them, and the two sides would agree however the resolution rule
// changed. parentDepthCap is written as the 32 main spliced into them for the same reason.
const mainIssueComponentsLateral = `
	left join lateral (
		with recursive chain as (
			select i.key as key, i.parent_key, 0 as depth
			union
			select p.key, p.parent_key, c.depth + 1
			from issues p join chain c on p.key = c.parent_key
			where c.depth < 32
		), owner as (
			select c.key, ic.mode, ic.reason
			from chain c join issue_components ic on ic.issue_key = c.key
			order by c.depth limit 1
		)
		select o.key as owner_key, o.mode, o.reason,
		       coalesce((select array_agg(m.component_id order by m.component_id)
		                 from issue_component_members m
		                 join components x on x.project_key = m.project_key and x.id = m.component_id
		                 where m.issue_key = o.key), '{}'::text[]) as ids,
		       coalesce((select array_agg(m.component_id order by m.component_id)
		                 from issue_component_members m
		                 where m.issue_key = o.key
		                   and not exists (select 1 from components x where x.project_key = m.project_key and x.id = m.component_id)), '{}'::text[]) as unknown
		from owner o
	) comp on true`

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

const mainIssueProgressLateral = `
	left join lateral (
		select count(*) filter (where c.status = 'done') as children_done, count(*) as children_total
		from issues c where c.parent_key = i.key
	) progress on true`

// mainIssueStatusCase is the order main's statements wrote, from model.IssueStatuses as it stood.
const mainIssueStatusCase = `case i.status
		when 'triage' then 1
		when 'icebox' then 2
		when 'backlog' then 3
		when 'todo' then 4
		when 'in_progress' then 5
		when 'testing' then 6
		when 'needs_review' then 7
		when 'retro' then 8
		when 'done' then 9
		end`

const mainIssueSummaryHead = `
	select i.key, i.title, i.status, i.priority, i.rank, i.labels, i.parent_key,
	       coalesce((select array_agg(l.target_key order by l.target_key) from issue_links l where l.issue_key = i.key and l.kind = 'blocked_by'), '{}'),
	       i.assignee, i.route, i.updated_at, i.last_seq,
	       coalesce((select a.id::text from artifacts a where a.issue_key = i.key and a.is_primary), ''),
	       i.claimed_by, i.claimed_at,
	       count(a.id) filter (where i.closed_at is null),
	       i.tasks_done, i.tasks_total, progress.children_done, progress.children_total,
	       comp.owner_key, comp.mode, comp.reason, comp.ids, comp.unknown,
	       i.repo, repo.resolved, repo.too_deep
	from issues i`

const mainIssueSummaryTail = `
	left join asks a on a.issue_key = i.key and a.state = 'open'
	` + mainIssueProgressLateral + `
	` + mainIssueComponentsLateral + `
	` + mainIssueRepoLateral + `
	where ($1 = '' or i.project_key = $1)
	  and ($2 = '' or i.status = $2)
	  and ($3 = '' or i.parent_key = $3)
	  and ($4::timestamptz is null or i.updated_at >= $4)
	  and ($5::text[] = '{}' or (select array_agg(lower(label)) from unnest(i.labels) as label) @> $5)
	  and (not $6::boolean or i.closed_at is null)
	  and ((cardinality($7::smallint[]) = 0 and not $8::boolean)
	       or i.priority = any($7::smallint[])
	       or ($8::boolean and i.priority is null))
	group by i.key, progress.children_done, progress.children_total, comp.owner_key, comp.mode, comp.reason, comp.ids, comp.unknown, repo.resolved, repo.too_deep
	order by ` + mainIssueStatusCase + `, i.rank asc, i.created_at asc, i.key asc
`

const (
	mainListIssuesQuery       = mainIssueSummaryHead + mainIssueSummaryTail
	mainListPinnedIssuesQuery = mainIssueSummaryHead + `
	join user_issue_state s on s.issue_key = i.key and s.login = $9 and s.pinned` + mainIssueSummaryTail
)

// mainIssueReadFacts, mainTreeIssueFacts and mainDeliveryIssueFacts are what main's laterals
// answered at the three reads that splice them beside the listing: the issue read, the
// architecture tree and the delivery timeline's issue facts. Each selects the key and the
// resolved columns alone, which is the whole of what this change moved at those reads; the rest
// of each statement is untouched text, and its route's own tests cover it.
const (
	mainIssueReadFacts = `
		select i.key, comp.owner_key, comp.mode, comp.reason, comp.ids, comp.unknown,
		       i.repo, repo.resolved, repo.too_deep,
		       i.tasks_done, i.tasks_total, progress.children_done, progress.children_total
		from issues i` + mainIssueProgressLateral + mainIssueComponentsLateral + mainIssueRepoLateral + `
		where i.key = $1`
	mainTreeIssueFacts = `
		select i.key, comp.owner_key, comp.mode, comp.reason, comp.ids, comp.unknown
		from issues i` + mainIssueComponentsLateral + `
		where i.project_key = $1
		order by ` + mainIssueStatusCase + `, i.rank asc, i.created_at asc`
	mainDeliveryIssueFacts = `
		select i.key, coalesce(comp.ids, '{}'::text[])
		from issues i` + mainIssueComponentsLateral + `
		where i.key = any($1)`
)

// mainProjectList is the project list main ran: one subquery for each project row.
const mainProjectList = `
		select p.key, p.name, (
			select count(*)
			from asks a
			left join issues i on i.key = a.issue_key
			left join artifacts ar on ar.id = a.artifact_id
			where a.state = 'open'
			  and coalesce(i.project_key, ar.project_key) = p.key
			  and (i.key is null or i.closed_at is null)
		), p.created_at
		from projects p
		order by p.key
	`

// The listing's four statements answer, row for row, what main's one statement answered over the
// same rows: the unpaged read whole, and the paged read the slice of it the caller asked for,
// with the count beside it. The paged read carries two columns of its own ahead of the issue's,
// which is what those rows are compared without.
func TestTheIssueListAnswersWhatMainsStatementDid(t *testing.T) {
	fixture := newIssueListFixture(t)
	filters := []any{"LIST", "", "", nil, []string{}, false, []int16{}, false}
	for _, listing := range []struct {
		name       string
		oracle     string
		paged      string
		unpaged    string
		pinnedBy   string
		leadColumn int
	}{
		{name: "the listing", oracle: mainListIssuesQuery, paged: listIssuesPageQuery, unpaged: listIssuesQuery},
		{name: "the pinned listing", oracle: mainListPinnedIssuesQuery, paged: listPinnedIssuesPageQuery, unpaged: listPinnedIssuesQuery, pinnedBy: "alice"},
	} {
		t.Run(listing.name, func(t *testing.T) {
			arguments := slices.Clone(filters)
			if listing.pinnedBy != "" {
				arguments = append(arguments, listing.pinnedBy)
			}
			want := issueListRows(t, fixture.database, listing.oracle, arguments)
			if len(want) < 4 {
				t.Fatalf("%s answered %d rows, too few for the comparison to decide anything", listing.name, len(want))
			}
			if got := issueListRows(t, fixture.database, listing.unpaged, arguments); !slices.Equal(got, want) {
				t.Fatalf("%s unpaged answered\n%v\nwant main's rows\n%v", listing.name, got, want)
			}
			for _, page := range []struct{ limit, offset int }{{2, 0}, {2, 2}, {3, 1}, {len(want), 0}, {2, len(want)}, {2, len(want) + 40}} {
				paged := append(slices.Clone(arguments), page.limit, page.offset)
				got, total := issueListPage(t, fixture.database, listing.paged, paged)
				start := min(page.offset, len(want))
				end := min(start+page.limit, len(want))
				if !slices.Equal(got, want[start:end]) {
					t.Errorf("%s limit %d offset %d answered\n%v\nwant main's rows\n%v", listing.name, page.limit, page.offset, got, want[start:end])
				}
				if total != len(want) {
					t.Errorf("%s limit %d offset %d counted %d, want main's %d", listing.name, page.limit, page.offset, total, len(want))
				}
			}
		})
	}
}

// A closed issue reports no open ask however many it holds, which is main's own rule
// (`count(a.id) filter (where i.closed_at is null)`) and the one the counting join must keep.
// The comparison above only proves it while such an issue is in the listing, so this says the
// fixture holds one.
func TestTheListedClosedIssueHoldsAnOpenAsk(t *testing.T) {
	fixture := newIssueListFixture(t)
	var open int
	if err := fixture.database.Pool.QueryRow(context.Background(),
		`select count(*) from asks where issue_key = $1 and state = 'open'`, fixture.closed).Scan(&open); err != nil {
		t.Fatalf("count %s's open asks: %v", fixture.closed, err)
	}
	if open == 0 {
		t.Fatalf("%s holds no open ask, so the listing's closed-issue rule is not under test", fixture.closed)
	}
	listed := decodeBody[[]model.IssueSummary](t, dispatchRequest(t, fixture.handler, http.MethodGet, "/api/v1/issues?project=LIST", nil, "alice"))
	for _, issue := range listed {
		if issue.Key == fixture.closed && issue.OpenAsks != 0 {
			t.Fatalf("the closed %s lists %d open asks, want 0 while it holds %d", issue.Key, issue.OpenAsks, open)
		}
	}
}

// The three other reads that resolve an issue's components or repository answer what main's
// laterals answered for every issue of the fixture: the issue read, the architecture tree and
// the delivery timeline's facts. Each is read through the code's own function, so the
// comparison covers the statement the server runs rather than a copy of it.
func TestTheOtherResolvedReadsAnswerWhatMainsLateralsDid(t *testing.T) {
	fixture := newIssueListFixture(t)
	ctx := context.Background()
	pool := fixture.database.Pool

	for _, key := range fixture.keys {
		var components componentsScan
		var repo repoScan
		var progress progressScan
		var scanned string
		targets := []any{&scanned}
		targets = append(targets, components.targets()...)
		targets = append(targets, repo.targets()...)
		targets = append(targets, progress.targets()...)
		if err := pool.QueryRow(ctx, mainIssueReadFacts, key).Scan(targets...); err != nil {
			t.Fatalf("read main's facts for %s: %v", key, err)
		}
		own, resolved, err := repo.resolve(key)
		if err != nil {
			t.Fatalf("resolve main's repository for %s: %v", key, err)
		}
		issue, read, err := fixture.server.loadIssueWithProgress(ctx, pool, key)
		if err != nil {
			t.Fatalf("read %s: %v", key, err)
		}
		if got, want := issueFactText(t, issue.Components), issueFactText(t, components.resolve(key)); got != want {
			t.Errorf("the read of %s answers components %s, want main's %s", key, got, want)
		}
		if got, want := issueFactText(t, []any{issue.Repo, issue.ResolvedRepo}), issueFactText(t, []any{own, resolved}); got != want {
			t.Errorf("the read of %s answers repository %s, want main's %s", key, got, want)
		}
		if got, want := issueFactText(t, read), issueFactText(t, progress.resolve()); got != want {
			t.Errorf("the read of %s answers progress %s, want main's %s", key, got, want)
		}
	}

	treeWant := map[string]string{}
	rows, err := pool.Query(ctx, mainTreeIssueFacts, "LIST")
	if err != nil {
		t.Fatalf("read main's tree facts: %v", err)
	}
	order := []string{}
	for rows.Next() {
		var key string
		var components componentsScan
		if err := rows.Scan(append([]any{&key}, components.targets()...)...); err != nil {
			t.Fatalf("scan main's tree facts: %v", err)
		}
		treeWant[key] = issueFactText(t, components.resolve(key))
		order = append(order, key)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("read main's tree facts: %v", err)
	}
	tree, err := loadTreeIssues(ctx, pool, "LIST")
	if err != nil {
		t.Fatalf("read the tree's issues: %v", err)
	}
	if len(tree) != len(order) {
		t.Fatalf("the tree reads %d issues, want main's %d", len(tree), len(order))
	}
	for index, issue := range tree {
		if issue.Key != order[index] {
			t.Fatalf("the tree reads %s at %d, want main's %s", issue.Key, index, order[index])
		}
		if got := issueFactText(t, issue.Components); got != treeWant[issue.Key] {
			t.Errorf("the tree reads %s with components %s, want main's %s", issue.Key, got, treeWant[issue.Key])
		}
	}

	deliveryWant := map[string]string{}
	rows, err = pool.Query(ctx, mainDeliveryIssueFacts, fixture.keys)
	if err != nil {
		t.Fatalf("read main's delivery facts: %v", err)
	}
	for rows.Next() {
		var key string
		var ids []string
		if err := rows.Scan(&key, &ids); err != nil {
			t.Fatalf("scan main's delivery facts: %v", err)
		}
		qualified := make([]string, len(ids))
		for index, id := range ids {
			qualified[index] = deliveryComponentKey("LIST", id)
		}
		deliveryWant[key] = fmt.Sprint(qualified)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("read main's delivery facts: %v", err)
	}
	facts, err := fixture.server.deliveryIssueFacts(ctx, fixture.keys)
	if err != nil {
		t.Fatalf("read the delivery facts: %v", err)
	}
	if len(facts) != len(deliveryWant) {
		t.Fatalf("the delivery facts cover %d issues, want main's %d", len(facts), len(deliveryWant))
	}
	for key, issue := range facts {
		if got := fmt.Sprint(issue.components); got != deliveryWant[key] {
			t.Errorf("the delivery facts give %s components %s, want main's %s", key, got, deliveryWant[key])
		}
	}
}

// The project list's grouped join counts each project's open asks as main's subquery per project
// counted them, 0 for a project with no open ask included: a left join answers that one null,
// which is what the coalesce is for.
func TestTheProjectListCountsWhatMainsSubqueryDid(t *testing.T) {
	fixture := newIssueListFixture(t)
	rows, err := fixture.database.Pool.Query(context.Background(), mainProjectList)
	if err != nil {
		t.Fatalf("read main's project list: %v", err)
	}
	want := []string{}
	counted := 0
	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			t.Fatalf("scan main's project list: %v", err)
		}
		want = append(want, fmt.Sprint(values...))
		if fmt.Sprint(values[2]) != "0" {
			counted++
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("read main's project list: %v", err)
	}
	if len(want) < 2 || counted == 0 {
		t.Fatalf("main's project list answered %v, want several projects and at least one open ask among them", want)
	}
	listed := decodeBody[[]model.Project](t, dispatchRequest(t, fixture.handler, http.MethodGet, "/api/v1/projects", nil, "alice"))
	got := make([]string, 0, len(listed))
	for _, project := range listed {
		got = append(got, fmt.Sprint(project.Key, project.Name, project.OpenAsks, project.CreatedAt))
	}
	if !slices.Equal(got, want) {
		t.Fatalf("the project list answered\n%v\nwant main's rows\n%v", got, want)
	}
}

// A route_status request keeps main's read and main's page cut, since that filter runs after the
// Envoy roster answers: its page is the same slice of its own unpaged listing, and its total the
// length of that listing.
func TestARouteStatusPageIsStillCutAfterTheRosterFilter(t *testing.T) {
	roster := &fakeRoster{}
	handler, _ := routeStatusServer(t, roster)
	keys := createListedIssues(t, handler, "ROUTED", 6)
	for index, key := range keys {
		if index%2 == 1 {
			continue
		}
		if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+key,
			map[string]any{"route": "role:reviewer"}, "alice"); response.Code != http.StatusOK {
			t.Fatalf("route %s: status=%d body=%s", key, response.Code, response.Body.String())
		}
	}
	roster.set("reviewer-session", "reviewer")
	whole := listedKeys(t, handler, "project=ROUTED&route_status=live")
	if len(whole) != 3 {
		t.Fatalf("the routed listing is %v, want the three routed issues", whole)
	}
	for _, page := range []struct{ limit, offset int }{{2, 0}, {2, 2}, {2, 9}} {
		query := "project=ROUTED&route_status=live&limit=" + strconv.Itoa(page.limit) + "&offset=" + strconv.Itoa(page.offset)
		read, ok := readPage(t, handler, query)
		if !ok {
			continue
		}
		start := min(page.offset, len(whole))
		end := min(start+page.limit, len(whole))
		if got := summaryKeys(read.Issues); !slices.Equal(got, whole[start:end]) || read.Total != len(whole) {
			t.Errorf("?%s answered %v total %d, want %v total %d", query, got, read.Total, whole[start:end], len(whole))
		}
	}
}

// A parent chain that loops has no repository to answer, so a read of an issue on it fails
// (500). A paged request resolves its own page alone, so it fails only when the bad issue is on
// the page it asked for; the unpaged listing still fails for any issue it matches, which is what
// it did before this change.
func TestAPagedListingFailsOnlyOnABadChainOfItsOwnRows(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	keys := createListedIssues(t, handler, "LOOP", 6)
	bad := keys[len(keys)-1]
	above := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "LOOP", "title": "Above the loop", "parent": bad, "force": true,
	}, "alice"))
	if _, err := database.Pool.Exec(context.Background(),
		`update issues set parent_key = $1 where key = $2`, above.Key, bad); err != nil {
		t.Fatalf("loop %s's parent chain: %v", bad, err)
	}
	for query, want := range map[string]int{
		"project=LOOP":                   http.StatusInternalServerError,
		"project=LOOP&limit=3&offset=0":  http.StatusOK,
		"project=LOOP&limit=3&offset=3":  http.StatusInternalServerError,
		"project=LOOP&limit=3&offset=90": http.StatusOK,
	} {
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues?"+query, nil, "alice")
		if response.Code != want {
			t.Errorf("?%s: status=%d body=%s, want %d", query, response.Code, response.Body.String(), want)
		}
	}
	if page, ok := readPage(t, handler, "project=LOOP&limit=3&offset=0"); ok {
		if got := summaryKeys(page.Issues); !slices.Equal(got, keys[0:3]) || page.Total != len(keys)+1 {
			t.Errorf("the page before the loop answered %v total %d, want %v total %d", got, page.Total, keys[0:3], len(keys)+1)
		}
	}
}

// issueFactText renders a resolved fact as the response writes it, so two reads of the same
// facts are compared by what they say rather than by where their pointers point.
func issueFactText(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("render %v: %v", value, err)
	}
	return string(encoded)
}

// issueListRows runs one unpaged listing statement and returns its rows as text, one string per
// row, which is how two statements' answers are compared here.
func issueListRows(t *testing.T, database *store.Store, statement string, arguments []any) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), statement, arguments...)
	if err != nil {
		t.Fatalf("run the listing: %v", err)
	}
	defer rows.Close()
	read := []string{}
	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			t.Fatalf("read a listed row: %v", err)
		}
		read = append(read, fmt.Sprint(values...))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the listing: %v", err)
	}
	return read
}

// issueListPage runs a paged listing statement and returns the rows of its page and the count it
// made of every issue the filters match, dropping the row an empty page answers to carry that
// count, as scanIssuePage does.
func issueListPage(t *testing.T, database *store.Store, statement string, arguments []any) ([]string, int) {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), statement, arguments...)
	if err != nil {
		t.Fatalf("run the paged listing: %v", err)
	}
	defer rows.Close()
	read := []string{}
	total := 0
	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			t.Fatalf("read a paged row: %v", err)
		}
		total = int(values[0].(int64))
		if !values[1].(bool) {
			continue
		}
		read = append(read, fmt.Sprint(values[2:]...))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the paged listing: %v", err)
	}
	return read, total
}

// issueListFixture is project LIST shaped like the listing's hard cases, in a database holding
// enough other issues that the planner chooses the plans production's shape gives it: a parent
// chain three issues deep, an issue with two blockers, a closed issue holding an open ask, an
// issue whose own repository overrides its project's default, explicit, inherited and `none`
// component attachments with a retired member among them, pinned rows, specs, and children of
// both states.
type issueListFixture struct {
	handler  http.Handler
	server   *server
	database *store.Store
	keys     []string
	closed   string
}

func newIssueListFixture(t *testing.T) issueListFixture {
	t.Helper()
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{})
	ctx := context.Background()
	for _, project := range []string{"LIST", "OTHER"} {
		if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
			"key": project, "name": project,
		}, "alice"); response.Code != http.StatusCreated {
			t.Fatalf("create project %s: status=%d body=%s", project, response.Code, response.Body.String())
		}
	}
	for repo, body := range map[string]map[string]any{
		"acme/widgets": {"project": "LIST", "default": true},
		"acme/gadgets": {"project": "LIST"},
	} {
		if response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/"+repo, body, "alice"); response.Code != http.StatusOK {
			t.Fatalf("map %s: status=%d body=%s", repo, response.Code, response.Body.String())
		}
	}
	for _, statement := range []string{
		`insert into architecture_sources (project_key, id, repo, branch, installation_id, created_by)
			values ('LIST', 1, 'acme/arch', 'main', 1, '{"kind":"system","id":"test"}')`,
		`insert into architecture_snapshots (id, project_key, commit, files) values (902, 'LIST', 'c1', '{}')`,
		`insert into components (project_key, id, title, prose, parent, snapshot_id, source_id) values
			('LIST', 'platform', 'The platform', '', null, 902, 1), ('LIST', 'api', 'Public API', '', 'platform', 902, 1)`,
	} {
		if _, err := database.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("seed the component model: %v", err)
		}
	}

	create := func(body map[string]any) string {
		t.Helper()
		body["project"], body["force"] = "LIST", true
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", body, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("create %v: status=%d body=%s", body["title"], response.Code, response.Body.String())
		}
		return decodeBody[model.Issue](t, response).Key
	}
	root := create(map[string]any{"title": "Platform work", "repo": "acme/gadgets",
		"components": map[string]any{"mode": "explicit", "ids": []string{"platform"}}})
	child := create(map[string]any{"title": "Service work", "parent": root, "priority": 1})
	grandchild := create(map[string]any{"title": "Endpoint work", "parent": child,
		"components": map[string]any{"mode": "explicit", "ids": []string{"api"}}})
	done := create(map[string]any{"title": "Finished child", "parent": child})
	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+done,
		map[string]any{"status": "done"}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("finish %s: status=%d body=%s", done, response.Code, response.Body.String())
	}
	blocked := create(map[string]any{"title": "Blocked on two", "blocked_by": []string{root, child},
		"labels": []string{"frontend"}, "assignee": "bob"})
	create(map[string]any{"title": "Not architectural", "components": map[string]any{"mode": "none", "reason": "process"}})
	withSpec := create(map[string]any{"title": "Issue with a spec", "spec": "A spec\n"})
	closed := create(map[string]any{"title": "Closed with a question"})
	keys := []string{root, child, grandchild, done, blocked, withSpec, closed}

	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+blocked,
		map[string]any{"route": "role:reviewer"}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("route %s: status=%d body=%s", blocked, response.Code, response.Body.String())
	}
	for _, key := range []string{root, grandchild, closed} {
		response := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/asks", map[string]any{
			"question": "What next on " + key + "?",
			"actor":    sessionActor(),
		})
		if response.Code != http.StatusCreated {
			t.Fatalf("ask on %s: status=%d body=%s", key, response.Code, response.Body.String())
		}
	}
	// A close withdraws its issue's open asks, so the one state the closed-issue rule is about —
	// a closed issue still holding an open ask — is written here rather than asked for.
	if _, err := database.Pool.Exec(ctx,
		`update issues set status = 'done', closed_at = now() where key = $1`, closed); err != nil {
		t.Fatalf("close %s: %v", closed, err)
	}
	// A member of a component a re-import retired is a read's `unknown`, and no write makes one.
	if _, err := database.Pool.Exec(ctx,
		`insert into issue_component_members (issue_key, project_key, component_id) values ($1, 'LIST', 'retired-thing')`,
		grandchild); err != nil {
		t.Fatalf("retire a member of %s: %v", grandchild, err)
	}
	for _, key := range []string{root, grandchild, blocked, closed} {
		if response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/issues/"+key+"/state",
			map[string]bool{"pinned": true}, "alice"); response.Code != http.StatusOK {
			t.Fatalf("pin %s: status=%d body=%s", key, response.Code, response.Body.String())
		}
	}
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+child+"/claim",
		map[string]any{}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("claim %s: status=%d body=%s", child, response.Code, response.Body.String())
	}

	// The bulk the planner needs: a project this size reads its listing the way production does,
	// and the plans an empty table would give say nothing (api/audited_reads_test.go).
	for _, bulk := range []struct {
		what      string
		statement string
	}{
		{"the other issues", `
			insert into issues (key, project_key, number, title, created_by, rank, parent_key)
			select 'LIST-' || n, 'LIST', n, 'Bulk issue ' || n, '{"kind":"user","id":"alice"}', 'U' || lpad(n::text, 6, '0'),
			       case when n % 5 = 0 then $1 else null end
			from generate_series(1000, 1300) n
		`},
		{"their blockers", `
			insert into issue_links (issue_key, target_key, kind)
			select 'LIST-' || n, $1, 'blocked_by' from generate_series(1000, 1300) n where n % 7 = 0
		`},
		{"their open asks", `
			insert into asks (issue_key, author, question)
			select 'LIST-' || n, '{"kind":"user","id":"alice"}', 'Bulk question ' || n
			from generate_series(1000, 1300) n where n % 11 = 0 and $1 <> ''
		`},
		{"another project's issues", `
			insert into issues (key, project_key, number, title, created_by, rank)
			select 'OTHER-' || n, 'OTHER', n, 'Other issue ' || n, '{"kind":"user","id":"alice"}', 'U' || lpad(n::text, 6, '0')
			from generate_series(1000, 1300) n where $1 <> ''
		`},
	} {
		if _, err := database.Pool.Exec(ctx, bulk.statement, root); err != nil {
			t.Fatalf("seed %s: %v", bulk.what, err)
		}
	}
	if _, err := database.Pool.Exec(ctx, "analyze"); err != nil {
		t.Fatalf("analyze the seeded fixture: %v", err)
	}
	return issueListFixture{handler: handler, server: served, database: database, keys: keys, closed: closed}
}
