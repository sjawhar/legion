package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// A child Dispatch already shows todo when its architect releases it — set todo before its tree was
// live, by a person or another session, so no todo event of it ever reached the live tree — is
// recorded under the tree and started by the release, as a child the release moves into todo is by
// that move's event. Dispatch makes no event for a status an issue already has, so before, the
// release answered the child released and nothing recorded or started it: read_record found no
// record, and park_child and rerun_child answered ISSUE_NOT_FOUND (LEGION-627). The architect's
// retry of the same wave changes nothing more.
func TestWaveReleaseRecordsAndStartsAChildDispatchAlreadyShowsTodo(t *testing.T) {
	h := newHarness(t)
	records := record.NewStore()
	engine := workflow.New(records, workflow.Config{Project: testProject, Repositories: testRepositories, DesignGate: config.DesignGateRootIssues}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	statuses := &statusRecorder{}
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, OperatorToken: testOperatorToken,
		Controller: h.store, Grants: credential.New(nil), Pool: h.store.Pool(), Record: records,
		Handlers: []intake.Handler{engine}, Dispatch: statuses,
	}).Handler
	root, child := "LEGION-208", "LEGION-209"
	approved := 1
	h.recordIssue(record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Project: testProject, Title: root, Phase: phase.Planning, Generation: 1, Status: "in_progress", HandedOver: true})
	if err := h.store.Tx(context.Background(), func(tx pgx.Tx) error {
		if err := records.PutSlot(context.Background(), tx, record.Slot{Issue: root, Index: 0, AdmittedAt: time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)}); err != nil {
			return err
		}
		return records.PutGate(context.Background(), tx, record.DesignGate{Issue: root, ArtifactID: "7d1e3a5c-9b2f-4c6e-8a40-3f5d7b9e1c26", LatestVersion: 1, ApprovedVersion: &approved})
	}); err != nil {
		t.Fatalf("seed the live tree: %v", err)
	}
	statuses.issues = map[string]dispatch.Issue{
		root:  {Key: root, ResolvedRepo: "acme/widgets", Status: "in_progress"},
		child: {Key: child, ResolvedRepo: "acme/widgets", Title: "the child", Status: "todo", Parent: &root, LastSeq: 7},
	}
	architect := newLiveClaim(t, h, root, claim.RoleArchitect)
	release := func() {
		t.Helper()
		recorder := h.request(http.MethodPost, "/legion/v1/waves/release", map[string]any{"grantId": architect.grant(t), "issues": []string{child}}, nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("wave release = %d: %s", recorder.Code, recorder.Body)
		}
	}

	release()
	recorded := recordedChild(t, h, child)
	if recorded == nil || recorded.Tree != root || recorded.Phase != phase.Planning || recorded.Generation != 1 || recorded.LastDispatchSeq != 7 || recorded.Title != "the child" {
		t.Fatalf("the released todo child's record = %#v, want it recorded in %s's tree at Dispatch's sequence 7 and planning under the open gate", recorded, root)
	}
	if len(statuses.writes) != 0 {
		t.Fatalf("Dispatch writes = %#v, want none: the child is already todo", statuses.writes)
	}

	release()
	if again := recordedChild(t, h, child); again == nil || again.Generation != 1 || again.Phase != phase.Planning {
		t.Fatalf("the child after the architect's retry = %#v, want its one run unchanged", again)
	}
	if recorder := h.request(http.MethodPost, "/legion/v1/children/park", map[string]any{"grantId": architect.grant(t), "issue": child}, nil); recorder.Code != http.StatusOK {
		t.Fatalf("park_child of the released child = %d: %s", recorder.Code, recorder.Body)
	}
}

