package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/admission"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// writerGateHandler serves the API on a request pool of maxConns connections, with limiter
// admitting its requests (nil is the default limits), and lets a test wrap the document service the
// API sees. Settlement waits an hour, so no settlement takes a connection a test counts.
func writerGateHandler(
	t *testing.T, maxConns int32, limiter *admission.Limiter, wrap func(*docs.Service) docs.API,
) (http.Handler, *docs.Service, *store.Store) {
	t.Helper()
	database := storetest.Open(t)
	config := database.Pool.Config().Copy()
	config.MaxConns = maxConns
	database.Pool.Close()
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatalf("open test pool: %v", err)
	}
	database.Pool = store.NewPool(pool)
	t.Cleanup(pool.Close)
	service := docs.New(docs.Deps{Store: database, Settle: time.Hour})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := service.Shutdown(ctx); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	var documents docs.API = service
	if wrap != nil {
		documents = wrap(service)
	}
	seedPeople(t, database, "alice", "bob")
	deps, err := NewDeps(DepsInput{
		Store:       database,
		Identity:    headerIdentity(database),
		AgentTokens: sharedAgentTokens(t, "agent-token"),
		Docs:        documents,
		ServerURL:   "https://dispatch.example",
		Admission:   limiter,
	})
	if err != nil {
		t.Fatalf("new API dependencies: %v", err)
	}
	mux := http.NewServeMux()
	t.Cleanup(Register(mux, t.Context(), deps).Wait)
	return withDeadline(mux), service, database
}

// waitForQueuedWriters waits until want writers are queued for a document's writer gate.
func waitForQueuedWriters(t *testing.T, service *docs.Service, want int64) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for service.QueuedDocumentWriters() != want {
		if time.Now().After(deadline) {
			t.Fatalf("queued document writers = %d, want %d", service.QueuedDocumentWriters(), want)
		}
		time.Sleep(time.Millisecond)
	}
}

// createSecondDocument uploads a second document to issue key and returns its id.
func createSecondDocument(t *testing.T, handler http.Handler, key string) string {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/artifacts", map[string]any{
		"content": "# Other\n\nother body\n", "name": "other.md",
	}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create the second document: status=%d body=%s", response.Code, response.Body.String())
	}
	return decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, response).Artifact.ID
}

// queuedEdits sends count edits to artifactID one after another, each once the one before it is
// queued for the document's writer gate, and returns the answers they get once wait runs.
func queuedEdits(t *testing.T, handler http.Handler, service *docs.Service, artifactID string, count int) (wait func() []*httptest.ResponseRecorder) {
	t.Helper()
	responses := make([]*httptest.ResponseRecorder, count)
	var edits sync.WaitGroup
	base := service.QueuedDocumentWriters()
	for index := range count {
		edits.Add(1)
		go func() {
			defer edits.Done()
			responses[index] = editDocument(t, handler, artifactID, insertAtEnd(fmt.Sprintf("Queued line %d.", index)))
		}()
		waitForQueuedWriters(t, service, base+int64(index+1))
	}
	return func() []*httptest.ResponseRecorder {
		edits.Wait()
		return responses
	}
}

// requireEditsLanded fails the test for each of responses that is not a landed edit's 200.
func requireEditsLanded(t *testing.T, responses []*httptest.ResponseRecorder) {
	t.Helper()
	for index, response := range responses {
		if response.Code != http.StatusOK {
			t.Errorf("queued edit %d: status=%d body=%s, want 200", index, response.Code, response.Body.String())
		}
	}
}

