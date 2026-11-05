package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// issueStatusOrderSQL derives the SQL ordering from Dispatch's canonical lifecycle.
func issueStatusOrderSQL() string {
	var order strings.Builder
	order.WriteString("case i.status")
	for index, status := range model.IssueStatuses {
		fmt.Fprintf(&order, "\n\t\twhen '%s' then %d", status, index+1)
	}
	order.WriteString("\n\t\tend")
	return order.String()
}

var issueStatusCase = issueStatusOrderSQL()

// The listing has four statement texts: unpaged and paged, each of them pinned or not
// (issueSummaryQuery writes them). A request that names a page and no route_status filter runs
// the paged text, which cuts the page in SQL and answers `total` beside it; a route_status
// request keeps the unpaged text and listIssues' own page cut, because that filter runs in Go
// after the Envoy roster enrichment and total must count what is left after it. pgx caches a
// plan for each text, so neither shape replans the other's.
var (
	listIssuesQuery           = issueSummaryQuery(false, false)
	listPinnedIssuesQuery     = issueSummaryQuery(true, false)
	listIssuesPageQuery       = issueSummaryQuery(false, true)
	listPinnedIssuesPageQuery = issueSummaryQuery(true, true)
)

// issueSummaryQuery writes one listing statement. `filtered` applies every filter to `issues i`
// alone, so `total` is a count over it with no join and the paged text orders and cuts it before
// anything else is resolved: the six joins then run for the page's rows alone, and a listing of
// twenty rows costs twenty issues' blockers, specs, open asks, children, components and
// repository rather than every matching issue's. Both counts come from the one statement, since
// two statements read two snapshots and can disagree about a table a write is moving.
//
// An empty page still carries the count the caller asked for, so `listed` answers one issue of
// the filtered set marked `in_page` false when the page holds nothing, and scanIssuePage drops
// it without resolving it; an empty filtered set answers no row at all, and the total is then 0.
// The order ends on the key, the one column no two issues share, so it is total and consecutive
// offsets cannot show a row twice or never while the listing does not change between the reads
// (issuePage.of says what a change does). Without the key they could: ranks repeat across
// projects (every project's first issue is "U"), nothing makes one unique within a project, and
// created_at is the creating transaction's start.
func issueSummaryQuery(pinned, paged bool) string {
	parameter := 9
	pinnedJoin := ""
	if pinned {
		pinnedJoin = fmt.Sprintf("\n\t\tjoin user_issue_state s on s.issue_key = i.key and s.login = $%d and s.pinned", parameter)
		parameter++
	}
	query := `with recursive filtered as (
		select i.key, i.status, i.rank, i.created_at
		from issues i` + pinnedJoin + `
		` + issueListFilters + `
	)`
	rows, columns := "filtered", issueSummaryColumns
	if paged {
		query += fmt.Sprintf(`, page as (
		select i.key from filtered i
		order by %s
		limit $%d offset $%d
	), listed as (
		select p.key, true as in_page from page p
		union all
		select (select f.key from filtered f limit 1), false where not exists (select 1 from page)
	)`, issueListOrder, parameter, parameter+1)
		rows = "listed"
		columns = "(select count(*) from filtered) as total, listed.in_page,\n\t       " + issueSummaryColumns
	}
	return query + `, subjects as (
		select key from ` + rows + `
	), ` + issueComponentsTerms + `, ` + refs.IssueRepoTerms + `
	select ` + columns + `
	from ` + rows + ` join issues i on i.key = ` + rows + `.key
	` + issueSummaryJoins + `
	order by ` + issueListOrder + `
`
}

// issueListFilters are the eight filters a listing request carries, written against `issues i`
// alone so the filtered term joins nothing: the page and the count both read it.
const issueListFilters = `where ($1 = '' or i.project_key = $1)
		  and ($2 = '' or i.status = $2)
		  and ($3 = '' or i.parent_key = $3)
		  and ($4::timestamptz is null or i.updated_at >= $4)
		  and ($5::text[] = '{}' or (select array_agg(lower(label)) from unnest(i.labels) as label) @> $5)
		  and (not $6::boolean or i.closed_at is null)
		  and ((cardinality($7::smallint[]) = 0 and not $8::boolean)
		       or i.priority = any($7::smallint[])
		       or ($8::boolean and i.priority is null))`

