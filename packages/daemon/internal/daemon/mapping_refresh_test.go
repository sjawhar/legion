package daemon

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
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
		repositories: newRepositories("LEGION"), defaultBranches: newDefaultBranches(roleTokens{}, github.url, defaultBranchTTL, time.Now), log: quietLogger()}
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
	return w, client, installs, stream
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

// A refresh publishes a repository as served only once GitHub intake hears it: a refresh that
// fails before its subscription update leaves the repository left out, so the issue that came up
// for admission is not recorded on it, and the next admission recheck refreshes again. An issue
// is never recorded on a repository GitHub intake does not hear.
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
			w, _, installs, stream := mappingRig(t, map[string]bool{"review zeta/tools": true})
			installs.missing["review zeta/tools"] = false
			w.records = &faultyStore{Store: w.records, failIssues: tc.fail}
			fact := intake.DispatchIssue{Key: "LEGION-301", Seq: 1, Status: "todo", Title: "new", Rank: "A", HandedOver: true, ResolvedRepo: "zeta/tools"}
			_, err := w.prepareRepository(ctx, fact)
			if tc.fail {
				if err == nil {
					t.Fatal("the failed snapshot did not fail the preparation")
				}
				if _, _, leftOut := w.repositories.lookup("zeta/tools"); !leftOut {
					t.Fatal("the failed refresh published zeta/tools as served")
				}
				if filters := githubFilterSubjects(t, stream); slices.Contains(filters, "notifications.github.zeta.tools.>") {
					t.Fatalf("GitHub filters = %v after a failed refresh, want zeta/tools not yet heard", filters)
				}
				if _, err = w.prepareRepository(ctx, fact); err != nil {
					t.Fatalf("the retried preparation: %v", err)
				}
			} else if err != nil {
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
// live records, so the repository it recorded stays heard. A refresh never unsubscribes a
// repository an admission is about to commit an issue on.
func TestARefreshThatUnmapsARepositoryKeepsHearingAnIssueAdmittedOnIt(t *testing.T) {
	ctx := context.Background()
	w, client, _, stream := mappingRig(t, map[string]bool{})
	if err := pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
		if err := w.records.PutIssue(ctx, tx, record.Issue{Key: "LEGION-300", Tree: "LEGION-300", Project: "LEGION", Title: "root", Phase: phase.Planning,
			Generation: 1, Status: "in_progress", Rank: "A", Repo: "acme/widgets", DefaultBranch: "main"}); err != nil {
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

// GitHub intake hears a repository as GitHub spells it, which Envoy's subjects carry, whatever
// spelling an issue's record keeps: a record the backfill filled with a clone's spelling is heard
// once, under the served repository's GitHub spelling, and a live record on a repository the
// mapping no longer serves is heard under the spelling GitHub last named for it.
func TestGitHubIntakeHearsEachRepositoryAsGitHubSpellsIt(t *testing.T) {
	ctx := context.Background()
	w, _, _, _ := mappingRig(t, map[string]bool{})
	github := newNamedGitHub(t, map[string]string{"orbit/gadgets": "Orbit/Gadgets@main"})
	w.defaultBranches = newDefaultBranches(roleTokens{}, github.url, defaultBranchTTL, time.Now)
	if _, err := w.defaultBranches.Named(ctx, ghrepo.MustParse("orbit/gadgets")); err != nil {
		t.Fatal(err)
	}
	if err := pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
		for index, issue := range []record.Issue{
			{Key: "LEGION-300", Repo: "ACME/widgets", DefaultBranch: "main"},
			{Key: "LEGION-301", Repo: "orbit/gadgets", DefaultBranch: "main"},
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
// on it. Once the App is installed, the issue's next admission recheck serves the repository and
// records the issue on it, which GitHub intake then hears.
func TestAProjectServingNoRepositoryStartsServingOnceItsAppIsInstalled(t *testing.T) {
	ctx := context.Background()
	w, _, installs, stream := mappingRig(t, map[string]bool{"review acme/widgets": true, "review zeta/tools": true})
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
		prepared, err := w.prepareRepository(ctx, fact)
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
