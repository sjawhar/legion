package daemon

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// A live issue whose record keeps a shared clone's spelling of its repository (acme/widgets) is
// heard after a restart in GitHub's spelling (Acme/Widgets), which Envoy's subjects carry and NATS
// matches case-sensitively, even once the mapping no longer names the repository: the record keeps
// GitHub's spelling beside its own, so the boot subscribes from the database, not from a read a
// restart forgets. The workspace keeps the clone's spelling.
func TestARestartStillHearsAnUnmappedRepositoryInGitHubsSpelling(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := projectRecords{Store: record.NewStore(), project: "LEGION"}
	if _, err := pool.Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('LEGION-301', 'LEGION-301', 'LEGION', 'running', 'implementing', 1, 'in_progress', 'A', 1)`); err != nil {
		t.Fatal(err)
	}
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		return records.PutSlot(ctx, tx, record.Slot{Issue: "LEGION-301", Index: 0, AdmittedAt: time.Now()})
	}); err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	clone, err := workspace.Location(state, ghrepo.MustParse("acme/widgets"), "LEGION-301")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(clone.Clone, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	github := newNamedGitHub(t, map[string]string{"acme/widgets": "Acme/Widgets@main", "zeta/tools": "zeta/tools@main"})

	// The upgrade's boot fills the record from Dispatch and GitHub.
	client := &listingDispatch{summaries: []dispatch.IssueSummary{{Key: "LEGION-301", ResolvedRepo: "acme/widgets"}}}
	if err := backfillRepositories(ctx, pool, records, client, "LEGION", state,
		newDefaultBranches(roleTokens{}, github.url, defaultBranchTTL, time.Now), quietLogger()); err != nil {
		t.Fatal(err)
	}

	// The next boot: the mapping serves zeta/tools alone, and this daemon has read nothing of GitHub.
	restarted := &workflowRuntime{pool: pool, records: records, repositories: newRepositories("LEGION"),
		defaultBranches: newDefaultBranches(roleTokens{}, github.url, defaultBranchTTL, time.Now), log: quietLogger()}
	restarted.repositories.set([]mappedRepository{{Repo: ghrepo.MustParse("zeta/tools"), Default: true}}, nil)
	if repo, _, err := restarted.issueRepository(ctx, "LEGION-301"); err != nil || repo.String() != "acme/widgets" {
		t.Fatalf("LEGION-301 works %s, %v; want the clone's acme/widgets", repo, err)
	}
	heard, err := restarted.heardAtBoot(ctx)
	if err != nil {
		t.Fatal(err)
	}
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
	if _, err := intake.OpenConsumers(ctx, js, intake.ConsumerSpec{Project: "LEGION", Repositories: heard, Logger: quietLogger()}); err != nil {
		t.Fatal(err)
	}
	if filters := githubFilterSubjects(t, stream); !slices.Contains(filters, "notifications.github.Acme.Widgets.>") {
		t.Fatalf("GitHub filters after the restart = %v, want Acme/Widgets heard in GitHub's spelling", filters)
	}
	if _, err := js.Publish(ctx, "notifications.github.Acme.Widgets.pr.42", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	consumer, err := stream.Consumer(ctx, "legion-go-LEGION-github")
	if err != nil {
		t.Fatal(err)
	}
	info, err := consumer.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.NumPending != 1 {
		t.Fatalf("pending on the GitHub consumer after GitHub's event = %d, want the event", info.NumPending)
	}
}

// A record written before the record carried GitHub's spelling of its repository is named at boot
// from one read of GitHub, before GitHub intake subscribes, so it is heard in GitHub's spelling; a
// repository GitHub cannot read refuses the boot, naming the issue, rather than subscribing in a
// spelling its events are not published under.
func TestTheBootNamesARecordWithNoGitHubSpellingBeforeSubscribing(t *testing.T) {
	ctx := context.Background()
	seed := func(t *testing.T) (*workflowRuntime, projectRecords) {
		t.Helper()
		pool := isolatedOutboxPool(t)
		records := projectRecords{Store: record.NewStore(), project: "LEGION"}
		if _, err := pool.Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq, repo, default_branch)
			values ('LEGION-301', 'LEGION-301', 'LEGION', 'running', 'implementing', 1, 'in_progress', 'A', 1, 'acme/widgets', 'main')`); err != nil {
			t.Fatal(err)
		}
		if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			return records.PutSlot(ctx, tx, record.Slot{Issue: "LEGION-301", Index: 0, AdmittedAt: time.Now()})
		}); err != nil {
			t.Fatal(err)
		}
		return &workflowRuntime{pool: pool, records: records, repositories: newRepositories("LEGION"), log: quietLogger()}, records
	}

	t.Run("GitHub names it", func(t *testing.T) {
		w, records := seed(t)
		github := newNamedGitHub(t, map[string]string{"acme/widgets": "Acme/Widgets@main"})
		w.defaultBranches = newDefaultBranches(roleTokens{}, github.url, defaultBranchTTL, time.Now)
		if err := backfillRepositories(ctx, w.pool, records, &listingDispatch{}, "LEGION", t.TempDir(), w.defaultBranches, quietLogger()); err != nil {
			t.Fatal(err)
		}
		got := recordedIssue(t, w, "LEGION-301")
		if got == nil || got.Repo != "acme/widgets" || got.GitHubRepo != "Acme/Widgets" {
			t.Fatalf("LEGION-301 = %+v, want its own acme/widgets and GitHub's Acme/Widgets", got)
		}
		heard, err := w.heardAtBoot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if names := repositoryNames(heard); !slices.Equal(names, []string{"Acme/Widgets"}) {
			t.Fatalf("heard %v, want Acme/Widgets", names)
		}
	})

	t.Run("GitHub cannot read it", func(t *testing.T) {
		w, records := seed(t)
		github := newNamedGitHub(t, map[string]string{})
		w.defaultBranches = newDefaultBranches(roleTokens{}, github.url, defaultBranchTTL, time.Now)
		err := backfillRepositories(ctx, w.pool, records, &listingDispatch{}, "LEGION", t.TempDir(), w.defaultBranches, quietLogger())
		if err == nil || !strings.HasPrefix(err.Error(), "name the repository of LEGION-301: ") {
			t.Fatalf("backfill = %v, want a refusal naming LEGION-301", err)
		}
		if got := recordedIssue(t, w, "LEGION-301"); got.GitHubRepo != "" {
			t.Fatalf("LEGION-301 named %q after a refused read", got.GitHubRepo)
		}
	})
}