// issueListOrder is the listing's order, over the issue aliased `i`: the lifecycle status, the
// rank, the creation time, then the key. The paged text orders the filtered keys by it and the
// statement orders its answer by it again, since a join keeps no order of its own.
var issueListOrder = issueStatusCase + `, i.rank asc, i.created_at asc, i.key asc`

// issueClaimColumns is the claim half of an issue select, named once like
// issueComponentsColumns so every read scans the two columns in one order.
const issueClaimColumns = `i.claimed_by, i.claimed_at`

// issueProgressColumns are an issue's progress in one order (progressScan): the task counts a
// version write stored on the row, and its direct children counted on this read, every status,
// done being `status = 'done'`. A grouped read answers no row for an issue with no child, where
// the single-issue lateral answers 0, so both coalesce to the count progressScan expects.
const issueProgressColumns = `i.tasks_done, i.tasks_total, coalesce(progress.children_done, 0), coalesce(progress.children_total, 0)`

// issueProgressLateral counts one issue's direct children in one pass over the partial
// issues_parent_key index, for the single-issue read; a listing counts the children of its whole
// page in one grouped read instead (issueSummaryJoins).
const issueProgressLateral = `
	left join lateral (
		select count(*) filter (where c.status = 'done') as children_done, count(*) as children_total
		from issues c where c.parent_key = i.key
	) progress on true`

// issueSummaryColumns are the columns a listing row scans, in issueSummaryRow's order.
const issueSummaryColumns = `i.key, i.title, i.status, i.priority, i.rank, i.labels, i.parent_key,
	       coalesce(blockers.keys, '{}'),
	       i.assignee, i.route, i.updated_at, i.last_seq,
	       coalesce(spec.id, ''),
	       ` + issueClaimColumns + `,
	       case when i.closed_at is null then coalesce(open_asks.total, 0) else 0 end,
	       ` + issueProgressColumns + `,
	       ` + issueComponentsColumns + `,
	       ` + refs.IssueRepoColumns

// issueSummaryJoins resolve the six values a listing row carries beside the issue's own columns,
// each for every issue of `subjects` at once rather than for one row at a time. The open-ask
// read keeps the state filter in its own where clause, so it matches the partial
// asks_open(issue_key) where state = 'open' index instead of scanning asks, and the closed-issue
// rule stays in the select list: a closed issue reports 0 open asks however many it has.
var issueSummaryJoins = `left join (
		select l.issue_key, array_agg(l.target_key order by l.target_key) as keys
		from issue_links l
		where l.kind = 'blocked_by' and l.issue_key in (select key from subjects)
		group by l.issue_key
	) blockers on blockers.issue_key = i.key
	left join (
		select a.issue_key, a.id::text as id
		from artifacts a
		where a.is_primary and a.issue_key in (select key from subjects)
	) spec on spec.issue_key = i.key
	left join (
		select a.issue_key, count(*) as total
		from asks a
		where a.state = 'open' and a.issue_key in (select key from subjects)
		group by a.issue_key
	) open_asks on open_asks.issue_key = i.key
	left join (
		select c.parent_key, count(*) filter (where c.status = 'done') as children_done, count(*) as children_total
		from issues c
		where c.parent_key in (select key from subjects)
		group by c.parent_key
	) progress on progress.parent_key = i.key
	` + issueComponentsJoin + `
	` + refs.IssueRepoJoin

const (
	maxIssueLabels  = 20
	maxIssueLabel16 = 40
	// unsetPriorityFilter is the `priority` query value that matches an issue with no priority.
	unsetPriorityFilter = "none"
)

// parsePriorityFilter reads the repeated `priority` query parameter: each value is 0..3, or
// "none" for an issue with no priority. No values means no priority filter.
func parsePriorityFilter(values []string) (priorities []int16, unset bool, err error) {
	priorities = []int16{}
	for _, raw := range values {
		value := strings.TrimSpace(raw)
		if value == unsetPriorityFilter {
			unset = true
			continue
		}
		switch value {
		case "0", "1", "2", "3":
			priorities = append(priorities, int16(value[0]-'0'))
		default:
			return nil, false, errorf(http.StatusBadRequest, "INVALID_PRIORITY", "priority filter values must be 0 to 3 or %s, got %q", unsetPriorityFilter, raw)
		}
	}
	return priorities, unset, nil
}

