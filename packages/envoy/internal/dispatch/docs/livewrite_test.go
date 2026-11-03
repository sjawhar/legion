package docs

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A settlement already past its entry check when a transaction opens its write can reach the
// room after the transaction commits and before its write is published. The room then lacks
// the committed write while the durable cursor includes it, so a version written then would
// stand without the write. It must settle again after the publish instead.
func TestSettlementBetweenCommitAndPublishWaitsForTheWrite(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")
	paused := make(chan struct{})
	resume := make(chan struct{})
	var pausedOnce atomic.Bool
	service.afterSettleWarm = func(room string) {
		if room == artifactID && pausedOnce.CompareAndSwap(false, true) {
			close(paused)
			<-resume
		}
	}
	state := service.room(artifactID)
	state.mu.Lock()
	generation := state.roomGeneration
	state.mu.Unlock()
	settled := make(chan struct{})
	go func() {
		defer close(settled)
		service.settleRoom(artifactID, generation)
	}()
	t.Cleanup(func() {
		select {
		case <-resume:
		default:
			close(resume)
		}
		<-settled
	})
	select {
	case <-paused:
	case <-time.After(5 * time.Second):
		t.Fatal("settlement never reached its warm hook")
	}

	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin edit transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, _, err := lockArtifactOwner(ctx, tx, artifactID); err != nil {
		t.Fatalf("lock document owner: %v", err)
	}
	joined, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	alice := model.Actor{Kind: "user", ID: "alice"}
	if _, err := service.ApplyOps(joined, artifactID, []model.EditOp{{Op: "replace", Find: "before", With: "after"}}, alice, nil); err != nil {
		t.Fatalf("apply joined edit: %v", err)
	}
	if _, err := service.SnapshotVersion(joined, artifactID, alice); err != nil {
		t.Fatalf("snapshot joined edit: %v", err)
	}
	// A browser types while the edit's transaction is open.
	editLiveTree(t, service, artifactID, appendBlocks(t, "typed"))
	if err := ledger.commit(ctx); err != nil {
		t.Fatalf("commit edit transaction: %v", err)
	}
	close(resume)
	select {
	case <-settled:
	case <-time.After(10 * time.Second):
		t.Fatal("paused settlement did not finish")
	}
	ledger.publish()
	settleCurrentGeneration(t, service, artifactID)

	var markdown string
	if err := service.store.Pool.QueryRow(ctx, `
		select markdown from artifact_versions where artifact_id = $1 order by number desc limit 1
	`, artifactID).Scan(&markdown); err != nil {
		t.Fatalf("read latest version: %v", err)
	}
	if markdown != "after\n\ntyped\n" {
		t.Fatalf("latest version = %q, want the committed edit and the browser's paragraph", markdown)
	}
}

// A commit that returns an error may still have committed. Failing the room instead of
// discarding its write makes the room reload whatever the durable document holds.
func TestFailedLiveWritesReloadTheDurableDocument(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "before")
	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin edit transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	joined, ledger := service.Join(ctx, tx)
	if _, err := service.ApplyOps(joined, artifactID, []model.EditOp{{Op: "replace", Find: "before", With: "after"}}, model.Actor{Kind: "user", ID: "alice"}, nil); err != nil {
		t.Fatalf("apply joined edit: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit edit transaction: %v", err)
	}
	ledger.fail(errors.New("commit outcome unknown"))
	if got, err := service.Text(ctx, artifactID); err != nil || got != "after\n" {
		t.Fatalf("document after failing its live write = %q (%v), want the committed text", got, err)
	}
}

// A transaction that does not commit credits no one: the next settled version names only the
// actor whose write reached the document, and so does the version's event.
func TestARolledBackWriteIsNoAuthorOfTheNextVersion(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "before")
	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin edit transaction: %v", err)
	}
	joined, ledger := service.Join(ctx, tx)
	rolledBack := model.Actor{Kind: "session", ID: "session-0123456789abcdef"}
	if _, err := service.ReplaceText(joined, artifactID, "rolled back", rolledBack); err != nil {
		t.Fatalf("replace text in the transaction: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("roll back edit transaction: %v", err)
	}
	ledger.Discard()

	alice := model.Actor{Kind: "user", ID: "alice"}
	if _, err := joinedReplaceText(service, artifactID, "after", alice); err != nil {
		t.Fatalf("replace text: %v", err)
	}
	version := waitForDocumentVersion(t, service.store, artifactID, 2)
	if !reflect.DeepEqual(version.Authors, []model.Actor{alice}) {
		t.Fatalf("version 2 authors = %#v, want only %v", version.Authors, alice)
	}
	var eventActor model.Actor
	if err := service.store.Pool.QueryRow(ctx, `
		select actor from events where issue_key = 'DOC-1' and type = 'artifact.version' order by seq desc limit 1
	`).Scan(&eventActor); err != nil {
		t.Fatalf("read version event: %v", err)
	}
	if eventActor != alice {
		t.Fatalf("version event actor = %v, want %v", eventActor, alice)
	}
}

