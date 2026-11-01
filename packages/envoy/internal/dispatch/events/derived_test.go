package events

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A write phase appends its events in one batch (QueueAppend), so what an append used to read back
// in Go before its next statement - the owner's last_seq, the inserted row's id and time - is
// derived in SQL instead. Each derived value is checked here against the one the client computed
// from what it read: the sequence of every event of one batch, on each kind of owner, continues
// the owner's last_seq without a gap and leaves last_seq at the last one; an issue's updated_at is
// the greater of what it held and its event's created_at; and the appended event the callback
// hands back is the row the table holds.
func TestDerivedEventSequencesAndOwnerRowsMatchTheRowsAppended(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name, last_seq) values ('DV', 'Derived', 4);
		insert into issues (key, project_key, number, title, created_by, rank, last_seq, updated_at)
		values ('DV-1', 'DV', 1, 'Issue', '{"kind":"user","id":"alice"}', 'U', 7, now() - interval '1 day');
		insert into issues (key, project_key, number, title, created_by, rank, last_seq, updated_at)
		values ('DV-2', 'DV', 2, 'Future', '{"kind":"user","id":"alice"}', 'V', 0, now() + interval '1 day');
	`); err != nil {
		t.Fatalf("seed owners: %v", err)
	}
	var artifactID string
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (project_key, slug, name, kind, created_by, last_seq)
		values ('DV', 'notes-md', 'notes.md', 'doc', '{"kind":"user","id":"alice"}', 2)
		returning id::text
	`).Scan(&artifactID); err != nil {
		t.Fatalf("create unlinked artifact: %v", err)
	}
	var futureUpdatedAt time.Time
	if err := database.Pool.QueryRow(ctx, `select updated_at from issues where key = 'DV-2'`).Scan(&futureUpdatedAt); err != nil {
		t.Fatalf("read the future issue: %v", err)
	}
	alice := model.Actor{Kind: "user", ID: "alice"}
	planned := []model.Event{
		{IssueKey: new("DV-1"), Type: "test.first", Actor: alice, Payload: map[string]any{}},
		{ArtifactID: new(artifactID), Type: "test.document", Actor: alice, Payload: map[string]any{}},
		{IssueKey: new("DV-1"), Type: "test.second", Actor: alice, Payload: map[string]any{}},
		{ProjectKey: new("DV"), Type: "test.project", Actor: alice, Payload: map[string]any{}},
		{IssueKey: new("DV-2"), Type: "test.future", Actor: alice, Payload: map[string]any{}},
		{ArtifactID: new(artifactID), Type: "test.document.again", Actor: alice, Payload: map[string]any{}},
		{IssueKey: new("DV-1"), Type: "test.third", Actor: alice, Payload: map[string]any{}},
	}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	broker := NewBroker()
	appended := make([]model.Event, len(planned))
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		for index, event := range planned {
			if err := broker.QueueAppend(b, event, func(written model.Event) { appended[index] = written }); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("append the batch: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	wantSeq := []int{8, 3, 9, 5, 1, 4, 10}
	for index, event := range appended {
		if event.Seq != wantSeq[index] {
			t.Errorf("event %d (%s) has seq %d, want %d: one past the owner's last_seq as the batch left it", index, event.Type, event.Seq, wantSeq[index])
		}
		if event.Project != "DV" {
			t.Errorf("event %d (%s) names project %q, want DV", index, event.Type, event.Project)
		}
		var id int64
		var seq int
		var createdAt time.Time
		if err := database.Pool.QueryRow(ctx, `select id, seq, created_at from events where type = $1`, event.Type).Scan(&id, &seq, &createdAt); err != nil {
			t.Fatalf("read event %s: %v", event.Type, err)
		}
		if id != event.ID || seq != event.Seq || !createdAt.Equal(event.CreatedAt) {
			t.Errorf("event %s appended as id %d seq %d at %s, stored as id %d seq %d at %s", event.Type, event.ID, event.Seq, event.CreatedAt, id, seq, createdAt)
		}
		if index > 0 && event.ID <= appended[index-1].ID {
			t.Errorf("event %d has id %d, not after the one queued before it (%d)", index, event.ID, appended[index-1].ID)
		}
	}
	var issueUpdatedAt time.Time
	if err := database.Pool.QueryRow(ctx, `select updated_at from issues where key = 'DV-1'`).Scan(&issueUpdatedAt); err != nil {
		t.Fatalf("read DV-1: %v", err)
	}
	lastSeqs := map[string]int{}
	for _, owner := range []struct{ name, query string }{
		{"DV-1", `select last_seq from issues where key = 'DV-1'`},
		{"DV-2", `select last_seq from issues where key = 'DV-2'`},
		{"artifact", `select last_seq from artifacts where id = '` + artifactID + `'`},
		{"project", `select last_seq from projects where key = 'DV'`},
	} {
		var lastSeq int
		if err := database.Pool.QueryRow(ctx, owner.query).Scan(&lastSeq); err != nil {
			t.Fatalf("read %s's last_seq: %v", owner.name, err)
		}
		lastSeqs[owner.name] = lastSeq
	}
	if lastSeqs["DV-1"] != 10 || lastSeqs["DV-2"] != 1 || lastSeqs["artifact"] != 4 || lastSeqs["project"] != 5 {
		t.Errorf("owners' last_seq = %v, want each at its last event's sequence (DV-1 10, DV-2 1, artifact 4, project 5)", lastSeqs)
	}
	if !issueUpdatedAt.Equal(appended[6].CreatedAt) {
		t.Errorf("DV-1 updated_at = %s, want its events' created_at %s, which is later than what it held", issueUpdatedAt, appended[6].CreatedAt)
	}
	var futureAfter time.Time
	if err := database.Pool.QueryRow(ctx, `select updated_at from issues where key = 'DV-2'`).Scan(&futureAfter); err != nil {
		t.Fatalf("read the future issue: %v", err)
	}
	if !futureAfter.Equal(futureUpdatedAt) {
		t.Errorf("DV-2 updated_at = %s, want the later %s it held, kept by greatest()", futureAfter, futureUpdatedAt)
	}
}

// An ask.opened event's payload records the event's own id (opened_event_id), which the batch
// writes through the id sequence's currval rather than an id read back in Go: the stored payload
// is the one the old append stored - the ask's payload encoded with opened_event_id set to the id -
// and so is the payload the appended event carries.
func TestDerivedAskOpenedPayloadEqualsTheEncodedPayloadWithItsEventID(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name) values ('DA', 'Derived asks');
		insert into issues (key, project_key, number, title, created_by, rank)
		values ('DA-1', 'DA', 1, 'Issue', '{"kind":"user","id":"alice"}', 'U');
	`); err != nil {
		t.Fatalf("seed owners: %v", err)
	}
	alice := model.Actor{Kind: "user", ID: "alice"}
	ask := model.Ask{
		ID: "6f9619ff-8b86-4d11-b42d-00c04fc964ff", IssueKey: new("DA-1"), Author: alice, Question: "Ship it?",
		Options: []model.AskOption{{Label: "Yes"}, {Label: "No"}}, Urgency: "med", State: "open", Kind: "question",
		CreatedAt: time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC),
	}
	opened := model.NewAskEventPayload(ask, model.ReferenceChanges{})
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	broker := NewBroker()
	// A second event after it in the same batch: its own insert moves currval on, which must not
	// reach the payload already recorded.
	var event, after model.Event
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		if err := broker.QueueAppend(b, model.Event{IssueKey: new("DA-1"), Type: "ask.opened", Actor: alice, Payload: opened}, func(written model.Event) { event = written }); err != nil {
			return err
		}
		return broker.QueueAppend(b, model.Event{IssueKey: new("DA-1"), Type: "test.after", Actor: alice, Payload: map[string]any{}}, func(written model.Event) { after = written })
	}); err != nil {
		t.Fatalf("append the ask's event: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if after.ID <= event.ID {
		t.Fatalf("the event after has id %d, not after the ask's %d", after.ID, event.ID)
	}
	want := opened
	want.OpenedEventID = &event.ID
	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("encode the payload the old append stored: %v", err)
	}
	var equal bool
	var storedID int64
	if err := database.Pool.QueryRow(ctx, `
		select payload = $2::jsonb, (payload->>'opened_event_id')::bigint from events where id = $1
	`, event.ID, string(encoded)).Scan(&equal, &storedID); err != nil {
		t.Fatalf("read the stored payload: %v", err)
	}
	if storedID != event.ID {
		t.Errorf("stored opened_event_id = %d, want the event's id %d", storedID, event.ID)
	}
	if !equal {
		var stored string
		_ = database.Pool.QueryRow(ctx, `select payload::text from events where id = $1`, event.ID).Scan(&stored)
		t.Errorf("stored payload differs from the old encoding:\n stored %s\n want   %s", stored, encoded)
	}
	carried, ok := event.Payload.(model.AskEventPayload)
	if !ok || carried.OpenedEventID == nil || *carried.OpenedEventID != event.ID {
		t.Fatalf("the appended event carries payload %#v, want the ask's payload naming its own id %d", event.Payload, event.ID)
	}
	carriedJSON, err := json.Marshal(carried)
	if err != nil {
		t.Fatalf("encode the carried payload: %v", err)
	}
	if string(carriedJSON) != string(encoded) {
		t.Errorf("the appended event's payload encodes as\n %s\nwant\n %s", carriedJSON, encoded)
	}
}
