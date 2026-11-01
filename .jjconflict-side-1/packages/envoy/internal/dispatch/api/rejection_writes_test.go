package api

import (
	"context"
	"net/http"
	"testing"
)

func TestRejectedConditionalEditWritesNothingDurable(t *testing.T) {
	handler, database, _ := preconditionTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Zero mutation", "before")
	read := readDocumentPrecondition(t, handler, issue.PrimaryArtifactID)

	count := func() (updates, versions, events int) {
		if err := database.Pool.QueryRow(context.Background(), `
			select
				(select count(*) from doc_updates where artifact_id = $1),
				(select count(*) from artifact_versions where artifact_id = $1),
				(select count(*) from events)
		`, issue.PrimaryArtifactID).Scan(&updates, &versions, &events); err != nil {
			t.Fatalf("count durable rows: %v", err)
		}
		return updates, versions, events
	}

	for _, testCase := range []struct {
		name string
		body map[string]any
		want int
	}{
		{"unconditional bad find", map[string]any{
			"ops": []map[string]string{{"op": "replace", "find": "nope", "with": "x"}},
		}, http.StatusNotFound},
		{"malformed precondition", map[string]any{
			"ops":          []map[string]string{{"op": "replace", "find": "before", "with": "x"}},
			"precondition": map[string]any{},
		}, http.StatusBadRequest},
		{"stale precondition", map[string]any{
			"ops":          []map[string]string{{"op": "replace", "find": "before", "with": "x"}},
			"precondition": map[string]string{"document": "sha256:stale"},
		}, http.StatusConflict},
		{"conditional bad find", map[string]any{
			"ops":          []map[string]string{{"op": "replace", "find": "nope", "with": "x"}},
			"precondition": map[string]string{"document": read.Token},
		}, http.StatusNotFound},
	} {
		updatesBefore, versionsBefore, eventsBefore := count()
		response := dispatchRequest(t, handler, http.MethodPost,
			"/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", testCase.body, "alice")
		if response.Code != testCase.want {
			t.Fatalf("%s: status=%d want %d body=%s", testCase.name, response.Code, testCase.want, response.Body.String())
		}
		updatesAfter, versionsAfter, eventsAfter := count()
		if updatesAfter != updatesBefore || versionsAfter != versionsBefore || eventsAfter != eventsBefore {
			t.Fatalf("%s wrote durable rows: doc_updates %d->%d versions %d->%d events %d->%d",
				testCase.name, updatesBefore, updatesAfter, versionsBefore, versionsAfter, eventsBefore, eventsAfter)
		}
	}

	var free bool
	probe, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin lock probe: %v", err)
	}
	defer probe.Rollback(context.Background())
	if err := probe.QueryRow(context.Background(),
		`select pg_try_advisory_xact_lock(hashtext($1))`, issue.PrimaryArtifactID).Scan(&free); err != nil {
		t.Fatalf("probe room lock: %v", err)
	}
	if !free {
		t.Fatal("a rejected conditional edit left the room advisory lock held")
	}
}
