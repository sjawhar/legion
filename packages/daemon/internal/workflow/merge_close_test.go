package workflow

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The merge of a child's pull request while it awaits the merge closes the child and opens its
// production check in the implementer's live session: the child is done on the record and on
// Dispatch, its architect is told child-closed, five of its six claims are closed, and the
// implementer's is handed the check's first look — a start with no phase, fenced on the check's
// look 1, whose task opens with the line a resumed session tells the check by — with no
// IssueSuspend at the close, since the check's close writes the release. The check records the
// repository, the pull request, the merge commit and its one open look; its look is bounded two
// hours out and the whole check at the project's bound.
func TestAChildsMergeClosesItAndHandsItsImplementerTheFirstLook(t *testing.T) {
	pool := migratedPool(t)
	now := time.Date(2026, 10, 19, 4, 0, 0, 0, time.UTC)
	seedMergingChild(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 7, Status: "in_progress", Rank: "U", LastDispatchSeq: 1})
	engine := New(record.NewStore(), Config{Project: "LEGION", Linger: time.Hour, ProductionCheckBound: 48 * time.Hour, Clock: func() time.Time { return now }}, nil)

	if result, err := intake.ApplyFact(context.Background(), pool, "github", "merged", intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 43, MergeSHA: "merge-43"}, engine, admissionStub{}); err != nil || result.Refusal != nil {
		t.Fatalf("ApplyFact merge = %+v, %v", result.Refusal, err)
	}

	var childPhase phase.Phase
	var childStatus string
	if err := pool.QueryRow(context.Background(), "select phase, status from issues where key = 'LEGION-209'").Scan(&childPhase, &childStatus); err != nil {
		t.Fatalf("read child: %v", err)
	}
	if childPhase != phase.Done || childStatus != "done" {
		t.Fatalf("the merged child = %s/%s, want done/done", childPhase, childStatus)
	}
	var statusWrites int
	if err := pool.QueryRow(context.Background(), `select count(*) from outbox where kind = 'dispatch_status' and issue = 'LEGION-209'
		and payload->>'status' = 'done' and payload->>'observedStatus' = 'retro'`).Scan(&statusWrites); err != nil || statusWrites != 1 {
		t.Fatalf("done status writes = %d, %v; want one over retro", statusWrites, err)
	}
	if got := noticeKinds(t, pool, "LEGION-209"); !containsNotice(got, "child-closed") {
		t.Fatalf("child notices = %v, want child-closed", got)
	}
	closed := superviseRequests(t, pool, "issue_close")
	for _, role := range claim.Roles {
		want := 1
		if role == claim.RoleImplementer {
			want = 0
		}
		if closed["LEGION-209/"+string(role)] != want {
			t.Fatalf("issue_close rows = %v, want one for every role but the implementer", closed)
		}
	}
	starts := superviseRowsOf(t, pool, "LEGION-209", "start")
	if len(starts) != 1 {
		t.Fatalf("start rows = %+v, want the implementer's check look alone", starts)
	}
	start := starts[0]
	if start.Role != claim.RoleImplementer || start.CheckLook != 1 || start.Phase != "" || start.Generation != 7 || start.Tree != "LEGION-208" {
		t.Fatalf("the check's start row = %+v, want the implementer's, check look 1, no phase, generation 7", start)
	}
	if lead, _, _ := strings.Cut(start.Task, "\n"); lead != "Production check of LEGION-209: child." {
		t.Fatalf("the check task opens %q, want the line a resumed session tells the check by", lead)
	}
	for _, want := range []string{"sjawhar/legion", "#43", "merge-43", "Look 1, woken by the merge.", "handoff_complete", "pass", "pending", "fail", "the daemon wakes the check",
		"fail for a defect, a dispatch ask and never an issue you file", "on the tree's root while the root is before accepting, else on the controller's report issue"} {
		if !strings.Contains(start.Task, want) {
			t.Fatalf("the check task lacks %q:\n%s", want, start.Task)
		}
	}
	// A defect the check finds is an ask a person answers, never an issue the implementer files.
	for _, never := range []string{"dispatch issue", "issue of its own"} {
		if strings.Contains(start.Task, never) {
			t.Fatalf("the check task still says %q:\n%s", never, start.Task)
		}
	}
	if strings.Contains(start.Task, "last look") {
		t.Fatalf("the first look's task says it is the last:\n%s", start.Task)
	}
	if got := issueSuspensions(t, pool, "LEGION-209"); len(got) != 0 {
		t.Fatalf("issue suspensions at the close = %+v, want none until the check closes", got)
	}
	check := productionCheck(t, pool, "LEGION-209")
	if check == nil || !check.Open() || check.Repo != "sjawhar/legion" || check.Number != 43 || check.MergeSHA != "merge-43" || check.Tree != "LEGION-208" || check.Project != "LEGION" ||
		check.Looks != 1 || !check.LookOpen || !check.OpenedAt.Equal(now) || !check.BoundAt.Equal(now.Add(48*time.Hour)) || check.LastWakeKind != "merge" || check.Last {
		t.Fatalf("the production check = %+v, want it open on sjawhar/legion#43 at merge-43, look 1 open, woken by the merge, bound 48h out", check)
	}
	if look := issueOutboxOf(t, pool, "LEGION-209", "check_look"); len(look) != 1 || !look[0].nextAt.Equal(now.Add(CheckLookBound)) || decodeLook(t, look[0]) != 1 {
		t.Fatalf("check_look rows = %+v, want one for look 1 due two hours out", look)
	}
	if bound := issueOutboxOf(t, pool, "LEGION-209", "check_bound"); len(bound) != 1 || !bound[0].nextAt.Equal(now.Add(48*time.Hour)) {
		t.Fatalf("check_bound rows = %+v, want one due at the bound", bound)
	}
	if got := issueSuspensions(t, pool, "LEGION-208"); len(got) != 0 || superviseRequests(t, pool, "suspend")["LEGION-208/implementer"] != 0 {
		t.Fatalf("the child's close touched its root: suspensions %+v, suspends %v", got, superviseRequests(t, pool, "suspend"))
	}
}

