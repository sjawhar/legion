package api

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A comment list is answered under a validator read before its body is built: the watermark three
// deferred constraint triggers raise for every write of comments, comment_mentions and
// comment_deliveries (migration 20261106020619, answerCommentList). This file holds that bargain
// to both of its sides - every write the list shows moves the validator, and a read that finds it
// unmoved builds nothing - and to the two properties the triggers' shape turns on: a write held
// open blocks no other write of the same list, and two writers committing at once each keep their
// bump.

// commentListOf is the route an issue's comment list is read at.
func commentListOf(key string) string {
	return "/api/v1/issues/" + key + "/comments"
}

// commentListValidator is the validator the list answers now, read as the dashboard reads it.
func commentListValidator(t *testing.T, handler http.Handler, target, what string) string {
	t.Helper()
	return listETagOf(t, listRead(t, handler, target, listReadOptions{trees: true}), what)
}

// requireMovedValidator fails unless a reader holding the validator from before write is answered
// the whole list again under one of its own, and returns the validator the list now stands at.
func requireMovedValidator(t *testing.T, handler http.Handler, target, before, write string) string {
	t.Helper()
	response := listRead(t, handler, target, listReadOptions{trees: true, etag: before})
	if response.Code == http.StatusNotModified {
		t.Fatalf("after %s, GET %s answers 304 to the validator %s it gave before: its reader is told nothing changed",
			write, target, before)
	}
	moved := listETagOf(t, response, "GET "+target+" after "+write)
	if moved == before {
		t.Fatalf("after %s, GET %s answers the validator %s it gave before it", write, target, before)
	}
	return moved
}

// commentWatermark is the counter an issue's comment list stands on, 0 for an issue nothing has
// written a comment row of yet.
func commentWatermark(t *testing.T, database *store.Store, key string) int64 {
	t.Helper()
	var seq int64
	if err := database.Pool.QueryRow(context.Background(),
		`select coalesce((select seq from comment_watermarks where issue_key = $1), 0)`, key).Scan(&seq); err != nil {
		t.Fatalf("read %s's comment watermark: %v", key, err)
	}
	return seq
}

// A delivery claim moves the comment list's validator although it appends no event and leaves the
// issue's last_seq where it was. The claim is the insert openCommentDeliveryAttempt makes in the
// server's own first delivery transaction, and it is one of the two writes that rule out the
// feed's validator for this list: a reader holding an ETag over last_seq would be answered 304
// while the list now shows an attempt it has never seen.
func TestADeliveryClaimMovesTheCommentListsValidator(t *testing.T) {
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "CLAIMW", "A delivery claim", "A spec")
	comment := createReferenceAPIComment(t, handler, issue.Key, "A comment a session is to be told about.")
	target := commentListOf(issue.Key)
	before := commentListValidator(t, handler, target, "GET "+target)
	lastSeq := issueLastSeq(t, database, issue.Key)

	ctx := context.Background()
	tx, err := served.begin(ctx)
	if err != nil {
		t.Fatalf("open the claim's transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := openCommentDeliveryAttempt(ctx, tx, comment, ResolvedMention{Target: "session:s1", Delivery: "steer"}); err != nil {
		t.Fatalf("claim the delivery attempt: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the claim: %v", err)
	}

	if moved := issueLastSeq(t, database, issue.Key); moved != lastSeq {
		t.Fatalf("the issue's last_seq moved from %d to %d; this case needs a write that appends no event", lastSeq, moved)
	}
	requireMovedValidator(t, handler, target, before, "the delivery claim")
	answered := listRead(t, handler, target, listReadOptions{trees: true})
	if _, held := valueAt(t, answered, "0.deliveries.0"); !held {
		t.Errorf("the answer carries no delivery attempt:\n\t%s", answered.Body.String())
	}
}

