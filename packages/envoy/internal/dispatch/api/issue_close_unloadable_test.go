package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A document of the issue whose stored history does not load is one nothing can read or open, so
// it shows nobody an ask block open, and it stays unloadable however often a write meets it: the
// close of its issue goes on without it and withdraws the plain ask beside it, naming the closer,
// the reopen restores that ask, and the repair passes over the issue, withdrawing for a close an
// older release made and restoring for its reopen, succeed rather than fail on that document for
// good.
func TestClosingAnIssueWithAnUnloadableDocumentWithdrawsItsAsksAndReopeningRestoresThem(t *testing.T) {
	// An hour, so no settlement timer loads the unloadable document between the requests.
	f := newRepairFixtureSettlingAfter(t, time.Hour)
	issue := createInteractionIssue(t, f.handler, "TEST", "Closing beside an unloadable document", "Context\n")
	uploaded := dispatchRequest(t, f.handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{
		"name": "notes.md", "content": "Notes\n",
	}, "alice")
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("upload the notes: status=%d body=%s", uploaded.Code, uploaded.Body.String())
	}
	notes := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, uploaded).Artifact.ID
	plainAsk := createSessionAsk(t, f.handler, issue.Key, "Which region?")

	if corrupted := makeDocumentUnloadable(t, f, notes); corrupted != 1 {
		t.Fatalf("the notes hold %d stored updates, want the upload's one", corrupted)
	}

	closed := dispatchRequest(t, f.handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"status": "done"}, "alice")
	if closed.Code != http.StatusOK {
		t.Fatalf("close beside the unloadable notes: status=%d body=%s", closed.Code, closed.Body.String())
	}
	requireWithdrawn(t, f.handler, issue.Key, plainAsk, model.Actor{Kind: "user", ID: "alice"})
	reopened := dispatchRequest(t, f.handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"status": "todo"}, "alice")
	if reopened.Code != http.StatusOK {
		t.Fatalf("reopen beside the unloadable notes: status=%d body=%s", reopened.Code, reopened.Body.String())
	}
	if got := readBlockAsk(t, f.handler, plainAsk); got.State != "open" || got.Resolution != nil {
		t.Fatalf("ask %s after the reopen = %s %#v, want open with no record", plainAsk, got.State, got.Resolution)
	}

	closer := repairActor("session-older-closer")
	closeWithoutWithdrawing(t, f, issue.Key, &closer, time.Now())
	if repair := repairNow(t, f); repair.Withdrawn != 1 {
		t.Fatalf("repair of the closed issue = %+v, want its plain ask withdrawn", repair)
	}
	requireWithdrawn(t, f.handler, issue.Key, plainAsk, closer)
	reopenWithoutRestoring(t, f, issue.Key, "todo", repairActor("session-older-reopener"))
	if repair := repairNow(t, f); repair.Restored != 1 {
		t.Fatalf("repair of the reopened issue = %+v, want its plain ask restored", repair)
	}
	if got := readBlockAsk(t, f.handler, plainAsk); got.State != "open" || got.Resolution != nil {
		t.Fatalf("ask %s after the repair's restoration = %s %#v, want open with no record", plainAsk, got.State, got.Resolution)
	}
}

