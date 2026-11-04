// Package metrics computes the Delivery page's four Legion measures — work time to merge, phase
// time by worker type, rework, and splitting and follow-ups (dispatch://INFRA-1077, the spec's
// "The four Legion metrics" and "Rule details").
//
// It is split the way api/delivery_pipeline.go is split: Read makes every database and GitHub read
// one answer needs, and Compute turns that population into the answer and touches nothing, so the
// route and the live hub share one body rather than two implementations of the rules.
//
// Two disciplines run through it:
//
//   - The daemon's state is read loosely. What Dispatch holds of it is legion_observations, whose
//     fields are legion/state.go's; a field a daemon does not send is a field this package has no
//     observation of, never an assumption. An issue's tree comes from the daemon's own `tree`
//     where a read carried one (dispatch://INFRA-980 adds it) and from the parent chain
//     otherwise, so nothing here takes a slot holder for a root.
//   - The precedence of the phase-time segments lives in exactly one ordered list, segments.go's
//     `precedence`, and each segment is one function.
package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// askTurnEventTypes are the ask events a person's turn starts and ends with: the two that hand an
// ask to a person and the two that take it back (api/events.go names the same family).
var askTurnEventTypes = []string{"ask.opened", "ask.handed_back", "ask.answered", "ask.resolved"}

// fetchedMergeTimesCap bounds how many pull requests one answer fetches from GitHub for a merge
// time Dispatch has not already stored. Every finished issue whose pull request the delivery
// intake has reconciled reads from delivery_pull_requests; this is the tail of issues closed since
// the last reconcile, and a window that names hundreds of them is a backfill, not a dashboard.
const fetchedMergeTimesCap = 50

// fetchedMergeTimesConcurrency bounds how many of those fetches are in flight at once. Each one
// mints an installation token and makes two GitHub calls, so a serial pass over the cap is up to
// 150 round trips inside one request; the bound is the one the broadcast resolver holds its own
// fan-out to (api/broadcasts.go's broadcastResolveConcurrency).
const fetchedMergeTimesConcurrency = 8

// Mark is one value of one field as of a moment: an observation the sampler recorded, or a status
// an event snapshotted. The value of an observation is the JSON text legion_observations holds; a
// status's is the status itself.
type Mark struct {
	At    time.Time
	Value string
}

// Handoff is one handoffs row: which phase's handoff, which version, and when it was recorded.
type Handoff struct {
	Phase   string
	Version int
	At      time.Time
}

// Issue is one issue of the population with every fact the rules read of it.
type Issue struct {
	Key       string
	Title     string
	Priority  *int
	Parent    string
	CreatedAt time.Time
	ClosedAt  *time.Time

	// Tree is the issue's tree root as the daemon named it, from the `tree` field
	// dispatch://INFRA-980 adds to each issue of `GET /legion/v1/state`, where a read carried
	// one. Empty is "no read named one", which is every daemon before that change: treeRoot then
	// walks the parent chain. Nothing reads a slot to decide a root.
	Tree string

	// Children are the issues of the population whose tree root is this issue, this issue aside.
	Children []string

	// FirstRoleEvent is the first event of a session titled `Legion <role> · <this issue's key>`,
	// by role: what the phase fallback starts a role's phase at, and what makes a closed issue
	// one that finished with Legion phase work.
	FirstRoleEvent map[string]time.Time

	// Handoffs are the issue's handoff records, oldest first.
	Handoffs []Handoff

	// StatusMarks is every status the issue's own events snapshotted, oldest first.
	StatusMarks []Mark

	// Observed is every value a read found changed, by legion_observations field (legion's
	// FieldPhase, FieldSlot, ClaimFieldPrefix+role and the rest), oldest first within a field.
	Observed map[string][]Mark

	// AskTurns are the spans in which an ask a Legion session of this issue's tree opened on
	// this issue stood on a person's turn, from its opening or hand-back to its answer or
	// resolution, and to the issue's end where neither came (keepTreeAsks).
	AskTurns []Span

	// MergedAt is the merge time of the pull request Dispatch holds for the issue, where it holds
	// one.
	MergedAt *time.Time
}

// Filed is one issue a Legion session created, as metric 4 classes it: the issue, the moment, and
// the session's own title parsed into the role and issue (or controller) that filed it. The parent
// and ancestors are the ones the issue had at its `issue.created` event, not the ones it has now.
// The class is read off the parent alone, by walking the chain above it: Dispatch writes an empty
// `ancestors` for an issue filed under a child, so the list is recorded as the event carried it
// and no rule turns on it (the spec's "Rule details — The tree of a filed issue").
type Filed struct {
	Key       string
	Title     string
	At        time.Time
	Session   string
	Filer     SessionTitle
	Parent    string
	Ancestors []string
}

