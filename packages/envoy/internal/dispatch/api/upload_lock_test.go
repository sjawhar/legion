package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A project document's owner row is the artifact itself, and the upload has to take it before the
// document's room lock. A settlement holds that owner row and then takes the room; an upload that
// took the room first - locking no owner until the event it appends at the end - would close a
// cycle with it, which Postgres breaks with `deadlock detected`, a 500 from this path under
// load. Holding the owner row here must therefore stop the upload before the room lock, leaving
// the room free to take behind it.
func TestProjectDocumentUploadTakesItsOwnerRowBeforeTheRoomLock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "LOCK", "name": "Lock order",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	document := createProjectDocument(t, handler, "LOCK", "Notes", "# Notes\n")

	settling, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the settlement-shaped transaction: %v", err)
	}
	defer settling.Rollback(context.Background())
	var held bool
	if err := settling.QueryRow(ctx, `
		select true from artifacts where id = $1 and issue_key is null for no key update
	`, document.ID).Scan(&held); err != nil {
		t.Fatalf("hold the document owner: %v", err)
	}

	uploaded := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		uploaded <- dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects/LOCK/artifacts", map[string]string{
			"name": "Notes", "content": "# Notes\n\nsecond\n",
		}, "alice")
	}()
	// The upload blocks on the owner row this transaction holds, and on nothing else.
	waitForDatabaseLocks(t, settling, 1)

	if _, err := settling.Exec(ctx, `select pg_advisory_xact_lock(hashtext($1))`, document.ID); err != nil {
		t.Fatalf("take the room lock behind the upload: %v", err)
	}
	if err := settling.Rollback(context.Background()); err != nil {
		t.Fatalf("release the document owner: %v", err)
	}

	response := <-uploaded
	if response.Code != http.StatusCreated {
		t.Fatalf("upload: status=%d body=%s", response.Code, response.Body.String())
	}
}

