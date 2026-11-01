package api

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// pooledDocumentHandler serves a Dispatch API whose pool is maxConns wide and whose documents
// settle after settle, and returns the document service so a test can evict or settle a room.
func pooledDocumentHandler(t *testing.T, maxConns int32, settle time.Duration) (http.Handler, *docs.Service) {
	handler, service, _ := pooledDocumentHandlerWith(t, maxConns, settle, nil)
	return handler, service
}

// pooledDocumentHandlerWith lets a test wrap the document service the API sees, so it can hold
// one document call open while it arranges the rest of the pool.
func pooledDocumentHandlerWith(
	t *testing.T,
	maxConns int32,
	settle time.Duration,
	wrap func(*docs.Service) docs.API,
) (http.Handler, *docs.Service, *store.Store) {
	t.Helper()
	var service *docs.Service
	handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
		config := database.Pool.Config().Copy()
		config.MaxConns = maxConns
		database.Pool.Close()
		pool, err := pgxpool.NewWithConfig(context.Background(), config)
		if err != nil {
			t.Fatalf("open test pool: %v", err)
		}
		database.Pool = store.NewPool(pool)
		t.Cleanup(pool.Close)
		service = docs.New(docs.Deps{Store: database, Settle: settle})
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := service.Shutdown(ctx); err != nil {
				t.Errorf("shutdown document service: %v", err)
			}
		})
		if wrap != nil {
			return wrap(service)
		}
		return service
	})
	return handler, service, database
}

func TestConditionalEditFloodDoesNotStarveSmallPool(t *testing.T) {
	handler, _ := pooledDocumentHandler(t, 2, time.Hour)
	issue := createInteractionIssue(t, handler, "TEST", "Conditional flood", "alpha")
	for round := range 40 {
		read := readDocumentPrecondition(t, handler, issue.PrimaryArtifactID)
		var group sync.WaitGroup
		for index := range 6 {
			group.Add(1)
			go func(index int) {
				defer group.Done()
				body := map[string]any{
					"ops": []map[string]string{{
						"op":       "insert",
						"markdown": fmt.Sprintf("r%d-%d", round, index),
						"after":    "end",
					}},
					"precondition": map[string]string{"document": read.Token},
				}
				dispatchRequest(t, handler, http.MethodPost,
					"/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", body, "alice")
			}(index)
		}
		finished := make(chan struct{})
		go func() {
			group.Wait()
			close(finished)
		}()
		select {
		case <-finished:
		case <-time.After(20 * time.Second):
			t.Fatalf("round %d wedged: conditional edit flood did not return", round)
		}
	}
}

func TestConcurrentConditionalAndUnconditionalEditsDoNotWedge(t *testing.T) {
	// Unconditional edits hold a transaction connection before warm-up and wedge
	// at pool_max_conns≈3 even on c34767d4; the follow-up fix owns that baseline.
	handler, _ := pooledDocumentHandler(t, 8, time.Hour)
	issue := createInteractionIssue(t, handler, "TEST", "Mixed concurrency", "alpha")
	for round := range 40 {
		read := readDocumentPrecondition(t, handler, issue.PrimaryArtifactID)
		var group sync.WaitGroup
		for index := range 6 {
			group.Add(1)
			go func(index int) {
				defer group.Done()
				body := map[string]any{
					"ops": []map[string]string{{
						"op":       "insert",
						"markdown": fmt.Sprintf("mixed-%d-%d", round, index),
						"after":    "end",
					}},
				}
				if index%2 == 0 {
					body["precondition"] = map[string]string{"document": read.Token}
				}
				dispatchRequest(t, handler, http.MethodPost,
					"/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", body, "alice")
			}(index)
		}
		finished := make(chan struct{})
		go func() {
			group.Wait()
			close(finished)
		}()
		select {
		case <-finished:
		case <-time.After(20 * time.Second):
			t.Fatalf("round %d wedged: concurrent conditional and unconditional edits did not return", round)
		}
	}
}
