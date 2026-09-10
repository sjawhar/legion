package docs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	ygws "github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

func seedLegacyDocument(t *testing.T, database *store.Store, artifactID, markdown string) {
	t.Helper()
	doc := crdt.New()
	content := doc.GetText("content")
	doc.Transact(func(txn *crdt.Transaction) {
		content.Insert(txn, 0, markdown, nil)
	})
	tx, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin legacy document: %v", err)
	}
	defer tx.Rollback(context.Background())
	if _, err := NewPgVersioned(database).AppendUpdateTx(context.Background(), tx, artifactID, crdt.EncodeStateAsUpdateV1(doc, nil)); err != nil {
		t.Fatalf("append legacy document: %v", err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("commit legacy document: %v", err)
	}
}

func insertLegacyAnchor(t *testing.T, database *store.Store, table, artifactID, quote string, from, to int, orphaned bool) string {
	t.Helper()
	anchor, err := json.Marshal(map[string]any{
		"artifact_id": artifactID,
		"version":     1,
		"quote":       quote,
		"from":        from,
		"to":          to,
		"orphaned":    orphaned,
	})
	if err != nil {
		t.Fatalf("encode legacy anchor: %v", err)
	}
	var id string
	if err := database.Pool.QueryRow(context.Background(), `select gen_random_uuid()::text`).Scan(&id); err != nil {
		t.Fatalf("allocate legacy row id: %v", err)
	}
	switch table {
	case "asks":
		_, err = database.Pool.Exec(context.Background(), `
			insert into asks (id, issue_key, author, question, anchor)
			values ($1, 'DOC-1', '{"kind":"user","id":"alice"}', 'Legacy ask', $2)
		`, id, anchor)
	case "comments":
		_, err = database.Pool.Exec(context.Background(), `
			insert into comments (id, issue_key, author, body, anchor)
			values ($1, 'DOC-1', '{"kind":"user","id":"alice"}', 'Legacy comment', $2)
		`, id, anchor)
	default:
		t.Fatalf("unsupported legacy anchor table %q", table)
	}
	if err != nil {
		t.Fatalf("insert legacy %s anchor: %v", table, err)
	}
	return id
}

func insertLegacyReply(t *testing.T, database *store.Store, rootID string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		insert into comments (issue_key, author, body, reply_to)
		values ('DOC-1', '{"kind":"session","id":"reply"}', 'Legacy reply', $1)
	`, rootID); err != nil {
		t.Fatalf("insert legacy reply: %v", err)
	}
}

func TestMigrateLegacyDocumentsConvertsContentAndAnchors(t *testing.T) {
	database := openTestStore(t)
	artifactID := createDocument(t, database, "")
	legacy := "## Database\nUse SQLite, brown fox, brown dog [shown](https://example.com)"
	seedLegacyDocument(t, database, artifactID, legacy)
	askID := insertLegacyAnchor(t, database, "asks", artifactID, "brown", 35, 40, false)
	urlFrom := strings.Index(legacy, "https://example.com")
	rawURLID := insertLegacyAnchor(t, database, "asks", artifactID, "https://example.com", urlFrom, urlFrom+len("https://example.com"), false)
	commentID := insertLegacyAnchor(t, database, "comments", artifactID, "gone", 0, 4, false)
	rootCommentID := insertLegacyAnchor(t, database, "comments", artifactID, "SQLite", 12, 18, false)
	insertLegacyReply(t, database, rootCommentID)

	if err := MigrateLegacyDocuments(context.Background(), database); err != nil {
		t.Fatalf("migrate legacy documents: %v", err)
	}
	service := New(Deps{Store: database, Events: events.NewBroker(), Settle: time.Hour})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	if text, err := service.Text(context.Background(), artifactID); err != nil || text != "## Database\n\nUse SQLite, brown fox, brown dog [shown](https://example.com)\n" {
		t.Fatalf("Text = %q %v", text, err)
	}
	tree := liveTree(t, service, artifactID)
	occurrence := 1
	want, err := pmdoc.FindQuote(tree, "brown", &occurrence, nil)
	if err != nil {
		t.Fatalf("find second brown: %v", err)
	}
	if got, quote, ok := pmdoc.FindMark(tree, string(MarkAsk), askID); !ok || quote != "brown" || got != want {
		t.Fatalf("ask mark = %v %q %v, want %v brown true", got, quote, ok, want)
	}
	if ask := loadAskAnchor(t, service, askID); ask.MarkID != askID || ask.Orphaned || ask.Version != 1 {
		t.Fatalf("ask anchor = %#v, want live mark-backed anchor", ask)
	}
	if anchor := loadAskAnchor(t, service, rawURLID); anchor.MarkID != rawURLID || !anchor.Orphaned {
		t.Fatalf("link-destination anchor = %#v, want orphaned mark-backed anchor", anchor)
	}
	if comment := loadCommentAnchor(t, service, commentID); comment.MarkID != commentID || !comment.Orphaned {
		t.Fatalf("comment anchor = %#v, want orphaned mark-backed anchor", comment)
	}
	if projection := legacyMarkProjection(t, service, artifactID, rootCommentID); projection["kind"] != "comment" || projection["text"] != "Legacy comment" || len(projection["replies"].([]any)) != 1 {
		t.Fatalf("root comment projection = %#v, want comment with one reply", projection)
	}
	var raw []byte
	if err := database.Pool.QueryRow(context.Background(), `select anchor from asks where id = $1`, askID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"from"`) || strings.Contains(string(raw), `"to"`) {
		t.Fatalf("legacy offsets survived: %s", raw)
	}
	var updates int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from doc_updates where artifact_id = $1`, artifactID).Scan(&updates); err != nil {
		t.Fatal(err)
	}
	if updates != 1 {
		t.Fatalf("doc_updates = %d, want one new update", updates)
	}
	var applied bool
	if err := database.Pool.QueryRow(context.Background(), `select exists(select 1 from schema_migrations where version = $1)`, legacyMigrationVersion).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if !applied {
		t.Fatal("legacy migration marker is missing")
	}
	if err := MigrateLegacyDocuments(context.Background(), database); err != nil {
		t.Fatalf("second migration: %v", err)
	}
}

func TestMigrateLegacyDocumentsFailsLoudlyOnUnparseableMarkdown(t *testing.T) {
	database := openTestStore(t)
	good := createDocument(t, database, "")
	seedLegacyDocument(t, database, good, "fine")
	bad := createDocument(t, database, "")
	seedLegacyDocument(t, database, bad, "<details>\nraw block HTML\n</details>\n")

	err := MigrateLegacyDocuments(context.Background(), database)
	if err == nil || !strings.Contains(err.Error(), bad) || !errors.Is(err, pmdoc.ErrSchema) {
		t.Fatalf("migration error = %v, want schema error naming %s", err, bad)
	}
	var applied bool
	if err := database.Pool.QueryRow(context.Background(), `select exists(select 1 from schema_migrations where version = $1)`, legacyMigrationVersion).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if applied {
		t.Fatal("legacy migration marker recorded after failed migration")
	}
	var updates int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from doc_updates where artifact_id = $1`, bad).Scan(&updates); err != nil {
		t.Fatal(err)
	}
	if updates != 1 {
		t.Fatalf("failing legacy document updates = %d, want original single update", updates)
	}
	reports, err := InspectLegacyDocuments(context.Background(), database)
	if err != nil {
		t.Fatalf("inspect documents: %v", err)
	}
	if report := legacyReport(t, reports, good); report.State != "tree" || report.ParseError != nil {
		t.Fatalf("converted document report = %#v, want tree with no parse error", report)
	}
	if report := legacyReport(t, reports, bad); report.State != "legacy" || report.ParseError == nil || !strings.Contains(report.ParseError.Error(), "block HTML") {
		t.Fatalf("failing document report = %#v, want legacy block-HTML parse error", report)
	}
}

