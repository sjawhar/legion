package metrics

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

// day is the fixture corpus's first moment, and `after` a whole number of hours past it.
var day = time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)

func after(hours float64) time.Time {
	return day.Add(time.Duration(hours * float64(time.Hour)))
}

// corpus writes the event log, handoffs and observations of one finished child of one root, the
// shapes Read gathers: a session titled by its role and issue, status snapshots, a handoff, the
// sampler's observations, a merged pull request, and an issue the implementer filed in the tree.
func corpus(t *testing.T) *store.Store {
	t.Helper()
	database := storetest.Open(t)
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := database.Pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed (%s): %v", sql, err)
		}
	}
	exec(`insert into projects (key, name) values ('INFRA', 'INFRA')`)
	exec(`insert into issues (key, project_key, number, title, status, rank, created_by, created_at)
		values ('INFRA-1', 'INFRA', 1, 'The root', 'in_progress', 'm', '{"kind":"user","id":"alice"}'::jsonb, $1)`, after(-1))
	exec(`insert into issues (key, project_key, number, title, status, rank, parent_key, created_by, created_at, closed_at)
		values ('INFRA-2', 'INFRA', 2, 'The child', 'done', 'n', 'INFRA-1', '{"kind":"user","id":"alice"}'::jsonb, $1, $2)`, after(0), after(10))
	exec(`insert into issues (key, project_key, number, title, status, rank, parent_key, created_by, created_at)
		values ('INFRA-3', 'INFRA', 3, 'A follow-up', 'triage', 'o', 'INFRA-1', '{"kind":"user","id":"alice"}'::jsonb, $1)`, after(6))

	session := func(title string) string {
		return fmt.Sprintf(`{"kind":"session","id":"s-%s","origin":{"session_title":%q}}`, title, title)
	}
	event := func(issue string, seq int, kind, actor, payload string, at time.Time) {
		t.Helper()
		exec(`insert into events (issue_key, seq, type, actor, payload, notify, created_at)
			values ($1, $2, $3, $4::jsonb, $5::jsonb, false, $6)`, issue, seq, kind, actor, payload, at)
	}
	// The root's architect opens the tree, and the child's implementer works it.
	event("INFRA-1", 1, "issue.created", `{"kind":"user","id":"alice"}`, `{"status":"triage"}`, after(-1))
	event("INFRA-1", 2, "comment.created", session("Legion architect · INFRA-1"), `{}`, after(0))
	event("INFRA-2", 1, "issue.created", session("Legion architect · INFRA-1"),
		`{"status":"triage","parent":"INFRA-1","ancestors":["INFRA-1"]}`, after(0))
	event("INFRA-2", 2, "comment.created", session("Legion implementer · INFRA-2"), `{}`, after(1))
	event("INFRA-2", 3, "issue.updated", `{"kind":"user","id":"alice"}`, `{"status":"backlog"}`, after(2))
	event("INFRA-2", 4, "issue.updated", `{"kind":"user","id":"alice"}`, `{"status":"in_progress"}`, after(4))
	event("INFRA-2", 5, "ask.opened", session("Legion implementer · INFRA-2"), `{"id":"ask-1"}`, after(5))
	event("INFRA-2", 6, "ask.answered", `{"kind":"user","id":"alice"}`, `{"id":"ask-1"}`, after(6))
	event("INFRA-2", 7, "issue.closed", session("Legion merger · INFRA-2"), `{"status":"done"}`, after(10))
	// The child's implementer files a follow-up in the tree, after its own implementing started.
	event("INFRA-3", 1, "issue.created", session("Legion implementer · INFRA-2"),
		`{"status":"triage","parent":"INFRA-1","ancestors":["INFRA-1"]}`, after(6))

	exec(`insert into handoffs (issue_key, phase, version, body, author, created_at)
		values ('INFRA-2', 'implement', 1, 'done', '{"kind":"session","id":"s-impl"}'::jsonb, $1)`, after(7))

	observe := func(field, value string, at time.Time) {
		t.Helper()
		exec(`insert into legion_observations (project, issue_key, field, value, observed_at)
			values ('INFRA', 'INFRA-2', $1, $2::jsonb, $3)`, field, value, at)
	}
	observe(legion.FieldPhase, `"implementing"`, after(1))
	observe(legion.ClaimFieldPrefix+"implementer", `"working"`, after(1))
	observe(legion.ClaimFieldPrefix+"implementer", `"idle"`, after(3))
	observe(legion.FieldPhase, `"done"`, after(10))

	exec(`insert into delivery_pull_requests (repo, number, title, url, author, issue_key, merged_at)
		values ('sjawhar/legion', 42, 'A change', 'https://github.com/sjawhar/legion/pull/42', 'bot', 'INFRA-2', $1)`, after(9))
	return database
}

