package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A write transaction runs a read phase holding no transaction id - its owner's advisory lock
// (store.LockOwner), its reads, its document work - and then a write phase of back-to-back
// statements, sent as one pipelined batch (docs.Ledger's queue, store.WriteBatch). The tests here
// pin what a caller sees of that split: a close committed while a writer waited still refuses it,
// a thread read still waits for a writer it would otherwise race, writers of one owner still run
// one at a time, and a write phase that fails anywhere in its batch leaves nothing behind.

// gatedProjectMark holds the first ProjectMark called once the test arms it, inside the writer's
// read phase: its handler holds the owner's lock and has queued its writes, none of which has run.
type gatedProjectMark struct {
	docs.API
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *gatedProjectMark) ProjectMark(ctx context.Context, artifactID, markID string, record docs.MarkRecord, actor model.Actor) error {
	if g.armed.Load() {
		g.once.Do(func() {
			close(g.entered)
			<-g.release
		})
	}
	return g.API.ProjectMark(ctx, artifactID, markID, record, actor)
}

// ownerLockWaiters counts this test database's backends waiting on an owner lock (the statements
// store.LockOwner and LockOwnerShared run, which name their result owner_lock), and how many of
// them hold a transaction id while they wait.
func ownerLockWaiters(t *testing.T, database *store.Store) (waiting, holdingXid int) {
	t.Helper()
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*), count(*) filter (where backend_xid is not null)
		from pg_stat_activity
		where datname = current_database() and wait_event_type = 'Lock' and wait_event = 'advisory'
		  and query like '%owner_lock%' and pid <> pg_backend_pid()
	`).Scan(&waiting, &holdingXid); err != nil {
		t.Fatalf("read pg_stat_activity: %v", err)
	}
	return waiting, holdingXid
}

// awaitOwnerLockWaiter waits until a backend waits on an owner lock and answers how many of the
// waiters hold a transaction id. A request that answers first did not wait, which fails the test:
// what refuses it is that it ran while the lock's holder was still in its read phase.
func awaitOwnerLockWaiter(t *testing.T, database *store.Store, request string, answered <-chan *httptest.ResponseRecorder) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case response := <-answered:
			t.Fatalf("%s answered %d %s without waiting for the owner lock its writer holds", request, response.Code, response.Body.String())
		default:
		}
		if waiting, holdingXid := ownerLockWaiters(t, database); waiting > 0 {
			return holdingXid
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never waited on the owner lock within 10s", request)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A close committed while a comment waited for the issue's owner lock refuses the comment: the
// comment reads the issue in the statement after the lock, whose snapshot holds the close. Read in
// the lock's own statement, the issue would be the row as it stood before the wait, and the
// comment would land on a closed issue.
func TestACloseCommittedWhileAWriterWaitsOnTheOwnerLockIsRefused(t *testing.T) {
	// The seeded spec's settlement takes the owner lock this test holds; keep it out of the queue.
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	issue := createInteractionIssue(t, handler, "TEST", "Closed while waiting", "A spec")
	before := databaseFingerprint(t, database)

	closer, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin the close: %v", err)
	}
	defer func() { _ = closer.Rollback(context.Background()) }()
	// The close is a PATCH's: under the issue's owner lock.
	if err := store.LockOwner(context.Background(), closer, store.IssueOwner(issue.Key)); err != nil {
		t.Fatalf("take the issue's owner lock: %v", err)
	}
	if _, err := closer.Exec(context.Background(), `
		update issues set status = 'done', closed_at = now() where key = $1
	`, issue.Key); err != nil {
		t.Fatalf("close the issue: %v", err)
	}
	responses := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		responses <- dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments",
			map[string]any{"body": "Written while the issue closed."}, "alice")
	}()
	waitForDatabaseLocks(t, closer, 1)
	if err := closer.Commit(context.Background()); err != nil {
		t.Fatalf("commit the close: %v", err)
	}

	response := awaitResponse(t, responses)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"code":"ISSUE_CLOSED"`) {
		t.Fatalf("comment that waited out the close: status=%d body=%s, want 409 ISSUE_CLOSED", response.Code, response.Body.String())
	}
	// The close changed the issue row; the refused comment changed nothing at all: no comment, no
	// event, no thread row.
	after := databaseFingerprint(t, database)
	delete(before, "issues")
	delete(after, "issues")
	assertUnchanged(t, "a comment refused for a close it waited out", before, after)
}

