package api

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// An approval request reads the document's approval twice inside its own transaction, once
// before and once after it opens or renews the ask, each a batch on that transaction
// (attachApproval, queueApprovals). Run in sequence and concurrently, on documents of their own
// and on one document, every request finishes, none fails, each document is left awaiting its
// one open request, and the transactions give every connection back.
func TestApprovalRequestsReadTheirApprovalInTheirTransactionInSequenceAndAtOnce(t *testing.T) {
	handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
		documents := docs.New(docs.Deps{Store: database, Settle: time.Hour})
		t.Cleanup(func() { _ = documents.Shutdown(context.Background()) })
		return documents
	})
	first := createInteractionIssue(t, handler, "APPR", "Approve the first", "A spec")
	documents := []string{first.PrimaryArtifactID}
	for range 7 {
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": "APPR", "title": "Approve another", "spec": "A spec", "force": true,
		}, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("create issue: status=%d body=%s", response.Code, response.Body.String())
		}
		documents = append(documents, decodeBody[struct {
			PrimaryArtifactID string `json:"primary_artifact_id"`
		}](t, response).PrimaryArtifactID)
	}
	request := func(artifactID string) int {
		return sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/approval-requests",
			map[string]any{"actor": sessionActor()}).Code
	}
	// atOnce sends each of targets' requests at the same moment and answers their statuses, or
	// fails when one has not answered within a bound no healthy transaction reaches.
	atOnce := func(targets []string) []int {
		t.Helper()
		statuses := make([]int, len(targets))
		var started, finished sync.WaitGroup
		started.Add(1)
		for index, artifactID := range targets {
			finished.Add(1)
			go func() {
				defer finished.Done()
				started.Wait()
				statuses[index] = request(artifactID)
			}()
		}
		done := make(chan struct{})
		go func() { finished.Wait(); close(done) }()
		started.Done()
		select {
		case <-done:
		case <-time.After(90 * time.Second):
			t.Fatalf("approval requests sent at once did not all answer within 90 s")
		}
		return statuses
	}

	// In sequence: the first request opens the ask, and a repeat answers the open one.
	if code := request(documents[0]); code != http.StatusCreated {
		t.Fatalf("first request: status=%d, want 201", code)
	}
	if code := request(documents[0]); code != http.StatusOK {
		t.Fatalf("repeat request: status=%d, want 200", code)
	}
	// At once, one each on seven documents: every one opens its document's ask.
	for index, code := range atOnce(documents[1:]) {
		if code != http.StatusCreated {
			t.Fatalf("request %d on its own document: status=%d, want 201", index, code)
		}
	}
	// At once, sixteen on one document that already has its request open: none fails.
	same := make([]string, 16)
	for index := range same {
		same[index] = documents[1]
	}
	for index, code := range atOnce(same) {
		if code != http.StatusOK && code != http.StatusConflict {
			t.Fatalf("request %d on one document: status=%d, want 200 or 409", index, code)
		}
	}
	for _, artifactID := range documents {
		if got := readApproval(t, handler, artifactID); got.Approval == nil || got.Approval.State != "awaiting" || got.Approval.AskID == nil {
			t.Fatalf("document %s approval = %#v, want awaiting its open request", artifactID, got.Approval)
		}
	}
	if use := database.Pool.Use(); use.InUse != 0 {
		t.Fatalf("connections still in use after every request answered: %d", use.InUse)
	}
}