// TestReadGathersEveryFactTheRulesRead: one read over a seeded event log carries the role events,
// the statuses, the ask turn, the handoff, the observations, the merge time and the filing, and
// the report computed from it is the one the rules describe.
func TestReadGathersEveryFactTheRulesRead(t *testing.T) {
	database := corpus(t)
	population, err := Read(t.Context(), database.Pool, nil, "INFRA", after(-2), after(24))
	if err != nil {
		t.Fatalf("read the population: %v", err)
	}
	child := population.Issues["INFRA-2"]
	if child == nil {
		t.Fatalf("the population holds %v, want the child among them", population.Keys)
	}
	if at, ok := child.FirstRoleEvent["implementer"]; !ok || !at.Equal(after(1)) {
		t.Errorf("the child's first implementer event = %v (%t), want %v", at, ok, after(1))
	}
	if _, architect := child.FirstRoleEvent["architect"]; architect {
		t.Error("an event of the root's architect session was counted as the child's, which no title says it is")
	}
	if len(child.Handoffs) != 1 || child.Handoffs[0].Phase != "implement" {
		t.Errorf("the child's handoffs = %+v, want its one implement handoff", child.Handoffs)
	}
	if len(child.AskTurns) != 1 || !child.AskTurns[0].From.Equal(after(5)) || !child.AskTurns[0].To.Equal(after(6)) {
		t.Errorf("the child's ask turns = %+v, want the hour from the question to the answer", child.AskTurns)
	}
	if child.MergedAt == nil || !child.MergedAt.Equal(after(9)) {
		t.Errorf("the child's merge time = %v, want %v", child.MergedAt, after(9))
	}
	if root := population.TreeRoot("INFRA-2"); root != "INFRA-1" {
		t.Errorf("the child's tree root = %q, want INFRA-1 from the parent chain", root)
	}
	if len(population.Filed) != 2 || population.Filed[0].Key != "INFRA-2" || population.Filed[1].Key != "INFRA-3" {
		t.Fatalf("the filings are %+v, want the child the architect filed and the follow-up the implementer filed", population.Filed)
	}

	report := Compute(population, ScopeAll, after(24))
	if report.Finished != 1 || report.WorkTime.Issues[0].Key != "INFRA-2" {
		t.Fatalf("the report counted %d issues (%+v), want the child alone", report.Finished, report.WorkTime.Issues)
	}
	// Nine hours from the first worker event to the close, less the two parked ones.
	if row := report.WorkTime.Issues[0]; row.Hours != 7 || row.ParkedHours != 2 {
		t.Errorf("the child's work time = %g hours with %g parked, want 7 and 2", row.Hours, row.ParkedHours)
	}
	// The turn the daemon recorded runs from hour 1 to hour 3, and the issue was parked from
	// hour 2, so one of its two hours is off the bar with the rest of the parked time: the bar's
	// seven hours are the work time's own.
	bar := barOf(t, report, "implementer")
	if bar.Segments.InATurn != 1 || bar.Segments.WaitingOnPerson != 1 || bar.Hours != 7 {
		t.Errorf("the implementer bar = %g hours, %+v, want seven hours with the unparked hour of the turn and the hour of the ask on the person",
			bar.Hours, bar.Segments)
	}
	classed := map[string]string{}
	for _, row := range report.Splitting.Filed {
		classed[row.Key] = row.Class
	}
	// The architect filed the child before its own first implementer event, and the child's
	// implementer filed the follow-up after it.
	if classed["INFRA-2"] != ClassInTreeAtDesign || classed["INFRA-3"] != ClassInTreeLater {
		t.Fatalf("the filings are classed %v, want one at design and one later", classed)
	}
}