// A person marking a thread read while a session's reply to it is in its read phase waits for that
// reply: the mark takes the owner's lock in shared mode, which the reply holds exclusively from
// before its first read to its commit. Both succeed, neither is refused as a deadlock, and the
// mark, made after the reply was written, leaves the thread read.
func TestAThreadReadWaitsForAWritersReadPhase(t *testing.T) {
	gate := &gatedProjectMark{entered: make(chan struct{}), release: make(chan struct{})}
	handler, _, database := pooledDocumentHandlerWith(t, 8, time.Hour, func(service *docs.Service) docs.API {
		gate.API = service
		return gate
	})
	handler = withDeadline(handler)
	var released sync.Once
	release := func() { released.Do(func() { close(gate.release) }) }
	defer release()
	issue := createInteractionIssue(t, handler, "TEST", "Read while replying", "alpha bravo charlie")
	root := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "Is alpha right?", "anchor": map[string]any{"artifact": "spec", "quote": "alpha"},
	}, "alice")

	// A reply to an anchored comment projects the thread onto its root's mark (ProjectMark), in
	// its read phase, after it has read its creation time and queued its insert.
	gate.armed.Store(true)
	replies := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		replies <- sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
			"body": "Yes.", "reply_to": root.ID, "actor": sessionActor(),
		})
	}()
	select {
	case <-gate.entered:
	case response := <-replies:
		t.Fatalf("the reply answered %d %s before projecting its thread", response.Code, response.Body.String())
	case <-time.After(10 * time.Second):
		t.Fatal("the reply never reached its thread's projection")
	}

	mark := time.Now()
	marks := make(chan *httptest.ResponseRecorder, 1)
	go func() { marks <- markThreadRead(t, handler, "alice", root.ID, mark) }()
	awaitOwnerLockWaiter(t, database, "the thread read", marks)
	release()

	reply := awaitResponse(t, replies)
	if reply.Code != http.StatusCreated {
		t.Fatalf("reply: status=%d body=%s, want 201", reply.Code, reply.Body.String())
	}
	marked := awaitResponse(t, marks)
	if marked.Code != http.StatusNoContent {
		t.Fatalf("thread read: status=%d body=%s, want 204", marked.Code, marked.Body.String())
	}
	written := decodeBody[model.Comment](t, reply)
	through, repliedAt := readThrough(t, database, root.ID)
	if through.Before(mark.Truncate(time.Microsecond)) {
		t.Fatalf("read mark %s lost the mark %s", through, mark)
	}
	if repliedAt == nil || repliedAt.Before(written.CreatedAt) {
		t.Fatalf("replied_at %v lost the reply at %s", repliedAt, written.CreatedAt)
	}
	if listsThread(getMyReplies(t, handler, "alice", ""), root.ID) {
		t.Fatalf("the thread is still unread after a mark made after the reply (read through %s, replied at %s)", through, written.CreatedAt)
	}
}

