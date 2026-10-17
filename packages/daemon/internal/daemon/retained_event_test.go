package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
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
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// detailDispatch answers GetIssue with each issue's resolved repository, refusing the first
// failures reads with Dispatch's trouble, and counts the reads.
type detailDispatch struct {
	outboxDispatch
	mu       sync.Mutex
	resolved map[string]string
	failures int
	reads    int
}

func (d *detailDispatch) GetIssue(_ context.Context, key string) (dispatch.Issue, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reads++
	if d.failures > 0 {
		d.failures--
		return dispatch.Issue{}, errors.New("Dispatch is restarting")
	}
	return dispatch.Issue{Key: key, ResolvedRepo: d.resolved[key]}, nil
}

func (d *detailDispatch) readCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.reads
}

// retainedIssueEnvelope is a Dispatch issue event as JetStream retains it: written before Dispatch
// resolved repositories (resolvedRepo nil omits the field) or after it (the field set).
func retainedIssueEnvelope(t *testing.T, key string, id, seq int64, status string, actor map[string]string, resolvedRepo *string) []byte {
	t.Helper()
	issue := map[string]any{"key": key, "status": status, "title": key, "rank": "A", "parent": nil, "labels": []string{"legion"}}
	if resolvedRepo != nil {
		issue["resolved_repo"] = *resolvedRepo
	}
	event, err := json.Marshal(map[string]any{"id": id, "issue_key": key, "seq": seq, "type": "issue.updated", "actor": actor, "notify": true, "payload": issue})
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := json.Marshal(map[string]any{"event_id": "dispatch-retained-" + key, "source": "dispatch", "source_event_id": "retained", "topic": "issue",
		"dedupe_key": "dispatch-retained-" + key, "issued_at": 1, "payload_summary": "retained event", "payload": string(event), "trace_id": "retained"})
	if err != nil {
		t.Fatal(err)
	}
	return envelope
}

// An event JetStream retained from before the Dispatch upgrade carries no resolved_repo, and keeps
// its actor: a session writing backlog on an admitted root is refused by the workflow's session
// fence whether its event names the repository or not. Neither event is terminated, so the boot
// snapshot admission holds behind it never stands in for the event's own, actor-bearing write. The
// daemon's preparation fills a recorded issue's missing repository from its record.
func TestARetainedEventKeepsTheSessionFenceOfItsActor(t *testing.T) {
	for _, tc := range []struct {
		name     string
		retained bool
		prepared bool
	}{
		{"an event that names the repository", false, false},
		{"a retained event, applied from the record", true, false},
		{"a retained event, prepared by the daemon", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			root := record.Issue{Key: "LEGION-300", Tree: "LEGION-300", Project: "LEGION", Title: "root", Phase: phase.Planning, Generation: 1, Status: "in_progress",
				DispatchStatus: "in_progress", Rank: "A", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", HandedOver: true, LastDispatchSeq: 1}
			if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
				if err := records.PutIssue(ctx, tx, root); err != nil {
					return err
				}
				return records.PutSlot(ctx, tx, record.Slot{Issue: root.Key, Index: 0, AdmittedAt: time.Now()})
			}); err != nil {
				t.Fatal(err)
			}
			engine := workflow.New(records, workflow.Config{Project: "LEGION", Repositories: testRepositories, DesignGate: config.DesignGateOff}, quietLogger())
			admission := admit.New(records, engine, 1, "LEGION", quietLogger())
			conn, err := nats.Connect(workflowNATS(t))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(conn.Close)
			js, err := jetstream.New(conn)
			if err != nil {
				t.Fatal(err)
			}
			client := &detailDispatch{resolved: map[string]string{root.Key: "acme/gadgets"}}
			spec := intake.ConsumerSpec{Project: "LEGION", Repositories: []ghrepo.Repository{ghrepo.MustParse("acme/widgets")}, AckWait: time.Second, Logger: quietLogger()}
			if tc.prepared {
				w := &workflowRuntime{pool: pool, records: records, dispatch: client, repositories: newRepositories("LEGION")}
				spec.PrepareRepository = w.prepareRepository
			}
			consumers, err := intake.OpenConsumers(ctx, js, spec)
			if err != nil {
				t.Fatal(err)
			}
			var resolved *string
			if !tc.retained {
				repo := "acme/widgets"
				resolved = &repo
			}
			ack, err := js.Publish(ctx, "notifications.dispatch.issue.LEGION-300.issue.updated",
				retainedIssueEnvelope(t, root.Key, 102, 2, "backlog", map[string]string{"kind": "session", "id": "outside-session"}, resolved))
			if err != nil {
				t.Fatal(err)
			}
			reconcileAdmission(t, pool, admission, []dispatch.IssueSummary{{Key: root.Key, Title: root.Title, Rank: "A", Status: "backlog", LastSeq: 2, HandedOver: true,
				ResolvedRepo: "acme/widgets"}}, int64(ack.Sequence), intake.DispatchConsumerPosition{})
			if !admission.Held() {
				t.Fatal("the boot summary is not held behind the retained event")
			}
			run, cancel := context.WithCancel(ctx)
			done := make(chan error, 1)
			go func() { done <- consumers.Run(run, pool, engine, admission) }()
			t.Cleanup(func() {
				cancel()
				if err := <-done; err != nil {
					t.Error(err)
				}
			})
			var position intake.DispatchConsumerPosition
			testwait.Eventually(t, "the event is applied", func() bool {
				var err error
				position, err = consumers.DispatchPosition(ctx)
				return err == nil && position.Idle
			})
			if _, err := intake.ApplyFact(ctx, pool, "test", "release", position, engine, admission); err != nil {
				t.Fatal(err)
			}
			var got *record.Issue
			if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
				var err error
				got, err = records.Issue(ctx, tx, root.Key)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if got.Status != root.Status || got.Phase != root.Phase || got.LingerUntil != nil || got.Repo != "acme/widgets" {
				t.Fatalf("after the session's backlog: status %s, phase %s, linger %v, repo %s; want the root running on acme/widgets",
					got.Status, got.Phase, got.LingerUntil, got.Repo)
			}
			if reads := client.readCount(); reads != 0 {
				t.Fatalf("Dispatch was read %d times for a recorded issue, want none", reads)
			}
		})
	}
}

