package main

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/api"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The boot's repair withdraws the open ask a close by an older release left on a closed issue,
// naming that close's actor, and logs its counts.
func TestRepairIssueAsksAtBootWithdrawsAndLogsItsCounts(t *testing.T) {
	ctx := store.WithTransactionTracking(context.Background())
	database := storetest.Open(t)
	for _, query := range []string{
		`insert into projects (key, name) values ('TEST', 'Test')`,
		`insert into issues (key, project_key, number, title, status, created_by, rank, closed_at) values ('TEST-1', 'TEST', 1, 'Closed issue', 'done', '{"kind":"user","id":"alice"}', 'U', now() - interval '1 hour')`,
	} {
		if _, err := database.Pool.Exec(ctx, query); err != nil {
			t.Fatalf("seed the closed issue: %v", err)
		}
	}
	var ask string
	if err := database.Pool.QueryRow(ctx, `
		insert into asks (issue_key, author, question, state) values ('TEST-1', '{"kind":"session","id":"planner"}', 'Still open?', 'open') returning id::text
	`).Scan(&ask); err != nil {
		t.Fatalf("seed the open ask: %v", err)
	}
	broker := events.NewBroker()
	issueKey := "TEST-1"
	closer := model.Actor{Kind: "user", ID: "alice"}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	opened := model.NewAskEventPayload(model.Ask{
		ID: ask, IssueKey: &issueKey, Author: model.Actor{Kind: "session", ID: "planner"}, Question: "Still open?", State: "open",
	}, model.ReferenceChanges{})
	for _, event := range []model.Event{
		{IssueKey: &issueKey, Type: "ask.opened", Actor: model.Actor{Kind: "session", ID: "planner"}, Payload: opened},
		{IssueKey: &issueKey, Type: "issue.closed", Actor: closer, Payload: map[string]any{"key": issueKey, "status": "done"}},
	} {
		if _, err := broker.Append(ctx, tx, event); err != nil {
			t.Fatalf("append %s: %v", event.Type, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	documents := docs.New(docs.Deps{Store: database, Events: broker})
	t.Cleanup(func() { _ = documents.Shutdown(context.Background()) })
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	repairIssueAsks(ctx, api.Deps{Store: database, Docs: documents, Events: broker}, issueAskRepairBootTimeout)

	if out := logs.String(); !strings.Contains(out, "repaired the asks of closed and reopened issues") || !strings.Contains(out, "withdrawn=1 restored=0 swept=0") {
		t.Errorf("logged, want the counts withdrawn=1 restored=0 swept=0:\n%s", out)
	}
	var state, kind, actor string
	if err := database.Pool.QueryRow(ctx, `
		select state, resolution->>'kind', resolution->'actor'->>'id' from asks where issue_key = $1
	`, issueKey).Scan(&state, &kind, &actor); err != nil {
		t.Fatalf("read the ask: %v", err)
	}
	if state != "resolved" || kind != "withdrawn" || actor != closer.ID {
		t.Errorf("the ask = %s %s by %s, want resolved withdrawn by %s", state, kind, actor, closer.ID)
	}
	if count := 0; database.Pool.QueryRow(ctx, `select count(*) from events where type = 'ask.resolved' and payload->>'id' = $1`, ask).Scan(&count) != nil || count != 1 {
		t.Errorf("the ask has %d ask.resolved events, want 1", count)
	}
}
