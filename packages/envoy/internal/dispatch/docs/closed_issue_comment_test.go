package docs

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
)

// A closed issue still takes a comment, so its document still takes that comment's anchor under
// WithCommentMarks (mutation_test.go, TestClosedIssueTakesOnlyCommentMarksUnderWithCommentMarks).
// The tests here are the rooms a comment's anchor meets: none at all, where the write is cold
// and its publish arms a settlement (publishLiveWrite), and a resident one holding an update of
// its own that the store will never take, because the gate refuses a browser update the room
// took more than closeMargin after the close (gateAppend) - for the anchor that writes its own
// mark, and for the one anchored to a mark already in the document, which writes nothing and
// only pins a version. In each, the comment is written, nothing is left failed, and the document
// still reads and still takes the next comment.

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
	service, held := closedIssueRoomService(t, database, artifactID)
	seedServiceText(t, service, artifactID, "before the close")
	waitForLandedAppends(t, service, artifactID)

	alice := model.Actor{Kind: "user", ID: "alice"}
	holdOneRoomUpdate(t, held, func() {
		browserReplaceText(t, service, artifactID, "before the close\n\ntyped in the room\n", alice)
	})
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

// A comment anchored to a mark already in the document - a reply or a comment the browser points
// at an existing anchor (api/anchors.go, resolveAnchor's mark_id branch) - writes nothing to the
// document: it verifies the mark and pins the version its anchor names (VerifyMark,
// SnapshotVersion). With no write of its own, that version is captured from the room
// (captureLiveTextAndAuthors), which stores the room's updates ahead of it - and the gate refuses
// the one this room took after the close, which answered the comment 409 ISSUE_CLOSED. The
// version is taken from the stored document instead (closedIssueCommentVersion), as a version of
// a room behind the head is, and stores nothing of the room.
func TestAnAnchoredCommentOnAClosedIssuePinsAStoredVersionWhileItsRoomHoldsAnUnstoredUpdate(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before the close")
	service, held := closedIssueRoomService(t, database, artifactID)
	seedServiceText(t, service, artifactID, "before the close")
	waitForLandedAppends(t, service, artifactID)

	// The mark the later comment anchors to, placed while the issue is open.
	alice := model.Actor{Kind: "user", ID: "alice"}
	if _, err := commentAnchor(service, artifactID, "c1", "before", alice); err != nil {
		t.Fatalf("the comment's anchor while the issue is open: %v", err)
	}
	waitForLandedAppends(t, service, artifactID)
	// A keystroke that leaves the first paragraph, and the comment mark in it, as they are: the
	// room's alone, since the store holds its append.
	holdOneRoomUpdate(t, held, func() {
		editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
			tree.Children = append(tree.Children, &pmdoc.Node{
				Type: "paragraph", Children: []*pmdoc.Node{{Type: "text", Text: "typed in the room"}},
			})
			return tree
		})
	})
	closeIssueAgo(t, database, "DOC-1", closeMargin+30*time.Second)

	var failed atomic.Bool
	service.beforeFailedEviction = func(string) { failed.Store(true) }
	var anchored Anchored
	var snapshot VersionResult
	if err := joinedWrite(service, func(ctx context.Context) (err error) {
		ctx = WithCommentMarks(ctx)
		if anchored, err = service.VerifyMark(ctx, artifactID, MarkComment, "c1"); err != nil {
			return err
		}
		snapshot, err = service.SnapshotVersion(ctx, artifactID, alice)
		return err
	}); err != nil {
		t.Fatalf("a comment anchored to a mark already in the closed issue's document: %v", err)
	}
	if anchored.Quote != "before" {
		t.Fatalf("the verified mark anchored = %#v, want the quote it marks", anchored)
	}
	if snapshot.Version.Number == 0 {
		t.Fatalf("the anchor pinned version %#v, want the version holding its mark", snapshot.Version)
	}
	if stored := storedDocumentWith(t, database, artifactID); strings.Contains(stored, "typed in the room") {
		t.Fatalf("the stored document = %q, want the version to have stored none of the room's updates on the closed issue", stored)
	}
	if failed.Load() {
		t.Fatal("the anchor's version failed the document's room")
	}
	// The version the anchor names is the stored document's: it holds the text the mark is in,
	// and none of the room's update the store will never take.
	markdown := versionMarkdown(t, database, artifactID, snapshot.Version.Number)
	if !strings.Contains(markdown, "before the close") || strings.Contains(markdown, "typed in the room") {
		t.Fatalf("the pinned version reads %q, want the stored document the mark is in", markdown)
	}
	if err := joinedWrite(service, func(ctx context.Context) error {
		_, err := service.VerifyMark(WithCommentMarks(ctx), artifactID, MarkComment, "c1")
		return err
	}); err != nil {
		t.Fatalf("the mark the pinned version names, read again: %v", err)
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

// browserCommentMark is a write of a comment mark over a word of its own into the live tree, the
// way a browser's editor writes the mark it then sends as mark_id (ProofDocument.tsx,
// onMarkAction).
func browserCommentMark(t *testing.T, service *Service, artifactID, markID, text string) func() {
	return func() {
		editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
			paragraph := tree.Children[0]
			paragraph.Children = append(paragraph.Children, &pmdoc.Node{
				Type: "text", Text: text,
				Marks: []pmdoc.Mark{{Type: string(MarkComment), Attrs: pmdoc.Attrs{"id": markID, "by": "user:alice"}}},
			})
			return tree
		})
	}
}

