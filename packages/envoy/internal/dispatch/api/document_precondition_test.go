package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

type documentPreconditionRead struct {
	Markdown string `json:"markdown"`
	Token    string `json:"token"`
}

type documentPreconditionBlock struct {
	ID    string `json:"id"`
	Token string `json:"token"`
}

type documentPreconditionMismatch struct {
	Scope    string  `json:"scope"`
	BlockID  string  `json:"block_id,omitempty"`
	Expected string  `json:"expected"`
	Current  *string `json:"current"`
}

type documentPreconditionConflict struct {
	Code    string `json:"code"`
	Current struct {
		Document string                      `json:"document"`
		Blocks   []documentPreconditionBlock `json:"blocks"`
	} `json:"current"`
	Mismatches []documentPreconditionMismatch `json:"mismatches"`
}

func preconditionTestHandler(t *testing.T) (http.Handler, *store.Store, *docs.Service) {
	t.Helper()
	var service *docs.Service
	handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
		service = docs.New(docs.Deps{Store: database, Settle: time.Hour})
		t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
		return service
	})
	return handler, database, service
}

func readDocumentPrecondition(t *testing.T, handler http.Handler, artifactID string) documentPreconditionRead {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifactID+"/text", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read document: status=%d body=%s", response.Code, response.Body.String())
	}
	read := decodeBody[documentPreconditionRead](t, response)
	if read.Token == "" {
		t.Fatal("document read omitted its optimistic-concurrency token")
	}
	return read
}

func readDocumentPreconditionBlocks(t *testing.T, handler http.Handler, artifactID string) []documentPreconditionBlock {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifactID+"/blocks", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read blocks: status=%d body=%s", response.Code, response.Body.String())
	}
	blocks := decodeBody[[]documentPreconditionBlock](t, response)
	for _, block := range blocks {
		if block.ID == "" || block.Token == "" {
			t.Fatalf("block precondition token = %#v, want id and token", block)
		}
	}
	return blocks
}

func documentMutationCounts(t *testing.T, database *store.Store, artifactID string) (versions, events int) {
	t.Helper()
	if err := database.Pool.QueryRow(context.Background(), `
		select
			(select count(*) from artifact_versions where artifact_id = $1),
			(select count(*) from events)
	`, artifactID).Scan(&versions, &events); err != nil {
		t.Fatalf("count document mutation rows: %v", err)
	}
	return versions, events
}

func TestDocumentEditPreconditionRejectsStaleDocumentWithoutMutation(t *testing.T) {
	handler, database, _ := preconditionTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Document precondition", "before")

	stale := readDocumentPrecondition(t, handler, issue.PrimaryArtifactID)
	writer := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops": []map[string]string{{"op": "replace", "find": "before", "with": "writer"}},
	}, "alice")
	if writer.Code != http.StatusOK {
		t.Fatalf("write current document: status=%d body=%s", writer.Code, writer.Body.String())
	}
	current := readDocumentPrecondition(t, handler, issue.PrimaryArtifactID)
	versionsBefore, eventsBefore := documentMutationCounts(t, database, issue.PrimaryArtifactID)

	rejected := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops":          []map[string]string{{"op": "replace", "find": "writer", "with": "late"}},
		"precondition": map[string]string{"document": stale.Token},
	}, "alice")
	if rejected.Code != http.StatusConflict {
		t.Fatalf("stale precondition: status=%d body=%s", rejected.Code, rejected.Body.String())
	}
	conflict := decodeBody[documentPreconditionConflict](t, rejected)
	if conflict.Code != "PRECONDITION_FAILED" || conflict.Current.Document != current.Token || len(conflict.Mismatches) != 1 || conflict.Mismatches[0].Scope != "document" || conflict.Mismatches[0].Expected != stale.Token || conflict.Mismatches[0].Current == nil || *conflict.Mismatches[0].Current != current.Token {
		t.Fatalf("stale precondition conflict = %#v", conflict)
	}
	if got := readDocumentPrecondition(t, handler, issue.PrimaryArtifactID); got.Markdown != "writer\n" {
		t.Fatalf("document after rejected edit = %q, want writer", got.Markdown)
	}
	versionsAfter, eventsAfter := documentMutationCounts(t, database, issue.PrimaryArtifactID)
	if versionsAfter != versionsBefore || eventsAfter != eventsBefore {
		t.Fatalf("rejected precondition mutated versions/events: got %d/%d, want %d/%d", versionsAfter, eventsAfter, versionsBefore, eventsBefore)
	}

	fresh := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops":          []map[string]string{{"op": "replace", "find": "writer", "with": "fresh"}},
		"precondition": map[string]string{"document": current.Token},
	}, "alice")
	if fresh.Code != http.StatusOK {
		t.Fatalf("fresh precondition: status=%d body=%s", fresh.Code, fresh.Body.String())
	}
	if got := readDocumentPrecondition(t, handler, issue.PrimaryArtifactID); got.Markdown != "fresh\n" {
		t.Fatalf("document after fresh precondition = %q, want fresh", got.Markdown)
	}
}