// A browser connected while a transaction changes its document made none of the change: the
// version the transaction writes names only its writer, as a write that reaches the room
// directly does, and a version settled after the browser left names only its own editor.
func TestAJoinedWritesVersionNamesItsWriterNotAConnectedBrowser(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")
	browser := model.Actor{Kind: "user", ID: "alice"}
	connectionID := service.nextConnection.Add(1)
	service.addConnection(artifactID, connectionID, browser)

	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin edit transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, _, err := lockArtifactOwner(ctx, tx, artifactID); err != nil {
		t.Fatalf("lock document owner: %v", err)
	}
	joined, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	writer := model.Actor{Kind: "user", ID: "bob"}
	if _, err := service.ApplyOps(joined, artifactID, []model.EditOp{{Op: "replace", Find: "before", With: "during"}}, writer, nil); err != nil {
		t.Fatalf("apply joined edit: %v", err)
	}
	snapshot, err := service.SnapshotVersion(joined, artifactID, writer)
	if err != nil {
		t.Fatalf("snapshot joined edit: %v", err)
	}
	if err := ledger.commit(ctx); err != nil {
		t.Fatalf("commit edit transaction: %v", err)
	}
	ledger.publish()
	if want := []model.Actor{writer}; !reflect.DeepEqual(snapshot.Version.Authors, want) {
		t.Fatalf("joined edit's version authors = %#v, want only the writer %v (browser %v only watched)", snapshot.Version.Authors, want, browser)
	}

	service.removeConnection(artifactID, connectionID)
	later := model.Actor{Kind: "user", ID: "carol"}
	if _, err := joinedReplaceText(service, artifactID, "after", later); err != nil {
		t.Fatalf("replace text: %v", err)
	}
	settleCurrentGeneration(t, service, artifactID)
	version := waitForDocumentVersion(t, service.store, artifactID, 3)
	if !reflect.DeepEqual(version.Authors, []model.Actor{later}) {
		t.Fatalf("version 3 authors = %#v, want only %v", version.Authors, later)
	}
}

// A browser's update that its room's batch still holds back, lingering for the keystrokes after it,
// can be stored first by a transaction that reads the room: a snapshot's capture stores the room's
// unstored updates through the point it read (Ledger.flushThrough). That update is the room's own,
// and the instance's cursor advances over it only when the transaction commits, so the
// transaction's next write must still fork the room, not the committed store, which holds neither
// the update nor anything after it. A comment the browser anchored on the words that update typed
// - the margin's re-anchor after a refused send (packages/dispatch/e2e/compose-wait.e2e.ts) - was
// otherwise refused, or marked orphaned by the refresh of the stale fork's anchors.
func TestAWriteAfterItsTransactionStoredTheRoomsLingeringUpdateForksTheRoom(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "# First")
	appends := &heldStore{VersionedStore: NewPgVersioned(database), room: artifactID, entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(appends.let)
	service := newShutdownTestService(t, database, appends)
	seedServiceText(t, service, artifactID, "First.\n")
	settleCurrentGeneration(t, service, artifactID)
	waitForLandedAppends(t, service, artifactID)

	// The browser's update reaches the room's persistence, whose batch is held before it takes the
	// document's lock, as one lingering for the keystrokes after it is.
	appends.armed.Store(true)
	editAsPeer(t, service, artifactID, appendBlocks(t, "Browser words.\n"))
	select {
	case <-appends.entered:
	case <-time.After(roomWait(t)):
		t.Fatal("the browser's update never reached the room's persistence")
	}

	alice := model.Actor{Kind: "user", ID: "alice"}
	const markID = "00000000-0000-4000-8000-000000000605"
	held := storedHead(t, database, artifactID)
	cursor := service.cursorOf(service.srv.GetDoc(artifactID))
	if cursor == nil || cursor.heldThrough() != held {
		t.Fatalf("the room's cursor does not stand at the stored head %d before the snapshot", held)
	}
	err := joinedWrite(service, func(ctx context.Context) error {
		if _, err := service.SnapshotVersion(ctx, artifactID, alice); err != nil {
			return fmt.Errorf("snapshot the document: %w", err)
		}
		// What the write below meets: the snapshot stored the browser's held update in this
		// transaction, and the room's cursor has not moved over it, since that happens only when
		// the transaction commits. Without both, the write would fork the room either way.
		tx, _ := txFromContext(ctx)
		head, err := currentUpdateCursor(ctx, tx, artifactID)
		if err != nil {
			return fmt.Errorf("read the transaction's head: %w", err)
		}
		if head <= held {
			return fmt.Errorf("the snapshot stored nothing of the room's held update: the transaction's head is %d, the stored head was %d", head, held)
		}
		if through := cursor.heldThrough(); through != held {
			return fmt.Errorf("the room's cursor stands at %d inside the transaction, want %d: the snapshot's versions are not committed", through, held)
		}
		if _, err := service.MarkQuote(ctx, artifactID, MarkSpec{Kind: MarkComment, ID: markID, By: alice}, "Browser words.", nil); err != nil {
			return fmt.Errorf("mark the browser's words after the snapshot stored them: %w", err)
		}
		return nil
	})
	appends.let()
	if err != nil {
		t.Fatal(err)
	}
	waitForLandedAppends(t, service, artifactID)
	tree, err := service.storedTree(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("read the stored document: %v", err)
	}
	if _, err := readAnchored(tree, MarkComment, markID); err != nil {
		t.Fatalf("the stored document does not hold the comment's mark on the browser's words: %v", err)
	}
}