// Population is every fact one metrics answer is computed from.
type Population struct {
	Project string
	From    time.Time
	To      time.Time

	// Issues is every issue of the project the read touched, by key; Keys lists them in order so
	// a pass over them is deterministic.
	Issues map[string]*Issue
	Keys   []string

	// Filed is every issue a Legion session created in the window, in creation order.
	Filed []Filed

	// askTurns is every ask turn the read gathered, before the tree rule picked the ones that
	// count for the issue they were opened on. The rule reads each issue's tree, which the
	// observations of the same read name, so it runs once every read has landed (keepTreeAsks).
	askTurns []askTurn
}

// Read is every database and GitHub read one metrics answer makes: the project's issues, the
// Legion phase work the event log records for the ones that could have finished in the window,
// the handoffs and observations of those issues, the issues Legion sessions created in the window,
// and the merge time of each finished issue's pull request.
//
// github may be nil, and a GitHub read that fails is not an error: a merge time Dispatch cannot
// reach leaves that issue with none, since no metric's rule is written in terms of one.
func Read(ctx context.Context, pool *store.Pool, github *githubapp.Client, project string, from, to time.Time) (*Population, error) {
	population := &Population{Project: project, From: from, To: to, Issues: map[string]*Issue{}}
	if err := population.readIssues(ctx, pool, project); err != nil {
		return nil, err
	}
	if err := population.readFiled(ctx, pool, project, from, to); err != nil {
		return nil, err
	}
	keys := population.scopeKeys(from, to)
	// The six reads below take only those keys and write disjoint fields of each issue, so they
	// run together rather than one after another, as the delivery population's independent reads
	// do (api/delivery.go). errgroup answers the first error recorded, so which of two failing
	// reads names the failure is no longer fixed.
	independent := []func(ctx context.Context) error{
		func(ctx context.Context) error { return population.readRoleEvents(ctx, pool, keys) },
		func(ctx context.Context) error { return population.readStatusMarks(ctx, pool, keys) },
		func(ctx context.Context) error { return population.readAskTurns(ctx, pool, keys) },
		func(ctx context.Context) error { return population.readHandoffs(ctx, pool, keys) },
		func(ctx context.Context) error { return population.readObservations(ctx, pool, project, keys) },
		func(ctx context.Context) error { return population.readMergeTimes(ctx, pool, github, keys) },
	}
	reads, readsCtx := errgroup.WithContext(ctx)
	read := func(fn func(ctx context.Context) error) error {
		readCtx, err := store.ForConcurrentRead(readsCtx)
		if err != nil {
			return err
		}
		reads.Go(func() error { return fn(readCtx) })
		return nil
	}
	for _, fn := range independent {
		if err := read(fn); err != nil {
			_ = reads.Wait()
			return nil, err
		}
	}
	if err := reads.Wait(); err != nil {
		return nil, err
	}
	population.linkTrees()
	population.keepTreeAsks()
	return population, nil
}

// readIssues reads the project's issues. It reads all of them rather than the window's: the window
// names which issues finished, while the rules read their whole trees (a child that finished in
// the window belongs to a root that may not have) and each issue's whole life (the work time runs
// from its first worker event, which is older than the window whenever the issue is).
func (p *Population) readIssues(ctx context.Context, pool *store.Pool, project string) error {
	rows, err := pool.Query(ctx, `
		select key, title, priority, parent_key, created_at, closed_at
		from issues
		where project_key = $1
		order by key
	`, project)
	if err != nil {
		return fmt.Errorf("read the issues of %s: %w", project, err)
	}
	defer rows.Close()
	for rows.Next() {
		issue := &Issue{FirstRoleEvent: map[string]time.Time{}, Observed: map[string][]Mark{}}
		var parent *string
		if err := rows.Scan(&issue.Key, &issue.Title, &issue.Priority, &parent, &issue.CreatedAt, &issue.ClosedAt); err != nil {
			return fmt.Errorf("scan an issue of %s: %w", project, err)
		}
		if parent != nil {
			issue.Parent = *parent
		}
		p.Issues[issue.Key] = issue
		p.Keys = append(p.Keys, issue.Key)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read the issues of %s: %w", project, err)
	}
	return nil
}

