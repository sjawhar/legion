package events

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
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

func TestAnchorRefreshEventsDoNotWakeAgents(t *testing.T) {
	broker := NewBroker()
	for _, eventType := range []string{"comment.anchor_refreshed", "ask.anchor_refreshed"} {
		if broker.Notify(model.Event{Type: eventType, Actor: model.Actor{Kind: "user", ID: "alice"}}) {
			t.Fatalf("%s must not notify", eventType)
		}
	}
}

func TestAppendSequencesArtifactOwnedEvents(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
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

func TestAppendRejectsEventsWithoutAValidOwnerOrAgentTarget(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
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
			name:  "neither owner nor agent target",
			event: model.Event{Type: "comment.created", Actor: model.Actor{Kind: "user", ID: "alice"}, Payload: map[string]any{}},
			want:  "ownerless event requires session target",
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
			want: "at most one owner",
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
	database := storetest.Open(t)
	seed := context.Background()
	if _, err := database.Pool.Exec(seed, `insert into projects (key, name) values ('PP', 'Project')`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := database.Pool.Exec(seed, `
		insert into issues (key, project_key, number, title, created_by, rank)
		values
			('PP-1', 'PP', 1, 'Parent', '{"kind":"user","id":"alice"}', 'U'),
			('PP-2', 'PP', 2, 'Child', '{"kind":"user","id":"alice"}', 'V')
	`); err != nil {
		t.Fatalf("seed parent and child: %v", err)
	}
	// The deadline bounds only the two patches, started once the database is provisioned and
	// seeded, which on a loaded machine can take longer than the patches ever should. It is a
	// ceiling on a hang, not a measure of speed: an inverted lock order ends in Postgres's
	// deadlock error, and a patch that waits on the other for good never ends.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
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

func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

// statementLog records the text of every statement a pool's connections send, a pipeline's one by
// one (pgx.QueryTracer and pgx.BatchTracer).
type statementLog struct {
	mu         sync.Mutex
	statements []string
}

func (l *statementLog) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.statements = append(l.statements, data.SQL)
	return ctx
}

func (l *statementLog) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (l *statementLog) TraceBatchStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, queued := range data.Batch.QueuedQueries {
		l.statements = append(l.statements, queued.SQL)
	}
	return ctx
}

func (l *statementLog) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {}

func (l *statementLog) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}

// count is how many statements sent from since on contain text.
func (l *statementLog) count(since int, text string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	count := 0
	for _, statement := range l.statements[since:] {
		if strings.Contains(statement, text) {
			count++
		}
	}
	return count
}

func (l *statementLog) mark() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.statements)
}

