package daemon

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// A boot that holds a key for the Dispatch consumer reads the repositories the mapping serves at
// that moment, and nothing else: the boot's own preparation of a key on a repository the mapping
// does not serve reads no default branch for it. Dispatch may map that repository afterwards
// (settings.repo_project.updated), and the refresh reads no default branch either. The release
// then records the key inside the position fact's own transaction, where the workflow resolves a
// repository from memory alone and refuses a served repository whose default branch was never
// read: the fact failed, every retry of it failed the same way, and the hold never released at
// all. The poll now readies each held key's repository before it applies a position.
func TestAHeldKeyReleasesOnARepositoryTheMappingBeganToServeAfterTheBoot(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	logs := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(logs, nil))
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("the daemon's own log:\n%s", logs.String())
		}
	})
	github := newNamedGitHub(t, map[string]string{"acme/widgets": "acme/widgets@release/v7.1"})
	repos := newRepositories("CAPTURE")
	w := &workflowRuntime{
		pool: pool, records: records, repositories: repos, dispatchProject: "CAPTURE", bootID: "prepare-boot",
		defaultBranches: newDefaultBranches(roleTokens{}, github.url, defaultBranchTTL, time.Now),
		log:             log, holdPollInterval: 10 * time.Millisecond,
	}
	engine := workflow.New(records, workflow.Config{Project: "CAPTURE", Repositories: w, DesignGate: config.DesignGateOff}, log)
	admission := admit.New(records, engine, 1, "CAPTURE", log)
	w.admission, w.engine = admission, engine
	w.handlers = []intake.Handler{engine, admission}

	// The boot: the mapping serves nothing yet, so preparing the listing's own todo root reads no
	// default branch for it, and the key is held for the Dispatch consumer.
	listed := dispatch.IssueSummary{Key: "LEGION-EDGE", ResolvedRepo: "acme/widgets", Title: "edge", Status: "todo", Rank: "A", HandedOver: true, LastSeq: 2}
	if _, err := w.prepareIssue(ctx, intake.ListedIssue(listed), false, false); err != nil {
		t.Fatalf("prepare the boot listing: %v", err)
	}
	reconcileAdmission(t, pool, admission, []dispatch.IssueSummary{listed}, 5, intake.DispatchConsumerPosition{AckFloorStream: 0, Idle: false})
	if !admission.Held() {
		t.Fatal("Held() = false after the boot listing of an unrecorded root, want it held for the consumer")
	}

	// Dispatch maps the repository to the project after the boot. The refresh reads the mapping
	// alone; no default branch is read for the repository it adds.
	if added, _ := repos.set([]mappedRepository{{Repo: ghrepo.MustParse("acme/widgets"), Default: true}}, nil); len(added) != 1 {
		t.Fatalf("the mapping refresh added %v, want acme/widgets", added)
	}

	run, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	reader := &failingPositionReader{position: intake.DispatchConsumerPosition{AckFloorStream: 5}}
	go func() { w.pollHoldReleaseWith(run, reader); close(done) }()
	testwait.Eventually(t, "the held root releases on the repository the mapping now serves", func() bool { return !admission.Held() })
	cancel()
	<-done

	if strings.Contains(logs.String(), "was never read before an issue on it was recorded") {
		t.Fatalf("the release resolved a repository whose default branch was never read; the poll must ready it first. Log:\n%s", logs.String())
	}
	var admitted *record.Issue
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var err error
		admitted, err = records.Issue(ctx, tx, "LEGION-EDGE")
		return err
	}); err != nil {
		t.Fatalf("read the released root: %v", err)
	}
	if admitted == nil || admitted.Repo != "acme/widgets" || admitted.DefaultBranch != "release/v7.1" {
		t.Fatalf("the released root = %#v, want it recorded on acme/widgets at release/v7.1", admitted)
	}
}

// A repository GitHub does not answer for leaves the hold as it is: the poll says so and applies
// no position, and the release happens once GitHub answers again.
func TestAHeldKeyWhoseRepositoryGitHubRefusesKeepsItsHoldAndReleasesLater(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	logs := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(logs, nil))
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("the daemon's own log:\n%s", logs.String())
		}
	})
	named := map[string]string{}
	github := newNamedGitHub(t, named)
	repos := newRepositories("CAPTURE")
	repos.set([]mappedRepository{{Repo: ghrepo.MustParse("acme/widgets"), Default: true}}, nil)
	w := &workflowRuntime{
		pool: pool, records: records, repositories: repos, dispatchProject: "CAPTURE", bootID: "refusing-boot",
		defaultBranches: newDefaultBranches(roleTokens{}, github.url, defaultBranchTTL, time.Now),
		log:             log, holdPollInterval: 10 * time.Millisecond,
	}
	engine := workflow.New(records, workflow.Config{Project: "CAPTURE", Repositories: w, DesignGate: config.DesignGateOff}, log)
	admission := admit.New(records, engine, 1, "CAPTURE", log)
	w.admission, w.engine = admission, engine
	w.handlers = []intake.Handler{engine, admission}

	listed := dispatch.IssueSummary{Key: "LEGION-EDGE", ResolvedRepo: "acme/widgets", Title: "edge", Status: "todo", Rank: "A", HandedOver: true, LastSeq: 2}
	reconcileAdmission(t, pool, admission, []dispatch.IssueSummary{listed}, 5, intake.DispatchConsumerPosition{AckFloorStream: 0, Idle: false})
	if !admission.Held() {
		t.Fatal("Held() = false after the boot listing of an unrecorded root, want it held for the consumer")
	}

	run, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	reader := &failingPositionReader{position: intake.DispatchConsumerPosition{AckFloorStream: 5}}
	go func() { w.pollHoldReleaseWith(run, reader); close(done) }()
	testwait.Eventually(t, "the poll says why it cannot ready the held key's repository", func() bool {
		return strings.Contains(logs.String(), "ready the repositories of the held keys")
	})
	if !admission.Held() {
		t.Fatal("the hold released while GitHub would not name the repository, want it kept")
	}

	// GitHub answers for the repository, and the next tick releases the hold.
	named["acme/widgets"] = "acme/widgets@main"
	testwait.Eventually(t, "the held root releases once GitHub names its repository", func() bool { return !admission.Held() })
	cancel()
	<-done
}
