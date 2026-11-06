package docs

import (
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
)

// A closed issue still takes a comment, so its document still takes that comment's anchor under
// WithCommentMarks (mutation_test.go, TestClosedIssueTakesOnlyCommentMarksUnderWithCommentMarks).
// The tests here are the two rooms the anchor's write meets: none at all, where the write is
// cold and its publish arms a settlement (publishLiveWrite), and a resident one holding an
// update of its own that the store will never take, because the gate refuses a browser update
// the room took more than closeMargin after the close (gateAppend). In both, the comment is
// written, nothing is left failed, and the document still reads and still takes the next
// comment.

// commentAnchor writes a comment's anchor on artifactID the way the comment's own request writes
// it (api/anchors.go, resolveAnchor): one joined transaction under WithCommentMarks, holding the
// owner row that decided the comment is written, with the mark on the quote, the comment's margin
// record and the version the anchor is pinned to.
func commentAnchor(service *Service, artifactID, markID, quote string, actor model.Actor) (Anchored, error) {
	var anchored Anchored
	err := joinedWrite(service, func(ctx context.Context) (err error) {
		ctx = WithCommentMarks(ctx)
		mark := MarkSpec{Kind: MarkComment, ID: markID, By: actor}
		if anchored, err = service.MarkQuote(ctx, artifactID, mark, quote, nil); err != nil {
			return err
		}
		record := MarkRecord{
			Kind:      "comment",
			By:        ActorRef(actor),
			CreatedAt: time.Now().UTC().Format(time.RFC3339),
			Text:      "About this.",
		}
		if err := service.ProjectMark(ctx, artifactID, markID, record, actor); err != nil {
			return err
		}
		_, err = service.SnapshotVersion(ctx, artifactID, actor)
		return err
	})
	return anchored, err
}

// closeIssueAgo stamps issueKey's closed_at that long ago on the database's clock, as a close
// whose notification this process never heard would stand (writeIssueClosed stamps it now). An
// update a room took more than closeMargin after that moment is the one the gate refuses
// (observedBeforeClose).
func closeIssueAgo(t *testing.T, database *store.Store, issueKey string, ago time.Duration) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(),
		`update issues set closed_at = now() - make_interval(secs => $2) where key = $1`,
		issueKey, ago.Seconds()); err != nil {
		t.Fatalf("close issue %s %v ago: %v", issueKey, ago, err)
	}
}