// verifyAndPin is what a comment anchored to a mark already in the document does
// (api/anchors.go, resolveAnchor's mark_id branch): it verifies the mark and pins the version
// its anchor names, both under WithCommentMarks, in the comment's own transaction.
func verifyAndPin(service *Service, artifactID, markID string, actor model.Actor) (Anchored, VersionResult, error) {
	var anchored Anchored
	var snapshot VersionResult
	err := joinedWrite(service, func(ctx context.Context) (err error) {
		ctx = WithCommentMarks(ctx)
		if anchored, err = service.VerifyMark(ctx, artifactID, MarkComment, markID); err != nil {
			return err
		}
		snapshot, err = service.SnapshotVersion(ctx, artifactID, actor)
		return err
	})
	return anchored, snapshot, err
}

// storedVersions is how many versions the document has.
func storedVersions(t *testing.T, database *store.Store, artifactID string) int {
	t.Helper()
	var versions int
	if err := database.Pool.QueryRow(context.Background(),
		`select count(*) from artifact_versions where artifact_id = $1`, artifactID).Scan(&versions); err != nil {
		t.Fatalf("count the document's versions: %v", err)
	}
	return versions
}

// The mark a mark_id anchor names is usually one the browser wrote itself (ProofDocument.tsx,
// onMarkAction): on a process that has not heard the close its editor is still writable, so that
// mark can be in the room while no stored row holds it - the gate refuses the update carrying it
// (gateAppend, appendBrowser). The version the anchor is pinned to comes from the stored
// document (captureLiveTextAndAuthors), so a mark verified against the room would pin a tree
// that does not hold it: a 201 whose anchor the next refresh orphans at once, told to nobody.
// The mark is read from that same stored document (closedIssueCommentAnchor), so a mark only the
// room holds is no anchor: ErrAnchorMissing, which the API answers 409 ANCHOR_MISSING and the
// browser's margin recovers from, and no version is written behind it.
func TestAMarkOnlyAClosedIssuesRoomHoldsIsNoAnchor(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before the close")
	service, held := closedIssueRoomService(t, database, artifactID)
	seedServiceText(t, service, artifactID, "before the close")
	alice := model.Actor{Kind: "user", ID: "alice"}
	// A stored change the latest version does not hold, so a version taken here writes a row:
	// the count below says whether this anchor wrote one.
	if _, err := joinedApplyOps(service, artifactID, []model.EditOp{
		{Op: "replace", Find: "before the close", With: "before the close, edited"},
	}, alice, nil); err != nil {
		t.Fatalf("edit the document while its issue is open: %v", err)
	}
	waitForLandedAppends(t, service, artifactID)
	versions := storedVersions(t, database, artifactID)

	// The browser's own comment mark, written into the room while this process still takes its
	// edits and held out of the store.
	holdOneRoomUpdate(t, held, browserCommentMark(t, service, artifactID, "b1", "typed"))
	closeIssueAgo(t, database, "DOC-1", closeMargin+30*time.Second)

	var failed atomic.Bool
	service.beforeFailedEviction = func(string) { failed.Store(true) }
	_, _, err := verifyAndPin(service, artifactID, "b1", alice)
	if !errors.Is(err, ErrAnchorMissing) {
		t.Fatalf("a comment anchored to a mark only the closed issue's room holds = %v, want ErrAnchorMissing", err)
	}
	if wrote := storedVersions(t, database, artifactID) - versions; wrote != 0 {
		t.Fatalf("the refused anchor wrote %d version(s), want none pinned to a tree without its mark", wrote)
	}
	if failed.Load() {
		t.Fatal("the refused anchor failed the document's room")
	}

	// What the browser does next: it writes the mark again and sends the comment, which on a
	// read-only reconnect names its text by quote (ProofDocument.tsx, the quote-comment action).
	// That comment is written, and the document still reads.
	if _, err := commentAnchor(service, artifactID, "c1", "before", alice); err != nil {
		t.Fatalf("the comment sent again by quote on the closed issue: %v", err)
	}
	text, err := service.Text(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("read the closed issue's document after its comment: %v", err)
	}
	if !strings.Contains(text, "before the close") {
		t.Fatalf("the closed issue's document reads %q, want the text the comment was anchored in", text)
	}
	if _, err := commentAnchor(service, artifactID, "c2", "edited", alice); err != nil {
		t.Fatalf("a second comment's anchor on the closed issue: %v", err)
	}
}

