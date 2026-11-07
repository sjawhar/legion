package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/texttrees"
)

// storedTreeOf is the table's display tree of text, in the mode and at the version a read serves,
// and whether the table holds one at all. It reads no markdown of its own, so a test can ask
// whether a row exists without putting that text's tree in the process's display cache - which
// requireStoredTree's comparison does, and which every test below has to do without until it has
// counted the builds its read cost.
func storedTreeOf(t *testing.T, database *store.Store, text string) (json.RawMessage, bool) {
	t.Helper()
	return storedTreeIn(t, database, pmdoc.DisplayLatest, text)
}

// storedTreeIn is storedTreeOf in one named mode: a past version's tree is stored in the version
// mode and every other in the latest one, and one text's two modes are two rows.
func storedTreeIn(t *testing.T, database *store.Store, mode pmdoc.DisplayMode, text string) (json.RawMessage, bool) {
	t.Helper()
	stored, err := texttrees.New(database.Pool).Read(
		context.Background(), mode, [][32]byte{texttrees.Sum(text)},
	)
	if err != nil {
		t.Fatalf("read the stored tree of %q: %v", text, err)
	}
	tree, held := stored[texttrees.Sum(text)]
	return tree, held
}

// requireAnsweredTree fails unless the answer carries, at treePath, the tree ReadForDisplay makes
// of text. It builds that tree itself, so a test that counts builds calls it only after it has
// counted them.
func requireAnsweredTree(t *testing.T, response *httptest.ResponseRecorder, treePath, text string) {
	t.Helper()
	got, held := valueAt(t, response, treePath)
	if !held {
		t.Fatalf("the answer carries no %s:\n\t%s", treePath, response.Body.String())
	}
	want := sameShape(t, pmdoc.DisplayJSON(text, pmdoc.DisplayLatest))
	if !bytes.Equal(got, want) {
		t.Errorf("%s is\n\t%s\nwant ReadForDisplay's tree of %q\n\t%s", treePath, got, text, want)
	}
}

// commentingIssue is an issue with one comment on it, written through the route a client calls so
// the comment's display tree is stored exactly as a production write stores it.
func commentingIssue(t *testing.T, handler http.Handler, project, body string) string {
	t.Helper()
	issue := createInteractionIssue(t, handler, project, "Stored display tree reads", "A spec")
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments",
		map[string]any{"body": body}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create the comment: status=%d body=%s", created.Code, created.Body.String())
	}
	return issue.Key
}

// A comment list read after a restart makes no display tree: every tree it answers is the bytes
// the write stored, copied out of text_trees and into the process's display cache
// (primeDisplayTrees), and pmdoc.DisplayTreeBuilds - which counts readings of markdown and
// nothing else - does not move.
//
// The restart is the cache's state for this text and not the process's age: the cache is keyed by
// the text itself (its SHA-256 and the mode), so a text no reader of this process has been shown
// stands in exactly the state a restart leaves every text in. The body below is written by this
// test alone, and the write that stored its tree read it through pmdoc.DisplayTreeToStore, which
// fills no cache.
func TestACommentListAfterARestartMakesNoDisplayTree(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	const commentBody = "The **restart** read's own body: stored once, read back as bytes."
	key := commentingIssue(t, handler, "RSTRT", commentBody)
	if _, held := storedTreeOf(t, database, commentBody); !held {
		t.Fatalf("the write stored no display tree of %q, so the read has nothing to copy", commentBody)
	}

	builds := pmdoc.DisplayTreeBuilds()
	response := treesRequest(t, handler, "/api/v1/issues/"+key+"/comments", true)
	if built := pmdoc.DisplayTreeBuilds() - builds; built != 0 {
		t.Errorf("the comment list built %d display trees with every tree stored, want none", built)
	}
	requireAnsweredTree(t, response, "0.body_tree", commentBody)
}