// claimScan reads an issue's two claim columns as one nullable claim. A row has both or
// neither (the issues_claim_complete constraint), so either absent means unclaimed.
type claimScan struct {
	actor []byte
	at    *time.Time
}

func (c *claimScan) targets() []any { return []any{&c.actor, &c.at} }

func (c *claimScan) resolve(key string) (*model.IssueClaim, error) {
	if len(c.actor) == 0 || c.at == nil {
		return nil, nil
	}
	claim := model.IssueClaim{At: *c.at}
	if err := json.Unmarshal(c.actor, &claim.Actor); err != nil {
		return nil, fmt.Errorf("decode issue %s claim actor: %w", key, err)
	}
	return &claim, nil
}

// progressScan reads issueProgressColumns for one issue. The task columns are null together on an
// issue whose spec was never counted (issues_task_progress_complete), and a counted spec with no
// task item is total 0: both answer nil, so the API's `tasks: null` means "no task list" either
// way. Children is nil for an issue with no child.
type progressScan struct {
	tasksDone, tasksTotal       *int
	childrenDone, childrenTotal int
}

func (p *progressScan) targets() []any {
	return []any{&p.tasksDone, &p.tasksTotal, &p.childrenDone, &p.childrenTotal}
}

func (p *progressScan) resolve() model.IssueProgress {
	var progress model.IssueProgress
	if p.tasksTotal != nil && *p.tasksTotal > 0 {
		progress.Tasks = &model.ProgressCount{Done: *p.tasksDone, Total: *p.tasksTotal}
	}
	if p.childrenTotal > 0 {
		progress.Children = &model.ProgressCount{Done: p.childrenDone, Total: p.childrenTotal}
	}
	return progress
}

func normalizeIssueLabels(values []string) ([]string, error) {
	if len(values) > maxIssueLabels {
		return nil, countExceededError("LABELS_INPUT", "labels", len(values), maxIssueLabels)
	}
	labels := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		label := strings.TrimSpace(value)
		if length := itemblocks.Units16(label); length == 0 || length > maxIssueLabel16 {
			return nil, errorf(http.StatusBadRequest, "LABELS_INPUT", "each label must be 1 to %d characters", maxIssueLabel16)
		}
		key := strings.ToLower(label)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		labels = append(labels, label)
	}
	return labels, nil
}

