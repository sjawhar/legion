package docs

import (
	"context"
	"errors"
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
