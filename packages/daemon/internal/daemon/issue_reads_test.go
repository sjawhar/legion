package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A webhook GitHub never delivers leaves the record at a head GitHub has moved past, and every
// decision after it - a review, READY, a CI verdict - judged against that head. The per-issue read
// is the backstop: it reads the issue's own pull request and applies the synchronize the webhook
// would have carried, with GitHub's updated_at as its clock, so the record stands at the head
// GitHub shows.
func TestThePerIssueReadFollowsAStaleRecordedHead(t *testing.T) {
	read := time.Date(2026, 10, 26, 20, 0, 0, 0, time.UTC)
	pool := isolatedOutboxPool(t)
	seedCapture(t, pool, phase.Testing, "testing", record.PhaseRow{Issue: "CAPTURE-1", Role: claim.RoleTester, Claim: "test-claim"})
	w := readRuntime(pool, pullRequestStandIn(t, "pushed", read, "src/widget.go", &atomic.Int32{}).URL, quietLogger())
	if err := w.readIssuePullRequest(context.Background(), "CAPTURE-1"); err != nil {
		t.Fatalf("read the issue's pull request: %v", err)
	}
	_, pr := capturePhase(t, pool)
	if pr.HeadSHA != "pushed" || !pr.HeadUpdatedAt.Equal(read) {
		t.Fatalf("after the read the record is at %q (clock %v), want pushed (clock %v)", pr.HeadSHA, pr.HeadUpdatedAt, read)
	}
	if got := headReads(t, pool, "head-read"); got != 1 {
		t.Fatalf("the read applied %d head facts, want 1", got)
	}
	// A second read of the same pull request finds the head it already applied and applies nothing
	// further: the read is safe to run twice.
	if err := w.readIssuePullRequest(context.Background(), "CAPTURE-1"); err != nil {
		t.Fatalf("read the issue's pull request again: %v", err)
	}
	if got := headReads(t, pool, "head-read"); got != 1 {
		t.Fatalf("two reads applied %d head facts, want 1: the second read changed nothing", got)
	}
}

// An issue's pull request is recorded only by its opened webhook, so one lost leaves the issue in
// implementing with its pull request open on GitHub and its implementer waiting. The per-issue read
// of an issue whose record holds no pull request looks up the open one on the issue's branch and
// applies the opened that webhook would have carried, which records it and moves the issue on; it
// then reads that pull request, as it reads a recorded one.
func TestThePerIssueReadFindsThePullRequestOfTheIssuesBranch(t *testing.T) {
	opened := time.Date(2026, 10, 26, 19, 30, 0, 0, time.UTC)
	pool := isolatedOutboxPool(t)
	seedUnrecorded(t, pool, "handoff")
	var listed atomic.Int32
	w := readRuntime(pool, pullRequestStandIn(t, "handoff", opened, "", &listed).URL, quietLogger())
	if err := w.readIssuePullRequest(context.Background(), "CAPTURE-1"); err != nil {
		t.Fatalf("read the issue's pull request: %v", err)
	}
	got, pr := capturePhase(t, pool)
	if got != phase.Testing {
		t.Fatalf("after the read the issue is in %s, want testing: the opened the read applied moves it on", got)
	}
	if pr == nil || pr.State != record.PullRequestOpen || pr.Number != 86 || pr.Branch != "legion/CAPTURE-1" || pr.HeadSHA != "handoff" || !pr.HeadUpdatedAt.Equal(opened) {
		t.Fatalf("after the read the record is %+v, want #86 open on legion/CAPTURE-1 at handoff, clock %v", pr, opened)
	}
	if listed.Load() != 1 || headReads(t, pool, "opened-read") != 1 {
		t.Fatalf("the read listed the branch %d times and applied %d opened facts, want 1 and 1", listed.Load(), headReads(t, pool, "opened-read"))
	}
}

// An implementer that completed with no pull request open on its branch leaves its issue waiting in
// implementing with nobody told. The per-issue read, the one its completion queues, finds no open
// pull request there and tells the issue's architect - once per handoff commit, however often the
// read runs.
func TestThePerIssueReadTellsTheArchitectOfAMissingPullRequest(t *testing.T) {
	pool := isolatedOutboxPool(t)
	seedUnrecorded(t, pool, "handoff")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/repos/acme/widgets/pulls") {
			_, _ = w.Write([]byte("[]"))
			return
		}
		http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
	}))
	t.Cleanup(server.Close)
	w := readRuntime(pool, server.URL, quietLogger())
	for range 2 {
		if err := w.readIssuePullRequest(context.Background(), "CAPTURE-1"); err != nil {
			t.Fatalf("read the issue's pull request: %v", err)
		}
	}
	reasons := noticeReasons(t, pool, "pull-request-missing")
	if len(reasons) != 1 || !strings.Contains(reasons[0], "legion/CAPTURE-1") || !strings.Contains(reasons[0], "handoff") {
		t.Fatalf("two reads told pull-request-missing %q, want one naming the branch and the handoff commit", reasons)
	}
}