// The merge of a root's pull request while it awaits the merge closes it and lingers its tree, as
// before: every member's claims are suspended and each member's Sandbox kept. The root's production
// check opens behind the linger's rows — its start row is newer than every suspend and every
// IssueSuspend the linger wrote — so the root's own suspension finishes without acting on the
// check's start, and the start resumes the implementer in the still-live pod.
func TestARootsMergeLingersItsTreeAndOpensItsCheckBehindTheLingersRows(t *testing.T) {
	pool := migratedPool(t)
	now := time.Date(2026, 10, 19, 4, 0, 0, 0, time.UTC)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.AwaitingMerge, Generation: 7, Status: "retro", Rank: "U"})
	parent := "LEGION-208"
	seedIssue(t, pool, record.Issue{Key: "LEGION-209", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "child", Parent: &parent, Phase: phase.Implementing, Generation: 3, Status: "in_progress", Rank: "V"})
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head", Failing: []string{}})
	engine := New(record.NewStore(), Config{Project: "LEGION", Linger: time.Hour, Clock: func() time.Time { return now }}, nil)

	if _, err := intake.ApplyFact(context.Background(), pool, "github", "merged", intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 42, MergeSHA: "merge"}, engine, admissionStub{}); err != nil {
		t.Fatalf("ApplyFact merge: %v", err)
	}
	assertPhase(t, pool, phase.Done)
	everyClaim := map[string]int{}
	for _, issue := range []string{"LEGION-208", "LEGION-209"} {
		for _, role := range claim.Roles {
			everyClaim[issue+"/"+string(role)]++
		}
	}
	if suspended := superviseRequests(t, pool, "suspend"); !sameCounts(suspended, everyClaim) {
		t.Fatalf("the linger suspended %v, want each tree claim once: %v", suspended, everyClaim)
	}
	if got := superviseRequests(t, pool, "issue_close"); len(got) != 0 {
		t.Fatalf("issue_close rows at a root's close = %v, want none: the tree's close retires its claims", got)
	}
	var start outboxRow
	for _, row := range issueOutboxOf(t, pool, "LEGION-208", "supervise") {
		if request := decodeSupervise(t, row); request.Op == "start" && request.CheckLook == 1 {
			start = row
		}
	}
	if start.id == 0 {
		t.Fatalf("the root's supervise rows hold no check look 1 start")
	}
	for _, kind := range []string{"issue_suspend", "linger_close"} {
		for _, issue := range []string{"LEGION-208", "LEGION-209"} {
			for _, row := range issueOutboxOf(t, pool, issue, kind) {
				if row.id > start.id {
					t.Fatalf("%s row %d of %s is newer than the check's start row %d; the start must follow the linger's rows", kind, row.id, issue, start.id)
				}
			}
		}
	}
	for _, row := range issueOutboxOf(t, pool, "LEGION-208", "supervise") {
		if row.id > start.id {
			t.Fatalf("supervise row %d of the root is newer than the check's start row %d: %s", row.id, start.id, row.payload)
		}
	}
	if got := issueSuspensions(t, pool, "LEGION-208"); len(got) != 1 || got[0].Release {
		t.Fatalf("the root's issue suspensions = %+v, want the linger's one keeping row", got)
	}
	if check := productionCheck(t, pool, "LEGION-208"); check == nil || !check.Open() || check.Looks != 1 || !check.LookOpen || check.MergeSHA != "merge" {
		t.Fatalf("the root's production check = %+v, want it open at look 1 on merge", check)
	}
}

