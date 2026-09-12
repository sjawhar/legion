package docs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/reearth/ygo/persistence"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	baseURL, err := url.Parse(os.Getenv("DISPATCH_TEST_DATABASE_URL"))
	if err != nil || baseURL.String() == "" {
		t.Skip("DISPATCH_TEST_DATABASE_URL must be set to run Postgres document tests")
	}
	adminURL := *baseURL
	adminURL.Path = "/postgres"
	admin, err := pgxpool.New(context.Background(), adminURL.String())
	if err != nil {
		t.Fatalf("open test database admin pool: %v", err)
	}
	t.Cleanup(admin.Close)

	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatalf("random database name: %v", err)
	}
	databaseName := "dispatch_docs_test_" + hex.EncodeToString(suffix[:])
	if _, err := admin.Exec(context.Background(), "create database "+databaseName); err != nil {
		t.Fatalf("create isolated database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "drop database "+databaseName+" with (force)"); err != nil {
			t.Errorf("drop isolated database: %v", err)
		}
	})

	testURL := *baseURL
	testURL.Path = "/" + databaseName
	database, err := store.Open(context.Background(), testURL.String())
	if err != nil {
		t.Fatalf("open isolated database: %v", err)
	}
	t.Cleanup(database.Pool.Close)
	if err := database.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate isolated database: %v", err)
	}
	return database
}

func createDocument(t *testing.T, database *store.Store, markdown string) string {
	t.Helper()
	tree, err := pmdoc.Parse(markdown)
	if err != nil {
		t.Fatalf("parse test document: %v", err)
	}
	markdown, _, err = pmdoc.Render(pmdoc.StripAnchorMarks(tree))
	if err != nil {
		t.Fatalf("render test document: %v", err)
	}
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin create document: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		insert into projects (key, name) values ('DOC', 'Documents')
		on conflict (key) do nothing
	`); err != nil {
		t.Fatalf("create test project: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		insert into issues (key, project_key, number, title, created_by, rank)
		values ('DOC-1', 'DOC', 1, 'Document', '{"kind":"user","id":"alice"}', 'U')
		on conflict (key) do nothing
	`); err != nil {
		t.Fatalf("create test issue: %v", err)
	}
	var artifactID string
	if err := tx.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ('DOC-1', 'DOC', $1, 'document.md', 'doc', false, '{"kind":"user","id":"alice"}')
		returning id::text
	`, "document-"+genRandomSuffix(t)).Scan(&artifactID); err != nil {
		t.Fatalf("create test artifact: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		values ($1, 1, $2, '[{"kind":"user","id":"alice"}]')
	`, artifactID, markdown); err != nil {
		t.Fatalf("create initial document version: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit create document: %v", err)
	}
	return artifactID
}

func createProjectDocument(t *testing.T, database *store.Store, markdown string) string {
	t.Helper()
	tree, err := pmdoc.Parse(markdown)
	if err != nil {
		t.Fatalf("parse test project document: %v", err)
	}
	markdown, _, err = pmdoc.Render(pmdoc.StripAnchorMarks(tree))
	if err != nil {
		t.Fatalf("render test project document: %v", err)
	}
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin create project document: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		insert into projects (key, name) values ('DOC', 'Documents')
		on conflict (key) do nothing
	`); err != nil {
		t.Fatalf("create test project: %v", err)
	}
	var artifactID string
	if err := tx.QueryRow(ctx, `
		insert into artifacts (project_key, slug, name, kind, is_primary, created_by)
		values ('DOC', $1, 'document.md', 'doc', false, '{"kind":"user","id":"alice"}')
		returning id::text
	`, "document-"+genRandomSuffix(t)).Scan(&artifactID); err != nil {
		t.Fatalf("create test project artifact: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		values ($1, 1, $2, '[{"kind":"user","id":"alice"}]')
	`, artifactID, markdown); err != nil {
		t.Fatalf("create initial project document version: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit create project document: %v", err)
	}
	return artifactID
}

func genRandomSuffix(t *testing.T) string {
	t.Helper()
	var value [4]byte
	if _, err := rand.Read(value[:]); err != nil {
		t.Fatalf("random suffix: %v", err)
	}
	return hex.EncodeToString(value[:])
}

func TestPgVersionedConformance(t *testing.T) {
	database := openTestStore(t)
	persistence.RunConformance(t, func() persistence.VersionedPersistence {
		return newMappedTestPersistence(t, database)
	})
}

type mappedTestPersistence struct {
	t     *testing.T
	store *PgVersioned
	db    *store.Store
	mu    sync.Mutex
	rooms map[string]string
}

func newMappedTestPersistence(t *testing.T, database *store.Store) *mappedTestPersistence {
	t.Helper()
	return &mappedTestPersistence{
		t:     t,
		store: NewPgVersioned(database),
		db:    database,
		rooms: make(map[string]string),
	}
}

func (p *mappedTestPersistence) Load(ctx context.Context, room string) (persistence.LoadResult, error) {
	return p.store.Load(ctx, p.artifactIDForRoom(room))
}

func (p *mappedTestPersistence) AppendUpdate(ctx context.Context, room string, update []byte) (persistence.Version, error) {
	return p.store.AppendUpdate(ctx, p.artifactIDForRoom(room), update)
}

func (p *mappedTestPersistence) ListVersions(ctx context.Context, room string) ([]persistence.VersionMeta, error) {
	return p.store.ListVersions(ctx, p.artifactIDForRoom(room))
}

func (p *mappedTestPersistence) GetUpdate(ctx context.Context, room string, version persistence.Version) ([]byte, persistence.VersionMeta, bool, error) {
	return p.store.GetUpdate(ctx, p.artifactIDForRoom(room), version)
}

func (p *mappedTestPersistence) MaterializeAt(ctx context.Context, room string, version persistence.Version) ([]byte, error) {
	return p.store.MaterializeAt(ctx, p.artifactIDForRoom(room), version)
}

func (p *mappedTestPersistence) CaptureSnapshot(ctx context.Context, room, name string, state []byte) (persistence.Version, error) {
	return p.store.CaptureSnapshot(ctx, p.artifactIDForRoom(room), name, state)
}

func (p *mappedTestPersistence) RestoreSnapshot(ctx context.Context, room, name string) ([]byte, persistence.Version, bool, error) {
	return p.store.RestoreSnapshot(ctx, p.artifactIDForRoom(room), name)
}

func (p *mappedTestPersistence) PruneAfter(ctx context.Context, room string, target persistence.Version, rolledBack []byte) error {
	return p.store.PruneAfter(ctx, p.artifactIDForRoom(room), target, rolledBack)
}

func (p *mappedTestPersistence) Compact(ctx context.Context, room string, keep int) (int, error) {
	return p.store.Compact(ctx, p.artifactIDForRoom(room), keep)
}

func (p *mappedTestPersistence) Delete(ctx context.Context, room string) error {
	return p.store.Delete(ctx, p.artifactIDForRoom(room))
}

func (p *mappedTestPersistence) artifactIDForRoom(room string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if id, ok := p.rooms[room]; ok {
		return id
	}
	id := createDocument(p.t, p.db, "")
	p.rooms[room] = id
	return id
}
