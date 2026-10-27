package daemon

import (
	"context"
	"errors"
	"fmt"
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
	"github.com/sjawhar/legion/daemon/internal/appauth"
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
	return mappingRigWith(t, &installations{missing: missing})
}

// mappingRigWith is mappingRig answering each installation read from installs, with each of opts
// applied to the runtime before its boot reads the mapping.
func mappingRigWith(t *testing.T, installs *installations, opts ...func(*workflowRuntime)) (*workflowRuntime, *mappingDispatch, *installations, jetstream.Stream) {
	t.Helper()
	w, client, installs, stream, _ := mappingRigJS(t, installs, opts...)
	return w, client, installs, stream
}

// mappingRigJS is mappingRigWith with the JetStream it opened, for a test that publishes on it.
func mappingRigJS(t *testing.T, installs *installations, opts ...func(*workflowRuntime)) (*workflowRuntime, *mappingDispatch, *installations, jetstream.Stream, jetstream.JetStream) {
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
	if installs.missing == nil {
		installs.missing = map[string]bool{}
	}
	w := &workflowRuntime{pool: isolatedOutboxPool(t), records: record.NewStore(), dispatch: client, dispatchProject: "LEGION", installs: installs,
		repositories: newRepositories("LEGION"), defaultBranches: newDefaultBranches(roleTokens{}, github.url, defaultBranchTTL, time.Now), log: quietLogger(),
		mappingChanged: make(chan struct{}, 1)}
	for _, opt := range opts {
		opt(w)
	}
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
	if w.consumers, err = intake.OpenConsumers(ctx, js, intake.ConsumerSpec{Project: "LEGION", Repositories: heard, Logger: quietLogger(),
		AckWait: time.Second, NakDelay: 10 * time.Millisecond, PrepareRepository: w.prepareRepository, RepositoryMapping: w.mappingChange}); err != nil {
		t.Fatal(err)
	}
	running, stop := context.WithCancel(ctx)
	refreshed := make(chan struct{})
	go func() { w.refreshOnMappingChange(running); close(refreshed) }()
	t.Cleanup(func() { stop(); <-refreshed })
	return w, client, installs, stream, js
}

