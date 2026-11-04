package store

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
)

// beforeRefsGrammar is the last migration before the re-index queue: 20261103205037_refs_grammar_owed
// through 20261103205042_refs_grammar_seed apply after it. lastRefsGrammarTrigger is the last of
// the triggers (20261103205038-20261103205041) that the seed runs after.
const (
	beforeRefsGrammar      = 20261101183525
	lastRefsGrammarTrigger = 20261103205041
)

// owedSource is one row of refs_grammar_owed: a source owed an index with the whole grammar, or,
// for the kind `scope`, a document whose sources are.
type owedSource struct{ kind, id string }

func readOwedSources(t *testing.T, store *Store) map[owedSource]time.Time {
	t.Helper()
	rows, err := store.Pool.Query(context.Background(), `select kind, id::text, enqueued_at from refs_grammar_owed`)
	if err != nil {
		t.Fatalf("read refs_grammar_owed: %v", err)
	}
	defer rows.Close()
	owed := map[owedSource]time.Time{}
	for rows.Next() {
		var source owedSource
		var at time.Time
		if err := rows.Scan(&source.kind, &source.id, &at); err != nil {
			t.Fatalf("scan refs_grammar_owed: %v", err)
		}
		owed[source] = at
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate refs_grammar_owed: %v", err)
	}
	return owed
}

func assertOwedSources(t *testing.T, store *Store, want ...owedSource) map[owedSource]time.Time {
	t.Helper()
	owed := readOwedSources(t, store)
	got := map[owedSource]bool{}
	for source := range owed {
		got[source] = true
	}
	wanted := map[owedSource]bool{}
	for _, source := range want {
		wanted[source] = true
	}
	if !reflect.DeepEqual(got, wanted) {
		t.Fatalf("owed sources = %v, want %v", got, wanted)
	}
	return owed
}

// refsGrammarFixture is one of every reference source, written by raw SQL as a server of the
// release before the queue writes them - which is what such a write is at the database, a row and
// nothing indexed from it: a document with a markdown version, an image whose version has none, an
// ask, a comment, an issue message and a direct message.
type refsGrammarFixture struct {
	document, image, ask, comment, message, direct string
}

func writeRefsGrammarFixture(t *testing.T, store *Store) refsGrammarFixture {
	t.Helper()
	seedBlockIndexIssue(t, store)
	fixture := refsGrammarFixture{}
	fixture.document = insertReturningID(t, store, `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ('CORE-1', 'CORE', 'spec', 'spec.md', 'doc', true, '{"kind":"user","id":"alice"}')
		returning id::text`)
	if _, err := store.Pool.Exec(context.Background(), `
		insert into artifact_versions (artifact_id, number, markdown) values ($1, 1, 'Blocked on CORE-1.')
	`, fixture.document); err != nil {
		t.Fatalf("write version 1: %v", err)
	}
	fixture.image = insertReturningID(t, store, `
		insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
		values ('CORE-1', 'CORE', 'photo-png', 'photo.png', 'image', '{"kind":"user","id":"alice"}')
		returning id::text`)
	if _, err := store.Pool.Exec(context.Background(), `
		insert into artifact_versions (artifact_id, number, content, mime) values ($1, 1, '\x89504e47', 'image/png')
	`, fixture.image); err != nil {
		t.Fatalf("write the image's version: %v", err)
	}
	fixture.ask = insertReturningID(t, store, `
		insert into asks (issue_key, author, question) values ('CORE-1', '{"kind":"session","id":"s1"}', 'After CORE-1?')
		returning id::text`)
	fixture.comment = insertReturningID(t, store, `
		insert into comments (issue_key, author, body) values ('CORE-1', '{"kind":"user","id":"alice"}', 'Blocked on CORE-1.')
		returning id::text`)
	fixture.message = insertReturningID(t, store, `
		insert into messages (issue_key, author, body) values ('CORE-1', '{"kind":"session","id":"s1"}', 'See #q1.')
		returning id::text`)
	fixture.direct = insertReturningID(t, store, `
		insert into messages (author, body, target) values ('{"kind":"user","id":"alice"}', 'CORE-1?', 'session:s1')
		returning id::text`)
	return fixture
}

// Text stored before this release was indexed without the whole grammar - the bare issue keys, the
// `#id`s and the short ids it resolves - so the seed owes a re-index to every comment, every ask,
// every issue message and every document with a markdown version, and to each such document's
// scope, with no edit and no command run. An image's version and a direct message, which no
// mention is indexed from, are owed nothing.
func TestTheRefsGrammarSeedOwesTextStoredBeforeIt(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	migrateThrough(t, store, beforeRefsGrammar)
	fixture := writeRefsGrammarFixture(t, store)

	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	assertOwedSources(t, store,
		owedSource{"artifact", fixture.document},
		owedSource{"scope", fixture.document},
		owedSource{"ask", fixture.ask},
		owedSource{"comment", fixture.comment},
		owedSource{"message", fixture.message})
}