func TestInspectLegacyDocumentsReportsLegacyParseAndAnchorResolutionWithoutWriting(t *testing.T) {
	database := openTestStore(t)
	artifactID := createDocument(t, database, "")
	seedLegacyDocument(t, database, artifactID, "visible target")
	insertLegacyAnchor(t, database, "asks", artifactID, "target", 8, 14, false)

	reports, err := InspectLegacyDocuments(context.Background(), database)
	if err != nil {
		t.Fatalf("inspect documents: %v", err)
	}
	report := legacyReport(t, reports, artifactID)
	if report.State != "legacy" || report.ParseError != nil || report.Anchors != 1 || report.Resolvable != 1 {
		t.Fatalf("legacy document report = %#v, want parsed one-anchor resolvable legacy document", report)
	}
	var updates int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from doc_updates where artifact_id = $1`, artifactID).Scan(&updates); err != nil {
		t.Fatal(err)
	}
	if updates != 1 {
		t.Fatalf("preflight wrote document updates = %d, want original single update", updates)
	}
}

func TestUnmigratedLegacyRoomRefusesToLoad(t *testing.T) {
	database := openTestStore(t)
	artifactID := createDocument(t, database, "")
	seedLegacyDocument(t, database, artifactID, "legacy")
	loaded, err := NewPgVersioned(database).Load(context.Background(), artifactID)
	if err != nil {
		t.Fatal(err)
	}
	doc := crdt.New()
	if err := crdt.ApplyUpdateV1(doc, loaded.Update, nil); err != nil {
		t.Fatal(err)
	}
	service := New(Deps{Store: database, Events: events.NewBroker(), Settle: time.Hour})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	if err := service.onLoadDocument(context.Background(), artifactID, doc); !errors.Is(err, ErrDocSchema) {
		t.Fatalf("legacy load error = %v, want ErrDocSchema", err)
	}
}

func legacyMarkProjection(t *testing.T, service *Service, artifactID, markID string) map[string]any {
	t.Helper()
	var projection map[string]any
	err := service.srv.Apply(context.Background(), artifactID, func(doc *crdt.Doc, _ func(func(*crdt.Transaction))) {
		value, _ := doc.GetMap(marksMapName).Get(markID)
		projection, _ = value.(map[string]any)
	})
	if err != nil && !errors.Is(err, ygws.ErrNoChanges) {
		t.Fatalf("read legacy mark projection: %v", err)
	}
	if projection == nil {
		t.Fatalf("mark projection %q is missing", markID)
	}
	return projection
}

func legacyReport(t *testing.T, reports []LegacyReport, artifactID string) LegacyReport {
	t.Helper()
	for _, report := range reports {
		if report.ArtifactID == artifactID {
			return report
		}
	}
	t.Fatalf("artifact %s is missing from reports: %s", artifactID, fmt.Sprint(reports))
	return LegacyReport{}
}
