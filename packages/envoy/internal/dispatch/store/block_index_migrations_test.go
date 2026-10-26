package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// beforeBlockGraph is the last migration before the block index: 0118_block_graph through
// 0123_block_grammar_seed apply after it. lastBlockGrammarTrigger is the last of the triggers
// (0120-0122) that 0123's seed runs after.
const (
	beforeBlockGraph        = 117
	lastBlockGrammarTrigger = 122
)

// owedItem is one row of block_grammar_owed: an item owed an index with the block grammar.
type owedItem struct{ kind, id string }

func readOwedItems(t *testing.T, store *Store) map[owedItem]time.Time {
	t.Helper()
	rows, err := store.Pool.Query(context.Background(), `select kind, id::text, enqueued_at from block_grammar_owed`)
	if err != nil {
		t.Fatalf("read block_grammar_owed: %v", err)
	}
	defer rows.Close()
	owed := map[owedItem]time.Time{}
	for rows.Next() {
		var item owedItem
		var at time.Time
		if err := rows.Scan(&item.kind, &item.id, &at); err != nil {
			t.Fatalf("scan block_grammar_owed: %v", err)
		}
		owed[item] = at
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate block_grammar_owed: %v", err)
	}
	return owed
}

func assertOwedItems(t *testing.T, store *Store, want ...owedItem) map[owedItem]time.Time {
	t.Helper()
	owed := readOwedItems(t, store)
	got := map[owedItem]bool{}
	for item := range owed {
		got[item] = true
	}
	wanted := map[owedItem]bool{}
	for _, item := range want {
		wanted[item] = true
	}
	if !reflect.DeepEqual(got, wanted) {
		t.Fatalf("owed items = %v, want %v", got, wanted)
	}
	return owed
}

// documentBlockVersion reads a document's row of document_block_versions: found, and its indexed
// version, nil when owed.
func documentBlockVersion(t *testing.T, store *Store, artifactID string) (*int, bool) {
	t.Helper()
	var version *int
	err := store.Pool.QueryRow(context.Background(),
		`select version from document_block_versions where artifact_id = $1`, artifactID).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false
	}
	if err != nil {
		t.Fatalf("read document_block_versions(%s): %v", artifactID, err)
	}
	return version, true
}

func seedBlockIndexIssue(t *testing.T, store *Store) {
	t.Helper()
	if _, err := store.Pool.Exec(context.Background(), `
		insert into projects (key, name) values ('CORE', 'Core');
		insert into issues (key, project_key, number, title, created_by, rank)
			values ('CORE-1', 'CORE', 1, 'One', '{"kind":"user","id":"alice"}', 'U');
	`); err != nil {
		t.Fatalf("seed issue: %v", err)
	}
}