// readFiled reads every `issue.created` event of the window a session wrote, whatever project the
// new issue belongs to — an issue filed out of the filer's tree is one of metric 4's classes — and
// keeps the ones a Legion session of this project filed.
func (p *Population) readFiled(ctx context.Context, pool *store.Pool, project string, from, to time.Time) error {
	rows, err := pool.Query(ctx, `
		select coalesce(events.issue_key, ''), coalesce(issues.title, ''), events.created_at,
			coalesce(events.actor->>'id', ''), coalesce(events.payload->>'parent', ''),
			coalesce(events.payload->'ancestors', '[]'::jsonb)
		from events
		left join issues on issues.key = events.issue_key
		where events.type = 'issue.created'
			and events.actor->>'kind' = 'session'
			and events.created_at >= $1 and events.created_at < $2
		order by events.created_at, events.id
	`, from, to)
	if err != nil {
		return fmt.Errorf("read the issues filed in the window: %w", err)
	}
	defer rows.Close()
	var filed []Filed
	sessions := map[string]bool{}
	for rows.Next() {
		var one Filed
		var ancestors []byte
		if err := rows.Scan(&one.Key, &one.Title, &one.At, &one.Session, &one.Parent, &ancestors); err != nil {
			return fmt.Errorf("scan a filed issue: %w", err)
		}
		if one.Key == "" || one.Session == "" {
			continue
		}
		if err := json.Unmarshal(ancestors, &one.Ancestors); err != nil {
			one.Ancestors = nil
		}
		filed = append(filed, one)
		sessions[one.Session] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read the issues filed in the window: %w", err)
	}
	titles, err := resolveTitles(ctx, pool, sessions)
	if err != nil {
		return err
	}
	for _, one := range filed {
		parsed, ok := ParseSessionTitle(titles[one.Session])
		if !ok || !parsed.OfProject(project) {
			continue
		}
		one.Filer = parsed
		p.Filed = append(p.Filed, one)
	}
	return nil
}

// scopeKeys is every issue the event, handoff and observation reads cover: the issues that could
// have finished in the window, every issue each one shares a tree with (its ancestors, since a
// child's root is a tree of its own, and their descendants), and the issues the window's filings
// name as their filer, whose phase at the filing metric 4 reports.
func (p *Population) scopeKeys(from, to time.Time) []string {
	wanted := map[string]bool{}
	var reach func(key string)
	reach = func(key string) {
		issue := p.Issues[key]
		if issue == nil || wanted[key] {
			return
		}
		wanted[key] = true
		if issue.Parent != "" {
			reach(issue.Parent)
		}
	}
	for _, key := range p.Keys {
		issue := p.Issues[key]
		if issue.ClosedAt != nil && !issue.ClosedAt.Before(from) && issue.ClosedAt.Before(to) {
			reach(key)
		}
	}
	for _, filed := range p.Filed {
		if filed.Filer.Issue != "" {
			reach(filed.Filer.Issue)
		}
	}
	// Every descendant of a reached issue, however deep: the roots reached above carry their
	// trees, which segment 4 and metric 4's children-per-root read.
	for added := true; added; {
		added = false
		for _, key := range p.Keys {
			issue := p.Issues[key]
			if !wanted[key] && issue.Parent != "" && wanted[issue.Parent] {
				wanted[key], added = true, true
			}
		}
	}
	keys := make([]string, 0, len(wanted))
	for _, key := range p.Keys {
		if wanted[key] {
			keys = append(keys, key)
		}
	}
	return keys
}

// readRoleEvents reads the first event each session wrote on each of the issues, and keeps the
// ones a session titled `Legion <role> · <that issue's key>` wrote. The first event per session is
// all the rule needs ("each role's phase starts at the first event of a session titled …"), so
// this reads one row per session and issue rather than every event of every issue.
func (p *Population) readRoleEvents(ctx context.Context, pool *store.Pool, keys []string) error {
	rows, err := pool.Query(ctx, `
		select issue_key, actor->>'id', min(created_at)
		from events
		where issue_key = any($1) and actor->>'kind' = 'session' and actor->>'id' is not null
		group by issue_key, actor->>'id'
	`, keys)
	if err != nil {
		return fmt.Errorf("read the Legion session events: %w", err)
	}
	defer rows.Close()
	type sessionEvent struct {
		issue, session string
		at             time.Time
	}
	var found []sessionEvent
	sessions := map[string]bool{}
	for rows.Next() {
		var one sessionEvent
		if err := rows.Scan(&one.issue, &one.session, &one.at); err != nil {
			return fmt.Errorf("scan a session event: %w", err)
		}
		found = append(found, one)
		sessions[one.session] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read the Legion session events: %w", err)
	}
	titles, err := resolveTitles(ctx, pool, sessions)
	if err != nil {
		return err
	}
	for _, one := range found {
		parsed, ok := ParseSessionTitle(titles[one.session])
		if !ok || parsed.Controller || parsed.Issue != one.issue {
			continue
		}
		issue := p.Issues[one.issue]
		if issue == nil {
			continue
		}
		if held, seen := issue.FirstRoleEvent[parsed.Role]; !seen || one.at.Before(held) {
			issue.FirstRoleEvent[parsed.Role] = one.at
		}
	}
	return nil
}