// A comment's anchor written while no room of this process holds the document is a cold write,
// and its publish arms the settlement the room's update observer would have armed for a
// published update (publishLiveWrite). A closed issue settles nothing: the settlement's warm-up
// load is refused (allowInject, ErrIssueClosed) and settleRoomWithin ends there, leaving the
// document's pending-settlement row for the resumption once the issue reopens. Counted as a
// failure instead, each refusal was retried and the third failed the room (retrySettleLocked):
// three ERROR lines behind the comment's 201, and then every write to the document answered 503
// until the room recovered.
func TestAnAnchoredCommentOnAClosedIssueWithNoRoomFailsNoRoom(t *testing.T) {
	logs := &storetest.LockedLog{}
	logtest.SetDefault(t, slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	service, artifactID := newTestService(t)
	settle := 50 * time.Millisecond
	service.settle = settle
	seedServiceText(t, service, artifactID, "before the close")
	waitForLandedAppends(t, service, artifactID)
	closeDocumentRoom(t, service, artifactID)
	writeIssueClosed(t, service.store, "DOC-1", true)

	var failed atomic.Bool
	service.beforeFailedEviction = func(string) { failed.Store(true) }
	started := make(chan struct{}, 1)
	service.beforeSettleStart = func(string) {
		select {
		case started <- struct{}{}:
		default:
		}
	}

	alice := model.Actor{Kind: "user", ID: "alice"}
	anchored, err := commentAnchor(service, artifactID, "c1", "before", alice)
	if err != nil {
		t.Fatalf("a comment's anchor on the closed issue, with no room of the document: %v", err)
	}
	if anchored.Quote != "before" {
		t.Fatalf("the comment's mark anchored = %#v, want the quote it marked", anchored)
	}
	if service.roomHeld(artifactID) {
		t.Fatal("the cold comment write loaded a room of the document")
	}
	select {
	case <-started:
	case <-time.After(roomWait(t)):
		t.Fatal("the cold write armed no settlement, so this test no longer covers the settlement its publish arms")
	}
	// The third refusal failed the room, three settle delays after the first; this watches ten,
	// and the failure count below catches even the first refusal taken as a fault.
	deadline := time.Now().Add(10 * settle)
	for time.Now().Before(deadline) && !failed.Load() {
		time.Sleep(settle / 5)
	}
	state := service.lockState(artifactID)
	failures := state.settleFailures
	service.unlockState(artifactID, state)
	if failed.Load() || failures != 0 {
		t.Fatalf("the closed issue's settlement was counted %d failure(s), room failed=%t; it logged: %s",
			failures, failed.Load(), logs.Lines("settle"))
	}
	if err := service.roomFailure(artifactID); err != nil {
		t.Fatalf("the document's room = %v, want no failed room", err)
	}
	if lines := logs.Lines("dispatch: settle document"); lines != "" {
		t.Fatalf("the closed issue's settlement logged an error: %s", lines)
	}

	// Behind the comment, the document still reads and still takes the next comment.
	text, err := service.Text(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("read the closed issue's document after its comment: %v", err)
	}
	if !strings.Contains(text, "before the close") {
		t.Fatalf("the closed issue's document reads %q, want the text the comment was anchored in", text)
	}
	if _, err := commentAnchor(service, artifactID, "c2", "close", alice); err != nil {
		t.Fatalf("a second comment's anchor on the closed issue: %v", err)
	}
}

// A comment's anchor written while a room of this process holds the document, on a process whose
// listener has not heard the close (its redial outlasts closeMargin), meets a room still taking
// its browsers' keystrokes. A warm fork is made on them, so the write must store them ahead of
// its own append (recordLiveOperation), and the gate refuses an update the room took that long
// after the close (gateAppend, appendBrowser) - which turned the comment a closed issue takes
// into a 409 ISSUE_CLOSED. The write forks cold instead (readFork): the mark goes on the text the
// store holds, the room's own update is left for the append that closes the room for its issue
// (refuseClosedIssueAppend), and the comment is written.
func TestAnAnchoredCommentOnAClosedIssueIsWrittenWhileItsRoomHoldsAnUnstoredUpdate(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before the close")
	held := &heldStore{
		VersionedStore: NewPgVersioned(database),
		room:           artifactID,
		entered:        make(chan struct{}),
		release:        make(chan struct{}),
	}
	defer held.let()
	service := New(Deps{
		Store: database, Persistence: held, Events: events.NewBroker(),
		Identity: identity.HeaderIdentity{Header: "X-Dispatch-User", People: store.NewPgPeopleStore(database.Pool, testSigningKey, nil)},
		Settle:   time.Hour,
	})
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	seedServiceText(t, service, artifactID, "before the close")
	waitForLandedAppends(t, service, artifactID)

	// A browser's keystroke the room takes and its worker has not stored: the store holds the
	// room's append until this test lets it go, so the update is the room's alone.
	alice := model.Actor{Kind: "user", ID: "alice"}
	held.armed.Store(true)
	browserReplaceText(t, service, artifactID, "before the close\n\ntyped in the room\n", alice)
	select {
	case <-held.entered:
	case <-time.After(roomWait(t)):
		t.Fatal("the room's own append of the browser's update never reached the store")
	}
	// The issue closed that long ago, and nothing told this process: the room took the keystroke
	// above well past the margin the gate admits (observedBeforeClose).
	closeIssueAgo(t, database, "DOC-1", closeMargin+30*time.Second)

	var failed atomic.Bool
	service.beforeFailedEviction = func(string) { failed.Store(true) }
	anchored, err := commentAnchor(service, artifactID, "c1", "before", alice)
	if err != nil {
		t.Fatalf("a comment's anchor on the closed issue, with a room holding an unstored update: %v", err)
	}
	if anchored.Quote != "before" {
		t.Fatalf("the comment's mark anchored = %#v, want the quote it marked", anchored)
	}
	if stored := storedDocumentWith(t, database, artifactID); strings.Contains(stored, "typed in the room") {
		t.Fatalf("the stored document = %q, want the comment's write to have stored none of the room's updates on the closed issue", stored)
	}
	if failed.Load() {
		t.Fatal("the comment's write failed the document's room")
	}

	// Behind the comment, the document still reads and still takes the next comment.
	text, err := service.Text(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("read the closed issue's document after its comment: %v", err)
	}
	if !strings.Contains(text, "before the close") {
		t.Fatalf("the closed issue's document reads %q, want the text the comment was anchored in", text)
	}
	if _, err := commentAnchor(service, artifactID, "c2", "close", alice); err != nil {
		t.Fatalf("a second comment's anchor on the closed issue: %v", err)
	}
}
