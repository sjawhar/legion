package daemon

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// faultyStore is a record store whose next live-record snapshot fails once, and whose write of
// pauseKey's record waits at reached until release closes.
type faultyStore struct {
	record.Store
	failIssues bool
	pauseKey   string
	reached    chan struct{}
	release    chan struct{}
}

func (s *faultyStore) Issues(ctx context.Context, tx pgx.Tx) ([]record.Issue, error) {
	if s.failIssues {
		s.failIssues = false
		return nil, errors.New("the database refused the live-record snapshot")
	}
	return s.Store.Issues(ctx, tx)
}

func (s *faultyStore) PutIssue(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
	if issue.Key == s.pauseKey {
		s.pauseKey = ""
		close(s.reached)
		<-s.release
	}
	return s.Store.PutIssue(ctx, tx, issue)
}

// mappingRig is a workflow runtime over a fresh database and NATS whose project maps acme/widgets
// (its default) and zeta/tools, each App installed on each but those missing names ("<role>
// <owner/name>"), with GitHub intake hearing what the boot's mapping serves.
func mappingRig(t *testing.T, missing map[string]bool) (*workflowRuntime, *mappingDispatch, *installations, jetstream.Stream) {
	t.Helper()
	ctx := context.Background()
	conn, err := nats.Connect(workflowNATS(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(conn.Close)
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := js.Stream(ctx, "ENVOY_NOTIFICATIONS")
	if err != nil {
		t.Fatal(err)
	}
	github := newNamedGitHub(t, map[string]string{"acme/widgets": "acme/widgets@main", "zeta/tools": "zeta/tools@main"})
	client := &mappingDispatch{mappings: []dispatch.RepoProject{{Repo: "acme/widgets", Project: "LEGION", Default: true}, {Repo: "zeta/tools", Project: "LEGION"}}}
	installs := &installations{missing: missing}
	w := &workflowRuntime{pool: isolatedOutboxPool(t), records: record.NewStore(), dispatch: client, dispatchProject: "LEGION", installs: installs,
		repositories: newRepositories("LEGION"), defaultBranches: newDefaultBranches(roleTokens{}, github.url, defaultBranchTTL, time.Now), log: quietLogger(),
		mappingChanged: make(chan struct{}, 1)}
	if err := w.loadMapping(ctx); err != nil {
		t.Fatal(err)
	}
	if err := w.nameMapping(ctx); err != nil {
		t.Fatal(err)
	}
	heard, err := w.heardAtBoot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if w.consumers, err = intake.OpenConsumers(ctx, js, intake.ConsumerSpec{Project: "LEGION", Repositories: heard, Logger: quietLogger()}); err != nil {
		t.Fatal(err)
	}
	running, stop := context.WithCancel(ctx)
	refreshed := make(chan struct{})
	go func() { w.refreshOnMappingChange(running); close(refreshed) }()
	t.Cleanup(func() { stop(); <-refreshed })
	return w, client, installs, stream
}

// prepareAsDelivered prepares fact as the Dispatch consumer does, delivering it again after a
// held-back preparation (intake.RetryLater) until it is prepared or fails.
func prepareAsDelivered(t *testing.T, w *workflowRuntime, fact intake.DispatchIssue) (intake.DispatchIssue, error) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		prepared, err := w.prepareRepository(context.Background(), fact)
		var later *intake.RetryLater
		if !errors.As(err, &later) || time.Now().After(deadline) {
			return prepared, err
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func githubFilterSubjects(t *testing.T, stream jetstream.Stream) []string {
	t.Helper()
	consumer, err := stream.Consumer(context.Background(), "legion-go-LEGION-github")
	if err != nil {
		t.Fatal(err)
	}
	info, err := consumer.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return info.Config.FilterSubjects
}

func recordedIssue(t *testing.T, w *workflowRuntime, key string) *record.Issue {
	t.Helper()
	var got *record.Issue
	if err := pgx.BeginFunc(context.Background(), w.pool, func(tx pgx.Tx) error {
		var err error
		got, err = w.records.Issue(context.Background(), tx, key)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return got
}

// A read of the mapping publishes a repository as served only once GitHub intake hears it: a read
// that fails before its subscription update leaves the repository left out, so no issue is
// recorded on it, and the next read covers the issue that came up for admission. An issue is never
// recorded on a repository GitHub intake does not hear.
func TestAFailedRefreshServesNothingUntilARetrySubscribesIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		fail bool
	}{
		{"a refresh that succeeds", false},
		{"a refresh whose live-record snapshot fails once", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			w, client, installs, stream := mappingRig(t, map[string]bool{"review zeta/tools": true})
			client.issue = dispatch.Issue{Key: "LEGION-301", Status: "todo", HandedOver: true, ResolvedRepo: "zeta/tools", LastSeq: 1}
			installs.missing["review zeta/tools"] = false
			w.records = &faultyStore{Store: w.records, failIssues: tc.fail}
			fact := intake.DispatchIssue{Key: "LEGION-301", Seq: 1, Status: "todo", Title: "new", Rank: "A", HandedOver: true, ResolvedRepo: "zeta/tools"}
			if tc.fail {
				if err := w.refreshMapping(ctx); err == nil {
					t.Fatal("the failed snapshot did not fail the read")
				}
				if _, _, leftOut := w.repositories.lookup("zeta/tools"); !leftOut {
					t.Fatal("the failed refresh published zeta/tools as served")
				}
				if filters := githubFilterSubjects(t, stream); slices.Contains(filters, "notifications.github.zeta.tools.>") {
					t.Fatalf("GitHub filters = %v after a failed refresh, want zeta/tools not yet heard", filters)
				}
			}
			if _, err := prepareAsDelivered(t, w, fact); err != nil {
				t.Fatal(err)
			}
			engine := workflow.New(w.records, workflow.Config{Project: "LEGION", Repositories: w, DesignGate: config.DesignGateOff}, quietLogger())
			admission := admit.New(w.records, engine, 1, "LEGION", quietLogger())
			if _, err := intake.ApplyFact(ctx, w.pool, "test", "admission", fact, engine, admission); err != nil {
				t.Fatal(err)
			}
			if got := recordedIssue(t, w, fact.Key); got == nil || got.Repo != "zeta/tools" {
				t.Fatalf("LEGION-301 = %+v, want it recorded on zeta/tools", got)
			}
			if filters := githubFilterSubjects(t, stream); !slices.Contains(filters, "notifications.github.zeta.tools.>") {
				t.Fatalf("GitHub filters = %v, want zeta/tools heard once an issue is recorded on it", filters)
			}
		})
	}
}

// A refresh that unmaps a repository and an admission recording an issue on it are serialized: an
// admission that resolved the repository before the refresh commits before the refresh reads the
// records, so the repository it recorded stays heard. A refresh never unsubscribes a
// repository an admission is about to commit an issue on.
func TestARefreshThatUnmapsARepositoryKeepsHearingAnIssueAdmittedOnIt(t *testing.T) {
	ctx := context.Background()
	w, client, _, stream := mappingRig(t, map[string]bool{})
	if err := pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
		if err := w.records.PutIssue(ctx, tx, record.Issue{Key: "LEGION-300", Tree: "LEGION-300", Project: "LEGION", Title: "root", Phase: phase.Planning,
			Generation: 1, Status: "in_progress", Rank: "A", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"}); err != nil {
			return err
		}
		return w.records.PutSlot(ctx, tx, record.Slot{Issue: "LEGION-300", Index: 0, AdmittedAt: time.Now()})
	}); err != nil {
		t.Fatal(err)
	}
	store := &faultyStore{Store: w.records, pauseKey: "LEGION-301", reached: make(chan struct{}), release: make(chan struct{})}
	w.records = store
	engine := workflow.New(store, workflow.Config{Project: "LEGION", Repositories: w, DesignGate: config.DesignGateOff}, quietLogger())
	child := intake.DispatchIssue{Key: "LEGION-301", Parent: "LEGION-300", Seq: 1, Status: "todo", Title: "child", Rank: "A", ResolvedRepo: "zeta/tools"}
	if _, err := w.prepareRepository(ctx, child); err != nil {
		t.Fatal(err)
	}
	admitted := make(chan error, 1)
	go func() {
		_, err := intake.ApplyFact(ctx, w.pool, "test", "child", child, engine)
		admitted <- err
	}()
	select {
	case <-store.reached:
	case <-time.After(10 * time.Second):
		t.Fatal("the admission never reached its record write")
	}
	client.set(dispatch.RepoProject{Repo: "acme/widgets", Project: "LEGION", Default: true})
	refreshed := make(chan error, 1)
	go func() { refreshed <- w.refreshMapping(ctx) }()
	// Without the serialization the refresh finishes here, while the admission waits.
	select {
	case err := <-refreshed:
		refreshed <- err
	case <-time.After(500 * time.Millisecond):
	}
	close(store.release)
	if err := <-admitted; err != nil {
		t.Fatal(err)
	}
	if err := <-refreshed; err != nil {
		t.Fatal(err)
	}
	if got := recordedIssue(t, w, child.Key); got == nil || got.Repo != "zeta/tools" {
		t.Fatalf("LEGION-301 = %+v, want it recorded on zeta/tools", got)
	}
	if filters := githubFilterSubjects(t, stream); !slices.Contains(filters, "notifications.github.zeta.tools.>") {
		t.Fatalf("GitHub filters = %v, want zeta/tools heard: a live child is recorded on it", filters)
	}
}

// GitHub intake hears every repository a record of the project names, whatever its tree's state:
// a root that closed on a repository the mapping no longer serves is heard across the refresh
// that unmaps it and across a restart, so a person moving it back to todo re-admits it on a
// repository whose pull request facts still arrive.
func TestAClosedTreesRepositoryStaysHeardAfterItIsUnmapped(t *testing.T) {
	ctx := context.Background()
	w, client, _, stream := mappingRig(t, map[string]bool{})
	if err := pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
		return w.records.PutIssue(ctx, tx, record.Issue{Key: "LEGION-300", Tree: "LEGION-300", Project: "LEGION", Title: "root", Phase: phase.Done,
			Generation: 1, Status: "done", Rank: "A", Repo: "zeta/tools", DefaultBranch: "main", GitHubRepo: "zeta/tools"})
	}); err != nil {
		t.Fatal(err)
	}
	client.set(dispatch.RepoProject{Repo: "acme/widgets", Project: "LEGION", Default: true})
	if err := w.refreshMapping(ctx); err != nil {
		t.Fatal(err)
	}
	if filters := githubFilterSubjects(t, stream); !slices.Contains(filters, "notifications.github.zeta.tools.>") {
		t.Fatalf("GitHub filters = %v after unmapping zeta/tools, want it heard: a closed root is recorded on it", filters)
	}
	heard, err := w.heardAtBoot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := repositoryNames(heard), []string{"acme/widgets", "zeta/tools"}; !slices.Equal(got, want) {
		t.Fatalf("a restart hears %v, want %v", got, want)
	}
	reopened := intake.DispatchIssue{Key: "LEGION-300", Seq: 2, Status: "todo", Title: "root", Rank: "A", HandedOver: true, ResolvedRepo: "zeta/tools"}
	if reopened, err = w.prepareRepository(ctx, reopened); err != nil {
		t.Fatal(err)
	}
	engine := workflow.New(w.records, workflow.Config{Project: "LEGION", Repositories: w, DesignGate: config.DesignGateOff}, quietLogger())
	admission := admit.New(w.records, engine, 1, "LEGION", quietLogger())
	if _, err := intake.ApplyFact(ctx, w.pool, "test", "reopen", reopened, engine, admission); err != nil {
		t.Fatal(err)
	}
	if got := recordedIssue(t, w, "LEGION-300"); got == nil || got.Phase != phase.Admitted || got.Repo != "zeta/tools" {
		t.Fatalf("LEGION-300 = %+v, want it re-admitted on zeta/tools", got)
	}
	if filters := githubFilterSubjects(t, stream); !slices.Contains(filters, "notifications.github.zeta.tools.>") {
		t.Fatalf("GitHub filters = %v, want zeta/tools heard for the re-admitted root", filters)
	}
}