// A todo child whose repository cannot be read before it is recorded (the daemon's preparation
// reads Dispatch's issue detail, the repository's Apps and its default branch from GitHub) is not
// released: the answer names the read that failed, whatever answered it, and records nothing, so
// the architect's retry of release_children records it. A child held back for a read of the
// repository mapping (intake.RetryLater) is answered the release's backoff, naming the wait.
func TestWaveReleaseNamesTheRepositoryReadThatFailed(t *testing.T) {
	for _, tc := range []struct {
		name, code, want string
		status           int
		failure          error
	}{
		{"a read that failed", "REPOSITORY_READ_FAILED",
			"could not read the repository of LEGION-209: read the repository Dispatch resolves for LEGION-209: Dispatch is down; retry release_children",
			http.StatusBadGateway, errors.New("read the repository Dispatch resolves for LEGION-209: Dispatch is down")},
		{"a child held for a read of the mapping", "CHILD_RELEASE_BACKOFF",
			"LEGION-209: LEGION-209 resolves to zeta/tools, which the repository mapping did not serve when last read; retry after 1s",
			http.StatusConflict, &intake.RetryLater{After: time.Second, Err: errors.New("LEGION-209 resolves to zeta/tools, which the repository mapping did not serve when last read")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			records := record.NewStore()
			engine := workflow.New(records, workflow.Config{Project: testProject, Repositories: testRepositories, DesignGate: config.DesignGateOff}, slog.New(slog.NewTextHandler(io.Discard, nil)))
			statuses := &statusRecorder{}
			h.handler = NewServer("127.0.0.1", 8437, Options{
				Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, OperatorToken: testOperatorToken,
				Controller: h.store, Grants: credential.New(nil), Pool: h.store.Pool(), Record: records,
				Handlers: []intake.Handler{engine}, Dispatch: statuses,
				PrepareRepository: func(_ context.Context, issue intake.DispatchIssue) (intake.DispatchIssue, error) {
					return issue, tc.failure
				},
			}).Handler
			root, child := "LEGION-208", "LEGION-209"
			h.recordIssue(record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Project: testProject, Title: root, Phase: phase.Planning, Generation: 1, Status: "in_progress", HandedOver: true})
			if err := h.store.Tx(context.Background(), func(tx pgx.Tx) error {
				return records.PutSlot(context.Background(), tx, record.Slot{Issue: root, Index: 0, AdmittedAt: time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)})
			}); err != nil {
				t.Fatalf("seed the live tree: %v", err)
			}
			statuses.issues = map[string]dispatch.Issue{
				root:  {Key: root, ResolvedRepo: "acme/widgets", Status: "in_progress"},
				child: {Key: child, ResolvedRepo: "acme/widgets", Title: "the child", Status: "todo", Parent: &root, LastSeq: 7},
			}
			architect := newLiveClaim(t, h, root, claim.RoleArchitect)
			recorder := h.request(http.MethodPost, "/legion/v1/waves/release", map[string]any{"grantId": architect.grant(t), "issues": []string{child}}, nil)
			var failure struct{ Code, Error string }
			if err := json.Unmarshal(recorder.Body.Bytes(), &failure); err != nil {
				t.Fatalf("wave release = %d %q, not a failure body: %v", recorder.Code, recorder.Body, err)
			}
			if recorder.Code != tc.status || failure.Code != tc.code || failure.Error != tc.want {
				t.Fatalf("wave release = %d %s %q, want %d %s %q", recorder.Code, failure.Code, failure.Error, tc.status, tc.code, tc.want)
			}
			if recorded := recordedChild(t, h, child); recorded != nil {
				t.Fatalf("the child's record = %#v after a failed preparation, want none", recorded)
			}
		})
	}
}

// unreadRepositories serves every repository testRepositories serves, and zeta/tools at main
// unless unread is set, while it answers zeta/tools as the daemon does a repository whose GitHub
// App installation read failed: left out for now (workflow.Resolution.Unread).
type unreadRepositories struct{ unread *atomic.Bool }

func (r unreadRepositories) ResolveRepository(resolved string) (workflow.Resolution, error) {
	if resolved != "zeta/tools" {
		return testRepositories.ResolveRepository(resolved)
	}
	if r.unread.Load() {
		return workflow.Resolution{Refusal: "the implement App's installation on zeta/tools could not be read: GitHub answered 502; Legion reads it again", LeftOut: true, Unread: true}, nil
	}
	return workflow.Resolution{Repo: resolved, DefaultBranch: "main", GitHubRepo: resolved}, nil
}

// unreadRelease is a harness whose release_children runs over a live tree LEGION-208 on
// acme/widgets, with zeta/tools left out while unread is set, and every child it leaves to a
// recovery (Options.AwaitRecovery) in awaited.
type unreadRelease struct {
	h        *harness
	records  record.Store
	engine   *workflow.Engine
	statuses *statusRecorder
	unread   *atomic.Bool
	awaited  []string
	grant    func() string
}

const unreadRoot = "LEGION-208"

