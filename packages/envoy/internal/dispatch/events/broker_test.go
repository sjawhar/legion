package events

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

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

// Quiet and Notify answer different questions, and the difference is the whole point of Quiet:
// an agent's comment, document or ask wakes nobody (Notify false) but is the activity a session
// following `notifications.dispatch.issue.<KEY>.>` subscribed for, so it is not quiet.
func TestQuietSeparatesAnAgentsActivityFromBookkeeping(t *testing.T) {
	broker := NewBroker()
	agent := model.Actor{Kind: "session", ID: "5a660655-04ad-4ce0-8a9b-93dd03c412b7"}
	person := model.Actor{Kind: "user", ID: "alice"}
	for _, tc := range []struct {
		name  string
		event model.Event
		quiet bool
	}{
		{name: "an agent's comment", event: model.Event{Type: "comment.created", Actor: agent}, quiet: false},
		{name: "an agent's document", event: model.Event{Type: "artifact.created", Actor: agent}, quiet: false},
		{name: "an agent's ask", event: model.Event{Type: "ask.opened", Actor: agent}, quiet: false},
		{name: "an agent's issue update", event: model.Event{Type: "issue.updated", Actor: agent}, quiet: false},
		{name: "a person's comment", event: model.Event{Type: "comment.created", Actor: person}, quiet: false},
		{name: "a delivery receipt", event: model.Event{Type: "comment.delivery", Actor: agent}, quiet: true},
		{name: "a message receipt", event: model.Event{Type: "message.delivery", Actor: person}, quiet: true},
		{name: "an anchor refresh", event: model.Event{Type: "comment.anchor_refreshed", Actor: person}, quiet: true},
		{name: "per-viewer bookkeeping", event: model.Event{Type: "user_state.updated", Actor: person}, quiet: true},
		{name: "a person's grant change", event: model.Event{Type: "artifact.grants_changed", Actor: person}, quiet: true},
		{
			name: "a targeted message its session already had",
			event: model.Event{Type: "message.created", Actor: person, Payload: model.MessageEventPayload{
				Message: model.Message{
					ID:     "d7657c0d-71b9-43d5-8783-a5d98f7812e0",
					Target: new("5a660655-04ad-4ce0-8a9b-93dd03c412b7"),
				},
			}},
			quiet: true,
		},
		// The outbox asks Quiet of the event it scanned out of Postgres, whose payload JSON
		// decoded into a map (outbox/publisher.go, scanPendingEvents): one event is quiet
		// whichever shape its payload reached Quiet in.
		{
			name: "that targeted message as the outbox scanned it",
			event: model.Event{Type: "message.created", Actor: person, Payload: map[string]any{
				"id":     "d7657c0d-71b9-43d5-8783-a5d98f7812e0",
				"target": "5a660655-04ad-4ce0-8a9b-93dd03c412b7",
			}},
			quiet: true,
		},
		{
			name: "a message the outbox scanned that names no session",
			event: model.Event{Type: "message.created", Actor: person, Payload: map[string]any{
				"id":     "d7657c0d-71b9-43d5-8783-a5d98f7812e0",
				"target": nil,
			}},
			quiet: false,
		},
		{
			name: "an ask edit that only moved the version it names",
			event: model.Event{Type: "ask.edited", Actor: person, Payload: model.AskEditEventPayload{
				Ask:   model.Ask{ID: "ask-1"},
				Quiet: true,
			}},
			quiet: true,
		},
		{
			name: "that ask edit as the outbox scanned it",
			event: model.Event{Type: "ask.edited", Actor: person, Payload: map[string]any{
				"id":    "ask-1",
				"quiet": true,
			}},
			quiet: true,
		},
		{
			name:  "an unnamed version",
			event: model.Event{Type: "artifact.version", Actor: agent, Payload: map[string]any{"version": map[string]any{"named": false}}},
			quiet: true,
		},
		{
			name:  "a named version",
			event: model.Event{Type: "artifact.version", Actor: agent, Payload: map[string]any{"version": map[string]any{"named": true}}},
			quiet: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Quiet(tc.event); got != tc.quiet {
				t.Fatalf("Quiet(%s by %s) = %t, want %t", tc.event.Type, tc.event.Actor.Kind, got, tc.quiet)
			}
			// Notify is unchanged by the split: it is quiet, a child event, or a person's.
			wantNotify := !tc.quiet && tc.event.Actor.Kind == "user"
			if got := broker.Notify(tc.event); got != wantNotify {
				t.Fatalf("Notify(%s by %s) = %t, want %t", tc.event.Type, tc.event.Actor.Kind, got, wantNotify)
			}
		})
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

// seedEventOwners creates one of every kind of event owner: project PP, issues PP-1 and PP-2,
// and an unlinked artifact of PP, whose id it returns.
func seedEventOwners(t *testing.T, database *store.Store) string {
	t.Helper()
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name) values ('PP', 'Project');
		insert into issues (key, project_key, number, title, created_by, rank)
		values
			('PP-1', 'PP', 1, 'One', '{"kind":"user","id":"alice"}', 'U'),
			('PP-2', 'PP', 2, 'Two', '{"kind":"user","id":"alice"}', 'V');
	`); err != nil {
		t.Fatalf("seed project and issues: %v", err)
	}
	var artifactID string
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (project_key, slug, name, kind, created_by)
		values ('PP', 'notes-md', 'notes.md', 'doc', '{"kind":"user","id":"alice"}')
		returning id::text
	`).Scan(&artifactID); err != nil {
		t.Fatalf("create unlinked artifact: %v", err)
	}
	return artifactID
}