// An upload of a project document holding a copied ask block and an answer to the block's source
// meet on the copy's pending-settlement row and on the events' commit-order lock, and both take the
// row first. The upload takes it with its document write (appendUpdateTxClass), before it appends
// its event; the answer takes it under the project's copy lock (docs.SettleCopiesOf), before it
// appends its own. The test holds the upload between its document write and its event and the
// answer just before its event, lets the answer reach the row, then lets the upload's event go
// first and the answer's after it: both finish. An upload that took the row only at its commit
// (Ledger.Commit's recordLatestEditSource), after its event, would wait on the row the answer
// marked while holding the commit-order lock the answer's event waits for, and Postgres would end
// one of them with 40P01.
func TestAnUploadOfACopyAndAnAnswerToItsSourceBothTakeTheCopysPendingRowBeforeTheirEvents(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	logs := &storetest.LockedLog{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	handler, database, _ := newTestServer(t, testServerOptions{})
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "COPY", "name": "Copies",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	block := ":::ask{#shared urgency=\"med\" multiple=\"false\"}\nWhich region?\n:::\n"
	source := createProjectDocument(t, handler, "COPY", "Source", "Context\n\n"+block)
	awaitIndexedAskBlock(t, handler, source.ID, "shared", "Which region?")
	var ask string
	if err := database.Pool.QueryRow(ctx, `select id::text from asks where block_artifact_id = $1`, source.ID).Scan(&ask); err != nil {
		t.Fatalf("read the source ask: %v", err)
	}
	copied := createProjectDocument(t, handler, "COPY", "Copy", "Kept\n\n"+block)
	// The copy settles as a copy of the source's ask, and no document owes a settlement once it has.
	awaitCopyShows(t, ctx, database, copied.ID, `state="open" copied_from="`+ask+`"`)

	// Each hold is an advisory lock the test takes, which a trigger makes one statement wait for: the
	// upload's version insert, after its document write and before its event, and the answer's write
	// of its ask row, after it marked the copy owed and before its event.
	const holdUpload, holdAnswer = int64(4815162343), int64(4815162344)
	for _, statement := range []string{
		`create function dispatch_test_hold() returns trigger language plpgsql as $$
		begin
			perform pg_advisory_xact_lock(tg_argv[0]::bigint);
			return new;
		end;
		$$`,
		fmt.Sprintf(`create trigger dispatch_test_hold_upload before insert on artifact_versions for each row
			when (new.artifact_id = '%s') execute function dispatch_test_hold('%d')`, copied.ID, holdUpload),
		fmt.Sprintf(`create trigger dispatch_test_hold_answer before update on asks for each row
			when (new.id = '%s' and new.state = 'answered') execute function dispatch_test_hold('%d')`, ask, holdAnswer),
	} {
		if _, err := database.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("hold the upload and the answer before their events: %v", err)
		}
	}
	hold := func(key int64) pgx.Tx {
		t.Helper()
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin a hold: %v", err)
		}
		t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
		if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock($1)`, key); err != nil {
			t.Fatalf("take a hold: %v", err)
		}
		return tx
	}
	release := func(tx pgx.Tx) {
		t.Helper()
		if err := tx.Rollback(ctx); err != nil {
			t.Fatalf("release a hold: %v", err)
		}
	}
	uploadHold, answerHold := hold(holdUpload), hold(holdAnswer)
	const markingCopy, writingAnswer = "%insert into doc_settlements_pending%", "%update asks set state%"

	upload := startRequest(func() *httptest.ResponseRecorder {
		return dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects/COPY/artifacts", map[string]string{
			"name": "Copy", "content": "Kept again\n\n" + block,
		}, "alice")
	})
	awaitLockWaits(t, ctx, database, "the upload never waited between its document write and its event", func(waiting func(string) int) bool {
		return upload.finished() || waiting("%insert into artifact_versions%") > 0
	})
	if upload.finished() {
		t.Fatalf("the upload finished before its event could be held: %s", upload)
	}
	answer := startRequest(func() *httptest.ResponseRecorder {
		return dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+ask+"/answer", map[string]any{
			"text": "eu-west-1",
		}, "alice")
	})
	// The answer waits on the copy's pending row the upload holds; had the upload not taken it, the
	// answer would mark it and wait before its own event instead.
	awaitLockWaits(t, ctx, database, "the answer never reached the copy's pending row", func(waiting func(string) int) bool {
		return answer.finished() || waiting(markingCopy)+waiting(writingAnswer) > 0
	})
	release(uploadHold)
	// The upload's event goes first: the upload commits, or it waits on the row the answer marked
	// while the answer waits before its event.
	awaitLockWaits(t, ctx, database, "the upload neither finished nor waited on the copy's pending row", func(waiting func(string) int) bool {
		return upload.finished() || (waiting(markingCopy) > 0 && waiting(writingAnswer) > 0)
	})
	release(answerHold)
	for _, request := range []*pendingRequest{upload, answer} {
		select {
		case <-request.done:
		case <-ctx.Done():
			t.Fatalf("the upload (%s) and the answer (%s) never both finished", upload, answer)
		}
	}
	if upload.response.Code != http.StatusCreated || answer.response.Code != http.StatusOK {
		t.Fatalf("upload: %s\nanswer: %s\nwant 201 and 200; the server logged:\n%s", upload, answer, logs.Lines("API handler failed"))
	}
	awaitCopyShows(t, ctx, database, copied.ID, `state="answered" answered_by="alice"`)
}

// pendingRequest is a request a test runs on a goroutine of its own.
type pendingRequest struct {
	done     chan struct{}
	response *httptest.ResponseRecorder
}

func startRequest(send func() *httptest.ResponseRecorder) *pendingRequest {
	request := &pendingRequest{done: make(chan struct{})}
	go func() {
		request.response = send()
		close(request.done)
	}()
	return request
}

func (r *pendingRequest) finished() bool {
	select {
	case <-r.done:
		return true
	default:
		return false
	}
}

func (r *pendingRequest) String() string {
	if !r.finished() {
		return "still running"
	}
	return fmt.Sprintf("status=%d body=%s", r.response.Code, strings.TrimSpace(r.response.Body.String()))
}

// awaitLockWaits polls until reached holds, given how many sessions of the test's database wait on
// a lock whose statement matches a LIKE pattern (storetest.CountLockWaits), and fails the test with
// never when it does not within 10 s.
func awaitLockWaits(t *testing.T, ctx context.Context, database *store.Store, never string, reached func(waiting func(like string) int) bool) {
	t.Helper()
	waiting := func(like string) int {
		count, err := storetest.CountLockWaits(ctx, database, like)
		if err != nil {
			t.Fatal(err)
		}
		return count
	}
	deadline := time.Now().Add(10 * time.Second)
	for !reached(waiting) {
		if time.Now().After(deadline) {
			t.Fatal(never)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// awaitCopyShows waits for the latest version of copy to hold want, with no document owing a
// settlement.
func awaitCopyShows(t *testing.T, ctx context.Context, database *store.Store, copy, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var markdown string
		var pending int
		if err := database.Pool.QueryRow(ctx, `
			select (select markdown from artifact_versions where artifact_id = $1 order by number desc limit 1),
				(select count(*) from doc_settlements_pending)
		`, copy).Scan(&markdown, &pending); err != nil {
			t.Fatalf("read the copy's latest version: %v", err)
		}
		if strings.Contains(markdown, want) && pending == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the copy's latest version never held %s with nothing owed (%d owed):\n%s", want, pending, markdown)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The upload holds the document's owner row across the room lock it then waits for, so that lock
// has to be `for no key update`: a durable writer holding the room reaches the same row through
// doc_updates' foreign key, and while the owner lock was `for update` that key share conflicted
// and the two deadlocked.
func TestProjectDocumentUploadOwnerLockLeavesTheForeignKeyFree(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "LEVEL", "name": "Lock level",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	document := createProjectDocument(t, handler, "LEVEL", "Notes", "# Notes\n")

	appending, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the durable writer: %v", err)
	}
	defer appending.Rollback(context.Background())
	if _, err := appending.Exec(ctx, `select pg_advisory_xact_lock(hashtext($1))`, document.ID); err != nil {
		t.Fatalf("hold the document room lock: %v", err)
	}

	uploaded := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		uploaded <- dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects/LEVEL/artifacts", map[string]string{
			"name": "Notes", "content": "# Notes\n\nsecond\n",
		}, "alice")
	}()
	// The upload takes the owner row, then waits here for the room.
	waitForDatabaseLocks(t, appending, 1)

	if _, err := appending.Exec(ctx, `
		insert into doc_updates (artifact_id, version, update, content_changed)
		values ($1, (select coalesce(max(version), 0) + 1 from doc_updates where artifact_id = $1), $2, true)
	`, document.ID, []byte{0}); err != nil {
		t.Fatalf("append while the upload holds the document owner: %v", err)
	}
	if err := appending.Rollback(context.Background()); err != nil {
		t.Fatalf("release the room lock: %v", err)
	}

	response := <-uploaded
	if response.Code != http.StatusCreated {
		t.Fatalf("upload: status=%d body=%s", response.Code, response.Body.String())
	}
}