// rigWorkflow is the rig's engine and admission (of cap), wired to the rig's runtime as the boot
// wires the daemon's: the engine holds an issue whose repository resolves unread in w
// (holdForRead), and w's own applies run through both.
func rigWorkflow(w *workflowRuntime, cap int) (*workflow.Engine, *admit.Admission) {
	engine := workflow.New(w.records, workflow.Config{Project: "LEGION", Repositories: w, HoldUnread: w.holdForRead, DesignGate: config.DesignGateOff}, quietLogger())
	admission := admit.New(w.records, engine, cap, "LEGION", quietLogger())
	w.engine, w.admission, w.handlers = engine, admission, []intake.Handler{engine, admission}
	return engine, admission
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
				if _, _, leftOut, _ := w.repositories.lookup("zeta/tools"); !leftOut {
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
					_, refusal, _, _ := w.repositories.lookup("zeta/tools")
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

// failingGitHub answers GET /repos/{owner}/{repo} as newNamedGitHub does, and 502 while fail is
// set: for every path, or only for those containing only when it is set.
type failingGitHub struct {
	named map[string]string
	only  string
	fail  atomic.Bool
}

func (g *failingGitHub) start(t *testing.T) string {
	t.Helper()
	named := newNamedGitHub(t, g.named)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if g.fail.Load() && (g.only == "" || strings.Contains(r.URL.Path, g.only)) {
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

// quickInstallReads shortens installReadRetry for a test, restoring it once the rig the test
// opens after it has stopped reading it.
func quickInstallReads(t *testing.T) {
	t.Helper()
	retry := installReadRetry
	installReadRetry.Initial, installReadRetry.Max = 20*time.Millisecond, 40*time.Millisecond
	t.Cleanup(func() { installReadRetry = retry })
}

// served is whether w serves repo now.
func served(w *workflowRuntime, repo string) bool {
	_, refusal, _, _ := w.repositories.lookup(repo)
	return refusal == ""
}

// repositoryNotes is every repository note the daemon queued on key.
func repositoryNotes(t *testing.T, w *workflowRuntime, key string) []string {
	t.Helper()
	rows, err := w.pool.Query(context.Background(), "select payload->>'body' from outbox where issue = $1 and kind = 'dispatch_message'", key)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var notes []string
	for rows.Next() {
		var note string
		if err := rows.Scan(&note); err != nil {
			t.Fatal(err)
		}
		notes = append(notes, note)
	}
	return notes
}

// A repository whose installation read failed at boot is read again of the daemon's own, with no
// mapping change, until GitHub answers; it is then served as a mapping change's read serves it,
// GitHub intake hearing it, with no restart. An issue handed to Legion on it meanwhile is held,
// not refused, and admitted on it once it is served. A served repository whose read fails stays
// served on its last answer and holds nothing else back; once GitHub answers for it too, the
// daemon stops reading again.
func TestARepositoryWhoseInstallationReadFailedIsServedOnceARetryReadsIt(t *testing.T) {
	ctx := context.Background()
	quickInstallReads(t)
	installs := &installations{}
	installs.setFailing(errors.New("read whether the implement App is installed on zeta/tools failed (403)"), "implement zeta/tools")
	w, client, installs, stream := mappingRigWith(t, installs)
	if !served(w, "acme/widgets") {
		t.Fatal("acme/widgets is not served, want it served beside the repository whose read failed")
	}
	if _, refusal, leftOut, unread := w.repositories.lookup("zeta/tools"); !leftOut || !unread || !strings.Contains(refusal, "the implement App's installation on zeta/tools could not be read") {
		t.Fatalf("zeta/tools refusal = %q (left out %v), want it left out for its failed read", refusal, leftOut)
	}
	testwait.Eventually(t, "zeta/tools' installation read again", func() bool { return installs.reads("implement zeta/tools") >= 3 })
	if served(w, "zeta/tools") {
		t.Fatal("zeta/tools served while its read still fails")
	}

	engine, admission := rigWorkflow(w, 1)
	client.issue = dispatch.Issue{Key: "LEGION-301", Status: "todo", Title: "new", Rank: "A", HandedOver: true, ResolvedRepo: "zeta/tools", LastSeq: 1}
	fact := intake.DispatchIssue{Key: "LEGION-301", Seq: 1, Status: "todo", Title: "new", Rank: "A", HandedOver: true, ResolvedRepo: "zeta/tools"}
	prepared, err := w.prepareRepository(ctx, fact)
	if err != nil {
		t.Fatalf("prepare LEGION-301 on zeta/tools = %v, want it prepared to be applied and acknowledged", err)
	}
	if _, err := intake.ApplyFact(ctx, w.pool, "test", "todo-1", prepared, engine, admission); err != nil {
		t.Fatal(err)
	}
	if !w.repositoryHeld("LEGION-301") {
		t.Fatal("LEGION-301 is not held for zeta/tools' read after its apply")
	}
	if got := recordedIssue(t, w, "LEGION-301"); got != nil {
		t.Fatalf("LEGION-301 recorded as %+v while zeta/tools' read fails", got)
	}
	if notes := repositoryNotes(t, w, "LEGION-301"); len(notes) != 0 {
		t.Fatalf("notes on LEGION-301 = %q while it is held, want none", notes)
	}

	installs.setFailing(errors.New("GitHub answered 502"), "review acme/widgets")
	installs.setFailing(nil, "implement zeta/tools")
	testwait.Eventually(t, "zeta/tools served while acme/widgets' read fails", func() bool { return served(w, "zeta/tools") })
	if !served(w, "acme/widgets") {
		t.Fatalf("acme/widgets left out once its read failed, want it served on its last answer; refused %v", w.repositories.Refused())
	}
	if waiting := w.repositories.awaitingRead(); len(waiting) != 1 || !strings.HasPrefix(waiting[0], "acme/widgets: ") {
		t.Fatalf("awaiting a read = %q, want acme/widgets alone", waiting)
	}
	if filters := githubFilterSubjects(t, stream); !slices.Contains(filters, "notifications.github.zeta.tools.>") || !slices.Contains(filters, "notifications.github.acme.widgets.>") {
		t.Fatalf("GitHub filters = %v, want acme/widgets and zeta/tools heard", filters)
	}
	testwait.Eventually(t, "LEGION-301 admitted on zeta/tools with no one moving it", func() bool {
		got := recordedIssue(t, w, "LEGION-301")
		return got != nil && got.Repo == "zeta/tools"
	})
	if notes := repositoryNotes(t, w, "LEGION-301"); len(notes) != 0 {
		t.Fatalf("notes on LEGION-301 = %q, want none: it was held, then admitted", notes)
	}
	if w.repositoryHeld("LEGION-301") {
		t.Fatal("LEGION-301 still held once admitted")
	}

	installs.setFailing(nil, "review acme/widgets")
	testwait.Eventually(t, "acme/widgets read again", func() bool { return len(w.repositories.awaitingRead()) == 0 })
	reads := installs.reads("review acme/widgets")
	time.Sleep(10 * installReadRetry.Max)
	if got := installs.reads("review acme/widgets"); got != reads {
		t.Fatalf("acme/widgets read %d times more once GitHub answered, want the reads again to stop", got-reads)
	}
}

// A repository an App is not installed on (GitHub's 404) stays left out as before, read again only
// while another repository's failed read is: once that one is served, nothing is read again until
// a mapping change or an issue held for one asks.
func TestANotInstalledRepositoryStaysLeftOutWithNoReadAgain(t *testing.T) {
	quickInstallReads(t)
	installs := &installations{missing: map[string]bool{"review acme/widgets": true}}
	installs.setFailing(&appauth.TransientError{Err: errors.New("GitHub answered 502")}, "review zeta/tools")
	w, _, installs, _ := mappingRigWith(t, installs)
	if refused := readFailures(w.repositories.Refused()); len(refused) != 1 || refused[0].Repo != "zeta/tools" {
		t.Fatalf("refused for a failed read = %#v, want zeta/tools alone", refused)
	}
	installs.setFailing(nil, "review zeta/tools")
	testwait.Eventually(t, "zeta/tools served", func() bool { return served(w, "zeta/tools") })
	if _, refusal, leftOut, unread := w.repositories.lookup("acme/widgets"); !leftOut || unread || refusal != "the review App is not installed on acme/widgets" {
		t.Fatalf("acme/widgets refusal = %q (left out %v), want the App it lacks", refusal, leftOut)
	}
	reads := installs.reads("review acme/widgets")
	time.Sleep(10 * installReadRetry.Max)
	if got := installs.reads("review acme/widgets"); got != reads {
		t.Fatalf("acme/widgets read %d times more with only its 404 left, want no read again", got-reads)
	}
}

// A labelled todo root the boot's listing shows on a repository whose installation read failed is
// held as its live event is, not refused: no note is said on it, and once a read again serves the
// repository the root is admitted on it with no person's move. A root on a repository GitHub says
// lacks an App is refused at boot with its note, as before.
func TestABootListingsRootOnARepositoryWhoseReadFailedIsAdmittedOnceItIsServed(t *testing.T) {
	ctx := context.Background()
	quickInstallReads(t)
	installs := &installations{missing: map[string]bool{"review acme/widgets": true}}
	installs.setFailing(errors.New("read whether the implement App is installed on zeta/tools failed (403)"), "implement zeta/tools")
	w, client, installs, _ := mappingRigWith(t, installs)
	rigWorkflow(w, 2)
	w.bootID = "listing-boot"
	client.mu.Lock()
	client.listed = []dispatch.IssueSummary{
		{Key: "LEGION-401", ResolvedRepo: "zeta/tools", Title: "on the unread repository", Status: "todo", Rank: "A", HandedOver: true, LastSeq: 1},
		{Key: "LEGION-402", ResolvedRepo: "acme/widgets", Title: "on the repository missing an App", Status: "todo", Rank: "B", HandedOver: true, LastSeq: 1},
	}
	client.mu.Unlock()
	client.issue = dispatch.Issue{Key: "LEGION-401", ResolvedRepo: "zeta/tools", Title: "on the unread repository", Status: "todo", Rank: "A", HandedOver: true, LastSeq: 1}
	if err := w.reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := recordedIssue(t, w, "LEGION-401"); got != nil {
		t.Fatalf("LEGION-401 recorded as %+v while zeta/tools' read fails", got)
	}
	if notes := repositoryNotes(t, w, "LEGION-401"); len(notes) != 0 {
		t.Fatalf("notes on LEGION-401 = %q while it is held, want none", notes)
	}
	if notes := repositoryNotes(t, w, "LEGION-402"); len(notes) != 1 || !strings.Contains(notes[0], "the review App is not installed on acme/widgets") {
		t.Fatalf("notes on LEGION-402 = %q, want its refusal naming the App acme/widgets lacks", notes)
	}

	installs.setFailing(nil, "implement zeta/tools")
	testwait.Eventually(t, "LEGION-401 admitted on zeta/tools", func() bool {
		got := recordedIssue(t, w, "LEGION-401")
		return got != nil && got.Repo == "zeta/tools"
	})
	if notes := repositoryNotes(t, w, "LEGION-401"); len(notes) != 0 {
		t.Fatalf("notes on LEGION-401 = %q, want none: it was held, then admitted", notes)
	}
	if held := w.heldForReadReasons(); len(held) != 0 {
		t.Fatalf("issues still held = %q, want none", held)
	}
	if got := recordedIssue(t, w, "LEGION-402"); got != nil {
		t.Fatalf("LEGION-402 recorded as %+v, want it left refused", got)
	}
}

// within is whether condition holds within bound, polled every 10 ms: a bounded wait for a test
// whose old behaviour never meets the condition.
func within(bound time.Duration, condition func() bool) bool {
	for deadline := time.Now().Add(bound); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if condition() {
			return true
		}
	}
	return condition()
}

// A live admission event of an issue on a repository whose installation read failed is
// acknowledged and held in the daemon, not left on the stream: the Dispatch consumer reaches the
// position the boot's admission waits for, so a root on a served repository, which the boot's
// listing held for that position, is admitted while the failing repository still fails, and the
// held issue is admitted once GitHub answers for its repository, with no note on it. Held on the
// stream, the event kept the consumer's acknowledgement floor below the boot's target, and no
// root of the boot's listing was admitted on any repository until the failing one healed.
func TestAHeldEventIsAcknowledgedSoTheBootAdmitsRootsOnServedRepositories(t *testing.T) {
	ctx := context.Background()
	quickInstallReads(t)
	installs := &installations{}
	installs.setFailing(errors.New("read whether the implement App is installed on zeta/tools failed (403)"), "implement zeta/tools")
	w, client, installs, _, js := mappingRigJS(t, installs)
	engine, admission := rigWorkflow(w, 2)
	w.bootID, w.holdPollInterval = "held-event-boot", 10*time.Millisecond
	failing, served := dispatch.Issue{Key: "LEGION-501", ResolvedRepo: "zeta/tools", Title: "LEGION-501", Status: "todo", Rank: "A", HandedOver: true, LastSeq: 1},
		dispatch.Issue{Key: "LEGION-502", ResolvedRepo: "acme/widgets", Title: "LEGION-502", Status: "todo", Rank: "B", HandedOver: true, LastSeq: 1}
	client.mu.Lock()
	client.issues = map[string]dispatch.Issue{failing.Key: failing, served.Key: served}
	client.listed = []dispatch.IssueSummary{
		{Key: failing.Key, ResolvedRepo: failing.ResolvedRepo, Title: failing.Title, Status: "todo", Rank: "A", HandedOver: true, LastSeq: 1},
		{Key: served.Key, ResolvedRepo: served.ResolvedRepo, Title: served.Title, Status: "todo", Rank: "B", HandedOver: true, LastSeq: 1},
	}
	client.mu.Unlock()
	repo := "zeta/tools"
	if _, err := js.Publish(ctx, "notifications.dispatch.issue.LEGION-501.issue.updated",
		retainedIssueEnvelope(t, failing.Key, 501, 1, "todo", map[string]string{"kind": "user", "id": "a-person"}, &repo)); err != nil {
		t.Fatal(err)
	}
	if err := w.reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !admission.Held() {
		t.Fatal("the boot's admission holds nothing for the consumer, want the listing held for the event still on the stream")
	}
	run, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- w.consumers.Run(run, w.pool, engine, admission) }()
	polled := make(chan struct{})
	go func() { w.pollHoldRelease(run); close(polled) }()
	t.Cleanup(func() { cancel(); <-done; <-polled })

	if !within(10*time.Second, func() bool { return recordedIssue(t, w, served.Key) != nil }) {
		position, _ := w.consumers.DispatchPosition(ctx)
		t.Fatalf("%s on served acme/widgets not admitted while zeta/tools' read fails: admission held %t, consumer at %+v", served.Key, admission.Held(), position)
	}
	if got := recordedIssue(t, w, failing.Key); got != nil {
		t.Fatalf("%s recorded as %+v while zeta/tools' read fails", failing.Key, got)
	}
	if !w.repositoryHeld(failing.Key) {
		t.Fatalf("%s is not held for zeta/tools' read", failing.Key)
	}
	if notes := repositoryNotes(t, w, failing.Key); len(notes) != 0 {
		t.Fatalf("notes on %s = %q while it is held, want none", failing.Key, notes)
	}

	installs.setFailing(nil, "implement zeta/tools")
	testwait.Eventually(t, failing.Key+" admitted on zeta/tools", func() bool {
		got := recordedIssue(t, w, failing.Key)
		return got != nil && got.Repo == "zeta/tools"
	})
	if notes := repositoryNotes(t, w, failing.Key); len(notes) != 0 {
		t.Fatalf("notes on %s = %q, want none: it was held, then admitted", failing.Key, notes)
	}
}

// A served repository kept on its last answer, its installation read failing, is kept with the
// name GitHub last gave it and asks GitHub nothing more: its name read failing too, once the read
// it holds is old, fails no read of the mapping, so another repository whose installation read
// fails at boot is still served once GitHub answers for it.
func TestAServedRepositoryKeptOnItsLastAnswerHoldsNoOtherRepositoryBack(t *testing.T) {
	quickInstallReads(t)
	installs := &installations{}
	installs.setFailing(errors.New("GitHub answered 502"), "implement zeta/tools")
	github := &failingGitHub{named: map[string]string{"acme/widgets": "acme/widgets@main", "zeta/tools": "zeta/tools@main"}, only: "/acme/widgets"}
	url := github.start(t)
	w, _, installs, _ := mappingRigWith(t, installs, func(w *workflowRuntime) {
		// Every read of a repository's name asks GitHub again, as one older than its TTL does.
		w.defaultBranches = newDefaultBranches(roleTokens{}, url, 0, time.Now)
	})
	if !served(w, "acme/widgets") || served(w, "zeta/tools") {
		t.Fatalf("served %v at boot, want acme/widgets alone", w.repositories.Accepted())
	}
	github.fail.Store(true)
	installs.setFailing(errors.New("GitHub answered 502"), "review acme/widgets")
	installs.setFailing(nil, "implement zeta/tools")
	if !within(10*time.Second, func() bool { return served(w, "zeta/tools") }) {
		t.Fatalf("zeta/tools not served after %d reads while acme/widgets' reads fail, want it served", installs.reads("implement zeta/tools"))
	}
	if !served(w, "acme/widgets") {
		t.Fatalf("acme/widgets left out, want it kept on its last answer; refused %v", w.repositories.Refused())
	}
	if resolved, err := w.ResolveRepository("acme/widgets"); err != nil || resolved.Repo != "acme/widgets" || resolved.DefaultBranch != "main" {
		t.Fatalf("ResolveRepository(acme/widgets) = %#v, %v; want its last read, acme/widgets@main", resolved, err)
	}
	// An admission on the kept repository reads no default branch from GitHub: it takes the last
	// read, so it waits for no read that is failing.
	if _, err := w.prepareUnrecorded(context.Background(), intake.DispatchIssue{Key: "LEGION-801", Seq: 1, Status: "todo", Title: "kept", Rank: "A", HandedOver: true, ResolvedRepo: "acme/widgets"}, false, 0); err != nil {
		t.Fatalf("prepare an admission on kept acme/widgets = %v, want its last read with no GitHub read", err)
	}
}

// Held issues are admitted parents first, as the boot listing's are: a child under a child of a
// live tree, both held for their repository's installation read, is recorded under the tree once
// GitHub answers, whatever order their keys sort in (LEGION-100 sorts before LEGION-99). The
// workflow records a child only under a recorded parent, so a child admitted before its parent was
// recorded nowhere and never held again.
func TestHeldChildrenAreAdmittedParentsFirst(t *testing.T) {
	ctx := context.Background()
	quickInstallReads(t)
	installs := &installations{}
	installs.setFailing(errors.New("GitHub answered 502"), "implement zeta/tools")
	w, client, installs, _ := mappingRigWith(t, installs)
	engine, admission := rigWorkflow(w, 1)
	w.bootID = "parents-first-boot"
	root := dispatch.Issue{Key: "LEGION-98", ResolvedRepo: "acme/widgets", Title: "root", Status: "todo", Rank: "A", HandedOver: true, LastSeq: 1}
	parent := dispatch.Issue{Key: "LEGION-99", ResolvedRepo: "zeta/tools", Title: "parent", Status: "todo", Rank: "B", Parent: new(root.Key), LastSeq: 1}
	child := dispatch.Issue{Key: "LEGION-100", ResolvedRepo: "zeta/tools", Title: "child", Status: "todo", Rank: "C", Parent: new(parent.Key), LastSeq: 1}
	client.mu.Lock()
	client.issues = map[string]dispatch.Issue{root.Key: root, parent.Key: parent, child.Key: child}
	client.mu.Unlock()
	prepared, err := prepareAsDelivered(t, w, intake.DispatchIssue{Key: root.Key, Seq: 1, Status: "todo", Title: "root", Rank: "A", HandedOver: true, ResolvedRepo: "acme/widgets"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := intake.ApplyFact(ctx, w.pool, "test", "root-todo", prepared, engine, admission); err != nil {
		t.Fatal(err)
	}
	if got := recordedIssue(t, w, root.Key); got == nil {
		t.Fatalf("%s not admitted on acme/widgets", root.Key)
	}
	client.mu.Lock()
	client.listed = []dispatch.IssueSummary{
		{Key: child.Key, ResolvedRepo: "zeta/tools", Title: "child", Status: "todo", Rank: "C", Parent: child.Parent, LastSeq: 1},
		{Key: parent.Key, ResolvedRepo: "zeta/tools", Title: "parent", Status: "todo", Rank: "B", Parent: parent.Parent, LastSeq: 1},
	}
	client.mu.Unlock()
	if err := w.reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	for _, key := range []string{parent.Key, child.Key} {
		if got := recordedIssue(t, w, key); got != nil {
			t.Fatalf("%s recorded as %+v while zeta/tools' read fails", key, got)
		}
	}

	installs.setFailing(nil, "implement zeta/tools")
	if !within(10*time.Second, func() bool {
		got := recordedIssue(t, w, child.Key)
		return got != nil && got.Tree == root.Key
	}) {
		t.Fatalf("%s = %+v and %s = %+v once zeta/tools is served, want both recorded in %s's tree", parent.Key, recordedIssue(t, w, parent.Key), child.Key, recordedIssue(t, w, child.Key), root.Key)
	}
	if got := recordedIssue(t, w, parent.Key); got == nil || got.Tree != root.Key {
		t.Fatalf("%s = %+v, want it recorded in %s's tree", parent.Key, got, root.Key)
	}
}

// A GitHub that answers each installation read slowly, but within installReadAttempt, is answered
// for on a read of the mapping too, however long the whole check takes: only a read longer than
// installReadAttempt is a failed read. A refresh bounded as a whole by appMintAttempt cut such a
// check off, so a repository whose read failed once was never served.
func TestASlowButAnsweringGitHubIsServedOnARefresh(t *testing.T) {
	quickInstallReads(t)
	attempt, read := appMintAttempt, installReadAttempt
	appMintAttempt, installReadAttempt = 500*time.Millisecond, 2*time.Second
	t.Cleanup(func() { appMintAttempt, installReadAttempt = attempt, read })
	installs := &installations{delay: 600 * time.Millisecond}
	installs.setFailing(errors.New("GitHub answered 502"), "implement zeta/tools")
	w, _, installs, _ := mappingRigWith(t, installs)
	if served(w, "zeta/tools") {
		t.Fatal("zeta/tools served at boot while its read fails")
	}
	installs.setFailing(nil, "implement zeta/tools")
	if !within(10*time.Second, func() bool { return served(w, "zeta/tools") }) {
		t.Fatalf("zeta/tools not served after %d slow reads, want it served once GitHub answers", installs.reads("implement zeta/tools"))
	}
}

// applyTodo prepares and applies key's labelled todo event at seq on repo, as the Dispatch
// consumer does.
func applyTodo(t *testing.T, w *workflowRuntime, engine *workflow.Engine, admission *admit.Admission, key, repo string, seq int64) {
	t.Helper()
	fact := intake.DispatchIssue{Key: key, Seq: seq, Status: "todo", Title: key, Rank: "A", HandedOver: true, ResolvedRepo: repo}
	prepared, err := prepareAsDelivered(t, w, fact)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := intake.ApplyFact(context.Background(), w.pool, "test", fmt.Sprintf("%s-todo-%d", key, seq), prepared, engine, admission); err != nil {
		t.Fatal(err)
	}
}

// An issue held for its repository's installation read that a person moves out of todo and back
// while a drain reads it stays held: the drain reads the older status (backlog) and lets go only
// the hold it read, not the one the issue's new todo event made meanwhile, so the issue is admitted
// once its repository is served. A drain that let go of whatever hold the key had left the issue
// held by nothing, labelled todo in Dispatch and never admitted.
func TestAHoldMadeAgainWhileADrainReadsTheIssueOutlivesThatRead(t *testing.T) {
	quickInstallReads(t)
	installs := &installations{}
	installs.setFailing(errors.New("read whether the implement App is installed on zeta/tools failed (403)"), "implement zeta/tools")
	w, client, installs, _ := mappingRigWith(t, installs)
	engine, admission := rigWorkflow(w, 1)
	const key = "LEGION-601"
	todo := dispatch.Issue{Key: key, ResolvedRepo: "zeta/tools", Title: key, Status: "todo", Rank: "A", HandedOver: true, LastSeq: 1}
	client.mu.Lock()
	client.issues = map[string]dispatch.Issue{key: todo}
	client.mu.Unlock()
	applyTodo(t, w, engine, admission, key, "zeta/tools", 1)
	if !w.repositoryHeld(key) {
		t.Fatalf("%s not held for zeta/tools' read", key)
	}
	var bounced atomic.Bool
	client.mu.Lock()
	client.reading = map[string]func() dispatch.Issue{key: func() dispatch.Issue {
		// While the drain reads the issue, the person moves it to backlog and back to todo: the
		// drain is answered the backlog, and the todo event at sequence 3 is applied meanwhile.
		backlog := todo
		backlog.Status, backlog.LastSeq = "backlog", 2
		again := todo
		again.LastSeq = 3
		client.mu.Lock()
		client.issues[key] = again
		client.mu.Unlock()
		applyTodo(t, w, engine, admission, key, "zeta/tools", 3)
		bounced.Store(true)
		return backlog
	}}
	client.mu.Unlock()
	testwait.Eventually(t, "a drain read "+key+" while it bounced", bounced.Load)
	time.Sleep(10 * installReadRetry.Max)
	if !w.repositoryHeld(key) {
		t.Fatalf("%s let go by the drain that read it in backlog, though its todo event held it again meanwhile", key)
	}
	installs.setFailing(nil, "implement zeta/tools")
	if !within(10*time.Second, func() bool { return recordedIssue(t, w, key) != nil }) {
		t.Fatalf("%s = %+v once zeta/tools is served, held %t; want it admitted", key, recordedIssue(t, w, key), w.repositoryHeld(key))
	}
}

// An event prepared while its repository was refused for a missing App (no hold at preparation)
// whose apply, after a read of the mapping, finds the repository unread is held all the same: the
// hold is made where the fact applies, so the issue, with nothing said on it, is admitted once
// GitHub answers. Held only at preparation, the apply said nothing and held nothing, and the issue
// was never admitted.
func TestAnApplyThatFindsItsRepositoryUnreadHoldsTheIssue(t *testing.T) {
	ctx := context.Background()
	quickInstallReads(t)
	installs := &installations{missing: map[string]bool{"review zeta/tools": true}}
	w, client, installs, _ := mappingRigWith(t, installs)
	engine, admission := rigWorkflow(w, 1)
	const key = "LEGION-701"
	client.mu.Lock()
	client.issues = map[string]dispatch.Issue{key: {Key: key, ResolvedRepo: "zeta/tools", Title: key, Status: "todo", Rank: "A", HandedOver: true, LastSeq: 1}}
	client.mu.Unlock()
	prepared, err := prepareAsDelivered(t, w, intake.DispatchIssue{Key: key, Seq: 1, Status: "todo", Title: key, Rank: "A", HandedOver: true, ResolvedRepo: "zeta/tools"})
	if err != nil {
		t.Fatal(err)
	}
	installs.mu.Lock()
	delete(installs.missing, "review zeta/tools")
	installs.mu.Unlock()
	installs.setFailing(errors.New("GitHub answered 502"), "implement zeta/tools")
	read := w.requestMappingRead()
	testwait.Eventually(t, "the read of the mapping after zeta/tools' read failed", func() bool { return w.mappingApplied.Load() >= read })
	if _, _, _, unread := w.repositories.lookup("zeta/tools"); !unread {
		t.Fatal("zeta/tools is not unread after the read whose installation read failed")
	}
	if _, err := intake.ApplyFact(ctx, w.pool, "test", key+"-todo-1", prepared, engine, admission); err != nil {
		t.Fatal(err)
	}
	if got := recordedIssue(t, w, key); got != nil {
		t.Fatalf("%s recorded as %+v while zeta/tools' read fails", key, got)
	}
	if notes := repositoryNotes(t, w, key); len(notes) != 0 {
		t.Fatalf("notes on %s = %q, want none: it is held", key, notes)
	}
	if !w.repositoryHeld(key) {
		t.Fatalf("%s not held after an apply that found zeta/tools unread", key)
	}
	installs.setFailing(nil, "implement zeta/tools")
	if !within(10*time.Second, func() bool { return recordedIssue(t, w, key) != nil }) {
		t.Fatalf("%s not admitted once zeta/tools is served", key)
	}
}