// Anchored writes queued for one document wait in its writer gate, ahead of their transactions,
// so they hold no connection: before the gate each held one while it waited for the issue row and
// the writer slot, and writers clustered on one document took the whole pool.
func TestAnchoredWritesQueuedForADocumentHoldNoConnection(t *testing.T) {
	gate := &gatedMarkQuote{entered: make(chan struct{}), release: make(chan struct{})}
	handler, service, database := writerGateHandler(t, 16, nil, func(service *docs.Service) docs.API {
		gate.API = service
		return gate
	})
	var released sync.Once
	release := func() { released.Do(func() { close(gate.release) }) }
	defer release()
	issue := createInteractionIssue(t, handler, "TEST", "Queued anchored writes",
		"alpha bravo charlie delta echo foxtrot golf hotel india")
	quotes := []string{"bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel", "india"}

	statuses := make([]int, len(quotes)+1)
	var writes sync.WaitGroup
	anchored := func(index int, quote string) {
		defer writes.Done()
		statuses[index] = dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments",
			map[string]any{
				"anchor": map[string]string{"artifact": "spec", "quote": quote},
				"body":   fmt.Sprintf("comment %d", index),
			}, "alice").Code
	}
	writes.Add(1)
	go anchored(0, "alpha")
	<-gate.entered // the first write holds the document's gate, its transaction and the issue row
	before := database.Pool.Stat().AcquiredConns()

	for index, quote := range quotes {
		writes.Add(1)
		go anchored(index+1, quote)
		waitForQueuedWriters(t, service, int64(index+1))
	}
	if acquired := database.Pool.Stat().AcquiredConns(); acquired > before+1 {
		t.Errorf("%d anchored writes queued for the document raised the pool's acquired connections from %d to %d, want at most %d",
			len(quotes), before, acquired, before+1)
	}
	release()
	writes.Wait()
	for index, status := range statuses {
		if status != http.StatusCreated {
			t.Errorf("anchored write %d: status=%d, want 201", index, status)
		}
	}
}

// A document's writer gate queues at most 32 writers behind its holder; the next is answered 429
// EDIT_QUEUE_FULL with Retry-After before it opens a transaction, and the queued ones still land.
func TestTheWriterQueuedPastTheBoundIsRefusedWithRetryAfter(t *testing.T) {
	handler, service, _ := writerGateHandler(t, 8, nil, nil)
	issue := createInteractionIssue(t, handler, "TEST", "Writer queue bound", "First line.")
	_, hold, err := service.AcquireDocumentWriter(context.Background(), issue.PrimaryArtifactID)
	if err != nil {
		t.Fatalf("hold the document's writer gate: %v", err)
	}
	var held sync.Once
	releaseHold := func() { held.Do(hold) }
	defer releaseHold()
	const bound = 32
	wait := queuedEdits(t, handler, service, issue.PrimaryArtifactID, bound)

	refused := editDocument(t, handler, issue.PrimaryArtifactID, insertAtEnd("One writer too many."))
	if refused.Code != http.StatusTooManyRequests || decodeBody[struct {
		Code string `json:"code"`
	}](t, refused).Code != "EDIT_QUEUE_FULL" {
		t.Errorf("writer %d queued for the document: status=%d body=%s, want 429 EDIT_QUEUE_FULL", bound+1, refused.Code, refused.Body.String())
	}
	if retryAfter := refused.Header().Get("Retry-After"); retryAfter != admission.RetryAfter {
		t.Errorf("Retry-After = %q, want %q", retryAfter, admission.RetryAfter)
	}
	releaseHold()
	requireEditsLanded(t, wait())
}

// Writers queued for one document do not hold up a write to another.
func TestAWriteToAnotherDocumentProceedsWhileWritersQueueOnTheFirst(t *testing.T) {
	handler, service, _ := writerGateHandler(t, 8, nil, nil)
	issue := createInteractionIssue(t, handler, "TEST", "Writers on two documents", "First line.")
	other := createSecondDocument(t, handler, issue.Key)
	_, hold, err := service.AcquireDocumentWriter(context.Background(), issue.PrimaryArtifactID)
	if err != nil {
		t.Fatalf("hold the document's writer gate: %v", err)
	}
	var held sync.Once
	releaseHold := func() { held.Do(hold) }
	defer releaseHold()
	wait := queuedEdits(t, handler, service, issue.PrimaryArtifactID, 8)

	if edited := editDocument(t, handler, other, insertAtEnd("Written past the queue.")); edited.Code != http.StatusOK {
		t.Errorf("a write to another document while eight writers queue on the first: status=%d body=%s, want 200",
			edited.Code, edited.Body.String())
	}
	releaseHold()
	requireEditsLanded(t, wait())
}