// The resolution a sender records on its claimed attempt before it sends moves the validator too,
// on the same terms: recordCommentDeliveryResolution writes the session the frame is going to
// onto the pending row in a transaction of its own and appends no event, and the list shows that
// session.
func TestADeliveryResolutionMovesTheCommentListsValidator(t *testing.T) {
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "RESOLVEW", "A delivery resolution", "A spec")
	comment := createReferenceAPIComment(t, handler, issue.Key, "A comment whose delivery is about to be resolved.")
	target := commentListOf(issue.Key)

	ctx := context.Background()
	tx, err := served.begin(ctx)
	if err != nil {
		t.Fatalf("open the claim's transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	claim, err := openCommentDeliveryAttempt(ctx, tx, comment, ResolvedMention{Target: "session:s1", Delivery: "steer"})
	if err != nil {
		t.Fatalf("claim the delivery attempt: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the claim: %v", err)
	}

	// The validator the claim left behind: what moves from here is the resolution alone.
	before := commentListValidator(t, handler, target, "GET "+target+" after the claim")
	lastSeq := issueLastSeq(t, database, issue.Key)
	resolved := ResolvedMention{Target: "session:s1", Delivery: "steer", SessionID: new("s1")}
	recorded, _, held, err := served.recordCommentDeliveryResolution(ctx, comment, resolved, claim, nil)
	if err != nil {
		t.Fatalf("record the delivery's resolution: %v", err)
	}
	if !held || recorded.SessionID == nil || *recorded.SessionID != "s1" {
		t.Fatalf("the resolution was not written onto the claimed attempt: held=%t row=%#v", held, recorded)
	}

	if moved := issueLastSeq(t, database, issue.Key); moved != lastSeq {
		t.Fatalf("the issue's last_seq moved from %d to %d; this case needs a write that appends no event", lastSeq, moved)
	}
	requireMovedValidator(t, handler, target, before, "the delivery's resolution")
}

// Every write of a comment moves the validator of the list it is on: the create, the edit, the
// resolve, the reopen, and the delete, which no route makes and a repair or a test makes straight
// through SQL. The triggers carry no column list for exactly this reason - whatever a write
// writes, the list that shows it moves.
func TestEveryCommentWriteMovesTheCommentListsValidator(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "WRITEW", "Every comment write", "A spec")
	target := commentListOf(issue.Key)
	etag := commentListValidator(t, handler, target, "GET "+target+" before any comment")

	comment := createReferenceAPIComment(t, handler, issue.Key, "The **first** wording of this comment.")
	etag = requireMovedValidator(t, handler, target, etag, "the comment was created")

	requireEdit(t, "edit the comment", editCommentAs(t, handler, comment.ID, "The second wording of this comment.", nil, nil))
	etag = requireMovedValidator(t, handler, target, etag, "the comment was edited")

	for _, action := range []string{"resolve", "reopen"} {
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/"+action, map[string]any{}, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("%s the comment: status=%d body=%s", action, response.Code, response.Body.String())
		}
		etag = requireMovedValidator(t, handler, target, etag, "the comment was "+action+"d")
	}

	if _, err := database.Pool.Exec(context.Background(), `delete from comments where id = $1`, comment.ID); err != nil {
		t.Fatalf("delete the comment: %v", err)
	}
	requireMovedValidator(t, handler, target, etag, "the comment was deleted")
}