func newUnreadRelease(t *testing.T, issues ...dispatch.Issue) *unreadRelease {
	t.Helper()
	u := &unreadRelease{h: newHarness(t), records: record.NewStore(), statuses: &statusRecorder{}, unread: &atomic.Bool{}}
	u.unread.Store(true)
	u.engine = workflow.New(u.records, workflow.Config{Project: testProject, Repositories: unreadRepositories{u.unread}, DesignGate: config.DesignGateOff}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	u.h.handler = NewServer("127.0.0.1", 8437, Options{
		Supervisor: u.h.supervisor, BootTokens: u.h.tokens, Project: testProject, OperatorToken: testOperatorToken,
		Controller: u.h.store, Grants: credential.New(nil), Pool: u.h.store.Pool(), Record: u.records,
		Handlers: []intake.Handler{u.engine}, Dispatch: u.statuses,
		RepositoryUnread: func(resolved string) bool { return resolved == "zeta/tools" && u.unread.Load() },
		AwaitRecovery: func(repo, tree, child, _ string) {
			u.awaited = append(u.awaited, repo+" "+tree+" "+child)
		},
	}).Handler
	u.h.recordIssue(record.Issue{Key: unreadRoot, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: unreadRoot, Project: testProject, Title: unreadRoot, Phase: phase.Planning, Generation: 1, Status: "in_progress", HandedOver: true})
	if err := u.h.store.Tx(context.Background(), func(tx pgx.Tx) error {
		return u.records.PutSlot(context.Background(), tx, record.Slot{Issue: unreadRoot, Index: 0, AdmittedAt: time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)})
	}); err != nil {
		t.Fatalf("seed the live tree: %v", err)
	}
	u.statuses.issues = map[string]dispatch.Issue{unreadRoot: {Key: unreadRoot, ResolvedRepo: "acme/widgets", Status: "in_progress"}}
	for _, issue := range issues {
		u.statuses.issues[issue.Key] = issue
	}
	architect := newLiveClaim(t, u.h, unreadRoot, claim.RoleArchitect)
	u.grant = func() string { return architect.grant(t) }
	return u
}

// release releases wave and answers the release's answer, failing unless it is 200 with every
// child of wave released and none held on a link.
func (u *unreadRelease) release(t *testing.T, wave ...string) {
	t.Helper()
	recorder := u.h.request(http.MethodPost, "/legion/v1/waves/release", map[string]any{"grantId": u.grant(), "issues": wave}, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("wave release of %v = %d: %s, want it released", wave, recorder.Code, recorder.Body)
	}
	var answer WaveReleaseResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(answer.Released, wave) || len(answer.Held) != 0 {
		t.Fatalf("wave release = %+v, want %v released and held on no link", answer, wave)
	}
}

