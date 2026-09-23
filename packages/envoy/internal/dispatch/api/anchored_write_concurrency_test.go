package api

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"
)

// Anchoring a comment or an ask stamps a mark in the live document while the handler's
// transaction is open. That transaction holds one pooled connection, so the document work must
// take none: as soon as the concurrent anchored writes fill the pool, a handler that needs a
// second connection waits for one its own peers can only release by finishing, and the server
// never recovers. Real callers reach this whenever enough agents comment on one issue at once.
func TestConcurrentAnchoredWritesDoNotWedgeTheirPool(t *testing.T) {
	const maxConns = 4
	for _, writers := range []int{maxConns, maxConns * 2} {
		t.Run(fmt.Sprintf("writers=%d", writers), func(t *testing.T) {
			handler, _ := pooledDocumentHandler(t, maxConns)
			issue := createInteractionIssue(t, handler, "TEST", "Anchored concurrency",
				"alpha bravo charlie delta echo foxtrot golf hotel")
			quotes := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel"}

			var group sync.WaitGroup
			for index := range writers {
				group.Add(1)
				go func(index int) {
					defer group.Done()
					anchor := map[string]string{
						"artifact": "spec",
						"quote":    quotes[index%len(quotes)],
					}
					if index%2 == 0 {
						dispatchRequest(t, handler, http.MethodPost,
							"/api/v1/issues/"+issue.Key+"/comments",
							map[string]any{"anchor": anchor, "body": fmt.Sprintf("comment %d", index)}, "alice")
						return
					}
					dispatchRequest(t, handler, http.MethodPost,
						"/api/v1/issues/"+issue.Key+"/asks",
						map[string]any{
							"anchor":   anchor,
							"options":  []map[string]string{{"label": "Yes"}},
							"question": fmt.Sprintf("ask %d?", index),
						}, "alice")
				}(index)
			}

			finished := make(chan struct{})
			go func() {
				group.Wait()
				close(finished)
			}()
			select {
			case <-finished:
			case <-time.After(30 * time.Second):
				t.Fatalf("%d concurrent anchored writes wedged a %d-connection pool", writers, maxConns)
			}
		})
	}
}
