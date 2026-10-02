package docs

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

const rebuildUpdateItems = 100_000

func oversizedHistoryUpdate(t *testing.T) []byte {
	t.Helper()
	doc := crdt.New()
	fragment := doc.GetXmlFragment(fragmentName)
	doc.Transact(func(transaction *crdt.Transaction) {
		for index := range rebuildUpdateItems {
			fragment.InsertElement(transaction, index, crdt.NewYXmlElement("paragraph"))
		}
	})
	return crdt.EncodeStateAsUpdateV1(doc, nil)
}

// rebuildDocument rebuilds the way the route does: inside a transaction joined with Service.Join,
// committed only when the rebuild succeeds.
func rebuildDocument(t *testing.T, service *Service, artifactID string, markdown *string) (RebuildReport, VersionResult, error) {
	t.Helper()
	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin rebuild: %v", err)
	}
	defer tx.Rollback(ctx)
	joined, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	report, written, err := service.RebuildDocument(joined, artifactID, markdown, model.Actor{Kind: "user", ID: "alice"})
	if err != nil {
		return RebuildReport{}, VersionResult{}, err
	}
	if err := ledger.Commit(ctx); err != nil {
		t.Fatalf("commit rebuild: %v", err)
	}
	return report, written, nil
}

// A history that exceeds ygo's merged-update item cap cannot load. Rebuilding it preserves the
// latest saved version, advances the durable cursor, and leaves the artifact's version history.
// Until the rebuild's transaction ends its room refuses loads and a second rebuild, which would
// read the old history the transaction has not yet replaced.
func TestRebuildDocumentRestoresDocumentThatCannotLoad(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before\n")
	persistence := NewPgVersioned(database)
	for range 11 {
		if _, err := persistence.AppendUpdate(context.Background(), artifactID, oversizedHistoryUpdate(t)); err != nil {
			t.Fatalf("append accepted history update: %v", err)
		}
	}

	service := newSchemaRepairService(database)
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown rebuilt service: %v", err)
		}
	})
	if _, err := service.Text(context.Background(), artifactID); !errors.Is(err, ErrServiceUnavailable) {
		t.Fatalf("read over-cap history: %v, want ErrServiceUnavailable", err)
	}
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin rebuild: %v", err)
	}
	defer tx.Rollback(ctx)
	joined, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	report, written, err := service.RebuildDocument(joined, artifactID, nil, model.Actor{Kind: "user", ID: "alice"})
	if err != nil {
		t.Fatalf("rebuild document: %v", err)
	}
	if report.SourceVersion != 1 || report.RemovedUpdates != 11 || report.Head != 12 || written.Wrote {
		t.Fatalf("rebuild report = %#v (%#v), want source version 1, 11 updates removed, head 12, no version", report, written)
	}
	if _, _, err := rebuildDocument(t, service, artifactID, nil); !errors.Is(err, ErrDocumentLive) {
		t.Fatalf("second rebuild before the first commits: %v, want ErrDocumentLive", err)
	}
	if err := service.warmLiveDocument(ctx, artifactID); !errors.Is(err, ErrServiceUnavailable) {
		t.Fatalf("load before the rebuild commits: %v, want ErrServiceUnavailable", err)
	}
	if err := ledger.Commit(ctx); err != nil {
		t.Fatalf("commit rebuild: %v", err)
	}
	if text, err := service.Text(context.Background(), artifactID); err != nil || text != "before\n" {
		t.Fatalf("read rebuilt document: %q (%v), want latest version markdown", text, err)
	}
	var updates, versions int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from doc_updates where artifact_id = $1`, artifactID).Scan(&updates); err != nil {
		t.Fatalf("count rebuilt updates: %v", err)
	}
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from artifact_versions where artifact_id = $1`, artifactID).Scan(&versions); err != nil {
		t.Fatalf("count artifact versions after rebuild: %v", err)
	}
	if updates != 1 || versions != 1 {
		t.Fatalf("rebuild left %d updates and %d artifact versions, want 1 and 1", updates, versions)
	}
}

// A resident room remains live even when no writer holds its advisory lock, so the server registry
// refuses rebuilding it without changing durable history.
func TestRebuildDocumentRefusesALiveRoom(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "before\n")
	if err := service.warmLiveDocument(context.Background(), artifactID); err != nil {
		t.Fatalf("load live room: %v", err)
	}
	var before int
	if err := service.store.Pool.QueryRow(context.Background(), `select count(*) from doc_updates where artifact_id = $1`, artifactID).Scan(&before); err != nil {
		t.Fatalf("count updates before rebuild: %v", err)
	}
	if _, _, err := rebuildDocument(t, service, artifactID, nil); !errors.Is(err, ErrDocumentLive) {
		t.Fatalf("rebuild resident room: %v, want ErrDocumentLive", err)
	}
	var after int
	if err := service.store.Pool.QueryRow(context.Background(), `select count(*) from doc_updates where artifact_id = $1`, artifactID).Scan(&after); err != nil {
		t.Fatalf("count updates after refused rebuild: %v", err)
	}
	if after != before {
		t.Fatalf("resident-room rebuild changed doc_updates from %d to %d", before, after)
	}
}

type rebuildCaptureStore struct {
	VersionedStore
	invalid  bool
	rebuilds int
}

func (s *rebuildCaptureStore) Load(ctx context.Context, room string) (persistence.LoadResult, error) {
	if s.invalid {
		s.invalid = false
		return persistence.LoadResult{Update: []byte{0xff}}, nil
	}
	return s.VersionedStore.Load(ctx, room)
}

func (s *rebuildCaptureStore) RebuildTx(ctx context.Context, tx pgx.Tx, room string, seed []byte) (RebuildReport, error) {
	s.rebuilds++
	return s.VersionedStore.RebuildTx(ctx, tx, room, seed)
}

func TestRebuildDocumentWritesThroughItsInjectedPersistence(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before\n")
	persist := &rebuildCaptureStore{
		VersionedStore: NewPgVersioned(database),
		invalid:        true,
	}
	service := New(Deps{Store: database, Persistence: persist})
	replacement := "replacement\n"
	if _, _, err := rebuildDocument(t, service, artifactID, &replacement); err != nil {
		t.Fatalf("rebuild document: %v", err)
	}
	if persist.rebuilds != 1 {
		t.Fatalf("injected persistence rebuild calls = %d, want 1", persist.rebuilds)
	}
}