// The same mark, taken by the room inside closeMargin of the close: the gate admits it
// (observedBeforeClose), the store takes it, and the stored document the anchor reads holds it.
// The mark anchors and the version pinned after it holds it. An append still in flight when the
// anchor is read is the same case a moment earlier: VerifyMark's wait ends in one last read, of
// the store it lands in.
func TestAMarkAClosedIssuesRoomTookInsideTheMarginStillAnchors(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before the close")
	service, held := closedIssueRoomService(t, database, artifactID)
	seedServiceText(t, service, artifactID, "before the close")
	waitForLandedAppends(t, service, artifactID)

	alice := model.Actor{Kind: "user", ID: "alice"}
	holdOneRoomUpdate(t, held, browserCommentMark(t, service, artifactID, "b1", "typed"))
	// The close lands while the room's append of that mark is still held, so the room took it
	// inside the margin; the append then goes through.
	writeIssueClosed(t, database, "DOC-1", true)
	held.let()
	waitForLandedAppends(t, service, artifactID)
	if stored := storedDocumentWith(t, database, artifactID); !strings.Contains(stored, "typed") {
		t.Fatalf("the stored document = %q, want the mark the gate admitted inside the margin", stored)
	}

	anchored, snapshot, err := verifyAndPin(service, artifactID, "b1", alice)
	if err != nil {
		t.Fatalf("a comment anchored to a mark the closed issue's store holds: %v", err)
	}
	if anchored.Quote != "typed" {
		t.Fatalf("the verified mark anchored = %#v, want the text it marks", anchored)
	}
	if snapshot.Version.Number == 0 {
		t.Fatalf("the anchor pinned version %#v, want the version holding its mark", snapshot.Version)
	}
	if markdown := versionMarkdown(t, database, artifactID, snapshot.Version.Number); !strings.Contains(markdown, "typed") {
		t.Fatalf("the pinned version reads %q, want the text its mark is on", markdown)
	}
}

// closedIssueRoomService is a document service whose persistence holds the room's own append once
// armed (heldStore), so a test can leave an update in the room that the store has not taken
// (holdOneRoomUpdate). The hold is released before the service shuts down - cleanups run in
// reverse, and Shutdown waits for the room's persistence worker this hold parks.
func closedIssueRoomService(t *testing.T, database *store.Store, artifactID string) (*Service, *heldStore) {
	t.Helper()
	held := &heldStore{
		VersionedStore: NewPgVersioned(database),
		room:           artifactID,
		entered:        make(chan struct{}),
		release:        make(chan struct{}),
	}
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
	t.Cleanup(held.let)
	return service, held
}

// holdOneRoomUpdate runs write, a browser's change straight into the room, with the store holding
// the room's own append of it: from the moment it returns, that update is the room's and no
// stored row holds it.
func holdOneRoomUpdate(t *testing.T, held *heldStore, write func()) {
	t.Helper()
	held.armed.Store(true)
	write()
	select {
	case <-held.entered:
	case <-time.After(roomWait(t)):
		t.Fatal("the room's own append of the browser's update never reached the store")
	}
}