// transactionIDHolders is how many backends of this test's database hold a transaction id, and
// what each of them is running, so a holder this test does not drive is named rather than
// counted alone.
func transactionIDHolders(t *testing.T, database *store.Store) (int, string) {
	t.Helper()
	var holders int
	var running string
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*), coalesce(string_agg(query, ' | '), '') from pg_stat_activity
		where datname = current_database() and backend_xid is not null and pid <> pg_backend_pid()
	`).Scan(&holders, &running); err != nil {
		t.Fatalf("read pg_stat_activity: %v", err)
	}
	return holders, running
}

// awaitNoTransactionIDHolder waits until no backend holds a transaction id. An issue create
// leaves one write running past its answer: the refresh of the project's search term statistics
// its related-item check starts for a project that has none (ensureSearchTermStats), a
// background write with an id of its own. A test that counts every backend waits it out first,
// as the refusal tests wait it out, so what it counts afterwards is the writers it drives.
func awaitNoTransactionIDHolder(t *testing.T, database *store.Store) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		holders, running := transactionIDHolders(t, database)
		if holders == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d backends still hold a transaction id after 30s: %s", holders, running)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Two anchored comments on one spec still run one at a time, and the second waits for the first
// holding no transaction id, so a transaction waiting its turn holds back no live update capped by
// the event horizon. Both land, their events in the order they arrived and the issue's sequence
// without a gap.
func TestTwoWritersOfOneOwnerSerializeWithoutATransactionID(t *testing.T) {
	gate := &gatedMarkQuote{entered: make(chan struct{}), release: make(chan struct{})}
	handler, _, database := pooledDocumentHandlerWith(t, 8, time.Hour, func(service *docs.Service) docs.API {
		gate.API = service
		return gate
	})
	handler = withDeadline(handler)
	var released sync.Once
	release := func() { released.Do(func() { close(gate.release) }) }
	defer release()
	issue := createInteractionIssue(t, handler, "TEST", "Serialized writers", "alpha bravo charlie delta")
	awaitNoTransactionIDHolder(t, database)

	comment := func(quote, body string) *httptest.ResponseRecorder {
		return dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
			"anchor": map[string]string{"artifact": "spec", "quote": quote}, "body": body,
		}, "alice")
	}
	firsts := make(chan *httptest.ResponseRecorder, 1)
	go func() { firsts <- comment("alpha", "first") }()
	select {
	case <-gate.entered:
	case response := <-firsts:
		t.Fatalf("the first comment answered %d %s before marking its quote", response.Code, response.Body.String())
	case <-time.After(10 * time.Second):
		t.Fatal("the first comment never reached its quote's mark")
	}
	holders, running := transactionIDHolders(t, database)
	if holders != 0 {
		t.Fatalf("%d backends hold a transaction id while the first comment is in its read phase, want none: %s", holders, running)
	}

	seconds := make(chan *httptest.ResponseRecorder, 1)
	go func() { seconds <- comment("bravo", "second") }()
	if holdingXid := awaitOwnerLockWaiter(t, database, "the second comment", seconds); holdingXid != 0 {
		t.Fatalf("the second comment waits on the owner lock holding a transaction id")
	}
	release()

	first := awaitResponse(t, firsts)
	second := awaitResponse(t, seconds)
	if first.Code != http.StatusCreated || second.Code != http.StatusCreated {
		t.Fatalf("comments: first %d %s, second %d %s, want 201 each", first.Code, first.Body.String(), second.Code, second.Body.String())
	}
	firstID := decodeBody[model.Comment](t, first).ID
	secondID := decodeBody[model.Comment](t, second).ID

	rows, err := database.Pool.Query(context.Background(), `
		select seq, type, coalesce(payload->>'id', '') from events where issue_key = $1 order by seq
	`, issue.Key)
	if err != nil {
		t.Fatalf("read the issue's events: %v", err)
	}
	defer rows.Close()
	created := []string{}
	expected := 1
	for rows.Next() {
		var seq int
		var eventType, commentID string
		if err := rows.Scan(&seq, &eventType, &commentID); err != nil {
			t.Fatalf("scan event: %v", err)
		}
		if seq != expected {
			t.Fatalf("event %s has seq %d, want %d: the issue's sequence has a gap", eventType, seq, expected)
		}
		expected++
		if eventType == "comment.created" {
			created = append(created, commentID)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the issue's events: %v", err)
	}
	if len(created) != 2 || created[0] != firstID || created[1] != secondID {
		t.Fatalf("comment.created events = %v, want the first comment %s then the second %s", created, firstID, secondID)
	}
	var lastSeq int
	if err := database.Pool.QueryRow(context.Background(), `select last_seq from issues where key = $1`, issue.Key).Scan(&lastSeq); err != nil {
		t.Fatalf("read the issue's sequence: %v", err)
	}
	if lastSeq != expected-1 {
		t.Fatalf("issue last_seq = %d, want %d, the last event's", lastSeq, expected-1)
	}
}

// A write whose write phase fails part way answers what that failure answers and leaves nothing:
// no row of its own, no document update in the room or the store, no event. Accepting a suggestion
// queues the document's replacement first and its events last, so a failure anywhere in between
// shows in every one of them.
func TestAFailedWritePhaseLeavesNothing(t *testing.T) {
	suggest := func(t *testing.T, handler http.Handler) (issueKey, artifactID, commentID string) {
		t.Helper()
		issue := createInteractionIssue(t, handler, "TEST", "Failed write phase", "foo")
		created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
			"body": "Replace foo.", "anchor": map[string]any{"artifact": "spec", "quote": "foo"},
			"suggestion": map[string]string{"replace_with": "foo2"}, "actor": sessionActor(),
		})
		if created.Code != http.StatusCreated {
			t.Fatalf("create suggestion: status=%d body=%s", created.Code, created.Body.String())
		}
		return issue.Key, issue.PrimaryArtifactID, decodeBody[model.Comment](t, created).ID
	}
	assertDocumentUnchanged := func(t *testing.T, service *docs.Service, artifactID string) {
		t.Helper()
		for _, where := range []string{"live room", "reloaded room"} {
			if where == "reloaded room" {
				if err := service.Evict(context.Background(), artifactID); err != nil {
					t.Fatalf("evict the document: %v", err)
				}
			}
			markdown, err := service.Text(context.Background(), artifactID)
			if err != nil {
				t.Fatalf("read the %s: %v", where, err)
			}
			if markdown != "foo\n" {
				t.Fatalf("%s after the failed accept = %q, want foo", where, markdown)
			}
		}
	}

	// The suggestion is decided, by a writer that does not hold the owner's lock, after the accept
	// read it undecided and before its write phase: the queued update's guard finds it decided and
	// refuses with the 409 a decided suggestion answers.
	t.Run("a refused guard", func(t *testing.T) {
		var service *docs.Service
		var gate *firstReplaceGate
		handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
			service = docs.New(docs.Deps{Store: database, Settle: time.Hour})
			t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
			gate = &firstReplaceGate{
				API: service, firstEntered: make(chan struct{}), releaseFirst: make(chan struct{}), secondEntered: make(chan struct{}),
			}
			return gate
		})
		handler = withDeadline(handler)
		var released sync.Once
		release := func() { released.Do(func() { close(gate.releaseFirst) }) }
		defer release()
		_, artifactID, commentID := suggest(t, handler)

		responses := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			responses <- dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+commentID+"/accept", map[string]any{}, "alice")
		}()
		select {
		case <-gate.firstEntered:
		case response := <-responses:
			t.Fatalf("the accept answered %d %s before replacing the quote", response.Code, response.Body.String())
		case <-time.After(10 * time.Second):
			t.Fatal("the accept never reached the replacement")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := database.Pool.Exec(ctx, `
			update comments set suggestion = jsonb_set(suggestion, '{accepted}', 'false'::jsonb) where id = $1
		`, commentID); err != nil {
			t.Fatalf("decide the suggestion behind the accept: %v", err)
		}
		// The accept's read phase wrote nothing, so this is the database as it stood before it.
		before := databaseFingerprint(t, database)
		release()

		response := awaitResponse(t, responses)
		if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"code":"ALREADY_ACTIONED"`) {
			t.Fatalf("accept of a suggestion decided behind it: status=%d body=%s, want 409 ALREADY_ACTIONED", response.Code, response.Body.String())
		}
		assertUnchanged(t, "an accept whose guard refused in its write phase", before, databaseFingerprint(t, database))
		assertDocumentUnchanged(t, service, artifactID)
	})

	// The issue's next sequence number is taken by a row its counter does not know of, so the
	// accept's first event append, in its write phase, is a unique violation: the server's failure,
	// a 500, after the replacement, the comment's update and the named version were already
	// written in the transaction.
	t.Run("a unique violation", func(t *testing.T) {
		var service *docs.Service
		handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
			service = docs.New(docs.Deps{Store: database, Settle: time.Hour})
			t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
			return service
		})
		handler = withDeadline(handler)
		issueKey, artifactID, commentID := suggest(t, handler)
		if _, err := database.Pool.Exec(context.Background(), `
			insert into events (issue_key, seq, type, actor, payload, notify)
			select key, last_seq + 1, 'test.seq_taken', '{"kind":"session","id":"seq-taken"}'::jsonb, '{}'::jsonb, false
			from issues where key = $1
		`, issueKey); err != nil {
			t.Fatalf("take the issue's next sequence number: %v", err)
		}
		before := databaseFingerprint(t, database)

		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+commentID+"/accept", map[string]any{}, "alice")
		if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), `"code":"INTERNAL"`) {
			t.Fatalf("accept whose event append collides: status=%d body=%s, want 500 INTERNAL", response.Code, response.Body.String())
		}
		assertUnchanged(t, "an accept whose event append failed in its write phase", before, databaseFingerprint(t, database))
		assertDocumentUnchanged(t, service, artifactID)
	})

	// takeNextSeq inserts a row at the issue's next sequence number, which its counter does not
	// know of, so the next event appended on the issue is a unique violation.
	takeNextSeq := func(t *testing.T, database *store.Store, issueKey string) {
		t.Helper()
		if _, err := database.Pool.Exec(context.Background(), `
			insert into events (issue_key, seq, type, actor, payload, notify)
			select key, last_seq + 1, 'test.seq_taken', '{"kind":"session","id":"seq-taken"}'::jsonb, '{}'::jsonb, false
			from issues where key = $1
		`, issueKey); err != nil {
			t.Fatalf("take the issue's next sequence number: %v", err)
		}
	}

	// An anchored comment's write phase is one batch: the document's update, the comment row, its
	// references, then comment.created and its stamp. The event's insert collides, after every
	// other statement of the batch ran on the server, and the whole transaction goes.
	t.Run("a comment whose event collides", func(t *testing.T) {
		var service *docs.Service
		handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
			service = docs.New(docs.Deps{Store: database, Settle: time.Hour})
			t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
			return service
		})
		handler = withDeadline(handler)
		issue := createInteractionIssue(t, handler, "TEST", "Failed comment write phase", "foo")
		takeNextSeq(t, database, issue.Key)
		before := databaseFingerprint(t, database)

		response := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
			"body": "See dispatch://" + issue.Key + ".", "anchor": map[string]any{"artifact": "spec", "quote": "foo"},
			"actor": sessionActor(),
		})
		if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), `"code":"INTERNAL"`) {
			t.Fatalf("comment whose event append collides: status=%d body=%s, want 500 INTERNAL", response.Code, response.Body.String())
		}
		assertUnchanged(t, "a comment whose event append failed in its write phase", before, databaseFingerprint(t, database))
		assertDocumentUnchanged(t, service, issue.PrimaryArtifactID)
	})

	// A message writes without a document ledger, its insert, references, event and stamp one batch
	// of their own (store.RunWrite): the event's collision takes the message's row with it.
	t.Run("a message whose event collides", func(t *testing.T) {
		handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
			service := docs.New(docs.Deps{Store: database, Settle: time.Hour})
			t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
			return service
		})
		handler = withDeadline(handler)
		issue := createInteractionIssue(t, handler, "TEST", "Failed message write phase", "foo")
		takeNextSeq(t, database, issue.Key)
		before := databaseFingerprint(t, database)

		response := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]any{
			"body": "Status: see dispatch://" + issue.Key + ".", "actor": sessionActor(),
		})
		if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), `"code":"INTERNAL"`) {
			t.Fatalf("message whose event append collides: status=%d body=%s, want 500 INTERNAL", response.Code, response.Body.String())
		}
		assertUnchanged(t, "a message whose event append failed in its write phase", before, databaseFingerprint(t, database))
	})
}