func issueUpdated(key string) model.Event {
	return model.Event{
		IssueKey: new(key), Type: "issue.updated", Actor: model.Actor{Kind: "user", ID: "alice"}, Payload: map[string]any{},
	}
}

// A write on one issue must never wait for an open write on another: Append locks only its
// owner's row (Broker.Append). The second write here must commit while the first is still open,
// and its own backend must never queue behind a lock the first holds.
func TestAppendsOnDifferentIssuesDoNotWaitForEachOther(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	seedEventOwners(t, database)
	broker := NewBroker()

	open, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the open write: %v", err)
	}
	defer open.Rollback(context.Background())
	first, err := broker.Append(ctx, open, issueUpdated("PP-1"))
	if err != nil {
		t.Fatalf("append on PP-1: %v", err)
	}

	// The deadline is a ceiling on a hang, not a measure of speed: a write that waits for the
	// open one waits until that one ends, which is only after this test gives up on it.
	const writeDeadline = 5 * time.Second
	writeCtx, cancel := context.WithTimeout(ctx, writeDeadline)
	defer cancel()
	// The second write runs on one connection it holds for its whole transaction, so every lock
	// Append takes is taken on that backend and the watch below sees each of them.
	backends := make(chan uint32, 1)
	written := make(chan error, 1)
	var second model.Event
	go func() {
		tx, err := database.Pool.Begin(writeCtx)
		if err != nil {
			backends <- 0
			written <- err
			return
		}
		defer tx.Rollback(context.Background())
		backends <- tx.Conn().PgConn().PID()
		if second, err = broker.Append(writeCtx, tx, issueUpdated("PP-2")); err != nil {
			written <- err
			return
		}
		written <- tx.Commit(writeCtx)
	}()

	// What says the two writes wait for each other is PP-2's own backend queued behind a lock
	// PP-1's open write holds, directly or behind an earlier waiter
	// (storetest.WaitForQueuedBehind). A count of every backend of this database waiting on any
	// lock answers a different question, and storetest.CountLockWaits' own callers pass a
	// statement pattern for that reason: with the pattern "%" it also reports a waiter neither
	// write made - an autovacuum worker, or any other backend of this cloned database - and
	// fails on it, which is what took this test red on main at 4ca6415a8. That the counted
	// waiter was not PP-2 waiting on PP-1 is certain rather than likely: the count was only
	// read out after PP-2's write had committed, and PP-2 cannot commit while PP-1's write
	// holds a lock it waits on. The watch samples every 2 ms, finer than the 10 ms ticker it
	// replaces but not continuous, and runs until the write answers, so a write that queued and
	// then got through before the deadline is caught; its own ceiling is above the write's, so
	// the write's answer, not the watch, is what ends it.
	pid := <-backends
	answer, answered := storetest.WaitForQueuedBehind(t, open, pid, written, 2*writeDeadline)
	if !answered {
		t.Fatalf("backend %d, the append on PP-2, queued behind a lock PP-1's open write holds; "+
			"an append must wait for no lock of another owner's write", pid)
	}
	if answer != nil {
		t.Fatalf("append on PP-2 while PP-1's write is open: %v", answer)
	}
	var visible bool
	if err := database.Pool.QueryRow(ctx, `select exists (select 1 from events where id = $1)`, second.ID).Scan(&visible); err != nil {
		t.Fatalf("read PP-2's event: %v", err)
	}
	if !visible {
		t.Fatalf("PP-2's event %d is not visible after its commit", second.ID)
	}
	if second.ID <= first.ID {
		t.Fatalf("PP-2's event id %d is not above the open PP-1 event's %d", second.ID, first.ID)
	}
}

