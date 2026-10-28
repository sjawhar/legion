package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
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
	engine := workflow.New(w.records, workflow.Config{Project: "LEGION", Repositories: w, NoteUnread: w.noteUnread, DesignGate: config.DesignGateOff}, quietLogger())
	w.engine, w.admission = engine, admit.New(w.records, engine, 3, "LEGION", quietLogger())
	w.handlers = []intake.Handler{w.engine, w.admission}
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

// rigHandlers is the rig's engine and admission (of three slots), wired to the rig's runtime as the
// boot wires the daemon's before the refresher starts, so w's own applies and recoveries run
// through both.
func rigHandlers(w *workflowRuntime) (*workflow.Engine, *admit.Admission) {
	return w.engine, w.admission
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
// GitHub intake hearing it, with no restart. An issue handed to Legion on it meanwhile is not
// refused and nothing is said on it; once the repository is served, the daemon recovers it from
// Dispatch's listing and admits it on it. A served repository whose read fails stays served on its
// last answer and holds nothing else back; once GitHub answers for it too, the daemon stops
// reading again.
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

	engine, admission := rigHandlers(w)
	client.issue = dispatch.Issue{Key: "LEGION-301", Status: "todo", Title: "new", Rank: "A", HandedOver: true, ResolvedRepo: "zeta/tools", LastSeq: 1}
	client.mu.Lock()
	client.listed = []dispatch.IssueSummary{{Key: "LEGION-301", Status: "todo", Title: "new", Rank: "A", HandedOver: true, ResolvedRepo: "zeta/tools", LastSeq: 1}}
	client.mu.Unlock()
	fact := intake.DispatchIssue{Key: "LEGION-301", Seq: 1, Status: "todo", Title: "new", Rank: "A", HandedOver: true, ResolvedRepo: "zeta/tools"}
	prepared, err := w.prepareRepository(ctx, fact)
	if err != nil {
		t.Fatalf("prepare LEGION-301 on zeta/tools = %v, want it prepared to be applied and acknowledged", err)
	}
	if _, err := intake.ApplyFact(ctx, w.pool, "test", "todo-1", prepared, engine, admission); err != nil {
		t.Fatal(err)
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
		t.Fatalf("notes on LEGION-301 = %q, want none: it was left, then recovered", notes)
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
// not refused: no note is said on it, and once a read again serves the repository the daemon
// recovers it from Dispatch's listing and admits it on it with no person's move. A root on a
// repository GitHub says lacks an App is refused at boot with its note, as before.
func TestABootListingsRootOnARepositoryWhoseReadFailedIsAdmittedOnceItIsServed(t *testing.T) {
	ctx := context.Background()
	quickInstallReads(t)
	installs := &installations{missing: map[string]bool{"review acme/widgets": true}}
	installs.setFailing(errors.New("read whether the implement App is installed on zeta/tools failed (403)"), "implement zeta/tools")
	w, client, installs, _ := mappingRigWith(t, installs)
	rigHandlers(w)
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
	if waiting := w.recoveringReasons(); len(waiting) != 0 {
		t.Fatalf("repositories still to recover = %q, want none", waiting)
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
// acknowledged, not left on the stream: the Dispatch consumer reaches the position the boot's
// admission waits for, so a root on a served repository, which the boot's listing held for that
// position, is admitted while the failing repository still fails, and the issue is recovered once
// GitHub answers for its repository, with no note on it. Left on the stream, the event kept the
// consumer's acknowledgement floor below the boot's target, and no root of the boot's listing was
// admitted on any repository until the failing one healed.
func TestAnUnreadEventIsAcknowledgedSoTheBootAdmitsRootsOnServedRepositories(t *testing.T) {
	ctx := context.Background()
	quickInstallReads(t)
	installs := &installations{}
	installs.setFailing(errors.New("read whether the implement App is installed on zeta/tools failed (403)"), "implement zeta/tools")
	w, client, installs, _, js := mappingRigJS(t, installs)
	engine, admission := rigHandlers(w)
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
	if notes := repositoryNotes(t, w, failing.Key); len(notes) != 0 {
		t.Fatalf("notes on %s = %q while it is held, want none", failing.Key, notes)
	}

	installs.setFailing(nil, "implement zeta/tools")
	testwait.Eventually(t, failing.Key+" admitted on zeta/tools", func() bool {
		got := recordedIssue(t, w, failing.Key)
		return got != nil && got.Repo == "zeta/tools"
	})
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

// A repository's recovery records its children parents first, as the boot listing does: a child
// under a child of a live tree, both on a repository whose read failed, is recorded under the tree
// once GitHub answers, whatever order their keys sort in (LEGION-100 sorts before LEGION-99) or
// the listing's: the workflow records a child only under a recorded parent.
func TestARecoveryRecordsChildrenParentsFirst(t *testing.T) {
	ctx := context.Background()
	quickInstallReads(t)
	installs := &installations{}
	installs.setFailing(errors.New("GitHub answered 502"), "implement zeta/tools")
	w, client, installs, _ := mappingRigWith(t, installs)
	engine, admission := rigHandlers(w)
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

// show makes Dispatch show each of issues as it is, in its issue detail and in the project's
// listing, in place of what it showed of the same key.
func (d *mappingDispatch) show(issues ...dispatch.Issue) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.issues == nil {
		d.issues = map[string]dispatch.Issue{}
	}
	for _, issue := range issues {
		d.issues[issue.Key] = issue
		summary := dispatch.IssueSummary{Key: issue.Key, Title: issue.Title, Status: issue.Status, Parent: issue.Parent, Rank: issue.Rank,
			HandedOver: issue.HandedOver, LastSeq: issue.LastSeq, ResolvedRepo: issue.ResolvedRepo}
		d.listed = slices.DeleteFunc(d.listed, func(listed dispatch.IssueSummary) bool { return listed.Key == issue.Key })
		d.listed = append(d.listed, summary)
	}
}

// deliver shows issue in Dispatch and delivers its event, at its sequence, as the Dispatch consumer
// prepares and applies one through w's handlers.
func deliver(t *testing.T, w *workflowRuntime, client *mappingDispatch, issue dispatch.Issue) {
	t.Helper()
	client.show(issue)
	fact := intake.DispatchIssue{Key: issue.Key, Seq: issue.LastSeq, Type: "issue.updated", Status: issue.Status, Title: issue.Title, Rank: issue.Rank,
		Parent: parentKey(issue.Parent), HandedOver: issue.HandedOver, ResolvedRepo: issue.ResolvedRepo}
	prepared, err := prepareAsDelivered(t, w, fact)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := intake.ApplyFact(context.Background(), w.pool, "test", fmt.Sprintf("%s-%s-%d", issue.Key, issue.Status, issue.LastSeq), prepared, w.handlers...); err != nil {
		t.Fatal(err)
	}
}

// issue is a Dispatch issue key on repo in status, labelled for Legion when handed, under parent
// ("" for none), at sequence seq.
func issue(key, repo, status string, handed bool, parent string, seq int64) dispatch.Issue {
	shown := dispatch.Issue{Key: key, ResolvedRepo: repo, Title: key, Status: status, Rank: "A" + key, HandedOver: handed, LastSeq: seq}
	if parent != "" {
		shown.Parent = new(parent)
	}
	return shown
}

// liveRoot admits key as a labelled todo root on served acme/widgets, so its tree is live.
func liveRoot(t *testing.T, w *workflowRuntime, client *mappingDispatch, key string) {
	t.Helper()
	deliver(t, w, client, issue(key, "acme/widgets", "todo", true, "", 1))
	if recordedIssue(t, w, key) == nil {
		t.Fatalf("%s not admitted on acme/widgets", key)
	}
}

// rowCount is the number of rows query counts in w's database.
func rowCount(t *testing.T, w *workflowRuntime, query string) int {
	t.Helper()
	var n int
	if err := w.pool.QueryRow(context.Background(), query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// serveAndRecover lets GitHub answer zeta/tools' implement App read again and waits until the
// daemon serves zeta/tools and has nothing left to recover.
func serveAndRecover(t *testing.T, w *workflowRuntime, installs *installations) {
	t.Helper()
	installs.setFailing(nil, "implement zeta/tools")
	testwait.Eventually(t, "zeta/tools served and recovered", func() bool {
		return served(w, "zeta/tools") && len(w.recoveringReasons()) == 0
	})
}

// A repository whose installation read fails at boot is recovered once GitHub answers for it: the
// boot's listing admits a root on a served repository while it fails, and once it is served the
// daemon takes the issues on it from Dispatch's listing once, as the boot would have had it been
// served: a labelled root admitted, a child of a live tree and that child's own child recorded
// under the tree, and a person's sub-issue under a person's epic left as it is. After that the
// daemon reads nothing more and applies nothing more, and tells the controller nothing. The
// per-issue hold kept the person's sub-issue held for good, reading and applying it at every
// read of the mapping.
func TestARepositoryUnreadAtBootIsRecoveredOnceServed(t *testing.T) {
	ctx := context.Background()
	quickInstallReads(t)
	installs := &installations{}
	installs.setFailing(errors.New("GitHub answered 502"), "implement zeta/tools")
	w, client, installs, _ := mappingRigWith(t, installs)
	rigHandlers(w)
	w.bootID = "recovery-boot"
	const tree, root, child, grandchild, epic, sub = "LEGION-1000", "LEGION-1001", "LEGION-1002", "LEGION-1003", "LEGION-1004", "LEGION-1005"
	client.show(
		issue(tree, "acme/widgets", "todo", true, "", 1),
		issue(root, "zeta/tools", "todo", true, "", 1),
		issue(child, "zeta/tools", "todo", false, tree, 1),
		issue(grandchild, "zeta/tools", "todo", false, child, 1),
		issue(epic, "zeta/tools", "in_progress", false, "", 1),
		issue(sub, "zeta/tools", "todo", false, epic, 1),
	)
	if err := w.reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if recordedIssue(t, w, tree) == nil {
		t.Fatalf("%s on served acme/widgets not admitted while zeta/tools' read fails", tree)
	}
	for _, key := range []string{root, child, grandchild, epic, sub} {
		if got := recordedIssue(t, w, key); got != nil {
			t.Fatalf("%s recorded as %+v while zeta/tools' read fails", key, got)
		}
	}
	notices := rowCount(t, w, "select count(*) from outbox where kind = 'controller_notice'")

	serveAndRecover(t, w, installs)
	if recordedIssue(t, w, root) == nil {
		t.Fatalf("%s, a labelled root on zeta/tools, not admitted once zeta/tools is served", root)
	}
	for _, key := range []string{child, grandchild} {
		if got := recordedIssue(t, w, key); got == nil || got.Tree != tree {
			t.Fatalf("%s = %+v once zeta/tools is served, want it recorded in %s's tree", key, got, tree)
		}
	}
	for _, key := range []string{epic, sub} {
		if got := recordedIssue(t, w, key); got != nil {
			t.Fatalf("%s, a person's issue, recorded as %+v, want it left as it is", key, got)
		}
	}
	listings, reads := client.listingCount(), installs.reads("implement zeta/tools")
	facts := rowCount(t, w, "select count(*) from processed_events")
	time.Sleep(10 * installReadRetry.Max)
	if got := client.listingCount(); got != listings {
		t.Fatalf("Dispatch listed %d times more once recovered, want none", got-listings)
	}
	if got := installs.reads("implement zeta/tools"); got != reads {
		t.Fatalf("zeta/tools read %d times more once recovered, want none", got-reads)
	}
	if got := rowCount(t, w, "select count(*) from processed_events"); got != facts {
		t.Fatalf("%d more facts applied once recovered, want none", got-facts)
	}
	if got := rowCount(t, w, "select count(*) from outbox where kind = 'controller_notice'"); got != notices {
		t.Fatalf("%d controller notices queued by the recovery, want none", got-notices)
	}
}

// Events applied while a repository's read fails say nothing and keep nothing, and the recovery
// once it is served ends each issue where its events would have, had it been served: a root handed
// over during the outage is admitted; one moved back to backlog during it is not, and its later
// move to todo admits it once; one whose event was prepared while GitHub said an App was missing
// and applied after the read failed is admitted too.
func TestEventsDuringAnOutageEndWhereTheyWouldHaveServed(t *testing.T) {
	quickInstallReads(t)
	installs := &installations{missing: map[string]bool{"review zeta/tools": true}}
	w, client, installs, _ := mappingRigWith(t, installs)
	rigHandlers(w)
	const handed, withdrawn, prepared404 = "LEGION-1100", "LEGION-1101", "LEGION-1102"
	z := issue(prepared404, "zeta/tools", "todo", true, "", 1)
	client.show(z)
	prepared, err := prepareAsDelivered(t, w, intake.DispatchIssue{Key: z.Key, Seq: 1, Status: "todo", Title: z.Title, Rank: z.Rank, HandedOver: true, ResolvedRepo: "zeta/tools"})
	if err != nil {
		t.Fatal(err)
	}
	installs.mu.Lock()
	delete(installs.missing, "review zeta/tools")
	installs.mu.Unlock()
	installs.setFailing(errors.New("GitHub answered 502"), "implement zeta/tools")
	read := w.requestMappingRead()
	testwait.Eventually(t, "the read that finds zeta/tools unread", func() bool { return w.mappingApplied.Load() >= read })
	if _, _, _, unread := w.repositories.lookup("zeta/tools"); !unread {
		t.Fatal("zeta/tools is not unread after the read whose installation read failed")
	}
	if _, err := intake.ApplyFact(context.Background(), w.pool, "test", z.Key+"-todo-1", prepared, w.handlers...); err != nil {
		t.Fatal(err)
	}
	deliver(t, w, client, issue(handed, "zeta/tools", "todo", true, "", 1))
	deliver(t, w, client, issue(withdrawn, "zeta/tools", "todo", true, "", 1))
	deliver(t, w, client, issue(withdrawn, "zeta/tools", "backlog", true, "", 2))
	for _, key := range []string{handed, withdrawn, prepared404} {
		if got := recordedIssue(t, w, key); got != nil {
			t.Fatalf("%s recorded as %+v while zeta/tools' read fails", key, got)
		}
	}

	serveAndRecover(t, w, installs)
	for _, key := range []string{handed, prepared404} {
		if recordedIssue(t, w, key) == nil {
			t.Fatalf("%s not admitted once zeta/tools is served", key)
		}
	}
	if got := recordedIssue(t, w, withdrawn); got != nil {
		t.Fatalf("%s, moved to backlog during the outage, recorded as %+v", withdrawn, got)
	}
	deliver(t, w, client, issue(withdrawn, "zeta/tools", "todo", true, "", 3))
	if got := recordedIssue(t, w, withdrawn); got == nil || got.LastDispatchSeq != 3 {
		t.Fatalf("%s = %+v after its move to todo, want it admitted at that move", withdrawn, got)
	}
	for _, key := range []string{handed, withdrawn, prepared404} {
		if notes := repositoryNotes(t, w, key); len(notes) != 0 {
			t.Fatalf("notes on %s = %q, want none", key, notes)
		}
	}
}

// A child on a served repository under a parent on a repository whose read fails, the parent a todo
// child of a live tree, is recovered with its parent: the child's own event recorded nothing, its
// parent then unrecorded, and the recovery records both, parent first. The per-issue hold held
// only the parent, and the child, already todo in Dispatch, was never recorded.
func TestAChildOnAServedRepositoryUnderAnUnreadParentIsRecovered(t *testing.T) {
	quickInstallReads(t)
	installs := &installations{}
	installs.setFailing(errors.New("GitHub answered 502"), "implement zeta/tools")
	w, client, installs, _ := mappingRigWith(t, installs)
	rigHandlers(w)
	const root, parent, child = "LEGION-1200", "LEGION-1201", "LEGION-1202"
	liveRoot(t, w, client, root)
	deliver(t, w, client, issue(parent, "zeta/tools", "todo", false, root, 1))
	deliver(t, w, client, issue(child, "acme/widgets", "todo", false, parent, 1))
	for _, key := range []string{parent, child} {
		if got := recordedIssue(t, w, key); got != nil {
			t.Fatalf("%s recorded as %+v while zeta/tools' read fails", key, got)
		}
	}
	serveAndRecover(t, w, installs)
	for _, key := range []string{parent, child} {
		if got := recordedIssue(t, w, key); got == nil || got.Tree != root {
			t.Fatalf("%s = %+v once zeta/tools is served, want it recorded in %s's tree", key, got, root)
		}
	}
}

// A recovery whose read of Dispatch's listing fails stays to recover, and is tried again on the
// refresher's backoff: the next listing records the issue, and none is read after it.
func TestARecoveryWhoseListingFailsIsTriedAgain(t *testing.T) {
	quickInstallReads(t)
	installs := &installations{}
	installs.setFailing(errors.New("GitHub answered 502"), "implement zeta/tools")
	w, client, installs, _ := mappingRigWith(t, installs)
	rigHandlers(w)
	const key = "LEGION-1300"
	deliver(t, w, client, issue(key, "zeta/tools", "todo", true, "", 1))
	client.mu.Lock()
	client.listFailures = 1
	client.mu.Unlock()
	serveAndRecover(t, w, installs)
	if recordedIssue(t, w, key) == nil {
		t.Fatalf("%s not admitted by the recovery's second listing", key)
	}
	time.Sleep(10 * installReadRetry.Max)
	if got := client.listingCount(); got != 2 {
		t.Fatalf("Dispatch listed %d times, want two: the failed listing and the one that recovered", got)
	}
}

// A recovery while the Dispatch consumer is behind the stream holds its listing as the boot's is,
// and admits the root once the consumer catches up, once; a second recovery in the same boot, on a
// stream the consumer has caught up with, admits at once.
func TestARecoveryWhileTheConsumerIsBehindAdmitsOnceItCatchesUp(t *testing.T) {
	ctx := context.Background()
	quickInstallReads(t)
	installs := &installations{}
	installs.setFailing(errors.New("GitHub answered 502"), "implement zeta/tools")
	w, client, installs, _, js := mappingRigJS(t, installs)
	engine, admission := rigHandlers(w)
	w.bootID, w.holdPollInterval = "behind-boot", 10*time.Millisecond
	const first, second, unrelated = "LEGION-1400", "LEGION-1401", "LEGION-1499"
	client.show(issue(first, "zeta/tools", "todo", true, "", 1), issue(unrelated, "acme/widgets", "backlog", false, "", 1))
	repo := "acme/widgets"
	if _, err := js.Publish(ctx, "notifications.dispatch.issue."+unrelated+".issue.updated",
		retainedIssueEnvelope(t, unrelated, 1499, 1, "backlog", map[string]string{"kind": "user", "id": "a-person"}, &repo)); err != nil {
		t.Fatal(err)
	}
	serveAndRecover(t, w, installs)
	if !admission.Held() || recordedIssue(t, w, first) != nil {
		t.Fatalf("admission held %t, %s recorded %t; want the recovery held for the consumer behind the stream", admission.Held(), first, recordedIssue(t, w, first) != nil)
	}
	run, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- w.consumers.Run(run, w.pool, engine, admission) }()
	polled := make(chan struct{})
	go func() { w.pollHoldRelease(run); close(polled) }()
	t.Cleanup(func() { cancel(); <-done; <-polled })
	testwait.Eventually(t, first+" admitted once the consumer caught up", func() bool { return recordedIssue(t, w, first) != nil })
	if n := rowCount(t, w, "select count(*) from slots where issue = '"+first+"'"); n != 1 {
		t.Fatalf("%s holds %d slots, want one", first, n)
	}

	// A served repository whose read fails is kept on its last answer, so the second outage is
	// zeta/tools mapped again after a person unmapped it, while GitHub fails its read.
	readMappingNow := func(what string, done func() bool) {
		t.Helper()
		read := w.requestMappingRead()
		testwait.Eventually(t, what, func() bool { return w.mappingApplied.Load() >= read && done() })
	}
	client.set(dispatch.RepoProject{Repo: "acme/widgets", Project: "LEGION", Default: true})
	readMappingNow("zeta/tools unmapped", func() bool {
		_, refusal, leftOut, _ := w.repositories.lookup("zeta/tools")
		return refusal != "" && !leftOut
	})
	installs.setFailing(errors.New("GitHub answered 502"), "implement zeta/tools")
	client.set(dispatch.RepoProject{Repo: "acme/widgets", Project: "LEGION", Default: true}, dispatch.RepoProject{Repo: "zeta/tools", Project: "LEGION"})
	readMappingNow("zeta/tools mapped again and unread", func() bool { _, _, _, unread := w.repositories.lookup("zeta/tools"); return unread })
	client.show(issue(second, "zeta/tools", "todo", true, "", 1))
	serveAndRecover(t, w, installs)
	testwait.Eventually(t, second+" admitted by the second recovery", func() bool { return recordedIssue(t, w, second) != nil })
}

// The refresher reads the mapping again of its own while a repository's installation read fails, 30
// s doubling to 10 min, and no more; once nothing waits it reads nothing of its own, and the next
// failed read starts again at 30 s.
func TestTheUnreadRepositoryBackoffDoublesToItsCapAndResets(t *testing.T) {
	var mu sync.Mutex
	var waits []time.Duration
	var spin atomic.Bool
	spin.Store(true)
	fired := make(chan time.Time)
	close(fired)
	installs := &installations{}
	installs.setFailing(errors.New("GitHub answered 502"), "implement zeta/tools")
	w, _, installs, _ := mappingRigWith(t, installs, func(w *workflowRuntime) {
		w.recheckAfter = func(d time.Duration) <-chan time.Time {
			mu.Lock()
			waits = append(waits, d)
			mu.Unlock()
			if spin.Load() {
				return fired
			}
			return nil
		}
	})
	rigHandlers(w)
	recorded := func() []time.Duration {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(waits)
	}
	testwait.Eventually(t, "eight waits", func() bool { return len(recorded()) >= 8 })
	spin.Store(false)
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 10 * time.Minute, 10 * time.Minute, 10 * time.Minute}
	if got := recorded()[:8]; !slices.Equal(got, want) {
		t.Fatalf("waits = %v, want %v", got, want)
	}

	installs.setFailing(nil, "implement zeta/tools")
	w.requestMappingRead()
	testwait.Eventually(t, "zeta/tools served and recovered", func() bool { return served(w, "zeta/tools") && len(w.recoveringReasons()) == 0 })
	time.Sleep(100 * time.Millisecond)
	idle := len(recorded())
	installs.setFailing(errors.New("GitHub answered 502"), "implement zeta/tools")
	w.requestMappingRead()
	testwait.Eventually(t, "a wait after zeta/tools' read fails again", func() bool { return len(recorded()) > idle })
	if got := recorded()[idle]; got != 30*time.Second {
		t.Fatalf("the first wait after the read failed again = %s, want 30s", got)
	}
}

// treeNotices is the reason of every notice queued on tree, in queue order.
func treeNotices(t *testing.T, w *workflowRuntime, tree string) []string {
	t.Helper()
	rows, err := w.pool.Query(context.Background(), "select payload::text from outbox where issue = $1 and kind = 'notice' order by id", tree)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var notices []string
	for rows.Next() {
		var notice string
		if err := rows.Scan(&notice); err != nil {
			t.Fatal(err)
		}
		notices = append(notices, notice)
	}
	return notices
}

// A labelled todo root whose event the daemon applied silently while its repository was unread is
// recovered even when Dispatch resolves it to another repository by the time GitHub answers (it
// took its repository from the project's default, or from a parent, and that changed, with no
// event of its own): the event's silence is noted with the repository it was silent for, and that
// repository's recovery takes the issue in whatever it resolves to. Scoped by its resolution at
// the recovery alone, it waited for the next boot.
func TestAnIssueSilentForAnUnreadRepositoryIsRecoveredWhereverItResolvesThen(t *testing.T) {
	quickInstallReads(t)
	installs := &installations{}
	installs.setFailing(errors.New("GitHub answered 502"), "implement zeta/tools")
	w, client, installs, _ := mappingRigWith(t, installs)
	const key = "LEGION-1500"
	deliver(t, w, client, issue(key, "zeta/tools", "todo", true, "", 1))
	if recordedIssue(t, w, key) != nil {
		t.Fatalf("%s recorded while zeta/tools' read fails", key)
	}
	// The project's default moves to acme/widgets: Dispatch resolves the issue there now, and sends
	// no event of it.
	moved := issue(key, "acme/widgets", "todo", true, "", 1)
	client.show(moved)
	serveAndRecover(t, w, installs)
	if got := recordedIssue(t, w, key); got == nil || got.Repo != "acme/widgets" {
		t.Fatalf("%s = %+v once zeta/tools is recovered, want it admitted on acme/widgets, where it resolves now", key, got)
	}
}

// A repository's recovery tells each tree's architect, once, which children its release_children
// left for the repository the recovery still did not record, with what to release: here C, todo
// under a backlog parent P, which the recovery records no child under. A child the recovery
// recorded, E, is not named. Before, such a child stranded silently.
func TestARecoveryTellsTheArchitectOfTheChildrenItDidNotRecord(t *testing.T) {
	quickInstallReads(t)
	installs := &installations{}
	installs.setFailing(errors.New("GitHub answered 502"), "implement zeta/tools")
	w, client, installs, _ := mappingRigWith(t, installs)
	const tree, p, c, e = "LEGION-1600", "LEGION-1601", "LEGION-1602", "LEGION-1603"
	liveRoot(t, w, client, tree)
	client.show(issue(p, "zeta/tools", "backlog", false, tree, 1), issue(c, "zeta/tools", "todo", false, p, 1), issue(e, "zeta/tools", "todo", false, tree, 1))
	w.awaitRecovery("zeta/tools", tree, c, p)
	w.awaitRecovery("zeta/tools", tree, e, tree)
	before := len(treeNotices(t, w, tree))
	serveAndRecover(t, w, installs)
	if got := recordedIssue(t, w, e); got == nil || got.Tree != tree {
		t.Fatalf("%s = %+v, want the recovery to record it in %s's tree", e, got, tree)
	}
	if got := recordedIssue(t, w, c); got != nil {
		t.Fatalf("%s recorded as %+v under backlog %s", c, got, p)
	}
	notices := treeNotices(t, w, tree)[before:]
	if len(notices) != 1 || !strings.Contains(notices[0], "child-release-stalled") ||
		!strings.Contains(notices[0], c+" (its parent "+p+" is not recorded: release "+p+" first, then "+c+")") || strings.Contains(notices[0], e) {
		t.Fatalf("notices to %s's architect after the recovery = %q, want one naming %s and %s, not %s", tree, notices, c, p, e)
	}
	time.Sleep(10 * installReadRetry.Max)
	if got := treeNotices(t, w, tree)[before:]; len(got) != 1 {
		t.Fatalf("notices after the recovery = %d, want the one only", len(got))
	}
}

// A child release_children left for unread zeta/tools under a parent todo on served acme/widgets
// that the daemon had not recorded yet is recorded with its parent once both happen: the parent's
// own event records the parent, the recovery records the child, and the architect is told
// nothing, since nothing is left unrecorded.
func TestARecoveryTellsNothingOnceAChildUnderALaggingTodoParentIsRecorded(t *testing.T) {
	quickInstallReads(t)
	installs := &installations{}
	installs.setFailing(errors.New("GitHub answered 502"), "implement zeta/tools")
	w, client, installs, _ := mappingRigWith(t, installs)
	const tree, parent, child = "LEGION-1700", "LEGION-1701", "LEGION-1702"
	liveRoot(t, w, client, tree)
	client.show(issue(child, "zeta/tools", "todo", false, parent, 1))
	w.awaitRecovery("zeta/tools", tree, child, parent)
	deliver(t, w, client, issue(parent, "acme/widgets", "todo", false, tree, 1))
	before := len(treeNotices(t, w, tree))
	serveAndRecover(t, w, installs)
	for _, key := range []string{parent, child} {
		if got := recordedIssue(t, w, key); got == nil || got.Tree != tree {
			t.Fatalf("%s = %+v once zeta/tools is recovered, want it recorded in %s's tree", key, got, tree)
		}
	}
	time.Sleep(10 * installReadRetry.Max)
	if got := treeNotices(t, w, tree)[before:]; len(got) != 0 {
		t.Fatalf("notices after the recovery = %q, want none: nothing is left unrecorded", got)
	}
}

// childEnvelope is the Dispatch todo event of key, a child of parent on repo, at seq, as the
// Dispatch consumer reads it off the stream.
func childEnvelope(t *testing.T, key, parent, repo string, id, seq int64) []byte {
	t.Helper()
	issue := map[string]any{"key": key, "status": "todo", "title": key, "rank": "A", "parent": parent, "labels": []string{}, "resolved_repo": repo}
	event, err := json.Marshal(map[string]any{"id": id, "issue_key": key, "seq": seq, "type": "issue.updated", "actor": map[string]string{"kind": "user", "id": "a-person"}, "notify": true, "payload": issue})
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := json.Marshal(map[string]any{"event_id": fmt.Sprintf("dispatch-%s-%d", key, seq), "source": "dispatch", "source_event_id": fmt.Sprint(id), "topic": "issue",
		"dedupe_key": fmt.Sprintf("dispatch-%s-%d", key, seq), "issued_at": 1, "payload_summary": "issue updated", "payload": string(event), "trace_id": "child"})
	if err != nil {
		t.Fatal(err)
	}
	return envelope
}

// A recovery whose listing stays held, here because the todo parent's own event waits on a paused
// Dispatch consumer and keeps it below the stream's last position, tells the architect of the child
// its release left under that parent once the hold has lasted recoveryHoldBound, and only once;
// when the consumer runs, the parent's event records the parent, the hold releases, the child is
// recorded, and nothing more is told. Before, the notice waited for the hold to release, so while
// the hold lasted the child was neither recorded nor named.
func TestARecoveryHeldPastItsBoundTellsItsReleaseWaitsOnce(t *testing.T) {
	h := newHeldRecovery(t, 40*time.Millisecond, 300*time.Millisecond)
	before := h.notices(t)
	h.behind(t, heldParent)
	h.serve()
	testwait.Eventually(t, "the recovery's listing held", func() bool { return served(h.w, "zeta/tools") && h.admission.Held() })
	testwait.Eventually(t, "the release wait told past the bound", func() bool { return h.notices(t) > before })
	notices := treeNotices(t, h.w, heldTree)[before:]
	if len(notices) != 1 || !strings.Contains(notices[0], heldChild+" (its parent "+heldParent+" is not recorded: release "+heldParent+" first, then "+heldChild+")") {
		t.Fatalf("notices once the hold outlasted its bound = %q, want one naming %s and %s", notices, heldChild, heldParent)
	}
	time.Sleep(4 * recoveryHoldBound)
	if n := h.notices(t) - before; n != 1 || !h.admission.Held() {
		t.Fatalf("notices = %d with the hold still %t, want the one only while the hold lasts", n, h.admission.Held())
	}

	h.consume()
	testwait.Eventually(t, "parent and child recorded once the consumer catches up", func() bool {
		p, c := recordedIssue(t, h.w, heldParent), recordedIssue(t, h.w, heldChild)
		return p != nil && p.Tree == heldTree && c != nil && c.Tree == heldTree
	})
	testwait.Eventually(t, "nothing left to recover", func() bool { return len(h.w.recoveringReasons()) == 0 })
	time.Sleep(4 * recoveryHoldBound)
	if n := h.notices(t) - before; n != 1 {
		t.Fatalf("notices after the hold released = %d, want the one only", n)
	}
}

// heldRecovery is a rig whose unread zeta/tools left the child C, under P (todo on acme/widgets,
// never recorded, its event never sent), of the live tree T to the recovery. The boot's hold
// poller runs throughout; the Dispatch consumer runs only between consume and pause, so an event
// published while it is paused keeps it behind the stream and holds the recovery's listing, and
// its run catches it up and releases the hold. (An apply that fails naks its event, and between
// redeliveries the consumer reads as idle, which would release the hold at random; one that
// blocks would hold the fact lock the recovery's listing takes.)
type heldRecovery struct {
	w          *workflowRuntime
	client     *mappingDispatch
	js         jetstream.JetStream
	installs   *installations
	admission  *admit.Admission
	cancel     context.CancelFunc
	done       chan error
	seq        int64
	failNotice atomic.Bool
}

// noticeFailingStore is the rig's record store, its Enqueue failing for a notice while fail is set:
// the write of a recovery's notice fails, as a database refusing it would.
type noticeFailingStore struct {
	record.Store
	fail *atomic.Bool
}

func (s noticeFailingStore) Enqueue(ctx context.Context, tx pgx.Tx, row record.OutboxRow) error {
	if s.fail.Load() && row.Kind == "notice" {
		return errors.New("the notice's write fails")
	}
	return s.Store.Enqueue(ctx, tx, row)
}

const heldTree, heldParent, heldChild = "LEGION-1900", "LEGION-1901", "LEGION-1902"

// newHeldRecovery opens the rig with installReadRetry at backoff (initial and cap) and
// recoveryHoldBound at bound, the consumer paused. The hold poller reads the consumer's position,
// or reader's when one is given.
func newHeldRecovery(t *testing.T, backoff, bound time.Duration, reader ...positionReader) *heldRecovery {
	t.Helper()
	quickInstallReads(t)
	installReadRetry.Initial, installReadRetry.Max = backoff, backoff
	was := recoveryHoldBound
	recoveryHoldBound = bound
	t.Cleanup(func() { recoveryHoldBound = was })
	h := &heldRecovery{installs: &installations{}}
	h.installs.setFailing(errors.New("GitHub answered 502"), "implement zeta/tools")
	h.w, h.client, h.installs, _, h.js = mappingRigJS(t, h.installs)
	_, h.admission = rigHandlers(h.w)
	h.w.records = noticeFailingStore{Store: h.w.records, fail: &h.failNotice}
	h.w.bootID, h.w.holdPollInterval = "held-clock-boot", 10*time.Millisecond
	liveRoot(t, h.w, h.client, heldTree)
	h.client.show(issue(heldParent, "acme/widgets", "todo", false, heldTree, 1), issue(heldChild, "zeta/tools", "todo", false, heldParent, 1))
	h.w.awaitRecovery("zeta/tools", heldTree, heldChild, heldParent)
	var poll positionReader = h.w.consumers
	if len(reader) != 0 {
		poll = reader[0]
	}
	run, cancel := context.WithCancel(context.Background())
	polled := make(chan struct{})
	go func() { h.w.pollHoldReleaseWith(run, poll); close(polled) }()
	t.Cleanup(func() { h.pause(); cancel(); <-polled })
	return h
}

// behind publishes a todo event of key, a child of T on acme/widgets, for the paused consumer.
func (h *heldRecovery) behind(t *testing.T, key string) {
	t.Helper()
	h.seq++
	h.client.show(issue(key, "acme/widgets", "todo", false, heldTree, 1))
	if _, err := h.js.Publish(context.Background(), "notifications.dispatch.issue."+key+".issue.updated", childEnvelope(t, key, heldTree, "acme/widgets", 2000+h.seq, 1)); err != nil {
		t.Fatal(err)
	}
}

// consume runs the Dispatch consumer until pause.
func (h *heldRecovery) consume() {
	run, cancel := context.WithCancel(context.Background())
	h.cancel, h.done = cancel, make(chan error, 1)
	go func() { h.done <- h.w.consumers.Run(run, h.w.pool, h.w.handlers...) }()
}

func (h *heldRecovery) pause() {
	if h.cancel != nil {
		h.cancel()
		<-h.done
		h.cancel = nil
	}
}

// serve lets GitHub answer zeta/tools and asks for a read of the mapping at once.
func (h *heldRecovery) serve() {
	h.installs.setFailing(nil, "implement zeta/tools")
	h.w.requestMappingRead()
}

func (h *heldRecovery) notices(t *testing.T) int {
	t.Helper()
	return len(treeNotices(t, h.w, heldTree))
}

// A recovery held with release waits is told at its bound, not at the refresher's next backoff:
// with the backoff's cap at 2 s and the bound at 500 ms, the notice comes about 500 ms after the
// hold begins. The refresher cut its wait to the time left to the bound; before, the bound was
// checked only when the backoff next fired, up to its cap later.
func TestARecoveryHeldWithReleaseWaitsIsToldAtItsBoundNotItsBackoff(t *testing.T) {
	h := newHeldRecovery(t, 2*time.Second, 500*time.Millisecond)
	before := h.notices(t)
	h.behind(t, "LEGION-1950")
	h.serve()
	testwait.Eventually(t, "the recovery's listing held", func() bool { return served(h.w, "zeta/tools") && h.admission.Held() })
	start := time.Now()
	if !within(1500*time.Millisecond, func() bool { return h.notices(t) > before }) {
		t.Fatalf("no notice %s after the hold began, want one at about the 500ms bound, before the 2s backoff", time.Since(start))
	}
	if n := h.notices(t) - before; n != 1 {
		t.Fatalf("notices = %d, want one", n)
	}
}

// A hold that flaps (held at each recovery run, released between them) is told at the bound all
// the same: the clock runs from the first held run, so the notice comes once, at about the bound.
// Here the consumer stays behind the stream, so each run's listing is held, and the hold poller
// reads a position that has reached it, so each hold is released before the next run. A clock
// that started anew at each hold released and made again never reached the bound.
func TestARecoveryWhoseHoldFlapsIsToldOnceAtTheBound(t *testing.T) {
	caughtUp := &failingPositionReader{position: intake.DispatchConsumerPosition{AckFloorStream: 1 << 40, Idle: true}}
	h := newHeldRecovery(t, 100*time.Millisecond, time.Second, caughtUp)
	before := h.notices(t)
	h.behind(t, "LEGION-1960")
	h.serve()
	testwait.Eventually(t, "zeta/tools served", func() bool { return served(h.w, "zeta/tools") })
	start := time.Now()
	// Each run's hold is released at the next poll, so the flap is seen as Held turning false
	// between runs that keep listing.
	listings := h.client.listingCount()
	testwait.Eventually(t, "the recovery's listing held and released at several runs", func() bool { return h.client.listingCount() >= listings+3 })
	if !within(3*time.Second, func() bool { return h.notices(t) > before }) {
		t.Fatalf("no notice %s after the flapping hold began, want one at about the 1s bound", time.Since(start))
	}
	time.Sleep(500 * time.Millisecond)
	if n := h.notices(t) - before; n != 1 {
		t.Fatalf("notices = %d, want one", n)
	}
}

// failPastTheBound holds the recovery's listing, lets the bound pass with fault set, and then
// checks the refresher waits its backoff (1 s here) between runs, not a busy loop: in 2 s, fewer
// than five Dispatch listings and five installation reads of zeta/tools. It then clears the fault
// with clear, and the notice comes once.
func failPastTheBound(t *testing.T, fault, clear func(h *heldRecovery)) {
	t.Helper()
	h := newHeldRecovery(t, time.Second, 300*time.Millisecond)
	before := h.notices(t)
	h.behind(t, "LEGION-1970")
	h.serve()
	testwait.Eventually(t, "the recovery's listing held", func() bool { return served(h.w, "zeta/tools") && h.admission.Held() })
	fault(h)
	time.Sleep(2 * recoveryHoldBound)
	listings, reads := h.client.listingCount(), h.installs.reads("implement zeta/tools")
	time.Sleep(2 * time.Second)
	if got, read := h.client.listingCount()-listings, h.installs.reads("implement zeta/tools")-reads; got >= 5 || read >= 5 {
		t.Fatalf("in 2 s past the bound: %d Dispatch listings and %d installation reads of zeta/tools, want fewer than 5 each at a 1 s backoff", got, read)
	}
	if n := h.notices(t) - before; n != 0 {
		t.Fatalf("notices while the fault lasts = %d, want none", n)
	}
	clear(h)
	if !within(4*time.Second, func() bool { return h.notices(t) > before }) {
		t.Fatal("no notice once the fault cleared")
	}
	time.Sleep(500 * time.Millisecond)
	if n := h.notices(t) - before; n != 1 {
		t.Fatalf("notices = %d, want one", n)
	}
}

// A held recovery whose Dispatch listing keeps failing once its bound has passed waits the
// refresher's backoff between runs, and tells its waits once the listing answers. Before, the wait
// cut to the bound's zero time left made every failed run re-run at once.
func TestARecoveryWhoseListingFailsPastItsBoundWaitsItsBackoff(t *testing.T) {
	failPastTheBound(t, func(h *heldRecovery) {
		h.client.mu.Lock()
		h.client.listFailures = 1 << 30
		h.client.mu.Unlock()
	}, func(h *heldRecovery) {
		h.client.mu.Lock()
		h.client.listFailures = 0
		h.client.mu.Unlock()
	})
}

// The same when the write of the recovery's notice keeps failing past the bound.
func TestARecoveryWhoseNoticeFailsPastItsBoundWaitsItsBackoff(t *testing.T) {
	failPastTheBound(t, func(h *heldRecovery) { h.failNotice.Store(true) }, func(h *heldRecovery) { h.failNotice.Store(false) })
}