// A text the table holds no tree for is read for display as it always was, answered, and stored
// by the request that answered it; the next read of that text builds nothing. The rows are
// dropped after the write to stand for a text written before there was a table, which is what
// every text on the server is until the backfill reaches it.
func TestAReadOfAnUnstoredTextBuildsItsTreeAndStoresIt(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	const commentBody = "A body *no* row holds a tree of yet, read the long way."
	key := commentingIssue(t, handler, "UNSTR", commentBody)
	if _, err := database.Pool.Exec(context.Background(), `delete from text_trees`); err != nil {
		t.Fatalf("drop the stored trees: %v", err)
	}

	builds := pmdoc.DisplayTreeBuilds()
	response := treesRequest(t, handler, "/api/v1/issues/"+key+"/comments", true)
	if built := pmdoc.DisplayTreeBuilds() - builds; built != 1 {
		t.Errorf("the comment list built %d display trees for one unstored body, want 1", built)
	}
	stored, held := storedTreeOf(t, database, commentBody)
	if !held {
		t.Fatalf("the read answered %q and stored no tree of it", commentBody)
	}

	builds = pmdoc.DisplayTreeBuilds()
	again := treesRequest(t, handler, "/api/v1/issues/"+key+"/comments", true)
	if built := pmdoc.DisplayTreeBuilds() - builds; built != 0 {
		t.Errorf("a second read of the same body built %d display trees, want none", built)
	}
	requireAnsweredTree(t, response, "0.body_tree", commentBody)
	requireAnsweredTree(t, again, "0.body_tree", commentBody)
	if want := pmdoc.DisplayJSON(commentBody, pmdoc.DisplayLatest); !bytes.Equal(stored, want) {
		t.Errorf("the tree the read stored is\n\t%s\nwant the one it answered\n\t%s", stored, want)
	}
}

// A tree stored at another display parser version is never served: the reading the stored bytes
// hold is not the reading this build makes, so the read builds its own and stores it beside the
// row it would not take (texttrees.Read keys by pmdoc.DisplayTreeVersionNow).
func TestAReadOfATreeStoredAtAnotherVersionBuildsItsOwn(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	const commentBody = "A body whose stored tree another **parser** version read."
	key := commentingIssue(t, handler, "OLDVR", commentBody)
	if _, held := storedTreeOf(t, database, commentBody); !held {
		t.Fatalf("the write stored no display tree of %q", commentBody)
	}

	restore := pmdoc.SetDisplayTreeVersionForTest(pmdoc.DisplayTreeVersion + 1)
	defer restore()
	builds := pmdoc.DisplayTreeBuilds()
	response := treesRequest(t, handler, "/api/v1/issues/"+key+"/comments", true)
	if built := pmdoc.DisplayTreeBuilds() - builds; built != 1 {
		t.Errorf("the comment list built %d display trees at a version the table does not hold, want 1", built)
	}
	if _, held := storedTreeOf(t, database, commentBody); !held {
		t.Errorf("the read at version %d stored no tree of its own", pmdoc.DisplayTreeVersionNow())
	}
	requireAnsweredTree(t, response, "0.body_tree", commentBody)
}

// countedTreesRead reads target with the display trees asked for, on a request a roundTrips
// counts (countedRead, issue_read_batch_test.go).
func countedTreesRead(t *testing.T, handler http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	ctx := context.WithValue(context.Background(), countedRead{}, true)
	request := httptest.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	request.Header.Set("X-Dispatch-User", "alice")
	request.Header.Set("Dispatch-Trees", "1")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("GET %s: status=%d body=%s", target, response.Code, response.Body.String())
	}
	return response
}

// A comment list reads the stored trees of its whole page in one statement, however many comments
// it answers, and stores nothing when the table held every one of them: the cost of the trees on
// a cold list is one round trip, not one per comment and not one markdown parse per comment.
func TestACommentListReadsItsStoredTreesInOneStatement(t *testing.T) {
	database := storetest.Open(t)
	counter := &roundTrips{}
	traced := &store.Store{Pool: storetest.Traced(t, database.Pool, counter, nil)}
	handler, _, _ := newTestServer(t, testServerOptions{
		store: traced,
		// The caller's identity is resolved on the untraced pool, so the count is the read's own
		// statements and not the sign-in lookup every request makes.
		identity: headerIdentity(database),
		// A settlement of the document is database work of its own on the same pool; a delay no
		// test run reaches keeps it out of the count.
		settle: time.Hour,
	})
	bodies := []string{
		"The **first** of several bodies this list answers.",
		"The *second*, with a `code span` in it.",
		"The third, which [links](https://example.test) somewhere.",
		"The fourth, whose body is plain words.",
		"The fifth and last, ending the page.",
	}
	issue := createInteractionIssue(t, handler, "ONEQ", "One query for the whole page", "A spec")
	for _, body := range bodies {
		created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments",
			map[string]any{"body": body}, "alice")
		if created.Code != http.StatusCreated {
			t.Fatalf("create the comment %q: status=%d body=%s", body, created.Code, created.Body.String())
		}
	}
	counter.take()

	response := countedTreesRead(t, handler, "/api/v1/issues/"+issue.Key+"/comments")
	counted := counter.take()
	var reads, writes int
	for _, statement := range counted {
		switch {
		case strings.Contains(statement, "from text_trees"):
			reads++
		case strings.Contains(statement, "insert into text_trees"):
			writes++
		}
	}
	if reads != 1 || writes != 0 {
		t.Fatalf("the list of %d comments made %d reads and %d writes of text_trees, want 1 and 0:\n%s",
			len(bodies), reads, writes, strings.Join(roundTripLines(counted), "\n"))
	}
	for index, body := range bodies {
		requireAnsweredTree(t, response, strconv.Itoa(index)+".body_tree", body)
	}
}