// A writer queued for a document gives its admission units back while it waits: with room for two
// writes at once, two writers queued on one document leave room for a write to another.
func TestWritersQueuedOnADocumentLeaveAdmissionRoomForOthers(t *testing.T) {
	limiter := admission.New(admission.Limits{Read: 4, Write: 2})
	handler, service, _ := writerGateHandler(t, 8, limiter, nil)
	issue := createInteractionIssue(t, handler, "TEST", "Parked writers", "First line.")
	other := createSecondDocument(t, handler, issue.Key)
	_, hold, err := service.AcquireDocumentWriter(context.Background(), issue.PrimaryArtifactID)
	if err != nil {
		t.Fatalf("hold the document's writer gate: %v", err)
	}
	var held sync.Once
	releaseHold := func() { held.Do(hold) }
	defer releaseHold()
	wait := queuedEdits(t, handler, service, issue.PrimaryArtifactID, 2)

	if edited := editDocument(t, handler, other, insertAtEnd("Written while both write units' holders queue.")); edited.Code != http.StatusOK {
		t.Errorf("a write to another document while two writers queue on the first: status=%d body=%s, want 200",
			edited.Code, edited.Body.String())
	}
	releaseHold()
	requireEditsLanded(t, wait())
}

// A writer that gave its admission unit back while it queued, and finds no room to take it back
// once the gate passes to it, is answered 429 OVERLOADED, and hands the gate on.
func TestAQueuedWriterThatFindsNoAdmissionRoomIsRefusedOverloaded(t *testing.T) {
	limiter := admission.New(admission.Limits{Read: 4, Write: 1, Wait: 20 * time.Millisecond})
	gate := &gatedMarkQuote{entered: make(chan struct{}), release: make(chan struct{})}
	handler, service, _ := writerGateHandler(t, 8, limiter, func(service *docs.Service) docs.API {
		gate.API = service
		return gate
	})
	var released sync.Once
	release := func() { released.Do(func() { close(gate.release) }) }
	defer release()
	issue := createInteractionIssue(t, handler, "TEST", "Unresumed writer", "First line.")
	other := createSecondDocument(t, handler, issue.Key)
	_, hold, err := service.AcquireDocumentWriter(context.Background(), issue.PrimaryArtifactID)
	if err != nil {
		t.Fatalf("hold the document's writer gate: %v", err)
	}
	var held sync.Once
	releaseHold := func() { held.Do(hold) }
	defer releaseHold()
	wait := queuedEdits(t, handler, service, issue.PrimaryArtifactID, 1)

	// An anchored comment on the other document takes the only write unit and keeps it, held
	// inside its anchoring.
	var comment sync.WaitGroup
	comment.Add(1)
	commented := 0
	go func() {
		defer comment.Done()
		commented = dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments",
			map[string]any{
				"anchor": map[string]string{"artifact": other, "quote": "other body"},
				"body":   "holds the unit",
			}, "alice").Code
	}()
	<-gate.entered
	releaseHold()
	refused := wait()[0]
	if refused.Code != http.StatusTooManyRequests || decodeBody[struct {
		Code string `json:"code"`
	}](t, refused).Code != admission.CodeOverloaded {
		t.Errorf("a queued writer with no admission room to resume: status=%d body=%s, want 429 %s",
			refused.Code, refused.Body.String(), admission.CodeOverloaded)
	}
	if retryAfter := refused.Header().Get("Retry-After"); retryAfter != admission.RetryAfter {
		t.Errorf("Retry-After = %q, want %q", retryAfter, admission.RetryAfter)
	}
	release()
	comment.Wait()
	if commented != http.StatusCreated {
		t.Errorf("the comment holding the unit: status=%d, want 201", commented)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, next, err := service.AcquireDocumentWriter(ctx, issue.PrimaryArtifactID)
	if err != nil {
		t.Fatalf("the document's gate after the refused writer = %v, want it free", err)
	}
	next()
}