// GitHub intake hears a repository as GitHub spells it, which Envoy's subjects carry, whatever
// spelling an issue's record keeps for its workspace: a record the backfill filled with a clone's
// spelling is heard once, under the served repository's GitHub spelling, and a record on a
// repository the mapping no longer serves is heard under the GitHub spelling its record keeps, with
// no read of GitHub.
func TestGitHubIntakeHearsEachRepositoryAsGitHubSpellsIt(t *testing.T) {
	ctx := context.Background()
	w, _, _, _ := mappingRig(t, map[string]bool{})
	if err := pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
		for index, issue := range []record.Issue{
			{Key: "LEGION-300", Repo: "ACME/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"},
			{Key: "LEGION-301", Repo: "orbit/gadgets", DefaultBranch: "main", GitHubRepo: "Orbit/Gadgets"},
		} {
			issue.Tree, issue.Project, issue.Title, issue.Phase, issue.Generation, issue.Status, issue.Rank = issue.Key, "LEGION", issue.Key, phase.Planning, 1, "in_progress", "A"
			if err := w.records.PutIssue(ctx, tx, issue); err != nil {
				return err
			}
			if err := w.records.PutSlot(ctx, tx, record.Slot{Issue: issue.Key, Index: index, AdmittedAt: time.Now()}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	heard, err := w.heardAtBoot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := repositoryNames(heard), []string{"acme/widgets", "Orbit/Gadgets", "zeta/tools"}; !slices.Equal(got, want) {
		t.Fatalf("heard %v, want %v", got, want)
	}
}

// A project whose every mapped repository lacks an App boots serving none: GitHub intake hears no
// repository's events, and an issue handed to Legion on one is not admitted, the daemon saying why
// on it. Once the App is installed, the issue's next hand-over is held for a read of the mapping,
// which serves the repository and records the issue on it, which GitHub intake then hears.
func TestAProjectServingNoRepositoryStartsServingOnceItsAppIsInstalled(t *testing.T) {
	ctx := context.Background()
	w, client, installs, stream := mappingRig(t, map[string]bool{"review acme/widgets": true, "review zeta/tools": true})
	client.issue = dispatch.Issue{Key: "LEGION-301", Status: "todo", HandedOver: true, ResolvedRepo: "acme/widgets", LastSeq: 2}
	if served := w.repositories.Accepted(); len(served) != 0 {
		t.Fatalf("served %v, want none", served)
	}
	if filters := githubFilterSubjects(t, stream); !slices.Equal(filters, []string{"notifications.github.-.-"}) {
		t.Fatalf("GitHub filters = %v, want the one no repository publishes on", filters)
	}
	engine := workflow.New(w.records, workflow.Config{Project: "LEGION", Repositories: w, DesignGate: config.DesignGateOff}, quietLogger())
	admission := admit.New(w.records, engine, 1, "LEGION", quietLogger())
	handOver := func(seq int64) {
		t.Helper()
		fact := intake.DispatchIssue{Key: "LEGION-301", Seq: seq, Status: "todo", Title: "new", Rank: "A", HandedOver: true, ResolvedRepo: "acme/widgets"}
		prepared, err := prepareAsDelivered(t, w, fact)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := intake.ApplyFact(ctx, w.pool, "test", "todo-"+string(rune('0'+seq)), prepared, engine, admission); err != nil {
			t.Fatal(err)
		}
	}
	handOver(1)
	if got := recordedIssue(t, w, "LEGION-301"); got != nil {
		t.Fatalf("LEGION-301 recorded as %+v while no repository is served", got)
	}
	var notes []string
	rows, err := w.pool.Query(ctx, "select payload->>'body' from outbox where issue = 'LEGION-301' and kind = 'dispatch_message'")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var note string
		if err := rows.Scan(&note); err != nil {
			t.Fatal(err)
		}
		notes = append(notes, note)
	}
	rows.Close()
	if len(notes) != 1 || !strings.Contains(notes[0], "the review App is not installed on acme/widgets") {
		t.Fatalf("notes on LEGION-301 = %q, want one naming the review App acme/widgets lacks", notes)
	}

	installs.missing["review acme/widgets"] = false
	handOver(2)
	if got := recordedIssue(t, w, "LEGION-301"); got == nil || got.Repo != "acme/widgets" {
		t.Fatalf("LEGION-301 = %+v once the App is installed, want it recorded on acme/widgets", got)
	}
	if filters := githubFilterSubjects(t, stream); !slices.Equal(filters, []string{"notifications.github.acme.widgets.>"}) {
		t.Fatalf("GitHub filters = %v, want acme/widgets heard", filters)
	}
}

// An issue handed to Legion on a repository mapped in Dispatch just before, whose mapping event
// the daemon has not yet read, is not refused as not mapped: its preparation waits for a read of
// the mapping that covers it. The held event comes back after the issue's newer events, so it is
// prepared as Dispatch now shows the issue: still handed over, it is recorded on its repository;
// withdrawn while it was held (moved to backlog, the label taken off), it records nothing; and on
// another repository with no event of its own (an ancestor's or the project's default changed),
// it is recorded on the one it resolves to now.
func TestAnIssueOnARepositoryMappedJustBeforeIsAdmittedOnIt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		now    dispatch.Issue
		wantOn string
	}{
		{"still handed over in todo", dispatch.Issue{Key: "LEGION-302", Status: "todo", HandedOver: true, ResolvedRepo: "zeta/tools", LastSeq: 2}, "zeta/tools"},
		{"withdrawn while it was held", dispatch.Issue{Key: "LEGION-302", Status: "backlog", ResolvedRepo: "zeta/tools", LastSeq: 2}, ""},
		{"on another repository with no event of its own", dispatch.Issue{Key: "LEGION-302", Status: "todo", HandedOver: true, ResolvedRepo: "acme/widgets", LastSeq: 1}, "acme/widgets"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			w, client, _, stream := mappingRig(t, map[string]bool{})
			client.set(dispatch.RepoProject{Repo: "acme/widgets", Project: "LEGION", Default: true})
			if err := w.refreshMapping(ctx); err != nil {
				t.Fatal(err)
			}
			client.set(dispatch.RepoProject{Repo: "acme/widgets", Project: "LEGION", Default: true}, dispatch.RepoProject{Repo: "zeta/tools", Project: "LEGION"})
			client.issue = tc.now
			fact := intake.DispatchIssue{Key: "LEGION-302", Seq: 1, Status: "todo", Title: "new", Rank: "A", HandedOver: true, ResolvedRepo: "zeta/tools"}
			prepared, err := prepareAsDelivered(t, w, fact)
			if err != nil {
				t.Fatal(err)
			}
			if len(w.awaiting) != 0 {
				t.Fatalf("held %v after the event was prepared, want none", w.awaiting)
			}
			engine := workflow.New(w.records, workflow.Config{Project: "LEGION", Repositories: w, DesignGate: config.DesignGateOff}, quietLogger())
			admission := admit.New(w.records, engine, 1, "LEGION", quietLogger())
			if _, err := intake.ApplyFact(ctx, w.pool, "test", "handover", prepared, engine, admission); err != nil {
				t.Fatal(err)
			}
			got := recordedIssue(t, w, "LEGION-302")
			if tc.wantOn == "" {
				if got != nil {
					t.Fatalf("LEGION-302 = %+v, want nothing recorded: the person withdrew it while its event was held", got)
				}
				return
			}
			if got == nil || got.Repo != tc.wantOn {
				t.Fatalf("LEGION-302 = %+v, want it recorded on %s", got, tc.wantOn)
			}
			if filters := githubFilterSubjects(t, stream); !slices.Contains(filters, "notifications.github."+strings.ReplaceAll(tc.wantOn, "/", ".")+".>") {
				t.Fatalf("GitHub filters = %v, want %s heard", filters, tc.wantOn)
			}
		})
	}
}

