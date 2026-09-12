package refs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	baseURL, err := url.Parse(os.Getenv("DISPATCH_TEST_DATABASE_URL"))
	if err != nil || baseURL.String() == "" {
		t.Skip("DISPATCH_TEST_DATABASE_URL must be set to run Postgres reference tests")
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
	databaseName := "dispatch_refs_test_" + hex.EncodeToString(suffix[:])
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

func createReferenceProject(t *testing.T, tx pgx.Tx, key string) {
	t.Helper()
	if _, err := tx.Exec(context.Background(), `insert into projects (key, name) values ($1, $2)`, key, key+" project"); err != nil {
		t.Fatalf("create project %q: %v", key, err)
	}
}

func createReferenceIssue(t *testing.T, tx pgx.Tx, key, project string) {
	t.Helper()
	if _, err := tx.Exec(context.Background(), `
		insert into issues (key, project_key, number, title, created_by, rank)
		values ($1, $2, 1, 'Reference issue', '{"kind":"user","id":"alice"}', 'U')
	`, key, project); err != nil {
		t.Fatalf("create issue %q: %v", key, err)
	}
}

func createReferenceArtifact(t *testing.T, tx pgx.Tx, issueKey, project, slug string) (string, string) {
	t.Helper()
	var id, refKey string
	var err error
	if issueKey == "" {
		err = tx.QueryRow(context.Background(), `
			insert into artifacts (project_key, slug, name, kind, created_by)
			values ($1, $2, $3, 'doc', '{"kind":"user","id":"alice"}')
			returning id::text, ref_key
		`, project, slug, slug).Scan(&id, &refKey)
	} else {
		err = tx.QueryRow(context.Background(), `
			insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
			values ($1, $2, $3, $4, 'doc', true, '{"kind":"user","id":"alice"}')
			returning id::text, ref_key
		`, issueKey, project, slug, slug).Scan(&id, &refKey)
	}
	if err != nil {
		t.Fatalf("create artifact %q: %v", slug, err)
	}
	return id, refKey
}

func createReferenceComment(t *testing.T, tx pgx.Tx, issueKey string) string {
	t.Helper()
	var id string
	if err := tx.QueryRow(context.Background(), `
		insert into comments (issue_key, author, body)
		values ($1, '{"kind":"user","id":"alice"}', 'reference')
		returning id::text
	`, issueKey).Scan(&id); err != nil {
		t.Fatalf("create reference comment: %v", err)
	}
	return id
}

func insertReference(t *testing.T, tx pgx.Tx, fromKind, fromID, toKind, toID string) {
	t.Helper()
	if _, err := tx.Exec(context.Background(), `
		insert into refs (from_kind, from_id, to_kind, to_id) values ($1, $2, $3, $4)
	`, fromKind, fromID, toKind, toID); err != nil {
		t.Fatalf("insert reference %s/%s -> %s/%s: %v", fromKind, fromID, toKind, toID, err)
	}
}

func TestReplaceWritesRefKeysAndSkipsExternalURLs(t *testing.T) {
	database := openTestStore(t)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin replace transaction: %v", err)
	}
	defer tx.Rollback(ctx)

	body := "see dispatch://CORE/artifact/design-notes and dispatch://OPS-2/artifact/spec and https://example.com"
	if err := Replace(ctx, tx, "comment", "c1", body, "https://dispatch.example"); err != nil {
		t.Fatalf("replace references: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit references: %v", err)
	}

	rows, err := database.Pool.Query(ctx, `
		select to_kind, to_id from refs where from_kind = 'comment' and from_id = 'c1' order by to_id
	`)
	if err != nil {
		t.Fatalf("read replacement references: %v", err)
	}
	defer rows.Close()
	var got []struct {
		Kind string
		ID   string
	}
	for rows.Next() {
		var row struct {
			Kind string
			ID   string
		}
		if err := rows.Scan(&row.Kind, &row.ID); err != nil {
			t.Fatalf("scan replacement reference: %v", err)
		}
		got = append(got, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate replacement references: %v", err)
	}
	want := []struct {
		Kind string
		ID   string
	}{
		{Kind: "artifact", ID: "CORE/design-notes"},
		{Kind: "artifact", ID: "OPS-2/spec"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stored references = %#v; want %#v", got, want)
	}

	tx, err = database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin clear transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	if err := Replace(ctx, tx, "comment", "c1", "", "https://dispatch.example"); err != nil {
		t.Fatalf("clear references: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit cleared references: %v", err)
	}
	var count int
	if err := database.Pool.QueryRow(ctx, `select count(*) from refs where from_kind = 'comment' and from_id = 'c1'`).Scan(&count); err != nil {
		t.Fatalf("count cleared references: %v", err)
	}
	if count != 0 {
		t.Fatalf("references after clear = %d; want 0", count)
	}
}

func TestClosureFollowsArtifactLinksToDepthEightAndReportsTruncation(t *testing.T) {
	database := openTestStore(t)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin fixture transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	createReferenceProject(t, tx, "CORE")
	createReferenceProject(t, tx, "OPS")
	createReferenceIssue(t, tx, "CORE-1", "CORE")
	_, ownRefKey := createReferenceArtifact(t, tx, "CORE-1", "CORE", "spec")

	artifactIDs := make(map[string]string)
	artifactRefs := make(map[string]string)
	for index := 1; index <= 9; index++ {
		slug := "d" + strconv.Itoa(index)
		artifactIDs[slug], artifactRefs[slug] = createReferenceArtifact(t, tx, "", "CORE", slug)
	}
	_, diagramRefKey := createReferenceArtifact(t, tx, "", "OPS", "diagram-png")
	commentID := createReferenceComment(t, tx, "CORE-1")
	insertReference(t, tx, "comment", commentID, "artifact", artifactRefs["d1"])
	insertReference(t, tx, "comment", commentID, "artifact", diagramRefKey)
	for index := 1; index < 9; index++ {
		from := "d" + strconv.Itoa(index)
		to := "d" + strconv.Itoa(index+1)
		insertReference(t, tx, "artifact", artifactIDs[from], "artifact", artifactRefs[to])
	}
	insertReference(t, tx, "artifact", artifactIDs["d1"], "artifact", ownRefKey)
	insertReference(t, tx, "artifact", artifactIDs["d1"], "artifact", "CORE/ghost")
	insertReference(t, tx, "artifact", artifactIDs["d3"], "artifact", artifactRefs["d1"])
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit fixture: %v", err)
	}

	got, truncated, err := Closure(ctx, database.Pool, "CORE-1")
	if err != nil {
		t.Fatalf("read closure: %v", err)
	}
	want := []Member{
		{RefKey: artifactRefs["d1"], Depth: 1, Via: model.ReferenceVia{Kind: "comment", ID: commentID}},
		{RefKey: diagramRefKey, Depth: 1, Via: model.ReferenceVia{Kind: "comment", ID: commentID}},
		{RefKey: artifactRefs["d2"], Depth: 2, Via: model.ReferenceVia{Kind: "artifact", ID: artifactIDs["d1"]}},
		{RefKey: artifactRefs["d3"], Depth: 3, Via: model.ReferenceVia{Kind: "artifact", ID: artifactIDs["d2"]}},
		{RefKey: artifactRefs["d4"], Depth: 4, Via: model.ReferenceVia{Kind: "artifact", ID: artifactIDs["d3"]}},
		{RefKey: artifactRefs["d5"], Depth: 5, Via: model.ReferenceVia{Kind: "artifact", ID: artifactIDs["d4"]}},
		{RefKey: artifactRefs["d6"], Depth: 6, Via: model.ReferenceVia{Kind: "artifact", ID: artifactIDs["d5"]}},
		{RefKey: artifactRefs["d7"], Depth: 7, Via: model.ReferenceVia{Kind: "artifact", ID: artifactIDs["d6"]}},
		{RefKey: artifactRefs["d8"], Depth: 8, Via: model.ReferenceVia{Kind: "artifact", ID: artifactIDs["d7"]}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("closure = %#v; want %#v", got, want)
	}
	if !truncated {
		t.Fatal("closure did not report the ninth hop as truncated")
	}
}

func TestReferencedByIncludesArtifactSources(t *testing.T) {
	database := openTestStore(t)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin fixture transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	createReferenceProject(t, tx, "CORE")
	createReferenceIssue(t, tx, "CORE-1", "CORE")
	d1ID, d1RefKey := createReferenceArtifact(t, tx, "", "CORE", "d1")
	_, d2RefKey := createReferenceArtifact(t, tx, "", "CORE", "d2")
	commentID := createReferenceComment(t, tx, "CORE-1")
	insertReference(t, tx, "artifact", d1ID, "artifact", d2RefKey)
	insertReference(t, tx, "comment", commentID, "artifact", d2RefKey)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit fixture: %v", err)
	}

	got, err := ReferencedBy(ctx, database.Pool, d2RefKey)
	if err != nil {
		t.Fatalf("read artifact sources: %v", err)
	}
	issueKey := "CORE-1"
	want := []model.ReferencedBy{
		{Kind: "artifact", ID: d1ID, Project: "CORE", Excerpt: "d1", RefKey: d1RefKey},
		{Kind: "comment", ID: commentID, IssueKey: &issueKey, Project: "CORE", Excerpt: "reference"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("referenced by = %#v; want %#v", got, want)
	}
}