// One owner's events must commit in id order, and its sequence with them: a reader of one
// issue's (or document's, or project's) events compares them by id and by seq, and the outbox
// publishes them to the owner's subject in id order. With no database-wide lock, that order
// rests on the owner row alone: a second write on the owner must wait for the first to commit,
// and only then draw its id.
func TestAppendsOnOneOwnerCommitInIDOrder(t *testing.T) {
	database := storetest.Open(t)
	artifactID := seedEventOwners(t, database)
	broker := NewBroker()
	actor := model.Actor{Kind: "user", ID: "alice"}
	for _, owner := range []struct {
		name  string
		event model.Event
	}{
		{name: "issue", event: issueUpdated("PP-1")},
		{name: "unlinked artifact", event: model.Event{
			ArtifactID: new(artifactID), Type: "comment.created", Actor: actor, Payload: map[string]any{},
		}},
		{name: "project", event: model.Event{
			ProjectKey: new("PP"), Type: "project.updated", Actor: actor, Payload: map[string]any{},
		}},
	} {
		t.Run(owner.name, func(t *testing.T) {
			// A ceiling on a hang, not a measure of speed.
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			open, err := database.Pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin the first write: %v", err)
			}
			defer open.Rollback(context.Background())
			first, err := broker.Append(ctx, open, owner.event)
			if err != nil {
				t.Fatalf("first append: %v", err)
			}

			written := make(chan error, 1)
			var second model.Event
			go func() {
				tx, err := database.Pool.Begin(ctx)
				if err != nil {
					written <- err
					return
				}
				defer tx.Rollback(context.Background())
				if second, err = broker.Append(ctx, tx, owner.event); err != nil {
					written <- err
					return
				}
				written <- tx.Commit(ctx)
			}()
			if waiting := storetest.WaitForLockWaiters(t, open, 1, 10*time.Second); waiting < 1 {
				t.Fatalf("the second write on the %s did not wait for the first's open transaction", owner.name)
			}
			if err := open.Commit(ctx); err != nil {
				t.Fatalf("commit the first write: %v", err)
			}
			if err := <-written; err != nil {
				t.Fatalf("second append: %v", err)
			}
			if second.ID <= first.ID || second.Seq != first.Seq+1 {
				t.Fatalf("second write = id %d seq %d, first = id %d seq %d; want a higher id and the next seq",
					second.ID, second.Seq, first.ID, first.Seq)
			}
		})
	}
}

// The safe horizon's proof (horizon.go) needs nextval to hand event ids out in time order across
// every backend, which holds only while events_id_seq caches one value: with a larger cache each
// backend draws from a block of its own, a later write can hold a lower id, and a reader capped at
// the horizon could pass an id that is still open and never read it.
func TestEventIDSequenceCachesOneValue(t *testing.T) {
	database := storetest.Open(t)
	var cache int64
	if err := database.Pool.QueryRow(context.Background(), `
		select seqcache from pg_sequence where seqrelid = pg_get_serial_sequence('events', 'id')::regclass
	`).Scan(&cache); err != nil {
		t.Fatalf("read events_id_seq's cache: %v", err)
	}
	if cache != 1 {
		t.Fatalf("events_id_seq caches %d values, want 1", cache)
	}
}