// An admission event whose preparation failed for any reason (here, the repository's default
// branch read from GitHub), held for the mapping first or not, is delivered again after the
// issue's newer events, so it is prepared as Dispatch now shows the issue: a person who withdrew
// it meanwhile is not overruled, and one still handed over is recorded.
func TestAnAdmissionWhosePreparationFailedIsPreparedAsDispatchNowShowsIt(t *testing.T) {
	for _, tc := range []struct {
		name      string
		heldFirst bool
		now       dispatch.Issue
		admitted  bool
	}{
		{"withdrawn after a failed branch read", false, dispatch.Issue{Key: "LEGION-305", Status: "backlog", ResolvedRepo: "acme/widgets", LastSeq: 2}, false},
		{"withdrawn after a hold and a failed branch read", true, dispatch.Issue{Key: "LEGION-305", Status: "backlog", ResolvedRepo: "zeta/tools", LastSeq: 2}, false},
		{"still handed over after a failed branch read", false, dispatch.Issue{Key: "LEGION-305", Status: "todo", HandedOver: true, ResolvedRepo: "acme/widgets", LastSeq: 2}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			w, client, _, _ := mappingRig(t, map[string]bool{})
			repo := "acme/widgets"
			if tc.heldFirst {
				repo = "zeta/tools"
				client.set(dispatch.RepoProject{Repo: "acme/widgets", Project: "LEGION", Default: true})
				if err := w.refreshMapping(ctx); err != nil {
					t.Fatal(err)
				}
				client.set(dispatch.RepoProject{Repo: "acme/widgets", Project: "LEGION", Default: true}, dispatch.RepoProject{Repo: "zeta/tools", Project: "LEGION"})
			}
			github := &failingGitHub{named: map[string]string{"acme/widgets": "acme/widgets@main", "zeta/tools": "zeta/tools@main"}}
			w.defaultBranches = newDefaultBranches(roleTokens{}, github.start(t), 0, time.Now)
			client.issue = dispatch.Issue{Key: "LEGION-305", Status: "todo", HandedOver: true, ResolvedRepo: repo, LastSeq: 1}
			fact := intake.DispatchIssue{Key: "LEGION-305", Seq: 1, Status: "todo", Title: "new", Rank: "A", HandedOver: true, ResolvedRepo: repo}
			if tc.heldFirst {
				var later *intake.RetryLater
				if _, err := w.prepareRepository(ctx, fact); !errors.As(err, &later) {
					t.Fatalf("the first delivery = %v, want it held for the mapping", err)
				}
				testwait.Eventually(t, "the mapping read the held event asked for", func() bool {
					_, refusal, _ := w.repositories.lookup("zeta/tools")
					return refusal == ""
				})
			}
			github.fail.Store(true)
			if _, err := w.prepareRepository(ctx, fact); err == nil {
				t.Fatal("the delivery whose branch read failed was prepared, want it retried")
			}
			github.fail.Store(false)
			client.issue = tc.now
			prepared, err := prepareAsDelivered(t, w, fact)
			if err != nil {
				t.Fatal(err)
			}
			engine := workflow.New(w.records, workflow.Config{Project: "LEGION", Repositories: w, DesignGate: config.DesignGateOff}, quietLogger())
			admission := admit.New(w.records, engine, 1, "LEGION", quietLogger())
			if _, err := intake.ApplyFact(ctx, w.pool, "test", "handover", prepared, engine, admission); err != nil {
				t.Fatal(err)
			}
			if got := recordedIssue(t, w, "LEGION-305"); (got != nil) != tc.admitted {
				t.Fatalf("LEGION-305 = %+v, want recorded %t", got, tc.admitted)
			}
			if len(w.awaiting) != 0 {
				t.Fatalf("held %v after the event was prepared, want none", w.awaiting)
			}
		})
	}
}