// A mention row written and a mention row removed each move the validator, though neither touches
// the comment and neither appends an event: the list carries every comment's mentions
// (hydrateCommentSideTables), so a reader answered 304 across either would hold a list that is no
// longer the one Dispatch would answer.
func TestAMentionWrittenAndRemovedEachMoveTheCommentListsValidator(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "MENTIONW", "A mention row", "A spec")
	comment := createReferenceAPIComment(t, handler, issue.Key, "A comment a mention is about to be written on.")
	target := commentListOf(issue.Key)
	etag := commentListValidator(t, handler, target, "GET "+target)
	lastSeq := issueLastSeq(t, database, issue.Key)

	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
		insert into comment_mentions (comment_id, target, delivery, resolved_session_id)
		values ($1, 'session:s1', 'steer', 's1')
	`, comment.ID); err != nil {
		t.Fatalf("write the mention: %v", err)
	}
	etag = requireMovedValidator(t, handler, target, etag, "the mention was written")

	if _, err := database.Pool.Exec(ctx, `delete from comment_mentions where comment_id = $1`, comment.ID); err != nil {
		t.Fatalf("remove the mention: %v", err)
	}
	requireMovedValidator(t, handler, target, etag, "the mention was removed")

	if moved := issueLastSeq(t, database, issue.Key); moved != lastSeq {
		t.Fatalf("the issue's last_seq moved from %d to %d; neither mention write appends an event", lastSeq, moved)
	}
}

// The second read of an unchanged comment list builds nothing. It costs the one statement its
// validator is read with and nothing else: no comment row, no mention, no delivery attempt, and
// so no shape, no encode and no compression either. This is the whole point of the watermark -
// the validator over the answer's own bytes could only be known by building the answer - and the
// count below is the number that says the build is gone.
func TestAWarmCommentListReadRunsOnlyItsValidatorsStatement(t *testing.T) {
	database := storetest.Open(t)
	counter := &roundTrips{}
	traced := &store.Store{Pool: storetest.Traced(t, database.Pool, counter, nil)}
	handler, _, _, served := newRegisteredTestServer(t, testServerOptions{
		store: traced,
		// The caller's identity is resolved on the untraced pool, so the count is the read's own
		// statements and not the sign-in lookup every request makes.
		identity: headerIdentity(database),
		// A settlement of the issue's spec is database work of its own on the same pool; a delay
		// no test run reaches keeps it out of the count.
		settle: time.Hour,
	})
	key := issueWithComments(t, handler, "WARMW", 5)
	target := commentListOf(key)

	first := listRead(t, handler, target, listReadOptions{trees: true})
	etag := listETagOf(t, first, "the first read of "+target)
	answered := served.listAnswers.answered()
	counter.take()
	second := listRead(t, handler, target, listReadOptions{trees: true, counted: true})
	counted := counter.take()

	if got := listETagOf(t, second, "the second read of "+target); got != etag {
		t.Fatalf("the second read of %s answers %s, want the %s the first gave", target, got, etag)
	}
	if written := served.listAnswers.answered() - answered; written != 1 {
		t.Errorf("the cache wrote %d answers over the second read of %s, want the one it kept", written, target)
	}
	if len(counted) != 1 || !strings.Contains(counted[0], "comment_watermarks") {
		t.Fatalf("the warm read ran %d statements, want the one its validator is read with:\n%s",
			len(counted), strings.Join(roundTripLines(counted), "\n"))
	}
	t.Logf("a warm comment-list read costs %d round trip: %s", len(counted), strings.Join(roundTripLines(counted), "\n"))
}

// No two comment lists of one deploy share a validator: not two issues standing at the same
// counts, and not an issue's whole list and the same list scoped to one of its documents. The
// owner and the ?artifact= scope are hashed in beside the watermark for this, and a validator
// crossed between them is answered the whole list rather than a 304.
func TestNoTwoCommentListsShareAValidator(t *testing.T) {
	handler := newTestHandler(t)
	first := createInteractionIssue(t, handler, "SCOPEW", "The first issue", "A spec")
	// The second issue is of the same project and carries one comment too, so both lists stand at
	// the same counts: without the owner in the hash they would answer one validator.
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "SCOPEW", "title": "The second issue", "spec": "A spec",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create the second issue: status=%d body=%s", created.Code, created.Body.String())
	}
	second := decodeBody[struct {
		Key string `json:"key"`
	}](t, created)
	createReferenceAPIComment(t, handler, first.Key, "A comment on the first issue.")
	createReferenceAPIComment(t, handler, second.Key, "A comment on the second issue.")

	lists := map[string]string{
		commentListOf(first.Key):  "",
		commentListOf(second.Key): "",
		commentListOf(first.Key) + "?artifact=" + first.PrimaryArtifactID: "",
	}
	for target := range lists {
		lists[target] = commentListValidator(t, handler, target, "GET "+target)
	}
	seen := map[string]string{}
	for target, etag := range lists {
		if other, held := seen[etag]; held {
			t.Errorf("GET %s and GET %s answer one validator %s", target, other, etag)
		}
		seen[etag] = target
	}
	crossed := listRead(t, handler, commentListOf(second.Key),
		listReadOptions{trees: true, etag: lists[commentListOf(first.Key)]})
	if crossed.Code != http.StatusOK {
		t.Errorf("GET %s holding the other issue's validator: status=%d, want 200", commentListOf(second.Key), crossed.Code)
	}
}

// A project document's comment list answers under a validator of its own, moves it for a comment
// written on that document, and answers its own 304 with the two Vary lines every list carries:
// it is the same list as an issue's, through the same handler (answerCommentList).
func TestADocumentsCommentListAnswersItsOwnNotModified(t *testing.T) {
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "DOCW", "name": "Documents",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create the project: status=%d body=%s", response.Code, response.Body.String())
	}
	document := createProjectDocument(t, handler, "DOCW", "Design notes", "# Design notes\n")
	target := "/api/v1/artifacts/" + document.ID + "/comments"

	etag := commentListValidator(t, handler, target, "GET "+target)
	requireListVary(t, listRead(t, handler, target, listReadOptions{trees: true}), "the 200 of GET "+target)
	notModified := listRead(t, handler, target, listReadOptions{trees: true, etag: etag})
	requireNotModified(t, notModified, etag, "GET "+target+" holding its own validator")
	requireListVary(t, notModified, "the 304 of GET "+target)

	written := dispatchRequest(t, handler, http.MethodPost, target, map[string]any{
		"body": "A comment on the document itself.",
	}, "alice")
	if written.Code != http.StatusCreated {
		t.Fatalf("comment on the document: status=%d body=%s", written.Code, written.Body.String())
	}
	requireMovedValidator(t, handler, target, etag, "the document was commented on")
}

// A comment write held open blocks no other write on the same issue. This is what the triggers
// are deferred for: an immediate trigger would take the issue's counter row where the write
// happens and hold it until that transaction ended, so one long-running comment transaction would
// serialise every comment write of its issue for its whole life. The second writer sets a lock
// timeout, so an immediate trigger fails this test in seconds instead of hanging the suite.
func TestACommentWriteHeldOpenBlocksNoOtherWriteOnTheSameIssue(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "HELDW", "A write held open", "A spec")
	ctx := context.Background()

	holder, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("open the held transaction: %v", err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx, `
		insert into comments (issue_key, author, body) values ($1, '{"kind":"user","id":"alice"}', 'The write that stays open.')
	`, issue.Key); err != nil {
		t.Fatalf("write the uncommitted comment: %v", err)
	}

	other, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("open the second writer's transaction: %v", err)
	}
	defer func() { _ = other.Rollback(ctx) }()
	if _, err := other.Exec(ctx, `set local lock_timeout = '5s'`); err != nil {
		t.Fatalf("set the second writer's lock timeout: %v", err)
	}
	if _, err := other.Exec(ctx, `
		insert into comments (issue_key, author, body) values ($1, '{"kind":"user","id":"alice"}', 'The write that must not wait.')
	`, issue.Key); err != nil {
		t.Fatalf("the second write on %s waited on the held one: %v", issue.Key, err)
	}
	if err := other.Commit(ctx); err != nil {
		t.Fatalf("the second write on %s could not commit while the first was held open: %v", issue.Key, err)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("commit the held write: %v", err)
	}

	target := commentListOf(issue.Key)
	if listed := decodeBody[[]map[string]any](t, listRead(t, handler, target, listReadOptions{})); len(listed) != 2 {
		t.Fatalf("the list carries %d comments, want the two both writers committed", len(listed))
	}
}

// Two writers committing at once each keep their bump. The counter is raised at commit by a
// deferred trigger, so the risk is one commit's bump overwriting the other's rather than adding
// to it: an upsert whose update reads the row it waited on adds both, a plpgsql read of the
// counter followed by a write of what it read would lose one. Both transactions are open before
// either commits, and the list's validator is read after each commit, so a lost bump shows as a
// validator that did not move.
func TestTwoWritersCommittingAtOnceEachKeepTheirBump(t *testing.T) {
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "BOTHW", "Two writers at once", "A spec")
	comment := createReferenceAPIComment(t, handler, issue.Key, "The comment a delivery is claimed on.")
	target := commentListOf(issue.Key)
	before := commentListValidator(t, handler, target, "GET "+target)
	start := commentWatermark(t, database, issue.Key)
	ctx := context.Background()

	writer, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("open the comment writer's transaction: %v", err)
	}
	defer func() { _ = writer.Rollback(ctx) }()
	if _, err := writer.Exec(ctx, `
		insert into comments (issue_key, author, body) values ($1, '{"kind":"user","id":"alice"}', 'A second comment.')
	`, issue.Key); err != nil {
		t.Fatalf("write the second comment: %v", err)
	}

	claimer, err := served.begin(ctx)
	if err != nil {
		t.Fatalf("open the claim's transaction: %v", err)
	}
	defer func() { _ = claimer.Rollback(ctx) }()
	if _, err := openCommentDeliveryAttempt(ctx, claimer, comment, ResolvedMention{Target: "session:s1", Delivery: "steer"}); err != nil {
		t.Fatalf("claim the delivery attempt: %v", err)
	}

	if err := writer.Commit(ctx); err != nil {
		t.Fatalf("commit the comment: %v", err)
	}
	between := requireMovedValidator(t, handler, target, before, "the comment's commit")
	if err := claimer.Commit(ctx); err != nil {
		t.Fatalf("commit the claim: %v", err)
	}
	after := requireMovedValidator(t, handler, target, between, "the claim's commit")
	if after == before {
		t.Fatalf("the list answers the validator %s it stood at before either commit", before)
	}

	if moved := commentWatermark(t, database, issue.Key); moved != start+2 {
		t.Fatalf("the watermark moved from %d to %d over two committed writes, want %d: a bump was lost",
			start, moved, start+2)
	}
}

// Two transactions whose commits overlap each add to the counter. Both write a row the triggers
// watch and both are committed from goroutines of their own, so the second commit's bump waits on
// the first's row lock and is applied to what that commit left, never to what it read before it.
func TestOverlappingCommitsEachAddToTheWatermark(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "RACEW", "Overlapping commits", "A spec")
	ctx := context.Background()
	start := commentWatermark(t, database, issue.Key)

	const writers = 8
	open := make([]pgx.Tx, 0, writers)
	for writer := range writers {
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("open writer %d's transaction: %v", writer, err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, `
			insert into comments (issue_key, author, body) values ($1, '{"kind":"user","id":"alice"}', 'A comment.')
		`, issue.Key); err != nil {
			t.Fatalf("writer %d's comment: %v", writer, err)
		}
		open = append(open, tx)
	}

	var wait sync.WaitGroup
	failures := make([]error, writers)
	for writer, tx := range open {
		wait.Add(1)
		go func() {
			defer wait.Done()
			failures[writer] = tx.Commit(ctx)
		}()
	}
	wait.Wait()
	for writer, err := range failures {
		if err != nil {
			t.Fatalf("writer %d's commit: %v", writer, err)
		}
	}

	if moved := commentWatermark(t, database, issue.Key); moved != start+writers {
		t.Fatalf("the watermark moved from %d to %d over %d committed writes, want %d: bumps were lost",
			start, moved, writers, start+writers)
	}
}