// The triggers are live before the seed runs, so a write that lands between them is owed by its
// trigger and keeps the enqueued_at that write gave it: the seed adds the rows it finds missing
// and rewrites none.
func TestTheRefsGrammarSeedKeepsWhatATriggerAlreadyOwed(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	migrateThrough(t, store, lastRefsGrammarTrigger)
	fixture := writeRefsGrammarFixture(t, store)
	owed := readOwedSources(t, store)
	enqueued, ok := owed[owedSource{"comment", fixture.comment}]
	if !ok {
		t.Fatalf("the triggers owed %v, want the comment among them", owed)
	}

	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	after := assertOwedSources(t, store,
		owedSource{"artifact", fixture.document},
		owedSource{"scope", fixture.document},
		owedSource{"ask", fixture.ask},
		owedSource{"comment", fixture.comment},
		owedSource{"message", fixture.message})
	if at := after[owedSource{"comment", fixture.comment}]; !at.Equal(enqueued) {
		t.Fatalf("the seed moved the comment's enqueued_at to %v, want the trigger's %v", at, enqueued)
	}
}

// What a build that predates the whole grammar writes during the roll is owed a re-index by the
// triggers, whatever its text says: no SQL predicate finds a bare issue key, a `#id` or a short
// id. A comment, an ask and an issue message are owed on insert and on an edit of the text or of
// the columns that say which document a `#id` in it resolves against; a document version owes its
// document and its scope; an ask that gains a question block owes that document's scope; and an
// image's version, a direct message and an update of a column the index does not read owe nothing.
func TestTheRefsGrammarTriggersOweWhatAnyBuildWrites(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	fixture := writeRefsGrammarFixture(t, store)

	document := owedSource{"artifact", fixture.document}
	scope := owedSource{"scope", fixture.document}
	comment := owedSource{"comment", fixture.comment}
	ask := owedSource{"ask", fixture.ask}
	message := owedSource{"message", fixture.message}
	owed := assertOwedSources(t, store, document, scope, comment, ask, message)

	// A drain settles what it indexed; an edit of the text owes it again.
	if _, err := store.Pool.Exec(ctx, `delete from refs_grammar_owed`); err != nil {
		t.Fatalf("drain the queue: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `update comments set resolved = true where id = $1`, fixture.comment); err != nil {
		t.Fatalf("resolve the comment: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `update asks set urgency = 'high' where id = $1`, fixture.ask); err != nil {
		t.Fatalf("raise the ask's urgency: %v", err)
	}
	assertOwedSources(t, store)
	if _, err := store.Pool.Exec(ctx, `update comments set body = body || ' And CORE-1.' where id = $1`, fixture.comment); err != nil {
		t.Fatalf("edit the comment: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `update messages set body = body || ' And #q1.' where id = $1`, fixture.message); err != nil {
		t.Fatalf("edit the message: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `update messages set body = body || ' And #q1.' where id = $1`, fixture.direct); err != nil {
		t.Fatalf("edit the direct message: %v", err)
	}
	assertOwedSources(t, store, comment, message)

	// A later write of a row already owed moves its enqueued_at, so a drain that listed the
	// earlier time leaves it owed.
	past := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := store.Pool.Exec(ctx, `update refs_grammar_owed set enqueued_at = $2 where kind = 'comment' and id = $1`, fixture.comment, past); err != nil {
		t.Fatalf("age the owed comment: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `update comments set body = body || ' Again.' where id = $1`, fixture.comment); err != nil {
		t.Fatalf("edit the comment again: %v", err)
	}
	if at := readOwedSources(t, store)[comment]; !at.After(past) {
		t.Fatalf("an edited owed comment kept enqueued_at %v, want it moved past %v", at, past)
	}

	// A version of a document owes the document and its scope; an image's version owes neither.
	if _, err := store.Pool.Exec(ctx, `delete from refs_grammar_owed`); err != nil {
		t.Fatalf("drain the queue: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown) values ($1, 2, 'Blocked on CORE-1, see #q1.')
	`, fixture.document); err != nil {
		t.Fatalf("write version 2: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, content, mime) values ($1, 2, '\x89504e47', 'image/png')
	`, fixture.image); err != nil {
		t.Fatalf("write the image's second version: %v", err)
	}
	assertOwedSources(t, store, document, scope)

	// The settlement of a question block is an ask that gains it, and every `#id` naming that
	// block in the document's scope resolves from then on: the document's scope is owed.
	if _, err := store.Pool.Exec(ctx, `delete from refs_grammar_owed`); err != nil {
		t.Fatalf("drain the queue: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `
		update asks set block_id = 'q1', block_artifact_id = $2 where id = $1
	`, fixture.ask, fixture.document); err != nil {
		t.Fatalf("settle the question block: %v", err)
	}
	assertOwedSources(t, store, ask, scope)

	// The drain's read, exactly as refs.OwedSources runs it, goes through the queue's own index
	// and in the order the index holds: no sort, so the limit cuts the backlog before it is read
	// rather than after, which matters where the seed gives every row one enqueued_at.
	const read = `select o.kind, o.id::text, o.enqueued_at from refs_grammar_owed o order by o.enqueued_at, o.kind, o.id limit 200`
	plan := explainWithoutSeqScan(t, store, read)
	if !strings.Contains(plan, "refs_grammar_owed_enqueued") || strings.Contains(plan, "Sort") {
		t.Errorf("the owed read's plan does not read refs_grammar_owed_enqueued in order:\n%s", plan)
	}
	if len(owed) != 5 {
		t.Fatalf("the fixture owed %v, want one row per source and one scope", owed)
	}
}