// readStatusMarks reads every status the issues' own events snapshotted: what the parked
// subtraction and the pre-observation review rounds read.
func (p *Population) readStatusMarks(ctx context.Context, pool *store.Pool, keys []string) error {
	rows, err := pool.Query(ctx, `
		select issue_key, created_at, payload->>'status'
		from events
		where issue_key = any($1) and type like 'issue.%' and payload->>'status' is not null
		order by created_at, id
	`, keys)
	if err != nil {
		return fmt.Errorf("read the status snapshots: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key, status string
		var at time.Time
		if err := rows.Scan(&key, &at, &status); err != nil {
			return fmt.Errorf("scan a status snapshot: %w", err)
		}
		if issue := p.Issues[key]; issue != nil {
			issue.StatusMarks = append(issue.StatusMarks, Mark{At: at, Value: status})
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read the status snapshots: %w", err)
	}
	return nil
}

// askTurn is one span an ask stood on a person's turn: the issue it was opened on, the issue the
// opening session's title named, and the span itself. The second is what the tree rule reads
// (keepTreeAsks); it is empty for a controller, whose title names a project and no issue.
type askTurn struct {
	issue string
	by    string
	span  Span
}

// readAskTurns reads the ask events of the issues and turns each ask into the spans it stood on a
// person's turn: from an opening or a hand-back by a Legion session to the answer or resolution
// that took it back, and to the issue's end where neither came. Which of them count for the
// issue is keepTreeAsks's rule, which runs once every read has landed.
func (p *Population) readAskTurns(ctx context.Context, pool *store.Pool, keys []string) error {
	rows, err := pool.Query(ctx, `
		select issue_key, created_at, type, coalesce(payload->>'id', ''), coalesce(actor->>'id', ''),
			coalesce(actor->>'kind', '')
		from events
		where issue_key = any($1) and type = any($2)
		order by created_at, id
	`, keys, askTurnEventTypes)
	if err != nil {
		return fmt.Errorf("read the ask events: %w", err)
	}
	defer rows.Close()
	type askEvent struct {
		issue, kind, ask, session, actorKind string
		at                                   time.Time
	}
	var events []askEvent
	sessions := map[string]bool{}
	for rows.Next() {
		var one askEvent
		if err := rows.Scan(&one.issue, &one.at, &one.kind, &one.ask, &one.session, &one.actorKind); err != nil {
			return fmt.Errorf("scan an ask event: %w", err)
		}
		if one.ask == "" {
			continue
		}
		events = append(events, one)
		if one.actorKind == "session" {
			sessions[one.session] = true
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read the ask events: %w", err)
	}
	titles, err := resolveTitles(ctx, pool, sessions)
	if err != nil {
		return err
	}
	open := map[string]askTurn{}
	for _, event := range events {
		switch event.kind {
		case "ask.opened", "ask.handed_back":
			if _, already := open[event.ask]; already {
				continue
			}
			if event.actorKind != "session" {
				continue
			}
			title, isLegion := ParseSessionTitle(titles[event.session])
			if !isLegion {
				continue
			}
			open[event.ask] = askTurn{issue: event.issue, by: title.Issue, span: Span{From: event.at}}
		case "ask.answered", "ask.resolved":
			turn, waiting := open[event.ask]
			if !waiting {
				continue
			}
			delete(open, event.ask)
			turn.span.To = event.at
			p.askTurns = append(p.askTurns, turn)
		}
	}
	for _, turn := range open {
		issue := p.Issues[turn.issue]
		if issue == nil {
			continue
		}
		turn.span.To = p.issueEnd(issue)
		p.askTurns = append(p.askTurns, turn)
	}
	return nil
}

// keepTreeAsks gives each issue the ask turns that count for it: "An ask counts for an issue when
// it is opened on that issue by any Legion session of the issue's tree, that is, a session titled
// `Legion <role> · <KEY>` where KEY is any issue of the same tree, walked as for splitting and
// follow-ups. An ask opened by a session outside the tree does not count" (the spec's "Rule
// details — Whose ask counts as waiting on a person"). The walk is TreeRoot, the one splitting
// and follow-ups walks with, so one reading of a tree serves both rules; a controller's session
// names a project and no issue, so it is of no tree and its asks count for nothing.
func (p *Population) keepTreeAsks() {
	roots := map[string]string{}
	rootOf := func(key string) string {
		if root, held := roots[key]; held {
			return root
		}
		root := p.TreeRoot(key)
		roots[key] = root
		return root
	}
	for _, turn := range p.askTurns {
		issue := p.Issues[turn.issue]
		if issue == nil || turn.by == "" || rootOf(turn.issue) != rootOf(turn.by) {
			continue
		}
		issue.AskTurns = append(issue.AskTurns, turn.span)
	}
	p.askTurns = nil
	for _, key := range p.Keys {
		if issue := p.Issues[key]; issue != nil {
			slices.SortFunc(issue.AskTurns, func(a, b Span) int { return a.From.Compare(b.From) })
		}
	}
}

func (p *Population) readHandoffs(ctx context.Context, pool *store.Pool, keys []string) error {
	rows, err := pool.Query(ctx, `
		select issue_key, phase, version, created_at
		from handoffs
		where issue_key = any($1)
		order by created_at, version
	`, keys)
	if err != nil {
		return fmt.Errorf("read the handoffs: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var handoff Handoff
		if err := rows.Scan(&key, &handoff.Phase, &handoff.Version, &handoff.At); err != nil {
			return fmt.Errorf("scan a handoff: %w", err)
		}
		if issue := p.Issues[key]; issue != nil {
			issue.Handoffs = append(issue.Handoffs, handoff)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read the handoffs: %w", err)
	}
	return nil
}

// readObservations reads every change the sampler recorded of the issues, whenever it recorded it:
// an observation older than the window is what says what the issue's phase was when the window
// opened. The field is kept as the sampler wrote it, so a field a later daemon adds (the `tree` of
// dispatch://INFRA-980) is read the day the sampler records it and is absent, not wrong, until
// then.
func (p *Population) readObservations(ctx context.Context, pool *store.Pool, project string, keys []string) error {
	rows, err := pool.Query(ctx, `
		select issue_key, field, value::text, observed_at
		from legion_observations
		where project = $1 and issue_key = any($2)
		order by observed_at, id
	`, project, keys)
	if err != nil {
		return fmt.Errorf("read the Legion observations of %s: %w", project, err)
	}
	defer rows.Close()
	for rows.Next() {
		var key, field, value string
		var at time.Time
		if err := rows.Scan(&key, &field, &value, &at); err != nil {
			return fmt.Errorf("scan a Legion observation: %w", err)
		}
		issue := p.Issues[key]
		if issue == nil {
			continue
		}
		issue.Observed[field] = append(issue.Observed[field], Mark{At: at, Value: value})
		if field == legion.FieldTree {
			if tree := legion.JSONString([]byte(value)); tree != "" {
				issue.Tree = tree
			}
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read the Legion observations of %s: %w", project, err)
	}
	return nil
}

// readMergeTimes reads the merge time of each issue's pull request from the delivery population,
// and fetches the ones that population does not hold yet from GitHub (delivery.FetchPullRequest),
// up to fetchedMergeTimesCap of them. A fetch that fails leaves the issue with no merge time.
func (p *Population) readMergeTimes(ctx context.Context, pool *store.Pool, github *githubapp.Client, keys []string) error {
	rows, err := pool.Query(ctx, `
		select issue_key, repo, number, merged_at
		from delivery_pull_requests
		where issue_key = any($1) and unfetchable_at is null
		order by issue_key, number
	`, keys)
	if err != nil {
		return fmt.Errorf("read the pull requests of the finished issues: %w", err)
	}
	defer rows.Close()
	type unmerged struct {
		issue, repo string
		number      int
	}
	var fetch []unmerged
	for rows.Next() {
		var key, repo string
		var number int
		var mergedAt *time.Time
		if err := rows.Scan(&key, &repo, &number, &mergedAt); err != nil {
			return fmt.Errorf("scan a pull request: %w", err)
		}
		issue := p.Issues[key]
		if issue == nil {
			continue
		}
		switch {
		case mergedAt == nil:
			fetch = append(fetch, unmerged{issue: key, repo: repo, number: number})
		case issue.MergedAt == nil || mergedAt.Before(*issue.MergedAt):
			issue.MergedAt = mergedAt
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read the pull requests of the finished issues: %w", err)
	}
	if github == nil {
		return nil
	}
	// The cap first, over the flat list, so the same entries are attempted as a serial pass
	// would attempt; then one group per issue, in the order the rows arrived. Each group runs
	// its own entries one after another, so the first pull request of an issue that answers a
	// merge time is still the one assigned and the rest of that issue's are still skipped;
	// groups write to distinct issues, so no two of them touch the same field.
	if len(fetch) > fetchedMergeTimesCap {
		fetch = fetch[:fetchedMergeTimesCap]
	}
	grouped := map[string][]unmerged{}
	var order []string
	for _, one := range fetch {
		if _, seen := grouped[one.issue]; !seen {
			order = append(order, one.issue)
		}
		grouped[one.issue] = append(grouped[one.issue], one)
	}
	fetches := new(errgroup.Group)
	fetches.SetLimit(fetchedMergeTimesConcurrency)
	for _, key := range order {
		fetches.Go(func() error {
			for _, one := range grouped[key] {
				issue := p.Issues[one.issue]
				if issue == nil || issue.MergedAt != nil {
					continue
				}
				owner, name, found := strings.Cut(one.repo, "/")
				if !found {
					continue
				}
				fetched, err := delivery.FetchPullRequest(ctx, github, owner, name, one.number)
				if err != nil {
					slog.Debug("dispatch: read a Legion issue's merge time", "issue", one.issue, "repo", one.repo, "number", one.number, "error", err)
					continue
				}
				issue.MergedAt = fetched.MergedAt
			}
			return nil
		})
	}
	// No group returns an error — a fetch that fails leaves the issue with no merge time, as a
	// serial pass left it — so this waits and nothing more.
	_ = fetches.Wait()
	return nil
}

// linkTrees fills each issue's children: the issues whose tree root is it.
func (p *Population) linkTrees() {
	for _, key := range p.Keys {
		root := p.TreeRoot(key)
		if root == key || p.Issues[root] == nil {
			continue
		}
		p.Issues[root].Children = append(p.Issues[root].Children, key)
	}
}

// TreeRoot is the issue's tree root, by the one rule the Legion view places an issue in a tree
// with (legion.TreeRoot): the `tree` the daemon named where a read carried one, and the topmost
// Legion issue of its parent chain otherwise. This population holds every issue of the project,
// Legion's and the rest, so an issue carrying Legion phase work is what the walk climbs through;
// an issue whose parent carries none is a root of its own, which is what keeps a Legion tree from
// climbing into the tracking issues above it.
func (p *Population) TreeRoot(key string) string {
	return legion.TreeRoot(key, func(key string) (legion.TreeNode, bool) {
		issue := p.Issues[key]
		if issue == nil {
			return legion.TreeNode{}, false
		}
		return legion.TreeNode{Tree: issue.Tree, Parent: issue.Parent, Legion: p.hasLegionWork(issue)}, true
	})
}

// isRoot is whether the issue is its own tree's root.
func (p *Population) isRoot(key string) bool { return p.TreeRoot(key) == key }

// hasLegionWork is whether the issue carries Legion phase work: an event of a session titled
// `Legion <role> · <its key>`, a handoff, or an observation the daemon's own state produced.
func (p *Population) hasLegionWork(issue *Issue) bool {
	return len(issue.FirstRoleEvent) > 0 || len(issue.Handoffs) > 0 || len(issue.Observed) > 0
}

// issueEnd is where the issue's timeline stops: its close, or the window's end for one still open.
func (p *Population) issueEnd(issue *Issue) time.Time {
	if issue.ClosedAt != nil {
		return *issue.ClosedAt
	}
	return p.To
}

// resolveTitles is each session's most recent known title (delivery.ResolveSessionTitles, which
// reads the one index Dispatch keeps of them).
func resolveTitles(ctx context.Context, pool *store.Pool, sessions map[string]bool) (map[string]string, error) {
	ids := make([]string, 0, len(sessions))
	for id := range sessions {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	titles, err := delivery.ResolveSessionTitles(ctx, pool, ids)
	if err != nil {
		return nil, fmt.Errorf("resolve the session titles: %w", err)
	}
	return titles, nil
}