// AppendAll writes a transaction's events as Append would one after another - each owner's
// events take its next sequences in the order given, its last_seq ends at the last of them, an
// ownerless event's sequence is 0 - but locks each owner once, takes the commit-order lock once
// and inserts every row in one statement, and hands the events back in the order given, each with
// the id of its own row.
func TestAppendAllSequencesMixedOwnersInInputOrderWithOneInsert(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name) values ('PP', 'Project');
		insert into issues (key, project_key, number, title, created_by, rank)
		values
			('PP-1', 'PP', 1, 'First', '{"kind":"user","id":"alice"}', 'U'),
			('PP-2', 'PP', 2, 'Second', '{"kind":"user","id":"alice"}', 'V');
	`); err != nil {
		t.Fatalf("seed owners: %v", err)
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
	alice := model.Actor{Kind: "user", ID: "alice"}
	// PP-1 already has an event, so its next sequences start past 1.
	appendEvent(t, database, broker, model.Event{IssueKey: new("PP-1"), Type: "issue.updated", Actor: alice, Payload: map[string]any{}})

	log := &statementLog{}
	traced := storetest.Traced(t, database, log).Pool

	type owner struct {
		key     string
		event   func() model.Event
		lastSeq string
		arg     string
	}
	issue1 := owner{key: "PP-1", event: func() model.Event { return model.Event{IssueKey: new("PP-1")} },
		lastSeq: `select last_seq from issues where key = $1`, arg: "PP-1"}
	issue2 := owner{key: "PP-2", event: func() model.Event { return model.Event{IssueKey: new("PP-2")} },
		lastSeq: `select last_seq from issues where key = $1`, arg: "PP-2"}
	artifact := owner{key: "artifact", event: func() model.Event { return model.Event{ArtifactID: new(artifactID)} },
		lastSeq: `select last_seq from artifacts where id = $1`, arg: artifactID}
	project := owner{key: "project", event: func() model.Event { return model.Event{ProjectKey: new("PP")} },
		lastSeq: `select last_seq from projects where key = $1`, arg: "PP"}
	session := owner{key: "session", event: func() model.Event {
		return model.Event{Payload: model.UserAgentStateEventPayload{SessionID: "session-1"}}
	}}
	order := []owner{issue2, artifact, issue1, issue2, project, session, issue1, artifact, issue2}
	given := make([]model.Event, len(order))
	for index, owner := range order {
		event := owner.event()
		event.Actor = alice
		event.Type = fmt.Sprintf("comment.probe_%d", index)
		if owner.key == "session" {
			event.Type = "user_agent_state.updated"
		} else {
			event.Payload = map[string]any{"index": index}
		}
		if index == 2 {
			// A refresh never wakes anyone, so the notify flags are not all one value.
			event.Type = "comment.anchor_refreshed"
		}
		given[index] = event
	}

	tx, err := traced.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	since := log.mark()
	appended, err := broker.AppendAll(ctx, tx, given)
	if err != nil {
		t.Fatalf("append all: %v", err)
	}
	statements := log.mark() - since
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if inserts := log.count(since, "insert into events"); inserts != 1 {
		t.Errorf("AppendAll sent %d inserts into events, want 1", inserts)
	}
	if locks := log.count(since, "pg_advisory_xact_lock"); locks != 1 {
		t.Errorf("AppendAll took the commit-order lock %d times, want 1", locks)
	}
	if ownerLocks := log.count(since, "for no key update"); ownerLocks != 4 {
		t.Errorf("AppendAll sent %d owner locks for 4 owners, want 4", ownerLocks)
	}
	// Four owner locks, the commit-order lock, the insert and four last_seq advances.
	if statements != 10 {
		t.Errorf("AppendAll of %d events on 4 owners and a session sent %d statements, want 10", len(given), statements)
	}

	if len(appended) != len(given) {
		t.Fatalf("AppendAll returned %d events for %d given", len(appended), len(given))
	}
	wantSeq := map[string]int{"PP-1": 1}
	for index, event := range appended {
		owner := order[index]
		want := 0
		if owner.key != "session" {
			wantSeq[owner.key]++
			want = wantSeq[owner.key]
		}
		if event.Type != given[index].Type || event.Seq != want {
			t.Errorf("event %d = %s seq %d, want %s seq %d", index, event.Type, event.Seq, given[index].Type, want)
		}
		if owner.key != "session" && event.Project != "PP" {
			t.Errorf("event %d project = %q, want PP", index, event.Project)
		}
		if want := broker.Notify(event); event.Notify != want {
			t.Errorf("event %d notify = %t, want %t", index, event.Notify, want)
		}
		if index > 0 && event.ID <= appended[index-1].ID {
			t.Errorf("event %d id %d is not above event %d's %d: ids are not in input order", index, event.ID, index-1, appended[index-1].ID)
		}
		var storedType string
		var storedSeq int
		var storedNotify bool
		var payload []byte
		var createdAt time.Time
		if err := database.Pool.QueryRow(ctx, `
			select type, seq, notify, payload, created_at from events where id = $1
		`, event.ID).Scan(&storedType, &storedSeq, &storedNotify, &payload, &createdAt); err != nil {
			t.Fatalf("read event %d's row: %v", index, err)
		}
		if storedType != event.Type || storedSeq != event.Seq || storedNotify != event.Notify || !createdAt.Equal(event.CreatedAt) {
			t.Errorf("event %d's row %d = %s seq %d notify %t at %s, want %s seq %d notify %t at %s",
				index, event.ID, storedType, storedSeq, storedNotify, createdAt, event.Type, event.Seq, event.Notify, event.CreatedAt)
		}
		if owner.key != "session" {
			var body struct {
				Index int `json:"index"`
			}
			if err := json.Unmarshal(payload, &body); err != nil || body.Index != index {
				t.Errorf("event %d's row %d carries payload %s, want index %d", index, event.ID, payload, index)
			}
		}
	}
	for _, owner := range []owner{issue1, issue2, artifact, project} {
		var lastSeq int
		if err := database.Pool.QueryRow(ctx, owner.lastSeq, owner.arg).Scan(&lastSeq); err != nil {
			t.Fatalf("read %s last_seq: %v", owner.key, err)
		}
		if lastSeq != wantSeq[owner.key] {
			t.Errorf("%s last_seq = %d, want %d", owner.key, lastSeq, wantSeq[owner.key])
		}
	}
}

// The owner row's lock is what allocates the per-owner event sequence, so weakening it to
// `for no key update` - which it is, so a foreign-key child insert never waits on it - has to
// leave that allocation exactly as strict. Concurrent appends on one owner must still produce
// a dense, gapless, duplicate-free run, and leave last_seq at its end.
func TestConcurrentAppendsKeepOwnerSequencesDense(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name) values ('DD', 'Density');
		insert into issues (key, project_key, number, title, created_by, rank)
		values ('DD-1', 'DD', 1, 'Issue', '{"kind":"user","id":"alice"}', 'U');
	`); err != nil {
		t.Fatalf("seed owners: %v", err)
	}
	var artifactID string
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (project_key, slug, name, kind, created_by)
		values ('DD', 'notes-md', 'notes.md', 'doc', '{"kind":"user","id":"alice"}')
		returning id::text
	`).Scan(&artifactID); err != nil {
		t.Fatalf("create unlinked artifact: %v", err)
	}

	const writers = 12
	broker := NewBroker()
	for _, owner := range []struct {
		name     string
		event    func() model.Event
		match    string
		lastSeq  string
		ownerArg string
	}{
		{
			name: "project document",
			event: func() model.Event {
				return model.Event{ArtifactID: new(artifactID), Type: "comment.created",
					Actor: model.Actor{Kind: "user", ID: "alice"}, Payload: map[string]any{}}
			},
			match:    `artifact_id = $1::uuid`,
			lastSeq:  `select last_seq from artifacts where id = $1`,
			ownerArg: artifactID,
		},
		{
			name: "issue",
			event: func() model.Event {
				return model.Event{IssueKey: new("DD-1"), Type: "comment.created",
					Actor: model.Actor{Kind: "user", ID: "alice"}, Payload: map[string]any{}}
			},
			match:    `issue_key = $1`,
			lastSeq:  `select last_seq from issues where key = $1`,
			ownerArg: "DD-1",
		},
	} {
		t.Run(owner.name, func(t *testing.T) {
			appended := make(chan error, writers)
			for range writers {
				go func() {
					tx, err := database.Pool.Begin(ctx)
					if err != nil {
						appended <- err
						return
					}
					defer tx.Rollback(ctx)
					if _, err := broker.Append(ctx, tx, owner.event()); err != nil {
						appended <- err
						return
					}
					appended <- tx.Commit(ctx)
				}()
			}
			for range writers {
				if err := <-appended; err != nil {
					t.Fatalf("concurrent append: %v", err)
				}
			}
			var count, distinct, minSeq, maxSeq, lastSeq int
			if err := database.Pool.QueryRow(ctx, `
				select count(*), count(distinct seq), min(seq), max(seq)
				from events where type = 'comment.created' and `+owner.match, owner.ownerArg,
			).Scan(&count, &distinct, &minSeq, &maxSeq); err != nil {
				t.Fatalf("read appended sequences: %v", err)
			}
			if err := database.Pool.QueryRow(ctx, owner.lastSeq, owner.ownerArg).Scan(&lastSeq); err != nil {
				t.Fatalf("read owner last_seq: %v", err)
			}
			if count != writers || distinct != writers || minSeq != 1 || maxSeq != writers || lastSeq != writers {
				t.Fatalf(
					"%s sequences: count=%d distinct=%d min=%d max=%d last_seq=%d, want a dense 1..%d",
					owner.name, count, distinct, minSeq, maxSeq, lastSeq, writers,
				)
			}
		})
	}
}