// A merge that lands while the tree lingers still closes the member: its phase and status are done,
// its architect is told child-closed, and its production check opens with look 1 as a wake start
// row, since the linger already suspended its implementer. It retires nobody and suspends nothing:
// no issue_close and no IssueSuspend, which the linger's own rows already wrote and the tree's
// close completes.
func TestALingeringMembersMergeClosesItAndWakesItsCheck(t *testing.T) {
	pool := migratedPool(t)
	now := time.Date(2026, 10, 19, 4, 0, 0, 0, time.UTC)
	until := now.Add(time.Hour)
	seedMergingChild(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Done, Generation: 7, Status: "done", Rank: "U", LingerUntil: &until, LastDispatchSeq: 1})
	engine := New(record.NewStore(), Config{Project: "LEGION", Linger: time.Hour, Clock: func() time.Time { return now }}, nil)

	if _, err := intake.ApplyFact(context.Background(), pool, "github", "merged", intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 43, MergeSHA: "merge-43"}, engine, admissionStub{}); err != nil {
		t.Fatalf("ApplyFact merge: %v", err)
	}
	var childPhase phase.Phase
	var childStatus string
	if err := pool.QueryRow(context.Background(), "select phase, status from issues where key = 'LEGION-209'").Scan(&childPhase, &childStatus); err != nil {
		t.Fatalf("read child: %v", err)
	}
	if childPhase != phase.Done || childStatus != "done" {
		t.Fatalf("the lingering member = %s/%s, want done/done", childPhase, childStatus)
	}
	var statusWrites int
	if err := pool.QueryRow(context.Background(), `select count(*) from outbox where kind = 'dispatch_status' and issue = 'LEGION-209' and payload->>'status' = 'done'`).Scan(&statusWrites); err != nil || statusWrites != 1 {
		t.Fatalf("done status writes = %d, %v; want one", statusWrites, err)
	}
	if got := noticeKinds(t, pool, "LEGION-209"); !containsNotice(got, "child-closed") {
		t.Fatalf("child notices = %v, want child-closed", got)
	}
	starts := superviseRowsOf(t, pool, "LEGION-209", "start")
	if len(starts) != 1 || starts[0].Role != claim.RoleImplementer || starts[0].CheckLook != 1 || starts[0].Phase != "" {
		t.Fatalf("start rows = %+v, want the check's look 1 as the implementer's wake", starts)
	}
	for _, op := range []string{"issue_close", "suspend"} {
		if got := superviseRequests(t, pool, op); len(got) != 0 {
			t.Fatalf("%s rows = %v, want none: the linger's rows already stopped the member", op, got)
		}
	}
	if got := issueSuspensions(t, pool, "LEGION-209"); len(got) != 0 {
		t.Fatalf("issue suspensions = %+v, want none", got)
	}
	if check := productionCheck(t, pool, "LEGION-209"); check == nil || !check.Open() || check.Looks != 1 || !check.LookOpen || check.MergeSHA != "merge-43" {
		t.Fatalf("the production check = %+v, want it open at look 1 on merge-43", check)
	}
}

