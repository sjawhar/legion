package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A project document's owner row is the artifact itself, and the upload has to take it before the
// document's room lock. A settlement holds that owner row and then takes the room; while the
// upload took the room first - it locked no owner until the event it appends at the end - the two
// closed a cycle and Postgres broke it with `deadlock detected`, the 500 this path returned under
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
