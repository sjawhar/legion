package daemon

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
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
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// A mapping change at runtime reaches the daemon through its Dispatch consumer: on Dispatch's
// settings.repo_project.updated of its project, it reads the mapping again. A newly mapped
// repository both Apps are installed on is served (GitHub intake hears it, its owner joins the
// owners), one missing an App is left out and not heard, and a repository unmapped while a
// record names it is still heard, so its running issue's facts still arrive. Its owner leaves the
// owners every role's gh bundle mints for: one App uninstalled there would otherwise fail every
// pane's bundle, and the issue's own panes keep that owner as their default (appauth's
// GitHubCredential mints for a role's own owner whether or not the set holds it).
func TestAMappingChangeAtRuntimeChangesWhatTheDaemonServes(t *testing.T) {
	t.Run("GitHub answering at once", func(t *testing.T) { mappingChangeAtRuntime(t, 5*time.Second, 0) })
	// A refresh reads Dispatch and GitHub, which can take longer than the event's acknowledgement
	// window (AckWait): a GitHub answering every read later than that window still has the change
	// applied, rather than its event redelivered with a refresh that never finishes.
	t.Run("every GitHub read outlasting the acknowledgement window", func(t *testing.T) {
		mappingChangeAtRuntime(t, 300*time.Millisecond, 400*time.Millisecond)
	})
}

func mappingChangeAtRuntime(t *testing.T, ackWait, delay time.Duration) {
	pool := isolatedOutboxPool(t)
	url := workflowNATS(t)
	conn, err := nats.Connect(url, nats.Timeout(time.Second))
	if err != nil {
		t.Fatalf("connect NATS: %v", err)
	}
	t.Cleanup(conn.Close)
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	stream, err := js.Stream(context.Background(), "ENVOY_NOTIFICATIONS")
	if err != nil {
		t.Fatal(err)
	}
	github := newNamedGitHub(t, map[string]string{
		"acme/widgets": "acme/widgets@main", "zeta/tools": "Zeta/Tools@release/v7.1", "orbit/gadgets": "orbit/gadgets@main",
	})
	client := &mappingDispatch{mappings: []dispatch.RepoProject{
		{Repo: "acme/widgets", Project: "CAPTURE", Default: true},
		{Repo: "orbit/gadgets", Project: "CAPTURE"},
	}}
	records := projectRecords{Store: record.NewStore(), project: "CAPTURE"}
	// A running tree's issue on orbit/gadgets, which the mapping change below unmaps.
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		if err := records.PutIssue(context.Background(), tx, record.Issue{Key: "CAPTURE-7", Repo: "orbit/gadgets", DefaultBranch: "main", GitHubRepo: "orbit/gadgets", Tree: "CAPTURE-7",
			Project: "CAPTURE", Title: "running", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "A", HandedOver: true, LastDispatchSeq: 1}); err != nil {
			return err
		}
		return records.PutSlot(context.Background(), tx, record.Slot{Issue: "CAPTURE-7", Index: 0, AdmittedAt: time.Now()})
	}); err != nil {
		t.Fatal(err)
	}
	installs := &installations{missing: map[string]bool{}, delay: delay}
	w := &workflowRuntime{mappingChanged: make(chan struct{}, 1),
		pool: pool, records: records, dispatch: client, dispatchProject: "CAPTURE", installs: installs,
		repositories: newRepositories("CAPTURE"), defaultBranches: newDefaultBranches(roleTokens{}, github.url, defaultBranchTTL, time.Now),
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if err := w.loadMapping(context.Background()); err != nil {
		t.Fatalf("loadMapping: %v", err)
	}
	if err := w.nameMapping(context.Background()); err != nil {
		t.Fatalf("nameMapping: %v", err)
	}
	heard, err := w.heardAtBoot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	w.consumers, err = intake.OpenConsumers(context.Background(), js, intake.ConsumerSpec{
		Project: "CAPTURE", Repositories: heard, AckWait: ackWait, NakDelay: 10 * time.Millisecond, Logger: w.log,
		RepositoryMapping: w.mappingChange,
	})
	if err != nil {
		t.Fatalf("OpenConsumers: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done, refreshed := make(chan error, 1), make(chan struct{})
	go func() { done <- w.consumers.Run(ctx, pool) }()
	go func() { w.refreshOnMappingChange(ctx); close(refreshed) }()
	t.Cleanup(func() { cancel(); <-done; <-refreshed })

	// orbit/gadgets moves to another project; zeta/tools is mapped; acme/thing is mapped with no
	// review App installed on it.
	client.set(
		dispatch.RepoProject{Repo: "acme/widgets", Project: "CAPTURE", Default: true},
		dispatch.RepoProject{Repo: "zeta/tools", Project: "CAPTURE"},
		dispatch.RepoProject{Repo: "acme/thing", Project: "CAPTURE"},
		dispatch.RepoProject{Repo: "orbit/gadgets", Project: "OTHER"},
	)
	installs.mu.Lock()
	installs.missing["review acme/thing"] = true
	installs.mu.Unlock()
	payload, err := json.Marshal(map[string]any{"id": 70, "issue_key": nil, "project": "CAPTURE", "seq": 0, "type": "settings.repo_project.updated",
		"actor": map[string]string{"kind": "user", "id": "smoke"}, "notify": true, "created_at": "2026-10-16T18:00:00Z",
		"payload": map[string]any{"mapping": map[string]any{"repo": "zeta/tools", "project": "CAPTURE", "default": false}, "deleted": false}})
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := json.Marshal(map[string]any{"event_id": "dispatch-70", "source": "dispatch", "source_event_id": "70", "topic": "test", "dedupe_key": "dispatch-70",
		"issued_at": 1, "payload_summary": "mapping updated", "payload": string(payload), "trace_id": "dispatch-70"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := js.Publish(context.Background(), "notifications.dispatch.project.CAPTURE.settings.repo_project.updated", envelope); err != nil {
		t.Fatalf("publish the mapping change: %v", err)
	}

	// The refresh sets the GitHub consumer's filters first and publishes what the daemon serves only
	// once they took, so the test waits for both.
	want := []string{"notifications.github.Zeta.Tools.>", "notifications.github.acme.widgets.>", "notifications.github.orbit.gadgets.>"}
	testwait.Eventually(t, "the GitHub consumer hears the changed mapping and the daemon serves it", func() bool {
		consumer, err := stream.Consumer(context.Background(), "legion-go-CAPTURE-github")
		if err != nil {
			return false
		}
		info, err := consumer.Info(context.Background())
		if err != nil {
			return false
		}
		got := slices.Clone(info.Config.FilterSubjects)
		slices.Sort(got)
		return slices.Equal(got, want) && slices.Equal(w.repositories.Owners(), []string{"acme", "zeta"})
	})
	refused := w.repositories.Refused()
	if len(refused) != 1 || refused[0].Repo != "acme/thing" || !strings.Contains(refused[0].Reason, "the review App is not installed on acme/thing") {
		t.Fatalf("Refused = %#v, want acme/thing left out for its review App", refused)
	}
	if accepted := w.repositories.Accepted(); slices.Contains(accepted, ghrepo.MustParse("orbit/gadgets")) {
		t.Fatalf("Accepted = %v still serves orbit/gadgets, unmapped from the project", accepted)
	}
}