// A read inside a read-only transaction stores nothing and reads nothing: it holds a pooled
// connection already, and a second one taken while one is held is the deadlock
// store.ErrNestedAcquire refuses (docs loads a document and compacts it inside such a
// transaction). Every text comes back as one to read, and the shaping answers its trees as it
// always did. Outside the transaction the same pair stores, which is what makes the skip the
// held connection's doing and not the text's.
func TestAReadInsideAReadOnlyTransactionStoresNoTree(t *testing.T) {
	_, database, deps := newTestServer(t, testServerOptions{})
	served := directServer(deps)
	const body = "A body shaped inside a **read-only** transaction."

	held := store.WithTransactionTracking(context.Background())
	tx, err := database.Pool.BeginTx(held, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatalf("open the read-only transaction: %v", err)
	}
	missing := served.primeDisplayTrees(held, body)
	if len(missing) != 1 || missing[0] != body {
		t.Fatalf("the prime under a held connection answered %q, want the one text it was given", missing)
	}
	served.storeDisplayTrees(held, "a read-only transaction", missing...)
	if _, stored := storedTreeOf(t, database, body); stored {
		t.Errorf("the read inside the read-only transaction stored a tree of %q", body)
	}
	if read := newCommentRead(model.Comment{Body: body}, true); read.BodyTree == nil {
		t.Errorf("the shaping inside the read-only transaction answered no tree of %q", body)
	}
	if err := tx.Rollback(context.Background()); err != nil {
		t.Fatalf("roll the read-only transaction back: %v", err)
	}

	served.storeDisplayTrees(context.Background(), "the same texts outside it", body)
	if _, stored := storedTreeOf(t, database, body); !stored {
		t.Errorf("the same store outside the transaction stored no tree of %q", body)
	}
}

// A past version is read in the version mode, which keeps the attributes the server wrote, and
// no write stores that mode: the first read of a version is where its tree reaches the table,
// and the latest mode's row of the same text is a row of its own.
func TestAVersionsDisplayTreeIsStoredOnItsFirstRead(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "VRTR", "A version's own tree", "# Head\n\nIts *own* text.")
	target := "/api/v1/artifacts/" + issue.PrimaryArtifactID + "/versions/1"

	first := treesRequest(t, handler, target, true)
	version := decodeBody[struct {
		Markdown string `json:"markdown"`
	}](t, first)
	if version.Markdown == "" {
		t.Fatalf("the version read answered no markdown:\n\t%s", first.Body.String())
	}
	stored, held := storedTreeIn(t, database, pmdoc.DisplayVersion, version.Markdown)
	if !held {
		t.Fatalf("the first read of the version stored no tree of %q", version.Markdown)
	}
	if want := sameShape(t, pmdoc.DisplayJSON(version.Markdown, pmdoc.DisplayVersion)); !bytes.Equal(sameShape(t, stored), want) {
		t.Errorf("the stored version tree is\n\t%s\nwant the version mode's reading\n\t%s", stored, want)
	}
	if _, latest := storedTreeIn(t, database, pmdoc.DisplayLatest, version.Markdown); !latest {
		t.Errorf("the version write stored no latest-mode tree of %q, which the document's own reads answer", version.Markdown)
	}
}