func TestDocumentEditWithoutPreconditionRemainsUnconditional(t *testing.T) {
	handler, _, _ := preconditionTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Unconditional document edit", "before")

	for _, edit := range []map[string]string{
		{"op": "replace", "find": "before", "with": "first"},
		{"op": "replace", "find": "first", "with": "second"},
	} {
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
			"ops": []map[string]string{edit},
		}, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("unconditional edit %q: status=%d body=%s", edit["with"], response.Code, response.Body.String())
		}
	}
	if got := readDocumentPrecondition(t, handler, issue.PrimaryArtifactID); got.Markdown != "second\n" {
		t.Fatalf("unconditional edits = %q, want second", got.Markdown)
	}
}

func TestDocumentEditBlockPreconditionsAllowIndependentEdits(t *testing.T) {
	handler, _, _ := preconditionTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Independent blocks", "alpha\n\nbeta")
	blocks := readDocumentPreconditionBlocks(t, handler, issue.PrimaryArtifactID)
	if len(blocks) != 2 {
		t.Fatalf("blocks = %#v, want alpha and beta", blocks)
	}

	first := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops":          []map[string]string{{"op": "replace", "find": "alpha", "with": "ALPHA"}},
		"precondition": map[string]any{"blocks": []map[string]string{{"id": blocks[0].ID, "token": blocks[0].Token}}},
	}, "alice")
	if first.Code != http.StatusOK {
		t.Fatalf("first independent edit: status=%d body=%s", first.Code, first.Body.String())
	}
	stale := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops":          []map[string]string{{"op": "replace", "find": "ALPHA", "with": "late"}},
		"precondition": map[string]any{"blocks": []map[string]string{{"id": blocks[0].ID, "token": blocks[0].Token}}},
	}, "alice")
	if stale.Code != http.StatusConflict {
		t.Fatalf("stale block precondition: status=%d body=%s", stale.Code, stale.Body.String())
	}
	conflict := decodeBody[documentPreconditionConflict](t, stale)
	if len(conflict.Mismatches) != 1 || conflict.Mismatches[0].Scope != "block" || conflict.Mismatches[0].BlockID != blocks[0].ID || conflict.Mismatches[0].Expected != blocks[0].Token || conflict.Mismatches[0].Current == nil || len(conflict.Current.Blocks) != 1 || conflict.Current.Blocks[0].ID != blocks[0].ID || conflict.Current.Blocks[0].Token != *conflict.Mismatches[0].Current {
		t.Fatalf("stale block conflict = %#v", conflict)
	}
	second := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops":          []map[string]string{{"op": "replace", "find": "beta", "with": "BETA"}},
		"precondition": map[string]any{"blocks": []map[string]string{{"id": blocks[1].ID, "token": blocks[1].Token}}},
	}, "alice")
	if second.Code != http.StatusOK {
		t.Fatalf("second independent edit: status=%d body=%s", second.Code, second.Body.String())
	}
	if got := readDocumentPrecondition(t, handler, issue.PrimaryArtifactID); got.Markdown != "ALPHA\n\nBETA\n" {
		t.Fatalf("independent edits = %q, want both edits", got.Markdown)
	}
}

func TestDocumentEditPreconditionRejectsWriterAfterDurableAppendRace(t *testing.T) {
	handler, database, service := preconditionTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Precondition race", "before")
	stale := readDocumentPrecondition(t, handler, issue.PrimaryArtifactID)

	blocker, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin document lock holder: %v", err)
	}
	defer blocker.Rollback(context.Background())
	if _, err := blocker.Exec(context.Background(), `select pg_advisory_xact_lock(hashtext($1))`, issue.PrimaryArtifactID); err != nil {
		t.Fatalf("lock document room: %v", err)
	}

	writerDone := make(chan error, 1)
	go func() {
		_, err := service.ApplyOps(context.Background(), issue.PrimaryArtifactID, []model.EditOp{{
			Op: "replace", Find: "before", With: "writer",
		}}, model.Actor{Kind: "user", ID: "alice"})
		writerDone <- err
	}()
	waitForDatabaseLocks(t, blocker, 1)

	conditionalDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		conditionalDone <- dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
			"ops":          []map[string]string{{"op": "replace", "find": "writer", "with": "late"}},
			"precondition": map[string]string{"document": stale.Token},
		}, "alice")
	}()
	waitForDatabaseLocks(t, blocker, 2)
	if err := blocker.Commit(context.Background()); err != nil {
		t.Fatalf("release document lock: %v", err)
	}
	if err := <-writerDone; err != nil {
		t.Fatalf("concurrent writer: %v", err)
	}
	response := awaitResponse(t, conditionalDone)
	if response.Code != http.StatusConflict {
		t.Fatalf("late conditional writer: status=%d body=%s", response.Code, response.Body.String())
	}
	conflict := decodeBody[documentPreconditionConflict](t, response)
	if conflict.Code != "PRECONDITION_FAILED" || conflict.Current.Document == stale.Token {
		t.Fatalf("race conflict = %#v, want current token after writer", conflict)
	}
	if got := readDocumentPrecondition(t, handler, issue.PrimaryArtifactID); got.Markdown != "writer\n" {
		t.Fatalf("race document = %q, want writer only", got.Markdown)
	}
}