// TestADaemonsTreeFieldNamesTheRootWhereAReadCarriedOne: the tree an issue belongs to is the
// daemon's own `tree` (dispatch://INFRA-980) where a read recorded one, and the parent chain only
// where none did. Nothing reads a slot to decide it.
func TestADaemonsTreeFieldNamesTheRootWhereAReadCarriedOne(t *testing.T) {
	database := corpus(t)
	ctx := t.Context()
	if _, err := database.Pool.Exec(ctx, `
		insert into legion_observations (project, issue_key, field, value, observed_at)
		values ('INFRA', 'INFRA-2', $1, '"INFRA-9"'::jsonb, $2)
	`, legion.FieldTree, after(1)); err != nil {
		t.Fatalf("record a tree observation: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into issues (key, project_key, number, title, status, rank, created_by, created_at)
		values ('INFRA-9', 'INFRA', 9, 'Another root', 'in_progress', 'p', '{"kind":"user","id":"alice"}'::jsonb, $1)
	`, after(-1)); err != nil {
		t.Fatalf("seed the other root: %v", err)
	}
	population, err := Read(ctx, database.Pool, nil, "INFRA", after(-2), after(24))
	if err != nil {
		t.Fatalf("read the population: %v", err)
	}
	if root := population.TreeRoot("INFRA-2"); root != "INFRA-9" {
		t.Fatalf("the child's tree root = %q, want the INFRA-9 the daemon named over its parent", root)
	}
	if children := population.Issues["INFRA-9"].Children; len(children) != 1 || children[0] != "INFRA-2" {
		t.Fatalf("INFRA-9's children = %v, want the issue whose tree it is", children)
	}
}

// TestADaemonsTreeFieldNamesTheRootOfAnIssueTheWindowDoesNotReach: the same rule for an issue no
// read of the window covers. The metrics read the event log, the handoffs and the observations
// of the issues the window could have finished and their trees (scopeKeys), and an open issue in
// none of those carries no observation of its own; the daemon's `tree` for it is a fact of the
// project all the same, and the Legion view reads it project-wide. Reading it for the window's
// issues alone put one issue in two trees: the board's and the metrics'.
func TestADaemonsTreeFieldNamesTheRootOfAnIssueTheWindowDoesNotReach(t *testing.T) {
	database := corpus(t)
	ctx := t.Context()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := database.Pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed (%s): %v", sql, err)
		}
	}
	// A tree of its own, beside the one the corpus holds: none of these closed in the window,
	// none filed anything in it and none is a filer's issue, so scopeKeys reaches no key of it.
	exec(`insert into issues (key, project_key, number, title, status, rank, created_by, created_at)
		values ('INFRA-6', 'INFRA', 6, 'An unrelated parent', 'in_progress', 'r', '{"kind":"user","id":"alice"}'::jsonb, $1)`, after(-1))
	exec(`insert into issues (key, project_key, number, title, status, rank, parent_key, created_by, created_at)
		values ('INFRA-7', 'INFRA', 7, 'An open issue', 'in_progress', 's', 'INFRA-6', '{"kind":"user","id":"alice"}'::jsonb, $1)`, after(0))
	exec(`insert into issues (key, project_key, number, title, status, rank, created_by, created_at)
		values ('INFRA-8', 'INFRA', 8, 'The tree the daemon names', 'in_progress', 't', '{"kind":"user","id":"alice"}'::jsonb, $1)`, after(-1))
	// The daemon reports INFRA-7 in a tree its parent chain does not reach.
	exec(`insert into legion_observations (project, issue_key, field, value, observed_at)
		values ('INFRA', 'INFRA-7', $1, '"implementing"'::jsonb, $2)`, legion.FieldPhase, after(1))
	exec(`insert into legion_observations (project, issue_key, field, value, observed_at)
		values ('INFRA', 'INFRA-7', $1, '"INFRA-8"'::jsonb, $2)`, legion.FieldTree, after(2))

	population, err := Read(ctx, database.Pool, nil, "INFRA", after(-2), after(24))
	if err != nil {
		t.Fatalf("read the population: %v", err)
	}
	if marks := population.Issues["INFRA-7"].Observed; len(marks) != 0 {
		t.Fatalf("the window's observation read carried %v of INFRA-7, so the issue is not the one this test is about", marks)
	}
	if root := population.TreeRoot("INFRA-7"); root != "INFRA-8" {
		t.Fatalf("the open issue's tree root = %q, want the INFRA-8 the daemon named for it", root)
	}
	if children := population.Issues["INFRA-8"].Children; len(children) != 1 || children[0] != "INFRA-7" {
		t.Fatalf("INFRA-8's children = %v, want the issue whose tree it is", children)
	}
}

// TestOnlyAnAskOpenedByASessionOfTheIssuesOwnTreeMakesItWaitOnAPerson: "An ask counts for an
// issue when it is opened on that issue by any Legion session of the issue's tree, that is, a
// session titled `Legion <role> · <KEY>` where KEY is any issue of the same tree, walked as for
// splitting and follow-ups. An ask opened by a session outside the tree does not count" (the
// spec's "Rule details — Whose ask counts as waiting on a person"). Two asks more stand on a
// person's turn over the child's last hours: one a session of a grandchild of its own root
// opened, which is an hour of waiting on a person, and one a session of another root's tree
// opened, which is on no segment at all.
func TestOnlyAnAskOpenedByASessionOfTheIssuesOwnTreeMakesItWaitOnAPerson(t *testing.T) {
	database := corpus(t)
	ctx := t.Context()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := database.Pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed (%s): %v", sql, err)
		}
	}
	// A grandchild of the root, under the child the asks are opened on: its own tree root is
	// the child's, two parents up.
	exec(`insert into issues (key, project_key, number, title, status, rank, parent_key, created_by, created_at)
		values ('INFRA-5', 'INFRA', 5, 'A grandchild', 'in_progress', 'p', 'INFRA-2', '{"kind":"user","id":"alice"}'::jsonb, $1)`, after(1))
	// Another root, with Legion work of its own, so its sessions are a tree of their own.
	exec(`insert into issues (key, project_key, number, title, status, rank, created_by, created_at)
		values ('INFRA-9', 'INFRA', 9, 'Another root', 'in_progress', 'q', '{"kind":"user","id":"alice"}'::jsonb, $1)`, after(0))
	session := func(title string) string {
		return fmt.Sprintf(`{"kind":"session","id":"s-%s","origin":{"session_title":%q}}`, title, title)
	}
	event := func(issue string, seq int, kind, actor, payload string, at time.Time) {
		t.Helper()
		exec(`insert into events (issue_key, seq, type, actor, payload, notify, created_at)
			values ($1, $2, $3, $4::jsonb, $5::jsonb, false, $6)`, issue, seq, kind, actor, payload, at)
	}
	event("INFRA-9", 1, "comment.created", session("Legion architect · INFRA-9"), `{}`, after(0))
	// The grandchild's implementer asks on the child: a session of the child's own tree.
	event("INFRA-2", 8, "ask.opened", session("Legion implementer · INFRA-5"), `{"id":"ask-2"}`, after(7))
	event("INFRA-2", 9, "ask.answered", `{"kind":"user","id":"alice"}`, `{"id":"ask-2"}`, after(8))
	// The other root's implementer asks on the child: a Legion session outside its tree.
	event("INFRA-2", 10, "ask.opened", session("Legion implementer · INFRA-9"), `{"id":"ask-3"}`, after(8))
	event("INFRA-2", 11, "ask.answered", `{"kind":"user","id":"alice"}`, `{"id":"ask-3"}`, after(9))

	population, err := Read(ctx, database.Pool, nil, "INFRA", after(-2), after(24))
	if err != nil {
		t.Fatalf("read the population: %v", err)
	}
	if root := population.TreeRoot("INFRA-5"); root != "INFRA-1" {
		t.Fatalf("the grandchild's tree root = %q, want INFRA-1 two parents up", root)
	}
	if root := population.TreeRoot("INFRA-9"); root != "INFRA-9" {
		t.Fatalf("the other root's tree root = %q, want itself", root)
	}
	child := population.Issues["INFRA-2"]
	if len(child.AskTurns) != 2 || !child.AskTurns[1].From.Equal(after(7)) || !child.AskTurns[1].To.Equal(after(8)) {
		t.Fatalf("the child's ask turns = %+v, want its own implementer's hour and the grandchild's, and not the other tree's",
			child.AskTurns)
	}
	// The child's seven bar hours: the unparked hour of its turn, the two hours of the asks its
	// own tree opened, and four idle. The hour of the ask the other tree opened is idle too.
	bar := barOf(t, Compute(population, ScopeAll, after(24)), "implementer")
	if bar.Hours != 7 || bar.Segments.WaitingOnPerson != 2 || bar.Segments.InATurn != 1 || bar.Segments.Idle != 4 {
		t.Fatalf("the implementer bar = %g hours, %+v, want seven hours with two waiting on a person", bar.Hours, bar.Segments)
	}
}