// A child closed as done with no pull request — a human's done on an issue short of its merge —
// has nothing to check in production: today's close, every claim closed and the Sandbox released
// with its volume, and no check, no look and no bound.
func TestAChildClosedAsDoneWithNoPullRequestOpensNoCheck(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 7, Status: "in_progress", Rank: "U", LastDispatchSeq: 1})
	parent := "LEGION-208"
	seedIssue(t, pool, record.Issue{Key: "LEGION-209", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "child", Parent: &parent, Phase: phase.Implementing, Generation: 7, Status: "in_progress", Rank: "V", LastDispatchSeq: 1})
	engine := New(record.NewStore(), Config{Project: "LEGION", Linger: time.Hour, Clock: func() time.Time { return time.Date(2026, 10, 19, 4, 0, 0, 0, time.UTC) }}, nil)

	if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", "child-done", intake.DispatchIssue{Key: "LEGION-209", Seq: 2, Type: "issue.closed", Status: "done", Title: "child", Parent: "LEGION-208", Rank: "V"}, engine, admissionStub{}); err != nil {
		t.Fatalf("ApplyFact done: %v", err)
	}
	closed := superviseRequests(t, pool, "issue_close")
	for _, role := range claim.Roles {
		if closed["LEGION-209/"+string(role)] != 1 {
			t.Fatalf("issue_close rows = %v, want one for every role", closed)
		}
	}
	if got := issueSuspensions(t, pool, "LEGION-209"); len(got) != 1 || !got[0].Release {
		t.Fatalf("issue suspensions = %+v, want one releasing row", got)
	}
	if check := productionCheck(t, pool, "LEGION-209"); check != nil {
		t.Fatalf("production check = %+v, want none for a child with no pull request", check)
	}
	for _, kind := range []string{"check_look", "check_bound"} {
		assertOutboxCount(t, pool, kind, 0)
	}
	if got := superviseRequests(t, pool, "start"); len(got) != 0 {
		t.Fatalf("start rows = %v, want none", got)
	}
}

// seedMergingChild seeds root, its child LEGION-209 awaiting its merge at generation 7 with status
// retro, the child's merger row and its open pull request sjawhar/legion#43.
func seedMergingChild(t *testing.T, pool *pgxpool.Pool, root record.Issue) {
	t.Helper()
	seedIssue(t, pool, root)
	parent := root.Key
	seedIssue(t, pool, record.Issue{Key: "LEGION-209", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: root.Key, Project: "LEGION", Title: "child", Parent: &parent, Phase: phase.AwaitingMerge, Generation: 7, Status: "retro", Rank: "V", LastDispatchSeq: 1})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-209", Role: claim.RoleMerger, Claim: "child-merger", HandoffCommit: "head-43"})
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-209", Repo: "sjawhar/legion", Number: 43, Branch: "legion/LEGION-209", HeadSHA: "head-43", Failing: []string{}})
}

// productionCheck reads issue's production check, nil for none.
func productionCheck(t *testing.T, pool *pgxpool.Pool, issue string) *record.ProductionCheck {
	t.Helper()
	var check *record.ProductionCheck
	seedRecord(t, pool, func(tx pgx.Tx) error {
		var err error
		check, err = record.NewStore().ProductionCheck(t.Context(), tx, issue)
		return err
	})
	return check
}

// outboxRow is one outbox row of an issue as the tests read it: its id, when it is due and its
// payload.
type outboxRow struct {
	id      int64
	nextAt  time.Time
	payload []byte
}

// issueOutboxOf is issue's outbox rows of kind, oldest first (outboxOf, repository_test.go, is
// every issue's of a kind).
func issueOutboxOf(t *testing.T, pool *pgxpool.Pool, issue, kind string) []outboxRow {
	t.Helper()
	rows, err := pool.Query(t.Context(), "select id, next_at, payload from outbox where issue = $1 and kind = $2 order by id", issue, kind)
	if err != nil {
		t.Fatalf("read %s rows of %s: %v", kind, issue, err)
	}
	defer rows.Close()
	var got []outboxRow
	for rows.Next() {
		var row outboxRow
		if err := rows.Scan(&row.id, &row.nextAt, &row.payload); err != nil {
			t.Fatalf("scan %s row: %v", kind, err)
		}
		got = append(got, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate %s rows: %v", kind, err)
	}
	return got
}

// superviseRowsOf decodes issue's supervise rows of op, oldest first.
func superviseRowsOf(t *testing.T, pool *pgxpool.Pool, issue, op string) []record.SuperviseRequest {
	t.Helper()
	var requests []record.SuperviseRequest
	for _, row := range issueOutboxOf(t, pool, issue, "supervise") {
		if request := decodeSupervise(t, row); string(request.Op) == op {
			requests = append(requests, request)
		}
	}
	return requests
}

// decodeSupervise is the supervise request a supervise row carries.
func decodeSupervise(t *testing.T, row outboxRow) record.SuperviseRequest {
	t.Helper()
	var request record.SuperviseRequest
	if err := json.Unmarshal(row.payload, &request); err != nil {
		t.Fatalf("decode supervise row %d: %v", row.id, err)
	}
	return request
}

// decodeLook is the look a check_look row names.
func decodeLook(t *testing.T, row outboxRow) int {
	t.Helper()
	var look record.CheckLook
	if err := json.Unmarshal(row.payload, &look); err != nil {
		t.Fatalf("decode check_look row %d: %v", row.id, err)
	}
	return look.Look
}