// notices is the reason of every notice queued to the root's architect.
func (u *unreadRelease) notices(t *testing.T) []string {
	t.Helper()
	var reasons []string
	if err := u.h.store.Tx(context.Background(), func(tx pgx.Tx) error {
		rows, err := tx.Query(context.Background(), `select payload::text from outbox where issue = $1 and kind = 'notice' order by id`, unreadRoot)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var reason string
			if err := rows.Scan(&reason); err != nil {
				return err
			}
			reasons = append(reasons, reason)
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	return reasons
}

func (u *unreadRelease) retryRow(t *testing.T, key string) bool {
	t.Helper()
	var row *record.ReleaseRetry
	if err := u.h.store.Tx(context.Background(), func(tx pgx.Tx) error {
		var err error
		row, err = u.records.ReleaseRetry(context.Background(), tx, key)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return row != nil
}

func on(key, repo, status string, parent string, seq int64) dispatch.Issue {
	return dispatch.Issue{Key: key, ResolvedRepo: repo, Title: key, Status: status, Parent: &parent, LastSeq: seq}
}

// While a repository's GitHub App installation read fails, release_children never ends a wave on a
// child it leaves unrecorded for it: the wave goes on, and each such child is answered by what the
// daemon's recovery of the repository will do. P (backlog, zeta) holds C (todo, zeta), and C holds
// G (backlog, acme); Q (backlog, acme) holds D (todo, acme). Released together while zeta is
// unread, Q is written and recorded and D recorded under it, after C in the wave's order; C is
// left to the recovery and its architect told at once that P must be released first, since the
// recovery records no child under a backlog parent; G, under C, is not written, and its architect
// told to release it again once C is recorded. Neither has a release-retry record: they are no
// unrecorded parent that has not reached the daemon. Before, C's answer, REPOSITORY_READ_FAILED,
// ended the wave: D was never recorded and nothing told the architect when zeta was answered.
func TestWaveReleaseGoesOnPastAChildLeftForAnUnreadRepository(t *testing.T) {
	const p, c, q, d, g = "LEGION-220", "LEGION-221", "LEGION-222", "LEGION-223", "LEGION-224"
	u := newUnreadRelease(t,
		on(p, "zeta/tools", "backlog", unreadRoot, 1), on(c, "zeta/tools", "todo", p, 2),
		on(q, "acme/widgets", "backlog", unreadRoot, 3), on(d, "acme/widgets", "todo", q, 4),
		on(g, "acme/widgets", "backlog", c, 5))
	u.release(t, c, d, q, g)
	for _, key := range []string{q, d} {
		if got := recordedChild(t, u.h, key); got == nil || got.Tree != unreadRoot {
			t.Fatalf("%s = %+v, want it recorded in %s's tree past the child left for zeta/tools", key, got, unreadRoot)
		}
	}
	for _, key := range []string{c, g} {
		if got := recordedChild(t, u.h, key); got != nil {
			t.Fatalf("%s recorded as %+v while zeta/tools is unread", key, got)
		}
		if u.retryRow(t, key) {
			t.Fatalf("%s has a release-retry record, want none", key)
		}
	}
	if want := []statusWrite{{issue: q, status: "todo"}}; !slices.Equal(u.statuses.writes, want) {
		t.Fatalf("Dispatch writes = %#v, want only %s's: %s waits for %s", u.statuses.writes, q, g, c)
	}
	if want := []string{"zeta/tools " + unreadRoot + " " + c, "zeta/tools " + unreadRoot + " " + g}; !slices.Equal(u.awaited, want) {
		t.Fatalf("children left to the recovery = %q, want %q", u.awaited, want)
	}
	notices := u.notices(t)
	if len(notices) != 2 ||
		!strings.Contains(notices[0], c+" is todo but not recorded") || !strings.Contains(notices[0], p+", on its parent chain, is backlog") || !strings.Contains(notices[0], "Release "+p+" first, then release "+c+" again") ||
		!strings.Contains(notices[1], g+" was not written todo: it is under "+c) || !strings.Contains(notices[1], "Release "+g+" again once "+c+" is recorded") {
		t.Fatalf("notices to %s's architect = %q, want one for %s naming %s and one for %s naming %s", unreadRoot, notices, c, p, g, c)
	}
}

// A todo child on an unread repository under a parent that Dispatch shows todo on a served
// repository, but that the daemon has not recorded yet (its own event not applied: a consumer
// behind), is answered released with no notice and left to the recovery: the parent's event
// records the parent, and the recovery then records the child. Before, the architect was told at
// once that the recovery would not record the child and to release the parent first, which was
// false.
func TestWaveReleaseSendsNoNoticeForAChildUnderATodoParentNotRecordedYet(t *testing.T) {
	const parent, child = "LEGION-230", "LEGION-231"
	u := newUnreadRelease(t, on(parent, "acme/widgets", "todo", unreadRoot, 2), on(child, "zeta/tools", "todo", parent, 3))
	u.release(t, child)
	if notices := u.notices(t); len(notices) != 0 {
		t.Fatalf("notices = %q, want none: %s's own event records it, and the recovery then records %s", notices, parent, child)
	}
	if want := []string{"zeta/tools " + unreadRoot + " " + child}; !slices.Equal(u.awaited, want) {
		t.Fatalf("children left to the recovery = %q, want %q", u.awaited, want)
	}
	if recordedChild(t, u.h, child) != nil || u.retryRow(t, child) {
		t.Fatalf("%s has a record or a release-retry record while zeta/tools is unread, want neither", child)
	}
	// The parent's own event arrives; then GitHub answers and the recovery applies the child.
	if _, err := intake.ApplyFact(context.Background(), u.h.store.Pool(), "dispatch", "event-"+parent, intake.ObservedIssue(u.statuses.issues[parent]), u.engine); err != nil {
		t.Fatal(err)
	}
	u.unread.Store(false)
	if _, err := intake.ApplyFact(context.Background(), u.h.store.Pool(), "daemon", "recovery-"+child, intake.ObservedIssue(u.statuses.issues[child]), u.engine); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{parent, child} {
		if got := recordedChild(t, u.h, key); got == nil || got.Tree != unreadRoot {
			t.Fatalf("%s = %+v after the serve, want it recorded in %s's tree", key, got, unreadRoot)
		}
	}
}

// A todo child left for an unread repository is answered released, with no notice, when the
// recovery will record it: under the recorded root, or under a chain of todo issues up to it.
// Under a parent the recovery will not record (one in
// backlog on a served repository), it is answered released too, with a notice naming the parent to
// release first: the parent is unrecorded for a reason of its own, not for GitHub, so the message
// says so. Once GitHub answers and the parent is recorded, the architect's release records the
// child.
func TestWaveReleaseAnswersAChildOnAnUnreadRepositoryByWhatItsRecoveryWillDo(t *testing.T) {
	const child, middle, grandchild, held, orphan = "LEGION-209", "LEGION-210", "LEGION-211", "LEGION-212", "LEGION-213"
	u := newUnreadRelease(t,
		on(child, "zeta/tools", "todo", unreadRoot, 7),
		on(middle, "zeta/tools", "todo", unreadRoot, 3), on(grandchild, "zeta/tools", "todo", middle, 5),
		on(held, "acme/widgets", "backlog", unreadRoot, 2), on(orphan, "zeta/tools", "todo", held, 6))
	u.release(t, child)
	u.release(t, grandchild)
	if notices := u.notices(t); len(notices) != 0 {
		t.Fatalf("notices = %q, want none: the recovery records %s and %s", notices, child, grandchild)
	}
	u.release(t, orphan)
	notices := u.notices(t)
	if len(notices) != 1 || !strings.Contains(notices[0], held+", on its parent chain, is backlog and not recorded") || !strings.Contains(notices[0], "Release "+held+" first, then release "+orphan+" again") {
		t.Fatalf("notices = %q, want one naming %s, in backlog on acme/widgets, to release first", notices, held)
	}
	for _, key := range []string{child, grandchild, orphan} {
		if recordedChild(t, u.h, key) != nil || u.retryRow(t, key) {
			t.Fatalf("%s has a record or a release-retry record while zeta/tools is unread, want neither", key)
		}
	}

	// GitHub answers: the recovery records the parent, as its listing does; the release records the
	// grandchild.
	u.unread.Store(false)
	if _, err := intake.ApplyFact(context.Background(), u.h.store.Pool(), "daemon", "recovery-"+middle, intake.ObservedIssue(u.statuses.issues[middle]), u.engine); err != nil {
		t.Fatal(err)
	}
	if recordedChild(t, u.h, middle) == nil {
		t.Fatalf("%s not recorded under %s", middle, unreadRoot)
	}
	u.release(t, grandchild)
	if got := recordedChild(t, u.h, grandchild); got == nil || got.Tree != unreadRoot {
		t.Fatalf("%s = %+v after the release, want it recorded in %s's tree", grandchild, got, unreadRoot)
	}
}

func recordedChild(t *testing.T, h *harness, key string) *record.Issue {
	t.Helper()
	var got *record.Issue
	if err := h.store.Tx(context.Background(), func(tx pgx.Tx) error {
		var err error
		got, err = record.NewStore().Issue(context.Background(), tx, key)
		return err
	}); err != nil {
		t.Fatalf("read %s: %v", key, err)
	}
	return got
}

// A todo child whose parent the same wave releases out of triage is recorded in that one request,
// in whichever order the wave names the two: the release records the parent from its own todo write
// first, and the workflow, which records a child only under a recorded parent, then records the
// child under it. Before, the parent was recorded only once its write's todo event came back
// through the Dispatch consumer, so the release answered CHILD_NOT_RECORDED and the child waited for
// the architect's retry after that event.
func TestWaveReleaseRecordsATodoChildOfAChildTheSameWaveReleases(t *testing.T) {
	root, parent, child := "LEGION-208", "LEGION-209", "LEGION-210"
	for _, wave := range [][]string{{parent, child}, {child, parent}} {
		t.Run(strings.Join(wave, ","), func(t *testing.T) {
			h := newHarness(t)
			records := record.NewStore()
			engine := workflow.New(records, workflow.Config{Project: testProject, Repositories: testRepositories, DesignGate: config.DesignGateRootIssues}, slog.New(slog.NewTextHandler(io.Discard, nil)))
			statuses := &statusRecorder{}
			h.handler = NewServer("127.0.0.1", 8437, Options{
				Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, OperatorToken: testOperatorToken,
				Controller: h.store, Grants: credential.New(nil), Pool: h.store.Pool(), Record: records,
				Handlers: []intake.Handler{engine}, Dispatch: statuses,
			}).Handler
			h.recordIssue(record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Project: testProject, Title: root, Phase: phase.Planning, Generation: 1, Status: "in_progress", HandedOver: true})
			if err := h.store.Tx(context.Background(), func(tx pgx.Tx) error {
				return records.PutSlot(context.Background(), tx, record.Slot{Issue: root, Index: 0, AdmittedAt: time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)})
			}); err != nil {
				t.Fatalf("seed the live tree: %v", err)
			}
			statuses.issues = map[string]dispatch.Issue{
				root:   {Key: root, ResolvedRepo: "acme/widgets", Status: "in_progress"},
				parent: {Key: parent, ResolvedRepo: "acme/widgets", Title: parent, Status: "triage", Parent: &root, LastSeq: 2},
				child:  {Key: child, ResolvedRepo: "acme/widgets", Title: child, Status: "todo", Parent: &parent, LastSeq: 5},
			}
			architect := newLiveClaim(t, h, root, claim.RoleArchitect)

			recorder := h.request(http.MethodPost, "/legion/v1/waves/release", map[string]any{"grantId": architect.grant(t), "issues": wave}, nil)
			if recorder.Code != http.StatusOK {
				t.Fatalf("wave release = %d: %s", recorder.Code, recorder.Body)
			}
			if got := recordedChild(t, h, parent); got == nil || got.Tree != root || got.LastDispatchSeq != 3 {
				t.Fatalf("the parent after the release = %#v, want it recorded in %s's tree at its write's sequence 3", got, root)
			}
			if got := recordedChild(t, h, child); got == nil || got.Tree != root || got.LastDispatchSeq != 5 {
				t.Fatalf("the child after the release = %#v, want it recorded in %s's tree at Dispatch's sequence 5", got, root)
			}
			if want := []statusWrite{{issue: parent, status: "todo"}}; len(statuses.writes) != 1 || statuses.writes[0] != want[0] {
				t.Fatalf("Dispatch writes = %#v, want only the parent's release", statuses.writes)
			}
		})
	}
}

// A child whose parent still has not reached the daemon does not make an architect retry forever.
// After the bounded backoff attempts, release_children answers CHILD_RELEASE_STALLED and queues one
// notice to the tree architect.
func TestWaveReleaseBoundsAnUnrecordedChildRetryAndNotifiesTheArchitect(t *testing.T) {
	h := newHarness(t)
	records := record.NewStore()
	engine := workflow.New(records, workflow.Config{Project: testProject, Repositories: testRepositories}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	statuses := &statusRecorder{}
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, OperatorToken: testOperatorToken,
		Controller: h.store, Grants: credential.New(nil), Pool: h.store.Pool(), Record: records,
		Handlers: []intake.Handler{engine}, Dispatch: statuses,
	}).Handler
	root, parent, child := "LEGION-208", "LEGION-209", "LEGION-210"
	h.recordIssue(record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Project: testProject, Title: root, Phase: phase.Planning, Generation: 1, Status: "in_progress", HandedOver: true})
	if err := h.store.Tx(context.Background(), func(tx pgx.Tx) error {
		for _, put := range []error{
			records.PutSlot(context.Background(), tx, record.Slot{Issue: root, Index: 0, AdmittedAt: time.Now()}),
			records.PutReleaseRetry(context.Background(), tx, record.ReleaseRetry{Issue: child, Sequence: 5, Attempts: maxChildReleaseAttempts - 1, NextAt: time.Now().Add(-time.Second)}),
		} {
			if put != nil {
				return put
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed retry: %v", err)
	}
	statuses.issues = map[string]dispatch.Issue{
		root:   {Key: root, ResolvedRepo: "acme/widgets", Status: "in_progress"},
		parent: {Key: parent, ResolvedRepo: "acme/widgets", Title: parent, Status: "todo", Parent: &root, LastSeq: 3},
		child:  {Key: child, ResolvedRepo: "acme/widgets", Title: child, Status: "todo", Parent: &parent, LastSeq: 5},
	}
	architect := newLiveClaim(t, h, root, claim.RoleArchitect)
	assertFailure(t, h.request(http.MethodPost, "/legion/v1/waves/release", map[string]any{"grantId": architect.grant(t), "issues": []string{child}}, nil), http.StatusConflict, "CHILD_RELEASE_STALLED")
	var notices int
	if err := h.store.Tx(context.Background(), func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `select count(*) from outbox where issue = $1 and kind = 'notice' and payload->>'kind' = 'child-release-stalled'`, root).Scan(&notices)
	}); err != nil {
		t.Fatalf("read the stalled notice: %v", err)
	}
	if notices != 1 {
		t.Fatalf("stalled notices = %d, want one", notices)
	}
}