// The daemon reads an issue's pull request when one of its agents hands off: the completion is the
// moment it decides the issue's next step, and the state it decides by must be GitHub's own. The
// read is queued by the completion and made on the runtime's own goroutine, for that issue alone -
// another issue's pull request is not read, and its record stands where its own webhooks left it.
func TestAHandoffCompletionReadsThatIssuesPullRequestAlone(t *testing.T) {
	read := time.Date(2026, 10, 26, 20, 0, 0, 0, time.UTC)
	pool := isolatedOutboxPool(t)
	seedCapture(t, pool, phase.Testing, "testing", record.PhaseRow{Issue: "CAPTURE-1", Role: claim.RoleTester, Claim: "test-claim"})
	seedSecondIssue(t, pool)
	github := newIssueReadStandIn(t, read)
	w := readRuntime(pool, github.URL, quietLogger())
	w.handedOff = newWakeSet[string]()
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		w.readHandedOff(ctx)
		close(stopped)
	}()
	defer func() {
		cancel()
		<-stopped
	}()

	w.handoffCompleted("CAPTURE-1")
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, pr := capturePhase(t, pool); pr.HeadSHA == "pushed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the completion's read did not follow CAPTURE-1's head; GitHub was read %v", github.counts())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := github.count("/repos/acme/widgets/pulls/86"); got != 1 {
		t.Fatalf("the completion read pull request 86 %d times, want 1", got)
	}
	if got := github.count("/repos/acme/widgets/pulls/87"); got != 0 {
		t.Fatalf("the completion read pull request 87 %d times, want none: the handoff was CAPTURE-1's", got)
	}
	var other *record.PullRequest
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		var err error
		other, err = record.NewStore().PullRequest(context.Background(), tx, "CAPTURE-2")
		return err
	}); err != nil {
		t.Fatalf("read CAPTURE-2's pull request: %v", err)
	}
	if other == nil || other.HeadSHA != "code" {
		t.Fatalf("CAPTURE-2's record is %+v, want it still at code", other)
	}
}

// seedSecondIssue records CAPTURE-2 in testing with its own pull request 87 open on its own branch
// at head code, so a read of another issue that reaches for it is seen.
func seedSecondIssue(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	records := record.NewStore()
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		ctx := context.Background()
		if err := records.PutIssue(ctx, tx, record.Issue{Key: "CAPTURE-2", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "CAPTURE-2", Project: "CAPTURE", Title: "another root",
			Phase: phase.Testing, Generation: 1, Status: "testing", Rank: "V"}); err != nil {
			return err
		}
		if err := records.PutPullRequest(ctx, tx, record.PullRequest{State: record.PullRequestOpen, Issue: "CAPTURE-2", Repo: "acme/widgets",
			Number: 87, Branch: "legion/CAPTURE-2", HeadSHA: "code", CheckedHead: "code", Failing: []string{},
			Generation: 1, Snapshot: "settled"}); err != nil {
			return err
		}
		return records.PutPhase(ctx, tx, record.PhaseRow{Issue: "CAPTURE-2", Role: claim.RoleImplementer, Claim: "implement-claim-2"})
	}); err != nil {
		t.Fatalf("seed the second issue: %v", err)
	}
}

// issueReadStandIn is GitHub's REST API for acme/widgets with a pull request for each of two
// issues - 86 on legion/CAPTURE-1 and 87 on legion/CAPTURE-2, both open at head pushed, where both
// records stand at code - counting every path it answers, so a test can see which pull request a
// read asked for and which it left alone.
type issueReadStandIn struct {
	*httptest.Server
	mu   sync.Mutex
	read map[string]int
}

func newIssueReadStandIn(t *testing.T, updated time.Time) *issueReadStandIn {
	t.Helper()
	stand := &issueReadStandIn{read: map[string]int{}}
	pull := func(number int, issue string) string {
		return `{"number":` + strconv.Itoa(number) + `,"state":"open","html_url":"https://github.com/acme/widgets/pull/` + strconv.Itoa(number) + `",` +
			`"body":"Dispatch: ` + issue + `","base":{"ref":"main"},"head":{"ref":"legion/` + issue + `","sha":"pushed"},` +
			`"updated_at":"` + updated.Format(time.RFC3339) + `","mergeable":true}`
	}
	stand.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stand.mu.Lock()
		stand.read[r.URL.Path]++
		stand.mu.Unlock()
		if strings.HasPrefix(r.URL.Path, "/repos/acme/widgets/compare/") {
			w.Write([]byte(`{"status":"ahead","files":[{"filename":"src/widget.go"}]}`))
			return
		}
		switch r.URL.Path {
		case "/repos/acme/widgets/pulls/86":
			w.Write([]byte(pull(86, "CAPTURE-1")))
		case "/repos/acme/widgets/pulls/87":
			w.Write([]byte(pull(87, "CAPTURE-2")))
		case "/repos/acme/widgets/pulls/86/reviews", "/repos/acme/widgets/pulls/87/reviews":
			w.Write([]byte(`[]`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(stand.Server.Close)
	return stand
}

// count is how often the stand-in answered path, and counts every path it answered.
func (s *issueReadStandIn) count(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.read[path]
}

func (s *issueReadStandIn) counts() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	counted := map[string]int{}
	for path, n := range s.read {
		counted[path] = n
	}
	return counted
}