// A comment edit whose input is refused on its own - blocks with no base_revision - is answered
// at once with its 400, before it queues for the document's writer gate: it changes nothing, so a
// writer holding that gate must not delay it, and a queue full of writers must not turn it into a
// retryable 429 that a client would send again for ever.
func TestACommentEditRefusedForItsInputWaitsOnNoWriterGate(t *testing.T) {
	handler, service, _ := writerGateHandler(t, 8, nil, nil)
	issue := createInteractionIssue(t, handler, "TEST", "Refused edit", "alpha bravo charlie")
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"anchor": map[string]string{"artifact": "spec", "quote": "bravo"},
		"body":   "Anchored note.",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create the anchored comment: status=%d body=%s", created.Code, created.Body.String())
	}
	comment := decodeBody[model.Comment](t, created)
	_, hold, err := service.AcquireDocumentWriter(context.Background(), issue.PrimaryArtifactID)
	if err != nil {
		t.Fatalf("hold the document's writer gate: %v", err)
	}
	var held sync.Once
	releaseHold := func() { held.Do(hold) }
	defer releaseHold()

	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		answered <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/comments/"+comment.ID, map[string]any{
			"body":   "Edited.",
			"blocks": []map[string]any{{"id": "b1", "from": 0}},
		}, "alice")
	}()
	select {
	case refused := <-answered:
		if refused.Code != http.StatusBadRequest {
			t.Errorf("an edit with blocks and no base_revision: status=%d body=%s, want 400", refused.Code, refused.Body.String())
		}
	case <-time.After(2 * time.Second):
		t.Errorf("an edit with blocks and no base_revision is still unanswered 2 s into another writer's hold of the document's gate (%d writers queued), want its 400 at once",
			service.QueuedDocumentWriters())
		releaseHold()
		<-answered
	}
}

// An anchored comment that carries a turn names no ask and no reply, so no row can make it an
// ask's reply: its 400 TURN_REQUIRES_ASK is the request's own, answered before it queues for the
// anchored document's writer gate, like the other refusals the request decides alone.
func TestAnAnchoredCommentWithATurnIsRefusedBeforeTheWriterGate(t *testing.T) {
	handler, service, _ := writerGateHandler(t, 8, nil, nil)
	issue := createInteractionIssue(t, handler, "TEST", "Refused turn", "alpha bravo charlie")
	_, hold, err := service.AcquireDocumentWriter(context.Background(), issue.PrimaryArtifactID)
	if err != nil {
		t.Fatalf("hold the document's writer gate: %v", err)
	}
	var held sync.Once
	releaseHold := func() { held.Do(hold) }
	defer releaseHold()

	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		answered <- dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
			"anchor": map[string]string{"artifact": "spec", "quote": "bravo"},
			"body":   "Anchored note.",
			"turn":   "agent",
		}, "alice")
	}()
	select {
	case refused := <-answered:
		if refused.Code != http.StatusBadRequest || decodeBody[struct {
			Code string `json:"code"`
		}](t, refused).Code != "TURN_REQUIRES_ASK" {
			t.Errorf("an anchored comment with a turn: status=%d body=%s, want 400 TURN_REQUIRES_ASK", refused.Code, refused.Body.String())
		}
	case <-time.After(2 * time.Second):
		t.Errorf("an anchored comment with a turn is still unanswered 2 s into another writer's hold of the document's gate (%d writers queued), want its 400 at once",
			service.QueuedDocumentWriters())
		releaseHold()
		<-answered
	}
}