// Dispatch's outbox puts off an event whose publish failed and publishes the issue's later events
// first, so an admission event can reach the daemon on its first delivery after a person withdrew
// the issue. It is prepared as Dispatch now shows the issue, as a redelivered one is: the
// withdrawal stands.
func TestALateFirstDeliveryOfAnAdmissionEventIsPreparedAsDispatchNowShowsIt(t *testing.T) {
	ctx := context.Background()
	w, client, _, _ := mappingRig(t, map[string]bool{})
	engine := workflow.New(w.records, workflow.Config{Project: "LEGION", Repositories: w, DesignGate: config.DesignGateOff}, quietLogger())
	admission := admit.New(w.records, engine, 1, "LEGION", quietLogger())
	client.issue = dispatch.Issue{Key: "LEGION-307", Status: "backlog", Title: "new", Rank: "A", ResolvedRepo: "acme/widgets", LastSeq: 2}
	withdrawal := intake.DispatchIssue{Key: "LEGION-307", Seq: 2, Status: "backlog", Title: "new", Rank: "A", ResolvedRepo: "acme/widgets"}
	late := intake.DispatchIssue{Key: "LEGION-307", Seq: 1, Status: "todo", Title: "new", Rank: "A", HandedOver: true, ResolvedRepo: "acme/widgets"}
	for _, delivery := range []struct {
		id    string
		event intake.DispatchIssue
	}{{"withdrawal", withdrawal}, {"late-handover", late}} {
		prepared, err := w.prepareRepository(ctx, delivery.event)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := intake.ApplyFact(ctx, w.pool, "test", delivery.id, prepared, engine, admission); err != nil {
			t.Fatal(err)
		}
	}
	if got := recordedIssue(t, w, "LEGION-307"); got != nil {
		t.Fatalf("LEGION-307 = %+v, want nothing recorded: the person withdrew it before its hand-over reached the daemon", got)
	}
}