// A retained event of an issue the daemon has not recorded is applied with the repository
// Dispatch resolves for it now, read from the issue's detail, and with the event's own actor and
// sequence. A read Dispatch fails leaves the event unapplied and retried; it is never applied with
// no repository, and never dropped for the boot snapshot to stand in for.
func TestARetainedEventOfAnUnrecordedIssueReadsItsRepositoryFromDispatch(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	conn, err := nats.Connect(workflowNATS(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(conn.Close)
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatal(err)
	}
	client := &detailDispatch{resolved: map[string]string{"LEGION-301": "acme/widgets"}, failures: 1}
	github := newNamedGitHub(t, map[string]string{"acme/widgets": "acme/widgets@release/v7.1"})
	repos := newRepositories("LEGION")
	repos.set([]mappedRepository{{Repo: ghrepo.MustParse("acme/widgets"), Default: true}}, nil)
	w := &workflowRuntime{pool: pool, records: records, dispatch: client, repositories: repos,
		defaultBranches: newDefaultBranches(roleTokens{}, github.url, defaultBranchTTL, time.Now)}
	engine := workflow.New(records, workflow.Config{Project: "LEGION", Repositories: w, DesignGate: config.DesignGateOff}, quietLogger())
	admission := admit.New(records, engine, 1, "LEGION", quietLogger())
	consumers, err := intake.OpenConsumers(ctx, js, intake.ConsumerSpec{Project: "LEGION", Repositories: []ghrepo.Repository{ghrepo.MustParse("acme/widgets")},
		AckWait: time.Second, NakDelay: 10 * time.Millisecond, Logger: quietLogger(), PrepareRepository: w.prepareRepository})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := js.Publish(ctx, "notifications.dispatch.issue.LEGION-301.issue.updated",
		retainedIssueEnvelope(t, "LEGION-301", 103, 4, "todo", map[string]string{"kind": "user", "id": "a-person"}, nil)); err != nil {
		t.Fatal(err)
	}
	run, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- consumers.Run(run, pool, engine, admission) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	var got *record.Issue
	testwait.Eventually(t, "the retained event records its issue", func() bool {
		if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			var err error
			got, err = records.Issue(ctx, tx, "LEGION-301")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return got != nil
	})
	if got.Repo != "acme/widgets" || got.DefaultBranch != "release/v7.1" || got.LastDispatchSeq != 4 {
		t.Fatalf("recorded %s on %s@%s at seq %d, want acme/widgets@release/v7.1 at the event's sequence 4", got.Key, got.Repo, got.DefaultBranch, got.LastDispatchSeq)
	}
	if reads := client.readCount(); reads != 2 {
		t.Fatalf("Dispatch was read %d times, want the failed read and its retry", reads)
	}
}
