package events

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

func TestPublishDisconnectsOverflowedSubscriber(t *testing.T) {
	broker := NewBroker()
	stream, cancel := broker.Subscribe()
	defer cancel()

	for sequence := 1; sequence <= 65; sequence++ {
		broker.Publish(model.Event{Seq: sequence})
	}
	if count := broker.SubscriberCount(); count != 0 {
		t.Fatalf("subscriber count after overflow = %d, want 0", count)
	}
	for sequence := 1; sequence <= 64; sequence++ {
		event, ok := <-stream
		if !ok || event.Seq != sequence {
			t.Fatalf("event %d = %#v (open=%t), want ordered buffered event", sequence, event, ok)
		}
	}
	if _, ok := <-stream; ok {
		t.Fatal("overflowed subscriber remained open")
	}
}

func TestAppendSequencesArtifactOwnedEvents(t *testing.T) {
	ctx := context.Background()
	database := openTestStore(t)
	if _, err := database.Pool.Exec(ctx, `insert into projects (key, name) values ('PP', 'Project')`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into issues (key, project_key, number, title, created_by, rank)
		values ('PP-1', 'PP', 1, 'Issue', '{"kind":"user","id":"alice"}', 'U')
	`); err != nil {
		t.Fatalf("seed issue: %v", err)
	}
	var artifactID string
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (project_key, slug, name, kind, created_by)
		values ('PP', 'notes-md', 'notes.md', 'doc', '{"kind":"user","id":"alice"}')
		returning id::text
	`).Scan(&artifactID); err != nil {
		t.Fatalf("create unlinked artifact: %v", err)
	}
	broker := NewBroker()
	first := appendEvent(t, database, broker, model.Event{
		ArtifactID: new(artifactID),
		Type:       "comment.created",
		Actor:      model.Actor{Kind: "user", ID: "alice"},
		Payload:    map[string]any{},
	})
	second := appendEvent(t, database, broker, model.Event{
		ArtifactID: new(artifactID),
		Type:       "comment.resolved",
		Actor:      model.Actor{Kind: "user", ID: "alice"},
		Payload:    map[string]any{},
	})
	issueEvent := appendEvent(t, database, broker, model.Event{
		IssueKey: new("PP-1"),
		Type:     "issue.updated",
		Actor:    model.Actor{Kind: "user", ID: "alice"},
		Payload:  map[string]any{},
	})

	if first.Seq != 1 || second.Seq != 2 {
		t.Fatalf("artifact event sequences = (%d, %d), want (1, 2)", first.Seq, second.Seq)
	}
	if first.Project != "PP" || second.Project != "PP" {
		t.Fatalf("artifact event projects = (%q, %q), want PP", first.Project, second.Project)
	}
	if issueEvent.Seq != 1 || issueEvent.Project != "PP" {
		t.Fatalf("issue event = seq %d project %q, want seq 1 project PP", issueEvent.Seq, issueEvent.Project)
	}
	var lastSeq int
	if err := database.Pool.QueryRow(ctx, `select last_seq from artifacts where id = $1`, artifactID).Scan(&lastSeq); err != nil {
		t.Fatalf("read artifact sequence: %v", err)
	}
	if lastSeq != 2 {
		t.Errorf("artifact last_seq = %d, want 2", lastSeq)
	}
	var issueKey, eventArtifactID *string
	if err := database.Pool.QueryRow(ctx, `
		select issue_key, artifact_id::text from events where id = $1
	`, second.ID).Scan(&issueKey, &eventArtifactID); err != nil {
		t.Fatalf("read persisted artifact event: %v", err)
	}
	if issueKey != nil || eventArtifactID == nil || *eventArtifactID != artifactID {
		t.Errorf("persisted artifact event owner = issue %v artifact %v, want nil issue and %q artifact", issueKey, eventArtifactID, artifactID)
	}
}