// An unrecorded issue's admission event is prepared from Dispatch's view outside the fact lock,
// and release_children may record the issue before that view applies; release_children's own view
// may likewise follow an event that recorded it. A view never reaches a recorded issue: the event
// it was prepared in place of applies as it was delivered, with its actor (here fenced below the
// record), and release_children's, prepared in place of no event, applies nothing. Either way the
// record stays as it was, and the issue's later events, each with its own actor, carry it on.
func TestAViewPreparedBeforeTheIssueWasRecordedNeverAppliesToItsRecord(t *testing.T) {
	for _, tc := range []struct {
		name          string
		releasedFirst bool
	}{
		{"an event's view, release_children recording the issue first", true},
		{"release_children's view, an event recording the issue first", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			w, client, _, _ := mappingRig(t, map[string]bool{})
			engine := workflow.New(w.records, workflow.Config{Project: "LEGION", Repositories: w, DesignGate: config.DesignGateOff}, quietLogger())
			admission := admit.New(w.records, engine, 1, "LEGION", quietLogger())
			apply := func(id string, fact intake.DispatchIssue) {
				t.Helper()
				if _, err := intake.ApplyFact(ctx, w.pool, "test", id, fact, engine, admission); err != nil {
					t.Fatal(err)
				}
			}
			atSix := dispatch.Issue{Key: "LEGION-308", Status: "todo", Title: "new", Rank: "A", HandedOver: true, ResolvedRepo: "acme/widgets", LastSeq: 6}
			var stale intake.DispatchIssue
			if tc.releasedFirst {
				// A session's backlog at sequence 7, whose own event the daemon has not yet read.
				client.issue = dispatch.Issue{Key: "LEGION-308", Status: "backlog", Title: "new", Rank: "A", ResolvedRepo: "acme/widgets", LastSeq: 7}
				handOver := intake.DispatchIssue{Key: "LEGION-308", Seq: 5, Status: "todo", Title: "new", Rank: "A", HandedOver: true, ResolvedRepo: "acme/widgets"}
				var err error
				if stale, err = w.prepareRepository(ctx, handOver); err != nil {
					t.Fatal(err)
				}
				released, err := w.prepareObserved(ctx, intake.ObservedIssue(atSix))
				if err != nil {
					t.Fatal(err)
				}
				apply("release", released)
			} else {
				var err error
				if stale, err = w.prepareObserved(ctx, intake.ObservedIssue(dispatch.Issue{Key: "LEGION-308", Status: "todo", Title: "renamed", Rank: "A", HandedOver: true, ResolvedRepo: "acme/widgets", LastSeq: 7})); err != nil {
					t.Fatal(err)
				}
				client.issue = atSix
				handOver := intake.DispatchIssue{Key: "LEGION-308", Seq: 6, Status: "todo", Title: "new", Rank: "A", HandedOver: true, ResolvedRepo: "acme/widgets"}
				event, err := w.prepareRepository(ctx, handOver)
				if err != nil {
					t.Fatal(err)
				}
				apply("handover", event)
			}
			before := recordedIssue(t, w, "LEGION-308")
			if before == nil || before.LastDispatchSeq != 6 {
				t.Fatalf("LEGION-308 = %+v, want it recorded at sequence 6", before)
			}
			apply("stale-view", stale)
			after := recordedIssue(t, w, "LEGION-308")
			if after.LastDispatchSeq != 6 || after.Status != before.Status || after.DispatchStatus != before.DispatchStatus || after.Title != before.Title {
				t.Fatalf("LEGION-308 = %+v after the view prepared before it was recorded, want it as recorded: %+v", after, before)
			}
		})
	}
}