// A close writes the blocks of the issue's open asks into every document of the issue, so it
// names those documents and gates them before its transaction opens, and names them again under
// the issue's row. One added while it waited for the gates leaves it writing a document it did
// not gate: like every other gated write, it gates them once more and lands, rather than
// refusing a close for a document somebody else created in between (gatedWrite).
func TestAnIssueCloseWhoseDocumentsMovedWhileItWaitedIsGatedAgain(t *testing.T) {
	handler, service, _ := writerGateHandler(t, 8, nil, nil)
	issue := createInteractionIssue(t, handler, "TEST", "Close past a new document", "First line.")
	_, hold, err := service.AcquireDocumentWriter(context.Background(), issue.PrimaryArtifactID)
	if err != nil {
		t.Fatalf("hold the document's writer gate: %v", err)
	}
	var held sync.Once
	releaseHold := func() { held.Do(hold) }
	defer releaseHold()

	closed := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		closed <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key,
			map[string]any{"status": "done"}, "alice")
	}()
	waitForQueuedWriters(t, service, 1)
	// The issue's documents move while the close waits for the one it named.
	createSecondDocument(t, handler, issue.Key)
	releaseHold()

	if response := <-closed; response.Code != http.StatusOK {
		t.Errorf("a close whose issue gained a document while it waited for the writer gate: status=%d body=%s, want 200",
			response.Code, response.Body.String())
	}
}

// An edit holds its document's writer gate until its events are published, not until its
// transaction commits: a writer let in between the two would publish its own events ahead of
// this edit's, and every reader of the stream - the page, the delivery hub - would be told the
// later write first. So a second writer of the document waits at the gate for as long as the
// first edit's publish is held (1f2f572d; the same holds for storeArtifact).
func TestAnEditHoldsItsDocumentsGateUntilItsEventsArePublished(t *testing.T) {
	handler, _, deps, served := newRegisteredTestServer(t, testServerOptions{})
	service, documents := deps.Docs.(*docs.Service)
	if !documents {
		t.Fatalf("the test server's document service is a %T, want the service whose gate queue this reads", deps.Docs)
	}
	issue := createInteractionIssue(t, handler, "TEST", "Gated publish", "A spec")
	publishing, holdPublish := make(chan struct{}), make(chan struct{})
	// Only the first edit's publish is held, and no later one waits on it: a second writer that
	// the gate let in too early must be free to answer, which is what this test watches for.
	var holding atomic.Bool
	served.beforePublish = func() {
		if holding.CompareAndSwap(false, true) {
			close(publishing)
			<-holdPublish
		}
	}

	first := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		first <- editDocument(t, handler, issue.PrimaryArtifactID, insertAtEnd("The first edit."))
	}()
	select {
	case <-publishing:
	case <-time.After(15 * time.Second):
		t.Fatal("the first edit never reached its publish")
	}
	second := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		second <- editDocument(t, handler, issue.PrimaryArtifactID, insertAtEnd("The second edit."))
	}()

	deadline := time.Now().Add(15 * time.Second)
	for service.QueuedDocumentWriters() == 0 {
		select {
		case response := <-second:
			t.Fatalf("a second writer of the document answered %d while the first edit's events were still unpublished, want it held at the gate until they are out",
				response.Code)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the second edit never reached the document's writer gate")
		}
		time.Sleep(time.Millisecond)
	}
	close(holdPublish)

	for name, answers := range map[string]chan *httptest.ResponseRecorder{"first": first, "second": second} {
		select {
		case response := <-answers:
			if response.Code != http.StatusOK {
				t.Fatalf("the %s edit: status=%d body=%s, want 200", name, response.Code, response.Body.String())
			}
		case <-time.After(15 * time.Second):
			t.Fatalf("the %s edit was never answered", name)
		}
	}
}