func TestAppendRejectsEventWithoutExactlyOneOwner(t *testing.T) {
	ctx := context.Background()
	database := openTestStore(t)
	if _, err := database.Pool.Exec(ctx, `insert into projects (key, name) values ('PP', 'Project')`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into issues (key, project_key, number, title, created_by, rank)
		values ('PP-1', 'PP', 1, 'Issue', '{"kind":"user","id":"alice"}', 'U')
	`); err != nil {
		t.Fatalf("seed issue: %v", err)
	}
	var unlinkedID, linkedID string
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (project_key, slug, name, kind, created_by)
		values ('PP', 'unlinked-md', 'unlinked.md', 'doc', '{"kind":"user","id":"alice"}')
		returning id::text
	`).Scan(&unlinkedID); err != nil {
		t.Fatalf("create unlinked artifact: %v", err)
	}
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
		values ('PP-1', 'PP', 'linked-md', 'linked.md', 'doc', '{"kind":"user","id":"alice"}')
		returning id::text
	`).Scan(&linkedID); err != nil {
		t.Fatalf("create linked artifact: %v", err)
	}
	broker := NewBroker()
	for _, test := range []struct {
		name  string
		event model.Event
		want  string
	}{
		{
			name:  "neither owner",
			event: model.Event{Type: "comment.created", Actor: model.Actor{Kind: "user", ID: "alice"}, Payload: map[string]any{}},
			want:  "exactly one owner",
		},
		{
			name: "both owners",
			event: model.Event{
				IssueKey:   new("PP-1"),
				ArtifactID: new(unlinkedID),
				Type:       "comment.created",
				Actor:      model.Actor{Kind: "user", ID: "alice"},
				Payload:    map[string]any{},
			},
			want: "exactly one owner",
		},
		{
			name: "linked artifact",
			event: model.Event{
				ArtifactID: new(linkedID),
				Type:       "comment.created",
				Actor:      model.Actor{Kind: "user", ID: "alice"},
				Payload:    map[string]any{},
			},
			want: "lock unlinked artifact for event",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			tx, err := database.Pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin event transaction: %v", err)
			}
			defer tx.Rollback(ctx)
			_, err = broker.Append(ctx, tx, test.event)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("append error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestChildAndParentPatchesCompleteWithoutDeadlock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	database := openTestStore(t)
	if _, err := database.Pool.Exec(ctx, `insert into projects (key, name) values ('PP', 'Project')`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into issues (key, project_key, number, title, created_by, rank)
		values
			('PP-1', 'PP', 1, 'Parent', '{"kind":"user","id":"alice"}', 'U'),
			('PP-2', 'PP', 2, 'Child', '{"kind":"user","id":"alice"}', 'V')
	`); err != nil {
		t.Fatalf("seed parent and child: %v", err)
	}
	broker := NewBroker()
	childEvent := model.Event{
		IssueKey: new("PP-2"), Type: "issue.updated", Actor: model.Actor{Kind: "user", ID: "alice"}, Payload: map[string]any{},
	}
	childStatus := model.Event{
		IssueKey: new("PP-1"), Type: "child.status", Actor: model.Actor{Kind: "user", ID: "alice"}, Payload: map[string]any{},
	}
	parentEvent := model.Event{
		IssueKey: new("PP-1"), Type: "issue.updated", Actor: model.Actor{Kind: "user", ID: "alice"}, Payload: map[string]any{},
	}
	childTx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin child patch: %v", err)
	}
	defer childTx.Rollback(context.Background())
	if err := broker.LockOwners(ctx, childTx, childEvent, childStatus); err != nil {
		t.Fatalf("lock child patch owners: %v", err)
	}

	parentStarted := make(chan struct{})
	parentDone := make(chan error, 1)
	go func() {
		parentTx, err := database.Pool.Begin(ctx)
		if err != nil {
			parentDone <- err
			return
		}
		defer parentTx.Rollback(context.Background())
		close(parentStarted)
		if err := broker.LockOwners(ctx, parentTx, parentEvent); err != nil {
			parentDone <- err
			return
		}
		if _, err := broker.Append(ctx, parentTx, parentEvent); err != nil {
			parentDone <- err
			return
		}
		parentDone <- parentTx.Commit(ctx)
	}()
	<-parentStarted

	if _, err := broker.Append(ctx, childTx, childEvent); err != nil {
		t.Fatalf("append child patch event: %v", err)
	}
	if _, err := broker.Append(ctx, childTx, childStatus); err != nil {
		t.Fatalf("append parent child.status event: %v", err)
	}
	if err := childTx.Commit(ctx); err != nil {
		t.Fatalf("commit child patch: %v", err)
	}
	select {
	case err := <-parentDone:
		if err != nil {
			t.Fatalf("complete parent patch: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("child and parent patches deadlocked")
	}
}

func appendEvent(t *testing.T, database *store.Store, broker *Broker, event model.Event) model.Event {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin event transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	event, err = broker.Append(ctx, tx, event)
	if err != nil {
		t.Fatalf("append event: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit event: %v", err)
	}
	return event
}

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	baseURL := os.Getenv("DISPATCH_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("DISPATCH_TEST_DATABASE_URL must be set to run broker tests")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		t.Fatalf("parse test database URL: %v", err)
	}
	adminURL := *parsed
	adminURL.Path = "/postgres"
	admin, err := pgxpool.New(context.Background(), adminURL.String())
	if err != nil {
		t.Fatalf("open test database admin: %v", err)
	}
	t.Cleanup(admin.Close)
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatalf("generate database suffix: %v", err)
	}
	databaseName := "dispatch_test_" + hex.EncodeToString(suffix[:])
	if _, err := admin.Exec(context.Background(), "create database "+databaseName); err != nil {
		t.Fatalf("create isolated database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "drop database "+databaseName+" with (force)"); err != nil {
			t.Errorf("drop isolated database: %v", err)
		}
	})
	testURL := *parsed
	testURL.Path = "/" + databaseName
	database, err := store.Open(context.Background(), testURL.String())
	if err != nil {
		t.Fatalf("open isolated store: %v", err)
	}
	t.Cleanup(database.Pool.Close)
	if err := database.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate isolated store: %v", err)
	}
	return database
}