// An admission event held for a read of the mapping stays held under the sequence it was
// delivered at while Dispatch moves on (a comment on the issue), so its redeliveries wait for the
// one read it asked for rather than asking again. A withdrawal meanwhile ends the hold: the
// redelivery, prepared as Dispatch now shows the issue, records nothing and leaves no hold.
func TestAHeldEventKeepsItsDeliveredSequenceAndAWithdrawalEndsItsHold(t *testing.T) {
	ctx := context.Background()
	w, client, _, _ := mappingRig(t, map[string]bool{})
	client.set(dispatch.RepoProject{Repo: "acme/widgets", Project: "LEGION", Default: true})
	if err := w.refreshMapping(ctx); err != nil {
		t.Fatal(err)
	}
	// Every read of the mapping fails from here, so the hold's read never covers it.
	client.mu.Lock()
	client.err = errors.New("Dispatch is restarting")
	client.mu.Unlock()
	fact := intake.DispatchIssue{Key: "LEGION-306", Seq: 1, Status: "todo", Title: "new", Rank: "A", HandedOver: true, ResolvedRepo: "zeta/tools"}
	var later *intake.RetryLater
	client.issue = dispatch.Issue{Key: "LEGION-306", Status: "todo", Title: "new", Rank: "A", HandedOver: true, ResolvedRepo: "zeta/tools", LastSeq: 1}
	if _, err := w.prepareRepository(ctx, fact); !errors.As(err, &later) {
		t.Fatalf("the first delivery = %v, want it held for a read of the mapping", err)
	}
	client.issue.LastSeq = 2
	if _, err := w.prepareRepository(ctx, fact); !errors.As(err, &later) {
		t.Fatalf("the redelivery after a comment = %v, want it still held", err)
	}
	if requested := w.mappingRequested.Load(); requested != 1 {
		t.Fatalf("%d reads of the mapping requested, want one: the hold stays under the sequence its event was delivered at", requested)
	}
	client.issue = dispatch.Issue{Key: "LEGION-306", Status: "backlog", Title: "new", Rank: "A", ResolvedRepo: "zeta/tools", LastSeq: 3}
	prepared, err := w.prepareRepository(ctx, fact)
	if err != nil || prepared.Status != "backlog" {
		t.Fatalf("the redelivery after the withdrawal = %s, %v; want it prepared as Dispatch shows it, in backlog", prepared.Status, err)
	}
	if len(w.awaiting) != 0 {
		t.Fatalf("held %v after the withdrawal, want no hold left", w.awaiting)
	}
}

// failingGitHub answers GET /repos/{owner}/{repo} as newNamedGitHub does, and 502 while fail is set.
type failingGitHub struct {
	named map[string]string
	fail  atomic.Bool
}

func (g *failingGitHub) start(t *testing.T) string {
	t.Helper()
	named := newNamedGitHub(t, g.named)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if g.fail.Load() {
			http.Error(w, `{"message":"Server Error"}`, http.StatusBadGateway)
			return
		}
		proxied, err := http.Get(named.url + r.URL.Path)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer proxied.Body.Close()
		w.WriteHeader(proxied.StatusCode)
		_, _ = io.Copy(w, proxied.Body)
	}))
	t.Cleanup(server.Close)
	return server.URL
}