func (s *server) listIssues(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	pinned := query.Get("pinned") == "true"
	login := ""
	if pinned {
		actor, ok := s.actingPerson(w, r)
		if !ok {
			return
		}
		login = actor.ID
	} else if !s.requireAuthenticated(w, r) {
		return
	}
	project := strings.TrimSpace(query.Get("project"))
	status := strings.TrimSpace(query.Get("status"))
	parent := strings.TrimSpace(query.Get("parent"))
	labels, err := normalizeIssueLabels(query["label"])
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	for index, label := range labels {
		labels[index] = strings.ToLower(label)
	}
	var updatedSince *time.Time
	if query.Has("updated_since") {
		parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(query.Get("updated_since")))
		if err != nil {
			writeError(w, "INVALID_UPDATED_SINCE", http.StatusBadRequest, "updated_since must be an RFC3339 timestamp")
			return
		}
		updatedSince = &parsed
	}
	priorities, unsetPriority, err := parsePriorityFilter(query["priority"])
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	routeStatus, err := parseRouteStatusFilter(query.Get("route_status"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	page, err := parseIssuePage(query)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// A route matters only while there is work to reach, so the route_status filter reads open
	// issues alone: the owner audit is every open issue whose route reaches nobody.
	open := query.Get("open") == "true" || routeStatus != ""
	arguments := []any{project, status, parent, updatedSince, labels, open, priorities, unsetPriority}
	if pinned {
		arguments = append(arguments, login)
	}
	// SQL cuts the page only when no route_status filter runs after the read: that filter needs
	// the Envoy roster, which this read cannot reach, and total counts what is left after it.
	inSQL := page != nil && routeStatus == ""
	listQuery := listIssuesQuery
	switch {
	case pinned && inSQL:
		listQuery = listPinnedIssuesPageQuery
	case pinned:
		listQuery = listPinnedIssuesQuery
	case inSQL:
		listQuery = listIssuesPageQuery
	}
	var issues []model.IssueSummary
	total := 0
	if inSQL {
		arguments = append(arguments, page.limit, page.offset)
		issues, total, err = s.scanIssuePage(r.Context(), listQuery, arguments)
	} else {
		issues, err = s.scanIssueSummaries(r.Context(), listQuery, arguments)
	}
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if routed := slices.ContainsFunc(issues, func(issue model.IssueSummary) bool { return issue.Route != nil }); routed {
		judged := routeStatus != "" && routeStatus != model.RouteUnknown
		var roster routeRoster
		if judged {
			// A filter for a judged state cannot be cut from an answer it could not read: a failed
			// read of the stored answer is the database's failure and is answered as one, and only
			// a listener that did not answer is the listener's.
			if roster, err = s.readRouteRoster(r.Context()); err != nil {
				s.writeHandlerError(w, err)
				return
			}
		} else {
			roster = s.routeRosterOrUnknown(r.Context())
		}
		if judged && !roster.answered {
			writeError(w, "ENVOY_UNAVAILABLE", http.StatusServiceUnavailable,
				"the Envoy listener did not answer, so Dispatch cannot tell which routes reach a live session; route_status=unknown lists the routed issues")
			return
		}
		for index := range issues {
			issues[index].IssueRouteReach = roster.reach(issues[index].Route)
		}
	}
	if routeStatus != "" {
		issues = slices.DeleteFunc(issues, func(issue model.IssueSummary) bool {
			return issue.RouteStatus == nil || *issue.RouteStatus != routeStatus
		})
	}
	if inSQL {
		WriteJSON(w, http.StatusOK, page.holding(issues, total))
		return
	}
	// A route_status page is cut from the listing every filter has narrowed, that one included,
	// so total counts what the caller's filters match.
	if page != nil {
		WriteJSON(w, http.StatusOK, page.of(issues))
		return
	}
	WriteJSON(w, http.StatusOK, issues)
}

// issueSummaryRow holds one listing row as the statement answers it. Scanning and resolving are
// two steps because a paged read answers one row that is no part of its page (issueSummaryQuery
// says why), and resolving that row would refuse a page over a parent chain of its own.
type issueSummaryRow struct {
	issue      model.IssueSummary
	claim      claimScan
	progress   progressScan
	components componentsScan
	repo       repoScan
}

// targets are the row's scan targets, after leading, which a paged read fills with the columns
// it carries ahead of the issue's own.
func (r *issueSummaryRow) targets(leading ...any) []any {
	targets := append(leading, &r.issue.Key, &r.issue.Title, &r.issue.Status, &r.issue.Priority, &r.issue.Rank, &r.issue.Labels, &r.issue.Parent, &r.issue.BlockedBy, &r.issue.Assignee, &r.issue.Route, &r.issue.UpdatedAt, &r.issue.LastSeq, &r.issue.PrimaryArtifactID)
	targets = append(targets, r.claim.targets()...)
	targets = append(targets, &r.issue.OpenAsks)
	targets = append(targets, r.progress.targets()...)
	targets = append(targets, r.components.targets()...)
	return append(targets, r.repo.targets()...)
}

// resolve turns the scanned row into the summary the response carries, refusing an issue whose
// parent chain the repository walk could not finish (repoScan.resolve).
func (r *issueSummaryRow) resolve() (model.IssueSummary, error) {
	issue := r.issue
	var err error
	if issue.Repo, issue.ResolvedRepo, err = r.repo.resolve(issue.Key); err != nil {
		return model.IssueSummary{}, err
	}
	claim, err := r.claim.resolve(issue.Key)
	if err != nil {
		return model.IssueSummary{}, err
	}
	issue.Claim = claim
	issue.Progress = r.progress.resolve()
	issue.Components = r.components.resolve(issue.Key)
	return issue, nil
}

// scanIssueSummaries runs an unpaged list query and returns its rows with the pooled connection
// already released, so the listener read that follows holds nothing.
func (s *server) scanIssueSummaries(ctx context.Context, listQuery string, arguments []any) ([]model.IssueSummary, error) {
	rows, err := s.deps.Store.Pool.Query(ctx, listQuery, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	issues := []model.IssueSummary{}
	for rows.Next() {
		var row issueSummaryRow
		if err := rows.Scan(row.targets()...); err != nil {
			return nil, err
		}
		issue, err := row.resolve()
		if err != nil {
			return nil, err
		}
		issues = append(issues, issue)
	}
	return issues, rows.Err()
}

// scanIssuePage runs a paged list query and returns the page's rows and the count of every issue
// the filters match, which the same statement made of the same snapshot. The row the statement
// answers for an empty page carries that count alone: it is dropped before it is resolved, so a
// parent chain of an issue off the page refuses nothing. No row at all is an empty filtered set,
// whose count is 0.
func (s *server) scanIssuePage(ctx context.Context, listQuery string, arguments []any) ([]model.IssueSummary, int, error) {
	rows, err := s.deps.Store.Pool.Query(ctx, listQuery, arguments...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	issues := []model.IssueSummary{}
	total := 0
	for rows.Next() {
		var row issueSummaryRow
		var inPage bool
		if err := rows.Scan(row.targets(&total, &inPage)...); err != nil {
			return nil, 0, err
		}
		if !inPage {
			continue
		}
		issue, err := row.resolve()
		if err != nil {
			return nil, 0, err
		}
		issues = append(issues, issue)
	}
	return issues, total, rows.Err()
}

// getIssue answers the issue page's read. Its database work is two batches, each one round trip
// on one pooled connection once that connection has prepared their statements (pgx prepares a
// statement it has not cached in a round trip of its own first, as before a single query), where
// it was a statement at a time: the issue row, its links, its artifacts, a version query per
// artifact, its documents' reviews and approval requests, its open asks with their events and
// counts, its children and its own backlink count, every one of them a connection taken and
// given back in turn (store.sharedPoolSize says what that costs a loaded server).
//
// Batch 1 is everything keyed on the issue key alone: the issue row, its external links, its
// artifacts, its open asks, its children and its backlink count. A key no issue carries is what
// the issue row's callback records, answered as that row's pgx.ErrNoRows - 404 NOT_FOUND - once
// the batch has closed: a callback that returned the error would close the batch on it, and pgx
// invalidates every statement of a batch whose Close answers an error in the connection's
// statement and description caches (invalidateCachesOnBatchResultsError), so one 404 would cost
// that connection the whole read's prepared statements. The other callbacks run against an issue
// with no row, answer empty lists, and readIssue drops them with the rest of the read.
//
// Batch 2 is what only batch 1's answers name: every artifact's versions in one statement, the
// documents' latest reviews and open approval requests, and the open asks' identity events and
// backlink counts. An issue with no artifact and no open ask names nothing, so it makes no
// second round trip at all.
//
// Each batch's results are closed before anything else reads: a batch holds its connection until
// then, and a second connection taken while it is held is the nesting store.ErrNestedAcquire exists
// to stop. The pool would not refuse it here: store.Pool.SendBatch checks the request's mark when
// the batch is sent but sets none while the batch is open, so no callback below reads the database.
// The route's reach is read after both (routeRosterOrUnknown), and only for an issue with a route:
// a statement of its own for the listener's stored answer (storedRouteRoster), and a shared listener
// fetch only when that answer is stale, so a routed issue's read is one round trip more.
func (s *server) getIssue(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	read, err := s.readIssue(r.Context(), r.PathValue("key"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if read.Route != nil {
		read.IssueRouteReach = s.routeRosterOrUnknown(r.Context()).reach(read.Route)
	}
	WriteJSON(w, http.StatusOK, read)
}

// issueDetail is everything GET /api/v1/issues/{key} answers, in the shape its response carries:
// readIssue fills every field but the route's reach, which getIssue takes from the roster, and the
// batch test builds one from the sequential loaders, so one type is the encoding all three answer.
type issueDetail struct {
	model.Issue
	model.IssueRouteReach
	Progress  model.IssueProgress `json:"progress"`
	Artifacts []model.Artifact    `json:"artifacts"`
	OpenAsks  []issueOpenAsk      `json:"open_asks"`
	Children  []model.IssueChild  `json:"children"`
	// ReferencedByCount is what the header's `Referenced by (N)` control names. The page
	// pays for it here, with the detail it already reads, instead of a graph request of
	// its own; the edges themselves are read only when the reader opens the panel.
	ReferencedByCount int `json:"referenced_by_count"`
}

// readIssue runs getIssue's two batches, whose statements and scans are the ones the sequential
// loaders run - loadIssueWithProgress, loadArtifacts, queryOwnerAsks with issueOpenAsksOf,
// issueChildrenStatement with scanIssueChildren and refs.BacklinkCounts - so the two paths answer
// the same bytes.
func (s *server) readIssue(ctx context.Context, key string) (issueDetail, error) {
	var read issueDetail
	var links []model.ExternalLink
	var asks []model.Ask
	var replies map[string]model.AskLastReply
	issueCounts := map[string]int{}
	missing := false

	first := &pgx.Batch{}
	first.Queue(issueRowStatement, key).QueryRow(func(row pgx.Row) error {
		issue, progress, err := scanIssueRow(row)
		if errors.Is(err, pgx.ErrNoRows) {
			missing = true
			return nil
		}
		read.Issue, read.Progress = issue, progress
		return err
	})
	first.Queue(issueExternalLinksStatement, key).Query(func(rows pgx.Rows) error {
		scanned, err := scanExternalLinks(rows)
		links = scanned
		return err
	})
	first.Queue(issueArtifactsStatement, key).Query(func(rows pgx.Rows) error {
		scanned, err := collectArtifacts(rows)
		read.Artifacts = scanned
		return err
	})
	first.Queue(listIssueAsksQueryOpen, key).Query(func(rows pgx.Rows) error {
		scannedAsks, scannedReplies, err := scanAskReads(rows)
		asks, replies = scannedAsks, scannedReplies
		return err
	})
	first.Queue(issueChildrenStatement, key).Query(func(rows pgx.Rows) error {
		scanned, err := scanIssueChildren(rows)
		read.Children = scanned
		return err
	})
	if err := refs.QueueBacklinkCounts(first, "issue", []string{key}, nil, issueCounts); err != nil {
		return issueDetail{}, err
	}
	if err := s.deps.Store.Pool.SendBatch(ctx, first).Close(); err != nil {
		return issueDetail{}, err
	}
	if missing {
		return issueDetail{}, pgx.ErrNoRows
	}
	read.Issue.ExternalLinks = links
	read.ReferencedByCount = issueCounts[read.Issue.Key]

	if err := s.readIssueDetails(ctx, artifactPointers(read.Artifacts), askReadPointers(asks)); err != nil {
		return issueDetail{}, err
	}
	read.OpenAsks = issueOpenAsksOf(asks, replies)
	return read, nil
}

// readIssueDetails is getIssue's second batch: what only the first batch's answers name, filled
// into the artifacts and asks it read, by the one queue-and-apply each of them has
// (queueArtifactDetails, queueAskDetails). An issue with no artifact and no open ask names
// nothing, so it queues nothing and sends no batch.
func (s *server) readIssueDetails(ctx context.Context, artifacts []*model.Artifact, asks []*model.Ask) error {
	batch := &pgx.Batch{}
	applyArtifacts := queueArtifactDetails(batch, artifacts)
	applyAsks := queueAskDetails(batch, asks)
	if batch.Len() == 0 {
		return nil
	}
	if err := s.deps.Store.Pool.SendBatch(ctx, batch).Close(); err != nil {
		return err
	}
	applyArtifacts()
	return applyAsks()
}

// issueOpenAsk is an open ask on the issue detail together with the newest
// comment in its thread — the same last_reply the inbox row carries — so the
// issue header can show who spoke last beside the ask's waiting_on.
type issueOpenAsk struct {
	model.Ask
	LastReply *model.AskLastReply `json:"last_reply"`
}

// issueOpenAsksOf pairs each ask with the newest reply in its thread, the shape the detail
// response lists; an ask nobody has replied to carries a null last_reply.
func issueOpenAsksOf(asks []model.Ask, replies map[string]model.AskLastReply) []issueOpenAsk {
	openAsks := make([]issueOpenAsk, len(asks))
	for index, ask := range asks {
		openAsks[index] = issueOpenAsk{Ask: ask}
		if reply, ok := replies[ask.ID]; ok {
			openAsks[index].LastReply = &reply
		}
	}
	return openAsks
}

func (s *server) loadIssue(ctx context.Context, q queryer, key string) (model.Issue, error) {
	issue, _, err := s.loadIssueWithProgress(ctx, q, key)
	return issue, err
}

// issueRowStatement reads one issue with its progress, $1 its key; scanIssueRow decodes it. It
// is the issue read's first queued statement (getIssue), whose callback records a missing issue
// for the read to answer as this row's pgx.ErrNoRows whatever the rest of the batch found. The
// text is built once, at start, rather than on every read: its component and repository terms
// are package-level values of their own, so the concatenation is work a request would repeat
// for a statement that never differs, and pgx caches a prepared plan per statement text.
var issueRowStatement = `
		with recursive subjects as (select $1::text as key),
		` + issueComponentsTerms + `,
		` + refs.IssueRepoTerms + `
		select i.key, i.project_key, i.number, i.title, i.status, i.priority, i.rank, i.labels, i.parent_key,
		       coalesce((select array_agg(l.target_key order by l.target_key) from issue_links l where l.issue_key = i.key and l.kind = 'blocked_by'), '{}'),
		       i.assignee, i.route, i.created_by, i.created_at, i.updated_at, i.closed_at,
		       coalesce((select a.id::text from artifacts a where a.issue_key = i.key and a.is_primary), ''),
		       coalesce((select a.id::text from artifacts a where a.issue_key = i.key and ` + aliasedVerificationRole + `), ''),
		       coalesce((select a.id::text from artifacts a where a.issue_key = i.key and ` + aliasedPlanRole + `), ''),
		       i.last_seq, ` + issueClaimColumns + `,
		       ` + issueProgressColumns + `,
		       ` + issueComponentsColumns + `,
		       ` + refs.IssueRepoColumns + `
		from issues i
		` + issueProgressLateral + `
		` + issueComponentsJoin + `
		` + refs.IssueRepoJoin + `
		where i.key = $1
	`

// issueExternalLinksStatement reads an issue's external links, $1 its key, in the order the
// response lists them; scanExternalLinks decodes them.
const issueExternalLinksStatement = `select url, kind from issue_external_links where issue_key = $1 order by url`

// scanIssueRow decodes one issueRowStatement row into the issue and the progress the read
// carries beside it. ExternalLinks is left nil: issueExternalLinksStatement carries them, run
// after this row or queued beside it.
func scanIssueRow(row pgx.Row) (model.Issue, model.IssueProgress, error) {
	var issue model.Issue
	var createdBy []byte
	var components componentsScan
	var claim claimScan
	var progress progressScan
	var repo repoScan
	targets := []any{
		&issue.Key, &issue.Project, &issue.Number, &issue.Title, &issue.Status, &issue.Priority, &issue.Rank, &issue.Labels,
		&issue.Parent, &issue.BlockedBy, &issue.Assignee, &issue.Route, &createdBy, &issue.CreatedAt, &issue.UpdatedAt, &issue.ClosedAt,
		&issue.PrimaryArtifactID, &issue.VerificationArtifactID, &issue.PlanArtifactID, &issue.LastSeq,
	}
	targets = append(targets, claim.targets()...)
	targets = append(targets, progress.targets()...)
	targets = append(targets, components.targets()...)
	if err := row.Scan(append(targets, repo.targets()...)...); err != nil {
		return model.Issue{}, model.IssueProgress{}, err
	}
	own, resolvedRepo, err := repo.resolve(issue.Key)
	if err != nil {
		return model.Issue{}, model.IssueProgress{}, err
	}
	issue.Repo, issue.ResolvedRepo = own, resolvedRepo
	issue.Components = components.resolve(issue.Key)
	resolved, err := claim.resolve(issue.Key)
	if err != nil {
		return model.Issue{}, model.IssueProgress{}, err
	}
	issue.Claim = resolved
	if err := json.Unmarshal(createdBy, &issue.CreatedBy); err != nil {
		return model.Issue{}, model.IssueProgress{}, fmt.Errorf("decode issue actor: %w", err)
	}
	return issue, progress.resolve(), nil
}

// scanExternalLinks reads the rows issueExternalLinksStatement returns. An issue with none keeps
// a non-nil empty list, which the response carries as [].
func scanExternalLinks(rows pgx.Rows) ([]model.ExternalLink, error) {
	links := []model.ExternalLink{}
	for rows.Next() {
		var link model.ExternalLink
		if err := rows.Scan(&link.URL, &link.Kind); err != nil {
			return nil, err
		}
		links = append(links, link)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return links, nil
}

// loadIssueWithProgress is loadIssue together with the issue's progress, which the read response
// carries beside the issue (model.IssueProgress says why it is not a field of it).
func (s *server) loadIssueWithProgress(ctx context.Context, q queryer, key string) (model.Issue, model.IssueProgress, error) {
	issue, progress, err := scanIssueRow(q.QueryRow(ctx, issueRowStatement, key))
	if err != nil {
		return model.Issue{}, model.IssueProgress{}, err
	}
	rows, err := q.Query(ctx, issueExternalLinksStatement, key)
	if err != nil {
		return model.Issue{}, model.IssueProgress{}, err
	}
	defer rows.Close()
	links, err := scanExternalLinks(rows)
	if err != nil {
		return model.Issue{}, model.IssueProgress{}, err
	}
	issue.ExternalLinks = links
	return issue, progress, nil
}

// issueChildrenStatement reads the issue's direct children, $1 its key, each with a rollup over
// its whole subtree (the child itself included, every status): done/total counts and the newest
// updated_at. The recursive walk uses `union` with parentDepthCap like refs.Closure, so it
// terminates even if a raced reparent ever commits a cycle. scanIssueChildren decodes its rows.
//
// The walk reads each subtree issue's row already, so it carries that row's status and
// updated_at and the rollup counts them where it stands. Joining the walk's keys back to issues
// instead read the whole table: the planner has no estimate of a recursive CTE's size, so it
// hashed all 20,000 issues rather than key 1,834 primary-key lookups, which was 2,499 of the
// statement's 8,097 buffers on the measurement database. The two columns are functionally
// determined by the key the walk already carries, so `union` removes exactly the rows it
// removed before.
var issueChildrenStatement = `
		with recursive subtree as (
			select key as root, key, 1 as depth, status, updated_at from issues where parent_key = $1
			union
			select s.root, i.key, s.depth + 1, i.status, i.updated_at
			from issues i join subtree s on i.parent_key = s.key
			where s.depth < ` + parentDepthCapSQL + `
		), rollup as (
			select s.root,
			       count(distinct s.key) as total,
			       count(distinct s.key) filter (where s.status = 'done') as done,
			       max(s.updated_at) as active_at
			from subtree s
			group by s.root
		)
		select c.key, c.title, c.status, r.done, r.total, r.active_at,
		       coalesce((select json_agg(json_build_object('url', l.url, 'kind', l.kind) order by l.url)
		                 from issue_external_links l where l.issue_key = c.key), '[]')
		from issues c join rollup r on r.root = c.key
		where c.parent_key = $1 order by c.key
	`

// scanIssueChildren reads the rows issueChildrenStatement returns. An issue with no child keeps
// a non-nil empty list, which the response carries as [].
func scanIssueChildren(rows pgx.Rows) ([]model.IssueChild, error) {
	children := []model.IssueChild{}
	for rows.Next() {
		var child model.IssueChild
		var links []byte
		if err := rows.Scan(&child.Key, &child.Title, &child.Status,
			&child.SubtreeDone, &child.SubtreeTotal, &child.ActiveAt, &links); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(links, &child.ExternalLinks); err != nil {
			return nil, err
		}
		children = append(children, child)
	}
	return children, rows.Err()
}
