package store

import (
	"context"
	"reflect"
	"sort"
	"testing"
	"time"
)

// beforeRefsOwed is the last migration before the re-index queue: the six that make, feed and seed
// it (0120_refs_owed through 0125_refs_owed_seed) apply after it.
const beforeRefsOwed = 119

// readOwed is every refs_owed row as "kind id", sorted.
func readOwed(t *testing.T, store *Store) []string {
	t.Helper()
	rows, err := store.Pool.Query(context.Background(), `select kind || ' ' || id from refs_owed`)
	if err != nil {
		t.Fatalf("read refs_owed: %v", err)
	}
	defer rows.Close()
	owed := []string{}
	for rows.Next() {
		var row string
		if err := rows.Scan(&row); err != nil {
			t.Fatalf("scan refs_owed: %v", err)
		}
		owed = append(owed, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate refs_owed: %v", err)
	}
	sort.Strings(owed)
	return owed
}

func sortedOwed(rows ...string) []string {
	sort.Strings(rows)
	return rows
}

func execAll(t *testing.T, store *Store, statements ...string) {
	t.Helper()
	for _, statement := range statements {
		if _, err := store.Pool.Exec(context.Background(), statement); err != nil {
			t.Fatalf("%v\n%s", err, statement)
		}
	}
}

// refsOwedFixture is one of every reference source, written by raw SQL as a server of the release
// before the queue writes them: a document with a markdown version, a file whose only version has
// none, a question block's ask on the document, an issue ask, a comment, an issue message and a
// direct message.
type refsOwedFixture struct {
	document, file, blockAsk, ask, comment, message, direct string
}

func writeRefsOwedFixture(t *testing.T, store *Store) refsOwedFixture {
	t.Helper()
	execAll(t, store, `
		insert into projects (key, name) values ('ACME', 'Acme') on conflict (key) do nothing`, `
		insert into issues (key, project_key, number, title, created_by, rank)
		values ('ACME-1', 'ACME', 1, 'One', '{"kind":"user","id":"alice"}', 'U') on conflict (key) do nothing`)
	var fixture refsOwedFixture
	fixture.document = insertReturningID(t, store, `
		insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
		values ('ACME-1', 'ACME', 'plan-' || gen_random_uuid(), 'plan.md', 'doc', '{"kind":"user","id":"alice"}')
		returning id::text`)
	execAll(t, store, `insert into artifact_versions (artifact_id, number, markdown) values ('`+fixture.document+`', 1, 'Blocked on ACME-1, see #q1.')`)
	fixture.file = insertReturningID(t, store, `
		insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
		values ('ACME-1', 'ACME', 'photo-' || gen_random_uuid(), 'photo.png', 'image', '{"kind":"user","id":"alice"}')
		returning id::text`)
	execAll(t, store, `insert into artifact_versions (artifact_id, number) values ('`+fixture.file+`', 1)`)
	fixture.blockAsk = insertReturningID(t, store, `
		insert into asks (issue_key, author, question, block_id, block_artifact_id)
		values ('ACME-1', '{"kind":"user","id":"alice"}', 'Ship it?', 'q1', $1)
		returning id::text`, fixture.document)
	fixture.ask = insertReturningID(t, store, `
		insert into asks (issue_key, author, question) values ('ACME-1', '{"kind":"user","id":"alice"}', 'After ACME-1?')
		returning id::text`)
	fixture.comment = insertReturningID(t, store, `
		insert into comments (issue_key, author, body) values ('ACME-1', '{"kind":"user","id":"alice"}', 'Blocked on ACME-1.')
		returning id::text`)
	fixture.message = insertReturningID(t, store, `
		insert into messages (issue_key, author, body) values ('ACME-1', '{"kind":"user","id":"alice"}', 'See #q1.')
		returning id::text`)
	fixture.direct = insertReturningID(t, store, `
		insert into messages (author, body, target) values ('{"kind":"user","id":"alice"}', 'ACME-1?', 'session:s1')
		returning id::text`)
	return fixture
}

// Every source stored before the queue exists is owed a re-index once it does: each document with
// a markdown version, each ask, each comment and each issue message. A file and a direct message,
// which nothing indexes, are not.
func TestRefsOwedSeedOwesEveryStoredSource(t *testing.T) {
	store := openEmptyTestStore(t)
	migrateThrough(t, store, beforeRefsOwed)
	fixture := writeRefsOwedFixture(t, store)

	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	want := sortedOwed(
		"artifact "+fixture.document,
		"ask "+fixture.blockAsk,
		"ask "+fixture.ask,
		"comment "+fixture.comment,
		"message "+fixture.message,
	)
	if got := readOwed(t, store); !reflect.DeepEqual(got, want) {
		t.Fatalf("owed after the migration = %v; want %v", got, want)
	}
}

// What a server of the release before the queue writes reaches the database as the row alone, and
// each such write is owed: a comment, an ask, an issue message and a document version, and a
// question block's ask also owes its document as a scope. An edit of what the index reads is owed
// again, and re-dates a row already owed; an update the index does not read - a resolution, an
// answer, an anchor refreshed within its document - owes nothing, and neither does a file version or
// a direct message.
func TestRefsOwedTriggersOweWhatAnEarlierReleaseWrites(t *testing.T) {
	store := openEmptyTestStore(t)
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	fixture := writeRefsOwedFixture(t, store)

	want := sortedOwed(
		"artifact "+fixture.document,
		"scope "+fixture.document,
		"ask "+fixture.blockAsk,
		"ask "+fixture.ask,
		"comment "+fixture.comment,
		"message "+fixture.message,
	)
	if got := readOwed(t, store); !reflect.DeepEqual(got, want) {
		t.Fatalf("owed after the writes = %v; want %v", got, want)
	}

	execAll(t, store, `delete from refs_owed`)
	execAll(t, store,
		`update comments set resolved = true where id = '`+fixture.comment+`'`,
		`update comments set anchor = jsonb_build_object('artifact_id', '`+fixture.document+`', 'mark_id', 'm1') where id = '`+fixture.comment+`'`,
		`update asks set state = 'answered', answer = jsonb_build_object('user', 'alice', 'selected', '[]'::jsonb, 'at', now()) where id = '`+fixture.ask+`'`,
		`update asks set question = question where id = '`+fixture.ask+`'`,
	)
	if got := readOwed(t, store); !reflect.DeepEqual(got, []string{"comment " + fixture.comment}) {
		t.Fatalf("owed after a resolution, an anchor naming a document, an answer and an unchanged question = %v; want the comment, whose anchor moved it into the document's scope", got)
	}
	execAll(t, store,
		`delete from refs_owed`,
		`update comments set anchor = jsonb_build_object('artifact_id', '`+fixture.document+`', 'mark_id', 'm2') where id = '`+fixture.comment+`'`,
	)
	if got := readOwed(t, store); len(got) != 0 {
		t.Fatalf("owed after an anchor refreshed within its document = %v; want nothing", got)
	}

	execAll(t, store, `update refs_owed set enqueued_at = '2020-01-01T00:00:00Z'`)
	execAll(t, store,
		`update comments set body = 'Blocked on ACME-1 still.' where id = '`+fixture.comment+`'`,
		`update asks set question = 'Ship it now?' where id = '`+fixture.ask+`'`,
	)
	if got := readOwed(t, store); !reflect.DeepEqual(got, sortedOwed("ask "+fixture.ask, "comment "+fixture.comment)) {
		t.Fatalf("owed after a comment's and an ask's edit = %v", got)
	}
	execAll(t, store, `update refs_owed set enqueued_at = '2020-01-01T00:00:00Z'`)
	execAll(t, store, `update comments set body = 'Blocked on ACME-1 again.' where id = '`+fixture.comment+`'`)
	var redated time.Time
	if err := store.Pool.QueryRow(context.Background(), `
		select enqueued_at from refs_owed where kind = 'comment' and id = $1
	`, fixture.comment).Scan(&redated); err != nil {
		t.Fatalf("read the comment's row: %v", err)
	}
	if !redated.After(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("a second edit left the owed comment enqueued at %v; want it re-dated", redated)
	}
}

// A document version owes its document as a scope only when the lines its question map is read
// from change, in document order: a question block added, renamed, dropped or moved, settlement
// writing copied_from into one, or a fence opened or closed around one, each owes it, while an
// edit of the prose, or of fenced code, around blocks that stand as they were owes the version
// alone. A document's first version owes the scope when it declares a block, and not when it
// declares none.
func TestRefsOwedVersionOwesItsScopeOnlyWhenItsQuestionBlocksChange(t *testing.T) {
	store := openEmptyTestStore(t)
	ctx := context.Background()
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	execAll(t, store, `
		insert into projects (key, name) values ('ACME', 'Acme')`, `
		insert into issues (key, project_key, number, title, created_by, rank)
		values ('ACME-1', 'ACME', 1, 'One', '{"kind":"user","id":"alice"}', 'U')`)
	newDocument := func(slug string) string {
		return insertReturningID(t, store, `
			insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
			values ('ACME-1', 'ACME', $1, $1, 'doc', '{"kind":"user","id":"alice"}')
			returning id::text`, slug)
	}
	plain := newDocument("plain")
	declared := newDocument("declared")

	const copiedFrom = "6f1c1f39-3d43-4b8e-9d0a-2f0d6c5b7a10"
	versions := map[string]int{}
	for _, step := range []struct {
		name, doc, markdown string
		scope               bool
	}{
		{"a first version declaring no block", plain, "Blocked on ACME-1, see #q1.\n", false},
		{"a first version declaring a block", declared, "Intro.\n\n:::ask{#q1}\nWhich?\n:::\n", true},
		{"an edit of the prose alone", declared, "Intro, reworded.\n\n:::ask{#q1}\nWhich one?\n:::\n", false},
		{"a block added", declared, "Intro, reworded.\n\n:::ask{#q1}\nWhich one?\n:::\n\n:::ask{#q2}\nWhen?\n:::\n", true},
		{"the blocks moved", declared, ":::ask{#q2}\nWhen?\n:::\n\nIntro, reworded.\n\n:::ask{#q1}\nWhich one?\n:::\n", true},
		{"a block renamed", declared, ":::ask{#q3}\nWhen?\n:::\n\nIntro, reworded.\n\n:::ask{#q1}\nWhich one?\n:::\n", true},
		{"copied_from written into a block", declared, `:::ask{#q3 copied_from="` + copiedFrom + `"}` + "\nWhen?\n:::\n\nIntro, reworded.\n\n:::ask{#q1}\nWhich one?\n:::\n", true},
		{"a block dropped", declared, `:::ask{#q3 copied_from="` + copiedFrom + `"}` + "\nWhen?\n:::\n\nIntro, reworded.\n", true},
		{"a fence opened around an unchanged block", declared, "```\n" + `:::ask{#q3 copied_from="` + copiedFrom + `"}` + "\nWhen?\n:::\n```\n\nIntro, reworded.\n", true},
		{"the fenced code edited", declared, "```\n" + `:::ask{#q3 copied_from="` + copiedFrom + `"}` + "\nWhen, exactly?\n:::\n```\n\nIntro, reworded.\n", false},
		{"the fence removed", declared, `:::ask{#q3 copied_from="` + copiedFrom + `"}` + "\nWhen, exactly?\n:::\n\nIntro, reworded.\n", true},
		{"a later version declaring no block, after none did", plain, "Blocked on ACME-1 still.\n", false},
	} {
		execAll(t, store, `delete from refs_owed`)
		versions[step.doc]++
		if _, err := store.Pool.Exec(ctx, `
			insert into artifact_versions (artifact_id, number, markdown) values ($1, $2, $3)
		`, step.doc, versions[step.doc], step.markdown); err != nil {
			t.Fatalf("%s: insert version %d: %v", step.name, versions[step.doc], err)
		}
		want := []string{"artifact " + step.doc}
		if step.scope {
			want = sortedOwed("artifact "+step.doc, "scope "+step.doc)
		}
		if got := readOwed(t, store); !reflect.DeepEqual(got, want) {
			t.Errorf("owed after %s = %v; want %v", step.name, got, want)
		}
	}
}
