package docs

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
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
	generation := state.gen
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
	joined, collector := joinTx(ctx, tx)
	defer service.DiscardLiveWrites(collector)
	alice := model.Actor{Kind: "user", ID: "alice"}
	if _, err := service.ApplyOps(joined, artifactID, []model.EditOp{{Op: "replace", Find: "before", With: "after"}}, alice, nil); err != nil {
		t.Fatalf("apply joined edit: %v", err)
	}
	if _, err := service.SnapshotVersion(joined, tx, artifactID, alice); err != nil {
		t.Fatalf("snapshot joined edit: %v", err)
	}
	// A browser types while the edit's transaction is open.
	editLiveTree(t, service, artifactID, appendBlocks(t, "typed"))
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit edit transaction: %v", err)
	}
	service.CreditLiveWrites(collector)
	close(resume)
	select {
	case <-settled:
	case <-time.After(10 * time.Second):
		t.Fatal("paused settlement did not finish")
	}
	service.PublishLiveWrites(collector)
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
	joined, collector := joinTx(ctx, tx)
	if _, err := service.ApplyOps(joined, artifactID, []model.EditOp{{Op: "replace", Find: "before", With: "after"}}, model.Actor{Kind: "user", ID: "alice"}, nil); err != nil {
		t.Fatalf("apply joined edit: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit edit transaction: %v", err)
	}
	service.FailLiveWrites(collector, errors.New("commit outcome unknown"))
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
	joined, collector := joinTx(ctx, tx)
	rolledBack := model.Actor{Kind: "session", ID: "session-0123456789abcdef"}
	if _, err := service.ReplaceText(joined, artifactID, "rolled back", rolledBack); err != nil {
		t.Fatalf("replace text in the transaction: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("roll back edit transaction: %v", err)
	}
	service.DiscardLiveWrites(collector)

	alice := model.Actor{Kind: "user", ID: "alice"}
	if _, err := service.ReplaceText(ctx, artifactID, "after", alice); err != nil {
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

// A browser connected when a transaction changes its document is credited on the version the
// transaction writes, as it is when a write reaches the room directly, and not again on a
// version settled after it left.
func TestAJoinedWriteCreditsConnectedBrowsersOnItsOwnVersionOnly(t *testing.T) {
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
	joined, collector := joinTx(ctx, tx)
	defer service.DiscardLiveWrites(collector)
	writer := model.Actor{Kind: "user", ID: "bob"}
	if _, err := service.ApplyOps(joined, artifactID, []model.EditOp{{Op: "replace", Find: "before", With: "during"}}, writer, nil); err != nil {
		t.Fatalf("apply joined edit: %v", err)
	}
	snapshot, err := service.SnapshotVersion(joined, tx, artifactID, writer)
	if err != nil {
		t.Fatalf("snapshot joined edit: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit edit transaction: %v", err)
	}
	service.CreditLiveWrites(collector)
	service.CommitVersion(artifactID, snapshot.Version)
	service.PublishLiveWrites(collector)
	if want := []model.Actor{browser, writer}; !reflect.DeepEqual(snapshot.Version.Authors, want) {
		t.Fatalf("joined edit's version authors = %#v, want %v", snapshot.Version.Authors, want)
	}

	service.removeConnection(artifactID, connectionID)
	later := model.Actor{Kind: "user", ID: "carol"}
	if _, err := service.ReplaceText(ctx, artifactID, "after", later); err != nil {
		t.Fatalf("replace text: %v", err)
	}
	settleCurrentGeneration(t, service, artifactID)
	version := waitForDocumentVersion(t, service.store, artifactID, 3)
	if !reflect.DeepEqual(version.Authors, []model.Actor{later}) {
		t.Fatalf("version 3 authors = %#v, want only %v", version.Authors, later)
	}
}

// A write that starts while its room is failing waits for the room to recover and opens on the
// recovered room, whose settlement it then holds off. A browser types while the write is open;
// a settlement between the write's commit and its publish must not version that typing past the
// committed write, which the room does not hold yet.
func TestAWriteOpenedDuringRoomRecoveryHoldsOffTheRecoveredRoom(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")
	ctx := context.Background()
	if err := service.warmLiveDocument(ctx, artifactID); err != nil {
		t.Fatalf("load live document: %v", err)
	}
	// An outside transaction holds the document's advisory lock, so the failed room's eviction,
	// which compacts the document, cannot finish until the test releases it.
	holder, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin lock holder: %v", err)
	}
	defer holder.Rollback(ctx)
	if err := lockDocumentRoom(ctx, holder, artifactID); err != nil {
		t.Fatalf("hold document lock: %v", err)
	}
	service.failRoom(artifactID, errors.New("injected room failure"))

	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin edit transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	joined, collector := joinTx(ctx, tx)
	defer service.DiscardLiveWrites(collector)
	edited := make(chan error, 1)
	go func() {
		_, err := service.ReplaceText(joined, artifactID, "after", model.Actor{Kind: "user", ID: "alice"})
		edited <- err
	}()
	// Give the edit time to reach the failed room before its eviction can finish.
	time.Sleep(100 * time.Millisecond)
	if err := holder.Rollback(ctx); err != nil {
		t.Fatalf("release document lock: %v", err)
	}
	select {
	case err := <-edited:
		if err != nil {
			t.Fatalf("replace text in the transaction: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("joined edit did not finish once the room could recover")
	}
	editLiveTree(t, service, artifactID, appendBlocks(t, "typed"))
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit edit transaction: %v", err)
	}
	service.CreditLiveWrites(collector)
	settleCurrentGeneration(t, service, artifactID)
	service.PublishLiveWrites(collector)
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