// A document the close of its issue left as it was, because its stored history does not load, is
// rebuilt from its latest version, saved while the issue was open and showing its decision block
// open. The rebuild writes the close's states into the rebuilt document in its own transaction,
// so once it commits the block reads resolved, as the withdrawn ask's row does, rather than open
// on a closed issue until a reopen. The reopen then opens the block again with its ask.
func TestRebuildingAClosedIssuesUnloadableDocumentShowsItsWithdrawnDecisionBlockResolved(t *testing.T) {
	f := newRepairFixture(t)
	issue := createInteractionIssue(t, f.handler, "TEST", "Rebuilding a closed issue's document", "Context\n")
	uploaded := sessionRequest(t, f.handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{
		"actor": sessionActor(), "name": "notes.md",
		"content": "Notes\n\n:::ask{#rebuilt-decision urgency=\"med\" multiple=\"false\"}\nShip it?\n\n- Yes: Now.\n- No: Later.\n:::\n",
	})
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("upload the notes: status=%d body=%s", uploaded.Code, uploaded.Body.String())
	}
	notes := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, uploaded).Artifact.ID
	awaitIndexedAskBlock(t, f.handler, notes, "rebuilt-decision", "Ship it?")
	blockAsk := requireBlockAsk(t, f.handler, issue.Key, notes, "rebuilt-decision")
	open := `#rebuilt-decision urgency="med" multiple="false" state="open"`
	awaitDocumentText(t, f.handler, notes, "the open decision in the latest version", contains(open), nil)
	awaitSettled(t, f.database, notes)

	makeDocumentUnloadable(t, f, notes)

	if closed := dispatchRequest(t, f.handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"status": "done"}, "alice"); closed.Code != http.StatusOK {
		t.Fatalf("close beside the unloadable notes: status=%d body=%s", closed.Code, closed.Body.String())
	}
	requireWithdrawn(t, f.handler, issue.Key, blockAsk, model.Actor{Kind: "user", ID: "alice"})

	rebuilt := dispatchRequest(t, f.handler, http.MethodPost, "/api/v1/artifacts/"+notes+"/rebuild", map[string]any{}, "alice")
	if rebuilt.Code != http.StatusOK {
		t.Fatalf("rebuild the notes: status=%d body=%s", rebuilt.Code, rebuilt.Body.String())
	}
	live, _ := documentTexts(t, f.handler, notes)
	if !strings.Contains(live, `#rebuilt-decision urgency="med" multiple="false" state="resolved"`) {
		t.Fatalf("the rebuilt notes' live text:\n%s\nwant the withdrawn decision block resolved", live)
	}
	if strings.Contains(live, open) {
		t.Fatalf("the rebuilt notes' live text:\n%s\nshows the withdrawn decision block open on a closed issue", live)
	}
	requireWithdrawn(t, f.handler, issue.Key, blockAsk, model.Actor{Kind: "user", ID: "alice"})

	if reopened := dispatchRequest(t, f.handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"status": "todo"}, "alice"); reopened.Code != http.StatusOK {
		t.Fatalf("reopen: status=%d body=%s", reopened.Code, reopened.Body.String())
	}
	if got := readBlockAsk(t, f.handler, blockAsk); got.State != "open" || got.Resolution != nil {
		t.Fatalf("ask %s after the reopen = %s %#v, want open with no record", blockAsk, got.State, got.Resolution)
	}
	requireLiveText(t, f.handler, notes, open)
}

// makeDocumentUnloadable replaces every stored update of document with bytes that do not decode,
// with no room holding the document and no settlement owed that the close's own settlement would
// load it for, and returns how many stored updates it replaced, so a test can check it corrupted
// the history it expects.
func makeDocumentUnloadable(t *testing.T, f repairFixture, document string) int64 {
	t.Helper()
	ctx := context.Background()
	if err := f.documents.Evict(ctx, document); err != nil {
		t.Fatalf("evict the notes: %v", err)
	}
	if _, err := f.database.Pool.Exec(ctx, `delete from doc_settlements_pending where artifact_id = $1`, document); err != nil {
		t.Fatalf("clear the notes' owed settlement: %v", err)
	}
	corrupted, err := f.database.Pool.Exec(ctx, `update doc_updates set update = '\xff'::bytea where artifact_id = $1`, document)
	if err != nil {
		t.Fatalf("corrupt the notes' history: %v", err)
	}
	if _, err := docs.NewPgVersioned(f.database).LoadDocument(ctx, document); !errors.Is(err, docs.ErrDocumentUnloadable) {
		t.Fatalf("load the corrupted notes: %v, want ErrDocumentUnloadable", err)
	}
	return corrupted.RowsAffected()
}