// What a build that predates the block index writes during the roll reaches the index: every
// document version it inserts marks its document owed again, and each comment, ask and issue
// message whose text holds `#b-` is owed, on insert and on an edit of its text. A text without
// `#b-`, a direct message, an image's version and an edit of a column other than the text owe
// nothing. A block-aware build's own index, in the version's transaction, leaves the document
// indexed, and the owed reads are served by their indexes.
func TestTheBlockIndexTriggersOweWhatAnyBuildWrites(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	seedBlockIndexIssue(t, store)
	doc := insertReturningID(t, store, `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ('CORE-1', 'CORE', 'spec', 'spec.md', 'doc', true, '{"kind":"user","id":"alice"}')
		returning id::text`)
	image := insertReturningID(t, store, `
		insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
		values ('CORE-1', 'CORE', 'photo-png', 'photo.png', 'image', '{"kind":"user","id":"alice"}')
		returning id::text`)

	insertVersion := func(number int) {
		t.Helper()
		if _, err := store.Pool.Exec(ctx, `
			insert into artifact_versions (artifact_id, number, markdown) values ($1, $2, '# Spec')
		`, doc, number); err != nil {
			t.Fatalf("insert version %d: %v", number, err)
		}
	}
	insertVersion(1)
	if version, ok := documentBlockVersion(t, store, doc); !ok || version != nil {
		t.Fatalf("after version 1: row %v (found %v), want the document owed", version, ok)
	}
	if _, err := store.Pool.Exec(ctx, `update document_block_versions set version = 1 where artifact_id = $1`, doc); err != nil {
		t.Fatalf("index version 1: %v", err)
	}
	insertVersion(2)
	if version, ok := documentBlockVersion(t, store, doc); !ok || version != nil {
		t.Fatalf("after an older build's version 2 over indexed version 1: row %v (found %v), want the document owed again", version, ok)
	}

	// A block-aware build inserts the version and then indexes it in one transaction.
	tx, err := store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `insert into artifact_versions (artifact_id, number, markdown) values ($1, 3, '# Spec')`, doc); err != nil {
		t.Fatalf("insert version 3: %v", err)
	}
	if _, err := tx.Exec(ctx, `update document_block_versions set version = 3 where artifact_id = $1`, doc); err != nil {
		t.Fatalf("index version 3: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit version 3: %v", err)
	}
	if version, ok := documentBlockVersion(t, store, doc); !ok || version == nil || *version != 3 {
		t.Fatalf("after a block-aware build's version 3: row %v (found %v), want indexed at 3", version, ok)
	}

	if _, err := store.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, content, mime) values ($1, 1, '\x89504e47', 'image/png')
	`, image); err != nil {
		t.Fatalf("insert image version: %v", err)
	}
	if version, ok := documentBlockVersion(t, store, image); ok {
		t.Fatalf("an image version wrote row %v, want none", version)
	}

	blockComment := insertReturningID(t, store, `
		insert into comments (issue_key, author, body) values ('CORE-1', '{"kind":"user","id":"alice"}', 'See dispatch://CORE-1/spec#b-ship-it')
		returning id::text`)
	plainComment := insertReturningID(t, store, `
		insert into comments (issue_key, author, body) values ('CORE-1', '{"kind":"user","id":"alice"}', 'See dispatch://CORE-1/spec')
		returning id::text`)
	blockAsk := insertReturningID(t, store, `
		insert into asks (issue_key, author, question) values ('CORE-1', '{"kind":"session","id":"s1"}', 'Keep dispatch://CORE-1/spec#b-ship-it?')
		returning id::text`)
	insertReturningID(t, store, `
		insert into asks (issue_key, author, question) values ('CORE-1', '{"kind":"session","id":"s1"}', 'Ship it?')
		returning id::text`)
	blockMessage := insertReturningID(t, store, `
		insert into messages (issue_key, author, body) values ('CORE-1', '{"kind":"session","id":"s1"}', 'Read dispatch://CORE-1/spec#b-ship-it')
		returning id::text`)
	insertReturningID(t, store, `
		insert into messages (issue_key, author, body) values ('CORE-1', '{"kind":"session","id":"s1"}', 'Read the spec')
		returning id::text`)
	insertReturningID(t, store, `
		insert into messages (author, body, target) values ('{"kind":"user","id":"alice"}', 'Read dispatch://CORE-1/spec#b-ship-it', 'session:s1')
		returning id::text`)
	comment := owedItem{"comment", blockComment}
	assertOwedItems(t, store, comment, owedItem{"ask", blockAsk}, owedItem{"message", blockMessage})

	// An edit that writes `#b-` into a text owes it; an edit of another column owes nothing.
	if _, err := store.Pool.Exec(ctx, `update comments set body = body || '#b-ship-it' where id = $1`, plainComment); err != nil {
		t.Fatalf("edit comment: %v", err)
	}
	assertOwedItems(t, store, comment, owedItem{"comment", plainComment}, owedItem{"ask", blockAsk}, owedItem{"message", blockMessage})
	if _, err := store.Pool.Exec(ctx, `delete from block_grammar_owed where kind = 'comment' and id = $1`, blockComment); err != nil {
		t.Fatalf("drain comment: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `update comments set resolved = true where id = $1`, blockComment); err != nil {
		t.Fatalf("resolve comment: %v", err)
	}
	if _, ok := readOwedItems(t, store)[comment]; ok {
		t.Fatalf("resolving a comment owed it again, want only a text edit to")
	}

	// An edit of a text already owed moves its enqueued_at, so a drain that listed the earlier
	// time leaves it owed.
	past := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := store.Pool.Exec(ctx, `update block_grammar_owed set enqueued_at = $2 where kind = 'ask' and id = $1`, blockAsk, past); err != nil {
		t.Fatalf("age the owed ask: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `update asks set question = question || ' Now.' where id = $1`, blockAsk); err != nil {
		t.Fatalf("edit ask: %v", err)
	}
	if at := readOwedItems(t, store)[owedItem{"ask", blockAsk}]; !at.After(past) {
		t.Fatalf("an edited owed ask kept enqueued_at %v, want it moved past %v", at, past)
	}

	for name, read := range map[string]struct{ query, index string }{
		"owed documents": {`select artifact_id from document_block_versions where version is null and artifact_id > '00000000-0000-0000-0000-000000000000' order by artifact_id limit 20`, "document_block_versions_owed"},
		"owed items":     {`select kind, id, enqueued_at from block_grammar_owed order by enqueued_at limit 200`, "block_grammar_owed_enqueued"},
	} {
		if plan := explainWithoutSeqScan(t, store, read.query); !strings.Contains(plan, read.index) {
			t.Errorf("%s: plan does not read %s:\n%s", name, read.index, plan)
		}
	}
}

// Text stored before the block index is owed by the migrations themselves: 0118 owes every
// document, and 0123 owes each comment, ask and issue message whose text holds `#b-`, and nothing
// else. A row a trigger already owed before the seed ran keeps its enqueued_at.
func TestTheBlockGrammarSeedOwesOlderText(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	migrateThrough(t, store, beforeBlockGraph)
	seedBlockIndexIssue(t, store)
	doc := insertReturningID(t, store, `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ('CORE-1', 'CORE', 'spec', 'spec.md', 'doc', true, '{"kind":"user","id":"alice"}')
		returning id::text`)
	image := insertReturningID(t, store, `
		insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
		values ('CORE-1', 'CORE', 'photo-png', 'photo.png', 'image', '{"kind":"user","id":"alice"}')
		returning id::text`)
	blockComment := insertReturningID(t, store, `
		insert into comments (issue_key, author, body) values ('CORE-1', '{"kind":"user","id":"alice"}', 'See dispatch://CORE-1/spec#b-ship-it')
		returning id::text`)
	insertReturningID(t, store, `
		insert into comments (issue_key, author, body) values ('CORE-1', '{"kind":"user","id":"alice"}', 'See dispatch://CORE-1/spec')
		returning id::text`)
	blockAsk := insertReturningID(t, store, `
		insert into asks (issue_key, author, question) values ('CORE-1', '{"kind":"session","id":"s1"}', 'Keep dispatch://CORE-1/spec#b-ship-it?')
		returning id::text`)
	insertReturningID(t, store, `
		insert into asks (issue_key, author, question) values ('CORE-1', '{"kind":"session","id":"s1"}', 'Ship it?')
		returning id::text`)
	blockMessage := insertReturningID(t, store, `
		insert into messages (issue_key, author, body) values ('CORE-1', '{"kind":"session","id":"s1"}', 'Read dispatch://CORE-1/spec#b-ship-it')
		returning id::text`)
	insertReturningID(t, store, `
		insert into messages (issue_key, author, body) values ('CORE-1', '{"kind":"session","id":"s1"}', 'Read the spec')
		returning id::text`)
	insertReturningID(t, store, `
		insert into messages (author, body, target) values ('{"kind":"user","id":"alice"}', 'Read dispatch://CORE-1/spec#b-ship-it', 'session:s1')
		returning id::text`)

	// A comment written once the triggers exist, before the seed runs, is owed by its trigger.
	migrateThrough(t, store, lastBlockGrammarTrigger)
	raced := insertReturningID(t, store, `
		insert into comments (issue_key, author, body) values ('CORE-1', '{"kind":"session","id":"s1"}', 'Also dispatch://CORE-1/spec#b-ship-it')
		returning id::text`)
	past := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := store.Pool.Exec(ctx, `update block_grammar_owed set enqueued_at = $2 where kind = 'comment' and id = $1`, raced, past); err != nil {
		t.Fatalf("age the raced comment: %v", err)
	}

	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	owed := assertOwedItems(t, store,
		owedItem{"comment", blockComment}, owedItem{"comment", raced},
		owedItem{"ask", blockAsk}, owedItem{"message", blockMessage})
	if at := owed[owedItem{"comment", raced}]; !at.Equal(past) {
		t.Fatalf("the seed moved the raced comment's enqueued_at to %v, want %v kept", at, past)
	}
	if version, ok := documentBlockVersion(t, store, doc); !ok || version != nil {
		t.Fatalf("an older document: row %v (found %v), want it owed", version, ok)
	}
	if version, ok := documentBlockVersion(t, store, image); ok {
		t.Fatalf("an image has row %v, want none", version)
	}
}
